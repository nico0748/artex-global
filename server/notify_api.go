package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// このファイルは通知の機能の HTTP のインターフェース。すべてのルートは requireAuth の後ろにかける（Handler() を参照）。
// 他の管理のインターフェースと同じ。

// notify_api.go 가 HTTP 응답으로 돌려주는 사용자 노출 오류 문구다. 한국어 UI 에서 알림
// 설정 요청이 실패하면 이 문구가 그대로 토스트로 뜨므로 한국어로 둔다. 식별자(id)·JSON
// 필드명·enum 값(realtime·digest)·채널 종류 키는 사용자가 설정을 고치는 데 쓰는 값이라
// 원문 그대로 둔다. %s·%q 가 든 상수는 fmt.Sprintf 형식 문자열이다. 用語: チャンネル→채널,
// 通知の送信→알림(크롬 nav "알림 발송"), 配信→전송, 送信のモード→발송 모드. 로그·주석은 BRIEF
// 방침상 최하위라 이 묶음 밖이다.
const (
	notifyErrBadJSON         = "요청 본문이 올바른 JSON 형식이 아닙니다: "
	notifyErrKindInvalidFmt  = "채널 유형이 올바르지 않습니다. 가능한 값: %s"
	notifyErrNameMissing     = "채널 이름을 입력하세요"
	notifyErrNameEmpty       = "채널 이름은 비워 둘 수 없습니다"
	notifyErrModeInvalid     = "발송 모드가 올바르지 않습니다. 가능한 값: realtime / digest"
	notifyErrRateNegative    = "전송 제한 값은 음수일 수 없습니다"
	notifyErrChannelID       = "채널 id 가 올바르지 않습니다"
	notifyErrKindUnregFmt    = "채널 유형 %q 는 등록되어 있지 않습니다"
	notifyErrDeliveryID      = "전송 id 가 올바르지 않습니다"
	notifyErrChannelNotFound = "알림 채널을 찾을 수 없습니다"
)

// 채널 연결을 점검할 때 보내는 테스트 메시지 본문이다. 사용자가 등록한 채널로 실제 발송되므로
// 한국어로 두되, 받는 사람이 실제 취약점으로 오해하지 않도록 한눈에 테스트임이 드러나게 한다.
const (
	notifyTestName    = "테스트 메시지 · 채널 설정 정상"
	notifyTestClass   = "연결 테스트"
	notifyTestSummary = "ARTEX 알림 채널 테스트 메시지입니다. 이 메시지를 받으셨다면 채널 설정이 정상입니다."
)

// notifyChannelDTO はチャンネルの外向きの表現。
//
// Config は**マスクした後の**設定：資格情報のフィールドは notify.MaskedPrefix で始まる値に置き換える。
// フロントエンドがマスクした値をそのまま送り返したら「このフィールドは変えていない」ことを表し、サーバーはそれをもとに DB の元の値を残す
// （notify.MergeConfig を参照）。
type notifyChannelDTO struct {
	ID         int64          `json:"id"`
	Name       string         `json:"name"`
	Kind       string         `json:"kind"`
	Enabled    bool           `json:"enabled"`
	Mode       string         `json:"mode"`
	Config     map[string]any `json:"config"`
	Filter     notify.Filter  `json:"filter"`
	RatePerMin int            `json:"rate_per_min"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
	// SecretKeys はどのフィールドが資格情報かをフロントエンドに知らせ、それをもとにパスワードの欄と「空なら変えない」の案内を描画する。
	// チャンネル自身が宣言し（notify.Channel.SecretKeys）、フロントエンドはチャンネルの知識をハードコードしない。
	SecretKeys []string `json:"secret_keys"`
}

// notifyDeliveryDTO は配信の履歴の外向きの表現。
type notifyDeliveryDTO struct {
	ID          int64      `json:"id"`
	FindingID   int64      `json:"finding_id,string"`
	EventKind   string     `json:"event_kind"`
	ChannelID   int64      `json:"channel_id"`
	ChannelName string     `json:"channel_name"`
	ChannelKind string     `json:"channel_kind"`
	State       string     `json:"state"`
	Attempts    int        `json:"attempts"`
	LastError   string     `json:"last_error"`
	BatchID     *int64     `json:"batch_id,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	SentAt      *time.Time `json:"sent_at,omitempty"`
	NextAttempt time.Time  `json:"next_attempt_at"`
	// メッセージのタイトルの要約で、履歴の一覧で開かなくてもこの通知が何を送ったかがわかるようにする。
	Title    string `json:"title"`
	Severity string `json:"severity"`
}

