package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// このファイルは配信の仕事の取得と状態の遷移。
//
// 取得には長いトランザクションではなく「リース」を使う：行を sending にし、next_attempt_at を未来に進めて
// リースの期限とし、トランザクションをコミットしてからネットワークでの配信をする。こうすれば配信の間に DB のロックを持たない——
// ネットワークのリクエストは数秒かかることがあり（クライアントのタイムアウトは 15 秒）、行のロックを握ったままだと同じ DB のほかの書き込みを引きずり倒す。
//
// 代償は、配信の途中でプロセスが落ちると行が sending のまま止まること。これは**自然に治る**：リースの期限が切れると
// next_attempt_at が過去になり、次のラウンドの取得が同じ行を拾い直す（取得の条件の
// state IN ('pending','sending') を参照）。再試行の数は取得のときにもう +1 しているので、落ちても
// 無限の再試行にはならない——MaxNotifyAttempts 回の機会を使い切ったら failed になり、人の対応を待つ。

// MaxNotifyAttempts は配信1件の最大の試行回数（初回を含む）。
// 配信のエンジンではなくここで定義するのは、それが状態機械自身の方針で、エンジンは実行するだけだから。
const MaxNotifyAttempts = 3

// MaxDigestBatchSize はまとめの一括1つで一度に合わせる配信の最大の数。
//
// ある理由は資源：まとめの1周期で何万件の脆弱性が出てきたら（十分ありうる——全体のスキャン1回で
// できる）、上限がなければ取得はすべての行をメモリに読み込み、とても長いメッセージ1件に描画し、
// そのあとチャンネルの長さの上限で大半が切られる——メモリを無駄にするうえ、切られた脆弱性を**黙って失う**。
// 上限を設ければ、はみ出た分は DB に残って次の一括になり、次の周期で自然に送られ、失われない。
//
// 500 にした根拠：メッセージに描画した後に WeCom の 4096 バイトの上限の中でまだ「読める内容がある」桁がこのくらい；
// これより大きくしても、切り捨てが起きる位置が後ろにずれるだけ。
const MaxDigestBatchSize = 500

