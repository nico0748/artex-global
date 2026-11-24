package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Autumn-27/artex/notify"
)

// このファイルは IM の通知のチャンネルの設定とイベントの層。配信の仕事の取得と状態の遷移は
// db/notification_delivery.go。
//
// 2つの不変条件。このファイルを変えるときは必ず保つこと：
//
//  1. 脆弱性を書くトランザクション(RecordFindingTx)は InsertNotificationEventTx を呼んで盲目の挿入を1回するだけで、
//     通知に関わるどのテーブルも読まず、絞り込みの照合もしない。ここに持ち込んだ読み取りはどれも、
//     ユーザーが設定を誤った絞り込みの条件のせいで脆弱性の書き込みのトランザクションを汚したり止めたりしうる。
//  2. 絞り込みの照合は決してエラーにしない：設定がおかしければ一律に「当たり」として扱う(notify.Match を参照)。多めに送っても、
//     送り漏らしてはならない。

// ErrNotificationChannelNotFound はチャンネルが存在しない。
var ErrNotificationChannelNotFound = errors.New("通知のチャンネルが存在しない")

// 配信の状態。
const (
	NotifyStatePending = "pending" // 送信待ち
	NotifyStateSending = "sending" // どこかの dispatcher が取った。リースの期限前
	NotifyStateSent    = "sent"    // 届いた
	NotifyStateFailed  = "failed"  // 再試行を使い切ったか恒久的な失敗。手動で送り直せる
	NotifyStateSkipped = "skipped" // チャンネルを止めたので、もう送らない
)

// 通知のモード。
const (
	NotifyModeRealtime = "realtime"
	NotifyModeDigest   = "digest"
)

// ValidNotifyMode は通知のモードをホワイトリストで確かめる（findings.status と同じ理由：DB の CHECK を使わず、
// のちに広げやすくする）。
func ValidNotifyMode(m string) bool {
	return m == NotifyModeRealtime || m == NotifyModeDigest
}

// NotificationChannel はチャンネルの実体の設定。Config と Filter は元の JSON のまま持ち、
// 解析は notify パッケージに任せる——db の層はそれらのフィールドの意味を知らない。
type NotificationChannel struct {
	ID     int64           `json:"id"`
	Name   string          `json:"name"`
	Kind   string          `json:"kind"`
	Mode   string          `json:"mode"`
	Config json.RawMessage `json:"config"`
	Filter json.RawMessage `json:"filter"`
	// Enabled をポインタにするのは「このフィールドを渡さなかった」と「明示的に false を渡した」を区別するため——
	// フロントエンドの切り替えの部品は変えたフィールドだけを送る。
	Enabled    *bool     `json:"enabled,omitempty"`
	RatePerMin int       `json:"rate_per_min"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// IsEnabled はチャンネルが有効かを返す；Enabled が nil（読み込んでいない）なら有効として扱う。
func (c *NotificationChannel) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// NotificationEvent はイベントの事実1件。
type NotificationEvent struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	FindingID int64           `json:"finding_id"`
	Snapshot  json.RawMessage `json:"snapshot"`
	CreatedAt time.Time       `json:"created_at"`
}

const notificationChannelCols = `id, name, kind, enabled, config, mode, filter, rate_per_min, created_at, updated_at`

func scanNotificationChannel(sc interface{ Scan(...any) error }) (*NotificationChannel, error) {
	var c NotificationChannel
	var enabled bool
	if err := sc.Scan(&c.ID, &c.Name, &c.Kind, &enabled, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	c.Enabled = &enabled
	return &c, nil
}

// ListNotificationChannels はすべてのチャンネルの実体を返し、有効なものを前に、同じ並びなら id の順にする。
// 並べ替えを SQL に置くのは、UI と dispatcher が同じ安定した順序を見るようにするため。
func (d *DB) ListNotificationChannels(ctx context.Context) ([]*NotificationChannel, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels
ORDER BY enabled DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		c, err := scanNotificationChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// NotificationChannelByID はチャンネルを1つ取る。
func (d *DB) NotificationChannelByID(ctx context.Context, id int64) (*NotificationChannel, error) {
	row := d.QueryRowContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels WHERE id=$1`, id)
	c, err := scanNotificationChannel(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotificationChannelNotFound
	}
	return c, err
}

