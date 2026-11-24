package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// このファイルは通知の機能の端から端までの挙動を覆う：脆弱性の保存 → イベント → 振り分け → 本当に HTTP を送る。
//
// 安全上の注意点：これらのケースは**全体の Notifier.step() を呼ばず**、自分で作った
// チャンネルについてだけ stepRealtime/stepDigest を呼ぶ。理由は、step() が DB のすべての有効なチャンネルを辿るから——
// 本物の DingTalk/WeCom のボットを設定済みの開発用の DB でテストを走らせると、全体の step はテストの間に
// 生じた脆弱性をそれらのグループに本当に送ってしまう。チャンネルごとに呼ぶことで、影響の範囲をテストが自分で作った偽の受け手に厳密に限る。
//
// 後片付け：ケースの終わりに、そのケースが生んだイベント（配信も連鎖して消える）とチャンネルを消し、本物のチャンネルに溜まりを残さない。
//
// 断言の基準：stepRealtime/stepDigest は値を返さず中でログを記録するので、ここで断言するのは
// **外から観察できる挙動**（偽の受け手が何を受け取ったか、配信の行がどの状態になったか）で、関数の
// 戻り値ではない——戻り値をスタブにするより本物の呼び出しの経路に近い。

// notifyFixture はこのファイルのケースの共通の装置。
type notifyFixture struct {
	s       *Server
	pg      *db.DB
	request func(method, path, body string) *httptest.ResponseRecorder
	n       *Notifier
	// 自分で作った task/exploration：ケースは脆弱性をここに記録し、他のケースのデータと切り離す。
	taskID int64
	expID  int64
	// cleanupMark の後に生じたイベントは後片付けのときに一緒に消す。
	cleanupMark int64
}

func newNotifyFixture(t *testing.T) *notifyFixture {
	t.Helper()
	// このファイルの偽の受け手はすべて 127.0.0.1 で動くが、配信は既定でループバックのアドレスを拒む
	// （同じマシンのサービスやクラウドのメタデータに SSRF で届くのを防ぐ）。テストはこの切り替えを明示的に開ける；
	// 「既定で拒む」挙動を守るのは notify パッケージの ssrf_test.go。
	t.Setenv(notify.AllowLocalTargetsEnv, "1")
	// 백그라운드 투입(配信) 루프(3초 tick)를 끈다. 이 파일의 케이스들은 stepRealtime·
	// stepDigest 를 직접 호출해 한 번의 분배 결과(앞 K건 송달·나머지 보류)를 검증하는데,
	// 백그라운드 루프가 같은 채널을 동시에 처리하면 집계가 타이밍에 따라 흔들려(느린 CI
	// 에서 간헐 실패) 결정성이 깨진다. 이 변수는 trafficEvidenceServer 가 서버를 세우기
	// 전에 설정돼야 효과가 있다.
	t.Setenv(notifyBackgroundDisabledEnv, "1")
	s, _, request := trafficEvidenceServer(t)
	pg := s.m.pg

	// task を自分で作る：共有の装置 trafficEvidenceServer が作る task では exploration id が取れず、
	// 脆弱性を記録するにはそれが必要。
	task, err := s.m.CreateTask("通知のテスト", "通知の挙動の検証", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Exec(`DELETE FROM tasks WHERE id=$1`, taskID) })

	var mark int64
	if err := pg.QueryRow(`SELECT COALESCE(max(id),0) FROM notification_events`).Scan(&mark); err != nil {
		t.Fatal(err)
	}
	// 装置を自己完結させる：fixture を作る前からあるイベントを一度にすべて振り分け済みにする。
	//
	// なぜ必要か：FanOutPendingEvents は**全体**に効き、DB のすべての未振り分けのイベントを
	// 一致するすべてのチャンネルに展開する。共有の装置 trafficEvidenceServer は自分で脆弱性を1件記録し
	// （まさにそれが返す最初の finding）、他のケースの残りもありうる。切り離さないと、
	// これらの迷子のイベントがこのケースのチャンネルに振り分けられ、「配信が N 件あるはず」のような断言が
	// 通ったり通らなかったりする——しかも失敗のしかたがケースの実行順に左右され、すぐ失敗するより調べにくい。
	if _, err := pg.Exec(`UPDATE notification_events SET fanned_out = true WHERE id <= $1 AND NOT fanned_out`, mark); err != nil {
		t.Fatal(err)
	}

	f := &notifyFixture{s: s, pg: pg, request: request, n: newNotifier(s), taskID: taskID, expID: task.ExpID, cleanupMark: mark}
	t.Cleanup(func() {
		if _, err := pg.Exec(`DELETE FROM notification_events WHERE id > $1`, f.cleanupMark); err != nil {
			t.Logf("通知のイベントを片付けられなかった: %v", err)
		}
	})
	// 全体のスイッチはオンでなければならない（他のケースがオフにしたかもしれない）。
	if err := pg.SetBool(settingNotifyEnabled, true); err != nil {
		t.Fatal(err)
	}
	return f
}

// record は本物の証拠の書き込みの経路で脆弱性を1件保存し、finding id を返す。
// この経路は**同じトランザクション**で通知のイベントを登録する——まさにこの機能のつなぎ目。
func (f *notifyFixture) record(t *testing.T, vulnclass, severity string) int64 {
	t.Helper()
	out, err := f.s.evidenceStore().Record(context.Background(), db.RecordFindingInput{
		TaskID:        f.taskID,
		ExplorationID: f.expID,
		Worker:        "test",
		VulnClass:     vulnclass,
		Name:          vulnclass,
		Severity:      severity,
		Summary:       vulnclass + " の要約",
		Evidence:      "poc",
	}, nil)
	if err != nil {
		t.Fatalf("脆弱性を記録できなかった: %v", err)
	}
	return out.FindingID
}

