---
name: api-recon
description: Web サイトの API エンドポイントを集めるときに呼ぶ skill。
---

# API Recon（フロントエンドのエンドポイント偵察）

**認可されている**ことを前提に、できるかぎり漏れなく見つける：**バックエンドの API**（パス、メソッド、パラメータ、レスポンスボディ）、**フロントエンドのルート**、**UI の機能の起点**（タブ、モーダル、表の操作など）。

---

## 境界と禁止事項（Agent は必読 · 破れば範囲外）

この skill は **API / パラメータ面の偵察だけ**を行う。脆弱性の探索や侵入・悪用の段階ではない。

### タスクの境界

| 範囲 | 許可 | 禁止 |
|---|---|---|
| **目的** | path、method、パラメータ、ルート、UI の起点を列挙する | SQLi/XSS/権限昇格/総当たり/fuzz による脆弱性探し、改ざんしたリクエストでの攻撃、破壊的な操作 |
| **認証** | Hook + stub/mock で**クライアント側**のログインの関門を越える | ユーザーにアカウントやパスワードを求める、推測する；本物のログインフォームの送信を試す |
| **実行時** | 資格情報なしでエンドポイントを hook し、mock のレスポンスで SPA をログイン後の殻に入れる | 本物のバックエンドのセッションがないと進めない流れに頼る |

### 資格情報なしの動的解析（Phase 3 の既定）

1. `preload.js` / `runtime_harvest.js` でログイン、権限、メニューなどの bootstrap のエンドポイントを**横取りして stub する**；
2. 業務の問い合わせのエンドポイントには **構造が正しく、業務コードが成功で、データは空でもよい** mock の body を返す；
3. バックエンドがない、または 401 の環境でもフロントエンドがログイン後のページを描けるようにし、より多くの XHR/fetch/WebSocket を起こさせる；
4. **空のデータ、空白の表、プレースホルダーの UI はすべて想定どおり**——それを理由に本物のログインや脆弱性テストに切り替えない。

**ひとことで言うと**：mock でフロントエンドのルートとコンポーネントのマウントを押し広げ、**outbound のリクエストだけを記録する**；バックエンドが何を返すかは重要ではなく、重要なのはフロントエンドが**ほかにどのエンドポイントを呼ぶか**。

### 手順上の絶対の禁止

| 禁止 | 代わりのやり方 |
|---|---|
| Phase 1 が終わる前に grep/curl/Read でメインの entry `index-*.js` から API path を取り出す | `OUTDIR/harvest_static.py` を走らせる |
| harvest の代わりになる `extract_apis.py` などのスクリプトを手で書く | `OUTDIR/harvest_static.py` を直して走らせ直す |
| 同じ grep/コマンドが 2 回以上失敗しても繰り返す | 戦略を変える：tool_logs を読む、harvest を直す、reference を調べる |
| 関門 A/B を飛ばして `scripts/` の原本をそのまま走らせる | OUTDIR に複製して対象に合わせて直す |
| 本物のユーザー名/パスワード、OTP、OAuth などの認証 | stub/mock（上記を参照） |
| 「本物のデータを取る」を理由に stub を飛ばし、権限昇格/インジェクションのテストをする | outbound だけを記録する。recon の境界に属する |
| 削除、機微なデータのエクスポート、一括の書き込みなど取り返しのつかない操作 | coverage のクリックでも同じ |
| runtime + 動的な列挙を終えていないのに、すべてのページとエンドポイントを得たと主張する | 「完了の定義」を参照するか、限界を明記する |
| パラメータ起動の表 + diff を終えていないのに、すべてのパラメータを把握したと主張する | Phase 3b の表 + Phase 5 の diff |
| 1 つの runtime のサンプルから必須/任意を推定する | 複数のサンプルの diff、または検証の規則/エラーからの逆算 |

---

## 2 層のモデル + 実行モード

| 層 | 産物 | 限界 |
|---|---|---|
| **静的**（JS bundle） | すべての endpoint のパス、ルートの下書き、リクエストを組み立てる箇所のフィールドの候補 | HTTP メソッドはない；パラメータは Phase 1b が要る；実行時に組み立てる URL を取りこぼす |
| **実行時**（生きたセッション） | メソッド + body + レスポンス + 動的な URL + WS/SSE；複数のサンプルの diff でパラメータを補う | ページを実際に描画しないとリクエストは出ない；1 つのサンプルでは必須/任意を決めるのに足りない |