// NotificationDelivery は配信の仕事1件で、描画に要るチャンネルの設定とイベントのスナップショットを含む。
type NotificationDelivery struct {
	ID            int64           `json:"id"`
	EventID       int64           `json:"event_id"`
	ChannelID     int64           `json:"channel_id"`
	State         string          `json:"state"`
	Attempts      int             `json:"attempts"`
	NextAttemptAt time.Time       `json:"next_attempt_at"`
	LastError     string          `json:"last_error"`
	BatchID       *int64          `json:"batch_id,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	SentAt        *time.Time      `json:"sent_at,omitempty"`
	Snapshot      json.RawMessage `json:"snapshot,omitempty"`
	// 合わせて読み込む描画の文脈で、JSON には入れない（DTO は server の層が組み立てる）。
	Channel *NotificationChannel `json:"-"`
	// FindingID/EventKind はイベントから持ってきて、履歴の一覧から脆弱性の詳細へ直接飛べるようにする。
	FindingID int64  `json:"finding_id,string"`
	EventKind string `json:"event_kind"`
	// ChannelName/ChannelKind は一覧の表示用の冗長なフィールドで、フロントエンドの2度目の問い合わせを省く。
	ChannelName string `json:"channel_name"`
	ChannelKind string `json:"channel_kind"`
}

const notificationDeliveryCols = `d.id, d.event_id, d.channel_id, d.state, d.attempts, d.next_attempt_at,
       d.last_error, d.batch_id, d.created_at, d.sent_at`

// joinedDeliveryQuery は配信の行をそろえて読む形：配信 + イベントのスナップショット + チャンネルの設定。
// メッセージを1件描画するにはこの3つのどれも欠かせず、別々に問い合わせると3往復になる。
const joinedDeliveryQuery = `SELECT ` + notificationDeliveryCols + `,
       e.snapshot, e.kind, e.finding_id,
       c.id, c.name, c.kind, c.enabled, c.config, c.mode, c.filter, c.rate_per_min
FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id
JOIN notification_channels c ON c.id = d.channel_id`

func scanNotificationDelivery(sc interface{ Scan(...any) error }) (*NotificationDelivery, error) {
	var (
		dl        NotificationDelivery
		lastErr   sql.NullString
		batchID   sql.NullInt64
		sentAt    sql.NullTime
		snapshot  []byte
		eventKind string
		channel   NotificationChannel
		chEnabled bool
	)
	if err := sc.Scan(&dl.ID, &dl.EventID, &dl.ChannelID, &dl.State, &dl.Attempts, &dl.NextAttemptAt,
		&lastErr, &batchID, &dl.CreatedAt, &sentAt,
		&snapshot, &eventKind, &dl.FindingID,
		&channel.ID, &channel.Name, &channel.Kind, &chEnabled, &channel.Config, &channel.Mode, &channel.Filter, &channel.RatePerMin); err != nil {
		return nil, err
	}
	dl.LastError = lastErr.String
	if batchID.Valid {
		dl.BatchID = &batchID.Int64
	}
	if sentAt.Valid {
		dl.SentAt = &sentAt.Time
	}
	dl.Snapshot = json.RawMessage(snapshot)
	dl.EventKind = eventKind
	dl.ChannelName = channel.Name
	dl.ChannelKind = channel.Kind
	channel.Enabled = &chEnabled
	dl.Channel = &channel
	return &dl, nil
}

// claimQuery は1回の取得を表す：まず sel で候補を選んでロックし、それらを sending にして
// リースを延ばす。sel の中の lease の位置は呼び出し側が $n で場所を取り、自分で引数を渡す。
type claimQuery struct {
	sql  string
	args []any
}

// ClaimRealtimeDeliveries はあるチャンネルの期限の来たリアルタイムの配信をひとまとまり、最大 limit 件取る。
//
// わざと**チャンネル1つ**単位で取り、「全体でひとまとまり取ってから選んで送る」はしない：レート制限の関門は配信のエンジンがチャンネルごとに
// 保っていて、このチャンネルがこのラウンドであと何件送れるかを先に知り、それと同じ数の行を取ってこそ、レート制限が
// 再試行の回数を使わずに済む。逆に先に取ってから捨てると、レート制限で止められた行はすでに attempts を1回数えており、
// 3 回の予算は純粋な待ちで使い切られ、最後に failed になる。
//
// 条件には「リースの期限の切れた sending」を含む——それが落ちたときに自然に治る行き先。lease は1回の
// 配信の最悪の所要時間（チャンネルの HTTP クライアントのタイムアウト 15 秒）よりはっきり大きくなければならず、さもないと同じ行を2つの dispatcher が
// 同時に配信する。あわせて停止したチャンネルを止める：停止の操作は既存の配信をすでに skipped にしているが、
// ここでもう一度止め、停止と取得が同時に起きたときの漏れを避ける。
func (d *DB) ClaimRealtimeDeliveries(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	return d.claimDeliveries(ctx, lease, claimQuery{
		sql: `SELECT dd.id FROM notification_deliveries dd
JOIN notification_channels c ON c.id = dd.channel_id
WHERE dd.channel_id = $1 AND dd.state IN ($2,$3) AND dd.next_attempt_at <= now()
  AND c.enabled AND c.mode = $4
ORDER BY dd.next_attempt_at, dd.id
FOR UPDATE OF dd SKIP LOCKED
LIMIT $5`,
		args: []any{channelID, NotifyStatePending, NotifyStateSending, NotifyModeRealtime, limit},
	}, nil)
}

// DigestBatchDue はそのチャンネルに期限の来た一括が溜まったかを報告する：送信待ちの配信があり、かつ**最も古いもの**の
// 経過時間がまとめの周期に達している。
//
// 判定の根拠は壁の時計ではなく最も古い配信の経過時間：こうすれば作ったばかりのチャンネルが正時に合わせたせいで
// すぐに1件だけの「まとめ」を吐き出すこともなく、長く溜まった一括がもう1ラウンド無駄に待つこともない。
//
// ClaimDigestBatch と分けたのは意味が違うから：この関数は「送るべきか」に答えるだけで、
// 取得はそのチャンネルの**すべての**送信待ちの行（まだ経過時間に達していないものも含む）を持っていく——さもないと1周期が
// 複数のメッセージに分かれ、まとめの意味がなくなる。
func (d *DB) DigestBatchDue(ctx context.Context, channelID int64, minAge time.Duration) (bool, error) {
	var due bool
	err := d.QueryRowContext(ctx, `SELECT EXISTS (
  SELECT 1 FROM notification_deliveries d
  JOIN notification_channels c ON c.id = d.channel_id
  WHERE d.channel_id = $1 AND d.state IN ($2,$3) AND c.enabled
  GROUP BY d.channel_id
  HAVING min(d.created_at) <= now() - make_interval(secs => $4)
)`, channelID, NotifyStatePending, NotifyStateSending, int64(minAge.Seconds())).Scan(&due)
	return due, err
}

// ClaimDigestBatch はあるチャンネルのいま期限の来た送信待ちの配信を、まとめの一括として取る。
// 一括1つで最大 MaxDigestBatchSize 件。
//
// 同じ一括のすべての配信は batch_id を共有し、集合の最小の id を一括の番号にする（安定し、読め、
// 別のシーケンスが要らない）。再試行のときは COALESCE で元の一括の番号を残し、「この N 件は一緒に送った」が
// 何度再試行しても成り立つようにする。
//
// ランダムではなく id の昇順で先頭の N 件を取る：最も早く生じた配信が最初に送られ、溜まったときに
// 「新しい脆弱性が先に送られ、古い脆弱性がいつまでも後ろに並ぶ」という飢えが起きない。
func (d *DB) ClaimDigestBatch(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	// limit は**メモリの上限**で、呼び出し側は MaxDigestBatchSize を渡す；ここでもう一度挟み、
	// 呼び出し側がもっと大きい値を渡すのを防ぐ。
	//
	// わざと「レート制限の枠」を一括の大きさとして受け付けない：レート制限の単位はメッセージの件数——一括1つは
	// メッセージを1件送り、トークンを1つ使い、server の層の takeTokens が差し引く——で、「一括に脆弱性を何件
	// 入れるか」とは別の次元。かつて rate_per_min を digest に効かせるために、ラウンドごとの
	// リクエストの予算を一括の大きさとして渡したら、rate=20/min のチャンネルは一括ごとに脆弱性を1件しか入れられず、
	// digest はまとめの文面の付いたリアルタイムの通知に退化した。レート制限を変えたいなら takeTokens の want を変え、
	// ここには触れないこと。
	if limit > MaxDigestBatchSize {
		limit = MaxDigestBatchSize
	}
	out, err := d.claimDeliveries(ctx, lease, claimQuery{
		sql: `SELECT dd.id FROM notification_deliveries dd
JOIN notification_channels c ON c.id = dd.channel_id
WHERE dd.channel_id = $1 AND dd.state IN ($2,$3) AND dd.next_attempt_at <= now() AND c.enabled
ORDER BY dd.id
FOR UPDATE OF dd SKIP LOCKED
LIMIT $4`,
		args: []any{channelID, NotifyStatePending, NotifyStateSending, limit},
	}, func(tx *sql.Tx, ids []int64) error {
		batchID := ids[0]
		for _, id := range ids {
			if id < batchID {
				batchID = id
			}
		}
		ph, idArgs := placeholders(2, ids)
		_, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET batch_id = COALESCE(batch_id, $1)
WHERE id IN (`+ph+`)`, append([]any{batchID}, idArgs...)...)
		return err
	})
	return out, err
}

