package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// compactIntents distills intents to {id, summary, state, asset_ids, parents,
// yields} so the planner sees both the direction and its LINEAGE — parents (the
// upstream nodes it derived from: facts/intents/findings) and yields (the facts/
// findings it produced) — without pulling full payloads. parentsOf/yieldsOf are
// built from the exploration edges in graph_overview.
func compactIntents(ns []*db.Node, parentsOf, yieldsOf map[int64][]int64) []map[string]any {
	out := make([]map[string]any, 0, len(ns))
	for _, n := range ns {
		var p map[string]any
		_ = json.Unmarshal(n.Payload, &p)
		m := map[string]any{"id": n.ID, "summary": p["summary"], "state": n.State}
		if n.Inherited {
			m["source_task_id"] = n.SourceTaskID
			m["inherited"] = true
		}
		// asset_ids is the structured "which assets this direction covers" signal for
		// dedup; fall back to legacy payload keys (target_ids plural, then target_id
		// single) so intents stored before the rename still surface their anchors.
		if tg, ok := p["asset_ids"]; ok && tg != nil {
			m["asset_ids"] = tg
		} else if tg, ok := p["target_ids"]; ok && tg != nil {
			m["asset_ids"] = tg
		} else if tg, ok := p["target_id"]; ok && tg != nil && tg != "" {
			m["asset_ids"] = []any{tg}
		}
		if ps := parentsOf[n.ID]; len(ps) > 0 {
			m["parents"] = ps // 上流：この意図がどのノードから派生したか（複数の事実がともに1つの意図を生むことがある）
		}
		if ys := yieldsOf[n.ID]; len(ys) > 0 {
			m["yields"] = ys // 下流：この意図がどの事実/発見を生んだか
		}
		out = append(out, m)
	}
	return out
}

// ToolSet exposes the PG-backed dual graph (asset + exploration) to an LLM agent.
// One ToolSet is created per planner/worker run; per-run signals live here.
type ToolSet struct {
	findingRecorder FindingRecorder
	as              *db.AssetStore   // asset store (optional; nil = asset tools not available)
	cs              *db.CompanyStore // company store (optional)
	ts              *db.ExplorationStore
	worker          string
	taskID          int64 // PG tasks.id; 0 when unknown (tests / orchestrator cross-task reads)
	// coverageDisabled mirrors tasks.coverage_enabled=false. Stored inverted so the
	// zero value (all existing ToolSet constructions) means ENABLED — matching the
	// DB default (true). When true: graphOverviewData drops the coverage block, the
	// auto-scope hook (insertAssets) is skipped, and add_task_scope/list_untested_assets
	// are filtered out of the agent's tool list. The scope field stays regardless.
	coverageDisabled bool
	// ownerNode is the exploration node that writes attach to: assets this run
	// touches get anchored to it as lineage/provenance (NOT visibility — the asset
	// graph is global and shared). Worker = its claimed intent; planner = begin root.
	ownerNode int64
	GoalMet   bool
	Reason    string
	writes    WriteCounts
	// killWork, if set, terminates a running work by intent id (engine callback,
	// wired by the planner). nil = the kill_work tool reports unavailable.
	killWork func(intentID int64) error
	// steerWork, if set, queues a mid-run course-correction for the work running an
	// intent id (engine callback, wired by the planner): the worker injects it before
	// its next tool call and re-plans, without being killed. nil = tool unavailable.
	steerWork func(intentID int64, msg string) error
	// enrich, if set, receives async auto-completion triggers (DNS resolve for a
	// domain, HTTP probe for a site). nil = no engine enrichment.
	enrich EnrichTrigger
	// notify, if set, wakes the task's planner after a graph change that should be
	// re-planned promptly (currently: a new hint). nil = no wake (the hint is still
	// stored and read on the next round triggered by other events). debounced.
	notify func()
	// notifyFinding, if set, wakes the task's planner when this run reports a finding,
	// carrying (intentID, summary) so the round can spell out which intent found what.
	// Wired for workers; nil elsewhere → falls back to notify (bare wake).
	notifyFinding func(intentID int64, summary string)
	// resumeTask, if set, revives the task after a graph change that should make a
	// stopped task run again (currently: set_goals adds a goal). It flips a terminal/
	// paused task back to running and (re)starts the engine loops — a plain notify()
	// can't, because the planner's terminal gate swallows wakes. Wired ONLY for the
	// main agent (human steering); nil for the goals decomposer and workers.
	resumeTask func()
	// notifyGoal, if set, wakes the planner AND records ONE "人が目標を N 件追加した：…" trigger
	// for a whole set_goals call (batch-aware — one call, one trigger, not one per goal)
	// so the next round spells out the added goals (instead of the planner having to
	// spot new open goals in the overview). Wired ONLY for the main agent; nil for the
	// goals decomposer (round-0 has no running planner to inform) and workers → those
	// fall back to the bare notify.
	notifyGoal func(texts []string)
	// notifyHint, if set, wakes the planner AND records ONE "人が戦略上のヒントを N 件追加した：…"
	// trigger for a whole add_hint call (batch-aware — one call, one trigger) so the next
	// round is told the round was fired by a new hint and spells the hint out, instead of
	// the planner having to spot it folded into the graph overview. Wired for the main
	// agent + cross-task orchestration; nil elsewhere → falls back to the bare notify.
	notifyHint func(texts []string)
}

// SetNotifyGoal wires the goal-add trigger callback (see ToolSet.notifyGoal). Set only
// by the main-agent chat, so runtime-added goals are announced to the planner by name.
func (t *ToolSet) SetNotifyGoal(fn func([]string)) { t.notifyGoal = fn }

// SetNotifyHint wires the hint-add trigger callback (see ToolSet.notifyHint). Set by
// the main-agent chat and cross-task orchestration, so a runtime-added hint fires a
// planner round announced by name instead of a bare wake.
func (t *ToolSet) SetNotifyHint(fn func([]string)) { t.notifyHint = fn }

// SetResumeTask wires the task-revive callback (see ToolSet.resumeTask). Set only by
// the main-agent chat, so runtime-added goals can pull a finished task back to running.
func (t *ToolSet) SetResumeTask(fn func()) { t.resumeTask = fn }

// SetNotify wires the planner-wake callback (see ToolSet.notify). Set by callers
// that hold the task handle (main-agent chat, cross-task orchestration).
func (t *ToolSet) SetNotify(fn func()) { t.notify = fn }

// SetNotifyFinding wires the finding-wake callback (see ToolSet.notifyFinding).
func (t *ToolSet) SetNotifyFinding(fn func(int64, string)) { t.notifyFinding = fn }

// EnrichTrigger is the enrichment engine seen from the tool layer (see package
// enrich). Kept as an interface here to avoid coupling agent → enrich.
type EnrichTrigger interface {
	ResolveDomain(id int64, host string)
	ProbeSite(id int64, url string)
}

// WriteCounts breaks down what a worker persisted this run, by node kind, so the
// engine can log an accurate "wrote back" summary instead of lumping assets and
// findings under "facts" (record_fact → Facts, insert_assets → Assets,
// report_finding → Findings; each element of a batch counts once).
type WriteCounts struct {
	Facts    int
	Assets   int
	Findings int
}

// Total is every node persisted this run, regardless of kind — the
// "explored but persisted nothing" signal (Total == 0).
func (w WriteCounts) Total() int { return w.Facts + w.Assets + w.Findings }

// String renders the per-kind breakdown for logs, e.g. "事実1 資産25 脆弱性0".
func (w WriteCounts) String() string {
	return fmt.Sprintf("事実%d 資産%d 脆弱性%d", w.Facts, w.Assets, w.Findings)
}

// Writes reports what this run wrote back, split by node kind (so the engine can
// tell "explored but persisted nothing" apart from a completed intent, and log an
// honest breakdown instead of calling assets/findings "facts").
func (t *ToolSet) Writes() WriteCounts { return t.writes }

func NewToolSet(ts *db.ExplorationStore, worker string) *ToolSet {
	return &ToolSet{ts: ts, worker: worker}
}

// SetTaskID sets the PG task id on this ToolSet so that report_finding can
// dual-write to the standalone findings table (which survives task deletion).
func (t *ToolSet) SetTaskID(id int64) { t.taskID = id }

// SetCoverageEnabled records whether this task has the asset-coverage feature on
// (default enabled). Passing false makes graphOverviewData omit the coverage block
// and DropCoverageTools filter the two coverage-only tools out of the agent's tool
// list. It does NOT stop scope accumulation: insertAssets' auto-scope hook runs
// either way, because task_scope is the task's range boundary (the filter basis for
// asset queries), not merely a coverage denominator.
func (t *ToolSet) SetCoverageEnabled(enabled bool) { t.coverageDisabled = !enabled }

// CoverageDisabled reports whether the coverage feature is off for this task.
func (t *ToolSet) CoverageDisabled() bool { return t.coverageDisabled }

// coverageOnlyTools are the LLM tools that only make sense when asset coverage is
// on. When the feature is off they are filtered out of the agent's tool list so
// they neither pollute the prompt nor let the model build a disabled denominator.
// add_task_scope is deliberately NOT here: task_scope is the task's range boundary
// (the filter basis for asset queries), not merely a coverage denominator, so the
// agents that own範囲の定義 keep it either way — in lockstep with insertAssets'
// auto-scope hook, which also runs regardless of the switch.
var coverageOnlyTools = map[string]bool{"list_untested_assets": true}

// DropCoverageTools returns tools with the coverage-only ones removed when this
// task has the feature disabled; otherwise it returns tools unchanged.
func (t *ToolSet) DropCoverageTools(tools []actool.CoreTool) []actool.CoreTool {
	if !t.coverageDisabled {
		return tools
	}
	out := tools[:0:0]
	for _, tool := range tools {
		if coverageOnlyTools[tool.Name()] {
			continue
		}
		out = append(out, tool)
	}
	return out
}

// Cross-task reuse: exported accessors returning the per-task tool logic bound to
// THIS ToolSet's store. Host-side orchestration tools build a ToolSet for an
// arbitrary task, then Call these — so cross-task reads/hint reuse the exact
// same logic as the in-task tools. (readTool ignores ToolContext, so Call(…,nil)
// is safe; add_hint is a writeTool but also doesn't deref the context here.)
func (t *ToolSet) GraphOverviewTool() actool.CoreTool      { return t.graphOverview() }
func (t *ToolSet) ListFindingsTool() actool.CoreTool       { return t.listFindings() }
func (t *ToolSet) GetWorkerTraceTool() actool.CoreTool     { return t.getWorkerTrace() }
func (t *ToolSet) ListWorkerTracesTool() actool.CoreTool   { return t.listWorkerTraces() }
func (t *ToolSet) SearchWorkerTracesTool() actool.CoreTool { return t.searchAllWorkerTraces() }
func (t *ToolSet) NodeDetailTool() actool.CoreTool         { return t.nodeDetail() }
func (t *ToolSet) AddHintTool() actool.CoreTool            { return t.addHint() }

// SetEnrich wires the async enrichment engine (DNS/HTTP auto-completion).
func (t *ToolSet) SetEnrich(e EnrichTrigger) { t.enrich = e }

// SetOwnerNode sets the exploration node that writes anchor to (worker: its
// intent node; planner/main: the begin root). Assets created/referenced while
// ownerNode is set are anchored to it as lineage (not visibility).
func (t *ToolSet) SetOwnerNode(id int64) { t.ownerNode = id }

// anchorOwner records a lineage edge from this run's owner node to an asset
// (no-op if unset). Provenance only — the asset graph is global and shared, so
// this no longer affects which assets a task can read.
func (t *ToolSet) anchorOwner(assetID int64) {
	if t.ts != nil && t.ownerNode > 0 && assetID > 0 {
		_ = t.ts.Anchor(t.ownerNode, assetID)
	}
}

// pid parses an id that may arrive as a JSON number or string ("" / 0 → 0).
func pid(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		v, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		return v
	}
	return 0
}

