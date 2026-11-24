package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// goalsDefaultTmpl is the built-in EDITABLE body (段 [A]) of the goals-decomposer
// prompt, seeded into agent_prompts. No template vars are used today.
const goalsDefaultTmpl = `あなたはペネトレーションテストの目標分解器である。あなたの役割は、ユーザーの入力から**最終的に達成すべき結果**を見分けることであり、攻撃の段階を計画することではない。

**第一段階（目標を分ける前に行う）：操作の制約を取り出す**
「タスクの目標 / タスクの説明」から、操作者が【何をしてよく、何をしてはならないか】をはっきり定めたものを見分け、set_constraints を呼んで1件ずつ登録する（説明や目標に操作の制約が含まれていなければ、操作の制約を取り出さなくてよい）：
- type=deny：禁止された操作（「ポートをスキャンしない」「本番環境に書き込み/削除の操作をしてはならない」「ブルートフォース禁止」「あるサブドメインには触れない」など）。
- type=allow：はっきり許された/限定された操作の範囲（「受動的な偵察だけを許す」「あるドメインだけを対象にする」など）。
- 制約 ≠ 目標であり、≠ 攻撃の段階でもない：それは操作の振る舞いの境界を定めたものである。
- **制約は【それだけで完結し、具体的な対象を固定して書く】こと**：「現在の対象/現在のポート/現在の IP/現在のドメイン/このサイト」のような**指示語**を、タスクの目標/説明にある**具体的な値**に置き換える。制約は実行段階のプロンプトに単独で注入されるので、文脈から切り離されると指示語が何を指すか判断できない。
  例：目標が https://abc.example.net → 「現在の対象だけをテストしてよい」ではなく「abc.example.net だけをテストしてよい」と書く。「現在のポートだけをテストする」ではなく「対象のポート 443 だけをテストし、他のポートはスキャンしない」と書く。原文が「現在の対象」としか言っていなくても対象のアドレスがはっきりしていれば、アドレスを埋める。
- **目標/説明に【はっきり書かれている、または強調されている】制約だけを登録し、作り上げることは厳禁**。種類に自信がなければ deny を使う（より保守的）。
- 目標/説明に操作の制約が本当に1つもなければ、set_constraints を呼ば**ない**。
制約を登録し終えたら（あれば）、次の目標の分割に進む。

**目標 = 最終的に引き渡せる/確かめられる結果**

**目標でないもの（子目標として挙げることを禁止）**：
- 情報収集、偵察、エンドポイントのスキャン
- 脆弱性の分析と検証の過程
- 攻撃の段階、悪用の手段
- 結果の検証の段階

**分割の原則**：
- ユーザーが述べた最終目標が1つだけ → 1つ出力する
- **互いに独立した**最終的な成果物が複数ある → それぞれ挙げる
- はっきりした脆弱性の種類に対応できるものには vulnclass を付ける。情報収集/業務ロジック系の目標は空にする
- ユーザーが触れていない目標を作り上げることは厳禁

set_goals を呼んで結果を提出する。`

