"use client";

import * as React from "react";

import type { Graph as G6Graph } from "@antv/g6";
import { AppWindow, Building2, Globe, Link2, type LucideIcon, Radio, RefreshCw, Server, Waypoints } from "lucide-react";
import { useTranslations } from "next-intl";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Sheet, SheetContent, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { api } from "@/lib/api";
import type { CoverageAssetRef, CoverageAssetRefs, CoverageGraphEdge, CoverageGraphNode } from "@/lib/types";
import { cn } from "@/lib/utils";

// 親のノードごと、同じ種類の子のノードを既定で表示する数；超えたら折りたたみ、"もっと見る"のたびにこの数だけ足す。
const FOLD_LIMIT = 20;
const FOLD_STEP = 20;

type Kind = CoverageGraphNode["kind"];

// label 은 더 이상 여기 두지 않는다. 노드 유형 이름은 화면에 노출되므로
// `taskDetail.coverageGraph.kind.*` 에서 번역해 가져온다(캔버스 라벨 포함).
type KindMeta = { icon: LucideIcon; iconBg: string; hex: string; size: number };

const kindMeta: Record<Kind, KindMeta> = {
  company: { icon: Building2, iconBg: "bg-slate-500", hex: "#64748b", size: 46 },
  root_domain: { icon: Globe, iconBg: "bg-indigo-500", hex: "#6366f1", size: 38 },
  subdomain: { icon: Waypoints, iconBg: "bg-blue-500", hex: "#3b82f6", size: 30 },
  ip: { icon: Server, iconBg: "bg-cyan-600", hex: "#0891b2", size: 28 },
  service: { icon: Radio, iconBg: "bg-amber-500", hex: "#f59e0b", size: 26 },
  app: { icon: AppWindow, iconBg: "bg-fuchsia-500", hex: "#d946ef", size: 26 },
  endpoint: { icon: Link2, iconBg: "bg-rose-500", hex: "#f43f5e", size: 20 },
};

// G6 のノードのアイコンはプラットフォームでそろえた lucide のアイコンを使う：lucide の SVG のパス（v1.22）を白い輪郭の
// data URI にし、ノードの iconSrc にする（白は塗りの色/灰色の地のどちらでもくっきり見える）。手で埋め込み、react-dom/server が
// React19/Next のクライアントのバンドルで起こす問題を避ける。
function svgUri(inner: string, filled = false): string {
  const attrs = filled
    ? 'fill="#fff" stroke="none"'
    : 'fill="none" stroke="#fff" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"';
  const svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" ${attrs}>${inner}</svg>`;
  return `data:image/svg+xml,${encodeURIComponent(svg)}`;
}

const KIND_ICON: Record<Kind, string> = {
  company: svgUri(
    '<path d="M10 12h4"/><path d="M10 8h4"/><path d="M14 21v-3a2 2 0 0 0-4 0v3"/><path d="M6 10H4a2 2 0 0 0-2 2v7a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2V9a2 2 0 0 0-2-2h-2"/><path d="M6 21V5a2 2 0 0 1 2-2h8a2 2 0 0 1 2 2v16"/>',
  ),
  root_domain: svgUri(
    '<circle cx="12" cy="12" r="10"/><path d="M12 2a14.5 14.5 0 0 0 0 20 14.5 14.5 0 0 0 0-20"/><path d="M2 12h20"/>',
  ),
  subdomain: svgUri(
    '<path d="m10.586 5.414-5.172 5.172"/><path d="m18.586 13.414-5.172 5.172"/><path d="M6 12h12"/><circle cx="12" cy="20" r="2"/><circle cx="12" cy="4" r="2"/><circle cx="20" cy="12" r="2"/><circle cx="4" cy="12" r="2"/>',
  ),
  ip: svgUri(
    '<rect width="20" height="8" x="2" y="2" rx="2" ry="2"/><rect width="20" height="8" x="2" y="14" rx="2" ry="2"/><line x1="6" x2="6.01" y1="6" y2="6"/><line x1="6" x2="6.01" y1="18" y2="18"/>',
  ),
  service: svgUri(
    '<path d="M16.247 7.761a6 6 0 0 1 0 8.478"/><path d="M19.075 4.933a10 10 0 0 1 0 14.134"/><path d="M4.925 19.067a10 10 0 0 1 0-14.134"/><path d="M7.753 16.239a6 6 0 0 1 0-8.478"/><circle cx="12" cy="12" r="2"/>',
  ),
  app: svgUri(
    '<rect x="2" y="4" width="20" height="16" rx="2"/><path d="M10 4v4"/><path d="M2 8h20"/><path d="M6 4v4"/>',
  ),
  endpoint: svgUri(
    '<path d="M9 17H7A5 5 0 0 1 7 7h2"/><path d="M15 7h2a5 5 0 1 1 0 10h-2"/><line x1="8" x2="16" y1="12" y2="12"/>',
  ),
};

