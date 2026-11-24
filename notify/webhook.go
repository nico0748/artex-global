package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"text/template"
	"time"
)

// webhookChannel は汎用 Webhook のアダプタ：ユーザーが URL、メソッド、リクエストヘッダー、JSON テンプレートを自由に決める。
// これがあるので、この機能は Slack / Mattermost / Discord / 自前のシステムごとに実装を書かずに済む——
// それらのプラットフォームはどれも設定できるテンプレート1つで扱える。
type webhookChannel struct{}

func (webhookChannel) Kind() string { return KindWebhook }

// 汎用 Webhook には公式の制限がないので、0 を返して既定ではレート制限をせず、利用者が相手の能力に応じて決める。
func (webhookChannel) DefaultRatePerMin() int { return 0 }

// url と headers をマスクする：宛先そのものに token が入っていることが多く、カスタムヘッダーには通常認証情報が入っている。
// どちらも API の返却値に現れるので、両方とも隠す必要がある。
// 代償として、編集時にどれか1つのヘッダーを変えたければヘッダー一式を入れ直す必要がある（マスク値は「元の値を保つ」と解釈される）——
// この取捨は意図したもの：認証情報をブラウザに返すくらいなら、もう一度入力するほうがよい。
func (webhookChannel) SecretKeys() []string { return []string{"url", "headers"} }

// 宛先は url。url を変えるときは headers について改めて態度を示さなければならない —— さもないと元の Authorization ヘッダーが
// そのまま新しい宛先に送られる。これこそマスクをすり抜ける主な経路だ。
func (webhookChannel) DestinationKeys() []string { return []string{"url"} }

// webhookDefaultTemplate はテンプレートが空のときのリクエストボディの既定値：素直な JSON の構造で、
// 「JSON を1件受け取って保存する」大多数の自前の受信側を扱える。
const webhookDefaultTemplate = `{
  "title": {{json .Title}},
  "batch": {{.Batch}},
  "count": {{.Count}},
  "items": [
{{- range $i, $it := .Items}}
{{- if $i}},{{end}}
    {
      "finding_id": {{$it.FindingID}},
      "name": {{json $it.Name}},
      "vulnclass": {{json $it.VulnClass}},
      "severity": {{json $it.Severity}},
      "summary": {{json $it.Summary}},
      "assets": {{json $it.Assets}},
      "detail_url": {{json $it.DetailURL}}
    }
{{- end}}
  ]
}`

// webhookTemplateData はユーザーのテンプレートに公開するコンテキスト。
type webhookTemplateData struct {
	Title   string
	Batch   bool
	Count   int
	Items   []webhookItem
	HomeURL string
	// SentAt は今回の配信の時刻（RFC3339）で、受信側の記録に使う。
	SentAt string
}

type webhookItem struct {
	FindingID     int64
	Name          string
	VulnClass     string
	Severity      string
	SeverityLabel string
	Summary       string
	Assets        []string
	DetailURL     string
	FromStatus    string
	ToStatus      string
	// StatusLabel は状態変更の読みやすい説明で、たとえば「未対応 → 修正済み」。状態変更でなければ空。
	StatusLabel string
}

func (webhookChannel) Validate(cfg map[string]any) error {
	raw := cfgString(cfg, "url")
	if raw == "" {
		return errors.New("대상 URL이 없습니다")
	}
	if err := validateHTTPURL(raw); err != nil {
		return fmt.Errorf("대상 URL이 올바르지 않습니다: %w", err)
	}
	if m := strings.ToUpper(cfgString(cfg, "method")); m != "" && m != http.MethodGet && m != http.MethodPost && m != http.MethodPut && m != http.MethodPatch {
		return fmt.Errorf("지원하지 않는 메서드입니다: %s (GET/POST/PUT/PATCH 중에서 선택하세요)", m)
	}
	if tpl := cfgString(cfg, "body_template"); tpl != "" {
		if _, err := parseWebhookTemplate(tpl); err != nil {
			return fmt.Errorf("요청 본문 템플릿 문법이 올바르지 않습니다: %w", err)
		}
	}
	return nil
}

