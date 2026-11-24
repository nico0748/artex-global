package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"
)

// dingTalkChannel は DingTalk のカスタムボットを実装する。
//
// プラットフォームの特性（ここでの実装の取捨選択を決めている）：
//   - ボット1つにつき 20 件/分のレート制限があり、超えると黙って捨てられる（HTTP は 200 のこともある）。
//     そのためレート制限はクライアント側で行う必要がある。DefaultRatePerMin を参照。
//   - セキュリティ設定は3つから1つを選ぶ：署名 / カスタムキーワード / IP 許可リスト。署名は
//     メッセージの内容に依存しない唯一の方式なので、署名だけに対応する（3つとも使わない素の webhook にも対応する）。
//   - 成功も失敗も HTTP 200 を返し、body の errcode で区別する——errcode を確かめないと
//     配信の失敗を成功と記録してしまう。
type dingTalkChannel struct{}

func (dingTalkChannel) Kind() string { return KindDingTalk }

func (dingTalkChannel) DefaultRatePerMin() int { return 20 }

// DingTalk の Webhook アドレスには access_token が含まれており、それ自体が認証情報なので、全体をマスクする。
func (dingTalkChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// 宛先は DingTalk の Webhook アドレスそのもの。アドレスを変えるときは、新しいアドレスに対して署名の鍵も改めて指定しなければならない。
func (dingTalkChannel) DestinationKeys() []string { return []string{"webhook"} }

func (dingTalkChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook 주소가 없습니다")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Webhook 주소가 올바르지 않습니다: %w", err)
	}
	return nil
}

// Send はメッセージを1回届ける。戻りリンクがあって単発なら ActionCard（ボタン付き）を、そうでなければ markdown を使う。
func (c dingTalkChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	hook := cfgString(cfg, "webhook")
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := dingTalkSignedURL(hook, cfgString(cfg, "secret"), time.Now())
	if err != nil {
		return 0, Permanent(err)
	}

	title := markdownTitle(m)
	// DingTalk の markdown の本文にははっきりしたバイト数の上限はないが、証拠のフィールドが異常に膨らむのを防ぐため上限を設ける。
	text, kept := markdownBody(m, 20000)

	var payload any
	if !m.Batch && len(m.Items) == 1 && m.Items[0].DetailURL != "" {
		payload = map[string]any{
			"msgtype": "actionCard",
			"actionCard": map[string]any{
				"title":          title,
				"text":           text,
				"btnOrientation": "0",
				"singleTitle":    "상세 보기",
				"singleURL":      m.Items[0].DetailURL,
			},
		}
	} else {
		payload = map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]any{"title": title, "text": text},
		}
	}

	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	// DingTalk は業務エラーを 200 のレスポンスの中に隠す。
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("DingTalk 응답을 해석하지 못했습니다: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 301000 は署名の検証失敗、310000 はキーワードの不一致——どちらも設定の誤りで、
		// 再試行しても直らない。
		return 0, Permanent(fmt.Errorf("DingTalk에서 오류가 발생했습니다 (%d): %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}

// dingTalkSignedURL は公式の署名ルールに従って webhook に timestamp と sign のパラメータを付け加える。
//
// ルール：署名対象の文字列 = timestamp + "\n" + secret。HMAC-SHA256 の**鍵も secret**で、
// 結果を base64 にしてから URL エンコードする。timestamp はミリ秒。secret が空ならそのまま返し、
// 署名を有効にしていないボットにも対応する。
func dingTalkSignedURL(hook, secret string, now time.Time) (string, error) {
	if secret == "" {
		return hook, nil
	}
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	u, err := url.Parse(hook)
	if err != nil {
		// err はそのまま渡さない：url.Parse のエラー文には完全なアドレス（access_token を含む）が入っている。
		return "", fmt.Errorf("Webhook 주소를 해석하지 못했습니다: %s", redactRequestTarget(hook))
	}
	q := u.Query()
	q.Set("timestamp", ts)
	q.Set("sign", sign)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// validateHTTPURL はアドレスが使えるか、プロトコルが対応しているかを確かめ、IP リテラルの宛先については内部ネットワークかを判断する。
//
// 気をつけている点は2つ：
//
//  1. **エラーメッセージは必ず秘匿化する**。url.Parse 自身が返すのは *url.Error で、その Error() には
//     **元のアドレス全体**が入っている。そしてこの機能が扱う各社のアドレスには認証情報が埋め込まれている（DingTalk の access_token、
//     WeCom の key、Telegram の bot token、Feishu の hook id）。以前はここで直接 `return err` していたため、
//     「アドレスの形式が不正」というエラーが認証情報を持ち出し、テスト用 API の 400 レスポンス、
//     配信のたびに DB に記録される last_error、サーバーのログ、配信履歴の API に流れていた。
//
//  2. **IP リテラルはその場で内部ネットワークかを判断し**、ドメインは接続の段階に任せる（実際に効くのは blockInternalDial で、
//     DNS リバインディングにも対応できる）。ここでも一度判断するのは、最初の配信が失敗するのを待たずに、
//     設定の保存時に知らせるため。
//
// プロトコルの制限は防御のため：file:///gopher:// などは http.Client に予想外の
// 動作をさせうる（scheme の確認ですでに止めているが、この面を開ける理由はない）。
func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("주소를 해석할 수 없습니다 (%s)", redactRequestTarget(raw))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("http 또는 https 만 지원합니다. 받은 값: %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("호스트 이름이 없습니다")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && isBlockedDialIP(ip) && !allowLocalTargets() {
		return fmt.Errorf("로컬 또는 링크 로컬 주소 %s 로는 전송하지 않습니다 (로컬 서비스로 보내야 하면 %s=1 로 설정하세요)", ip, AllowLocalTargetsEnv)
	}
	return nil
}
