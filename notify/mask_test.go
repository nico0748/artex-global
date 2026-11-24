package notify

import (
	"errors"
	"strings"
	"testing"
)

func TestMaskedValueHidesBodyButKeepsTailHint(t *testing.T) {
	const secret = "https://oapi.dingtalk.com/robot/send?access_token=abcdef123456"
	got := MaskedValue(secret)
	if strings.Contains(got, "abcdef123456") {
		t.Fatalf("マスク値が完全な認証情報を漏らした: %q", got)
	}
	if strings.Contains(got, "oapi.dingtalk.com") {
		t.Fatalf("マスク値はアドレスの本体を見せるべきでない: %q", got)
	}
	// 末尾6文字は残す。そうすればユーザーはどのボットか見分けられる。
	if !strings.HasSuffix(got, "123456") {
		t.Fatalf("見分けの手がかりとして末尾6文字を残すべき: %q", got)
	}
	if !IsMasked(got) {
		t.Fatalf("マスク値は IsMasked で識別できなければならない: %q", got)
	}
}

func TestMaskedValueShortSecretGivesNoHint(t *testing.T) {
	// 短い認証情報でも末尾6文字を見せると、認証情報全体を見せるのと同じになる。
	for _, s := range []string{"abc", "abcdef", ""} {
		got := MaskedValue(s)
		if got != MaskedPrefix {
			t.Fatalf("長さ %d の認証情報には末尾の手がかりを与えるべきでないが、%q を得た", len(s), got)
		}
		if s != "" && strings.Contains(got, s) {
			t.Fatalf("マスク値に元の値が含まれている: %q", got)
		}
	}
}

func TestMaskConfigMasksOnlySecrets(t *testing.T) {
	cfg := map[string]any{
		"webhook": "https://example.com/hook?token=SECRETVALUE",
		"secret":  "SECtest123456",
		"port":    float64(587),
		"host":    "smtp.example.com",
	}
	masked := MaskConfig(KindDingTalk, cfg)
	for _, k := range []string{"webhook", "secret"} {
		s, _ := masked[k].(string)
		if !IsMasked(s) {
			t.Errorf("%s はマスクされるべきだが、%q を得た", k, s)
		}
	}
	// 認証情報でないフィールドはそのまま残すこと。さもないと UI で表示できない。
	if masked["port"] != float64(587) {
		t.Errorf("認証情報でないフィールド port は変えるべきでない: %v", masked["port"])
	}
}

func TestMaskConfigUnknownKindReturnsEmpty(t *testing.T) {
	// チャネルの種類が識別できないときは、認証情報を含みうる元の内容を返すくらいなら、UI に空の設定を表示させるほうがよい。
	got := MaskConfig("nope", map[string]any{"webhook": "https://x/y?token=LEAK"})
	if len(got) != 0 {
		t.Fatalf("不明なチャネルの種類では空の設定を返すべきだが、%v を得た", got)
	}
}

func TestMaskConfigDoesNotMutateInput(t *testing.T) {
	// マスクは表示層の動作であり、逆に DB の本物の値を変えてはならない。
	cfg := map[string]any{"webhook": "https://example.com/hook", "secret": "SECtest123456"}
	_ = MaskConfig(KindDingTalk, cfg)
	if IsMasked(cfg["secret"].(string)) {
		t.Fatal("MaskConfig が引数を変更した。本物の認証情報がマスク値で上書きされてしまう")
	}
}

func TestMergeConfigKeepsStoredOnMaskedIncoming(t *testing.T) {
	stored := map[string]any{"webhook": "https://real/hook", "secret": "REALSECRET", "method": "POST"}
	// ユーザーは method だけを変え、ブラウザはマスク値+新しい method を送った。
	incoming := map[string]any{
		"webhook": MaskedValue("https://real/hook"),
		"secret":  MaskedValue("REALSECRET"),
		"method":  "PUT",
	}
	got := MergeConfig(stored, incoming)
	if got["webhook"] != "https://real/hook" || got["secret"] != "REALSECRET" {
		t.Fatalf("マスクされたフィールドは DB の元の値を残すべきだが、%v を得た", got)
	}
	if got["method"] != "PUT" {
		t.Fatalf("変更されたフィールドは反映されるべきだが、%v を得た", got["method"])
	}
}

func TestMergeConfigEmptyStringClears(t *testing.T) {
	stored := map[string]any{"webhook": "https://real/hook", "secret": "REALSECRET"}
	got := MergeConfig(stored, map[string]any{"secret": ""})
	if _, ok := got["secret"]; ok {
		t.Fatalf("空文字列はそのフィールドを消去すべきだが、%v を得た", got)
	}
	// 触れられていないフィールドは残す（部分更新の意味）。
	if got["webhook"] != "https://real/hook" {
		t.Fatalf("触れられていないフィールドは残すべきだが、%v を得た", got)
	}
}