| 実行モード | エンジン | 向いている用途 |
|---|---|---|
| **depth** | `runtime_harvest.js`（Puppeteer） | API の一覧、METHOD/params/レスポンスボディ、WS/SSE、再現できる一括の実行 |
| **coverage** | browser + `preload.js` | タブ/モーダル/表を押し、機能の起点をより深く覆う |
| **both** | depth の後に coverage | 最も漏れがなく、最も時間がかかる |

**パラメータの方法論**（汎用のスクリプトはない）：path は harvest/正規表現で；パラメータは **アンカーからの窓の拡大 + UI の結びつきの連鎖 + 複数のサンプルの diff + エラーからの逆算** で（grep のレシピは [reference.md](reference.md) の J 節を参照）。

---

## 完了の定義

すべてを満たしたときだけ recon の完了を主張してよい：

- [ ] **静的**：Phase 1 の harvest が `api_static.txt`、`routes.txt`、`js/` を出した
- [ ] **実行時**：depth か coverage の少なくとも一方；coverage/both は **Hook が効いている + 動的な列挙のループ**が必須
- [ ] **殻に入る**：業務の path を開いたときに `/login` でない（hash ルートに注意）
- [ ] **パラメータ**：coverage/both でパラメータ起動の表 + `param_samples.json` を終え；Phase 5 で `params_merged.json` に合わせる
- [ ] **深さ**（モジュールのページが空白なら）：Phase 4 で権限ツリーを復元して走らせ直し、**module 単位の API**（locale/bootstrap だけではないもの）が出るまで続ける
- [ ] **納品**：Phase 5 の産物がそろっている（Phase 5 の産物の表を参照）；`insert_assets` でサービスとエンドポイントの資産を書き込んだ

---

## スクリプトと関門

`scripts/` は参考のテンプレートにすぎず、原本をそのまま走らせて最終結果にするのは**禁止**。

**規則**：まず読む → 対象に合わせて直す → `OUTDIR`（例 `recon/`）に書き込む → `CHANGES.md` に記録する；合わなければ方法論に沿って書き直し、構造だけを借りる。

| 関門 | いつ | 参考スクリプト → OUTDIR の写し | よく直す必要がある項目 |
|---|---|---|---|
| **A（静的）** | Phase 0 の後、harvest/spider を**初めて**走らせる前 | `harvest_static.py` / `spider_mpa.py` | **多くのサイトは既定の regex のままで走る**；manifest/方言が合わないときだけ endpoint の正規表現、webpack/Vite の `publicPath`、MPA の exclude/cookie を直す |
| **B（実行時）** | Phase 2 の後、depth/coverage を走らせる前 | `runtime_harvest.js` / `preload.js` + `config.json` | Cookie/localStorage のキー、neutralize の成功値、stubs、login の正規表現、api の接頭辞、hash/history |

**SPA で強制する順序**（入れ替え不可；Phase の番号が「先に探ってからスクリプト」より優先する）：

| 手順 | 必須 | 禁止 |
|---|---|---|
| Phase 0 の完了後 | 次の Bash = `python3 OUTDIR/harvest_static.py <URL> OUTDIR` | メインの entry `index-*.js`（ふつう >500KB）を curl/grep/Read する |
| 関門 A | スクリプトを複製 → 必要に応じて少し直す → **すぐ実行する** | 先に手で API を取り出してから harvest するか決める |
| Phase 1 の完了前 | `wc -l` で産物を確かめる；404 なら harvest を直してやり直す | extract のスクリプトを手で書く；ダウンロードしていない URL を何度も grep する |
| Phase 1b から | grep は `OUTDIR/js/*.js` だけ | メインの bundle で harvest の代わりにする |

- ✅ `harvest_static.py` を複製 → （任意で）regex を直す → **すぐ実行する**
- ❌ メインの bundle を curl → 何度も grep → 一時的な extract を書く → 最後にやっと harvest
- **MPA**：Phase 0 の後の次の Bash = `python3 OUTDIR/spider_mpa.py ...`

