package repository

// keys_logic_test.go 是 internal/repository 的白盒键空间门禁。
//
// 为什么放在这里：logic 单测的注入缝在 Cacher 之下（见 internal/logic/fakes_test.go 的
// 「覆盖边界」），用例只看得到 (mid, feedID) 级身份，看不见真实 Redis 键名，也看不见有没有设过期。
// 这两条恰恰是 feed 读写两侧必须一致的约定：
//   - 键格式改了但只改一处，读写会静默分裂（PushFeed 写 feed:inbox:{mid}、PullFeed 读另一个键，
//     表现为「所有人关注流永远为空」且不报错）；
//   - 未读/收件箱/置顶都没有 TTL（本期靠 DB 投影兜底），一旦有人加了 Setex，
//     红点数会在过期后依赖回源，行为与时序耦合。
//
// 因此这里直接锁源文件里的常量与调用形态：不连 Redis、不连 MySQL。

import (
	"os"
	"strings"
	"testing"
)

// TestFeedKeyFormats 锁五个键空间的拼接结果（与 README/AGENTS.md §5 的数据所有权一致）。
func TestFeedKeyFormats(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
		own  string // 谁写这把键
	}{
		{"收件箱 ZSet", keyInbox(7001), "feed:inbox:7001", "本服务 PushFeed 写扩散 / PullFeed 读"},
		{"发件箱 ZSet", keyOutbox(101), "feed:outbox:101", "本服务 PushFeed 写 / ListUserFeed 读"},
		{"置顶集合", keyPin(101), "feed:pin:101", "本服务 PinFeed/UnpinFeed 读写"},
		{"未读计数器", keyUnread(7001), "feed:unread:7001", "本服务 PushFeed 递增 / ClearUnread 清零"},
		{"粉丝集合", keyFollowers(101), "feed:followers:101", "social-graph 写，本服务只读"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s：%s = %q, want %q（%s）", tc.name, tc.name, tc.got, tc.want, tc.own)
		}
	}
	// 关注流与发件箱必须是两条独立键空间：同一条动态在两侧的键不能撞。
	if keyInbox(101) == keyOutbox(101) {
		t.Errorf("inbox 与 outbox 键冲突：%s", keyInbox(101))
	}
	if keyPin(101) == keyUnread(101) {
		t.Errorf("pin 与 unread 键冲突：%s", keyPin(101))
	}
}

// TestNoMidCollidesAcrossNamespaces mid 相同的两条键空间必须靠前缀区分，
// 而不是靠数据类型（ZSet/Set/String 同名会让误用变成 WRONGTYPE 错误）。
func TestNoMidCollidesAcrossNamespaces(t *testing.T) {
	seen := map[string]string{}
	for _, k := range []string{keyInbox(1), keyOutbox(1), keyPin(1), keyUnread(1), keyFollowers(1)} {
		if prev, ok := seen[k]; ok {
			t.Fatalf("键空间重名：%s 同时属于 %s 与 %s", k, prev, k)
		}
		seen[k] = k
	}
}

// TestFeedKeysHaveNoTTL 锁「所有 feed 键都不设过期」这条不变量：
// 源文件里不允许出现 Setex/Expire/PExpire/ExpireNX 之类的调用。
// 将来若真要加 TTL，必须同时改这里并补上「过期后回源」的用例，而不是静默加一行。
func TestFeedKeysHaveNoTTL(t *testing.T) {
	src, err := os.ReadFile("repository.go")
	if err != nil {
		t.Fatalf("读 repository.go 失败：%v", err)
	}
	body := string(src)
	for _, banned := range []string{"Setex", "SetexCtx", "Expire", "ExpireCtx", "PExpire", "ExpireNX", "ExpireXX"} {
		if strings.Contains(body, banned) {
			t.Errorf("repository.go 出现了 %q：feed 的键空间约定是「无 TTL，靠 DB 投影兜底」，"+
				"加过期必须同时补回源用例", banned)
		}
	}
	// 反向确认写侧确实用的是无过期版本（SetCtx / IncrbyCtx），否则上面那条断言是空的。
	for _, required := range []string{"SetCtx", "IncrbyCtx", "SaddCtx", "ZaddCtx"} {
		if !strings.Contains(body, required) {
			t.Errorf("repository.go 里找不到 %q：缓存写路径形态变了，本门禁的口径需要重审", required)
		}
	}
}

// TestFollowersSetIsReadOnlyHere 锁数据所有权：本包不得出现任何写粉丝集合的调用
// （SADD/SREM 只允许出现在 feed:pin 键空间上）。
func TestFollowersSetIsReadOnlyHere(t *testing.T) {
	src, err := os.ReadFile("repository.go")
	if err != nil {
		t.Fatalf("读 repository.go 失败：%v", err)
	}
	body := string(src)
	if strings.Contains(body, "SaddCtx(ctx, keyFollowers") || strings.Contains(body, "SremCtx(ctx, keyFollowers") {
		t.Errorf("出现了写粉丝集合的调用：feed:followers:{mid} 由 social-graph 关注事件写入（AGENTS.md §5）")
	}
	if !strings.Contains(body, "SmembersCtx(ctx, keyFollowers") {
		t.Errorf("找不到读取粉丝集合的调用：fan-out 的读侧路径消失了")
	}
}
