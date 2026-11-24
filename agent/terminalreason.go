package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Autumn-27/norma/harness"
)

// runTrace retains the latest tool call so an interrupted run can identify the
// operation that was still in flight.
type runTrace struct {
	startedAt time.Time
	id        string
	name      string
	input     string
	at        time.Time
	pending   bool
}

func (t *runTrace) start(id, name, input string) {
	t.id, t.name, t.input, t.at, t.pending = id, name, input, time.Now(), true
}

func (t *runTrace) done(id string) {
	if id == t.id {
		t.pending = false
	}
}

var reasonHint = map[harness.TerminalReason]string{
	harness.ReasonCompleted:         "モデルはこのラウンドを正常に終えたが、文の総括を残さなかった；事実と資産はこのラウンドのツールの呼び出しの記録に従う",
	harness.ReasonMaxTurns:          "手順の上限(MaxTurns)に達した：SDK はすでに締めくくりをして事実と資産を書き戻した。意図は exhausted になり、計画者が方向を変えて続けるためのもので、失敗としては扱わない",
	harness.ReasonTimeout:           "1回の実行の実時間の予算(MaxDuration)に達した：時間になると実行中のツールを打ち切り、その場で締めくくりに入って、識別した事実と資産を書き戻す。意図は exhausted になる",
	harness.ReasonModelError:        "モデルまたは API の呼び出しに失敗した（ネットワーク、認証、レート制限、プロバイダーの 5xx など）。再試行を使い切った後に意図は blocked になる——転送層の障害で、この意図はほとんど本当には探索できていない；その実行の過程（get_worker_trace）を調べてから、出し直すか方法を変えるかを決める",
	harness.ReasonBlockingLimit:     "コンテキストの長さが強制の上限に達し、リクエストは送る前に遮断された；意図の粒度を狭めるか、ツールの戻り値を圧縮すること",
	harness.ReasonPromptTooLong:     "プロンプトが長すぎ、コンテキストの圧縮の再試行もすでに使い切ったので、実行を続けられない",
	harness.ReasonImageError:        "現在のモデルはこのラウンドのマルチモーダルの内容に対応していない；画像に対応したモデルに切り替えるか、ツールが画像を返さないようにすること",
	harness.ReasonStopHookPrevented: "Stop フックがこのラウンドの終了を止め、その後続けられなかった；タスクの Guard のルールが厳しすぎないか調べること",
	harness.ReasonHookStopped:       "ツールかフックが自分から実行を止めた。範囲外の対象や禁止のコマンドなど；最後の tool_result の遮断の説明を調べること",
	harness.ReasonAbortedStreaming:  "モデルの出力をストリーミングで生成する段階で実行が取り消された",
	harness.ReasonAbortedTools:      "ツールを実行する段階で実行が取り消された",
}

// terminalText renders a terminal event with no final text into a compact summary
// and a Markdown detail block.
func terminalText(ctx context.Context, term *harness.Terminal, tr *runTrace) (string, string) {
	reason := term.Reason
	aborted := reason == harness.ReasonAbortedStreaming || reason == harness.ReasonAbortedTools
	// Prompt may return ctx.Err directly without a terminal event. Preserve the
	// cancellation cause instead of falling back to an empty/unknown terminal reason.
	if reason == "" && ctx.Err() != nil {
		aborted = true
	}

	var sum string
	if aborted {
		_, short, _, ok := AbortReason(ctx)
		if !ok {
			short = "取り消しの原因を取得できなかった"
		}
		stage := "実行の途中"
		switch reason {
		case harness.ReasonAbortedStreaming:
			stage = "モデルの出力の段階"
		case harness.ReasonAbortedTools:
			stage = "ツールの実行の段階"
		}
		sum = "（実行が中断された：" + short + "；" + stage + "で止まった" + progressSuffix(term, tr) + "。未完了）"
	} else if reason == harness.ReasonMaxTurns || reason == harness.ReasonTimeout {
		sum = "（実行の予算の上限(" + string(reason) + ")に達した。締めくくって事実を書き戻した" + progressSuffix(term, tr) + "；今回は文の総括がない）"
	} else {
		hint := terminalReasonHint(reason)
		sum = "（文の総括がない。終端の状態 " + terminalReasonLabel(reason) + "：" + firstLine(hint, 80) + "）"
	}

	var b strings.Builder
	b.WriteString(sum)
	b.WriteString("\n\n")
	displayReason := terminalReasonLabel(reason)
	fmt.Fprintf(&b, "- **終端の状態**: `%s` - %s\n", displayReason, terminalReasonHint(reason))
	if aborted {
		code, _, why, ok := AbortReason(ctx)
		if ok {
			fmt.Fprintf(&b, "- **中断の原因** (`%s`): %s\n", code, why)
		} else {
			b.WriteString("- **中断の原因**: 取得できない；取り消した側が context.WithCancelCause で名前付きの原因を付けていない可能性がある\n")
		}
	}
	if term.Err != nil {
		fmt.Fprintf(&b, "- **下層のエラー**: `%v`\n", term.Err)
	}
	if aborted && strings.TrimSpace(term.Text) != "" {
		b.WriteString("- **取り消し前に生成された一部の出力**:\n\n")
		b.WriteString(term.Text)
		b.WriteString("\n\n")
	}
	if term.Turns > 0 {
		fmt.Fprintf(&b, "- **実行済み**: モデルのターン %d 回\n", term.Turns)
	}
	if !tr.startedAt.IsZero() {
		fmt.Fprintf(&b, "- **今回の実行の所要時間**: %s\n", roundDur(time.Since(tr.startedAt)))
	}
	if u := term.Usage; u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.CacheWriteTokens > 0 {
		fmt.Fprintf(&b, "- **累計の token**: 入力 %d / 出力 %d / キャッシュ読み取り %d / キャッシュ書き込み %d\n",
			u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens)
	}
	if tr.name == "" {
		b.WriteString("- **ツールの呼び出し**: 今回の実行はツールの呼び出しを出す前に終わった\n")
	} else if tr.pending {
		fmt.Fprintf(&b, "- **中断時に実行中だったツール**: `%s`（%s 実行していた。**結果は戻っていない**）\n\n  ```json\n  %s\n  ```\n",
			tr.name, roundDur(time.Since(tr.at)), firstLine(tr.input, 300))
	} else {
		fmt.Fprintf(&b, "- **中断前の最後のツール**: `%s`（正常に戻った）\n", tr.name)
	}
	return sum, b.String()
}

func terminalReasonLabel(reason harness.TerminalReason) string {
	if reason == "" {
		return "context_canceled"
	}
	return string(reason)
}

func terminalReasonHint(reason harness.TerminalReason) string {
	if hint := reasonHint[reason]; hint != "" {
		return hint
	}
	if reason == "" {
		return "実行の context は取り消されたが、下層で Terminal イベントが生じなかった"
	}
	return "不明な終端の状態；harness が TerminalReason を新しく足した可能性があるので、reasonHint を補うこと"
}

func progressSuffix(term *harness.Terminal, tr *runTrace) string {
	var parts []string
	if term.Turns > 0 {
		parts = append(parts, fmt.Sprintf("%d ターン", term.Turns))
	}
	if !tr.startedAt.IsZero() {
		parts = append(parts, roundDur(time.Since(tr.startedAt)))
	}
	if len(parts) == 0 {
		return ""
	}
	return "。実行済み：" + strings.Join(parts, " / ")
}

func roundDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	case d < time.Hour:
		return d.Round(time.Second).String()
	default:
		return d.Round(time.Minute).String()
	}
}
