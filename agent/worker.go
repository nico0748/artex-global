package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Worker is an LLM work agent (docs §4.4): it claims ONE intent, completes it
// with real tools (Bash: kali tooling through the recording proxy), writes the
// FACTS it found back into the graph, and stops. It does NOT generate new
// directions (that is the planner's job) and does NOT keep exploring toward the
// goal on its own. Multiple workers run concurrently as goroutines.
// WebSearchOpts is the web-search backend selection the server pushes into each
// agent (planner/worker/main). Enabled=false leaves the web_search tool off.
// Backend is "ddgs" (no key), "brave-free" (BraveKey required), "tavily"
// (TavilyKey required), or "deepseek" (DeepSeek* required, filled from the
// active LLM profile). It maps directly onto agentcore.Options.
// Proxy is a dedicated egress proxy for the search request (http/https/socks5),
// independent of the traffic-recording MITM proxy — set it when the search endpoint
// is only reachable via a VPN/SOCKS proxy. Empty = direct.
//
// deepseek のバックエンドは他の3つと性質が違うことに注意：DeepSeek には直接呼べる検索 API がなく、
// 検索はその Anthropic 互換の messages API の内部(web_search_20250305 server
// tool)にしか存在しない。そのため検索のたびにモデルの呼び出しを1回消費し、検索のリクエストは DeepSeek のサーバーから送られる——
// このマシンの Proxy を通らず、通信の痕跡にも残らない。
type WebSearchOpts struct {
	Enabled   bool
	Backend   string
	BraveKey  string
	TavilyKey string
	Proxy     string
	// DeepSeek* は現在有効な LLM 設定(anthropic 形式の DeepSeek 公式エンドポイントだけ)から来ており、
	// 個別には設定せず、LLM 設定の切り替えに合わせて変わる。
	DeepSeekBaseURL string
	DeepSeekAPIKey  string
	DeepSeekModel   string
}

type Worker struct {
	findingRecorder FindingRecorder
	prov            llm.Provider
	model           string
	workDir         string
	proxyAddr       string
	proxyCACert     string            // recording proxy's CA cert path (for WebFetch HTTPS verify)
	webSearch       WebSearchOpts     // web_search tool backend selection (off by default)
	tx              *transcript.Store // raw LLM conversation persistence (nil = off)
	window          int               // context window in tokens (for compaction)
	windowFn        func() int        // optional dynamic task-chain minimum
	maxTurns        int               // max agent turns per run (0 = unlimited)
	// runTimeout is the wall-clock budget for the main exploration of one intent
	// (0 = unlimited). When it fires, the run is cut and a settlement round is
	// forced so already-identified facts get written back instead of being lost.
	runTimeout time.Duration
	// extraTools are host-provided tools (e.g. traffic query, oast) appended to
	// the worker's graph write-back tools.
	extraTools []actool.CoreTool
	// injectConstraints resolves whether this task's operation constraints get
	// injected into the worker system prompt. Read per run so the settings toggle
	// takes effect without rebuilding the agent. nil = inject (default).
	injectConstraints func() bool
	// nonStreamingFn resolves whether this run uses the non-streaming (Complete)
	// path. Read per run so a profile/task toggle takes effect without rebuilding
	// the agent. nil = streaming (default).
	nonStreamingFn func() bool
	// noaEnabledFn resolves whether this run uses the experimental noa context-
	// compression mechanism. Read per run, like nonStreaming. nil = off (built-in
	// compaction).
	noaEnabledFn func() bool
	// maxTokensFn resolves the per-reply output cap in tokens, on the same
	// per-run basis. nil or 0 = send no cap and let the endpoint decide.
	maxTokensFn func() int
}

// WorkerSessionID returns the stable transcript key used by a worker intent.
// Worker slots are reusable, so the intent id (rather than work#N) is the
// session identity. Keep this helper public so the Worker message API and UI
// can refer to exactly the conversation that will be resumed.
func WorkerSessionID(explorationID, intentID int64) string {
	return fmt.Sprintf("exp%d-worker-i%d", explorationID, intentID)
}

const workerChatMarkerPrefix = "<!-- ARTEX_WORKER_CHAT:"

