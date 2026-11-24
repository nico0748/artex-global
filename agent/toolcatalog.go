package agent

import (
	"context"
	"encoding/json"
	"strings"

	actool "github.com/Autumn-27/norma/tool"
)

// このファイルは「組み込みのツール」を純粋なコードから、列挙でき DB で上書きできる目録に変える：
//   - BuiltinToolSeeds()：3つの実行 agent の組み込みのツール群を seed の記録（key +
//     説明 + 引数の schema + 既定で結びつける agent）に展開し、サーバーが起動時に冪等に tools 表へ種をまく。
//   - ToolResolve フック：実行時に DB の tools の行に従い、組み立て済みのツールに「agent ごとの絞り込み +
//     説明/schema の上書き + 引数の既定値の注入」をする。key/handler は引き続きコードの層にあり、DB が変えるのは「文と既定値」だけ。
// handler（Call の挙動）は常にコードから来る——DB はそれを変えられず、モデルが見る説明と既定の入力だけを変えられる。

// ToolSeed は組み込みのツール1つの種まき用のスナップショット：key は CoreTool.Name()（handler と固く結びつき、
// UI では読み取り専用）、Desc/Schema はコードのツールの定義から取り、Agents はコードが既定でそれをどの agent に渡したか。
type ToolSeed struct {
	Key    string         // = CoreTool.Name()。主キーで、変えられない
	Desc   string         // 最上位の説明（UI で上書きできる）
	Schema map[string]any // 引数の JSON-Schema（構造は読み取り専用。description/default は UI で変えられる）
	Agents []string       // 既定で結びつける agent の key（worker/planner/mainagent）
}

// builtinToolsByAgent は「読み取り専用の空の殻」の ToolSet（nil stores）で各実行 agent の
// 領域のツール群を組み立てる。ツールのコンストラクターはクロージャを Spec に入れるだけで、組み立て時に store を参照しないので nil でも安全——
// これらのツールはここでは Name()/Description()/InputSchema() を読むのにだけ使い、決して Call しない。
//
// あえて SDK の汎用ツール actool.DefaultTools()（Read/Write/Edit/MultiEdit/LS/Glob/
// Grep/Bash）を【含めない】：それらはどの agent も固定で持ち、「誰に結びつけるか」の取捨がなく、説明もほとんど Prompt()
// にある（この表は Description() だけを上書きするので、半端な上書きで誤解を招く）。seed しない → DB の行がない → ToolResolve は
// そのまま通して上書きせず、挙動は以前と同じ。管理できる表に入るのは artex 自身の領域のツールだけ。
func builtinToolsByAgent() map[string][]actool.CoreTool {
	ts := NewToolSet(nil, "")
	return map[string][]actool.CoreTool{
		"mainagent": ts.MainAgentTools(),
		"planner":   ts.PlannerTools(),
		"worker":    ts.WorkerTools(),
		// goals（目標の分解器）は既定で set_goals + set_constraints を結びつける：それらで分解した目標と、
		// 抜き出した操作の制約を DB に書く。mainagent と同じ管理されたツールを共有し、web 側で説明/schema を変えたり、agent ごとにチェックしたりできる。
		"goals": {ts.setGoals(), ts.setConstraints()},
		// auto は既定で脆弱性の報告 + 資産の管理のツールを結びつけ、他の領域のツールは UI で必要に応じてチェックできる。
		// 新しい DB はこの seed で書き込む；古い DB は seedAutoDefaultBindings で移行する。
		"auto": {ts.addFinding(), ts.insertAssets(), ts.addCompanyScope(), ts.listAssets(), ts.listCompanies()},
		// pentest（独立した侵入テストの agent）は既定で次を結びつける：資産の検索 / 資産の挿入 / 脆弱性の報告 / 脆弱性の検索 / 企業の検索。
		// 新しい DB はこの seed で書き込む；古い DB は seedPentestDefaultBindings で移行する。
		"pentest": {ts.listAssets(), ts.insertAssets(), ts.addFinding(), ts.listFindings(), ts.listCompanies()},
	}
}

