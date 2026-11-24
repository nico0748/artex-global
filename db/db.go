// Package db is the PostgreSQL data source for ARTEX (旧来の graph 単一ファイルの SQLite に代わる)。
// 接続を開き、schema を適用し、組み込みの agent と変数の目録を seed する。
package db

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/Autumn-27/artex/config"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // pgx database/sql driver ("pgx")
)

//go:embed schema.sql
var schemaSQL string

const schemaMigrationLockKey int64 = 7337741001

// maxOpenConns 는 연결 풀 상한이다(`Open` 참조). PostgreSQL 기본 max_connections(100)
// 보다 충분히 낮고, 애플리케이션 자신의 중첩 커넥션 깊이(기동기의 스키마 advisory lock 이
// 커넥션 하나를 쥔 채 seedBuiltins 가 다른 커넥션을 잡는다)보다는 충분히 높아 자기 잠금에
// 빠지지 않는다.
const maxOpenConns = 32

var schemaDeadlockRetryDelays = [...]time.Duration{
	100 * time.Millisecond,
	250 * time.Millisecond,
	500 * time.Millisecond,
	time.Second,
}

type schemaExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func isPostgresDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

func applySchemaWithRetry(ctx context.Context, execer schemaExecer, sleep func(time.Duration)) error {
	for attempt := 0; ; attempt++ {
		if _, err := execer.ExecContext(ctx, schemaSQL); err != nil {
			if !isPostgresDeadlock(err) || attempt >= len(schemaDeadlockRetryDelays) {
				return err
			}
			sleep(schemaDeadlockRetryDelays[attempt])
			continue
		}
		return nil
	}
}

// withSchemaMigrationLock pins the session-level lock to one checked-out
// connection. Running pg_advisory_lock through *sql.DB is incorrect because a
// later schema or unlock call may use a different pooled PostgreSQL session.
func withSchemaMigrationLock(ctx context.Context, sqlDB *sql.DB, action func(*sql.Conn) error) (err error) {
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, schemaMigrationLockKey); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	defer func() {
		if _, unlockErr := conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, schemaMigrationLockKey); unlockErr != nil && err == nil {
			err = fmt.Errorf("advisory unlock: %w", unlockErr)
		}
	}()
	return action(conn)
}

// coordinateWithSchemaMigration makes long, multi-table archive transactions
// mutually exclusive with startup DDL while allowing ordinary runtime queries
// to continue normally.
func coordinateWithSchemaMigration(tx *sql.Tx) error {
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock($1)`, schemaMigrationLockKey); err != nil {
		return fmt.Errorf("coordinate with schema migration: %w", err)
	}
	return nil
}

// DSN resolves the PostgreSQL connection string and reports where it came from.
// Precedence: env ARTEX_PG_DSN > config file (config.json). There is no
// built-in default — it errors if neither source is configured.
func DSN() (dsn, source string, err error) {
	return config.PostgresDSN()
}

// DB wraps the shared *sql.DB. PG handles its own connection pool + concurrency
// (MVCC), so unlike the old SQLite store there is no process-wide write mutex.
type DB struct{ *sql.DB }

// ensureDatabase connects to the postgres system database and creates the target
// database if it does not exist. dsn must be a postgres:// URL.
func ensureDatabase(dsn string) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil // unparseable DSN — let the normal Open fail with a clear error
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	if dbName == "" || dbName == "postgres" {
		return nil
	}
	// connect to the postgres maintenance database instead
	adminDSN := *u
	adminDSN.Path = "/postgres"
	admin, err := sql.Open("pgx", adminDSN.String())
	if err != nil {
		return nil // best-effort; let Open surface the real error
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		return nil
	}
	var exists bool
	_ = admin.QueryRow(`SELECT true FROM pg_database WHERE datname=$1`, dbName).Scan(&exists)
	if !exists {
		if _, err := admin.Exec(`CREATE DATABASE "` + dbName + `"`); err != nil {
			return fmt.Errorf("create database %q: %w", dbName, err)
		}
	}
	return nil
}

// Open connects, applies the schema (idempotent), and seeds builtin rows.
func Open(dsn string) (*DB, error) {
	if err := ensureDatabase(dsn); err != nil {
		return nil, err
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	// database/sql 은 기본적으로 커넥션 수를 제한하지 않아, 풀에 유휴 커넥션이 없으면 무조건
	// 새로 열어 PostgreSQL 의 max_connections(기본 100)까지 밀어붙이고 나서야 거절당한다.
	// 그러면 부하가 높을 때 쿼리는 "읽을 값이 없음"이 아니라 `FATAL: sorry, too many clients
	// already` 라는 오류를 받고, 이 오류는 아래 인증 경로의 "읽기 실패"와 같은 모양이 된다.
	// 상한을 두면 초과분은 유휴 커넥션을 기다려(오류 대신 느려짐), reset-password.sh·psql 을
	// 위한 여유도 남는다. max_connections 를 낮춰 운영하면 이 값도 함께 낮춘다.
	sqlDB.SetMaxOpenConns(maxOpenConns)
	sqlDB.SetMaxIdleConns(maxOpenConns)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)
	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("ping postgres (%s): %w", config.Redact(dsn), err)
	}
	d := &DB{sqlDB}
	// pgx runs multi-statement Exec via the simple protocol when there are no args.
	// Keep the dedicated lock connection checked out until both DDL and seeding
	// finish so concurrent application instances cannot initialize out of order.
	err = withSchemaMigrationLock(context.Background(), sqlDB, func(conn *sql.Conn) error {
		if err := applySchemaWithRetry(context.Background(), conn, time.Sleep); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
		if err := d.seedBuiltins(); err != nil {
			return fmt.Errorf("seed builtins: %w", err)
		}
		return nil
	})
	if err != nil {
		sqlDB.Close()
		return nil, err
	}
	return d, nil
}

