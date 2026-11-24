package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// このファイルは doJSON の HTTP 層のエラーの段階分けを扱う。
//
// 別にテストする理由：各チャネルのアダプタはプラットフォーム自身の業務エラーコード（DingTalk の errcode、
// Feishu の code、Telegram の ok フィールド）しか見ず、**HTTP 層**の段階分けは doJSON がまとめて行う。
// 両者は独立した2つの防衛線だ。これがないと、503 を返す中継のゲートウェイが恒久的な失敗とみなされて
// すぐに再試行をやめてしまい、403 は再試行できるとみなされて無駄に3回待つことになる。

func replyServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDoJSONClassifiesHTTPStatus(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		permanent bool
	}{
		{"200 は成功でエラーではない", 200, false},
		{"429 のレート制限は再試行できる", 429, false},
		{"408 のリクエストタイムアウトは再試行できる", 408, false},
		{"500 のサーバーエラーは再試行できる", 500, false},
		{"502 のゲートウェイエラーは再試行できる", 502, false},
		{"503 のサービス利用不可は再試行できる", 503, false},
		{"400 の引数エラーは恒久的な失敗", 400, true},
		{"401 の認証失敗は恒久的な失敗", 401, true},
		{"403 のアクセス禁止は恒久的な失敗", 403, true},
		{"404 のアドレス不存在は恒久的な失敗", 404, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := replyServer(t, tc.status, `{"detail":"upstream says no"}`)
			_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
			if tc.status < 300 {
				if err != nil {
					t.Fatalf("2xx はエラーにすべきでない: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("2xx でなければエラーにすべき")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("HTTP %d の permanent の判定が誤っている：%v を期待したが %v を得た (%v)",
					tc.status, tc.permanent, got, err)
			}
			// ステータスコードは必ずエラーに出すこと。さもないとユーザーは自分の設定ミスか相手の故障かを判断できない。
			// Go の英語の StatusText ではなく数字を確かめる：このパッケージの文言は Go の英語ではなく
			// （プロジェクトの他の部分と同じく）各言語で書かれており、数字こそ言語に依存せず安定して確かめられる部分だからだ。
			if !strings.Contains(err.Error(), strconv.Itoa(tc.status)) {
				t.Errorf("エラーメッセージには HTTP ステータスコード %d を含めるべきだが、%v を得た", tc.status, err)
			}
		})
	}
}

// TestDoJSONIncludesResponseSnippet は snippet を扱う：相手が返したエラーの説明は持ち帰ること。
// さもないとユーザーには「失敗した」ことしかわからず、相手がなぜ拒否したのかわからない。
func TestDoJSONIncludesResponseSnippet(t *testing.T) {
	srv := replyServer(t, 400, `{"error":"invalid webhook token"}`)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("エラーにすべき")
	}
	if !strings.Contains(err.Error(), "invalid webhook token") {
		t.Errorf("エラーメッセージには相手の説明を持ち帰るべきだが、%v を得た", err)
	}
}

// TestDoJSONSnippetIsSingleLineAndBounded は snippet の形を制約する：
// 相手のレスポンスはそのまま last_error 列とフロントエンドの表に入るので、複数行や長すぎる内容はレイアウトとペイロードを壊す。
func TestDoJSONSnippetIsSingleLineAndBounded(t *testing.T) {
	// 改行、タブ、5000 文字の長すぎる内容を含むレスポンス。
	long := strings.Repeat("x", 5000)
	srv := replyServer(t, 500, "line1\nline2\r\n\tline3 "+long)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("エラーにすべき")
	}
	msg := err.Error()
	if strings.ContainsAny(msg, "\r\n\t") {
		t.Errorf("エラーメッセージは1行にまとめるべきだが、%q を得た", msg)
	}
	// snippet の上限は 200 文字 + 固定の接頭辞で、全体は元のレスポンスよりはるかに小さくなければならない。
	if len(msg) > 400 {
		t.Errorf("エラーメッセージが長すぎる（%d バイト）。snippet で切り詰めるべき: %q", len(msg), msg)
	}
}

// TestDoJSONRejectsOversizedResponse は読み込みに上限があることを確かめる：相手が異常に巨大な内容を返したとき
// レスポンス全体をメモリに読み込んではならない（配信履歴の1件ごとに last_error が1つ保存される）。
func TestDoJSONRejectsOversizedResponse(t *testing.T) {
	huge := strings.Repeat("A", 1<<20) // 1 MiB
	srv := replyServer(t, 400, huge)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("エラーにすべき")
	}
	if len(err.Error()) > 400 {
		t.Errorf("巨大なレスポンスは長さを制限して読み、切り詰めるべき。エラーメッセージの長さ %d", len(err.Error()))
	}
}
