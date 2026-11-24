package db

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// 資産の遮断の規則の照合/実行の層。asset_intercept.go は規則の保存だけを受け持ち、ここでは
// 「標的の資産」のドメイン/IP/URL を有効な規則と照らし合わせる。agent のツール（add_intent、
// insert_assets）が意図を出す / 資産を挿入する前に呼び、当たれば拒む。

// AssetInterceptKindLabel は kind のラベルを返し、agent への説明のメッセージに使う。
func AssetInterceptKindLabel(kind string) string {
	switch kind {
	case "exact_domain":
		return "ドメイン(完全一致)"
	case "exact_ip":
		return "IP(完全一致)"
	case "exact_url":
		return "URL(完全一致)"
	case "fuzzy_domain":
		return "ドメイン(あいまい一致)"
	case "fuzzy_ip":
		return "IP(あいまい一致)"
	case "fuzzy_url":
		return "URL(あいまい一致)"
	case "cidr":
		return "CIDR のネットワーク"
	}
	return kind
}

// Reason は読める当たりの理由を1件返す。形は：資産の遮断の規則に当たった [ドメイン(あいまい一致): .gov.cn]（メモ）。
func (r AssetInterceptRule) Reason() string {
	s := fmt.Sprintf("資産の遮断の規則に当たった [%s: %s]", AssetInterceptKindLabel(r.Kind), r.Pattern)
	if note := strings.TrimSpace(r.Note); note != "" {
		s += "（" + note + "）"
	}
	return s
}

// matchOne は有効な規則1つが与えたドメイン/IP/URL の候補の文字列に当たるかを判定し、当たった具体的な値を返す。
func matchOne(r AssetInterceptRule, domains, ips, urls []string) (string, bool) {
	p := strings.TrimSpace(r.Pattern)
	if p == "" {
		return "", false
	}
	switch r.Kind {
	case "exact_domain":
		for _, d := range domains {
			if strings.EqualFold(strings.TrimSpace(d), p) {
				return d, true
			}
		}
	case "exact_ip":
		for _, ip := range ips {
			if strings.TrimSpace(ip) == p {
				return ip, true
			}
		}
	case "exact_url":
		for _, u := range urls {
			if strings.TrimSpace(u) == p {
				return u, true
			}
		}
	case "fuzzy_domain":
		lp := strings.ToLower(p)
		for _, d := range domains {
			if d != "" && strings.Contains(strings.ToLower(d), lp) {
				return d, true
			}
		}
	case "fuzzy_ip":
		for _, ip := range ips {
			if ip != "" && strings.Contains(ip, p) {
				return ip, true
			}
		}
	case "fuzzy_url":
		lp := strings.ToLower(p)
		for _, u := range urls {
			if u != "" && strings.Contains(strings.ToLower(u), lp) {
				return u, true
			}
		}
	case "cidr":
		_, ipnet, err := net.ParseCIDR(p)
		if err != nil {
			return "", false
		}
		for _, ip := range ips {
			if pip := net.ParseIP(strings.TrimSpace(ip)); pip != nil && ipnet.Contains(pip) {
				return ip, true
			}
		}
	}
	return "", false
}

// MatchAssetInterceptRules は与えた ドメイン/IP/URL の候補の文字列に当たる最初の有効な規則と、当たった具体的な値を返す。
// insert_assets が元の入力（まだ保存していない assetInputItem）で照合するのに使う。
func MatchAssetInterceptRules(rules []AssetInterceptRule, domains, ips, urls []string) (AssetInterceptRule, string, bool) {
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		if v, ok := matchOne(r, domains, ips, urls); ok {
			return r, v, true
		}
	}
	return AssetInterceptRule{}, "", false
}