// claimDeliveries は「選ぶ + sending にしてリースを延ばす + 行をまるごと読む」をすべて1つのトランザクションでする。
// postClaim は任意の追加の手順（まとめの一括はこれで batch_id を書き込む）。
func (d *DB) claimDeliveries(ctx context.Context, lease time.Duration, cq claimQuery, postClaim func(*sql.Tx, []int64) error) ([]*NotificationDelivery, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // コミットに成功した後は no-op

	ids, err := selectForClaim(ctx, tx, cq.sql, cq.args...)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, tx.Commit()
	}
	// sending にして next_attempt_at を未来に進める：この未来の時刻がリースの期限で、
	// そのため「リースの期限前」と「再試行の時刻の前」が同じ条件の式を共有し、列を足さずに済む。
	ph, idArgs := placeholders(3, ids)
	if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, attempts=attempts+1, next_attempt_at=now()+make_interval(secs => $2)
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateSending, lease.Seconds()}, idArgs...)...); err != nil {
		return nil, err
	}
	if postClaim != nil {
		if err := postClaim(tx, ids); err != nil {
			return nil, err
		}
	}
	out, err := loadDeliveriesTx(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func selectForClaim(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func loadDeliveriesTx(ctx context.Context, tx *sql.Tx, ids []int64) ([]*NotificationDelivery, error) {
	ph, args := placeholders(1, ids)
	rows, err := tx.QueryContext(ctx, joinedDeliveryQuery+` WHERE d.id IN (`+ph+`) ORDER BY d.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationDelivery{}
	for rows.Next() {
		dl, err := scanNotificationDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dl)
	}
	return out, rows.Err()
}

// MarkDeliveriesSent はひとまとまりの配信に届いた印を付ける。
func (d *DB) MarkDeliveriesSent(ctx context.Context, ids []int64) error {
	ph, args := placeholders(2, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, sent_at=now(), last_error='' WHERE id IN (`+ph+`)`, append([]any{NotifyStateSent}, args...)...)
	return err
}

// RescheduleDeliveries はひとまとまりの配信を pending に戻し、再試行の時刻を後ろにずらす。
//
// 新しい中間の状態を持ち込まずに pending に戻すのは、「あと何回機会が残っているか」を1か所だけで
// 表す（MaxNotifyAttempts）ようにし、状態機械の分岐が再試行の方針とともに膨らむのを避けるため。
func (d *DB) RescheduleDeliveries(ctx context.Context, ids []int64, delay time.Duration, errMsg string) error {
	ph, args := placeholders(4, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, next_attempt_at=now()+make_interval(secs => $2), last_error=$3
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStatePending, delay.Seconds(), truncateNotifyError(errMsg)}, args...)...)
	return err
}

