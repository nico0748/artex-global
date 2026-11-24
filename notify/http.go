package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// allowLocalTargets はメッセージをループバック / リンクローカルのアドレスに届けることを許すかを決める。
//
// 既定は拒否。これらのアドレス範囲は IM のボットや公開されたメールサーバーが現れる場所ではなく、しかもそこから
// 届くものは機微である：同じマシンの別のサービスの管理ポートや、クラウド環境のメタデータのエンドポイント
// （169.254.169.254。インスタンスの認証情報を読める）。配信先は管理者が設定するが、XSS/CSRF に
// 乗っ取られた管理セッションや、同じ JWT を共有する2人目は、設定を変えるだけでレスポンスの内容を読み戻せる
// ——doJSON は 4xx/5xx のレスポンスボディの先頭 200 バイトを last_error に書き、配信履歴の API が
// それを返すので、これは半ば見えない読み取りの手段になる。
//
// ただし「ローカルの SMTP 中継」（127.0.0.1:25 の postfix）は自前のメールでよくある設定なので、
// 一律に止めると困る人が出る。そこでハードコードで通すのではなく、明示的な抜け道を残している：
// ARTEX_NOTIFY_ALLOW_LOCAL=1 を設定すれば許可する。
//
// AllowLocalTargetsEnv として公開しているのは、テストで明示的に有効にできるようにするため——このパッケージと server パッケージの
// テストケースは 127.0.0.1 上の httptest の偽の受信側を多用しており、有効にしないとすべて防御に止められる。
const AllowLocalTargetsEnv = "ARTEX_NOTIFY_ALLOW_LOCAL"

func allowLocalTargets() bool {
	v := strings.TrimSpace(os.Getenv(AllowLocalTargetsEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

// isBlockedDialIP は宛先の IP が「既定では配信を許さない」アドレス範囲に属するかを報告する。
//
// 拒否するのはループバック、リンクローカル（クラウドのメタデータ 169.254.169.254 を含む）、未指定、マルチキャストだけ。
// RFC1918 のプライベートネットワークは**拒否しない**：社内の自前の Mattermost / SMTP 中継はごく一般的な正当な使い方で、
// それまで止めると実際の環境で機能がそのまま使えなくなる。この取捨は意図したもの——
// 防御は本当に機微な宛先を止めつつ、通常の構成まで使えなくしてはならない。
func isBlockedDialIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// IPv4 にマップされた IPv6（::ffff:127.0.0.1）は IPv4 に戻してから判定する。さもないと確認をすり抜ける。
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// blockInternalDial は http.Transport のダイヤラの Control フックで、**接続の確立時に**
// 宛先のアドレスを確かめる。
//
// 設定の保存時の検証だけでなく接続の段階に置く理由：ここが実際に効く場所だからだ。
// 設定の検証をすり抜ける2つの状況をあわせて防げる——DNS リバインディング（検証時は公開 IP に解決し、
// 実際の接続時には内部ネットワークに解決する）とリダイレクト（ホストをまたぐ移動はすでに拒否しているが、同じホスト内の移動で
// パスが別の場所を指すことはありうる）。
func blockInternalDial(_, address string, _ syscall.RawConn) error {
	if allowLocalTargets() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("대상 주소를 해석할 수 없습니다: %q", host)
	}
	if isBlockedDialIP(ip) {
		return fmt.Errorf("로컬 또는 링크 로컬 주소 %s 로는 전송하지 않습니다 (로컬 서비스로 보내야 하면 %s=1 로 설정하세요)", ip, AllowLocalTargetsEnv)
	}
	return nil
}

// notifyTransport は既定の Transport に接続時の防御を1つ足すだけ。
// Clone で既定の調整（接続プール、HTTP/2、タイムアウト、proxy など）をすべて残し、
// 確認を1つ足すために他の動作を変えることを避ける。
var notifyTransport = func() *http.Transport {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{}
	}
	clone := t.Clone()
	clone.DialContext = (&net.Dialer{Timeout: 10 * time.Second, Control: blockInternalDial}).DialContext
	return clone
}()

// httpClient はすべてのチャネルの配信で共有するクライアント。
//
// プロジェクト全体の出口プロキシ（server 側の GlobalProxy）はわざと使い回さ**ない**：そのプロキシはペネトレーションの
// 対象への通信用で、不安定なトンネルであることが多く、通知の可用性が対象ネットワークの揺らぎに左右されるべきではない。
// IM の通知は直接つなげばよい。タイムアウトは 15 秒——これより遅い相手は実際にはもう故障している。
//
// ホストをまたぐリダイレクトは拒否する：この機能の配信先はどれも「1つの固定された endpoint」の形で、通常は
// 別のホストにリダイレクトされない。しかもこれら各社の認証情報（DingTalk の access_token、WeCom の key、Telegram の
// bot token）は**URL の中にある**ので、ホストをまたぐ移動に従うことは認証情報をリダイレクト先に渡すのと同じだ。同じホスト内の
// 移動（末尾にスラッシュを補うなど）は引き続き許す。
var httpClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: notifyTransport,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("리다이렉트가 너무 많습니다")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("다른 호스트로의 리다이렉트는 허용하지 않습니다 (%s → %s)", via[0].URL.Host, req.URL.Host)
		}
		return nil
	},
}

// respBodyLimit はレスポンスボディを読む大きさを制限する。相手が異常なとき巨大な内容を返すことがあるが、必要なのは
// 配信履歴に表示するためのエラーコードと短いエラーの説明だけだ。
const respBodyLimit = 8 << 10

