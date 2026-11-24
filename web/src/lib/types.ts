// ARTEX domain model — types used across the UI.
// Derived from the functional spec (section 7: 主要なデータの形).

export type TaskStatus = "created" | "queued" | "running" | "paused" | "done" | "failed" | "timeout";
export type EngineMode = "exploring" | "paused" | "stalled" | "idle";

export interface Task {
  id: string;
  name?: string; // 任意のタスクの名前;空/省略=名前なし。表示するときは説明に戻る
  category_id?: number;
  category_name?: string;
  pinned?: boolean;
  pinned_at?: string | null;
  description: string;
  goal: string;
  status: TaskStatus;
  created_at: string;
  created_unix?: number; // created_at as unix seconds (run-duration calc)
  completed_at?: string; // RFC3339 finish time (done/failed); "" if unfinished
  completed_unix?: number; // completed_at as unix seconds (0/undef if unfinished)
  last_activity_unix?: number; // unix seconds of the last activity (0/undef if none)
  paused?: boolean;
  queued?: boolean;
  active?: boolean;
  in_flight?: number;
  findings?: { critical: number; high: number; medium: number; low: number }; // 登録済みの脆弱性の数(深刻度ごとの段)
  last_activity?: string;
  stalled?: boolean;
  goals_total?: number;
  goals_met?: number;
  engine_mode?: EngineMode;
  tokens?: TokenTotal; // whole-task token consumption
  llm_profile_id?: number; // LLM profile used; absent = default profile
  llm_profile_ids?: number[]; // ordered task-level failover chain
  active_llm_profile_id?: number; // profile used by the next LLM call
  llm_failover_state?: "default" | "ready" | "chain_exhausted" | string;
  llm_failover_reason?: string;
  source_task_ids?: string[]; // directly related tasks inherited as read-only context
  archive_blocked_by_task_id?: string; // live direct dependent that must be archived first
  company_ids?: number[]; // associated company scopes; current company assets join the task at creation
  coverage_enabled?: boolean; // 資産のカバー率の機能の切り替え(作るときに決め、既定でオン)；false=カバー率を計算/表示しない
}

export interface TaskCategory {
  id: number;
  name: string;
  task_count: number;
  created_at: string;
  updated_at: string;
}

export interface TaskTemplate {
  id: number;
  name: string;
  description: string;
  goal: string;
  category_id?: number | null; // あらかじめ決めた分類；null/省略=なし
  intercept_rules?: AssetInterceptRuleInput[]; // あらかじめ決めたタスク単位の遮断/許可の規則
  created_at: string;
  updated_at: string;
}

export interface DeleteTaskOptions {
  delete_assets: boolean;
  delete_traffic: boolean;
  delete_files: boolean;
  delete_findings: boolean;
  delete_llm_records: boolean;
}

export interface DeleteTaskResult {
  deleted: string;
  assets_deleted: number;
  assets_detached: number;
  traffic_deleted: number;
  files_deleted: boolean;
  findings_deleted: number;
  llm_records_deleted: number;
  cleanup_warning?: string;
}

export type TaskArchiveState =
  | "archive_queued"
  | "archiving"
  | "archive_failed"
  | "ready"
  | "restore_queued"
  | "restoring"
  | "restore_failed"
  | "delete_queued"
  | "deleting"
  | "delete_failed";

export interface TaskArchiveTokenStats {
  calls?: number;
  input_tokens?: number;
  output_tokens?: number;
  cache_read_tokens?: number;
  cache_write_tokens?: number;
}

export interface TaskArchive {
  id: number;
  task_id: number;
  state: TaskArchiveState;
  phase: string;
  progress: number;
  error?: string;
  warnings?: string[];
  format_version: number;
  sha256?: string;
  original_size: number;
  compressed_size: number;
  task_name: string;
  task_description: string;
  task_goal: string;
  original_status: TaskStatus;
  category_id?: number;
  category_name?: string;
  source_task_ids: number[];
  remaining_timeout_seconds: number;
  data_counts: Record<string, number>;
  aggregate_stats: {
    tokens?: TaskArchiveTokenStats;
    skills?: Record<string, number>;
    tools?: Record<string, number>;
    findings?: Record<string, number>;
  };
  archived_at?: string;
  requested_at: string;
  created_at: string;
  updated_at: string;
}

export interface TaskArchivePage {
  items: TaskArchive[];
  total: number;
  page: number;
  size: number;
}

export interface ArchiveBatchItem {
  id: string;
  archive_id?: number;
  ok: boolean;
  queued: boolean;
  error?: string;
}

// ---- Asset graph (global, shared across tasks) ----
export type AssetType =
  | "company"
  | "domain"
  | "ip"
  | "port"
  | "service"
  | "site"
  | "endpoint"
  | "parameter"
  | "tech"
  | "credential"
  | "data";

export type NodeState = "observed" | "confirmed" | "tombstoned";

export interface AssetNode {
  id: string;
  type: AssetType;
  name: string;
  key: string; // nkey
  value?: string;
  company_id?: string; // 帰属する企業の資産の id；空=未帰属
  state: NodeState;
  confidence: number; // 0..1
  attrs?: Record<string, unknown>;
  first_seen: string;
  last_seen: string;
}

export type AssetRel =
  | "owns"
  | "resolves"
  | "exposes"
  | "runs"
  | "serves"
  | "has_endpoint"
  | "has_param"
  | "fingerprinted"
  | "authenticates_as"
  | "reachable"
  | "has_subdomain";

export interface Edge {
  src: string;
  dst: string;
  rel: AssetRel | ExploreRel;
}

// Task asset view — server-side enriched, paginated.
export interface TaskAssetRef {
  id: string;
  name?: string;
  key: string;
  attrs?: Record<string, unknown>;
}

export interface TaskAssetItem extends AssetNode {
  techs?: TaskAssetRef[];
  auth?: TaskAssetRef[];
  params?: TaskAssetRef[];
}

export interface TaskAssetView {
  counts: Record<string, number>;
  total: number;
  items: TaskAssetItem[];
}

// ---- New unified asset model (new backend) ----
export type NewAssetType = "root_domain" | "ip" | "subdomain" | "app" | "service" | "endpoint";

export interface Asset {
  id: number;
  type: NewAssetType;
  company_id?: number;
  task_ids: number[];
  domain?: string;
  root_domain?: string;
  ip?: string;
  c_segment?: string;
  port?: number;
  icp?: string;
  bound_domains?: string[];
  open_ports?: { port: number; service?: string }[];
  record_type?: string;
  record_value?: string[] | string;
  bundle_id?: string;
  app_name?: string;
  category?: string;
  app_description?: string;
  app_icp?: string;
  url?: string;
  service_type?: string;
  service_name?: string;
  favicon_mmh3?: string;
  status_code?: number;
  content_length?: number;
  page_title?: string;
  technologies?: string[];
  auth?: Record<string, unknown>[];
  method?: string;
  params?: Record<string, unknown>[];
  extra?: Record<string, unknown>;
  last_seen: string;
  task_source?: string;
  task_source_summary?: string;
  task_source_node_id?: number;
}

