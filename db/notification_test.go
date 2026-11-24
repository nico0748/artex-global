package db

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Autumn-27/artex/notify"
)

// このファイルのケースはどれも本当に PostgreSQL につなぐ（DB がなければ飛ばす）。これらの SQL は
// FOR UPDATE SKIP LOCKED、make_interval、JSONB、複数行の IN(...) の場所取りの組み立てを使っており、
// どれも「コンパイルは通るが実行時にエラーになりうる」書き方なので、実際に走らせて初めて確かめたことになる。

func notifyTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// newTestChannel はチャンネルを1つ作り、テストの終わりに自動で消す。
func newTestChannel(t *testing.T, d *DB, kind, mode string, filter string) *NotificationChannel {
	t.Helper()
	if filter == "" {
		filter = `{}`
	}
	ch := &NotificationChannel{
		Name:       "テストのチャンネル-" + t.Name(),
		Kind:       kind,
		Mode:       mode,
		Config:     json.RawMessage(`{"webhook":"https://example.com/hook"}`),
		Filter:     json.RawMessage(filter),
		RatePerMin: 100,
	}
	id, err := d.SaveNotificationChannel(context.Background(), ch)
	if err != nil {
		t.Fatalf("チャンネルを作れなかった: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })
	ch.ID = id
	return ch
}

// addTestEvent はイベントを1件直接書き込み（finding を通さない）、振り分けと配信のテストに使う。
func addTestEvent(t *testing.T, d *DB, kind string, findingID int64, snap notify.Snapshot) int64 {
	t.Helper()
	snap.Kind = kind
	snap.FindingID = findingID
	id, err := d.AddNotificationEvent(context.Background(), kind, findingID, snap)
	if err != nil {
		t.Fatalf("イベントを書けなかった: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE id=$1`, id) })
	return id
}

func TestNotificationAssetNamesResolvesAndPreservesOrder(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// 3種類の資産はそれぞれ表示の基準が違う：ドメイン、IP、URL。
	insertAsset := func(query, value string) int64 {
		t.Helper()
		var id int64
		if err := d.QueryRow(query, value).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	domID := insertAsset(`INSERT INTO assets(type, domain) VALUES('subdomain',$1) RETURNING id`, "a.example.com")
	ipID := insertAsset(`INSERT INTO assets(type, ip) VALUES('ip',$1) RETURNING id`, "10.1.2.3")
	svcID := insertAsset(`INSERT INTO assets(type, url) VALUES('service',$1) RETURNING id`, "https://a.example.com/admin")
	t.Cleanup(func() {
		d.Exec(`DELETE FROM assets WHERE id IN ($1,$2,$3)`, domID, ipID, svcID)
	})

	// 渡す順序はわざとばらばらにし、存在しない id を1つ含める。
	got, err := d.NotificationAssetNames(ctx, []int64{svcID, 999999999, domID, ipID, svcID})
	if err != nil {
		t.Fatalf("資産名を解決できなかった: %v", err)
	}
	want := []string{"https://a.example.com/admin", "a.example.com", "10.1.2.3"}
	if len(got) != len(want) {
		t.Fatalf("資産名の数が合わない。期待 %v 実際 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("順序/値が合わない。期待 %v 実際 %v", want, got)
		}
	}
}

// TestRecordNotificationEventTxUnwindsOnFailure は保存点の仕組みの核心のケース：
// トランザクションの中で notification_events の書き込みが必ず失敗するようにし（常に false の制約を一時的に足す）、
// ① この関数が false を返す ② トランザクションが aborted の状態に入らず、後の文がまだ実行できる、を断言する。
//
// 保存点がなければ、PostgreSQL はトランザクション全体を無効にし、後のどの文も
// "current transaction is aborted" で失敗する——まさに「通知のテーブルの問題で
// 脆弱性を保存できなくなる」故障の経路。
//
// ここではわざと **COMMIT ではなく ROLLBACK で締める**：ALTER TABLE は PG ではトランザクションの一部なので、
// いったんコミットすると、その一時的な制約が schema に永久に残り、後のすべてのケースをまとめて落とす。
// ロールバックすれば DDL は自動で取り消され、手で片づける必要はない。断言に要るのは「トランザクションがまだ生きている」ことだけで、
// 本当にコミットする必要はない。
func TestRecordNotificationEventTxUnwindsOnFailure(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// 守りの片づけ：過去の実行がこの制約を残していたら、先に外す。
	if _, err := d.Exec(`ALTER TABLE notification_events DROP CONSTRAINT IF EXISTS notify_test_never`); err != nil {
		t.Fatal(err)
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck // 一時的な制約を取り消す。関数の注釈を参照

	// NOT VALID：これから書き込む行だけを制約し、DB にある過去のイベントは確かめない
	// （さもないと既存の行の違反で制約を足せない）。
	if _, err := tx.ExecContext(ctx, `ALTER TABLE notification_events ADD CONSTRAINT notify_test_never CHECK (false) NOT VALID`); err != nil {
		t.Fatalf("一時的な制約を足せなかった: %v", err)
	}
	if RecordNotificationEventTx(ctx, tx, notify.EventFindingCreated, 1, notify.Snapshot{Severity: "high"}) {
		t.Fatal("必ず失敗する制約のもとで、まだ書き込みの成功を報告した")
	}
	// 肝心の断言：トランザクションがまだ使える。
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("トランザクションが汚された（保存点が効いていない）: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("ロールバックに失敗: %v", err)
	}
	// DDL がロールバックとともに取り消されたことを確かめ、後のケースに地雷を残さない。
	var exists bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conname='notify_test_never')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("一時的な制約がロールバックで取り消されておらず、後のケースを汚す")
	}
}

func TestFanOutRoutesEventsByFilter(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	all := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	onlyCritical := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"min_severity":"critical"}`)
	sqlOnly := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"vulnclass_include":["SQL"]}`)

	highSQL := addTestEvent(t, d, notify.EventFindingCreated, 1001, notify.Snapshot{Severity: "high", VulnClass: "SQLインジェクション"})
	lowXSS := addTestEvent(t, d, notify.EventFindingCreated, 1002, notify.Snapshot{Severity: "low", VulnClass: "XSS"})
	criticalXSS := addTestEvent(t, d, notify.EventFindingCreated, 1003, notify.Snapshot{Severity: "critical", VulnClass: "XSS"})

	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatalf("振り分けに失敗: %v", err)
	}

	cases := []struct {
		name    string
		eventID int64
		channel int64
		want    bool
	}{
		{"すべて受けるチャンネルが high を受け取る", highSQL, all.ID, true},
		{"すべて受けるチャンネルが low を受け取る", lowXSS, all.ID, true},
		{"緊急だけのチャンネルは high を飛ばす", highSQL, onlyCritical.ID, false},
		{"緊急だけのチャンネルが critical を受け取る", criticalXSS, onlyCritical.ID, true},
		{"SQL だけのチャンネルが SQL を受け取る", highSQL, sqlOnly.ID, true},
		{"SQL だけのチャンネルは XSS を飛ばす", lowXSS, sqlOnly.ID, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var exists bool
			if err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notification_deliveries WHERE event_id=$1 AND channel_id=$2)`,
				tc.eventID, tc.channel).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if exists != tc.want {
				t.Fatalf("配信があるか: 期待 %v 実際 %v", tc.want, exists)
			}
		})
	}

	// もう一度振り分けても重複した配信を生むべきでない（fanned_out の冪等）。
	events, deliveries, err := d.FanOutPendingEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if events != 0 || deliveries != 0 {
		t.Fatalf("振り分け済みのイベントはもう処理されるべきでないのに、events=%d deliveries=%d だった", events, deliveries)
	}
}

// TestFanOutMarksEventsWithNoMatchingChannel は「イベントがどのチャンネルにも当たらない」場合を覆う。
// この種のイベントも振り分け済みの印を付けなければならない。さもないと振り分け待ちの集合に永遠に残り、tick のたびに見直される。
func TestFanOutMarksEventsWithNoMatchingChannel(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	pick := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"vulnclass_include":["決して当たらない種類"]}`)
	_ = pick

	ev := addTestEvent(t, d, notify.EventFindingCreated, 2001, notify.Snapshot{Severity: "high", VulnClass: "XSS"})
	_, deliveries, err := d.FanOutPendingEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if deliveries != 0 {
		t.Fatalf("配信を生むべきでないのに、%d 件あった", deliveries)
	}
	var fanned bool
	if err := d.QueryRowContext(ctx, `SELECT fanned_out FROM notification_events WHERE id=$1`, ev).Scan(&fanned); err != nil {
		t.Fatal(err)
	}
	if !fanned {
		t.Fatal("チャンネルに当たらなかったイベントも振り分け済みの印を付けなければならない。さもないと無限に見直される")
	}
}

func TestClaimRealtimeDeliveriesHonorsLeaseAndMode(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	realtime := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	digest := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)

	addTestEvent(t, d, notify.EventFindingCreated, 3001, notify.Snapshot{Severity: "high", VulnClass: "XSS"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}

	// リアルタイムの取得は realtime のチャンネルの1件だけを取り、digest のチャンネルのものには触れないはず。
	got, err := d.ClaimRealtimeDeliveries(ctx, realtime.ID, 10, time.Minute)
	if err != nil {
		t.Fatalf("取得に失敗: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("1 件取るはずが、%d 件だった", len(got))
	}
	if got[0].State != NotifyStateSending || got[0].Attempts != 1 {
		t.Fatalf("取った後は sending で attempts=1 のはずが、state=%s attempts=%d だった", got[0].State, got[0].Attempts)
	}
	// 合わせて読み込む描画の文脈はそろっていなければならない（チャンネルの設定 + イベントのスナップショット + finding id）。
	if got[0].Channel == nil || len(got[0].Channel.Config) == 0 {
		t.Fatal("取得の結果にチャンネルの設定がなく、描画が失敗する")
	}
	if got[0].FindingID != 3001 {
		t.Fatalf("finding id がイベントから持ってこられていない。%d だった", got[0].FindingID)
	}

	// リースの期限前なので、2回目の取得は空のはず——これが「同じ行を2つの dispatcher が同時に配信しない」
	// ことの保証。
	again, err := d.ClaimRealtimeDeliveries(ctx, realtime.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("リースの期間内に重ねて取るべきでないのに、%d 件取った", len(again))
	}

	// digest のチャンネルの配信はリアルタイムの取得に触れられるべきでない。
	left, err := d.ClaimRealtimeDeliveries(ctx, digest.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("リアルタイムの取得は digest のチャンネルの配信を取るべきでないのに、%d 件取った", len(left))
	}
}

// TestClaimExpiredLeaseRecovers は落ちたときの自然な回復を覆う：配信の途中でプロセスが落ちると sending の
// 行が残り、リースの期限が切れたら取り直せなければならない。さもないとその配信は永遠に詰まる。
func TestClaimExpiredLeaseRecovers(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 4001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	first, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil || len(first) != 1 {
		t.Fatalf("初回の取得に失敗: %v (%d 件)", err, len(first))
	}
	// リースを手で過去に進め、「リースの期限切れ」をまねる。
	if _, err := d.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE id=$1`, first[0].ID); err != nil {
		t.Fatal(err)
	}
	second, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 {
		t.Fatalf("リースの期限が切れた sending の行は取り直せるはずが、%d 件だった", len(second))
	}
	if second[0].Attempts != 2 {
		t.Fatalf("取り直しは試行の回数を足すはずが、%d だった", second[0].Attempts)
	}
}

func TestClaimSkipsDisabledChannel(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 5001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	// 停止すると既存の送信待ちの配信もまとめて skipped になる。
	if err := d.SetNotificationChannelEnabled(ctx, ch.ID, false); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := d.QueryRow(`SELECT state FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStateSkipped {
		t.Fatalf("停止したチャンネルの既存の送信待ちの配信は skipped になるはずが、%s だった", state)
	}
	got, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("停止したチャンネルは取れないはずが、%d 件取れた", len(got))
	}
}

func TestDigestBatchDueAndStableBatchID(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)
	for i := 0; i < 3; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(6000+i), notify.Snapshot{Severity: "high"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}

	// 一括は作ったばかりで経過時間は 0、30 分の周期では期限が来ないはず。
	due, err := d.DigestBatchDue(ctx, ch.ID, 30*time.Minute)
	if err != nil {
		t.Fatalf("一括の期限の判定に失敗: %v", err)
	}
	if due {
		t.Fatal("作ったばかりの一括はすぐに期限が来るべきでない")
	}

	// 3件の配信の作成時刻をまとめて古くし、周期分溜まった一括をまねる。
	if _, err := d.Exec(`UPDATE notification_deliveries SET created_at = now() - interval '40 minutes' WHERE channel_id=$1`, ch.ID); err != nil {
		t.Fatal(err)
	}
	due, err = d.DigestBatchDue(ctx, ch.ID, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !due {
		t.Fatal("周期を超えた一括は期限が来たと判定されるはず")
	}

	batch, err := d.ClaimDigestBatch(ctx, ch.ID, MaxDigestBatchSize, time.Minute)
	if err != nil {
		t.Fatalf("まとめの一括の取得に失敗: %v", err)
	}
	if len(batch) != 3 {
		t.Fatalf("まとめは一度に 3 件すべてを取るはずが、%d 件だった", len(batch))
	}
	if batch[0].BatchID == nil {
		t.Fatal("まとめの一括は batch_id を書かなければならない。さもないと履歴でこれらが一緒に送られたとわからない")
	}
	firstBatchID := *batch[0].BatchID
	for _, dl := range batch {
		if dl.BatchID == nil || *dl.BatchID != firstBatchID {
			t.Fatalf("同じ一括は batch_id を共有するはずが、%v vs %d だった", dl.BatchID, firstBatchID)
		}
	}

	// この一括を**まるごと**失敗させて並べ直してから取ると、batch_id は元の値を保たなければならない（COALESCE の働き）：
	// さもないと1回の再試行で「この一括は一緒に送った」という事実が消える。
	//
	// 1件だけでなく一括まるごとを並べ直さなければならない——配信のエンジンはまとめのメッセージを送るときにまさにそう扱う
	// （メッセージ1件が一括全体を表し、成否をともにする）。1件だけを並べ直すと、ほかはまだリースの期間内なので、
	// 取り直しは当然その1件しか取れない。
	allIDs := make([]int64, 0, len(batch))
	for _, dl := range batch {
		allIDs = append(allIDs, dl.ID)
	}
	if err := d.RescheduleDeliveries(ctx, allIDs, time.Second, "失敗をまねる"); err != nil {
		t.Fatal(err)
	}
	// リースを過去に進め、バックオフの時間が来たことをまねる。
	if _, err := d.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE channel_id=$1`, ch.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := d.ClaimDigestBatch(ctx, ch.ID, MaxDigestBatchSize, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 3 {
		t.Fatalf("取り直しは 3 件すべてを取るはずが、%d だった", len(reclaimed))
	}
	if reclaimed[0].BatchID == nil || *reclaimed[0].BatchID != firstBatchID {
		t.Fatalf("再試行の後も batch_id は元の値 %d を保つはずが、%v だった", firstBatchID, reclaimed[0].BatchID)
	}
}

func TestDeliveryStateTransitions(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 7001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	got, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil || len(got) != 1 {
		t.Fatalf("取得に失敗: %v (%d)", err, len(got))
	}
	id := got[0].ID

	if err := d.RescheduleDeliveries(ctx, []int64{id}, time.Second, "ネットワークの揺らぎ"); err != nil {
		t.Fatal(err)
	}
	var state, lastErr string
	if err := d.QueryRow(`SELECT state, last_error FROM notification_deliveries WHERE id=$1`, id).Scan(&state, &lastErr); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStatePending || lastErr != "ネットワークの揺らぎ" {
		t.Fatalf("並べ直した後は pending で理由を記録するはずが、state=%s err=%q だった", state, lastErr)
	}

	if err := d.FailDeliveries(ctx, []int64{id}, "再試行を使い切った"); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT state FROM notification_deliveries WHERE id=$1`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStateFailed {
		t.Fatalf("failed のはずが、%s だった", state)
	}

	// 手動の送り直しは再試行の数を 0 にしてすぐ期限の来た状態にしなければならない。さもないと古い失敗の予算を引き継ぐ。
	if err := d.RetryNotificationDelivery(ctx, id); err != nil {
		t.Fatalf("送り直しに失敗: %v", err)
	}
	var attempts int
	var next time.Time
	if err := d.QueryRow(`SELECT state, attempts, next_attempt_at FROM notification_deliveries WHERE id=$1`, id).Scan(&state, &attempts, &next); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStatePending || attempts != 0 {
		t.Fatalf("送り直した後は pending で attempts=0 のはずが、state=%s attempts=%d だった", state, attempts)
	}
	if next.After(time.Now().Add(time.Second)) {
		t.Fatal("送り直したらすぐ取れるはず")
	}

	// 届いた配信は送り直せないはず。
	if err := d.MarkDeliveriesSent(ctx, []int64{id}); err != nil {
		t.Fatal(err)
	}
	if err := d.RetryNotificationDelivery(ctx, id); err == nil {
		t.Fatal("届いた配信の送り直しを許すべきでない")
	}
}

func TestListNotificationDeliveriesPagingAndFilter(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	for i := 0; i < 5; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(8000+i), notify.Snapshot{Severity: "high", Name: "ページ分けのテスト"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute); err != nil {
		t.Fatal(err)
	}

	page1, total, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStateSending}, 1, 2)
	if err != nil {
		t.Fatalf("問い合わせに失敗: %v", err)
	}
	if total != 5 {
		t.Fatalf("総数は 5 のはずが、%d だった", total)
	}
	if len(page1) != 2 {
		t.Fatalf("1ページ 2 件のはずが、%d だった", len(page1))
	}
	// 新しいものが前：1ページ目の先頭の id は2ページ目の先頭より大きいはず。
	page2, _, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStateSending}, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 2 || page2[0].ID >= page1[0].ID {
		t.Fatalf("ページの順序は新しいものが前のはずが、page1[0]=%d page2[0]=%d だった", page1[0].ID, page2[0].ID)
	}
	// 描画の文脈は履歴と一緒に返さなければならない。さもないと一覧が「何を送ったか」を表示できない。
	if page1[0].ChannelName == "" || page1[0].FindingID == 0 {
		t.Fatalf("履歴の項目に表示のフィールドがない: %+v", page1[0])
	}

	// 状態で絞り込む：pending のものはない。
	pending, totalPending, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStatePending}, 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	if totalPending != 0 || len(pending) != 0 {
		t.Fatalf("pending の配信はないはずが、%d 件あった (total=%d)", len(pending), totalPending)
	}
}