// channel はチャンネルの設定を読み戻す（チャンネルごとに stepX を呼ぶのに使う）。
func (f *notifyFixture) channel(t *testing.T, id int64) *db.NotificationChannel {
	t.Helper()
	ch, err := f.pg.NotificationChannelByID(context.Background(), id)
	if err != nil {
		t.Fatalf("チャンネルを読めなかった: %v", err)
	}
	return ch
}

// deliver はイベントを振り分け、指定したチャンネルについてだけ配信を1ラウンド走らせる。
func (f *notifyFixture) deliver(t *testing.T, chID int64, baseURL string) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatalf("振り分けに失敗: %v", err)
	}
	f.n.stepRealtime(ctx, f.channel(t, chID), 50, baseURL)
}

// createChannel は HTTP のインターフェースでチャンネルを作り、ついでにインターフェース自身の検証の経路も覆う。
func (f *notifyFixture) createChannel(t *testing.T, payload map[string]any) int64 {
	t.Helper()
	raw, _ := json.Marshal(payload)
	r := f.request("POST", "/api/notify/channels", string(raw))
	if r.Code != 200 {
		t.Fatalf("チャンネルを作れなかった %d: %s", r.Code, r.Body)
	}
	var res struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &res); err != nil || res.ID == 0 {
		t.Fatalf("チャンネルの作成の戻り値がおかしい: %s (%v)", r.Body, err)
	}
	t.Cleanup(func() { f.pg.Exec(`DELETE FROM notification_channels WHERE id=$1`, res.ID) })
	return res.ID
}

// fakeWebhook は受け取ったリクエストの本文を記録する偽の受け手。
type fakeWebhook struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
}

func newFakeWebhook(t *testing.T) *fakeWebhook {
	t.Helper()
	f := &fakeWebhook{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeWebhook) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakeWebhook) body(t *testing.T, i int) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.bodies) {
		t.Fatalf("偽の受け手は %d 件のリクエストしか受け取っておらず、%d 件目を取れない", len(f.bodies), i)
	}
	return f.bodies[i]
}

func (f *fakeWebhook) last(t *testing.T) map[string]any {
	t.Helper()
	if f.count() == 0 {
		t.Fatal("偽の受け手はリクエストを1つも受け取っていない")
	}
	return f.body(t, f.count()-1)
}

// markdownText はリクエストの本文から本文を取り出し、各社のフィールド名の違いに対応する：
// DingTalk の markdown は `text`、ActionCard は `text`、WeCom の markdown は `content`。
func markdownText(t *testing.T, body map[string]any) string {
	t.Helper()
	for _, key := range []string{"markdown", "actionCard"} {
		section, ok := body[key].(map[string]any)
		if !ok {
			continue
		}
		for _, field := range []string{"text", "content"} {
			if s, ok := section[field].(string); ok && s != "" {
				return s
			}
		}
	}
	t.Fatalf("リクエストの本文に識別できる本文がない: %v", body)
	return ""
}

// agePendingBatch はそのチャンネルの送信待ちの配信を古くし、まとめの一括の期限切れのテストに使う。
func (f *notifyFixture) agePendingBatch(t *testing.T, chID int64) {
	t.Helper()
	if _, err := f.pg.Exec(`UPDATE notification_deliveries SET created_at = now() - interval '2 hours'
WHERE channel_id=$1 AND state=$2`, chID, db.NotifyStatePending); err != nil {
		t.Fatal(err)
	}
}

