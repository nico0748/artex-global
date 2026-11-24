package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Planner is the event-driven LLM planner (docs §4.3): each time the asset or
// exploration graph changes (debounced), it reads the exploration route, queries
// assets, judges whether the task goal is met, and emits 0..N exploration intents
// into the frontier. It is the sole intent generator.
type Planner struct {
	findingRecorder   FindingRecorder
	prov              llm.Provider
	model             string
	tx                *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window            int                                    // context window in tokens (for compaction)
	windowFn          func() int                             // optional dynamic task-chain minimum
	maxTurns          int                                    // max agent turns per run (0 = unlimited)
	killWork          func(intentID int64) error             // engine callback to terminate a running work (nil = off)
	steerWork         func(intentID int64, msg string) error // engine callback to steer a running work mid-run (nil = off)
	proxyAddr         string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert       string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch         WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir           string                                 // shared work dir (surfaced in prompt as artifact-output target)
	injectConstraints func() bool                            // resolver: inject task operation constraints into system prompt? (nil = yes)
	nonStreamingFn    func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn      func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn       func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
	compactor         *Compactor                             // cold-node compaction (§7); nil = disabled

	// todos keeps ONE plan-scratchpad per task (keyed by exploration id) so the
	// planner's multi-step plan survives across wake-ups — each Plan() is a fresh
	// session, but the shared store lets it record a serial exploit chain once and
	// dispatch it step-by-step over rounds instead of front-loading it in parallel.
	todoMu sync.Mutex
	todos  map[int64]*actool.TodoStore
}

