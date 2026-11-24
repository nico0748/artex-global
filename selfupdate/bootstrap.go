package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// smokeEnv は、スモークテストで起動された子プロセスに Bootstrap をそのまま飛ばさせる。
//
// 厳密には付けなくても問題は起きない：子プロセスの os.Executable() は artex.new なので、導き出される
// パスはすべて .new 接頭辞付きになり、本物の更新ファイルには触れない。ただしこうした偶然に頼るのはもろすぎるので、
// 明示的に短絡させたほうがひと目でわかり、子プロセスの無駄なディスク探索も省ける。
const smokeEnv = "ARTEX_SELFUPDATE_SMOKE"

// Action は Bootstrap が main に与える指示。
type Action int

const (
	// Continue：通常どおり server を起動する。
	Continue Action = iota
	// Restart：すぐに ExitRestart で終了し、常駐スクリプトに再起動させる。
	Restart
)

// State は今回の起動時の更新状態を表し、/api/update/check がフロントエンドに
// 「前回の更新が成功したのか、ロールバックされたのか」をありのまま伝えるのに使う。
type State struct {
	Pending     bool   // 差し替え後、まだ安定を確認していない
	RolledBack  bool   // 今回の起動で自動ロールバックを実行したばかり
	FailedStage bool   // 一時ファイルの検証/スモークテストに通らず、破棄した
	Detail      string // ユーザー向けの一文の説明
}

// Bootstrap は main の冒頭で動き、ポートの待ち受けやデータベースを開くより前に呼ばなければならない。
//
// 3つの局面：
//
//	① 一時ファイル artex.new がある → 検証 + スモークテスト。通れば差し替えて再起動を求め、通らなければ破棄して旧版のまま動く
//	② マーカーファイルだけが残っている → 差し替えた直後なので試行を1回数える。連続失敗が規定回数に達したらロールバック
//	③ 何もない                     → 通常の起動
func Bootstrap() (Action, State) {
	if os.Getenv(smokeEnv) != "" {
		return Continue, State{}
	}
	p, err := ResolvePaths()
	if err != nil {
		log.Printf("[update] ブートストラップを飛ばす：%v", err)
		return Continue, State{}
	}

	if _, err := os.Stat(p.New); err == nil {
		return applyStaged(p)
	}

	m, ok := readMarker(p.Marker)
	if !ok {
		return Continue, State{}
	}
	return confirmOrRollback(p, m)
}

