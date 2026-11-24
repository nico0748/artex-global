package notify

import (
	"fmt"
	"strings"
)

// このファイルは「Markdown 系」のチャネル（DingTalk、WeCom）が共有するメッセージの描画。
// Feishu はカードの JSON、Telegram は HTML、メールは HTML を使い、それぞれアダプタの中で描画する。

// maxAssetsShown はメッセージに最大で何件の資産を挙げるか。1件の脆弱性が数十の資産に結びつくことがあり、
// 全部挙げるとメッセージがあふれるうえ情報としての価値もない——4件目以降のドメインを IM で見る人はいない。
const maxAssetsShown = 3

// maxSummaryRunes は要約を何文字に縮めるか。IM のメッセージは「詳細を見るよう促す」もので、
// レポートそのものではなく、完全な内容はプラットフォームにある。
const maxSummaryRunes = 120

// markdownReservedBytes はメッセージの見出し（まとめの行 + 深刻度の分布 + 切り詰めの注記があればそれ）
// と末尾（プラットフォームへのリンク）のために取っておく。項目単位で詰めるときにこの分を予算から差し引き、見出しと末尾が切り詰められないようにする——
// 見出しや末尾が切られると、読む人は「これはどの一括分で、表示されていないのがあと何件あるか」すらわからなくなる。
const markdownReservedBytes = 320

// markdownEscape は markdown のメタ文字をエスケープする。
//
// 必ず行う理由：脆弱性のタイトル、要約、種類、資産の表示名はすべて**信頼できない出所**から来る——
// タイトルと要約はモデルの出力（モデルが読むのは検査対象のレスポンス）に由来し、資産の url はスキャンで
// 得た完全な URL（対象が制御できるクエリ文字列を含む）だ。エスケープしないと、タイトルが
//
//	ログイン画面の SQL インジェクション\n[緊急：ここをクリックしてアカウントを確認](http://attacker.tld)
//
// である脆弱性は、セキュリティエンジニアの DingTalk/Feishu で**クリックできる外部リンク**として描画される。また
// `![](http://attacker.tld/beacon)` は描画時にクライアントが取得するので、「この脆弱性はもう見られた」と
// 知らせ、読んだ人の IP を漏らすことになる。悪意のない内容でも、注入された太字や
// 引用ブロックが下にある深刻な脆弱性を折りたたみの線の外へ押し出しうる。
//
// エスケープの対象は、見出し/リンク/強調/リスト/引用/取り消し線といった、構造を変えたりクリックできる
// 要素を生んだりする文字を網羅する。`\` は必ず最初に処理する。さもないとあとで足したバックスラッシュをもう一度エスケープしてしまう。
func markdownEscape(s string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		"`", "\\`",
		"*", `\*`,
		"_", `\_`,
		"[", `\[`,
		"]", `\]`,
		"(", `\(`,
		")", `\)`,
		"!", `\!`,
		"#", `\#`,
		">", `\>`,
		"|", `\|`,
		"~", `\~`,
	)
	return replacer.Replace(s)
}

// markdownText は信頼できない文を1行にまとめてエスケープし、markdown の本文に使う。
// 1行化はエスケープと対をなすもう半分：改行それ自体で新しいリスト項目や引用ブロックを偽造でき、
// エスケープ文字ではそれを止められない。
func markdownText(s string, maxRunes int) string {
	return markdownEscape(OneLine(s, maxRunes))
}

// markdownTitle はメッセージのタイトル（IM プラットフォームのタイトルバー/カードのタイトル）を返し、内容は**エスケープしていない原文**。
//
// ここではわざとエスケープしない：このタイトルは4種類の文脈の描画で共有される——markdown の本文、Telegram の
// HTML、Feishu のカードの plain_text、そして汎用 Webhook の JSON とメールの件名。文脈ごとに
// エスケープのルールが違う（markdown のエスケープを HTML に入れると見えるバックスラッシュが残り、JSON に入れると
// データを汚す）ので、エスケープはそれぞれの出力側が受け持つ。writeItem / feishuItemLines /
// telegramEscape を参照。以前に共有の関数で markdown のエスケープを足したところ、Telegram のメッセージに
// `\(1\)` のような見えるバックスラッシュが出た。
func markdownTitle(m Message) string {
	if m.Batch {
		return fmt.Sprintf("취약점 요약 · 총 %d건", len(m.Items))
	}
	if len(m.Items) == 0 {
		return "취약점 알림"
	}
	it := m.Items[0]
	return fmt.Sprintf("[%s] %s", SeverityLabel(it.Severity), OneLine(it.Title(), 0))
}

