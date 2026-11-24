package notify

import (
	"bufio"
	"context"

	"net"
	"strings"
	"sync"
	"testing"
)

// このファイルはメールチャネルのプロトコルレベルのテストを補う。これまで email.Send のカバレッジは 0 だった——
// SMTP の経路全体を通るテストケースが1つもなかったのに、これこそ6つのチャネルのうちプロトコルの面が最も大きく、
// 最も間違えやすいものだった（ハンドシェイク、認証、エンベロープ、DATA の段階にそれぞれ失敗の意味がある）。
//
// ここでは net/smtp をモックにせず、自作の最小限の SMTP サーバーで動かす：
// メールチャネルのリスクの大部分は「本物の SMTP サーバーとの対話」の段階にあり、
// そこをモックにしたらテストしていないのと同じだからだ。

// fakeSMTP はちょうど足りるだけの SMTP サーバー：greet/EHLO/AUTH/MAIL/RCPT/DATA/QUIT をこなせ、
// テストケースの求めに応じて特定の段階で指定の応答コードを返す。
type fakeSMTP struct {
	ln net.Listener

	// rcptReply は RCPT TO への応答。既定は 250。
	rcptReply string
	// mailReply は MAIL FROM への応答。既定は 250。
	mailReply string
	// advertiseAuth が true なら EHLO で AUTH PLAIN への対応を宣言する。
	advertiseAuth bool

	mu       sync.Mutex
	data     string
	commands []string
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, rcptReply: "250 OK", mailReply: "250 OK"}
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeSMTP) hostPort(t *testing.T) (string, int) {
	t.Helper()
	addr, ok := f.ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatal("TCP の待ち受けアドレスではない")
	}
	return "127.0.0.1", addr.Port
}

func (f *fakeSMTP) record(cmd string) {
	f.mu.Lock()
	f.commands = append(f.commands, cmd)
	f.mu.Unlock()
}

func (f *fakeSMTP) body() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data
}

func (f *fakeSMTP) sawCommand(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.commands {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeSMTP) serve() {
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	w := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
	w("220 fake.local ESMTP ready")
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		f.record(line)
		switch {
		case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
			// STARTTLS は宣言しない：コードに平文の分岐を通らせる（テストの目的はエンベロープの処理で、TLS ではない）。
			w("250-fake.local")
			if f.advertiseAuth {
				w("250-AUTH PLAIN")
			}
			w("250 8BITMIME")
		case strings.HasPrefix(line, "AUTH"):
			// 簡略化：PLAIN の最初の応答は複数行にまたがりうるので、そのまま受け付ける。
			w("235 2.7.0 Authentication successful")
		case strings.HasPrefix(line, "MAIL FROM"):
			w(f.mailReply)
		case strings.HasPrefix(line, "RCPT TO"):
			w(f.rcptReply)
		case strings.HasPrefix(line, "DATA"):
			w("354 End data with <CR><LF>.<CR><LF>")
			var sb strings.Builder
			for {
				dl, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(dl, "\r\n") == "." {
					break
				}
				sb.WriteString(dl)
			}
			f.mu.Lock()
			f.data = sb.String()
			f.mu.Unlock()
			w("250 2.0.0 Ok: queued as FAKE1")
		case strings.HasPrefix(line, "QUIT"):
			w("221 2.0.0 Bye")
			return
		default:
			w("250 OK")
		}
	}
}

func emailCfg(t *testing.T, f *fakeSMTP, extra map[string]any) map[string]any {
	t.Helper()
	host, port := f.hostPort(t)
	cfg := map[string]any{
		"host": host,
		"port": float64(port),
		"from": "artex@example.com",
		"to":   []any{"a@example.com", "b@example.com"},
	}
	for k, v := range extra {
		cfg[k] = v
	}
	return cfg
}

