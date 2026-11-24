package agent

import (
	"encoding/json"
	"fmt"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

const findingIDGuidance = "\n\n**脆弱性の番号の約束**：finding_id は独立した脆弱性の記録 ID；finding_node_id は探索ノードの ID。list_findings / list_task_findings / node_detail / get_task_node_detail の id は探索ノードの ID のままなので、独立した番号は同じ戻り値の finding_id から読むこと。get_finding_traffic / bind_finding_traffic は独立した finding_id を使う。古い update_finding_report の finding_id 引数には引き続き finding_node_id を渡す。report_finding の1行目の数字を証拠のツールに使わないこと、番号のエラーに出会っても他の数字を推測しないこと。"

// The server supplies the persisted setting. A missing setting/host is off.
// Consulted at assembly and again on writes so an already-running session
// cannot keep binding after the user switches the feature off.
var FindingTrafficBindingEnabled func() bool

func findingTrafficBindingEnabled() bool {
	return FindingTrafficBindingEnabled != nil && FindingTrafficBindingEnabled()
}

// Applied after ToolResolve: user descriptions and prompts remain intact, while
// all actual reporters (including Planner and custom chat agents) see the same
// API contract. Disabled/unbound tools are never reintroduced here.
func findingWorkflowTools(agentKey string, tools []actool.CoreTool) ([]actool.CoreTool, string) {
	if !findingTrafficBindingEnabled() {
		out := make([]actool.CoreTool, 0, len(tools))
		for _, tool := range tools {
			if tool.Name() == "bind_finding_traffic" {
				continue
			}
			if agentKey == "reporter" && (tool.Name() == "traffic_search" || tool.Name() == "traffic_get" || tool.Name() == "traffic_blob") {
				continue
			}
			switch tool.Name() {
			case "report_finding", "add_hint", "add_task_hint":
				// Work on a copy: toggling back on must restore the original schema.
				raw, _ := json.Marshal(tool.InputSchema())
				var schema map[string]any
				if json.Unmarshal(raw, &schema) == nil {
					stripTrafficParameters(schema)
					tool = DecorateTool(tool, tool.Description(), schema)
				}
			}
			out = append(out, tool)
		}
		return out, ""
	}
	out := append([]actool.CoreTool(nil), tools...)
	has := map[string]bool{}
	for i, tool := range out {
		has[tool.Name()] = true
		note := ""
		switch tool.Name() {
		case "report_finding":
			note = "\n既定では報告の Agent が報告を書く前に通信を照合して結びつける。報告する側は evidence に検証のコマンド、重要な出力、手元にある本物の通信の ID とその用途を残し、報告の Agent が実行の記録と照らして確かめられるようにする；結びつけのためにわざわざパケットを調べる必要はない。明示的な即時の結びつけにも対応する：traffic_refs か evidence_hint_id で確かめ済みの参照を出せ、後者はこのタスクで指定したヒントの構造化された参照を読む；どちらかが無効なら今回の報告はすべて失敗する。TCP/パケットなしならこれらの任意の引数は要らない。返す finding_id と finding_node_id は、それぞれ独立した記録と探索ノードを表す。"
		case "add_hint", "add_task_hint":
			note = "\n確認済みの脆弱性を引き継ぐときは、対応するヒントの traffic_refs に確かめ済みの通信の ID、用途、説明、順序を残し（単発なら最上位に、一括なら対応する hints の要素に入れる）、text でそれが証明する具体的な脆弱性を説明する。呼び出す側は文だけを引き継いで手元の通信の参照を捨ててはならない。確かめていない候補は証拠として渡せない。"
		case "get_finding_traffic", "bind_finding_traffic", "list_findings", "list_task_findings", "node_detail", "get_task_node_detail", "update_finding_report":
			note = findingIDGuidance
		}
		if note != "" {
			out[i] = DecorateTool(tool, tool.Description()+note, tool.InputSchema())
		}
	}
	guidance := ""
	if has["report_finding"] || has["add_task_hint"] || has["add_hint"] {
		guidance = "\n\n**通信の証拠の引き継ぎ（任意）**：自動の結びつけは既定で、報告の Agent が脆弱性の登録後、報告を書く前に行う。報告する側は evidence に検証のコマンド、重要な出力、手元にある本物の通信の ID とその用途を残し、タスクでは intent_id を付けて、報告の Agent がたどれるようにする；結びつけのためにわざわざパケットを調べなくてよい。Auto / Planner が代わりに報告するときは、実行した側の手元の参照を捨てないこと。add_hint / add_task_hint は traffic_refs で引き継げる；明示的な即時の結びつけは引き続き report_finding の traffic_refs / evidence_hint_id に対応する。TCP やパケットがないときは通常どおり登録し、ID を推測してはならず、パケットを補うためだけに探査を繰り返してもならない。"
		if has["add_task_hint"] && !has["add_hint"] {
			guidance += "\nプラットフォームの会話にタスクのコンテキストがないときは、report_finding を直接呼ばない；add_task_hint で既存の対応するタスクに引き継いでタスクの Agent に登録させ、list_task_findings で結果を照合する。"
		}
		if has["prove_goal"] || has["goal_met"] {
			guidance += "\n目標の完了を判定する前に、今回手元にある証拠の報告/引き継ぎを先に終える。証拠の引き継ぎが終わっていないのに、文の脆弱性が登録済みだというだけでタスクを終えたり Worker を取り消したりしないこと；パケットがなければ待つことも無理に取得することも求めない。"
		}
	}
	if has["update_finding_report"] && has["bind_finding_traffic"] && has["get_finding_traffic"] {
		guidance += "\n\n**報告前の通信の自動の関連付け（オン）**：あなたは今回起動した脆弱性について通信を照合して結びつけ、それから報告を書く役目を持つ。まず report_finding が返した JSON か get_task_node_detail / list_task_findings から、はっきりした finding_id と finding_node_id を得る。脆弱性の詳細、対応する意図の実行の記録、手元の証拠の一覧を読み、報告した側が引き継いだ本物の ID を優先して使う。今回の検証が HTTP で通信のツールが使えるなら、traffic_search で候補を絞り、traffic_get でリクエスト/レスポンスが本当にその脆弱性を裏づけることを1件ずつ確かめる；ドメインと時刻は絞り込みにだけ使い、帰属の証明にはならない。確認した証拠は再現の順に bind_finding_traffic(finding_id, traffic_refs) で関連付け、baseline / proof / verification / supporting を選んで用途を説明する。操作できるのは今回の脆弱性だけで、脆弱性を重ねて作ったり対象を探査し直したりしない。結びつけに成功したら get_finding_traffic を呼び直して最新の version を得て、必要な本文を読み、実際に読んだ version を evidence_version として update_finding_report に渡す（その finding_id 引数には引き続き finding_node_id を使う）。既存の結びつけは重ねて追加しなくてよい。TCP、取得していない、ツールが使えない、または正確に一致するものがないときは自動の結びつけを飛ばし、文/コマンドの証拠をもとに通常どおり報告を書いて理由を説明し、通信をそろえるために推測してはならない。結びつけに失敗したら成功と言わない；手元の証拠を残し、報告で結びつけなかった理由を説明する。"
	}
	if guidance != "" || has["get_finding_traffic"] || has["update_finding_report"] {
		guidance += findingIDGuidance
	}
	return out, guidance
}

func stripTrafficParameters(schema map[string]any) {
	props, _ := schema["properties"].(map[string]any)
	delete(props, "traffic_refs")
	delete(props, "evidence_hint_id")
	if required, ok := schema["required"].([]any); ok {
		kept := required[:0]
		for _, key := range required {
			if key != "traffic_refs" && key != "evidence_hint_id" {
				kept = append(kept, key)
			}
		}
		schema["required"] = kept
	}
	if hints, ok := props["hints"].(map[string]any); ok {
		if items, ok := hints["items"].(map[string]any); ok {
			stripTrafficParameters(items)
		}
	}
}

// HintTrafficSchema is shared by the task-local and cross-task hint tools.
func HintTrafficSchema() map[string]any {
	return map[string]any{"type": "array", "description": "任意：確かめ済みで、このヒントの具体的な脆弱性に対応する通信の参照。順序を保つ；引き継いだ後は report_finding で evidence_hint_id を渡してこれらの参照を持ち込める。", "items": obj(map[string]any{"traffic_id": str("本物の通信の ID"), "role": str("baseline / proof / verification / supporting"), "note": str("その通信がどんな結論を裏づけるか")}, "traffic_id")}
}

func (t *ToolSet) findingRefsFromHint(hintID int64, explicit []db.TrafficRef) ([]db.TrafficRef, error) {
	if hintID <= 0 {
		return db.NormalizeTrafficRefs(explicit)
	}
	n, err := t.ts.GetNode(hintID) // local store only: inherited hints cannot supply evidence
	if err != nil {
		return nil, err
	}
	if n == nil || n.Kind != db.KindHint {
		return nil, fmt.Errorf("evidence_hint_id=%d はこのタスクのヒントのノードでなければならない（引き継いだヒントは結びつけに直接使えない）", hintID)
	}
	var payload struct {
		Refs []db.TrafficRef `json:"traffic_refs"`
	}
	if err := json.Unmarshal(n.Payload, &payload); err != nil {
		return nil, err
	}
	return db.NormalizeTrafficRefs(append(append([]db.TrafficRef{}, explicit...), payload.Refs...))
}
