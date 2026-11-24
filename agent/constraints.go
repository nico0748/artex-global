package agent

import (
	"strings"

	"github.com/Autumn-27/artex/db"
)

// constraintBlock renders this task's operation constraints (task_constraints) as a
// high-priority block appended to the planner/worker system prompt. allow/deny are
// grouped; empty string when there are no constraints (or ts is nil). The framing
// deliberately puts these ABOVE the exploration/expansion heuristics so a declared
// boundary wins the tug-of-war against "chase another entry surface".
func constraintBlock(ts *db.ExplorationStore) string {
	if ts == nil {
		return ""
	}
	rows, err := ts.ListConstraints()
	if err != nil || len(rows) == 0 {
		return ""
	}
	var allow, deny []string
	for _, c := range rows {
		text := strings.TrimSpace(c.Text)
		if text == "" {
			continue
		}
		if c.Kind == "allow" {
			allow = append(allow, "- "+text)
		} else {
			deny = append(deny, "- "+text)
		}
	}
	if len(allow) == 0 && len(deny) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【操作の制約（最優先。下にある探索/範囲の拡大のどんな経験則よりも上に立つ；意図を1つ生成するたび、動作を1つ実行するたびに、その前に違反していないかを必ず自分で確かめ、違反するなら行ってはならない）】：")
	if len(allow) > 0 {
		b.WriteString("\n許可された操作：\n")
		b.WriteString(strings.Join(allow, "\n"))
	}
	if len(deny) > 0 {
		b.WriteString("\n禁止された操作：\n")
		b.WriteString(strings.Join(deny, "\n"))
	}
	b.WriteString("\n（制約の外で新しい対象/新しいポート/新しいホストを見つけても、許可を得たことにはならない：上の許可の範囲に入っていない限り、out-of-scope の事実として記録して飛ばし、そのために意図を派生させたり動作を実行したりしてはならない。）")
	return b.String()
}