const FOLD_ICON = svgUri(
  '<circle cx="5" cy="12" r="1.6"/><circle cx="12" cy="12" r="1.6"/><circle cx="19" cy="12" r="1.6"/>',
  true,
);

// ---------------------------------------------------------------------------
// Folding: full graph → currently-visible node/edge set (上から下へ連鎖して折りたたむ)。
// ---------------------------------------------------------------------------
type FoldNode = {
  fold: true;
  key: string;
  groupId: string;
  parentKey: string;
  kind: Kind;
  hidden: CoverageGraphNode[];
};
type AssetRenderNode = { fold: false; key: string; kind: Kind; node: CoverageGraphNode };
type RenderNode = AssetRenderNode | FoldNode;

function sortChildren(a: CoverageGraphNode, b: CoverageGraphNode): number {
  if (a.tested !== b.tested) return a.tested ? -1 : 1;
  return (a.label || "").localeCompare(b.label || "");
}

function computeVisible(
  nodes: CoverageGraphNode[],
  edges: CoverageGraphEdge[],
  expanded: Map<string, number>,
): { renderNodes: RenderNode[]; renderEdges: CoverageGraphEdge[] } {
  const byKey = new Map(nodes.map((n) => [n.key, n]));
  const parentOf = new Map<string, string>();
  const childrenOf = new Map<string, string[]>();
  for (const e of edges) {
    if (!byKey.has(e.src) || !byKey.has(e.dst)) continue;
    if (!parentOf.has(e.src)) parentOf.set(e.src, e.dst);
    const arr = childrenOf.get(e.dst);
    if (arr) arr.push(e.src);
    else childrenOf.set(e.dst, [e.src]);
  }

  const renderNodes: RenderNode[] = [];
  const visible = new Set<string>();
  const foldNodes: FoldNode[] = [];

  const queue: string[] = [];
  for (const n of nodes) {
    if (!parentOf.has(n.key)) {
      queue.push(n.key);
      visible.add(n.key);
    }
  }

  for (let qi = 0; qi < queue.length; qi++) {
    const parent = queue[qi];
    const kids = childrenOf.get(parent) ?? [];
    if (kids.length === 0) continue;
    const groups = new Map<Kind, CoverageGraphNode[]>();
    for (const ck of kids) {
      const cn = byKey.get(ck);
      if (!cn) continue;
      const g = groups.get(cn.kind);
      if (g) g.push(cn);
      else groups.set(cn.kind, [cn]);
    }
    for (const [kind, arr] of groups) {
      arr.sort(sortChildren);
      const groupId = `${parent}::${kind}`;
      const shown = expanded.get(groupId) ?? FOLD_LIMIT;
      const visibleChildren = arr.slice(0, shown);
      const hidden = arr.slice(shown);
      for (const c of visibleChildren) {
        if (!visible.has(c.key)) {
          visible.add(c.key);
          queue.push(c.key);
        }
      }
      if (hidden.length > 0) {
        foldNodes.push({ fold: true, key: `fold:${groupId}`, groupId, parentKey: parent, kind, hidden });
      }
    }
  }

  for (const n of nodes) {
    if (visible.has(n.key)) renderNodes.push({ fold: false, key: n.key, kind: n.kind, node: n });
  }
  const renderEdges: CoverageGraphEdge[] = edges.filter((e) => visible.has(e.src) && visible.has(e.dst));
  for (const f of foldNodes) {
    renderNodes.push(f);
    renderEdges.push({ src: f.key, dst: f.parentKey });
  }
  return { renderNodes, renderEdges };
}