func (c webhookChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	method := strings.ToUpper(cfgString(cfg, "method"))
	if method == "" {
		method = http.MethodPost
	}

	// GET にはリクエストボディを付けない：内容を query に入れるのはテンプレートの能力の範囲を超え、GET の意味にも合わないので、
	// GET は「一致したらフックを起動する」類の受信側にしか向かない。
	var payload any
	if method != http.MethodGet {
		body, err := renderWebhookBody(cfgString(cfg, "body_template"), m)
		if err != nil {
			return 0, Permanent(err)
		}
		// テンプレートで描画されるのは文字列の形の JSON なので、ここで json.RawMessage に変えてそのまま送り、
		// 二重のエスケープでユーザーが丹念に組んだ構造が JSON の文字列に包まれるのを避ける。
		if !json.Valid([]byte(body)) {
			return 0, Permanent(errors.New("요청 본문 템플릿의 렌더링 결과가 올바른 JSON이 아닙니다"))
		}
		payload = json.RawMessage(body)
	}

	headers := cfgMap(cfg, "headers")
	if ct := cfgString(cfg, "content_type"); ct != "" {
		// 上書きは許すが、headers のあとに適用し、明示的な設定が優先されるようにする。
		if headers == nil {
			headers = map[string]string{}
		}
		headers["Content-Type"] = ct
	}
	if _, err := doJSON(ctx, method, cfgString(cfg, "url"), headers, payload); err != nil {
		return 0, err
	}
	// 汎用 Webhook は本文を切り詰めない（受信側はユーザー自身のサービスで、大きさは body_template で決まる）ので、
	// 一括分すべてを配信済みとする。
	return len(m.Items), nil
}

// renderWebhookBody はユーザーのテンプレート（または既定のテンプレート）でリクエストボディを描画する。
func renderWebhookBody(tpl string, m Message) (string, error) {
	if strings.TrimSpace(tpl) == "" {
		tpl = webhookDefaultTemplate
	}
	t, err := parseWebhookTemplate(tpl)
	if err != nil {
		return "", fmt.Errorf("요청 본문 템플릿 문법이 올바르지 않습니다: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, newWebhookTemplateData(m)); err != nil {
		return "", fmt.Errorf("요청 본문 템플릿을 렌더링하지 못했습니다: %w", err)
	}
	return buf.String(), nil
}

// parseWebhookTemplate はテンプレートを解析する。
//
// missingkey=zero は欠けた map のキーをエラーでなくゼロ値として描画させる——ただしこのファイルのコンテキストは構造体なので、
// 主な効果は .Items が空のときに range がエラーにならないことだ。本当に防ぐべきは .Items が nil であること。
func parseWebhookTemplate(tpl string) (*template.Template, error) {
	return template.New("body").Funcs(webhookTemplateFuncs).Option("missingkey=zero").Parse(tpl)
}

// webhookTemplateFuncs はテンプレートに公開する補助関数。
var webhookTemplateFuncs = template.FuncMap{
	// json は任意の値を JSON にシリアライズする。
	//
	// この関数はおまけではなく必須だ：これを省くと、ユーザーは {{.Title}} で直接埋め込むしかなく、
	// 脆弱性のタイトルに引用符や改行が1つでもあれば、リクエストボディ全体が正しい JSON でなくなる——受信側は
	// 拒否し、エラーメッセージは「JSON の解析に失敗」を指すので、タイトルに引用符があるせいだとはまったく思いつかない。
	"json": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	},
	// jsons は JSON の断片を別の JSON の文字列値の中に埋め込むのに使う（文字列のエスケープを1段行う）。
	"jsons": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		quoted, err := json.Marshal(string(raw))
		if err != nil {
			return "", err
		}
		// 外側の引用符を外す：引用符を付けるかどうかは呼び出し側が決める。
		return string(quoted[1 : len(quoted)-1]), nil
	},
}

func newWebhookTemplateData(m Message) webhookTemplateData {
	d := webhookTemplateData{
		Title:   markdownTitle(m),
		Batch:   m.Batch,
		Count:   len(m.Items),
		HomeURL: m.HomeURL,
		SentAt:  time.Now().Format(time.RFC3339),
		Items:   make([]webhookItem, 0, len(m.Items)),
	}
	for _, it := range m.Items {
		wi := webhookItem{
			FindingID:     it.FindingID,
			Name:          it.Name,
			VulnClass:     it.VulnClass,
			Severity:      it.Severity,
			SeverityLabel: SeverityLabel(it.Severity),
			Summary:       it.Summary,
			Assets:        append([]string{}, it.Assets...),
			DetailURL:     it.DetailURL,
			FromStatus:    it.FromStatus,
			ToStatus:      it.ToStatus,
		}
		if it.IsStatusChange() {
			wi.StatusLabel = StatusLabel(it.FromStatus) + " → " + StatusLabel(it.ToStatus)
		}
		d.Items = append(d.Items, wi)
	}
	return d
}
