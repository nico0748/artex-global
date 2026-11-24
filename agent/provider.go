// Package agent wires real LLM-driven planner and work agents (on top of the
// agent-core SDK) to the dual SQLite graph. See docs/ARTEX-架构设计.md
// §4.3 (planner) and §4.4 (work agent).
//
// Provider configuration is read from the environment so the system runs with
// any Anthropic- or OpenAI-format endpoint. If no key is configured, FromEnv
// returns ok=false and the exploration engine stays idle (an LLM is required).
package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/transcript"
)

// Config describes the LLM backend resolved from the environment.
type Config struct {
	Format  llm.Format
	BaseURL string
	APIKey  string
	Model   string
	// Proxy routes all LLM requests through the given proxy URL (http/https/socks5,
	// optionally with user:pass@ credentials). Empty means direct — it does NOT
	// fall back to the standard *_PROXY environment variables.
	Proxy string
	// RatePerSecond / RatePerMinute cap the shared request rate across ALL agents
	// using the provider (0 = that window unlimited).
	RatePerSecond float64
	RatePerMinute float64
	// ContextWindowK is the model's context window in K tokens (user-configured),
	// used to size compaction thresholds. 0 = default; see CompactionWindow.
	ContextWindowK int
	// ThinkingType は思考の「オン/オフ」のフィールド(thinking.type)を独立して制御する:
	//   "" = 送らない(既定。このフィールドに対応しないモデルとの互換); "disabled" = 明示的にオフ;
	//   "enabled" = オン. ReasoningEffort とは完全に切り離してある——thinking フィールドがなく、
	//   強度の引数だけで思考を有効にできるインターフェースもあるので、2つはそれぞれ単独で設定できる.
	ThinkingType string
	// ReasoningEffort は思考の「強度」のフィールドを独立して制御する:
	//   "" = 送らない(既定); "low"/"medium"/"high"/"xhigh"/"max" = 対応する強度.
	//   OpenAI では最上位の reasoning_effort に、Anthropic では output_config.effort に対応づける.
	ReasoningEffort string
	// Stream はこの profile がストリーミング(SSE)のインターフェースを使うかを制御する。true(既定)= ストリーミング;false = 本当の
	// 非ストリーミング(stream:false を送り、完全な JSON を一度に受け取り、Provider.Complete を通る)。非ストリーミングにすると
	// 一部のゲートウェイのひどい SSE の実装(空のフレーム、思考のフィールドのフレーム落ち)を避けられるが、代わりに実行中のリアルタイムの進み具合/リアルタイムの
	// token の数え上げを失う。agentcore.Options.NonStreaming = !Stream に対応づける。
	Stream bool
	// MaxTokens は1回の返答の出力の上限(token)。0 = このフィールドを送らず、サーバー側の既定値で決まる
	// (従来の挙動)。ContextWindowK とは違う:後者はモデルの全体の容量で、ローカルで圧縮のしきい値を計算するのにだけ使い、
	// リクエストには現れない;この値はリクエストのたびに送る。agentcore.Options.MaxTokens に対応づける。
	MaxTokens int
	// MaxTokensField は MaxTokens にどのリクエストのフィールド名を使うかを選ぶ。format=openai のときだけ効く:
	//   "" = max_tokens(既定); "max_completion_tokens" = 新しいフィールド。
	// OpenAI の推論モデル(o シリーズ/GPT-5)は後者しか認めず、max_tokens を受け取るとそのまま
	// unsupported_parameter を返す;一方、多くの互換ゲートウェイは前者しか認めないので、自動では推測せず、ユーザーがエンドポイントに応じて選ぶ。
	MaxTokensField string
	// SessionHeaderKey が空でないとき、LLM のリクエストのたびにカスタムの HTTP ヘッダーを付ける。ヘッダー名はこの値、
	// ヘッダーの値は【現在のセッションの session id】(chat のセッション=conv-<id>、worker=exp<x>-worker-i<intent>
	// など。WorkerSessionID を参照)。session-id ヘッダーでプロンプトのキャッシュ/スティッキーなルーティングをするゲートウェイ向け。
	// 空 = 送らない。値は transcript.WithSessionID でリクエストの context に載せ、RoundTripper が
	// 読み取って入れるので、同じ共有の provider でもセッションごとに違うヘッダーの値を送れる。
	SessionHeaderKey string
	// Retry はこの設定を解決した後の再試行の引数(profile の上書き → 全体の方針 → 組み込みの既定。
	// server 側で解決する)。3層の意味は RetryConfig を参照;ゼロ値 = 組み込みの既定をそのまま使う。
	Retry RetryConfig
}

