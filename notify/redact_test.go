package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// このファイルは不変条件のテスト：チャネルの実装から出てくる**どの**エラー文にも認証情報を含めてはならない。
//
// 別のファイルにした理由：最初のチャネルのテストケースは成功の経路とプラットフォームの業務エラーしか扱っておらず、
// 伝送層の失敗はまったく見ていなかった。しかし伝送層のエラー（接続拒否/DNS の失敗/タイムアウト）こそ最も危険だ——
// http.Client.Do が返す *url.Error は**完全な URL** をエラー文に書き込み、この機能の
// 各社の認証情報は URL の中にある。認証情報はこの文字列に乗って4つの出口に流れていた：
//
//	notification_deliveries.last_error  → 平文で DB に記録
//	GET /api/notify/deliveries のレスポンス → チャネル設定のマスクをすり抜けてブラウザに返される
//	サーバーのログ                      → 外部に送られて保管されることが多い
//	テスト送信の API の 502 レスポンス  → そのままフロントエンドに表示される
//
// そのためここでは関数を1つテストするだけでなく、チャネルごとに必ず失敗するリクエストを実際に1回送り、エラー文の中に
// その認証情報が見つからないことを確かめる。

// credentialCases は「認証情報が URL の中にある」すべてのチャネルの形を扱う：
// DingTalk/WeCom は query、Feishu はパスの末尾、Telegram はパスの途中。
var credentialCases = []struct {
	name   string
	ch     Channel
	cfg    map[string]any
	secret string
}{
	{
		name:   "DingTalk の access_token が query にある",
		ch:     dingTalkChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/robot/send?access_token=" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "WeCom の key が query にある",
		ch:     weComChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/cgi-bin/webhook/send?key=" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "Feishu の hook id がパスの末尾にある",
		ch:     feishuChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/open-apis/bot/v2/hook/" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "Telegram の bot token がパスの途中にある",
		ch:     telegramChannel{},
		cfg:    map[string]any{"bot_token": leakProbeToken, "chat_id": "1", "base_url": "http://127.0.0.1:1"},
		secret: leakProbeToken,
	},
	{
		name:   "DingTalk の署名の鍵",
		ch:     dingTalkChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/robot/send", "secret": leakProbeToken},
		secret: leakProbeToken,
	},
}

// leakProbeToken は本物の認証情報ではありえない目印の値で、エラー文の中で探すのに使う。
const leakProbeToken = "LEAKPROBE0123456789abcdef"

// TestChannelErrorsNeverLeakCredentials は中心となる不変条件。
func TestChannelErrorsNeverLeakCredentials(t *testing.T) {
	for _, tc := range credentialCases {
		t.Run(tc.name, func(t *testing.T) {
			// 必ず失敗する相手：127.0.0.1:1 は誰も待ち受けておらず、接続拒否の経路を通る。
			_, err := tc.ch.Send(context.Background(), tc.cfg, Message{
				Items: []Item{{FindingID: 1, Severity: "high", Name: "漏洩の探針"}},
			})
			if err == nil {
				t.Fatal("到達できないアドレスにはエラーにすべき")
			}
			assertNoSecret(t, err.Error(), tc.secret)
		})
	}
}

// TestChannelErrorsNeverLeakCredentialsInPermanentPath は恒久的な失敗の分岐を扱う：
// URL の検証失敗やプラットフォームの業務エラーなどもエラー文を外に伝えるので、同じく認証情報を含めてはならない。
func TestChannelErrorsNeverLeakCredentialsInPermanentPath(t *testing.T) {
	cases := []struct {
		name string
		ch   Channel
		cfg  map[string]any
	}{
		// アドレスに認証情報が入っているが形式が不正 → validateHTTPURL / url.Parse の分岐を通る。
		{"DingTalk のアドレスが不正", dingTalkChannel{}, map[string]any{"webhook": "file:///" + leakProbeToken}},
		{"WeCom のアドレスが不正", weComChannel{}, map[string]any{"webhook": "gopher://" + leakProbeToken}},
		{"Feishu のアドレスが不正", feishuChannel{}, map[string]any{"webhook": "ftp://" + leakProbeToken + "/hook"}},
		{"Telegram の API アドレスが不正", telegramChannel{}, map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": "file://" + leakProbeToken}},
		{"汎用 Webhook のアドレスが不正", webhookChannel{}, map[string]any{"url": "javascript:" + leakProbeToken}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.ch.Send(context.Background(), tc.cfg, Message{Items: []Item{{Severity: "high"}}})
			if err == nil {
				t.Fatal("不正な設定はエラーにすべき")
			}
			assertNoSecret(t, err.Error(), leakProbeToken)
		})
	}
}

func assertNoSecret(t *testing.T, text, secret string) {
	t.Helper()
	if strings.Contains(text, secret) {
		t.Fatalf("エラー文が認証情報 %q を漏らした:\n    %s", secret, text)
	}
}

