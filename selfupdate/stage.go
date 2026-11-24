package selfupdate

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"runtime"
	"strings"
	"time"
)

// sumsAsset は release.yml が生成するチェックサムの一覧で、Release のすべての zip を対象とする。
const sumsAsset = "SHA256SUMS"

// maxBinarySize は展開したバイナリの大きさを制限し、不正な zip でディスクが埋まるのを防ぐ。
const maxBinarySize = 512 << 20 // 512 MiB

// Phase は更新の過程の段階で、SSE イベントの phase フィールドにそのまま使う。
type Phase string

const (
	PhaseIdle     Phase = "idle"
	PhaseDownload Phase = "downloading"
	PhaseVerify   Phase = "verifying"
	PhaseExtract  Phase = "extracting"
	PhaseStaged   Phase = "staged"
	PhaseFailed   Phase = "failed"
)

// Progress は呼び出し側が渡し、進み具合をフロントエンドに送るのに使う。pct はダウンロードの段階でだけ意味を持ち（0-100）、
// それ以外の段階では -1 を渡す。
type Progress func(ph Phase, pct int, msg string)

// Stage は指定した Release の現在のプラットフォーム用パッケージをダウンロードし、検証したうえで新しいバイナリを artex.new として一時保存する。
//
// バイナリ単体でなく zip 全体を使う理由は2つある：既存の Release の SHA256SUMS はもともと
// zip しか対象にしていないので、zip を使えば CI を変えずに済み、すでに公開した過去の版とも互換を保てる。さらに zip には
// skills/ も入っているので、将来組み込みの skill を同期する余地を残せる。代償は skills の数百 KB を余分にダウンロードすることだけ。
//
// 関数が戻った時点で一時保存は完了しており、呼び出し側はこのあと穏やかに停止して ExitRestart で終了する。
func Stage(ctx context.Context, c *http.Client, rel *Release, currentVersion string, prog Progress) error {
	if prog == nil {
		prog = func(Phase, int, string) {}
	}
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if err := checkWritable(p.Dir); err != nil {
		return err
	}

	name := AssetName(rel.TagName, runtime.GOOS, runtime.GOARCH)
	asset, ok := rel.FindAsset(name)
	if !ok {
		return fmt.Errorf("이 버전은 %s/%s 용 릴리스 패키지를 제공하지 않습니다(%s 누락)", runtime.GOOS, runtime.GOARCH, name)
	}

	prog(PhaseDownload, 0, "체크섬 목록을 가져오는 중…")
	sums, err := fetchSums(ctx, c, rel)
	if err != nil {
		return err
	}
	want, ok := sums[name]
	if !ok {
		return fmt.Errorf("%s 에 %s 항목이 없어 검증되지 않은 바이너리 설치를 거부합니다", sumsAsset, name)
	}

	// 一時ファイルはすべて宛先のディレクトリに置き、最後の rename が同じファイルシステム内のアトミックな操作になるようにする
	// （デバイスをまたぐ rename は失敗し、/tmp はしばしば別のマウントポイントになっている）。
	zipPath := p.New + ".zip.part"
	binPath := p.New + ".part"
	defer func() {
		_ = os.Remove(zipPath)
		_ = os.Remove(binPath)
	}()

	prog(PhaseDownload, 0, fmt.Sprintf("%s 내려받는 중(%s)…", name, humanSize(asset.Size)))
	got, err := download(ctx, c, asset, zipPath, prog)
	if err != nil {
		return err
	}

	prog(PhaseVerify, -1, "SHA256 검증 중…")
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("SHA256 이 일치하지 않습니다: 기대값 %s, 실제값 %s(다운로드가 손상됐거나 변조됨)", short(want), short(got))
	}

	prog(PhaseExtract, -1, "압축을 풀고 스모크 테스트 중…")
	if err := extractBinary(zipPath, binPath); err != nil {
		return err
	}
	if err := smokeTest(binPath); err != nil {
		return fmt.Errorf("새 버전이 현재 시스템에서 실행되지 않습니다: %w", err)
	}

	// 一時ファイル自身の sha256 を別に保存しておく：次回の起動で差し替える前にもう一度検証し、
	// 一時保存から再起動までの間にファイルが変えられたり壊れたりするのを防ぐ。
	binSum, err := fileSHA256(binPath)
	if err != nil {
		return fmt.Errorf("새 바이너리 체크섬 계산 실패: %w", err)
	}
	if err := os.WriteFile(p.Sum, []byte(binSum), 0o644); err != nil {
		return fmt.Errorf("체크섬 기록 실패: %w", err)
	}
	if err := os.Rename(binPath, p.New); err != nil {
		_ = os.Remove(p.Sum)
		return fmt.Errorf("새 버전 준비 실패: %w", err)
	}

	if err := writeMarker(p.Marker, marker{
		From:     currentVersion,
		To:       strings.TrimPrefix(rel.TagName, "v"),
		StagedAt: time.Now().Unix(),
	}); err != nil {
		// マーカーが影響するのは自動ロールバックの能力だけで、一時ファイル自体はもう配置済みなので、これを理由に更新を中断しない。
		prog(PhaseStaged, -1, "경고: 업데이트 표시 파일 기록에 실패해 이번 업데이트는 자동 롤백 보호를 받지 못합니다")
	}

	prog(PhaseStaged, 100, "새 버전이 준비되었습니다. 재시작 중…")
	return nil
}

