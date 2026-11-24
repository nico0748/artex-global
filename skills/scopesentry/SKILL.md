---

## name: scopesentry-mcp
description: ScopeSentry MCP でセキュリティスキャンのプラットフォーム（プロジェクト、タスク、テンプレート、資産、ノード）を管理する。ユーザーが ScopeSentry、MCP、API Key、スキャンのタスク、資産の検索に触れたときに使う。

# ScopeSentry MCP の使い方

**ScopeSentry のインスタンスを配備済み**のユーザー向け。Cursor（またはほかの MCP クライアント）からプラットフォームにつなぎ、手元のソースは要らない。

## 1. 準備

### 1.1 サービスにつながるか確かめる

- 既定の Web の画面：`http://<ホスト>`
- MCP のエンドポイント：`http://<ホスト>/mcp`（前にリバースプロキシやフロントエンドのプロキシがあるなら、実際の `/mcp` のアドレスを正とする）

### 1.2 API Key を作る

1. ブラウザーで ScopeSentry の Web の画面にログインする
2. **API Key** の管理のページで鍵を作る（または管理者が用意したインターフェースで作る）
3. 返ってきた `ssk_...` の文字列を保存する（**一度しか表示されない**）

### 1.3 Cursor の MCP を設定する

Cursor → Settings → MCP → サーバーを追加：

```json
{
  "mcpServers": {
    "scopesentry": {
      "url": "http://<あなたのホスト>:8082/mcp",
      "headers": {
        "X-API-Key": "ssk_あなたの鍵"
      }
    }
  }
}
```

`Authorization: Bearer ssk_あなたの鍵` も使える。

設定が終わったら MCP を再起動するか Cursor を再読み込みし、ツールの一覧に `list_projects`、`list_assets` などが出ることを確かめる。

---

## 2. ツールの一覧


| ツール                     | 用途                |
| ---------------------- | ----------------- |
| `list_projects`        | タグごとにまとめたプロジェクトのツリー（プロジェクト ID を含む） |
| `list_projects_data`   | ページ分けしたプロジェクトの一覧。名前で検索できる     |
| `get_project`          | プロジェクトの詳細              |
| `create_project`       | プロジェクトを新しく作る              |
| `list_tasks`           | スキャンのタスクの一覧            |
| `get_task`             | タスクの詳細              |
| `list_scan_templates`  | スキャンのテンプレートの一覧            |
| `get_scan_template`    | テンプレートの詳細              |
| `list_plugin_modules`  | スキャンのパイプラインのモジュール名          |
| `list_plugins`         | 使えるプラグイン（hash、既定のパラメータを含む） |
| `create_scan_template` | スキャンのテンプレートを作る            |
| `create_scan_task`     | スキャンのタスクを作る            |
| `list_assets`          | 各種の資産を検索する（ページ分けした一覧）       |
| `count_assets`         | 資産の数を数える（`/api/assets/common/total`） |
| `get_asset_detail`     | 資産か脆弱性の詳細           |
| `add_asset_tag`        | 資産にタグを付ける           |
| `list_nodes`           | スキャンのノードの一覧            |


各ツールのパラメータは MCP のツールの説明（schema）を正とする；`list_assets` / `count_assets` の search、filter の構文は同じなので、資産を検索する前に `list_assets` の description を読むとよい。

「全部で何件か」を知りたいときは `count_assets`（Web のページ分けの総数のインターフェースに対応）を使い、総数を数えるために `list_assets` のページを何度もめくる必要はない。

---

## 3. よく使う流れ

### 3.1 プロジェクトごとに資産を調べる

ユーザーか文脈に**プロジェクトの条件がすでにある**ときは、`filter.project` を付けて範囲を絞ることを優先し、プロジェクトをまたいだデータが多すぎて応答が遅くなるのを避ける。はっきりしたプロジェクトがなければ、プロジェクトの絞り込みを無理に付けなくてよい。

1. `list_projects` か `list_projects_data` で目標のプロジェクトの **ObjectID**（`id` / `children[].value`）を取る
2. `list_assets` に `filter.project` を渡す（**ID でなければならず、プロジェクトの表示名は書けない**）

```json
{
  "asset_type": "asset",
  "pageIndex": 1,
  "pageSize": 20,
  "search": "domain=^example.com",
  "filter": {
    "project": ["<プロジェクトのObjectID>"]
  }
}
```

### 3.2 スキャンのタスクを作る

