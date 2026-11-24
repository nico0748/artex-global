# api-recon — 参考の手引き

grep のレシピ、`config.json` のテンプレートとトラブルシューティング。grep はすべて `js/` ディレクトリに対して実行する。bundle が 1 行のときは先に `js-beautify` か `sed 's/}/}\n/g'` をかけてもよいが、ふつうは文脈の窓付きの素の grep で足りる。

## スクリプトの説明

`scripts/` の中のファイルはすべて**参考のテンプレート**で、実行する前に必ず対象のサイトに合わせて調整する。よく直す箇所：

| スクリプト | よく調整が要る項目 |
|---|---|
| `harvest_static.py` | endpoint の正規表現、webpack/Vite の manifest の解析、マイクロフロントエンドの publicPath、再試行/並行 |
| `runtime_harvest.js` | neutralize のフィールド名と成功値、stub の照合の規則と body の構造、routes の出どころ、WS の記録、`waitUntil`/`routeTimeout`/`proxy` |
| `preload.js` | `loginPathRe`、L1 の stubs、`neutralize.fields`、`apiPattern`、L3 を有効にするか、`recordDetail`、`observe.*`、`neutralizeVueRouter` |
| `spider_mpa.py` | `--exclude` で除く破壊的なリンク、cookie、depth/max、同じドメインでの絞り込み |
| `extract_route_map.py` | `routeMap` / `routeLink` の正規表現、KEY の命名のパターン |
| `build_perm_tree.py` | `userRouteAuth` の解析、`ROOTS`/`PREFIX_PARENT` の階層の推測、stub の外側のフィールド名 |
| `config.json` | 上のすべてのサイト固有のパラメータをまとめる入口 |

調整したファイルはタスクの作業ディレクトリ（例 `recon/`）に置くことを勧め、報告には参考スクリプトに対する具体的な変更を明記する。

---

## A. 3 つの関門の逆解析

### A1. 描画の関門 — 「ログイン済みをどう判断するか？」

```bash
grep -rhoaE '.{0,40}(isLogin|isAuthenticated|loggedIn|hasLogin|requireAuth)\b.{0,80}' js | head
grep -rhoaE 'function (getUser|getToken|getAuth)[0-9]?\([^)]*\)\{.{0,200}' js | head
grep -rhoaE '(localStorage|sessionStorage)\.getItem\("[^"]+"\)' js | sort -u
grep -rhoaE '(Cookies?|cookie)\.(get|load)\("[^"]+"\)' js | sort -u
grep -rhoaE '\batob\(|JSON\.parse\(|jwt|decode' js | head
```

`isLogin = f(getUser())` → `getUser = decode(storage.read(KEY))` の連鎖を見つけ、**保存のキー**、**入れ物**（Cookie か localStorage か）、**符号化**を決める：

| 符号化 | config での偽装のしかた |
|---|---|
| 平文の文字列 / `"1"` / token | `"value": "anything-truthy"` |
| `JSON.parse(x)` | `"value": "json:{\"id\":1,\"username\":\"admin\"}"` |
| `JSON.parse(atob(x))` | `"value": "b64json:{\"id\":1,\"username\":\"admin\"}"` |
| JWT | 署名なし/`alg:none` の JWT、または bundle の中の鍵で署名する |
| 暗号化（SM2/AES/RSA） | 決め打ちの鍵を探す；描画の関門が復号できる blob だけを要るなら forge できる；そうでなければ静的解析で支える |

→ `cookies` / `localStorage` に書き込む。

### A2. インターセプターの関門 — 「何が /login への遷移を起こすか？」

```bash
grep -rhoaE '.{0,60}(interceptors\.response|axios|request\.use).{0,120}' js | head
grep -rhoaE '.{0,40}(response_code|errcode|errno|\bcode\b|\bret\b|\bstatus\b)\s*[=!]==?\s*[\-0-9]{1,4}.{0,60}' js | head -20
grep -rhoaE '.{0,40}(未登录|请重新登录|登录已过期|unauthorized|登录失效|授权|ログインしていません|再ログイン|ログインの有効期限|token.{0,10}invalid).{0,40}' js | head
grep -rhoaE '.{0,30}(location\.href|router\.(push|replace)|navigate)\([^)]*login[^)]*\)' js | head
```

（3 行目の中国語のキーワードは、標的のアプリが中国語で出すメッセージに当てるためのもの。）