// ---------------------------------------------------------------------------
// G6 のデータの対応づけ。カスタムのフィールドはノードの最上位に置く（G6 v5 の公式の force の見本の約束：style/layout のコールバックが d.<field> を直接読む）。
// ---------------------------------------------------------------------------
type G6NodeDatum = {
  id: string;
  kind: Kind;
  fold: boolean;
  tested: boolean;
  inScope: boolean;
  lbl: string;
  size: number;
};

function trunc(s: string, n = 26): string {
  return s.length > n ? `${s.slice(0, n - 1)}…` : s;
}

// 図のノードの文面：サービスは完全な URL を出さず、ポート·タイトル·状態コードだけを出す；エンドポイントは path だけを出す。
// ほかの種類はバックエンドが与えた label をそのまま使う。
function graphLabel(n: CoverageGraphNode): string {
  if (n.kind === "service") {
    const parts: string[] = [];
    if (n.port) parts.push(`:${n.port}`);
    if (n.page_title) parts.push(n.page_title);
    if (n.status_code) parts.push(String(n.status_code));
    // biome-ignore lint/nursery/useNullishCoalescing: 표시용 폴백 — 빈 문자열도 다음 후보(ip·label)로 넘겨야 하므로 || 가 의도된 동작이다(?? 는 빈 문자열을 그대로 노출한다)
    return parts.length ? parts.join(" · ") : n.domain || n.ip || n.label;
  }
  if (n.kind === "endpoint" && n.url) {
    try {
      return new URL(n.url).pathname || "/";
    } catch {
      /* 不正な URL：完全な label に戻る */
    }
  }
  return n.label;
}

function toG6Nodes(
  renderNodes: RenderNode[],
  kindLabel: (k: Kind) => string,
  moreLabel: (count: number, k: Kind) => string,
): G6NodeDatum[] {
  return renderNodes.map((rn) => {
    if (rn.fold) {
      return {
        id: rn.key,
        kind: rn.kind,
        fold: true,
        tested: false,
        inScope: false,
        lbl: moreLabel(rn.hidden.length, rn.kind),
        size: 24,
      };
    }
    return {
      id: rn.key,
      kind: rn.kind,
      fold: false,
      tested: rn.node.tested,
      inScope: rn.node.in_scope,
      lbl: trunc(graphLabel(rn.node) || kindLabel(rn.kind)),
      size: kindMeta[rn.kind].size,
    };
  });
}

// G6 の型はカスタムのフィールドを data の下に置くが、公式の force の見本（と実行時）は最上位の d.<field> を読む。
// コールバックの引数は unknown にして G6 のシグネチャを満たし、中で nd() を使って最上位の構造に戻す。
const nd = (d: unknown) => d as G6NodeDatum;

// テスト済み=塗りの色で強調；範囲の中でテストしていない=灰色；範囲の外/折りたたみ=もっと薄い灰色 + 破線の輪郭。
function nodeFill(d: G6NodeDatum): string {
  if (d.fold) return "#f1f5f9";
  if (!d.inScope) return "#e2e8f0";
  if (d.tested) return kindMeta[d.kind].hex;
  return "#94a3b8";
}
function nodeStroke(d: G6NodeDatum): string {
  if (d.fold || !d.inScope) return "#94a3b8";
  if (d.tested) return "#0f766e";
  return "#64748b";
}

// ---------------------------------------------------------------------------
// 引き出し：資産のノードは詳細を見る；折りたたみのノードは隠れた一覧 + "もっと見る"。
// ---------------------------------------------------------------------------
function DetailRow({ label, children }: { label: string; children: React.ReactNode }) {
  if (children === undefined || children === null || children === "") return null;
  return (
    <div className="flex items-start gap-3 py-1 text-sm">
      <span className="text-muted-foreground w-16 shrink-0">{label}</span>
      <span className="text-foreground min-w-0 flex-1 break-words">{children}</span>
    </div>
  );
}

