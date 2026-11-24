// Mock の切り替え。ビルドのときに入れる公開の変数（NEXT_PUBLIC_ の接頭辞のものだけがブラウザーで読める）。
// Vercel で NEXT_PUBLIC_MOCK=1 を設定すればサイト全体が mock を通り、バックエンドは要らない。
export const MOCK = process.env.NEXT_PUBLIC_MOCK === "1";
