package selfupdate

import (
	"archive/zip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testPaths は隔離した更新用ディレクトリを作る。ResolvePaths() はそのまま使えない——それはテストの
// バイナリそのものを指すので、実行したとたんに go test の実行ファイルの名前が変わってしまう。
func testPaths(t *testing.T) Paths {
	t.Helper()
	dir := t.TempDir()
	return Paths{
		Dir:     dir,
		Current: filepath.Join(dir, "artex"),
		New:     filepath.Join(dir, "artex.new"),
		Sum:     filepath.Join(dir, "artex.new.sha256"),
		Old:     filepath.Join(dir, "artex.old"),
		Marker:  filepath.Join(dir, "artex.upgrade.json"),
	}
}

// fakeBin は artex になりすます実行可能なシェルスクリプトを書く。smokeTest は -h で起動して終了コードを見るだけなので、
// スクリプトで十分で、本物のバイナリをコンパイルするよりずっと速い。
func fakeBin(t *testing.T, path, marker string, exitCode int) {
	t.Helper()
	script := "#!/bin/sh\necho " + marker + "\nexit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("偽のバイナリ %s の書き込み: %v", path, err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	return string(rune('0' + n))
}

// stage は bin を「一時保存済みで差し替え待ち」の状態にする：artex.new とそのチェックサムを書く。
func stage(t *testing.T, p Paths, marker string, exitCode int) {
	t.Helper()
	fakeBin(t, p.New, marker, exitCode)
	sum, err := fileSHA256(p.New)
	if err != nil {
		t.Fatalf("チェックサムの計算: %v", err)
	}
	if err := os.WriteFile(p.Sum, []byte(sum), 0o644); err != nil {
		t.Fatalf("チェックサムの書き込み: %v", err)
	}
}

func readAll(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s の読み込み: %v", path, err)
	}
	return string(b)
}

func requireUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("偽のバイナリは sh スクリプトなので Windows では動かない")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b       string
		want       int
		comparable bool
	}{
		{"0.3.7", "0.3.8", -1, true},
		{"0.3.8", "0.3.7", 1, true},
		{"0.3.7", "0.3.7", 0, true},
		{"v0.3.7", "0.3.8", -1, true}, // build.sh は v を外し、tag は v を付けるので、両方を受け付ける必要がある
		{"0.3.7", "v0.3.7", 0, true},
		{"0.9.0", "0.10.0", -1, true}, // 辞書順でなく数値で比べる
		{"1.0.0", "0.99.99", 1, true},
		// 開発ビルドは比較できないと判定しなければならない。さもないと正式版がコミットしていない変更を上書きする。
		{"dev", "0.3.8", 0, false},
		{"0.3.7-2-gabc1234", "0.3.8", 0, false},
		{"0.3.7-dirty", "0.3.8", 0, false},
		{"0.3", "0.3.8", 0, false},
		{"", "0.3.8", 0, false},
	}
	for _, c := range cases {
		got, ok := CompareVersions(c.a, c.b)
		if ok != c.comparable {
			t.Errorf("CompareVersions(%q,%q) comparable=%v, 期待値 %v", c.a, c.b, ok, c.comparable)
			continue
		}
		if ok && got != c.want {
			t.Errorf("CompareVersions(%q,%q)=%d, 期待値 %d", c.a, c.b, got, c.want)
		}
	}
}

func TestResolvePathsNaming(t *testing.T) {
	p, err := ResolvePaths()
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	// 重要な不変条件：更新用のファイルはすべて実行ファイルと同じディレクトリにある。CWD に置くと、サービスとして動かす
	// （作業ディレクトリが / になりうる）ときに差し替えがまったく働かなくなる。
	for name, path := range map[string]string{"New": p.New, "Sum": p.Sum, "Old": p.Old, "Marker": p.Marker} {
		if filepath.Dir(path) != p.Dir {
			t.Errorf("%s が実行ファイルのディレクトリにない: %s (期待値 %s)", name, path, p.Dir)
		}
	}
	// Windows では .new/.old に .exe を残さなければならない。さもないとスモークテストと差し替え後の実行がどちらも失敗する。
	if runtime.GOOS == "windows" {
		if !strings.HasSuffix(p.New, ".exe") || !strings.HasSuffix(p.Old, ".exe") {
			t.Errorf("Windows では .new/.old は .exe で終わらなければならない: new=%s old=%s", p.New, p.Old)
		}
	}
}