func toNotifyChannelDTO(ch *db.NotificationChannel) notifyChannelDTO {
	var cfg map[string]any
	if len(ch.Config) > 0 {
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	secrets := []string{}
	if c, ok := notify.Get(ch.Kind); ok {
		secrets = c.SecretKeys()
	}
	return notifyChannelDTO{
		ID:         ch.ID,
		Name:       ch.Name,
		Kind:       ch.Kind,
		Enabled:    ch.IsEnabled(),
		Mode:       ch.Mode,
		Config:     notify.MaskConfig(ch.Kind, cfg),
		Filter:     notify.ParseFilter(ch.Filter),
		RatePerMin: ch.RatePerMin,
		CreatedAt:  ch.CreatedAt,
		UpdatedAt:  ch.UpdatedAt,
		SecretKeys: secrets,
	}
}

func toNotifyDeliveryDTO(dl *db.NotificationDelivery) notifyDeliveryDTO {
	snap, _ := parseSnapshot(dl)
	dto := notifyDeliveryDTO{
		ID:          dl.ID,
		FindingID:   dl.FindingID,
		EventKind:   dl.EventKind,
		ChannelID:   dl.ChannelID,
		ChannelName: dl.ChannelName,
		ChannelKind: dl.ChannelKind,
		State:       dl.State,
		Attempts:    dl.Attempts,
		LastError:   dl.LastError,
		BatchID:     dl.BatchID,
		CreatedAt:   dl.CreatedAt,
		SentAt:      dl.SentAt,
		NextAttempt: dl.NextAttemptAt,
		Severity:    snap.Severity,
	}
	if snap.Name != "" {
		dto.Title = snap.Name
	} else {
		dto.Title = snap.VulnClass
	}
	return dto
}

// notifyMeta は通知のページに必要な静的なメタデータと全体の設定を返し、1回のリクエストでまとめて取る。
// フロントエンドがプルダウン1つを描画するために3回リクエストするのを避ける。
func (s *Server) notifyMeta(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	kinds := make([]map[string]any, 0, len(notify.Kinds()))
	for _, k := range notify.Kinds() {
		ch, _ := notify.Get(k)
		kinds = append(kinds, map[string]any{
			"kind":                 k,
			"default_rate_per_min": ch.DefaultRatePerMin(),
			"secret_keys":          ch.SecretKeys(),
		})
	}
	baseURL, _, _ := pg.GetSetting(settingNotifyPublicBaseURL)
	digest, _, _ := pg.GetSetting(settingNotifyDigestMinutes)
	stats, err := pg.NotificationStatsSnapshot(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"kinds":               kinds,
		"enabled":             pg.GetBool(settingNotifyEnabled, true),
		"public_base_url":     baseURL,
		"digest_interval_min": digest,
		"defaults": map[string]any{
			"digest_interval_min": notifyDefaultDigestMinutes,
		},
		"stats": stats,
	})
}

func (s *Server) notifyListChannels(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	channels, err := pg.ListNotificationChannels(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := make([]notifyChannelDTO, 0, len(channels))
	for _, ch := range channels {
		out = append(out, toNotifyChannelDTO(ch))
	}
	writeJSON(w, 200, map[string]any{"channels": out})
}

// notifyChannelRequest はチャンネルの作成/更新のリクエストの本文。
//
// 業務のフィールドはすべてポインターにし、「渡していない」と「ゼロ値を渡した」を区別できるようにする：PATCH の意味では、
// 渡していないフィールドは DB の元の値を残さなければならない。
type notifyChannelRequest struct {
	Name       *string        `json:"name"`
	Kind       *string        `json:"kind"`
	Enabled    *bool          `json:"enabled"`
	Mode       *string        `json:"mode"`
	Config     map[string]any `json:"config"`
	Filter     *notify.Filter `json:"filter"`
	RatePerMin *int           `json:"rate_per_min"`
}

func (s *Server) notifyCreateChannel(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req notifyChannelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, notifyErrBadJSON+err.Error())
		return
	}
	if req.Kind == nil || !notify.ValidKind(*req.Kind) {
		writeErr(w, 400, fmt.Sprintf(notifyErrKindInvalidFmt, strings.Join(notify.Kinds(), " / ")))
		return
	}
	name := ""
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
	}
	if name == "" {
		writeErr(w, 400, notifyErrNameMissing)
		return
	}
	channel, _ := notify.Get(*req.Kind)
	if err := channel.Validate(req.Config); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	ch := &db.NotificationChannel{
		Name:       name,
		Kind:       *req.Kind,
		Enabled:    req.Enabled,
		Mode:       db.NotifyModeRealtime,
		RatePerMin: channel.DefaultRatePerMin(),
	}
	if req.Mode != nil {
		if !db.ValidNotifyMode(*req.Mode) {
			writeErr(w, 400, notifyErrModeInvalid)
			return
		}
		ch.Mode = *req.Mode
	}
	if req.RatePerMin != nil {
		// 明示的に値を渡したらそのまま使う——0 も含む。それは「レート制限なし」を表す正当な設定。
		if *req.RatePerMin < 0 {
			writeErr(w, 400, notifyErrRateNegative)
			return
		}
		ch.RatePerMin = *req.RatePerMin
	}
	// 「フィールドを省略した」ときだけチャンネルの既定値を当てる。既定値は db の層でなくここで決めなければならない：
	// 「そのフィールドを渡していない」と「明示的に 0 を渡した」を区別できるのはリクエストの本文だけで、2つの意味はまったく違う
	// （前者=既定を使う、後者=レート制限なし）。db の層が 0 も未指定として扱うと、レート制限なしの設定に届かなくなる。
	if req.RatePerMin == nil {
		ch.RatePerMin = channel.DefaultRatePerMin()
	}
	if req.Filter != nil {
		// 書き込み時に値が限られた絞り込みのフィールド（min_severity など）を検証する。詳しくは notify.Filter.Validate：
		// しきい値を打ち間違えると絞り込みが黙って効かなくなり全部を送るので、入口で止めなければならない。
		if err := req.Filter.Validate(); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		raw, _ := json.Marshal(req.Filter)
		ch.Filter = raw
	}
	rawCfg, _ := json.Marshal(req.Config)
	ch.Config = rawCfg

	id, err := pg.SaveNotificationChannel(r.Context(), ch)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id})
}