1. `list_nodes` でオンラインのノードの名前を取る
2. `list_scan_templates` か `create_scan_template` でテンプレートの **ObjectID** を取る
3. `create_scan_task`：`name`、`node` は必須、`template` にはテンプレートの ID を入れる（テンプレートの名前は入れられない）

**目標の出どころ `targetSource`（Web 側と同じ）：**

| targetSource | 説明 | 必須のパラメータ |
| --- | --- | --- |
| `general` | 目標を直接入れる | `target` |
| `project` | プロジェクトから目標を読む | `project`（プロジェクトの ObjectID の配列） |
| `asset` | Web の資産のライブラリから検索する | `search`；任意で `project`、`filter`、`targetNumber` |
| `RootDomain` | ルートドメインのライブラリから検索する | `search`；任意で `project`、`filter`、`targetNumber` |
| `subdomain` | サブドメインのライブラリから検索する | `search`；任意で `project`、`filter`、`targetNumber` |
| `UrlScan` | URL のスキャンの結果から検索する | `search`；任意で `project`、`filter`、`targetNumber` |
| `*Source`（例 `subdomainSource`） | 資産のページの「選択/検索」から作る | `targetTp=search` なら `search`；`targetTp=select` なら `targetIds` |

**例 — ルートドメインを直接スキャンする：**

```json
{
  "name": "example-サブドメインの収集",
  "node": ["node-1"],
  "template": "<テンプレートのObjectID>",
  "targetSource": "general",
  "target": "example.com\nfoo.com",
  "project": ["<プロジェクトのObjectID>"]
}
```

**例 — サブドメインのライブラリから続けてスキャンする（前のタスクの名前で絞り込む）：**

```json
{
  "name": "example-ポートと脆弱性",
  "node": ["node-1"],
  "template": "<後続のモジュールのテンプレートのObjectID>",
  "targetSource": "subdomain",
  "search": "task==\"example-サブドメインの収集\"",
  "project": ["<プロジェクトのObjectID>"]
}
```

### 3.3 ルートドメインの完全な情報収集（2 段階を勧める）

入力が**ルートドメイン**で**完全な情報収集**をするときは、2 回に分けてスキャンすることを勧め、パイプライン全体を一度に走らせない。

**理由：** 分散したタスクは**目標 1 つ**を単位に配られる。ルートドメインを目標にすると、あるノードがそのルートドメインを受け取った後、そのノードで見つかったサブドメインも同じノードで後続のモジュールを続けて実行するので、負荷が偏り、遅く、エラーも起きやすい。

**最善のやり方：**

1. **第 1 段階 — サブドメインの収集だけ**
   - `targetSource`: `general`
   - `target`: すべてのルートドメイン（複数行）
   - テンプレート：`SubdomainScan`、`SubdomainSecurity`（サブドメインのスキャン + サブドメインの乗っ取り）だけを有効にする
   - `get_task` でタスクの完了を待つ

2. **第 2 段階 — 後続のモジュール**
   - `targetSource`: `subdomain`
   - `search`: `task=="<第 1 段階のタスクの名前>"`（タスクの名前に完全一致）
   - 任意で `project` で範囲を絞る
   - テンプレート：ポートスキャン、資産のマッピング、脆弱性スキャンなど（SubdomainScan は含めなくてよい）
   - サブドメインが独立した目標として各ノードに配られ、並列の効率が上がる

Web の画面の「サブドメイン」の資産のページでタスクの名前で絞り込んでから、「サブドメインからタスクを作る」を使っても同じ効果になる。

```mermaid
flowchart LR
  A[ルートドメインの一覧] --> B[段階1: general + SubdomainScan]
  B --> C[サブドメインを保存]
  C --> D[段階2: subdomain + task==段階1のタスク名]
  D --> E[ポート/資産/脆弱性などのモジュール]
```

### 3.4 スキャンのテンプレートを作る

1. `list_plugin_modules` → モジュール名の一覧
2. `list_plugins`（`module` で絞り込める）→ 各プラグインの `hash` と既定の `parameter`
3. `create_scan_template`：`modules` で「モジュール → プラグインの hash の配列」を指定する

---

## 4. 資産の検索（`list_assets` / `count_assets`）

`count_assets` は `list_assets` と同じ `asset_type`、`search`、`filter` を使い、`{ "total": N }` を返す。Web 側の `/api/assets/common/total` に対応する。

```json
{
  "asset_type": "subdomain",
  "search": "task==\"あるタスクの名前\"",
  "filter": {"project": ["<プロジェクトのObjectID>"]}
}
```