func TestNotifyEndToEndRealtimeDelivery(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "リアルタイムの通知",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	f.record(t, "SQLインジェクション", "high")
	f.deliver(t, chID, "")

	if hook.count() != 1 {
		t.Fatalf("メッセージを 1 件送るはずが、実際は %d", hook.count())
	}
	text := markdownText(t, hook.last(t))
	// "SQLインジェクション" 은 입력으로 넣은 취약점 제목이라 카드에 그대로 에코된다(사용자 데이터,
	// 번역 대상 아님). "높음"·"개요" 는 렌더 라벨이다 — 심각도는 notify.SeverityLabel("high")
	// 가 내는 "🟠 높음", 요약 머리글은 writeItem 이 붙이는 "**개요**:" 에 각각 들어 있다.
	for _, want := range []string{"SQLインジェクション", "높음", "개요"} {
		if !strings.Contains(text, want) {
			t.Fatalf("メッセージの本文に %q がない:\n%s", want, text)
		}
	}
	// 配信は sent に進むはず。
	var pending int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1 AND state <> $2`,
		chID, db.NotifyStateSent).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("配信の後もまだ %d 件が sent になっていない", pending)
	}
}

func TestNotifyChannelAPIMasksSecretsAndPreservesOnUpdate(t *testing.T) {
	f := newNotifyFixture(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "マスクのケース",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "https://oapi.dingtalk.com/robot/send?access_token=abc123456", "secret": "SECabcdef123456"},
	})

	r := f.request("GET", "/api/notify/channels", "")
	if r.Code != 200 {
		t.Fatalf("チャンネルの一覧に失敗 %d: %s", r.Code, r.Body)
	}
	if strings.Contains(r.Body.String(), "abc123456") || strings.Contains(r.Body.String(), "SECabcdef123456") {
		t.Fatalf("インターフェースの返した値が資格情報を漏らした: %s", r.Body)
	}
	var listed struct {
		Channels []struct {
			ID         int64          `json:"id"`
			Config     map[string]any `json:"config"`
			SecretKeys []string       `json:"secret_keys"`
		} `json:"channels"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	var mine *struct {
		ID         int64          `json:"id"`
		Config     map[string]any `json:"config"`
		SecretKeys []string       `json:"secret_keys"`
	}
	for i := range listed.Channels {
		if listed.Channels[i].ID == chID {
			mine = &listed.Channels[i]
		}
	}
	if mine == nil {
		t.Fatal("作ったチャンネルが一覧に現れない")
	}
	if !notify.IsMasked(fmt.Sprint(mine.Config["webhook"])) || !notify.IsMasked(fmt.Sprint(mine.Config["secret"])) {
		t.Fatalf("資格情報のフィールドはマスクした値のはず: %v", mine.Config)
	}
	if len(mine.SecretKeys) == 0 {
		t.Fatal("インターフェースはどのフィールドが資格情報かをフロントエンドに知らせるはず")
	}

	// PATCH で名前だけを変え + マスクした資格情報を送り返す：本物の資格情報はそのまま残らなければならない。
	body, _ := json.Marshal(map[string]any{
		"name":   "名前を変えた後",
		"config": map[string]any{"webhook": fmt.Sprint(mine.Config["webhook"]), "secret": fmt.Sprint(mine.Config["secret"])},
	})
	if r := f.request("PATCH", fmt.Sprintf("/api/notify/channels/%d", chID), string(body)); r.Code != 200 {
		t.Fatalf("更新に失敗 %d: %s", r.Code, r.Body)
	}
	cfg := f.channelConfig(t, chID)
	if cfg["webhook"] != "https://oapi.dingtalk.com/robot/send?access_token=abc123456" {
		t.Fatalf("マスクの送り返しが本物の資格情報を上書きした: %v", cfg["webhook"])
	}
	if cfg["secret"] != "SECabcdef123456" {
		t.Fatalf("マスクの送り返しが secret を上書きした: %v", cfg["secret"])
	}
	if f.channel(t, chID).Name != "名前を変えた後" {
		t.Fatal("名前が更新されていない")
	}

	// 明示的に secret を消したら効くはず（「マスクを送り返す=変えない」とは違う）。
	body, _ = json.Marshal(map[string]any{"config": map[string]any{"secret": ""}})
	if r := f.request("PATCH", fmt.Sprintf("/api/notify/channels/%d", chID), string(body)); r.Code != 200 {
		t.Fatalf("secret を消せなかった %d: %s", r.Code, r.Body)
	}
	if _, still := f.channelConfig(t, chID)["secret"]; still {
		t.Fatal("空の文字列で secret を消せるはず")
	}
}

func (f *notifyFixture) channelConfig(t *testing.T, id int64) map[string]any {
	t.Helper()
	var cfg map[string]any
	if err := json.Unmarshal(f.channel(t, id).Config, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestNotifyChannelAPICreateValidation(t *testing.T) {
	f := newNotifyFixture(t)
	cases := []struct {
		name    string
		payload map[string]any
		wantSub string
	}{
		// wantSub 는 server/notify_api.go 가 내보내는 한국어 검증 오류와 일치시킨다
		// (notifyErrKindInvalidFmt·notifyErrNameMissing·notifyErrModeInvalid 의 안정 문구).
		// webhook 두 건은 notify/dingtalk.go 의 Validate 가 내는 오류인데, 그 webhook 주소
		// 검증 문구는 이미 한국어로 현지화됐으므로(notify/dingtalk_feishu_wecom_localized_test.go
		// 가 "Webhook 주소가 없습니다"·"Webhook 주소가 올바르지 않습니다" 로 검증한다) 기대
		// 문자열도 한국어 안정 문구로 맞춘다.
		{"不正な種類", map[string]any{"name": "x", "kind": "nope", "config": map[string]any{}}, "채널 유형이 올바르지 않습니다"},
		{"名前なし", map[string]any{"kind": notify.KindDingTalk, "config": map[string]any{"webhook": "https://e.com/h"}}, "채널 이름을 입력하세요"},
		{"webhook なし", map[string]any{"name": "x", "kind": notify.KindDingTalk, "config": map[string]any{}}, "Webhook 주소가 없습니다"},
		{"webhook の不正なプロトコル", map[string]any{"name": "x", "kind": notify.KindDingTalk, "config": map[string]any{"webhook": "file:///etc/passwd"}}, "Webhook 주소가 올바르지 않습니다"},
		{"不正なモード", map[string]any{"name": "x", "kind": notify.KindDingTalk, "mode": "sometimes", "config": map[string]any{"webhook": "https://e.com/h"}}, "발송 모드가 올바르지 않습니다"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.payload)
			r := f.request("POST", "/api/notify/channels", string(raw))
			if r.Code != 400 {
				t.Fatalf("400 を返すはずが、%d だった: %s", r.Code, r.Body)
			}
			if !strings.Contains(r.Body.String(), tc.wantSub) {
				t.Fatalf("エラーのメッセージは %q に触れるはずが、%s だった", tc.wantSub, r.Body)
			}
		})
	}
	if r := f.request("DELETE", "/api/notify/channels/99999999", ""); r.Code != 404 {
		t.Fatalf("存在しないチャンネルの削除は 404 のはずが、%d だった", r.Code)
	}
}

func TestNotifyFilterBlocksBelowThreshold(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "緊急だけ",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
		"filter": map[string]any{"min_severity": "critical"},
	})
	f.record(t, "低い危険の問題", "low")
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("しきい値より低い脆弱性は配信を生むべきでないのに、%d 件あった", n)
	}
	f.n.stepRealtime(context.Background(), f.channel(t, chID), 50, "")
	if hook.count() != 0 {
		t.Fatal("絞り込まれた脆弱性はメッセージを送るべきでない")
	}
}

