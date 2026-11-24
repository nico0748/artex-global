"use client";

import * as React from "react";

import Link from "next/link";

import { ArrowUpCircleIcon } from "lucide-react";
import { useTranslations } from "next-intl";

import { api } from "@/lib/api";

/**
 * 上部のバーの「新しい版がある」の案内：ページ全体を読み込むときに1回問い合わせ、更新があれば版の番号の横に光らせ、
 * 押せばシステムの設定のページの「版と更新」のカードへ直接飛ぶ。
 *
 * バックエンドは GitHub の問い合わせの結果を 30 分キャッシュするので、ここで取り付けるたびに1回問い合わせても安全
 * ——認証なしの GitHub API は 60 回/時/IP しかなく、そのキャッシュがなければ、タブをいくつか開くだけで
 * 枠を使い切り、その後本当に更新したいときにかえって問い合わせられなくなる。
 *
 * 問い合わせの失敗は一律に黙る：上部のバーはエラーを出す場所ではなく、ユーザーが設定のページで「更新を確認」を押せば理由が見える。
 */
export function UpdateBadge() {
  const t = useTranslations("header");
  const [latest, setLatest] = React.useState("");

  React.useEffect(() => {
    let alive = true;
    api
      .checkUpdate()
      .then((r) => {
        // has_update にはもう「版の番号を比べられるか」の判断が入っているので、開発用のビルドではこの案内は光らない。
        if (alive && r.has_update && r.latest) setLatest(r.latest.replace(/^v(?=\d)/, ""));
      })
      .catch(() => {
        // 黙る：ネットワークがない / GitHub のレート制限のどちらも上部のバーにエラーを出すべきでない。
      });
    return () => {
      alive = false;
    };
  }, []);

  if (!latest) return null;

  return (
    <Link
      href="/system/settings"
      title={t("updateTitle", { latest })}
      className="inline-flex items-center gap-1.5 rounded-full bg-primary px-2.5 py-1 font-medium text-primary-foreground text-xs transition-opacity hover:opacity-90"
    >
      {/* 呼吸する点：上部のバーには要素が多く、文字だけだと見落とされやすいので、動きでひと目でわかるようにする。 */}
      <span className="relative flex size-1.5">
        <span className="absolute inline-flex size-full animate-ping rounded-full bg-primary-foreground opacity-75" />
        <span className="relative inline-flex size-1.5 rounded-full bg-primary-foreground" />
      </span>
      <ArrowUpCircleIcon className="size-3.5" />
      <span className="hidden sm:inline">{t("updateBadge", { latest })}</span>
      <span className="sm:hidden">{t("updateBadgeShort")}</span>
    </Link>
  );
}
