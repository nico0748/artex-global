package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/traffic"
	actool "github.com/Autumn-27/norma/tool"
)

// jaDefaultsFlag は、組み込みの既定を中国語から日本語に移した1回きりの移行の印。
const jaDefaultsFlag = "ja_defaults_v1"

// legacyAssistantPromptSHA256 は、中国語の時代の agent.DefaultAssistantPrompt（UI で作った
// カスタムの agent に最初に入る下書き）の sha256。カスタムの agent のプロンプトはユーザーの
// 内容なので、この下書きのまま一度も編集されていないものだけを日本語の下書きに替える。
const legacyAssistantPromptSHA256 = "a179c990bff1ab4e0a6f326945a1c44036ef4c8a54c7c5b635965f6cefdc9f0a"

// retester の表示の名前と説明（seedFindingRetester が新しい DB に入れる値と同じ）。
const (
	retesterAgentName        = "脆弱性の再テスト"
	retesterAgentDescription = "脆弱性の詳細から手動で始め、元の証拠を読んで独立した再テストの結論を保存する。"
)

// migrateJapaneseDefaults は、古い DB に中国語のまま残っている組み込みの既定を日本語の既定に替える。
// SeedPromptIfEmpty / SeedTool / 規則の種まきは最初の挿入だけなので、コードの既定を日本語にしても
// すでに動いている DB には届かない。ここで1回だけ（settings の flag で守る）次を行う：
//   - 組み込みの agent（goals/planner/mainagent/worker/auto/pentest）と reporter・retester の
//     現在のプロンプトが中国語なら、日本語の既定を新しい版として足して切り替える（ResetPromptToDefault）。
//     古い版は履歴に残るので、版の記録から取り戻せる。
//   - カスタムの agent で、中国語の下書きのまま編集されていないプロンプトを日本語の下書きに替える。
//   - 組み込みのツールの説明か schema（引数の説明）が中国語なら、コードの既定に塗り直す
//     （RefreshToolDefaults。agent の結びつけと有効/無効は残す）。
//   - reporter の「report_finding で起動」のメッセージが中国語なら、いまの既定に替える。
//   - retester の名前・説明が中国語なら、日本語に替える。
//   - 組み込みの遮断の規則と資産の遮断の規則の名前・メッセージ（db.MigrateBuiltinRulesToJapanese）。
//
// 日本語や韓国語ですでに書き換えた値は、かなやハングルを含むので触らない。
func (s *Server) migrateJapaneseDefaults() {
	pg := s.m.pg
	if v, _, _ := pg.GetSetting(jaDefaultsFlag); v == "true" {
		return
	}
	defer func() { _ = pg.SetSetting(jaDefaultsFlag, "true") }() // 成功してもしなくても1回だけ試す

	// 1. 組み込みの agent と reporter・retester のプロンプト。
	seeds := agent.BuiltinPromptSeeds()
	seeds["reporter"] = agent.ReporterDefaultPrompt
	seeds[db.FindingRetestAgentKey] = agent.RetesterDefaultPrompt
	prompts := 0
	for key, tmpl := range seeds {
		a, err := pg.GetAgentByKey(key)
		if err != nil || a == nil {
			continue
		}
		cur, err := pg.CurrentPrompt(a.ID)
		if err != nil || cur == tmpl || !db.LooksChinese(cur, 20) {
			continue
		}
		if _, err := pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
			log.Printf("[ja-defaults] %s のプロンプトを日本語の既定に替えられなかった: %v", key, err)
			continue
		}
		prompts++
	}
	if agents, err := pg.ListAgents(); err == nil {
		for _, a := range agents {
			if a.Builtin || seeds[a.Key] != "" {
				continue
			}
			cur, err := pg.CurrentPrompt(a.ID)
			if err != nil || sha256Hex(cur) != legacyAssistantPromptSHA256 {
				continue
			}
			if _, err := pg.ResetPromptToDefault(a.ID, agent.DefaultAssistantPrompt); err == nil {
				prompts++
			}
		}
	}

	// 2. 組み込みのツールの説明と schema。
	type toolSeed struct {
		key, desc string
		schema    json.RawMessage
	}
	var tools []toolSeed
	for _, sd := range agent.BuiltinToolSeeds() {
		b, _ := json.Marshal(sd.Schema)
		tools = append(tools, toolSeed{sd.Key, sd.Desc, b})
	}
	for _, t := range s.hostSeedTools() {
		b, _ := json.Marshal(t.InputSchema())
		tools = append(tools, toolSeed{t.Name(), t.Description(), b})
	}
	refreshed := 0
	for _, t := range tools {
		row, err := pg.GetTool(t.key)
		if err != nil || row == nil || !row.System {
			continue
		}
		if !db.LooksChinese(row.Description, 4) && !db.LooksChinese(string(row.Schema), 4) {
			continue
		}
		if err := pg.RefreshToolDefaults(t.key, t.desc, t.schema); err != nil {
			log.Printf("[ja-defaults] ツール %s の説明を日本語の既定に替えられなかった: %v", t.key, err)
			continue
		}
		refreshed++
	}

	// 3. reporter の起動のメッセージ。
	if triggers, err := pg.ListTriggersFor("reporter"); err == nil {
		for _, t := range triggers {
			if !t.OnToolCall || !db.LooksChinese(t.ToolCallMessage, 10) {
				continue
			}
			t.ToolCallMessage = reporterToolCallMessage
			if err := pg.UpdateTrigger(t); err != nil {
				log.Printf("[ja-defaults] reporter の起動のメッセージを替えられなかった: %v", err)
			}
		}
	}

	// 4. retester の名前と説明。
	if a, err := pg.GetAgentByKey(db.FindingRetestAgentKey); err == nil && a != nil && db.LooksChinese(a.Name, 2) {
		desc := a.Description
		if db.LooksChinese(desc, 4) {
			desc = retesterAgentDescription
		}
		if err := pg.UpdateAgentMeta(a.Key, retesterAgentName, desc); err != nil {
			log.Printf("[ja-defaults] retester の名前を替えられなかった: %v", err)
		}
	}

	// 5. 組み込みの規則。
	rules, err := pg.MigrateBuiltinRulesToJapanese()
	if err != nil {
		log.Printf("[ja-defaults] 組み込みの規則を日本語に替えられなかった: %v", err)
	}
	log.Printf("[ja-defaults] 中国語の既定を日本語に替えた：プロンプト %d 件、ツール %d 件、規則 %d 件（1回だけ）", prompts, refreshed, rules)
}

// hostSeedTools は、起動時に tools 表へ種まきするサーバー側のツール（通信、オーケストレーション、
// プラットフォーム、再テスト）。
func (s *Server) hostSeedTools() []actool.CoreTool {
	out := append([]actool.CoreTool{}, traffic.SeedToolMetas()...)
	out = append(out, s.orchestrationTools()...)
	out = append(out, s.platformTools()...)
	return append(out, s.findingRetestTools()...)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
