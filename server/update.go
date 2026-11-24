package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// ページからのワンクリックの更新の HTTP の面。本当のダウンロード/検証/入れ替えのロジックはすべて selfupdate パッケージにあり、
// ここは認証の境界、同時実行の排他、進み具合の配信、そして「もう終了してよい」と main に伝えることだけを受け持つ。
//
// 再起動はこのプロセスでは行わない：新しい版を一時保存したら、プロセスは selfupdate.ExitRestart で終了し、
// 見張りのスクリプト（start.sh / start.bat。Docker では ENTRYPOINT）が起動し直す。

// 사용자에게 노출되는 업데이트 메시지(SSE 진행 메시지·검사 사유·에러 응답).
// 프런트엔드(system/settings 의 update-card)가 progress.message·reason·writeErr
// 본문을 그대로 렌더하므로 한국어로 둔다. 로그·중국어 주석은 Z2(로그 최하위)라 범위 밖.
const (
	updateMsgPreparing          = "준비 중…"
	updateMsgFailed             = "업데이트 실패"
	updateMsgStaged             = "새 버전이 준비되었습니다. 재시작 중…"
	updateErrNotReleaseFmt      = "현재 버전 %q 은(는) 정식 릴리스 버전이 아니므로 원클릭 업데이트를 사용할 수 없습니다"
	updateErrAlreadyLatestFmt   = "이미 최신 버전입니다 (%s)"
	updateErrInProgress         = "이미 업데이트가 진행 중입니다"
	updateErrRollbackInProgress = "업데이트가 진행 중이어서 롤백할 수 없습니다"
)

// restartCh はアップグレードの準備ができたか、ロールバックが終わったら閉じ、main はそれを受けて ExitRestart で終了する。
var (
	restartOnce sync.Once
	restartCh   = make(chan struct{})
)

// RestartRequested は「終了して見張りのプロセスに起動し直させてほしい」ときに閉じる channel を返す。
func RestartRequested() <-chan struct{} { return restartCh }

func requestRestart() { restartOnce.Do(func() { close(restartCh) }) }

// bootState は今回の起動時の selfupdate.Bootstrap の結論（アップグレードに成功 / ロールバックしたばかり /
// 一時保存したものを捨てた）で、main が注入し、/api/update/check が前回のアップグレードの結果をありのままにフロントエンドに伝えるのに使う。
var (
	bootStateMu sync.Mutex
	bootState   selfupdate.State
)

// SetBootUpdateState は main が起動時に1回呼ぶ。
func SetBootUpdateState(st selfupdate.State) {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	bootState = st
}

func bootUpdateState() selfupdate.State {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	return bootState
}

// releaseCache は GitHub の最新版の検索の結果をキャッシュする。
//
// 上部のバーの「新しい版がある」の案内はページ全体を読み込むたびに1回調べるが、認証していない GitHub の API は
// IP ごとに1時間 60 回まで——キャッシュしないと、タブをいくつか開いたりページを何回か更新したりしただけで枠を使い切り、
// その後本当に更新したいときにかえって調べられなくなる。ユーザーが明示的に「更新を確認」を押したときは force でキャッシュを迂回できる。
type releaseCache struct {
	mu  sync.Mutex
	rel *selfupdate.Release
	err error
	at  time.Time
	// fetch はデータを取る関数で、テストのためだけの注入口；nil なら本物の GitHub の検索を通る。
	fetch func(context.Context, *http.Client) (*selfupdate.Release, error)
}

const (
	releaseTTL = 30 * time.Minute
	// 失敗の結果も少しの間キャッシュする。さもないと GitHub に届かないとき、ページを読み込むたびにタイムアウトを1回待たされる；
	// ただし TTL は短くし、ネットワークが戻ったらすぐ自然に直るようにする。
	releaseErrTTL = 2 * time.Minute
	// 検索に使うタイムアウト。NewClient の 30 分のタイムアウトは丸ごとのダウンロード用で、版を調べるのにそこまでは待てない。
	releaseTimeout = 20 * time.Second
)

var relCache = &releaseCache{}

