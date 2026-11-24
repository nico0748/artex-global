package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// 全体の設定のキー（settings のキーと値の表に保存し、表を作る必要はない）。
const (
	// settingNotifyEnabled は通知の全体のスイッチ。既定はオン：これは保守のときにワンクリックで止血するためのもので、
	// 機能を有効にする条件ではない——本当の有効の条件は「チャンネルを設定したかどうか」。
	settingNotifyEnabled = "notify_enabled"
	// settingNotifyPublicBaseURL は脆弱性の詳細へ戻るリンクを作る外部からのアクセスのアドレス
	// （例 https://artex.example.com）。空ならメッセージに戻るリンクのボタンを付けない。
	// プロジェクトには再利用できる外部のアドレスの設定がないので、ここで1項目足す。
	settingNotifyPublicBaseURL = "notify_public_base_url"
	// settingNotifyDigestMinutes はまとめのモードの周期（分）。
	settingNotifyDigestMinutes = "notify_digest_interval_min"
)

const (
	// notifyTick は配信エンジンのポーリングの間隔。3 秒がこのエンジンのリアルタイム性の上限で、
	// 「脆弱性の保存」から「メッセージが IM に届く」までの主な遅れの原因でもある。
	notifyTick = 3 * time.Second
	// notifyBackgroundDisabledEnv 가 설정되면 서버가 백그라운드 투입(配信) 루프
	// (newNotifier(s).Run)를 띄우지 않는다. 통합 테스트는 stepRealtime·stepDigest 를
	// 직접 호출해 분배 결과를 결정론적으로 검증하는데, 3 초 주기의 백그라운드 루프가
	// 같은 채널을 동시에 처리하면 집계가 타이밍에 따라 흔들린다. 이를 막기 위한
	// 테스트 전용 스위치이며, 변수가 없으면 평소대로 기동한다(프로덕션 동작 불변).
	notifyBackgroundDisabledEnv = "ARTEX_NOTIFY_BACKGROUND_DISABLED"
	// notifyLease は配信を取るときのリースの長さ。1回の配信の最悪の所要時間よりはっきり大きくなければならない
	// （notify パッケージの HTTP クライアントのタイムアウトは 15 秒）。さもないと同じ行を2つの
	// dispatcher が同時に配信してしまう。
	notifyLease = 3 * time.Minute
	// notifyFanOutPerTick はラウンドごとに振り分けるイベントの数を制限し、チャンネルを初めて有効にしたときに
	// 過去の溜まりを一度にすべて配信のタスクに展開するのを避ける。
	notifyFanOutPerTick = 200
	// notifyDefaultDigestMinutes はまとめの周期の既定値。
	notifyDefaultDigestMinutes = 30
	// notifyUnlimitedBurstPerTick はチャンネルにレート制限を設定していないときのラウンドごとの配信の上限。
	// これがあるのは「チャンネルを無制限に設定 + 一度に千件を超える脆弱性を拾う」で
	// 1ラウンドのループが長くふさがるのを防ぐため。
	notifyUnlimitedBurstPerTick = 50
	// notifyMaxSendsPerChannelPerTick はチャンネル1つにつき1ラウンドで最大何件配信するか。
	//
	// この上限は**リースの長さ**から逆算する：取るときに行に付けるのはリース（notifyLease = 3 分）で、
	// 1ラウンドで順に配信する件数が多く、最悪の所要時間がリースを超えると、後ろの数件は送り終わる前にリースが切れる。
	// 1つのプロセスの中では問題ない（Run は1つの goroutine で順に動き、tick は入れ子にならない）が、**2つの
	// プロセスが同じ DB につながる**と、相手がリースの切れた行を取り直して重ねて送り、さらに
	// attempts を二重に増やし、元のプロセスがまだ配信している間に失敗と判定してしまう。
	//
	// 値：3 分のリース / 30 秒の1回のタイムアウト = 6 は**リースをちょうど使い切る**のでゆとりがゼロで、
	// 選べない；5 にすれば最悪の所要時間 150 秒で 30 秒のゆとりが残る。この関係は
	// TestNotifyTickBudgetFitsWithinLease が固定している——notifyLease、
	// notifySendTimeout、この値のどれを変えてもその断言が失敗する。
	notifyMaxSendsPerChannelPerTick = 5
	// notifySendTimeout は1回の配信のタイムアウト。これは1つ前の定数の値も決め、
	// 2つを掛けたものは notifyLease を超えてはならない。TestNotifyTickBudgetFitsWithinLease を参照。
	notifySendTimeout = 30 * time.Second
)