function RefList({ title, items }: { title: string; items: CoverageAssetRef[] }) {
  const t = useTranslations("taskDetail.coverageGraph");
  if (items.length === 0) return null;
  return (
    <div>
      <h4 className="text-muted-foreground mb-1 text-xs font-medium">
        {title} ({items.length})
      </h4>
      <div className="flex flex-col gap-1">
        {items.map((r) => (
          <div
            key={`${r.kind}-${r.id}`}
            className="flex flex-wrap items-start gap-2 rounded-md bg-muted/50 px-2 py-1.5 text-xs"
          >
            <span className="shrink-0 font-mono text-muted-foreground">#{r.id}</span>
            {r.state && <span className="shrink-0 text-muted-foreground">{r.state}</span>}
            <span className="min-w-32 flex-1 break-words text-foreground">{r.summary || "—"}</span>
            {r.inherited && r.source_task_id && (
              <Badge variant="outline" className="shrink-0">
                {t("sheet.refSource", { id: r.source_task_id })}
              </Badge>
            )}
          </div>
        ))}
      </div>
    </div>
  );
}

function AssetSheet({
  node,
  taskId,
  onOpenChange,
}: {
  node: CoverageGraphNode | null;
  taskId: string;
  onOpenChange: (open: boolean) => void;
}) {
  const t = useTranslations("taskDetail.coverageGraph");
  const meta = node ? kindMeta[node.kind] : null;
  const Icon = meta?.icon;
  const kindLabel = node ? t(`kind.${node.kind}`) : "";
  const raw = node ? JSON.stringify(node, null, 2) : "";
  const [refs, setRefs] = React.useState<CoverageAssetRefs | null>(null);

  React.useEffect(() => {
    setRefs(null);
    if (!node?.asset_id) return;
    let cancelled = false;
    api
      .taskAssetRefs(taskId, node.asset_id)
      .then((r) => {
        if (!cancelled) setRefs(r);
      })
      .catch(() => {
        /* 関連がないかエラー：この区画を表示しない */
      });
    return () => {
      cancelled = true;
    };
  }, [node, taskId]);
  return (
    <Sheet open={node !== null} onOpenChange={onOpenChange}>
      <SheetContent side="right" className="flex w-full flex-col gap-0 p-0 data-[side=right]:sm:max-w-md">
        {node && meta && Icon && (
          <>
            <SheetHeader className="border-b p-4">
              <div className="flex items-center gap-2.5 pr-8">
                <span
                  className={cn("flex size-8 shrink-0 items-center justify-center rounded-lg shadow-sm", meta.iconBg)}
                >
                  <Icon className="size-4 text-white" />
                </span>
                <div className="min-w-0">
                  <SheetTitle className="leading-tight">{kindLabel}</SheetTitle>
                  <span className="text-muted-foreground truncate font-mono text-xs" title={node.label}>
                    {node.label}
                  </span>
                </div>
              </div>
            </SheetHeader>
            <ScrollArea className="min-h-0 flex-1">
              <div className="flex w-full min-w-0 flex-col gap-4 p-4">
                <section>
                  <h4 className="text-muted-foreground mb-1 text-xs font-medium">{t("sheet.attributes")}</h4>
                  <DetailRow label={t("sheet.attrType")}>{kindLabel}</DetailRow>
                  <DetailRow label={t("sheet.attrTestState")}>
                    {node.in_scope ? (
                      node.tested ? (
                        <span className="text-emerald-600 dark:text-emerald-400">{t("sheet.tested")}</span>
                      ) : (
                        <span className="text-neutral-500">{t("sheet.untested")}</span>
                      )
                    ) : (
                      <span className="text-neutral-400">{t("sheet.outOfScope")}</span>
                    )}
                  </DetailRow>
                  <DetailRow label={t("sheet.attrDomain")}>{node.domain}</DetailRow>
                  <DetailRow label={t("sheet.attrRootDomain")}>{node.root_domain}</DetailRow>
                  <DetailRow label="IP">{node.ip}</DetailRow>
                  <DetailRow label={t("sheet.attrPort")}>{node.port ? node.port : undefined}</DetailRow>
                  <DetailRow label="URL">
                    {node.url ? <span className="font-mono text-xs break-all">{node.url}</span> : undefined}
                  </DetailRow>
                  <DetailRow label={t("sheet.attrTitle")}>{node.page_title}</DetailRow>
                  <DetailRow label={t("sheet.attrStatusCode")}>
                    {node.status_code ? node.status_code : undefined}
                  </DetailRow>
                  <DetailRow label={t("sheet.attrApp")}>{node.app_name}</DetailRow>
                  <DetailRow label={t("sheet.attrAssetId")}>
                    {node.asset_id ? <span className="font-mono text-xs">{node.asset_id}</span> : undefined}
                  </DetailRow>
                </section>
                {refs && (refs.intents.length > 0 || refs.facts.length > 0 || refs.findings.length > 0) && (
                  <section className="flex flex-col gap-3 border-t pt-3">
                    <RefList title={t("sheet.refIntents")} items={refs.intents} />
                    <RefList title={t("sheet.refFacts")} items={refs.facts} />
                    <RefList title={t("sheet.refFindings")} items={refs.findings} />
                  </section>
                )}
                <section className="border-t pt-3">
                  <h4 className="text-muted-foreground mb-1.5 text-xs font-medium">{t("sheet.rawData")}</h4>
                  <pre className="bg-muted/50 text-foreground max-w-full overflow-hidden rounded-md border p-3 font-mono text-xs leading-relaxed break-all whitespace-pre-wrap">
                    {raw}
                  </pre>
                </section>
              </div>
            </ScrollArea>
          </>
        )}
      </SheetContent>
    </Sheet>
  );
}