func (s *Server) notifyUpdateChannel(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, notifyErrChannelID)
		return
	}
	current, err := pg.NotificationChannelByID(r.Context(), id)
	if err != nil {
		notifyChannelLookupErr(w, err)
		return
	}
	var req notifyChannelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, notifyErrBadJSON+err.Error())
		return
	}

	// kind は変えられるが、種類を変えるのは資格情報のフィールドを丸ごと入れ替えることを意味し、古い設定とはマージできない。
	kind := current.Kind
	if req.Kind != nil {
		if !notify.ValidKind(*req.Kind) {
			writeErr(w, 400, fmt.Sprintf(notifyErrKindInvalidFmt, strings.Join(notify.Kinds(), " / ")))
			return
		}
		kind = *req.Kind
	}
	channel, _ := notify.Get(kind)

	var stored map[string]any
	if kind == current.Kind {
		if len(current.Config) > 0 {
			_ = json.Unmarshal(current.Config, &stored)
		}
	}
	if stored == nil {
		stored = map[string]any{}
	}
	// 素の MergeConfig でなく PrepareConfigUpdate を使う：宛先のアドレスが変わったら、操作者に
	// 資格情報のフィールドについて改めて意思表示させなければならない。さもないと「アドレスだけ変えて資格情報はそのまま」で DB の本物の資格情報が新しいアドレスに送られてしまう。
	merged, err := notify.PrepareConfigUpdate(kind, stored, req.Config)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := channel.Validate(merged); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	rawCfg, _ := json.Marshal(merged)

	ch := &db.NotificationChannel{
		ID:         id,
		Name:       current.Name,
		Kind:       kind,
		Enabled:    current.Enabled,
		Mode:       current.Mode,
		Config:     rawCfg,
		Filter:     current.Filter,
		RatePerMin: current.RatePerMin,
	}
	if req.Name != nil {
		if ch.Name = strings.TrimSpace(*req.Name); ch.Name == "" {
			writeErr(w, 400, notifyErrNameEmpty)
			return
		}
	}
	if req.Enabled != nil {
		ch.Enabled = req.Enabled
	}
	if req.Mode != nil {
		if !db.ValidNotifyMode(*req.Mode) {
			writeErr(w, 400, notifyErrModeInvalid)
			return
		}
		ch.Mode = *req.Mode
	}
	if req.RatePerMin != nil {
		if *req.RatePerMin < 0 {
			writeErr(w, 400, notifyErrRateNegative)
			return
		}
		ch.RatePerMin = *req.RatePerMin
	}
	if req.Filter != nil {
		if err := req.Filter.Validate(); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		raw, _ := json.Marshal(req.Filter)
		ch.Filter = raw
	}

	// SaveNotificationChannel でなく SetNotificationChannelEnabled の経路を通るのは、
	// 「無効にする」と同時に既存の送信待ちの配信を skipped にし、有効に戻したときに
	// 古くなった溜まりのメッセージを一度に受け取るのを避けるため。
	enabledChanged := ch.Enabled != nil && current.Enabled != nil && *ch.Enabled != *current.Enabled
	if enabledChanged {
		// まず設定の更新を保存し（このとき enabled は古い値を使い、飛ばすロジックが早く起動するのを避ける）、
		// それから切り替えだけを変える。2つの手順の間に同時実行の隙間はない：このインターフェースがこの2つのフィールドを変える唯一の入口。
		prev := ch.Enabled
		ch.Enabled = current.Enabled
		if _, err := pg.SaveNotificationChannel(r.Context(), ch); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if err := pg.SetNotificationChannelEnabled(r.Context(), id, *prev); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"id": id})
		return
	}
	if _, err := pg.SaveNotificationChannel(r.Context(), ch); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id})
}

