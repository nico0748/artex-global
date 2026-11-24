package notify

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// このファイルは関連する2つの強化を扱う：
//   ① 配信先を使ってサーバーを足場に内部ネットワーク / クラウドのメタデータを攻撃させてはならない（SSRF）
//   ② アドレスの検証のエラーメッセージで、アドレスに含まれる認証情報を持ち出してはならない
//
// テスト環境について：このパッケージの多くのテストケースは 127.0.0.1 上の httptest の偽の受信側を使い、防御は既定でそれらを止める。
// そのため TestMain で AllowLocalTargetsEnv をまとめて有効にし、下の各 SSRF のテストケースでは
// 明示的にそれを消して、**既定で拒否する**動作を確かめる。

func TestMain(m *testing.M) {
	// 通常のテストケースがローカルの偽の受信側につなげるようにする。SSRF のテストケースは自分で一時的に消す。
	_ = os.Setenv(AllowLocalTargetsEnv, "1")
	os.Exit(m.Run())
}

// TestDialGuardRejectsLoopbackByDefault は SSRF 対策の中心となるアサーション：
// 既定の設定では、ループバックのアドレスへの配信は**接続層**で拒否されなければならない。
func TestDialGuardRejectsLoopbackByDefault(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "") // 抜け道を閉じる = 既定の動作
	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("既定ではループバックのアドレスへの配信を許すべきでない")
	}
	if hit {
		t.Fatal("リクエストがもうローカルのサービスに届いた——防御が効いていない")
	}
	// エラーメッセージは、ユーザーにどうすれば許可できるかを示せること（ローカルの SMTP 中継は正当な設定）。
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("拒否のメッセージは明示的に許可する方法を説明すべき: %v", err)
	}
}

// TestDialGuardAllowsLoopbackWhenOptedIn は逆のテストケース：明示的に有効にしたら必ず使えること。
// さもないとローカルの postfix / 社内の中継のような正当な構成が一律に使えなくなる。
func TestDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("明示的に許可したら配信できるべき: %v", err)
	}
}

func TestIsBlockedDialIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "::1",
		"169.254.169.254", // クラウドのメタデータのエンドポイント——この関数がある主な理由
		"169.254.1.1", "fe80::1",
		"0.0.0.0", "::",
		"224.0.0.1", "ff02::1",
		"::ffff:127.0.0.1", // IPv4 にマップされた形は戻してから判定しなければならない。さもないと抜け道になる
		"",
	}
	for _, s := range blocked {
		if !isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s は拒否すべき", s)
		}
	}
	// RFC1918 のプライベートネットワークは**わざと通す**：社内の自前の Mattermost / SMTP 中継はよくある正当な使い方だ。
	// このアサーションはその取捨を固定する——将来だれかが気軽にプライベートネットワークの判定を足したらここで失敗し、
	// （一群の構成を黙って使えなくするのではなく）意識的な決定を迫る。
	allowed := []string{"10.0.0.5", "172.16.3.4", "192.168.1.10", "8.8.8.8", "2606:4700::1111"}
	for _, s := range allowed {
		if isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s は通すべき（プライベートネットワークはよくある正当な配信先）", s)
		}
	}
}

// TestValidateHTTPURLRejectsLiteralPrivateTargets は設定段階での事前の知らせを扱う：
// IP リテラルは最初の配信が失敗するのを待たず、保存時に拒否されるべき。
func TestValidateHTTPURLRejectsLiteralPrivateTargets(t *testing.T) {
	t.Setenv(AllowLocalTargetsEnv, "")
	for _, raw := range []string{
		"http://127.0.0.1:8080/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:8080/hook",
	} {
		if err := validateHTTPURL(raw); err == nil {
			t.Errorf("%s は設定段階で拒否されるべき", raw)
		}
	}
	// 公開アドレスとプライベートアドレスは通常どおり通る（プライベートは接続段階に任せ、そこでは止めない）。
	for _, raw := range []string{"https://oapi.dingtalk.com/robot/send", "http://10.0.0.9/hook"} {
		if err := validateHTTPURL(raw); err != nil {
			t.Errorf("%s は検証を通るべき: %v", raw, err)
		}
	}
}

