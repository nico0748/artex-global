// チャンネルのフィールドの表と設定の値の解析の道具。
//
// ページと分けたのは、これが表示ではなく**データ**だから：チャンネルの種類ごとにどんなフィールドがあり、
// それぞれどの部品を使うか、そしてフォームの文字と設定の値（JSON）の双方向の変換を表す。
// 1つのファイルに分けておけば、チャンネルを足すときはここだけを変えればよく、ページそのものは変えなくてよい。
//
// このファイルは純粋なデータのモジュール（client component ではない）なので、useTranslations を呼べない。
// だから文面はここに決め打ちせず i18n のキー（notifyPage の名前空間）を持ち、それを描く
// 部品（page.tsx / channel-form.tsx）が t() で解決する——キーは ASCII で、上流の中国語の原文は
// messages/zh.json に残す。チャンネルの種類の表示名は notifyPage.kind.<kind> を通す。

// 各チャンネルの設定のフィールドの定義。
//
// ここではわざとフロントエンドのフィールドの表を1つ持ち、バックエンドに schema を送らせない：バックエンドは
// Validate（必須/形式）だけを受け持ち、UI に要るのは配置と部品の種類で、両者が気にすることは同じではない。
// 唯一の結びつきは secret_keys —— どのフィールドをパスワード欄で描くかはバックエンドが与える。
// どの値が資格情報にあたるかはチャンネルの実装自身しか知らないから（WeCom は Webhook 全体が資格情報で、
// DingTalk はそのうちの secret 1つだけ）。チャンネルを足すときにここに項目が1つ欠けてもフォームが空白になるだけで、
// 黙って誤ることはない（下の hasFields が知らせる）。
//
// label / help / options[].label には notifyPage の名前空間の i18n のキーを入れ、
// placeholder は文字どおりの値（URL、見本。訳さない）を残す。
export type FieldKind = "text" | "password" | "number" | "select" | "textarea" | "switch" | "kv" | "list";
export interface FieldDef {
  key: string;
  /** notifyPage の名前空間の i18n のキー */
  label: string;
  kind: FieldKind;
  placeholder?: string;
  /** notifyPage の名前空間の i18n のキー */
  help?: string;
  /** options[].label も i18n のキー */
  options?: { value: string; label: string }[];
}
export const CHANNEL_FIELDS: Record<string, FieldDef[]> = {
  dingtalk: [
    {
      key: "webhook",
      label: "field.dingtalk.webhook.label",
      kind: "text",
      placeholder: "https://oapi.dingtalk.com/robot/send?access_token=...",
    },
    {
      key: "secret",
      label: "field.dingtalk.secret.label",
      kind: "password",
      help: "field.dingtalk.secret.help",
    },
  ],
  feishu: [
    {
      key: "webhook",
      label: "field.feishu.webhook.label",
      kind: "text",
      placeholder: "https://open.feishu.cn/open-apis/bot/v2/hook/...",
    },
    { key: "secret", label: "field.feishu.secret.label", kind: "password", help: "field.feishu.secret.help" },
  ],
  wecom: [
    {
      key: "webhook",
      label: "field.wecom.webhook.label",
      kind: "text",
      placeholder: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=...",
    },
  ],
  webhook: [
    {
      key: "url",
      label: "field.webhook.url.label",
      kind: "text",
      placeholder: "https://your-endpoint.example.com/hook",
    },
    {
      key: "method",
      label: "field.webhook.method.label",
      kind: "select",
      options: [
        { value: "POST", label: "field.webhook.method.opt.POST" },
        { value: "PUT", label: "field.webhook.method.opt.PUT" },
        { value: "PATCH", label: "field.webhook.method.opt.PATCH" },
        { value: "GET", label: "field.webhook.method.opt.GET" },
      ],
    },
    { key: "headers", label: "field.webhook.headers.label", kind: "kv", help: "field.webhook.headers.help" },
    {
      key: "body_template",
      label: "field.webhook.body_template.label",
      kind: "textarea",
      help: "field.webhook.body_template.help",
    },
  ],
  telegram: [
    { key: "bot_token", label: "field.telegram.bot_token.label", kind: "password", placeholder: "123456:ABC-DEF..." },
    { key: "chat_id", label: "field.telegram.chat_id.label", kind: "text", placeholder: "-1001234567890" },
    {
      key: "base_url",
      label: "field.telegram.base_url.label",
      kind: "text",
      placeholder: "https://api.telegram.org",
      help: "field.telegram.base_url.help",
    },
  ],
  email: [
    { key: "host", label: "field.email.host.label", kind: "text", placeholder: "smtp.example.com" },
    {
      key: "port",
      label: "field.email.port.label",
      kind: "number",
      placeholder: "587",
      help: "field.email.port.help",
    },
    { key: "username", label: "field.email.username.label", kind: "text" },
    { key: "password", label: "field.email.password.label", kind: "password" },
    { key: "from", label: "field.email.from.label", kind: "text", placeholder: "artex@example.com" },
    { key: "to", label: "field.email.to.label", kind: "list", help: "field.email.to.help" },
    { key: "tls", label: "field.email.tls.label", kind: "switch", help: "field.email.tls.help" },
  ],
};

// value は絞り込みに使う min_severity の値、label は notifyPage.severityOpt.* の i18n のキー。
export const SEVERITY_OPTIONS = [
  { value: "", label: "severityOpt.all" },
  { value: "low", label: "severityOpt.low" },
  { value: "medium", label: "severityOpt.medium" },
  { value: "high", label: "severityOpt.high" },
  { value: "critical", label: "severityOpt.critical" },
];

export type ChannelForm = {
  name: string;
  kind: string;
  mode: "realtime" | "digest";
  enabled: boolean;
  ratePerMin: string;
  config: Record<string, unknown>;
  minSeverity: string;
  includeText: string;
  excludeText: string;
  taskIDsText: string;
  assetIDsText: string;
  onStatusChange: boolean;
};

export const emptyForm = (kind: string): ChannelForm => ({
  name: "",
  kind,
  mode: "realtime",
  enabled: true,
  ratePerMin: "",
  config: {},
  minSeverity: "",
  includeText: "",
  excludeText: "",
  taskIDsText: "",
  assetIDsText: "",
  onStatusChange: false,
});

// parseKV は「1行ごとに KEY=VALUE」のテキストエリアを解析する。
export function parseKV(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of text.split("\n")) {
    const t = line.trim();
    if (!t) continue;
    const i = t.indexOf("=");
    if (i > 0) out[t.slice(0, i).trim()] = t.slice(i + 1).trim();
  }
  return out;
}
// parseIDs はカンマ/空白で区切った id の一覧を解析する。区切りには全角のカンマ「，」も残す：
// ユーザーはよそから全角のカンマの付いた一覧を貼り付けることがあり、これは入力の解析のロジックで、表示の文面ではない。
export function parseIDs(text: string): number[] {
  return text
    .split(/[\s,，]+/)
    .map((s) => s.trim())
    .filter(Boolean)
    .map((s) => Number(s))
    .filter((n) => Number.isFinite(n) && n > 0);
}
// parseKeywords は行/カンマで区切ったキーワードの一覧を解析する（脆弱性の種類の名前は空白を含むことがあるので、行かカンマで切る）。
// 同じく全角のカンマ「，」を区切りとして残す（入力の解析で、表示ではない）。
export function parseKeywords(text: string): string[] {
  return text
    .split(/[\n,，]+/)
    .map((s) => s.trim())
    .filter(Boolean);
}
