import { activeMention, mentionSearch, mentionToken, selectedMentions } from "./chat-mentions.ts";
import assert from "node:assert/strict";
import test from "node:test";

test("mention trigger supports Japanese and cursor placement without hijacking email", () => {
  assert.equal(activeMention("user@example.com", 16), null);
  assert.equal(activeMention("選択済み @[脆弱性#1 X]", 14), null);
  assert.deepEqual(activeMention("確認@脆弱性 の後ろの文", 6), { start: 2, end: 6, query: "脆弱性" });
  assert.equal(activeMention("@脆弱性\n次の行", 9), null);
});

test("categories, Japanese and legacy Chinese aliases, IP and keyword search", () => {
  assert.equal(mentionSearch("").categories.length, 9);
  assert.equal(mentionSearch("脆").categories[0].kind, "finding");
  assert.equal(mentionSearch("脆弱性").kind, "finding");
  assert.equal(mentionSearch("脆弱性SQLインジェクション").query, "SQLインジェクション");
  assert.equal(mentionSearch("漏洞").kind, "finding");
  assert.equal(mentionSearch("ip 192.0.2.1").kind, "ip");
  assert.equal(mentionSearch("エンドポイント GET /api").query, "GET /api");
  assert.equal(mentionSearch("acme.com").kind, "");
});

test("tokens roundtrip labels and removing one reference preserves its neighbors", () => {
  const first = mentionToken({ kind: "finding", id: 12, label: "タイトル[1]\n説明" });
  const second = mentionToken({ kind: "ip", id: 13, label: "192.0.2.1" });
  assert.equal(first, "@[脆弱性#12 タイトル（1） 説明]");
  const value = `分析 ${first} と ${second}`;
  const selected = selectedMentions(value);
  assert.equal(selected.length, 2);
  assert.equal(selected[0].kind, "finding");
  assert.equal(selected[0].id, "12");
  assert.equal(selected[0].text, "タイトル（1） 説明");
  assert.equal(selected[1].kind, "ip");
  const next = value.slice(0, selected[0].start) + value.slice(selected[0].start + selected[0].token.length);
  assert.equal(selectedMentions(next)[0].token, second);
});

test("legacy Chinese tokens still parse", () => {
  const selected = selectedMentions("@[漏洞#7 旧] @[企业#8] @[子域名#9 a.example.com]");
  assert.deepEqual(
    selected.map((item) => [item.kind, item.id]),
    [
      ["finding", "7"],
      ["company", "8"],
      ["subdomain", "9"],
    ],
  );
});
