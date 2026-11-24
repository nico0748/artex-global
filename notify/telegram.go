package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// telegramTextLimit は Telegram の sendMessage の text フィールドの上限（文字数）。
const telegramTextLimit = 4096

// telegramChannel は Telegram Bot API を実装する。
//
// プラットフォームの特性：
//   - 認証はすべて URL の path の中にあり（/bot<token>/sendMessage）、署名は要らない。
//   - MarkdownV2 でなく HTML の解析モードを使う：MarkdownV2 は `_*[]()~`>#+-=|{}.!` の
//     18 文字のエスケープを求め、1つでも漏らすとメッセージごと拒否される。HTML なら & < > の3つだけでよい。
//   - 業務エラーは同じく HTTP 200 に隠されており、ok フィールドで判断する。
type telegramChannel struct{}

func (telegramChannel) Kind() string { return KindTelegram }

// Telegram は個人チャットでおよそ 1 件/秒、グループで 20 件/分。控えめな値を取る。
func (telegramChannel) DefaultRatePerMin() int { return 20 }

// Bot Token は完全な認証情報。chat_id は受信者にすぎず秘密ではない（それを手に入れても Token がなければメッセージは送れない）。
func (telegramChannel) SecretKeys() []string { return []string{"bot_token"} }

// base_url は Token をどの API のエンドポイント（自前のリバースプロキシなど）に送るかを決めるので、変えるときは Token について改めて態度を示す必要がある。
func (telegramChannel) DestinationKeys() []string { return []string{"base_url"} }

func (telegramChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "bot_token") == "" {
		return errors.New("Bot Token이 없습니다")
	}
	if cfgString(cfg, "chat_id") == "" {
		return errors.New("Chat ID가 없습니다")
	}
	if base := cfgString(cfg, "base_url"); base != "" {
		if err := validateHTTPURL(base); err != nil {
			return fmt.Errorf("API 주소가 올바르지 않습니다: %w", err)
		}
	}
	return nil
}

func (c telegramChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := telegramEndpoint(cfg)
	if err != nil {
		return 0, Permanent(err)
	}
	text, kept := telegramHTML(m)
	payload := map[string]any{
		"chat_id":                  cfgString(cfg, "chat_id"),
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": false,
	}
	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("Telegram 응답을 해석하지 못했습니다: %w (%s)", err, snippet(raw))
	}
	if res.OK {
		return kept, nil
	}
	// 429 はレート制限で、待ち時間を置いてから再試行すれば有効。それ以外（400 の引数エラー、401 の token の誤り、403 のブロック、
	// 404 の chat 不存在）はどれも設定の問題で、再試行しても直らない。
	if res.ErrorCode == 429 {
		return 0, fmt.Errorf("Telegram 요청 제한에 걸렸습니다: %s", res.Description)
	}
	return 0, Permanent(fmt.Errorf("Telegram 응답 오류 %d: %s", res.ErrorCode, res.Description))
}

// telegramEndpoint は sendMessage のアドレスを組み立てる。base_url が空なら公式の API を使い、
// 空でなければ自前の Bot API のリバースプロキシ（中国国内のネットワークでよくある要望）に使う。
func telegramEndpoint(cfg map[string]any) (string, error) {
	base := cfgString(cfg, "base_url")
	if base == "" {
		base = "https://api.telegram.org"
	}
	base = strings.TrimSuffix(base, "/")
	token := cfgString(cfg, "bot_token")
	raw := base + "/bot" + token + "/sendMessage"
	u, err := url.Parse(raw)
	if err != nil {
		// err はそのまま渡さない：アドレスには Bot Token が含まれており、この時点では addr さえ返すべきでない。
		return "", fmt.Errorf("API 주소를 조합하지 못했습니다 (API 주소: %s)", redactRequestTarget(base))
	}
	return u.String(), nil
}

