package notify

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// Channel は1つの通知チャネルのアダプタ。実装は必ず**状態を持たない**こと：同じインスタンスが複数のチャネル
// 設定から並行して使い回されるので、認証情報はすべて cfg 引数で受け取る。
type Channel interface {
	// Kind はチャネルの種類の識別子を返す。レジストリのキーと一致させること。
	Kind() string
	// Validate は設定の保存時に呼ばれ、必須フィールドと形式を検証する。返したエラーはそのまま
	// 設定者に表示されるので、文言は漠然とした「設定が無効」ではなく「どのフィールドが足りないか」を示すこと。
	Validate(cfg map[string]any) error
	// Send はメッセージを1回届け、**実際に届いた項目の数**とエラーを返す。
	//
	// 件数を返す理由：各プラットフォームにはメッセージの長さの上限があり、まとめのメッセージに一括分が収まらないと切り詰められる。
	// 呼び出し側が無条件に一括分すべてを配信済みにすると、切り捨てられた項目は消えてしまう——メッセージにも見えず、
	// 配信履歴でも成功と表示され、脆弱性が一度も送られていないことに気づける場所がどこにもない。kept を返せば、
	// 呼び出し側は先頭の kept 件だけを配信済みにし、残りは次の一括分に回せる。
	//
	// エラーを返したら配信の失敗を表し、そのうち *PermanentError は再試行すべきでないことを表す。
	// 失敗時の kept には意味がないので、呼び出し側は無視すること。
	Send(ctx context.Context, cfg map[string]any, m Message) (int, error)
	// DefaultRatePerMin はそのチャネルの公式が推奨する1分あたりの配信上限を返し、チャネルのインスタンスを
	// 新規作成するときの既定のレート制限値にする。0 を返したら既知の制限がないことを表す。
	DefaultRatePerMin() int
	// SecretKeys はそのチャネル設定のうち認証情報にあたるキー名を返す。API で返すときこれらのキーの値はマスクされ、
	// 更新時にマスク値を受け取ったら DB の元の値を残す。どのフィールドが認証情報かを知っているのは実装自身だけなので
	// （WeCom は Webhook アドレス全体が認証情報だが、DingTalk はその中の secret だけ）、
	// この知識はチャネルが提供しなければならず、上位層が推測してはならない。
	SecretKeys() []string
	// DestinationKeys はそのチャネル設定のうち「メッセージをどこへ送るか」を決めるキー名を返す。
	//
	// SecretKeys と同じくセキュリティに関わる：宛先と認証情報は独立した2組のフィールドであり、
	// 「宛先だけ変えて認証情報はそのまま残す」ことを許すと、チャネル設定を変えられる人なら誰でも DB にある本物の認証情報を
	// 自分の管理するサーバーへ送れてしまい、チャネル設定のマスクがまったく意味をなさなくなる。
	// 詳しくは PrepareConfigUpdate を参照。
	DestinationKeys() []string
}

// registry はチャネルのレジストリ。init() による自己登録でなく、わざと明示的なリテラルにしている：こうすれば「どのチャネルがあるか」が
// 1か所で全部見え、チャネルを追加したときの漏れが実行時の副作用ではなくコンパイル時にわかる。
var registry = map[string]Channel{
	KindDingTalk: dingTalkChannel{},
	KindFeishu:   feishuChannel{},
	KindWeCom:    weComChannel{},
	KindWebhook:  webhookChannel{},
	KindTelegram: telegramChannel{},
	KindEmail:    emailChannel{},
}

// Get は種類からチャネルの実装を取得する。
func Get(kind string) (Channel, bool) {
	c, ok := registry[kind]
	return c, ok
}

// ValidKind は kind が対応しているチャネルの種類かを報告する。
func ValidKind(kind string) bool {
	_, ok := registry[kind]
	return ok
}

// Kinds は対応しているすべてのチャネルの種類を辞書順で返す（UI のドロップダウンで安定して表示するため）。
func Kinds() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PermanentError は再試行すべきでない配信の失敗を表す：認証情報の誤り、宛先による拒否、リクエストボディの不正など。
// 再試行が意味を持つのは一時的な故障（ネットワークの揺らぎ、レート制限、相手側の 5xx）だけで、恒久的な失敗に待ち時間を置いて再試行を繰り返しても
// 成功せず、本当のエラーが再試行のログに埋もれてしまう。
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent は err を恒久的な失敗として印を付ける。err が nil なら nil を返すので、
// `return Permanent(someCheck())` と書ける。
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent は err の連鎖に恒久的な失敗の印があるかを報告する。
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

// ---- 設定読み取りの helper ----
//
// チャネル設定はデータベースの JSONB 列から来ており、encoding/json でデシリアライズすると map[string]any になる。
// 数値はすべて float64、配列は []any。以下の helper はこの変換を一手に引き受け、ユーザーが
// UI で空欄にしたことによる型のずれ（ポートを文字列で入れるなど）も許容する。

// cfgString は文字列の設定項目を取得し、前後の空白は一律に削る——Web フォームからのコピー＆ペーストでは付きやすい。
func cfgString(cfg map[string]any, key string) string {
	v, ok := cfg[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// cfgInt は整数の設定項目を取得し、float64（JSON の既定）と文字列の両方に対応する。
func cfgInt(cfg map[string]any, key string) int {
	switch v := cfg[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// cfgBool は真偽値の設定項目を取得し、文字列の "true"/"1" にも対応する。
func cfgBool(cfg map[string]any, key string) bool {
	switch v := cfg[key].(type) {
	case bool:
		return v
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		return s == "true" || s == "1" || s == "yes"
	default:
		return false
	}
}

// cfgStrings は文字列配列の設定項目を取得し、自動で空白を削って空文字列を捨てる。
func cfgStrings(cfg map[string]any, key string) []string {
	raw, ok := cfg[key].([]any)
	if !ok {
		// 単一の文字列も受け付け、値が1つだけのフォーム送信に対応する。
		if s := cfgString(cfg, key); s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// cfgMap は文字列マップの設定項目（カスタム HTTP ヘッダーなど）を取得し、キーと値の空白を削り、空のキーを捨てる。
func cfgMap(cfg map[string]any, key string) map[string]string {
	raw, ok := cfg[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		out[k] = s
	}
	return out
}
