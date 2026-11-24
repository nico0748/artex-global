// Package selfupdate implements ARTEX の画面からのワンクリック更新：GitHub Release から新しい版の
// バイナリを取得して検証し、一時保存したうえで、次回の起動時にアトミックに差し替える。
//
// 全体の分担（start.sh / start.bat を参照）：
//
//	起動スクリプト = 単純な常駐ループ。「プロセス終了後に終了コードで再起動するかを決める」ことだけを受け持つ
//	このパッケージ = 間違えやすい処理のすべて（ダウンロード / SHA256 の検証 / スモークテスト / 差し替え / 失敗時のロールバック）
//
// 差し替えをスクリプトでなく Go に置いたのは、sha256 の検証とスモークテストを sh と bat で
// 2通り（sha256sum / shasum / certutil）書く必要があり、そこがまさに最も間違えてはならない部分だからだ——動かない
// バイナリに差し替えると、常駐プロセスが律儀に何度も起動し続け、ユーザーはマシンに入って手で直すしかなくなる。
//
// 1回の更新を完了するまでに、プロセスは3回起動する：
//
//	① 旧版の server が /api/update/apply を受ける → ダウンロードと検証 → artex.new に一時保存 → exit 75
//	② スクリプトが旧版を再起動 → Bootstrap が artex.new を見つける → 検証+スモークテスト → 差し替え → exit 75
//	③ スクリプトが再起動し、この時点で新しい版になっている → Bootstrap が試行を1回記録 → 起動に成功したらマーカーを消す
//
// どの段階で失敗しても旧版に戻る：② で検証に通らなければ一時ファイルを消して旧版のまま動く。③ で3回続けて
// マーカーを消すまで生き延びられなければ（起動できずにクラッシュすれば）、自動的に artex.old へ戻す。
package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ExitRestart は「常駐プロセスに再起動してもらう」ための終了コード（EX_TEMPFAIL）。起動スクリプトはこれを見ると
// すぐに再実行し、クラッシュ時の待ち時間には数えない。0 はユーザーによる通常の停止（スクリプトはループを抜ける）で、それ以外はすべてクラッシュとみなす。
const ExitRestart = 75

// maxAttempts は差し替え後に許す起動の試行回数。新しい版は起動のたびに回数を +1 し、
// settleDelay を生き延びたらマーカーを消す。maxAttempts 回続けてクラッシュしたら新しい版はそもそも起動できないとみなし、自動でロールバックする。
const maxAttempts = 3

// Paths は1回の更新に関わるすべてのファイルで、**実行ファイルのあるディレクトリ**にまとめて置く。
// CWD はわざと使わない：サービスとして動かすと作業ディレクトリが / や任意のパスになりうるので、CWD を使うと一時ファイルが
// 別の場所に置かれ、差し替えの処理がまったく働かなくなる。
type Paths struct {
	Dir     string // 実行ファイルのあるディレクトリ
	Current string // 現在動いているバイナリ    artex      / artex.exe
	New     string // 一時保存した新しい版      artex.new  / artex.new.exe
	Sum     string // 新しい版の sha256（hex）  artex.new.sha256 / artex.new.exe.sha256
	Old     string // 差し替え前にバックアップした旧版  artex.old  / artex.old.exe
	Marker  string // 更新状態のマーカー        artex.upgrade.json
}

// ResolvePaths は現在の実行ファイルから更新に使うすべてのパスを導き出す。
//
// Windows では .new/.old にも .exe の拡張子が必要で、さもないとスモークテストと差し替え後の実行がどちらも失敗する。
// そのため先に拡張子を外してからつなげることで、2つのプラットフォームで命名を対称にしている。
func ResolvePaths() (Paths, error) {
	exe, err := os.Executable()
	if err != nil {
		return Paths{}, fmt.Errorf("실행 파일 위치 확인 실패: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	name := filepath.Base(exe)
	ext := filepath.Ext(name) // Windows では ".exe"、Unix では通常は空
	stem := strings.TrimSuffix(name, ext)

	join := func(suffix string) string { return filepath.Join(dir, stem+suffix+ext) }
	return Paths{
		Dir:     dir,
		Current: exe,
		New:     join(".new"),
		Sum:     join(".new") + ".sha256",
		Old:     join(".old"),
		Marker:  filepath.Join(dir, stem+".upgrade.json"),
	}, nil
}

// marker は1回の差し替えの進み具合を記録し、新しい版が起動できないときに自動ロールバックを起こすのに使う。
type marker struct {
	From     string `json:"from"`     // 更新前の版
	To       string `json:"to"`       // 更新先の版
	Attempts int    `json:"attempts"` // 差し替え後に起動を試みた回数
	StagedAt int64  `json:"staged_at"`
}

func readMarker(path string) (marker, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return marker{}, false
	}
	var m marker
	if json.Unmarshal(b, &m) != nil {
		return marker{}, false
	}
	return m, true
}

func writeMarker(path string, m marker) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// cleanStaged は一時ファイルを消す。差し替えの成功、検証の失敗、ユーザーによる取り消しのいずれでも通り、
// 残った artex.new が次回の起動でもう一度試されるのを防ぐ。
func cleanStaged(p Paths) {
	_ = os.Remove(p.New)
	_ = os.Remove(p.Sum)
}

// CompareVersions は2つのバージョン番号を比べ、-1/0/1（a<b / a==b / a>b）を返す。
// ok=false は少なくとも一方が比較できるバージョン番号でないこと（たとえばローカルの開発ビルドの "dev" や
// git describe が出す "0.3.7-2-gabc1234-dirty"）を表す。このとき呼び出し側はワンクリック更新を無効にすべきで、
// さもないと開発中のビルドが正式版に「更新」され、コミットしていない変更が上書きされる。
func CompareVersions(a, b string) (int, bool) {
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if !aok || !bok {
		return 0, false
	}
	for i := range 3 {
		if av[i] != bv[i] {
			if av[i] < bv[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// parseVersion は "v0.3.7" / "0.3.7" 形式のバージョン番号を [3]int に解析する。
//
// 受け付けるのは純粋な3段の形式だけ：build.sh は tag 以外のビルドで git describe を使って
// "0.3.7-2-gabc1234" のような接尾辞付きの版を出すが、これらは比較できないと判定しなければならず、
// 0.3.7 として扱ってはならない——さもないと開発ビルドが「すでに最新」と誤判定されたり、正式版に上書きされたりする。
func parseVersion(s string) ([3]int, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return [3]int{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// InDocker はプロセスがコンテナの中で動いているかを報告する。Docker では差し替えはコンテナの書き込み可能な層に書かれ、
// `docker compose up -d` でコンテナを作り直すとイメージに入っている版に戻る——これは想定どおりの動作だが
// （そのときユーザーはもともと新しいイメージを取得している）、フロントエンドがそれをもとに説明できる必要がある。
func InDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	b, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	s := string(b)
	return strings.Contains(s, "docker") || strings.Contains(s, "containerd")
}
