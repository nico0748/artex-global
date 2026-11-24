package notify

import (
	"net/url"
	"testing"
	"time"
)

// 署名の基準値は OpenSSL で独立に計算したもので、このパッケージ自身の実装で生成したものではない——
// さもないと「コードが変わっていない」ことしか証明できず、「アルゴリズムが正しい」ことは証明できない。
//
//	TS=1700000000000, SECRET=SECtest123
//	DingTalk: printf '%s\n%s' "$TS" "$SECRET" | openssl dgst -sha256 -hmac "$SECRET" -binary | openssl base64 -A
//	      -> w3RMHXzixTMdzr8OHJUmVLS4IoPJVdu+Ut1LE48MePE=
//	Feishu: printf '' | openssl dgst -sha256 -hmac "$(printf '%s\n%s' "$TS" "$SECRET")" -binary | openssl base64 -A
//	      -> Hd4xFWQU6R6ad4nzy4ETIznzlqebqH7xcTFVmONTudo=
const (
	signTestTSMillis = int64(1700000000000)
	signTestSecret   = "SECtest123"
	dingTalkExpected = "w3RMHXzixTMdzr8OHJUmVLS4IoPJVdu+Ut1LE48MePE="
	feishuExpected   = "Hd4xFWQU6R6ad4nzy4ETIznzlqebqH7xcTFVmONTudo="
)

func TestDingTalkSignMatchesReference(t *testing.T) {
	got, err := dingTalkSignedURL("https://oapi.dingtalk.com/robot/send?access_token=tok", signTestSecret, time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatalf("署名に失敗: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("生成したアドレスを解析できない: %v", err)
	}
	q := u.Query()
	if q.Get("sign") != dingTalkExpected {
		t.Errorf("署名が一致しない\n期待値 %s\n得たもの %s", dingTalkExpected, q.Get("sign"))
	}
	if q.Get("timestamp") != "1700000000000" {
		t.Errorf("タイムスタンプはミリ秒でそのまま付けるべきだが、%q を得た", q.Get("timestamp"))
	}
	// 元の query パラメータ（access_token）が署名で上書きされてはならない。
	if q.Get("access_token") != "tok" {
		t.Errorf("元の query パラメータが失われた。得たもの %q", q.Get("access_token"))
	}
}

func TestFeishuSignMatchesReference(t *testing.T) {
	got := feishuSign("1700000000000", signTestSecret)
	if got != feishuExpected {
		t.Errorf("署名が一致しない\n期待値 %s\n得たもの %s", feishuExpected, got)
	}
}

// TestSignAlgorithmsDiffer は2社のアルゴリズムの違いを固定する。両者はちょうど互いの引数の順序が逆で
// （DingTalk は key=secret、Feishu は key=署名対象の文字列）、もう一方をまねると必ず検証に失敗する。
// このテストケースは、将来のリファクタリングで両者が同じ関数にまとめられないようにする。
func TestSignAlgorithmsDiffer(t *testing.T) {
	ts := "1700000000000"
	dingURL, err := dingTalkSignedURL("https://example.com/hook", signTestSecret, time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatal(err)
	}
	dq, _ := url.Parse(dingURL)
	if dq.Query().Get("sign") == feishuSign(ts, signTestSecret) {
		t.Fatal("DingTalk と Feishu の署名が同じ。どちらかのアルゴリズムの実装が誤っている")
	}
}

func TestDingTalkNoSecretLeavesURLUntouched(t *testing.T) {
	// 署名を有効にしていないボット：timestamp/sign のパラメータを勝手に足してはならない。
	const hook = "https://oapi.dingtalk.com/robot/send?access_token=tok"
	got, err := dingTalkSignedURL(hook, "", time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatal(err)
	}
	if got != hook {
		t.Fatalf("secret を設定していないときはアドレスを変えるべきでないが、%q を得た", got)
	}
}

func TestValidateHTTPURL(t *testing.T) {
	ok := []string{"https://example.com/hook", "http://10.0.0.1:8080/x?y=1"}
	for _, s := range ok {
		if err := validateHTTPURL(s); err != nil {
			t.Errorf("%q は受け付けるべき: %v", s, err)
		}
	}
	// file:// などは通すべきでない——http.Client によるそれらの扱いは想定の範囲を超える。
	bad := []string{"", "file:///etc/passwd", "ftp://example.com", "https://", "gopher://x"}
	for _, s := range bad {
		if err := validateHTTPURL(s); err == nil {
			t.Errorf("%q は拒否すべき", s)
		}
	}
}