// builtinAgent describes one of the fixed agents and its prompt-variable catalog.
type builtinAgent struct {
	key, name, role, desc string
	vars                  []promptVar
	interactiveShell      bool // 行を作るときの対話型 shell の既定の切り替え；ON CONFLICT はユーザーがのちに手で変えた切り替えを上書きしない
	runSeconds            *int // 行を作るときの1回の run の実時間の上限(秒)；nil=種の既定(1200)、0=無制限
}

type promptVar struct{ name, desc, example, source string }

// intp は v のポインタを返し、builtinAgent の任意のフィールド(runSeconds など)に明示的に値を与えるのに使う。
func intp(v int) *int { return &v }

// builtinAgents mirrors docs §5(a). 組み込みのツールは DB に入れない；ここでは agent と変数の目録だけを seed する。
// 注：planner/worker/mainagent/auto の対話型 shell の既定は下の interactive_shell_default_v1
// のブロックでまとめて true にする（のちの toggle を尊重する）；ここの interactiveShell は「行を作ったら既定でオン」にしたい新しい agent だけに与える。
var builtinAgents = []builtinAgent{
	{"goals", "目標の分解", "goals", "ペネトレーションテストの目標を、独立して検証できるいくつかの下位目標に分解する。", []promptVar{
		{"EngagementDescription", "タスクの説明（テスト対象/背景）", "example.com のサイトをテストする", "exploration"},
		// Now は全体の runtime 変数(server.globalPromptVars を参照)なので、各 agent の目録で
		// 重ねて定義しない。さもないと withGlobalVars が追加するときに全体の項目と名前がぶつかる。
	}, false, nil},
	{"planner", "計画", "planner", "状況を読み、目標を判定し、まだ覆っていない新しい方向が本当にあるときだけ探索の意図を足す（タスクごとに計画のループが1つ）。", []promptVar{
		{"Goal", "タスク全体の目標", "example.com の管理者権限を取る", "exploration"},
		{"AssetSummary", "資産の数/種類の分布の要約(任意)", "domain:3 ip:5 site:2", "distilled"},
	}, false, nil},
	{"mainagent", "メイン", "main", "人と機械の接点：進み具合を観察し、人の意図をヒントか優先度の高い意図に落とし込む。", []promptVar{
		{"Goal", "現在のタスクの目標", "example.com の管理者権限を取る", "exploration"},
		{"AssetSummary", "開始時の状況の要約(任意)", "domain:3 ip:5", "distilled"},
		{"FindingsSummary", "確認済みの脆弱性の要約(任意)", "high:1 medium:2", "distilled"},
	}, false, nil},
	{"worker", "実行", "worker", "意図を1つ受け取って実行し、見つけた事実/脆弱性をナレッジグラフに書き戻してから止まる。", []promptVar{
		{"ProxyAddr", "記録用のプロキシのアドレス(if の2通りの文面を切り替える)", "127.0.0.1:8080", "runtime"},
		{"WorkerName", "worker の自己の識別名(任意)", "worker-1", "runtime"},
	}, false, nil},
	// Auto:組み込みの「プラットフォームの操作」agent。ペネトレーションテストの編成のループには加わらず、会話のページから動かし、ツールでプラットフォームを操作する。
	{"auto", "Auto", "assistant", "プラットフォームの操作の助手：ツールでタスク(作成/閲覧/一時停止/ヒント)と資産を管理し、skill・カスタムツール・MCP を作成/変更できる。", nil, false, nil},
	// ペネトレーションテスト:組み込みの「単独のペネトレーションテスト」agent。会話のページから動かし、1人で偵察から締めくくりまでペネトレーションテストの流れ全体を通し、自分で計画し、自分で実行し、自分で検証する。対話型 shell が既定でオン。
	{"pentest", "ペネトレーションテスト", "assistant", "単独のペネトレーションテスト agent：1人で偵察→攻撃面の発見→深い悪用→検証→締めくくりの流れ全体を通し、自分で計画し、自分で実行し、自分で敵対的に検証する。", nil, true, intp(0)},
}

