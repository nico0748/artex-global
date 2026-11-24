package notify

import "testing"

func TestParseFilterMalformedFallsBackToMatchAll(t *testing.T) {
	// 不正な JSON、空の入力、型の合わないフィールド——どれも必ずゼロ値の Filter、
	// つまり「絞り込まない」に退化すること。この不変条件が「取りこぼすよりは多く通知する」の拠り所だ：
	// ここをエラーや中途半端な解析に変えると、ユーザーが1文字設定を誤っただけで高リスクの通知がすべて黙って失われる。
	cases := []struct {
		name string
		raw  string
	}{
		{"空の入力", ""},
		{"不正な JSON", `{not json`},
		{"途切れた JSON", `{"min_severity":`},
		{"型の不一致", `{"min_severity": 123, "task_ids": "abc"}`},
		{"最上位が配列", `[1,2,3]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := ParseFilter([]byte(tc.raw))
			if f.MinSeverity != "" || len(f.TaskIDs) != 0 || len(f.AssetIDs) != 0 {
				t.Fatalf("不正な設定はゼロ値の Filter に退化すべきだが、%+v を得た", f)
			}
			// ゼロ値の Filter はどのイベントにも一致しなければならない。
			ev := Snapshot{Kind: EventFindingCreated, Severity: "low", VulnClass: "XSS"}
			if !Match(f, ev) {
				t.Fatal("ゼロ値の Filter はすべてのイベントに一致すべき")
			}
		})
	}
}

func TestMatchSeverityThreshold(t *testing.T) {
	ev := func(sev string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: sev}
	}
	cases := []struct {
		min    string
		sev    string
		expect bool
	}{
		{"", "low", true},
		{"", "critical", true},
		{"high", "critical", true},
		{"high", "high", true},
		{"high", "medium", false},
		{"high", "low", false},
		{"critical", "high", false},
		{"critical", "critical", true},
		// 不明な深刻度の序数は 0 で、空でないしきい値ならどれでも止めるべき（疑わしいときは通知しない）。
		{"low", "", false},
		{"low", "unknown", false},
		{"", "", true},
	}
	for _, tc := range cases {
		got := Match(Filter{MinSeverity: tc.min}, ev(tc.sev))
		if got != tc.expect {
			t.Errorf("min=%q sev=%q: %v を期待したが %v を得た", tc.min, tc.sev, tc.expect, got)
		}
	}
}

func TestMatchScopeRestrictions(t *testing.T) {
	ev := Snapshot{
		Kind:      EventFindingCreated,
		Severity:  "high",
		TaskID:    7,
		AssetIDs:  []int64{10, 20},
		VulnClass: "SQLインジェクション",
	}
	cases := []struct {
		name   string
		filter Filter
		expect bool
	}{
		{"範囲が空=制限なし", Filter{}, true},
		{"タスクが一致", Filter{TaskIDs: []int64{7}}, true},
		{"タスクが不一致", Filter{TaskIDs: []int64{8}}, false},
		{"タスクの複数選択に一致を含む", Filter{TaskIDs: []int64{8, 7}}, true},
		{"資産が交わる", Filter{AssetIDs: []int64{20, 99}}, true},
		{"資産が交わらない", Filter{AssetIDs: []int64{99}}, false},
		{"タスクと資産がともに一致", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{10}}, true},
		{"タスクは一致するが資産が不一致", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{99}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev); got != tc.expect {
				t.Errorf("%v を期待したが %v を得た", tc.expect, got)
			}
		})
	}
}

func TestMatchVulnClassKeywords(t *testing.T) {
	ev := func(class string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: "high", VulnClass: class}
	}
	cases := []struct {
		name   string
		filter Filter
		class  string
		expect bool
	}{
		{"include が空=すべて受け取る", Filter{}, "任意の種類", true},
		{"include に一致", Filter{VulnClassInclude: []string{"SQL"}}, "SQLインジェクション", true},
		{"include に不一致", Filter{VulnClassInclude: []string{"コマンド実行"}}, "SQLインジェクション", false},
		{"include の複数語のいずれかに一致", Filter{VulnClassInclude: []string{"コマンド実行", "SQL"}}, "SQLインジェクション", true},
		{"大文字小文字を区別しない", Filter{VulnClassInclude: []string{"sql"}}, "SQLインジェクション", true},
		{"exclude に一致したら除外", Filter{VulnClassExclude: []string{"情報漏えい"}}, "情報漏えい", false},
		{"exclude に不一致なら通す", Filter{VulnClassExclude: []string{"情報漏えい"}}, "SQLインジェクション", true},
		// 除外は包含より優先：両方に一致したら外すべき。
		{"除外は包含より優先", Filter{
			VulnClassInclude: []string{"SQL"},
			VulnClassExclude: []string{"インジェクション"},
		}, "SQLインジェクション", false},
		// 空白だけのキーワードは無視すべき。さもないと「空白を含むあらゆる文字列に一致」に退化する。
		{"空白のキーワードは無視", Filter{VulnClassInclude: []string{"", "  "}}, "SQLインジェクション", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev(tc.class)); got != tc.expect {
				t.Errorf("%v を期待したが %v を得た", tc.expect, got)
			}
		})
	}
}

func TestMatchStatusChangeRequiresOptIn(t *testing.T) {
	ev := Snapshot{Kind: EventFindingStatusChanged, Severity: "critical", FromStatus: "pending", ToStatus: "fixed"}
	// 既定はオフ：大多数の人が言う「脆弱性の通知」は新しい脆弱性の発見のことで、状態の逐一の記録ではない。
	if Match(Filter{MinSeverity: "low"}, ev) {
		t.Fatal("状態変更イベントは有効にしていなければ飛ばすべき")
	}
	if !Match(Filter{OnStatusChange: true}, ev) {
		t.Fatal("on_status_change を有効にしたら状態変更イベントは一致すべき")
	}
	// 作成イベントは on_status_change の影響を受けない。
	created := Snapshot{Kind: EventFindingCreated, Severity: "critical"}
	if !Match(Filter{MinSeverity: "low"}, created) {
		t.Fatal("作成イベントは on_status_change に依存すべきでない")
	}
}