// pidList parses a list of ids (number|string), dropping zeros/invalids.
func pidList(raw []json.RawMessage) []int64 {
	var out []int64
	for _, r := range raw {
		if v := pid(r); v > 0 {
			out = append(out, v)
		}
	}
	return out
}

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}
func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func intp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func idp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func readTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		ReadOnly:    func(json.RawMessage) bool { return true },
		Concurrent:  func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, acperm.Context) acperm.Decision { return acperm.Allowed() },
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

func writeTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, acperm.Context) acperm.Decision { return acperm.Allowed() },
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

// readExpTool / writeExpTool build a domain tool whose handler dereferences the
// task-bound ExplorationStore. Two ToolSets carry a nil store: the catalog's
// seed-only shell (never called) and the server-level one behind buildDomainReg,
// which the tools table can bind to ANY agent — including ones that never run
// inside a task (auto/pentest/reporter/カスタム agent/脇道の質問). Refusing there
// keeps a mis-bound tool a bad tool call; without the guard it was a nil deref,
// and tool handlers run on the harness's own goroutine, so the panic is out of
// reach of every recover() in the server and kills the whole process.
func (t *ToolSet) readExpTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return readTool(name, desc, schema, t.needExploration(name, run))
}

func (t *ToolSet) writeExpTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return writeTool(name, desc, schema, t.needExploration(name, run))
}

// needExploration wraps a handler so it only runs with an exploration store.
// Tools that degrade more usefully than "unavailable" (report_finding points at
// add_task_hint, set_goals/set_constraints at the task itself) keep their own
// bespoke guard instead.
func (t *ToolSet) needExploration(name string, run func(context.Context, json.RawMessage) (actool.Result, error)) func(context.Context, json.RawMessage) (actool.Result, error) {
	return func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
		if t.ts == nil {
			return actool.Errorf(name + " にはタスクのコンテキスト（探索グラフ）が必要：現在の agent はどのタスクの中でも動いておらず、タスクの探索グラフを取得できないので、このツールは使えない。タスクの中で使うか、task_id 付きのタスクをまたぐ読み取りツール（get_task_node_detail / list_task_findings / get_task_graph など）を使うこと。"), nil
		}
		return run(ctx, in)
	}
}

func jsonResult(v any) (actool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(string(b)), nil
}

// --- read tools (planner + worker) ---

func (t *ToolSet) graphOverview() actool.CoreTool {
	return t.readExpTool("graph_overview",
		"(探索の経路グラフ)探索状況を蒸留した要約：資産の数、インターフェースのないサイト、frontier、発見、hints(人間/メイン agent の戦略上のヒント。意図を生成するときに必ず取り込む)。計画するときはまずこれを呼ぶ。",
		obj(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			return jsonResult(t.graphOverviewData())
		})
}

// graphOverviewData computes the distilled situational snapshot shared by the
// graph_overview tool and the planner's wake-up prompt (which pre-injects it so
// the model needn't spend a turn calling the tool — every plan round starts with
// an empty context and always needs this first).
func (t *ToolSet) graphOverviewData() map[string]any {
	out := map[string]any{}
	// goals summary folded in so the planner needn't call list_goals each round.
	goals, _ := t.ts.ListByKind(db.KindGoal, 100)
	gsum := make([]map[string]any, 0, len(goals))
	for _, g := range goals {
		var p map[string]any
		_ = json.Unmarshal(g.Payload, &p)
		gsum = append(gsum, map[string]any{"id": g.ID, "state": g.State, "text": p["text"]})
	}
	out["goals"] = gsum
	// hints: 人間/メイン agent が add_hint でグラフに付けた戦略上のヒント；folded in so the
	// planner reads them every round when generating intents (さもないと書くだけで読まれない).
	hints, _ := t.ts.ListByKind(db.KindHint, 50)
	hsum := make([]map[string]any, 0, len(hints))
	for _, h := range hints {
		var p map[string]any
		_ = json.Unmarshal(h.Payload, &p)
		hint := map[string]any{"id": h.ID, "state": h.State, "text": p["text"]}
		if findingTrafficBindingEnabled() && p["traffic_refs"] != nil {
			hint["traffic_refs"] = p["traffic_refs"]
		}
		hsum = append(hsum, hint)
	}
	out["hints"] = hsum
	// lineage from the exploration edges: an intent's parents (what it
	// derived_from — possibly several facts combined) and its yields (the
	// facts/findings it produced). factFrom maps a fact → the intent that
	// produced it. This is the relationship layer the flat lists lacked.
	edges, _ := t.ts.Edges(5000)
	parentsOf := map[int64][]int64{}
	yieldsOf := map[int64][]int64{}
	factFrom := map[int64]int64{}
	for _, e := range edges {
		switch e.Rel {
		case db.RelDerivedFrom, db.RelSpawns: // upstream: derived_from (fact/finding/intent→intent) or spawns (origin fact→goal, legacy begin→intent)
			parentsOf[e.To] = append(parentsOf[e.To], e.From)
		case db.RelYields: // intent --yields--> fact/finding
			yieldsOf[e.From] = append(yieldsOf[e.From], e.To)
			factFrom[e.To] = e.From
		}
	}
	// cold-digest §6: members folded into an active digest are shown via cold_digests
	// (below), not the flat recent_* lists. `covered` maps member id → its digest id.
	// §6 render-time revival check: a covered member that has become hot again (a new
	// intent derived from it) must reappear this round — so `hidden` folds a member out
	// only when it is covered AND still cold.
	covered, _ := t.ts.CoveredMembers()
	// Render-time hot set (ancestor of a live intent / fact under a live intent).
	// §6 revival check: a covered member that revived (now hot) must NOT stay folded
	// — hidden() only folds a member out when it is covered AND still cold. Computed
	// every round (cheap for real graph sizes); nil map degrades safely.
	var hotAtRender map[int64]bool
	if cg, _, err := loadColdGraph(t.ts); err == nil {
		hotAtRender = cg.hotSet()
	}
	hidden := func(id int64) bool { _, c := covered[id]; return c && !hotAtRender[id] }
	const openIntentsCap = 30
	fr, _ := t.ts.Frontier(openIntentsCap) // priority DESC, id ASC —— 優先度が最も高い先頭 N 件。本当の総数は frontier_open を参照
	out["open_intents"] = compactIntents(fr, parentsOf, yieldsOf)
	all, _ := t.ts.ListByKind(db.KindIntent, 300)
	var running, recentDone []*db.Node
	for _, n := range all {
		switch n.State {
		case "running":
			running = append(running, n)
		case "done", "blocked", "exhausted":
			if hidden(n.ID) {
				continue // in a cold_digest and still cold — shown via cold_digests (§6.2)
			}
			recentDone = append(recentDone, n) // 新しい順（all は id の降順）。たたんだものは除いてあり、出力時に最新の N 件に切る
		}
	}
	out["running_intents"] = compactIntents(running, parentsOf, yieldsOf)
	// done_intents_total：終わった意図（done/blocked/exhausted）の総数。recent_done_intents と
	// 対になる名前——後者はその最新の窓だけを切り出した見え方にすぎない。2つのキーを並べればそれ自体で説明になる：「見えているのは N/総数」。
	// planner が重複を排除するとき「表示されていない」を「出していない」と取り違えないようにし、プロンプトで別に説明する必要をなくす。
	if dt, err := t.ts.CountFinishedIntents(); err == nil {
		out["done_intents_total"] = dt
	}
	// frontier_open：開いている意図の本当の総数（open_intents はそのうち優先度が最も高い先頭 N 件を切り出した見え方にすぎない）。
	if fo, err := t.ts.CountOpenIntents(); err == nil {
		out["frontier_open"] = fo
	} else {
		out["frontier_open"] = len(fr)
	}
	// findings (confirmed vulns) and facts (worker exploration results) are
	// now distinct node kinds. recent_facts surfaces fact summaries (esp.
	// negative results) so the planner sees them in one call; full content
	// via node_detail(id).
	vulnNodes, _ := t.ts.ListByKind(db.KindFinding, 1000)
	factNodes, _ := t.ts.ListByKind(db.KindFact, 1000) // newest first
	out["findings_total"] = len(vulnNodes)             // 確認済みの脆弱性の総数（目標の判定はこれを見る）。明細は finding_list（最新の窓）を参照
	out["facts"] = len(factNodes)                      // 探索の事実/結論の数（否定の結論を含む）
	// findings はタスクで最も価値の高い成果物 → 概要に最新の窓を付ける（≤10 件。vulnNodes はすでに id の降順で新しい順）。
	// planner がラウンドごとに目標を判定するとき、最近確認された脆弱性がひと目でわかるようにする。全件/それより古いものは list_findings で取得する。
	// 各項目には {id, summary, from_intent?} だけを残す：from_intent はこの脆弱性を生んだ意図。
	// evidence/assets/vulnclass/severity/state などは引き続き list_findings / node_detail(id) で取得できる。
	const findingListCap = 10
	findingList := make([]map[string]any, 0, findingListCap)
	for _, n := range vulnNodes {
		if len(findingList) >= findingListCap {
			break
		}
		var fp map[string]any
		_ = json.Unmarshal(n.Payload, &fp)
		m := map[string]any{"id": n.ID, "summary": fp["summary"]}
		if from := factFrom[n.ID]; from > 0 {
			m["from_intent"] = from // この脆弱性がどの意図から生まれたか
		}
		findingList = append(findingList, m)
	}
	out["finding_list"] = findingList
	// recent_facts：たたんでいない事実のうち最新の窓（≤N。factNodes は id の降順で新しい順）。すでに
	// digest にたたまれてまだコールドなもの（hidden）は cold_digests に回し、ここでは重ねない。各項目は {id, summary, from_intent?,
	// confidence?}。evidence などの詳細は node_detail(id) で取得する。それより古いものは list_facts でめくる。
	const recentFactsCap = 20
	recentFacts := make([]map[string]any, 0, recentFactsCap)
	for _, n := range factNodes {
		if len(recentFacts) >= recentFactsCap {
			break
		}
		if hidden(n.ID) {
			continue // すでに digest にたたまれてまだコールド —— cold_digests を参照
		}
		m := compactNode(n)
		if from := factFrom[n.ID]; from > 0 {
			m["from_intent"] = from // この事実がどの意図から生まれたか
		}
		// confidence を概要に含める：どの結論が inferred にすぎないかを計画者がひと目でわかるようにする（特に否定の結論を
		// 確定事項として扱わないように）。evidence は長めなので node_detail(id) に任せる。
		var fp map[string]any
		if json.Unmarshal(n.Payload, &fp) == nil {
			if c, ok := fp["confidence"].(string); ok && c != "" {
				m["confidence"] = c
			}
		}
		recentFacts = append(recentFacts, m)
	}
	out["recent_facts"] = recentFacts
	// recent_done_intents：たたんでいない終わった意図のうち最新の窓（≤N。recentDone はすでに id の降順）。
	// それより古いものは done_intents_total の数 + node_detail(id) で見る。
	const recentDoneCap = 12
	if len(recentDone) > recentDoneCap {
		recentDone = recentDone[:recentDoneCap]
	}
	out["recent_done_intents"] = compactIntents(recentDone, parentsOf, yieldsOf)
	// cold-digest §6.1: たたんだコールド領域の digest の body を、最新のメンバーの時刻の降順で先頭 N 件取る。切り捨てたより古い digest は
	// 素の id だけを渡し（引き続き expand_digest で展開できる）、コールド領域の唯一の出口が際限なく長くなるのを避ける。
	const coldDigestsCap = 15
	if cds, more := coldDigestsRecent(t.ts, coldDigestsCap); len(cds) > 0 {
		out["cold_digests"] = cds // [{id, body, member_count}] —— body をそのまま読む (§6.1)
		if len(more) > 0 {
			out["cold_digests_more"] = more // 切り捨てたより古い digest の id。expand_digest(id) で展開する
		}
	}
	// the original task (root) so the planner always has it, not just the
	// decomposed goals.
	if description, goal, err := t.ts.Root(); err == nil {
		out["task"] = map[string]any{"description": description, "goal": goal}
	}
	// Direct source tasks are a live, read-only blackboard view. Keep their
	// summaries in a separate field so their intents never enter this task's
	// frontier or get mistaken for locally claimable work.
	out["related_tasks"] = t.relatedTaskOverviews()
	// coverage：大まかな資産のテストのカバレッジの参考——範囲(task_scope)内の資産のうち、fact が触れたものの
	// 割合 + by_type(種類ごとの 総数/テスト済み)。テストしていない具体的な資産を見るには、agent が必要に応じて list_untested_assets を呼んで自分で判断する。タスクのコンテキストにだけある。
	// 資産カバレッジの機能がオフ(coverageDisabled)のとき：host_count(対象ホストの数の感覚)だけを残し、
	// denominator/tested/pct/by_type/note などのカバレッジの尺度は捨てる。コンテキストを汚さず、
	// 隠してある add_task_scope/list_untested_assets に誘導もしないため。
	if t.as != nil && t.ts != nil && t.taskID > 0 {
		{
			m := map[string]any{}
			if !t.coverageDisabled {
				if cov, err := t.as.TaskCoverageWithSources(t.taskID); err == nil {
					m["denominator"] = cov.Denominator
					m["tested"] = cov.Tested
					m["by_type"] = cov.ByType
					m["note"] = "coverage は資産のテストのカバレッジ（インターフェースなど関連するあらゆる資産を含む）で、大まかな見積もりであり参考にとどまる：現在のタスクと直接関連するタスクの scope、事実の錨を含む。関連する scope は読み取り専用。コンテナ型の資産/大量の列挙があると低めに出るので、これをもとにテストし終えたと考えないこと。add_task_scope でこのタスクの範囲を補い、list_untested_assets でテストしていない資産を見られる【通常は list_untested_assets を呼ばず、タスクに沿って進めればよい】。"
					if cov.Denominator == 0 {
						m["pct"] = nil
						m["status"] = "範囲が固定されていない"
					} else {
						m["pct"] = cov.Pct
					}
				}
			}
			if hosts, err := t.as.HostsByTaskWithSources(t.taskID); err == nil {
				// ホストの総数だけを渡し、ホストの一覧を graph_overview に並べることはもうしない（範囲の大きいタスクでは、それはラウンドごとに
				// 繰り返し運ばれる大量の文字列で、計画の判断にとっての価値は限られる）。具体的なホストは必要に応じて list_assets で調べる。
				m["host_count"] = len(hosts)
			}
			if len(m) > 0 {
				out["coverage"] = m
			}
		}
	}
	return out
}