// notifyBackoff は失敗したときの再試行のバックオフの列で、添字は試した回数。
// 3 回の機会（初回を含む）は db.MaxNotifyAttempts に対応し、2つは一緒に変えなければならない。
var notifyBackoff = []time.Duration{
	time.Second,
	5 * time.Second,
	30 * time.Second,
}

// 전달 실패·지연 사유. notification_deliveries.last_error 에 저장되어
// notify_api.go 의 전달 이력 응답을 거쳐 알림 페이지 「전달 기록」 표(delivery-list.tsx)에
// 그대로 표시된다. 따라서 사용자 노출 문구라 한국어로 둔다(진단 로그 log.Printf 는 원문 보존, Z2).
const (
	errDeliveryChannelKindUnregistered = "등록되지 않은 채널 유형입니다: %q"
	errDeliverySnapshotUnrenderable    = "이벤트 스냅샷을 해석하지 못해 이 취약점을 메시지로 만들지 못했습니다"
	errDeliveryChannelLengthCapped     = "이 메시지가 채널 길이 상한에 도달해 앞의 %d건만 전송했고, 나머지는 다음 배치로 미룹니다"
	errDeliveryNoDeliveredCount        = "채널이 전송 건수를 보고하지 않았습니다(delivered=%d)"
	errDeliveryRetryExhausted          = "%d회 재시도 후에도 실패했습니다: %s"
	errDeliverySnapshotEmpty           = "전달 항목 #%d 의 이벤트 스냅샷이 비어 있습니다"
	errDeliverySnapshotParse           = "전달 항목 #%d 의 이벤트 스냅샷을 해석하지 못했습니다: %w"
	errDeliveryBatchAllUnparseable     = "요약 배치의 전달 %d건을 모두 해석하지 못했습니다"
)

// Notifier は脆弱性の通知の配信エンジン。
//
// Scheduler と並び、独立した goroutine として動く（server.New を参照）。あえて
// Scheduler の tick を再利用しない：通知のリアルタイム性の要求（3 秒）はトリガーの業務のリズムと違い、
// 両者の失敗は互いに巻き込まない——通知が詰まっても agent の起動に影響すべきでない。
type Notifier struct {
	s  *Server
	pg *db.DB

	// mu は buckets を守る。チャンネルの数は少なく競合も少ないので、相互排他のロック1つで足り、
	// そのためにより細かい構造を持ち込む価値はない。
	mu      sync.Mutex
	buckets map[int64]*notifyBucket
}

// notifyBucket はチャンネル1つのトークンバケット。
//
// 「1分ごとに数えてゼロに戻す」スライディングウィンドウでなくトークンバケットを使うのは、前者の境目の効果がひどいから：
// 窓の終わりに 20 件送り切り、次の瞬間にまた 20 件送ると、プラットフォームから見れば1秒で 40 件になり、
// レート制限に当たる；トークンバケットは一定の速さで補充するので、こうした突発を自然に避けられる。
type notifyBucket struct {
	tokens   float64
	lastFill time.Time
}

func newNotifier(s *Server) *Notifier {
	return &Notifier{s: s, pg: s.m.pg, buckets: map[int64]*notifyBucket{}}
}

// Run は ctx が終わるまでループする。server.New が1回起動する。
func (n *Notifier) Run(ctx context.Context) {
	if n.pg == nil {
		return
	}
	t := time.NewTicker(notifyTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.step(ctx)
		}
	}
}