// defaultUnbound：これらの system ツールは通常どおり目録に入る（web 側で見え、手動で agent ごとにチェックできる）が、
// 既定では【どの agent にも結びつけない】——ToolResolve は結びつけが空のツールをすべての agent について一律に捨てるので、明示的な opt-in が必要。
// それでもいずれかの agent の base のツール群に残しておく理由(PlannerTools の goal_met など)：1つは seed が
// それを組み立てて desc/schema を得られるように、もう1つはユーザーが手動で結びつけ直した後に実行時の base にそれがあり、ToolResolve で残せるようにするため。
//
// goal_met：prove_goal を1つずつするのを飛ばし、全体から直接【タスク全体の完了】を宣言する。影響が大きく誤判定のおそれがあり、
// 「prove_goal で最後の目標に印を付ける → 自動で締めくくる」と重なるので、既定ではどの agent にも渡さず、必要なときに手動で結びつける。
var defaultUnbound = map[string]bool{"goal_met": true}

// BuiltinToolSeeds は各 agent の組み込みのツール群を重複を除いて seed の一覧にまとめる：同じ名前のツール（list_assets など
// 複数の agent が持つもの）は1件にまとめ、Agents は和集合を取る；defaultUnbound のツールは結びつけを強制的に空にする。
func BuiltinToolSeeds() []ToolSeed {
	byAgent := builtinToolsByAgent()
	order := []string{"mainagent", "goals", "planner", "worker", "auto", "pentest"}

	type acc struct {
		tool   actool.CoreTool
		agents []string
	}
	m := map[string]*acc{}
	var keys []string
	for _, ak := range order {
		for _, t := range byAgent[ak] {
			a, ok := m[t.Name()]
			if !ok {
				a = &acc{tool: t}
				m[t.Name()] = a
				keys = append(keys, t.Name())
			}
			a.agents = append(a.agents, ak)
		}
	}

	out := make([]ToolSeed, 0, len(keys))
	for _, k := range keys {
		a := m[k]
		agents := a.agents
		if defaultUnbound[k] {
			agents = []string{} // 目録に入り、手動で結びつけられるが、既定ではどの agent にも渡さない（null ではなく [] を保存し、他のツールとそろえる）
		}
		out = append(out, ToolSeed{
			Key:    k,
			Desc:   a.tool.Description(),
			Schema: a.tool.InputSchema(),
			Agents: agents,
		})
	}
	return out
}

// ToolResolve, if set, post-processes an agent's fully-assembled tool list against
// the DB tools table: it drops tools not bound to this agent (or globally disabled)
// and wraps the rest so the model sees the DB-overridden description/schema and
// 既定の入力 get injected. Tools with no matching DB row (MCP/skill/host tools like
// traffic) pass through untouched. nil = tools unchanged. Wired in server/assembly.go.
var ToolResolve func(ctx context.Context, agentKey string, tools []actool.CoreTool) []actool.CoreTool

// DecorateTool wraps t so Description()/InputSchema() report the DB overrides and
// Call() injects scalar parameter defaults (from schema's "default" props) whenever
// the model omitted them. Name/Prompt/permission/scheduler flags delegate to t, so
// the tool's identity and handler are unchanged. Empty desc/schema fall back to t's.
func DecorateTool(t actool.CoreTool, desc string, schema map[string]any) actool.CoreTool {
	if desc == "" {
		desc = t.Description()
	}
	if len(schema) == 0 {
		schema = t.InputSchema()
	}
	return &overriddenTool{CoreTool: t, desc: desc, schema: schema}
}

// overriddenTool is a CoreTool decorator: it embeds the original (so all behavioral
// methods — Prompt/IsReadOnly/IsConcurrencySafe/CheckPermissions/Name — delegate)
// and overrides only the model-facing description/schema plus default injection.
type overriddenTool struct {
	actool.CoreTool
	desc   string
	schema map[string]any
}

func (o *overriddenTool) Description() string         { return o.desc }
func (o *overriddenTool) InputSchema() map[string]any { return o.schema }

func (o *overriddenTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	return o.CoreTool.Call(ctx, injectDefaults(in, o.schema), tc)
}