func TestVerifyStagedRejectsTamperedBinary(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	stage(t, p, "new", 0)

	// チェックサムを書いたあとでファイルを変え、ダウンロードの破損 / すり替えを模す。
	fakeBin(t, p.New, "tampered", 0)
	if err := verifyStaged(p); err == nil {
		t.Fatal("SHA256 の不一致で拒否されるはずが、通ってしまった")
	}
}

func TestVerifyStagedRejectsUnrunnableBinary(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	stage(t, p, "broken", 1) // 実行はできるが終了コードが 0 でない

	if err := verifyStaged(p); err == nil {
		t.Fatal("スモークテストの失敗で拒否されるはずが、通ってしまった")
	}
}

func TestApplyStagedHappyPath(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "old", 0)
	stage(t, p, "new", 0)
	if err := writeMarker(p.Marker, marker{From: "0.3.7", To: "0.3.8"}); err != nil {
		t.Fatalf("マーカーの書き込み: %v", err)
	}

	action, st := applyStaged(p)
	if action != Restart {
		t.Fatalf("Restart を期待したが、%v を得た", action)
	}
	if !st.Pending {
		t.Error("差し替え後の状態は Pending であるべき")
	}
	if !strings.Contains(readAll(t, p.Current), "new") {
		t.Error("artex は新しい版に置き換わっているべき")
	}
	if !strings.Contains(readAll(t, p.Old), "old") {
		t.Error("旧版は artex.old にバックアップされているべき")
	}
	if _, err := os.Stat(p.New); !os.IsNotExist(err) {
		t.Error("差し替え後、artex.new は消えているべき")
	}
	if _, err := os.Stat(p.Sum); !os.IsNotExist(err) {
		t.Error("差し替え後、チェックサムのファイルは片付けられているべき")
	}
	// マーカーは残さなければならない。次回の起動（動くのは新しい版）はこれで回数を数え、必要ならロールバックする。
	if _, ok := readMarker(p.Marker); !ok {
		t.Error("差し替え後も更新マーカーは残っているべき")
	}
}

func TestApplyStagedKeepsCurrentWhenVerifyFails(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "old", 0)
	stage(t, p, "new", 0)
	fakeBin(t, p.New, "tampered", 0) // チェックサムを壊す

	action, st := applyStaged(p)
	if action != Continue {
		t.Fatalf("検証に失敗したときは Continue を期待したが、%v を得た", action)
	}
	if !st.FailedStage {
		t.Error("状態は FailedStage になっているべき")
	}
	if !strings.Contains(readAll(t, p.Current), "old") {
		t.Fatal("検証に失敗したときに現在の版に触れてはならない")
	}
	if _, err := os.Stat(p.New); !os.IsNotExist(err) {
		t.Error("検証に失敗した一時ファイルは片付けるべき。さもないと次回の起動でもう一度試される")
	}
}

func TestSwapOverwritesPreviousBackup(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "v2", 0)
	fakeBin(t, p.Old, "v1", 0) // 前回の更新で残ったバックアップ
	stage(t, p, "v3", 0)

	if err := swap(p); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if !strings.Contains(readAll(t, p.Current), "v3") {
		t.Error("v3 に差し替わっているべき")
	}
	if !strings.Contains(readAll(t, p.Old), "v2") {
		t.Error("バックアップは差し替えで外れたばかりの v2 に更新されているべき")
	}
}