function FoldSheet({
  fold,
  onOpenChange,
  onShowMore,
  onPick,
}: {
  fold: FoldNode | null;
  onOpenChange: (open: boolean) => void;
  onShowMore: (groupId: string) => void;
  onPick: (n: CoverageGraphNode) => void;
}) {
  const t = useTranslations("taskDetail.coverageGraph");
  const meta = fold ? kindMeta[fold.kind] : null;
  const Icon = meta?.icon;
  const kindLabel = fold ? t(`kind.${fold.kind}`) : "";
  return (
    <Sheet open={fold !== null} onOpenChange={onOpenChange}>
      <SheetContent side="right" className="flex w-full flex-col gap-0 p-0 data-[side=right]:sm:max-w-md">
        {fold && meta && Icon && (
          <>
            <SheetHeader className="border-b p-4">
              <SheetTitle className="text-base">
                {t("sheet.foldTitle", { kind: kindLabel, count: fold.hidden.length })}
              </SheetTitle>
              <p className="text-muted-foreground text-xs">{t("sheet.foldHint")}</p>
            </SheetHeader>
            <ScrollArea className="min-h-0 flex-1">
              <div className="flex flex-col gap-1 p-3">
                {fold.hidden.map((n) => (
                  <button
                    key={n.key}
                    type="button"
                    onClick={() => onPick(n)}
                    className="hover:bg-accent flex items-center gap-2 rounded-md px-2 py-1.5 text-left text-sm"
                  >
                    <span
                      className={cn(
                        "flex size-5 shrink-0 items-center justify-center rounded",
                        n.tested ? meta.iconBg : "bg-neutral-400 dark:bg-neutral-600",
                      )}
                    >
                      <Icon className="size-3 text-white" />
                    </span>
                    <span className="min-w-0 flex-1 truncate font-mono text-xs" title={n.label}>
                      {n.label}
                    </span>
                    {n.tested && <span className="size-1.5 shrink-0 rounded-full bg-emerald-500" />}
                  </button>
                ))}
              </div>
            </ScrollArea>
            <div className="border-t p-3">
              <Button className="w-full" variant="outline" onClick={() => onShowMore(fold.groupId)}>
                {t("sheet.showMore", { step: FOLD_STEP })}
              </Button>
            </div>
          </>
        )}
      </SheetContent>
    </Sheet>
  );
}