func workerChatMarker(requestID string) string {
	return workerChatMarkerPrefix + requestID + " -->"
}

func hasWorkerChatMessage(messages []llm.Message, requestID string) bool {
	marker := workerChatMarker(requestID)
	for _, message := range messages {
		if message.Role == llm.RoleUser && strings.Contains(message.Text(), marker) {
			return true
		}
	}
	return false
}

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default). Read per
// run so a profile or task-chain toggle takes effect without rebuilding.
func (w *Worker) SetNonStreaming(fn func() bool) { w.nonStreamingFn = fn }

func (w *Worker) nonStreaming() bool { return w.nonStreamingFn != nil && w.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (w *Worker) SetNoaEnabled(fn func() bool) { w.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (w *Worker) SetMaxTokens(fn func() int) { w.maxTokensFn = fn }

func (w *Worker) maxTokens() int {
	if w.maxTokensFn == nil {
		return 0
	}
	return w.maxTokensFn()
}

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the worker system prompt. nil = inject (default).
func (w *Worker) SetConstraintInject(fn func() bool) { w.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (w *Worker) wantConstraints() bool { return w.injectConstraints == nil || w.injectConstraints() }

// SetRunTimeout configures the per-intent wall-clock budget for the main
// exploration (0 = unlimited). When it fires, the SDK settlement phase still runs
// so facts are never lost to a timeout. Safe to call before Execute.
func (w *Worker) SetRunTimeout(run time.Duration) {
	w.runTimeout = run
}

// settleWrapUpPrompt is injected by the SDK settlement phase when a worker hits its
// turn/time budget: stop probing, write back what was found, then end with a
// plain-text one-liner (which becomes this run's displayed result).
const settleWrapUpPrompt = "이번 실행이 예산 소진으로 곧 종료됩니다. 더 이상 어떤 명령이나 탐지도 실행하지 마십시오. 다음 순서대로 처리하십시오. (1) 위에서 이미 식별했지만 아직 기록하지 않은 내용을 하나씩 기록합니다. 새 자산은 insert_assets, 탐색 결론과 사실은 record_fact, 확인된 취약점은 report_finding 으로 기록합니다. (2) **맨 마지막에 한 문장짜리 순수 텍스트로만** 무엇을 했고 어떤 핵심 결론을 얻었는지 한국어로 요약합니다. 이 한 문장이 이번 실행의 결과로 사용자에게 표시되므로 반드시 출력해야 합니다."

func NewWorker(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int, extra ...actool.CoreTool) *Worker {
	return &Worker{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, extraTools: extra}
}

// defaultToolsExcept returns actool.DefaultTools() minus the named tools (by
// CoreTool.Name()). Used to trim SDK default tools an agent shouldn't have.
func defaultToolsExcept(exclude ...string) []actool.CoreTool {
	drop := make(map[string]bool, len(exclude))
	for _, n := range exclude {
		drop[n] = true
	}
	all := actool.DefaultTools()
	out := make([]actool.CoreTool, 0, len(all))
	for _, t := range all {
		if !drop[t.Name()] {
			out = append(out, t)
		}
	}
	return out
}

func (w *Worker) SetCompactionWindowResolver(fn func() int) { w.windowFn = fn }

func (w *Worker) compactionWindow() int {
	if w.windowFn != nil {
		return w.windowFn()
	}
	return w.window
}

// SetProxy configures the recording proxy address that workers route target
// traffic through, plus the CA cert path WebFetch trusts to verify HTTPS through
// that MITM proxy. Empty addr disables the hint.
func (w *Worker) SetProxy(addr, caCert string) { w.proxyAddr, w.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for this worker (off by default).
func (w *Worker) SetWebSearch(o WebSearchOpts) { w.webSearch = o }

// proxyEnv builds the Bash-subprocess env that routes child-command HTTP through
// the egress proxy (the recording MITM when capture is on, or the global proxy
// directly when it is off) and, only when a MITM CA is present, makes the common
// toolchain trust it — so tools need no manual -x/--proxy/-k. Each ecosystem reads
// a different CA var (verified empirically): SSL_CERT_FILE→curl/urllib/Go/openssl,
// REQUESTS_CA_BUNDLE→python requests (it ignores SSL_CERT_FILE), CURL_CA_BUNDLE→curl,
// GIT_SSL_CAINFO→git, NODE_EXTRA_CA_CERTS→node; NODE_USE_ENV_PROXY makes Node 24+
// honor the proxy vars. ALL_PROXY is set too so a socks5 egress proxy (which curl
// only reads from ALL_PROXY, not HTTP(S)_PROXY) works in the capture-off path.
// Empty proxyAddr → nil (direct, unchanged env).
func proxyEnv(proxyAddr, caCert string) []string {
	if proxyAddr == "" {
		return nil
	}
	env := []string{
		"HTTP_PROXY=" + proxyAddr, "HTTPS_PROXY=" + proxyAddr,
		"http_proxy=" + proxyAddr, "https_proxy=" + proxyAddr,
		"ALL_PROXY=" + proxyAddr, "all_proxy=" + proxyAddr, // socks5 egress: curl reads only this
		"NODE_USE_ENV_PROXY=1", // Node 24+: honor HTTP(S)_PROXY in built-in fetch/http
	}
	if caCert != "" {
		env = append(env,
			"SSL_CERT_FILE="+caCert,
			"CURL_CA_BUNDLE="+caCert,
			"REQUESTS_CA_BUNDLE="+caCert,
			"GIT_SSL_CAINFO="+caCert,
			"NODE_EXTRA_CA_CERTS="+caCert,
		)
	}
	return env
}

// workerDefaultTmpl is the built-in EDITABLE body (段 [A]) of the worker system
// prompt, seeded into agent_prompts. The trafficTool block and the 中間成果物の出力規約
// are NOT here — they are code-owned and appended by workerSystem after rendering
// (段 [B]/[C]), so editing the DB body can never drop them.
const workerDefaultTmpl = `あなたはサイバーセキュリティプラットフォームの、権限を得たペネトレーションテストシステムにおける「実行者」(work agent)である。あなたは【1本の意図】(1文の探索の方向)を受け取り、唯一の役割は：**その1本の意図をやり遂げ、発見をナレッジグラフに書き戻し、それから止まって戻ること。**

**境界（越えてはならない線）**：
1. **受け取ったこの1本の意図だけを行う**。**この意図を探る中で、この意図の外にある深掘りの価値がある手がかりを見かけたら**（エラーで漏れたパス、他の資産と連動しうる点、別の悪用の連鎖の入口らしきもの）、**fact の summary に一言書いて計画者に渡す**。
2. 最初につまずいた（payload がフィルタされた / 404 / インジェクションに応答がない）からといって探り尽くしたことにはならない——この意図のすり抜けの手段をすべてやり終えてから結論を出す。
3. 権限を得た範囲の中でだけ操作する。システムプロンプトの冒頭に【操作の制約】が付いていれば、それが最優先の越えてはならない線である：各コマンド/探査を実行する前に自己点検し、違反するなら行わない（たとえそれが受け取った意図の中にあっても）。

**発見しながら書き戻す**（グラフに書いたものだけが数えられ、頭の中/文章にあるものは数えない。結果が1つ出るたびにすぐ書き、最後までためて手数を使い切って失わないこと）。書き戻しは3種類で、グラフを取り違えない：
- **新しい資産/リソース → insert_assets（資産グラフ）**：サブドメイン / service / endpoint / フィンガープリント / 認証情報 などあらゆる資産【そのもの】。**ここには資産だけを登録する。探索の結論/判断はここに書かず、record_fact を使う。**
- **探索の結論/事実 → record_fact（探索グラフ。intent_id を渡す）**：すべてこれを使う。**複数の観察を【1件の】事実にまとめる**（summary は1文のまとめ + detail はまとめを広げたもので、本物の実行過程に基づく）。属性1つにつき1件にせず、1つの意図では通常1件だけにする。細かく分けるとグラフが際限なく膨らむ——**既定では1件だけ書き、detail に入れられるものはすべて入れる**。【互いに完全に独立していてまとめられない】結論が本当にあるときだけ facts の配列で分けて書く。これはごく少ない例外であって、通常ではない。**増えた分だけを書く**：今回【新しく得た】ものだけを記録し、既存の事実を言い換えて記録し直さない（既存のものを裏づけるだけで新しいものがなければ記録不要）。**本当に見たものだけを書く**：evidence を付け（1行：コマンド+最もよく証明する1、2行の出力。簡潔に、細部は detail に）、confidence を付ける（observed=直接見た / inferred=現象から推測した）。
- **脆弱性を確認した → report_finding（探索グラフ。PoC を含め、intent_id を渡す）**：**今回本当に引き起こし、再現できる証拠（リクエスト/レスポンスまたはコマンドの出力）を得たときにだけ使う**。「バージョン/フィンガープリントが CVE に一致」「引数がインジェクションできそうに見える」「外部の脆弱性データベース/更新履歴/コードの diff から推測」を確認済みとみなすことは厳禁で、CVE データベースの照会やパッチの版との比較で実際に引き起こすことの代わりにもしない。引き起こせないが疑いがある → record_fact で inferred の事実を1件記録し（疑いのある点+なぜ引き起こせなかったか）計画者に渡す。無理に finding として記録しない。


この意図をやり終えたら、何をしたか、どの事実を書き戻したかを1文でまとめる。`

// workerTrafficBlock is 段 [B]: the traffic-tool note, code-injected only when
// traffic capture (recording) is on — i.e. the traffic_* tools actually exist.
// Gated on recording, NOT on the egress proxy: a global proxy with capture off
// routes traffic but records nothing, so the tools would not be there. Not stored,
// not editable.
func workerTrafficBlock(recording bool) string {
	if !recording {
		return ""
	}
	return "\n\n**通信のツール**：\n- traffic_search / traffic_get / traffic_blob：レスポンスを見返し、アクセス済みのリソースを探す。**まず通信を調べ、同じ URL を curl で繰り返さない**。traffic_search は **host の指定が必須**で、既定ではごく軽いインデックス(id/method/url/status/resp_len、レスポンスの内容なし)を3件だけ返す。もっと必要なら limit を明示的に大きくする。body_contains でリクエスト/レスポンスの本文を全文検索できる(3文字以上。部分文字列と日本語・中国語などに対応し、パスワード/鍵/エラー/内部アドレスを探すのに使える)。ある項目の原文を見るには traffic_get(id) を使い、その中で巨大な本文は @blob sha256:<hash> と表示されるので、traffic_blob(hash) で全文を区切って取得する。"
}

// artifactSpec is 段 [C]: the code-owned, non-editable tail appended to every
// pentest agent's prompt — intermediate artifacts must land in the shared work
// dir, never /tmp. Guaranteed present regardless of how the DB body is edited.
func artifactSpec(dir string) string {
	return "\n\n**中間成果物の出力規約**：スクリプト、payload、取得したレスポンスボディ、一時データなどあらゆる中間成果物は、**すべてこのタスクの作業ディレクトリ " + dir + " に書く**（相対パスならここに書かれる。この絶対パスを使ってもよい）——**/tmp に書かず、他の絶対パスも使わない**。"
}

// workerArtifactSpec is the worker's 段 [C]: its per-intent run dir is pre-created
// by the engine (ensureRunDir), so it just writes relative paths there — no manual
// mkdir, no cross-worker name collisions.
func workerArtifactSpec(runDir string) string {
	return "\n\n**中間成果物の出力規約**：スクリプト、payload、取得したレスポンスボディ、一時データなどあらゆる中間成果物は、**すべて今回の意図専用の作業ディレクトリ " + runDir + " に書く**（自動で作成済みなので、相対パスでそのまま書けばよく、手でディレクトリを作る必要はない）——**/tmp に書かず、他の絶対パスも使わない**。"
}

// ensureRunDir builds and creates an agent's working directory under base:
// <base>/tasks/<taskID> for planner/main; <base>/tasks/<taskID>/i<intentID> for a
// worker (intentID<=0 → task dir only). The "tasks/" segment groups per-task dirs
// symmetrically with the chat agent's "sessions/<sessionID>". Best-effort mkdir — on
// failure, writes fail the same way an unwritable CWD would.
func ensureRunDir(base string, taskID, intentID int64) string {
	dir := filepath.Join(base, "tasks", strconv.FormatInt(taskID, 10))
	if intentID > 0 {
		dir = filepath.Join(dir, "i"+strconv.FormatInt(intentID, 10))
	}
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// cmdOutDir is the SDK large-tool-output spill dir under an agent's run dir.
func cmdOutDir(dir string) string { return filepath.Join(dir, "cmd-output") }

func workerSystem(proxyAddr, caCert, dataDir, runDir string) string {
	body := renderSystem("worker", workerDefaultTmpl, WorkerVars{ProxyAddr: proxyAddr, DataDir: dataDir, Now: nowStr()})
	// caCert is present only when the recording MITM is on, which is exactly when
	// the traffic_* tools are registered — so it gates the traffic-tool note.
	// Optional finding guidance is added for every role after tool resolution.
	return body + workerTrafficBlock(caCert != "") + workerArtifactSpec(runDir) + langDirective(resolveOutputLang())
}

// renderIntentTask formats the claimed intent for the worker's launch USER message:
// the intent is the worker's whole job. It used to live in the system prompt; it now
// rides in the first user turn (together with the situational overview) so the system
// prompt stays static/role-only — same move as the planner's situational block.
// intentAssetIDs pulls the intent's target asset ids out of its payload
// (planner's add_intent stores them as a numeric asset_ids array). nil on absence
// or malformed payload.
func intentAssetIDs(intent *db.Node) []int64 {
	if intent == nil {
		return nil
	}
	var p struct {
		AssetIDs []int64 `json:"asset_ids"`
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return nil
	}
	return p.AssetIDs
}

func renderIntentTask(intent *db.Node) string {
	return fmt.Sprintf("\n\n【あなたが受け取った意図（今回の唯一のタスク：この1本だけを行い、事実だけを生み、終わったら止まる）】：\n%s\n意図 id: %d（record_fact / report_finding で書き戻すときに渡す）", string(intent.Payload), intent.ID)
}

// renderWorkerGraphOverview folds the global situational snapshot into the worker's
// launch USER message for AWARENESS ONLY. The framing is deliberately strong: the overview
// must NOT widen the worker's job — it still does only its assigned intent. Its sole
// purpose is letting the worker read context (existing facts/assets/hints)
// so it avoids redundant work and doesn't re-derive what others already found.
func renderWorkerGraphOverview(data map[string]any) string {
	// coverage は計画者が「どの種類のテストが少ないか / 範囲を広げるか」を判断するための合図で、worker の「受け取った
	// 1本の意図だけを行い、扱われていない点を追わない」という役割の境界に反する → worker の視点からは外す。data は今回の worker
	// 専用の新しい map なので、キーを削除しても planner には影響しない。
	delete(data, "coverage")
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back silently: the worker just won't have the global context
	}
	return "\n\n【全体の探索状況（読み取り専用。自分の意図を大局の中に置いて見るため）】：\n" +
		"以下はタスク全体の現在の探索の概況である。用途は2つ：1つは他の人がすでに何を見つけたかを知り、重複を避けること。もう1つは自分の意図を探るとき、それと全体との関係を連想できるようにすること。\n" +
		"**発想を広げるのはよいこと**：この意図を探るときは存分に深く考え、多くを連想してよい。唯一の線引きは——他の意図を実際に手を動かして実行しないこと（それは他の worker の仕事で、計画者が割り振る）。価値のある手がかり（資産をまたぐ連動、別の悪用の連鎖の入口らしきもの、全体のレベルで疑わしい点）を連想したら、**必ず fact に書いて計画者に渡す**——これはあなたの重要な成果物であって、あってもなくてもよいものではない。自分で飲み込むくらいなら、1件多く報告して計画者に判断させるほうがよい。\n" +
		string(b)
}

// Execute runs one intent. hooks (the per-task Guard) gates every tool call; may
// be nil. emit, if non-nil, receives one ActivityRecord per execution step.
// notifyFinding, if non-nil, is called (intentID, summary) when this worker writes
// a finding (report_finding) so the task's planner wakes mid-flight — with context
// on which intent found what — instead of waiting for the worker to finish.
// Returns the terminal reason (so the engine can distinguish completed vs
// max_turns) and a per-kind breakdown of what was written back (so an intent that
// explored but persisted nothing isn't mistaken for done, and the engine can log
// facts/assets/findings separately instead of lumping them under "facts").
func (w *Worker) Execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string)) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, enr, notifyFinding, "", "")
}

