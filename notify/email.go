package notify

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// emailDialTimeout / emailSessionTimeout はそれぞれ接続の確立と SMTP セッション全体を制限する。
// net/smtp 自身にはタイムアウトの仕組みがまったくないので、この2つを設定しないと、止まった相手のせいで
// 配信の goroutine が永久にそこで止まる——しかも dispatcher は1つの goroutine で順に処理するので、
// 通知システム全体が止まるのと同じことになる。
const (
	emailDialTimeout    = 10 * time.Second
	emailSessionTimeout = 45 * time.Second
)

// emailChannel は SMTP によるメール配信を実装する。
type emailChannel struct{}

func (emailChannel) Kind() string { return KindEmail }

// メールにはプラットフォームのレート制限がないが、大量に送りつけるべきでもないので、緩めの既定値を与える。
func (emailChannel) DefaultRatePerMin() int { return 60 }

// マスクするのはパスワードだけ。SMTP ホスト、アカウント、受信者は秘密ではなく、マスクしても編集が面倒になるだけ。
func (emailChannel) SecretKeys() []string { return []string{"password"} }

// host/port はパスワードをどのサーバーに渡すかを決め、tls は通信を暗号化するかを決める。どれか1つでも変われば
// パスワードを改めて指定する必要がある——ついでに「TLS を切る」操作には認証情報を明示的に添えることを求め、気軽に変えられないようにしている。
func (emailChannel) DestinationKeys() []string { return []string{"host", "port", "tls"} }

func (emailChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "host") == "" {
		return errors.New("SMTP 서버 주소가 없습니다")
	}
	port := cfgInt(cfg, "port")
	if port <= 0 || port > 65535 {
		return errors.New("SMTP 포트가 올바르지 않습니다 (1-65535 범위여야 합니다)")
	}
	if cfgString(cfg, "from") == "" {
		return errors.New("발신자 주소가 없습니다")
	}
	if len(cfgStrings(cfg, "to")) == 0 {
		return errors.New("수신자 주소가 최소 하나 필요합니다")
	}
	return nil
}

func (c emailChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	host := cfgString(cfg, "host")
	port := cfgInt(cfg, "port")
	from := cfgString(cfg, "from")
	to := cfgStrings(cfg, "to")
	username := cfgString(cfg, "username")
	password := cfgString(cfg, "password")
	implicitTLS := cfgBool(cfg, "tls")

	msg, err := buildEmailMessage(from, to, m)
	if err != nil {
		return 0, Permanent(err)
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	client, err := emailDial(ctx, addr, host, implicitTLS)
	if err != nil {
		return 0, err
	}
	defer client.Close()

	// STARTTLS：相手が対応していれば昇格する。平文のセッションでは認証情報を送れない（下の auth の説明を参照）。
	if !implicitTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
				return 0, fmt.Errorf("STARTTLS 실패: %w", err)
			}
		}
	}
	if username != "" {
		if err := client.Auth(smtp.PlainAuth("", username, password, host)); err != nil {
			// smtp.PlainAuth は暗号化されていない接続での認証情報の送信を拒否する（宛先が localhost の場合を除く）。
			// これは**正しい**セキュリティ上の動作で、回避してはならないが、理由をきちんと言い換える必要がある——
			// さもないと利用者には「unencrypted connection」しか見えず、どうすればよいかわからない。
			if strings.Contains(err.Error(), "unencrypted connection") {
				return 0, Permanent(fmt.Errorf("자격 증명 전송을 거부했습니다: 연결이 암호화되지 않았습니다. TLS 를 켜거나, 465 포트(암시적 TLS)를 사용하거나, 「TLS 사용」을 체크하세요 (%w)", err))
			}
			return 0, Permanent(fmt.Errorf("SMTP 인증 실패: %w", err))
		}
	}
	if err := client.Mail(from); err != nil {
		return 0, smtpStageError(fmt.Sprintf("발신자 %s 주소가 거부되었습니다", from), err)
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return 0, smtpStageError(fmt.Sprintf("수신자 %s 주소가 거부되었습니다", rcpt), err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return 0, fmt.Errorf("SMTP DATA 실패: %w", err)
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return 0, fmt.Errorf("메일 본문 쓰기 실패: %w", err)
	}
	if err := w.Close(); err != nil {
		return 0, fmt.Errorf("메일 제출 실패: %w", err)
	}
	// Quit の失敗は「メールがサーバーに受け付けられた」という事実に影響しないので、そのエラーは無視する。
	_ = client.Quit()
	// メールには長さの切り詰めがない（HTML の本文をすべて送る）ので、一括分すべてを配信済みとする。
	return len(m.Items), nil
}