// goalsScopeTail is the code-owned tail appended after the editable goals body
// WHEN an asset store + task context are available. It teaches the decomposer to
// also lift the explicit asset scope out of the goal/description and register it
// via add_task_scope. Kept in code (not the DB-editable body) so it always applies
// on released DBs and can't be edited away — same pattern as the trafficTool tail.
const goalsScopeTail = `

**追加の役割：テスト対象の資産の範囲を登録する**
目標の分割に加えて、「タスクの目標 / タスクの説明」から**はっきり示されたテスト対象の資産の範囲**を見分け、add_task_scope を呼んで登録する（このタスクの権限の境界であり、資産のテストのカバレッジの分母でもある）。**最小範囲の原則：ユーザーがはっきり名指しした1つの対象だけを登録し、勝手に広げることは決してしない。**
- 対象が URL かホスト名付きのアドレス（https://xxx.example.com/path、app.example.com など）→ その**完全なホスト名**を取り、kind=subdomain、value=完全なホスト名。
  例：対象 https://a1b2c3.lab.example.net/path → kind=subdomain、value=a1b2c3.lab.example.net（example.net **ではない**）。
  サブドメイン付きのホスト名をルートドメインに縮めることは**厳禁**——xxx.example.com を見て example.com 全体を登録すると、範囲がユーザーの対象の外まで広がり、最小範囲の原則に反する。
- ユーザーが示したのが**素のルートドメインで、サブドメインを一切含まない**（example.com と直接書いているなど）場合、または「サイト全体 / すべてのサブドメイン / ドメイン全体」とはっきり言っている場合 → そのときだけ kind=root_domain、value=example.com を使う。
- IP やネットワーク範囲だけ → kind=ip / cidr、value=IP または CIDR。
- 会社の範囲（company）は登録**しない**——タスクが作られたばかりで、資産システムにはたいていまだその会社がないので登録できない。会社単位の範囲はあとの plan の段階に任せる。
その他のルール：
- **目標/説明にはっきり書かれた**範囲だけを登録する。触れられていないドメイン/IP を作り上げたり推測したりすることは厳禁。
- reason には根拠がどの文から来たかを簡潔に書き、監査しやすくする。
- 目標/説明に明確な資産の範囲が1つもなければ、add_task_scope を呼ば**ない**。
まず add_task_scope で範囲を登録し（あれば）、それから set_goals を呼んで目標を提出する。`

// goalsSystem assembles the goals-decomposer system prompt: the rendered body
// [A] (DB-overridable), the code-owned scope-extraction tail when add_task_scope
// is wired (withScope), and the code-owned Korean output-language tail [C] last —
// mirroring chatSystem/plannerSystem so a DB-edited body can never drop the tail.
// DecomposeGoalsWithProvider and the localization test share this one assembly, so
// the langDirective tail can't drift between runtime and test. EngagementDescription
// is intentionally left empty: the task description rides in the user message, not
// the {{.EngagementDescription}} var (see DecomposeGoalsWithProvider).
func goalsSystem(dataDir string, withScope bool) string {
	sys := renderSystem("goals", goalsDefaultTmpl, GoalsVars{DataDir: dataDir, Now: nowStr()})
	if withScope {
		sys += goalsScopeTail
	}
	return sys + langDirective(resolveOutputLang())
}

// GoalSpec is one decomposed objective.
type GoalSpec struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass,omitempty"`
}

// DecomposeGoals asks the LLM to break a pentest task goal into discrete,
// independently-verifiable objectives (each becomes a goal node). Returns nil if
// no provider is configured or the call yields nothing — the caller then falls
// back to a rule-based split so goal nodes always exist.
//
// prov is supplied by the caller (rather than built here from a Config) so goal
// decomposition rides the SAME provider instance as the rest of the engine — it
// shares the rate limiter, gets recorded by llmrec, and participates in LLM
// failover instead of quietly bypassing all three.
//
// desc is the task's free-text description (背景：標的の範囲/flag の数/交戦の説明など).
// It is fed alongside the goal so the decomposer no longer splits blind — the
// prompt still forbids inventing anything the two texts don't state.
//
// emit, when non-nil, receives every LLM step (thinking/tool_use/result) with
// Worker="planner" so the round-0 goal-decomposition activity is visible in the UI.
//
// as + taskID, when non-nil/positive, wire the add_task_scope tool so the
// decomposer can register the explicit asset scope it extracts from the goal.
//
// ts is the task's exploration store: set_goals writes the decomposed goal nodes
// straight into it (the same managed tool the main agent uses to add goals at
// runtime). The returned specs are read back from the store so callers can emit
// per-goal activity and detect the "LLM produced nothing" case for their fallback.
func DecomposeGoals(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	return DecomposeGoalsWithProvider(ctx, prov, dataDir, goalText, desc, as, ts, taskID, false, 0, emit)
}

