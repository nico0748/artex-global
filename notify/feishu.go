package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// feishuChannel は Feishu（Lark を含む）のカスタムボットを実装し、インタラクティブなカードを使う。
//
// プラットフォームの特性：
//   - 署名のアルゴリズムは DingTalk と**異なり**、しかも非常に書き間違えやすい。feishuSign のコメントを参照。
//   - DingTalk と同じく業務エラーを HTTP 200 の body に入れる（code != 0）。
//   - カードの header は色のテンプレートに対応しているので、深刻度に配色を対応させ、メッセージ一覧でひと目で深刻さがわかるようにする。
type feishuChannel struct{}

func (feishuChannel) Kind() string { return KindFeishu }

// Feishu のカスタムボットはおよそ 5 回/秒、つまり 100 回/分。
func (feishuChannel) DefaultRatePerMin() int { return 100 }

// Webhook アドレスの末尾がボットの一意な識別子で、認証情報にあたる。
func (feishuChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// 同様に：Webhook アドレスを変えるときは、新しいアドレスに対して署名の鍵を改めて指定しなければならない。
func (feishuChannel) DestinationKeys() []string { return []string{"webhook"} }

func (feishuChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook 주소가 없습니다")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Webhook 주소가 올바르지 않습니다: %w", err)
	}
	return nil
}

func (c feishuChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	card, kept := feishuCard(m)
	payload := map[string]any{
		"msg_type": "interactive",
		"card":     card,
	}
	// 署名パラメータはメッセージと同じ階層にあり、secret を設定したときだけ現れる。
	if secret := cfgString(cfg, "secret"); secret != "" {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		payload["timestamp"] = ts
		payload["sign"] = feishuSign(ts, secret)
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		// 一部の版の Feishu の hook はこちらのフィールド名を使うので、あわせて対応する。
		StatusCode    int    `json:"StatusCode"`
		StatusMessage string `json:"StatusMessage"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("Feishu 응답을 해석하지 못했습니다: %w (%s)", err, snippet(raw))
	}
	if res.Code != 0 {
		return 0, Permanent(fmt.Errorf("Feishu에서 오류가 발생했습니다 (%d): %s", res.Code, res.Msg))
	}
	if res.StatusCode != 0 {
		return 0, Permanent(fmt.Errorf("Feishu에서 오류가 발생했습니다 (%d): %s", res.StatusCode, res.StatusMessage))
	}
	return kept, nil
}

// feishuSign は Feishu の公式ルールに従って署名を計算する。
//
// ここは特に落とし穴にはまりやすい：公式のサンプルは
//
//	hmac.new(string_to_sign.encode(), digestmod=sha256)
//
// つまり **key = timestamp + "\n" + secret、message は空** であり、直感的な
// 「key=secret, message=stringToSign」ではない——それはまさに DingTalk のアルゴリズムだ。両者のアルゴリズムはちょうど逆になっていて、
// もう一方の実装をまねて書くと必ず署名の検証に失敗する（19021 が返る）。
func feishuSign(timestamp, secret string) string {
	stringToSign := timestamp + "\n" + secret
	mac := hmac.New(sha256.New, []byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// feishuSeverityTemplate は脆弱性の深刻度をカードの header の配色テンプレートに対応させる。
// 不明な深刻度には grey を使う——low と紛らわしくならないよう blue は使わない。
func feishuSeverityTemplate(severity string) string {
	switch severity {
	case "critical":
		return "red"
	case "high":
		return "orange"
	case "medium":
		return "yellow"
	case "low":
		return "blue"
	default:
		return "grey"
	}
}

// feishuMaxCardBytes はカードの内容の控えめな上限。Feishu はカードの大きさを制限しており、超えるとメッセージごと拒否される。
// JSON の包装のオーバーヘッドも含め、公式の上限より明らかに低い値を取る。
const feishuMaxCardBytes = 24000

// feishuCard はインタラクティブなカードを作り、カードと**実際に書き込んだ項目の数**を返す。
// kept の用途は markdownBody と同じ：本当にカードに入った項目だけを配信済みにすべきである。
func feishuCard(m Message) (map[string]any, int) {
	elements := []any{}
	kept := 0
	if m.Batch {
		// まず項目単位で詰めてから見出しをつなぐ：見出しには「残りの N 件は次のメッセージに続く」と書くので、
		// N は実際に収まった件数から取らなければならない。
		kept = packItemCount(m.Items, feishuMaxCardBytes, markdownReservedBytes, "", byteSize, func(it Item, idx int) string {
			return feishuBatchLine(it, idx+1)
		})
		items := m.Items[:kept]
		elements = append(elements, feishuMarkdownDiv(markdownBatchIntro(m, items, len(m.Items))))
		for i, it := range items {
			elements = append(elements, feishuMarkdownDiv(feishuBatchLine(it, i+1)))
		}
		if m.HomeURL != "" {
			elements = append(elements, feishuButton("플랫폼에서 전체 보기", m.HomeURL))
		}
	} else if len(m.Items) > 0 {
		kept = 1
		it := m.Items[0]
		elements = append(elements, feishuMarkdownDiv(feishuItemLines(it)))
		if it.DetailURL != "" {
			elements = append(elements, feishuButton("상세 보기", it.DetailURL))
		}
	}

	card := map[string]any{
		"config":   map[string]any{"wide_screen_mode": true},
		"header":   map[string]any{"title": map[string]any{"tag": "plain_text", "content": markdownTitle(m)}},
		"elements": elements,
	}
	if len(m.Items) > 0 {
		card["header"].(map[string]any)["template"] = feishuSeverityTemplate(m.Items[0].Severity)
	}
	return card, kept
}

func feishuMarkdownDiv(content string) map[string]any {
	return map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": content}}
}

func feishuButton(label, url string) map[string]any {
	return map[string]any{
		"tag": "action",
		"actions": []any{map[string]any{
			"tag":  "button",
			"text": map[string]any{"tag": "lark_md", "content": label},
			"url":  url,
			"type": "primary",
		}},
	}
}

// feishuItemLines は1件の脆弱性の lark_md の本文を描画する。
//
// lark_md は markdown と同系統のテキスト形式で、同じくリンクや強調を解釈するので、外部から来る
// フィールドはすべて markdownText（1行化 + エスケープ）を通す——さもないと脆弱性のタイトル1つで
// Feishu の中にクリックできる外部リンクができてしまう。
func feishuItemLines(it Item) string {
	out := fmt.Sprintf("**%s · %s**", SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if it.IsStatusChange() {
		out += fmt.Sprintf("\n**상태 변경**: %s → %s",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		out += fmt.Sprintf("\n**유형**: %s", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		out += fmt.Sprintf("\n**자산**: %s", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			out += fmt.Sprintf("\n**개요**: %s", s)
		}
	}
	return out
}

// feishuBatchLine はまとめのカードの1件を描画する。
func feishuBatchLine(it Item, index int) string {
	line := fmt.Sprintf("**%d. %s · %s**", index, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		line += " — " + markdownText(a, 0)
	}
	return line
}