// step は1ラウンドを実行する：まず新しいイベントを振り分け、それから期限の来たタスクを配信する。
//
// どの手順が失敗してもログに記録するだけで、ループは止めない——通知のシステムの障害を決してプロセス単位の問題に格上げしてはならない。
// どの tick も独立していて、次のラウンドで自然に再試行する。
func (n *Notifier) step(ctx context.Context) {
	if !n.enabled() {
		return
	}
	if _, _, err := n.pg.FanOutPendingEvents(ctx, notifyFanOutPerTick); err != nil {
		log.Printf("[notify] イベントを振り分けられなかった: %v", err)
		return
	}
	channels, err := n.pg.ListNotificationChannels(ctx)
	if err != nil {
		log.Printf("[notify] チャンネルを読めなかった: %v", err)
		return
	}
	baseURL := n.publicBaseURL()
	for _, ch := range channels {
		if !ch.IsEnabled() {
			continue
		}
		// トークンバケットの数える単位は**メッセージの件数**（HTTP のリクエストの数と同じ）で、脆弱性の件数ではない。
		// リアルタイムのモードでは2つは同じ（脆弱性1件にメッセージ1件）；まとめのモードでは脆弱性の一括を
		// 1件のメッセージにまとめるので、トークンを1つしか使わない。
		//
		// どちらのモードも先にトークンバケットに聞いてから、その分だけ取る——順序を逆にはできない。さもないとレート制限で止められた
		// 配信がすでに再試行の回数を使ってしまう。
		now := time.Now()
		if ch.Mode == db.NotifyModeDigest {
			tokens, claimLimit := digestTickPlan()
			if n.takeTokens(ch.ID, ch.RatePerMin, tokens, now) <= 0 {
				continue
			}
			n.stepDigest(ctx, ch, claimLimit, baseURL)
			continue
		}
		allow := n.takeTokens(ch.ID, ch.RatePerMin, notifyMaxSendsPerChannelPerTick, now)
		if allow <= 0 {
			continue
		}
		n.stepRealtime(ctx, ch, allow, baseURL)
	}
}

// digestTickPlan はまとめのチャンネルのこのラウンドのトークンの消費と、一括の大きさの上限を返す。
//
// 2つの戻り値は**別の次元の量**で、まさにそれが独立した関数にした理由：
//
//   - tokens はメッセージの件数。脆弱性の一括を1件のメッセージにまとめ、HTTP のリクエストを1回送るので、常に 1。
//     そのため rate_per_min は digest にも引き続き効く（1分あたり最大でその件数のまとめのメッセージ）。
//   - claimLimit はこの一括に脆弱性を最大何件入れるか。メモリの上限にだけ縛られ、リクエストの予算とは関係ない。
//
// 以前、rate_per_min を digest に効かせるために、ラウンドごとのリクエストの予算
// （notifyMaxSendsPerChannelPerTick。リースから逆算したもの）をそのまま一括の大きさとして渡していた。
// その結果、rate_per_min=20 のチャンネルは 3 秒の tick でトークンが 1 つしか補充されず、まとめの
// メッセージ1件に脆弱性が1件しか入らなかった——digest は「まとめの文の付いたリアルタイムの通知」に退化し、読み手が受け取るのは
// 「直近 30 分で脆弱性が 1 件増えた」の連続で、db.MaxDigestBatchSize には決して届かなかった。
//
// この症状は端から端までのテストでは見つけにくい（既存のケースはどれも手で十分に大きな limit を
// stepDigest に渡し、step の中の枠の計算を迂回している）ので、判断をここにまとめて
// TestDigestTickPlanDecouplesBatchSizeFromSendBudget で直接固定する。
func digestTickPlan() (tokens, claimLimit int) {
	return 1, db.MaxDigestBatchSize
}

// stepRealtime はあるチャンネルのリアルタイムのタスクを取って配信する。脆弱性1件にメッセージ1件。
func (n *Notifier) stepRealtime(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	deliveries, err := n.pg.ClaimRealtimeDeliveries(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] リアルタイムの配信を取れなかった channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf(trMsg(errDeliveryChannelKindUnregistered), ch.Kind))
		return
	}
	for _, dl := range deliveries {
		msg, err := n.renderSingle(ctx, dl, baseURL)
		if err != nil {
			// 描画の失敗はローカルのデータの問題で、再試行してもよくならない。
			_ = n.pg.FailDeliveries(ctx, []int64{dl.ID}, err.Error())
			continue
		}
		n.send(ctx, channel, cfg, msg, []*db.NotificationDelivery{dl})
	}
}

