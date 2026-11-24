package db

import "strings"

// LooksChinese は、文に漢字があり、かなが1文字もないときに true を返す。
// 日本語の文はほぼ必ずかなを含むので、組み込みの既定がまだ中国語のまま残っている行
// （古い DB に種まきされたプロンプト、ツールの説明、規則の名前など）を見分けるのに使う。
// 漢字が minHan 文字に満たない短い文は判定しない（固有名詞だけの説明などを誤って拾わない）。
func LooksChinese(s string, minHan int) bool {
	han := 0
	for _, r := range s {
		switch {
		case r >= 0x3040 && r <= 0x30ff: // ひらがな・カタカナ
			return false
		case r >= 0xac00 && r <= 0xd7a3: // ハングル（韓国語の文は対象外）
			return false
		case (r >= 0x3400 && r <= 0x9fff) || (r >= 0xf900 && r <= 0xfaff):
			han++
		}
	}
	return han >= minHan
}

// legacyBuiltinRulePrefix は、中国語の時代に種まきした組み込みの規則の名前の先頭。
// 古い DB の行を見分けるための照合用の値なので、翻訳しない。
const legacyBuiltinRulePrefix = "[内置]"

// MigrateBuiltinRulesToJapanese は、古い DB に中国語のまま残っている組み込みの規則
// （intercept_rules と asset_intercept_rules）を、いまの日本語の名前・メッセージに直す。
// 行は pattern でいまの種と対応づけ、名前が legacyBuiltinRulePrefix で始まる行だけを触る。
// メッセージは中国語のままのときだけ直す（ユーザーが書き換えたメッセージは残す）。
// 有効/無効、優先度、pattern、動作はユーザーの設定なので変えない。直した行の数を返す。
func (d *DB) MigrateBuiltinRulesToJapanese() (int, error) {
	type seed struct{ name, message string }
	byPattern := map[string]seed{}
	for _, r := range defaultInterceptRulesV1 {
		byPattern[r.pattern] = seed{r.name, r.message}
	}
	for _, r := range defaultInterceptRulesV2 {
		byPattern[r.pattern] = seed{r.name, r.message}
	}
	byPattern[deleteEndpointPathPattern] = seed{deleteEndpointRuleName, deleteEndpointRuleMessage}

	rows, err := d.Query(`SELECT id, name, pattern, message FROM intercept_rules WHERE name LIKE $1`, legacyBuiltinRulePrefix+"%")
	if err != nil {
		return 0, err
	}
	type row struct {
		id                     int64
		name, pattern, message string
	}
	var olds []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.name, &r.pattern, &r.message); err != nil {
			rows.Close()
			return 0, err
		}
		olds = append(olds, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, r := range olds {
		sd, ok := byPattern[r.pattern]
		if !ok {
			continue // pattern をユーザーが変えた行は、どの種に当たるか決められないので触らない
		}
		msg := r.message
		if LooksChinese(msg, 2) {
			msg = sd.message
		}
		if _, err := d.Exec(`UPDATE intercept_rules SET name=$2, message=$3 WHERE id=$1`, r.id, sd.name, msg); err != nil {
			return n, err
		}
		n++
	}

	notes := map[string]string{}
	for _, r := range defaultAssetInterceptRules {
		notes[r.kind+"\x00"+r.pattern] = r.note
	}
	arows, err := d.Query(`SELECT id, kind, pattern FROM asset_intercept_rules WHERE builtin AND note LIKE $1`, legacyBuiltinRulePrefix+"%")
	if err != nil {
		return n, err
	}
	type arow struct {
		id            int64
		kind, pattern string
	}
	var aolds []arow
	for arows.Next() {
		var r arow
		if err := arows.Scan(&r.id, &r.kind, &r.pattern); err != nil {
			arows.Close()
			return n, err
		}
		aolds = append(aolds, r)
	}
	arows.Close()
	if err := arows.Err(); err != nil {
		return n, err
	}
	for _, r := range aolds {
		note, ok := notes[r.kind+"\x00"+strings.TrimSpace(r.pattern)]
		if !ok {
			continue
		}
		if _, err := d.Exec(`UPDATE asset_intercept_rules SET note=$2 WHERE id=$1`, r.id, note); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// InterceptRuleSeedForTest は、テストから組み込みの規則の種を読むための形。
type InterceptRuleSeedForTest struct{ Name, Pattern, Message string }

// DefaultInterceptRuleV1ForTest は v1 の組み込みの規則の i 番目を返す（テスト用）。
func DefaultInterceptRuleV1ForTest(i int) InterceptRuleSeedForTest {
	r := defaultInterceptRulesV1[i]
	return InterceptRuleSeedForTest{r.name, r.pattern, r.message}
}
