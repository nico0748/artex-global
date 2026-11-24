package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// このファイルは「項目単位で詰める」修正を扱う：まとめのメッセージがチャネルの長さの上限を超えたら、必ず**項目単位で**
// 切り詰め、収まらなかった項目の数をありのまま報告し、呼び出し側が本当に届いたものだけを配信済みにできるようにする。
//
// 以前は全文を描画してから切り詰め、一括分すべてを配信済みにしていた：メッセージの後半は跡形もなく消え、
// 配信履歴ではすべて成功と表示される——脆弱性がこうして失われ、どこからも気づけなかった。

func TestMarkdownBodyPacksWholeItemsWithinByteLimit(t *testing.T) {
	// 200 件の日本語のまとめは、必ず WeCom の 4096 バイトを大きく超える。
	m := batchMsg(200)
	body, kept := markdownBody(m, weComMarkdownLimit)

	if len(body) > weComMarkdownLimit {
		t.Fatalf("本文 %d バイトが上限 %d を超えている", len(body), weComMarkdownLimit)
	}
	if !utf8.ValidString(body) {
		t.Fatal("本文が正しい UTF-8 ではない")
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("一部だけが収まるべき（0 < kept < %d）だが、%d を得た", len(m.Items), kept)
	}
	// 見出しには、このメッセージに何件だけ含まれ、残りが何件あるかをありのまま示すこと——さもないと読む人は見出しの
	// 数字をすべての件数だと思ってしまう。
	if !strings.Contains(body, "나머지") || !strings.Contains(body, "다음 메시지에서") {
		t.Fatalf("見出しには、このメッセージに含まれていないのがあと何件あるかを示すべき:\n%s", body[:minInt(400, len(body))])
	}
	// 先頭の kept 件だけを含むべき。
	for i := 0; i < kept; i++ {
		if !strings.Contains(body, "脆弱性"+itoa(i+1)) {
			t.Fatalf("%d 件目はこのメッセージに入っているべき:\n%s", i+1, body)
		}
	}
	if strings.Contains(body, "脆弱性"+itoa(kept+1)) {
		t.Fatalf("%d 件目は出てくるべきでない（次の一括分に属する）", kept+1)
	}
}

func TestMarkdownBodyKeepsEverythingWhenUnderLimit(t *testing.T) {
	m := batchMsg(3)
	body, kept := markdownBody(m, 0) // 0 = 制限しない
	if kept != len(m.Items) {
		t.Fatalf("長さを制限しないときはすべて残すべきだが、kept=%d を得た", kept)
	}
	if strings.Contains(body, "残り") {
		t.Fatalf("切り詰めていないときに切り詰めの注記が出るべきでない:\n%s", body)
	}
}

func TestMarkdownBodyAlwaysKeepsAtLeastOneItem(t *testing.T) {
	// 予算が小さすぎて1件も収まらないときでも、1件は送る（最後の切り詰めが受け皿になる）。
	// さもないと長すぎる脆弱性1件が一括分全体を永久にその場に留める：取り出すたびに収まらず、毎回送られない。
	m := batchMsg(5)
	_, kept := markdownBody(m, 50)
	if kept != 1 {
		t.Fatalf("少なくとも1件は残すべきだが、%d を得た", kept)
	}
}

func TestMarkdownBodySingleReturnsOne(t *testing.T) {
	_, kept := markdownBody(singleMsg(), 4096)
	if kept != 1 {
		t.Fatalf("単発のメッセージは1件届いたと報告すべきだが、%d を得た", kept)
	}
	// 空のメッセージには届けられる項目がない。
	if _, k := markdownBody(Message{}, 4096); k != 0 {
		t.Fatalf("空のメッセージは 0 件と報告すべきだが、%d を得た", k)
	}
}

func TestTelegramPackingUsesRuneBudget(t *testing.T) {
	m := batchMsg(200)
	text, kept := telegramHTML(m)
	// Telegram は**文字数**で長さを制限する。バイト数の基準を使うと日本語のメッセージが3分の1に縮められてしまう。
	if n := utf8.RuneCountInString(text); n > telegramTextLimit {
		t.Fatalf("本文 %d 文字が上限 %d を超えている", n, telegramTextLimit)
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("一部だけが収まるべきだが、%d を得た", kept)
	}
	if !strings.Contains(text, "다음 메시지") {
		t.Fatalf("含まれていない残りがあることを示すべき:\n%.300s", text)
	}
}

func TestFeishuPackingReportsKept(t *testing.T) {
	m := batchMsg(2000)
	_, kept := feishuCard(m)
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("カードには一部だけが収まるべきだが、%d を得た", kept)
	}
}