// stepDigest は一括の期限が来たとき、あるチャンネルの送信待ちの配信を1件のメッセージにまとめて送る。
func (n *Notifier) stepDigest(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	window := n.digestInterval()
	due, err := n.pg.DigestBatchDue(ctx, ch.ID, window)
	if err != nil {
		log.Printf("[notify] まとめの一括を判定できなかった channel=%d: %v", ch.ID, err)
		return
	}
	if !due {
		return
	}
	deliveries, err := n.pg.ClaimDigestBatch(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] まとめの一括を取れなかった channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf(trMsg(errDeliveryChannelKindUnregistered), ch.Kind))
		return
	}
	msg, included, err := n.renderBatch(ctx, deliveries, baseURL, int(window.Minutes()))
	if err != nil {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), err.Error())
		return
	}
	// スナップショットが壊れていてメッセージに入れられなかった配信は、明示的に失敗と判定しなければならない。そうしないとそれらは
	// included の外に残り、メッセージにも失敗の一覧にも入らない——送信に成功したとき、その状態は
	// 後の一括の印付けから漏れ、リースが切れて何度も取られるまでずっと sending のままになる。
	if skipped := excludeDeliveries(deliveries, included); len(skipped) > 0 {
		reason := errDeliverySnapshotUnrenderable
		if fErr := n.pg.FailDeliveries(ctx, deliveryIDs(skipped), reason); fErr != nil {
			log.Printf("[notify] 壊れたスナップショットの配信に失敗の印を付けられなかった channel=%s ids=%v: %v", ch.Kind, deliveryIDs(skipped), fErr)
		}
		log.Printf("[notify] スナップショットを解析できない配信 %d 件を飛ばした channel=%d", len(skipped), ch.ID)
	}
	// メッセージに入ったものだけを send に渡す：included[i] と msg.Items[i] は厳密に対応し、
	// send はこの対応で「チャンネルが先頭の K 件を入れたと報告した」ことを正しい配信の行に反映する。
	n.send(ctx, channel, cfg, msg, included)
}

