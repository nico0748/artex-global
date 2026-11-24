package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// releaseCache は GitHub の枠を守る層：認証していない API は IP ごとに1時間 60 回までで、
// 上部のバーの「新しい版がある」の案内はページ全体を読み込むたびに1回調べる。キャッシュが効かなくなると、ユーザーがタブをいくつか
// 開いただけで枠を使い切り、その後本当に更新したいときにかえって調べられなくなる。

func newTestCache(fetch func(context.Context, *http.Client) (*selfupdate.Release, error)) *releaseCache {
	return &releaseCache{fetch: fetch}
}

func TestReleaseCacheServesFromCache(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	for range 5 {
		rel, err := c.get(t.Context(), nil, false)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if rel.TagName != "v0.3.8" {
			t.Fatalf("TagName = %q", rel.TagName)
		}
	}
	if calls != 1 {
		t.Errorf("5 回の検索で元から取るのは 1 回だけのはず。実際は %d 回", calls)
	}
}

func TestReleaseCacheForceBypasses(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// ユーザーが「更新を確認」を押したら必ずリアルタイムの結果を得なければならない。さもないと出たばかりの版がキャッシュの期限切れまで見えない。
	if _, err := c.get(t.Context(), nil, true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("force はキャッシュを迂回するはず。元から取るのは 2 回のはずが、実際は %d 回", calls)
	}
}

func TestReleaseCacheExpiresAfterTTL(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// 保存した時刻をちょうど期限切れまで戻し、TTL に達したのをまねる。
	c.at = time.Now().Add(-releaseTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("TTL の期限切れの後は元から取り直すはず。2 回のはずが、実際は %d 回", calls)
	}
}

func TestReleaseCacheUsesShorterTTLForErrors(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return nil, errors.New("github に届かない")
	})

	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("エラーを返すはず")
	}
	// 失敗の結果も少しの間キャッシュする。さもないと GitHub に届かないとき、ページを読み込むたびにタイムアウトを1回むだに待つ。
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("エラーを返すはず")
	}
	if calls != 1 {
		t.Errorf("エラーは短くキャッシュするはず。元から取るのは 1 回のはずが、実際は %d 回", calls)
	}

	// ただしエラーの TTL は成功のものよりはっきり短くなければならず、ネットワークが戻ったらすぐ自然に直る必要がある。
	if releaseErrTTL >= releaseTTL {
		t.Fatalf("エラーの TTL(%v) は成功の TTL(%v) より短くなければならない", releaseErrTTL, releaseTTL)
	}
	c.at = time.Now().Add(-releaseErrTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("エラーを返すはず")
	}
	if calls != 2 {
		t.Errorf("エラーの TTL の期限切れの後は再試行するはず。2 回のはずが、実際は %d 回", calls)
	}
}

func TestReleaseCacheDoesNotPoisonOnCallerCancel(t *testing.T) {
	good := &selfupdate.Release{TagName: "v0.3.8"}
	c := newTestCache(func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return good, nil
	})
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}

	// 訪問者がタブを閉じるとリクエストが取り消される。それは GitHub に問題があることを意味しないので、決して「取り消された」を
	// キャッシュに書いてはならない——さもないとその後 30 分の間、どの訪問者も訳のわからないエラーを受け取る。
	c.fetch = func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return nil, ctx.Err()
	}
	c.at = time.Now().Add(-releaseTTL - time.Second) // キャッシュを期限切れにし、元から取らせる

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.get(ctx, nil, false); err == nil {
		t.Fatal("呼び出し元が取り消したときは、エラーをそのまま渡すべき")
	}

	// 重要な不変条件：取り消された1回は何の痕跡も残さない——キャッシュには「取り消された」というエラーもなく、
	// 前回のよい結果もそのまま保っている。
	if c.err != nil {
		t.Fatalf("取り消しのエラーはキャッシュに書くべきでない。得たのは %v", c.err)
	}
	if c.rel == nil || c.rel.TagName != "v0.3.8" {
		t.Fatalf("キャッシュは前回のよい結果を保つべき。得たのは %+v", c.rel)
	}

	// その取り消しでは新しいデータを何も得ていないので、次の訪問者は元から取り直すのが当然で——しかも正常に結果を得られ、
	// 前回の取り消しの巻き添えにならない。
	c.fetch = func(context.Context, *http.Client) (*selfupdate.Release, error) {
		return good, nil
	}
	rel, err := c.get(t.Context(), nil, false)
	if err != nil {
		t.Fatalf("取り消しの後の正常なリクエストはエラーを返すべきでない: %v", err)
	}
	if rel == nil || rel.TagName != "v0.3.8" {
		t.Fatalf("正常な結果を得るはず。得たのは %+v", rel)
	}
}
