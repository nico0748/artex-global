// Package notify は脆弱性の発見を IM / メールで通知するチャネルのアダプタ層を実装する。
//
// 階層：このパッケージは**末端のパッケージ**で、標準ライブラリだけに依存する。データベースも server も知らない。チャネル設定は
// map[string]any で受け取り（notification_channels.config という JSONB 列に対応する）、
// 通知する内容は Message で受け取る。こう分けることで、署名の計算、UTF-8 の切り詰め、フィルタの照合といった
// 本当に間違えやすい部分を PostgreSQL なしで単体テストでき、ホスト側は server で組み立てるだけで済む。
//
// 並行性の約束：Channel の実装は必ず**状態を持たない**こと。同じ Channel のインスタンスが複数のチャネル設定
// （同じチャネルの複数のボットのインスタンスでさえ）から並行して使い回されるので、認証情報はすべて cfg 引数で受け取り、
// webhook URL などを実装自身のフィールドにキャッシュしてはならない。
package notify

// チャネルの種類の識別子。この値は同時に notification_channels.kind の正しい値の集合でもあり、server 側の
// 許可リストで検証する（findings.status と同じく、DB の CHECK は使わず、あとでチャネルを足しやすくしている）。
const (
	KindDingTalk = "dingtalk" // DingTalk のカスタムボット
	KindFeishu   = "feishu"   // Feishu（Lark を含む）のカスタムボット
	KindWeCom    = "wecom"    // WeCom のグループボット
	KindWebhook  = "webhook"  // 汎用 Webhook：メソッド/ヘッダー/JSON テンプレートを自由に設定
	KindTelegram = "telegram" // Telegram Bot API
	KindEmail    = "email"    // SMTP メール
)

// イベントの種類。notification_events.kind に対応する。
const (
	EventFindingCreated       = "finding_created"
	EventFindingStatusChanged = "finding_status_changed"
)

// InitKind は config で kind が空のときの既定値。
const InitKind = KindDingTalk

// severityRank は脆弱性の深刻度を比較できる序数に対応させる。不明な深刻度は 0 を返すので、どの
// min_severity の設定でも不明な深刻度は止められる——疑わしいときは通知せず、誤報で埋め尽くすのを避ける。
var severityRank = map[string]int{
	"low":      1,
	"medium":   2,
	"high":     3,
	"critical": 4,
}

// SeverityRank は深刻度の序数を返す。不明な深刻度は 0 を返す。
func SeverityRank(severity string) int { return severityRank[severity] }

// Lang, when wired by the server, returns the active user-facing output language
// ("en"/"ko"/"zh"/"es"/"ja") for the strings this leaf package renders into push /
// email messages (severity and status labels). Kept as an injected func so the
// package stays standard-library-only: it does not import config/db/server. nil
// (unit tests / standalone) → labelLang falls back to "ko", preserving the Korean
// localization golden tests.
var Lang func() string

func labelLang() string {
	if Lang != nil {
		switch v := Lang(); v {
		case "en", "ko", "zh", "es", "ja":
			return v
		}
	}
	return "ko"
}

// severityLabels / statusLabels hold the per-language display strings. "ko" keeps
// the exact Korean values the localization golden tests pin.
var severityLabels = map[string]map[string]string{
	"ko": {"critical": "🔴 심각", "high": "🟠 높음", "medium": "🟡 중간", "low": "🔵 낮음"},
	"en": {"critical": "🔴 Critical", "high": "🟠 High", "medium": "🟡 Medium", "low": "🔵 Low"},
	"zh": {"critical": "🔴 严重", "high": "🟠 高危", "medium": "🟡 中危", "low": "🔵 低危"},
	"es": {"critical": "🔴 Crítico", "high": "🟠 Alto", "medium": "🟡 Medio", "low": "🔵 Bajo"},
	"ja": {"critical": "🔴 重大", "high": "🟠 高", "medium": "🟡 中", "low": "🔵 低"},
}

var statusLabels = map[string]map[string]string{
	"ko": {"pending": "처리 대기", "in_progress": "처리 중", "confirmed": "확인됨", "resolved": "처리됨", "fixed": "수정됨", "false_positive": "오탐", "ignored": "무시", "duplicate": "중복", "risk_accepted": "위험 수용"},
	"en": {"pending": "Pending", "in_progress": "In progress", "confirmed": "Confirmed", "resolved": "Resolved", "fixed": "Fixed", "false_positive": "False positive", "ignored": "Ignored", "duplicate": "Duplicate", "risk_accepted": "Risk accepted"},
	"zh": {"pending": "待处理", "in_progress": "处理中", "confirmed": "已确认", "resolved": "已处理", "fixed": "已修复", "false_positive": "误报", "ignored": "忽略", "duplicate": "重复", "risk_accepted": "接受风险"},
	"es": {"pending": "Pendiente", "in_progress": "En curso", "confirmed": "Confirmado", "resolved": "Resuelto", "fixed": "Corregido", "false_positive": "Falso positivo", "ignored": "Ignorado", "duplicate": "Duplicado", "risk_accepted": "Riesgo aceptado"},
	"ja": {"pending": "対応待ち", "in_progress": "対応中", "confirmed": "確認済み", "resolved": "対応済み", "fixed": "修正済み", "false_positive": "誤検知", "ignored": "無視", "duplicate": "重複", "risk_accepted": "リスク受容"},
}

// SeverityLabel は emoji 付きの深刻度名（現在の出力言語による）を返し、メッセージのタイトルとカードの配色に使う。
// 不明な深刻度はそのまま返し、作り上げない。
func SeverityLabel(severity string) string {
	if v, ok := severityLabels[labelLang()][severity]; ok {
		return v
	}
	return severity
}

// StatusLabel は対応状況を現在の出力言語に訳し、状態変更のメッセージに使う。不明な状態はそのまま返す。
func StatusLabel(status string) string {
	if v, ok := statusLabels[labelLang()][status]; ok {
		return v
	}
	return status
}

// AtLeast は severity が min のしきい値に達しているかを判定する。min が空ならしきい値を設けず、一律に通す。
// 不明な severity の序数は 0 で、空でない min ならどれでも拒否されることに注意（severityRank のコメントを参照）。
func AtLeast(severity, min string) bool {
	if min == "" {
		return true
	}
	return SeverityRank(severity) >= SeverityRank(min)
}
