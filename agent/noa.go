package agent

import (
	"log"
	"path/filepath"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/noaadapter"
)

// noaWarn returns a diagnostics sink tagging non-fatal noa messages with the
// session, routed through the package logger (agents have no per-instance one).
func noaWarn(session string) func(string) {
	return func(msg string) { log.Printf("[noa] %s: %s", session, msg) }
}

// noa は norma v0.4.0 で入った「モデル駆動のコンテキストの圧縮」の仕組みで、プラットフォームの実験的な機能としてユーザーが
// システム設定でオン/オフする。組み込みの compaction とは同時に使えない:noaadapter.Enable が唯一の入口で、一度に
// コンテキストの引き受け手(Compactor)、Compress ツール、3段の常駐のプロンプトを付ける。Enable を呼ばなければオフ
// (組み込みの compaction が通常どおり動く)。切り替えは各 agent に注入した noaEnabledFn で解決し、run ごとに
// 1回読むので、切り替えはその後に始まる run にだけ効き、agent を作り直す必要はない。

// enableNoa は解決の結果がオンのとき noa を opts につなぐ。archiveRoot は圧縮の原文を保存する基本のディレクトリ
// (全体の workDir を使い、どの agent も <workDir>/noa の下にまとまり、タスク/意図のディレクトリに散らばらない)。sessionID は
// その下の保管用のサブディレクトリの名前(全体で一意なので、同じ基本のディレクトリ内でぶつからない)。
//
// noa は実験的な機能:つなぐのに失敗しても本物のタスクを止めてはならない。エラーが起きたら onWarn で報告して組み込みの圧縮に戻る。
// 有効にできたら opts.Compaction を消し、agentcore が「2つのコンテキストの管理者が同時に設定された」と警告するのを避ける。
func enableNoa(opts *agentcore.Options, enabled func() bool, archiveRoot, sessionID string, onWarn func(string)) {
	if enabled == nil || !enabled() {
		return
	}
	if opts.OnWarn == nil {
		opts.OnWarn = onWarn
	}
	if err := noaadapter.Enable(opts, noaadapter.Options{
		ArchiveBaseDir: filepath.Join(archiveRoot, "noa"),
		SessionID:      sessionID,
		OnWarn:         onWarn,
	}); err != nil {
		if onWarn != nil {
			onWarn("noa の圧縮を有効にできなかったので、組み込みの圧縮に戻る:" + err.Error())
		}
		return
	}
	// Compactor は Compaction を上書きするが、2つが並ぶと agentcore が毎回警告する;明示的に消す。
	opts.Compaction = nil
}