---

## ツールと出力の制約

| 制約 | 説明 |
|---|---|
| 大きいファイル | >100KB の `index-*.js` を Read/grep で文脈に入れるのは**禁止**；OUTDIR のスクリプトで一括処理する |
| grep の出力 | 必ず `\| head -20` か `-m 5` を付ける；会話には path の要約だけを残し、bundle の断片を貼らない |
| 確認 | `wc -l`、`ls \| wc -l` を使う；ディレクトリをまるごと Read しない |
| regex の下見 | 任意、1 回まで、≤50KB の小さい chunk か HTML だけ；正式な静的解析は harvest を正とする |
| reference | レシピ/テンプレート/トラブルシューティングは [reference.md](reference.md) を参照し、全文を重ねて inline に書かない |

---

## 実行の道筋

```
Phase 0 分類 + OUTDIR
  → 関門 A → Phase 1 harvest（★ すぐ実行 ★）
  → Phase 1b パラメータの逆解析
  → Phase 2 認証の 3 つの関門 → config.json
  → 関門 B → Phase 3 実行時 + パラメータの表
  → Phase 4 権限ツリー（必要なら）→ Phase 3 を走らせ直す
  → Phase 5 合わせた報告 + insert_assets で見つけたすべてのサービス、エンドポイントの api 資産を一括で挿入する。挿入するときは、見つけた資産をどんな場合も取りこぼしてはならない
```

順にチェックする；**前の項目が終わるまで次の Phase に進んではならない**。

