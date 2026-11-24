package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// assetInterceptCandidates は挿入待ちの資産の入力項目1件から ドメイン/IP/URL の候補の文字列を抜き出し、資産の遮断の照合に使う。
// URL の host は切り出して分類し、「URL だけを持つ」サービス/エンドポイントの資産も ドメイン/IP のルールに一致するようにする。
func assetInterceptCandidates(item assetInputItem) (domains, ips, urls []string) {
	add := func(dst *[]string, s string) {
		if s = strings.TrimSpace(s); s != "" {
			*dst = append(*dst, s)
		}
	}
	add(&domains, item.Domain)
	for _, d := range item.BoundDomains {
		add(&domains, d)
	}
	add(&ips, item.IP)
	add(&ips, item.ServiceIP)
	add(&urls, item.URL)
	if item.URL != "" {
		if u, err := url.Parse(item.URL); err == nil {
			if h := u.Hostname(); h != "" {
				if net.ParseIP(h) != nil {
					add(&ips, h)
				} else {
					add(&domains, h)
				}
			}
		}
	}
	return domains, ips, urls
}

// assetInputLabel は挿入待ちの資産1件の短い識別子を返し、遮断の説明のメッセージに使う。
func assetInputLabel(item assetInputItem) string {
	typ := strings.TrimSpace(item.Type)
	var target string
	switch {
	case strings.TrimSpace(item.Domain) != "":
		target = strings.TrimSpace(item.Domain)
	case strings.TrimSpace(item.URL) != "":
		target = strings.TrimSpace(item.URL)
	case strings.TrimSpace(item.IP) != "":
		target = strings.TrimSpace(item.IP)
	case strings.TrimSpace(item.ServiceIP) != "":
		target = strings.TrimSpace(item.ServiceIP)
	default:
		target = "(不明)"
	}
	if typ != "" {
		return fmt.Sprintf("[%s] %s", typ, target)
	}
	return target
}

// =====================================================================
// Unified asset insertion tools
// =====================================================================

// SetAssetStore wires the asset store and company store onto this ToolSet
// so the insert_assets, add_company_scope, and list_assets tools are active.
func (t *ToolSet) SetAssetStore(as *db.AssetStore, cs *db.CompanyStore) {
	t.as = as
	t.cs = cs
}

// assetInputItem is one element of the insert_assets "assets" array.
type assetInputItem struct {
	Type string `json:"type"` // root_domain|ip|subdomain|app|service|endpoint

	// ---- root_domain / subdomain ----
	Domain      string   `json:"domain"`
	ICP         string   `json:"icp"`
	RecordType  string   `json:"record_type"`
	RecordValue []string `json:"record_value"`

	// ---- ip ----
	IP           string           `json:"ip"`
	BoundDomains []string         `json:"bound_domains"`
	OpenPorts    []db.PortService `json:"open_ports"`

	// ---- app ----
	AppName     string `json:"app_name"`
	BundleID    string `json:"bundle_id"`
	Category    string `json:"category"`
	Description string `json:"description"`
	AppICP      string `json:"app_icp"`
	CompanyID   *int64 `json:"company_id"` // explicit company link (app only; others auto-attribute via scope)

	// ---- service (http) ----
	URL           string           `json:"url"`
	Technologies  []string         `json:"technologies"`
	StatusCode    *int             `json:"status_code"`
	ContentLength *int64           `json:"content_length"`
	PageTitle     string           `json:"page_title"`
	FaviconMMH3   string           `json:"favicon_mmh3"`
	Auth          []map[string]any `json:"auth"`
	ServiceName   string           `json:"service_name"`
	ServiceIP     string           `json:"service_ip"` // optional enrichment IP

	// ---- service (other) ----
	Port  int    `json:"port"`
	Proto string `json:"proto"`

	// ---- endpoint ----
	Method string           `json:"method"`
	Params []map[string]any `json:"params"`
}