// ExecuteWithMessage runs the next turn in the same intent conversation with a
// human-authored message. The HTTP handler does not edit the transcript;
// agentcore records the message as a normal user turn when this Worker starts.
// This keeps Worker continuation identical to the regular agent chat flow.
func (w *Worker) ExecuteWithMessage(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, enr, notifyFinding, strings.TrimSpace(requestID), strings.TrimSpace(message))
}

func (w *Worker) execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	tsx := NewToolSet(ts, name)
	tsx.SetFindingRecorder(w.findingRecorder)
	tsx.SetTaskID(taskID)
	coverageEnabled := as == nil || as.CoverageEnabled(taskID)
	tsx.SetCoverageEnabled(coverageEnabled)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetOwnerNode(intent.ID)         // assets this worker discovers anchor to its intent → visible to the task
	tsx.SetEnrich(enr)                  // async DNS/HTTP auto-completion for assets this worker writes
	tsx.SetNotifyFinding(notifyFinding) // report_finding を DB に入れた時点で planner を起動し、「どの意図+finding」かを添える
	// base = built-in worker tools ∪ host tools (traffic) ∪ default tools (incl. Bash);
	// then augment with the agent's visible skills/MCP. During the SDK settlement
	// phase, Bash is hidden via Settlement.DisabledTools (no local gating needed).
	base := append(tsx.WorkerTools(), w.extraTools...)
	// worker にはわざと MultiEdit/Glob/Grep を与えない：ファイルの細かい修正は Edit、検索は Bash(grep/find) を使い、
	// ツールの面を絞って価値の低い呼び出しを減らす。それ以外の SDK の既定のツール(Read/Write/Edit/LS/Bash/Sleep)は通常どおり。
	base = append(base, defaultToolsExcept("MultiEdit", "Glob", "Grep")...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts), IntentID: intent.ID})
	tools, def, cleanup := AugmentTools(ctx, "worker", base)
	defer cleanup()

	// 意図は worker の【唯一の役割であり、run 全体を貫く不変条件】→ 起動の指示、意図が結びつく対象の資産の
	// 元データとあわせて system prompt に入れる：system は run のたびに組み立て直され、compaction で圧縮されることは決してないので、
	// 長い run でも意図は常にそこにあり、続きを走らせるときも transcript の履歴にその最初のメッセージが残っているかに依存しない。代償は system に
	// 意図ごとの変わりやすいデータが混ざり、意図をまたぐキャッシュの再利用を失うこと。これは意図した取捨である（意図を失うほうが token の節約よりはるかに深刻）。
	// planner の「状況のブロックを user turn に置く」と分かれているのは意図的：planner 自身は意図を生む側で、単一の任務を持たないが、
	// worker は持つ。【全体の状況 overview】だけを起動時の user メッセージに残す——それは劣化してよく、古くてもかまわないので、圧縮されても支障はない。
	// 今回の意図専用の作業ディレクトリ <workDir>/tasks/<taskID>/i<intentID> は、エンジン側で先に作っておく。
	runDir := ensureRunDir(w.workDir, taskID, intent.ID)
	// The run-wide intent is not the current tool action. Do not forward it or
	// inherit a parent run's background into the action reviewer.
	ctx = intercept.WithReviewContext(ctx, runDir, intercept.ReviewBackground{})
	overview := renderWorkerGraphOverview(tsx.graphOverviewData())
	sysBody := workerSystem(w.proxyAddr, w.proxyCACert, w.workDir, runDir)
	if w.wantConstraints() {
		sysBody += constraintBlock(ts) // 操作の制約(あれば)をシステムプロンプトに注入し、worker の実行時に厳守させる
	}
	// 意図のブロック → 意図が結びつく資産のブロック → 起動の指示、の順に system の末尾へ足していく（constraintBlock と同じ足し方）。
	sysBody += renderIntentTask(intent)
	if as != nil {
		if ids := intentAssetIDs(intent); len(ids) > 0 {
			if assets, err := as.GetByIDs(ids); err == nil && len(assets) > 0 {
				if b, err := json.Marshal(assets); err == nil {
					sysBody += "\n\nこの意図の asset_ids に対応する対象の資産：\n" + string(b)
				}
				// 意図がはっきり対象にしているこれらの資産 → 自動でタスクのテスト範囲に含める（insertAssets と同じ
				// 保守的な粒度）。upsertTaskScope の ON CONFLICT DO NOTHING + uq_task_scope の
				// 一意インデックスにより重複して追加されることはない。走り直し/再試行でも同じく冪等な no-op になる。
				// 資産カバレッジの機能がオフなら、テスト範囲(分母)はもう積み上げない。
				if coverageEnabled {
					for _, a := range assets {
						_ = as.AddAutoScope(taskID, a.Type, a.Domain, a.URL, a.IP)
					}
				}
			}
		}
	}
	sysBody += "\n\n上のこの意図の実行を始める：これだけを行い、事実、assets、finding だけを生み、終わったら止まる。"
	system, boundary := deferredSystem(sysBody, def)
	// タスク単位の deadline(ctx で注入)でこの run の実時間の予算を挟み込み + 締めくくりの言葉を決める(taskclock.go を参照)。
	tc := taskClockFrom(ctx)
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, w.runTimeout)
	settle := wrapupSettlement("worker", []string{"Bash"})
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("worker", []string{"Bash"}, clamped)
	}
	opts := agentcore.Options{
		Provider:        w.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		// WebFetch は記録プロキシを通り、その HTTP は curl と同じく痕跡が残る。プロキシの CA を読み込み、MITM で
		// 署名し直した HTTPS の証明書が【正常に検証を通る】ようにする（検証を切るのではない）。proxy が空なら直接つなぐ。
		EnableWebFetch: true,
		WebFetchProxy:  w.proxyAddr,
		WebFetchCACert: w.proxyCACert,
		// Web 検索(任意)。ddgs は key 不要。brave-free は BraveKey、tavily は TavilyKey が必要。
		// WebSearchProxy は独立した出口プロキシ(http/https/socks5)で、通信を記録する MITM プロキシとは関係ない。空なら直接つなぐ。
		EnableWebSearch:       w.webSearch.Enabled,
		WebSearchBackend:      w.webSearch.Backend,
		BraveSearchAPIKey:     w.webSearch.BraveKey,
		TavilySearchAPIKey:    w.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: w.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  w.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   w.webSearch.DeepSeekModel,
		WebSearchProxy:        w.webSearch.Proxy,
		// Bash の子コマンドの HTTP は既定で記録プロキシを通り + その CA を信頼する（ツールに -x/-k は要らない）。
		BashEnv:    proxyEnv(w.proxyAddr, w.proxyCACert),
		WorkingDir: runDir,
		MaxTurns:   w.maxTurns, // 0 = unlimited (configurable in agent management)
		// 実時間の予算。ラウンドの境目で判定し、途中では中断しない。0 = 制限なし。タスク単位の deadline があるときは min(自身の予算,
		// deadline までの残り) に挟み込み、この run がタスクの期限で自然に締めくくりに入るようにする(taskclock.go を参照)。
		MaxDuration: maxDur,
		// 予算(ラウンド数 OR 時間)に当たったら → SDK が締めくくりのラウンドを1回走らせ(Bash は隠す)、見つけたものを書き戻させて、中途半端に終わるのを避ける。
		// clamped(タスクの deadline で挟み込まれた)ときは PromptByReason を使う:タイムアウトが原因=タスクの期限→タスクのタイムアウトの言葉、
		// 手数が原因=挟み込まれた窓の中で手数を先に使い切った→per-run の言葉に戻る。clamped でなければ純粋な per-run のまま。
		Settlement: settle,
		// large tool output spills to cmd-output/ with a head + pointer (SDK tool.Capture);
		// full output preserved on disk. 切り詰めの上限は SDK の既定(30000 文字)を使う。
		ToolOutputDir: cmdOutDir(runDir),
		Compaction:    compactionConfig(w.compactionWindow()), // long tool-heavy runs stay within the window
		Todos:         actool.NewTodoStore(),                  // セッション単位の一時的な ToDo（TodoWrite）。計画専用で、終了したら捨てる
		NonStreaming:  w.nonStreaming(),                       // その profile で非ストリーミングを選んでいれば Provider.Complete を通る
		MaxTokens:     w.maxTokens(),                          // 0 = 上限を送らず、サーバーの既定値に任せる
	}
	if hooks != nil { // typed-nil guard: only set when concrete (avoids harness panic)
		opts.Hooks = hooks
	}
	if w.tx != nil { // persist raw LLM conversation; one file per worked intent
		opts.Transcript = w.tx
		opts.SessionID = WorkerSessionID(ts.ID(), intent.ID)
	}
	intentID := intent.ID
	emitWrap := func(r db.Activity) {
		if emit != nil {
			r.NodeID, r.Worker = &intentID, name
			emit(r)
		}
	}
	// 意図 / 起動の指示 / 意図が結びつく資産はすでに system prompt とともに渡してある（上の sysBody の組み立てを参照）。
	// この起動時の user メッセージが担うのは【全体の状況 overview】だけ——劣化してよい大局の情報で、圧縮されても支障はない。
	// overview がまれに marshal に失敗して空になったら、起動の言葉1文に戻し、最初のラウンドに空の user メッセージが出るのを避ける。
	input := overview
	if strings.TrimSpace(input) == "" {
		input = "system で受け取った意図の実行を始める：これだけを行い、事実、assets、finding だけを生み、終わったら止まる。"
	}

	// 実験的な機能:有効にすると noa がコンテキストの圧縮を引き受ける(アーカイブは <workDir>/noa/<SessionID> の下にまとめて永続化)。
	noaSession := WorkerSessionID(ts.ID(), intent.ID)
	enableNoa(&opts, w.noaEnabledFn, w.workDir, noaSession, noaWarn(noaSession))
	ctx = attachSideCapture(ctx, &opts)
	s := agentcore.NewSession(opts)
	defer s.Close() // release the session's background-task manager (temp dir + processes)

	// Resume prior conversation if this intent was paused/blocked/exhausted and is
	// being re-run. The transcript ID is deterministic per intent, so if a prior
	// session exists the worker continues from where it left off instead of
	// restarting from scratch.
	alreadyRecorded := false
	if w.tx != nil {
		_ = s.Resume(opts.SessionID)
		alreadyRecorded = requestID != "" && hasWorkerChatMessage(s.Messages(), requestID)
		if len(s.Messages()) > 0 && message == "" {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
			input = "実行を続ける。"
		} else if len(s.Messages()) > 0 {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
		}
	}
	if message != "" {
		if alreadyRecorded {
			input = "前回、人が会話で入力した新しい意図の実行を続ける。すでに完了した動作を繰り返さない。"
		} else if len(s.Messages()) > 0 {
			input = workerChatMarker(requestID) + "\n【人が会話で入力した新しい意図】\n" + message +
				"\n\nこの人の入力にすぐ従って実行し、終わったら文脈をもとに元のタスクを続ける必要があるかを判断すること。"
		} else {
			input += "\n\n" + workerChatMarker(requestID) + "\n【人が会話で入力した新しい意図】\n" + message +
				"\n\nこの人の入力を優先して実行すること。"
		}
	}

	// Budgets + settlement are owned by the SDK (MaxTurns/MaxDuration + Settlement):
	// on hit it runs a wrap-up turn and finishes with ReasonMaxTurns/ReasonTimeout.
	// MaxDuration now interrupts an in-flight tool at the wall-clock deadline and
	// enters the wrap-up phase on the live ctx, so a run whose tool overran the budget
	// still settles (no external hard-timeout backstop needed). ctx itself carries only
	// pause / planner kill / shutdown, which the engine distinguishes and re-queues/stops.
	_, reason, err := captureRunSession(ctx, s, input, emitWrap)
	return reason, tsx.Writes(), err
}
