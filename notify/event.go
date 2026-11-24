package notify

// Snapshot は notification_events.snapshot という JSONB 列の契約。書き込み側は db 層の
// 脆弱性を DB に入れるトランザクション、読み取り側は server 層の配信エンジンとフィルタの照合。定義をこのパッケージに置くのは、これが
// 「通知の領域」のペイロードだから：db はシリアライズだけを受け持ち、フィールドの意味は理解しない。
//
// 描画時に引き直さず脆弱性のフィールドを重ねて保存する理由：脆弱性はあとから名前、深刻度、状態が変えられるが、
// 通知の内容は**発生した時点**の結論を反映すべきだからだ——引き直すと「あとで low に変えられた」という
// 危険な誤解を招く。それに、こうすれば fan-out と描画で findings/tasks/assets の3つのテーブルを JOIN せずに済む。
type Snapshot struct {
	// イベントの種類：finding_created / finding_status_changed
	Kind      string  `json:"kind"`
	FindingID int64   `json:"finding_id"`
	TaskID    int64   `json:"task_id"`
	VulnClass string  `json:"vulnclass"`
	Name      string  `json:"name"`
	Severity  string  `json:"severity"`
	Summary   string  `json:"summary"`
	AssetIDs  []int64 `json:"asset_ids"`
	// kind=finding_status_changed のときだけ空でない。
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status,omitempty"`
}

// Item は通知を待つ脆弱性1件で、チャネルの描画に使う。
type Item struct {
	FindingID int64
	Name      string
	VulnClass string
	Severity  string
	Summary   string
	// Assets は解析済みの資産の表示名（ドメイン/IP など）。server 層が埋める——
	// このパッケージはデータベースに触れないので、名前を取得できない。
	Assets []string
	// DetailURL は脆弱性の詳細への戻りリンク。空なら public_base_url が設定されていないことを表し、描画時に省く。
	DetailURL string
	// 状態変更イベント専用。2つとも空でなければ「未対応 → 修正済み」のように描画する。
	FromStatus string
	ToStatus   string
}

// IsStatusChange はこの項目が状態変更イベントかを報告する。
func (i Item) IsStatusChange() bool { return i.FromStatus != "" || i.ToStatus != "" }

// Title は項目の表示用タイトルを返す：人が付けた name を優先し、なければ脆弱性の種類 vulnclass に戻り、
// どちらも空ならプレースホルダーを使う——空のタイトルは決して出力しない。
func (i Item) Title() string {
	if i.Name != "" {
		return i.Name
	}
	if i.VulnClass != "" {
		return i.VulnClass
	}
	return "(이름 없는 취약점)"
}

// Message は1回のチャネル送信の完全な内容。
type Message struct {
	// 単発の通知では長さが 1、まとめの通知（digest）では一括分すべてになる。
	// 空のスライスは不正で、呼び出し側は少なくとも1件あることを保証すること。
	Items []Item
	// Batch=true のときはまとめのメッセージとして描画する（タイトルを変え、時間枠と件数を付ける）。
	Batch bool
	// WindowMinutes はまとめの周期（分）で、Batch=true のときだけ文言「直近 N 分」に使う。
	// 描画時に time.Since で計算せず、わざと設定から明示的に渡している：描画を決定的に保てば、テストしやすい。
	WindowMinutes int
	// HomeURL はプラットフォームのパネルのアドレス（全体の public_base_url）。空ならパネルへの入口を付けない。
	HomeURL string
}
