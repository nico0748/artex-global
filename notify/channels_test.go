package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// singleMsg は引用符と改行を含む単発のメッセージを作る。わざと `"` と `\n` を含むタイトル/要約にしている：
// これこそテンプレートの埋め込みで不正な JSON を最も生みやすい入力だからだ。
func singleMsg() Message {
	return Message{
		Items: []Item{{
			FindingID: 42,
			Name:      `ログイン画面の "SQLインジェクション" のリスク`,
			VulnClass: "SQLインジェクション",
			Severity:  "high",
			Summary:   "引数 id\nが未フィルタでインジェクションにつながる",
			Assets:    []string{"a.example.com", "b.example.com"},
			DetailURL: "https://artex.local/function/findings/detail?id=42",
		}},
	}
}

// batchMsg はまとめのメッセージを一括分作る。
func batchMsg(n int) Message {
	m := Message{Batch: true, WindowMinutes: 30, HomeURL: "https://artex.local/function/findings"}
	for i := 0; i < n; i++ {
		m.Items = append(m.Items, Item{
			FindingID: int64(i + 1),
			Name:      "脆弱性" + itoa(i+1),
			VulnClass: "XSS",
			Severity:  "medium",
			Summary:   "反射型クロスサイトスクリプティング",
			Assets:    []string{"target.example.com"},
		})
	}
	return m
}

// capturePost は偽の受信側を立て、受け取ったリクエストボディとヘッダーをアサーション関数に渡す。
func capturePost(t *testing.T, respBody string, assert func(t *testing.T, body map[string]any, r *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("リクエストボディが正しい JSON ではない: %v\n原文: %s", err, raw)
			}
		}
		if assert != nil {
			assert(t, body, r)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDingTalkSendsActionCardWhenLinkPresent(t *testing.T) {
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msgtype"] != "actionCard" {
			t.Fatalf("戻りリンクがあるときは actionCard を送るべきだが、%v を得た", body["msgtype"])
		}
		card, _ := body["actionCard"].(map[string]any)
		if card["singleURL"] != "https://artex.local/function/findings/detail?id=42" {
			t.Errorf("戻りリンクが失われた: %v", card["singleURL"])
		}
	})
	if _, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("配信に失敗: %v", err)
	}
}

func TestDingTalkFallsBackToMarkdownForBatch(t *testing.T) {
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msgtype"] != "markdown" {
			t.Fatalf("まとめのメッセージは markdown で送るべきだが、%v を得た", body["msgtype"])
		}
		md, _ := body["markdown"].(map[string]any)
		if !strings.Contains(md["text"].(string), "최근 30분간") {
			t.Errorf("まとめの本文に時間枠がない: %v", md["text"])
		}
	})
	if _, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, batchMsg(3)); err != nil {
		t.Fatalf("配信に失敗: %v", err)
	}
}

// TestDingTalkBusinessErrorIsPermanent は「HTTP 200 だが errcode が 0 でない」の判定を固定する。
// errcode を確かめないと配信の失敗を成功と記録してしまう——これは中国の各 IM プラットフォームに共通の落とし穴。
func TestDingTalkBusinessErrorIsPermanent(t *testing.T) {
	srv := capturePost(t, `{"errcode":310000,"errmsg":"keywords not in content"}`, nil)
	_, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg())
	if err == nil {
		t.Fatal("errcode が 0 でなければエラーにすべき")
	}
	if !IsPermanent(err) {
		t.Fatalf("キーワードの不一致は設定の誤りなので恒久的な失敗とすべきだが、%v を得た", err)
	}
	if !strings.Contains(err.Error(), "310000") {
		t.Errorf("エラーメッセージにはプラットフォームのエラーコードを含めるべきだが、%v を得た", err)
	}
}

func TestWeComTruncatesCJKWithinByteLimit(t *testing.T) {
	var contentLen int
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		md, _ := body["markdown"].(map[string]any)
		content, _ := md["content"].(string)
		contentLen = len(content)
		if !utf8.ValidString(content) {
			t.Fatal("切り詰め後が正しい UTF-8 ではない——WeCom はメッセージごと拒否する")
		}
	})
	// 十分に長い日本語のまとめを一括分作る。必ず 4096 バイトを超える。
	m := batchMsg(200)
	if _, err := (weComChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, m); err != nil {
		t.Fatalf("配信に失敗: %v", err)
	}
	if contentLen > weComMarkdownLimit {
		t.Fatalf("本文 %d バイトが WeCom の上限 %d を超えている", contentLen, weComMarkdownLimit)
	}
	if contentLen == 0 {
		t.Fatal("本文が空")
	}
}