決めること：**フィールド名**、**成功値**（ふつう `0` か `200`）、**遷移を起こす失敗値**。junk のセッションで確かめる：

```bash
curl -sk -X POST -H 'Cookie: <fakekey>=junk' https://target/api/<protected> -d '{}' -H 'Content-Type: application/json'
```

→ `neutralize.fields` + `neutralize.success` に書き込む。

### A3. 内容の関門 — 「メニュー/権限はどこから来るか？」

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl|resource|nav)[^"]*"' js | sort -u
grep -rhoaE '.{0,30}(menus|permissions|menuList|routeList|authList|role_permissions)\b.{0,120}' js | head
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|hasPermission|checkAuth' js | head
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head
```

**2 層のデータ**（企業の管理画面でよくある）：

| API | 典型的な payload | 使う側 |
|---|---|---|
| `.../role_permissions` | `{ permissions: string[], role_type }` | ルートの守り、ボタン単位の ACL |
| `.../permissions/all` | `tree[{ code, position, children }]` | サイドバーのメニューの描画 |
| bundle の中の `userRouteAuth` | `{ CODE: { url, name? } }` | code → フロントエンドの path |
| bundle の中の `routeMap` | `{ KEY: { name, link } }` | 別名の解決（webpack の `o.DASHBOARD`） |

使う側のコードを読んで確かめる：`getResultTree(tree, permissions)` がどう絞り込むか、`v-if` / `hasAuth(code)` がどのフィールドを調べるか。

**手で forge する**（小さいサイト）：何でも通す payload を作る → `stubs`。

**権限ツリーの完全な復元**（大きいサイトで、サイドバー/下位のモジュールがまだ空白）：**I 節**を参照。

---

## B. config.json のテンプレート

```json
{
  "baseUrl": "https://target/",
  "runtimeMode": "both",
  "chromium": "/usr/bin/chromium",

  "cookies": [
    { "name": "auth", "value": "b64json:{\"id\":1,\"username\":\"admin\",\"role\":\"admin\",\"func\":{},\"permissions\":[\"*\"]}" }
  ],
  "localStorage": { "token": "faketoken", "isLogin": "1" },

  "neutralize": {
    "fields": ["response_code", "code", "errno", "ret", "status"],
    "success": 0,
    "flags": { "success": true, "message": "ok" }
  },
  "forward": true,
  "loginUrlPattern": "/login",
  "apiPattern": "/api/|/rest/|/graphql",

  "mockTier": "L1+L2",
  "recordDetail": true,
  "observe": {
    "storageReads": false,
    "cookieReads": false,
    "xhrHeaders": true
  },
  "neutralizeVueRouter": true,
  "stubs": [
    {
      "match": "permissions/all|/menu|role_permissions",
      "body": {
        "response_code": 0, "code": 0,
        "data": {
          "permissions": ["*"],
          "menus": [
            { "name": "dashboard", "path": "/dashboard", "show": true, "children": [] },
            { "name": "alert", "path": "/alert", "show": true, "children": [] }
          ]
        }
      }
    }
  ],

  "explore": {
    "clickTabs": true,
    "clickTables": true,
    "pushStateFallback": true,
    "maxMenuItems": 50
  },

  "routes": ["/dashboard", "/alert", "/asset", "/device", "/report", "/config", "/system"],
  "waitMs": 1500, "perRouteMs": 900, "headless": true,
  "waitUntil": "domcontentloaded",
  "routeTimeout": 12000,
  "proxy": "",

  "captureResponses": true, "recordWs": true, "respMax": 600
}
```

フィールドの説明：
- `runtimeMode`：`depth`（Puppeteer）、`coverage`（browser MCP）、`both`
- `cookies[].value` の接頭辞：`b64json:` → base64(JSON)；`json:` → 生の JSON；接頭辞なし → 文字どおりの値
- `forward: true` は本物のリクエストを転送してコードのフィールドを書き換える；`false` は完全にオフラインの stub
- `mockTier`：coverage のモードで preload が有効にする層。例 `L1+L2`、`L1+L2+L3`
- `routes` は `routes.txt` から来る；メニューを forge した後は harness が `<a href>` を自動で足す
- `captureResponses` / `recordWs` は depth のモードでだけ効く
- `waitUntil`：大きい SPA では `domcontentloaded` を使い、`networkidle2` で止まるのを避ける
- `routeTimeout`：ルート 1 つの `page.goto` のタイムアウト（ミリ秒）
- `proxy`：Puppeteer の `--proxy-server`；`HTTP_PROXY` / `HTTPS_PROXY` でも設定できる

### B1. 2 つの stub のテンプレート（role_permissions + permissions/all）

```json
"stubs": [
  {
    "match": "role_permissions",
    "body": {
      "response_code": 0,
      "data": {
        "permissions": ["MONITOR", "MONITOR_ALERT", "THREAT", "ASSETS_RISK"],
        "role_type": "SUPER_ADMIN"
      }
    }
  },
  {
    "match": "permissions/all",
    "body": {
      "response_code": 0,
      "data": [
        {
          "code": "MONITOR",
          "position": 1,
          "children": [
            { "code": "MONITOR_ALERT", "position": 1, "children": [] }
          ]
        }
      ]
    }
  }
]
```

外側のフィールド名（`response_code` / `code` / `data`）は A2 のインターセプターの関門と一致させる；`permissions` は tree のすべての leaf の code を覆う。

---

## C. coverage のモード：preload の設定

`scripts/preload.js` の先頭の `CONFIG` オブジェクトを編集するか、CDP で入れる前に置き換える：

```javascript
const CONFIG = {
  loginPathRe: /\/(login|signin)(\/|$|\?)/i,
  mockTier: 'L1+L2',
  forward: true,
  recordDetail: true,
  extractUrlsFromResponse: true,
  neutralizeVueRouter: true,
  observe: { storageReads: false, cookieReads: false, xhrHeaders: true },
  neutralize: { fields: ['response_code', 'code'], success: 0 },
  stubs: [ /* config.json の stubs と同じ */ ],
  apiPattern: /\/(api|apis|v\d+|dev|internal|graphql)\//i,
};
```

確認：`window.__API_RECON_PRELOAD__ === true` で、pathname が落ち着いている。

記録の結果を書き出す：

```javascript
JSON.stringify({
  apis: [...window.__API_RECON_LOG__],
  detail: window.__API_RECON_DETAIL__,
  routes: [...(window.__API_RECON_ROUTES__ || [])],
  observe: window.__API_RECON_OBSERVE__,
}, null, 2)
```

---

## D. preload / runtime の Hook の能力

preload（coverage）と runtime_harvest（depth）に組み込んだブラウザーの Hook の能力と覆う範囲：

| Hook の能力 | API の発見での価値 | 覆う |
|---|---|---|
| fetch / XHR.open を Hook | リクエストの URL/メソッドを記録 | ✅ `recordDetail` + `__API_RECON_LOG__` |
| XHR.setRequestHeader を Hook | Authorization などのヘッダーを見つける | ✅ `observe.xhrHeaders` |
| localStorage/cookie の読み取りを Hook | セッションのキー名を確かめる | ⚠️ 任意の `observe.storageReads/cookieReads` |
| Vue のルートの取得 | frontendRoutes を補う | ✅ `__API_RECON_ROUTES__`（読み込み済みのルート） |
| Vue のルートの守りの無力化 / ログインへの遷移の阻止 | モジュールを押し広げて API を起こす | ✅ `neutralizeVueRouter` + ネイティブの遷移の無力化 |
| React のルートの取得 | ルートを補う | ⚠️ 静的 + クリック；専用の Hook はない |
| ページの遷移の阻止（ログインの path） | ページに留まって解析する | ⚠️ ログインの path だけを止め、業務のナビゲーションを止めないようにする |
| 暗号ライブラリ（CryptoJS/SM など）を Hook | 暗号化したパラメータ → 平文の API の body | ❌ 暗号化関数の引数を手で Hook する必要がある；結論は config に書く |
| デバッグ対策の回避 | さもないと runtime で API を記録できない | ❌ 手で扱う必要がある；静的解析はやはり使える |

---

## E. Endpoint の抽出の正規表現（静的に少なすぎるとき）

`harvest_static.py` の `extract_endpoints` を緩めるか、手で：

```bash
grep -rhoaE '"/[a-z][A-Za-z0-9_/\-]{3,}"' js | sort -u
grep -rhoaE '/api/[a-zA-Z0-9_./-]+' js | sort -u
```

---

## F. トラブルシューティング

| 現象 | 原因 → 対処 |
|---|---|
| 静的な API がとても少ない | endpoint の方言が合わない → 正規表現を緩める（D 節） |
| chunk の数 ≪ manifest | CSS だけか、配備されていない chunk；404 はもう再試行した |
| runtime がまだログインのページを出す | 描画の関門の誤り → A1 を見直す：キー名、入れ物、符号化、domain |
| 殻に入ったがモジュールが空白 | 内容の関門 → メニューを forge する（A3）；`routes` の path が違うかもしれない |
| ルートごとに bootstrap/locale しかない | 権限コードが足りない → I 節の権限ツリーの復元；`role_permissions` + `permissions/all` の 2 つの stub を調べる |
| サイドバーに項目はあるが下位のページが空白 | tree に中間のノードがないか、code が `userRouteAuth` と一致しない |
| どの API もログインに飛ぶ | インターセプターの関門 → `neutralize` を確かめる；入れ子のフィールドは walk のロジックを広げる必要がある |
| WS のフレームが 0 | ユーザーが操作して初めて subscribe する；`perRouteMs` を長くする |
| レスポンスボディが空 | `forward: true` のときだけ本物のレスポンスがある |
| Chromium がない | chromium を入れるか、`config.chromium` / `CHROMIUM` を設定する |
| Mock を多く入れてもログインに戻る | Hook が遅すぎるか `location.href` の setter がない → document-start + preload |
| 一覧がすべて空 | L3 の空の配列は正常；タブ/設定/詳細を押し続ける |
| Redux の action をルートと取り違える | get/set/change/clear/toggle/upload を含む内部の path を除く |
| Vue がまだログインに飛ぶ | preload が document-start でない → 入れる時機を変える；または `neutralizeVueRouter: false` のときは守りを手で外す |
| レスポンスに URL があるのに log に入らない | `extractUrlsFromResponse` をオンにする；または `__API_RECON_DETAIL__` から人が取り出す |
| Authorization のヘッダー名がわからない | `observe.xhrHeaders` をオンにするか、DevTools でリクエストのヘッダーを見る |
| runtime がとても遅い / タイムアウト | `waitUntil: domcontentloaded` に変える；`routeTimeout` を下げる；`networkidle2` は使わない |
| プロキシにつながらない | `proxy` / 環境変数を調べる；Puppeteer と curl のプロキシのポートをそろえる |

---

## G. 守りの固い標的

サーバーがセッションを段階的に検証する（forge できない署名付きの cookie、サーバー側で描画して stub できないメニュー）ときは、runtime は shell のところで止まる。想定どおりの挙動：

- **endpoint の列挙は静的解析で足りる** — モジュールの path はコードの中にある
- 認可が許すなら、**本物のセッション**で同じ harness を走らせる：`forward: true`、neutralize は要らず、本物の methods/params/responses を捕まえる

---

## H. 1 回のタスクのチェックリスト

1. 認可の範囲を確かめる
2. `scripts/harvest_static.py` を**読む** → 対象に合わせて調整する → 走らせる → `api_static.txt`、`routes.txt` を見る
3. **Phase 1b**：path のアンカーからの窓の拡大 + 結びつきの層 → `param_candidates.json`（J 節）
4. A1/A2/A3 を逆解析する → サイト固有の `config.json` を書く
5. `runtime_harvest.js` / `preload.js` を**読んで調整してから**実行する
6. `runtimeMode=depth`：`npm install` → 調整した harvest のスクリプトを走らせる
7. `runtimeMode=coverage/both`：document-start で調整した preload を入れる → browser MCP で動的に列挙 + **パラメータ起動の表**
8. モジュールが描画されない → **I 節の権限ツリーの復元** → stubs を直す → 走らせ直す
9. パラメータの複数のサンプルの diff + エラーからの逆算 → `params_merged.json`
10. 合わせる → `site_map.json` + `api_merged.txt`。覆った範囲、欠け、スクリプトの変更点を正直に記す

---

## I. 権限ツリーの復元（Phase 4 の深掘り）

単純な `menus: [{ path, show: true }]` の forge が効かず、下位のモジュールがまだ mount されないときに使う。

### I1. auth のモジュールを見つける

```bash
grep -l 'userRouteAuth' js/*.js
grep -l 'routeMap\|routeLink' js/*.js
grep -rhoaE 'getResultTree|role_permissions|permissions/all' js | head
```

記録する：**権限の API の path**、**レスポンスのフィールド名**、**それを使う chunk のファイル名**。

### I2. routeMap を取り出す

```bash
python3 scripts/extract_route_map.py recon/js recon/
# recon/route_map.json を出す
```

`[!] no routeMap pattern found` なら：`extract_route_map.py` の正規表現を緩めるか、手で grep する：

```bash
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head -20
```

### I3. 権限ツリー + stub を作る

```bash
python3 scripts/build_perm_tree.py recon/js recon/ --config recon/config.json
```

スクリプトのロジック：
1. `userRouteAuth={MONITOR:{url:...},...}` を解析する（webpack の別名 `He=o.DASHBOARD` を含む）
2. `route_map.json` で alias → 本物の path を解決する
3. code の接頭辞から parent を推測する（`MONITOR_ALERT` → `MONITOR`）
4. `permissions_tree.json`、`permissions_all_stub.json`、`role_permissions_stub.json` を出す
5. `--config` のときは `config.json` の `stubs` に自動で書き込み、`routes` を広げる

**対象に合わせて調整する**（スクリプトの先頭で）：
- `DEFAULT_ROOTS`：最上位のモジュールの code の一覧
- `DEFAULT_PREFIX_PARENT`：`PREFIX_` → parent の対応
- `DEFAULT_EXTRA_PARENT`：接頭辞の関係にない孤立したノード

### I4. stub の一貫性を確かめる

```bash
# permissions の数は userRouteAuth の項目の数とほぼ同じのはず
wc -l recon/perm_codes_all.txt
# routes は route_map のすべての link を覆うはず
python3 -c "import json; m=json.load(open('recon/route_map.json')); r=set(json.load(open('recon/config.json'))['routes']); print('missing', [v['link'] for v in m.values() if v['link'] not in r])"
```

### I5. runtime を走らせ直して比べる

```bash
node recon/runtime_harvest.js recon/config.json
# forge の前後で runtime_api.json の件数を比べる；/attack、/asset などでモジュールの API が出たかを調べる
```

| forge の前 | forge の後（成功） |
|---|---|
| どのルートでも同じ 3–5 件の bootstrap | ルートごとに違う module の API が起きる |
| `/api/locale/language` だけ | `/api/web/...` のモジュールの endpoint が出る |
| `routes.txt` のルートは 1 桁 | `routes` が 80–110+ になり、route_map から来る |

### I6. それでも失敗するとき

- **coverage のモード**：サイドバー + タブを押す。権限の関門は操作の後に初めてリクエストすることがある
- **stub のフィールド**：本物の API（curl + 本物の session）と stub の入れ子を比べる
- **追加の守り**：`hasPermission|checkRole|func.` などのボタン単位の検査を grep し、`role_permissions.permissions` を広げる
- **静的解析で支える**：モジュールの API の path は `api_static.txt` にまだあり、runtime は METHOD/body を補うだけ；パラメータは `param_candidates.json` + 記録済みのサンプルを残す

---

## J. パラメータの逆解析（Phase 1b / 5b / 5c）

**方法論であって、汎用のスクリプトではない。** path は正規表現で探し、パラメータはアンカーからの窓の拡大 + UI の結びつきの連鎖 + 複数のサンプルの diff + エラーからの逆算で探す。

### J1. アンカーからの窓の拡大 — path からリクエストを組み立てるオブジェクトを探す

```bash
# Phase 1 でわかった path をアンカーにする
grep -n '"/api/user/list"' js/*.js
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' js | head
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' js | head
grep -rhoaE '(get|post|put|delete|patch)\([^,]+,\s*\{' js | head
```

### J2. 包む層と送り方の形

```bash
# axios / 共通の request
grep -rhoaE '(axios|request)\.(get|post|put|delete|patch)\(' js | head
grep -rhoaE 'interceptors\.(request|response)' js | head

# GraphQL
grep -rhoaE '(query|mutation)\s+\w+|gql`|graphql\(' js | head
grep -rhoaE '\$[a-zA-Z_]+\s*:\s*(Int|String|Boolean|\[)' js | head

# FormData / multipart
grep -rhoaE 'FormData|\.append\(' js | head

# パスのパラメータ
grep -rhoaE 'path:\s*"/[^"]*:[^"]+"' js | head
grep -rhoaE 'useParams|route\.params|\$route\.params' js | head
```

### J3. 検証の関門 — 必須 / 形式 / 列挙

```bash
grep -rhoaE '(required|message|pattern|enum|validator)\s*:' js | head
grep -rhoaE 'yup\.|zod\.|async-validator|Form\.Item|a-form-item|el-form-item' js | head
grep -rhoaE 'rules\s*:\s*\[|name:\s*["\'][a-zA-Z_]+["\']' js | head
grep -rhoaE 'label.*value|options\s*:\s*\[' js | head
```

### J4. 結びつきの層 — フォーム → API

```bash
grep -rhoaE 'onFinish|handleSubmit|getFieldsValue|validateFields' js | head
grep -rhoaE '(pick|omit|transform|dayjs|moment)\(' js | head
```

runtime での補い：DevTools → Network → リクエスト → **イニシエーター**（call stack）で `fetch`/`send` から上へ、リクエストを組み立てる関数をたどる。

### J5. 暗号化したパラメータ

```bash
grep -rhoaE 'encrypt|decrypt|sign|CryptoJS|sm2|sm3|sm4|RSA|AES' js | head
```

**暗号文からフィールドを推測しない** — 暗号化関数の**引数**を Hook し、暗号化の前に plaintext の payload を記録する；結論は `config.json` / `param_candidates.json` に書く。

### J6. パラメータ起動の表（Phase 3 で必須）

モジュールごとに操作ごとに 1 回ずつ記録し、リクエストの body/query を diff する：

| 操作 | 注目すること |
|---|---|
| 一覧の最初の画面 | ページ分けの既定値 |
| 検索 | keyword、filters |
| 詳細な絞り込み | optional のフィールド |
| 新規作成/編集 | 完全な entity |
| 一括/エクスポート | `ids[]`、`exportType` |
| 並べ替え/ページ送り | `sortField`、`order` |

`param_samples.json` を出す：`[{ "path", "method", "action": "search", "body", "query", "headers" }]`

### J7. 確信度の規則

| 確信度 | 条件 |
|---|---|
| **高** | 静的な callsite + runtime の 2 つ以上のサンプルが一致 |
| **中** | 静的だけ、または runtime が 1 回だけ |
| **低** | レスポンス/エラーからの逆算で、2 度目の確認をしていない |
| **起動待ち** | 静的にはわかっているフィールドだが、UI/権限がそこまで動いていない |

### J8. 場面ごとの早見

| 場面 | 順序 |
|---|---|
| REST の一覧のページ | J1 組み立てのオブジェクト → J6 4 回の diff → J3 rules |
| 新規作成/編集のフォーム | J3 Form の name → J4 submit の連鎖 → runtime で送信 + わざと空にして 400 を見る |
| GraphQL | J2 variables の宣言 → runtime で operation ごとに variables を記録 |
| 暗号化した body | J5 引数を Hook → 暗号化の前のフィールドが本物の params |

### J9. api-recon の段階との対応

| api-recon | パラメータの recon |
|---|---|
| Phase 1 静的 | J1 アンカーからの窓の拡大 |
| Phase 2 A2 インターセプター | 全体で入れるフィールド（tenantId、sign） |
| Phase 3 runtime | J6 起動の表 + `param_samples.json` |
| Phase 4 権限ツリー | モジュールごとにフォームが違う → 権限が足りて初めてすべてのフィールドが起きる |
| Phase 5 合わせ | `params_merged.json` + 確信度；1 つのサンプルで必須を決めない |

### J10. トラブルシューティング

| 現象 | 対処 |
|---|---|
| 静的にはフィールド名があるのに runtime で一度も出ない | 「起動待ち」と記す；権限ツリーを補う / 詳細な絞り込みを押す / 連動する select の各 option を選ぶ |
| 同じ path で body の形が違う | 正常 — `action` ごとに分けて記録し、schema を無理に合わせない |
| stub のレスポンスは偽物だが params を見たい | **outbound のリクエスト**の body/headers を見る。stub のレスポンスから逆算しない |
| 400 が nested field を報告する | 外側の包み `data`/`bizData`/`variables` に注意する |
| GraphQL で operation の名前しか見えない | `variables` の JSON を開く；静的には `$var: Type` を探す |

---