**性能の勧め（`list_assets` / `count_assets` で共通）：** プロジェクトの条件があるときは `filter.project` で範囲を絞ることを優先する；`search` ではインデックスのあるフィールドにできるだけ `==` の完全一致か `^` の前方一致を使い（[4.3](#43-search-の検索式) を参照）、広い範囲の `=` のあいまい検索で応答を遅くするのを避ける。プロジェクトの文脈がなければ、プロジェクトの絞り込みを無理に付けなくてよい。

`filter.project` に対応する種類は [4.4](#44-filter-の厳密な絞り込み) の表を参照。

### 4.1 資産の種類 `asset_type`

`asset`、`RootDomain`、`subdomain`、`app`、`mp`、`UrlScan`、`SensitiveResult`、`DirScanResult`、`crawler`、`vulnerability`、`PageMonitoring`、`IPAsset`、`SubdomainTakerResult`

別名の例：`web`→asset、`vuln`→vulnerability、`ip`→IPAsset、`url`→UrlScan

### 4.2 パラメータの説明


| パラメータ                       | 説明                                      |
| ------------------------ | --------------------------------------- |
| `pageIndex` / `pageSize` | ページ分け。既定は 1 / 20                            |
| `search`                 | 検索式（次の節を参照）                              |
| `filter`                 | 厳密な絞り込みの JSON（次の節を参照）                          |
| `sort`                   | UrlScan、DirScanResult だけが `length` での並べ替えに対応 |
| `sid`                    | SensitiveResult だけ：機微な情報の規則の名前                |


`search` と `filter` は**同時に使える**。

### 4.3 search の検索式

独自の DSL（**SQL ではない**）：


| 演算子  | 意味   | インデックス | 例                          |
| ---- | ---- | ---- | --------------------------- |
| `=`  | あいまい一致（regex） | 使わない | `domain=example`            |
| `==` | 厳密な一致（完全一致） | **使う** | `port==443`                 |
| `!=` | 除外   | — | `port!="80"`                |
| `&&` | かつ    | — | `domain==example.com && port==443` |
| `||` | または    | — | `title=admin || body=login` |


**インデックスと演算子：** `domain`、`ip`、`port`、`title` などのフィールドにはインデックスがあるが、インデックスを使えるのは **`==` の完全一致**か**値が `^` で始まる前方一致**（例 `domain=^example.com`）だけ；**`=` は regex のあいまい一致に変わり、インデックスを使えない**ので、データが多いと遅くなりやすい。

**どの種類でも使える search のフィールド：** `tag`、`task`（タスクの名前）、`rootDomain`

**project は search に書けない**（効かないか、`&&` と組み合わせるとエラーになる）。プロジェクトで絞るには `filter.project` を使う。

**種類ごとによく使う search のフィールド：**


| asset_type           | フィールド                                                                                  |
| -------------------- | ----------------------------------------------------------------------------------- |
| asset                | domain, ip, port, service, app, title, statuscode, icon, banner, type, body, header |
| RootDomain           | domain, icp, company                                                                |
| subdomain            | domain, ip, type, value                                                             |
| app                  | name, icp, company, category, description, url, apk                                 |
| mp                   | name, icp, company, category, description, url                                      |
| UrlScan              | url, input, source, resultId, type                                                  |
| SensitiveResult      | url, sname, body, info, md5                                                         |
| DirScanResult        | url, statuscode, redirect, length                                                   |
| vulnerability        | url, vulname, matched, request, response, level                                     |
| crawler              | url, method, body, resultId                                                         |
| PageMonitoring       | url, hash, diff, response                                                           |
| IPAsset              | ip, domain, port, service, webServer, app                                           |
| SubdomainTakerResult | domain, value, type, response                                                       |


**search の例：**

- `domain==www.example.com && port==443`（完全一致、インデックスを使う）
- `domain=^example.com`（前方一致、インデックスを使う）
- `ip==192.168.1.1`
- `task=="あるタスクの名前"`
- `level==high`（vulnerability）
- `statuscode==200`（DirScanResult）

あいまいに含むものを探したいときだけ `=` を使う。例 `title=admin`（インデックスを使わないので、プロジェクトなどの条件と合わせて範囲を絞るのがよい）。

### 4.4 filter の厳密な絞り込み

JSON のオブジェクト：同じ key の複数の値は **OR**、違う key どうしは **AND**。

**プロジェクトの条件があるときは `project` を優先する：** ユーザーか文脈でプロジェクトがはっきりしていて、asset_type が `project` に対応するなら、付けて範囲を絞るべき；プロジェクトの情報がなければ無理に付けない。


| filter key   | 意味       | 値の説明                                                     |
| ------------ | -------- | -------------------------------------------------------- |
| `project`    | 属するプロジェクト     | **ObjectID**。`list_projects` / `list_projects_data` で取る |
| `task`       | 元のタスク     | **タスクの名前**。`list_tasks` の `name`                         |
| `port`       | ポート       | 例 `"443"`                                                |
| `service`    | サービス/プロトコル    | 例 `"https"`                                              |
| `app`        | アプリのフィンガープリント     | 例 `"Nginx"`                                              |
| `icon`       | アイコンの hash  |                                                          |
| `statuscode` | HTTP の状態コード | 主に asset に使う                                               |
| `status`     | 状態       | UrlScan/DirScan の HTTP コード；脆弱性/機微な情報の対応の状態                       |
| `level`      | 脆弱性の深刻度     | critical / high / medium / low / info                    |
| `type`       | 種類       | 例 サブドメインのレコードの種類 A、CNAME                                         |
| `color`      | 機微な情報の規則の色   | SensitiveResult                                          |
| `sname`      | 機微な情報の規則の名前    | SensitiveResult                                          |
| `tags`       | タグ       |                                                          |


**種類ごとに使える filter key：**


| asset_type                            | filter key                                                      |
| ------------------------------------- | --------------------------------------------------------------- |
| asset                                 | project, port, service, app, icon, statuscode, type, task, tags |
| RootDomain                            | project, tags                                                   |
| subdomain                             | project, type, task, tags                                       |
| app / mp                              | project, tags                                                   |
| UrlScan                               | status, tags                                                    |
| DirScanResult                         | status, tags                                                    |
| SensitiveResult                       | status, color, sname, tags                                      |
| crawler                               | project, task, tags                                             |
| vulnerability                         | project, level, status, task, tags                              |
| PageMonitoring / SubdomainTakerResult | tags                                                            |
| IPAsset                               | project, port, service, app                                     |


**filter の例：**

```json
{"project": ["<プロジェクトのObjectID>"], "port": ["443"]}
```

**組み合わせた検索の例：**

```json
{
  "asset_type": "asset",
  "search": "domain=^baidu && port==443",
  "filter": {"project": ["<プロジェクトのObjectID>"]},
  "pageIndex": 1,
  "pageSize": 10
}
```

**注意：**

- プロジェクトの条件があるときは `filter.project` を優先して付ける（対応しているなら）；プロジェクトの文脈がなければ無理に付けなくてよい
- `filter.project` にプロジェクトの表示名を入れない
- わかっている値には `==`、前方一致には `^` を使う；大きなテーブルで `=` のあいまい一致を乱用しない
- UrlScan の HTTP の状態は `filter.status` を使う；DirScanResult は search で `statuscode==200` を使える
- SensitiveResult を規則の名前で：`search` で `sname=規則の名前`、または `filter.sname`

### 4.5 並べ替え sort

**UrlScan**、**DirScanResult** だけが対応：

```json
{"length": "ascending"}
```

ほかの種類は `sort` を無視し、時刻の既定の順に並べる。

---

## 5. スキャンのテンプレートのモジュール名

`TargetHandler`、`SubdomainScan`、`SubdomainSecurity`、`PortScanPreparation`、`PortScan`、`PortFingerprint`、`AssetMapping`、`AssetHandle`、`URLScan`、`WebCrawler`、`URLSecurity`、`DirScan`、`VulnerabilityScan`、`PassiveScan`

---

## 6. トラブルシューティング


| 現象        | 対処                                                 |
| --------- | -------------------------------------------------- |
| MCP にツールがない   | URL、API Key、ScopeSentry が動いているかを調べる                    |
| 401 / 403 | API Key を作り直すか換える                                    |
| 資産が見つからない     | `filter.project` が ObjectID かを確かめる；search に project を書かない |
| テンプレート/タスクの作成に失敗 | `template` はテンプレートの ObjectID でなければならない；`node` にはオンラインのノードの名前を入れる            |
| 検索がとても遅い/止まる   | プロジェクトがあれば `filter.project` を付ける；search ではインデックスのあるフィールドに `==` か `^` の前方一致を使い、`=` を減らす；`pageSize` を小さくする |


---