func TestNotifyDigestBatchesMultipleFindingsIntoOneMessage(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "まとめの通知",
		"kind":   notify.KindDingTalk,
		"mode":   db.NotifyModeDigest,
		"config": map[string]any{"webhook": hook.URL},
	})
	for i := 0; i < 3; i++ {
		f.record(t, fmt.Sprintf("まとめの脆弱性%d", i+1), "high")
	}
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	ch := f.channel(t, chID)

	// 期限前：送らない。
	f.n.stepDigest(ctx, ch, 50, "")
	if hook.count() != 0 {
		t.Fatal("まとめの一括が期限前に送られた")
	}

	// 一括を古くした後：3件が1件のメッセージにまとまる。
	f.agePendingBatch(t, chID)
	f.n.stepDigest(ctx, ch, 50, "")
	if got := hook.count(); got != 1 {
		t.Fatalf("3件は1件のメッセージにまとまるはずが、実際は %d 件送った", got)
	}
	text := markdownText(t, hook.last(t))
	// 시간창이 있는 다이제스트 머리말은 markdown.go 가 "**최근 N분간 신규 취약점 N건**"
	// 으로 렌더한다(digestInterval 기본 30분이라 WindowMinutes>0).
	if !strings.Contains(text, "최근") || !strings.Contains(text, "신규 취약점 3건") {
		t.Fatalf("まとめのメッセージに件数/時間の窓の文がない:\n%s", text)
	}
	for i := 1; i <= 3; i++ {
		if !strings.Contains(text, fmt.Sprintf("まとめの脆弱性%d", i)) {
			t.Fatalf("まとめのメッセージに %d 件目がない:\n%s", i, text)
		}
	}
	// 同じ一括は batch_id を共有するはず。
	var distinct, total int
	if err := f.pg.QueryRow(`SELECT count(DISTINCT batch_id), count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&distinct, &total); err != nil {
		t.Fatal(err)
	}
	if total != 3 || distinct != 1 {
		t.Fatalf("3件の配信は batch_id を1つ共有するはずが、distinct=%d total=%d だった", distinct, total)
	}
}

func TestNotifyDisabledChannelDoesNotSend(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":    "無効にしたチャンネル",
		"kind":    notify.KindDingTalk,
		"enabled": false,
		"config":  map[string]any{"webhook": hook.URL},
	})
	f.record(t, "無効の間の脆弱性", "critical")
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("無効にしたチャンネルは配信を生むべきでないのに、%d 件あった", n)
	}
}

func TestNotifyStatusChangeDelivery(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "状態の変更の購読",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
		"filter": map[string]any{"on_status_change": true},
	})
	finding := f.record(t, "状態の変更のケース", "high")
	r := f.request("PATCH", fmt.Sprintf("/api/exploration/findings/%d", finding), `{"status":"fixed"}`)
	if r.Code != 200 {
		t.Fatalf("状態を変えられなかった %d: %s", r.Code, r.Body)
	}
	f.deliver(t, chID, "")

	// 2件あるはず：fixed の1件は状態の変更；finding_created の1件も同じラウンドで送られることがある。
	// 状態の変更のほうが実際には後に作られるが、順序には頼らず、全部から探す。
	found := false
	for i := 0; i < hook.count(); i++ {
		text := markdownText(t, hook.body(t, i))
		// 상태 변경 카드는 markdown.go 가 "**상태 변경**: %s → %s" 로, 상태값은
		// notify.StatusLabel 이 렌더한다(fixed → "수정됨").
		if strings.Contains(text, "상태 변경") && strings.Contains(text, "수정됨") {
			found = true
		}
	}
	if !found {
		t.Fatalf("「状態の変更 → 修正済み」を含むメッセージを受け取っていない（全 %d 件）", hook.count())
	}
}

func TestNotifyStatusChangeSuppressedByDefault(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "状態の変更を購読しない",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	finding := f.record(t, "変更を購読しない", "high")
	if r := f.request("PATCH", fmt.Sprintf("/api/exploration/findings/%d", finding), `{"status":"false_positive"}`); r.Code != 200 {
		t.Fatalf("状態を変えられなかった %d: %s", r.Code, r.Body)
	}
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id
WHERE d.channel_id=$1 AND e.kind=$2`, chID, notify.EventFindingStatusChanged).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("状態の変更を購読していないチャンネルは状態の変更の配信を受け取るべきでないのに、%d 件あった", n)
	}
}

func TestNotifyTestMessageEndpoint(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "テストの送信",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	if r := f.request("POST", fmt.Sprintf("/api/notify/channels/%d/test", chID), ""); r.Code != 200 {
		t.Fatalf("テストの送信に失敗 %d: %s", r.Code, r.Body)
	}
	if hook.count() != 1 {
		t.Fatalf("偽の受け手はテストのメッセージを 1 件受け取るはずが、%d だった", hook.count())
	}
	// テストのメッセージはひと目でテストとわかる必要があり、本物の脆弱性と取り違えられてはならない。
	// 테스트 메시지 제목은 notify_api.go 의 notifyTestName("테스트 메시지 · 채널 설정 정상").
	if text := markdownText(t, hook.last(t)); !strings.Contains(text, "테스트") {
		t.Fatalf("テストのメッセージはテストだと示すはず: %s", text)
	}
	// 設定が壊れているときは、チャンネルの生のエラーをありのままにユーザーに返すはず。
	badID := f.createChannel(t, map[string]any{
		"name":   "壊れたアドレス",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "http://127.0.0.1:1/hook"},
	})
	if r := f.request("POST", fmt.Sprintf("/api/notify/channels/%d/test", badID), ""); r.Code != 502 {
		t.Fatalf("配信に失敗したら 502 を返すはずが、%d だった: %s", r.Code, r.Body)
	}
}

