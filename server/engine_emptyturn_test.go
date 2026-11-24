package server

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

// 空回りのターン(思考だけで、本文もツールもない)の識別と続行。steerHooks.Stop を参照。

func assistantThinking(text string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockThinking, Thinking: text, Signature: "sig"},
	}}
}

func TestIsThinkingOnlyTurn(t *testing.T) {
	toolUse := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockThinking, Thinking: "まずポートをスキャンする"},
		{Type: llm.BlockToolUse, ID: "t1", Name: "run_nuclei"},
	}}
	cases := []struct {
		name string
		msgs []llm.Message
		want bool
	}{
		{"思考だけ", []llm.Message{llm.UserText("開始"), assistantThinking("考える")}, true},
		{"思考+ツール", []llm.Message{llm.UserText("開始"), toolUse}, false},
		{"思考+本文", []llm.Message{assistantThinking("考える"), {
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "x"}, llm.TextBlock("結論")},
		}}, false},
		{"本文が空白文字だけ", []llm.Message{{
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "x"}, llm.TextBlock("  \n ")},
		}}, true},
		{"完全に空の assistant のターン", []llm.Message{{Role: llm.RoleAssistant}}, true},
		// ツールの結果は user の役割なので、判定はその前の assistant までさかのぼらなければならず、近くのもので誤判定してはならない。
		{"最後がツールの結果", []llm.Message{toolUse, {
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{{Type: llm.BlockToolResult, ToolUseID: "t1"}},
		}}, false},
		{"assistant のメッセージがない", []llm.Message{llm.UserText("開始")}, false},
		{"空の履歴", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isThinkingOnlyTurn(c.msgs); got != c.want {
				t.Fatalf("isThinkingOnlyTurn = %v, want %v", got, c.want)
			}
		})
	}
}

// fakeHooks はプログラムできる inner の HookRunner で、steerHooks が inner の決定を尊重するかを確かめるのに使う。
type fakeHooks struct {
	prevent  bool
	blocking []string
	msg      string
}

func (f fakeHooks) PreToolUse(context.Context, string, []byte) (bool, string, []byte) {
	return false, "", nil
}
func (f fakeHooks) PostToolUse(context.Context, string, []byte, []byte, bool) {}
func (f fakeHooks) Stop(context.Context, []llm.Message) (bool, []string, string) {
	return f.prevent, f.blocking, f.msg
}

func TestSteerHooksStopNudgesEmptyTurn(t *testing.T) {
	empty := []llm.Message{assistantThinking("まずサブドメインを列挙すべきだ")}

	t.Run("空回りのターンに続行の指示を注入する", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges, label: "worker-1 · #1"}
		prevent, blocking, _ := h.Stop(context.Background(), empty)
		if prevent {
			t.Fatal("空回りのターンで強制的に止めてはならない")
		}
		if len(blocking) != 1 || blocking[0] != emptyTurnNudge {
			t.Fatalf("blocking = %v, want [emptyTurnNudge]", blocking)
		}
	})

	t.Run("本文かツールがあれば介入しない", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		normal := []llm.Message{{
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{llm.TextBlock("スキャンを終えた。開いているポートは見つからなかった")},
		}}
		if _, blocking, _ := h.Stop(context.Background(), normal); blocking != nil {
			t.Fatalf("正常な終わりが空回りと誤判定された: %v", blocking)
		}
		if n := h.nudges.Load(); n != 0 {
			t.Fatalf("介入しないときは数えてはならない, got %d", n)
		}
	})

	t.Run("上限に達したら終わらせる", func(t *testing.T) {
		const limit = 5 // ユーザーが「空のレスポンスの再試行の回数」を 5 に設定した
		h := steerHooks{nudges: &atomic.Int64{}, limit: limit}
		for i := 1; i <= limit; i++ {
			if _, blocking, _ := h.Stop(context.Background(), empty); len(blocking) != 1 {
				t.Fatalf("%d 回目はまだ枠の中のはず, blocking = %v", i, blocking)
			}
		}
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("上限を超えてもまだ注入している: %v", blocking)
		}
	})

	// 「空のレスポンスの再試行の回数」を -1 に設定 = この層をオフにし、emptyTurnNudgeLimit は 0 に解決する。
	t.Run("設定でオフなら介入しない", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: 0}
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("オフなのにまだ注入している: %v", blocking)
		}
	})

	t.Run("inner が強制の停止を決めたら重ねない", func(t *testing.T) {
		h := steerHooks{inner: fakeHooks{prevent: true, msg: "guard が終わりを拒んだ"}, nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		prevent, blocking, msg := h.Stop(context.Background(), empty)
		if !prevent || msg != "guard が終わりを拒んだ" || blocking != nil {
			t.Fatalf("inner の強制の停止が書き換えられた: prevent=%v blocking=%v msg=%q", prevent, blocking, msg)
		}
		if n := h.nudges.Load(); n != 0 {
			t.Fatalf("inner に譲るときは枠を使ってはならない, got %d", n)
		}
	})

	t.Run("inner がすでに続行しようとしていたら重ねない", func(t *testing.T) {
		h := steerHooks{inner: fakeHooks{blocking: []string{"guard の続行の理由"}}, nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		_, blocking, _ := h.Stop(context.Background(), empty)
		if len(blocking) != 1 || blocking[0] != "guard の続行の理由" {
			t.Fatalf("inner の続行のメッセージが書き換えられた: %v", blocking)
		}
	})

	t.Run("カウンターを付けていなければ挙動は変わらない", func(t *testing.T) {
		h := steerHooks{limit: defaultEmptyTurnNudges} // 例えば将来ほかの呼び出し箇所が nudges を渡し忘れた場合
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("カウンターがないときは注入してはならない: %v", blocking)
		}
	})
}