// RetryConfig は LLM の設定1つについて回る再試行の引数。各層の「回数」の意味は共通:
// 0 = 組み込みの既定の回数を使う;負の数 = その層の再試行をオフ;>0 = その値を使う。各層の「間隔」:
// 0 = その層の元の指数バックオフを使う;>0 = 代わりにこの固定の間隔を使う。
type RetryConfig struct {
	// ConnectAttempts/ConnectInterval:SDK の接続の再試行(接続のリセット/タイムアウト/429/5xx、ストリームの開始前)、
	// そのまま llm.Config.MaxRetries / RetryInterval に対応づける。既定は 3 回、0.5s から指数(上限 8s)。
	ConnectAttempts int
	ConnectInterval time.Duration
	// EmptyAttempts/EmptyInterval:SDK の空のレスポンスの再試行(完了したが content block がない。openai
	// 形式だけ)、llm.Config.EmptyResponseRetries / EmptyResponseInterval に対応づける。
	// 既定は 2 回、同じ指数の段階。
	EmptyAttempts int
	EmptyInterval time.Duration
	// StreamAttempts/StreamInterval:同じ provider の安全な窓での再試行——このプロジェクトが SDK の上に足した
	// 層で、「まだ呼び出し元に何の出力も渡していない」ときにだけ、切れたストリーム/過負荷/ストリーム内の 429 をやり直す。SDK からは見えず、
	// server/task_llm.go が使う。既定は 2 回、0.5s から指数(上限 4s)。
	StreamAttempts int
	StreamInterval time.Duration
}

// compaction window resolution bounds (in K tokens). Below the floor the
// threshold math (window − summary reserve − buffer) would go non-positive and
// compaction would fire every turn; above the cap it would never fire.
const (
	defaultWindowK = 200  // unset → assume a 200K window (Claude default)
	minWindowK     = 32   // floor so effectiveWindow stays comfortably positive
	maxWindowK     = 1000 // cap at 1M tokens (user request)
)

// CompactionWindow returns the model context window in TOKENS for compaction
// thresholds, resolved from the user-configured size (ContextWindowK). 0/unset →
// a 200K default; otherwise clamped to [32K, 1M] so compaction stays effective.
func (c Config) CompactionWindow() int {
	k := c.ContextWindowK
	if k <= 0 {
		k = defaultWindowK
	}
	if k < minWindowK {
		k = minWindowK
	}
	if k > maxWindowK {
		k = maxWindowK
	}
	return k * 1000
}

// compactionConfig builds the agent-core compaction config for a context window
// in tokens. agentcore.NewSession wires the summarizer (same provider) when this
// is set on Options.Compaction.
func compactionConfig(windowTokens int) *compaction.Config {
	if windowTokens <= 0 {
		windowTokens = defaultWindowK * 1000
	}
	return &compaction.Config{ContextWindow: windowTokens}
}