// TestValidateHTTPURLErrorNeverLeaksCredentials は、監査で私が前回見落としたと指摘された分岐。
//
// url.Parse が**失敗**すると *url.Error を返し、その Error() には元のアドレス全体が含まれる。前回私は
// http.Client.Do が返すエラーしか秘匿化しておらず、ここを漏らしていた。しかもそのとき足した「恒久的な失敗の経路」のテストケース
// （file://、gopher://、ftp://）は、実はどれも url.Parse で解析に成功し scheme の分岐を通っていた。
// だからすべて緑でもこの経路の安全は証明できず——見せかけの保証だった。
func TestValidateHTTPURLErrorNeverLeaksCredentials(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/%zz?access_token=" + leakProbeToken,         // 不正なパーセントエスケープ
		"https://a.example.com:port/x?access_token=" + leakProbeToken, // ポートが数字でない
		"http://[::1?access_token=" + leakProbeToken,                  // 括弧が対になっていない
	}
	for _, raw := range cases {
		// まずこの入力で url.Parse が**本当に**失敗することを確かめる。これをしないと、テストケースが
		// 気づかないうちに別の分岐を通るおそれがある（前回の見せかけの保証はこうして生まれた）。
		if _, err := url.Parse(raw); err == nil {
			t.Errorf("%q は解析に失敗するはずだった。さもないとこのテストケースは目的の分岐を扱えていない", raw)
			continue
		}
		err := validateHTTPURL(raw)
		if err == nil {
			t.Errorf("%q は検証に失敗すべき", raw)
			continue
		}
		assertNoSecret(t, err.Error(), leakProbeToken)
	}
	// チャネル層の包装もアドレスを持ち出していないことを確かめる。
	t.Setenv(AllowLocalTargetsEnv, "")
	err := (dingTalkChannel{}).Validate(map[string]any{"webhook": cases[0]})
	if err == nil {
		t.Fatal("不正なアドレスは検証に失敗すべき")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestEmailDialGuardRejectsLoopbackByDefault は SMTP チャネルの接続時の防御を扱う。
//
// メールチャネルは以前は素の net.Dialer を使っており、SSRF 対策全体で唯一の穴だった：host を
// 169.254.169.254 や 127.0.0.1 にすれば直接つながり、smtp.NewClient のハンドシェイクが失敗すると
// 相手が返した1行がエラーに包まれ、last_error を経て配信履歴の API で返される——まさに他のチャネルでは
// すでに塞いだ半ば見えない読み取りの手段だ。「接続拒否か、タイムアウトか」の所要時間の差もポートの探査に使える。
//
// このパッケージの TestMain は AllowLocalTargetsEnv を全体で有効にしている（多くのテストケースが 127.0.0.1 上の
// 偽の受信側を使う）ので、このテストケースは自分でそれを消さなければならない——さもないと防御があってもなくても通ってしまい、
// それこそがこの穴が当初どのテストでも見つからなかった理由だ。
func TestEmailDialGuardRejectsLoopbackByDefault(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "") // 抜け道を閉じる = 既定の動作
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Fatal("既定ではメールをループバックのアドレスに配信することを許すべきでない")
	}
	// 接続はそもそも確立されるべきでない：防御は Control フックで止め、EHLO は決して送られない。
	if f.sawCommand("EHLO") || f.sawCommand("HELO") {
		t.Fatal("SMTP セッションがもう確立された——防御が効いていない")
	}
	// エラーメッセージは、ユーザーにどうすれば許可できるかを示せること（ローカルの postfix 中継は正当な設定）。
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("拒否のメッセージは明示的に許可する方法を説明すべき: %v", err)
	}
}

// TestEmailDialGuardAllowsLoopbackWhenOptedIn は対になる逆のテストケース：明示的に有効にしたら
// 正常に配信できなければならない。社内の自前の SMTP / ローカルの中継はごく一般的な構成で、防御が一律に止めてはならない。
func TestEmailDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("明示的に許可したらローカルの SMTP に配信できるべき: %v", err)
	}
	if !f.sawCommand("EHLO") {
		t.Fatal("EHLO が見えない——セッションが本当には確立されていない")
	}
}