func TestWeComRateLimitIsRetryableButKeyErrorIsPermanent(t *testing.T) {
	limited := capturePost(t, `{"errcode":45009,"errmsg":"api freq out of limit"}`, nil)
	_, err := (weComChannel{}).Send(context.Background(), map[string]any{"webhook": limited.URL}, singleMsg())
	if err == nil || IsPermanent(err) {
		t.Fatalf("45009 はスライディングウィンドウのレート制限なので再試行できるべきだが、%v を得た", err)
	}

	badKey := capturePost(t, `{"errcode":93000,"errmsg":"invalid webhook url"}`, nil)
	_, err = (weComChannel{}).Send(context.Background(), map[string]any{"webhook": badKey.URL}, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("93000 は key が無効で、再試行では直らないので恒久的な失敗とすべきだが、%v を得た", err)
	}
}

func TestFeishuCardStructureAndSign(t *testing.T) {
	const secret = "SECtest123"
	srv := capturePost(t, `{"code":0,"msg":"success"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msg_type"] != "interactive" {
			t.Fatalf("インタラクティブなカードを送るべきだが、%v を得た", body["msg_type"])
		}
		card, _ := body["card"].(map[string]any)
		header, _ := card["header"].(map[string]any)
		if header["template"] != "orange" {
			t.Errorf("high レベルは orange の配色であるべきだが、%v を得た", header["template"])
		}
		// secret を設定したら署名パラメータを必ず付ける。さもないと Feishu は 19021 で拒否する。
		if body["sign"] == nil || body["timestamp"] == nil {
			t.Fatalf("署名パラメータがない: %v", body)
		}
		// カードの要素には、url が脆弱性の詳細を指すボタンが1つ含まれているべき。
		elements, _ := card["elements"].([]any)
		foundButton := false
		for _, e := range elements {
			em, _ := e.(map[string]any)
			if em["tag"] != "action" {
				continue
			}
			actions, _ := em["actions"].([]any)
			for _, a := range actions {
				am, _ := a.(map[string]any)
				if am["url"] == "https://artex.local/function/findings/detail?id=42" {
					foundButton = true
				}
			}
		}
		if !foundButton {
			t.Fatal("カードに詳細ページを指すボタンがない")
		}
	})
	cfg := map[string]any{"webhook": srv.URL, "secret": secret}
	if _, err := (feishuChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("配信に失敗: %v", err)
	}
}

func TestFeishuWithoutSecretOmitsSign(t *testing.T) {
	srv := capturePost(t, `{"code":0,"msg":"success"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["sign"] != nil || body["timestamp"] != nil {
			t.Fatalf("secret を設定していないときは署名パラメータを付けるべきでない: %v", body)
		}
	})
	if _, err := (feishuChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("配信に失敗: %v", err)
	}
}

func TestTelegramEscapesHTMLInUntrustedContent(t *testing.T) {
	var text string
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		text, _ = body["text"].(string)
		if body["parse_mode"] != "HTML" {
			t.Fatalf("HTML の解析モードを使うべきだが、%v を得た", body["parse_mode"])
		}
	})
	m := Message{Items: []Item{{
		Severity: "high",
		// タイトルと要約は検査対象/モデルの出力から来ており、信頼できない内容である。
		Name:    `<script>alert(1)</script>`,
		Summary: "a & b < c",
	}}}
	if _, err := (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": srv.URL}, m); err != nil {
		t.Fatalf("配信に失敗: %v", err)
	}
	if strings.Contains(text, "<script>") {
		t.Fatalf("HTML がエスケープされておらず、インジェクションの余地がある: %q", text)
	}
	if !strings.Contains(text, "&lt;script&gt;") {
		t.Fatalf("エスケープ後のエンティティを期待したが、%q を得た", text)
	}
	if !strings.Contains(text, "a &amp; b") {
		t.Fatalf("& がエスケープされていない。得たもの %q", text)
	}
}