// FromEnv resolves the LLM provider config:
//
//	ARTEX_LLM_PROVIDER = anthropic|openai (default: inferred from keys)
//	ARTEX_LLM_MODEL    = model id        (default: per provider)
//	ARTEX_LLM_BASE_URL = endpoint        (optional)
//	ARTEX_LLM_PROXY    = proxy URL        (optional; http/https/socks5)
//	ANTHROPIC_API_KEY / OPENAI_API_KEY         = credentials
func FromEnv() (Config, bool) {
	prov := os.Getenv("ARTEX_LLM_PROVIDER")
	anthKey := os.Getenv("ANTHROPIC_API_KEY")
	oaiKey := os.Getenv("OPENAI_API_KEY")

	if prov == "" {
		switch {
		case anthKey != "":
			prov = "anthropic"
		case oaiKey != "":
			prov = "openai"
		default:
			return Config{}, false
		}
	}

	c := Config{
		BaseURL: os.Getenv("ARTEX_LLM_BASE_URL"),
		Model:   os.Getenv("ARTEX_LLM_MODEL"),
		Proxy:   strings.TrimSpace(os.Getenv("ARTEX_LLM_PROXY")),
		// 既定はストリーミング;ARTEX_LLM_STREAM=false/0/off で明示的にオフにして非ストリーミングを通る。
		Stream: !isFalsy(os.Getenv("ARTEX_LLM_STREAM")),
	}
	switch prov {
	case "openai":
		c.Format = llm.FormatOpenAI
		c.APIKey = oaiKey
		if c.Model == "" {
			c.Model = "gpt-4o"
		}
	case "openai-responses":
		c.Format = llm.FormatOpenAIResponses
		c.APIKey = oaiKey
		if c.Model == "" {
			c.Model = "gpt-5"
		}
	default:
		c.Format = llm.FormatAnthropic
		c.APIKey = anthKey
		if c.Model == "" {
			c.Model = "claude-opus-4-8"
		}
	}
	if c.APIKey == "" {
		return Config{}, false
	}
	return c, true
}

// ConfigFrom builds a Config from UI-provided strings (provider defaults to
// anthropic; model defaults per provider). Inputs are trimmed and the base URL
// is normalized to the API base the provider expects (the provider appends the
// endpoint path itself), so a full endpoint URL is tolerated.
func ConfigFrom(provider, model, baseURL, apiKey, proxy string) Config {
	c := Config{
		Model:   strings.TrimSpace(model),
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		APIKey:  strings.TrimSpace(apiKey),
		Proxy:   strings.TrimSpace(proxy),
		Stream:  true, // 既定はストリーミング;呼び出し元が profile に応じて上書きする
	}
	switch strings.TrimSpace(provider) {
	case "openai":
		c.Format = llm.FormatOpenAI
		// provider appends "/chat/completions"; tolerate a full endpoint URL.
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/chat/completions"), "/")
		if c.Model == "" {
			c.Model = "gpt-4o"
		}
	case "openai-responses":
		c.Format = llm.FormatOpenAIResponses
		// provider appends "/responses"; tolerate a full endpoint URL.
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/responses"), "/")
		if c.Model == "" {
			c.Model = "gpt-5"
		}
	default:
		c.Format = llm.FormatAnthropic
		// provider appends "/v1/messages".
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/v1/messages"), "/")
		if c.Model == "" {
			c.Model = "claude-opus-4-8"
		}
	}
	return c
}

// isFalsy reports whether an env-var string explicitly requests "off". Empty or
// unrecognized → false (so an unset var keeps the streaming default).
func isFalsy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "0", "false", "off", "no":
		return true
	}
	return false
}

// Provider returns the short provider name ("anthropic"/"openai").
func (c Config) Provider() string {
	switch c.Format {
	case llm.FormatOpenAI:
		return "openai"
	case llm.FormatOpenAIResponses:
		return "openai-responses"
	}
	return "anthropic"
}