func TestMergeConfigKeepsUnmentionedStoredKeys(t *testing.T) {
	stored := map[string]any{"host": "smtp.example.com", "port": float64(587), "password": "pw"}
	got := MergeConfig(stored, map[string]any{"port": float64(465)})
	if got["host"] != "smtp.example.com" || got["password"] != "pw" {
		t.Fatalf("触れられていないフィールドは残すべきだが、%v を得た", got)
	}
	if got["port"] != float64(465) {
		t.Fatalf("触れられたフィールドは更新すべきだが、%v を得た", got["port"])
	}
}

// TestPrepareConfigUpdateBlocksDestinationSwap はこのパッケージで最も重要なセキュリティ上の不変条件：
// **宛先を変えても古い認証情報を持っていかせない**。
//
// これらのテストケースは攻撃の形をした入力（宛先だけ変えて認証情報には触れない）を使っており、
// 「防御の処理にとって正しい入力」ではない——後者だけをテストすると、防御が効いていなくてもすべて緑になってしまう。
func TestPrepareConfigUpdateBlocksDestinationSwap(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		stored   map[string]any
		incoming map[string]any
		// wantMissing は名前を挙げられると期待する認証情報のキー。
		wantMissing string
	}{
		{
			name: "汎用 Webhook で宛先を変えて Authorization ヘッダーを引き継ごうとする",
			kind: KindWebhook,
			stored: map[string]any{
				"url":     "https://legit.example.com/hook",
				"headers": map[string]any{"Authorization": "Bearer REAL-TOKEN"},
			},
			incoming:    map[string]any{"url": "https://attacker.tld/c"},
			wantMissing: "headers",
		},
		{
			name:        "Telegram で base_url を変えて Bot Token を自分のエンドポイントに送ろうとする",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld"},
			wantMissing: "bot_token",
		},
		{
			name:        "メールで SMTP ホストを変えてパスワードを渡させようとする",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"host": "smtp.attacker.tld"},
			wantMissing: "password",
		},
		{
			name:        "メールで TLS を切る場合もパスワードについて改めて態度を示す必要がある",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "tls": false, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"tls": true},
			wantMissing: "password",
		},
		{
			// マスク値 = 「古い認証情報を引き継ぐ」なので、宛先の変更という文脈では同じく拒否しなければならない。
			name:        "マスクされた認証情報を返す + 新しい宛先",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld", "bot_token": MaskedValue("123456:REAL")},
			wantMissing: "bot_token",
		},
		{
			name:        "DingTalk で Webhook を変えて署名の鍵を引き継ごうとする",
			kind:        KindDingTalk,
			stored:      map[string]any{"webhook": "https://oapi.dingtalk.com/robot/send?access_token=OLD", "secret": "REALSEC"},
			incoming:    map[string]any{"webhook": "https://attacker.tld/hook"},
			wantMissing: "secret",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := PrepareConfigUpdate(tc.kind, tc.stored, tc.incoming)
			if err == nil {
				t.Fatalf("宛先を変えたのに認証情報について改めて態度を示していないので拒否すべきだが、設定 %v を得た", merged)
			}
			var target *ErrDestinationChangedWithoutCredentials
			if !errors.As(err, &target) {
				t.Fatalf("API が操作可能な案内を出せるよう専用のエラー型を返すべきだが、%T: %v を得た", err, err)
			}
			found := false
			for _, m := range target.Missing {
				if m == tc.wantMissing {
					found = true
				}
			}
			if !found {
				t.Fatalf("欠けている認証情報のキー %q の名前を挙げるべきだが、%v を得た", tc.wantMissing, target.Missing)
			}
			// エラーメッセージは操作する人に直し方を示せるものであること。
			if !strings.Contains(err.Error(), tc.wantMissing) {
				t.Errorf("エラーメッセージは %q に触れるべき: %v", tc.wantMissing, err)
			}
		})
	}
}

