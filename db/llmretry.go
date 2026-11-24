package db

import (
	"encoding/json"
	"time"
)

// LLM の再試行の方針：5層の再試行の「回数 + 間隔」の全体の設定。docs/LLM重试设计.md を参照。
// settings のテーブルの1つの JSON の値に保存する —— それはマシン全体で1組の実行のパラメータで、そのためにテーブルを1つ作るほどではない；
// 読み取りは組み込みの既定を支えにするので、キーがない(まっさらな DB/一度も設定していない)ときの挙動は定数を決め打ちしていた時代とまったく同じ。

const settingLLMRetryPolicy = "llm_retry_policy"

// RetryRule is one layer's knob pair. The zero value means "unset":
//
//	Attempts   0 = 組み込みの既定の回数; -1 = この層の再試行をオフ; >0 = その値
//	IntervalMS 0 = この層の元の間隔の方針(ふつうは指数バックオフ); >0 = 固定のミリ秒の間隔を使う
//
// -1 は「0 回」ではなく「明示的にオフ」。0 はもう「未設定」が使っているから。
type RetryRule struct {
	Attempts   int `json:"attempts"`
	IntervalMS int `json:"interval_ms"`
}

// Interval returns the configured fixed interval, or 0 when unset (caller keeps
// its own default ladder).
func (r RetryRule) Interval() time.Duration {
	if r.IntervalMS <= 0 {
		return 0
	}
	return time.Duration(r.IntervalMS) * time.Millisecond
}

// Or returns the rule with each unset field filled in from fallback. Used to
// layer a profile override on top of the global policy field by field, so a
// profile that only pins the interval still inherits the global count.
func (r RetryRule) Or(fallback RetryRule) RetryRule {
	if r.Attempts == 0 {
		r.Attempts = fallback.Attempts
	}
	if r.IntervalMS == 0 {
		r.IntervalMS = fallback.IntervalMS
	}
	return r
}

// retry knob bounds. A count above the cap turns a blip into a token bonfire;
// an interval above an hour outlives any transient failure worth waiting out.
const (
	maxRetryAttempts   = 20
	maxRetryIntervalMS = 3600_000 // 1h
)

// Clamped returns the rule with out-of-range values pulled back into the sane
// band (attempts within [-1, 20], interval within [0, 1h]).
func (r RetryRule) Clamped() RetryRule {
	if r.Attempts < -1 {
		r.Attempts = -1
	}
	if r.Attempts > maxRetryAttempts {
		r.Attempts = maxRetryAttempts
	}
	if r.IntervalMS < 0 {
		r.IntervalMS = 0
	}
	if r.IntervalMS > maxRetryIntervalMS {
		r.IntervalMS = maxRetryIntervalMS
	}
	return r
}

// Clamped bounds a profile's override the same way the global policy is bounded,
// so a hand-crafted API payload can't land a value the CHECK constraint rejects.
func (o RetryOverride) Clamped() RetryOverride {
	o.Connect, o.Empty, o.Stream = o.Connect.Clamped(), o.Empty.Clamped(), o.Stream.Clamped()
	return o
}

// LLMRetryPolicy holds the 5層の retry configuration. Connect/Empty/Stream are the
// per-request layers (a profile may override them, see LLMProfile.Retry);
// Breaker and Intent are process-wide by nature and live only here.
type LLMRetryPolicy struct {
	// Connect：SDK の接続の再試行(接続のリセット/タイムアウト/429/5xx、ストリームが始まる前)。既定 3 回、指数バックオフ。
	Connect RetryRule `json:"connect"`
	// Empty：SDK の空の応答の再試行(完了したのに content block がない。openai の形式だけ)。既定 2 回、指数バックオフ。
	Empty RetryRule `json:"empty"`
	// Stream：同じ provider の安全の窓の再試行(出力を渡す前にストリームが切れたときの再生)。既定 2 回、0.5s からの指数(上限 4s)。
	Stream RetryRule `json:"stream"`
	// Breaker：ローテーションのサーキットブレーカー。Attempts=一時的な失敗が何回続いたら遮断するか(既定 3、-1=一時的な失敗では遮断しない。
	// 残高不足/鍵の失効のような決定的な失敗はやはりすぐ遮断する)；IntervalMS=固定の冷却の長さ(0=既定の 1/5/30min の段階)。
	Breaker RetryRule `json:"breaker"`
	// Intent：worker が model_error で終わった後に意図をまるごとやり直す。既定 2 回、固定 3s。
	Intent RetryRule `json:"intent"`
}

// Clamped returns the policy with every rule clamped.
func (p LLMRetryPolicy) Clamped() LLMRetryPolicy {
	p.Connect, p.Empty, p.Stream = p.Connect.Clamped(), p.Empty.Clamped(), p.Stream.Clamped()
	p.Breaker, p.Intent = p.Breaker.Clamped(), p.Intent.Clamped()
	return p
}

// LLMRetryPolicy reads the global retry policy. A missing or unparseable value
// yields the zero policy — i.e. every layer on its built-in default.
func (d *DB) LLMRetryPolicy() LLMRetryPolicy {
	var p LLMRetryPolicy
	if d == nil {
		return p
	}
	raw, ok, err := d.GetSetting(settingLLMRetryPolicy)
	if err != nil || !ok || raw == "" {
		return p
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return LLMRetryPolicy{}
	}
	return p.Clamped()
}

// SetLLMRetryPolicy persists the global retry policy (values are clamped first).
func (d *DB) SetLLMRetryPolicy(p LLMRetryPolicy) error {
	raw, err := json.Marshal(p.Clamped())
	if err != nil {
		return err
	}
	return d.SetSetting(settingLLMRetryPolicy, string(raw))
}