// get は最新の Release を返し、キャッシュに当たればネットワークにはアクセスしない。
//
// データを取る間ずっとロックを持つ：同時のリクエストは同じ1回の検索の結果を並んで待ち、それぞれが GitHub をたたくことはない
// （ページを読み込んだばかりで複数のタブが同時に調べに来るときが、まさにレート制限に最も当たりやすい）。
func (c *releaseCache) get(ctx context.Context, client *http.Client, force bool) (*selfupdate.Release, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !force {
		ttl := releaseTTL
		if c.err != nil {
			ttl = releaseErrTTL
		}
		if !c.at.IsZero() && time.Since(c.at) < ttl {
			return c.rel, c.err
		}
	}

	fetch := c.fetch
	if fetch == nil {
		fetch = selfupdate.FetchLatest
	}
	ctx, cancel := context.WithTimeout(ctx, releaseTimeout)
	defer cancel()
	rel, err := fetch(ctx, client)
	// リクエストが取り消された（ユーザーがタブを閉じた）からといって GitHub に問題があるわけではないので、キャッシュに書かない。
	// さもないと次の訪問者が「取り消された」という訳のわからないエラーを受け取る。
	if err != nil && ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
		return c.rel, err
	}
	c.rel, c.err, c.at = rel, err, time.Now()
	return rel, err
}

// updateProgress はフロントエンドに送る進み具合の1件。
type updateProgress struct {
	Phase   selfupdate.Phase `json:"phase"`
	Percent int              `json:"percent"` // ダウンロードの段階でだけ意味がある；他は -1
	Message string           `json:"message"`
	Version string           `json:"version,omitempty"`
	Error   string           `json:"error,omitempty"`
}

// updateHub は1回のアップグレードの進み具合を持ち、SSE の購読者に配信する。
//
// running は排他も兼ねる：アップグレード中にもう一度 POST /api/update/apply したらそのまま 409 にし、
// 2つの goroutine が同じ artex.new に同時に書くのを避ける。
type updateHub struct {
	mu      sync.Mutex
	running bool
	cur     updateProgress
	subs    map[chan updateProgress]struct{}
}

var updHub = &updateHub{
	cur:  updateProgress{Phase: selfupdate.PhaseIdle, Percent: -1},
	subs: map[chan updateProgress]struct{}{},
}

// begin はアップグレードの権利を取る。すでに進行中なら false を返す。
func (h *updateHub) begin(version string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.running {
		return false
	}
	h.running = true
	h.cur = updateProgress{Phase: selfupdate.PhaseDownload, Percent: 0, Message: updateMsgPreparing, Version: version}
	h.fanout(h.cur)
	return true
}

// finish は1回のアップグレードを終える。err が nil なら一時保存に成功し、再起動を待っていることを表す。
func (h *updateHub) finish(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.running = false
	if err != nil {
		h.cur = updateProgress{Phase: selfupdate.PhaseFailed, Percent: -1, Message: updateMsgFailed, Error: err.Error(), Version: h.cur.Version}
	} else {
		h.cur = updateProgress{Phase: selfupdate.PhaseStaged, Percent: 100, Message: updateMsgStaged, Version: h.cur.Version}
	}
	h.fanout(h.cur)
}

func (h *updateHub) publish(ph selfupdate.Phase, pct int, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cur = updateProgress{Phase: ph, Percent: pct, Message: msg, Version: h.cur.Version}
	h.fanout(h.cur)
}

// fanout は h.mu を持っているときに呼ばなければならない。購読者の channel はバッファ付きで、満杯なら捨てる——
// 進み具合は捨ててよい一時的な情報で、詰まった SSE の接続1つにアップグレードそのものを止めさせては決してならない。
func (h *updateHub) fanout(p updateProgress) {
	for ch := range h.subs {
		select {
		case ch <- p:
		default:
		}
	}
}

func (h *updateHub) snapshot() (updateProgress, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cur, h.running
}

func (h *updateHub) subscribe() (<-chan updateProgress, func()) {
	ch := make(chan updateProgress, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			h.mu.Unlock()
			close(ch)
		})
	}
}