// send は配信し、結果に応じて状態を進める。
//
// 同じ一括の配信（まとめのモードでは数十件になることがある）は1つの送信の結果を共有する：届くか、一括まるごと再試行するか。
// 1件ずつの再試行はしない——まとめのメッセージは1件なので、その一部を送り直すと一括の意味が乱れる。
//
// 唯一の例外は**チャンネルの長さの上限による分割**：チャンネルが実際には先頭の K 件しか入らなかったと報告したら、
// K+1 件目からは次の一括に残さなければならず、一緒に成功の印を付けてはならない。さもないと切られた
// 脆弱性はメッセージにも失敗の一覧にもなく、完全に消えてしまう。
func (n *Notifier) send(ctx context.Context, channel notify.Channel, cfg map[string]any, msg notify.Message, deliveries []*db.NotificationDelivery) {
	// 1回の配信に上限を設け、あるチャンネルが詰まってこのラウンドの残りのチャンネルをすべて引き止めるのを避ける。
	sendCtx, cancel := context.WithTimeout(ctx, notifySendTimeout)
	defer cancel()
	delivered, err := channel.Send(sendCtx, cfg, msg)
	if err == nil && delivered > 0 {
		if delivered > len(deliveries) {
			// チャンネルが報告する件数が配信の数を超えることはありえない；本当に起きたら描画の層の計算違いなので、
			// すべて届いたものとして扱い問題を記録する。記録を乱すよりずっとよい。
			log.Printf("[notify] チャンネルが報告した届いた件数 %d が配信の数 %d を超えた channel=%s。すべて届いたものとして扱う",
				delivered, len(deliveries), channel.Kind())
			delivered = len(deliveries)
		}
		sent, rest := deliveries[:delivered], deliveries[delivered:]
		if err := n.pg.MarkDeliveriesSent(ctx, deliveryIDs(sent)); err != nil {
			log.Printf("[notify] 届いた印を付けられなかった channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(sent), err)
		}
		if len(rest) > 0 {
			// このメッセージはチャンネルの長さの上限に達した：残りはすぐ待ち行列に戻し、次の tick で続きを送る。
			// RescheduleDeliveries でなく DeferDeliveries を使う —— これは失敗ではないので、
			// 再試行の予算を使うべきでない（取るときに楽観的に +1 しているので、そこで戻す）。
			if err := n.pg.DeferDeliveries(ctx, deliveryIDs(rest),
				fmt.Sprintf(trMsg(errDeliveryChannelLengthCapped), delivered)); err != nil {
				log.Printf("[notify] 分割の続きを待ち行列に入れられなかった channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(rest), err)
			}
		}
		return
	}
	if err == nil {
		// チャンネルはエラーも報告せず、何件届いたかも言わなかった。失敗として扱い（バックオフを通る）、
		// この配信が何度も取られるのに決して印を付けられない状態を避ける。
		err = fmt.Errorf(trMsg(errDeliveryNoDeliveredCount), delivered)
	}

	// 失敗の扱いは**1件ずつ**決め、一括の最大の試行回数では判断しない。
	//
	// 以前は `if maxAttempts(deliveries) >= MaxNotifyAttempts` で一括まるごと失敗にしていたが、一括の中の
	// 各件の試行回数は同じではない：すでに2回再試行した古い配信（attempts=2）が同じ一括の
	// 真新しい配信（attempts=1）を一緒に failed に引きずり込む——新しい脆弱性は1回も再試行しないまま永久に失われ、
	// 「古い行に新しい行を巻き込ませない」という本来の意図とちょうど逆になる。
	permanent := notify.IsPermanent(err)
	var failIDs, exhaustedIDs []int64
	byDelay := map[time.Duration][]int64{}
	for _, dl := range deliveries {
		switch {
		case permanent:
			failIDs = append(failIDs, dl.ID)
		case dl.Attempts >= db.MaxNotifyAttempts:
			exhaustedIDs = append(exhaustedIDs, dl.ID)
		default:
			delay := notifyBackoff[min(dl.Attempts, len(notifyBackoff)-1)]
			byDelay[delay] = append(byDelay[delay], dl.ID)
		}
	}

	if len(failIDs) > 0 {
		if fErr := n.pg.FailDeliveries(ctx, failIDs, err.Error()); fErr != nil {
			log.Printf("[notify] 失敗の状態を付けるときにエラー channel=%s ids=%v: %v", channel.Kind(), failIDs, fErr)
		}
	}
	if len(exhaustedIDs) > 0 {
		reason := fmt.Sprintf(trMsg(errDeliveryRetryExhausted), db.MaxNotifyAttempts, err)
		if fErr := n.pg.FailDeliveries(ctx, exhaustedIDs, reason); fErr != nil {
			log.Printf("[notify] 失敗の状態を付けるときにエラー channel=%s ids=%v: %v", channel.Kind(), exhaustedIDs, fErr)
		}
	}
	// 遅れごとにまとめて並べ直す：バックオフは 3 段しかないので、まとまりの数はもともと小さく、1件ずつ
	// UPDATE を送る必要はない（そうすると 500 件の一括で 500 回の往復が生じる）。
	for delay, group := range byDelay {
		if rErr := n.pg.RescheduleDeliveries(ctx, group, delay, err.Error()); rErr != nil {
			log.Printf("[notify] 配信を並べ直せなかった channel=%s ids=%v: %v", channel.Kind(), group, rErr)
		}
	}
	if len(failIDs)+len(exhaustedIDs) > 0 {
		log.Printf("[notify] 配信に失敗 channel=%d kind=%s 永久に失敗=%d 再試行を使い切った=%d 再試行待ち=%d: %s",
			deliveries[0].ChannelID, channel.Kind(), len(failIDs), len(exhaustedIDs), len(byDelay), err)
	}
}

// excludeDeliveries は all のうち keep にないもの（ポインターの同一性で比べる）を返す。
// 「メッセージに入れられなかった」配信を見つけるのに使う——それらは明示的に処置しなければならず、灰色の領域に残してはならない。
func excludeDeliveries(all, keep []*db.NotificationDelivery) []*db.NotificationDelivery {
	inKeep := make(map[*db.NotificationDelivery]bool, len(keep))
	for _, dl := range keep {
		inKeep[dl] = true
	}
	var out []*db.NotificationDelivery
	for _, dl := range all {
		if !inKeep[dl] {
			out = append(out, dl)
		}
	}
	return out
}

// adapt はチャンネルの実装を取り、その設定を解析する。
// ok=false を返したら種類が登録されていないことを表し、配信は無限に再試行せずすぐ失敗と判定する。
func (n *Notifier) adapt(ch *db.NotificationChannel) (notify.Channel, map[string]any, bool) {
	channel, ok := notify.Get(ch.Kind)
	if !ok {
		return nil, nil, false
	}
	var cfg map[string]any
	if len(ch.Config) > 0 {
		// 設定の解析に失敗したら空の map を渡す：チャンネル自身の Validate が「どのフィールドが足りないか」を報告し、
		// そのエラーのほうが JSON の解析のエラーよりユーザーが直す手がかりになる。
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return channel, cfg, true
}

// renderSingle は脆弱性1件のメッセージを描画する。
func (n *Notifier) renderSingle(ctx context.Context, dl *db.NotificationDelivery, baseURL string) (notify.Message, error) {
	snap, err := parseSnapshot(dl)
	if err != nil {
		return notify.Message{}, err
	}
	item, err := n.itemFor(ctx, snap, baseURL)
	if err != nil {
		return notify.Message{}, err
	}
	return notify.Message{Items: []notify.Item{item}, HomeURL: baseURL}, nil
}

// renderBatch はまとめのメッセージを描画する。スナップショットを1件ずつ解析する——1件が壊れたらその1件だけを飛ばし、
// それで一括のまとめ全体を失わないようにする。
//
// 戻り値の included と msg.Items は**厳密に1対1に対応する**（i 番目の配信 ↔ i 番目の項目）。
// この対応は必須の要件：呼び出し元は「チャンネルが先頭の K 件を入れたと報告した」ことで先頭の K 個の配信に
// 届いた印を付けると決める。ここで壊れたスナップショットを飛ばしたのに、飛ばした配信を included から除かないと、
// 添字がずれる——失敗すべき壊れた項目に届いた印が付き、正常な項目が届いていないと誤判定される。
// 壊れたものは呼び出し元が明示的に失敗の印を付ける。stepDigest を参照。
func (n *Notifier) renderBatch(ctx context.Context, deliveries []*db.NotificationDelivery, baseURL string, windowMinutes int) (notify.Message, []*db.NotificationDelivery, error) {
	items := make([]notify.Item, 0, len(deliveries))
	included := make([]*db.NotificationDelivery, 0, len(deliveries))
	for _, dl := range deliveries {
		snap, err := parseSnapshot(dl)
		if err != nil {
			// 壊れたスナップショットはメッセージにも included にも入れない——その処置は呼び出し元が受け持つ
			// （「届いた」に紛れ込ませてごまかすのでなく、明示的に失敗の印を付ける）。
			log.Printf("[notify] まとめの一括で解析できないスナップショットを飛ばした delivery=%d: %v", dl.ID, err)
			continue
		}
		item, err := n.itemFor(ctx, snap, baseURL)
		if err != nil {
			return notify.Message{}, nil, err
		}
		items = append(items, item)
		included = append(included, dl)
	}
	if len(items) == 0 {
		return notify.Message{}, nil, fmt.Errorf(trMsg(errDeliveryBatchAllUnparseable), len(deliveries))
	}
	return notify.Message{
		Items:         items,
		Batch:         true,
		WindowMinutes: windowMinutes,
		HomeURL:       baseURL,
	}, included, nil
}

// itemFor はイベントのスナップショットを通知待ちの項目に描画し、ついでに資産の名前と詳細へ戻るリンクを解決する。
func (n *Notifier) itemFor(ctx context.Context, snap notify.Snapshot, baseURL string) (notify.Item, error) {
	assets, err := n.pg.NotificationAssetNames(ctx, snap.AssetIDs)
	if err != nil {
		// 資産の名前の解決に失敗しても通知を止めるべきでない：名前が読めないのは通知が届かないよりずっと軽く、
		// メッセージの資産の行が1つ減るだけ。
		log.Printf("[notify] 資産の名前を解決できなかった finding=%d: %v", snap.FindingID, err)
	}
	item := notify.Item{
		FindingID:  snap.FindingID,
		Name:       snap.Name,
		VulnClass:  snap.VulnClass,
		Severity:   snap.Severity,
		Summary:    snap.Summary,
		Assets:     assets,
		FromStatus: snap.FromStatus,
		ToStatus:   snap.ToStatus,
	}
	if baseURL != "" {
		// 詳細のページのルートは web/src/app/(main)/function/findings/detail/page.tsx を参照。
		// それは query の引数 id から脆弱性の id を読む。
		item.DetailURL = fmt.Sprintf("%s/function/findings/detail?id=%d", baseURL, snap.FindingID)
	}
	return item, nil
}

// takeTokens はチャンネルのトークンバケットから**最大 want 個**のトークンを取り、実際に取れた数を返す。
//
// トークン1つ = メッセージ1件（HTTP のリクエスト1回）。リアルタイムのモードでは呼び出し元が欲しい件数をそのまま渡す；
// まとめのモードでは脆弱性の一括でメッセージを1件しか送らないので、1 を渡す。
//
// バケットの容量はそのチャンネルの1分あたりの上限で、一定の速さで補充する。ratePerMin<=0 はレート制限なしを表し、
// 有限だが十分に大きな値を返して、1ラウンドのループが無限の溜まりに引き止められるのを防ぐ。
//
// want の上限は必須：これがないとバケットを丸ごと空にするしかないが、呼び出し元にはラウンドごとの上限が別にあるので、
// 多く取ったトークンは使えないまま次の補充の前に消えてしまう——貯めた突発の容量には決して届かず、
// 「このラウンドに送信待ちの配信が1つもない」ときまで同じように差し引かれる。
func (n *Notifier) takeTokens(channelID int64, ratePerMin, want int, now time.Time) int {
	if want <= 0 {
		return 0
	}
	if ratePerMin <= 0 {
		return min(want, notifyUnlimitedBurstPerTick)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	b := n.buckets[channelID]
	if b == nil {
		b = &notifyBucket{tokens: float64(ratePerMin), lastFill: now}
		n.buckets[channelID] = b
	}
	// 実際に経った時間で補充し、速さは1秒あたり ratePerMin/60。
	if elapsed := now.Sub(b.lastFill).Seconds(); elapsed > 0 {
		b.tokens = minF(float64(ratePerMin), b.tokens+elapsed*float64(ratePerMin)/60)
		b.lastFill = now
	}
	// ごく小さな epsilon を足してから整数にする：トークンの数は浮動小数点の足し算でできるので、2回に分けて満たすと
	// 0.5 + 0.5 が 0.9999999999 になることがあり、そのまま int() すると 0 に切られる——
	// 数学的には満杯のバケットからトークンが取れない。1e-9 はトークン1つよりはるかに小さいので、本当の不足を見逃すことはない。
	take := min(int(b.tokens+1e-9), want)
	if take <= 0 {
		return 0
	}
	b.tokens -= float64(take)
	return take
}

// enabled は全体のスイッチを読む。
func (n *Notifier) enabled() bool {
	return n.pg.GetBool(settingNotifyEnabled, true)
}

// publicBaseURL は戻るリンクに使う外部のアドレスを、末尾のスラッシュを除いて返す。
func (n *Notifier) publicBaseURL() string {
	v, ok, err := n.pg.GetSetting(settingNotifyPublicBaseURL)
	if err != nil || !ok {
		return ""
	}
	return trimTrailingSlash(v)
}

// digestInterval はまとめの周期を返し、不正か未設定なら既定値に戻る。
func (n *Notifier) digestInterval() time.Duration {
	v, ok, err := n.pg.GetSetting(settingNotifyDigestMinutes)
	if err != nil || !ok {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	m := 0
	if _, err := fmt.Sscanf(v, "%d", &m); err != nil || m <= 0 {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	return time.Duration(m) * time.Minute
}

// parseSnapshot は配信に対応するイベントのスナップショットを解析する。
func parseSnapshot(dl *db.NotificationDelivery) (notify.Snapshot, error) {
	var snap notify.Snapshot
	if len(dl.Snapshot) == 0 {
		return snap, fmt.Errorf(trMsg(errDeliverySnapshotEmpty), dl.ID)
	}
	if err := json.Unmarshal(dl.Snapshot, &snap); err != nil {
		return snap, fmt.Errorf(trMsg(errDeliverySnapshotParse), dl.ID, err)
	}
	if snap.Kind == "" {
		// イベントの種類はイベントの行に従う。スナップショットのほうは古い版が書いたかもしれない。
		snap.Kind = dl.EventKind
	}
	return snap, nil
}

func deliveryIDs(deliveries []*db.NotificationDelivery) []int64 {
	out := make([]int64, 0, len(deliveries))
	for _, dl := range deliveries {
		out = append(out, dl.ID)
	}
	return out
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