// SaveNotificationChannel はチャンネルを新しく作るか更新する。
//
// 更新のときは呼び出し側が明示的に与えたフィールド（nil でない / 空でない）だけを上書きする。こうすればフロントエンドは一部だけ
// 変えた引き出しのフォームを送れ、config のうち表示していないフィールドを送り返さずに済む——送り返すとかえって
// 「マスクした値が本物の鍵を上書きする」事故になる。
func (d *DB) SaveNotificationChannel(ctx context.Context, c *NotificationChannel) (int64, error) {
	if c.Mode == "" {
		c.Mode = NotifyModeRealtime
	}
	// ここではわざと 0 に**何の**手も加えない：0 は正しい設定で、意味は「レート制限なし」。
	//
	// かつては `if c.RatePerMin <= 0 { c.RatePerMin = 既定値 }` と書いていた。意図は「指定がないときに
	// 安全な既定を与える」だったが、それは「明示的に 0 にした」もまとめて飲み込んでいた——文書、UI の案内、
	// takeTokens はどれも 0 をレート制限なしと解釈するのに、ここだけが黙って 20（DingTalk/WeCom/Telegram）
	// か 100（Feishu）に変えていて、運用者はレート制限を外したつもりなのに実際は 20/分 で止められ、何の知らせもなかった。
	//
	// 「指定なし」と「明示的な 0」の違いは呼び出し側しか知らない（リクエストの本文でフィールドを省いたか、はっきり 0 を渡したか）ので、
	// 既定値はフィールドが省かれたときに server の層が入れる。notifyCreateChannel を参照。
	if c.RatePerMin < 0 {
		return 0, errors.New("レート制限の値は負にできない")
	}
	if c.Config == nil {
		c.Config = json.RawMessage(`{}`)
	}
	if c.Filter == nil {
		c.Filter = json.RawMessage(`{}`)
	}
	enabled := c.IsEnabled()

	if c.ID == 0 {
		var id int64
		err := d.QueryRowContext(ctx, `INSERT INTO notification_channels(name,kind,enabled,config,mode,filter,rate_per_min)
VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
			c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin).Scan(&id)
		return id, err
	}
	res, err := d.ExecContext(ctx, `UPDATE notification_channels
SET name=$2, kind=$3, enabled=$4, config=$5, mode=$6, filter=$7, rate_per_min=$8
WHERE id=$1`,
		c.ID, c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotificationChannelNotFound
	}
	return c.ID, nil
}

// SetNotificationChannelEnabled は有効/停止を切り替える。
//
// チャンネルを止めるときは、まだ送っていない配信をまとめて skipped にする：さもないと有効に戻した後に
// 「停止の間に溜まった」古い脆弱性をいきなりまとめて受け取り、鮮度を失っているうえ新しいものと取り違えやすい。
func (d *DB) SetNotificationChannelEnabled(ctx context.Context, id int64, enabled bool) error {
	return d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE notification_channels SET enabled=$2 WHERE id=$1`, id, enabled)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotificationChannelNotFound
		}
		if !enabled {
			if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET state=$2, last_error=$3
WHERE channel_id=$1 AND state IN ($4,$5)`,
				id, NotifyStateSkipped, "チャンネルを停止した", NotifyStatePending, NotifyStateSending); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteNotificationChannel はチャンネルを消す。その配信の履歴は外部キーで連鎖して消える
// （チャンネルの設定がなくなれば、履歴は読み解けない）。
func (d *DB) DeleteNotificationChannel(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `DELETE FROM notification_channels WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotificationChannelNotFound
	}
	return nil
}