func TestRedactRequestTargetKeepsOnlySchemeAndHost(t *testing.T) {
	cases := map[string]string{
		"https://oapi.dingtalk.com/robot/send?access_token=S1":    "https://oapi.dingtalk.com/…",
		"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=S2": "https://qyapi.weixin.qq.com/…",
		"https://open.feishu.cn/open-apis/bot/v2/hook/S3":         "https://open.feishu.cn/…",
		"https://api.telegram.org/botS4/sendMessage":              "https://api.telegram.org/…",
		"http://10.0.0.5:8080/hook":                               "http://10.0.0.5:8080/…",
	}
	for in, want := range cases {
		got := redactRequestTarget(in)
		if got != want {
			t.Errorf("redactRequestTarget(%q) = %q、期待値 %q", in, got, want)
		}
		// 秘匿化した結果に、元のアドレスのパス/クエリの断片が残っていてはならない。
		if parts := strings.SplitN(in, "://", 2); len(parts) == 2 {
			if hostAndRest := strings.SplitN(parts[1], "/", 2); len(hostAndRest) == 2 && hostAndRest[1] != "" {
				if strings.Contains(got, hostAndRest[1]) {
					t.Errorf("秘匿化したあともパス/クエリの断片 %q が残っている: %q", hostAndRest[1], got)
				}
			}
		}
	}
	// 解析できない入力は決して元の文字列を返さない。
	for _, bad := range []string{"", "://", "not a url", "http://"} {
		if got := redactRequestTarget(bad); strings.Contains(got, bad) && bad != "" {
			t.Errorf("解析できない入力 %q が %q として返された", bad, got)
		}
	}
}

// TestRedactTransportErrorStripsURL は *url.Error という具体的な型を直接見張る：
// これは http.Client.Do の戻り値の型であり、漏洩が最初に起きる現場でもある。
func TestRedactTransportErrorStripsURL(t *testing.T) {
	inner := errors.New("dial tcp 127.0.0.1:1: connect: connection refused")
	uerr := &url.Error{
		Op:  "Post",
		URL: "https://api.telegram.org/bot" + leakProbeToken + "/sendMessage",
		Err: inner,
	}
	got := redactTransportError(uerr)
	assertNoSecret(t, got, leakProbeToken)
	if !strings.Contains(got, "api.telegram.org") {
		t.Errorf("調査できるよう host は残すべきだが、%q を得た", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("調査できるよう下層の原因は残すべきだが、%q を得た", got)
	}
	// Op も残す（POST か GET かは調査に意味がある）。
	if !strings.Contains(got, "Post") {
		t.Errorf("操作名は残すべきだが、%q を得た", got)
	}
}

// TestRedactURLsInTextHandlesFallback は最後の受け皿の経路：*url.Error でない独自のエラー
// （リダイレクトのポリシーが返すエラーなど）の中のアドレスも取り除くこと。
func TestRedactURLsInTextHandlesFallback(t *testing.T) {
	in := fmt.Sprintf("ホストをまたぐリダイレクトを拒否（a.example → http://b.example/bot%s/send）", leakProbeToken)
	got := redactURLsInText(in)
	assertNoSecret(t, got, leakProbeToken)
	if !strings.Contains(got, "http://b.example/…") {
		t.Errorf("アドレスは秘匿化した形に置き換えるべきだが、%q を得た", got)
	}
	// アドレスを含まない文はそのまま残す。
	if plain := "dial tcp: connection refused"; redactURLsInText(plain) != plain {
		t.Error("アドレスを含まない文は変えるべきでない")
	}
}

// TestCrossHostRedirectRefused は「認証情報が URL の中にある + ホストをまたぐ移動に従う = 認証情報を渡す」を扱う。
// httptest の2つのサーバーは 127.0.0.1 の別のポートで待ち受けており、ポートが違えば Host も違うので、
// ちょうどホストをまたぐ移動になる。
func TestCrossHostRedirectRefused(t *testing.T) {
	var hit bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/robot/send?access_token="+leakProbeToken, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": redirector.URL + "/robot/send?access_token=" + leakProbeToken},
		Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("ホストをまたぐリダイレクトは拒否すべき")
	}
	if hit {
		t.Fatal("移動先にアクセスされた——認証情報がリダイレクトとともに外へ漏れた")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestSameHostRedirectAllowed は逆のテストケース：同じホスト内の移動（末尾にスラッシュを補うなど）は引き続き使えなければならない。
// さもないと通常の作業の流れまで止めてしまう。
func TestSameHostRedirectAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robot/send" {
			// 同じホスト、同じポートの移動。
			http.Redirect(w, r, "/robot/send/", http.StatusTemporaryRedirect)
			return
		}
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"},
		Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("同じホストのリダイレクトは拒否すべきでない: %v", err)
	}
}
