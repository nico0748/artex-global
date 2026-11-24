package db

import (
	"regexp"
	"testing"
)

// 組み込みの「削除にあたるエンドポイントのパス」の規則は tool_input の JSON の文字列全体に照らすので、ケースはそのまま
// JSON の形で与え、Interceptor が実際に受け取る subject と一致させる。
func TestDeleteEndpointPathPattern(t *testing.T) {
	re := regexp.MustCompile(deleteEndpointPathPattern)

	hit := []string{
		`{"command":"curl -s 'http://t.com/api/user/delete?id=1'"}`,    // GET で削除のエンドポイントを叩く
		`{"command":"curl -X POST http://t.com/admin/delete -d id=1"}`, // POST で削除のエンドポイントを叩く
		`{"command":"curl 'http://t.com/api/deleteAll'"}`,
		`{"command":"curl 'http://t.com/api/delete_user?id=1'"}`,
		`{"command":"curl 'http://t.com/api/delete-user?id=1'"}`,
		`{"url":"http://t.com/api/remove?id=1"}`,
		`{"command":"curl http://t.com/files/unlink/3"}`,
		`{"command":"curl http://t.com/api/del?id=2"}`,
		`{"command":"curl -X POST http://t/v1/erase"}`,
		`{"command":"curl http://t/admin/destroyAll"}`, // v1 のパスの規則は後ろに続く文字を許さないので、ここで補う
	}
	for _, s := range hit {
		if !re.MatchString(s) {
			t.Errorf("当たるはずなのに通した: %s", s)
		}
	}

	// 動詞の後には区切りが必ず続く。/delivery、/details のような読み取りだけのパスを誤って止めないため。
	miss := []string{
		`{"command":"curl 'http://t.com/api/delivery?id=1'"}`,
		`{"command":"curl 'http://t.com/order/details'"}`,
		`{"command":"curl 'http://t.com/api/delta/sync'"}`,
		`{"command":"curl 'http://t.com/user/delegate'"}`,
		`{"command":"curl 'http://delete.example.com/'"}`, // 削除の動詞がパスではなくドメインに出ている
		`{"command":"curl 'http://t.com/remote/status'"}`,
		`{"command":"nmap -p80 10.0.0.1"}`,
	}
	for _, s := range miss {
		if re.MatchString(s) {
			t.Errorf("誤って止めた: %s", s)
		}
	}
}