func inheritedMap(m map[string]any, sourceTaskID int64) map[string]any {
	m["source_task_id"] = sourceTaskID
	m["inherited"] = true
	return m
}

const (
	relatedOverviewTotalTextRunes      = 48_000
	relatedOverviewMaxTextPerSource    = 8_000
	relatedOverviewMaxGoalsPerSource   = 8
	relatedOverviewMaxHintsPerSource   = 6
	relatedOverviewMaxFactsPerSource   = 12
	relatedOverviewMaxFindingsPerTask  = 6
	relatedOverviewMaxIntentsPerTask   = 8
	relatedOverviewMaxScopePerSource   = 12
	relatedOverviewMaxDigestsPerSource = 6
)

// overviewTextBudget bounds inherited prompt text while preserving a fair slice
// for every direct source. Full evidence remains available through the on-demand
// read tools, so truncation here does not discard persisted blackboard data.
type overviewTextBudget struct {
	remaining int
	truncated bool
}

func relatedOverviewBudgetForSources(sourceCount int) int {
	if sourceCount <= 0 {
		return 0
	}
	if sourceCount > db.MaxTaskSourceCount {
		sourceCount = db.MaxTaskSourceCount
	}
	perSource := relatedOverviewTotalTextRunes / sourceCount
	if perSource > relatedOverviewMaxTextPerSource {
		perSource = relatedOverviewMaxTextPerSource
	}
	return perSource
}

func (b *overviewTextBudget) take(value any, fieldLimit int) string {
	var text string
	switch value := value.(type) {
	case string:
		text = strings.TrimSpace(value)
	case nil:
		return ""
	default:
		text = strings.TrimSpace(fmt.Sprint(value))
	}
	if text == "" {
		return ""
	}
	if b.remaining <= 0 || fieldLimit <= 0 {
		b.truncated = true
		return ""
	}
	runes := []rune(text)
	limit := fieldLimit
	if limit > b.remaining {
		limit = b.remaining
	}
	if len(runes) > limit {
		b.truncated = true
		if limit == 1 {
			text = "…"
		} else {
			text = string(runes[:limit-1]) + "…"
		}
		runes = []rune(text)
	}
	b.remaining -= len(runes)
	return text
}

func recentTerminalIntents(store *db.ExplorationStore, limit int) []*db.Node {
	if limit <= 0 {
		return []*db.Node{}
	}
	const batch = 300
	cursor := int64(0)
	out := make([]*db.Node, 0, limit)
	for len(out) < limit {
		page, more, err := store.ListByKindPage(db.KindIntent, cursor, batch)
		if err != nil || len(page) == 0 {
			break
		}
		for _, intent := range page {
			switch intent.State {
			case "done", "blocked", "exhausted", "stopped":
				out = append(out, intent)
			}
			if len(out) >= limit {
				break
			}
		}
		if !more {
			break
		}
		cursor = page[len(page)-1].ID
	}
	return out
}

// relatedTaskOverviews distills persistent blackboard state from direct source
// tasks. It intentionally reads each source's local store methods, never its own
// related sources, so inheritance is one level only.
func (t *ToolSet) relatedTaskOverviews() []map[string]any {
	sources, err := t.ts.DirectSourceStores()
	if err != nil {
		return []map[string]any{}
	}
	if len(sources) > db.MaxTaskSourceCount {
		sources = sources[:db.MaxTaskSourceCount]
	}
	perSourceTextBudget := relatedOverviewBudgetForSources(len(sources))
	out := make([]map[string]any, 0, len(sources))
	for _, source := range sources {
		ts := source.Store
		// §2 cross-task: render the source task's OWN folded view — fold out the
		// members it has already folded, and surface its cold_digests read-only.
		hidden := hiddenMembersFor(ts)
		budget := overviewTextBudget{remaining: perSourceTextBudget}
		item := map[string]any{
			"source_task_id": source.Task.TaskID,
			"inherited":      true,
			"task": map[string]any{
				"description": budget.take(source.Task.Description, 800),
				"goal":        budget.take(source.Task.Goal, 800),
				"status":      source.Task.Status,
			},
		}
		stats, statsErr := ts.Stats()

		edges, _ := ts.Edges(5000)
		parentsOf := map[int64][]int64{}
		yieldsOf := map[int64][]int64{}
		factFrom := map[int64]int64{}
		for _, edge := range edges {
			switch edge.Rel {
			case db.RelDerivedFrom, db.RelSpawns:
				parentsOf[edge.To] = append(parentsOf[edge.To], edge.From)
			case db.RelYields:
				yieldsOf[edge.From] = append(yieldsOf[edge.From], edge.To)
				factFrom[edge.To] = edge.From
			}
		}

		goals, _ := ts.ListByKind(db.KindGoal, relatedOverviewMaxGoalsPerSource)
		goalSummary := make([]map[string]any, 0, len(goals))
		for _, goal := range goals {
			var payload map[string]any
			_ = json.Unmarshal(goal.Payload, &payload)
			goalSummary = append(goalSummary, inheritedMap(map[string]any{
				"id": goal.ID, "state": goal.State, "text": budget.take(payload["text"], 400),
			}, source.Task.TaskID))
		}
		item["goals"] = goalSummary

		hints, _ := ts.ListByKind(db.KindHint, relatedOverviewMaxHintsPerSource)
		hintSummary := make([]map[string]any, 0, len(hints))
		for _, hint := range hints {
			var payload map[string]any
			_ = json.Unmarshal(hint.Payload, &payload)
			hintSummary = append(hintSummary, inheritedMap(map[string]any{
				"id": hint.ID, "state": hint.State, "text": budget.take(payload["text"], 400),
			}, source.Task.TaskID))
		}
		item["hints"] = hintSummary

		facts, _ := ts.ListByKind(db.KindFact, relatedOverviewMaxFactsPerSource)
		findings, _ := ts.ListByKind(db.KindFinding, relatedOverviewMaxFindingsPerTask)
		intentNodes, _ := ts.ListByKind(db.KindIntent, 300)
		terminalIntent := make(map[int64]bool, len(intentNodes))
		for _, intent := range intentNodes {
			terminalIntent[intent.ID] = inheritedIntentSummaryState(intent.State)
		}
		item["facts"] = len(facts)
		item["findings"] = len(findings)
		if statsErr == nil {
			item["facts"] = stats[db.KindFact]
			item["findings"] = stats[db.KindFinding]
			if stats[db.KindGoal] > len(goals) || stats[db.KindHint] > len(hints) ||
				stats[db.KindFact] > len(facts) || stats[db.KindFinding] > len(findings) {
				budget.truncated = true
			}
		}
		recentFindings := make([]map[string]any, 0, len(findings))
		for _, finding := range findings {
			entry := inheritedMap(compactFinding(finding), source.Task.TaskID)
			entry["summary"] = budget.take(entry["summary"], 400)
			recentFindings = append(recentFindings, entry)
		}
		item["recent_findings"] = recentFindings
		recentFacts := make([]map[string]any, 0, len(facts))
		for _, fact := range facts {
			if hidden(fact.ID) {
				continue // folded into this source's cold_digests — shown there (§2/§6.2)
			}
			m := inheritedMap(compactNode(fact), source.Task.TaskID)
			m["summary"] = budget.take(m["summary"], 400)
			if from := factFrom[fact.ID]; from > 0 && terminalIntent[from] {
				m["from_intent"] = from
			}
			var payload map[string]any
			if json.Unmarshal(fact.Payload, &payload) == nil {
				if confidence, ok := payload["confidence"].(string); ok && confidence != "" {
					m["confidence"] = confidence
				}
			}
			recentFacts = append(recentFacts, m)
		}
		item["recent_facts"] = recentFacts

		recentDoneRaw := recentTerminalIntents(ts, relatedOverviewMaxIntentsPerTask)
		recentDone := recentDoneRaw[:0] // in-place filter: drop this source's folded intents (§2)
		for _, intent := range recentDoneRaw {
			if hidden(intent.ID) {
				continue
			}
			recentDone = append(recentDone, intent)
		}
		for _, intent := range recentDone {
			intent.Inherited = true
			intent.SourceTaskID = source.Task.TaskID
		}
		intentResults := compactIntents(recentDone, parentsOf, yieldsOf)
		for i, intent := range recentDone {
			intentResults[i]["summary"] = budget.take(intentResults[i]["summary"], 400)
			acts, _, err := ts.ActivityPageForTerminalIntent(intent.ID, 0, 20)
			if err != nil {
				continue
			}
			var resultSummary, textFallback string
			for _, activity := range acts {
				switch activity.Kind {
				case "result":
					resultSummary = activity.Summary
				case "text":
					textFallback = activity.Summary
				}
			}
			if resultSummary == "" {
				resultSummary = textFallback
			}
			if resultSummary != "" {
				intentResults[i]["result_summary"] = budget.take(resultSummary, 800)
			}
		}
		item["recent_intent_results"] = intentResults
		// §2 cross-task: the source task's folded cold region, read-only, newest-member
		// first & capped like the current task's. Members (and overflow digests) are
		// resolvable via expand_digest(id)/node_detail(id), which search source tasks.
		if cds, more := coldDigestsRecent(ts, relatedOverviewMaxDigestsPerSource); len(cds) > 0 {
			for _, cd := range cds {
				cd["inherited"] = true
				cd["source_task_id"] = source.Task.TaskID
			}
			item["cold_digests"] = cds
			if len(more) > 0 {
				item["cold_digests_more"] = more // 切り捨てたより古い digest の id。expand_digest(id) で展開する
			}
		}
		if statsErr == nil {
			item["node_stats"] = stats
		}

		if t.as != nil {
			if scopeRows, err := t.as.ListTaskScope(source.Task.TaskID); err == nil && len(scopeRows) > 0 {
				scopeCount := len(scopeRows)
				if len(scopeRows) > relatedOverviewMaxScopePerSource {
					scopeRows = scopeRows[:relatedOverviewMaxScopePerSource]
					budget.truncated = true
				}
				scope := make([]map[string]any, 0, len(scopeRows))
				for _, row := range scopeRows {
					entry := map[string]any{"kind": row.Kind, "source": budget.take(row.Source, 300)}
					switch {
					case row.Domain != "":
						entry["value"] = budget.take(row.Domain, 400)
					case row.Net != "":
						entry["value"] = budget.take(row.Net, 400)
					case row.Value != "":
						entry["value"] = budget.take(row.Value, 400)
					case row.CompanyID != nil:
						entry["company_id"] = *row.CompanyID
					}
					scope = append(scope, entry)
				}
				item["asset_scope"] = scope
				item["asset_scope_count"] = scopeCount
			}
			if coverage, err := t.as.TaskCoverage(source.Task.TaskID, source.Task.ExplorationID); err == nil {
				item["asset_coverage"] = map[string]any{
					"denominator": coverage.Denominator,
					"tested":      coverage.Tested,
					"pct":         coverage.Pct,
					"by_type":     coverage.ByType,
				}
			}
		}
		if budget.truncated {
			item["summary_truncated"] = true
		}
		out = append(out, item)
	}
	return out
}