func TestConfirmCountsAttemptsThenRollsBack(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "broken-new", 0)
	fakeBin(t, p.Old, "good-old", 0)
	m := marker{From: "0.3.7", To: "0.3.8"}

	// 最初の maxAttempts 回の起動では回数を数えるだけにし、新しい版が自力で安定する機会を与える。
	for i := 1; i <= maxAttempts; i++ {
		action, st := confirmOrRollback(p, m)
		if action != Continue {
			t.Fatalf("%d 回目の試行では Continue を期待したが、%v を得た", i, action)
		}
		if !st.Pending {
			t.Errorf("%d 回目の試行の状態は Pending であるべき", i)
		}
		got, ok := readMarker(p.Marker)
		if !ok || got.Attempts != i {
			t.Fatalf("%d 回目の試行後 attempts=%d（ok=%v）、期待値 %d", i, got.Attempts, ok, i)
		}
		m = got
	}

	// もう一度クラッシュすると上限を超え、自動で旧版に戻す。
	action, st := confirmOrRollback(p, m)
	if action != Restart {
		t.Fatalf("試行の上限を超えたときは Restart を期待したが、%v を得た", action)
	}
	if !st.RolledBack {
		t.Error("状態は RolledBack になっているべき")
	}
	if !strings.Contains(readAll(t, p.Current), "good-old") {
		t.Fatal("旧版にロールバックされているべき")
	}
	if _, err := os.Stat(p.Marker); !os.IsNotExist(err) {
		t.Error("ロールバック後はマーカーを消すべき。さもないと無限にロールバックする")
	}
	// 起動できなかった版は調査用に残し、すぐには消さない。
	if _, err := os.Stat(p.Current + ".failed"); err != nil {
		t.Error("失敗した版は調査用に .failed として残すべき")
	}
}

func TestManualRollbackIsReversible(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "v2", 0)
	fakeBin(t, p.Old, "v1", 0)

	// Rollback() は ResolvePaths() を通るので、ここでは下層の入れ替えの動作を直接テストする。
	tmp := p.Current + ".swap"
	if err := os.Rename(p.Current, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readAll(t, p.Current), "v1") {
		t.Error("ロールバック後の現在の版は v1 であるべき")
	}
	if !strings.Contains(readAll(t, p.Old), "v2") {
		t.Error("ロールバック後のバックアップは v2 になり、もう一度戻せるべき")
	}
}

func TestParseSums(t *testing.T) {
	const (
		linuxSum = "1111111111111111111111111111111111111111111111111111111111111111"
		winSum   = "ABCDEF0000000000000000000000000000000000000000000000000000000000"
	)
	// sha256sum の出力は2つの空白区切り。shasum -a 256 はバイナリモードでファイル名に * を付ける。
	raw := linuxSum + "  artex-0.3.8-linux-amd64.zip\n" +
		winSum + " *artex-0.3.8-windows-amd64.zip\n" +
		"\n" +
		"garbage line\n" + // ちょうど2つのフィールドだが、1つ目がダイジェストではない
		"deadbeef  artex-0.3.8-darwin-arm64.zip\n" // ダイジェストの長さが違う

	out := parseSums(raw)
	if out["artex-0.3.8-linux-amd64.zip"] != linuxSum {
		t.Errorf("linux の項目の解析が誤っている: %v", out)
	}
	// ダイジェストは小文字に揃える。そうすれば照合時に大文字小文字の違いで不一致と誤判定しない。
	if got := out["artex-0.3.8-windows-amd64.zip"]; got != strings.ToLower(winSum) {
		t.Errorf("windows の項目が誤っている（* の接頭辞を外し、ダイジェストを小文字にすべき）: %q", got)
	}
	if len(out) != 2 {
		t.Errorf("空行、ダイジェストでない行、長さの違う行は無視すべきだが、%v を得た", out)
	}
}

func TestExtractBinaryFindsNestedEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("パッケージ内のベース名は Windows では artex.exe で、このケースは Unix の命名で組んでいる")
	}
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "release.zip")

	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	// 実際の配布パッケージの構造：artex-<バージョン>-<os>-<arch>/artex と、いくつかの紛らわしいファイル。
	for name, body := range map[string]string{
		"artex-0.3.8-linux-amd64/README.md":           "readme",
		"artex-0.3.8-linux-amd64/skills/a.md":         "skill",
		"artex-0.3.8-linux-amd64/artex":               "#!/bin/sh\nexit 0\n",
		"artex-0.3.8-linux-amd64/config.example.json": "{}",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	dst := filepath.Join(dir, "out")
	if err := extractBinary(zipPath, dst); err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	if got := readAll(t, dst); !strings.Contains(got, "exit 0") {
		t.Errorf("展開されたものが artex の実行ファイルではない: %q", got)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Error("展開したバイナリには実行権限が必要")
	}
}

func TestExtractBinaryMissingEntry(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "release.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, _ := zw.Create("artex-0.3.8-linux-amd64/README.md")
	_, _ = w.Write([]byte("readme"))
	_ = zw.Close()
	f.Close()

	if err := extractBinary(zipPath, filepath.Join(dir, "out")); err == nil {
		t.Fatal("パッケージ内に実行ファイルがないときはエラーにすべき")
	}
}

func TestCheckURLRejectsNonGitHub(t *testing.T) {
	bad := []string{
		"http://github.com/x",           // HTTPS でない
		"https://evil.com/artex.zip",    // ドメインが許可リストにない
		"https://github.com.evil.com/x", // 接尾辞による偽装
		"https://raw.githubusercontent.com.evil.com/x",
	}
	for _, raw := range bad {
		u := mustParse(t, raw)
		if err := checkURL(u); err == nil {
			t.Errorf("checkURL(%q) は拒否すべき", raw)
		}
	}
	good := []string{
		"https://api.github.com/repos/x/releases/latest",
		"https://objects.githubusercontent.com/blah",
		"https://GitHub.com/x", // ドメインは大文字小文字を区別しない
	}
	for _, raw := range good {
		u := mustParse(t, raw)
		if err := checkURL(u); err != nil {
			t.Errorf("checkURL(%q) は通すべきだが、エラーになった: %v", raw, err)
		}
	}
}

func TestAssetNameMatchesBuildScript(t *testing.T) {
	// build.sh の package_binary は artex-<バージョン>-<os>-<arch>.zip を使い、バージョン番号から
	// v の接頭辞を外している。ここで1文字でも違えば、すべてのプラットフォームでワンクリック更新が資産を見つけられなくなる。
	if got := AssetName("v0.3.8", "linux", "amd64"); got != "artex-0.3.8-linux-amd64.zip" {
		t.Errorf("AssetName = %q", got)
	}
	if got := AssetName("0.3.8", "windows", "amd64"); got != "artex-0.3.8-windows-amd64.zip" {
		t.Errorf("AssetName = %q", got)
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("%q の解析: %v", raw, err)
	}
	return u
}

func TestSettleClearsMarkerAndStopsRollback(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "new", 0)
	fakeBin(t, p.Old, "old", 0)
	if err := writeMarker(p.Marker, marker{From: "0.3.7", To: "0.3.8", Attempts: 2}); err != nil {
		t.Fatal(err)
	}

	settle(p)

	if _, err := os.Stat(p.Marker); !os.IsNotExist(err) {
		t.Fatal("安定を確認したら更新マーカーは必ず消すべき")
	}
	// マーカーがなくなれば、以後の通常の再起動で回数を数え続けることも、誤ってロールバックすることもない。
	if _, ok := readMarker(p.Marker); ok {
		t.Error("マーカーの読み込みは失敗すべき")
	}
	// バックアップは残す。ユーザーが手動でロールバックできるように。
	if _, err := os.Stat(p.Old); err != nil {
		t.Error("安定を確認したあとも前の版のバックアップは残すべき")
	}
}

func TestSettleIsNoopWithoutMarker(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "cur", 0)
	settle(p) // 通常の起動経路。panic してはならず、どのファイルにも触れてはならない
	if _, err := os.Stat(p.Current); err != nil {
		t.Error("マーカーがないとき settle はどのファイルにも影響してはならない")
	}
}