// ---------------------------------------------------------------------------
function GraphInner({ taskId, coverageEnabled = true }: { taskId: string; coverageEnabled?: boolean }) {
  const [data, setData] = React.useState<{
    nodes: CoverageGraphNode[];
    edges: CoverageGraphEdge[];
  } | null>(null);
  const [expanded, setExpanded] = React.useState<Map<string, number>>(new Map());
  const [selectedAsset, setSelectedAsset] = React.useState<CoverageGraphNode | null>(null);
  const [selectedFoldId, setSelectedFoldId] = React.useState<string | null>(null);
  const [loading, setLoading] = React.useState(true);

  const t = useTranslations("taskDetail.coverageGraph");
  const kindLabel = React.useCallback((k: Kind) => t(`kind.${k}`), [t]);
  const moreLabel = React.useCallback(
    (count: number, k: Kind) => t("graph.moreCount", { kind: t(`kind.${k}`), count }),
    [t],
  );

  const containerRef = React.useRef<HTMLDivElement>(null);
  const graphRef = React.useRef<G6Graph | null>(null);
  // click の処理には最新の key→RenderNode の対応が要る（G6 のイベントのコールバックはクロージャの外の ref を読む）。
  const renderMapRef = React.useRef<Map<string, RenderNode>>(new Map());
  const gDataRef = React.useRef<{ nodes: G6NodeDatum[]; edges: { source: string; target: string }[] }>({
    nodes: [],
    edges: [],
  });

  const fetchGraph = React.useCallback(() => {
    setLoading(true);
    api
      .taskCoverageGraph(taskId)
      .then((g) => {
        // 資産のカバー率の機能がオフ(B1)：図は出す(範囲の中の資産/company の関連はやはり見える)が、
        // tested の状態をならす——テストの進み具合を出さず、テスト済みの強調もしない。
        const nodes = coverageEnabled ? (g.nodes ?? []) : (g.nodes ?? []).map((n) => ({ ...n, tested: false }));
        setData({ nodes, edges: g.edges ?? [] });
      })
      .catch(() => {
        /* 前回のデータを残す */
      })
      .finally(() => setLoading(false));
  }, [taskId, coverageEnabled]);

  React.useEffect(() => {
    fetchGraph();
  }, [fetchGraph]);

  const { renderNodes, renderEdges } = React.useMemo(
    () =>
      data
        ? computeVisible(data.nodes, data.edges, expanded)
        : { renderNodes: [] as RenderNode[], renderEdges: [] as CoverageGraphEdge[] },
    [data, expanded],
  );

  // 構造の署名：見えるノード/辺の集合が変わったときだけ図を作り直し + レイアウトをやり直し、要らない揺れを避ける。
  const sig = React.useMemo(
    () =>
      `${renderNodes
        .map((n) => `${n.key}:${n.fold ? "f" : n.node.tested ? "t" : "u"}`)
        .sort()
        .join(",")}|${renderEdges.length}`,
    [renderNodes, renderEdges],
  );

  // gDataRef + renderMapRef を保つ（イベントと図のデータの適用が読む）。
  gDataRef.current = {
    nodes: toG6Nodes(renderNodes, kindLabel, moreLabel),
    edges: renderEdges.map((e) => ({ source: e.src, target: e.dst })),
  };
  const rmap = new Map<string, RenderNode>();
  for (const rn of renderNodes) rmap.set(rn.key, rn);
  renderMapRef.current = rmap;

  const applyData = React.useCallback(() => {
    const graph = graphRef.current;
    if (!graph || graph.destroyed) return;
    graph.setData(gDataRef.current);
    // render() は非同期で d3-force のレイアウトを走らせる;レイアウトが決まる前に部品が外される/壊されると、g6 は空にした
    // context で transform に触れて例外を投げる(runtime/layout transformDataAfterLayout を参照)。これは純粋な
    // teardown の競合なので、飲み込んでも機能に影響しない;本当の描画のエラー(図が壊されていない)はやはりログに出す。
    void graph.render().catch((err) => {
      if (!graph.destroyed) console.error("[coverage-graph] render:", err);
    });
  }, []);

  // 図を作る（1回）。動的 import で SSR/静的なエクスポートのときの window への依存を避ける。
  React.useEffect(() => {
    let destroyed = false;
    let graph: G6Graph | null = null;
    void (async () => {
      const { Graph } = await import("@antv/g6");
      if (destroyed || !containerRef.current) return;
      graph = new Graph({
        container: containerRef.current,
        autoResize: true,
        autoFit: "view",
        background: "#f0f2f7",
        node: {
          style: {
            size: (d: unknown) => nd(d).size,
            fill: (d: unknown) => nodeFill(nd(d)),
            stroke: (d: unknown) => nodeStroke(nd(d)),
            lineWidth: (d: unknown) => (nd(d).fold || !nd(d).inScope ? 1 : 1.5),
            lineDash: (d: unknown) => (nd(d).fold || !nd(d).inScope ? [3, 3] : [0]),
            iconSrc: (d: unknown) => (nd(d).fold ? FOLD_ICON : KIND_ICON[nd(d).kind]),
            iconWidth: (d: unknown) => Math.max(12, nd(d).size * 0.55),
            iconHeight: (d: unknown) => Math.max(12, nd(d).size * 0.55),
            labelText: (d: unknown) => nd(d).lbl,
            labelFontSize: 10,
            labelPlacement: "bottom",
            labelFill: "#475569",
            labelBackground: true,
            labelBackgroundFill: "rgba(255,255,255,0.75)",
            labelBackgroundRadius: 3,
            labelPadding: [1, 3],
          },
        },
        edge: {
          style: { stroke: "#cbd5e1", lineWidth: 1, endArrow: false },
        },
        layout: {
          type: "d3-force",
          collide: { radius: (d: unknown) => (nd(d).size ? nd(d).size : 20) + 8 },
          link: {
            distance: (edge: unknown) => {
              // 最上位（企業/ルートドメイン）は子のノードから少し遠く、葉は近く。edge.source は id か解決済みのノードのことがある。
              const s = (edge as { source: string | { id?: string } }).source;
              const srcId = typeof s === "string" ? s : (s?.id ?? "");
              const src = renderMapRef.current.get(srcId);
              const k = src && !src.fold ? src.kind : "endpoint";
              return k === "company" || k === "root_domain" ? 120 : 60;
            },
          },
          manyBody: {
            strength: (d: unknown) => (nd(d).kind === "endpoint" || nd(d).fold ? -60 : -200),
          },
        },
        behaviors: ["drag-element-force", "drag-canvas", "zoom-canvas"],
      });
      graph.on("node:click", (evt: unknown) => {
        const id = (evt as { target?: { id?: string } }).target?.id;
        if (!id) return;
        const rn = renderMapRef.current.get(id);
        if (!rn) return;
        if (rn.fold) setSelectedFoldId(rn.key);
        else setSelectedAsset(rn.node);
      });
      graphRef.current = graph;
      applyData();
    })();
    return () => {
      destroyed = true;
      // 先にレイアウトを止めてから壊す:「レイアウトが動いているのに context が空になる」競合の窓をできるだけ短くする。
      try {
        graph?.stopLayout();
      } catch {
        /* 図がまだできていないか、もうレイアウトの文脈がない */
      }
      graph?.destroy();
      graphRef.current = null;
    };
  }, [applyData]);

  // 見える集合が変わった → データを流し込み直し + レイアウト。sig は並べ直しのきっかけとしてだけ使う（applyData は gDataRef を読む）。
  // biome-ignore lint/correctness/useExhaustiveDependencies: sig はわざと置いた並べ直しのきっかけの依存
  React.useEffect(() => {
    applyData();
  }, [sig, applyData]);

  const showMore = React.useCallback((groupId: string) => {
    setExpanded((prev) => {
      const m = new Map(prev);
      m.set(groupId, (m.get(groupId) ?? FOLD_LIMIT) + FOLD_STEP);
      return m;
    });
  }, []);

  const selectedFold =
    selectedFoldId != null
      ? ((renderNodes.find((n) => n.fold && n.key === selectedFoldId) as FoldNode | undefined) ?? null)
      : null;
  React.useEffect(() => {
    if (selectedFoldId != null && !selectedFold) setSelectedFoldId(null);
  }, [selectedFoldId, selectedFold]);

  const total = data?.nodes.length ?? 0;
  const tested = data?.nodes.filter((n) => n.in_scope && n.tested).length ?? 0;
  const inScope = data?.nodes.filter((n) => n.in_scope).length ?? 0;

  return (
    <div className="relative h-full w-full">
      <div ref={containerRef} className="h-full w-full" />

      {/* 凡例 + 統計 + 更新（重ねた層） */}
      <div className="bg-card/95 pointer-events-auto absolute top-3 left-3 flex max-w-[340px] flex-col gap-2.5 rounded-lg border p-3 text-xs shadow-sm backdrop-blur">
        <div className="flex items-center justify-between gap-3">
          {total > 0 ? (
            <span className="text-muted-foreground">
              {t("graph.scopeIn")} <span className="text-foreground font-semibold tabular-nums">{inScope}</span>
              {coverageEnabled && (
                <>
                  {" "}
                  · {t("graph.tested")}{" "}
                  <span className="font-semibold tabular-nums text-emerald-600 dark:text-emerald-400">{tested}</span>
                </>
              )}
            </span>
          ) : (
            <span className="text-muted-foreground">{loading ? t("graph.loading") : t("graph.empty")}</span>
          )}
          <Button variant="ghost" size="icon" className="size-6" onClick={fetchGraph} title={t("graph.refresh")}>
            <RefreshCw className={cn("size-3.5", loading && "animate-spin")} />
          </Button>
        </div>
        <div className="flex flex-wrap gap-x-3 gap-y-1.5">
          {(Object.keys(kindMeta) as Kind[]).map((k) => {
            const m = kindMeta[k];
            const Icon = m.icon;
            return (
              <span key={k} className="text-foreground inline-flex items-center gap-1.5">
                <span className={cn("flex size-4 items-center justify-center rounded", m.iconBg)}>
                  <Icon className="size-2.5 text-white" />
                </span>
                {t(`kind.${k}`)}
              </span>
            );
          })}
        </div>
        <div className="border-border/60 text-muted-foreground flex flex-wrap gap-x-3 gap-y-1.5 border-t pt-2">
          {coverageEnabled && (
            <>
              <span className="inline-flex items-center gap-1.5">
                <span className="size-3 rounded-full bg-emerald-500" /> {t("graph.legendTested")}
              </span>
              <span className="inline-flex items-center gap-1.5">
                <span className="size-3 rounded-full bg-neutral-400" /> {t("graph.legendUntested")}
              </span>
            </>
          )}
          <span className="inline-flex items-center gap-1.5">
            <span className="size-3 rounded-full border border-dashed border-neutral-400 bg-neutral-200" />{" "}
            {t("graph.legendOutOfScope")}
          </span>
        </div>
        <p className="text-muted-foreground/80 border-border/60 border-t pt-2 leading-relaxed">{t("graph.hint")}</p>
      </div>

      <AssetSheet node={selectedAsset} taskId={taskId} onOpenChange={(o) => !o && setSelectedAsset(null)} />
      <FoldSheet
        fold={selectedFold}
        onOpenChange={(o) => !o && setSelectedFoldId(null)}
        onShowMore={showMore}
        onPick={(n) => {
          setSelectedFoldId(null);
          setSelectedAsset(n);
        }}
      />
    </div>
  );
}

export function CoverageGraphTab({ taskId, coverageEnabled = true }: { taskId: string; coverageEnabled?: boolean }) {
  return (
    <Card>
      <CardContent className="p-0">
        <div className="h-[72vh] w-full overflow-hidden rounded-xl">
          <GraphInner taskId={taskId} coverageEnabled={coverageEnabled} />
        </div>
      </CardContent>
    </Card>
  );
}