// applyStaged は「一時ファイルがある」局面を扱う：検証に通れば差し替え、失敗すれば破棄する。
//
// ここは更新の流れ全体で実行ファイルを上書きする唯一の場所であり、最後の関門でもある——スモークテストが
// ダウンロードの破損、アーキテクチャの取り違え、動的リンクの欠落といった問題を食い止める。動かないバイナリを一度通してしまうと、
// 常駐スクリプトが飽きることなく何度も起動し続け、Go のコードには実行の機会がまったくないので、自動ロールバックもしようがない。
func applyStaged(p Paths) (Action, State) {
	m, _ := readMarker(p.Marker)

	if err := verifyStaged(p); err != nil {
		log.Printf("[update] 一時保存した新しい版が検証に通らなかったため破棄し、現在の版のまま動かす：%v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "새 버전 검증에 실패해 폐기했습니다: " + err.Error()}
	}

	if err := swap(p); err != nil {
		log.Printf("[update] 差し替えに失敗したため、現在の版のまま動かす：%v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "버전 교체에 실패했습니다: " + err.Error()}
	}

	// 差し替え成功。マーカーは残し、次回の起動（動くのは新しい版）で安定しているかを確かめる。
	m.Attempts = 0
	if m.StagedAt == 0 {
		m.StagedAt = time.Now().Unix()
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] 更新マーカーの書き込みに失敗（自動ロールバックができなくなる）：%v", err)
	}
	log.Printf("[update] %s に差し替えたので、再起動のため終了する（exit %d）", orUnknown(m.To), ExitRestart)
	return Restart, State{Pending: true}
}

// confirmOrRollback は「差し替え後の起動」を扱う：試行回数を数え、上限を超えたら旧版に戻す。
//
// 回数は Go のコードが動き出してから増えるので、対象は「実行はできるが初期化中にクラッシュする」
// （設定の非互換、ポートの使用中、DB マイグレーションの失敗）類の故障である。「そもそも exec できない」は差し替え前の
// スモークテストで食い止める。両者を合わせて初めて完全になる。
func confirmOrRollback(p Paths, m marker) (Action, State) {
	m.Attempts++
	if m.Attempts > maxAttempts {
		if err := rollback(p); err != nil {
			// ロールバックまで失敗したら、もう再起動しない。さもないと無限に再起動する。マーカーを消し、
			// 現在の状態のままプロセスを起動させる——起動できなくても、ユーザーは少なくともログで原因を見られる。
			log.Printf("[update] 新しい版が %d 回連続で起動に失敗し、ロールバックにも失敗した：%v", maxAttempts, err)
			_ = os.Remove(p.Marker)
			return Continue, State{Detail: "새 버전 시작에 실패하고 롤백도 실패했습니다: " + err.Error()}
		}
		log.Printf("[update] 新しい版が %d 回連続で起動に失敗したため、%s にロールバックした。再起動のため終了する（exit %d）",
			maxAttempts, orUnknown(m.From), ExitRestart)
		_ = os.Remove(p.Marker)
		return Restart, State{RolledBack: true, Detail: fmt.Sprintf("새 버전 시작에 실패해 이전 버전으로 롤백했습니다(%s)", orUnknown(m.From))}
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] 更新マーカーの更新に失敗：%v", err)
	}
	log.Printf("[update] 新しい版を起動中（%d/%d 回目の試行）。安定して動いたら更新を確定する",
		m.Attempts, maxAttempts)
	return Continue, State{Pending: true}
}

// Settle は新しい版が安定して動いていることを確かめ、更新マーカーを消す。
//
// main が HTTP の待ち受けを始めたあとに遅れて呼ぶ：この時間を生き延びて初めて有効とし、そうでなければマーカーはそのまま残り、
// 次回の起動でも試行回数を数え続け、やがてロールバックが起きる。
func Settle() {
	p, err := ResolvePaths()
	if err != nil {
		return
	}
	settle(p)
}

func settle(p Paths) {
	if _, ok := readMarker(p.Marker); !ok {
		return // 更新後の起動ではないので、することはない
	}
	if err := os.Remove(p.Marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("[update] 更新マーカーの削除に失敗：%v", err)
		return
	}
	log.Printf("[update] 新しい版が安定して動いているので、更新を完了した（前の版は %s として残す）", p.Old)
}

// SettleDelay は「新しい版が生き延びた」と判定するのに必要な稼働時間。
const SettleDelay = 30 * time.Second