func inheritedIntentSummaryState(state string) bool {
	switch state {
	case "done", "blocked", "exhausted", "stopped":
		return true
	default:
		return false
	}
}

// compactNode distills any exploration node to id + summary + state, dropping the
// big detail/evidence (fetch that on demand via node_detail).
func compactNode(n *db.Node) map[string]any {
	var p map[string]any
	_ = json.Unmarshal(n.Payload, &p)
	m := map[string]any{"id": n.ID, "state": n.State, "summary": p["summary"]}
	if n.Inherited {
		inheritedMap(m, n.SourceTaskID)
	}
	return m
}

// compactFinding is compactNode plus the vuln-specific vulnclass/severity.
func compactFinding(n *db.Node) map[string]any {
	var p map[string]any
	_ = json.Unmarshal(n.Payload, &p)
	m := map[string]any{"id": n.ID, "state": n.State, "summary": p["summary"]}
	if n.Inherited {
		inheritedMap(m, n.SourceTaskID)
	}
	if vc, ok := p["vulnclass"]; ok && vc != nil && vc != "" {
		m["vulnclass"] = vc
	}
	if sv, ok := p["severity"]; ok && sv != nil && sv != "" {
		m["severity"] = sv
	}
	return m
}

func (t *ToolSet) listFindings() actool.CoreTool {
	return t.readExpTool("list_findings", "このタスクと直接関連するタスクの【確認済みの脆弱性】を一覧にする(コンパクト：id+task_id+intent_id+vulnclass+severity+要約+状態)。関連するタスクの項目には source_task_id/inherited=true が付き、読み取り専用。ここには脆弱性だけを含む。通常の探索の事実は list_facts、詳細は node_detail(id) を使う。",
		obj(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			f, _ := t.ts.ListByKindWithSources(db.KindFinding, 500)
			if err := t.ts.PopulateFindingTrafficIDs(f); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			intentOf, _ := t.ts.FindingIntentsWithSources() // finding id -> それを生んだ intent id
			taskID := t.taskID
			if taskID <= 0 {
				taskID, _ = t.ts.TaskID()
			}
			out := make([]map[string]any, 0, len(f))
			for _, n := range f {
				m := compactFinding(n)
				if n.FindingID > 0 {
					m["finding_id"], m["finding_node_id"], m["traffic_count"] = n.FindingID, n.ID, n.TrafficCount
				}
				if n.Inherited {
					m["task_id"] = n.SourceTaskID
				} else {
					m["task_id"] = taskID
				}
				if iid, ok := intentOf[n.ID]; ok {
					m["intent_id"] = iid
				}
				out = append(out, m)
			}
			return jsonResult(out)
		})
}

// factsPageSize is the default page size for list_facts. Facts pile up on long
// tasks; returning all of them at once (the old behaviour) could blow up the
// context, so default to the newest page and let the agent page/filter for more.
const factsPageSize = 20

func (t *ToolSet) listFacts() actool.CoreTool {
	return t.readExpTool("list_facts", "このタスクと直接関連するタスクの【探索の事実/結論】をページ分けして新しい順に一覧にする(コンパクト：id+要約+状態。要約が長すぎると切り詰める。全文は node_detail(id))。引数はどれも任意：limit(既定 20、上限 100)、before(カーソル。前のページが返した next_before を渡すと、より古いページを取得する。省略/0=最新のページ)、q(要約のキーワードで絞り込む)。戻り値は {facts, total, has_more, next_before}：total は絞り込み後の総数で、has_more=true なら next_before でページを送り続ける。関連するタスクの項目には source_task_id/inherited=true が付き、読み取り専用。脆弱性は list_findings で見る。",
		obj(map[string]any{
			"limit":  intp("返す件数。既定 20、上限 100"),
			"before": intp("ページ分けのカーソル：id がこの値より小さい、より古い事実だけを返す。省略または 0 = 最新のページ"),
			"q":      str("事実の要約のキーワードで絞り込む（大文字小文字を区別しない）。省略 = 絞り込まない"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Limit  int    `json:"limit"`
				Before int64  `json:"before"`
				Q      string `json:"q"`
			}
			_ = json.Unmarshal(in, &a)
			limit := a.Limit
			if limit <= 0 {
				limit = factsPageSize
			}
			if limit > 100 {
				limit = 100
			}
			f, hasMore, total, err := t.ts.ListByKindPageWithSources(db.KindFact, a.Before, limit, strings.TrimSpace(a.Q))
			if err != nil {
				return actool.Result{}, err
			}
			out := make([]map[string]any, 0, len(f))
			for _, n := range f {
				out = append(out, compactFact(n))
			}
			res := map[string]any{"facts": out, "total": total, "has_more": hasMore}
			if hasMore && len(f) > 0 {
				res["next_before"] = f[len(f)-1].ID // これを返せば次のページ(より古いもの)を取得できる
			}
			return jsonResult(res)
		})
}

// factSummaryMax caps a fact summary in list_facts output. Facts carry one-line
// conclusions, but nothing enforces brevity; a runaway summary must not bloat a
// whole page. Full text stays available via node_detail(id).
const factSummaryMax = 160

// compactFact is compactNode with the summary rune-capped for list_facts, so a
// page of facts stays bounded regardless of how long any single summary grew.
func compactFact(n *db.Node) map[string]any {
	m := compactNode(n)
	if s, ok := m["summary"].(string); ok && len([]rune(s)) > factSummaryMax {
		m["summary"] = string([]rune(s)[:factSummaryMax]) + "…"
		m["summary_truncated"] = true
	}
	return m
}

func (t *ToolSet) nodeDetail() actool.CoreTool {
	return t.readExpTool("node_detail", "このタスクまたは直接関連するタスクの【探索グラフのノード】の完全な内容を id で取得する。引き継いだノードには source_task_id/inherited=true が付き、読み取り専用。list_facts/list_findings/graph_overview が返した探索ノードの id に限る。資産には list_assets/asset_neighbors を使うこと。",
		obj(map[string]any{"id": idp("探索グラフのノードの id(資産の id ではない)")}, "id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				ID json.RawMessage `json:"id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.ID)
			if id <= 0 {
				return actool.Errorf("id は必須"), nil
			}
			n, err := t.ts.GetNodeWithSources(id)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if n == nil {
				return actool.Errorf(fmt.Sprintf("探索ノード %d が見つからない。調べたいのが資産なら list_assets / asset_neighbors を使うこと（資産と探索ノードは別の id 空間で、資産の id は node_detail に渡せない）。", id)), nil
			}
			if err := t.ts.PopulateFindingTrafficIDs([]*db.Node{n}); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(n) // full payload incl. detail / evidence, plus explicit finding IDs
		})
}

// --- planner write tools ---

// intentItem は add_intent の一括/単発の1本の探索の方向。
type intentItem struct {
	Summary   string            `json:"summary"`
	AssetIDs  []json.RawMessage `json:"asset_ids"`
	ParentIDs []json.RawMessage `json:"parent_ids"`
	Priority  int               `json:"priority"`
}

// addOneIntent は意図のノードを1つ作って上流の系譜につなぎ、id を返す。
// 制約：意図は確認済みの知識にしか錨を下ろせない——各 parent_id は既存の fact/finding
// ノードでなければならない（別の意図/目標/ヒントには付けられない）。最上位の新しい方向は parent_ids を空にし、最後の受け皿として origin fact につなぐ。
// これで「どの意図も fact ノードにつながり、何もないところからの計画でなく発見に駆動される」ことを作成の経路で強制する。
func (t *ToolSet) addOneIntent(it intentItem) (int64, error) {
	if strings.TrimSpace(it.Summary) == "" {
		return 0, fmt.Errorf("summary は空にできない")
	}
	// まず錨を検証する（ノードを作る前に。不正な錨で孤児の意図が残るのを避ける）。
	parents := pidList(it.ParentIDs)
	for _, pidv := range parents {
		n, err := t.ts.GetNodeWithSources(pidv)
		if err != nil || n == nil {
			return 0, fmt.Errorf("parent_id %d はこのタスクにも直接関連するタスクにも存在しない：parent_ids は既存の【事実(fact)/発見(finding)】ノードの id でなければならない。最上位の新しい方向なら parent_ids を空にすること", pidv)
		}
		if n.Kind != db.KindFact && n.Kind != db.KindFinding {
			return 0, fmt.Errorf("parent_id %d は %q ノードなので、意図の錨にできない：意図は確認済みの【事実(fact)/発見(finding)】にしか錨を下ろせず、意図/目標/ヒントには付けられない。最上位の新しい方向なら parent_ids を空にすること", pidv, n.Kind)
		}
	}
	priority := it.Priority
	if priority == 0 {
		priority = 5
	}
	anchors := pidList(it.AssetIDs)
	// 資産の遮断：意図に結びつけた資産がシステムの資産遮断ルールに一致したら、その意図を出すことを禁じる。
	if t.as != nil && len(anchors) > 0 {
		hits, err := t.as.CheckAssetsIntercept(t.taskID, anchors)
		if err != nil {
			return 0, fmt.Errorf("資産の遮断の検証に失敗：%w", err)
		}
		if len(hits) > 0 {
			var b strings.Builder
			fmt.Fprintf(&b, "意図「%s」に結びつけた資産がテスト範囲の検証を通らなかったので、関係する資産へのテストを止めること：", it.Summary)
			for _, h := range hits {
				fmt.Fprintf(&b, "\n - %s", h.Describe())
			}
			return 0, fmt.Errorf("%s", b.String())
		}
	}
	payload := map[string]any{"summary": it.Summary}
	if len(anchors) > 0 {
		payload["asset_ids"] = anchors
	}
	id, err := t.ts.AddIntent(payload, priority, anchors, "planner")
	if err != nil {
		return 0, err
	}
	// upstream lineage: link each (validated) fact/finding parent → this intent, so
	// "multiple facts combine into one new intent" is expressible.
	for _, parent := range parents {
		_ = t.ts.Link(parent, db.RelDerivedFrom, id)
	}
	// a top-level intent (no explicit parent) connects to the origin fact, so every
	// intent still traces back to a fact node — at task start the only fact is the
	// origin, and the first intents derive from it.
	if len(parents) == 0 {
		if origin, _ := t.ts.OriginFactID(); origin > 0 {
			_ = t.ts.Link(origin, db.RelDerivedFrom, id)
		}
	}
	return id, nil
}