// injectDefaults fills scalar parameter defaults declared in the (possibly edited)
// schema into the input JSON whenever the model omitted the field or left it empty/
// null. Structure (names/types/required) is untouched — only 既定値 are merged in.
func injectDefaults(in json.RawMessage, schema map[string]any) json.RawMessage {
	defs := scalarDefaults(schema)
	if len(defs) == 0 {
		return in
	}
	m := map[string]json.RawMessage{}
	if len(in) > 0 {
		if err := json.Unmarshal(in, &m); err != nil {
			return in // non-object input: don't touch it
		}
	}
	changed := false
	for k, dv := range defs {
		if cur, ok := m[k]; !ok || isEmptyJSON(cur) {
			m[k] = dv
			changed = true
		}
	}
	if !changed {
		return in
	}
	b, err := json.Marshal(m)
	if err != nil {
		return in
	}
	return b
}

// scalarDefaults extracts properties[k]["default"] for scalar params (string/
// integer/number/boolean). Array/object defaults are skipped: merging them is
// ambiguous and not worth the surprise.
func scalarDefaults(schema map[string]any) map[string]json.RawMessage {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	out := map[string]json.RawMessage{}
	for name, raw := range props {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		dv, ok := p["default"]
		if !ok || dv == nil {
			continue
		}
		switch p["type"] {
		case "string", "integer", "number", "boolean":
			if b, err := json.Marshal(dv); err == nil {
				out[name] = b
			}
		}
	}
	return out
}

func isEmptyJSON(raw json.RawMessage) bool {
	s := string(raw)
	return s == "null" || s == `""`
}

// aliasTool is a CoreTool decorator that overrides only Name(): it lets a
// CamelCase built-in (e.g. Bash) also answer to the lowercase/snake_case spelling
// a model drifts into (bash, web_fetch). Handler, description, schema, prompt and
// permission all delegate to the wrapped tool, so an alias is behaviourally
// identical — it only repairs exact-match dispatch lookup (norma's execOne does
// reg.Get(use.Name), no case folding). See issue #19.
type aliasTool struct {
	actool.CoreTool
	name string
}

func (a *aliasTool) Name() string { return a.name }

// toolAliasNames returns the lowercase / snake_case / UPPER spellings a CamelCase
// tool name is commonly confused with, dropping the original and duplicates. A
// name already in lower/snake form yields nothing (no drift to defend against).
func toolAliasNames(name string) []string {
	if name == strings.ToLower(name) {
		return nil
	}
	candidates := []string{strings.ToLower(name), camelToSnake(name), strings.ToUpper(name)}
	out := make([]string, 0, len(candidates))
	seen := map[string]bool{name: true}
	for _, v := range candidates {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// camelToSnake converts CamelCase to snake_case (WebFetch → web_fetch,
// TodoWrite → todo_write) while leaving an all-caps run intact (LS → ls).
func camelToSnake(s string) string {
	runes := []rune(s)
	var b strings.Builder
	for i, r := range runes {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				prev := runes[i-1]
				var next rune
				if i+1 < len(runes) {
					next = runes[i+1]
				}
				if (prev >= 'a' && prev <= 'z') || (next >= 'a' && next <= 'z') {
					b.WriteByte('_')
				}
			}
			b.WriteRune(r + ('a' - 'A'))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// addToolAliases registers drift-tolerant aliases for every CamelCase tool. Each
// alias is added to the returned tool list (so exact-match dispatch finds it) and
// its name is appended to deferred (so the schema is withheld and the model keeps
// seeing only the official CamelCase names — no duplicate exposure, no token
// waste). Names already taken by a real tool are skipped, so nothing is shadowed.
func addToolAliases(tools []actool.CoreTool, deferred []string) ([]actool.CoreTool, []string) {
	taken := map[string]bool{}
	for _, t := range tools {
		taken[t.Name()] = true
	}
	for _, n := range deferred {
		taken[n] = true
	}
	out := tools
	extra := deferred
	for _, t := range tools {
		for _, alias := range toolAliasNames(t.Name()) {
			if taken[alias] {
				continue
			}
			taken[alias] = true
			out = append(out, &aliasTool{CoreTool: t, name: alias})
			extra = append(extra, alias)
		}
	}
	return out, extra
}
