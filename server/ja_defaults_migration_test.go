package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// TestMigrateJapaneseDefaults は、中国語の時代に種まきされた古い DB の値（下の中国語は
// その再現のためのテストデータ）が日本語の既定に替わり、ユーザーが日本語で書き換えた値は
// 残ることを確かめる。
func TestMigrateJapaneseDefaults(t *testing.T) {
	s, _ := newRetestServer(t)
	pg := s.m.pg
	if _, err := pg.Exec(`DELETE FROM settings WHERE key=$1`, jaDefaultsFlag); err != nil {
		t.Fatal(err)
	}

	// プロンプト：goals は中国語の古い既定、worker はユーザーが日本語で書き換えたもの。
	goals, _ := pg.GetAgentByKey("goals")
	worker, _ := pg.GetAgentByKey("worker")
	if goals == nil || worker == nil {
		t.Fatal("builtin agents missing")
	}
	if _, err := pg.SavePrompt(goals.ID, strings.Repeat("你是渗透测试目标分解器，负责把总目标拆成可独立验证的子目标。", 2), "旧", "system"); err != nil {
		t.Fatal(err)
	}
	const custom = "ユーザーが書き換えた worker のプロンプト。"
	if _, err := pg.SavePrompt(worker.ID, custom, "test", "test"); err != nil {
		t.Fatal(err)
	}

	// ツール：説明が中国語のまま残った組み込みのツールと、ユーザーが日本語で書き換えたツール。
	seeds := agent.BuiltinToolSeeds()
	if len(seeds) < 2 {
		t.Fatal("need two builtin tool seeds")
	}
	zhTool, jaTool := seeds[0], seeds[1]
	for _, sd := range []agent.ToolSeed{zhTool, jaTool} {
		schema, _ := json.Marshal(sd.Schema)
		agents, _ := json.Marshal([]string{"worker", "planner"})
		if err := pg.UpsertToolForce(sd.Key, sd.Desc, schema, agents); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pg.Exec(`UPDATE tools SET description='列出当前任务的全部漏洞记录', enabled=false WHERE key=$1`, zhTool.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Exec(`UPDATE tools SET description='自分で書いた説明' WHERE key=$1`, jaTool.Key); err != nil {
		t.Fatal(err)
	}

	// 規則：古い名前の組み込みの規則、pattern を変えた規則、資産の遮断の規則。
	v1 := db.DefaultInterceptRuleV1ForTest(0)
	var ruleID, editedID, assetID int64
	if err := pg.QueryRow(`INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message)
VALUES ('[内置] 递归强制删除 rm -rf', false, 100, 'tool_input', 'regex', $1, 'deny', '禁止递归强制删除') RETURNING id`, v1.Pattern).Scan(&ruleID); err != nil {
		t.Fatal(err)
	}
	if err := pg.QueryRow(`INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message)
VALUES ('[内置] 自定义', true, 1, 'tool_input', 'regex', 'zz-user-pattern', 'deny', '用户改过') RETURNING id`).Scan(&editedID); err != nil {
		t.Fatal(err)
	}
	if err := pg.QueryRow(`INSERT INTO asset_intercept_rules(enabled, kind, pattern, note, builtin)
VALUES (true, 'fuzzy_domain', '.gov', '[内置] 政府网站 (.gov)', true) RETURNING id`).Scan(&assetID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pg.Exec(`DELETE FROM intercept_rules WHERE id IN ($1, $2)`, ruleID, editedID)
		_, _ = pg.Exec(`DELETE FROM asset_intercept_rules WHERE id=$1`, assetID)
	})

	s.migrateJapaneseDefaults()

	if cur, _ := pg.CurrentPrompt(goals.ID); cur != agent.BuiltinPromptSeeds()["goals"] {
		t.Fatalf("goals prompt was not switched to the Japanese default: %.60q", cur)
	}
	if cur, _ := pg.CurrentPrompt(worker.ID); cur != custom {
		t.Fatalf("customized worker prompt was overwritten: %.60q", cur)
	}
	if row, _ := pg.GetTool(zhTool.Key); row == nil || row.Description != zhTool.Desc || row.Enabled || len(row.Agents) != 2 {
		t.Fatalf("Chinese tool description not refreshed or binding lost: %+v", row)
	}
	if row, _ := pg.GetTool(jaTool.Key); row == nil || row.Description != "自分で書いた説明" {
		t.Fatalf("customized tool description was overwritten: %+v", row)
	}
	var name, msg string
	var enabled bool
	if err := pg.QueryRow(`SELECT name, message, enabled FROM intercept_rules WHERE id=$1`, ruleID).Scan(&name, &msg, &enabled); err != nil {
		t.Fatal(err)
	}
	if name != v1.Name || msg != v1.Message || enabled {
		t.Fatalf("builtin rule not migrated (or toggle changed): %q %q %v", name, msg, enabled)
	}
	if err := pg.QueryRow(`SELECT name FROM intercept_rules WHERE id=$1`, editedID).Scan(&name); err != nil || name != "[内置] 自定义" {
		t.Fatalf("rule with a user pattern should stay untouched: %q %v", name, err)
	}
	var note string
	if err := pg.QueryRow(`SELECT note FROM asset_intercept_rules WHERE id=$1`, assetID).Scan(&note); err != nil || note != "[組み込み] 政府のサイト (.gov)" {
		t.Fatalf("asset rule note not migrated: %q %v", note, err)
	}

	// 2回目は何もしない（flag で守る）。
	if _, err := pg.SavePrompt(goals.ID, strings.Repeat("再次写入的中文提示词，用来确认迁移只执行一次。", 2), "test", "test"); err != nil {
		t.Fatal(err)
	}
	s.migrateJapaneseDefaults()
	if cur, _ := pg.CurrentPrompt(goals.ID); !db.LooksChinese(cur, 20) {
		t.Fatal("migration ran twice")
	}
	if _, err := pg.ResetPromptToDefault(goals.ID, agent.BuiltinPromptSeeds()["goals"]); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.ResetPromptToDefault(worker.ID, agent.BuiltinPromptSeeds()["worker"]); err != nil {
		t.Fatal(err)
	}
}

func TestLooksChinese(t *testing.T) {
	for s, want := range map[string]bool{
		"列出当前任务的全部漏洞记录":    true,
		"現在のタスクの脆弱性を一覧にする": false,
		"취약점 목록": false,
		"rm -rf": false,
		"漏洞":     false, // minHan に満たない
	} {
		if got := db.LooksChinese(s, 4); got != want {
			t.Errorf("LooksChinese(%q)=%v want %v", s, got, want)
		}
	}
}