// interceptCandidates は保存済みの資産から、遮断の照合に使う ドメイン/IP/URL の候補の文字列を取り出す。
// URL の host を取り出して分類し、「URL しか持たない」サービスの資産も ドメイン/IP の規則に当たるようにする。
func (a *Asset) interceptCandidates() (domains, ips, urls []string) {
	add := func(dst *[]string, s string) {
		if s = strings.TrimSpace(s); s != "" {
			*dst = append(*dst, s)
		}
	}
	add(&domains, a.Domain)
	add(&domains, a.RootDomain)
	for _, d := range a.BoundDomains {
		add(&domains, d)
	}
	add(&ips, a.IP)
	add(&urls, a.URL)
	if a.URL != "" {
		if u, err := url.Parse(a.URL); err == nil {
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

// InterceptLabel は資産の短い識別を返し、agent への説明のメッセージに使う。
func (a *Asset) InterceptLabel() string {
	var target string
	switch {
	case a.Domain != "":
		target = a.Domain
	case a.URL != "":
		target = a.URL
	case a.IP != "":
		target = a.IP
	default:
		target = fmt.Sprintf("#%d", a.ID)
	}
	return fmt.Sprintf("資産#%d[%s] %s", a.ID, a.Type, target)
}

// hasEnabledRule は規則の集合に有効な規則が1つでもあるかを判定する。
func hasEnabledRule(rules []AssetInterceptRule) bool {
	for _, r := range rules {
		if r.Enabled {
			return true
		}
	}
	return false
}

// AssetGateDecision は「先に遮断し、次に許可する」関門が候補の文字列の組に下した判定の結果。
type AssetGateDecision struct {
	Allowed bool
	Reason  string // 拒んだ理由（資産の識別は含まない）；Allowed=true のときは空
}

// EvaluateAssetGate はタスク単位の関門の判定をする：
//  1. 有効な blockRules のどれかに当たる → 拒む（遮断の理由）。
//  2. そうでなく、allowRules に有効なものがあってどれにも当たらない → 拒む（許可の範囲の外）。
//  3. それ以外は通す。
//
// allowRules が空/有効なものがないときは、許可の関門は効かない（つまりホワイトリストを使わず、すべて通す）。
// 「許可の規則を設定していない」せいですべての資産が止められるのを避ける。
func EvaluateAssetGate(blockRules, allowRules []AssetInterceptRule, domains, ips, urls []string) AssetGateDecision {
	if rule, _, ok := MatchAssetInterceptRules(blockRules, domains, ips, urls); ok {
		return AssetGateDecision{Allowed: false, Reason: rule.Reason()}
	}
	if hasEnabledRule(allowRules) {
		if _, _, ok := MatchAssetInterceptRules(allowRules, domains, ips, urls); !ok {
			return AssetGateDecision{Allowed: false, Reason: "タスクの許可(ホワイトリスト)の範囲の外なので、テストを許可しない"}
		}
	}
	return AssetGateDecision{Allowed: true}
}

// AssetInterceptHit は関門に拒まれた資産1つを表す（遮断に当たった か 許可の範囲の外）。
type AssetInterceptHit struct {
	Asset  *Asset
	Reason string // 読める理由
}

// Describe は読める説明を1件返す：資産の情報 + 理由。
func (h AssetInterceptHit) Describe() string {
	return fmt.Sprintf("%s → %s", h.Asset.InterceptLabel(), h.Reason)
}

// ListAssetInterceptRules は *DB の同じ名前のメソッドをそのまま通すもので、AssetStore しか持たない呼び出し側
// （agent のツールなど）も規則を読めるようにする。
func (s *AssetStore) ListAssetInterceptRules() ([]AssetInterceptRule, error) {
	return s.db.ListAssetInterceptRules()
}

// CheckAssetsIntercept は id で資産を読み込み、1つずつ「先に遮断し、次に許可する」関門で判定し、拒まれた
// すべての資産を返す。遮断の規則 = 全体 ∪ タスク単位の block；許可の規則 = タスク単位の allow（このタスクだけ）。
// id がなければすぐ返す。全体の GetByIDs（タスクの範囲の絞り込みを受けない）を使い、遮断が scope で弱められないようにする。
func (s *AssetStore) CheckAssetsIntercept(taskID int64, ids []int64) ([]AssetInterceptHit, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	blockRules, err := s.db.ListAssetInterceptRules()
	if err != nil {
		return nil, err
	}
	var allowRules []AssetInterceptRule
	if taskID > 0 {
		tb, ta, err := s.TaskInterceptRulesSplit(taskID)
		if err != nil {
			return nil, err
		}
		blockRules = append(blockRules, tb...)
		allowRules = ta
	}
	// 遮断の規則も有効な許可の規則もない → 判定は要らず、すべて通す。
	if len(blockRules) == 0 && !hasEnabledRule(allowRules) {
		return nil, nil
	}
	assets, err := s.GetByIDs(ids)
	if err != nil {
		return nil, err
	}
	var hits []AssetInterceptHit
	for _, a := range assets {
		domains, ips, urls := a.interceptCandidates()
		if d := EvaluateAssetGate(blockRules, allowRules, domains, ips, urls); !d.Allowed {
			hits = append(hits, AssetInterceptHit{Asset: a, Reason: d.Reason})
		}
	}
	return hits, nil
}