func TestNotifyDeliveriesHistoryAndRetry(t *testing.T) {
	f := newNotifyFixture(t)
	// 必ず失敗するアドレスを指し、failed の配信を作る。
	chID := f.createChannel(t, map[string]any{
		"name":   "失敗と再試行",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "http://127.0.0.1:1/hook"},
	})
	f.record(t, "失敗する通知", "high")
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	ch := f.channel(t, chID)
	// 再試行の予算を使い切るまで続けて配信する。
	for i := 0; i < db.MaxNotifyAttempts; i++ {
		f.n.stepRealtime(ctx, ch, 50, "")
		if _, err := f.pg.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE channel_id=$1`, chID); err != nil {
			t.Fatal(err)
		}
	}
	var state string
	if err := f.pg.QueryRow(`SELECT state FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != db.NotifyStateFailed {
		t.Fatalf("再試行を使い切った後は failed のはずが、%s だった", state)
	}

	r := f.request("GET", fmt.Sprintf("/api/notify/deliveries?channel_id=%d&state=failed", chID), "")
	if r.Code != 200 {
		t.Fatalf("履歴を調べられなかった %d: %s", r.Code, r.Body)
	}
	var hist struct {
		Deliveries []struct {
			ID        int64  `json:"id"`
			State     string `json:"state"`
			LastError string `json:"last_error"`
			Attempts  int    `json:"attempts"`
			Title     string `json:"title"`
		} `json:"deliveries"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &hist); err != nil {
		t.Fatal(err)
	}
	if hist.Total != 1 || len(hist.Deliveries) != 1 {
		t.Fatalf("失敗した配信が 1 件見つかるはずが、total=%d len=%d だった", hist.Total, len(hist.Deliveries))
	}
	if hist.Deliveries[0].LastError == "" {
		t.Fatal("履歴には失敗の理由が付くはず。さもないとユーザーは調べられない")
	}
	if hist.Deliveries[0].Attempts < db.MaxNotifyAttempts {
		t.Fatalf("試行の回数が記録されるはずが、%d だった", hist.Deliveries[0].Attempts)
	}
	if hist.Deliveries[0].Title != "失敗する通知" {
		t.Fatalf("履歴には脆弱性のタイトルが出るはずが、%q だった", hist.Deliveries[0].Title)
	}

	// 手動での送り直し：pending に戻り、数がゼロに戻るはず。
	if r := f.request("POST", fmt.Sprintf("/api/notify/deliveries/%d/retry", hist.Deliveries[0].ID), ""); r.Code != 200 {
		t.Fatalf("送り直しに失敗 %d: %s", r.Code, r.Body)
	}
	var attempts int
	if err := f.pg.QueryRow(`SELECT state, attempts FROM notification_deliveries WHERE id=$1`, hist.Deliveries[0].ID).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != db.NotifyStatePending || attempts != 0 {
		t.Fatalf("送り直した後は pending で attempts=0 のはずが、%s/%d だった", state, attempts)
	}
}

func TestNotifyMetaAndSettingsRoundTrip(t *testing.T) {
	f := newNotifyFixture(t)
	r := f.request("GET", "/api/notify/meta", "")
	if r.Code != 200 {
		t.Fatalf("meta に失敗: %s", r.Body)
	}
	var meta struct {
		Kinds []struct {
			Kind       string   `json:"kind"`
			SecretKeys []string `json:"secret_keys"`
		} `json:"kinds"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if len(meta.Kinds) != len(notify.Kinds()) {
		t.Fatalf("meta はすべての %d 個のチャンネルを並べるはずが、%d だった", len(notify.Kinds()), len(meta.Kinds))
	}
	for _, k := range meta.Kinds {
		if len(k.SecretKeys) == 0 {
			t.Errorf("チャンネル %s が資格情報のフィールドを報告していない", k.Kind)
		}
	}

	// 3つの全体の設定の往復。末尾のスラッシュはそろえて除くはず。さもないと戻るリンクが "//function/..." になる。
	if r := f.request("PUT", "/api/settings", `{"notify_public_base_url":"https://artex.example.com/","notify_digest_interval_min":15,"notify_enabled":true}`); r.Code != 200 {
		t.Fatalf("設定を書けなかった %d: %s", r.Code, r.Body)
	}
	t.Cleanup(func() {
		f.pg.Exec(`DELETE FROM settings WHERE key IN ($1,$2)`, settingNotifyPublicBaseURL, settingNotifyDigestMinutes)
	})
	payload := f.s.settingsPayload()
	if payload["notify_public_base_url"] != "https://artex.example.com" {
		t.Fatalf("戻るリンクのアドレスがそろえられていない: %v", payload["notify_public_base_url"])
	}
	if payload["notify_digest_interval_min"] != 15 {
		t.Fatalf("まとめの周期が効いていない: %v", payload["notify_digest_interval_min"])
	}

	// 不正な値は拒まれるはず。
	for _, body := range []string{
		`{"notify_public_base_url":"ftp://x"}`,
		`{"notify_digest_interval_min":0}`,
		`{"notify_digest_interval_min":99999}`,
	} {
		if r := f.request("PUT", "/api/settings", body); r.Code != 400 {
			t.Errorf("%s は 400 を返すはずが、%d だった", body, r.Code)
		}
	}
}

// TestNotifyDeepLinkUsesPublicBaseURL は戻るリンクの組み立てを覆う：public_base_url を設定したとき、
// 1件のメッセージはボタン付きの ActionCard を使い、リンクは脆弱性の詳細のページを指さなければならない。
func TestNotifyDeepLinkUsesPublicBaseURL(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "戻るリンク",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	finding := f.record(t, "戻るリンク付きの脆弱性", "high")
	f.deliver(t, chID, "https://artex.example.com")

	body := hook.last(t)
	card, _ := body["actionCard"].(map[string]any)
	if card == nil {
		t.Fatalf("戻るリンクがあるときは ActionCard を使うはずが、msgtype=%v だった", body["msgtype"])
	}
	want := fmt.Sprintf("https://artex.example.com/function/findings/detail?id=%d", finding)
	if card["singleURL"] != want {
		t.Fatalf("戻るリンクが違う\n期待 %s\n実際 %v", want, card["singleURL"])
	}
}

// TestNotifyNoDeepLinkWithoutBaseURL は逆を覆う：外部のアドレスを設定していないときは壊れたリンクを生むべきでなく
// （localhost や相対パスを指すなど）、素の markdown に戻るはず。
func TestNotifyNoDeepLinkWithoutBaseURL(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "戻るリンクなし",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	f.record(t, "戻るリンクなしの脆弱性", "high")
	f.deliver(t, chID, "")

	body := hook.last(t)
	if body["msgtype"] != "markdown" {
		t.Fatalf("外部のアドレスを設定していないときは markdown を送るはずが、%v だった", body["msgtype"])
	}
	// 상세 링크 머리글은 markdown.go 가 "[상세 보기](URL)" 로 렌더한다 — 외부 주소가
	// 없으면 이 링크가 아예 나오지 않아야 한다(옛 중국어 라벨 "詳細を見る" 원문을 검사하면 라벨이
	// 한국어로 바뀐 지금은 항상 통과해 회귀를 못 잡는다).
	if text := markdownText(t, body); strings.Contains(text, "상세 보기") {
		t.Fatalf("外部のアドレスを設定していないときは詳細のリンクが出るべきでない:\n%s", text)
	}
}

// TestNotifyDigestSegmentsAndDefersRemainder は「黙って失われる」の修正の端から端までの証拠。
//
// まとめのメッセージはチャンネルの長さの上限に縛られる（WeCom は 4096 バイト）ので、1つの一括に入りきらなければ**1件単位で**分けなければならない：
// このメッセージに入ったものに届いた印を付け、残りは待ち行列に戻して次のメッセージを待つ。以前の実装は一括まるごとに
// 成功の印を付けていた——切られたものはメッセージにも失敗の一覧にもなく、配信の履歴は成功と表示し、
// 脆弱性はそのまま消えていた。
//
// 断言するのは4つ：① 実際に入った件数だけに印を付けた ② 残りはまだ送信待ち ③ 先送りした項目は
// **再試行の回数を使っていない** ④ もう1ラウンド走らせれば残りを送れる（詰まらない）。
func TestNotifyDigestSegmentsAndDefersRemainder(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	// WeCom を使う：markdown の上限は 4096 バイトで、6つのチャンネルの中で最も厳しい。
	chID := f.createChannel(t, map[string]any{
		"name":   "分割したまとめ",
		"kind":   notify.KindWeCom,
		"mode":   db.NotifyModeDigest,
		"config": map[string]any{"webhook": hook.URL},
	})
	const total = 60
	// タイトルを長めにし、60 件が 4096 バイトをはるかに超えて必ず分割されるようにする。
	longName := strings.Repeat("とても長い脆弱性の名前", 6)
	for i := 0; i < total; i++ {
		f.record(t, longName+strconv.Itoa(i+1), "high")
	}
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	f.agePendingBatch(t, chID)
	ch := f.channel(t, chID)

	f.n.stepDigest(ctx, ch, 50, "")
	if hook.count() != 1 {
		t.Fatalf("メッセージを1件だけ送るはずが、%d だった", hook.count())
	}

	var sent, pending int
	if err := f.pg.QueryRow(`SELECT
    count(*) FILTER (WHERE state=$2),
    count(*) FILTER (WHERE state=$3)
  FROM notification_deliveries WHERE channel_id=$1`, chID, db.NotifyStateSent, db.NotifyStatePending).
		Scan(&sent, &pending); err != nil {
		t.Fatal(err)
	}
	if sent == 0 {
		t.Fatal("届いた印の付いた項目があるはず")
	}
	if pending == 0 {
		t.Fatalf("一括 %d 件がすべて 4096 バイトに入ることはありえないので、送信待ちが残るはず；sent=%d", total, sent)
	}
	if sent+pending != total {
		t.Fatalf("項目の数が合わない：sent=%d pending=%d total=%d（届いてもおらず送信待ちでもない=失われた）", sent, pending, total)
	}
	// メッセージの本文は、このメッセージに含まれていない件数がまだいくつあるかをありのままに伝えなければならない。
	// 분절 안내는 markdown.go 가 "(이 메시지에는 앞 N건만 … 나머지 N건은 다음 메시지에서 …)" 로 렌더한다.
	if text := markdownText(t, hook.last(t)); !strings.Contains(text, "나머지") {
		t.Fatalf("メッセージはこのメッセージに含まれていない項目がまだあることを説明するはず:\n%.400s", text)
	}

	// 先送りした項目は再試行の予算を使ってはならない：取るときに attempts を楽観的に +1 しているので、先送りするときに戻す必要がある。
	var maxAttempts int
	if err := f.pg.QueryRow(`SELECT COALESCE(max(attempts),0) FROM notification_deliveries
WHERE channel_id=$1 AND state=$2`, chID, db.NotifyStatePending).Scan(&maxAttempts); err != nil {
		t.Fatal(err)
	}
	if maxAttempts > 0 {
		t.Fatalf("先送りした項目は再試行の回数を使うべきでない（さもないと数件後に失敗と判定される）のに、attempts=%d だった", maxAttempts)
	}

	// 収束するまで繰り返し走らせる。断言するのは**最後にすべて届いた**ことと、途中で本当に何ラウンドかに分かれたこと——
	// これは「2ラウンド目で送り終えた」より強い：分割が詰まらず、残りの項目を失わないことを証明する。
	rounds := 0
	for {
		var undelivered int
		if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries
WHERE channel_id=$1 AND state <> $2 AND state <> $3`, chID, db.NotifyStateSent, db.NotifyStateFailed).
			Scan(&undelivered); err != nil {
			t.Fatal(err)
		}
		if undelivered == 0 {
			break
		}
		rounds++
		if rounds > total+5 {
			t.Fatalf("分割した配信が収束しない：%d ラウンド走らせてもまだ %d 件が宙に浮いている", rounds, undelivered)
		}
		before := hook.count()
		f.n.stepDigest(ctx, ch, 50, "")
		if hook.count() == before {
			t.Fatalf("%d ラウンド目で何も進まなかったので、残りの %d 件は永久に詰まる", rounds, undelivered)
		}
	}
	if rounds < 2 {
		t.Fatalf("4096 バイトのメッセージ1件には長いタイトルの脆弱性 %d 件は入らないので、何ラウンドかに分けて送るはずが、実際は %d ラウンドしか使っていない", total, rounds)
	}
	// 最初のラウンドの後のどのラウンドも**純粋な続きの送信**のはずで、チャンネルに拒まれた項目はない。
	var failed int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1 AND state=$2`,
		chID, db.NotifyStateFailed).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if failed != 0 {
		t.Fatalf("偽の受け手は常に成功を返すので、失敗した項目はないはずが、%d だった", failed)
	}
}

// TestNotifyBackoffTableMatchesAttemptBudget はずれを防ぐ断言。
//
// 再試行の予算（db.MaxNotifyAttempts）とバックオフの列の表（notifyBackoff）は2つのパッケージに分かれている：
// 前者は状態機械の方針、後者はエンジンの実行のリズム。片方だけを変えると——例えば予算を 5
// 回に上げたのにバックオフの段を足し忘れると——コードはエラーにならず、4、5 回目の再試行が最後の段の間隔をそのまま使うだけで、
// 「再試行のリズムがなぜか遅くなる」と現れ、調べるときにここだと思いつくのは難しい。
// 2つの長さが一致することを断言し、こうしたずれを CI で明らかにする。
func TestNotifyBackoffTableMatchesAttemptBudget(t *testing.T) {
	if len(notifyBackoff) != db.MaxNotifyAttempts {
		t.Fatalf("バックオフの段の数(%d)と最大の試行回数(%d)が一致しない——片方を変えたらもう片方も同時に変えなければならない",
			len(notifyBackoff), db.MaxNotifyAttempts)
	}
	// バックオフの間隔は単調に減らないものでなければならない。さもないと再試行が試すほど急になり、かえってレート制限を悪化させる。
	for i := 1; i < len(notifyBackoff); i++ {
		if notifyBackoff[i] < notifyBackoff[i-1] {
			t.Fatalf("バックオフの間隔は単調に減らないものでなければならない：%d 段目 %v < %d 段目 %v",
				i, notifyBackoff[i], i-1, notifyBackoff[i-1])
		}
	}
}

// TestNotifyRateLimitDoesNotConsumeRetryBudget は「先にトークンを取ってから取る」の順序を固定する。
// 逆にすると（先に取ってから捨てる）、レート制限で止められた配信はすでに attempts を1回数えており、
// 予算は純粋な待ちで使い切られ、最後に failed になる。
func TestNotifyRateLimitDoesNotConsumeRetryBudget(t *testing.T) {
	// トークンバケットそのものだけをテストし、Server は要らない（そのために作るべきでもない）。
	n := &Notifier{buckets: map[int64]*notifyBucket{}}
	now := time.Now()
	// 1分あたり 1 件：満杯のとき最大 1 件。
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now); got != 1 {
		t.Fatalf("満杯のとき1分あたり 1 件ならトークンを 1 つ取るはずが、%d だった", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Millisecond)); got != 0 {
		t.Fatalf("トークンを使い切ったらすぐ 0 を返すはずが、%d だった", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(30*time.Second)); got != 0 {
		t.Fatalf("半分の時間ではトークン1つ分は補充されないはずが、%d だった", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Minute)); got != 1 {
		t.Fatalf("1周期が経てばトークンが 1 つ戻るはずが、%d だった", got)
	}
	// レート制限のないチャンネルは有限の上限を通り、1ラウンドが無限の溜まりに引き止められるのを避ける。
	if got := n.takeTokens(2, 0, notifyUnlimitedBurstPerTick+10, now); got != notifyUnlimitedBurstPerTick {
		t.Fatalf("レート制限なしならラウンドごとの上限 %d を返すはずが、%d だった", notifyUnlimitedBurstPerTick, got)
	}
	// チャンネルどうしのトークンバケットは互いに独立している。
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Millisecond)); got != 0 {
		t.Fatalf("チャンネル 1 のバケットはまだ空のはずが、%d だった", got)
	}
}