// seedBuiltins inserts the fixed built-in agents and their variable catalog (idempotent).
func (d *DB) seedBuiltins() error {
	for _, a := range builtinAgents {
		var agentID int64
		err := d.QueryRow(`
INSERT INTO agents(key, name, description, role, builtin, enabled, interactive_shell, run_seconds)
VALUES ($1, $2, NULLIF($3,''), $4, true, true, $5, COALESCE($6, 1200))
ON CONFLICT (key) DO UPDATE SET name = EXCLUDED.name, description = EXCLUDED.description
RETURNING id`, a.key, a.name, a.desc, a.role, a.interactiveShell, a.runSeconds).Scan(&agentID)
		if err != nil {
			return fmt.Errorf("agent %s: %w", a.key, err)
		}
		for _, v := range a.vars {
			if _, err := d.Exec(`
INSERT INTO agent_prompt_vars(agent_id, var_name, description, example, source)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (agent_id, var_name) DO UPDATE
  SET description = EXCLUDED.description, example = EXCLUDED.example, source = EXCLUDED.source`,
				agentID, v.name, v.desc, v.example, v.source); err != nil {
				return fmt.Errorf("agent %s var %s: %w", a.key, v.name, err)
			}
		}
	}
	// Drop catalog entries for variables that were renamed, so the white-list no
	// longer advertises a name templates can't resolve (EngagementTitle→Description).
	// 'Now' を各 agent の目録から全体の runtime 変数に上げた後も、古い DB の goals には 'Now' が1件残り、
	// 全体の項目と名前がぶつかる(フロントエンドの変数一覧の key が重複する)；あわせて消す。
	if _, err := d.Exec(`DELETE FROM agent_prompt_vars WHERE var_name IN ('EngagementTitle', 'CoverageGaps', 'Now')`); err != nil {
		return fmt.Errorf("cleanup renamed vars: %w", err)
	}
	// Default-on interactive_shell for the runtime agents (planner/worker/mainagent/auto)
	// ONCE — respects a later user toggle-off (guarded by a settings flag). goals(one-shot
	// decomposer) stays off. Runs after the column exists (schema applied before seed).
	if v, _, _ := d.GetSetting("interactive_shell_default_v1"); v != "true" {
		if _, err := d.Exec(`UPDATE agents SET interactive_shell=true WHERE key IN ('planner','worker','mainagent','auto')`); err != nil {
			return fmt.Errorf("seed interactive_shell defaults: %w", err)
		}
		_ = d.SetSetting("interactive_shell_default_v1", "true")
	}
	// Seed the built-in browser (Playwright) MCP once — DISABLED by default (ユーザーが
	// 必要なときに自分で有効にする), no proxy by default. The traffic-capture toggle injects/strips
	// the recording proxy + CA at runtime (server.Manager.syncBrowserMCPProxy).
	// Insert only if absent so we never clobber user edits (args/env/enabled/
	// visibility) on restart.
	if _, err := d.Exec(`
INSERT INTO mcp_servers(name, transport, command, args, env, enabled)
VALUES ('browser', 'stdio', 'npx', $1, '{}', false)
ON CONFLICT (name) DO NOTHING`,
		`["@playwright/mcp","--headless"]`); err != nil {
		return fmt.Errorf("seed browser mcp: %w", err)
	}
	// NOTE: the placeholder ScopeSentry data-source MCP (empty URL + empty X-API-Key,
	// disabled) is seeded directly in schema.sql §F so a raw `psql < schema.sql` init
	// also gets it. schema.sql is Exec'd on every startup, so it stays idempotent.
	if err := d.seedBuiltinSkillVisibility(); err != nil {
		return fmt.Errorf("seed skill visibility: %w", err)
	}
	if err := d.seedDefaultInterceptRules(); err != nil {
		return fmt.Errorf("seed intercept rules: %w", err)
	}
	if err := d.seedDefaultInterceptRulesV2(); err != nil {
		return fmt.Errorf("seed intercept rules v2: %w", err)
	}
	if err := d.seedDefaultInterceptRulesV3(); err != nil {
		return fmt.Errorf("seed intercept rules v3: %w", err)
	}
	if err := d.seedDefaultAssetInterceptRules(); err != nil {
		return fmt.Errorf("seed asset intercept rules: %w", err)
	}
	return nil
}