// RecordNotificationEventTx は呼び出し側のトランザクションの中で通知のイベントを1件**できるかぎり**書き込む。
//
// これは脆弱性の書き込みの経路で通知に関わる唯一の変更：INSERT を1回し、どのテーブルも読まず、チャンネルを知らず、
// 絞り込みも走らせない。トランザクションのコミットで「脆弱性の保存」と「通知の仕事の存在」の原子的な一致を保証し、
// コミットは成功したのに待ち行列に入らず、メッセージが永久に失われる窓はない。
//
// 2つの肝心な設計で、どちらも思いつきで書いたものではない：
//
//  1. **なぜ SAVEPOINT を使うか**：PostgreSQL ではトランザクションの中のどれか1つの文がエラーになると、トランザクション全体が
//     aborted の状態になり、その後のすべての文（COMMIT を含む）が一律に失敗する。だから「この INSERT の
//     エラーを無視して、呼び出し側にコミットを続けさせる」ことは PG ではできない——保存点でエラーを
//     この1文に閉じ込めないかぎり。保存点がなければ、「まるごとロールバック」という選択肢しか残らない。
//
//  2. **なぜまるごとのロールバックは誤りか**：通知は便利な機能で、脆弱性の記録こそが製品そのもの。通知の
//     テーブルの問題（古い DB の移行漏れ、ディスクの一時的な故障）で高い危険の脆弱性を保存できなくなってはならない。だからここではエラーを
//     閉じ込め、ログに記録し、false を返し、脆弱性の書き込みはいつもどおりコミットさせる——代償はこの通知1件を失うこと。
//     error ではなく bool を返すのはわざと：呼び出し側はこれを書き込みの成否に関わるエラーとして扱うべきでない。
func RecordNotificationEventTx(ctx context.Context, tx *sql.Tx, kind string, findingID int64, snap notify.Snapshot) bool {
	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[notify] 通知のイベントのシリアライズに失敗 finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT notify_event`); err != nil {
		log.Printf("[notify] 保存点の作成に失敗 finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3)`,
		kind, findingID, string(raw)); err != nil {
		log.Printf("[notify] 通知のイベントの書き込みに失敗 finding=%d（脆弱性の記録には影響しない）: %v", findingID, err)
		// 保存点までロールバックし、トランザクションを aborted の状態から救い出す。
		if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT notify_event`); rbErr != nil {
			log.Printf("[notify] 保存点へのロールバックに失敗 finding=%d: %v", findingID, rbErr)
		}
		return false
	}
	// 保存点を解放し、長いトランザクションに要らない保存点が溜まるのを避ける。
	_, _ = tx.ExecContext(ctx, `RELEASE SAVEPOINT notify_event`)
	return true
}

// AddNotificationEvent は InsertNotificationEventTx の独立したトランザクションの版で、
// 既存のトランザクションの外にある呼び出し点が使う（チャンネルの「テストのメッセージを送る」など。本物の finding がない）。
func (d *DB) AddNotificationEvent(ctx context.Context, kind string, findingID int64, snap notify.Snapshot) (int64, error) {
	raw, err := json.Marshal(snap)
	if err != nil {
		return 0, fmt.Errorf("通知のイベントのスナップショットのシリアライズに失敗: %w", err)
	}
	var id int64
	err = d.QueryRowContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3) RETURNING id`,
		kind, findingID, string(raw)).Scan(&id)
	return id, err
}