// TestNotifyTakeTokensKeepsUnusedTokens は「want 個だけを取る」の意味を固定する。
//
// 以前の実装はバケットを丸ごと空にしてから呼び出し元が切り詰めていたので、rate=100/min のチャンネルがバケットを満たしても、
// 1ラウンドで 5 件しか使わず、残りの 95 個のトークンはそのまま捨てられていた；チャンネルにそのラウンドの送信待ちの配信がないときも同じように差し引かれた。
// その結果、コメントがうたう「溜まったときに rate_per_min 件を一気に送れる」はどんな場合にもできなかった。
func TestNotifyTakeTokensKeepsUnusedTokens(t *testing.T) {
	n := &Notifier{buckets: map[int64]*notifyBucket{}}
	now := time.Now()
	// バケットは最初満杯（100）で、このラウンドは 5 個だけ欲しい。
	if got := n.takeTokens(1, 100, 5, now); got != 5 {
		t.Fatalf("want=5 ならちょうど 5 個のトークンを取るはずが、%d だった", got)
	}
	// 重要な断言：残りの 95 個はバケットに残っていなければならず、空にされて捨てられてはならない。
	// 時間を進めず、取れたものが補充ではなく手元の分からしか来ないようにする。
	if got := n.takeTokens(1, 100, 95, now); got != 95 {
		t.Fatalf("残りのトークンはまだ取れるはず（95 を期待）が、%d だった——バケットがラウンドまるごと空にされた", got)
	}
	if got := n.takeTokens(1, 100, 1, now); got != 0 {
		t.Fatalf("バケットはもう取り尽くしたので、0 を返すはずが、%d だった", got)
	}
	// want<=0 ではトークンを1つも差し引くべきでない（空のラウンドは課金しない）。
	n2 := &Notifier{buckets: map[int64]*notifyBucket{}}
	if got := n2.takeTokens(1, 20, 0, now); got != 0 {
		t.Fatalf("want=0 なら 0 を返すはずが、%d だった", got)
	}
	if got := n2.takeTokens(1, 20, 20, now); got != 20 {
		t.Fatalf("want=0 の回はトークンを使うべきでないので、まだ 20 を取り切れるはずが、%d だった", got)
	}
}