// interceptRuleSeed is one built-in intercept rule seeded by seedDefaultInterceptRules /
// seedDefaultInterceptRulesV2. Package-level so the one-time Japanese migration
// (MigrateBuiltinRulesToJapanese) can map an old row to its current name/message by pattern.
type interceptRuleSeed struct {
	name     string
	target   string // tool_name | tool_input
	typ      string // string | regex
	pattern  string
	action   string
	message  string
	enabled  bool
	priority int
}

var defaultInterceptRulesV1 = []interceptRuleSeed{
	// ── システムを壊すコマンド (priority 100) ──────────────────────────────────
	{
		name:     "[組み込み] 再帰的な強制削除 rm -rf",
		target:   "tool_input",
		typ:      "regex",
		pattern:  `(?i)\brm\b.{0,80}(?:-[a-z]*r[a-z]*f[a-z]*|-[a-z]*f[a-z]*r[a-z]*|--recursive|--no-preserve-root)`,
		action:   "deny",
		message:  "再帰的な強制削除（rm -rf / rm --recursive）は禁止。システムや標的の環境を永久に壊すおそれがある",
		priority: 100,
	},
	{
		name:     "[組み込み] システムの重要なディレクトリの削除",
		target:   "tool_input",
		typ:      "regex",
		pattern:  `\brm\b[^"'\n]{0,60}["'\s](/|/etc|/bin|/usr|/boot|/var|/lib|/sys|/proc|/dev|/sbin|/root)`,
		action:   "deny",
		message:  "システムの重要なパスの削除は禁止",
		priority: 100,
	},
	{
		name:     "[組み込み] ディスクのフォーマット mkfs",
		target:   "tool_input",
		typ:      "regex",
		pattern:  `\bmkfs\b`,
		action:   "deny",
		message:  "ディスクのフォーマット（mkfs）は禁止",
		priority: 100,
	},
	{
		name:     "[組み込み] ディスクデバイスの上書き dd",
		target:   "tool_input",
		typ:      "regex",
		pattern:  `\bdd\b[^|\n]{0,100}\bof=\s*/dev/[a-zA-Z]`,
		action:   "deny",
		message:  "dd でディスクデバイスを上書きするのは禁止",
		priority: 100,
	},
	{
		name:     "[組み込み] Fork 爆弾",
		target:   "tool_input",
		typ:      "regex",
		pattern:  `:\(\)\s*\{[^}]*:\|:`,
		action:   "deny",
		message:  "Fork 爆弾の実行は禁止",
		priority: 100,
	},
	{
		name:     "[組み込み] シャットダウン / 再起動",
		target:   "tool_input",
		typ:      "regex",
		pattern:  `\b(?:shutdown|reboot|halt|poweroff|init\s+[06])\b`,
		action:   "deny",
		message:  "シャットダウンや再起動のコマンドの実行は禁止",
		priority: 100,
	},
	{
		name:     "[組み込み] すべてのプロセスの強制終了",
		target:   "tool_input",
		typ:      "regex",
		pattern:  `\bkill\s+-9\s+-1\b|\bkillall\s+-9\b`,
		action:   "deny",
		message:  "kill -9 -1 や killall -9（すべてのプロセスの強制終了）は禁止",
		priority: 100,
	},
	{
		name:     "[組み込み] ディスクの消去 shred / wipe",
		target:   "tool_input",
		typ:      "regex",
		pattern:  `\b(?:shred|wipe)\b[^|\n]{0,80}/dev/[a-zA-Z]`,
		action:   "deny",
		message:  "ディスクデバイスに shred/wipe で消去をかけるのは禁止",
		priority: 100,
	},
	{
		name:     "[組み込み] ファイアウォールの規則の全消去",
		target:   "tool_input",
		typ:      "regex",
		pattern:  `\biptables\s+(?:-F|--flush)\b|\bnft\s+flush\s+ruleset\b`,
		action:   "deny",
		message:  "ファイアウォールの規則の全消去（iptables -F / nft flush）は禁止",
		priority: 100,
	},
	// ── データベースを壊す操作 (priority 90) ─────────────────────────────────
	{
		name:     "[組み込み] SQL DROP DATABASE / TABLE / SCHEMA",
		target:   "tool_input",
		typ:      "regex",
		pattern:  `(?i)\bDROP\s+(?:DATABASE|TABLE|SCHEMA|INDEX|VIEW|TABLESPACE|USER|ROLE)\b`,
		action:   "deny",
		message:  "DROP の実行は禁止。データベースのオブジェクトを取り返しのつかない形で消すおそれがある",
		priority: 90,
	},
	{
		name:     "[組み込み] SQL TRUNCATE",
		target:   "tool_input",
		typ:      "regex",
		pattern:  `(?i)\bTRUNCATE\s+(?:TABLE\s+)?\w`,
		action:   "deny",
		message:  "TRUNCATE の実行は禁止。テーブルのすべてのデータを消すおそれがある",
		priority: 90,
	},
	{
		name:     "[組み込み] MongoDB drop / dropDatabase",
		target:   "tool_input",
		typ:      "regex",
		pattern:  `(?i)\.(?:dropDatabase|dropCollection|drop)\s*\(`,
		action:   "deny",
		message:  "MongoDB の drop の操作は禁止",
		priority: 90,
	},
	{
		name:     "[組み込み] Redis FLUSHALL / FLUSHDB",
		target:   "tool_input",
		typ:      "regex",
		pattern:  `(?i)\b(?:FLUSHALL|FLUSHDB)\b`,
		action:   "deny",
		message:  "Redis の FLUSHALL / FLUSHDB の実行は禁止。キャッシュのデータをすべて消すおそれがある",
		priority: 90,
	},
	// ── 破壊的な HTTP リクエスト (priority 80) ──────────────────────────────────
	// Agent が DELETE リクエストを送るよくある3つの方法：
	//   1. curl -X DELETE / --request DELETE（Bash ツールで直接実行するか、スクリプトに書く）
	//   2. Python の HTTP クライアントの .delete() メソッド
	//   3. JS/一般のスクリプトの method: 'DELETE' / method="DELETE"
	{
		name:     "[組み込み] curl / wget での DELETE リクエストの送信",
		target:   "tool_input",
		typ:      "regex",
		pattern:  `(?i)\bcurl\b[^|\n&;"]{0,300}(?:-X\s*DELETE|--request\s+DELETE|-XDELETE)|\bwget\b[^|\n&;"]{0,300}--method[=\s]+DELETE`,
		action:   "deny",
		message:  "curl/wget で HTTP DELETE リクエストを送るのは禁止。標的のシステムのデータを消すおそれがある",
		priority: 80,
	},
	{
		name:     "[組み込み] Python の HTTP クライアントの DELETE（requests/httpx/aiohttp）",
		target:   "tool_input",
		typ:      "regex",
		pattern:  `(?i)\b(?:requests|httpx|aiohttp|urllib\.request)\.delete\s*\(|session\.delete\s*\(|client\.delete\s*\(`,
		action:   "deny",
		message:  "Python の HTTP クライアントで DELETE リクエストを送るのは禁止",
		priority: 80,
	},
	{
		name:     "[組み込み] スクリプトでの HTTP DELETE メソッドの宣言（JS/一般）",
		target:   "tool_input",
		typ:      "regex",
		pattern:  `(?i)axios\.delete\s*\(|method\s*[:=]\s*['"]DELETE['"]`,
		action:   "deny",
		message:  "スクリプトで HTTP DELETE リクエストを宣言して送るのは禁止",
		priority: 80,
	},
	{
		name:     "[組み込み] 一括の全消去 / 消去のエンドポイントのパス",
		target:   "tool_input",
		typ:      "regex",
		pattern:  `(?i)/(?:clear|wipe|flush|purge|truncate|drop|destroy|factory[-_]reset|reset[-_]all)(?:[/?#"'\s]|$)`,
		action:   "deny",
		message:  "一括の全消去や破棄にあたるエンドポイント（/clear /wipe /flush /purge など）の呼び出しは禁止",
		priority: 80,
	},
}