export interface IntentAsset {
  intent_id: number | string;
  asset_id: number;
  type: NewAssetType;
  label: string;
  source: string;
  source_summary: string;
  source_node_id?: number;
  source_task_id: number;
  inherited: boolean;
}

export interface TaskAssetMutation {
  requested: number;
  attached: number;
  existing: number;
}

export interface TaskAssetScopeMutation {
  requested: number;
  assets_linked: number;
  assets_existing: number;
  scopes_added: number;
  scopes_existing: number;
}

// ---- Asset coverage graph (per task) ----
// 力学モデルの「資産のカバー率の図」のノード1つ。key は一意：資産="a:<id>"、企業="c:<id>"、
// 資産の行のないルートドメイン="r:<domain>"。in_scope=false のものは線をつなぐためだけの灰色の文脈のノード。
export interface CoverageGraphNode {
  key: string;
  kind: "company" | "root_domain" | "subdomain" | "ip" | "service" | "app" | "endpoint";
  label: string;
  tested: boolean;
  in_scope: boolean;
  asset_id?: number;
  company_id?: number;
  domain?: string;
  root_domain?: string;
  ip?: string;
  url?: string;
  port?: number;
  service_type?: string;
  app_name?: string;
  page_title?: string;
  status_code?: number;
}

export interface CoverageGraphEdge {
  src: string;
  dst: string;
}

export interface CoverageGraphData {
  nodes: CoverageGraphNode[];
  edges: CoverageGraphEdge[];
}

// ある資産がこのタスクの探索の図で関連づいた意図/事実/発見（カバー率の図のノードの引き出しで使う）。
export interface CoverageAssetRef {
  id: number;
  kind: string;
  state: string;
  summary: string;
  source_task_id?: string;
  inherited?: boolean;
}
export interface CoverageAssetRefs {
  intents: CoverageAssetRef[];
  facts: CoverageAssetRef[];
  findings: CoverageAssetRef[];
}

// ---- Workspace file manager (workDir) ----
export interface WorkspaceEntry {
  name: string;
  path: string; // workspace-relative, forward slashes
  dir: boolean;
  size: number;
  mtime: number; // unix millis
}
export interface WorkspaceListing {
  path: string;
  entries: WorkspaceEntry[];
}
export interface WorkspaceFile {
  path: string;
  size: number;
  binary: boolean;
  too_large?: boolean;
  content?: string;
}

// タスクのテストの範囲の1件（カバー率の分母 + 認可の境界）。
export interface TaskScopeRow {
  id: number;
  task_id: number;
  kind: "company" | "root_domain" | "subdomain" | "ip" | "cidr" | "icp" | "keyword";
  company_id?: number;
  company_name?: string; // バックエンドが companies を JOIN して解決する。kind=company のときだけ値がある
  domain?: string;
  net?: string;
  value?: string;
  source: "auto" | "agent" | "manual";
  reason?: string;
}

export type CompanyScopeKind = "domain" | "ip" | "cidr" | "icp" | "keyword";

// 企業を追加するときに送る構造化した資産の範囲の規則。
export interface CompanyScopeRule {
  kind: CompanyScopeKind;
  value: string;
}

// 資産の範囲を書き込んだ結果。errors は今回送ったうちの不正な行；warnings は今回送ったものとは関係ないが、
// 帰属の結果を期待どおりでなくしてしまう既存のデータの問題（ip のフィールドにホスト名を入れた資産など）。
export interface CompanyScopeMutation {
  added: number;
  skipped: number;
  invalid: number;
  errors?: string[];
  warnings?: string[];
}

// 企業の資産の範囲の規則の1件（帰属の唯一の正の源）。
export interface ScopeRow {
  id: number;
  company_id: number;
  kind: CompanyScopeKind;
  domain?: string; // kind=domain のとき値がある
  net?: string; // kind=ip|cidr のとき値がある
  value?: string; // kind=icp|keyword のときはバックエンドが直接返すことがある
  raw: string; // ユーザーの元の入力。表示と書き戻しに使う
  reason?: string;
}

// 企業：type=company の資産のノード + アイコン + 資産の数 + 資産の範囲の規則。
export interface Company {
  id: number;
  name: string;
  logo?: string; // 外部のアイコンの URL；空ならフロントエンドは名前の頭文字を使う
  asset_count: number;
  scope?: ScopeRow[];
}

// ---- Exploration graph (per task) ----
export type ExploreKind = "task" | "begin" | "goal" | "intent" | "fact" | "finding" | "hint" | "digest";
export type GoalState = "open" | "met" | "abandoned";
export type IntentState = "open" | "running" | "paused" | "done" | "blocked" | "exhausted" | "stopped";
export type FindingState = "confirmed" | "dismissed";
export type HintState = "active" | "consumed";
export type ExploreRel = "spawns" | "derived_from" | "yields" | "proves" | "covers";

export interface TaskNode {
  id: string;
  type: ExploreKind;
  payload?: string;
  priority: number; // 0..10
  state: string; // GoalState | IntentState | FindingState | HintState
  origin: string;
  ts: string;
  source_task_id?: string;
  inherited?: boolean;
  delete_reason?: string; // 意図の論理削除(state='deleted')のときの削除の理由
}

// 実況ボードの1ページ:作成の順でページに分けたノード + このページに関わる辺 + 辺の反対の端のノード(refs、id で引く)。
// こうすれば図全体を取ってこなくても、実況の1件ごとに「どこから来て、何を生んだか」を説明できる。
export interface ExplorationNodePage {
  items: TaskNode[];
  total: number;
  page: number;
  size: number;
  edges: Edge[];
  refs: Record<string, TaskNode>;
  // ノードの id → そのノードが裏づける資産(実況ボードを開いたときに一緒に表示する。このページのノードとその隣を含む)。
  assets: Record<string, FindingAsset[]>;
}

export interface ExplorationNodeQuery {
  page?: number;
  size?: number;
  kinds?: ExploreKind[];
  states?: string[];
  q?: string;
  order?: "asc" | "desc";
}

// 目標の管理のカードで使う目標(バックエンドが payload を text/vulnclass に分け済み)。
export interface TaskGoal {
  id: string;
  text: string;
  vulnclass?: string;
  state: string; // GoalState
  origin?: string;
  ts: string;
}