// doJSON はリクエストを1回送り、レスポンスボディ（長さを制限済み）を返す。
//
// payload が nil なら空の body を送る（GET や、プラットフォームが body を求めない場面で使う）。
// headers のキーと値はそのまま付け加え、汎用 Webhook のカスタムヘッダーに使う。
//
// エラーの分類がこの関数の中心的な役割：ネットワーク層の失敗と 5xx/408/429 は「再試行できる」とし、
// それ以外の 4xx は「恒久的な失敗」とする——403 を再試行しても同じエラーをログに3回書くだけだ。
func doJSON(ctx context.Context, method, url string, headers map[string]string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			// シリアライズの失敗はローカルのバグ（設定のフィールドの型の誤り）で、再試行してもよくならない。
			return nil, Permanent(fmt.Errorf("요청 본문을 만들지 못했습니다: %w", err))
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		// URL が不正——たいていはユーザーがアドレスを書き間違えたもので、恒久的な失敗にあたる。
		// ここでも err はそのまま渡せない：url.Parse のエラー文には完全なアドレスが含まれている。
		return nil, Permanent(fmt.Errorf("요청 주소가 올바르지 않습니다: %s", redactRequestTarget(url)))
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// 接続拒否、DNS の失敗、タイムアウト——多くは一時的な故障なので、待ち時間を置いた再試行に任せる。
		//
		// エラー文は必ず秘匿化してから外へ渡す。理由：http.Client.Do が返すのは *url.Error で、
		// その Error() は `Op "完全なURL": 下層のエラー` であり、この機能の各社の認証情報は**URL の中にある**
		// （DingTalk の access_token、WeCom の key、Feishu の hook id、Telegram の /bot<token>/）。
		// 秘匿化しないと、認証情報がこのエラー文に乗って4か所に流れる：notification_deliveries
		// の last_error（平文で DB に記録）、配信履歴の API のレスポンス（**チャネル設定のマスクをすり抜ける**）、
		// サーバーのログ、そしてテスト送信の API がフロントエンドに返す 502 の文。
		return nil, fmt.Errorf("요청에 실패했습니다: %s", redactTransportError(err))
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, respBodyLimit))
	if readErr != nil {
		return nil, fmt.Errorf("응답을 읽지 못했습니다: %w", readErr)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}
	// 429（レート制限）と 408（タイムアウト）は再試行する価値がある。それ以外の 4xx は設定や権限の問題で、再試行しても意味がない。
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
		return nil, fmt.Errorf("상대 서버가 요청을 제한하거나 응답이 지연되었습니다 (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("상대 서버에 오류가 발생했습니다 (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	return nil, Permanent(fmt.Errorf("상대 서버가 요청을 거부했습니다 (HTTP %d): %s", resp.StatusCode, snippet(raw)))
}

// snippet はレスポンスボディを1行の短い文にまとめ、エラーメッセージに使う。レスポンスには改行や大量の空白が含まれることがあり、
// そのまま last_error に入れると配信履歴のページのレイアウトが崩れる。
func snippet(raw []byte) string {
	return OneLine(string(raw), 200)
}

// redactRequestTarget は配信先を「scheme://host/…」にまとめ、エラーメッセージに使う。
//
// これはこのパッケージで唯一のアドレス秘匿化の基準で、わざと**十分に大ざっぱに**している：scheme と host 以外は
// すべて捨てる。URL のどの部分が認証情報かを「汎用的かつ安全に」判断する方法はないからだ：
//
//	DingTalk 認証情報は query      /robot/send?access_token=xxx
//	WeCom    認証情報は query      /cgi-bin/webhook/send?key=xxx
//	Feishu   認証情報は**パスの末尾** /open-apis/bot/v2/hook/<hook_id>
//	Telegram 認証情報は**パスの途中** /bot<token>/sendMessage
//
// 「役に立つ部分だけ残す」にはチャネルごとに手当てが要り、どこか1社でも漏らせばそれが認証情報の漏洩になる。
// host を残せば調査には十分で（DNS で解決できない、つながらない、証明書が違う、のいずれも突き止められる）、
// どのボットかはチャネル設定のマスクされた末尾の文字で見分ける。
//
// 解析に失敗したら固定のプレースホルダーを返す——元の文字列は決して返さない。
func redactRequestTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(주소를 해석할 수 없음)"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

// redactTransportError は伝送層のエラーからアドレスを取り除き、下層の原因だけを残す。
//
// *url.Error の構造は {Op, URL, Err} で、Error() は URL も一緒に出力する。
// ここでは明示的に Err フィールドを取り、その Error() を避ける——あとから文字列を置換するより確実だ。
// 置換では URL エンコードやエスケープのさまざまな変形に正しく対応する必要があり、漏れやすいからだ。
func redactTransportError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		host := ""
		if u, parseErr := url.Parse(uerr.URL); parseErr == nil {
			host = u.Host
		}
		if uerr.Err != nil {
			return fmt.Sprintf("%s %s: %s", uerr.Op, host, uerr.Err)
		}
		return fmt.Sprintf("%s %s: 알 수 없는 오류", uerr.Op, host)
	}
	// *url.Error でないもの（リダイレクトのポリシーが返すエラーなど）もアドレスを含みうるので、まとめて秘匿化する。
	return redactURLsInText(err.Error())
}

// redactURLsInText は文章の中に現れる http(s) のアドレスを秘匿化した形に置き換える。
//
// 構造化されたフィールドを取れないエラー（リダイレクトのポリシーのエラー、外部ライブラリの独自エラー）の最後の受け皿に使う。
// http/https の接頭辞だけを認識し、空白と引用符で区切る——アドレスにこの2種類の文字は含まれない。
func redactURLsInText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		rest := s[i:]
		if strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
			end := len(rest)
			if j := strings.IndexAny(rest, " \t\n\"'"); j >= 0 {
				end = j
			}
			b.WriteString(redactRequestTarget(rest[:end]))
			i += end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