var defaultInterceptRulesV2 = []interceptRuleSeed{
	{
		name:     "[組み込み] システムを壊すコマンド",
		pattern:  `(?i)\b(rm\s+-rf\s+/|mkfs|dd\s+if=|:\(\)\s*\{|shutdown|reboot|>\s*/dev/sd)`,
		action:   "deny",
		message:  "破壊的なコマンドを拒否した（rm -rf / / mkfs / dd / fork bomb / シャットダウン・再起動 / ディスクデバイスの上書き）",
		enabled:  true,
		priority: 100,
	},
	{
		name:     "[組み込み] データ持ち出しのパイプ",
		pattern:  `(?i)(curl|wget|nc|ncat)\b[^|]*\b(\|\s*(curl|wget|nc))`,
		action:   "deny",
		message:  "データ持ち出しの疑いのあるパイプを拒否した（コマンドの出力を curl/wget/nc で外に送る）",
		enabled:  false,
		priority: 80,
	},
}

// assetInterceptRuleSeed is one built-in asset blocklist entry.
type assetInterceptRuleSeed struct {
	kind    string
	pattern string
	note    string
}

var defaultAssetInterceptRules = []assetInterceptRuleSeed{
	{"fuzzy_domain", ".gov", "[組み込み] 政府のサイト (.gov)"},
	{"fuzzy_domain", ".gov.cn", "[組み込み] 政府のサイト (.gov.cn)"},
	{"fuzzy_domain", ".edu", "[組み込み] 教育機関のサイト (.edu)"},
	{"fuzzy_domain", ".edu.cn", "[組み込み] 教育機関のサイト (.edu.cn)"},
}