// 制約の管理のカードで使う操作の制約(allow=許可 / deny=禁止)。
export type ConstraintKind = "allow" | "deny";
export interface TaskConstraint {
  id: string;
  kind: ConstraintKind;
  text: string;
  origin?: string;
  ts?: string;
}

// ---- Findings ----
export type Severity = "critical" | "high" | "medium" | "low";

// 脆弱性の対応の状態:未対応 / 対応中 / 確認済み / 対応済み / 修正済み / 誤検知 / 無視 / 重複 / リスク受容。
export type FindingStatus =
  | "pending"
  | "in_progress"
  | "confirmed"
  | "resolved"
  | "fixed"
  | "false_positive"
  | "ignored"
  | "duplicate"
  | "risk_accepted";

// FindingAsset は脆弱性に結びついた資産1つ(バックエンドで label を描画済み)。
export interface FindingAsset {
  id: string;
  type: string;
  label: string;
}

export interface Finding {
  traffic_count?: number;
  evidence_version?: number;
  report_evidence_version?: number;
  report_stale?: boolean;
  id: string;
  finding_id?: string; // 独立した findings のテーブルの行の id。状態の更新の手がかり(タスクの中の古いノードにはないことがある)
  vulnclass: string;
  name?: string; // 脆弱性の名前;空のときは vulnclass を表示する
  severity: Severity;
  status: FindingStatus;
  summary: string;
  evidence: string;
  report?: string; // 詳しい報告書(Markdown);詳細のインターフェースだけが返し、一覧では空
  intent_id?: string;
  param_id?: string;
  task_id?: string;
  task_description?: string;
  source_task_id?: string;
  inherited?: boolean;
  assets?: FindingAsset[];
  ts: string;
}

// FindingsPage は発見の一覧のサーバー側のページ分けの応答。
export interface FindingsPage {
  items: Finding[];
  total: number;
  page: number;
  page_size: number;
}

export interface FindingGroup {
  task_id: string | number | null;
  task_name?: string; // 任意のタスクの名前;空/省略=名前なし
  task_description: string;
  task_status: string;
  count: number;
  critical: number;
  high: number;
  medium: number;
  low: number;
  last_found_at: string;
}

export interface FindingGroupsPage {
  items: FindingGroup[];
  total: number;
  finding_total: number;
  page: number;
  page_size: number;
}

export interface FindingDeepenResponse {
  task_id: string;
  intent_id: string;
  state: IntentState;
  queued: boolean;
}

// FindingStats は発見のテーブル全体の集計(統計のカード + 脆弱性の種類のプルダウン)。サーバー側で計算し、ページ分けの影響を受けない。
export interface FindingStats {
  total: number;
  pending: number;
  critical: number;
  high: number;
  medium: number;
  low: number;
  vulnclasses: string[];
  tasks: FindingTaskOption[];
}

// FindingTaskOption は発見のページの「タスクごと」の絞り込みのプルダウンの1項目:脆弱性のあるタスク(説明が空ならタスクが削除済みで、
// フロントエンドは id を表示する)とその脆弱性の件数。
export interface FindingTaskOption {
  id: string | number;
  name?: string; // 任意のタスクの名前;空/省略=名前なし
  description: string;
  count: number;
}

// FindingQuery は発見の一覧のページ分け/絞り込み/並べ替えのパラメータ。
export interface FindingQuery {
  page: number;
  pageSize: number;
  severity?: "all" | Severity;
  status?: "all" | FindingStatus;
  vulnclass?: string;
  task?: string; // タスクの id;"all"/空 = タスクで絞り込まない
  query?: string;
  sort?: "severity" | "time";
  // 資産の木のノードの key;ノードを1つ選ぶ = その部分木全体を選ぶ。空 = 資産で絞り込まない。
  assetScope?: string;
}

// ---- Findings by asset (資産の表示) ----
export type FindingAssetKind = "company" | "root_domain" | "subdomain" | "ip" | "service" | "app" | "endpoint" | "none";

// FindingAssetNode は資産の木のノード1つ。key の形は a:<id>(資産)、c:<id>(企業)、
// r:<domain>(DB に資産の行のないルートドメイン)、__none__(資産に関連づいていない)。
export interface FindingAssetNode {
  key: string;
  parent?: string;
  kind: FindingAssetKind;
  label: string;
  asset_id?: number;
  company_id?: number;
  self: number; // その資産に直接ぶら下がる発見の数
  total: number; // 子孫を含み、発見ごとに重複を除く
  critical: number;
  high: number;
  medium: number;
  low: number;
  last_found_at: string;
}

export interface FindingAssetTree {
  nodes: FindingAssetNode[];
  finding_total: number;
  truncated: boolean;
  dropped_kinds?: string[];
}

// FINDING_UNASSIGNED_ASSET はバックエンドの db.FindingUnassignedAsset に対応する。
export const FINDING_UNASSIGNED_ASSET = "__none__";

// ---- Activity / sessions ----
export type ActivityKind =
  | "tool_use"
  | "tool_result"
  | "text"
  | "thinking"
  | "result"
  | "user"
  | "intent" // LLM-generated exploration objective leading a worker session (UI-synthesized)
  | "round" // planner round boundary marker (engine-emitted)
  | "usage" // live cumulative token usage (per model turn); not rendered
  | "llm_switch" // automatic/manual task-level LLM switch
  | "llm_failover" // task-level provider switch / chain exhaustion audit event
  | "intercept_request"; // user-approval request from the intercept layer

// ChatAttachment は1回のアップロードのファイル:path はその会話/タスクの作業ディレクトリ(つまり agent の CWD)からの相対パス。
export interface ChatAttachment {
  name: string;
  path: string;
  size: number;
  abs?: string; // 絶対パス(scope=staging の一時アップロードのときに返す;タスクを作る前に説明に書き込む)
}

export interface Activity {
  seq: number;
  intent_id?: string;
  worker: string; // session owner: planner | mainagent | work#1 ...
  ts: string;
  kind: ActivityKind;
  tool?: string;
  tool_use_id?: string;
  is_error?: boolean;
  summary: string;
  detail?: string;
  metadata?: {
    llm_transition?: LLMTransition;
  };
  source_task_id?: string;
  inherited?: boolean;
  main_seg?: number; // main-agent conversation segment (present only on worker="mainagent" rows)
  // token usage (present only on kind='result')
  input_tokens?: number;
  output_tokens?: number;
  cache_read_tokens?: number;
  cache_write_tokens?: number;
}

export interface LLMAuditProfile {
  id: number;
  name: string;
  format: string;
  model: string;
}

