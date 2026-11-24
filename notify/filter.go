package notify

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Filter は notification_channels.filter という JSONB 列の契約：チャネルのインスタンスの絞り込み条件。
// すべてのフィールドは任意で、省略すれば「絞り込まない」——これがまさに不正な設定に対する最後の受け皿の意味になる。ParseFilter を参照。
type Filter struct {
	// MinSeverity は最低の深刻度のしきい値（low/medium/high/critical）。空ならしきい値を設けない。
	MinSeverity string `json:"min_severity"`
	// TaskIDs / AssetIDs が空の配列なら制限しない。空でなければイベントがそれと交わることを求める。
	TaskIDs  []int64 `json:"task_ids"`
	AssetIDs []int64 `json:"asset_ids"`
	// VulnClassInclude が空ならすべて受け取る。空でなければ vulnclass がそのいずれかのキーワードに一致することを求める。
	// VulnClassExclude はいずれかのキーワードに一致したら除外する（除外は包含より優先する）。
	// 一致の方式は大文字小文字を区別しない部分文字列——正規表現より安全：ユーザーが正規表現を書き間違えてもチャネルが黙って無効になることはない。
	VulnClassInclude []string `json:"vulnclass_include"`
	VulnClassExclude []string `json:"vulnclass_exclude"`
	// OnStatusChange はそのチャネルが脆弱性の状態変更イベントを受け取るかを決める（realtime モードでのみ意味を持つ）。
	OnStatusChange bool `json:"on_status_change"`
}

// ParseFilter はチャネルの絞り込み設定を解析する。
//
// **決して error を返さない。** これは意図した設計上の選択である：絞り込み条件の設定が不正なときは一律にゼロ値の
// Filter（= 絞り込まない = すべて一致）に退化させる。脆弱性の通知システムにとっては、**1件多く通知するほうが、
// 高リスクの1件を黙って取りこぼすよりはるかによい**からだ。解析の失敗を「通知しない」にすると、ユーザーには設定済みに見えて
// 実際には何も通知しないチャネルを渡すことになる——これは最悪の失敗の形だ。
func ParseFilter(raw []byte) Filter {
	var f Filter
	if len(raw) == 0 {
		return f
	}
	// 解析に失敗したら f はゼロ値のまま、つまり絞り込まない。
	_ = json.Unmarshal(raw, &f)
	return f
}

// ValidMinSeverity は s が正しい深刻度のしきい値かを報告する（空文字列はしきい値を設けないことを表す）。
func ValidMinSeverity(s string) bool {
	if s == "" {
		return true
	}
	_, ok := severityRank[s]
	return ok
}

// Validate は絞り込み設定のうち**値が限られている**フィールドを検証し、チャネルの保存時に呼ぶ。
//
// 書き込み時に止める必要がある理由：Match の未知のしきい値に対する判定は `rank >= 0` で、常に真になる——
// つまり min_severity を1文字打ち間違える（"hgih"）と、フィルタは**黙って無効になり**
// 「すべて通知」になる。これはこのパッケージの「取りこぼすよりは多く通知する」という取捨の方向には沿う（取りこぼさない）が、
// 結果としてユーザーは深刻度ごとに通知しているつもりで、実際にはすべての脆弱性をグループに流し込み、
// しかも設定を誤っていることを示す兆候が何もない。こうした「黙った劣化」こそ入口で止めるべきだ。
//
// Validate は**書き込み**の経路でだけ使うことに注意。読み取りの経路は引き続き ParseFilter の寛容な意味に従うので、
// 過去のデータにすでにある不正な値のせいでチャネル全体が読めなくなることはない。
func (f Filter) Validate() error {
	if !ValidMinSeverity(f.MinSeverity) {
		return fmt.Errorf("올바르지 않은 최소 심각도입니다: %q. low / medium / high / critical 중에서 선택하거나, 비워 두면 제한하지 않습니다", f.MinSeverity)
	}
	return nil
}

// Match はイベントをこの絞り込み条件を持つチャネルに配信すべきかを判定する。
//
// **決して error を返さない**。理由は ParseFilter と同じ：内部の異常はすべて「一致」として扱う。
// 判定の順序：イベントの種類 → 深刻度のしきい値 → タスク/資産の範囲 → 脆弱性の種類のキーワード。
func Match(f Filter, s Snapshot) bool {
	// 状態変更イベントは明示的に有効にしたチャネルだけが受け取る。既定はオフ。大多数の利用者は
	// 「通知」を「新しい脆弱性の発見」の意味で期待しており、状態の移り変わりを逐一追う記録ではないからだ。
	if s.Kind == EventFindingStatusChanged && !f.OnStatusChange {
		return false
	}
	if !AtLeast(s.Severity, f.MinSeverity) {
		return false
	}
	if len(f.TaskIDs) > 0 && !slices.Contains(f.TaskIDs, s.TaskID) {
		return false
	}
	if len(f.AssetIDs) > 0 && !intersectsInt(f.AssetIDs, s.AssetIDs) {
		return false
	}
	// 除外を優先：除外キーワードのいずれかに一致したら、包含リストにも一致していても外す。
	if len(f.VulnClassExclude) > 0 && containsAnyFold(s.VulnClass, f.VulnClassExclude) {
		return false
	}
	if len(f.VulnClassInclude) > 0 && !containsAnyFold(s.VulnClass, f.VulnClassInclude) {
		return false
	}
	return true
}

func intersectsInt(a, b []int64) bool {
	// 小さな集合なので線形の走査で十分。どちらの量も「人が手で選んだ数十件」程度で、
	// map を作るコストのほうが効果より大きい。
	for _, v := range b {
		if slices.Contains(a, v) {
			return true
		}
	}
	return false
}

// containsAnyFold は s が keywords のいずれかのキーワードを含むかを報告する（大文字小文字を区別しない）。
func containsAnyFold(s string, keywords []string) bool {
	lower := strings.ToLower(s)
	for _, kw := range keywords {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if kw != "" && strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}
