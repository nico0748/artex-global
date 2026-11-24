package notify

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// このファイルは汎用 Webhook のテンプレートの**能力の境界**を固定する。
//
// ここはパッケージ全体で唯一「ユーザーが渡した文字列がコードとして評価される」場所なので、何ができて
// 何ができないかをはっきりさせ、その性質をテストで固定する必要がある——さもないと将来だれかが気軽にテンプレートのコンテキストに
// メソッドを足したり、FuncMap に readFile を足したりして、能力の面が黙って広がり、しかも diff は
// 無害な小さい関数にしか見えない。

// TestTemplateContextHasNoMethods が最も重要なテスト。
//
// text/template は公開メソッドを呼ぶ（{{.Foo}} はフィールドの取得もメソッドの呼び出しもできる）。そのためテンプレートのコンテキストが
// 公開メソッドを持つ**いずれかの**型に触れられれば、それらのメソッドをテンプレートの作者に公開したのと同じになる。
// この機能のコンテキストはわざとすべて純粋なデータ（公開フィールドだけで、メソッドはゼロ）にしている。
//
// これが失敗したら：だれかが webhookTemplateData / webhookItem にメソッドを足したということだ。
// 通すと決める前に、そのメソッドがテンプレートから公開したくないものを読むのに使えないかをよく考えること。
func TestTemplateContextHasNoMethods(t *testing.T) {
	for _, v := range []any{webhookTemplateData{}, webhookItem{}} {
		typ := reflect.TypeOf(v)
		if n := typ.NumMethod(); n != 0 {
			var names []string
			for i := 0; i < n; i++ {
				names = append(names, typ.Method(i).Name)
			}
			t.Fatalf("%s が %d 個のメソッド（%s）を公開している：text/template はそれらを呼べるので、"+
				"これらのメソッドの能力をテンプレートの作者に開放したのと同じになる", typ.Name(), n, strings.Join(names, ", "))
		}
	}
}

// TestTemplateFuncsAreMinimal はテンプレートに公開する関数の集合を固定する。
//
// FuncMap に関数が1つ増えるごとに能力が1つ増える。いまは json / jsons だけで、役割は値を
// JSON の断片にシリアライズすること——ファイルを読めず、リクエストを送れず、コマンドを実行できない。
func TestTemplateFuncsAreMinimal(t *testing.T) {
	var got []string
	for name := range webhookTemplateFuncs {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"json", "jsons"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("テンプレートの関数の集合が変わった：%v を得たが、期待値は %v。関数を足す前に、それが能力の面を広げないことを確かめること"+
			"（ファイルを読み書きできない、ネットワークのリクエストを送れない、コマンドを実行できない）", got, want)
	}
}

// TestTemplateCannotReachUnknownData はテンプレートでの範囲外のアクセスを扱う：
// 存在しないものへのアクセスは必ず失敗すべきで、何かを返してはならない。しかも失敗のメッセージで内部のデータを持ち出してはならない。
func TestTemplateCannotReachUnknownData(t *testing.T) {
	_, err := renderWebhookBody(`{"x": {{.Environment}}, "y": {{.Env}}}`, singleMsg())
	if err == nil {
		t.Fatal("存在しないフィールドへのアクセスはエラーにすべき")
	}
	// エラーにテンプレートのコンテキストの本物の内容（脆弱性のタイトル/要約）が出てはならない。
	for _, leak := range []string{"SQLインジェクション", "引数 id"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("テンプレートのエラーがメッセージの内容 %q を漏らした: %v", leak, err)
		}
	}
}

// TestTemplateRenderFailsPermanently テンプレートの書き間違いは設定の誤りで、再試行しても直らない。
// 再試行できると判定されると、壊れたテンプレート1つで配信のたびに3回の待ち時間を無駄にする。
func TestTemplateRenderFailsPermanently(t *testing.T) {
	cfg := map[string]any{
		"url":           "https://example.com/hook",
		"body_template": `{{.Items.`,
	}
	if err := (webhookChannel{}).Validate(cfg); err == nil {
		t.Fatal("テンプレートの構文エラーは保存時に止められるべき")
	}
	// 検証をすり抜けて直接配信した場合でも、繰り返し再試行せずに恒久的な失敗と判定しなければならない。
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("壊れたテンプレートは恒久的な失敗と判定すべきだが、%v を得た", err)
	}
}

// TestTemplateCanOnlyProduceJSON は「テンプレートの描画結果は正しい JSON でなければならない」という制約を扱う。
// これでついでに「テンプレートで平文を生成して別のプロトコルを起動する」ような使い方も防げる。
func TestTemplateCanOnlyProduceJSON(t *testing.T) {
	// 正しいテンプレートは通る。
	ok := map[string]any{"url": "https://example.com/hook", "body_template": `{"t":{{json .Title}}}`}
	if err := (webhookChannel{}).Validate(ok); err != nil {
		t.Fatalf("正しいテンプレートは検証を通るべき: %v", err)
	}
	// JSON でないものを描画したら必ず拒否する（そのまま送らない）。
	bad := map[string]any{"url": "http://127.0.0.1:1/hook", "body_template": `not json {{.Count}}`}
	_, err := (webhookChannel{}).Send(context.Background(), bad, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("JSON でないものを描画したら恒久的な失敗と判定すべきだが、%v を得た", err)
	}
	if !strings.Contains(err.Error(), "올바른 JSON") {
		t.Errorf("エラーメッセージは JSON の問題であることを説明すべきだが、%v を得た", err)
	}
}