// DeferDeliveries はひとまとまりの配信を pending に戻してすぐまた取れるようにし、**取得のときに数えた1回の試行を取り消す**。
//
// 用途はひとつだけ：まとめのメッセージをチャンネルの長さの上限で分けて送るとき、このメッセージに入らなかった項目を次の一括に残す。
// それは失敗ではないので、再試行の予算を使うべきでない——取得のときに attempts をもう楽観的に +1 しているので、
// ここで必ず戻す。さもないと 500 件の溜まりを 20 件ずつに切って 25 の部分にしたとき、
// 末尾の項目は 3 番目の部分で MaxNotifyAttempts により failed と判定される。何のエラーも出していないのに。
//
// GREATEST(...,0) は「誰かが手で送り直して attempts を 0 にした後にここに来た」場合を支え、
// 数が負にならないようにする。
func (d *DB) DeferDeliveries(ctx context.Context, ids []int64, reason string) error {
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, attempts=GREATEST(attempts-1, 0), next_attempt_at=now(), last_error=$2
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStatePending, truncateNotifyError(reason)}, args...)...)
	return err
}

// FailDeliveries はひとまとまりの配信に最終的な失敗の印を付け、人が配信の履歴で送り直すのを待つ。
func (d *DB) FailDeliveries(ctx context.Context, ids []int64, errMsg string) error {
	// 場所取りは $3 から始まる：$1 は state、$2 は last_error。
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries SET state=$1, last_error=$2 WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateFailed, truncateNotifyError(errMsg)}, args...)...)
	return err
}