// telegramHTML は HTML の本文を描画し、本文と実際に書き込んだ項目の数を返す（Channel.Send を参照）。
func telegramHTML(m Message) (string, int) {
	var b strings.Builder
	b.WriteString("<b>" + telegramEscape(markdownTitle(m)) + "</b>\n")
	if m.Batch {
		// Telegram の上限は**文字数**なので、詰めるときも文字数で計量する（runeSize）。
		footer := ""
		if m.HomeURL != "" {
			footer = fmt.Sprintf("\n\n<a href=\"%s\">플랫폼에서 전체 보기</a>", telegramEscapeAttr(m.HomeURL))
		}
		kept := packItemCount(m.Items, telegramTextLimit, telegramReservedRunes, footer, runeSize, func(it Item, idx int) string {
			return telegramBatchLine(it, idx+1)
		})
		items := m.Items[:kept]
		b.Reset()
		b.WriteString("<b>" + telegramEscape(telegramBatchTitle(m, items, len(m.Items))) + "</b>")
		for i, it := range items {
			b.WriteString("\n" + telegramEscape(telegramBatchLine(it, i+1)))
		}
		b.WriteString(footer)
		return TruncateHTML(b.String(), telegramTextLimit), kept
	}
	if len(m.Items) == 0 {
		return b.String(), 0
	}
	it := m.Items[0]
	if it.IsStatusChange() {
		b.WriteString(fmt.Sprintf("\n<b>상태 변경</b>: %s → %s",
			telegramEscape(StatusLabel(it.FromStatus)), telegramEscape(StatusLabel(it.ToStatus))))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		b.WriteString("\n<b>유형</b>: " + telegramEscape(it.VulnClass))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		b.WriteString("\n<b>자산</b>: " + telegramEscape(a))
	}
	if s := OneLine(it.Summary, maxSummaryRunes); s != "" {
		b.WriteString("\n<b>개요</b>: " + telegramEscape(s))
	}
	if it.DetailURL != "" {
		b.WriteString(fmt.Sprintf("\n\n<a href=\"%s\">상세 보기</a>", telegramEscapeAttr(it.DetailURL)))
	}
	return TruncateHTML(b.String(), telegramTextLimit), 1
}

// telegramReservedRunes はメッセージのタイトルと、出るかもしれない切り詰めの注記のために取っておく（文字数で数える）。
const telegramReservedRunes = 160

// telegramBatchLine はまとめの中の1件を描画する（エスケープはせず、呼び出し側がまとめてエスケープする）。
func telegramBatchLine(it Item, idx int) string {
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		return fmt.Sprintf("%d. %s · %s — %s", idx, SeverityLabel(it.Severity), it.Title(), a)
	}
	return fmt.Sprintf("%d. %s · %s", idx, SeverityLabel(it.Severity), it.Title())
}

// telegramBatchTitle はまとめのメッセージのタイトル行を描画する。件数は一括分の総数ではなく**このメッセージに実際に含まれる**件数を使う。
// さもないと読む人は見出しに書かれた数がすべてだと思ってしまう。
func telegramBatchTitle(m Message, items []Item, total int) string {
	title := fmt.Sprintf("취약점 요약 · 총 %d건", total)
	if extra := total - len(items); extra > 0 {
		title += fmt.Sprintf(" (앞 %d건만 표시, 나머지 %d건은 다음 메시지에서 이어집니다)", len(items), extra)
	}
	if m.WindowMinutes > 0 {
		title = fmt.Sprintf("최근 %d분간 · %s", m.WindowMinutes, title)
	}
	return title
}

// telegramEscape は HTML のテキスト内容をエスケープする。
// Telegram はこの3種類のエンティティしか認めず、エスケープすると &amp; などの既存のエンティティは二重にエスケープされる——これは
// 期待どおりの動作だ：表示したいのは元の文字であって、ユーザーに HTML を注入させることではない。
func telegramEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// telegramEscapeAttr は HTML の属性値をエスケープする。テキストのエスケープに加えて引用符も処理する必要がある——
// URL に引用符が入っていると href 属性が早まって閉じ、後ろの内容が注入の入口になる。
func telegramEscapeAttr(s string) string {
	s = telegramEscape(s)
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}
