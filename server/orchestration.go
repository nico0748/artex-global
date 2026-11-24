package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// jsonResult marshals v to a JSON tool result.
func jsonResult(v any) (actool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(string(b)), nil
}

// このファイルは P2「タスクをまたぐオーケストレーションのツール群」(docs/跑分编排 §2 P2)を実装する。これらは host ツール——
// Manager(任意のタスクの Store)、Engine(一時停止)、タスクの作成の流れにアクセスする必要があるので、server の層に置く。
// 読み取り系のツールは「既存のタスクごとのツール」を対象のタスクの store に向け直して実行し(一時的な ToolSet を作って
// 対応するツールを Call する)、まったく同じロジックを再利用する；制御系(spawn/pause)は Manager/Engine を直接呼ぶ。
// 通信のツールと同じく tools 表に種まきし、agent ごとに結びつける(オーケストレーションの agent に結びつけたときだけ見える)。

// hostTools is the runtime host-tool provider fed to ToolAugment: traffic tools
// (gated by capture) + cross-task orchestration tools + user-defined custom tools.
// The second return is the names of custom tools flagged `deferred` (schema
// withheld, routed via SearchExtraTools/ExecuteExtraTool). Per-agent binding still
// decides who actually sees any of them.
//
//nolint:unused // used as the hostTools provider in wireAgentAugment
func (s *Server) hostTools() ([]actool.CoreTool, map[string][]string) {
	tools := append(s.m.HostTools(), s.orchestrationTools()...)
	tools = append(tools, s.findingRetestTools()...)
	tools = append(tools, s.platformTools()...) // プラットフォームの操作のツール(skill/ツール/MCP の作成と変更。Auto 用)
	custom, err := s.customTools()
	if err != nil {
		log.Printf("[custom-tool] 読み込みに失敗: %v", err)
		return tools, nil
	}
	tools = append(tools, custom...)
	// deferred custom tools → name -> its bound agent keys. ToolAugment turns a
	// name into a deferred entry only for agents it's actually bound to (so we don't
	// advertise a tool the per-agent binding will drop from the callable set).
	deferred := map[string][]string{}
	rows, _ := s.m.pg.ListCustomTools()
	for _, t := range rows {
		if t.Deferred && t.Enabled {
			deferred[t.Key] = t.Agents
		}
	}
	return tools, deferred
}

// orchestrationTools returns the cross-task tool set. Bound per-agent via the
// tools table (default: no binding — opt-in for orchestration agents).
func (s *Server) orchestrationTools() []actool.CoreTool {
	return []actool.CoreTool{
		s.toolListTasks(),
		s.toolListLLMProfiles(),
		s.toolSpawnTask(),
		s.toolPauseTask(),
		s.toolGetTaskGraph(),
		s.toolListTaskFindings(),
		s.toolAddHint(),
		s.toolGetWorkerTrace(),
		s.toolListWorkerTraces(),
		s.toolSearchWorkerTraces(),
		s.toolGetTaskNodeDetail(),
		s.toolUpdateFindingReport(),
		s.toolGetFindingTraffic(),
		s.toolBindFindingTraffic(),
	}
}

// --- schema helpers ---

func strParam(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// parseProfileID reads an LLM profile id from a tool arg that may arrive as a JSON
// number (5) or a numeric string ("5"); returns 0 when absent/unparseable.
func parseProfileID(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		v, _ := strconv.ParseInt(strings.TrimSpace(str), 10, 64)
		return v
	}
	return 0
}

func objSchema(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		req := make([]any, len(required))
		for i, r := range required {
			req[i] = r
		}
		m["required"] = req
	}
	return m
}

func roTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		ReadOnly:   func(json.RawMessage) bool { return true },
		Concurrent: func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

func wrTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

// delegateToTask resolves the `task_id` in the input, builds a ToolSet bound to
// that task's store, strips task_id, and calls the chosen per-task tool — so the
// cross-task read reuses the exact in-task logic against another task.
func (s *Server) delegateToTask(ctx context.Context, in json.RawMessage, pick func(*agent.ToolSet) actool.CoreTool) (actool.Result, error) {
	var head struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal(in, &head)
	if strings.TrimSpace(head.TaskID) == "" {
		return actool.Errorf("task_id は必須"), nil
	}
	t, ok := s.m.Task(head.TaskID)
	if !ok {
		return actool.Errorf("task が存在しない: " + head.TaskID), nil
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(in, &m)
	delete(m, "task_id")
	inner, _ := json.Marshal(m)
	tsx := agent.NewToolSet(t.Store, "orchestrator")
	if s.m.Assets() != nil {
		tsx.SetAssetStore(s.m.Assets(), s.m.Assets().Companies())
	}
	tsx.SetNotify(t.Notify)         // 汎用の起こし（専用のコールバックのない書き込み操作はこれを通る；読み取りのツールでは no-op）
	tsx.SetNotifyHint(t.NotifyHint) // add_hint → 「人が戦略上のヒントを N 件追加した：…」の起動を1件記録して planner を起こす
	return pick(tsx).Call(ctx, inner, nil)
}

// --- tools ---

func (s *Server) toolListTasks() actool.CoreTool {
	return roTool("list_tasks",
		"すべてのタスク(id/説明/目標/状態/実行時間/親タスク/LLM の設定)を一覧にする。オーケストレーションの agent が全体をつかみ、どのタスクが長く詰まっているか、それぞれどの LLM を使っているかを見るのに使う。実行時間：実行中=作成→現在、終端=作成→最後の活動(秒)。llm_profile：タスクの planner/worker が使う設定名で、(有効な設定)=全体の有効な設定に従う。",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			lastAct, _ := s.m.PG().LastActivityAll()
			// id -> name to resolve each task's pinned LLM profile.
			profName := map[int64]string{}
			if profs, err := s.m.pg.ListProfiles(); err == nil {
				for _, p := range profs {
					profName[p.ID] = p.Name
				}
			}
			out := make([]map[string]any, 0)
			for _, t := range s.m.List() {
				status := s.deriveTaskStatus(t)
				end := lastAct[t.ExpID]
				if live := s.engine.LastActivity(t.ID); live > end {
					end = live
				}
				dur := int64(0)
				if status == "running" {
					dur = time.Now().Unix() - t.CreatedAt
				} else if end > t.CreatedAt {
					dur = end - t.CreatedAt
				}
				row := map[string]any{"id": t.ID, "description": t.Description, "goal": t.Goal, "status": status, "run_seconds": dur}
				if t.ParentRef != "" {
					row["parent_ref"] = t.ParentRef
				}
				llmState := t.llmStateSnapshot()
				if llmState.ProfileID == nil {
					row["llm_profile"] = "(有効な設定)"
				} else if n, ok := profName[*llmState.ProfileID]; ok {
					row["llm_profile"] = n
				} else {
					row["llm_profile"] = fmt.Sprintf("#%d(削除済み)", *llmState.ProfileID)
				}
				out = append(out, row)
			}
			return jsonResult(out)
		})
}