// fetchSums は SHA256SUMS をダウンロードして解析し、ファイル名 → 16進数のダイジェスト を返す。
func fetchSums(ctx context.Context, c *http.Client, rel *Release) (map[string]string, error) {
	asset, ok := rel.FindAsset(sumsAsset)
	if !ok {
		return nil, fmt.Errorf("이 릴리스에 %s 항목이 없어 무결성을 검증할 수 없으므로 업데이트를 거부합니다", sumsAsset)
	}
	body, err := get(ctx, c, asset.URL)
	if err != nil {
		return nil, fmt.Errorf("%s 내려받기 실패: %w", sumsAsset, err)
	}
	defer body.Close()

	raw, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%s 읽기 실패: %w", sumsAsset, err)
	}
	out := parseSums(string(raw))
	if len(out) == 0 {
		return nil, fmt.Errorf("%s 내용이 비어 있거나 형식을 인식할 수 없습니다", sumsAsset)
	}
	return out, nil
}

// parseSums は sha256sum 形式の一覧を解析し、ファイル名 → 16進数のダイジェスト を返す。
//
// 1つ目のフィールドが 64 桁の16進数のときだけ採用する。「ちょうど2つのフィールド」だけで判断するのは不十分で——
// 2語から成る説明文の行がどれも正当な項目とみなされ、ダイジェストの表にごみの値が入り、
// 本物の資産がかえって誤ったダイジェストに一致してしまうおそれがある。
func parseSums(raw string) map[string]string {
	out := map[string]string{}
	for line := range strings.Lines(raw) {
		// 形式は "<sha256>  <filename>"（sha256sum は2つの空白を使う。shasum のバイナリ
		// モードはファイル名に * の接頭辞を付ける）。
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 || !isHexSHA256(fields[0]) {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == "" {
			continue
		}
		out[name] = strings.ToLower(fields[0])
	}
	return out
}

func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// download は資産を dst に書き込み、同時に SHA256 を計算し、Content-Length をもとに進み具合を報告する。
func download(ctx context.Context, c *http.Client, a Asset, dst string, prog Progress) (string, error) {
	body, err := get(ctx, c, a.URL)
	if err != nil {
		return "", fmt.Errorf("%s 내려받기 실패: %w", a.Name, err)
	}
	defer body.Close()

	f, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf("임시 파일 생성 실패: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	pw := &progressWriter{total: a.Size, prog: prog, name: a.Name, last: time.Now()}
	if _, err := io.Copy(io.MultiWriter(f, h, pw), body); err != nil {
		return "", fmt.Errorf("다운로드가 중단되었습니다: %w", err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("디스크 기록 실패: %w", err)
	}
	if a.Size > 0 && pw.written != a.Size {
		return "", fmt.Errorf("다운로드가 완전하지 않습니다: 기대 %d바이트, 실제 %d바이트", a.Size, pw.written)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// get は許可リストに縛られた GET を発行し、レスポンスボディを返す。
func get(ctx context.Context, c *http.Client, rawURL string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if err := checkURL(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "artex-selfupdate")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// extractBinary は配布パッケージから artex の実行ファイルを取り出す。
//
// パッケージ内の構造は artex-<バージョン>-<os>-<arch>/artex だが、ここでは完全なパスを組み立てず**ベース名**で一致させる：
// バージョン番号はパッケージ名に一度出てくるので、1文字でも組み立てを誤ると更新全体が失敗する。ベース名で探すほうが変更に強い。
func extractBinary(zipPath, dst string) error {
	want := "artex"
	if runtime.GOOS == "windows" {
		want = "artex.exe"
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("릴리스 패키지 열기 실패: %w", err)
	}
	defer zr.Close()

	for _, entry := range zr.File {
		if entry.FileInfo().IsDir() || !strings.EqualFold(path.Base(entry.Name), want) {
			continue
		}
		rc, err := entry.Open()
		if err != nil {
			return fmt.Errorf("%s 읽기 실패: %w", entry.Name, err)
		}
		defer rc.Close()

		f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return fmt.Errorf("새 바이너리 쓰기 실패: %w", err)
		}
		defer f.Close()

		n, err := io.Copy(f, io.LimitReader(rc, maxBinarySize+1))
		if err != nil {
			return fmt.Errorf("%s 압축 해제 실패: %w", entry.Name, err)
		}
		if n > maxBinarySize {
			return fmt.Errorf("릴리스 패키지 안의 실행 파일이 %s 보다 커서 압축 해제를 거부합니다", humanSize(maxBinarySize))
		}
		if n == 0 {
			return fmt.Errorf("릴리스 패키지 안의 %s 파일이 비어 있습니다", want)
		}
		return f.Sync()
	}
	return fmt.Errorf("릴리스 패키지에서 %s 파일을 찾지 못했습니다", want)
}

// checkWritable はディレクトリが書き込み可能かを先に確かめる。これがないと、root 以外で動かしたり、バイナリがシステムの
// ディレクトリに置かれていたりするとき、数十 MB をダウンロードし終えたあとの差し替えの瞬間になって失敗する。
func checkWritable(dir string) error {
	probe, err := os.CreateTemp(dir, ".artex-update-probe-*")
	if err != nil {
		return fmt.Errorf("프로그램 디렉터리 %s 에 쓸 수 없어 자동 업데이트를 할 수 없습니다(권한을 확인하거나 수동 업데이트를 사용하세요): %w", dir, err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}

// progressWriter は書き込んだバイト数を数え、報告の頻度を抑えて、32KiB の塊ごとに SSE を1件送るのを避ける。
type progressWriter struct {
	total   int64
	written int64
	name    string
	prog    Progress
	last    time.Time
}

func (w *progressWriter) Write(b []byte) (int, error) {
	w.written += int64(len(b))
	if time.Since(w.last) < 300*time.Millisecond {
		return len(b), nil
	}
	w.last = time.Now()
	pct := -1
	if w.total > 0 {
		pct = int(w.written * 100 / w.total)
	}
	w.prog(PhaseDownload, pct, fmt.Sprintf("내려받는 중 %s / %s", humanSize(w.written), humanSize(w.total)))
	return len(b), nil
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12] + "…"
	}
	return sum
}