// insertAssets is the unified insert_assets agent tool.
func (t *ToolSet) insertAssets() actool.CoreTool {
	return writeTool(
		"insert_assets",
		"新しく見つけた資産をまとめて登録する。一度に複数の種類を混ぜられる（type は列挙を参照）。\n"+
			"種類ごとの必須フィールド：root_domain→domain；ip→ip（IPv4/IPv6 でなければならず、ホスト名は不可）；subdomain→domain；app→app_name；service(HTTP)→url；service(HTTP 以外)→service_name+port（ip/domain の少なくとも1つを入れる）；endpoint→url+method。他のフィールドの意味はそれぞれの説明を参照。\n"+
			"auth/technologies/params は追加でマージ(append)し、元の値を上書きしない。\n"+
			"戻り値：{results:[{index,id,type}], errors:[{index,error}]}",
		obj(map[string]any{
			// task_id はモデルに見せない：worker がどの task に属するかはプログラムが SetTaskID で正式に割り当てる(handler を参照)。
			"assets": map[string]any{
				"type":        "array",
				"description": "資産の配列で、各要素が資産の記録1件に対応する",
				"items": obj(map[string]any{
					"type": map[string]any{
						"type":        "string",
						"enum":        []string{"root_domain", "ip", "subdomain", "app", "service", "endpoint"},
						"description": "資産の種類",
					},
					// root_domain / subdomain
					"domain":      str("ルートドメインまたはサブドメイン（root_domain/subdomain で必須）"),
					"icp":         str("ICP 届出番号（任意）"),
					"record_type": str("DNS の解決の種類：A/AAAA/CNAME/MX など（subdomain で任意）"),
					"record_value": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "DNS の解決値の一覧（subdomain で任意。例 [\"1.2.3.4\",\"2.3.4.5\"]）",
					},
					// ip
					"ip": str("IP アドレス。IPv4/IPv6 のアドレスでなければならず、ホスト名は入れられない（ホスト名は type=subdomain の domain フィールドを使う）；ip の種類で必須；service/endpoint の種類では任意で、IP を関連付けるのに使う"),
					"bound_domains": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "その IP に結びついたドメインの一覧（ip の種類で任意）",
					},
					"open_ports": map[string]any{
						"type":        "array",
						"description": "開いているポートの一覧（ip の種類で任意）",
						"items": obj(map[string]any{
							"port":    intp("ポート番号"),
							"service": str("サービス名。http/ssh/mysql など（任意）"),
						}, "port"),
					},
					// app
					"app_name":    str("アプリ名（app の種類で必須）"),
					"bundle_id":   str("Bundle ID（app の種類で任意）"),
					"category":    str("アプリの分類（任意）"),
					"description": str("アプリの説明（任意）"),
					"app_icp":     str("アプリの ICP 届出（任意）"),
					"company_id":  intp("帰属する企業の id（app の種類で任意；app は scope で自動では帰属できないので、明示的に指定する必要がある。id は add_company_scope が返す）"),
					// service (http)
					"url":         str("完全な URL。プロトコルとポートを含む（HTTP サービスで必須；service_type は自動で http になる）"),
					"status_code": intp("HTTP レスポンスのステータスコード。200/301/403/404 など（任意）"),
					"content_length": map[string]any{
						"type":        "integer",
						"description": "HTTP レスポンスの本文のバイト数（任意）",
					},
					"page_title":   str("ページの <title> の内容（任意）"),
					"favicon_mmh3": str("favicon の MMH3 ハッシュ（任意）"),
					"technologies": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "フィンガープリント/技術スタックの一覧。例 [\"Nginx\",\"Vue\",\"Bootstrap\"]（任意）",
					},
					"auth": map[string]any{
						"type":        "array",
						"description": "見つけた認証情報の一覧。各件に type/username/password などのフィールドを含む（任意。追加で、上書きしない）",
						"items":       map[string]any{"type": "object"},
					},
					// service (other、HTTP 以外)
					"service_name": str("サービス名。ssh/mysql/redis など（service が HTTP 以外のとき必須）"),
					"port":         intp("ポート番号（service が HTTP 以外のとき必須）"),
					// endpoint
					"method": str("HTTP メソッド：GET/POST/PUT/PATCH/DELETE など（endpoint で必須）"),
					"params": map[string]any{
						"type":        "array",
						"description": "リクエストの引数の一覧。各件に location(query/body/header/path)/name/value/type を含む（任意。追加で、上書きしない）",
						"items":       map[string]any{"type": "object"},
					},
				}, "type"),
			},
		}, "assets"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("insert_assets は有効になっていない: AssetStore が初期化されていない"), nil
			}
			var a struct {
				Assets []assetInputItem `json:"assets"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf("invalid input: " + err.Error()), nil
			}
			// task_id はプログラムが正式に割り当て(worker: SetTaskID)、モデルからの入力は受け付けない——モデルが渡し忘れ/渡し間違いをして
			// 資産がタスクに属さない、または違うタスクに属するのを避ける。タスクのコンテキストのない呼び出し元(auto/pentest/chat)は t.taskID=0。
			taskID := t.taskID

			type result struct {
				Index int    `json:"index"`
				ID    int64  `json:"id"`
				Type  string `json:"type"`
			}
			type errEntry struct {
				Index int    `json:"index"`
				Error string `json:"error"`
			}

			var results []result
			var errs []errEntry

			// 資産の門のルールを一度に読み込む；読み込みに失敗したら判定を飛ばす（挿入は妨げない）。
			// 遮断のルール = 全体 ∪ タスク単位の block；許可のルール = タスク単位の allow。
			blockRules, _ := t.as.ListAssetInterceptRules()
			var allowRules []db.AssetInterceptRule
			if t.taskID > 0 {
				if tb, ta, err := t.as.TaskInterceptRulesSplit(t.taskID); err == nil {
					blockRules = append(blockRules, tb...)
					allowRules = ta
				}
			}

			for i, item := range a.Assets {
				// 資産の門：先に遮断し、それから許可する。拒まれた資産は挿入を禁じる（Upsert とその後の副作用を飛ばす）。
				domains, ips, urls := assetInterceptCandidates(item)
				if d := db.EvaluateAssetGate(blockRules, allowRules, domains, ips, urls); !d.Allowed {
					errs = append(errs, errEntry{
						Index: i,
						Error: fmt.Sprintf("資産 %s %s。挿入を禁止した", assetInputLabel(item), d.Reason),
					})
					continue
				}

				typ := strings.TrimSpace(item.Type)
				var id int64
				var err error

				switch typ {
				case "root_domain":
					id, err = t.as.UpsertRootDomain(db.UpsertRootDomainReq{
						Domain: item.Domain,
						ICP:    item.ICP,
						TaskID: taskID,
					})

				case "ip":
					id, err = t.as.UpsertIP(db.UpsertIPReq{
						IP:           item.IP,
						BoundDomains: item.BoundDomains,
						OpenPorts:    item.OpenPorts,
						TaskID:       taskID,
					})

				case "subdomain":
					id, err = t.as.UpsertSubdomain(db.UpsertSubdomainReq{
						Domain:      item.Domain,
						RecordType:  item.RecordType,
						RecordValue: item.RecordValue,
						ICP:         item.ICP,
						TaskID:      taskID,
					})

				case "app":
					id, err = t.as.UpsertApp(db.UpsertAppReq{
						Name:        item.AppName,
						BundleID:    item.BundleID,
						Category:    item.Category,
						Description: item.Description,
						ICP:         item.AppICP,
						CompanyID:   item.CompanyID,
						TaskID:      taskID,
					})

				case "service":
					// distinguish HTTP vs other by presence of url
					if item.URL != "" {
						// agent may send "ip" or "service_ip" for the enrichment IP; accept both
						svcIP := item.ServiceIP
						if svcIP == "" {
							svcIP = item.IP
						}
						id, err = t.as.UpsertHTTPService(db.UpsertHTTPServiceReq{
							URL:           item.URL,
							Technologies:  item.Technologies,
							StatusCode:    item.StatusCode,
							ContentLength: item.ContentLength,
							PageTitle:     item.PageTitle,
							FaviconMMH3:   item.FaviconMMH3,
							Auth:          item.Auth,
							IP:            svcIP,
							TaskID:        taskID,
						})
					} else {
						id, err = t.as.UpsertOtherService(db.UpsertOtherServiceReq{
							Domain:      item.Domain,
							IP:          item.IP,
							Port:        item.Port,
							ServiceName: item.ServiceName,
							Auth:        item.Auth,
							TaskID:      taskID,
						})
					}

				case "endpoint":
					id, err = t.as.UpsertEndpoint(db.UpsertEndpointReq{
						URL:    item.URL,
						Method: item.Method,
						Params: item.Params,
						IP:     item.ServiceIP,
						TaskID: taskID,
					})

				default:
					errs = append(errs, errEntry{Index: i, Error: "unknown type: " + typ})
					continue
				}

				if err != nil {
					errs = append(errs, errEntry{Index: i, Error: err.Error()})
					continue
				}
				results = append(results, result{Index: i, ID: id, Type: typ})
				t.writes.Assets++
				t.anchorOwner(id)
				if taskID > 0 {
					var sourceNodeID *int64
					if t.ownerNode > 0 {
						nodeID := t.ownerNode
						sourceNodeID = &nodeID
					}
					summary := "Agent が insert_assets で登録"
					if t.ownerNode > 0 {
						summary = fmt.Sprintf("Worker の意図 #%d が insert_assets で登録", t.ownerNode)
					}
					_ = t.as.SetTaskAssetSource(taskID, id, "agent", summary, sourceNodeID)
				}
				// 自動でテスト範囲に入れる(source='auto')：worker が最上位で明示的に挿入したこの項目だけに、その
				// 種類に応じた保守的な範囲を加える。副作用で派生した資産はここを通らないので、範囲がむやみに広がらない。taskID=0 のときは何もしない。
				// カバレッジの切り替えとは関係ない：task_scope はタスクの範囲の境界(一覧/検索の絞り込みの基準)で、
				// カバレッジの切り替えはそれを分母として指標を計算するかどうかを決めるだけで、範囲そのものを積み上げるかどうかは決めない。
				{
					svcIP := item.ServiceIP
					if svcIP == "" {
						svcIP = item.IP
					}
					_ = t.as.AddAutoScope(taskID, typ, item.Domain, item.URL, svcIP)
				}
			}

			return jsonResult(map[string]any{
				"results": results,
				"errors":  errs,
			})
		},
	)
}

// addCompanyScope writes to company_scope table and triggers asset attribution.
func (t *ToolSet) addCompanyScope() actool.CoreTool {
	return writeTool(
		"add_company_scope",
		"ドメイン/IP/CIDR/ICP 届出/企業のキーワードをある会社の【資産の範囲】に加える——ドメイン、ネットワーク、ICP は一致した資産を自動で引き受け、キーワードは範囲のヒントとして Agent に渡すだけ。\n"+
			"会社名は一意：company がなければ新しく作り、あれば再利用する(範囲だけを加える)。\n"+
			"scope は1行に1件で、システムが自動で識別する：ルートドメイン / URL / 単一の IP / CIDR のネットワーク / ICP 届出 / 企業のキーワード。\n"+
			"必ず reason で帰属の根拠(whois/証明書/ASN など)を説明する。\n"+
			"安全柵：むき出しの TLD と広すぎるネットワーク(IPv4 のプレフィックスは /16-/32、IPv6 のプレフィックスは /32-/128 でなければならない)を拒み、不正な行は飛ばして errors で返す。",
		obj(map[string]any{
			"company": str("会社名(なければ新しく作り、あれば再利用する。名前は一意)"),
			"scope":   str("資産の範囲。1行に1件：ドメイン / URL / IP / CIDR / ICP 届出 / 企業のキーワード"),
			"reason":  str("帰属の根拠(証拠/出どころ)。必ず入れる"),
			"logo":    str("会社のアイコンの URL(任意。新しく会社を作るときだけ効く)"),
		}, "company", "scope"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.cs == nil {
				return actool.Errorf("add_company_scope は有効になっていない: CompanyStore が初期化されていない"), nil
			}
			var a struct {
				Company string `json:"company"`
				Scope   string `json:"scope"`
				Reason  string `json:"reason"`
				Logo    string `json:"logo"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if strings.TrimSpace(a.Company) == "" {
				return actool.Errorf("company は空にできない"), nil
			}
			companyID, _, err := t.cs.UpsertCompany(a.Company, a.Logo)
			if err != nil {
				return actool.Errorf("会社の作成/取得に失敗: " + err.Error()), nil
			}
			lines := splitLines(a.Scope)
			added, skipped, invalid, errMsgs := t.cs.AddScope(companyID, lines, a.Reason)
			out := map[string]any{
				"company_id": companyID,
				"added":      added,
				"skipped":    skipped,
				"invalid":    invalid,
			}
			if len(errMsgs) > 0 {
				out["errors"] = errMsgs
			}
			return jsonResult(out)
		},
	)
}