// RetryNotificationDelivery は配信1件を手動で送り直す：pending に戻し、再試行の数を 0 にし、
// すぐ期限の来た状態にする。数を 0 にするのはわざと——人が「送り直す」を押すのは、前の数回の失敗の原因がもう片づいたということで、
// 古い数でそれを縛る理由はない。
func (d *DB) RetryNotificationDelivery(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$2, attempts=0, next_attempt_at=now(), last_error=''
WHERE id=$1 AND state IN ($3,$4)`, id, NotifyStatePending, NotifyStateFailed, NotifyStateSkipped)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("配信 %d は存在しないか、いまの状態では送り直せない", id)
	}
	return nil
}

// NotificationDeliveryFilter は配信の履歴の問い合わせの条件。
type NotificationDeliveryFilter struct {
	ChannelID int64
	State     string
	EventKind string
}

func (f NotificationDeliveryFilter) where() (string, []any) {
	var conds []string
	var args []any
	if f.ChannelID > 0 {
		args = append(args, f.ChannelID)
		conds = append(conds, fmt.Sprintf("d.channel_id=$%d", len(args)))
	}
	if f.State != "" {
		args = append(args, f.State)
		conds = append(conds, fmt.Sprintf("d.state=$%d", len(args)))
	}
	if f.EventKind != "" {
		args = append(args, f.EventKind)
		conds = append(conds, fmt.Sprintf("e.kind=$%d", len(args)))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// ListNotificationDeliveries は配信の履歴をページ単位で、新しいものを前にして返す。
func (d *DB) ListNotificationDeliveries(ctx context.Context, f NotificationDeliveryFilter, page, pageSize int) ([]*NotificationDelivery, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 50
	}
	where, args := f.where()

	var total int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := fmt.Sprintf("%s%s ORDER BY d.id DESC LIMIT $%d OFFSET $%d",
		joinedDeliveryQuery, where, len(args)+1, len(args)+2)
	rows, err := d.QueryContext(ctx, q, append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*NotificationDelivery{}
	for rows.Next() {
		dl, err := scanNotificationDelivery(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, dl)
	}
	return out, total, rows.Err()
}

// truncateNotifyError はエラーのメッセージを列が受け付ける長さに切り詰める。チャンネルが返す応答の本文は長いことがあり
// （汎用の Webhook で自前のサービスに送るときは特に）、切り詰めないと履歴の一覧の中身が膨らむ。
func truncateNotifyError(msg string) string {
	const max = 500
	if len(msg) <= max {
		return msg
	}
	// 文字の境目まで戻り、UTF-8 の文字を半分残してフロントエンドで文字化けさせるのを避ける。
	cut := max
	for cut > 0 && !isUTF8Start(msg[cut]) {
		cut--
	}
	return msg[:cut] + "…"
}

func isUTF8Start(b byte) bool { return b&0xC0 != 0x80 }

// placeholders は start から始まる $n の場所取りの文字列と対応する引数を作り、IN (...) に使う。
// 例えば start=3, ids=[7,8] → "$3,$4", [7,8]。
func placeholders(start int, ids []int64) (string, []any) {
	ph := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		ph = append(ph, fmt.Sprintf("$%d", start+i))
		args = append(args, id)
	}
	return strings.Join(ph, ","), args
}