// NewProvider builds an llm.Provider from the config. When a rate is set, the
// limiter lives on the single provider instance — so planner + all workers +
// main agent (which share this provider) are bounded by one shared rate limit.
func (c Config) NewProvider() (llm.Provider, error) {
	client, err := quotaAwareHTTPClient(c.Proxy, c.SessionHeaderKey)
	if err != nil {
		return nil, err
	}
	lc := llm.Config{
		Format:     c.Format,
		BaseURL:    c.BaseURL,
		APIKey:     c.APIKey,
		Model:      c.Model,
		HTTPClient: client,
	}
	// 思考のオン/オフと強度の2つのフィールドはそれぞれそのまま渡す(空 = そのフィールドを送らない)。2つは切り離してある:
	// thinking.type だけ、effort だけ、両方、どちらも送らない、のいずれもできる。
	lc.ThinkingType = c.ThinkingType
	lc.ReasoningEffort = c.ReasoningEffort
	// 出力の上限のフィールド名の選択(空 = max_tokens を使う)。上限の「値」はここにはない:それはラウンドごとに
	// agentcore.Options.MaxTokens について回り、provider はそれをどのキーに入れるかだけを決める。
	lc.MaxTokensField = c.MaxTokensField
	// 再試行の引数は SDK と同じ意味(回数 0=既定/負=オフ、間隔 0=指数バックオフ/>0=固定)で、そのまま渡す。
	lc.MaxRetries = c.Retry.ConnectAttempts
	lc.RetryInterval = c.Retry.ConnectInterval
	lc.EmptyResponseRetries = c.Retry.EmptyAttempts
	lc.EmptyResponseInterval = c.Retry.EmptyInterval
	if c.RatePerSecond > 0 || c.RatePerMinute > 0 {
		lc.RateLimit = &llm.RateLimit{PerSecond: c.RatePerSecond, PerMinute: c.RatePerMinute}
	}
	return llm.NewProvider(lc)
}

// IsQuotaExhaustedMessage deliberately recognizes only explicit balance,
// billing, credit, or quota-exhaustion signals. Generic 429/rate-limit text,
// authentication failures, network errors, and server failures are excluded.
var nonFailoverHTTPStatus = regexp.MustCompile(`(?:status(?:\s+code)?|http(?:\s+status)?)\s*[=:]?\s*(?:401|403|5\d\d)\b`)
var transientQuotaLimit = regexp.MustCompile(`(?i)(?:\b(?:rpm|tpm|rpd|qps)\b|quota[_\s-]*metric|rate[_\s-]*limit|too many requests|(?:requests?|tokens?)\s+(?:per|/)\s*(?:second|minute)|(?:per|/)\s*(?:second|minute)\s+(?:requests?|tokens?)|generate[_\s-]*requests[_\s-]*per[_\s-]*(?:minute|second)|tokens?[_\s-]*per[_\s-]*(?:minute|second))`)

func IsQuotaExhaustedMessage(message string) bool {
	message = strings.ToLower(message)
	// Authentication/authorization and provider-side 5xx failures never rotate,
	// even when a gateway happens to echo a quota-looking phrase in the body.
	if nonFailoverHTTPStatus.MatchString(message) {
		return false
	}
	// Provider APIs frequently describe an ordinary rate limit as "quota
	// exceeded", especially Google-style responses containing a quota metric.
	// These limits recover with time and must stay on the current provider.
	if transientQuotaLimit.MatchString(message) {
		return false
	}
	markers := []string{
		"insufficient_quota", "quota_exceeded", "quota exceeded", "quota exhausted",
		"exceeded your current quota", "billing_hard_limit_reached",
		"billing hard limit", "billing_not_active", "credit balance", "insufficient credit",
		"insufficient balance", "balance is too low", "payment required", "status 402",
		"余额不足", "额度不足", "额度已用尽", "欠费",
	}
	for _, marker := range markers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	// gRPC RESOURCE_EXHAUSTED is overloaded for both account quota and ordinary
	// request-rate limiting. Preserve it as an explicit exhaustion signal only
	// when the same error does not identify a transient rate limit.
	return strings.Contains(message, "resource_exhausted") &&
		!strings.Contains(message, "rate limit") &&
		!strings.Contains(message, "too many requests")
}