1. [ ] **Phase 0**：SPA/MPA を下見する；`OUTDIR` を作る → [Phase 0](#phase-0--分類)
2. [ ] **関門 A + Phase 1**：スクリプトを複製 → **すぐ** harvest → `wc -l` で確かめる → [Phase 1](#phase-1--静的)
3. [ ] **Phase 1b**：アンカーからの窓の拡大 + 結びつきの層 → `param_candidates.json` → [Phase 1b](#phase-1b--パラメータの逆解析)
4. [ ] **Phase 2**：認証の 3 つの関門 → `config.json` → [Phase 2](#phase-2--認証の-3-つの関門)
5. [ ] **関門 B**：runtime のスクリプトを調整する → [Phase 3](#phase-3--実行時)
6. [ ] **Phase 3**：depth / coverage / both；殻に入ったことを確かめる；パラメータ起動の表 → `param_samples.json`
7. [ ] **Phase 4**（必要なら）：権限ツリー → stubs を直す → Phase 3 を走らせ直す → [Phase 4](#phase-4--権限ツリーの復元)
8. [ ] **Phase 5**：産物を合わせる + 報告 + `insert_assets` → [Phase 5](#phase-5--合わせと報告)

---

## Phase 0 — 分類

入口の HTML を取り、**`OUTDIR` を作る**（skill の中の `scripts/` は変えない）：

- **SPA**：空の殻 + `<div id=app>` + chunk → Phase 1–5
- **MPA**：SSR + `<form>`、endpoint の bundle なし → 関門 A の後：

```bash
python3 recon/spider_mpa.py <BASE_URL> <OUTDIR> [--cookie "session=..."] [--max 300] [--depth 5] [--exclude "logout|delete|destroy"]
```

`forms.txt`、`links.txt`、`api_inline.txt` を出す。SPA で forms ≈ 0 なら → Phase 1 に切り替える。

---

## Phase 1 — 静的

[スクリプトと関門](#スクリプトと関門) · [ツールと出力の制約](#ツールと出力の制約) に従う。

```bash
python3 recon/harvest_static.py <BASE_URL> <OUTDIR>
```

harvest：HTML の script を解析 → webpack/Vite の manifest → すべての lazy chunk をダウンロード → `js/`、`api_static.txt`、`routes.txt`、`chunkmap.txt` を出す。

```bash
wc -l OUTDIR/api_static.txt OUTDIR/routes.txt
ls OUTDIR/js | wc -l
```

- chunk の数と manifest を比べる：404 なら harvest を直してやり直す。chunk を 1 つずつ手で curl しない
- `api_static.txt` が少なすぎる → OUTDIR の中の endpoint の正規表現を緩めて走らせ直す（reference を参照）

### Phase 1b — パラメータの逆解析

path は Phase 1 から来る；パラメータのフィールドは別に recon する必要がある。grep の規則は [ツールと出力の制約](#ツールと出力の制約) を参照。

**完了の基準**：重要なエンドポイントについて答えられる——フィールド名、送る位置、型の推定、必須か、サンプルの値、確信度。

#### 1b.0 — 送り方の形

| 形 | パラメータの場所 | 静的にまず見るところ |
|---|---|---|
| REST JSON | body + query | path のアンカーの近くの `(params\|data\|body)\s*:\s*\{` |
| GraphQL | `variables` | gql のテンプレート、`$page: Int` |
| 従来の form | urlencoded | `<form>`、`FormData` |
| ファイルのアップロード | multipart | `FormData.append` |
| パスのパラメータ | `/user/:id` | ルートの表 + `useParams` / `$route.params` |
| 暗号化/署名 | `sign`/`data` に包む | 暗号化関数の引数を Hook する（reference の D 節） |

産物：エンドポイントごとに `transport: query|json|form|graphql|encrypted` を記す。

#### 1b.1 — アンカーからの窓の拡大

わかっている path をアンカーにし、窓を広げてリクエストを組み立てるオブジェクトを探す：

```bash
grep -n '"/api/user/list"' OUTDIR/js/*.js | head -20
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' OUTDIR/js/*.js | head -20
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' OUTDIR/js/*.js | head -20
```

| 包む層 | パラメータの手がかり |
|---|---|
| axios のインスタンス | `data` / `params` |
| 共通の request | インターセプターが全体のフィールドを入れる |
| OpenAPI のクライアント | 生成された method のシグネチャ |
| React Query / SWR | hook の第 2 引数 |
| Vue composable | composable の引数 |

型の名残：`yup`/`zod`/rules、`Form.Item name=`、埋め込まれた Swagger。

→ `param_candidates.json`：`{ path, fields[], source: "static-callsite", confidence }`

#### 1b.2 — 結びつきの層

```
Form field → onFinish/handleSubmit → transform → API payload
```

| 結びつきの元 | 手法 |
|---|---|
| フォームの submit | submit → transform → API とたどる |
| 表の検索 | `getFieldsValue()` → `params` |
| ルート | `:id` / `?tab=` |
| インターセプター | 全体の `tenantId`、ページ分け、sign |
| 列挙の select | `options` → API の列挙値 |

DevTools の call stack で `fetch`/`XHR.send` から上へ、リクエストを組み立てる関数をたどる。

#### 1b.3 — 組み立ての 3 つの問い（≠ Phase 2 の認証の 3 つの関門）

| 問い | 答えるべきこと |
|---|---|
| **組み立て** | payload をどこで build するか、transform の痕跡 |
| **検証** | required、pattern、enum |
| **送り方** | path / query / body / multipart / ヘッダー |

インターセプターの関門（Phase 2）でついでに全体で入れるフィールド（Authorization、`X-Tenant-Id`、sign）を読む。

#### 1b.4 — Phase 3 とのつなぎ

候補のフィールドは静的/結びつきの層から来る；**必須/任意/条件つきの依存**は Phase 3 のパラメータの表 + diff + Phase 5 のエラーからの逆算が要る。

---

## Phase 2 — 認証の 3 つの関門

`OUTDIR/js/` で grep し（`head` を付ける）、`config.json` に書き込む（レシピは reference を参照）：

| 関門 | 問い | キーワード |
|---|---|---|
| **描画の関門** | ログイン済みをどう判断するか？ | `isLogin`、`getToken`、Cookie/localStorage |
| **インターセプターの関門** | 何が `/login` への遷移を起こすか？ | `response_code`、`errno`、axios interceptor |
| **内容の関門** | メニュー/権限はどこから来るか？ | `menu`、`permission`、`role`、`acl`、`routes` |

localStorage のキー名を資格情報とみなすのは禁止——chunk/リクエストの連鎖で確かめること。

**出口 = 関門 B**：結論を `config.json` に落とし、`OUTDIR/runtime_harvest.js` / `preload.js` を直す。

### Phase 2b — API の観察（任意）

OUTDIR の中の `preload.js` でセッションのキー名、Authorization、入れ子の API の URL を確かめる：

| 設定 | 産物 |
|---|---|
| `recordDetail: true` | `__API_RECON_DETAIL__` |
| `observe.xhrHeaders: true` | headers の観察 |
| `extractUrlsFromResponse: true` | レスポンスの中の下位 API |
| `observe.storageReads/cookieReads: true` | config に書き戻す |
| `neutralizeVueRouter: true` | `__API_RECON_ROUTES__` |

coverage はラウンドごとに書き出す：`__API_RECON_LOG__`、`__API_RECON_DETAIL__`、`__API_RECON_ROUTES__`、`__API_RECON_OBSERVE__`。

---

## Phase 3 — 実行時

関門 B を通っていること；[境界と禁止事項](#境界と禁止事項agent-は必読--破れば範囲外) · 資格情報なしの mock の方針に従う。

`config.json` に `"runtimeMode": "depth" | "coverage" | "both"` を設定する（テンプレートは reference を参照）。

### Hook と stub（depth と coverage で共通）

| 層 | 範囲 | 目的 |
|---|---|---|
| L1 精密 | auth/権限/bootstrap の stub | 最初の画面の認証を通る |
| L2 負の修正 | すべての JSON のレスポンス | 未ログインのコード → 成功 |
| L3 最後の砦 | L1 に当たらない `/api` など | 空の成功の body で UI を押し広げる |

- **depth**：偽の auth + `forward` で業務コードを変える + `stubs`；`routes`（hash/history）をたどる；`runtime_api.json` を出す
- **coverage**：**document-start** で `preload.js` を入れる（CDP の `addScriptToEvaluateOnNewDocument` か Userscript）

確認：`window.__API_RECON_PRELOAD__` がある；業務の path で `/login` に戻らない。

```bash
cd recon && npm install
node runtime_harvest.js config.json
```

### 3b — coverage の動的な列挙（必須）

1. メインのナビゲーション/サイドバー — 項目ごとにクリックし、ネットワークを 1–3s 待つ
2. タブ — `role=tab`、`.ant-tabs-tab`
3. 表 — 先頭の行の閲覧/編集/詳細
4. ツールバー — エクスポート、絞り込み、新規作成（**取り返しのつかない削除は避ける**）
5. モジュールに入るたび — API/ルートを合わせる
6. SPA — `routes.txt` のうち覆っていない path に制御した `pushState` をする（MPA では禁止）

**パラメータ起動の表**（必須）：モジュールごとに操作の種類ごとに 1 回ずつ記録し、**複数のサンプルを diff する**：

| 操作 | ふつう増えるパラメータ |
|---|---|
| 一覧の最初の画面 | ページ分け + 既定の絞り込み |
| 検索を押す | keyword、filter |
| 詳細な絞り込み | さらに optional |
| 新規作成/編集 | 完全な entity |
| 一括/エクスポート/並べ替え | `ids[]`、`exportType`、`sortField` |

**stub のもとでも outbound の body/headers は本物**——リクエストを正とする。記録 → `scan_raw.json`、`param_samples.json`、`api_detail.json`。

- **Vue**：`neutralizeVueRouter: true` + document-start の preload
- **React**：`routes.txt` + サイドバーのクリック + `pushState`
- **both**：先に 3a の depth、次に 3b の coverage

---

## Phase 4 — 権限ツリーの復元

**きっかけ**：モジュールのページが空白 / ルートごとに bootstrap（locale など）しかない → 内容の関門を通っていない。

| 現象 | 意味 |
|---|---|
| 殻に入れた | 描画の関門 + インターセプターの関門は通った |
| サイドバーに項目がない/押すと空白 | stub の形か権限コードが足りない |
| ルートごとの API が同じでごく少ない | `v-if permission` を通っていない |
| `routes.txt` が bundle よりずっと少ない | auth のモジュールから補う必要がある |

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl)[^"]*"' OUTDIR/js/*.js | sort -u | head -30
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|menuList|authList' OUTDIR/js/*.js | head -20
```

典型的な連鎖：`role_permissions`（flat codes）+ `permissions/all`（tree）→ `getResultTree` → `userRouteAuth[CODE].url`。

```bash
python3 recon/extract_route_map.py recon/js recon/
python3 recon/build_perm_tree.py recon/js recon/ --config recon/config.json
```

途中の産物：`route_map.json`、`userRouteAuth.json`、`permissions_tree.json`、`*_stub.json`、`perm_codes_all.txt`。

stub の点検：外側の `response_code` がインターセプターの関門と一致する；flat codes と tree がそろう；`routes` が `route_map` のすべての link を覆う。

`config.json` を更新したら **Phase 3 を走らせ直す**。大きい SPA では `waitUntil`、`routeTimeout`、`perRouteMs` を調整できる（reference の A3/I 節を参照）。

---

## Phase 5 — 合わせと報告

### 産物の表

| ファイル | 段階 | 内容 |
|---|---|---|
| `js/`、`api_static.txt`、`routes.txt`、`chunkmap.txt` | 1 | 静的な bundle と path |
| `param_candidates.json` | 1b | 静的なパラメータのフィールドの候補 |
| `config.json` | 2 | 3 つの関門 + runtime の設定 |
| `runtime_api.json` | 3a | depth の詳しい記録（WS/SSE を含む） |
| `param_samples.json`、`scan_raw.json`、`api_detail.json` | 3b | 複数のサンプル、クリックのログ、detail |
| `route_map.json` など | 4 | 権限ツリーの途中のファイル（実行したなら） |
| `params_merged.json` | 5 | 合わせたパラメータのフィールド + 確信度 |
| `api_merged.txt` | 5 | `METHOD /path [params] [static\|runtime\|both]` |
| `site_map.json` | 5 | ルート、API、params、機能の起点、限界 |
| **insert_assets** | 5 | すべてのサービス、エンドポイントの資産を資産のライブラリに書き込む |

### 5b — パラメータの合わせ

`param_samples.json` から diff する。**汎用の合わせスクリプトはない**。確信度の規則は reference の J7（高/中/低/起動待ち）を参照。

### 5c — エラーからの逆算

認可の範囲の中で不完全なリクエストを送って 400 を読んでよい（**パラメータの recon に属し、脆弱性テストではない**）：`field 'x' is required`、列挙のエラーなど。`data` の包み、`variables`、暗号化前の `bizData` に注意する。

報告には次を明記する：runtimeMode、静的/実行時の API の数、パラメータの確信度、覆っていないモジュール、参考スクリプトに対する `CHANGES.md` の要約。

`site_map.json` の推奨の構造：

```json
{
  "site": "https://example.com",
  "runtimeMode": "both",
  "appType": "vue-spa",
  "routeGuardStrategy": ["nav-neutralize", "L1-auth", "L2-patch", "forward"],
  "apisFromStatic": [],
  "apisFromRuntime": [],
  "apis": [],
  "params": [{ "method": "POST", "path": "/api/user/list", "transport": "json", "fields": [] }],
  "frontendRoutes": [],
  "routesVerifiedByClick": [],
  "featuresTriggered": [],
  "limitations": ""
}
```

ほかのフィールドと grep のレシピは [reference.md](reference.md) を参照。

---

## 共通の説明

- **フレームワークによらない**：webpack/Vite/Angular の lazy load のやり方は同じ
- **送り方**：REST/JSON、GraphQL、WebSocket、SSE；gRPC-web は範囲外
- **SSR**：クライアントの fetch は記録できる；RSC/Server Actions は完全には列挙できない
- **死角**：JSVMP、WASM、HMAC/mTLS の強い検証 → 静的 + 限界を明記する
- **パラメータの死角**：条件つきの連動、hidden params、WASM での組み立て → 「起動待ち」/「到達不能」
- **静的は安全網**：runtime が止められても静的にはまだ endpoint を列挙できる

---

## 追加の資料

- grep のレシピ、`config.json` のテンプレート、トラブルシューティング、Hook、パラメータの逆解析の J 節、site_map のテンプレート：**[reference.md](reference.md)**
- 参考スクリプトのパスは [スクリプトと関門](#スクリプトと関門) の表を参照