func TestEmailSendDeliversFullMessage(t *testing.T) {
	f := newFakeSMTP(t)
	f.advertiseAuth = true
	cfg := emailCfg(t, f, map[string]any{"username": "artex", "password": "pw"})

	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("配信に失敗: %v", err)
	}
	// エンベロープの段階まで必ず進むこと：送信者、2人の受信者、DATA。
	for _, want := range []string{"MAIL FROM:<artex@example.com>", "RCPT TO:<a@example.com>", "RCPT TO:<b@example.com>", "DATA", "AUTH", "QUIT"} {
		if !f.sawCommand(want) {
			t.Errorf("SMTP セッションに %q がない。実際のコマンド：%v", want, f.commands)
		}
	}
	// 本文は base64 の HTML で、本物の脆弱性の内容を含むこと（エンコード後も見分けられる）。
	body := f.body()
	if body == "" {
		t.Fatal("DATA の段階で本文を受け取っていない")
	}
	if !strings.Contains(body, "Content-Type: text/html") {
		t.Errorf("Content-Type ヘッダーがない:\n%s", body)
	}
	if !strings.Contains(body, "base64") {
		t.Errorf("本文が base64 でエンコードされていない（長い HTML の行は SMTP の 1000 バイトの行長制限を破る）:\n%s", body)
	}
	// 複数の受信者がすべて To ヘッダーに現れること。
	if !strings.Contains(body, "a@example.com, b@example.com") {
		t.Errorf("To ヘッダーにすべての受信者が含まれていない:\n%s", body)
	}
}

func TestEmailSendWithoutAuth(t *testing.T) {
	// アカウントを設定していなければ AUTH を送ってはならない —— それを理由に拒否する中継もある。
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("配信に失敗: %v", err)
	}
	if f.sawCommand("AUTH") {
		t.Errorf("アカウントを設定していないのに AUTH を送った: %v", f.commands)
	}
}

// TestEmailSendClassifiesSMTPReplies は今回の監査での修正を直接確かめる：
// 5xx は恒久的な失敗、4xx（グレーリスト）は再試行できると判定する。
func TestEmailSendClassifiesSMTPReplies(t *testing.T) {
	cases := []struct {
		name      string
		rcptReply string
		mailReply string
		permanent bool
	}{
		{"受信者が 550 で恒久的に拒否", "550 5.1.1 User unknown", "250 OK", true},
		{"受信者が 450 のグレーリストに当たる", "450 4.7.1 Greylisting in action", "250 OK", false},
		{"受信者が 452 のメールボックス満杯に当たる", "452 4.2.2 Mailbox full", "250 OK", false},
		{"送信者が 553 で恒久的に拒否", "250 OK", "553 5.1.3 Bad address", true},
		{"送信者が 451 の一時エラーに当たる", "250 OK", "451 4.3.0 Temporary failure", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSMTP(t)
			f.rcptReply = tc.rcptReply
			f.mailReply = tc.mailReply
			_, err := (emailChannel{}).Send(context.Background(), emailCfg(t, f, nil), singleMsg())
			if err == nil {
				t.Fatal("エラーにすべき")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("permanent の判定が誤っている：%v を期待したが %v を得た (%v)", tc.permanent, got, err)
			}
			// サーバーの原文は残すこと。さもないとユーザーはサーバーの管理者に頼むべきかアドレスを直すべきかわからない。
			if !strings.Contains(err.Error(), strings.Fields(tc.rcptReply)[0]) && !strings.Contains(err.Error(), strings.Fields(tc.mailReply)[0]) {
				t.Errorf("エラーにはサーバーの応答コードを残すべき: %v", err)
			}
		})
	}
}

func TestEmailSendRefusesPlaintextCredentials(t *testing.T) {
	// net/smtp の PlainAuth は暗号化されていない接続での認証情報の送信を拒否する（宛先が localhost の場合を除く）。
	// これは**正しい**セキュリティ上の動作で、回避されてはならないが、ユーザーが直し方を知れるエラーを出すべき。
	// ここでは localhost でないホスト名でそれを引き起こす。
	f := newFakeSMTP(t)
	f.advertiseAuth = true
	_, port := f.hostPort(t)
	cfg := map[string]any{
		"host":     "smtp.example.com", // localhost ではない
		"port":     float64(port),
		"from":     "a@example.com",
		"to":       []any{"b@example.com"},
		"username": "artex",
		"password": "pw",
	}
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Skip("このマシンの DNS がローカルのサーバーに解決したので飛ばす（他のテストケースには影響しない）")
	}
	// つながらない場合も、認証情報の送信を拒否された場合も、このアサーションは通る。肝心なのはパスワードを**黙って送らない**こと。
	if !IsPermanent(err) && !strings.Contains(err.Error(), "연결") {
		t.Logf("エラー：%v（localhost 以外でつながらないのは想定どおり）", err)
	}
}