func TestWebhookAndEmailReportAllItems(t *testing.T) {
	// この2つのチャネルは本文を切り詰めないので、一括分すべてを配信済みとする。
	m := batchMsg(7)
	if n := len(m.Items); n != 7 {
		t.Fatal("前提条件が成り立たない")
	}
	// 描画関数の戻り値で間接的に確かめる：markdownBody(0) は制限しないときすべて残す。
	if _, k := markdownBody(m, 0); k != len(m.Items) {
		t.Fatalf("長さを制限しないときはすべて使うべきだが、%d を得た", k)
	}
}

// TestMarkdownEscapesUntrustedContent は「信頼できない内容がメッセージの構造を変えてはならない」の回帰テスト。
// タイトルと要約はモデルの出力（モデルが読むのは検査対象のレスポンス）から、資産名は検査対象の URL から来る。
func TestMarkdownEscapesUntrustedContent(t *testing.T) {
	cases := []struct {
		name  string
		item  Item
		must  []string // 結果に必ず現れること（エスケープした形）
		wrong []string // 結果に現れてはならないこと（エスケープしていない形）
	}{
		{
			name: "タイトル中の改行 + 外部リンク",
			item: Item{
				Severity: "high",
				Name:     "ログイン画面の SQL インジェクション\n[緊急：ここをクリックしてアカウントを確認](http://attacker.tld)",
			},
			// 改行は必ずたたむこと（さもないと新しいリスト項目/引用ブロックを偽造できる）。
			// 角括弧と丸括弧は必ずエスケープすること（さもないとクリックできる外部リンクになる）。
			must:  []string{`\[緊急：ここをクリックしてアカウントを確認\]`, `\(http://attacker.tld\)`},
			wrong: []string{"\n[緊急", "\n\n[緊急"},
		},
		{
			name: "タイトル中の画像ビーコン",
			item: Item{
				Severity: "high",
				Name:     "脆弱性 ![](http://attacker.tld/beacon)",
			},
			must:  []string{`\!`, `\(http://attacker.tld/beacon\)`},
			wrong: []string{"![]("},
		},
		{
			name: "資産名の中の強調と引用",
			item: Item{
				Severity: "high",
				Name:     "普通のタイトル",
				Assets:   []string{"a.com/*インジェクション*>引用"},
			},
			must:  []string{`\*インジェクション\*`, `\>`},
			wrong: []string{"*インジェクション*"},
		},
		{
			name: "要約の中のバッククォートと縦線",
			item: Item{
				Severity: "high",
				Name:     "タイトル",
				Summary:  "`code` | 表",
			},
			must:  []string{"\\`code\\`", `\|`},
			wrong: []string{"`code`"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := Message{Items: []Item{tc.item}}
			// 単発モードの writeItem は3つの markdown チャネルが共有する描画の経路。
			var b strings.Builder
			writeItem(&b, tc.item, "", true)
			got := b.String()
			for _, want := range tc.must {
				if !strings.Contains(got, want) {
					t.Errorf("エスケープした形 %q がない:\n%s", want, got)
				}
			}
			for _, bad := range tc.wrong {
				if strings.Contains(got, bad) {
					t.Errorf("エスケープしていない形 %q が現れた（構造や外部リンクの注入に使える）:\n%s", bad, got)
				}
			}
			_ = m
		})
	}
}

// TestMarkdownEscapeBackslashFirst はエスケープの順序を固定する：バックスラッシュは必ず最初に処理すること。
// さもないとあとで足したバックスラッシュにもう1段かぶせてしまい、出力に二重のバックスラッシュが出る。
func TestMarkdownEscapeBackslashFirst(t *testing.T) {
	if got := markdownEscape(`a\b*c`); got != `a\\b\*c` {
		t.Fatalf("エスケープの順序が誤っている。得たもの %q", got)
	}
}

// TestTelegramTitleHasNoMarkdownEscapes は具体的な回帰を固定する：
// markdown のエスケープが Telegram の HTML 出力に漏れてはならない（以前に共有のタイトル関数で
// エスケープを足したところ、Telegram のメッセージに `\(1\)` のような見えるバックスラッシュが出た）。
func TestTelegramTitleHasNoMarkdownEscapes(t *testing.T) {
	m := Message{Items: []Item{{Severity: "high", Name: "alert(1) *重要*"}}}
	text, _ := telegramHTML(m)
	if strings.Contains(text, `\(`) || strings.Contains(text, `\*`) {
		t.Fatalf("Telegram の本文に markdown のバックスラッシュのエスケープが出た:\n%s", text)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