export interface LLMTransition {
  mode: "automatic" | "manual" | "exhausted";
  reason: string;
  previous?: LLMAuditProfile;
  next?: LLMAuditProfile;
}

export interface TaskLLMResolution {
  profile_id?: number;
  name: string;
  format: string;
  model: string;
  source: "task_chain" | "agent_binding" | "global_profile" | "environment" | "global";
  available: boolean;
  reason?: string;
}

export interface TaskLLMResolutions {
  mainagent: TaskLLMResolution;
  planner: TaskLLMResolution;
  worker: TaskLLMResolution;
}

// ---- Agent triggers (P3 のスケジュール、カスタム agent だけ) ----
export interface AgentTrigger {
  id: number;
  agent_key: string;
  enabled: boolean;
  interval_sec: number; // 定期:N 秒ごと(0=定期なし)
  on_finding: boolean; // どれかのタスクが finding を見つけたら起動
  on_goal_met: boolean; // どれかのタスクが目標を達成したら起動
  on_task_timeout: boolean; // どれかのタスクが時間切れになったら起動
  on_tool_call: boolean; // 選んだツールが呼ばれた(実行が終わった)ら起動
  on_task_create: boolean; // どれかのタスクが作られたら起動
  interval_message: string; // 起動の条件ごとの独立したユーザーのメッセージ
  finding_message: string;
  goal_message: string;
  task_timeout_message: string;
  tool_call_message: string;
  task_create_message: string;
  tool_names: string[]; // on_tool_call で選んだツールの key(少なくとも1つ)
  last_fire?: string;
}

// ---- Conversations (chat page) ----
export interface ActiveFindingRetest {
  id: number;
  finding_id: string;
  conversation_id: number;
  status: "pending" | "running";
}

export interface FindingRetest {
  id: number;
  finding_id: number;
  conversation_id: number | null;
  status: "pending" | "running" | "completed" | "failed" | "stopped";
  verdict: "" | "reproduced" | "fixed" | "inconclusive";
  notes: string;
  summary: string;
  evidence: string;
  error: string;
  created_at: string;
  started_at: string | null;
  finished_at: string | null;
}

export interface Conversation {
  id: number;
  running?: boolean; // live server state, returned with the conversation list
  agent_key: string;
  title: string;
  llm_profile_id?: number;
  pinned?: boolean;
  pinned_at?: string | null;
  created_at: string;
  updated_at: string;
}

// ---- Backend logs (/logs page) ----
export interface LogLine {
  seq: number;
  db_id?: number; // server_logs.id; present for DB-persisted lines
  ts: string;
  level: "info" | "warn" | "error";
  tag: string;
  text: string;
}

export type SessionRole = "mainagent" | "planner" | "worker" | "system";
export type SessionStatus = "running" | "paused" | "done" | "blocked" | "exhausted" | "pending" | "stopped" | "deleted";