// FanOutPendingEvents はまだ振り分けていない脆弱性のイベントを、いま有効なチャンネルごとに配信の仕事に展開し、
// このラウンドで処理したイベントの数と新しく作った配信の数を返す。
//
// ラウンド全体の操作は1つのトランザクションの中：イベントは FOR UPDATE SKIP LOCKED で取り、複数のプロセスが同時に動いても
// それぞれ別の行を取る（プロジェクトのアーカイブの待ち行列の取得も同じ手法を使う。
// db/task_archives.go の completeNextArchiveJob を参照）。
//
// 絞り込みの照合はわざと SQL ではなく Go 側に置く：チャンネルの絞り込みの条件は任意のフィールドの組の JSONB で、
// SQL で6通りの組み合わせの照合を書くとクエリの保守が難しくなる。一方チャンネルの数は「人が手で設定した数件」なので、
// すべて読み込んでからメモリで1件ずつ比べるほうが速く、テストもしやすい。
//
// どのチャンネルにも当たらなかったイベントも fanned_out の印を付ける——さもないと振り分け待ちの集合に永遠に残り、
// tick のたびに見直される。
func (d *DB) FanOutPendingEvents(ctx context.Context, limit int) (eventCount, deliveryCount int, err error) {
	if limit <= 0 {
		limit = 200
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() //nolint:errcheck // コミットに成功した後は no-op

	channels, err := listEnabledNotificationChannelsTx(ctx, tx)
	if err != nil {
		return 0, 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, kind, finding_id, snapshot FROM notification_events
WHERE NOT fanned_out ORDER BY id FOR UPDATE SKIP LOCKED LIMIT $1`, limit)
	if err != nil {
		return 0, 0, err
	}
	var (
		events      []NotificationEvent
		parsedSnaps []notify.Snapshot
	)
	for rows.Next() {
		var ev NotificationEvent
		if err := rows.Scan(&ev.ID, &ev.Kind, &ev.FindingID, &ev.Snapshot); err != nil {
			rows.Close()
			return 0, 0, err
		}
		var snap notify.Snapshot
		// スナップショットは自分たちで書いたものなので、理屈では必ず解析できる；解析に失敗しても配信の流れは止めないが、
		// このイベントはフィールドがすべて空になるので、絞り込みの条件を持つすべてのチャンネルに飛ばされる——1件少なく送ってでも、
		// 壊れた1行で待ち行列全体を詰まらせない。
		_ = json.Unmarshal(ev.Snapshot, &snap)
		// kind は行の値を正とする：スナップショットのほうは描画用の写しで、古い版が書いたかもしれない。
		snap.Kind = ev.Kind
		events = append(events, ev)
		parsedSnaps = append(parsedSnaps, snap)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if len(events) == 0 {
		return 0, 0, tx.Commit()
	}

	type pending struct {
		eventID   int64
		channelID int64
	}
	var toInsert []pending
	for i, snap := range parsedSnaps {
		for _, ch := range channels {
			if !notify.Match(notify.ParseFilter(ch.Filter), snap) {
				continue
			}
			toInsert = append(toInsert, pending{eventID: events[i].ID, channelID: ch.ID})
		}
	}
	if len(toInsert) > 0 {
		var (
			vals []string
			args []any
		)
		for _, p := range toInsert {
			vals = append(vals, fmt.Sprintf("($%d,$%d)", len(args)+1, len(args)+2))
			args = append(args, p.eventID, p.channelID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notification_deliveries(event_id,channel_id) VALUES `+strings.Join(vals, ","), args...); err != nil {
			return 0, 0, err
		}
	}

	// このラウンドのイベントに振り分け済みの印を付ける。どのチャンネルにも当たらなかったイベントもまとめて印を付ける（関数の注釈を参照）。
	ids := make([]string, 0, len(events))
	markArgs := make([]any, 0, len(events))
	for _, ev := range events {
		markArgs = append(markArgs, ev.ID)
		ids = append(ids, fmt.Sprintf("$%d", len(markArgs)))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notification_events SET fanned_out=true WHERE id IN (`+strings.Join(ids, ",")+`)`, markArgs...); err != nil {
		return 0, 0, err
	}
	return len(events), len(toInsert), tx.Commit()
}

// listEnabledNotificationChannelsTx はトランザクションの中で有効なチャンネルを取る。数はごく少ないので、
// ページ分けもキャッシュもしない——キャッシュは「設定を変えたらいつ効くか」という余計な時間の問題を持ち込む。
func listEnabledNotificationChannelsTx(ctx context.Context, tx *sql.Tx) ([]*NotificationChannel, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, name, kind, config, mode, filter, rate_per_min
FROM notification_channels WHERE enabled ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		var c NotificationChannel
		if err := rows.Scan(&c.ID, &c.Name, &c.Kind, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// NotificationAssetNames は資産の id を短い表示名に解決し、通知のメッセージに使う。
//
// 返す順序は引数と同じで、長さは引数より短いことがある（存在しない id は飛ばす）。引数の順序を保つのは、
// 同じ脆弱性のメッセージが何度配信されても資産の順序が安定するようにするため——さもないと再試行の後に受け取ったメッセージで
// 資産の順序が変わり、「資産が変わった」と読み違えられる。
func (d *DB) NotificationAssetNames(ctx context.Context, ids []int64) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph, args := placeholders(1, ids)
	rows, err := d.QueryContext(ctx, `SELECT id, type, domain, ip, url, app_name, bundle_id FROM assets WHERE id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	labels := map[int64]string{}
	for rows.Next() {
		var (
			id                int64
			typ               string
			domain, ip, url   sql.NullString
			appName, bundleID sql.NullString
		)
		if err := rows.Scan(&id, &typ, &domain, &ip, &url, &appName, &bundleID); err != nil {
			return nil, err
		}
		labels[id] = assetDisplayName(typ, domain.String, ip.String, url.String, appName.String, bundleID.String)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ids))
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if label, ok := labels[id]; ok && label != "" {
			out = append(out, label)
		}
	}
	return out, nil
}