// markdownBody はメッセージの本文を描画し、本文と**実際に書き込んだ項目の数**を返す。
//
// 戻り値の kept は今回の配信で本当に届いた項目の数で、呼び出し側はこれをもとに先頭の kept 件だけを
// 配信済みにする——チャネルの長さの上限で外された項目は次の一括分に回すべきで、一緒に
// 成功扱いにしてはならない。これこそ「黙った取りこぼし」の原因だ：メッセージは切り詰められたのに、配信記録ではすべて届いたことになり、
// 後半が一度も送られていないことがどこからもわからない。
//
// maxBytes<=0 なら制限しない。
func markdownBody(m Message, maxBytes int) (string, int) {
	if !m.Batch {
		if len(m.Items) == 0 {
			return "", 0
		}
		var b strings.Builder
		writeItem(&b, m.Items[0], "", true)
		// 単発のメッセージは長すぎても送る（最後の切り詰めが受け皿になる）：脆弱性の一部の情報でも、
		// 1件も送らないよりはよい。
		return TruncateBytes(b.String(), maxBytes), 1
	}

	footer := ""
	if m.HomeURL != "" {
		footer = fmt.Sprintf("\n[플랫폼에서 전체 보기](%s)\n", m.HomeURL)
	}
	kept := packItemCount(m.Items, maxBytes, markdownReservedBytes, footer, byteSize, func(it Item, idx int) string {
		var b strings.Builder
		writeItem(&b, it, fmt.Sprintf("%d. ", idx+1), false)
		return b.String()
	})

	items := m.Items[:kept]
	var b strings.Builder
	b.WriteString(markdownBatchIntro(m, items, len(m.Items)))
	for i, it := range items {
		writeItem(&b, it, fmt.Sprintf("%d. ", i+1), false)
	}
	b.WriteString(footer)
	return TruncateBytes(b.String(), maxBytes), kept
}

// markdownBatchIntro はまとめのメッセージの冒頭を描画する：時間枠、件数、深刻度の分布。
// これがあれば、まとめを受け取った人はプラットフォームを開かなくても、この一括分をすぐ処理すべきかを判断できる。
//
// items は**実際に収まった**項目で、total はこの一括分の本来の総数。両者が違うときは
// 「あと何件が次のメッセージにあるか」を必ずはっきり書く——さもないと読む人は見出しに書かれた数がすべてだと思い、
// 送られなかった残りの項目は画面上にまったく存在しないことになる。
func markdownBatchIntro(m Message, items []Item, total int) string {
	var b strings.Builder
	if m.WindowMinutes > 0 {
		fmt.Fprintf(&b, "**최근 %d분간 신규 취약점 %d건**", m.WindowMinutes, total)
	} else {
		fmt.Fprintf(&b, "**신규 취약점 %d건**", total)
	}
	if extra := total - len(items); extra > 0 {
		fmt.Fprintf(&b, "(이 메시지에는 앞 %d건만 표시하며, 나머지 %d건은 다음 메시지에서 이어서 보냅니다)", len(items), extra)
	}
	// 深刻度ごとの分布を示し、深刻な項目があるかをひと目でわかるようにする。数えるのは**このメッセージに実際に含まれる**
	// 項目だけで、「緊急 3」と下で数えられる項目が一致するようにする。
	counts := map[string]int{}
	for _, it := range items {
		counts[it.Severity]++
	}
	var parts []string
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		if n := counts[sev]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", SeverityLabel(sev), n))
		}
	}
	if len(parts) > 0 {
		b.WriteString("\n" + strings.Join(parts, " · "))
	}
	b.WriteString("\n\n")
	return b.String()
}

// writeItem は1件の脆弱性の項目を描画する。
//
// prefix はまとめの一覧の通し番号に使う。single=true なら完全版（要約と戻りリンクを含む）を描画し、
// まとめの一覧では1行の要約だけを描画する——さもないと 50 件のまとめが長い文書になってしまう。
//
// 外部から来る内容（タイトル/種類/資産/要約）はすべて markdownText を通す：
// 1行化 + エスケープ。戻りリンクは管理者が設定した public_base_url から組み立てたもので、信頼できない内容ではなく、
// しかもクリックできるリンクでなければならないので、そのまま出力する。
func writeItem(b *strings.Builder, it Item, prefix string, single bool) {
	line := fmt.Sprintf("%s**%s · %s**", prefix, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if !single {
		// まとめのモード：1行で示し、資産と要約は縮めて後ろに続ける。
		var extras []string
		if a := assetLine(it.Assets, maxAssetsShown); a != "" {
			extras = append(extras, markdownText(a, 0))
		}
		if it.Summary != "" {
			extras = append(extras, markdownText(it.Summary, 60))
		}
		if len(extras) > 0 {
			line += " — " + strings.Join(extras, " · ")
		}
		b.WriteString(line + "\n")
		return
	}
	b.WriteString(line + "\n")
	if it.IsStatusChange() {
		fmt.Fprintf(b, "**상태 변경**: %s → %s\n",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		fmt.Fprintf(b, "**유형**: %s\n", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		fmt.Fprintf(b, "**자산**: %s\n", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			fmt.Fprintf(b, "**개요**: %s\n", s)
		}
	}
	if it.DetailURL != "" {
		fmt.Fprintf(b, "[상세 보기](%s)\n", it.DetailURL)
	}
}