// updateCheck は GitHub の最新の正式版を調べて現在の版と比べる。
//
// フロントエンドも api.github.com に直接つなぐ（GitHub の CORS は *）が、**このインターフェースを正とする**：
// ダウンロードはバックエンドが行うので、バックエンドが GitHub にアクセスできてはじめて更新の話になる。ブラウザーはつながるのにサーバーはつながらない
// ことはよくある（サーバーが社内ネットワークにある、またはプロキシをブラウザーにだけ設定した）ので、そのとき更新を押せば必ず失敗する。
// それなら確認の段階でありのままにエラーを返すほうがよい。
func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	current := BuildVersion
	mode := "binary"
	if selfupdate.InDocker() {
		mode = "docker"
	}
	boot := bootUpdateState()
	out := map[string]any{
		"current":     current,
		"mode":        mode,
		"os":          runtime.GOOS,
		"arch":        runtime.GOARCH,
		"has_backup":  selfupdate.HasBackup(),
		"repo":        selfupdate.Repo,
		"boot_notice": boot.Detail,
		"rolled_back": boot.RolledBack,
	}

	// 上部のバーの案内はキャッシュを通る（既定）；ユーザーが「更新を確認」を押したときは force=1 で強制的に元から取る。
	force := r.URL.Query().Get("force") != ""
	client := selfupdate.NewClient(s.m.GlobalProxy())
	rel, err := relCache.get(r.Context(), client, force)
	if err != nil {
		out["error"] = err.Error()
		writeJSON(w, 200, out)
		return
	}

	latest := rel.TagName
	out["latest"] = latest
	out["notes"] = rel.Body
	out["html_url"] = rel.HTMLURL
	if !rel.PublishedAt.IsZero() {
		out["published_at"] = rel.PublishedAt.Format(time.RFC3339)
	}

	asset := selfupdate.AssetName(latest, runtime.GOOS, runtime.GOARCH)
	out["asset"] = asset
	if a, ok := rel.FindAsset(asset); ok {
		out["asset_available"] = true
		out["size"] = a.Size
	} else {
		out["asset_available"] = false
	}

	cmp, comparable := selfupdate.CompareVersions(current, latest)
	out["comparable"] = comparable
	out["has_update"] = comparable && cmp < 0
	if !comparable {
		// 開発用のビルド（dev / git describe に接尾辞が付く）には比べられる版番号がない。通しても
		// ローカルでデバッグ中のバイナリを正式版で上書きするだけなので、そもそも更新させない。
		out["reason"] = fmt.Sprintf(updateErrNotReleaseFmt, current)
	}
	writeJSON(w, 200, out)
}

// updateApply は新しい版をダウンロードして一時保存し、終わったらプロセスを終了させて見張りのスクリプトに再起動を任せる。
//
// すぐに 202 を返し、実際の作業はバックグラウンドの goroutine で動く：丸ごとのダウンロードには数分かかることがあり、
// リクエストにぶら下げるとリバースプロキシのタイムアウトで切られる。進み具合は /api/update/stream を通る。
func (s *Server) updateApply(w http.ResponseWriter, r *http.Request) {
	current := BuildVersion

	// キャッシュを通る：インストールするのが、ユーザーが画面で見て確認したその版であることを保証する。
	client := selfupdate.NewClient(s.m.GlobalProxy())
	rel, err := relCache.get(r.Context(), client, false)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	cmp, comparable := selfupdate.CompareVersions(current, rel.TagName)
	if !comparable {
		writeErr(w, 400, fmt.Sprintf(updateErrNotReleaseFmt, current))
		return
	}
	if cmp >= 0 {
		writeErr(w, 400, fmt.Sprintf(updateErrAlreadyLatestFmt, current))
		return
	}
	if !updHub.begin(rel.TagName) {
		writeErr(w, 409, updateErrInProgress)
		return
	}

	go func() {
		// あえてリクエストの ctx ではなく s.ctx を使う：HTTP のレスポンスを返すとすぐリクエストは終わり、
		// それにぶら下げたダウンロードはすぐ取り消されてしまう。
		err := selfupdate.Stage(s.ctx, client, rel, current, func(ph selfupdate.Phase, pct int, msg string) {
			updHub.publish(ph, pct, msg)
		})
		updHub.finish(err)
		if err != nil {
			log.Printf("[update] 更新に失敗：%v", err)
			return
		}
		log.Printf("[update] %s → %s を一時保存した。入れ替えを終えるためにまもなく終了する", current, rel.TagName)
		// 最後の進み具合をフロントエンドに送る時間を少し残してから、終了を起動する。
		time.Sleep(1500 * time.Millisecond)
		requestRestart()
	}()

	writeJSON(w, 202, map[string]any{"ok": true, "target": rel.TagName})
}

// updateRollback は自分から前の版（入れ替えの前にバックアップした artex.old）に戻す。
func (s *Server) updateRollback(w http.ResponseWriter, r *http.Request) {
	if _, running := updHub.snapshot(); running {
		writeErr(w, 409, updateErrRollbackInProgress)
		return
	}
	if err := selfupdate.Rollback(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	log.Printf("[update] 手動で前の版にロールバックした。切り替えを終えるためにまもなく終了する")
	writeJSON(w, 202, map[string]any{"ok": true})
	go func() {
		time.Sleep(500 * time.Millisecond)
		requestRestart()
	}()
}

// updateStream は SSE で更新の進み具合を送る。
func (s *Server) updateStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, unsub := updHub.subscribe()
	defer unsub()

	send := func(p updateProgress) {
		b, _ := json.Marshal(p)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	// まず現在の状態を1件補い、ページを更新した後も進行中のアップグレードがすぐ見えるようにする。
	cur, _ := updHub.snapshot()
	send(cur)

	ctx := r.Context()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case p, ok := <-ch:
			if !ok {
				return
			}
			send(p)
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}