// assetDisplayName は資産の種類に応じて最も見分けやすい識別子を選ぶ。
// 最後は空の文字列を返し、「名前の取れない資産」をどう見せるかは呼び出し側が決める——この関数は置き場の文字をでっちあげない。
// さもないと「資産#42」のような雑音が通知のメッセージに混じり、読み手は本物のドメインだと思ってしまう。
func assetDisplayName(typ, domain, ip, url, appName, bundleID string) string {
	pick := func(vals ...string) string {
		for _, v := range vals {
			if strings.TrimSpace(v) != "" {
				return v
			}
		}
		return ""
	}
	switch typ {
	case "root_domain", "subdomain":
		return domain
	case "ip":
		return ip
	case "app":
		return pick(appName, bundleID)
	case "service", "endpoint":
		return pick(url, domain, ip)
	default:
		return pick(domain, ip, url, appName)
	}
}

// SetFindingStatusWithNotify は脆弱性の対応の状態を更新し、同じトランザクションの中で状態の変更の
// 通知のイベントを1件登録する。
//
// 返すのは from=変更前の状態；found=脆弱性が存在するか；notified=イベントの登録に成功したか。
//
// わざとそうしている3つの挙動：
//   - 状態が実際には変わっていないときはイベントを登録しない。フロントエンドの引き出しが同じ値を重ねて送っても、自動化のスクリプトが
//     冪等に再生しても、通知の雑音を生むべきでない。
//   - 脆弱性が存在しないときは found=false を返して何も書き込まず、呼び出し側が 404 に訳す。
//   - イベントの登録の失敗は状態の更新に影響しない（RecordNotificationEventTx の保存点の説明を参照）。
//     だから notified=false のときも状態の変更は成功しており、呼び出し側はそれでエラーにすべきでない。
func (d *DB) SetFindingStatusWithNotify(ctx context.Context, id int64, status string) (from string, found bool, notified bool, err error) {
	err = d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		var txErr error
		from, found, _, notified, txErr = SetFindingStatusTx(ctx, tx, id, status)
		return txErr
	})
	return from, found, notified, err
}

