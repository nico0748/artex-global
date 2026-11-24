package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateBytesKeepsValidUTF8(t *testing.T) {
	// これはこのパッケージで最も重要な不変条件。WeCom は**バイト数**で長さを制限し、日本語や中国語は1文字 3 バイトで、
	// バイト単位で無理に切る実装はどれも1文字を半分に切り、不正な UTF-8 を生んでプラットフォームに拒否される。
	// 長さが互いに素な複数の日英混在の入力で、ありうる切れ目をすべて突く。
	inputs := []string{
		"日本語のテスト内容",
		"混合 mixed 内容 content",
		"aあbいcうdえe",
		"🔴🟠🟡🔵", // 4 バイトの emoji。切り間違いがよりはっきり出る
		strings.Repeat("脆弱性", 100),
	}
	for _, in := range inputs {
		for max := 1; max <= len(in)+2; max++ {
			got := TruncateBytes(in, max)
			if !utf8.ValidString(got) {
				t.Fatalf("入力 %q max=%d: 不正な UTF-8 %q を生んだ", in, max, got)
			}
			if len(got) > max {
				t.Fatalf("入力 %q max=%d: 結果 %d バイトが上限を超えている", in, max, len(got))
			}
			// 切り詰めていないときは内容を変えてはならない。
			if len(in) <= max && got != in {
				t.Fatalf("入力 %q max=%d: 上限を超えていないのに内容を変えた -> %q", in, max, got)
			}
		}
	}
}

func TestTruncateBytesZeroMeansUnlimited(t *testing.T) {
	long := strings.Repeat("x", 10000)
	if got := TruncateBytes(long, 0); got != long {
		t.Fatal("max=0 は制限しないことを表すべき")
	}
	if got := TruncateBytes(long, -5); got != long {
		t.Fatal("max<0 は制限しないことを表すべき")
	}
}

func TestTruncateBytesEllipsisBudget(t *testing.T) {
	// max が省略記号そのものより小さいとき、省略記号を付け足したせいで逆に上限を超えてはならない。
	got := TruncateBytes("abcdefgh", 1)
	if len(got) > 1 {
		t.Fatalf("max=1 のとき結果 %q の長さ %d が上限を超えている", got, len(got))
	}
	// 通常は省略記号を付けるべき。
	if got := TruncateBytes("abcdefgh", 5); !strings.HasSuffix(got, ellipsis) {
		t.Fatalf("省略記号付きを期待したが、%q を得た", got)
	}
}

func TestTruncateRunesCountsCharactersNotBytes(t *testing.T) {
	// TruncateBytes との基準の違いは残さなければならない：Telegram は文字数で長さを制限し、
	// バイト数の基準を使うと日本語のメッセージが3分の1しか残らない。
	s := "一二三四五六七八九十"
	got := TruncateRunes(s, 5)
	if n := utf8.RuneCountInString(got); n != 5 {
		t.Fatalf("5 文字を期待したが、%d 文字を得た (%q)", n, got)
	}
	// 同じ文字列をバイト数の基準で切れば、明らかに短くなるべき。
	if utf8.RuneCountInString(TruncateBytes(s, 5)) >= 5 {
		t.Fatal("バイト数の基準は文字数の基準と同じ文字数を生むべきでない")
	}
}

func TestOneLineCollapsesWhitespace(t *testing.T) {
	got := OneLine("1行目\n\n2行目\tタブ付き   複数の空白", 0)
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("すべての空白をたたむべきだが、%q を得た", got)
	}
	if strings.Contains(got, "  ") {
		t.Fatalf("連続する空白を残すべきでないが、%q を得た", got)
	}
	// 切り詰めたあとも読めて正しいこと。
	got = OneLine("一二三四五六七八九十", 4)
	if n := utf8.RuneCountInString(got); n != 4 {
		t.Fatalf("4 文字を期待したが、%d を得た (%q)", n, got)
	}
}

func TestTruncateHTMLNeverCutsTagInHalf(t *testing.T) {
	// HTML をそのまま切り詰めると `<a href="htt` のような断片ができ、プラットフォームはメッセージごと拒否する。
	s := `<b>タイトル</b>本文本文本文<a href="https://example.com/very/long/path">詳細を見る</a>`
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		if n := utf8.RuneCountInString(got); max > 0 && n > max {
			t.Fatalf("max=%d: 結果 %d 文字が上限を超えている", max, n)
		}
		// 末尾に閉じていない `<` があってはならない（最後の区切りに `<` があるのに `>` がない）。
		if lt := strings.LastIndex(got, "<"); lt >= 0 && !strings.Contains(got[lt:], ">") {
			t.Fatalf("max=%d: 末尾のタグが切れている -> %q", max, got)
		}
	}
}

func TestAssetLineOmitsExcess(t *testing.T) {
	if got := assetLine(nil, 3); got != "" {
		t.Fatalf("資産がなければ空文字列を返すべきだが、%q を得た", got)
	}
	if got := assetLine([]string{"a", "b"}, 3); got != "a, b" {
		t.Fatalf("上限を超えていなければすべて挙げるべきだが、%q を得た", got)
	}
	// 上限を超えたら総数を必ず添えること。さもないと読む人は挙げられていない資産があと何件あるかわからない。
	got := assetLine([]string{"a", "b", "c", "d", "e"}, 2)
	if !strings.Contains(got, "등 5개") {
		t.Fatalf("総数 5 を添えるべきだが、%q を得た", got)
	}
}

func TestSeverityAndStatusLabels(t *testing.T) {
	if AtLeast("", "low") {
		t.Fatal("空の深刻度の序数は 0 で、どのしきい値でも止められるべき")
	}
	if !AtLeast("critical", "") {
		t.Fatal("空のしきい値は通すべき")
	}
	if got := StatusLabel("fixed"); got != "수정됨" {
		t.Fatalf("不明な状態の対応付けで %q を得た", got)
	}
	// 不明な状態はそのまま返し、ラベルを作り上げない。
	if got := StatusLabel("weird_status"); got != "weird_status" {
		t.Fatalf("不明な状態はそのまま返すべきだが、%q を得た", got)
	}
}

// TestTruncateHTMLNeverCutsEntity は監査で指摘された見落としを扱う：切り詰めは途中で切れた
// タグを避けるだけでなく、途中で切れた HTML エンティティも避けなければならない。
//
// `&amp;` が `&amp` に切れると、エンティティしか認めないパーサーは**メッセージ全体**を拒否しうる——
// 長すぎるまとめのメッセージはもともとよくあるので、代償が大きすぎる。
func TestTruncateHTMLNeverCutsEntity(t *testing.T) {
	s := "aaaa&amp;bbbb&lt;cccc&quot;dddd"
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		// 末尾に「& があるのに対応する ; がない」エンティティの断片があってはならない。
		if amp := strings.LastIndex(got, "&"); amp >= 0 && !strings.Contains(got[amp:], ";") {
			t.Fatalf("max=%d: 末尾にエンティティの断片 %q が残った", max, got[amp:])
		}
		if strings.Contains(got, "&amp\x00") {
			t.Fatalf("max=%d: 不正な形のエンティティが現れた", max)
		}
	}
}