// addTaskScope lets the plan agent add test scope to THE CURRENT TASK — the coverage
// denominator and the task's authorization edge. Worker discoveries are auto-scoped
// (precise host) by insertAssets; this tool is for DELIBERATELY WIDENING: pull a whole
// root domain or whole company into scope, or add a specific subdomain / ip.
func (t *ToolSet) addTaskScope() actool.CoreTool {
	return writeTool(
		"add_task_scope",
		"テスト範囲を【このタスク】に加える——これはこのタスクの許可の境界であり、資産のテストのカバレッジの分母でもある。\n"+
			"kind に使えるもの：company(会社の名義の資産全体) / root_domain(ルートドメイン全体。すべてのサブドメインを含む) / subdomain(単一の正確なサブドメイン) / ip / cidr / icp / keyword。\n"+
			"説明：worker が1つずつ触れたホストはシステムが【自動で】範囲に加える(正確なサブドメイン)。このツールは【自分から広げる】のに使う——ルートドメイン全体/会社全体を入れたり、特定のサブドメイン/IP を補ったりする。\n"+
			"value：company は会社名または id(会社がすでにあること)；root_domain/subdomain はドメイン；ip/cidr は IP またはネットワーク；icp/keyword は届出番号または企業のキーワード。\n"+
			"必ず reason で根拠を説明する(監査できるように)。複数件は entries 配列を使う。",
		obj(map[string]any{
			"entries": map[string]any{"type": "array", "description": "一括：[{kind, value}]。kind∈company/root_domain/subdomain/ip/cidr/icp/keyword。", "items": map[string]any{"type": "object"}},
			"kind":    str("[単発] company / root_domain / subdomain / ip / cidr / icp / keyword"),
			"value":   str("[単発] 会社名または id / ドメイン / IP / CIDR / ICP / キーワード"),
			"reason":  str("加える根拠(監査に使う)。必ず入れる"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("add_task_scope は有効になっていない: AssetStore が初期化されていない"), nil
			}
			if t.taskID <= 0 {
				return actool.Errorf("add_task_scope にはタスクのコンテキストが必要(現在は task がない)"), nil
			}
			type scopeEntry struct {
				Kind  string `json:"kind"`
				Value string `json:"value"`
			}
			var a struct {
				Entries    []scopeEntry `json:"entries"`
				scopeEntry              // 単発モード
				Reason     string       `json:"reason"`
			}
			_ = json.Unmarshal(in, &a)
			items := a.Entries
			if len(items) == 0 {
				items = []scopeEntry{a.scopeEntry}
			}
			var added []map[string]any
			errs := map[string]string{}
			for i, e := range items {
				ts, err := t.as.AddAgentScope(t.taskID, strings.TrimSpace(e.Kind), e.Value, a.Reason, "agent")
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				added = append(added, map[string]any{"kind": ts.Kind, "domain": ts.Domain, "net": ts.Net, "value": ts.Value, "company_id": ts.CompanyID})
			}
			out := map[string]any{"added": added}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		},
	)
}

// listUntestedAssets lets the plan agent pull the current + directly inherited
// scope's not-yet-tested assets on demand (filter by type, paginated).
func (t *ToolSet) listUntestedAssets() actool.CoreTool {
	return readTool(
		"list_untested_assets",
		"【このタスクと直接関連するタスク】の範囲内で、まだ事実の錨に覆われていない資産を調べる（関連する範囲は読み取り専用。テストを補うかをあなた自身が判断するためのもので、あなたの代わりに決めるものではない）。\n"+
			"資産の種類で絞り込める（任意）：root_domain/subdomain/service/app/endpoint/ip。\n"+
			"ページ分け：page は 1 から、page_size は既定 10。{assets:[{id,type,label}], total, page, page_size} を返す。タスクのコンテキストでだけ使える。",
		obj(map[string]any{
			"type":      str("資産の種類の絞り込み（任意）：root_domain/subdomain/service/app/endpoint/ip"),
			"page":      intp("ページ番号。1 から（既定 1）"),
			"page_size": intp("1ページの件数（既定 10）"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("list_untested_assets は有効になっていない: AssetStore が初期化されていない"), nil
			}
			if t.taskID <= 0 || t.ts == nil {
				return actool.Errorf("list_untested_assets にはタスクのコンテキストが必要"), nil
			}
			var a struct {
				Type     string `json:"type"`
				Page     int    `json:"page"`
				PageSize int    `json:"page_size"`
			}
			_ = json.Unmarshal(in, &a)
			if a.Page <= 0 {
				a.Page = 1
			}
			if a.PageSize <= 0 {
				a.PageSize = 10
			}
			offset := (a.Page - 1) * a.PageSize
			assets, total, err := t.as.ListUntestedAssetsWithSources(t.taskID, strings.TrimSpace(a.Type), a.PageSize, offset)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(map[string]any{
				"assets": assets, "total": total, "page": a.Page, "page_size": a.PageSize,
			})
		},
	)
}

// listAssets lets an agent query the asset table.
func (t *ToolSet) listAssets() actool.CoreTool {
	return readTool(
		"list_assets",
		"資産の DB を調べる：DSL の式で検索するか、id/ids で直接取得する。ページ分けに対応。【このタスクと直接関連するタスク】のテスト範囲内の資産だけを返す。\n"+
			"DSL：field=value あいまい(ILIKE) | field==value 完全一致 | field!=value 除外 | 数値のフィールドは > >= < <= に対応 | 裸の語=全文のあいまい検索；AND/OR で組み合わせ(AND の優先度が高い)、括弧でまとめられる。資産の種類は独立した type 引数を使い、DSL には書かない。\n"+
			"id/ids を渡さないときは dsl を空にできない（条件なしの全件の検索は許さない）。\n"+
			"使えるフィールド：domain(ルート/サブ/サービスのドメイン)、root_domain、ip、url、page_title、icp、service_name、app_name、method(GET/POST など)、service_type(http|other)、record_type(A/CNAME など)、technology(配列。=あいまい ==完全一致)、port/status_code/company_id(整数)。\n"+
			"例：status_code>=400 AND technology=shiro ；(port==80 OR port==443) AND technology=nginx",
		obj(map[string]any{
			"dsl":    str(`DSL の検索式（文法/フィールドはツールの説明を参照）。id/ids を渡さないときは空にできない。`),
			"type":   str("資産の種類の絞り込み：root_domain|ip|subdomain|app|service|endpoint（独立したフィールドで、dsl と重ねられる。type だけでは検索に足りず、dsl も必要）"),
			"id":     intp("資産の id 1つで直接取得する（任意。dsl/type とは同時に使えない）"),
			"ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "複数の資産の id で直接取得する（任意。dsl/type とは同時に使えない）"},
			"limit":  intp("返す上限。既定 10（任意）"),
			"offset": intp("ページ分けのオフセット。既定 0（任意）"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("list_assets は有効になっていない: AssetStore が初期化されていない"), nil
			}
			var a struct {
				DSL    string  `json:"dsl"`
				Type   string  `json:"type"`
				ID     int64   `json:"id"`
				IDs    []int64 `json:"ids"`
				Limit  int     `json:"limit"`
				Offset int     `json:"offset"`
			}
			_ = json.Unmarshal(in, &a)
			if a.Limit <= 0 {
				a.Limit = 10
			}

			var assets []*db.Asset
			var err error
			switch {
			case a.ID > 0:
				assets, err = t.as.GetByIDsInScope(t.taskID, []int64{a.ID})
			case len(a.IDs) > 0:
				assets, err = t.as.GetByIDsInScope(t.taskID, a.IDs)
			case a.DSL != "":
				assets, err = t.as.QueryDSLInScope(a.DSL, a.Type, t.taskID, a.Limit, a.Offset)
			default:
				return actool.Errorf("id/ids を渡さないときは dsl を空にできない：条件なしですべての資産を調べることは許さないので、検索の条件を渡すこと"), nil
			}
			if err != nil {
				return actool.Errorf("DSL のエラー: " + err.Error()), nil
			}
			return jsonResult(map[string]any{
				"count":  len(assets),
				"assets": assets,
			})
		},
	)
}

// listCompanies lets an agent enumerate companies (企業) with their scope + asset count.
func (t *ToolSet) listCompanies() actool.CoreTool {
	return readTool(
		"list_companies",
		"資産の DB にある【企業/会社】と、その資産の範囲(scope)・帰属済みの資産の数を一覧にする。どんな会社があるかを見て、"+
			"company_id を取るのに使う（insert_assets で app を関連付けるときや、list_assets で company_id で絞り込むときに使う）。"+
			"任意の search で会社名をあいまいに絞り込める(大文字小文字を区別しない)。空ならすべてを返す。",
		obj(map[string]any{
			"search": str("会社名であいまいに絞り込む(任意。大文字小文字を区別しない)。空ならすべてを返す"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.cs == nil {
				return actool.Errorf("list_companies は有効になっていない: CompanyStore が初期化されていない"), nil
			}
			var a struct {
				Search string `json:"search"`
			}
			_ = json.Unmarshal(in, &a)
			cos, err := t.cs.ListCompanies()
			if err != nil {
				return actool.Errorf("会社の検索に失敗: " + err.Error()), nil
			}
			q := strings.ToLower(strings.TrimSpace(a.Search))
			type companyOut struct {
				ID         int64    `json:"id"`
				Name       string   `json:"name"`
				AssetCount int      `json:"asset_count"`
				Scope      []string `json:"scope"`
			}
			out := make([]companyOut, 0, len(cos))
			for _, c := range cos {
				if q != "" && !strings.Contains(strings.ToLower(c.Name), q) {
					continue
				}
				scope := make([]string, 0, len(c.Scope))
				for _, r := range c.Scope {
					scope = append(scope, r.Raw)
				}
				out = append(out, companyOut{ID: c.ID, Name: c.Name, AssetCount: c.AssetCount, Scope: scope})
			}
			return jsonResult(map[string]any{"count": len(out), "companies": out})
		},
	)
}

// splitLines splits a multi-line string into non-empty trimmed lines.
func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// WorkerTools returns the tool set for a work agent.
func (t *ToolSet) WorkerTools() []actool.CoreTool {
	return []actool.CoreTool{
		// list_findings は残す：脆弱性を報告する前にこのタスクの確認済みの脆弱性を調べ、同じ脆弱性を重ねて報告するのを避ける。
		t.listFindings(),
		t.addFinding(), t.recordFact(),
		// asset management (handlers guard nil store internally)。
		// add_company_scope は worker に渡さない：企業の資産の範囲を定めるのは計画/主制御/Auto の役目で、worker は探索を実行するだけ。
		t.insertAssets(), t.listAssets(),
		// work をまたいだ振り返り：worker も他の work の観察を再利用でき、重複した作業を避けられる。
		// search_all_worker_traces：intent_id を先に知らなくてよく、キーワードで全体から一致した手順を拾う；
		// get_worker_trace：ある work に絞ってから手順を並べる/その場で検索する/完全な内容を取る。
		t.searchAllWorkerTraces(), t.getWorkerTrace(),
		// node_detail：worker は intent_id/ノードの id を得たら、そのノードの完全な詳細を調べられる（上の振り返りと組み合わせる）。
		t.nodeDetail(),
		// 以下のツールは引き続き worker に【渡さず】、planner/main にだけ残す（コンテキストを読む、work をまたいで振り返るのは計画の役目で、
		// worker は1本の意図の実行と書き戻しだけをする）：list_facts / list_companies / list_worker_traces。
	}
}

// MainAgentTools returns the human-interface tool set.
func (t *ToolSet) MainAgentTools() []actool.CoreTool {
	return []actool.CoreTool{
		t.graphOverview(), t.listFindings(), t.listFacts(), t.nodeDetail(),
		t.expandDigest(), // cold-digest §6.1
		t.getWorkerOutput(), t.getWorkerTrace(), t.searchAllWorkerTraces(), t.addHint(), t.addIntent(),
		// steer_work：人は実行中の意図(work)に軌道修正の指示をその場で注入できる（中断せず、進み具合も失わない）。
		t.steerWorkTool(),
		// set_goals：人は実行時にこのタスクに新しい最終的な目標を補える（計画者はそれをもとに達成したかを判定し直す）。
		t.setGoals(),
		// set_constraints：人は実行時にこのタスクに操作の制約（allow/deny）を補う/変えることができ、planner/worker の探索の境界を縛る。
		t.setConstraints(),
		// asset management (handlers guard nil store internally)
		t.insertAssets(), t.addCompanyScope(), t.listAssets(),
		t.addFinding(), t.recordFact(),
		t.addTaskScope(),
		// list_untested_assets：必要に応じてこのタスクの範囲内の未テストの資産を調べ(種類+ページ分け)、テストを補うかを自分で決める。
		t.listUntestedAssets(),
	}
}

// AllDomainTools returns the union of all domain tools across all agent types,
// deduped by name (mainagent order wins). Used by the server to build a registry
// for injecting domain tools into agents (Auto, custom) that don't own a per-task
// ToolSet. The caller provides real stores; tools are callable at taskID=0 scope.
func (t *ToolSet) AllDomainTools() []actool.CoreTool {
	seen := map[string]bool{}
	var out []actool.CoreTool
	all := append(append(t.MainAgentTools(), t.PlannerTools()...), t.WorkerTools()...)
	for _, tool := range all {
		if !seen[tool.Name()] {
			seen[tool.Name()] = true
			out = append(out, tool)
		}
	}
	return out
}
