package agent

import (
	"context"

	"github.com/Autumn-27/artex/db"
)

// FindingRecorder is injected by the host; agents never synthesize or copy
// evidence bodies themselves. Its implementation owns the atomic write.
type FindingRecorder interface {
	Record(context.Context, db.RecordFindingInput, []db.TrafficRef) (*db.RecordedFinding, error)
}

// Tool-use guidance is appended without replacing the user's editable prompt.
// It does not require capture or claim that unavailable traffic tools exist.
const findingTrafficGuidance = "\n\n**脆弱性の通信の証拠（任意）**：report_finding で脆弱性を報告するとき、見て脆弱性の結論を裏づけると確かめた HTTP のリクエスト/レスポンスがあれば、traffic_refs で再現の順に本物の ID を結びつけられる；ドメインと時刻は候補の絞り込みにだけ使い、関連を推定しない。TCP など HTTP 以外の脆弱性、取得していない、または正確に一致するものがないときは省くか [] を渡し、evidence にコマンドの出力やログなど他の確かめられる証拠を残し、結びつけなかった理由を説明するとよい。ID を推測しないこと、パケットを補うためだけに探査を繰り返さないこと。"

func (t *ToolSet) SetFindingRecorder(r FindingRecorder)   { t.findingRecorder = r }
func (w *Worker) SetFindingRecorder(r FindingRecorder)    { w.findingRecorder = r }
func (p *Planner) SetFindingRecorder(r FindingRecorder)   { p.findingRecorder = r }
func (m *MainAgent) SetFindingRecorder(r FindingRecorder) { m.findingRecorder = r }