// DecomposeGoalsWithProvider is the task-runtime variant used when a task has an
// ordered provider chain. It preserves the same tools and write behavior while
// letting the caller own provider selection/failover. maxTokens is the profile's
// per-reply output cap (0 = send none).
func DecomposeGoalsWithProvider(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, nonStreaming bool, maxTokens int, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	// 目標の分解は1回きりの呼び出し：transcript store を付けないので、agentcore は ctx に
	// session id を付けない（writer があるときだけ付ける。agentcore.Prompt を参照）。ところが session-id ヘッダーで
	// プロンプトのキャッシュ/固定ルーティングを行うゲートウェイ（opencode zen は x-opencode-session がないとすぐ 400
	// MissingSessionID）が読むのはまさに ctx 上のこの値だ——補わないと「会話は正常なのに、分解は 400」になる。
	// 安定した id を明示的に付ける：同じ探索の分解のリクエストはそれを共有し（キャッシュに当たりやすい）、名前も
	// planner/worker と衝突せず、llmrec.parseSession で正しく帰属させられる。
	if ts != nil {
		ctx = transcript.WithSessionID(ctx, fmt.Sprintf("exp%d-goals", ts.ID()))
	}
	// worker="goals" tags the goal nodes' provenance; ts/taskID let set_goals link
	// each goal under the task root. This is the catalog's real set_goals tool, so a
	// web-edited description/schema on it applies here too.
	tsx := &ToolSet{as: as, ts: ts, taskID: taskID, worker: "goals"}
	// Wire add_task_scope only when we have a real asset store + task to write to.
	// goalsSystem appends the scope-extraction tail in lockstep (withScope) so the
	// prompt never asks for a tool that isn't present, and it owns the output-language
	// tail last so a DB-edited body can't drop it. Description rides in the user
	// message, NOT the {{.EngagementDescription}} var, so a prompt can't inject it twice.
	withScope := as != nil && taskID > 0
	sys := goalsSystem(dataDir, withScope)
	// set_constraints は常に使える(asset store に依存しない):本文にはすでに「先に操作の制約を取り出してから目標を分ける」段階が入っている
	// (agent の編集ページで言い回しを変えられる)ので、ここではツールをつなぐだけでよい。
	tools := []actool.CoreTool{tsx.setGoals(), tsx.setConstraints()}
	if withScope {
		tools = append(tools, tsx.addTaskScope())
	}
	userMsg := "タスクの目標：\n" + goalText
	if d := strings.TrimSpace(desc); d != "" {
		userMsg += "\n\nタスクの説明（背景情報。標的の範囲/flag の数/交戦の説明を含むことがある。参考にとどめ、そこで触れられていない内容を作り上げないこと）：\n" + d
	}
	// Use captureRun so every LLM step is emitted as an activity record (visible in
	// the plan tab under the round-0 marker). Falls back gracefully when emit is nil.
	captureEmit := func(r db.Activity) {
		if emit != nil {
			r.Worker = "planner"
			emit(r)
		}
	}
	captureRun(ctx, agentcore.Options{
		Provider:               prov,
		SystemPrompt:           []string{sys},
		Tools:                  tools,
		PermissionMode:         acperm.ModeBypass,
		DisableBackgroundTasks: true,
		// 3段階(制約の取り出し → 範囲の登録 → 目標の分割)はそれぞれツールの呼び出しが1回要るので、締めくくりの前に set_goals を呼び漏らさないよう十分なターンを与える。
		MaxTurns:     8,
		NonStreaming: nonStreaming, // その profile で非ストリーミングを選んでいれば Provider.Complete を通る
		MaxTokens:    maxTokens,    // 0 = 上限を送らず、サーバーの既定値に任せる
	}, userMsg, captureEmit)
	// set_goals persisted the goals directly; read them back so the caller sees what
	// was written (empty slice ⇒ the LLM produced nothing ⇒ caller falls back).
	if ts == nil {
		return nil
	}
	nodes, _ := ts.ListByKind(db.KindGoal, 10000)
	var out []GoalSpec
	for _, n := range nodes {
		var p struct {
			Text      string `json:"text"`
			VulnClass string `json:"vulnclass"`
		}
		_ = json.Unmarshal(n.Payload, &p)
		if strings.TrimSpace(p.Text) != "" {
			out = append(out, GoalSpec{Text: p.Text, VulnClass: p.VulnClass})
		}
	}
	return out
}