// SetFindingStatusTx は**呼び出し側のトランザクション**の中で脆弱性の状態を更新し、状態の変更の通知のイベントを登録する。
//
// トランザクション単位の関数に切り出したのは、状態を変えるすべての経路に同じ意味を共有させるため——以前は
// patchFinding だけが通知付きの版を通っていて、**再テストの結論が「修正済み」のとき**（finding_retests
// のあの `UPDATE findings SET status=...`）は直接 DB に書いていた。そのため
// `on_status_change` を設定したチャンネルはこの種の状態の遷移の通知をまったく受け取れなかった：画面では状態が黙って変わり、
// 運用者はプラットフォームを開いて初めて気づいた。
//
// 返すのは from=変更前の状態、found=脆弱性が存在するか、changed=状態が本当に変わったか、
// notified=イベントの登録に成功したか（登録の失敗は状態の更新に影響しない。RecordNotificationEventTx を参照）。
func SetFindingStatusTx(ctx context.Context, tx *sql.Tx, id int64, status string) (from string, found bool, changed bool, notified bool, err error) {
	var (
		vulnclass, name, severity, summary string
		taskID                             sql.NullInt64
		assetIDs                           []byte
	)
	scanErr := tx.QueryRowContext(ctx, `SELECT vulnclass, name, severity, summary, task_id, asset_ids, status
FROM findings WHERE id=$1 FOR UPDATE`, id).
		Scan(&vulnclass, &name, &severity, &summary, &taskID, &assetIDs, &from)
	if scanErr == sql.ErrNoRows {
		return "", false, false, false, nil
	}
	if scanErr != nil {
		return "", false, false, false, scanErr
	}
	found = true
	if from == status {
		// 状態が本当に変わっていなければイベントを登録しない：同じ値を重ねて送っても、冪等に再生しても、
		// 通知の雑音を生むべきでない。
		return from, true, false, false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE findings SET status=$2 WHERE id=$1`, id, status); err != nil {
		return from, true, false, false, err
	}
	var assets []int64
	_ = json.Unmarshal(assetIDs, &assets)
	notified = RecordNotificationEventTx(ctx, tx, notify.EventFindingStatusChanged, id, notify.Snapshot{
		Kind:       notify.EventFindingStatusChanged,
		FindingID:  id,
		TaskID:     taskID.Int64,
		VulnClass:  vulnclass,
		Name:       name,
		Severity:   severity,
		Summary:    summary,
		AssetIDs:   assets,
		FromStatus: from,
		ToStatus:   status,
	})
	return from, true, true, notified, nil
}

// NotificationStats は通知のページの上部の概要の数。
type NotificationStats struct {
	Channels     int   `json:"channels"`
	ChannelsOn   int   `json:"channels_on"`
	Pending      int   `json:"pending"`
	Failed       int   `json:"failed"`
	SentToday    int   `json:"sent_today"`
	BacklogAgeMS int64 `json:"backlog_age_ms"` // 最も古い送信待ちの配信が今から何ミリ秒前か
}

// NotificationStatsSnapshot は通知のシステムの健全さをまとめる。
// BacklogAgeMS は「通知が詰まっていないか」の最も直接の指標——pending の数よりずっと役に立つ。
// 溜まりが 3 件でも、3 秒前からなのか 3 時間前からなのかで違いうるから。
func (d *DB) NotificationStatsSnapshot(ctx context.Context) (*NotificationStats, error) {
	var s NotificationStats
	if err := d.QueryRowContext(ctx, `SELECT
    (SELECT count(*) FROM notification_channels),
    (SELECT count(*) FROM notification_channels WHERE enabled),
    (SELECT count(*) FROM notification_deliveries WHERE state IN ($1,$2)),
    (SELECT count(*) FROM notification_deliveries WHERE state=$3),
    (SELECT count(*) FROM notification_deliveries WHERE state=$4 AND sent_at >= date_trunc('day', now())),
    COALESCE((SELECT EXTRACT(EPOCH FROM (now() - min(created_at))) * 1000 FROM notification_deliveries WHERE state=$1), 0)::bigint`,
		NotifyStatePending, NotifyStateSending, NotifyStateFailed, NotifyStateSent).
		Scan(&s.Channels, &s.ChannelsOn, &s.Pending, &s.Failed, &s.SentToday, &s.BacklogAgeMS); err != nil {
		return nil, err
	}
	return &s, nil
}