func TestTelegramErrorClassification(t *testing.T) {
	rateLimited := capturePost(t, `{"ok":false,"error_code":429,"description":"Too Many Requests"}`, nil)
	_, err := (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": rateLimited.URL}, singleMsg())
	if err == nil || IsPermanent(err) {
		t.Fatalf("429 は再試行できるべきだが、%v を得た", err)
	}

	forbidden := capturePost(t, `{"ok":false,"error_code":403,"description":"bot was blocked by the user"}`, nil)
	_, err = (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": forbidden.URL}, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("403 は設定の問題なので恒久的な失敗とすべきだが、%v を得た", err)
	}
}

func TestWebhookDefaultTemplateProducesValidJSON(t *testing.T) {
	// これが既定のテンプレートがある理由：タイトルに引用符や改行があると、素朴な
	// `"title": "{{.Title}}"` の書き方はどれも不正な JSON を生む。{{json .}} ならそうならない。
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["title"] != `[🟠 높음] ログイン画面の "SQLインジェクション" のリスク` {
			t.Errorf("タイトルが正しく復元されていない: %v", body["title"])
		}
		items, _ := body["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("items の数は 1 であるべきだが、%d を得た", len(items))
		}
		it, _ := items[0].(map[string]any)
		if it["summary"] != "引数 id\nが未フィルタでインジェクションにつながる" {
			t.Errorf("要約が正しく復元されていない: %v", it["summary"])
		}
		// 数値は文字列ではなく JSON の数値でなければならない（json:"...,string" のような書き方だとこの落とし穴にはまる）。
		if _, ok := it["finding_id"].(float64); !ok {
			t.Errorf("finding_id は数値であるべきだが、%T を得た", it["finding_id"])
		}
	})
	if _, err := (webhookChannel{}).Send(context.Background(), map[string]any{"url": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("配信に失敗: %v", err)
	}
}

func TestWebhookCustomTemplateAndHeaders(t *testing.T) {
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, r *http.Request) {
		if r.Header.Get("X-Token") != "s3cret" {
			t.Errorf("カスタムヘッダーが失われた: %v", r.Header)
		}
		if body["msg"] != "3 件" {
			t.Errorf("カスタムテンプレートの描画が誤っている: %v", body["msg"])
		}
		if body["first"] != "脆弱性1" {
			t.Errorf("range での取り出しが誤っている: %v", body["first"])
		}
	})
	cfg := map[string]any{
		"url":           srv.URL,
		"headers":       map[string]any{"X-Token": "s3cret"},
		"body_template": `{"msg": {{json (printf "%d 件" .Count)}}, "first": {{json (index .Items 0).Name}}}`,
	}
	if _, err := (webhookChannel{}).Send(context.Background(), cfg, batchMsg(3)); err != nil {
		t.Fatalf("配信に失敗: %v", err)
	}
}

func TestWebhookRejectsNonJSONRenderResult(t *testing.T) {
	cfg := map[string]any{"url": "https://example.com/hook", "body_template": `not json at all`}
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("描画結果が JSON でなければ恒久的な失敗とすべき（テンプレートの書き間違いで、再試行しても無駄）だが、%v を得た", err)
	}
}

func TestWebhookValidateCatchesBadConfigEarly(t *testing.T) {
	bad := []map[string]any{
		{},
		{"url": "file:///etc/passwd"},
		{"url": "https://example.com", "method": "DELETE"},
		{"url": "https://example.com", "body_template": `{{.Items.`},
	}
	for i, cfg := range bad {
		if err := (webhookChannel{}).Validate(cfg); err == nil {
			t.Errorf("%d 組目の設定は拒否されるべき: %v", i, cfg)
		}
	}
}

