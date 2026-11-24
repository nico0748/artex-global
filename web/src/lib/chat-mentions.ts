export const mentionKinds = [
  { kind: "finding", label: "脆弱性", legacy: "漏洞", alias: "finding" },
  { kind: "asset", label: "資産", legacy: "资产", alias: "asset" },
  { kind: "company", label: "企業", legacy: "企业", alias: "company" },
  { kind: "endpoint", label: "エンドポイント", legacy: "接口", alias: "api" },
  { kind: "ip", label: "IP", legacy: "IP", alias: "ip" },
  { kind: "app", label: "アプリ", legacy: "应用", alias: "app" },
  { kind: "root_domain", label: "ドメイン", legacy: "域名", alias: "domain" },
  { kind: "subdomain", label: "サブドメイン", legacy: "子域名", alias: "subdomain" },
  { kind: "service", label: "サービス", legacy: "服务", alias: "service" },
] as const;

export type MentionKind = (typeof mentionKinds)[number]["kind"];
export interface ChatMention {
  kind: MentionKind;
  id: number;
  label: string;
  description: string;
}

export function activeMention(value: string, caret: number) {
  const before = value.slice(0, caret);
  const start = before.lastIndexOf("@");
  if (start < 0 || (start > 0 && /[\w.+/-]/.test(before[start - 1]))) return null;
  const query = before.slice(start + 1);
  if (/[[\]\r\n@]/.test(query) || query.length > 220) return null;
  return { start, end: caret, query };
}

export function mentionSearch(query: string) {
  const text = query.trimStart().toLowerCase();
  for (const item of mentionKinds) {
    for (const alias of [item.label.toLowerCase(), item.legacy.toLowerCase(), item.alias]) {
      if (text === alias || text.startsWith(`${alias} `) || (/[^a-z]/.test(alias) && text.startsWith(alias))) {
        return { kind: item.kind, query: query.trimStart().slice(alias.length).trim(), categories: [] };
      }
    }
  }
  const categories = mentionKinds.filter(
    (item) => item.label.toLowerCase().startsWith(text) || item.alias.startsWith(text),
  );
  return { kind: "" as const, query: query.trim(), categories };
}

export function mentionToken(item: ChatMention) {
  const kind = mentionKinds.find((entry) => entry.kind === item.kind)?.label ?? "資産";
  const label = item.label
    .replace(/[[\]]/g, (char) => (char === "[" ? "（" : "）"))
    .replace(/\s+/g, " ")
    .slice(0, 100);
  return `@[${kind}#${item.id} ${label}]`;
}

export interface SelectedMention {
  token: string;
  kind: MentionKind;
  id: string;
  text: string;
  start: number;
}

// 토큰의 분류 라벨은 백엔드 와이어 포맷이다(server/chat_mentions.go 의 chatMentionPattern 과 동일).
// 새 토큰은 일본어 라벨(脆弱性·資産…)로 쓰고, 예전 중국어 라벨(漏洞·资产…)도 계속 읽는다.
// 화면에 보이는 분류명은 여기서 영어 kind 로 되돌려 주고, 소비 컴포넌트가 mentionTextarea.kind 로 현지화한다.
export function selectedMentions(value: string): SelectedMention[] {
  return [
    ...value.matchAll(
      /@\[(脆弱性|資産|企業|エンドポイント|IP|アプリ|ドメイン|サブドメイン|サービス|漏洞|资产|企业|接口|应用|域名|子域名|服务)#([0-9]+)(?: ([^\]\r\n]*))?\]/g,
    ),
  ].map((match) => ({
    token: match[0],
    kind: mentionKinds.find((entry) => entry.label === match[1] || entry.legacy === match[1])?.kind ?? "asset",
    id: match[2],
    text: match[3] ?? "",
    start: match.index,
  }));
}