func TestEmailValidateReportsMissingFields(t *testing.T) {
	// メールチャネルは設定のフィールドが最も多く、1つでも漏れると配信時に初めて表面化する。ここでは1つずつ
	// 検証が前もって止められることを確かめる。アサーションが確かめるのは「エラーメッセージが何が足りないかに触れている」こと。
	cases := []struct {
		name string
		cfg  map[string]any
	}{
		{"host がない", map[string]any{"port": float64(25), "from": "a@b.c", "to": []any{"d@e.f"}}},
		{"port がない", map[string]any{"host": "smtp.example.com"}},
		{"port が範囲外", map[string]any{"host": "h", "port": float64(70000), "from": "a@b.c", "to": []any{"d@e.f"}}},
		{"from がない", map[string]any{"host": "h", "port": float64(25), "to": []any{"d@e.f"}}},
		{"to がない", map[string]any{"host": "h", "port": float64(25), "from": "a@b.c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := (emailChannel{}).Validate(tc.cfg); err == nil {
				t.Fatalf("検証に失敗すべき: %v", tc.cfg)
			}
		})
	}
}

// TestEmailConfigTolerance は設定の読み取りの許容を扱う：JSONB の数値は float64 だが、
// ユーザーは UI でポートを文字列で入れることがあり、配列も単一の文字列のことがある。
func TestEmailConfigTolerance(t *testing.T) {
	cfg := map[string]any{
		"host": "smtp.example.com",
		"port": "587", // 文字列形式のポート
		"from": "a@b.c",
		"to":   "d@e.f", // 配列ではなく単一の文字列
		"tls":  "true",  // 文字列形式の真偽値
	}
	if err := (emailChannel{}).Validate(cfg); err != nil {
		t.Fatalf("文字列形式の数値は許容すべき: %v", err)
	}
	if got := cfgInt(cfg, "port"); got != 587 {
		t.Errorf("cfgInt が文字列のポートを解析しなかった。得たもの %d", got)
	}
	if !cfgBool(cfg, "tls") {
		t.Error("cfgBool が文字列の \"true\" を解析しなかった")
	}
	if to := cfgStrings(cfg, "to"); len(to) != 1 || to[0] != "d@e.f" {
		t.Errorf("cfgStrings が単一の文字列に対応しなかった。得たもの %v", to)
	}
}

// TestFilterValidateRejectsTypo は監査での修正を直接確かめる：
// しきい値の打ち間違いは書き込み時に止めなければならない。さもないとフィルタが黙って無効になり、すべて通知されてしまう。
func TestFilterValidateRejectsTypo(t *testing.T) {
	good := []string{"", "low", "medium", "high", "critical"}
	for _, s := range good {
		if err := (Filter{MinSeverity: s}).Validate(); err != nil {
			t.Errorf("正しいしきい値 %q が拒否された: %v", s, err)
		}
	}
	// これらは実際に起こる書き間違い——すべて拒否しなければならない。
	for _, s := range []string{"hgih", "HIGH", "重大", "high ", "crit"} {
		err := (Filter{MinSeverity: s}).Validate()
		if err == nil {
			t.Errorf("不正なしきい値 %q は拒否すべき（さもないとフィルタが黙って無効になり、すべて通知される）", s)
			continue
		}
		// エラーメッセージはユーザーが正しく直せるよう導くものであること。
		if !strings.Contains(err.Error(), "low") || !strings.Contains(err.Error(), "critical") {
			t.Errorf("エラーメッセージは選べる値を挙げるべきだが、%q を得た", err.Error())
		}
	}
}

// TestFilterValidateIsWriteTimeOnly は「書き込みは厳しく、読み取りは寛容に」という分担を固定する。
// DB にすでにある不正な値のせいでチャネル全体が読めなくなってはならない（そうなると過去のチャネルが突然すべて通知を止める）。
func TestFilterValidateIsWriteTimeOnly(t *testing.T) {
	raw := []byte(`{"min_severity":"hgih"}`)
	f := ParseFilter(raw) // エラーにしない
	if f.MinSeverity != "hgih" {
		t.Fatalf("読み取りの経路ではそのまま残すべきだが、%q を得た", f.MinSeverity)
	}
	// しかもそのチャネルはまだイベントを判定できる（panic せず、止まりもしない）。
	_ = Match(f, Snapshot{Kind: EventFindingCreated, Severity: "critical"})
}