func TestEmailMessageIsWellFormed(t *testing.T) {
	msg, err := buildEmailMessage("artex@example.com", []string{"a@example.com", "b@example.com"}, singleMsg())
	if err != nil {
		t.Fatalf("メールの組み立てに失敗: %v", err)
	}
	if !strings.HasPrefix(msg, "From: artex@example.com\r\n") {
		t.Fatalf("From ヘッダーが誤っている:\n%s", msg)
	}
	if !strings.Contains(msg, "To: a@example.com, b@example.com\r\n") {
		t.Fatalf("To ヘッダーが誤っている:\n%s", msg)
	}
	// 日本語の件名は RFC 2047 でエンコードしなければならない。さもないとクライアントで文字化けする。
	if !strings.Contains(msg, "Subject: =?utf-8?") {
		t.Fatalf("件名が RFC 2047 でエンコードされていない:\n%s", msg)
	}
	if dec, err := new(mime.WordDecoder).DecodeHeader(mustExtractHeader(t, msg, "Subject")); err != nil {
		t.Fatalf("件名をデコードできない: %v", err)
	} else if !strings.Contains(dec, "SQLインジェクション") {
		t.Fatalf("デコード後の件名の内容が誤っている: %q", dec)
	}

	// 本文は base64 で、デコードすると正しい HTML になるべき。
	parts := strings.SplitN(msg, "\r\n\r\n", 2)
	if len(parts) != 2 {
		t.Fatal("メールにヘッダーと本文の区切りがない")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.TrimSpace(parts[1]), "\r\n", ""))
	if err != nil {
		t.Fatalf("本文の base64 デコードに失敗: %v", err)
	}
	html := string(decoded)
	if !strings.HasPrefix(html, "<div") {
		t.Fatalf("本文が HTML ではない: %.80s", html)
	}
	// タイトルはテキストの位置にそのまま現れる：HTML のテキスト内容の中ではダブルクォートは正当な文字で、エスケープは要らない。
	// ここで「そのまま残る」ことを確かめるのは、将来だれかが誤って引用符のエスケープを1段足し、引用符が
	// &quot; と表示されるのを防ぐため。
	if !strings.Contains(html, `"SQLインジェクション"`) {
		t.Fatalf("タイトル中の引用符はテキストの位置ではそのまま残すべき: %.200s", html)
	}
}

// TestEmailEscapesStructuralInjection はメール本文で本当に防ぐべきインジェクションを扱う：
// 脆弱性のタイトルと要約は検査対象とモデルの出力から来ており、信頼できない内容である。テキストの位置では
// & < > を必ずエスケープし（さもないとタグを注入できる）、属性の位置では引用符もエスケープしなければならない（さもないと href を閉じられる）。
func TestEmailEscapesStructuralInjection(t *testing.T) {
	m := Message{
		Items: []Item{{
			Severity:  "high",
			Name:      `<script>alert(1)</script>`,
			Summary:   "a & b > c",
			DetailURL: `https://artex.local/x?a="onmouseover=alert(1)`,
		}},
	}
	html := htmlBody(m, 0)
	if strings.Contains(html, "<script>") {
		t.Fatalf("タイトルがエスケープされておらず、タグを注入できる: %s", html)
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Fatalf("エスケープ後のエンティティを期待した: %s", html)
	}
	if !strings.Contains(html, "a &amp; b &gt; c") {
		t.Fatalf("& と > がエスケープされていない: %s", html)
	}
	// 戻りリンクは管理者が設定できる public_base_url で、それ自体の信頼度は比較的高いが、属性の位置では
	// 引用符を必ずエスケープする——さもないと引用符を含むアドレスが href を閉じ、イベントハンドラを注入できる。
	if strings.Contains(html, `onmouseover=alert(1)">`) {
		t.Fatalf("href 属性が正しくエスケープされていない: %s", html)
	}
	if !strings.Contains(html, "&quot;") {
		t.Fatalf("属性の位置の引用符はエスケープされるべき: %s", html)
	}
}

func mustExtractHeader(t *testing.T, msg, name string) string {
	t.Helper()
	for _, line := range strings.Split(msg, "\r\n") {
		if strings.HasPrefix(line, name+": ") {
			return strings.TrimPrefix(line, name+": ")
		}
	}
	t.Fatalf("%s ヘッダーが見つからない", name)
	return ""
}

