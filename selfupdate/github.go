package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Repo はリリースの取得元。設定項目にせず固定にしている：更新元を設定できるようにすると、設定を変えられる人なら誰にでも
// リモートでコードを実行する経路を与えることになり、ペネトレーションテストのプラットフォームとしてはこの穴は開けられない。
const Repo = "Autumn-27/artex"

// latestURL は GitHub の「最新の正式版」API。prerelease と draft は自動的に飛ばされる。
const latestURL = "https://api.github.com/repos/" + Repo + "/releases/latest"

// allowedHosts は更新の流れがアクセスできるドメインを限定する。下の checkRedirect と組み合わせて、
// どの段階でもリストにないホストにリダイレクトされたら即座に失敗させる——これは DNS 汚染や中間者による
// バイナリのすり替えを防ぐ第一の関門で、第二の関門は SHA256SUMS の照合である。
var allowedHosts = map[string]bool{
	"api.github.com":                       true,
	"github.com":                           true,
	"objects.githubusercontent.com":        true, // release の資産が実際に置かれるオブジェクトストレージ
	"release-assets.githubusercontent.com": true,
	"raw.githubusercontent.com":            true,
}

// Release は GitHub Release のうち、ここで必要なフィールド。
type Release struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
	Assets      []Asset   `json:"assets"`
}

// Asset は Release に添付された1つのファイル。
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// NewClient は GitHub のドメインだけを受け付ける HTTP クライアントを作る。proxy が空なら直接つなぐ。
//
// 既定の Transport はわざと使い回さない：更新の流れでは TLS と証明書の検証を必ず強制し、他の場所で
// 設定された InsecureSkipVerify などの影響を受けてはならない。
func NewClient(proxy string) *http.Client {
	tr := &http.Transport{
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	if p := strings.TrimSpace(proxy); p != "" {
		if pu, err := url.Parse(p); err == nil {
			tr.Proxy = http.ProxyURL(pu)
		}
	}
	return &http.Client{
		Transport: tr,
		Timeout:   30 * time.Minute, // パッケージ全体をダウンロードするので、リクエスト単位のタイムアウトで止めてはならない
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("리다이렉트 횟수가 너무 많습니다")
			}
			return checkURL(req.URL)
		},
	}
}

// checkURL は https とドメインの許可リストを強制する。
func checkURL(u *url.URL) error {
	if u.Scheme != "https" {
		return fmt.Errorf("HTTPS 가 아닌 주소를 거부합니다: %s", u.Scheme+"://"+u.Host)
	}
	if !allowedHosts[strings.ToLower(u.Hostname())] {
		return fmt.Errorf("GitHub 도메인이 아닌 주소를 거부합니다: %s", u.Hostname())
	}
	return nil
}

// FetchLatest は最新の正式版を問い合わせる。
func FetchLatest(ctx context.Context, c *http.Client) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestURL, nil)
	if err != nil {
		return nil, err
	}
	if err := checkURL(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "artex-selfupdate")

	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitHub 에 접속하지 못했습니다(시스템 설정에서 전역 프록시를 설정할 수 있습니다): %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusTooManyRequests:
		// 認証なしの GitHub API は IP ごとに1時間あたり60回までで、出口 IP を共有しているとすぐに上限に当たる。
		return nil, fmt.Errorf("GitHub API 요청 제한에 걸렸습니다(시간당 60회). 잠시 후 다시 시도하세요")
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("저장소 %s 에 아직 정식 릴리스가 없습니다", Repo)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GitHub 응답 코드 %d", resp.StatusCode)
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("Release 해석 실패: %w", err)
	}
	if strings.TrimSpace(rel.TagName) == "" {
		return nil, fmt.Errorf("Release 에 tag 가 없습니다")
	}
	return &rel, nil
}

// AssetName は現在のプラットフォームに対応する配布パッケージ名を返す。build.sh の package_binary と揃えている：
// artex-<バージョン>-<os>-<arch>.zip（バージョン番号に v の接頭辞は付けない）。
func AssetName(tag, goos, goarch string) string {
	return fmt.Sprintf("artex-%s-%s-%s.zip", strings.TrimPrefix(tag, "v"), goos, goarch)
}

// FindAsset は Release の中から名前で資産を探す。
func (r *Release) FindAsset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if strings.EqualFold(a.Name, name) {
			return a, true
		}
	}
	return Asset{}, false
}
