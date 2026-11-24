package agent

import (
	"context"
	"errors"
	"fmt"
)

// AbortCause names why an agent run's context was cancelled. Every cancellation
// site should attach one so the activity trace can report the real initiator.
type AbortCause struct {
	Code  string
	Short string
	Text  string
}

func (c *AbortCause) Error() string { return c.Text }

func cause(code, short, text string) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: text}
}

// Causef builds a cause that includes runtime-specific detail.
func Causef(code, short, format string, args ...any) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: fmt.Sprintf(format, args...)}
}

var (
	// Task-level execution context.
	AbortPausedByUser = cause("paused_by_user", "ユーザーがタスクを一時停止した",
		"ユーザーがタスクの制御インターフェース（POST /api/tasks/{id}/control、action=pause）でタスクを一時停止した。今回の Planner/Worker の実行は意図的に取り消された；実行中の意図は frontier(open) に戻り、タスクを再開した後に取り直して最初から実行する")
	AbortPausedByOrchestrator = cause("paused_by_orchestrator", "オーケストレーションの Agent がタスクを一時停止した",
		"オーケストレーションの Agent が pause_task ツールを呼んでこのタスクを一時停止した。今回の Planner/Worker の実行は意図的に取り消された；実行中の意図は frontier(open) に戻り、再開後に実行し直す")
	AbortTaskDeleted = cause("task_deleted", "タスクが削除された",
		"タスクを削除している（DELETE /api/tasks/{id}）。削除の障壁がそのタスクで実行中の Planner、Worker、メイン Agent を取り消した；今回の実行の結果はもう使われない")
	AbortPausedOnReload = cause("paused_on_reload", "バックエンドがタスクの一時停止の状態を復元した",
		"バックエンドの起動時に、データベースに保存された状態からタスクの一時停止を復元した。今回の実行は取り消された；通常、復元の段階で実行中の Agent はない")
	AbortGoalMet = cause("goal_met", "計画者がタスクの目標を達成したと判定した",
		"計画者がタスクの目標を達成したと判定してタスクを done にし、その後まだ実行中の Worker を取り消した；これらの意図は失敗ではなく stopped になる")
	AbortSettleDrainTimeout = cause("settle_drain_timeout", "タスクのタイムアウトの締めくくりの待ち時間を使い切った",
		"タスクが timeout に達した後、実行中の Worker がきれいに締めくくるのを待ったが、90 秒の drain の猶予でも足りなかったので強制的に取り消した；意図は exhausted になり、締めくくりの段階ですでに書き込んだ事実と資産は残る")

	// Per-work context.
	AbortKilledByPlanner = cause("killed_by_planner", "計画者がこの意図を終わらせた",
		"計画者が kill_work を呼んでこの意図を意図的に終わらせた。通常は方向がそれたか、続ける価値がなくなったことを表す；意図は stopped になり、自動では取り直されない")
	AbortWorkPausedByUser = cause("work_paused_by_user", "ユーザーがこの Worker の意図を一時停止した",
		"ユーザーが実行中の Worker を一時停止した。今回の呼び出しは取り消され、意図は paused になる；登録済みの意図、事実、脆弱性、活動の記録はすべて残り、再開後に最初から実行し直す")
	AbortWorkCancelledByUser = cause("work_cancelled_by_user", "ユーザーがこの Worker の意図を削除した",
		"ユーザーが実行中の Worker を削除した。今回の呼び出しは取り消された；Worker が書き込みの領域を出た後、サーバーはユーザーが選んだ削除のモードに従ってその意図を処理する——論理削除は削除済みの印を付けるだけですべての産出を残し、物理削除はその意図と、それだけに支えられた下流のノードを連鎖して取り除く")
	AbortWorkFinished = cause("work_finished", "Worker は正常に終わり context を解放した",
		"Worker は正常に終わり、エンジンが detachWork でその context の資源を解放した。これは実行の中断ではない；中断のメッセージに現れたなら、取り消しと終了のイベントが競合したことを表す")
	AbortPausedRaceGuard = cause("paused_race_guard", "タスクの一時停止中は新しい実行の開始を拒む",
		"タスクが一時停止の状態にあるとき、エンジンは新しい実行の context を出すのを拒む。claim と一時停止の競合で Worker が起動し続けるのを防ぐため；取得済みの意図は frontier に戻る")

	// Main Agent and standalone conversation contexts.
	AbortChatStoppedByUser = cause("chat_stopped_by_user", "ユーザーがこのラウンドの会話を止めた",
		"ユーザーが停止をクリックし、このラウンドのメイン Agent またはセッションの Agent の実行を意図的に中止した。すでに生じた活動の記録は残り、次のメッセージを続けて送れる")
	AbortChatPausedWithTask = cause("chat_paused_with_task", "タスクの一時停止でメイン Agent の会話も中止した",
		"ユーザーがタスクを一時停止したとき、実行中のメイン Agent の会話も同時に取り消された。すでに生じた活動の記録は残る；タスクを再開してもこのラウンドのメッセージは自動ではやり直されない")
	AbortChatTurnFinished = cause("chat_turn_finished", "このラウンドの会話は正常に終わり context を解放した",
		"このラウンドの会話は正常に終わり、サーバーがそのラウンドの context の資源を解放している。これは実行の中断ではない；中断のメッセージに現れたなら、取り消しと終了のイベントが競合したことを表す")

	// Process-level and per-run hard backstop.
	AbortShutdown = cause("shutdown", "バックエンドのプロセスが終了している",
		"バックエンドのプロセスが SIGINT か SIGTERM を受け取り、再起動、更新、または終了している。実行中のすべての Agent は取り消される；再起動後に残った running の意図は open に戻して実行し直す")
	AbortRunHardTimeout = cause("run_hard_timeout", "1回の実行の強制タイムアウトの最後の砦が働いた",
		"1回の実行がソフトな実時間の予算と追加の猶予を超えた。モデルのリクエストかいずれかのツールが長く戻らず、通常のターンの境界での締めくくりができなかったことを表す。中断前に最後に戻らなかったツールの呼び出しを重点的に調べること")
)

// AbortReason resolves the named cause attached to a cancelled run context.
func AbortReason(ctx context.Context) (code, short, text string, ok bool) {
	c := context.Cause(ctx)
	if c == nil {
		return "", "", "", false
	}
	var ac *AbortCause
	if errors.As(c, &ac) {
		return ac.Code, ac.Short, ac.Text, true
	}
	switch {
	case errors.Is(c, context.DeadlineExceeded):
		return "deadline_exceeded", "上流の context が deadline に達した",
			"上流の context が deadline に達したが、設定した側が WithTimeoutCause で名前付きの原因を付けていない: " + c.Error(), true
	case errors.Is(c, context.Canceled):
		return "canceled_no_cause", "取り消した側が名前付きの原因を付けていない",
			"上流の context が取り消されたが、取り消した側が context.WithCancelCause で名前付きの原因を付けていない；agent/cancelcause.go に原因を登録し、その取り消しの箇所につなぐこと", true
	default:
		return "other", firstLine(c.Error(), 80), c.Error(), true
	}
}