// seedDefaultAssetInterceptRules inserts the built-in asset blocklist (fuzzy
// domain matches for government / education sites) once on first startup. Gated
// by a settings flag so a user's later disable/delete is never resurrected on
// restart — same policy as the intercept-rule seed.
func (d *DB) seedDefaultAssetInterceptRules() error {
	if v, _, _ := d.GetSetting("asset_intercept_default_rules_v1"); v == "done" {
		return nil
	}
	rules := defaultAssetInterceptRules
	for _, r := range rules {
		if _, err := d.Exec(`
INSERT INTO asset_intercept_rules(enabled, kind, pattern, note, builtin)
VALUES (true, $1, $2, $3, true)
ON CONFLICT DO NOTHING`, r.kind, r.pattern, r.note); err != nil {
			return fmt.Errorf("asset rule %q: %w", r.pattern, err)
		}
	}
	return d.SetSetting("asset_intercept_default_rules_v1", "done")
}

// builtinSkillVisibility maps a shipped skill's directory name → the built-in
// agent keys that should see it by default. The skill FILES themselves live on the
// filesystem (SkillDir, loaded by norma at runtime); DB only carries this visibility
// binding. Skills omitted here (e.g. playwright-cli, scopesentry) ship invisible by
// default — the user turns them on per-agent when needed. scopesentry additionally
// declares `mcps: ScopeSentry`, which only takes effect once it's made visible and
// that MCP is enabled/configured.
var builtinSkillVisibility = map[string][]string{
	"api-recon": {"auto", "pentest", "worker"},
}

// seedBuiltinSkillVisibility binds the shipped built-in skills to their default
// agents. Insert-if-absent (ON CONFLICT DO NOTHING) so a user's later toggle-off is
// never resurrected on restart — matches the browser-MCP / intercept-rule seed policy.
func (d *DB) seedBuiltinSkillVisibility() error {
	for skillName, agentKeys := range builtinSkillVisibility {
		for _, key := range agentKeys {
			if _, err := d.Exec(`
INSERT INTO agent_skill_visibility(agent_id, skill_name, enabled)
SELECT id, $2, true FROM agents WHERE key=$1
ON CONFLICT (agent_id, skill_name) DO NOTHING`, key, skillName); err != nil {
				return fmt.Errorf("skill %s → agent %s: %w", skillName, key, err)
			}
		}
	}
	return nil
}