func TestChannelValidateReportsMissingFields(t *testing.T) {
	// 検証エラーは設定者にそのまま表示されるので、漠然とした「設定が無効」ではなく、何が足りないかをはっきり示さなければならない。
	cases := []struct {
		kind   string
		cfg    map[string]any
		substr string
	}{
		{KindDingTalk, map[string]any{}, "Webhook"},
		{KindFeishu, map[string]any{}, "Webhook"},
		{KindWeCom, map[string]any{}, "Webhook"},
		{KindTelegram, map[string]any{}, "Bot Token"},
		{KindTelegram, map[string]any{"bot_token": "t"}, "Chat ID"},
		{KindEmail, map[string]any{}, "SMTP"},
		{KindEmail, map[string]any{"host": "h"}, "포트"},
		{KindEmail, map[string]any{"host": "h", "port": 587, "from": "f"}, "수신자"},
	}
	for _, tc := range cases {
		ch, ok := Get(tc.kind)
		if !ok {
			t.Fatalf("チャネル %s が登録されていない", tc.kind)
		}
		err := ch.Validate(tc.cfg)
		if err == nil {
			t.Errorf("%s の設定 %v は検証に失敗すべき", tc.kind, tc.cfg)
			continue
		}
		if !strings.Contains(err.Error(), tc.substr) {
			t.Errorf("%s のエラーメッセージは %q に触れるべきだが、%q を得た", tc.kind, tc.substr, err.Error())
		}
	}
}

// TestEmailSMTPErrorClassification は SMTP の 4xx/5xx の意味の区別を固定する。
// 4xx まで恒久的な失敗と判定すると、グレーリストを有効にしたメールサーバーでは、どの通知も最初の
// 試行で failed になってしまう——グレーリストこそ自動再試行が最も役立つ場面なのに。
func TestEmailSMTPErrorClassification(t *testing.T) {
	cases := []struct {
		reply     string
		permanent bool
	}{
		{"450 4.7.1 Greylisting in action, please come back later", false},
		{"451 4.3.0 Temporary system failure", false},
		{"452 4.2.2 Mailbox full", false},
		{"550 5.1.1 User unknown", true},
		{"553 5.1.3 Bad address syntax", true},
		{"554 5.7.1 Relay access denied", true},
		// 応答コードが取れないときは「再試行できる」とみなす：一時的かもしれない故障を見限るより、
		// もう一度試すほうがよい。
		{"unexpected EOF", false},
		{"", false},
	}
	for _, tc := range cases {
		err := smtpStageError("受信者が拒否された", errors.New(tc.reply))
		if got := IsPermanent(err); got != tc.permanent {
			t.Errorf("応答 %q: permanent=%v を期待したが %v を得た", tc.reply, tc.permanent, got)
		}
		// どう分類しても、原文は利用者の調査のために残さなければならない。
		if tc.reply != "" && !strings.Contains(err.Error(), tc.reply) {
			t.Errorf("応答 %q の原文が捨てられた: %v", tc.reply, err)
		}
	}
}

func TestRegistryCoversAllKinds(t *testing.T) {
	// 6つのチャネルはどれも欠かせない——1つでも欠けると UI のドロップダウンから黙って消える。
	want := []string{KindDingTalk, KindEmail, KindFeishu, KindTelegram, KindWebhook, KindWeCom}
	got := Kinds()
	if len(got) != len(want) {
		t.Fatalf("チャネルの数は %d であるべきだが、%d を得た: %v", len(want), len(got), got)
	}
	for _, k := range want {
		if !ValidKind(k) {
			t.Errorf("チャネル %s が登録されていない", k)
		}
		if ch, ok := Get(k); !ok || ch.Kind() != k {
			t.Errorf("チャネル %s の Kind() が登録キーと一致しない", k)
		}
	}
	if ValidKind("nope") {
		t.Error("登録されていない種類は検証を通るべきでない")
	}
}

func TestPermanentErrorUnwrap(t *testing.T) {
	base := &permanentSentinel{}
	err := Permanent(base)
	if !IsPermanent(err) {
		t.Fatal("恒久的な失敗と識別すべき")
	}
	if !strings.Contains(err.Error(), "sentinel") {
		t.Fatalf("エラーメッセージは下層のものをそのまま伝えるべき: %v", err)
	}
	if Permanent(nil) != nil {
		t.Fatal("Permanent(nil) は必ず nil を返すこと")
	}
	if IsPermanent(nil) {
		t.Fatal("nil は恒久的な失敗ではない")
	}
}

type permanentSentinel struct{}

func (*permanentSentinel) Error() string { return "sentinel" }
