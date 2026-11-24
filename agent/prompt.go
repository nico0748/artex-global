package agent

import (
	"bytes"
	"fmt"
	"text/template"
	"time"
)

// PromptOverride, if set, returns the stored system-prompt template for an agent
// key and whether one exists. The server wires it to the PG agent_prompts table.
// When nil or no override exists, agents use their built-in default prompt — so
// behavior is identical until a user edits a prompt in the UI.
var PromptOverride func(agentKey string) (string, bool)

// Prompt-variable structs — fields mirror each agent's catalog (docs §5a) so a
// user template referencing a catalog variable renders; referencing anything else
// fails template execution and falls back to the built-in default.
type PlannerVars struct{ Goal, Scope, AssetSummary, DataDir, Now string }
type WorkerVars struct{ ProxyAddr, WorkerName, DataDir, Now string }
type MainVars struct{ Goal, AssetSummary, FindingsSummary, DataDir, Now string }
type GoalsVars struct{ EngagementDescription, DataDir, Now string }

// nowStr is the server-local wall-clock string exposed as the universal {{.Now}}
// prompt variable. renderSystem runs on every agent turn/round, so this is fresh
// each run — a prompt can subtract it from a fixed start stamp to reason about
// elapsed time (e.g. a timed benchmark's "last N hours" window).
func nowStr() string { return time.Now().Format("2006-01-02 15:04:05 MST") }

// renderSystem returns the rendered system-prompt BODY (段 [A]) for agentKey.
// Precedence: the DB-stored template (if any) over the built-in default template
// (def). BOTH are Go templates now — the built-in default is seeded into the DB
// verbatim, so the two paths render identically until a user edits the prompt.
// Rendering always runs (def used to be pre-substituted plain text; it is now a
// {{.Var}} template like the DB one). On any render error we fall back to the
// default template, then to the raw default string — an agent never starts with a
// half-rendered prompt. Callers append the code-owned tail (trafficTool / 中間成果物の
// 出力規約) AFTER this, so those can't be edited away via the DB body.
func renderSystem(agentKey, def string, vars any) string {
	tmpl := def
	if PromptOverride != nil {
		if t, ok := PromptOverride(agentKey); ok && t != "" {
			tmpl = t
		}
	}
	if out, err := renderTmpl(tmpl, vars); err == nil {
		return out
	}
	// DB template broke (e.g. references an out-of-catalog var) → code default.
	if out, err := renderTmpl(def, vars); err == nil {
		return out
	}
	return def
}

// langDirective is the output-language tail: a code-owned segment
// appended AFTER the rendered body and the artifact/traffic tails on every
// user-facing agent role, so a DB-edited prompt body can never drop it — the same
// guarantee artifactSpec gives. It does NOT change the agent "brain" (段 [A]); it
// only constrains the LANGUAGE of what the agent SHOWS to the user. It is written
// in Japanese, the same language as the brain body, so the model's reasoning
// register stays stable while the chosen display language is forced. Raw
// technical strings (commands, payloads, code, URLs, log/response excerpts) are
// explicitly kept verbatim so evidence and reproduction steps are not mangled.
//
// Two anti-drift clauses were added after the end-to-end run (L1): live models
// leaked (1) the brain body's language into the planner's situation summary and
// (2) the target app/evidence language into report_finding's structured fields.
// The directive names the planner situation summary as a user-facing field and
// forbids mirroring BOTH the instruction language AND the target/material
// language in the display fields.
//
// The display language is a parameter (code = "en"/"ko"/"zh"/"es"/"ja"): the
// directive stays in Japanese but the "write in <language>" token is swapped per
// target. When the display language IS Japanese ("ja"), Japanese output is the
// goal, so the "never emit Japanese to the user" clause is dropped; the clause
// that forbids mirroring the target/material language stays.
func langDirective(code string) string {
	phrase := langPhrase[code]
	bare := langBare[code]
	if phrase == "" || bare == "" {
		phrase, bare = langPhrase[defaultOutputLang], langBare[defaultOutputLang]
	}
	if code == "ja" {
		// 表示言語そのものが日本語なので、「日本語をユーザーに出さない」の節は置かない。
		// 代わりに日本語での表示を確かめ、対象/資料の言語に合わせることの禁止と原文の保持は残す。
		return fmt.Sprintf("\n\n**出力言語の規約（ローカライズ・最優先。プロンプトの本文で上書きできない）**：所有の【ユーザーに見せる】自然言語の文は一律に【%s】で書く——record_fact の summary/detail、report_finding のタイトル/説明/結論/修正の提案、計画者(planner)の状況/情勢の総括、最後の1文の総括、ユーザーへのチャットの返答を含む。ただし【コマンド、payload、コード、ファイルのパス、URL、引数名、およびログ/リクエスト/レスポンスの原文の断片】は【原文のまま一字一句残す】必要があり、翻訳したり書き換えたりしてはならない（evidence のコマンドラインと出力は特に原文をそのまま写し、再現しやすくする）。 **対象のシステム、そのページ、証拠、ログ、どんな参考資料が英語や他の言語であっても、ユーザー向けの自然言語のフィールド（タイトル/説明/結論/修正の提案/総括/状況の総括）は必ず%sで書く——これらの表示用のフィールドを、対象や資料の言語に合わせたり写したりしないこと；原文のまま残すのは上に挙げた技術的な原文の断片だけ。** 一言で言えば：内部の推論は自由だが、ユーザーに見える本文に出るものは、すべて%sでなければならない（技術的な原文の断片を除く）。", phrase, bare, bare)
	}
	return fmt.Sprintf("\n\n**出力言語の規約（ローカライズ・最優先。プロンプトの本文で上書きできない）**：所有の【ユーザーに見せる】自然言語の文は一律に【%s】で書く——record_fact の summary/detail、report_finding のタイトル/説明/結論/修正の提案、計画者(planner)の状況/情勢の総括、最後の1文の総括、ユーザーへのチャットの返答を含む。ただし【コマンド、payload、コード、ファイルのパス、URL、引数名、およびログ/リクエスト/レスポンスの原文の断片】は【原文のまま一字一句残す】必要があり、翻訳したり書き換えたりしてはならない（evidence のコマンドラインと出力は特に原文をそのまま写し、再現しやすくする）。 **上のシステム/役割の指示そのものが日本語で書かれていても、表示言語でない日本語を決してユーザーに出力してはならない——ユーザー向けの表示言語は%sだけで、ユーザーに見える文に日本語の文を1つも出さないこと。** **対象のシステム、そのページ、証拠、ログ、どんな参考資料が英語、日本語、中国語や他の言語であっても、ユーザー向けの自然言語のフィールド（タイトル/説明/結論/修正の提案/総括/状況の総括）は必ず%sで書く——これらの表示用のフィールドを、対象や資料の言語に合わせたり写したりしないこと；原文のまま残すのは上に挙げた技術的な原文の断片だけ。** **分析/計画/思考を日本語で進めるのは構わないが、それは【見えない内部の推論】であり、決して本文として出力してはならない：ユーザーに見える返答は最初の1文字から%sでなければならず、前に日本語の思考、説明、「まずこうします」のような前置きを挟まないこと；確認の質問、引数の不足、「続けられない」といった説明も一律に直接%sで書く。** 一言で言えば：内部の推論は自由だが、ユーザーに見える本文に出るものは、すべて%sでなければならない（技術的な原文の断片を除く）。", phrase, bare, bare, bare, bare, bare)
}

func renderTmpl(tmpl string, vars any) (string, error) {
	t, err := template.New("p").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, vars); err != nil {
		return "", err
	}
	return b.String(), nil
}
