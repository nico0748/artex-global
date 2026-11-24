package notify

import (
	"strings"
	"unicode/utf8"
)

const ellipsis = "…"

// TruncateBytes は s を max バイト以下に切り詰め、結果が正しい UTF-8 で文字を途中で切らないことを保証する。
//
// 文字の境界で切る必要がある理由：WeCom のグループボットの markdown には 4096 **バイト**の厳密な上限があり（文字数では
// ない）、日本語や中国語は1文字 3 バイトになる。バイト単位でそのままスライスすると1文字を2つに割ってしまい、不正な
// UTF-8 ができる——プラットフォーム側はメッセージごと拒否するか、文字化けした四角で表示する。ここでは予算の位置から
// いちばん近い rune の先頭バイトまで戻る（utf8.RuneStart で継続バイト 0b10xxxxxx を判定する）。
//
// max<=0 なら制限しない。切り詰めたあとは省略記号を付け足す。ただし max が小さすぎて省略記号が入らない場合を除く。
func TruncateBytes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	budget := max - len(ellipsis)
	suffix := ellipsis
	if budget < 0 {
		// max が省略記号より短い：省略記号はあきらめて単純に切り詰め、結果がかえって max を超えるのを避ける。
		budget = max
		suffix = ""
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}

// OneLine は複数行の文を1行にまとめる：すべての空白をたたみ、それから文字数で切り詰める。
// IM メッセージのタイトル行に使う——要約にはよく改行があり、そのまま表やタイトルに入れるとレイアウトが崩れる。
// max<=0 なら長さを制限しない。
func OneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return TruncateRunes(s, max)
}

// TruncateRunes は s を max 文字（バイトではない）以下に切り詰め、超えたら省略記号を付け足す。
// max<=0 なら制限しない。
//
// TruncateBytes との違いはプラットフォームの基準にある：WeCom はバイト数で、Telegram は文字数で長さを制限する。
// 基準を取り違えてもエラーにはならず、メッセージが予想よりずっと短く切られるだけだ（日本語や中国語は1文字 = 3 バイトで、
// バイト数で 4096 に切ると約 1365 文字しか残らない）。だから両方の関数を残し、チャネルに応じて使い分けなければならない。
func TruncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + ellipsis
}

// TruncateHTML は HTML の断片を文字数で切り詰め、途中で切れたタグができないことを保証する。
//
// HTML をそのまま文字数で切り詰めると `<a href="htt` のような欠けたタグができ、プラットフォームのパーサーは
// エラーにしてメッセージごと拒否するか、後ろの本文を属性値として飲み込んでしまう。ここでは、まず文字数で切り詰め、
// それから末尾に閉じていない `<` があるかを確かめ、あればその手前まで戻る。
//
// タグの釣り合いは取らない（</b> などを補わない）：Telegram の HTML パーサーは閉じていないタグを自動で閉じるし、
// 自前で釣り合いを取るには属性内の引用符、コメント、自己終了タグを扱う必要があり、複雑さが効果に見合わない。
func TruncateHTML(s string, max int) string {
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	cut := TruncateRunes(s, max)
	// 末尾が `<` で始まる断片（最後の `<` のあとに `>` がない）なら、`<` の手前まで戻る。
	if lt := strings.LastIndex(cut, "<"); lt >= 0 && !strings.Contains(cut[lt:], ">") {
		cut = cut[:lt]
	}
	// 末尾が途中で切れた HTML エンティティ（`&amp;` が `&amp` に切れたなど）なら、同じく戻る。
	// エンティティの断片は、エンティティしか認めないパーサーでは**メッセージ全体**を拒否させうる——長さの上限を
	// 超えるまとめのメッセージはもともとよくあるので、そのために通知を丸ごと失う価値はない。
	if amp := strings.LastIndex(cut, "&"); amp >= 0 && !strings.Contains(cut[amp:], ";") {
		cut = cut[:amp]
	}
	return cut
}

// packItemCount は予算内に**完全な形で**何件収まるかを計算し、まとめのメッセージを項目単位で詰めるのに使う。
//
// 全文を描画してから切り詰めるのでなく項目単位にする理由：切り詰めると後半の項目が跡形もなく消えるのに、
// それらの配信記録は配信済みにされてしまう——メッセージを見てもわからず、配信履歴を見てもわからず、
// 脆弱性がこうして失われる。項目単位で詰めれば、収まらない項目は DB に残って次の一括分になり、
// 呼び出し側が受け取る kept がこのメッセージで本当に届いた件数になる。
//
// 引数：maxSize<=0 なら制限しない。reserve はメッセージの見出し/末尾のために取っておく量。
// size は計量を受け持つ（プラットフォームごとに基準が違う：WeCom/DingTalk はバイト数、Telegram は文字数——
// 基準を取り違えてもエラーにはならず、日本語や中国語のメッセージが上限よりずっと小さく縮められるだけ）。
// render は idx 番目の項目を実際の文に描画する——長さは内容によって違うので、見積もりには頼れない。
//
// （項目が残っている限り）少なくとも 1 を返す。1件だけ極端に長い場合もその1件は送り、呼び出し側の
// 最後の切り詰めを受け皿にする。さもないと長すぎる脆弱性1件が一括分全体を永久にその場に留める。
func packItemCount(items []Item, maxSize, reserve int, footer string, size func(string) int, render func(Item, int) string) int {
	if maxSize <= 0 {
		return len(items)
	}
	budget := maxSize - reserve - size(footer)
	if budget < 0 {
		budget = 0
	}
	used := 0
	for i, it := range items {
		used += size(render(it, i))
		if used > budget && i > 0 {
			return i
		}
	}
	return len(items)
}

// byteSize / runeSize は packItemCount の2種類の計量の基準。名前を付けて、呼び出し箇所に
// 素の func(s string) int のクロージャが現れるのを避ける。さもないとどちらの基準を使っているかひと目でわかりにくい。
func byteSize(s string) int { return len(s) }
func runeSize(s string) int { return utf8.RuneCountInString(s) }

// assetLine は資産の一覧を1行の表示用の文に描画し、limit 件を超えたら残りを省いて総数を添える。
// 1件の脆弱性が数十の資産に結びつくことがあり、全部挙げるとメッセージがあふれる。
func assetLine(assets []string, limit int) string {
	if len(assets) == 0 {
		return ""
	}
	if limit <= 0 || len(assets) <= limit {
		return strings.Join(assets, ", ")
	}
	return strings.Join(assets[:limit], ", ") + " 등 " + itoa(len(assets)) + "개"
}

// itoa は strconv.Itoa の短い別名で、表示用の文をつなぐときだけ使い、あちこちで strconv を import するのを避ける。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
