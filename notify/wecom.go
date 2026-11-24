package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// weComMarkdownLimit は WeCom のグループボットの markdown content の厳密な上限（バイト数で、文字数ではない）。
// これは6つのチャネルすべての中で最も厳しい制限で、TruncateBytes がある主な理由でもある。
const weComMarkdownLimit = 4096

// weComChannel は WeCom のグループボットを実装する。
//
// プラットフォームの特性：
//   - URL の key による認証だけで、署名には対応しない——そのため webhook のアドレスそのものが認証情報のすべてになる。
//   - markdown content の上限は 4096 **バイト**で、長すぎるとメッセージごと拒否される（切り詰めではない）。日本語や中国語は1文字 3 バイトなので、
//     本文に書けるのは千数百文字だけで、クライアント側で切り詰める必要がある。
//   - レート制限は 20 件/分で、これも同じくクライアント側のレート制限で受け止める。
type weComChannel struct{}

func (weComChannel) Kind() string { return KindWeCom }

func (weComChannel) DefaultRatePerMin() int { return 20 }

// WeCom の認証情報は Webhook の1か所（URL の key）だけで、しかも署名に対応しない——
// アドレス全体が認証情報のすべてで、他にマスクすべきフィールドはない。
func (weComChannel) SecretKeys() []string { return []string{"webhook"} }

// WeCom のフィールドは Webhook の1つだけで、それが宛先でもあり認証情報でもあるので、「宛先を変えたあとに残る認証情報」は存在しない。
func (weComChannel) DestinationKeys() []string { return []string{"webhook"} }

func (weComChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook 주소가 없습니다")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Webhook 주소가 올바르지 않습니다: %w", err)
	}
	return nil
}

func (c weComChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	// まとめの一括分は長くなりうる（50 件 × 1件1行 + 接頭辞）ので、4096 バイトはすぐに超える。
	// プラットフォームのエラーに頼らずここで切り詰める：拒否されるとこの一括分がまるごと失われるが、切り詰めれば少なくとも先頭の何件かは届く。
	content, kept := markdownBody(m, weComMarkdownLimit)
	payload := map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]any{"content": content},
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("WeCom 응답을 해석하지 못했습니다: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 45009 は API の呼び出し回数が制限を超えたこと——プラットフォームのレート制限の窓は移動するので、待ち時間を置いてからの再試行は有効だ。
		// そのため明示的に再試行できるものに分類する。ここに来たのはクライアントの rate_per_min の設定が攻めすぎということで、
		// 再試行は受け皿にすぎず、本当の直し方はそのチャネルのレート制限の値を下げることだ。
		if res.ErrCode == 45009 {
			return 0, fmt.Errorf("WeCom 요청이 제한되었습니다 (%d): %s", res.ErrCode, res.ErrMsg)
		}
		// 93000 は webhook の key が無効——恒久的な失敗で、再試行しても直らない。
		return 0, Permanent(fmt.Errorf("WeCom에서 오류가 발생했습니다 (%d): %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}