// TestPrepareConfigUpdateAllowsLegitimateEdits は逆のテストケース：通常の編集を誤って止めてはならない。
// さもないとこの防御は「うるさすぎる」という理由で回避されたり削除されたりする。
func TestPrepareConfigUpdateAllowsLegitimateEdits(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		stored   map[string]any
		incoming map[string]any
	}{
		{
			name:     "名前だけを変える（設定はそのまま返す）",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "headers": map[string]any{"Authorization": "Bearer REAL"}},
			incoming: map[string]any{"url": MaskedValue("https://legit.example.com/hook")},
		},
		{
			name:     "リクエストのメソッドだけを変え、宛先と認証情報には触れない",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "method": "POST"},
			incoming: map[string]any{"method": "PUT"},
		},
		{
			name:     "宛先を変え、**同時に**新しい認証情報を渡す",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": map[string]any{"Authorization": "Bearer NEW"}},
		},
		{
			name:     "宛先を変え、もう認証情報が要らないことを明示的に宣言する",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": ""},
		},
		{
			name:     "Telegram で chat_id を変える（宛先ではない）",
			kind:     KindTelegram,
			stored:   map[string]any{"bot_token": "t", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming: map[string]any{"chat_id": "-100200"},
		},
		{
			name:     "メールで受信者を変える（宛先ではない）",
			kind:     KindEmail,
			stored:   map[string]any{"host": "smtp.corp.com", "port": 587, "password": "PW", "from": "a@b.c", "to": []any{"x@y.z"}},
			incoming: map[string]any{"to": []any{"new@y.z"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := PrepareConfigUpdate(tc.kind, tc.stored, tc.incoming)
			if err != nil {
				t.Fatalf("正当な編集が誤って止められた: %v", err)
			}
			if merged == nil {
				t.Fatal("マージ結果を返すべき")
			}
		})
	}
}

// TestPrepareConfigUpdatePortTypeTolerance は誤判定しやすい細部を扱う：
// フロントエンドが送るポートは JSON の number（float64）で、DB から読み戻しても float64 だが、
// 2つの値の型が違うことがある（int と float64 など）。== で比べると「変えていない」を「変えた」と判定し、
// 名前だけを変えたユーザーに「パスワードを入力し直してください」と出してしまう——誤報が続くと人はこの防御を信用しなくなる。
func TestPrepareConfigUpdatePortTypeTolerance(t *testing.T) {
	stored := map[string]any{"host": "smtp.corp.com", "port": float64(587), "password": "PW"}
	// 同じポートを int の形で送る。
	if _, err := PrepareConfigUpdate(KindEmail, stored, map[string]any{"port": 587}); err != nil {
		t.Fatalf("ポートの値が同じ（型だけが違う）ときは宛先の変更と判定すべきでない: %v", err)
	}
	// 本当にポートを変えたら必ず止める。
	if _, err := PrepareConfigUpdate(KindEmail, stored, map[string]any{"port": 25}); err == nil {
		t.Fatal("ポートの変更は止められるべき")
	}
}

// TestPrepareConfigUpdateSurvivesRepeatedSaveWithBlankDestination は「空にできる
// 宛先フィールド」の経路を扱う：Telegram の base_url を空にすると公式の API のアドレスを使う。
//
// 以前はここで、チャネルが2回目の保存から永久に保存できなくなっていた：
//
//	新規作成時に DB に base_url:"" を保存（作成の経路ではフロントエンドが送った config をそのまま保存し、MergeConfig を通らない）
//	→ 1回目の保存で、MergeConfig が空文字列を明示的な消去とみなし、そのキーを delete
//	→ 2回目の保存で、incoming はまだ "" なのに stored にはそのキーがもうなく、「宛先が変わった」と判定される
//	→ bot_token はマスクされた返却値 → 400「宛先が変わりました。認証情報のフィールドも入力し直してください」
//
// ユーザーは何も変えていないのに、Bot Token を貼り直さない限り二度と保存できなくなる。
func TestPrepareConfigUpdateSurvivesRepeatedSaveWithBlankDestination(t *testing.T) {
	stored := map[string]any{"bot_token": "123:ABC", "chat_id": "-100", "base_url": ""}

	// フロントエンドの buildConfig() は、そのチャネルの各フィールドの定義ごとに値を1つ送る：認証情報にはマスクを入れ、
	// 空のテキストボックスは空文字列を送る。ここではその出力をそのまま再現し、「変えたキー」だけを送るのではない。
	submit := func() map[string]any {
		return map[string]any{
			"bot_token": MaskedValue("123:ABC"),
			"chat_id":   "-100",
			"base_url":  "",
		}
	}

	// 1回目の保存：チャネルの名前だけを変え、config はそのまま返す。
	merged, err := PrepareConfigUpdate(KindTelegram, stored, submit())
	if err != nil {
		t.Fatalf("1回目の保存が誤って止められた: %v", err)
	}
	if _, ok := merged["base_url"]; ok {
		t.Fatal("前提が変わった：空文字列は MergeConfig で削除されるべき——このテストケースが扱うのはまさに「キーが消えたあと」の段階")
	}

	// 2回目の保存：送る内容は前回とまったく同じで、ユーザーは何も変えていない。
	merged2, err := PrepareConfigUpdate(KindTelegram, merged, submit())
	if err != nil {
		t.Fatalf("2回目の保存が誤って止められた（ユーザーは何も変えていない）: %v", err)
	}
	// 3回目：「1回だけ失敗する」のではなく、安定して保存できることを確かめる。
	if _, err := PrepareConfigUpdate(KindTelegram, merged2, submit()); err != nil {
		t.Fatalf("3回目の保存が誤って止められた: %v", err)
	}
	// 認証情報はずっと残っていなければならず、空文字列の処理に巻き込まれて消えてはならない。
	if got := merged2["bot_token"]; got != "123:ABC" {
		t.Fatalf("Bot Token は元の値を引き継ぐべきだが、%v を得た", got)
	}
}

