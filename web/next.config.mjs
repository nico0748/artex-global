import createNextIntlPlugin from "next-intl/plugin";

import { fileURLToPath } from "node:url";

// 静的なエクスポート：`NEXT_EXPORT=1 next build` で純粋に静的なディレクトリを web/out に出し、そのまま
// nginx の web のルートディレクトリに置いて動かせる。開発(next dev)ではこの変数を設定せず、/api のリバースプロキシとホットリロードを残す。
const isExport = process.env.NEXT_EXPORT === "1";
// Vercel の demo：サイト全体が mock を通り、バックエンドはなく、/api のリバースプロキシは要らない。
const isMock = process.env.NEXT_PUBLIC_MOCK === "1";

/** @type {import('next').NextConfig} */
const nextConfig = {
  // 親ディレクトリの lockfile がルートディレクトリの推定とリソースのパスの生成に影響するのを避ける。
  turbopack: { root: fileURLToPath(new URL(".", import.meta.url)) },
  reactCompiler: true,
  // LAN の IP から dev のリソース（HMR）に触れられるようにする。必要に応じて足したり消したりする。
  // dev の段階ではどの IPv4 の出どころからも /_next/* と HMR に触れられるようにする（LAN の IP が変わっても影響しない）。
  // 注意：Next は安全のために素の "*" を禁じているので、区切りごとのワイルドカードを使う；"*.*.*.*" はどの IPv4 にも当たる。
  allowedDevOrigins: ["*.*.*.*"],
  compiler: {
    removeConsole: process.env.NODE_ENV === "production",
  },
  ...(isExport
    ? {
        // 純粋に静的なエクスポート：Node の実行環境はなく、画像は最適化を通らず、ルートごとに <route>/index.html を出す。
        output: "export",
        images: { unoptimized: true },
        trailingSlash: true,
      }
    : isMock
      ? {
          // Vercel の mock の demo：バックエンドはなく、/api のリバースプロキシは要らない。
          images: { unoptimized: true },
        }
      : {
          // 開発：/api/* を Go のバックエンド（既定は :8787。AUTOPENTEST_API で上書きできる）にリバースプロキシする。
          async rewrites() {
            const backend = process.env.AUTOPENTEST_API ?? "http://localhost:8787";
            return [{ source: "/api/:path*", destination: `${backend}/api/:path*` }];
          },
        }),
};

// next-intl 플러그인. 요청 설정은 src/i18n/request.ts 에 둔다. i18n 경로 라우팅을
// 쓰지 않으므로 미들웨어는 추가하지 않는다(정적 내보내기 output: "export" 와 호환).
const withNextIntl = createNextIntlPlugin("./src/i18n/request.ts");

export default withNextIntl(nextConfig);