// quotaAwareTransport preserves Norma's normal retry behavior except for a 429
// whose body explicitly says the account quota/balance is exhausted. Norma's
// retry loop treats every 429 as transient; normalizing only that response to
// 402 lets a task router fail over immediately while retaining the original
// response body for provider-specific classification and audit logs.
type quotaAwareTransport struct {
	base http.RoundTripper
	// sessionHeaderKey, when non-empty, is the HTTP header name each request
	// carries; its value is the session id read from the request context. Empty
	// disables it. See Config.SessionHeaderKey.
	sessionHeaderKey string
}

func (t quotaAwareTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Custom session-id header: name is user-configured, value is THIS run's
	// session id (norma stashes it on the context via transcript.WithSessionID).
	// Stable across a session's turns and distinct across sessions — exactly what
	// a session-keyed prompt cache wants. Skipped when no session id is present.
	if t.sessionHeaderKey != "" {
		if sid := transcript.SessionIDFrom(req.Context()); sid != "" {
			req.Header.Set(t.sessionHeaderKey, sid)
		}
	}
	// When LLM recording is on, the Recorder puts a Capture on the context so the
	// raw wire bodies can be persisted. This is the only layer that still sees
	// them: norma builds the request body internally and decodes the SSE response
	// before either reaches the recorder.
	capt := llmrec.CaptureFrom(req.Context())
	capt.SetRequest(requestBodySnapshot(req))

	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	// Tee rather than read: a 200 is an SSE stream that must keep streaming. The
	// 429 branch below reads through this wrapper, so its body lands in the
	// capture before being replaced.
	resp.Body = capt.TeeResponse(resp.StatusCode, resp.Body)

	if resp.StatusCode != http.StatusTooManyRequests {
		return resp, nil
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	if readErr != nil {
		return resp, nil
	}
	if IsQuotaExhaustedMessage(string(body)) {
		resp.StatusCode = http.StatusPaymentRequired
		resp.Status = "402 Payment Required"
	}
	return resp, nil
}

// requestBodySnapshot copies an outgoing request body without consuming it.
// norma builds every model request from a *bytes.Reader, so net/http populates
// GetBody and the copy has no effect on what gets sent.
func requestBodySnapshot(req *http.Request) string {
	if req.GetBody == nil {
		return ""
	}
	rc, err := req.GetBody()
	if err != nil {
		return ""
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return ""
	}
	return string(b)
}

func quotaAwareHTTPClient(proxy, sessionHeaderKey string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	proxy = strings.TrimSpace(proxy)
	if proxy == "" {
		transport.Proxy = nil // 空=直接つなぐ。HTTP_PROXY/HTTPS_PROXY の環境変数には戻らない
	} else {
		proxyURL, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("llm: invalid proxy %q: %w", proxy, err)
		}
		switch proxyURL.Scheme {
		case "http", "https", "socks5":
		case "":
			return nil, fmt.Errorf("llm: proxy %q missing scheme (use http://, https:// or socks5://)", proxy)
		default:
			return nil, fmt.Errorf("llm: unsupported proxy scheme %q (use http, https or socks5)", proxyURL.Scheme)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return &http.Client{Transport: quotaAwareTransport{base: transport, sessionHeaderKey: strings.TrimSpace(sessionHeaderKey)}}, nil
}

// logTestConnection prints the raw HTTP status code(s) and response body of a
// connection test to the server log, so "テストをクリック" leaves a diagnosable trail of
// exactly what the gateway returned — 401 bodies, quota text, empty frames — not
// just the collapsed ok/err the UI shows. Bodies are clipped to keep a chatty
// SSE stream from flooding the log.
func logTestConnection(c Config, capt *llmrec.Capture) {
	attempts := capt.Attempts()
	if len(attempts) == 0 {
		log.Printf("[llm-test] %s / %s @ %s — HTTP のリクエストを1つも送っていない(設定の解決か接続の確立の時点で失敗)",
			c.Provider(), c.Model, c.BaseURL)
		return
	}
	for i, a := range attempts {
		log.Printf("[llm-test] %s / %s @ %s — 試行 %d/%d HTTP %d\nレスポンスの本文: %s",
			c.Provider(), c.Model, c.BaseURL, i+1, len(attempts), a.Status, clipBody(a.Body))
	}
}

// clipBody trims a wire body for logging. 4K is plenty to show an error JSON or
// the head of an SSE stream while bounding a runaway response.
func clipBody(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(空)"
	}
	const max = 4096
	if len(s) > max {
		return s[:max] + fmt.Sprintf("…(切り詰め。全 %d バイト)", len(s))
	}
	return s
}

// TestConnection makes a minimal real completion to verify the provider/model/
// endpoint/key actually work. Returns the round-trip latency and the model's
// reply text.
func TestConnection(ctx context.Context, c Config) (time.Duration, string, error) {
	prov, err := c.NewProvider()
	if err != nil {
		return 0, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// 生の wire の電文を取る:接続テストで最も見る必要があるのは、ゲートウェイが実際に何を返したか(ステータスコード+レスポンスの本文)で、
	// norma がレスポンスを StreamEvent にデコードするとそれらは消えてしまう。quotaAwareTransport が
	// context でこの Capture を見つけ、HTTP の試行ごとのステータスコードと body を入れる。
	ctx, capt := llmrec.NewCapture(ctx)
	defer logTestConnection(c, capt)
	// 接続テストは単発の経路で、agentcore のセッションのループを通らないので、誰も context に
	// session id を載せない。SessionHeaderKey を設定したエンドポイント(opencode zen は
	// x-opencode-session ヘッダーを強制し、ないとそのまま 400 MissingSessionID になる など)では、これが「会話は正常なのに、
	// テストをクリックすると 400」という食い違いを生む。ここで使い捨てのランダムな session id を補って載せ、テストと本物の会話が同じ
	// ヘッダーの送り方を通るようにする;SessionHeaderKey を設定していないエンドポイントはそれを読まないので、副作用はない。
	ctx = transcript.WithSessionID(ctx, "conntest-"+transcript.NewSessionID())
	start := time.Now()
	// MaxTokens は十分に与える：推論モデル(deepseek-v4-pro など)は答えを出す前にまず長い
	// 思考を出す(実測では "ping" の1文でも ~2900 token を使う)。32 しか与えないと、モデルはずっと「思考の段階」にとどまったまま
	// 出力の上限に当たり(finish=length)、切り詰められる。接続テストはそれでも通ったことになる(err=nil)が、表示は
	// 「中断/length/resume」でめちゃくちゃになる。予算を十分に与えて OK をきれいに出し切らせる(finish=stop)。
	// EscalateMaxTokens は false のまま:切り詰めで上限を上げて再試行することはせず、resume のループで空回りするのを避ける。
	reply, err := agentcore.Run(ctx, agentcore.Options{
		Provider:       prov,
		SystemPrompt:   []string{"あなたは接続テストです。2文字の OK をそのまま出力するだけでよく、考えず、説明せず、他には何も出さないでください。"},
		PermissionMode: acperm.ModeBypass,
		MaxTurns:       1,
		MaxTokens:      8192,
		NonStreaming:   !c.Stream, // その profile の本当の送受信のモードで接続テストをする
	}, "ping")
	lat := time.Since(start)
	if err != nil {
		return lat, "", err
	}
	// err==nil だけでは足りない：リクエストは通ったのにモデルが1文字も出さないことは実際にある(思考で予算を使い切る、
	// 本文が安全ポリシーに飲み込まれる、互換層が content を落とす)。そういう設定は会話では「返事をしない」のに、
	// テストは成功と報告する——まさにこの項目が消したい食い違い。見える本文がなければ一律に失敗と判定する。
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return lat, "", fmt.Errorf("モデルの返答に内容がない（リクエストは通ったが、文字列を1つも返さなかった）")
	}
	return lat, reply, nil
}