// emailDial は SMTP の接続を確立する。
//
// implicitTLS=true は 465 のような「つないだらすぐ TLS」の方式、false は 25/587 で平文で接続してから
// STARTTLS する方式。両者は混ぜられない：465 番ポートに平文の greeting を送るとすぐに切断される。
//
// セッションの期限は（あとから設定するのでなく）**接続の確立時に**設定する。net/smtp の Client は下層の
// 接続を非公開のフィールドに隠していて外から取れず、接続をいったん渡したら、あらかじめ設定した deadline に
// 頼るしかないからだ。これでハンドシェイクの段階での停止もついでに防げる。
// Control に blockInternalDial を付け、HTTP 系のチャネルと同じ防御を共有する。付けないと SMTP は
// SSRF 対策全体の穴になる：host に 169.254.169.254 や 127.0.0.1 を入れれば直接つながり、
// smtp.NewClient のハンドシェイクが失敗すると相手が返した1行がエラーに包まれ、last_error を経て
// 配信履歴の API で返されるので、半ば見えない読み取りの手段になる。「接続拒否か、タイムアウトか」の所要時間の差も
// ポートの探査に使える。接続の段階が実際に効く場所で、DNS リバインディングにも対応する。
func emailDial(ctx context.Context, addr, host string, implicitTLS bool) (*smtp.Client, error) {
	d := &net.Dialer{Timeout: emailDialTimeout, Control: blockInternalDial}
	var conn net.Conn
	var err error
	if implicitTLS {
		conn, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("SMTP 서버 연결 실패: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(emailSessionTimeout))
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SMTP 핸드셰이크 실패: %w", err)
	}
	return client, nil
}

// smtpStageError は SMTP の応答コードに応じて、ある段階の失敗を「再試行できる」と「恒久的な失敗」に分ける。
//
// 区別が必要な理由：SMTP の 4xx と 5xx は意味がまったく違う——
//   - 4xx（450 グレーリスト、451 ローカルエラー、452 容量不足）は**一時的な**拒否で、
//     正しくはしばらくしてから再試行する。特にグレーリストは、初回の配信ではほぼ毎回出会う。
//   - 5xx（550 ユーザーが存在しない、553 アドレスが不正）は恒久的な拒否で、再試行しても意味がない。
//
// 一律に恒久的な失敗とすると、グレーリストを有効にしたメールサーバーでは、**どの**通知も最初の
// 試行で failed になる——こうした失敗こそ自動再試行が最も役立つ場面なのに。
// 応答コードはエラー文の先頭3桁の数字を取る。コードが取れないときは再試行できるとみなす（解析できなかったからといって
// 一時的かもしれない故障を見限るより、もう一度試すほうがよい）。
func smtpStageError(what string, err error) error {
	code := smtpReplyCode(err.Error())
	if code >= 500 && code < 600 {
		return Permanent(fmt.Errorf("%s: %w", what, err))
	}
	return fmt.Errorf("%s: %w", what, err)
}

// smtpReplyCode は SMTP のエラー文から先頭の3桁の応答コードを取り出し、取れなければ 0 を返す。
// net/smtp はエラーコードのフィールドを公開していないので、文から取るしかない。形式は「450 4.7.1 ...」。
func smtpReplyCode(text string) int {
	if len(text) < 3 {
		return 0
	}
	n, err := strconv.Atoi(text[:3])
	if err != nil {
		return 0
	}
	return n
}

// buildEmailMessage は完全な RFC 5322 のメールを組み立てる。
//
// 本文を base64 でエンコードする理由は2つある：1つは SMTP が1行を 1000 バイト以内と定めているのに、HTML の
// 本文（特にまとめのメール）は長すぎる行になりやすいこと。もう1つは base64 なら "." で始まる行が
// もともと出てこないので、SMTP のドットのエスケープの手間が省けること。
func buildEmailMessage(from string, to []string, m Message) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	// 日本語などの件名は RFC 2047 でエンコードしなければならない。さもないとクライアントで文字化けする。
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", htmlTitle(m)))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	// メールには長さの厳密な上限がないので、本文は切り詰めない。
	b.WriteString("\r\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(htmlBody(m, 0)))
	// base64 は RFC 2045 に従って 76 文字で折り返す。
	for len(encoded) > 76 {
		b.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded + "\r\n")
	return b.String(), nil
}