func TestSetFindingStatusWithNotifyOnlyEmitsOnRealChange(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	tk, err := d.CreateTask("通知の状態の変更のテスト", "目標", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	es := d.Exploration(tk.ExplorationID)
	f, err := es.RecordFinding(ctx, RecordFindingInput{
		TaskID: tk.ID, Worker: "test", VulnClass: "SQLインジェクション", Name: "状態の変更のケース",
		Severity: "high", Summary: "要約",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE finding_id=$1`, f.FindingID) })

	// 保存したときに finding_created のイベントが1件登録されているので、まずそれを数えて基準にする。
	var base int
	if err := d.QueryRow(`SELECT count(*) FROM notification_events WHERE finding_id=$1`, f.FindingID).Scan(&base); err != nil {
		t.Fatal(err)
	}
	if base < 1 {
		t.Fatal("脆弱性の保存は同じトランザクションで通知のイベントを1件登録するはず")
	}

	// 同じ状態に変える：イベントを生むべきでない（重ねて送って通知の雑音を出さないため）。
	from, found, notified, err := d.SetFindingStatusWithNotify(ctx, f.FindingID, "pending")
	if err != nil || !found {
		t.Fatalf("状態を設定できなかった: found=%v err=%v", found, err)
	}
	if notified {
		t.Fatal("状態が変わらないときは通知のイベントを登録すべきでない")
	}
	if from != "pending" {
		t.Fatalf("変更前の状態 pending を返すはずが、%q だった", from)
	}

	// 本当の変更：イベントを登録し、from/to を記録するはず。
	from, found, notified, err = d.SetFindingStatusWithNotify(ctx, f.FindingID, "fixed")
	if err != nil || !found {
		t.Fatalf("状態を設定できなかった: found=%v err=%v", found, err)
	}
	if !notified {
		t.Fatal("状態が実際に変わったときは通知のイベントを登録するはず")
	}
	if from != "pending" {
		t.Fatalf("from は pending のはずが、%q だった", from)
	}
	var snapshot []byte
	if err := d.QueryRow(`SELECT snapshot FROM notification_events WHERE finding_id=$1 AND kind=$2`,
		f.FindingID, notify.EventFindingStatusChanged).Scan(&snapshot); err != nil {
		t.Fatalf("状態の変更のイベントが見つからない: %v", err)
	}
	var snap notify.Snapshot
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.FromStatus != "pending" || snap.ToStatus != "fixed" {
		t.Fatalf("スナップショットの状態の遷移が違う: %s → %s", snap.FromStatus, snap.ToStatus)
	}
	// スナップショットは描画に要るフィールドを持たなければならない。さもないと状態の変更のメッセージが空の殻になる。
	if snap.VulnClass != "SQLインジェクション" || snap.Severity != "high" || snap.Name != "状態の変更のケース" {
		t.Fatalf("スナップショットに描画のフィールドがない: %+v", snap)
	}
	var status string
	if err := d.QueryRow(`SELECT status FROM findings WHERE id=$1`, f.FindingID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "fixed" {
		t.Fatalf("状態は fixed に更新されたはずが、%s だった", status)
	}

	// 存在しない脆弱性：found=false で、エラーにしない。
	if _, found, _, err := d.SetFindingStatusWithNotify(ctx, 999999999, "fixed"); err != nil || found {
		t.Fatalf("存在しない脆弱性は found=false でエラーなしを返すはずが、found=%v err=%v だった", found, err)
	}
}

func TestNotificationStatsSnapshot(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 9001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	stats, err := d.NotificationStatsSnapshot(ctx)
	if err != nil {
		t.Fatalf("集計に失敗: %v", err)
	}
	if stats.Channels < 1 || stats.ChannelsOn < 1 {
		t.Fatalf("チャンネルの数が違う: %+v", stats)
	}
	if stats.Pending < 1 {
		t.Fatalf("送信待ちの配信を数えるはず: %+v", stats)
	}
	// 作ったばかりの配信の溜まりの経過時間は 0 に近いはずで、負の値や巨大な値ではない。
	if stats.BacklogAgeMS < 0 || stats.BacklogAgeMS > int64(time.Hour/time.Millisecond) {
		t.Fatalf("溜まりの経過時間がおかしい: %d ms", stats.BacklogAgeMS)
	}
	_ = ch
}

func TestNotificationChannelCRUDRoundTrip(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	ch := &NotificationChannel{
		Name:       "CRUD の往復",
		Kind:       notify.KindEmail,
		Mode:       NotifyModeDigest,
		Config:     json.RawMessage(`{"host":"smtp.example.com","port":587,"from":"a@b.c","to":["x@y.z"]}`),
		Filter:     json.RawMessage(`{"min_severity":"medium","on_status_change":true}`),
		RatePerMin: 42,
	}
	id, err := d.SaveNotificationChannel(ctx, ch)
	if err != nil {
		t.Fatalf("作成に失敗: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })

	got, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatalf("読み取りに失敗: %v", err)
	}
	if got.Mode != NotifyModeDigest || got.RatePerMin != 42 || got.Name != "CRUD の往復" {
		t.Fatalf("往復のフィールドが一致しない: %+v", got)
	}
	if !got.IsEnabled() {
		t.Fatal("既定は有効のはず")
	}
	var cfg map[string]any
	if err := json.Unmarshal(got.Config, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["host"] != "smtp.example.com" {
		t.Fatalf("設定が正しく保存されていない: %v", cfg)
	}
	var filter notify.Filter
	if err := json.Unmarshal(got.Filter, &filter); err != nil {
		t.Fatal(err)
	}
	if filter.MinSeverity != "medium" || !filter.OnStatusChange {
		t.Fatalf("絞り込みの条件が正しく保存されていない: %+v", filter)
	}

	// 更新してから読む。
	got.Name = "名前を変えた"
	off := false
	got.Enabled = &off
	if _, err := d.SaveNotificationChannel(ctx, got); err != nil {
		t.Fatal(err)
	}
	after, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != "名前を変えた" || after.IsEnabled() {
		t.Fatalf("更新が効いていない: %+v", after)
	}

	// 削除した後は黙って成功せず「存在しない」と報告するはず。
	if err := d.DeleteNotificationChannel(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := d.NotificationChannelByID(ctx, id); err != ErrNotificationChannelNotFound {
		t.Fatalf("ErrNotificationChannelNotFound を期待したが、%v だった", err)
	}
	if err := d.DeleteNotificationChannel(ctx, id); err != ErrNotificationChannelNotFound {
		t.Fatalf("重ねて削除したら存在しないと報告するはずが、%v だった", err)
	}
}

// TestSaveNotificationChannelKeepsExplicitZeroRate は、かつて書き誤った箇所を固定する：
// **0 は正しい設定で、意味は「レート制限なし」。db の層が「指定なし」とみなして既定値で上書きしてはならない**。
//
// 過去のバグ：SaveNotificationChannel に `if RatePerMin <= 0 { 既定値を取る }` と書いてあり、
// 文書、UI の案内、takeTokens はどれも「0=レート制限なし」と解釈するのに、DB に書くこの層だけが黙って
// 20（DingTalk/WeCom/Telegram）か 100（Feishu）に変えていた——運用者はレート制限を外したつもりなのに実際は止められ、
// しかも何の知らせもなかった。「指定なし」と「明示的な 0」の違いはリクエストの本文でしか表せないので、
// 既定値は server の層で入れ（notifyCreateChannel を参照）、db の層は保存するだけ。
func TestSaveNotificationChannelKeepsExplicitZeroRate(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// 明示的な 0（レート制限なし）：そのまま保存しなければならない。
	unlimited := &NotificationChannel{
		Name: "レート制限なし", Kind: notify.KindDingTalk, RatePerMin: 0,
		Config: json.RawMessage(`{"webhook":"https://example.com/h"}`),
	}
	id, err := d.SaveNotificationChannel(ctx, unlimited)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })
	got, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.RatePerMin != 0 {
		t.Fatalf("明示的な 0 はレート制限なしを表すので、そのまま保存しなければならないのに、%d だった", got.RatePerMin)
	}
	if got.Mode != NotifyModeRealtime {
		t.Fatalf("既定のモードは realtime のはずが、%s だった", got.Mode)
	}

	// 負の値は不正な入力なので、黙って別の値に変えず拒むはず。
	bad := &NotificationChannel{
		Name: "負のレート制限", Kind: notify.KindDingTalk, RatePerMin: -1,
		Config: json.RawMessage(`{"webhook":"https://example.com/h"}`),
	}
	if _, err := d.SaveNotificationChannel(ctx, bad); err == nil {
		t.Fatal("負のレート制限は拒まれるはず")
	}
}

// TestDeleteChannelCascadesDeliveries は外部キーの挙動を固定する：チャンネルを消すとその配信の履歴もまとめて消える
// （設定がなくなれば履歴は読み解けない）が、イベントそのものは残す——ほかのチャンネルがまだ参照しているかもしれない。
func TestDeleteChannelCascadesDeliveries(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	ev := addTestEvent(t, d, notify.EventFindingCreated, 9101, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := d.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before == 0 {
		t.Fatal("前提が成り立たない：配信が生まれていない")
	}
	if err := d.DeleteNotificationChannel(ctx, ch.ID); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := d.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != 0 {
		t.Fatalf("チャンネルを消した後はその配信も連鎖して消えるはずが、まだ %d 件ある", after)
	}
	var evExists bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM notification_events WHERE id=$1)`, ev).Scan(&evExists); err != nil {
		t.Fatal(err)
	}
	if !evExists {
		t.Fatal("チャンネルを消してもイベントそのものまで消すべきでない")
	}
}

// TestClaimDigestBatchHonorsCallerLimit は監査が指摘した抜け穴を覆う：
// まとめのチャンネルは以前トークンバケットを完全に迂回していた——allow は takeTokens に差し引かれたのに誰も使わず、
// rate_per_min は digest のモードにまったく効かなかった。いまは limit も制約に加わる。
func TestClaimDigestBatchHonorsCallerLimit(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)
	for i := 0; i < 10; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(7000+i), notify.Snapshot{Severity: "high"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	// limit=3 にする：3 件しか取れず、残りは DB に残る。
	got, err := d.ClaimDigestBatch(ctx, ch.ID, 3, time.Minute)
	if err != nil {
		t.Fatalf("取得に失敗: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("呼び出し側のレート制限の枠どおり 3 件だけ取るはずが、%d 件だった", len(got))
	}
	// limit=0 はこのラウンドの枠を使い切ったことを表す：1件も取らず、エラーにもしないはず。
	if got, err := d.ClaimDigestBatch(ctx, ch.ID, 0, time.Minute); err != nil || len(got) != 0 {
		t.Fatalf("枠が 0 のときは 0 件取ってエラーにしないはずが、%d 件 err=%v だった", len(got), err)
	}
}

// TestFinishFindingRetestEmitsStatusChange は監査が指摘した完全性の欠けを覆う：
// 再テストの結論が「修正済み」のとき、状態は確かに変わったが、その UPDATE は直接 DB に書いていて、
// 通知付きの版を迂回していた——そのため on_status_change を設定したチャンネルはこの種の状態の遷移の
// 通知をまったく受け取れず、画面では状態が黙って変わり、運用者はプラットフォームを開いて初めて知った。
//
// このケースは「状態を変えるすべての経路が状態の変更のイベントを登録しなければならない」を固定する。
func TestFinishFindingRetestEmitsStatusChange(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	tk, err := d.CreateTask("再テストの通知のテスト", "目標", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	es := d.Exploration(tk.ExplorationID)
	f, err := es.RecordFinding(ctx, RecordFindingInput{
		TaskID: tk.ID, Worker: "test", VulnClass: "SQLインジェクション", Name: "再テストの対象",
		Severity: "high", Summary: "要約",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE finding_id=$1`, f.FindingID) })

	// 再テストの記録を1件作り、そのまま完了の状態まで進める。
	rt, _, _, err := d.CreateFindingRetest(ctx, f.FindingID, "再確認")
	if err != nil {
		t.Fatal(err)
	}
	if rt.ConversationID == nil {
		t.Fatal("再テストは会話を1つ関連づけるはず")
	}
	// 再テストは先に running に入ってからでないと結論を保存できない（本物の流れと同じ）。
	if ok, err := d.StartFindingRetest(ctx, rt.ID); err != nil || !ok {
		t.Fatalf("再テストを始められなかった: ok=%v err=%v", ok, err)
	}
	if err := d.RecordFindingRetestResult(ctx, *rt.ConversationID, "fixed", "修正済み", "証拠"); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishFindingRetest(rt.ID, "completed", ""); err != nil {
		t.Fatalf("再テストを終えられなかった: %v", err)
	}

	var status string
	if err := d.QueryRow(`SELECT status FROM findings WHERE id=$1`, f.FindingID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != FindingFixed {
		t.Fatalf("再テストが修正済みと判定した後の状態は fixed のはずが、%s だった", status)
	}

	// 肝心の断言：状態の変更のイベントが1件あり、from/to が正しくなければならない。
	var snapshot []byte
	err = d.QueryRow(`SELECT snapshot FROM notification_events WHERE finding_id=$1 AND kind=$2 ORDER BY id DESC LIMIT 1`,
		f.FindingID, notify.EventFindingStatusChanged).Scan(&snapshot)
	if err != nil {
		t.Fatalf("再テストが修正済みと判定したら状態の変更の通知のイベントを登録するはず（さもないと on_status_change を設定したチャンネルが受け取れない）: %v", err)
	}
	var snap notify.Snapshot
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.FromStatus != "pending" || snap.ToStatus != FindingFixed {
		t.Fatalf("スナップショットの状態の遷移が違う: %s → %s", snap.FromStatus, snap.ToStatus)
	}
	// スナップショットは描画に要るフィールドを持たなければならない。さもないと送られるのは空の殻になる。
	if snap.Name != "再テストの対象" || snap.Severity != "high" {
		t.Fatalf("スナップショットに描画のフィールドがない: %+v", snap)
	}
}