// Daily token aggregate bucket (GET /api/tokens/daily).
export interface DailyTokenBucket {
  date: string; // "YYYY-MM-DD"
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// Per-worker token usage (GET /api/exploration/tokens).
export interface TokenUsage {
  worker: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

export interface SessionTokenUsage {
  session: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

export interface BatchControlItem {
  id: string;
  ok: boolean;
  status?: string;
  queued?: boolean;
  error?: string;
}

// 分類の一括変更のタスクごとの結果。失敗はタスクがすでに削除された場合しかありえず、分類そのものの書き込みは原子的。
export interface BatchCategoryItem {
  id: string;
  ok: boolean;
  error?: string;
}

// Whole-task (all agents) token aggregate.
export interface TokenTotal {
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// Global per-profile token spend from the llm_usage ledger (GET /api/tokens/usage).
export interface ProfileUsage {
  profile_name: string;
  calls: number;
  tasks: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// One (profile, UTC day) token bucket for the dashboard's daily chart (new source).
export interface ProfileDayUsage {
  profile_name: string;
  date: string; // YYYY-MM-DD
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
}

// Response of GET /api/tokens/usage — the dashboard's "new" (llm_usage) token view.
export interface UsageStats {
  by_profile: ProfileUsage[];
  daily: ProfileDayUsage[];
}

// Per-model token usage for one task (GET /api/llm/records/by-model), from the
// always-on llm_usage metering ledger. calls = number of LLM calls on this model.
export interface ModelTokenStat {
  model: string;
  calls: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

export interface Session {
  id: string;
  role: SessionRole;
  title: string;
  status: SessionStatus;
  live: boolean;
  last_activity: string;
  intent_id?: string;
  source_task_id?: string;
  inherited?: boolean;
  seg?: number; // main-agent session: which conversation segment (0 = original)
}

// ---- Security ----
export interface AuditEntry {
  ts: string;
  tool: string;
  action: "allow" | "block";
  reason?: string;
  command?: string;
}

export interface Audit {
  entries?: AuditEntry[];
  attributions?: Record<string, number>;
}

// ---- Traffic ----
export interface TrafficExchange {
  id: string;
  ts: string;
  host: string;
  method: string;
  url: string;
  status: number;
  content_type: string;
  resp_len: number;
}

export interface TrafficResp {
  enabled: boolean;
  proxy?: string;
  count?: number; // global total (unfiltered)
  total?: number; // rows matching the current filter (for pagination)
  page?: number;
  size?: number;
  exchanges?: TrafficExchange[];
}

// Full raw request/response of one exchange (lazy-loaded on row select).
export interface TrafficDetail {
  req: string;
  resp: string;
}

// One distinct recorded host with its exchange count (target picker).
export interface TrafficHost {
  host: string;
  count: number;
}

// ---- App settings (runtime toggles) ----
export interface Settings {
  traffic_capture: boolean;
  agent_traffic_binding: boolean; // Agent が通信の証拠を自動で結びつける。既定はオフ；人が結びつけるのには影響しない
  llm_record: boolean; // LLM の記録の切り替え（既定はオフ）；オフのときは LLM の呼び出しを一切記録しない
  // Web search. brave_key_set / tavily_key_set reflect whether a key is stored
  // (the values are never returned). On PUT, send the corresponding field to set/clear.
  web_search_enabled: boolean;
  web_search_backend: string; // "ddgs" | "brave-free" | "tavily" | "deepseek"
  brave_key_set: boolean;
  tavily_key_set: boolean;
  // write-only: only sent on PUT to store/clear the key.
  brave_search_api_key?: string;
  tavily_search_api_key?: string;
  // 独立した外向きのプロキシ(http/https/socks5)で、検索のエンドポイントに使う；通信を記録する MITM プロキシとは関係ない。空=直接。
  web_search_proxy?: string;
  // 全体の外向きのプロキシ(http/https/socks5。user:pass を付けられる)。標的へのすべての通信がこれを通る。通信の記録をオンにしたときは
  // MITM の上流になる；記録をオフにしたときは agent の bash/WebFetch に直接入れる。空=直接。
  global_proxy?: string;
  python_interpreter?: string; // カスタムのスクリプトのツールの python のインタープリタのパス(空=実行時に検出)
  workers?: number; // 並行して動く作業の agent の数(既定 3)；この後に始めるタスクに効く
  // タスクの並行の上限:同時に「実行中」のタスクの数の上限。オフ=無制限;オンにして新しいタスクが上限を超えたら並び、空きが出たら自動で始める。
  task_concurrency_enabled?: boolean; // 既定 false
  task_concurrency_limit?: number; // オンにしたときの既定は 5
  // LLM のローテーション(フェイルオーバー)。既定はオフ；オンにすると「モデルを指定していない」agent は、いまの設定が使えないとき
  // （残高不足/key の失効/レート制限/サービスの異常）に次の設定へ自動で切り替える。
  llm_pool_enabled?: boolean; // 既定 false
  // 特定の設定を結びつけた agent/タスクが失敗したときもローテーションの連なりに戻るか。既定 false = 結びつけたら独占。
  llm_pool_bind_fallback?: boolean;
  // 操作の制約を入れる範囲(既定はすべてオン):タスクの allow/deny の制約を対応する agent のシステムプロンプトにつなげる。
  constraints_inject_planner?: boolean;
  constraints_inject_worker?: boolean;
  // 実験的な機能:noa のモデルによる文脈の圧縮(既定はオフ)。オンにすると、プラットフォームにつないだ4種類の agent(planner/
  // worker/メイン agent/会話)の文脈の圧縮を noa が引き受け、組み込みの compaction に代わる;run ごとに1回読み、
  // この後に始める run に効く。
  noa_compaction?: boolean;
  // ---- 脆弱性の IM への通知（チャンネルそのものは独立した資源。/api/notify/* を参照。ここには全体の設定が3つあるだけ）----
  notify_enabled?: boolean; // 通知の全体のスイッチ。既定はオン；保守の間にワンクリックで止めるのに使う
  notify_public_base_url?: string; // 脆弱性の詳細へ戻るリンクの外部のアドレス；空=メッセージに戻るリンクを付けない
  notify_digest_interval_min?: number; // まとめのモードの周期（分）。既定 30
  // 사용자 대면 출력 언어(en/ko/zh/es/ja). 에이전트가 생성하는 리포트·요약·대화 응답이
  // 이 언어를 따른다. 설정 화면의 "표시 언어" 선택기가 UI 언어와 함께 이 값도 저장한다.
  output_language?: string;
}

// ---- 脆弱性の IM への通知 ----

// NotificationFilter はチャンネルの絞り込みの条件。フィールドはすべて任意で、省略すれば絞り込まない。
// バックエンドはどのフィールドも確かめない：設定がおかしければ「当たり」として扱う（多めに送っても送り漏らさない）。
export interface NotificationFilter {
  min_severity?: string; // "" | low | medium | high | critical
  task_ids?: number[]; // 空=制限なし；空でなければ脆弱性の属するタスクと共通部分があること
  asset_ids?: number[]; // 空=制限なし；空でなければ脆弱性が裏づける資産と共通部分があること
  vulnclass_include?: string[]; // 空=すべて受ける；空でなければ脆弱性の種類がどれかのキーワードに当たること（大文字小文字を区別しない部分文字列）
  vulnclass_exclude?: string[]; // どれかのキーワードに当たれば除く（除外が包含より優先）
  on_status_change?: boolean; // 脆弱性の対応の状態の変更のイベントも受けるか
}

// NotificationChannel はチャンネルの実体1つ。config のフィールドは kind によって違い、
// 資格情報のフィールドは読み取るときに "__masked__" で始まるマスクした値に置き換えられる——そのまま送り返せば「変えない」を表す。
export interface NotificationChannel {
  id: number;
  name: string;
  kind: string;
  enabled: boolean;
  mode: "realtime" | "digest";
  config: Record<string, unknown>;
  filter: NotificationFilter;
  rate_per_min: number;
  created_at: string;
  updated_at: string;
  // secret_keys はバックエンドがチャンネルの種類ごとに与え、フロントエンドはそれに従ってパスワード欄と「空のままなら変えない」の案内を描き、
  // チャンネルの知識を何も決め打ちしない。
  secret_keys: string[];
}

// NotificationKind は /api/notify/meta が返すチャンネルの種類のメタデータ。
export interface NotificationKind {
  kind: string;
  default_rate_per_min: number;
  secret_keys: string[];
}

export interface NotificationMeta {
  kinds: NotificationKind[];
  enabled: boolean;
  public_base_url: string;
  digest_interval_min: string;
  defaults: { digest_interval_min: number };
  stats: {
    channels: number;
    channels_on: number;
    pending: number;
    failed: number;
    sent_today: number;
    backlog_age_ms: number;
  };
}

// NotificationDelivery は配信の記録1件で、配信の履歴と失敗の送り直しに使う。
export interface NotificationDelivery {
  id: number;
  finding_id: string;
  event_kind: string; // finding_created | finding_status_changed
  channel_id: number;
  channel_name: string;
  channel_kind: string;
  state: "pending" | "sending" | "sent" | "failed" | "skipped";
  attempts: number;
  last_error: string;
  batch_id?: number;
  created_at: string;
  sent_at?: string;
  next_attempt_at: string;
  title: string;
  severity: string;
}

// ---- LLM config ----
export interface LLMProfile {
  id: string;
  name: string;
  format: "openai" | "anthropic" | "openai-responses";
  base_url?: string;
  proxy?: string;
  model: string;
  api_key_hint?: string;
  rate_per_second: number;
  rate_per_minute: number;
  context_window_k?: number;
  // 思考のオン/オフ(thinking.type): ""=送らない(既定) | "disabled"=オフ | "enabled"=オン
  thinking_type?: string;
  // 思考の強さ: ""=送らない(既定) | "low"/"medium"/"high"/"xhigh"/"max"
  reasoning_effort?: string;
  is_default: boolean;
  // ローテーションの順位：大きいほど先に選ばれる。有効な設定は常に先頭で、この値とは関係ない。
  priority?: number;
  // true = フェイルオーバーの対象にしない（agent/タスクから明示的に結びつければ使える）。
  pool_exclude?: boolean;
  // true（既定）= ストリーミング(SSE) | false = 本当の非ストリーミング(stream:false、一度にまとめて返す)。
  streaming?: boolean;
  // 1回の返答の出力の上限(token)。0 = このフィールドを送らず、サーバー側の既定値に任せる。
  // context_window_k と区別すること：あちらはモデルの全容量で、ローカルで圧縮のしきい値に使うだけ。
  max_tokens?: number;
  // 上限にどのリクエストのフィールド名を使うか。format="openai" のときだけ意味がある：
  // ""=max_tokens(既定) | "max_completion_tokens"(OpenAI の推論モデルはこれしか受け付けない)
  max_tokens_field?: string;
  // カスタムの会話のヘッダーの名前：空でなければリクエストのたびにこの HTTP ヘッダーを付け、値=いまの会話/意図の session id。
  // ""=送らない。session-id のヘッダーでプロンプトのキャッシュ/固定のルーティングをするゲートウェイ向け。
  session_header_key?: string;
  // この設定による再試行の上書き（接続/空の応答/同じ provider の安全の窓）。空/すべて 0 = 全体の方針に従う。
  retry?: LLMRetryOverride;
}

// ---- LLM の再試行の方針 ----
// 1層の再試行の2つのつまみ。どちらも「0 = 未設定」：
//   attempts    0=既定の回数 | -1=この層の再試行をオフ | >0=再試行の回数
//   interval_ms 0=既定の指数バックオフ | >0=この固定のミリ秒の間隔を使う
export interface LLMRetryRule {
  attempts: number;
  interval_ms: number;
}

// LLM の設定1つが上書きできる3層（どれも「エンドポイントについて回る」再試行）。
export interface LLMRetryOverride {
  connect: LLMRetryRule; // 接続の再試行：接続のリセット/タイムアウト/429/5xx、ストリームが始まる前
  empty: LLMRetryRule; // 空の応答の再試行：完了したのに内容が何もない（openai の形式だけ）
  stream: LLMRetryRule; // 同じ provider の安全の窓の再試行：出力を渡す前にストリームが切れたときの再生
}

// 全体の方針 = 上の3層の既定値 + 全体にしかない2層：
//   breaker ローテーションのサーキットブレーカー（attempts=一時的な失敗が何回続いたら遮断するか、interval_ms=固定の冷却の長さ）
//   intent  意図のやり直し（worker が model_error で終わった後に意図をまるごとやり直す）
export interface LLMRetryPolicy extends LLMRetryOverride {
  breaker: LLMRetryRule;
  intent: LLMRetryRule;
}

// ---- LLM のローテーション（フェイルオーバー）----
// 設定1つのローテーションの連なりでの位置と健全さ。state:
//   ok       正常
//   degraded 続けて失敗しているが遮断のしきい値には達していない
//   tripped  遮断済み。冷却の間は飛ばされる（cooldown_secs は残りの秒数）
export interface LLMPoolMember {
  profile_id: string;
  name: string;
  model: string;
  format: string;
  priority: number;
  active: boolean; // いま有効な設定か（常に先頭）
  excluded: boolean; // pool_exclude：ローテーションに加わらない
  state: "ok" | "degraded" | "tripped";
  fails: number;
  trips: number;
  cooldown_secs: number;
  last_error?: string;
  last_at?: string;
}

export interface LLMPoolStatus {
  enabled: boolean;
  bind_fallback: boolean;
  chain: LLMPoolMember[];
}

// ---- Agents ----
export interface Agent {
  id: string;
  key: string; // 組み込みは goals/planner/mainagent/worker；カスタムはユーザーが決めた key
  name: string;
  description?: string;
  role: string;
  builtin: boolean;
  enabled: boolean;
  llm_profile_id?: number | null; // 結びつけた LLM の設定；null/absent = タスク/会話/全体に従う
  max_turns?: number; // 0 = 無制限
  run_seconds?: number; // worker の1回の実行の実時間の上限(秒)；0 = 無制限
  web_search?: boolean; // Web 検索を有効にするか(システム全体のスイッチで制限される)
  interactive_shell?: boolean; // 対話型 shell(永続する PTY のセッションのツール群)を有効にするか
  // P3 の起動の後処理の方針(カスタム agent だけに意味がある)
  trigger_run_mode?: "serial" | "parallel"; // 順に並べる / 起動ごとにそれぞれ1つの会話を並行して動かす
  trigger_merge_mode?: "by_task" | "all" | "none"; // serial だけ：同じタスクをまとめる / すべてまとめる / まとめない
  trigger_max_parallel?: number; // parallel だけ：agent ごとの並行の上限；0=無制限
  // 結びつけた数(一覧のインターフェースだけが返す)：見える MCP / 見える Skill / 結びつけたツール
  mcp_count?: number;
  skill_count?: number;
  tool_count?: number;
}

export interface PromptVar {
  name: string;
  description: string;
  example: string;
  source: "exploration" | "runtime" | "distilled";
}

export interface PromptVersion {
  version: number;
  ts: string;
  note: string;
  template_text: string;
}

export interface AgentDetail {
  agent: Agent;
  prompt: string;
  variables: PromptVar[];
  versions: PromptVersion[];
  visibility: { mcp: number[]; skill: string[] };
  // 結びつけられる LLM の設定の候補(「既定のモデル」のプルダウン用)；いまの結びつけは agent.llm_profile_id を参照
  llm_profiles?: { id: number; name: string; model: string; is_default: boolean }[];
  wrapup_prompt?: string; // 保存済みの締めくくりのプロンプト(空=組み込みの既定)
  wrapup_default?: string; // 組み込みの既定の締めくくりのプロンプト(置き場の文字/既定に戻す)
  wrapup_max_turns?: number; // 保存済みの締めくくりのターン数(0=組み込みの既定)
  wrapup_max_turns_default?: number; // 組み込みの既定の締めくくりのターン数("0=既定N" の案内用)
  // タスク単位の時間切れの締めくくりの文(worker/planner だけ。task_timeout_wrapup_supported=true のときだけこの区画を表示する)
  task_timeout_wrapup_supported?: boolean;
  task_timeout_wrapup_prompt?: string;
  task_timeout_wrapup_default?: string;
  task_timeout_wrapup_max_turns?: number;
  task_timeout_wrapup_max_turns_default?: number;
}

// ---- MCP ----
export interface MCPServer {
  id: number;
  name: string;
  transport: "stdio" | "http" | "sse";
  command?: string;
  args: string[];
  env: Record<string, string>;
  url?: string;
  enabled: boolean;
  insecure?: boolean; // http: skip TLS cert verification (self-signed servers)
  tools?: string[]; // mcp_tools_cache (names only, for the count)
}

export interface MCPTool {
  name: string;
  description: string;
}

// ---- Skills ----
// Fields align with the agentskills.io open specification.
// description covers both "what the skill does" and "when to use it".
export interface SkillItem {
  name: string; // unique key = directory name
  description?: string; // required per spec; covers what + when to use
  license?: string; // optional: SPDX identifier or free text
  compatibility?: string; // optional: environment requirements
  mcps?: string[]; // MCP server names this skill unlocks on load
  files: string[]; // files in the skill directory
  // 呼び出しの統計（skill_usage の台帳）。一度も呼ばれていない skill：calls=0、last_used は省略。
  calls: number;
  tasks: number; // それを読み込んだタスクの数（chat の会話は数えない）
  usage_agents: string[]; // それを読み込んだ agent の key
  last_used?: string;
}

// SkillCall は Skill() の呼び出し1回（skill 1つの最近の呼び出しの一覧）。
export interface SkillCall {
  ts: string;
  agent_key: string;
  task_id: number; // 0 = タスクでない場面（会話）
  session_id: string;
  args_len: number;
}

// MissingSkill は名指しされたのに存在しない skill —— 「使いたいのにない」という欠け。
export interface MissingSkill {
  skill: string;
  calls: number;
  agents: string[];
  last_used?: string;
}

// ---- Tools (組み込みのツールの目録) ----
// key + handler live in Go; only these fields are page-editable. system tools lock
// the key and the parameter *structure* (name/type/required) — the per-param
// description/default and the agent binding are what move.
export interface Tool {
  key: string;
  system: boolean;
  description: string;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  schema: Record<string, any>; // full JSON-Schema (object with properties)
  agents: string[]; // bound agent keys
  enabled: boolean;
  kind?: "builtin" | "shell" | "command" | "script" | "http"; // カスタムツールの種類
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  exec?: Record<string, any>; // カスタムツールの実行の仕様(kind!=builtin)
  deferred?: boolean; // schema を遅らせる(SearchExtraTools/ExecuteExtraTool)
  calls?: number; // persistent runtime invocation count (older APIs may omit it)
}

// ---- Stats ----
export interface Stats {
  assets: number;
  engine_mode: EngineMode;
  llm_configured: boolean;
  roe_enabled: boolean;
  findings_confirmed: number;
  active_task?: Partial<Task>;
}

// ---- Intercept Rules ----
export type InterceptAction = "allow" | "deny" | "ask";
export type InterceptMatchTarget = "tool_name" | "tool_input";
export type InterceptMatchType = "string" | "regex";

export interface InterceptRule {
  id: number;
  name: string;
  enabled: boolean;
  priority: number;
  match_target: InterceptMatchTarget;
  match_type: InterceptMatchType;
  pattern: string;
  action: InterceptAction;
  message: string;
  timeout_enabled: boolean;
  timeout_seconds: number;
  timeout_action: "deny" | "allow";
  created_at: string;
  updated_at: string;
}

// ---- Asset Intercept Rules（資産の遮断：全体のブラックリスト） ----
export type AssetInterceptKind =
  | "exact_domain"
  | "exact_ip"
  | "exact_url"
  | "fuzzy_domain"
  | "fuzzy_ip"
  | "fuzzy_url"
  | "cidr";

// action はタスク単位の規則だけに使う：block=遮断(テストを禁止) allow=許可(ホワイトリスト)。
export type AssetInterceptAction = "block" | "allow";

// タスク単位の資産の遮断/許可の規則の入力項目（タスクの作成、タスクの詳細の編集で使う）。
export interface AssetInterceptRuleInput {
  action: AssetInterceptAction;
  kind: AssetInterceptKind;
  pattern: string;
  note: string;
  enabled: boolean;
}

export interface AssetInterceptRule {
  id: number;
  enabled: boolean;
  action?: AssetInterceptAction; // 全体の規則はこのフィールドを持たない（常に遮断）；タスク単位の規則は block/allow を区別する
  kind: AssetInterceptKind;
  pattern: string;
  note: string;
  builtin: boolean;
  created_at: string;
  updated_at: string;
}

export interface InterceptPending {
  decision_source?: "rule" | "model" | "unknown" | "";
  id: number;
  rule_id?: number;
  conversation_id?: number;
  task_id?: string;
  agent_name: string;
  tool_name: string;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  tool_input: Record<string, any>;
  status: "pending" | "allowed" | "denied" | "timeout";
  reason: string; // 規則の message かモデルの判定の理由(モデルの判定には [模型] の接頭辞が付く)
  decided_at?: string;
  created_at: string;
}

// JudgeConfig: モデルを最後の砦にした承認(遮断の規則に1つも当たらないときだけモデルが判断する)の全体の設定。
export interface JudgeConfig {
  enabled: boolean;
  profile_id: number; // 0 = 有効/既定の設定に従う
  prompt: string; // 判定のプロンプト;GET で未設定のときはバックエンドが組み込みのテンプレートの全文を返す
  timeout_seconds: number; // モデルの呼び出しのタイムアウト
  fail_action: "allow" | "ask" | "deny"; // モデルがエラー/時間切れ/解析できないときの戻り先
  ask_timeout_seconds: number; // モデルが ask と判定して人に回した後の承認の待ちのタイムアウト
  ask_timeout_action: "allow" | "deny"; // 承認が時間切れになった後の既定の動作
}

// JudgeUsage: 모델 판정(대체 승인, judge 레인)의 누적 token 사용량과 최근 N일 일별 시계열.
export interface JudgeDayUsage {
  date: string; // YYYY-MM-DD (UTC)
  calls: number;
  input_tokens: number;
  output_tokens: number;
}
export interface JudgeUsage {
  calls: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  daily: JudgeDayUsage[];
}

export interface InterceptApprovalFilter {
  status?: InterceptPending["status"];
  decision_source?: "rule" | "model" | "unknown";
}

// InterceptApprovalRow enriches InterceptPending with conversation/task and rule context.
export interface InterceptApprovalRow extends InterceptPending {
  conv_title: string; // "" if no linked conversation
  conv_agent_key: string; // "" if no linked conversation
  rule_name: string; // "" if rule was deleted
}

// ── 資産の同期 (ScopeSentry のデータ源) ──────────────────────────────────────────────
export interface SSProject {
  id: string; // MongoDB ObjectID — used as filter.project
  name: string;
  logo?: string;
  AssetCount?: number;
  tag?: string;
}

export interface SSTask {
  id: string;
  name: string; // used as filter.task
  status?: number;
  progress?: number;
  creatTime?: string;
  endTime?: string;
}

// ConvTokenSummary — one conversation's token total (+ profile/date) for merging
// chat usage into the dashboard token stats. GET /api/tokens/conversations.
export interface ConvTokenSummary {
  llm_profile_id: number | null;
  created_at: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// ---- Command recording (Bash execution history) ----
export interface CommandRecord {
  id: number;
  exploration_id: number;
  worker: string;
  tool: string;
  command: string; // raw tool input (JSON)
  output: string;
  is_error: boolean;
  created_at: string;
}

// ツール1つの呼び出しの統計（/commands/stats）；errors はそのうちの失敗の回数。
export interface ToolStat {
  tool: string;
  total: number;
  errors: number;
}

// ---- LLM recording ----
export interface LLMRecordItem {
  id: number;
  ts: string;
  model: string;
  profile_name: string;
  session_id: string;
  task_id: string;
  worker: string;
  latency_ms: number;
  input_tokens: number;
  output_tokens: number;
  cache_read: number;
  cache_write: number;
  status: string;
  error?: string;
}

export interface LLMRecordDetail extends LLMRecordItem {
  request_body: string;
  response_body: string;
  // provider が実際にやり取りした HTTP の原文：リクエストは buildBody() が送った完全な body（ツールの
  // schema を含む）、応答は元の SSE のフレーム。上の request_body/response_body はそろえた見え方で、
  // ツールの schema と tool_use のブロックを捨てている。古い記録では空。
  raw_request?: string;
  raw_response?: string;
}

// One distinct task with its LLM-record count (task picker on the records page).
export interface LLMTask {
  task_id: string;
  count: number;
}

// The exact JSON sent to the review model, retained for all model verdicts.
export interface InterceptReviewInput {
  version: number;
  background?: {
    // worker_summary is retained only for immutable v2/v3 snapshots.
    source: "user_message" | "worker_summary";
    text: string;
    truncated?: boolean;
  };
  // Version 1 snapshots are immutable and remain readable in historical audits.
  task?: {
    task_id: number;
    description: string;
    goal: string;
    constraints: { id: number; kind: string; text: string; origin: string; created_at: number }[];
    truncated?: boolean;
  };
  working_directory?: string;
  worker_intent?: string;
  turn_input?: string;
  background_truncated?: boolean;
  // Legacy v1/v2 snapshots only; v3 never sends execution history.
  history?: {
    tool_use_id: string;
    tool: string;
    arguments_preview: string;
    result: string;
    status: "succeeded" | "failed";
    truncated?: boolean;
  }[];
  history_truncated?: boolean;
  correlation?: "exact" | "ambiguous" | "unavailable";
  tool_name: string;
  arguments: Record<string, unknown>;
}

// Immutable review snapshot plus separately recorded execution outcome.
export interface InterceptAudit {
  model_input?: InterceptReviewInput;
  model_input_digest?: string;
  run_id?: string;
  tool_use_id?: string;
  correlation: "exact" | "ambiguous" | "unavailable";
  input_digest: string;
  user_message: string;
  user_truncated?: boolean;
  context:
    | { kind: string; tool?: string; tool_use_id?: string; text: string; is_error?: boolean; truncated?: boolean }[]
    | null;
  context_truncated?: boolean;
  captured_at: string;
  model_fallback?: boolean;
  initial_action: "allow" | "ask" | "deny";
  initial_reason: string;
  effective_action?: "allow" | "deny";
  decision_reason?: string;
  rule_name?: string;
  config_digest?: string;
  profile_id?: number;
  execution_status: "not_started" | "not_executed" | "awaiting_result" | "succeeded" | "failed" | "unknown";
  output?: string;
  output_truncated?: boolean;
  execution_ended_at?: string;
}
export interface InterceptDetail extends InterceptApprovalRow {
  audit: InterceptAudit | null;
}

export type TrafficEvidenceRole = "baseline" | "proof" | "verification" | "supporting";
export interface TrafficEvidenceRef {
  traffic_id: string;
  role?: TrafficEvidenceRole;
  note?: string;
}
export interface TrafficEvidenceSnapshot {
  id: string;
  source_traffic_id: string;
  captured_at: number;
  url: string;
  method: string;
  status: number;
  content_type: string;
  req_head?: string;
  resp_head?: string;
  req_hash: string;
  resp_hash: string;
  req_len: number;
  resp_len: number;
}
export interface FindingTrafficBinding {
  id: string;
  finding_id: string;
  snapshot_id: string;
  role: TrafficEvidenceRole;
  note: string;
  position: number;
  created_at: string;
  snapshot: TrafficEvidenceSnapshot;
}
export interface FindingTraffic {
  finding_id: string;
  version: number;
  report_version: number;
  bindings: FindingTrafficBinding[];
}
export interface EvidenceBodyPreview {
  content: string;
  offset: number;
  total: number;
  next_offset: number;
  truncated: boolean;
  binary: boolean;
}
export interface FindingTrafficDetail {
  binding: FindingTrafficBinding;
  request: EvidenceBodyPreview;
  response: EvidenceBodyPreview;
}

/** GET /api/update/check —— いまの版と GitHub の最新の正式版を比べた結果。 */
export interface UpdateCheck {
  /** いま動いている版；開発用のビルドでは "dev" か git describe の接尾辞付きの形。 */
  current: string;
  /** 動かし方。docker では入れ替えはコンテナの書き込める層にだけ効き、コンテナを作り直すとイメージの版に戻る。 */
  mode: "docker" | "binary";
  os: string;
  arch: string;
  repo: string;
  /** 戻せる前の版（artex.old）があるか。 */
  has_backup: boolean;
  /** 今回の起動での自己更新の立ち上げの結論（入れ替えの失敗 / ロールバック済みなど）。何もなければ空。 */
  boot_notice?: string;
  rolled_back?: boolean;
  /** GitHub への問い合わせに失敗したときの理由。このときは下のフィールドはどれもない。 */
  error?: string;
  latest?: string;
  notes?: string;
  html_url?: string;
  published_at?: string;
  /** いまのプラットフォームに対応する配布物の名前と、その Release が本当にそれを持っているか。 */
  asset?: string;
  asset_available?: boolean;
  size?: number;
  has_update?: boolean;
  /** 双方の版の番号を比べられるか；開発用のビルドでは false で、このときワンクリックの更新は使えない。 */
  comparable?: boolean;
  /** comparable が false のときの説明。 */
  reason?: string;
}

/** /api/update/stream が送る更新の進み具合1件。 */
export interface UpdateProgress {
  phase: "idle" | "downloading" | "verifying" | "extracting" | "staged" | "failed";
  /** ダウンロードの段階だけで意味がある（0-100）；ほかの段階では -1。 */
  percent: number;
  message: string;
  version?: string;
  error?: string;
}

// Original execution selected from an approval, never submitted to the reviewer.
export interface InterceptExecution {
  conversation_id: number | null;
  task_id: string | null;
  session: string;
  seq: number;
  items: Activity[];
}