// toolListLLMProfiles lists the available LLM profiles (name/model/active) so an
// orchestration agent can pick one for spawn_task's llm_profile. Never leaks keys.
func (s *Server) toolListLLMProfiles() actool.CoreTool {
	return roTool("list_llm_profiles",
		"使える LLM の設定(profile)を一覧にする：id、名前、モデル、形式、現在の有効な設定かどうか。id を spawn_task の llm_profile_id 引数に渡して子タスク専用の LLM を指定する（偵察には安いモデル、悪用には強いモデルなど）。API Key は含まない。",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			profs, err := s.m.pg.ListProfiles()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			out := make([]map[string]any, 0, len(profs))
			for _, p := range profs {
				out = append(out, map[string]any{
					"id": p.ID, "name": p.Name, "model": p.Model, "format": p.Format, "is_active": p.IsDefault,
				})
			}
			return jsonResult(map[string]any{"profiles": out})
		})
}

func (s *Server) toolSpawnTask() actool.CoreTool {
	return wrTool("spawn_task",
		"子タスクを1つ新しく作って探索エンジンを起動し、task_id を返す。1つのこと(1問/1つの対象など)を独立したタスクとして出すのに使う。parent_ref は任意：現在のオーケストレーションに関連する親タスクの id を入れて親子を関連付ける。",
		objSchema(map[string]any{
			"description":            strParam("タスクの説明(短いタイトル)"),
			"goal":                   strParam("タスクの目標(何を達成するか)"),
			"parent_ref":             strParam("任意：親タスクの id(親子を関連付ける)"),
			"source_task_ids":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": fmt.Sprintf("任意：読み取り専用で引き継ぐ元のタスクの id の一覧(最大 %d 個)。子タスクはこれらのタスクですでに明らかになった資産/結論を読み取り専用で参照して出発点にできる；parent_ref の純粋な親子のポインターとは違い、これは内容の引き継ぎ。", db.MaxTaskSourceCount)},
			"llm_profile_id":         map[string]any{"type": "integer", "description": "任意：この子タスクの planner/worker が使う LLM の設定の id を指定する(list_llm_profiles を参照)；空なら親タスクを引き継ぎ、さらに全体の有効な設定に戻る"},
			"timeout_seconds":        map[string]any{"type": "integer", "description": "任意：タスク単位のタイムアウト(秒)。時間になるときれいな締めくくりを起動して timeout の終端に入る；空か 0 = 時間の制限なし"},
			"plan_heartbeat_seconds": map[string]any{"type": "integer", "description": "任意：planner のハートビートの起動の間隔(秒)。前のラウンドの計画の終わり/タスクの開始からその値に達し、その間に起動がなければ → 計画を1ラウンド起動する(デッドロックの最後の砦 + 実行中の worker を見張るために起こす)。空か 0 = 既定 600(10min)；"},
			"seed_first_intent":      map[string]any{"type": "boolean", "description": "任意：簡単なタスクならオンにでき、作成時に種の意図を1件直接出して(内容=説明+目標)、worker が最初のラウンドの planner を待たずにすぐテストを始められる；既定は false(標準の「先に計画してから実行」を通る)。"},
		}, "description", "goal"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Description          string          `json:"description"`
				Goal                 string          `json:"goal"`
				ParentRef            string          `json:"parent_ref"`
				SourceTaskIDs        []string        `json:"source_task_ids"`
				LLMProfileID         json.RawMessage `json:"llm_profile_id"`
				TimeoutSeconds       int             `json:"timeout_seconds"`
				PlanHeartbeatSeconds int             `json:"plan_heartbeat_seconds"`
				SeedFirstIntent      bool            `json:"seed_first_intent"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Description) == "" {
				a.Description = "名前のないタスク"
			}
			if strings.TrimSpace(a.Goal) == "" {
				return actool.Errorf("goal は必須"), nil
			}
			if a.TimeoutSeconds < 0 {
				a.TimeoutSeconds = 0
			}
			// 読み取り専用で引き継ぐ元のタスク：数の上限 + 各 id が有効/重複なし/存在する。検証のルールは HTTP でのタスクの作成と同じ。
			if len(a.SourceTaskIDs) > db.MaxTaskSourceCount {
				return actool.Errorf(fmt.Sprintf("関連するタスクは最大 %d 個まで選べる", db.MaxTaskSourceCount)), nil
			}
			sourceIDs := make([]int64, 0, len(a.SourceTaskIDs))
			seenSources := map[int64]bool{}
			for _, raw := range a.SourceTaskIDs {
				id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
				if err != nil || id <= 0 || seenSources[id] {
					return actool.Errorf("関連するタスクの id が無効か重複している"), nil
				}
				if _, ok := s.m.Task(strconv.FormatInt(id, 10)); !ok {
					return actool.Errorf(fmt.Sprintf("関連するタスク #%d が存在しない", id)), nil
				}
				seenSources[id] = true
				sourceIDs = append(sourceIDs, id)
			}
			// LLM profile resolution: explicit id > inherit parent's pin > active(nil).
			var pin *int64
			if id := parseProfileID(a.LLMProfileID); id > 0 {
				if _, ok := s.loadProfileConfig(id); !ok {
					return actool.Errorf(fmt.Sprintf("LLM の設定 #%d が存在しないか、API Key が設定されていない", id)), nil
				}
				pin = &id
			} else if a.ParentRef != "" {
				if pt, ok := s.m.Task(a.ParentRef); ok {
					pin = pt.LLMProfileID
				}
			}
			var llmIDs []int64
			if pin != nil {
				llmIDs = []int64{*pin}
			}
			t, err := s.m.CreateTaskWithOptions(a.Description, a.Goal, db.TaskCreateOptions{
				SourceTaskIDs:        sourceIDs,
				LLMProfileIDs:        llmIDs,
				TimeoutSeconds:       a.TimeoutSeconds,
				PlanHeartbeatSeconds: a.PlanHeartbeatSeconds,
			})
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if a.ParentRef != "" {
				t.ParentRef = a.ParentRef
				if id, e := strconv.ParseInt(t.ID, 10, 64); e == nil {
					_ = s.m.PG().SetParentRef(id, a.ParentRef)
				}
			}
			// 作成後の共通の流れで、HTTP でのタスクの作成(server.go createTask)と同じ launchTask を再利用する:
			// seed + バックグラウンドで見える形での目標の分解(第0ラウンド/LLM の手順/goal を1件ずつ) + engine.Run。
			// seed_first_intent の既定は false(標準の「先に計画してから実行」);簡単なタスクではオンにして work を1つ直接出してテストできる。
			s.launchTask(t, a.Description+" "+a.Goal, a.SeedFirstIntent)
			return actool.Text(fmt.Sprintf("task created: %s", t.ID)), nil
		})
}

func (s *Server) toolPauseTask() actool.CoreTool {
	return wrTool("pause_task", "指定したタスクを一時停止する(その planner/worker のループを止める)。",
		objSchema(map[string]any{"task_id": strParam("一時停止するタスクの id")}, "task_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID string `json:"task_id"`
			}
			_ = json.Unmarshal(in, &a)
			t, ok := s.m.Task(a.TaskID)
			if !ok {
				return actool.Errorf("task が存在しない: " + a.TaskID), nil
			}
			if _, err := s.applyTaskControlWithCause(t, "pause", agent.AbortPausedByOrchestrator); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("task paused: " + a.TaskID), nil
		})
}

func (s *Server) toolGetTaskGraph() actool.CoreTool {
	return roTool("get_task_graph", "指定したタスクの探索グラフの概要を読む(graph_overview と同じ：資産の数/frontier/発見/カバレッジなど)。task_id でタスクを指定する。",
		objSchema(map[string]any{"task_id": strParam("タスクの id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GraphOverviewTool)
		})
}

func (s *Server) toolListTaskFindings() actool.CoreTool {
	return roTool("list_task_findings", "指定したタスクの確認済みの脆弱性を読む(flag/PoC を含む；各件に id/task_id/intent_id/vulnclass/severity/要約/状態が付く)。task_id でタスクを指定する。",
		objSchema(map[string]any{"task_id": strParam("タスクの id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListFindingsTool)
		})
}

func (s *Server) toolAddHint() actool.CoreTool {
	return wrTool("add_task_hint", "指定したタスクに戦略上のヒントを注入する(そのタスクの planner が次のラウンドで意図を生成するときにそれを読む)。\n"+
		"★まとめて出すことを優先：複数のヒントは hints 配列に入れて一度に提出する（ids 配列を返し、hints と同じ長さ・同じ順序。失敗した項目の id=0）；単発なら hints を省いて最上位の text を直接渡す。",
		objSchema(map[string]any{
			"task_id":      strParam("タスクの id"),
			"hints":        map[string]any{"type": "array", "description": "【こちらを優先して使う】ヒントの配列で、各要素のフィールドは最上位と同じ（text/asset_ids/traffic_refs）。", "items": objSchema(map[string]any{"text": strParam("ヒントの内容"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, "traffic_refs": agent.HintTrafficSchema()})},
			"text":         strParam("[単発] ヒントの内容"),
			"traffic_refs": agent.HintTrafficSchema(),
			"asset_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "錨を下ろす資産の id（任意。0/1/複数；そのタスク内の資産の id）"},
		}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).AddHintTool)
		})
}

func (s *Server) toolGetWorkerTrace() actool.CoreTool {
	return roTool("get_task_worker_trace",
		"指定したタスクのある work(意図)の実行の過程を見る：get_task_worker_trace(task_id, intent_id) で手順の要約を見る；さらに step_ids=[...] を付けてその手順の完全な内容を取る(一度に最大 5 個。多く渡しても先頭の 5 個だけを返す)。",
		objSchema(map[string]any{
			"task_id":   strParam("タスクの id"),
			"intent_id": map[string]any{"type": "integer", "description": "意図の id(そのタスクの work)"},
			"step_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "任意：完全な内容を取る手順の id(一度に最大 5 個。多く渡しても先頭の 5 個だけを返し、残りは omitted_step_ids に並べる)"},
		}, "task_id", "intent_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GetWorkerTraceTool)
		})
}

func (s *Server) toolListWorkerTraces() actool.CoreTool {
	return roTool("list_task_worker_traces", "指定したタスクでどの work(意図)が実行されたか + それぞれの手順の数を一覧にし、どの work を読み返す価値があるかを見つけるのに使う(その後 get_task_worker_trace を使う)。",
		objSchema(map[string]any{"task_id": strParam("タスクの id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListWorkerTracesTool)
		})
}

func (s *Server) toolSearchWorkerTraces() actool.CoreTool {
	return roTool("search_task_worker_traces", "指定したタスクで、すべての work の実行の過程をキーワードで検索する(一致した手順の要約 + intent_id を返す)。",
		objSchema(map[string]any{"task_id": strParam("タスクの id"), "q": strParam("検索のキーワード")}, "task_id", "q"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).SearchWorkerTracesTool)
		})
}

func (s *Server) toolGetTaskNodeDetail() actool.CoreTool {
	return roTool("get_task_node_detail",
		"指定したタスクのある探索グラフのノードの完全な内容を読む(発見/事実/意図/目標：要約 + 詳細/証拠/PoC)。id は探索ノードの id(report_finding が返したもの、または list_task_findings の id など)。脆弱性の報告を書く前に、これでその脆弱性の完全な証拠を取る。",
		objSchema(map[string]any{
			"task_id": strParam("タスクの id"),
			"id":      map[string]any{"type": "integer", "description": "探索グラフのノードの id(資産の id ではない)"},
		}, "task_id", "id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).NodeDetailTool)
		})
}

// toolUpdateFindingReport writes/overwrites a finding's detailed Markdown report.
// finding_id is the id report_finding returned ("finding recorded: <id>", the
// finding node id). The write (SetFindingReportByNodeID) is keyed by node_id and
// task-agnostic, so this host tool needs no task_id / exploration store.
func (s *Server) toolUpdateFindingReport() actool.CoreTool {
	return wrTool("update_finding_report",
		"登録済みの脆弱性に【詳しい報告】を書き込む/更新する(Markdown の全文で、古い内容を丸ごと上書きする)。finding_id には report_finding が返した id(\"finding recorded: <id>\" の数字)を渡す。報告には、脆弱性の概要、影響と危険、再現の手順、証拠/PoC、修正の提案を含めるとよい。",
		objSchema(map[string]any{
			"finding_id":       map[string]any{"type": "integer", "description": "対象の脆弱性の id(report_finding が返した id)"},
			"report":           strParam("詳しい報告の全文。Markdown 形式"),
			"evidence_version": map[string]any{"type": "integer", "description": "get_finding_traffic が返した証拠の version；報告が新しい証拠の変更を上書きするのを防ぐのに使う"},
		}, "finding_id", "report"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				EvidenceVersion *int64          `json:"evidence_version"`
				FindingID       json.RawMessage `json:"finding_id"`
				Report          string          `json:"report"`
			}
			_ = json.Unmarshal(in, &a)
			nodeID := parseProfileID(a.FindingID) // 「数字または数字の文字列」の解析を再利用する
			if nodeID <= 0 {
				return actool.Errorf("finding_id が無効"), nil
			}
			n, err := s.m.pg.SetFindingReportVersionByNodeID(ctx, nodeID, a.Report, a.EvidenceVersion)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if n == 0 {
				return actool.Errorf(fmt.Sprintf("finding_id=%d に対応する脆弱性の記録が見つからない(先に report_finding で登録すること)", nodeID)), nil
			}
			return actool.Text(fmt.Sprintf("finding %d report updated (%d chars)", nodeID, len(a.Report))), nil
		})
}

// deriveTaskStatus mirrors listTasks' status derivation for the list_tasks tool.
func (s *Server) deriveTaskStatus(t *Task) string {
	lifecycle := t.lifecycleSnapshot()
	switch {
	case isTerminalStatus(lifecycle.Status):
		return lifecycle.Status
	case lifecycle.Paused || s.engine.IsPaused(t.ID):
		return "paused"
	case s.engine.ReadyFor(t) && s.engine.Started(t.ID):
		return "running"
	}
	return "created"
}

// orchestrationToolSeeds seeds the cross-task tools into the tools table so they
// are bindable per-agent (default: bound to nobody — opt-in for orchestration
// agents). First-insert only, like the traffic seeds.
func (s *Server) seedOrchestrationTools() {
	// task-op + platform tools default-bind to the built-in Auto agent (もともとプラットフォームを
	// 操作するためのもの)。SeedTool は最初の挿入で効く;古い DB ですでに種まきした行は seedAutoDefaultBindings で結びつけを補う。
	autoAgents, _ := json.Marshal([]string{"auto"})
	for _, t := range s.orchestrationTools() {
		schema, _ := json.Marshal(t.InputSchema())
		bindings := autoAgents
		if t.Name() == "bind_finding_traffic" {
			bindings = json.RawMessage(`["reporter"]`)
		}
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, bindings)
	}
	for _, t := range s.platformTools() {
		schema, _ := json.Marshal(t.InputSchema())
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, autoAgents)
	}
	s.refreshBuiltinToolSchemas()
	s.seedAutoDefaultBindings()
	s.seedPlannerDefaultBindings()
	s.seedPlannerListAssetsBinding()
	s.seedCompanyScopeRebind()
	s.seedWorkerReadToolsUnbind() // list_facts/list_companies/list_worker_traces を worker の既定から外す(1回だけ)
	s.seedWorkerReadbackRebind()  // 古い移行の誤削除を直す：search_all_worker_traces/get_worker_trace/node_detail を worker に結びつけ直す(1回だけ)
	s.seedAutoReportFindingBinding()
	s.unbindGoalMetDefault()
	s.reseedGoalsPrompt()             // goals のプロンプトに「操作の制約を抜き出す」手順を加えた → 古い DB に新しい既定の版を足す(1回だけ)
	s.reseedMainAgentPrompt()         // mainagent のプロンプトに「目標の達成後に add_intent するとき、目標を作るか聞き返す」を加えた(1回だけ)
	s.reseedPlannerPrompt()           // planner のプロンプト:「意図 0 個」の正当な理由を書き直し + 数値での受け入れの照合を加えた(1回だけ)
	s.reseedWorkerPrompt()            // worker のプロンプト:否定の結論の証拠の基準を加えた(1回だけ)
	s.seedReporterAgent()             // 「보고서 작성」agent を用意 + ツールの結びつけ + finding のトリガー(1回だけ)
	s.upgradeReporterTriggerMessage() // 古い DB の移行を補う:reporter に evidence_version を返させる(1回だけ)
	s.seedFindingTrafficTools()       // 任意の証拠の引数と読み取り専用の証拠のツールを足し、ユーザーの設定は残す
	s.seedFindingWorkflowTools()
	// 注：pentest の既定のツールの結びつけは移行が要らない——BuiltinToolSeeds がまっさらな初期化のときに
	// list_assets/insert_assets/report_finding/list_findings/list_companies を
	// pentest と一緒に種まきする（プロジェクトにまだ古い DB はないので、移行はしない）。
}

// refreshBuiltinToolSchemas propagates code schema/description changes on the
// orchestration + platform tools into already-seeded rows ONCE per version flag —
// SeedTool is first-insert-only, so a new param (e.g. spawn_task の llm_profile) never
// reaches an old DB otherwise. Preserves each tool's agent binding + enabled flag.
// Bump the flag whenever these tools' schemas/descriptions change in code.
func (s *Server) refreshBuiltinToolSchemas() {
	const flag = "tool_schema_refresh_v7_list_facts_paging"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	tools := append(s.orchestrationTools(), s.platformTools()...)
	for _, t := range tools {
		schema, _ := json.Marshal(t.InputSchema())
		if err := s.m.pg.RefreshToolDefaults(t.Name(), t.Description(), schema); err != nil {
			log.Printf("[tools] refresh %s schema failed: %v", t.Name(), err)
		}
	}
	// 同時に一部の組み込みの agent のツールをコードの既定に塗り直す：
	//   - goal_met：古い DB で種まきした説明には「このラウンドの計画を終える」という誤解を招く言い回しがあり、planner がそれを
	//     「空のラウンドを終える」手段として扱い、動き出してすぐにタスク全体の完了と誤判定してしまう。
	//   - insert_assets：related の入力を足した(資産が現在のタスクと関連するかの印で、カバレッジに入れるかを決める)。
	//     SeedTool は最初の挿入だけなので、古い DB ですでに種まきした schema はそうしないとこの新しい引数を受け取れない。
	//   - list_facts：ページ分けに変え、limit/before/q の入力を足した；古い DB ですでに種まきした空の schema はそうしないと
	//     ツールの管理ページで「引数なし」と表示され、モデルもこれらの引数の説明を受け取れない。
	refreshBuiltin := map[string]bool{"goal_met": true, "insert_assets": true, "list_facts": true}
	for _, sd := range agent.BuiltinToolSeeds() {
		if !refreshBuiltin[sd.Key] {
			continue
		}
		schema, _ := json.Marshal(sd.Schema)
		if err := s.m.pg.RefreshToolDefaults(sd.Key, sd.Desc, schema); err != nil {
			log.Printf("[tools] refresh %s desc failed: %v", sd.Key, err)
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
	log.Print(logT("[tools] orchestration/platform 도구 스키마를 코드 기본값으로 새로고침(1회성)"))
}

// unbindGoalMetDefault removes goal_met's default "planner" binding ONCE (guarded by
// a settings flag), so existing DBs match the new default of NO agent. goal_met bypasses
// per-goal prove_goal to declare the whole task done — powerful/risky and redundant with
// the prove_goal→auto-complete path — so it ships unbound; users can re-bind it per agent
// in the UI. A user's own binding to another agent is untouched (we only strip planner).
func (s *Server) unbindGoalMetDefault() {
	const flag = "goal_met_unbind_default_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("planner", "goal_met"); err != nil {
		log.Printf("[tools] goal_met の planner からの結びつけを外せなかった: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// reseedGoalsPrompt は goals の目標の分解器のプロンプトを【現在のコードの既定】に塗り直す——既定の本文に
// 「まず操作の制約を抜き出し(set_constraints)、それから目標を分解する」という手順を足したが、SeedPromptIfEmpty は最初の挿入だけなので、古い DB に
// すでにある version 1 はこの手順を受け取れない。ここでは版の管理で【新しい版を1つ足して】そちらに切り替える(ResetPromptToDefault)。
// 古い版は履歴に残るので、ユーザーがカスタマイズしていたら版の記録から取り戻せる。settings の flag で守る → 1回だけ行う;
// 今後既定がまた変わったらこの flag を上げる。まっさらな DB では処理は要らない(SeedPromptIfEmpty がすでに最新の既定を種まきしている)。
func (s *Server) reseedGoalsPrompt() {
	const flag = "goals_prompt_constraint_step_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 成功してもしなくても1回だけ試す
	a, err := s.m.pg.GetAgentByKey("goals")
	if err != nil || a == nil {
		return // まっさらな DB でまだ agent の行を作っていなければ、seedPrompts が最新の既定を直接種まきするので、この移行は要らない
	}
	tmpl := agent.BuiltinPromptSeeds()["goals"]
	if tmpl == "" {
		return
	}
	// まっさらな DB では seedPrompts がすでに最新の既定を種まきした → 現在の版はすでにコードの既定と同じなので、重ねて版を足す必要はない。
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] goals のプロンプトを新しい既定に塗り直せなかった: %v", err)
		return
	}
	log.Printf("[prompts] goals のプロンプトに新しい既定の版を足した(操作の制約を抜き出す手順を加えた。1回だけ)")
}

// reseedMainAgentPrompt は mainagent のプロンプトを【現在のコードの既定】に塗り直す——既定の本文に「目標がすべて
// 達成された後に add_intent で意図を直接投げるとき、正式な目標として登録するかを人に聞き返す」という案内を足したが、SeedPromptIfEmpty は最初の挿入
// だけなので、古い DB にすでにある版は受け取れない。版の管理で【新しい版を1つ足して】そちらに切り替える(ResetPromptToDefault)。古い版は引き続き
// 履歴に残るので、ユーザーがカスタマイズしていたら版の記録から取り戻せる。settings の flag で守る → 1回だけ行う。まっさらな DB では処理は要らない
// (SeedPromptIfEmpty がすでに最新の既定を種まきしている)。reseedGoalsPrompt とまったく同じ形。
func (s *Server) reseedMainAgentPrompt() {
	const flag = "mainagent_prompt_goalless_intent_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 成功してもしなくても1回だけ試す
	a, err := s.m.pg.GetAgentByKey("mainagent")
	if err != nil || a == nil {
		return // まっさらな DB でまだ agent の行を作っていなければ、seedPrompts が最新の既定を直接種まきするので、この移行は要らない
	}
	tmpl := agent.BuiltinPromptSeeds()["mainagent"]
	if tmpl == "" {
		return
	}
	// まっさらな DB では seedPrompts がすでに最新の既定を種まきした → 現在の版はすでにコードの既定と同じなので、重ねて版を足す必要はない。
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] mainagent のプロンプトを新しい既定に塗り直せなかった: %v", err)
		return
	}
	log.Printf("[prompts] mainagent のプロンプトに新しい既定の版を足した(目標の達成後に目標を作るか聞き返す案内を加えた。1回だけ)")
}

// reseedPlannerPrompt は planner のプロンプトを【現在のコードの既定】に塗り直す——既定の本文を簡潔に組み直し、「控えめに」を
// 重複の排除だけに格下げし、「カバレッジより深さを優先」「最低ライン:目標が未達成で実行中の意図がなければ必ず産出する」を足し、否定の結論の確かめ直しに上限を付けた。
// 既定に実質的な変更があるたびに下の flag を上げ(現在は v2)、既存の古い DB にもう一度塗らせる。SeedPromptIfEmpty は最初の挿入だけなので、古い DB にすでにある版は受け取れず、版の管理で
// 【新しい版を1つ足して】そちらに切り替える(ResetPromptToDefault)。古い版は引き続き履歴に残るので、ユーザーがカスタマイズしていたら版の記録から
// 取り戻せる。settings の flag で守る → 1回だけ行う。まっさらな DB では処理は要らない(SeedPromptIfEmpty がすでに最新の既定を種まきしている)。
// reseedGoalsPrompt とまったく同じ形。
func (s *Server) reseedPlannerPrompt() {
	const flag = "planner_prompt_compact_realistic_v2"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 成功してもしなくても1回だけ試す
	a, err := s.m.pg.GetAgentByKey("planner")
	if err != nil || a == nil {
		return // まっさらな DB でまだ agent の行を作っていなければ、seedPrompts が最新の既定を直接種まきするので、この移行は要らない
	}
	tmpl := agent.BuiltinPromptSeeds()["planner"]
	if tmpl == "" {
		return
	}
	// まっさらな DB では seedPrompts がすでに最新の既定を種まきした → 現在の版はすでにコードの既定と同じなので、重ねて版を足す必要はない。
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] planner のプロンプトを新しい既定に塗り直せなかった: %v", err)
		return
	}
	log.Printf("[prompts] planner のプロンプトに新しい既定の版を足した(簡潔な組み直し+控えめを重複の排除に格下げ+深さの優先+否定の確かめ直しの上限。1回だけ)")
}

// reseedWorkerPrompt は worker のプロンプトを【現在のコードの既定】に塗り直す——既定の本文の record_fact の部分から「否定系の結論は
// 観察+試しの読みを書く」の1文を丸ごと消し、confidence(observed/inferred)と「この意図の手段をやり尽くしたか」を切り離した(これらは計画者を誤らせやすい)。
// 同時に facts 配列で分けて書くのを「互いに完全に独立し、まとめられない」ごく少数の例外に絞った。flag を v3 に上げ、既存の古い DB にもう一度塗らせる。
// SeedPromptIfEmpty は最初の挿入だけなので、古い DB にすでにある版は受け取れず、版の管理で【新しい版を1つ足して】そちらに切り替える。古い版は引き続き履歴に残り、取り戻せる。
// settings の flag で守る → 1回だけ行う。まっさらな DB では処理は要らない。reseedGoalsPrompt とまったく同じ形。
func (s *Server) reseedWorkerPrompt() {
	const flag = "worker_prompt_compact_v4"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 成功してもしなくても1回だけ試す
	a, err := s.m.pg.GetAgentByKey("worker")
	if err != nil || a == nil {
		return // まっさらな DB でまだ agent の行を作っていなければ、seedPrompts が最新の既定を直接種まきするので、この移行は要らない
	}
	tmpl := agent.BuiltinPromptSeeds()["worker"]
	if tmpl == "" {
		return
	}
	// まっさらな DB では seedPrompts がすでに最新の既定を種まきした → 現在の版はすでにコードの既定と同じなので、重ねて版を足す必要はない。
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] worker のプロンプトを新しい既定に塗り直せなかった: %v", err)
		return
	}
	log.Printf("[prompts] worker のプロンプトに新しい既定の版を足した(コンテキストを調べる部分を list_assets/list_findings に絞り、list_facts/node_detail/asset_neighbors を除いた。1回だけ)")
}

// reporterToolCallMessage は、報告を書く前に get_finding_traffic を1回読むことを無条件に求めなければならない。
// このツールは読み取り専用で「取得の切り替えに依存しない」ので、自動の結びつけがオンでもオフでも人が結びつけた証拠を読める。ここを
// 「自動の結びつけがオンのときだけ読む」と書くと、既定のオフの設定では reporter が evidence_version を渡さず、
// SetFindingReportVersionByNodeID が legacy の意味で -1 を書き、脆弱性の詳細と Markdown の書き出しが
// それ以降ずっと「証拠が変わったので、報告の更新が必要」のままになり、UI にはそれを消す入口がまったくない。
const reporterToolCallMessage = "直前に report_finding で脆弱性が1件登録された。戻り値の JSON の finding_id（独立した脆弱性の記録 ID）と finding_node_id（探索ノードの ID）を読み、" +
	"まず get_finding_traffic(finding_id) で現在の証拠の一覧とその version を読む（空の一覧は通常のことで、いつもどおり報告を書く）；" +
	"実行の案内で自動の結びつけがオンなら、読む前に今回の脆弱性の通信を確かめて関連付ける。ノードの詳細には finding_node_id を使う。" +
	"最後に update_finding_report(finding_id=finding_node_id, report, evidence_version=実際に読んだ版) を呼んで保存する。" +
	"evidence_version は必ず渡すこと。さもないと報告はずっと更新待ちの印が付いたままになる。2種類の番号を混ぜないこと。"

// 古い版の起動のメッセージ(0.3.8 以前)。それと一字一句同じ記録だけを移行で上書きし、ユーザーが変えたものはそのまま残す。
const reporterToolCallMessageV1 = "上面刚有一个漏洞被 report_finding 登记。请从触发上下文里取出 finding_id" +
	"（工具返回 \"finding recorded: <id>\" 里的数字）与任务 id，按你的职责撰写该漏洞的详细报告，" +
	"最后调用 update_finding_report(finding_id, report) 保存。"

// upgradeReporterTriggerMessage は古い DB でまだ既定の文のままの reporter の起動のメッセージを新しい版に塗り直す。
// seedReporterAgent は reporter_agent_seed_v1 で守られ、agent を新しく作るときだけトリガーを書くので、
// アップグレードしてきた DB は新しい文を受け取れない —— ツールの schema は seedFindingTrafficTools が
// evidence_version を補ったが、reporter にそれを使うよう伝えるものが何もない。1回だけで、変えられていない文だけを上書きする。
func (s *Server) upgradeReporterTriggerMessage() {
	const flag = "reporter_trigger_evidence_version_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 1回だけ試す
	triggers, err := s.m.pg.ListTriggersFor("reporter")
	if err != nil {
		log.Printf("[reporter] トリガーを読めなかった: %v", err)
		return
	}
	for _, t := range triggers {
		if !t.OnToolCall || t.ToolCallMessage != reporterToolCallMessageV1 {
			continue // ユーザーが変えたか finding のトリガーでないので、触らない。
		}
		t.ToolCallMessage = reporterToolCallMessage
		if err := s.m.pg.UpdateTrigger(t); err != nil {
			log.Printf("[reporter] 起動のメッセージを更新できなかった: %v", err)
			return
		}
		log.Printf("[reporter] 起動のメッセージを、evidence_version を読んで返す版に更新した")
	}
}

// reporterAgentName·reporterAgentDescription 은 시드되는 reporter 에이전트의 표시 전용
// 라벨이다(한국어화). 에이전트의 두뇌(段[A] agent.ReporterDefaultPrompt)와 트리거 주입
// 메시지(reporterToolCallMessage)는 모델 입력이라 일본어로 두지만(원래 중국어), 이 이름·설명은
// `agentDTO` 로 system/agents UI 에만 렌더되고(server_mgmt.go) 어떤 프롬프트·에이전트 선택에도
// 들어가지 않는 순수 UI 라벨이라 한국어화한다(BRIEF 사용자 노출 산출물 한국어화 범위). 트리거는
// report_finding 도구 호출 기반(OnToolCall·결정적)이라 reporter 는 key "reporter" 로 참조될 뿐
// 표시 이름으로 선택되지 않는다. 회귀 방어: reporter_seed_localized_test.go. [[F35]]
const reporterAgentName = "보고서 작성"
const reporterAgentDescription = "취약점 상세 보고서 작성: 취약점을 발견하면 자동으로 트리거되어, 증거와 실행 과정을 조회한 뒤 Markdown 보고서를 작성해 해당 취약점에 기록합니다."

// seedReporterAgent は「보고서 작성」というカスタムの agent を1つ用意する(builtin=false、UI で編集/削除できる)：
// update_finding_report + タスクを調べるツールを結びつけ、「report_finding が呼ばれたら起動する」
// トリガーを付ける —— 脆弱性を1件登録するたびにそれを起こして詳しい報告を書かせる。1回だけ(settings の flag で守る)：ユーザーが消したら作り直さない。
// 依存：orchestration のツールはこの関数の上の SeedTool ですでに DB に入っているので、結びつけられる。
func (s *Server) seedReporterAgent() {
	const flag = "reporter_agent_seed_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 成功してもしなくても1回だけ試す

	if exist, _ := s.m.pg.GetAgentByKey("reporter"); exist != nil {
		return // key はすでに使われている(ユーザーが手で作った)——上書きしない
	}
	a, err := s.m.pg.CreateAgent("reporter", reporterAgentName, reporterAgentDescription)
	if err != nil {
		log.Printf("[reporter] agent を作れなかった: %v", err)
		return
	}
	if err := s.m.pg.SeedPromptIfEmpty(a.ID, agent.ReporterDefaultPrompt); err != nil {
		log.Printf("[reporter] seed prompt に失敗: %v", err)
	}
	// 起動の実行の方針：parallel + none —— 脆弱性1件に報告1つで、複数の finding がそれぞれ同時に書く。
	// merge は none でなければならない：さもないと(既定の all)一波の finding が1回の実行にまとめられ、並列の意味がなくなる。
	// maxParallel=5：同時に最大 5 つの報告のセッションにし、一度に多すぎる LLM の呼び出しを避ける。
	if err := s.m.pg.SetAgentTriggerBehavior("reporter", "parallel", "none", 5); err != nil {
		log.Printf("[reporter] 起動の実行の方針を設定できなかった: %v", err)
	}
	// 必要なツールを結びつける：報告を書く + 証拠/実行の過程/状況を読む。
	if err := s.m.pg.AddAgentToToolBinding("reporter", []string{
		"update_finding_report", "get_task_node_detail", "list_task_findings",
		"get_task_worker_trace", "list_task_worker_traces", "search_task_worker_traces",
		"get_task_graph",
	}); err != nil {
		log.Printf("[reporter] ツールを結びつけられなかった: %v", err)
	}
	// トリガー：report_finding が呼ばれたら起動する（ツールの戻り値 "finding recorded: <id>" に finding_id が付き、
	// タスクの id も起動のメッセージにある）。
	if _, err := s.m.pg.CreateTrigger(&db.AgentTrigger{
		AgentKey:        "reporter",
		Enabled:         true,
		OnToolCall:      true,
		ToolNames:       []string{"report_finding"},
		ToolCallMessage: reporterToolCallMessage,
	}); err != nil {
		log.Printf("[reporter] トリガーを作れなかった: %v", err)
	}
	log.Printf(logT("[reporter] 「%s」 agent + finding 트리거 사전 구성"), reporterAgentName)
}

// seedAutoReportFindingBinding adds "auto" to report_finding's binding ONCE so
// conversation-context agents can call it without requiring an intent_id.
func (s *Server) seedAutoReportFindingBinding() {
	const flag = "auto_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("auto", []string{"report_finding"}); err != nil {
		log.Printf("[auto] report_finding の既定の結びつけに失敗: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerDefaultBindings adds "planner" to report_finding's binding ONCE
// (guarded by a settings flag), so existing DBs — whose report_finding row was
// seeded as worker-only — also let the planner record findings. Fresh DBs already
// get it via PlannerTools(); this only backfills without overriding a user unbind.
func (s *Server) seedPlannerDefaultBindings() {
	const flag = "planner_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"report_finding"}); err != nil {
		log.Printf("[planner] report_finding の既定の結びつけに失敗: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerListAssetsBinding adds "planner" to list_assets's binding ONCE
// (guarded by a settings flag), so existing DBs — whose list_assets row was seeded
// as auto/pentest-only — also let the planner query the asset store by DSL. Fresh
// DBs already get it via PlannerTools(); this only backfills without overriding a
// user unbind.
func (s *Server) seedPlannerListAssetsBinding() {
	const flag = "planner_list_assets_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"list_assets"}); err != nil {
		log.Printf("[planner] list_assets の既定の結びつけに失敗: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedCompanyScopeRebind changes add_company_scope's default binding ONCE on
// existing DBs (guarded by a settings flag): the tool moves off worker and onto
// planner — defining a company's asset scope is a planning/main/auto concern, not
// something a worker does mid-exploration. Fresh DBs already get planner via
// PlannerTools() and lack worker via WorkerTools(); this only backfills old rows.
// One-shot + flag-guarded so a user who later re-binds worker isn't overridden.
func (s *Server) seedCompanyScopeRebind() {
	const flag = "company_scope_rebind_v1" // worker→planner の既定の結びつけの切り替え
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"add_company_scope"}); err != nil {
		log.Printf("[planner] add_company_scope の既定の結びつけに失敗: %v", err)
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("worker", "add_company_scope"); err != nil {
		log.Printf("[worker] add_company_scope の結びつけを外せなかった: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadToolsUnbind strips the read-context tools off worker's default
// binding ONCE on existing DBs (guarded by a settings flag): a worker executes one
// intent and writes back — reading facts/companies and listing all workers' traces is
// a planning/main concern, not the executor's. Fresh DBs already lack these via
// WorkerTools(); this only backfills old rows without overriding a user who
// deliberately re-binds worker. Each RemoveAgentFromTool is per-tool +
// membership-guarded, so planner/mainagent bindings of the same tool are untouched.
//
// NOTE: search_all_worker_traces / get_worker_trace / node_detail are intentionally NOT
// unbound — worker owns them for cross-work look-back + node drill-down (see WorkerTools).
// They used to be in this list back when worker lacked them; seedWorkerReadbackRebind
// repairs DBs whose old run stripped them.
func (s *Server) seedWorkerReadToolsUnbind() {
	const flag = "worker_readtools_unbind_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	for _, k := range []string{
		"list_facts", "list_companies", "list_worker_traces",
	} {
		if err := s.m.pg.RemoveAgentFromTool("worker", k); err != nil {
			log.Printf("[worker] %s を worker から外せなかった: %v", k, err)
			return // エラーなら flag を保存せず、次の起動でやり直す
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadbackRebind re-binds the cross-work look-back / drill-down tools onto
// worker ONCE (guarded by a settings flag): an earlier seedWorkerReadToolsUnbind wrongly
// stripped search_all_worker_traces / get_worker_trace / node_detail from worker after
// they had been added to WorkerTools(), so any DB that ran that migration lost them.
// Fresh DBs already have them via WorkerTools() and this is a harmless no-op there.
// One-shot + flag-guarded so a user who later deliberately unbinds them isn't overridden.
func (s *Server) seedWorkerReadbackRebind() {
	const flag = "worker_readback_rebind_v2" // v2: node_detail を追加
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("worker", []string{
		"search_all_worker_traces", "get_worker_trace", "node_detail",
	}); err != nil {
		log.Printf("[worker] 振り返り/詳細のツールの結びつけを補えなかった: %v", err)
		return // エラーなら flag を保存せず、次の起動でやり直す
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedAutoDefaultBindings adds "auto" to the task-op + platform tools' bindings
// ONCE (guarded by a settings flag), so existing DBs whose tool rows were seeded
// before Auto existed still give Auto its default toolset — without re-adding it
// after a user deliberately unbinds.
func (s *Server) seedAutoDefaultBindings() {
	const flag = "auto_default_bindings_v3" // v3: 古い資産のツール名を置き換え、insert_assets/add_company_scope を加える
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	keys := make([]string, 0, len(platformToolKeys)+12)
	for _, t := range s.orchestrationTools() {
		keys = append(keys, t.Name())
	}
	keys = append(keys, platformToolKeys...)
	// 資産のツール：Auto がプラットフォームを操作するとき、資産を見る/登録する、会社の範囲を管理することがよくある。
	keys = append(keys, "insert_assets", "add_company_scope", "list_assets")
	if err := s.m.pg.AddAgentToToolBinding("auto", keys); err != nil {
		log.Printf("[auto] 既定の結びつけに失敗: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}
