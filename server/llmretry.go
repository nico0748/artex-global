package server

import (
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// 再試行の方針のサーバー側の解決。docs/LLM重试设计.md を参照。5層のうち：
//   - 接続の確立 / 空のレスポンス / 同じ provider の安全な窓 は「エンドポイントについて回る」もので、LLM の設定ごとに
//     全体の既定を上書きできる（profile のある項目が空なら全体を引き継ぎ、全体も設定していなければ組み込みの既定を使う）；
//   - 遮断 / 意図のやり直し はプロセス単位で、全体に1つだけある。
//
// 全体の方針は DB の settings の1行を1回読むだけで、呼び出し箇所はどれも頻度の低い経路（provider の組み立て、work の締めくくり、
// 設定の保存）なので、さらにキャッシュの層を足す価値はない；遮断の引数は例外——失敗の経路で毎回読む必要があるので、
// applyRetryPolicy が Registry に送って保存する。

// retryPolicy reads the global policy; a nil DB yields the zero policy (all
// layers on their built-in defaults).
func (s *Server) retryPolicy() db.LLMRetryPolicy {
	if s.m == nil || s.m.pg == nil {
		return db.LLMRetryPolicy{}
	}
	return s.m.pg.LLMRetryPolicy()
}

// resolveRetry layers one profile's override on top of the global policy and
// converts the result into the form agent.Config carries. Rules combine field by
// field, so a profile that only pins an interval still inherits the global count.
func resolveRetry(o db.RetryOverride, pol db.LLMRetryPolicy) agent.RetryConfig {
	connect := o.Connect.Or(pol.Connect)
	empty := o.Empty.Or(pol.Empty)
	stream := o.Stream.Or(pol.Stream)
	return agent.RetryConfig{
		// 回数はここで「0=既定 / 負=オフ」の元の意味を保つ:SDK の MaxRetries /
		// EmptyResponseRetries はそれとまったく同じ形なので、SDK 自身に解決させればよい。
		ConnectAttempts: connect.Attempts, ConnectInterval: connect.Interval(),
		EmptyAttempts: empty.Attempts, EmptyInterval: empty.Interval(),
		StreamAttempts: stream.Attempts, StreamInterval: stream.Interval(),
	}
}

// applyProfileRetry fills cfg.Retry for a profile read from the DB.
func (s *Server) applyProfileRetry(cfg *agent.Config, p *db.LLMProfile) {
	if p == nil {
		return
	}
	cfg.Retry = resolveRetry(p.Retry, s.retryPolicy())
}

// 遮断(ローテーションの冷却)の既定値で、llmpool の組み込みのものと同じ —— ここでは「ユーザーが値を設定した」ときだけ上書きする。
// 意図のやり直しの既定値は engine.go の modelErrorRetries / modelErrorRetryBackoff を参照。

// applyRetryPolicy pushes the process-wide layers of the policy into the objects
// that consume them on a hot path: the circuit-breaker registry. Called at
// startup and whenever the policy is saved.
func (s *Server) applyRetryPolicy() {
	pol := s.retryPolicy()
	if s.llmHealth != nil {
		s.llmHealth.SetPolicy(pol.Breaker.Attempts, pol.Breaker.Interval())
	}
}

// modelErrorRetryPolicy resolves the intent-level replay knobs (layer ⑤): how
// many times a model_error work is re-run and how long to back off between runs.
func (e *Engine) modelErrorRetryPolicy() (retries int, backoff time.Duration) {
	retries, backoff = modelErrorRetries, modelErrorRetryBackoff
	if e == nil || e.m == nil || e.m.pg == nil {
		return retries, backoff
	}
	rule := e.m.pg.LLMRetryPolicy().Intent
	if rule.Attempts != 0 {
		retries = max(rule.Attempts, 0)
	}
	if d := rule.Interval(); d > 0 {
		backoff = d
	}
	return retries, backoff
}

// emptyTurnNudgeLimit resolves how many empty-turn continuations one work may
// inject (see steerHooks.Stop). It deliberately reuses layer ②'s knob —— 「空のレスポンスの
// 再試行の回数」:2つは同じことに対する2つの手段。SDK の層は「content block が1つもない」を扱い、手段は
// 同じリクエストをそのまま送り直すこと;ここは「思考だけで、本文もツールもない」を扱い、手段は指示を1つ足して
// モデルにすでにある思考を持ったまま続けさせること(そのまま送り直しても、コンテキストの形で決まるこの空回りには意味がない)。空の判定の基準が
// 違うのは、SDK が「イベントを yield したか」を基準にし、思考の差分そのものがイベントだから——しかしユーザーが
// 「空のレスポンスで何回再試行するか」を設定するときに言いたいのは「モデルが実質的な内容を出さなければもう1回」なので、2つの層が1つの回数を共有して
// はじめてその考え方に合う。
//
// ある profile の上書きでなく全体の方針を読む:1つの run の途中でフェイルオーバーで profile が替わることがあり、これは
// 意図全体の合計の門なので、エンドポイントが替わっても変わるべきでない。意味は SDK の emptyRetries() と同じ形:
// 0 = 既定の defaultEmptyTurnNudges;-1(負) = 空回りの続行をオフ;>0 = その値を使う。
func (e *Engine) emptyTurnNudgeLimit() int {
	if e == nil || e.m == nil || e.m.pg == nil {
		return defaultEmptyTurnNudges
	}
	switch n := e.m.pg.LLMRetryPolicy().Empty.Attempts; {
	case n == 0:
		return defaultEmptyTurnNudges
	case n < 0:
		return 0
	default:
		return n
	}
}
