package agent

import (
	"context"
	"fmt"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// MainAgent is the thin human-interface orchestrator (docs §4.2 / §7). The human
// chats with it; it observes (read tools), and steers by injecting hints
// (→planner) or direct high-priority intents (→frontier). It does NOT run the
// autonomous intent-generation loop (that is the planner's job).
type MainAgent struct {
	findingRecorder FindingRecorder
	prov            llm.Provider
	model           string
	tx              *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window          int                                    // context window in tokens (for compaction)
	windowFn        func() int                             // optional dynamic task-chain minimum
	maxTurns        int                                    // max agent turns per run (0 = unlimited)
	proxyAddr       string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert     string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch       WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir         string                                 // shared work dir (surfaced in prompt as artifact-output target)
	steerWork       func(intentID int64, msg string) error // engine callback: steer a running work (nil = off)
	nonStreamingFn  func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn    func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn     func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
}

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (m *MainAgent) SetNoaEnabled(fn func() bool) { m.noaEnabledFn = fn }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (m *MainAgent) SetNonStreaming(fn func() bool) { m.nonStreamingFn = fn }

func (m *MainAgent) nonStreaming() bool { return m.nonStreamingFn != nil && m.nonStreamingFn() }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (m *MainAgent) SetMaxTokens(fn func() int) { m.maxTokensFn = fn }

func (m *MainAgent) maxTokens() int {
	if m.maxTokensFn == nil {
		return 0
	}
	return m.maxTokensFn()
}

func NewMainAgent(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *MainAgent {
	return &MainAgent{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns}
}

func (m *MainAgent) SetCompactionWindowResolver(fn func() int) { m.windowFn = fn }

func (m *MainAgent) compactionWindow() int {
	if m.windowFn != nil {
		return m.windowFn()
	}
	return m.window
}

// SetProxy points the main agent's WebFetch at the recording proxy plus the CA
// cert it trusts to verify HTTPS through it (empty addr = direct).
func (m *MainAgent) SetProxy(addr, caCert string) { m.proxyAddr, m.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the main agent (off by default).
func (m *MainAgent) SetWebSearch(o WebSearchOpts) { m.webSearch = o }

// SetSteerWork wires the engine callback that lets the main agent's steer_work
// tool inject a mid-run course-correction into a running work (nil = tool off).
func (m *MainAgent) SetSteerWork(fn func(intentID int64, msg string) error) { m.steerWork = fn }

// mainAgentDefaultTmpl is the built-in EDITABLE body (段 [A]) of the main agent
// prompt, seeded into agent_prompts. Goal is a {{.Goal}} template var; the 中間
// 成果物の出力規約 tail is code-owned (artifactSpec), appended after rendering.
const mainAgentDefaultTmpl = `あなたは権限を得たペネトレーションテストシステムの「メイン agent」であり、人間の操作者との窓口である。自分で探索はせず、自律的に意図を生成し続けることもしない（それは計画者の仕事）。あなたの役割：

1. 観察：graph_overview / list_findings / list_facts / list_assets / get_worker_output を使い、現在の進み具合について人の質問に答える。
2. 舵取り（人の意図をシステムに落とし込む）：
   - 人が「方向を変えたい/ある種類の脆弱性を強調したい/ある領域を重点にしたい」 → add_hint でヒントを書く（計画者が次回読む）。
   - 人が「ある具体的な対象をすぐにテストしたい」 → add_intent で優先度の高い意図を直接1本注入する（priority 8-10）。システムは完了したタスクを自動で実行中の状態に戻し、worker にこの意図を実行させ、走り終えたら完了の状態に戻る。
     **タスクの目標がすべて達成されているとき**（graph_overview の goals がすべて met）：出す前に、その意図の背後に「新しい、達成すべき結果」が隠れていないかを判断する。隠れていれば、推測した目標を1文で人に言い直し、**正式な目標として登録するかを問い返す**——人が望む → set_goals で登録する（タスクはこのあと通常の計画に入り、計画者が自律的に先へ進める）。人が望まない / 一時的に少し探りたいだけ → add_intent でこの1本だけを出し、worker が実行し終えたらタスクは完了の状態に戻る（自律的には続けない）。その意図が明らかに1回きりの確認で、新しい目標を含まないなら、そのまま add_intent すればよく、毎回問う必要はない。
   - 人が「実行中のある意図(work)をリアルタイムで軌道修正したい（もう X を進まない、Y に集中する）」 → steer_work を使う（中断せず、既存の進み具合も失わず、worker の次の動作の前に効く）。まず get_worker_output で何をしているかを見る。方向がまるごと誤っていれば add_intent で新しい意図を別に出す。
   - 人が「達成すべき最終目標を1つ追加したい」 → set_goals で目標を補う。システムはその目標をタスクのグラフに書き込み、**完了/一時停止したタスクを自動で実行中の状態に戻して走らせ続ける**（計画者がこのあとそれをもとに達成したかを判断し直す）ので、人が改めて再開を押す必要はない。
   - 人が「テストの制約を追加/変更したい（ある種類の操作を許す/禁じる。たとえば『現在のポートだけをテストする』『ブルートフォース禁止』『受動的な偵察だけを行う』）」 → set_constraints で登録する（type=allow で許可 / type=deny で禁止）。制約は次の計画のラウンドで planner/worker のプロンプトに注入され、探索の境界を枠づける。概要の「制約の管理」で追加・削除・変更することもできる。
3. わかりやすい言葉で簡潔に返し、何をしたかを説明する。

現在のタスクの目標：{{.Goal}}

発見を作り上げない。ツールが返した本物のデータだけをもとに答える。`

func mainAgentSystem(goal, dataDir, workDir string) string {
	body := renderSystem("mainagent", mainAgentDefaultTmpl, MainVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir) + langDirective(resolveOutputLang())
}

// Chat handles one human message and returns the assistant reply. emit, if
// non-nil, receives each execution step (thinking / tool_use / tool_result /
// text / result) so the main-agent session shows its work — exactly like the
// worker/planner sessions — not just the final answer.
func (m *MainAgent) Chat(ctx context.Context, taskID int64, mainSeg int, as *db.AssetStore, ts *db.ExplorationStore, goal, message string, emit func(db.Activity), notify, resume func(), notifyGoal, notifyHint func([]string)) (string, error) {
	tsx := NewToolSet(ts, "human")
	tsx.SetFindingRecorder(m.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.SetNotify(notify)         // 汎用の起動（専用のコールバックがない書き込み操作はこれを通る。debounced）
	tsx.SetResumeTask(resume)     // set_goals で目標を追加 → 完了/一時停止したタスクを running に戻す
	tsx.SetNotifyGoal(notifyGoal) // set_goals で目標を追加 → planner に「人が目標を追加した：…」の起動を1件記録する
	tsx.SetNotifyHint(notifyHint) // add_hint でヒントを追加 → planner に「人が戦略上のヒントを N 件追加した：…」の起動を1件記録する
	tsx.steerWork = m.steerWork   // enable steer_work tool (nil = unavailable)
	// 領域のツール + 基本の既定のツール群（Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash）
	// 資産カバレッジの機能がオフなら add_task_scope/list_untested_assets を外す（prompt に入れない）。
	base := append(tsx.DropCoverageTools(tsx.MainAgentTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "mainagent", base)
	defer cleanup()
	// このタスクの作業ディレクトリ <workDir>/tasks/<taskID> を先に作っておく。
	mainDir := ensureRunDir(m.workDir, taskID, 0)
	ctx = intercept.WithReviewWorkingDirectory(ctx, mainDir)
	system, boundary := deferredSystem(mainAgentSystem(goal, m.workDir, mainDir), def)
	opts := agentcore.Options{
		Provider:        m.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // 記録プロキシを通して痕跡を残す。プロキシの CA を読み込んで、MITM で署名し直した HTTPS の証明書を検証する
		WebFetchProxy:   m.proxyAddr,
		WebFetchCACert:  m.proxyCACert,
		// Web 検索(任意)。ddgs は key 不要。brave-free は BraveKey、tavily は TavilyKey が必要。
		// WebSearchProxy は独立した出口プロキシ(http/https/socks5)で、通信を記録する MITM プロキシとは関係ない。空なら直接つなぐ。
		EnableWebSearch:       m.webSearch.Enabled,
		WebSearchBackend:      m.webSearch.Backend,
		BraveSearchAPIKey:     m.webSearch.BraveKey,
		TavilySearchAPIKey:    m.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: m.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  m.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   m.webSearch.DeepSeekModel,
		WebSearchProxy:        m.webSearch.Proxy,
		BashEnv:               proxyEnv(m.proxyAddr, m.proxyCACert), // Bash の子コマンドは既定でプロキシを通り、CA を信頼する
		WorkingDir:            mainDir,                              // このタスクの作業ディレクトリ <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(mainDir),
		MaxTurns:              m.maxTurns,                             // 0 = unlimited (configurable in agent management)
		Compaction:            compactionConfig(m.compactionWindow()), // long chats stay within the window
		Todos:                 actool.NewTodoStore(),                  // セッション単位の一時的な ToDo（TodoWrite）。計画専用で、終了したら捨てる
		// 予算(手数)に当たったら → SDK が締めくくりを走らせる:ユーザーに進み具合のまとめを1文出す。Prompt と締めくくりのラウンド数は管理画面で編集できる(既定 10 ラウンド)。
		Settlement:   wrapupSettlement("mainagent", nil),
		NonStreaming: m.nonStreaming(), // その profile で非ストリーミングを選んでいれば Provider.Complete を通る
		MaxTokens:    m.maxTokens(),    // 0 = 上限を送らず、サーバーの既定値に任せる
	}
	if m.tx != nil { // persist raw human↔AI conversation; one accumulating file per segment
		opts.Transcript = m.tx
		// Segment 0 keeps the legacy "exp%d-main" name so existing transcripts still
		// load; each new session (seg>=1) gets its own file for a clean context.
		opts.SessionID = fmt.Sprintf("exp%d-main", ts.ID())
		if mainSeg > 0 {
			opts.SessionID = fmt.Sprintf("exp%d-main-s%d", ts.ID(), mainSeg)
		}
	}
	// 実験的な機能:有効にすると noa がコンテキストの圧縮を引き受ける(アーカイブは <workDir>/noa/<SessionID> の下にまとめて永続化)。
	// session id は transcript と同じ規則で(区切りを意識して)、アーカイブと復元を揃える。
	noaSession := fmt.Sprintf("exp%d-main", ts.ID())
	if mainSeg > 0 {
		noaSession = fmt.Sprintf("exp%d-main-s%d", ts.ID(), mainSeg)
	}
	enableNoa(&opts, m.noaEnabledFn, m.workDir, noaSession, noaWarn(noaSession))
	ctx = attachSideCapture(ctx, &opts)
	s := agentcore.NewSession(opts)
	defer s.Close()
	// reload the prior conversation from the transcript so the agent has context
	// across turns (each Chat is a fresh session; without this it can't see earlier
	// messages). First turn: no file yet → Resume loads nothing and proceeds.
	if m.tx != nil {
		_ = s.Resume(opts.SessionID)
	}
	// C2: this session is fresh each turn; re-unlock skill-gated MCPs from prior
	// Skill() calls in the reloaded history so revealed tools stay callable.
	seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
	text, _, err := captureRunSession(ctx, s, message, func(r db.Activity) {
		if emit != nil {
			r.Worker = "mainagent"
			emit(r)
		}
	})
	return text, err
}