func (t *ToolSet) addIntent() actool.CoreTool {
	return t.writeExpTool("add_intent", "【探索の方向】を生成して frontier に書き込み、探索の経路につなぐ。意図は開かれた探索の方向であって固定の種類ではない——summary で何を探索/検証/悪用するかを1文で自由に述べる。\n"+
		"★まとめて出すことを優先：1ラウンドで選んだ複数の新しい方向は intents 配列に入れて一度に提出する（1件ずつ呼ぶより往復が少ない）。ids 配列を返し、intents と同じ長さ・同じ順序になる（失敗した項目の id=0。詳細は errors を参照）。単発なら intents を省いて最上位の summary を直接渡す。",
		obj(map[string]any{
			"intents":    map[string]any{"type": "array", "description": "【こちらを優先して使う】追加する探索の方向の配列で、順に処理する。各要素のフィールドは下の最上位のフィールド（summary/asset_ids/parent_ids/priority）と同じ。返す ids はこの配列と同じ長さ・同じ順序。", "items": map[string]any{"type": "object"}},
			"summary":    str("[単発] この探索の方向を1文で述べる：何をするか+なぜか。方向がはっきり書けていればよく、資産の id には依存しない。"),
			"asset_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "この方向でテスト/攻撃する【対象の資産の id】（**できるだけ渡す**。0/1/複数。list_assets が返す資産の id で、探索ノードの id ではない）：この探索の方向がどの資産（サイト/インターフェース/引数/ホストなど）を対象にするか。方向がいずれかの具体的な資産を中心にしているなら必ず渡す——それは「この探索がどの対象を攻めるか」の構造化された印で、カバレッジの重複排除と、意図を資産の経路につなぐのに使う。純粋に全体的な偵察で、具体的な対象の資産が本当にないときだけ空にする。"},
			"parent_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "上流の錨の id（任意。0/1/複数）：この方向がどの【確認済みの事実(fact)/発見(finding)】を組み合わせて得られたか。**既存の fact/finding ノードの id しか入れられず、意図/目標/ヒントは入れられない**——意図は確認済みの知識に錨を下ろさなければならず、何もないところからの計画でなく発見に駆動される。複数の事実がともに新しい意図を生んだなら複数渡す。最上位の新しい偵察の方向なら空にする（タスクの起点 origin fact に自動で付く）。"},
			"priority":   intp("優先度 0-10。既定は 5"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Intents    []intentItem `json:"intents"`
				intentItem              // 単発モード：最上位の summary/asset_ids/parent_ids/priority
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Intents) > 0
			items := a.Intents
			if !batch {
				items = []intentItem{a.intentItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			createdAny := false
			for i, it := range items {
				id, err := t.addOneIntent(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				createdAny = true
			}

			// 人がメイン agent を通じて意図を直接投げた → タスクがすでに done（open の目標がない goalless の分岐）なら、
			// running に戻して worker がこの意図を受け取って実行できるようにする。resumeTask はメイン agent の Chat からだけつながる
			// (SetResumeTask)。planner の ToolSet は nil なので、planner 自身が add_intent を呼んだときこの部分は
			// no-op で、通常の意図の生成には影響しない。意図のノードは上ですでに作ってある(open)ので、復活時に空と誤判定されることはない。
			if createdAny && t.resumeTask != nil {
				t.resumeTask()
			}

			if !batch { // 単発：元の戻り値を保つ
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("intent created: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

func (t *ToolSet) listGoals() actool.CoreTool {
	return t.readExpTool("list_goals", "このタスクの目標のノードとその状態（open/met）を一覧にし、達成したかの判断に使う。",
		obj(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			g, _ := t.ts.ListByKind(db.KindGoal, 100)
			return jsonResult(g)
		})
}

func (t *ToolSet) proveGoal() actool.CoreTool {
	return t.writeExpTool("prove_goal", "ある発見/事実がある目標の達成を証明したと判断したときに呼ぶ：証拠のノードを目標のノードにつなぎ、目標を met にする。",
		obj(map[string]any{
			"goal_id":     idp("目標のノードの id"),
			"evidence_id": idp("それを証明する発見/事実のノードの id"),
			"reason":      str("なぜこの証拠がその目標を満たすのか"),
		}, "goal_id", "evidence_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				GoalID     json.RawMessage `json:"goal_id"`
				EvidenceID json.RawMessage `json:"evidence_id"`
				Reason     string          `json:"reason"`
			}
			_ = json.Unmarshal(in, &a)
			goal, ev := pid(a.GoalID), pid(a.EvidenceID)
			if goal == 0 || ev == 0 {
				return actool.Errorf("goal_id と evidence_id は必須"), nil
			}
			goalNode, err := t.ts.GetNode(goal)
			if err != nil || goalNode == nil || goalNode.Kind != db.KindGoal {
				return actool.Errorf("goal_id はこのタスクの目標のノードでなければならない（関連するタスクの目標は読み取り専用）"), nil
			}
			evidenceNode, err := t.ts.GetNodeWithSources(ev)
			if err != nil || evidenceNode == nil || (evidenceNode.Kind != db.KindFact && evidenceNode.Kind != db.KindFinding) {
				return actool.Errorf("evidence_id はこのタスクまたは直接関連するタスクの事実/脆弱性のノードでなければならない"), nil
			}
			_ = t.ts.Link(ev, db.RelProves, goal)
			_ = t.ts.SetNodeState(goal, "met")
			// 目標を1つ met にするたびに、このタスクの【すべての目標】が met になったかを確かめる。そうなら自動で
			// タスクを完了と判定し（GoalMet にする）、モデルが明示的に goal_met を呼ぶことにはもう頼らない。
			if goals, err := t.ts.ListByKind(db.KindGoal, 1000); err == nil && len(goals) > 0 {
				allMet := true
				for _, g := range goals {
					if g.State != "met" {
						allMet = false
						break
					}
				}
				if allMet {
					t.GoalMet = true
					t.Reason = fmt.Sprintf("%d 件の目標がすべて met（最後は goal %d で起動）", len(goals), goal)
					return actool.Text(fmt.Sprintf("goal %d marked met。このタスクのすべての目標が達成されたので、タスクを自動で完了と判定した", goal)), nil
				}
			}
			return actool.Text(fmt.Sprintf("goal %d marked met", goal)), nil
		})
}

func (t *ToolSet) goalMet() actool.CoreTool {
	return writeTool("goal_met", "【ただちにタスク全体を終える】——タスクの【すべての目標が本当に達成され、全体として締めくくった】と確認したときにだけ呼ぶ（タスクの【全体】の完了であることに注意。目標のうちの1つ/flag の1つ/脆弱性の1つを達成しただけでは【数えない】——その場合は prove_goal でその目標に印を付ければよい）。⚠️これは「このラウンドの計画を終える」ためのものではない：このラウンドで出す新しい意図がない、または worker の産出を待っているときは、どちらも【そのままこのラウンドを終えればよく、このツールを呼んではならない】（意図 0 個はまったく正常）。通常の判定では prove_goal で目標を1つずつ証明することを優先する。goal_met は1つずつの証明を飛ばし、全体から直接締めくくる手段にすぎない。",
		obj(map[string]any{"reason": str("達成の理由（目標が本当に達成された証拠でなければならず、「このラウンドに新しい方向がない」のようなラウンドを終える理由であってはならない）")}, "reason"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct{ Reason string }
			_ = json.Unmarshal(in, &a)
			t.GoalMet = true
			t.Reason = a.Reason
			return actool.Text("acknowledged: goal marked met"), nil
		})
}

// --- worker write tools ---

func (t *ToolSet) addFinding() actool.CoreTool {
	return writeTool("report_finding", "確認した脆弱性を記録し、evidence でコマンドの出力やログなどの確かめられる証拠を示す。タスクのコンテキストでは現在の intent_id を渡す。返される finding_id は独立した脆弱性の記録 ID、finding_node_id は探索ノードの ID（1行目にそのノードの番号を残す）。", obj(map[string]any{
		"vulnclass": str("脆弱性の種類"), "name": str("脆弱性の名前"), "severity": str("critical|high|medium|low"), "summary": str("発見の要約"),
		"intent_id": idp("現在のタスクの意図の id"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "影響を受ける資産の id"},
		"evidence":         str("証拠/PoC の文"),
		"evidence_hint_id": idp("任意：このタスクでこの脆弱性に対応するヒントのノード ID。その構造化された traffic_refs を自動で引き継ぐ。引き継いだヒントや他の脆弱性のヒントは参照できない"),
		"traffic_refs": map[string]any{"type": "array", "description": "任意。HTTP/HTTPS の脆弱性は、まず検索してリクエスト/レスポンスが本当に脆弱性の結論を裏づけることを1件ずつ確かめてから、再現の順に本物の ID を入れる。TCP など HTTP 以外の脆弱性、取得していない、または正確な記録が見つからないときは省くか [] を渡し、それでも報告は妨げない。evidence で理由を説明し、他の確かめられる証拠を示せる。ID を推測したり、ドメイン/時刻で関連を推定したり、パケットを補うためだけに探査を繰り返したりしないこと。用途 baseline 通常の対照 / proof 脆弱性の証明 / verification 補足の検証 / supporting 補助の証拠。",
			"items": obj(map[string]any{"traffic_id": str("traffic_search が返した本物の通信の ID"), "role": map[string]any{"type": "string", "enum": []string{"baseline", "proof", "verification", "supporting"}}, "note": str("その通信がどう脆弱性の結論を裏づけるか")}, "traffic_id")},
	}, "vulnclass", "severity", "summary"), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
		var a struct {
			VulnClass, Name, Severity, Summary, Evidence string
			IntentID                                     json.RawMessage   `json:"intent_id"`
			AssetIDs                                     []json.RawMessage `json:"asset_ids"`
			TrafficRefs                                  []db.TrafficRef   `json:"traffic_refs"`
			EvidenceHintID                               json.RawMessage   `json:"evidence_hint_id"`
		}
		if err := json.Unmarshal(in, &a); err != nil {
			return actool.Errorf(err.Error()), nil
		}
		if t.ts == nil {
			return actool.Errorf("report_finding にはタスクのコンテキストが必要。プラットフォームの会話からは add_task_hint で対応するタスクに脆弱性を引き継ぎ、ヒントに既存の traffic_refs を添えて、タスクの Agent に登録させること。登録済みの脆弱性は bind_finding_traffic で後から結びつけられる。"), nil
		}
		// Auto-binding off: ignore the evidence params instead of rejecting the call.
		// stripTrafficParameters already removes them from the advertised schema, but
		// models routinely emit fields anyway — failing here would discard a confirmed
		// finding over a stray parameter. The success path below reports evidence_status
		// "not_bound" with the "オフになっているので、画面で人が関連付けられる" note, which is what the caller needs.
		if !findingTrafficBindingEnabled() {
			a.TrafficRefs, a.EvidenceHintID = nil, nil
		}
		if len(a.EvidenceHintID) > 0 && pid(a.EvidenceHintID) <= 0 {
			return actool.Errorf("evidence_hint_id は有効なヒントのノード ID でなければならない。引き継ぎのヒントがなければ省くこと"), nil
		}
		refs, err := t.findingRefsFromHint(pid(a.EvidenceHintID), a.TrafficRefs)
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		input := db.RecordFindingInput{TaskID: t.taskID, ExplorationID: t.ts.ID(), IntentID: pid(a.IntentID), VulnClass: a.VulnClass, Name: a.Name, Severity: a.Severity, Summary: a.Summary, Evidence: a.Evidence, Worker: t.worker, AssetIDs: pidList(a.AssetIDs)}
		var recorded *db.RecordedFinding
		if t.findingRecorder != nil {
			recorded, err = t.findingRecorder.Record(ctx, input, refs)
		} else if len(refs) > 0 {
			return actool.Errorf("通信の証拠のストレージが使えないので、脆弱性は登録しなかった"), nil
		} else {
			recorded, err = t.ts.RecordFinding(ctx, input)
		}
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		if t.notifyFinding != nil {
			iid := input.IntentID
			if iid <= 0 {
				iid = t.ownerNode
			}
			t.notifyFinding(iid, a.Summary)
		} else if t.notify != nil {
			t.notify()
		}
		t.writes.Findings++
		// Keep the first line's node-ID contract for existing reporter triggers.
		for i := range recorded.Traffic.Bindings {
			recorded.Traffic.Bindings[i].Snapshot.ReqHead = ""
			recorded.Traffic.Bindings[i].Snapshot.RespHead = ""
		}
		result := struct {
			*db.RecordedFinding
			EvidenceStatus string `json:"evidence_status"`
			EvidenceNote   string `json:"evidence_note,omitempty"`
		}{RecordedFinding: recorded, EvidenceStatus: "bound"}
		if len(recorded.Traffic.Bindings) == 0 {
			result.EvidenceStatus = "not_bound"
			result.EvidenceNote = "脆弱性は保存したが、通信は結びつけていない。TCP/パケットのない場合は通常どおり続けてよい。確かめた HTTP の通信がすでにあれば、使える bind_finding_traffic か脆弱性のページで後から結びつけ、証拠の引き継ぎを終えること。脆弱性を重ねて作らないこと。"
			if !findingTrafficBindingEnabled() {
				result.EvidenceNote = "脆弱性は保存した。Agent による通信の自動の結びつけはオフになっているので、画面で人が通信を関連付けられる。"
			}
		}
		raw, _ := json.Marshal(result)
		return actool.Text(fmt.Sprintf("finding recorded: %d\n%s", recorded.NodeID, raw)), nil
	})
}

// recordFact writes a general exploration RESULT/conclusion (not a vuln, not a
// new asset) into the EXPLORATION graph, chained to the intent that produced it.
// This is the home for observations and — importantly — negative results
// ("port closed", "param not injectable", "no login found"). Such conclusions
// must NOT be stuffed into the asset graph via upsert_asset.
// factItem は record_fact の一括/単発の1件の事実。
type factItem struct {
	Summary    string            `json:"summary"`
	Detail     string            `json:"detail"`
	Evidence   string            `json:"evidence"`   // 1行の重要な証拠（コマンド+重要な出力の行）。結論を裏づけ、事後の照合をしやすくする
	Confidence string            `json:"confidence"` // observed（直接見た）| inferred（現象から推測した）
	IntentID   json.RawMessage   `json:"intent_id"`
	AssetIDs   []json.RawMessage `json:"asset_ids"`
}

// recordOneFact は fact のノードを1つ書いて意図につなぐ（intent→yields→fact）。defaultIntent は
// 一括のときの既定の意図（この項目に intent_id がないときに使う）。
func (t *ToolSet) recordOneFact(it factItem, defaultIntent int64) (int64, error) {
	if strings.TrimSpace(it.Summary) == "" {
		return 0, fmt.Errorf("summary は空にできない")
	}
	payload := map[string]any{"summary": it.Summary}
	if it.Detail != "" {
		payload["detail"] = it.Detail
	}
	if e := strings.TrimSpace(it.Evidence); e != "" {
		payload["evidence"] = e
	}
	if c := strings.TrimSpace(it.Confidence); c != "" {
		payload["confidence"] = c
	}
	intent := pid(it.IntentID)
	if intent <= 0 {
		intent = defaultIntent
	}
	if intent > 0 {
		node, err := t.ts.GetNode(intent)
		if err != nil || node == nil || node.Kind != db.KindIntent {
			return 0, fmt.Errorf("intent_id はこのタスクの意図でなければならない（関連するタスクの意図は読み取り専用）")
		}
	}
	// a fact is its OWN node kind (distinct from a vuln finding).
	id, err := t.ts.AddNode(db.KindFact, payload, 5, "confirmed", t.worker, pidList(it.AssetIDs))
	if err != nil {
		return 0, err
	}
	if intent > 0 {
		_ = t.ts.Link(intent, db.RelYields, id) // chain: intent -> fact
	}
	t.writes.Facts++
	return id, nil
}

func (t *ToolSet) recordFact() actool.CoreTool {
	return t.writeExpTool("record_fact", "探索の【事実/結論】を探索グラフに書き込み、それを生んだ意図（intent_id）につなぐ。探索の結果の記録に使う——フィンガープリント/列挙などの【肯定の結論】と、「ポートが閉じている」/「引数はインジェクションできない」/「ログインの入口が見つからない」などの【否定の結論】を含む。\n"+
		"⚠️1回の探索の複数の観察は【1件の事実にまとめ】、複数に分けないこと。1件の事実にまとめられるものはできるだけ1件で表す：summary=今回の結論をまとめた1文、detail=関係する細部（具体的な項目を複数含めてよい）。例：フィンガープリントの意図→事実1件 {summary:'X サイトの技術スタックとレスポンスの特徴を識別した', detail:'nginx 1.25 / Vue3 / 200 / title=.. / body_len=..'}。ステータスコード、フィンガープリント、タイトルをそれぞれ1件ずつ記録するのではない。1本の意図が生む事実は通常1件だけで、細かく分けすぎるとグラフが際限なく膨らむ。\n"+
		"★facts 配列は一度に複数の【互いに異なる】結論を書くのに使う（各項目は intent_id を省いてよく、既定では最上位の intent_id を使う）。ids 配列を返し、facts と同じ長さ・同じ順序になる。\n"+
		"⚠️ツールの出力で【本当に見た】結論だけを書き、想像で補わないこと。evidence と confidence は不正確な結論がグラフを汚すのを防ぐためのもの：\n"+
		"  · evidence=この結論を裏づける【1行の】重要な証拠（コマンド+最もよく証明する1、2行の出力）。**必ず簡潔に**——細部はすでに detail にあるので、ここに大量の出力を貼らない。\n"+
		"  · confidence=observed（出力で直接見た）| inferred（現象から推測した）。\n"+
		"  · **否定の結論**（インジェクションできない/ポートが閉じている/入口が見つからないなど）は「観察 + 試しの読み」だけを書く——実際に何を見たかを述べ、方向を捨てるかどうかは計画者が全体を総合して決める。evidence は必ず付け、手段をやり尽くしていない、または証拠が弱い（1回だけ探った、そう見える、を含む）なら inferred、本当にやり尽くして直接見たときだけ observed を付ける。",
		obj(map[string]any{
			"facts":      map[string]any{"type": "array", "description": "【異なる結論が複数あるときに使う】事実の配列で、要素のフィールドは下の最上位のフィールド（summary/detail/evidence/confidence/intent_id/asset_ids）と同じ。intent_id を省くと最上位の intent_id を使う。返す ids はこの配列と同じ長さ・同じ順序。", "items": map[string]any{"type": "object"}},
			"summary":    str("今回の探索の結論を【まとめた1文】（detail を要約したもの）"),
			"intent_id":  idp("この事実を生んだ意図の id（あなたが受け取った意図。一括のときは各項目の既定になる）"),
			"detail":     str("この事実に関係する細部：今回の探索の複数の観察をすべてここに書く"),
			"evidence":   str("【1行の】重要な証拠：コマンド + 結論を最もよく証明する1、2行の出力。必ず簡潔にし、大量の出力を貼らない（細部は detail に入れる）。"),
			"confidence": str("observed（出力で直接見た）| inferred（現象から推測した）。否定の結論は必ずありのままに付ける。"),
			"asset_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "関係する資産の id（任意。0/1/複数）：この事実がどの資産に関わるか"},
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Facts    []factItem `json:"facts"`
				factItem            // 単発モード + 一括の既定の intent_id
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Facts) > 0
			items := a.Facts
			if !batch {
				items = []factItem{a.factItem}
			}
			defaultIntent := pid(a.factItem.IntentID) // 最上位の intent_id = 一括の既定

			ids := make([]int64, len(items))
			errs := map[string]string{}
			for i, it := range items {
				id, err := t.recordOneFact(it, defaultIntent)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
			}

			if !batch { // 単発：元の戻り値を保つ
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("fact recorded: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

type hintItem struct {
	Text        string            `json:"text"`
	AssetIDs    []json.RawMessage `json:"asset_ids"`
	TrafficRefs []db.TrafficRef   `json:"traffic_refs"`
}

// addOneHint はヒントのノード(active/human)を探索グラフに1つ付け、資産に錨を下ろせる。id を返す。
func (t *ToolSet) addOneHint(it hintItem) (int64, error) {
	if len(it.TrafficRefs) > 0 && !findingTrafficBindingEnabled() {
		return 0, fmt.Errorf("Agent による通信の自動の結びつけはオフになっているので、traffic_refs 付きのヒントは保存しなかった。システム設定でオンにするか、文だけを引き継ぐこと")
	}
	if strings.TrimSpace(it.Text) == "" {
		return 0, fmt.Errorf("text は空にできない")
	}
	var anchors []int64
	for _, raw := range it.AssetIDs {
		if tid := pid(raw); tid > 0 {
			anchors = append(anchors, tid)
		}
	}
	refs, err := db.NormalizeTrafficRefs(it.TrafficRefs)
	if err != nil {
		return 0, err
	}
	payload := map[string]any{"text": it.Text}
	if len(refs) > 0 {
		payload["traffic_refs"] = refs
	}
	// planner を起こすのはここで1件ずつはしない——addHint が一括で書き終えてからまとめて1回起動する（ヒントの文を添えて）。
	// 1回の add_hint の複数のヒントが1件ずつ planner の起動の行を埋め尽くすのを避ける。
	return t.ts.AddNode(db.KindHint, payload, 0, "active", "human", anchors)
}

type goalItem struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass"`
}

// addOneGoal は目標のノード(open)を探索グラフに1つ付ける：タスクの根(origin fact, rel spawns)につなぐ。
// origin は t.worker(既定は system)：goals の分解器が書いたものは "goals"、メイン agent の実行時は
// "human"。planner を起こすのは setGoals が一括で書き終えてからまとめて行う(下を参照)。ここは保存だけを受け持つ。
func (t *ToolSet) addOneGoal(it goalItem) (int64, error) {
	text := strings.TrimSpace(it.Text)
	if text == "" {
		return 0, fmt.Errorf("text は空にできない")
	}
	payload := map[string]any{"text": text}
	if vc := strings.TrimSpace(it.VulnClass); vc != "" {
		payload["vulnclass"] = vc
	}
	origin := t.worker
	if origin == "" {
		origin = "system"
	}
	id, err := t.ts.AddNode(db.KindGoal, payload, 0, "open", origin, nil)
	if err != nil {
		return 0, err
	}
	if of, _ := t.ts.OriginFactID(); of > 0 && id > 0 {
		_ = t.ts.Link(of, db.RelSpawns, id) // goals descend from the task root (origin fact)
	}
	return id, nil
}

// setGoals は【このタスク】に探索の目標(goal ノード)を追加する。目標の分解器の提出ツールでもあり、メイン
// agent の実行時に目標を補うツールでもある——同じ管理されたツールで、web 側で説明/schema を変えたり、agent ごとに結びつけたりできる。
func (t *ToolSet) setGoals() actool.CoreTool {
	return writeTool("set_goals",
		"【このタスク】に探索の目標(goal)を追加する。目標=最終的に引き渡せる/確かめられる結果であり、攻撃の手順や偵察の動作ではない。\n"+
			"★まとめて出すことを優先：複数の目標は goals 配列に入れて一度に提出する。ids を返し、それと同じ長さ・同じ順序になる(失敗した項目の id=0。詳細は errors を参照)。単発なら goals を省いて最上位の text を直接渡す。\n"+
			"vulnclass は任意：対応する脆弱性の種類(SQLi/IDOR など)。ビジネスロジック系の目標は空にする。目標を達成したかはシステムが判定して met を付ける。このツールは追加だけを受け持つ。",
		obj(map[string]any{
			"goals":     map[string]any{"type": "array", "description": "【こちらを優先して使う】追加する目標の配列で、順に処理する。各要素：text(必須。独立して確かめられる最終的な目標1つ)+ vulnclass(任意)。返す ids はこの配列と同じ長さ・同じ順序。", "items": map[string]any{"type": "object"}},
			"text":      str("[単発] 独立して確かめられる最終的な目標1つ"),
			"vulnclass": str("[単発] 対応する脆弱性の種類(はっきりしているなら)。SQLi/IDOR など。ビジネスロジックの目標は空でよい"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("set_goals は有効になっていない: ExplorationStore が初期化されていない"), nil
			}
			var a struct {
				Goals    []goalItem `json:"goals"`
				goalItem            // 単発モード：最上位の text/vulnclass
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Goals) > 0
			items := a.Goals
			if !batch {
				items = []goalItem{a.goalItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			var addedTexts []string
			for i, it := range items {
				id, err := t.addOneGoal(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				addedTexts = append(addedTexts, strings.TrimSpace(it.Text))
			}
			if len(addedTexts) > 0 {
				// planner を起こす(一括で1回)。notifyGoal を優先：1回の set_goals で「人が目標を
				// N 件追加した：…」の起動を1件記録し、1件ずつ埋め尽くさない。分解器/worker にはこのコールバックがない → 素の notify に戻る(分解器の
				// round-0 は notify もつないでおらず、何もしない。このとき planner はまだ起動していないため)。
				switch {
				case t.notifyGoal != nil:
					t.notifyGoal(addedTexts)
				case t.notify != nil:
					t.notify()
				}
				// メイン agent の実行時に目標を追加した → 完了/一時停止したタスクを running に戻して続ける(終端の門は
				// 通常の notify を飲み込むので、明示的に復活させる必要がある)。このコールバックをつないでいるのは mainagent だけ。分解器/worker は nil。
				if t.resumeTask != nil {
					t.resumeTask()
				}
			}

			if !batch { // 単発：元の戻り値を保つ
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("goal added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

type constraintItem struct {
	Text string `json:"text"`
	Type string `json:"type"` // allow | deny
}

// addOneConstraint は操作の制約を1件 task_constraints に保存する。origin は t.worker(既定は system)：
// 分解器が書いたものは "goals"、メイン agent が書いたものは "human"。
func (t *ToolSet) addOneConstraint(it constraintItem) (int64, error) {
	text := strings.TrimSpace(it.Text)
	if text == "" {
		return 0, fmt.Errorf("text は空にできない")
	}
	kind := strings.TrimSpace(strings.ToLower(it.Type))
	if kind == "" {
		kind = "deny" // 既定では禁止として扱う：種類の印がないときはより保守的に
	}
	if kind != "allow" && kind != "deny" {
		return 0, fmt.Errorf("type は allow か deny でなければならない")
	}
	return t.ts.AddConstraint(kind, text, t.worker)
}

// setConstraints は【このタスク】に操作の制約(allow=何をしてよいか / deny=何を禁じるか)を追加する。目標の
// 分解器が round-0 で制約を抜き出すときの提出ツールでもあり、メイン agent の実行時に制約を補うツールでもある——同じ管理されたツールで、web
// 側で説明/schema を変えたり、agent ごとに結びつけたりできる。制約は planner/worker のシステムプロンプトに注入され、探索の境界を縛る。
func (t *ToolSet) setConstraints() actool.CoreTool {
	return writeTool("set_constraints",
		"【このタスク】に操作の制約を追加し、探索の境界を枠づける：type=allow(してよい操作)または deny(禁じる操作)。\n"+
			"制約=『どの操作をしてよい/してはいけないか』の定め(『現在のポートだけをテストし、他のポートはスキャンしない』『本番の DB への書き込み操作を禁じる』『受動的な偵察だけを許す』など)であり、目標でも攻撃の手順でもない。\n"+
			"★まとめて出すことを優先：複数件は constraints 配列に入れて一度に提出する。ids を返し、それと同じ長さ・同じ順序になる(失敗した項目の id=0。詳細は errors を参照)。単発なら constraints を省いて最上位の text/type を直接渡す。\n"+
			"タスクの目標/説明に【はっきり書かれた】制約だけを登録し、作り上げないこと。種類に迷ったら deny(より保守的)を使う。",
		obj(map[string]any{
			"constraints": map[string]any{"type": "array", "description": "【こちらを優先して使う】追加する制約の配列で、順に処理する。各要素：text(必須。制約1件)+ type(allow|deny)。返す ids はこの配列と同じ長さ・同じ順序。", "items": map[string]any{"type": "object"}},
			"text":        str("[単発] 操作の制約1件の内容"),
			"type":        str("[単発] allow(許可)または deny(禁止)。省略すると deny として扱う"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("set_constraints は有効になっていない: ExplorationStore が初期化されていない"), nil
			}
			var a struct {
				Constraints    []constraintItem `json:"constraints"`
				constraintItem                  // 単発モード：最上位の text/type
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Constraints) > 0
			items := a.Constraints
			if !batch {
				items = []constraintItem{a.constraintItem}
			}
			ids := make([]int64, len(items))
			errs := map[string]string{}
			for i, it := range items {
				id, err := t.addOneConstraint(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
			}
			if !batch { // 単発：簡単な戻り値を保つ
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("constraint added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

func (t *ToolSet) addHint() actool.CoreTool {
	return t.writeExpTool("add_hint", "人間/メイン agent の戦略上のヒントを探索グラフに付ける。計画者が次に意図を生成するときにそれを読む。\n"+
		"★まとめて出すことを優先：複数のヒントは hints 配列に入れて一度に提出する（1件ずつ呼ぶより往復が少ない）。ids 配列を返し、hints と同じ長さ・同じ順序になる（失敗した項目の id=0。詳細は errors を参照）。単発なら hints を省いて最上位の text を直接渡す。",
		obj(map[string]any{
			"hints":        map[string]any{"type": "array", "description": "【こちらを優先して使う】追加するヒントの配列で、順に処理する。各要素のフィールドは下の最上位のフィールド（text/asset_ids/traffic_refs）と同じ。返す ids はこの配列と同じ長さ・同じ順序。", "items": obj(map[string]any{"text": str("ヒントの内容"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, "traffic_refs": HintTrafficSchema()})},
			"text":         str("[単発] ヒントの内容。例：'認証後のインターフェースを重点的に掘る'"),
			"traffic_refs": HintTrafficSchema(),
			"asset_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "錨を下ろす資産の id（任意。0/1/複数）"},
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Hints    []hintItem `json:"hints"`
				hintItem            // 単発モード：最上位の text/asset_ids
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Hints) > 0
			items := a.Hints
			if !batch {
				items = []hintItem{a.hintItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			var addedTexts []string
			for i, it := range items {
				id, err := t.addOneHint(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				addedTexts = append(addedTexts, strings.TrimSpace(it.Text))
			}
			if len(addedTexts) > 0 {
				// planner を起こす（一括で1回）。notifyHint を優先：1回の add_hint で「人が戦略上のヒントを
				// N 件追加した：…」の起動を1件記録し、planner に「このラウンドはヒントの追加で起動した」ことをはっきりさせ、ヒントの内容を見せる。
				// このコールバックがつながっていなければ素の notify に戻る（bare wake。ヒントは引き続きグラフにたたまれ、自分で読める）。
				switch {
				case t.notifyHint != nil:
					t.notifyHint(addedTexts)
				case t.notify != nil:
					t.notify()
				}
			}

			if !batch { // 単発：元の戻り値を保つ
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("hint added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

// killWorkTool lets the planner terminate a single running work (by intent id).
func (t *ToolSet) killWorkTool() actool.CoreTool {
	return t.writeExpTool("kill_work", "実行中の意図(work)を1つ終わらせる。それた/意味のない探索を止めるのに使う。終わらせた意図は stopped になり、もう自動では取り直されない。まず get_worker_output で何をしているかを見てから決める。",
		obj(map[string]any{"intent_id": idp("終わらせる意図の id（= work のハンドル）")}, "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.killWork == nil {
				return actool.Errorf("kill_work は今は使えない"), nil
			}
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id は必須"), nil
			}
			node, err := t.ts.GetNode(id)
			if err != nil || node == nil || node.Kind != db.KindIntent {
				return actool.Errorf("intent_id はこのタスクの意図でなければならない（関連するタスクの意図は読み取り専用）"), nil
			}
			if err := t.killWork(id); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("意図 %d の work に終了の合図を送った", id)), nil
		})
}

// steerWorkTool lets the planner inject a mid-run course-correction into a running
// work WITHOUT killing it: the message reaches the worker before its next tool call,
// which re-plans its next step (already-gathered context is kept). For in-intent
// nudges ("X をやめて Y に集中"); if the whole direction is wrong use kill_work + a new intent.
func (t *ToolSet) steerWorkTool() actool.CoreTool {
	return t.writeExpTool("steer_work", "実行中の意図(work)に軌道修正の指示をその場で注入する。中断せず、これまでの進み具合も失わない：worker は次の動作の前にあなたの指示を受け取り、それに沿って調整する。'X をもう追わず、Y に集中'のような【意図の中での】軌道修正に使う。方向がまるごと間違っているなら kill_work をしてから新しい意図を出すこと。先に get_worker_output で何をしているかを見るとよい。",
		obj(map[string]any{
			"intent_id": idp("軌道修正する意図の id（= work のハンドル）"),
			"message":   str("worker への軌道修正の指示。何をやめ、何に向かうかをはっきりさせる"),
		}, "intent_id", "message"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.steerWork == nil {
				return actool.Errorf("steer_work は今は使えない"), nil
			}
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
				Message  string          `json:"message"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id は必須"), nil
			}
			node, err := t.ts.GetNode(id)
			if err != nil || node == nil || node.Kind != db.KindIntent {
				return actool.Errorf("intent_id はこのタスクの意図でなければならない（関連するタスクの意図は読み取り専用）"), nil
			}
			if strings.TrimSpace(a.Message) == "" {
				return actool.Errorf("message は必須"), nil
			}
			if err := t.steerWork(id, a.Message); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("意図 %d の work に軌道修正の指示を注入した（次の手順から効く）", id)), nil
		})
}

// getWorkerOutput returns a work's final (or 中断した時点までの) conclusion text by intent id.
func (t *ToolSet) getWorkerOutput() actool.CoreTool {
	return t.readExpTool("get_worker_output", "このタスクまたは直接関連するタスクのある意図(work)の最終的な出力の結論を取得する。関連するタスクの結果には source_task_id/inherited=true が付き、読み取り専用。正常に終わったものはその総括を返す。終わらせた(stopped)/異常の work は中断した時点までの最後の出力を返す。",
		obj(map[string]any{"intent_id": idp("意図の id（= work のハンドル）")}, "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id は必須"), nil
			}
			intentNode, err := t.ts.GetNodeWithSources(id)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if intentNode == nil || intentNode.Kind != db.KindIntent {
				return actool.Errorf("intent_id はこのタスクにもその直接関連するタスクにも属さない"), nil
			}
			acts, _, err := t.ts.ActivityListWithSources(id, 0, 1000)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			var chosen, fallback *db.Activity
			for i := range acts {
				switch acts[i].Kind {
				case "result":
					chosen = &acts[i]
					fallback = &acts[i]
				case "text":
					fallback = &acts[i]
				}
			}
			pick := chosen
			if pick == nil {
				pick = fallback
			}
			if pick == nil {
				if intentNode.Inherited {
					return jsonResult(inheritedMap(map[string]any{
						"intent_id": id, "final_text": "（この work にはまだ出力がない）",
					}, intentNode.SourceTaskID))
				}
				return actool.Text("（この work にはまだ出力がない）"), nil
			}
			detail, _ := t.ts.ActivityDetailWithSources(pick.ID)
			if detail == "" {
				detail = pick.Summary
			}
			result := map[string]any{
				"intent_id": id, "final_text": detail,
				"summary": pick.Summary, "is_error": pick.IsError,
			}
			if intentNode.Inherited {
				inheritedMap(result, intentNode.SourceTaskID)
			}
			return jsonResult(result)
		})
}

// traceSteps renders summary-only trace rows, re-truncating each summary to 100
// chars — the stored summary is capped at 200 for the UI transcript; the trace
// tools want it tighter since a whole work's step list is many rows.
func traceSteps(acts []db.Activity) []map[string]any {
	steps := make([]map[string]any, 0, len(acts))
	for i := range acts {
		step := map[string]any{
			"step_id": acts[i].ID, "kind": acts[i].Kind, "tool": acts[i].Tool,
			"is_error": acts[i].IsError, "summary": firstLine(acts[i].Summary, 100),
		}
		if acts[i].Inherited {
			inheritedMap(step, acts[i].SourceTaskID)
		}
		steps = append(steps, step)
	}
	return steps
}

// getWorkerTrace exposes a work's execution PROCESS (not just its final output):
// list step summaries, keyword-search within one work, or pull full detail of a
// few specific steps. Thinking steps are excluded everywhere.
func (t *ToolSet) getWorkerTrace() actool.CoreTool {
	return t.readExpTool("get_worker_trace",
		"ある意図(work)の【実行の過程】を見る（get_worker_output が最終的な結論だけを渡すのとは違う）。使い方は3つ：\n"+
			"① intent_id だけを渡す → その work の各手順の要約の流れを返す（summary≤100字、step_id 付き。動作の輪郭だけで、完全な出力は含まない）；\n"+
			"② intent_id + q → キーワードに一致した手順の要約だけを返す（要約と完全な出力の両方を検索する。返すのは summary だけで、内容を見るには③を使う）；\n"+
			"③ intent_id + step_ids → それらの手順の完全な内容(detail)を返す。一度に取れるのは最大 5 個で、超えたら先頭の 5 個だけを返し、取らなかったものを notice/omitted_step_ids で知らせる。\n"+
			"典型的な流れ：まず①/②で怪しい手順の step_id を突き止め、それから③で完全な出力を取る。思考(thinking)の手順は含まない。直接関連するタスクの過去の trace にも対応し、その結果には source_task_id/inherited=true が付き、読み取り専用。",
		obj(map[string]any{
			"intent_id": idp("意図の id（= work のハンドル）"),
			"q":         str("キーワード：要約/完全な出力がそれに一致した手順だけを返す（任意。step_ids とは同時に使えない）"),
			"step_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "完全な内容を取る step_id（①/②が返したもの。一度に取れるのは最大 5 個で、多く渡しても先頭の 5 個だけを返し、残りは omitted_step_ids に並べる）"},
			"limit":     intp("要約の流れ/検索の返す上限（任意）"),
		}, "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				IntentID json.RawMessage   `json:"intent_id"`
				Q        string            `json:"q"`
				StepIDs  []json.RawMessage `json:"step_ids"`
				Limit    int               `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id は必須"), nil
			}
			intentNode, nodeErr := t.ts.GetNodeWithSources(id)
			if nodeErr != nil {
				return actool.Errorf(nodeErr.Error()), nil
			}
			if intentNode == nil || intentNode.Kind != db.KindIntent {
				return actool.Errorf("intent_id はこのタスクにもその直接関連するタスクにも属さない"), nil
			}
			// ③ detail drill-down by step ids, thinking excluded by the store.
			if len(a.StepIDs) > 0 {
				// Dedup + drop invalid ids first so garbage/duplicates don't eat into
				// the per-call cap. detail is returned in full (untruncated), so the
				// cap bounds one tool result; over the cap we serve the first N and
				// tell the model exactly which ids were deferred, instead of erroring
				// and forcing it to re-plan the call.
				const maxStepIDs = 5
				var ids []int64
				seen := make(map[int64]bool)
				for _, raw := range a.StepIDs {
					if v := pid(raw); v > 0 && !seen[v] {
						seen[v] = true
						ids = append(ids, v)
					}
				}
				var omitted []int64
				if len(ids) > maxStepIDs {
					omitted = append(omitted, ids[maxStepIDs:]...)
					ids = ids[:maxStepIDs]
				}
				acts, err := t.ts.ActivityByIDsWithSources(ids)
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				steps := make([]map[string]any, 0, len(acts))
				for i := range acts {
					if acts[i].NodeID == nil || *acts[i].NodeID != id || acts[i].Inherited != intentNode.Inherited ||
						(acts[i].Inherited && acts[i].SourceTaskID != intentNode.SourceTaskID) {
						continue
					}
					step := map[string]any{
						"step_id": acts[i].ID, "kind": acts[i].Kind, "tool": acts[i].Tool,
						"is_error": acts[i].IsError, "detail": acts[i].Detail,
					}
					if acts[i].Inherited {
						inheritedMap(step, acts[i].SourceTaskID)
					}
					steps = append(steps, step)
				}
				result := map[string]any{"intent_id": id, "steps": steps, "returned_step_ids": ids}
				if len(omitted) > 0 {
					// returned_step_ids/omitted_step_ids let the model decide programmatically
					// whether another call is worth it; the notice states the same in prose.
					result["omitted_step_ids"] = omitted
					result["notice"] = fmt.Sprintf(
						"一度に完全な内容を取れる手順は最大 %d 個で、今回は先頭の %d 個（%v）を返した。取らなかった %d 個は %v。"+
							"これらの内容で十分に突き止められるなら、残りの手順を取る必要はない。どうしても続けるときは、これらの step_id でもう一度呼ぶこと。",
						maxStepIDs, len(ids), ids, len(omitted), omitted)
				}
				if intentNode.Inherited {
					inheritedMap(result, intentNode.SourceTaskID)
				}
				return jsonResult(result)
			}
			// ①/② summary stream, optionally keyword-filtered; 100-char summaries.
			var acts []db.Activity
			var err error
			if strings.TrimSpace(a.Q) != "" {
				acts, err = t.ts.ActivityTraceSearchWithSources(id, a.Q, a.Limit)
			} else {
				acts, err = t.ts.ActivityTraceWithSources(id, a.Limit)
			}
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			result := map[string]any{"intent_id": id, "steps": traceSteps(acts)}
			if intentNode.Inherited {
				inheritedMap(result, intentNode.SourceTaskID)
			}
			return jsonResult(result)
		})
}

// searchAllWorkerTraces keyword-searches EVERY work's process in this task — for
// finding what a worker saw but never wrote back as a fact. Returns only matching
// summaries (≤100 chars), each tagged with its intent_id for follow-up drill-down.
func (t *ToolSet) searchAllWorkerTraces() actool.CoreTool {
	return t.readExpTool("search_all_worker_traces",
		"【通常は使うことを勧めない。システムがすでにほとんどの情報を渡しているため】【このタスクの他の work の実行の過程】をキーワード(q)で検索する——ある worker が見たのに fact に書いていないもの（あるパス/token/エラーなど）を取り戻すのに使う。"+
			"あなた自身のこの意図の手順は自動で除いてある（それらはもともとあなたのコンテキストにある）。"+
			"一致した手順の要約(summary≤100字)だけを返し、各件に intent_id が付く。それをもとに get_worker_trace(intent_id, step_ids=[...]) で完全な内容を取る。",
		obj(map[string]any{
			"q":     str("キーワード（すべての work の手順の要約+完全な出力を検索する）"),
			"limit": intp("返す上限。既定 100（任意）"),
		}, "q"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Q     string `json:"q"`
				Limit int    `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Q) == "" {
				return actool.Errorf("q は必須"), nil
			}
			// 呼び出した側自身のこの意図の手順を除く（worker 自身の trace はすでにそのコンテキストにある）。
			acts, err := t.ts.ActivityTraceSearchAllWithSources(t.ownerNode, a.Q, a.Limit)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			hits := make([]map[string]any, 0, len(acts))
			for i := range acts {
				var intent int64
				if acts[i].NodeID != nil {
					intent = *acts[i].NodeID
				}
				hit := map[string]any{
					"intent_id": intent, "step_id": acts[i].ID, "worker": acts[i].Worker,
					"kind": acts[i].Kind, "tool": acts[i].Tool, "is_error": acts[i].IsError,
					"summary": firstLine(acts[i].Summary, 100),
				}
				if acts[i].Inherited {
					inheritedMap(hit, acts[i].SourceTaskID)
				}
				hits = append(hits, hit)
			}
			return jsonResult(map[string]any{"query": a.Q, "hits": hits})
		})
}

// listWorkerTraces gives a worker (which has no graph_overview and can't see the
// intent graph) a lightweight index of the works in this task — intent_id +
// one-line summary + state — so it can DISCOVER which works to inspect via
// get_worker_trace. Without this a worker only knows intent_ids that come back
// from search_all_worker_traces hits. Excludes still-open intents (not yet run →
// no process to inspect).
func (t *ToolSet) listWorkerTraces() actool.CoreTool {
	return t.readExpTool("list_worker_traces",
		"【通常は使うことを勧めない。システムがすでにほとんどの情報を渡しているため】このタスクの【すでに実行した work（意図）】の索引を一覧にする：intent_id + 1文の方向(summary) + 状態。"+
			"あなた(worker)には探索グラフが見えないので、これを使ってどの work を読み返す価値があるかを見つける——それから get_worker_trace(intent_id) でその手順を見て、get_worker_trace(intent_id, step_ids=[...]) で詳細を取る。"+
			"実行済みのもの(running/done/exhausted/blocked/stopped)だけを並べ、まだ実行していない open は含まない。注意：あなたのタスクの境界は引き続きあなたが受け取った意図であり、他の work を見るのは観察を再利用し、重複した作業を避けるためだけ。",
		obj(map[string]any{
			"q":     str("summary のキーワードで絞り込む（任意）"),
			"limit": intp("返す上限。既定 50（任意）"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Q     string `json:"q"`
				Limit int    `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			limit := a.Limit
			if limit <= 0 {
				limit = 50
			}
			all, err := t.ts.ListByKindWithSources(db.KindIntent, 500)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			q := strings.ToLower(strings.TrimSpace(a.Q))
			out := make([]map[string]any, 0, limit)
			for _, n := range all {
				if n.Inherited && n.State == "running" {
					continue
				}
				switch n.State {
				case "running", "done", "exhausted", "blocked", "stopped": // has run → has a process
				default:
					continue
				}
				var p map[string]any
				_ = json.Unmarshal(n.Payload, &p)
				summary, _ := p["summary"].(string)
				if q != "" && !strings.Contains(strings.ToLower(summary), q) {
					continue
				}
				item := map[string]any{"intent_id": n.ID, "summary": summary, "state": n.State}
				if n.Inherited {
					inheritedMap(item, n.SourceTaskID)
				}
				out = append(out, item)
				if len(out) >= limit {
					break
				}
			}
			return jsonResult(map[string]any{"works": out})
		})
}

// PlannerTools is the read + intent-generation + goal-judgement tool set.
func (t *ToolSet) PlannerTools() []actool.CoreTool {
	return []actool.CoreTool{
		t.graphOverview(), t.listFindings(), t.listFacts(), t.nodeDetail(),
		// cold-digest §6.1: restore folded cold nodes (digest body → members → detail).
		t.expandDigest(),
		t.getWorkerOutput(), t.getWorkerTrace(), t.searchAllWorkerTraces(), t.listGoals(), t.addIntent(), t.proveGoal(), t.goalMet(),
		t.killWorkTool(), t.steerWorkTool(),
		// report_finding：計画で状況を判断するときに自分で脆弱性を確認したなら、直接登録できる（worker と同じツール）。
		t.addFinding(),
		// list_companies：企業の一覧 + scope + 資産の数を見る（company_id を取る / 帰属の範囲を理解する）。
		t.listCompanies(),
		// list_assets：計画するときに DSL で全資産の DB を検索する（list_untested_assets の「範囲内で未テスト」の見方と組み合わせ、
		// 「ドメイン/フィンガープリント/ポート/ステータスコードなどの条件で DB 全体を調べる」力を補う）。
		t.listAssets(),
		// add_company_scope：計画するときに、ドメイン/IP/CIDR/ICP/キーワードをある会社の資産の範囲に入れられる（一致した資産を自動で引き受ける）。
		t.addCompanyScope(),
		// add_task_scope：ルートドメイン全体/会社全体/あるサブドメイン/IP を自分からこのタスクのテスト範囲(カバレッジの分母)に入れる。
		t.addTaskScope(),
		// list_untested_assets：必要に応じてこのタスクの範囲内の未テストの資産を調べ(種類+ページ分け)、テストを補うかを自分で決める。
		t.listUntestedAssets(),
	}
}