// seedDefaultInterceptRules inserts built-in safety intercept rules once on
// first startup. The seed is gated by a settings flag so user edits (disable,
// delete, re-order) are never overwritten on subsequent restarts.
func (d *DB) seedDefaultInterceptRules() error {
	if v, _, _ := d.GetSetting("intercept_default_rules_v1"); v == "done" {
		return nil
	}
	rules := defaultInterceptRulesV1
	for _, r := range rules {
		if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action)
VALUES ($1, true, $2, $3, $4, $5, $6, $7, false, 60, 'deny')
ON CONFLICT DO NOTHING`,
			r.name, r.priority, r.target, r.typ, r.pattern, r.action, r.message,
		); err != nil {
			return fmt.Errorf("rule %q: %w", r.name, err)
		}
	}
	return d.SetSetting("intercept_default_rules_v1", "done")
}

// seedDefaultInterceptRulesV2 migrates the two safety patterns that used to be
// hard-coded in guard.go (destructive shell + data-exfil pipe) into ordinary
// intercept rules. Gated by its own flag so it also lands on DBs that already ran
// v1. Unlike the old guard.go floor, these are plain [組み込み] rules — the user can
// disable or delete them. The exfil rule ships DISABLED by default (its
// curl/wget/nc pipe pattern mis-fires on legitimate CTF/pentest reverse-shell and
// data-transfer pipes); enable it manually when exfil gating is actually wanted.
func (d *DB) seedDefaultInterceptRulesV2() error {
	if v, _, _ := d.GetSetting("intercept_default_rules_v2"); v == "done" {
		return nil
	}
	rules := defaultInterceptRulesV2
	for _, r := range rules {
		if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action)
VALUES ($1, $2, $3, 'tool_input', 'regex', $4, $5, $6, false, 60, 'deny')
ON CONFLICT DO NOTHING`,
			r.name, r.enabled, r.priority, r.pattern, r.action, r.message,
		); err != nil {
			return fmt.Errorf("rule %q: %w", r.name, err)
		}
	}
	return d.SetSetting("intercept_default_rules_v2", "done")
}

// seedDefaultInterceptRulesV3 adds the delete-endpoint path rule. The v1 HTTP rules
// only catch the DELETE *method* (curl -X DELETE, requests.delete(, method:'DELETE'),
// and v1's path rule covers only /clear /wipe /flush /purge /truncate /drop /destroy
// /factory-reset /reset-all — so a plain `curl 'http://t/api/user/delete?id=1'` (a
// delete endpoint reached with GET/POST, which is how most web apps expose deletion)
// slipped through every built-in rule. Own flag so it also lands on DBs that already
// ran v1/v2, where editing the v1 seed would have no effect.
//
// The pattern deliberately requires a separator after the verb so /delivery,
// /details, /delta and /delegate do not match, while /deleteAll, /delete_user and
// /delete-user do. destroy is re-covered here because v1's rule does not allow a
// suffix (/destroyAll was missed).
//
// Exported as a package const only so the seeded regex is unit-testable without a DB.
const deleteEndpointPathPattern = `(?i)/(?:(?:delete|remove|unlink|erase|destroy)[-\w]*|del)(?:[/?#"'\s]|$)`

// deleteEndpointRuleName / deleteEndpointRuleMessage are the v3 rule's name and message
// (shared with MigrateBuiltinRulesToJapanese).
const deleteEndpointRuleName = "[組み込み] 削除にあたるエンドポイントのパス"
const deleteEndpointRuleMessage = "削除にあたるエンドポイント（/delete /remove /unlink /erase など）の呼び出しは、どの HTTP メソッドでも禁止——多くのアプリの削除のエンドポイントは GET/POST でも動き、同じように標的のデータを本当に消す"

func (d *DB) seedDefaultInterceptRulesV3() error {
	if v, _, _ := d.GetSetting("intercept_default_rules_v3"); v == "done" {
		return nil
	}
	const name = deleteEndpointRuleName
	if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action)
SELECT $1, true, 80, 'tool_input', 'regex', $2, 'deny', $3, false, 60, 'deny'
WHERE NOT EXISTS (SELECT 1 FROM intercept_rules WHERE name = $1)`,
		name,
		deleteEndpointPathPattern,
		deleteEndpointRuleMessage,
	); err != nil {
		return fmt.Errorf("rule %q: %w", name, err)
	}
	return d.SetSetting("intercept_default_rules_v3", "done")
}