// TestPrepareConfigUpdateStillGuardsBlankDestinationChanges は前のテストケースと対になる
// アサーション：空文字列と「キーが存在しない」を同等とみなしても、本当の宛先の変更を見逃しては**ならない**。
// この2つの方向はどちらも実際に認証情報が外へ送られる経路だ——Telegram の Bot Token は URL のパスに入っており、
// base_url を変えることは Token を新しい宛先に送るのと同じだ。
func TestPrepareConfigUpdateStillGuardsBlankDestinationChanges(t *testing.T) {
	// 方向その1：「空」（公式のアドレス）から自前のアドレスに変える。
	official := map[string]any{"bot_token": "123:ABC", "chat_id": "-100"}
	if _, err := PrepareConfigUpdate(KindTelegram, official, map[string]any{
		"bot_token": MaskedValue("123:ABC"),
		"base_url":  "https://tg-proxy.attacker.tld",
	}); err == nil {
		t.Fatal("公式のアドレスから自前のアドレスに変えるときは Token の再入力を求めなければならない")
	}

	// 方向その2：自前のアドレスを消す（= 公式の API に戻す）のも宛先の変更である。
	proxied := map[string]any{"bot_token": "123:ABC", "base_url": "https://proxy.internal/bot"}
	if _, err := PrepareConfigUpdate(KindTelegram, proxied, map[string]any{
		"bot_token": MaskedValue("123:ABC"),
		"base_url":  "",
	}); err == nil {
		t.Fatal("自前のアドレスを消す（公式の API に戻す）のも宛先の変更なので、Token の再入力を求めなければならない")
	}
}

func TestDestinationKeysDeclaredForEveryKind(t *testing.T) {
	// SecretKeys と同じ理屈：チャネルが宛先のキーの宣言を忘れると、PrepareConfigUpdate はそれを守れない。
	for kind, ch := range registry {
		if len(ch.DestinationKeys()) == 0 {
			t.Errorf("チャネル %s が宛先のキーを宣言していないので、宛先の変更で認証情報が持ち出されるのを防げない", kind)
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("チャネル %s が認証情報のキーを宣言していない", kind)
		}
	}
}

func TestSecretKeysDeclaredForEveryKind(t *testing.T) {
	// コンパイラがすでに各チャネルに SecretKeys の実装を強制しているが、ここでも「マスクについて
	// 白紙で出しているチャネルがない」ことを改めて確かめる——空のスライスを返すチャネルは、認証情報が平文でブラウザに返されることを意味する。
	expect := map[string]bool{
		KindDingTalk: true, KindFeishu: true, KindWeCom: true,
		KindWebhook: true, KindTelegram: true, KindEmail: true,
	}
	for kind, ch := range registry {
		if !expect[kind] {
			t.Errorf("チャネル %s のマスクの期待値がテストに登録されていない", kind)
			continue
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("チャネル %s が認証情報のフィールドを1つも宣言しておらず、その設定は平文で返される", kind)
		}
	}
}

// TestPrepareConfigUpdateRejectsMaskedInContainer は監査で指摘された穴を扱う：
// マスクの目印を**文字列でない**構造（webhook.headers はオブジェクトなど）に入れると、
// MergeConfig は「文字列で接頭辞付き」のものしかマスクと認めないので、リテラルの "__masked__" が
// 本物のヘッダーの値として DB に保存される——以後の認証は黙って無効になり、何のエラーも出ない。
func TestPrepareConfigUpdateRejectsMaskedInContainer(t *testing.T) {
	stored := map[string]any{
		"url":     "https://legit.example.com/hook",
		"headers": map[string]any{"Authorization": "Bearer REAL"},
	}
	// オブジェクトの内部にマスクの目印が紛れ込んでいる。
	incoming := map[string]any{
		"headers": map[string]any{"Authorization": MaskedPrefix},
	}
	if _, err := PrepareConfigUpdate(KindWebhook, stored, incoming); err == nil {
		t.Fatal("構造体の内部に紛れ込んだマスクの目印は拒否すべき（さもないとリテラルが DB に保存される）")
	}
	// オブジェクト全体を送る（本物の新しい値）場合は通常どおり受け付ける。
	ok := map[string]any{"headers": map[string]any{"Authorization": "Bearer NEW"}}
	if _, err := PrepareConfigUpdate(KindWebhook, stored, ok); err != nil {
		t.Fatalf("新しいリクエストヘッダーを普通に送るのは止めるべきでない: %v", err)
	}
}