// verifyStaged は一時ファイルを検証する：まず SHA256 を照合し、それから実際に一度起動してみる。
func verifyStaged(p Paths) error {
	want, err := os.ReadFile(p.Sum)
	if err != nil {
		return fmt.Errorf("체크섬 읽기 실패: %w", err)
	}
	got, err := fileSHA256(p.New)
	if err != nil {
		return fmt.Errorf("체크섬 계산 실패: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(string(want)), got) {
		return errors.New("SHA256 이 일치하지 않습니다(다운로드가 손상됐거나 변조됨)")
	}
	return smokeTest(p.New)
}

// smokeTest は -h で新しいバイナリを起動し、現在のシステムで本当に実行できるかを確かめる。
// これでダウンロードの途切れ、アーキテクチャの取り違え（exec format error）、依存関係の欠落といった多くの問題を食い止められる。
func smokeTest(bin string) error {
	if err := os.Chmod(bin, 0o755); err != nil {
		return fmt.Errorf("실행 권한 부여 실패: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "-h")
	cmd.Env = append(os.Environ(), smokeEnv+"=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return errors.New("스모크 테스트 시간 초과(새 바이너리가 응답하지 않음)")
	}
	if err != nil {
		snippet := strings.TrimSpace(string(out))
		if len(snippet) > 300 {
			snippet = snippet[:300] + "…"
		}
		return fmt.Errorf("스모크 테스트 실패: %v: %s", err, snippet)
	}
	return nil
}

// swap は現在のバイナリを一時保存した新しい版に差し替える。
//
// Unix も Windows も実行中の実行ファイルの rename は許している（Windows で禁止されているのは削除と
// 上書きで、rename は含まれない）ので、ここではプラットフォームを分ける必要も、先に自分を止める必要もない。
func swap(p Paths) error {
	// Windows の rename は既存の宛先を上書きしないので、前回の更新で残った .old を先に消さなければならない。
	if err := os.Remove(p.Old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("이전 백업 %s 정리 실패: %w", p.Old, err)
	}
	if err := os.Rename(p.Current, p.Old); err != nil {
		return fmt.Errorf("현재 버전 백업 실패: %w", err)
	}
	if err := os.Rename(p.New, p.Current); err != nil {
		// 差し替えに失敗したが現在の版はすでに移動されているので、必ず元に戻す。さもないと次回の起動で実行ファイルがない。
		if rerr := os.Rename(p.Old, p.Current); rerr != nil {
			return fmt.Errorf("새 버전 설치 실패(%v), 현재 버전 복구도 실패: %w", err, rerr)
		}
		return fmt.Errorf("새 버전 설치 실패: %w", err)
	}
	_ = os.Remove(p.Sum)
	return nil
}

// rollback は swap がバックアップした旧版を戻す。
func rollback(p Paths) error {
	if _, err := os.Stat(p.Old); err != nil {
		return fmt.Errorf("롤백할 백업이 없습니다(%s): %w", p.Old, err)
	}
	// 起動できない新しい版はすぐに消さず、調査用に .failed へ移しておく。
	failed := p.Current + ".failed"
	_ = os.Remove(failed)
	if err := os.Rename(p.Current, failed); err != nil {
		return fmt.Errorf("실패한 버전 이동 실패: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		return fmt.Errorf("이전 버전 복구 실패: %w", err)
	}
	return nil
}

// Rollback は /api/update/rollback の実装：自分から前の版に戻す。
// 行うのは差し替えだけで、再起動は同じく常駐スクリプトに任せる（呼び出し側はこのあと ExitRestart で終了する）。
func Rollback() error {
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p.Old); err != nil {
		return errors.New("롤백할 이전 버전이 없습니다(" + p.Old + " 없음)")
	}
	cleanStaged(p)
	if err := smokeTest(p.Old); err != nil {
		return fmt.Errorf("이전 버전을 실행할 수 없어 롤백을 거부합니다: %w", err)
	}
	// 現在の版とバックアップを入れ替える：ロールバックのあとでも元に戻せる。
	tmp := p.Current + ".swap"
	_ = os.Remove(tmp)
	if err := os.Rename(p.Current, tmp); err != nil {
		return fmt.Errorf("현재 버전 이동 실패: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		_ = os.Rename(tmp, p.Current)
		return fmt.Errorf("이전 버전 설치 실패: %w", err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		log.Printf("[update] ロールバック後のバックアップの整理に失敗（動作には影響しない）：%v", err)
	}
	_ = os.Remove(p.Marker)
	return nil
}

// HasBackup はロールバックできる前の版があるかを報告し、フロントエンドがロールバックボタンを出すかどうかを決めるのに使う。
func HasBackup() bool {
	p, err := ResolvePaths()
	if err != nil {
		return false
	}
	_, err = os.Stat(p.Old)
	return err == nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "알 수 없는 버전"
	}
	return s
}