// TestDigestTickPlanDecouplesBatchSizeFromSendBudget はまとめのモードの2つの次元を固定する。
//
// まとめの一括の大きさをラウンドごとのリクエストの予算に結びつけると、rate_per_min=20 のチャンネルは各
// 3 秒の tick でトークンが 1 つしか補充されず、まとめのメッセージ1件に脆弱性が1件しか入らない——機能としてはまとめが
// ないのと同じで、しかもメッセージの頭には「直近 30 分で脆弱性が 1 件増えた」と書かれる。この退化はエラーにならず、
// 既存の端から端までのケースでも見えない（それらは手で十分に大きな limit を stepDigest に渡し、
// step の中の枠の計算を迂回している）ので、ここで判断そのものを直接断言する。
func TestDigestTickPlanDecouplesBatchSizeFromSendBudget(t *testing.T) {
	tokens, claimLimit := digestTickPlan()
	// 一括 = メッセージ1件 = リクエスト1回 = トークン1つ。トークンの単位はメッセージで、脆弱性ではない。
	if tokens != 1 {
		t.Fatalf("まとめの一括はメッセージを1件だけ送るので、ちょうど 1 つのトークンを使うはずが、%d だった", tokens)
	}
	if claimLimit != db.MaxDigestBatchSize {
		t.Fatalf("まとめの一括の大きさはメモリの上限 db.MaxDigestBatchSize=%d のはずが、%d だった",
			db.MaxDigestBatchSize, claimLimit)
	}
	// 重要な関係：一括の大きさはラウンドごとのリクエストの予算よりはるかに大きくなければならない。2つが同じ桁になったら、
	// 「メッセージを何件送るか」と「一括に脆弱性を何件入れるか」をまた1つの数に混ぜたことを表す。
	if claimLimit <= notifyMaxSendsPerChannelPerTick {
		t.Fatalf("まとめの一括の大きさ %d はラウンドごとのリクエストの予算 %d に縛られるべきでない——"+
			"リクエストの予算はリースから逆算した「何回リクエストを送るか」で、「一括に脆弱性を何件入れるか」とは別の次元",
			claimLimit, notifyMaxSendsPerChannelPerTick)
	}
}

// TestNotifyTickBudgetFitsWithinLease はもう1つのずれを防ぐ断言。
//
// チャンネルごとの1ラウンドの配信の件数の上限（notifyMaxSendsPerChannelPerTick）はリースの長さから逆算している：
// 1ラウンドで順に配信する最悪の所要時間は < リースでなければならない。さもないと後ろの数件は送り終わる前にリースが切れ、
// 複数のインスタンスで動かすと相手がそれらを取り直して重ねて送る。この3つの定数は別々の場所にあり、
// どれを変えても関係を壊しうるのに何のエラーも出ない——だからここで固定する。
func TestNotifyTickBudgetFitsWithinLease(t *testing.T) {
	worst := time.Duration(notifyMaxSendsPerChannelPerTick) * notifySendTimeout
	if worst >= notifyLease {
		t.Fatalf("チャンネルごとの1ラウンドの最悪の所要時間 %v はリース %v に達したり超えたりしてはならない"+
			"（notifyMaxSendsPerChannelPerTick=%d × notifySendTimeout=%v）——"+
			"この3つの定数のどれを変えても、他の2つも合わせて確かめること",
			worst, notifyLease, notifyMaxSendsPerChannelPerTick, notifySendTimeout)
	}
}