func NewPlanner(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *Planner {
	return &Planner{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, todos: map[int64]*actool.TodoStore{}}
}

func (p *Planner) SetCompactionWindowResolver(fn func() int) { p.windowFn = fn }

// SetCompactor wires the cold-node compactor (cold-digest §7). Called each
// planner wake-up to advance the round counter, maintain cold stamps, and
// (off the hot path) fold cold nodes into digests. nil = feature disabled.
func (p *Planner) SetCompactor(c *Compactor) { p.compactor = c }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (p *Planner) SetNonStreaming(fn func() bool) { p.nonStreamingFn = fn }

func (p *Planner) nonStreaming() bool { return p.nonStreamingFn != nil && p.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (p *Planner) SetNoaEnabled(fn func() bool) { p.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (p *Planner) SetMaxTokens(fn func() int) { p.maxTokensFn = fn }

func (p *Planner) maxTokens() int {
	if p.maxTokensFn == nil {
		return 0
	}
	return p.maxTokensFn()
}

func (p *Planner) compactionWindow() int {
	if p.windowFn != nil {
		return p.windowFn()
	}
	return p.window
}

// SetProxy points the planner's WebFetch at the recording proxy plus the CA cert
// it trusts to verify HTTPS through it (empty addr = direct).
func (p *Planner) SetProxy(addr, caCert string) { p.proxyAddr, p.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the planner (off by default).
func (p *Planner) SetWebSearch(o WebSearchOpts) { p.webSearch = o }

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the planner system prompt. Read per round so the
// settings toggle takes effect without rebuilding the agent. nil = inject (default).
func (p *Planner) SetConstraintInject(fn func() bool) { p.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (p *Planner) wantConstraints() bool { return p.injectConstraints == nil || p.injectConstraints() }

// todoFor returns the task's persistent planning todo store, creating it on first
// use. Shared across all of this task's planner wake-ups.
func (p *Planner) todoFor(expID int64) *actool.TodoStore {
	p.todoMu.Lock()
	defer p.todoMu.Unlock()
	s := p.todos[expID]
	if s == nil {
		s = actool.NewTodoStore()
		p.todos[expID] = s
	}
	return s
}

// SetKillWork wires the engine's per-work terminate callback so the planner's
// kill_work tool can stop a single running worker.
func (p *Planner) SetKillWork(fn func(intentID int64) error) { p.killWork = fn }

// SetSteerWork wires the engine's per-work steering callback so the planner's
// steer_work tool can inject a mid-run course-correction into a running worker.
func (p *Planner) SetSteerWork(fn func(intentID int64, msg string) error) { p.steerWork = fn }

// renderPlannerTodos formats the persistent planning todo for injection into the
// wake-up prompt (empty when there are no todos yet — first wake-up).
func renderPlannerTodos(items []actool.Todo) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【あなたの計画の ToDo（起動をまたいで保持される。前のラウンドであなたが書いたもの）】：\n")
	for _, it := range items {
		mark := map[actool.TodoStatus]string{actool.TodoPending: "☐", actool.TodoInProgress: "▶", actool.TodoCompleted: "✔"}[it.Status]
		if mark == "" {
			mark = "☐"
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", mark, it.Content))
	}
	b.WriteString("これをもとに進める：【前の段階が完了している / 依存する fact がすでに存在する】次の段階にだけ意図を出す。TodoWrite で一覧を更新する（fact で満たされた段階は completed にする）。一覧にすでに pending/in_progress としてある段階を重ねて出さない。")
	return b.String()
}

// TriggerEvent describes what concretely caused this planning round to fire, so
// the planner looks first at the actual change instead of re-scanning the whole
// overview. Kind:
//
//	"done"    — a worker finished intent IntentID (its output conclusion is fetched).
//	"finding" — a worker reported a finding on intent IntentID (Detail = 要約).
//	"goal"    — the human (via メイン agent の set_goals) added one OR MORE goals in a
//	            single call (Goals = 今回新たに加わった目標の文、1件以上。set_goals は一括に対応).
//	"goal_deleted" — the human deleted a goal from 概要の目標管理 (Detail = 削除された目標の文).
//	"goal_edited"  — the human edited a goal from 概要の目標管理 (OldGoal→NewGoal の文).
//	"cancelled" — the human deleted intent IntentID (Detail = 削除の理由). The intent is
//	            stopped (not deleted) and the reason is attached to it as a fact.
type TriggerEvent struct {
	Kind     string
	IntentID int64
	Detail   string
	Summary  string   // Kind=="cancelled" 専用：削除前に取得した意図の要約（本当に削除したあとはノードがもう存在せず、調べられない）
	Goals    []string // Kind=="goal" 専用：今回の set_goals で加わった目標の文（1件または複数）
	OldGoal  string   // Kind=="goal_edited" 専用：変更前の目標の文
	NewGoal  string   // Kind=="goal_edited" 専用：変更後の目標の文
	Hints    []string // Kind=="hint" 専用：今回の add_hint で加わったヒントの文（1件または複数）
}

// renderTriggers spells out the change(s) that fired this round: for a finished
// worker — which intent + its output conclusion; for a finding — which intent +
// what was found. Empty for time/heartbeat wakes. Reads the store (best-effort;
// a blank field never blocks the round).
func renderTriggers(ts *db.ExplorationStore, evs []TriggerEvent) string {
	if len(evs) == 0 || ts == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【今回このラウンドを起動した実際の変化（まずここを見てから、方向を足すかを決める）】：")
	for _, ev := range evs {
		switch ev.Kind {
		case "goal":
			if len(ev.Goals) == 1 {
				b.WriteString(fmt.Sprintf("\n- 人（メイン agent）が目標を1つ追加した：%s —— 新たに達成すべき目標なので、これをもとに探索の方向を足すこと（対応する意図がまだなければ）。", ev.Goals[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- 人（メイン agent）が目標を %d 件追加した：%s —— いずれも新たに達成すべき目標なので、対応する意図がまだない目標ごとに探索の方向を足すこと。", len(ev.Goals), strings.Join(ev.Goals, "；")))
			}
		case "hint":
			if len(ev.Hints) == 1 {
				b.WriteString(fmt.Sprintf("\n- 人（メイン agent）が戦略上のヒントを1件追加した：%s —— すでに探索グラフに付けてあるので、これをもとに探索の方向を調整/追加すること（対応する意図がまだなければ）。", ev.Hints[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- 人（メイン agent）が戦略上のヒントを %d 件追加した：%s —— いずれもすでに探索グラフに付けてあるので、それぞれをもとに探索の方向を調整/追加すること。", len(ev.Hints), strings.Join(ev.Hints, "；")))
			}
		case "goal_deleted":
			b.WriteString(fmt.Sprintf("\n- 人がこの目標を削除した：%s —— この目標は取り除かれたので、これをもとに残りの目標/方向を判断し直すこと（この目標のために意図を出す必要はもうない）。", ev.Detail))
		case "goal_edited":
			b.WriteString(fmt.Sprintf("\n- 人が目標を「%s」から「%s」に変更した —— 新しい目標に合わせて探索の方向を調整すること（元の方向がもう当てはまらなければ出すのをやめる）。", ev.OldGoal, ev.NewGoal))
		case "finding":
			b.WriteString(fmt.Sprintf("\n- 意図 #%d（%s）の worker が finding を1件報告した：%s", ev.IntentID, intentSummary(ts, ev.IntentID), ev.Detail))
		case "cancelled":
			// 意図の内容は削除時に取得した Summary を優先する（本当に削除したあとはノードがもう存在せず、intentSummary では調べられない）。
			sm := ev.Summary
			if sm == "" {
				sm = intentSummary(ts, ev.IntentID)
			}
			b.WriteString(fmt.Sprintf("\n- 意図 #%d はユーザーが削除した。意図の内容は：%s、削除の理由は：%s。この意図は削除済み（もう実行しない）なので、これをもとに計画し直すこと。", ev.IntentID, sm, ev.Detail))
		default: // "done"
			b.WriteString(fmt.Sprintf("\n- 意図 #%d（%s）の worker が終了し、結論を出力した：%s", ev.IntentID, intentSummary(ts, ev.IntentID), workerOutput(ts, ev.IntentID)))
			if fids := factIDsYielded(ts, ev.IntentID); fids != "" {
				b.WriteString(fmt.Sprintf("。この意図が新たに生んだ事実の id：%s ", fids))
			}
		}
	}
	b.WriteString("\n（完全な細部は node_detail / get_worker_output / list_findings で改めて調べられる。）")
	return b.String()
}

// factIDsYielded lists the fact ids an intent produced this run as "#12、#15", so the
// planner can jump straight to the round's incremental facts. Empty (best-effort) when
// the intent yielded no facts or the lookup fails.
func factIDsYielded(ts *db.ExplorationStore, id int64) string {
	ids, err := ts.FactsYielded(id)
	if err != nil || len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, fid := range ids {
		parts[i] = fmt.Sprintf("#%d", fid)
	}
	return strings.Join(parts, "、")
}

// intentSummary reads an intent node's one-line summary (best-effort, "?" on miss).
func intentSummary(ts *db.ExplorationStore, id int64) string {
	n, err := ts.GetNode(id)
	if err != nil || n == nil {
		return "?"
	}
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if s, ok := p["summary"].(string); ok && s != "" {
			return s
		}
	}
	return "?"
}

// workerOutput returns the finished worker's conclusion for an intent — the last
// 'result' (else 'text') activity's full detail, truncated. Same source get_worker_output uses.
func workerOutput(ts *db.ExplorationStore, id int64) string {
	acts, _, err := ts.ActivityList(&id, 0, 1000)
	if err != nil {
		return "(出力の取得に失敗)"
	}
	var pick *db.Activity
	for i := range acts {
		if acts[i].Kind == "result" {
			pick = &acts[i]
		} else if acts[i].Kind == "text" && pick == nil {
			pick = &acts[i]
		}
	}
	if pick == nil {
		return "(このワークにはまだ出力の記録がない)"
	}
	out, _ := ts.ActivityDetail(pick.ID)
	if out == "" {
		out = pick.Summary
	}
	return truncOutput(out, 800)
}

// truncOutput caps a worker-output blob so the trigger context doesn't bloat the
// system prompt every round; full text is one get_worker_output call away.
func truncOutput(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " …（切り詰め済み。全文は get_worker_output を参照）"
}

// renderGraphOverview folds the pre-computed graph_overview snapshot into the
// wake-up prompt so the planner starts each round with the full situation in
// hand — saving the round-trip it would otherwise spend calling the tool. It is
// the exact same JSON graph_overview would return; deeper detail is still one
// tool call away (node_detail / list_facts / …).
func renderGraphOverview(data map[string]any) string {
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back to the model calling graph_overview itself
	}
	return "\n\n【このラウンドの状況（graph_overview で事前に取得したもので、あなたがそのツールを呼んだ戻り値と同じ。細部が必要なら node_detail/list_facts などを必要に応じて呼ぶ）】：\n" + string(b)
}

// plannerDefaultTmpl is the built-in EDITABLE body (段 [A]) of the planner prompt,
// seeded into agent_prompts. Goal is a {{.Goal}} template var; the 中間成果物の出力規約
// tail is code-owned (artifactSpec) and appended by plannerSystem after rendering.
const plannerDefaultTmpl = `あなたはサイバーセキュリティプラットフォームの、権限を得たペネトレーションテストシステムにおける「計画者」であり、頻繁に起動される（グラフが変わるたびに起動される）。役割：状況を読む → 目標を判定する → **まだ扱われていない新しい方向が本当にあるときだけ**探索の意図を足す。あなたは計画者であって実行者ではない：このラウンドの成果物は【意図を生成する/はっきり述べる】か【目標を判定する】ことだけで、plan の中で作業そのものをやってしまっては決してならない。

タスクの目標：{{.Goal}}

**このラウンドで意図をいくつ出すか（まずこれをよく考える）**：
- **絶対の下限（最優先）**：【目標が未達成】で、かつ【いま open や running の意図が1つもない】（frontier_open=0 で running_intents が空）なら、このラウンドでは目標に向けて進む意図を少なくとも1つ【必ず】出す——待つべき実行中のワークも、順番待ちの方向もないときに意図を 0 個出す=タスクが止まる。既知の方向がすべて recent_done にしかなくても、下の done/exhausted/blocked の判断に基づいて別の1本を開くか、1本を続けて出すこと。
- 絶対の下限以外では、**意図を 0 個出すのは正常な結果だが、正当な理由が必要**（「少なく出すほうが安全」という既定ではない）：①**扱い済み**——思いついた方向がどれも、まだ open/running の意図で処理されている（言い回しを変えて既存の意図を重ねて生成するのは重大な誤り）。②**依存を待っている**——次の段階が実行中のワークの産出物に依存しており、それがまだ出ていない（ここで無理に出すと下流が前提を得られず空回りするので、次の起動でグラフが更新されてから出すべき）。
- 逆に：【まだ扱われておらず、実行中のワークに依存しない】新しい方向が本当にある、または目標が未達成で範囲内にまだ試していない面があるなら、出すべきだ——意図 0 個を怠けるための既定にしてはならない。

**起動ごとの判断の流れ**：

1. **完全な状況はこのプロンプトの下に添付してある**（graph_overview の戻り値そのもので、改めて呼ぶ必要はない）：task（元のタイトル+目標/ルートノード）、資産の数、goals+状態、open/running/recent_done の意図、sites_without_endpoints（エンドポイントのないサイト。探るべき方向を示唆しうる）、facts（探索の事実の数。脆弱性とは別物）、recent_facts（{id,summary,confidence?}）。
   - **範囲**：探索ノード（goals/意図/facts/findings）はこのタスクのものだけを含む。**資産グラフは全体で共有される**（複数のタスクで同じものを使い、資産の数は全体のうち範囲内のもので、このタスク専有ではない）——このタスクに関係のない資産が出てきたら無視する。
   - **系譜**：各意図には parents（上流：どの事実/意図から派生したか）と yields（下流：どの事実/発見を生んだか）があり、recent_facts の各項目には from_intent がある。これをもとに「どの事実がどの方向から来たか、組み合わせて新しい方向を出せるか」を理解する。
   - **否定/疑わしい観察**（recent_facts の「ポートが閉じている/インジェクションできない」など）は worker の観察であって確定ではない：採用する前に node_detail(id) で evidence を見る——evidence がしっかりしていて、confidence=observed で、手段をやり尽くしたものだけを、その方向が当面閉じたとみなす。evidence がない、「そう見える/1回探っただけ」、または confidence=inferred のものは【まだ明らかでない】として扱い、範囲内で他の意図に扱われていなければ、既定で確かめるか覆すための再確認の意図を1本出す（**同じ否定の方向の再確認は多くても1回**。再確認後も否定で、証拠が妥当なら、その結論を尊重してもう出さない）。
   - **もっと深い細部が必要なときだけ呼ぶ**：list_facts（ページ分け、新しい順、既定 20、q で絞り込み、before でページ送り、total/has_more 付き）、list_findings（すべての脆弱性）、node_detail(id)（完全な証拠/詳細。一覧/recent_facts は要約だけ）、list_assets（pull：q で検索、type/company_id/task_id で絞り込み、ページ分け、または id/ids で直接取得）、asset_neighbors。資産は全体で共有されるので、既定で全件を取ってこない。

2. **目標を判定する（中心となる役割）**：goals フィールドに目標と状態が入っている。ある発見/事実で証明された未達成の目標には、prove_goal(goal_id, evidence_id, reason) を呼んで met にする。**あなたが印を付けたのが最後の未完了の目標なら、システムは自動的にタスク全体を完了と判定する**——締めくくりは1つずつの prove_goal によってだけ進み、ほかに「ワンクリックで完了」する手段はない。
   - ⚠️ **数値による受け入れの照合（早まって承認することは厳禁）**：目標に数値で表せる条件（カバレッジ X% 到達、flag を N 個取得、ある権限の獲得）があるときは、prove_goal の前に上の graph_overview の実測値（coverage.pct、findings_total の数など）を【必ず】照合する：基準に達していなければ prove_goal は【禁止】し、差を埋める意図を出す。「おおむね達成/核心は押さえた」を理由に早まって met にしてはならない。例：カバレッジ 100% が求められていて実測 coverage.pct=40% → 未達成なので、補う検査の意図を出し続ける。

3. **（任意、開始時だけ、ごく軽く）探って理解する**：グラフにまだ fact がほとんどなく（recent_facts がほぼ空で、タスクが始まったばかり）、状況だけでは最初の意図を具体的に述べられないときに限り、Bash などで対象をごく少量、読み取り専用で探る（1–2 回の curl でトップページ/フィンガープリントを見るなど）。**唯一の正当な成果物はより的確な意図の説明1文**であって——脆弱性の発見/検証/悪用でも、エンドポイント/ディレクトリ/引数の列挙結果でも決してない（それらは worker の仕事なので、意図として書いて出す）。3つの厳格な境界：
   - グラフにすでに worker が生んだ fact がある（facts>0 / recent_facts が空でない）→ 自分で探ることは【禁止】。すべての判断を既存の fact に基づかせ、このラウンドの成果物は「新しい意図を出す」か「終わる」だけ。ある手がかりを深掘りしたい → 自分で curl せず、意図を出して worker に調べさせる。
   - 開始時でも探るのは多くて ≤3 回で手を引き、最初の意図をはっきりさせるためだけに使う。「素早く方向を定める」でなく「深く調べる」ことをしていると気づいたら（エンドポイント/ディレクトリを1つずつ列挙する、id を1つずつ試す、デコードの連鎖、同じ API を繰り返し探る、インジェクション/権限昇格/脆弱性のあらゆるテストと検証——どれも worker の重い仕事）、すぐに手を止めて意図として書く。
   - 既存の事実/状況から判断できることは、そもそも探る必要がない。

4. **どの新しい方向を足すかを決める**：**ここでの「抑制」は【既存の意図を重ねない】ことだけを指し、「できるだけ少なく出す」ことではない**——目標が未達成のとき、既定で問うべきは「目標に迫るために、まだ扱われていない、より深く、より鋭い攻め方は何があるか」であって、「締めくくってよいか」ではない。意図は【開かれた探索の方向】であり（固定の種類/メニューではない）、既知の事実、資産、目標を組み合わせて自分で方向を判断し、open + running + recent_done と1つずつ照らし合わせる：
   - 既存の open/running で扱われている → もう生成しない（処理中）。
   - recent_done に出てきたことがある → **まずその意図の state（どれにも付いている）でどう止まったかを見分けてから決める**：
     · **done（正常に走り終えた）**：扱い済み → そのまま出し直さない。行き止まりかどうかは state でなく、それが yields した fact の結論で判断する。【実質的に新しい仕組み】（新しい事実/資産/引数/明らかに異なる攻め方）が現れたときだけ出し直し、summary に前回との違いをはっきり書く。言い回しを変える、「もう一度試せばうまくいくかも」は数えず、再試行は禁止する。
     · **exhausted（予算を使い切り、探る途中で打ち切られ、一部だけ書き戻した）/ blocked（モデルやネットワークの失敗で、ほとんど探れていない）**：どちらも途中でうまく終われず情報が不完全——まず get_worker_trace / get_worker_output で実際にどこまでやり、どこで詰まったかを見てから、次から選ぶ：突破に近かったのに予算で打ち切られた → 「前回の進み具合を引き継いで続ける」を出す。純粋に外部の故障で走れなかった（blocked によくある）→ 同じ方向をそのまま出し直す。毎回同じところで詰まる → 攻め方/方向を変える。根拠は常に trace にある本当の進み具合であって、state そのものではない。
   - どの意図にもまったく扱われていない新しい方向 → 生成する。
   - 既知の方向がすべて、まだ open/running の意図で扱われている → 生成せず、そのまま終わる（実行中/順番待ちのワークがあるので、それらが進むのを待つ）。ただし recent_done で扱われたものしか残っておらず、open/running がもうなく、目標が未達成なら → 冒頭の絶対の下限に従って、別の1本を開くか続けて出さなければならない。
   - **カバレッジより深さを優先する**：coverage は下限/受け入れ項目であって探索の目的そのものではない。価値の高い入口（RCE/権限昇格/データの持ち出しにつながりうる）を見つけたら、カバレッジをならすために広く浅く資産を1つずつ試すのでなく、その道を【深く打ち抜く】意図を優先して出す。
   - **経路の多様さを保ち、早まって収束しない**：目標が未達成のとき、既存の意図がすべて同じ経路/入口に集まっていて、【本質的に異なる】まだ扱われていない方向（別の入口の面/別の種類の資産/別の悪用の連鎖）があるなら、同じ線上に同義の意図を足すのでなく、その分かれた方向を優先して足す（言い回しでなく実質の違いを見る）。その分かれた方向がすでに既存の意図で扱われていれば、やはり生成しない。理想は仕組みの異なる 2–3 本の経路が並存すること（たとえば「アップロードの連鎖から攻める」と「認証のすり抜けから攻める」）で、ある経路が【目標に迫る】証拠を出してから資源をそこに集中する。**ただし多様さは常に冒頭の【操作の制約】に従う**：制約で除外された入口の面/ポート/ホスト/操作は、本質的に異なっていても決して意図を生成しない。

   **直列の悪用の連鎖：段階ごとに出し、並列に分けない。** 強く依存する直列の連鎖（①→②→③、後の段階が前の段階の実際の産出物に依存する）：一度に並列に出さない（下流はまだ存在しない前提を得られず、重複/空回りするだけ）。TodoWrite で連鎖全体を ToDo として記録し（1段階につき1項目）、このラウンドでは「前提がそろった」段階（通常は最初の段階）だけを出し、それが fact を生んだら次の起動で（プロンプトに ToDo の一覧が付く）次の段階を出し、満たされたものを completed にする。「同じこと」を2本に分けない（「起動点を確かめる」と「起動点を起動する」は同じ段階）。【並行し、互いに依存しない】次元（関係のない複数のエンドポイントを列挙するなど）だけを複数の意図で並列にする。

5. **提出する**：【1回の】add_intent で選んだ新しい方向をまとめて提出する（intents の配列、最も価値の高いものを最大4つ。1件ずつ何度も呼ばない）：
   - **summary**：その方向を自然な言葉で1文で説明する（テスト対象の完全なアドレス + 何をするか + なぜか）。固定の分類に当てはめない。重複の排除は主にこれと既存の意図の照合で行う。
   - **asset_ids**：この方向でテスト/攻撃する対象の資産の id（できるだけ渡す。0/1/複数、list_assets から）——方向が具体的な資産（サイト/API/引数/ホスト）を中心にしているなら必ず渡す。カバレッジの重複排除と資産の経路へのつなぎに使い、複数の資産にまたがるならすべて渡す。具体的な資産のない純粋に全体的な偵察のときだけ空にする。
   - **parent_ids**：この方向がどの上流のノードを組み合わせて得られたか（任意、0/1/複数）——複数の事実を組み合わせて1つの意図が生まれたならすべて渡し、ある上流の意図/発見から派生したならその id も渡す。最上位の新しい方向なら空にする。

重ねず、無理にこしらえない。ただし目標が未達成で、まだ扱われていないより深い攻め方があるなら、出すべきときは出す。簡潔に、焦点を絞り、効率よく。`

func plannerSystem(goal, dataDir, workDir string) string {
	body := renderSystem("planner", plannerDefaultTmpl, PlannerVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir) + langDirective(resolveOutputLang())
}

// Plan runs one planning round. emit, if non-nil, receives the planner's execution
// steps (so users can see how it reads the situation and judges goals — the
// planner is the intent generator and was previously a black box). Returns whether
// the planner judged the goal met.
// triggers carries the concrete change(s) that fired this round — worker(s) done
// and/or finding(s) reported (may be several — the engine debounces a burst; empty
// for time/heartbeat wakes). They are spelled out at the top of the prompt so the
// planner looks first at the actual change (which intent, its output/finding).
func (p *Planner) Plan(ctx context.Context, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, goal string, triggers []TriggerEvent, emit func(db.Activity)) (met bool, reason string, err error) {
	// cold-digest §2.3/§7: advance this task's planner-round counter, maintain the
	// cold_since_round stamps, and (if a threshold is hit) kick off background
	// compaction. Synchronous part is cheap (a few queries); the LLM compaction
	// runs in a detached goroutine so it never adds latency to this round.
	p.compactor.OnPlannerRound(ctx, ts)
	tsx := NewToolSet(ts, "planner")
	tsx.SetFindingRecorder(p.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.killWork = p.killWork   // enable kill_work tool (nil = unavailable)
	tsx.steerWork = p.steerWork // enable steer_work tool (nil = unavailable)
	if origin, _ := ts.OriginFactID(); origin > 0 {
		tsx.SetOwnerNode(origin) // planner-side anchors default to the task root (origin fact)
	}
	// 領域のツール + 基本の既定のツール群（Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash）
	// 資産カバレッジの機能がオフなら add_task_scope/list_untested_assets を外す（prompt に入れない）。
	base := append(tsx.DropCoverageTools(tsx.PlannerTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "planner", base)
	defer cleanup()
	// 重要な状況（完了したばかりの意図 + 事前に取得した完全なグラフ）は【このラウンドの user の入力】(下の input を参照)に移し、system には
	// 静的な計画の本文だけを残す。こう移すことで system がラウンドごとに安定し、キャッシュに有利になる。代償は、1ラウンドが長くなると状況が
	// compaction で圧縮されうること（planner の1ラウンドは通常短いので、リスクは低い）。situational は下の input につなぐ。
	situational := renderTriggers(ts, triggers) + renderGraphOverview(tsx.graphOverviewData())
	// タスク単位の deadline / 終局モード(ctx で注入。taskclock.go を参照)。終局のラウンドではタスクのタイムアウト用の
	// planner の締めくくりの言葉を【このラウンドの操作の指示】としてこのラウンドの user の入力に(situational とともに)つなぎ、最後の
	// 目標の判定だけをさせ、新しい意図は出させない。
	tc := taskClockFrom(ctx)
	if tc.Final {
		situational += "\n\n【タスクの終局の締めくくり（このラウンドの特別な指示。上の通常の計画の流れを上書きする）】：" + resolveTaskTimeoutWrapup("planner")
	}
	// このタスクの作業ディレクトリ <workDir>/tasks/<taskID> を先に作っておく。
	taskDir := ensureRunDir(p.workDir, taskID, 0)
	ctx = intercept.WithReviewContext(ctx, taskDir, intercept.ReviewBackground{})
	sysBody := plannerSystem(goal, p.workDir, taskDir)
	if p.wantConstraints() {
		sysBody += constraintBlock(ts) // 操作の制約(あれば)をシステムプロンプトに注入し、探索の境界を枠づける
	}
	system, boundary := deferredSystem(sysBody, def)
	// planner には自身の実時間の予算はない。deadline があるときは MaxDuration を残り時間に挟み込み、実行中の計画のラウンドが
	// タスクの期限で締めくくりに入るようにする(タイムアウトが原因→タスクのタイムアウトの言葉、手数が原因→per-run の言葉)。
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, 0)
	settle := wrapupSettlement("planner", nil)
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("planner", nil, clamped)
	}
	opts := agentcore.Options{
		Provider:        p.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // 記録プロキシを通して痕跡を残す。プロキシの CA を読み込んで、MITM で署名し直した HTTPS の証明書を検証する
		WebFetchProxy:   p.proxyAddr,
		WebFetchCACert:  p.proxyCACert,
		// Web 検索(任意)。ddgs は key 不要。brave-free は BraveKey、tavily は TavilyKey が必要。
		// WebSearchProxy は独立した出口プロキシ(http/https/socks5)で、通信を記録する MITM プロキシとは関係ない。空なら直接つなぐ。
		EnableWebSearch:       p.webSearch.Enabled,
		WebSearchBackend:      p.webSearch.Backend,
		BraveSearchAPIKey:     p.webSearch.BraveKey,
		TavilySearchAPIKey:    p.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: p.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  p.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   p.webSearch.DeepSeekModel,
		WebSearchProxy:        p.webSearch.Proxy,
		BashEnv:               proxyEnv(p.proxyAddr, p.proxyCACert), // Bash の子コマンドは既定でプロキシを通り、CA を信頼する
		WorkingDir:            taskDir,                              // このタスクの作業ディレクトリ <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(taskDir),
		MaxTurns:              p.maxTurns, // 0 = unlimited (configurable in agent management)
		MaxDuration:           maxDur,     // 0=制限なし。deadline があるときは=deadline までの残り
		Compaction:            compactionConfig(p.compactionWindow()),
		// 起動をまたいで共有する計画の ToDo：直列の連鎖を複数のラウンドの間で保持する（session は新しいが、store はそうではない）。
		Todos: p.todoFor(ts.ID()),
		// 【このラウンドの】手数の予算に当たったら → SDK が締めくくりを走らせる：計画をやめるのでなく、このラウンドで考えがまとまった結論を記録させる(出すべき add_intent、
		// 証明できる prove_goal、直列の連鎖の TodoWrite)。planner はこのあとも繰り返し起動されるからだ。
		// clamped(タスクの deadline で挟み込まれた)ときは代わりに PromptByReason を使う(wrapupSettlementForTask を参照)。
		Settlement:   settle,
		NonStreaming: p.nonStreaming(), // その profile で非ストリーミングを選んでいれば Provider.Complete を通る
		MaxTokens:    p.maxTokens(),    // 0 = 上限を送らず、サーバーの既定値に任せる
	}
	if p.tx != nil { // persist raw LLM conversation; one accumulating file per task's planner
		opts.Transcript = p.tx
		opts.SessionID = fmt.Sprintf("exp%d-planner", ts.ID())
	}
	// 実験的な機能:有効にすると noa がコンテキストの圧縮を引き受ける(アーカイブは <workDir>/noa/<SessionID> の下にまとめて永続化)。
	noaSession := fmt.Sprintf("exp%d-planner", ts.ID())
	enableNoa(&opts, p.noaEnabledFn, p.workDir, noaSession, noaWarn(noaSession))
	// 状況（完了したばかりの意図 + 完全なグラフ）はいまはこのラウンドの user の入力につなぐ（下の input を参照）。user にはほかに
	// 指示 + 起動をまたぐ ToDo がある（todo はモデル自身の計画のメモで、作り直せるので user に置けばよい）。
	// 冒頭の言葉は「このラウンドに具体的な変化があるか」で2通りに分ける：変化あり → 下の【実際の変化】ブロックを指す。変化なし
	// (ハートビートの定期巡回 / hint / 再開など) → 「グラフが変わった」と偽らず、ついでに実行中の意図を見直すよう促す。
	lead := "具体的な変化があったばかりなので（下の【今回このラウンドを起動した実際の変化】を参照）、これをもとに次の段階を計画する："
	if len(triggers) == 0 {
		lead = "このラウンドは**定期巡回（ハートビートの時刻）/具体的な変化の合図がない**起動である——グラフに新しい変化があるとは限らない。ついでに実行中の意図を見直す：長い間進まない、または的外れになったものは steer_work で軌道を正し、方向がまるごと誤っているものは kill_work で損を止める。そのうえで目標を判定し、方向を足すかを決める："
		// ハートビート/変化なしの起動で、グラフ全体に open や running の意図がもう1つもない → 探索は止まっている(worker が走っておらず、
		// 順番待ちの方向もない)。planner にはっきり伝え、このラウンドで新しい方向を足すよう強制し、実行中の意図を見直すだけで1ラウンドを空回りさせない。
		if active, err := ts.HasActiveIntent(); err == nil && !active {
			lead = "このラウンドは**定期巡回（ハートビートの時刻）**の起動で、しかもいま**open や running の意図が1つもない**——走っている worker も、順番待ちの方向もなく、探索は止まっている。あなたはこのラウンドで、目標に向けて進み、かつグラフの既存の意図と**互いに重ならない**新しい意図を1つ以上**必ず**出すこと(意図を 0 個出してはならない)。まず下の状況から目標がもう達成されたかを判定し、未達成ならすぐに方向を足す："
		}
	}
	input := lead + situational + "\n\n上の状況をもとに目標を判定する。目標が【本当に達成された】（目標の成果を得た/目標の脆弱性を確認した）ときは prove_goal で1つずつ印を付ける。**絶対の下限：目標がまだ達成されておらず、かつ open や running の意図が1つもない（frontier_open=0 で running_intents が空）なら、このラウンドで目標に向けて進む意図を少なくとも1つ必ず出す——このとき待つべき実行中のワークも順番待ちの方向もなく、意図を 0 個出す=タスクが止まる。すでに open/running の意図が進んでいるか、目標が達成されているときだけ、このラウンドで新しい意図を出さなくてよい。**" +
		renderPlannerTodos(opts.Todos.List())
	// MaxDuration はいまや実時間の期限で実行中のツールを中断し、その場で締めくくりに入る(生きている ctx 上で)ので、1ラウンドが止まっても
	// 締めくくりをすり抜けることはなく、外部の厳格な ctx で受け止める必要はない。ctx が担うのは pause / kill / shutdown だけ。
	_, _, err = captureRun(ctx, opts, input,
		func(r db.Activity) {
			if emit != nil {
				r.Worker = "planner" // planner activity has no intent_id (it generates them)
				emit(r)
			}
		})
	return tsx.GoalMet, tsx.Reason, err
}