func (s *Server) notifyDeleteChannel(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, notifyErrChannelID)
		return
	}
	if err := pg.DeleteNotificationChannel(r.Context(), id); err != nil {
		notifyChannelLookupErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// notifyTestChannel は現在保存している設定でテストのメッセージを1件送る。
//
// 配信の待ち行列を通さずチャンネルの Send を直接呼ぶ：テストの目的はユーザーに「この設定で送れるか」をすぐ伝えることで、
// 待ち行列を通すと結果が配信の履歴に隠れ、ユーザーはもう一度めくらないと成功したかがわからない。
// そのためこのインターフェースは**同期**で、タイムアウトの上限は notify パッケージの HTTP クライアントが決める（15 秒）。
func (s *Server) notifyTestChannel(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, notifyErrChannelID)
		return
	}
	ch, err := pg.NotificationChannelByID(r.Context(), id)
	if err != nil {
		notifyChannelLookupErr(w, err)
		return
	}
	channel, ok := notify.Get(ch.Kind)
	if !ok {
		writeErr(w, 400, fmt.Sprintf(notifyErrKindUnregFmt, ch.Kind))
		return
	}
	var cfg map[string]any
	if len(ch.Config) > 0 {
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if err := channel.Validate(cfg); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	msg := notifyTestMessage(s.notifierBaseURL(pg))
	start := time.Now()
	// テストのメッセージは1件だけなので、届いた件数はここでは要らない（チャンネルの長さの上限は1件のメッセージについては
	// 切り詰めで受け止め、分割はかかわらない）。
	if _, err := channel.Send(r.Context(), cfg, msg); err != nil {
		// チャンネルが返した生のエラーをありのままにユーザーに返す——それが設定をデバッグする唯一の手がかり。
		writeErr(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok":         true,
		"latency_ms": time.Since(start).Milliseconds(),
	})
}

// notifyTestMessage はテストのメッセージを組み立てる。あえてひと目でテストとわかる内容にする：
// 受け取った人がそれを本物の脆弱性と取り違えてはならない。
func notifyTestMessage(baseURL string) notify.Message {
	return notify.Message{
		Items: []notify.Item{{
			FindingID: 0,
			Name:      notifyTestName,
			VulnClass: notifyTestClass,
			Severity:  "low",
			Summary:   notifyTestSummary,
			Assets:    []string{"artex.example.com"},
			DetailURL: baseURL,
		}},
		HomeURL: baseURL,
	}
}

// notifierBaseURL は戻るリンクに使う外部のアドレスを読む。
func (s *Server) notifierBaseURL(pg *db.DB) string {
	v, _, _ := pg.GetSetting(settingNotifyPublicBaseURL)
	return trimTrailingSlash(v)
}

func (s *Server) notifyListDeliveries(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	f := db.NotificationDeliveryFilter{
		State:     r.URL.Query().Get("state"),
		EventKind: r.URL.Query().Get("event_kind"),
	}
	if v := r.URL.Query().Get("channel_id"); v != "" {
		f.ChannelID = int64(atoiDefault(v, 0))
	}
	page := queryInt(r, "page", 1)
	pageSize := queryInt(r, "page_size", 50)
	items, total, err := pg.ListNotificationDeliveries(r.Context(), f, page, pageSize)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := make([]notifyDeliveryDTO, 0, len(items))
	for _, dl := range items {
		out = append(out, toNotifyDeliveryDTO(dl))
	}
	writeJSON(w, 200, map[string]any{"deliveries": out, "total": total, "page": page, "page_size": pageSize})
}

func (s *Server) notifyRetryDelivery(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, notifyErrDeliveryID)
		return
	}
	if err := pg.RetryNotificationDelivery(r.Context(), id); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// notifyChannelLookupErr は「チャンネルが存在しない」を 404 に、その他のエラーを 500 に訳す。
func notifyChannelLookupErr(w http.ResponseWriter, err error) {
	if errors.Is(err, db.ErrNotificationChannelNotFound) {
		writeErr(w, 404, notifyErrChannelNotFound)
		return
	}
	writeErr(w, 500, err.Error())
}
