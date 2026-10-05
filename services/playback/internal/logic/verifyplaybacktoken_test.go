package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/playback/internal/signurl"
	"go-video/services/playback/model"
	rpc "go-video/services/playback/rpc"
)

const (
	verSessionID = "01VERIFYSESSION0000000000C"
	verExpireIn  = int64(600) // 会话剩余 600 秒：超过 cacheTTLSessionMax，回填 TTL 稳定取上限
)

// seedActiveVerifySession 布一个仍然有效的 UGC 会话（只落库，缓存留空，用来测读穿回填）。
func seedActiveVerifySession(t *testing.T, e *env, expireAt int64) *model.PlaybackSession {
	t.Helper()
	return seedSession(t, e.st, &model.PlaybackSession{
		SessionId: verSessionID, ContentType: model.ContentTypeUGC, ContentId: 700, Vid: "BV700",
		Mid: 42, Platform: model.PlatformAndroid, Region: "CN",
		ObjectKey: "ugc/12/34/700.m3u8", Uri: ugcURI, RequestId: "req-ver-1",
		ExpireAt: expireAt, State: model.SessionStateActive, Ctime: expireAt - 1200,
	})
}

// verifyReq 构造一次回源校验请求；authKey 由用例决定给合法串还是伪造串。
func verifyReq(uri, authKey string) *rpc.VerifyPlaybackTokenReq {
	return &rpc.VerifyPlaybackTokenReq{
		SessionId: verSessionID,
		Uri:       uri,
		AuthKey:   authKey,
		ClientIp:  "203.0.113.9", // 只用于日志，不落库不入事件
	}
}

func verify(t *testing.T, e *env, in *rpc.VerifyPlaybackTokenReq) (*rpc.VerifyPlaybackTokenReply, error) {
	t.Helper()
	return NewVerifyPlaybackTokenLogic(context.Background(), e.svcCtx).VerifyPlaybackToken(in)
}

// TestVerifyPlaybackTokenRejectsInvalidRequests 守卫必须发生在读会话之前。
func TestVerifyPlaybackTokenRejectsInvalidRequests(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.VerifyPlaybackTokenReq
		want error
	}{
		{"缺少 session_id", &rpc.VerifyPlaybackTokenReq{Uri: ugcURI}, model.ErrMissingSessionID},
		{"uri 为空", verifyReq("", "1-a-0-b"), model.ErrInvalidObjectKey},
		{"uri 是绝对 URL", verifyReq("https://cdn.example.com/a.m3u8", "1-a-0-b"), model.ErrInvalidObjectKey},
		{"uri 带 query", verifyReq("/a.m3u8?auth_key=1-2-3-4", "1-a-0-b"), model.ErrInvalidObjectKey},
		{"uri 路径穿越", verifyReq("/ugc/../../etc/passwd", "1-a-0-b"), model.ErrInvalidObjectKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			before := e.st.log.snapshot()

			reply, err := verify(t, e, tc.in)

			wantErrIs(t, tc.name, err, tc.want)
			if reply != nil {
				t.Errorf("%s：守卫失败却返回了判定 %+v", tc.name, reply)
			}
			wantNoCall(t, tc.name, e.st, before)
		})
	}
}

// TestVerifyPlaybackTokenDeniesAreAnswersNotErrors 是回源接口的核心契约：
// 「拒绝放行」必须用 allow=false + 稳定 deny_reason 表达，而不是 gRPC 错误——
// 边缘节点要把拒绝与故障分成两类处理。因此每条拒绝都断言 err==nil、reply!=nil、
// 且没有把 playCount 累加上去。
func TestVerifyPlaybackTokenDeniesAreAnswersNotErrors(t *testing.T) {
	expire := nowPlus(verExpireIn)
	goodKey := authKeyFor(t, ugcURI, expire, "42", "a1b2c3")

	cases := []struct {
		name   string
		uri    string
		key    string
		reason string
		// setup 在布完"有效会话"之后追加改动（状态、签名器配置等）。
		setup func(t *testing.T, e *env)
	}{
		{"会话不存在", ugcURI, goodKey, denySessionNotFound, func(t *testing.T, e *env) {
			// 反向布景：删掉刚布的那一行。
			delete(e.st.sessions.rows, verSessionID)
			delete(e.st.sessions.byReq, "req-ver-1")
		}},
		{"会话已撤销（版权撤回/风控）", ugcURI, goodKey, denySessionRevoked, func(t *testing.T, e *env) {
			e.st.sessions.rows[verSessionID].State = model.SessionStateRevoked
		}},
		{"会话已撤销且签名也无效：状态优先", ugcURI, "bad", denySessionRevoked, func(t *testing.T, e *env) {
			e.st.sessions.rows[verSessionID].State = model.SessionStateRevoked
		}},
		{"签名合法但资源与会话绑定的 URI 不同（auth_key 挪用）", "/pgc/other/2.m3u8",
			authKeyFor(t, "/pgc/other/2.m3u8", expire, "42", "a1b2c3"), denyURIMismatch, nil},
		{"auth_key 格式非法", ugcURI, "1-2-3", denyBadAuthFormat, nil},
		{"auth_key 段数不对", ugcURI, "1-2-3-4-5", denyBadAuthFormat, nil},
		{"auth_key 换了 uri（签名不匹配）", ugcURI, authKeyFor(t, "/ugc/12/34/701.m3u8", expire, "42", "a1b2c3"), denySignMismatch, nil},
		{"auth_key 换了私钥（轮换后旧地址）", ugcURI,
			fmt.Sprintf("%d-%s-%s-%s", expire, "a1b2c3", "42",
				signurl.Hash(ugcURI, expire, "a1b2c3", "42", "an-rotated-out-key")), denySignMismatch, nil},
		{"auth_key 自身已过期", ugcURI, authKeyFor(t, ugcURI, nowPlus(-30), "42", "a1b2c3"), denySessionExpired, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seedActiveVerifySession(t, e, expire)
			if tc.setup != nil {
				tc.setup(t, e)
			}
			stateBefore := e.st.sessions.state(verSessionID)

			reply, err := verify(t, e, verifyReq(tc.uri, tc.key))

			wantNoErr(t, tc.name, err)
			if reply == nil {
				t.Fatalf("%s：判定 = nil, want allow=false 的明确答复", tc.name)
			}
			wantEQ(t, tc.name, "allow", reply.Allow, false)
			wantEQ(t, tc.name, "deny_reason", reply.DenyReason, tc.reason)
			wantEQ(t, tc.name, "deny_reason 非空（CDN 与客户端据此排障）", reply.DenyReason != "", true)
			// 拒绝既不能占用播放计数名额，也不能顺手改写会话状态。
			wantCount(t, tc.name, e.st.log, "cache.Incr", 0)
			wantCount(t, tc.name, e.st.log, "cache.MarkVerified", 0)
			wantCount(t, tc.name, e.st.log, "session.MarkExpired", 0)
			wantEQ(t, tc.name, "会话状态未被拒绝路径改动", e.st.sessions.state(verSessionID), stateBefore)
			// deny_reason 面向 CDN 日志，绝不能带私钥或 SQL 片段（AGENTS.md §6）。
			if strings.Contains(reply.DenyReason, "mysql") || strings.Contains(reply.DenyReason, testPrivateKey) {
				t.Errorf("%s：deny_reason 泄漏内部信息：%s", tc.name, reply.DenyReason)
			}
		})
	}
}

// TestVerifyPlaybackTokenDeniesWhenPrivateKeyMissing 服务端签名配置异常（私钥丢失）时
// 必须拒绝回源，而不是"验不了就放行"。
func TestVerifyPlaybackTokenDeniesWhenPrivateKeyMissing(t *testing.T) {
	expire := nowPlus(verExpireIn)
	e := newEnvSigner(t, "", true)
	seedActiveVerifySession(t, e, expire)

	reply, err := verify(t, e, verifyReq(ugcURI, authKeyFor(t, ugcURI, expire, "42", "a1b2c3")))

	wantNoErr(t, "私钥缺失", err)
	wantEQ(t, "私钥缺失", "allow", reply.Allow, false)
	wantEQ(t, "私钥缺失", "deny_reason", reply.DenyReason, denySignerDisabled)
	wantCount(t, "私钥缺失", e.st.log, "cache.Incr", 0)
}

// TestVerifyPlaybackTokenExpiredSessionIsAdvancedAndUncached 会话到期时即使签名仍然合法
// 也必须拒绝，并且顺手把状态推进为 expired、把缓存里的旧副本删掉——
// 否则下一个分片还能靠缓存放行。
func TestVerifyPlaybackTokenExpiredSessionIsAdvancedAndUncached(t *testing.T) {
	expired := nowPlus(-5)
	e := newEnv(t)
	seedActiveVerifySession(t, e, expired)
	// auth_key 比会话活得更久（真实场景：窗口被撤回后重签、或时钟偏差）。
	in := verifyReq(ugcURI, authKeyFor(t, ugcURI, nowPlus(600), "42", "a1b2c3"))

	reply, err := verify(t, e, in)
	wantNoErr(t, "会话到期", err)
	wantEQ(t, "会话到期", "allow", reply.Allow, false)
	wantEQ(t, "会话到期", "deny_reason", reply.DenyReason, denySessionExpired)
	wantEQ(t, "会话到期", "回带的 expire_at", reply.ExpireAt, expired)

	wantOps(t, "会话到期的处理链", e.st.log.ops, []string{
		"cache.GetSession:" + keySession(verSessionID),
		"session.FindOne:" + verSessionID,
		"cache.SetSession:" + keySession(verSessionID),
		"session.MarkExpired:" + verSessionID,
		"cache.Del:" + keySession(verSessionID),
	})
	wantEQ(t, "会话到期", "库里状态", e.st.sessions.state(verSessionID), int32(model.SessionStateExpired))
	if e.st.cache.cached(verSessionID) != nil {
		t.Errorf("会话到期后缓存里仍有可放行的旧副本")
	}
}

// TestVerifyPlaybackTokenStillDeniesWhenMarkExpiredFails 状态推进是记账，判定不是它的副产品：
// 落库失败也必须返回拒绝，而不是因为报错让 CDN 按自己的策略放行。
func TestVerifyPlaybackTokenStillDeniesWhenMarkExpiredFails(t *testing.T) {
	e := newEnv(t)
	seedActiveVerifySession(t, e, nowPlus(-5))
	e.st.sessions.failWith("MarkExpired", errors.New("playback: update denied"))

	reply, err := verify(t, e, verifyReq(ugcURI, authKeyFor(t, ugcURI, nowPlus(600), "42", "a1b2c3")))

	wantNoErr(t, "MarkExpired 失败", err)
	wantEQ(t, "MarkExpired 失败", "allow", reply.Allow, false)
	wantEQ(t, "MarkExpired 失败", "deny_reason", reply.DenyReason, denySessionExpired)
	// 写库失败时不能宣称缓存已失效：repository 在 MarkExpired 出错处直接返回。
	wantCount(t, "MarkExpired 失败", e.st.log, "cache.Del", 0)
}

// TestVerifyPlaybackTokenFirstPassCountsOnce 首次放行：读穿回填 + SETNX 打标记 + 计数加一。
// 计数标记的 TTL 必须跟着会话剩余时间（并被上限钳制），否则标记会比授权活得久。
func TestVerifyPlaybackTokenFirstPassCountsOnce(t *testing.T) {
	expire := nowPlus(verExpireIn)
	e := newEnv(t)
	seedActiveVerifySession(t, e, expire)
	before := e.st.log.snapshot()

	reply, err := verify(t, e, verifyReq(ugcURI, authKeyFor(t, ugcURI, expire, "42", "a1b2c3")))
	wantNoErr(t, "首次放行", err)
	wantEQ(t, "首次放行", "allow", reply.Allow, true)
	wantEQ(t, "首次放行", "deny_reason 必须为空", reply.DenyReason, "")
	wantEQ(t, "首次放行", "expire_at", reply.ExpireAt, expire)
	wantEQ(t, "首次放行", "play_count", reply.PlayCount, int64(1))

	wantOps(t, "首次放行的调用链", e.st.log.opsFrom(before), []string{
		"cache.GetSession:" + keySession(verSessionID),
		"session.FindOne:" + verSessionID,
		"cache.SetSession:" + keySession(verSessionID),
		fmt.Sprintf("cache.MarkVerified:%s/300", keyVerified(verSessionID)),
		"cache.Incr:" + keyPlayCount(model.ContentTypeUGC, 700),
	})
	wantEQ(t, "首次放行", "计数落库", e.st.cache.count(model.ContentTypeUGC, 700), int64(1))
	// 回源校验绝不能再打 rights：版权已经烧进 expire_at 里了。
	wantCount(t, "首次放行", e.st.log, "rights.CheckPlayable", 0)
}

// TestVerifyPlaybackTokenRepeatedVerifyCountsOnce CDN 会对同一次播放的每个分片回源，
// 第二、第三次校验只能读计数，不能再加。
func TestVerifyPlaybackTokenRepeatedVerifyCountsOnce(t *testing.T) {
	expire := nowPlus(verExpireIn)
	e := newEnv(t)
	seedActiveVerifySession(t, e, expire)
	key := authKeyFor(t, ugcURI, expire, "42", "a1b2c3")

	first, err := verify(t, e, verifyReq(ugcURI, key))
	wantNoErr(t, "第一次放行", err)
	wantEQ(t, "第一次放行", "play_count", first.PlayCount, int64(1))
	before := e.st.log.snapshot()

	second, err := verify(t, e, verifyReq(ugcURI, key))
	wantNoErr(t, "第二次放行", err)
	wantEQ(t, "第二次放行", "allow", second.Allow, true)
	wantEQ(t, "第二次放行", "play_count 不重复累加", second.PlayCount, int64(1))

	wantOps(t, "第二次回源只读缓存与计数", e.st.log.opsFrom(before), []string{
		"cache.GetSession:" + keySession(verSessionID),
		fmt.Sprintf("cache.MarkVerified:%s/300", keyVerified(verSessionID)),
		"cache.PlayCount:" + keyPlayCount(model.ContentTypeUGC, 700),
	})
	wantEQ(t, "第二次回源", "计数仍是那一次", e.st.cache.count(model.ContentTypeUGC, 700), int64(1))
}

// TestVerifyPlaybackTokenUidSegmentIsLogOnly 钉住 auth_key 的 uid 段语义（signurl 包头的约定）：
// uid 参与摘要计算，所以没有私钥就无法伪造；但服务端**不**校验它等于会话所属 mid，
// 它只是 CDN 日志字段。若将来要把播放授权收紧到"地址只能本人用"，改动点就在这里，
// 该用例会失败并强制补上对应的实现与文档。
func TestVerifyPlaybackTokenUidSegmentIsLogOnly(t *testing.T) {
	expire := nowPlus(verExpireIn)

	t.Run("uid 与会话 mid 不同仍然放行", func(t *testing.T) {
		e := newEnv(t)
		seedActiveVerifySession(t, e, expire)

		reply, err := verify(t, e, verifyReq(ugcURI, authKeyFor(t, ugcURI, expire, "99", "a1b2c3")))

		wantNoErr(t, "uid=99", err)
		wantEQ(t, "uid=99", "allow", reply.Allow, true)
	})

	t.Run("改 uid 会让签名失效（不可伪造）", func(t *testing.T) {
		e := newEnv(t)
		seedActiveVerifySession(t, e, expire)
		// 拿合法串的骨架，只把 uid 段换掉而不重算摘要——等价于客户端篡改日志字段。
		valid := authKeyFor(t, ugcURI, expire, "42", "a1b2c3")
		seg := strings.Split(valid, "-")
		tampered := strings.Join([]string{seg[0], seg[1], "99", seg[3]}, "-")

		reply, err := verify(t, e, verifyReq(ugcURI, tampered))

		wantNoErr(t, "篡改 uid 段", err)
		wantEQ(t, "篡改 uid 段", "allow", reply.Allow, false)
		wantEQ(t, "篡改 uid 段", "deny_reason", reply.DenyReason, denySignMismatch)
	})
}

// TestVerifyPlaybackTokenCountFailureStillAllowsPlayback 播放可用性优先于统计完整性：
// 计数链路（SETNX 或 INCR）任一失败都只记日志，放行结果不受影响。
func TestVerifyPlaybackTokenCountFailureStillAllowsPlayback(t *testing.T) {
	expire := nowPlus(verExpireIn)
	boom := errors.New("redis: connection refused")

	t.Run("SETNX 标记失败", func(t *testing.T) {
		e := newEnv(t)
		seedActiveVerifySession(t, e, expire)
		e.st.cache.failWith("MarkVerifiedOnce", boom)

		reply, err := verify(t, e, verifyReq(ugcURI, authKeyFor(t, ugcURI, expire, "42", "a1b2c3")))

		wantNoErr(t, "SETNX 标记失败", err)
		wantEQ(t, "SETNX 标记失败", "allow", reply.Allow, true)
		wantEQ(t, "SETNX 标记失败", "play_count 退化成 0 而不是报错", reply.PlayCount, int64(0))
		wantCount(t, "SETNX 标记失败", e.st.log, "cache.Incr", 0)
	})

	t.Run("INCR 失败", func(t *testing.T) {
		e := newEnv(t)
		seedActiveVerifySession(t, e, expire)
		e.st.cache.failWith("IncrPlayCount", boom)

		reply, err := verify(t, e, verifyReq(ugcURI, authKeyFor(t, ugcURI, expire, "42", "a1b2c3")))

		wantNoErr(t, "INCR 失败", err)
		wantEQ(t, "INCR 失败", "allow", reply.Allow, true)
		wantEQ(t, "INCR 失败", "play_count", reply.PlayCount, int64(0))
		wantCount(t, "INCR 失败", e.st.log, "cache.MarkVerified", 1)
	})
}

// TestVerifyPlaybackTokenDegradesWhenCacheDown 读缓存失败不能拖垮回源：直读数据库放行。
// 但数据库本身失败必须报错（把故障与拒绝分开），不能当成"会话不存在"而悄悄拒放。
func TestVerifyPlaybackTokenDegradesWhenCacheDown(t *testing.T) {
	expire := nowPlus(verExpireIn)
	key := authKeyFor(t, ugcURI, expire, "42", "a1b2c3")

	t.Run("缓存读失败仍放行", func(t *testing.T) {
		e := newEnv(t)
		seedActiveVerifySession(t, e, expire)
		e.st.cache.failWith("GetSession", errors.New("redis: i/o timeout"))

		reply, err := verify(t, e, verifyReq(ugcURI, key))

		wantNoErr(t, "缓存读失败", err)
		wantEQ(t, "缓存读失败", "allow", reply.Allow, true)
		wantOps(t, "缓存读失败后的读库链", e.st.log.ops, []string{
			"cache.GetSession:" + keySession(verSessionID),
			"session.FindOne:" + verSessionID,
			"cache.SetSession:" + keySession(verSessionID),
			fmt.Sprintf("cache.MarkVerified:%s/300", keyVerified(verSessionID)),
			"cache.Incr:" + keyPlayCount(model.ContentTypeUGC, 700),
		})
	})

	t.Run("数据库失败按故障上报", func(t *testing.T) {
		e := newEnv(t)
		boom := errors.New("playback: mysql is gone")
		e.st.sessions.failWith("FindOne", boom)

		reply, err := verify(t, e, verifyReq(ugcURI, key))

		wantErrIs(t, "数据库失败", err, boom)
		if reply != nil {
			t.Errorf("数据库失败却返回了判定 %+v", reply)
		}
		wantCount(t, "数据库失败", e.st.log, "cache.MarkVerified", 0)
	})
}

// TestVerifyPlaybackTokenUsesCacheHitWithoutTouchingDB 签发后预热的首个分片回源应命中缓存，
// 不再打数据库。
func TestVerifyPlaybackTokenUsesCacheHitWithoutTouchingDB(t *testing.T) {
	expire := nowPlus(verExpireIn)
	e := newEnv(t)
	row := seedActiveVerifySession(t, e, expire)
	e.st.cache.warm(row)

	reply, err := verify(t, e, verifyReq(ugcURI, authKeyFor(t, ugcURI, expire, "42", "a1b2c3")))
	wantNoErr(t, "缓存命中放行", err)
	wantEQ(t, "缓存命中放行", "allow", reply.Allow, true)
	wantCount(t, "缓存命中放行", e.st.log, "session.FindOne", 0)
	wantCount(t, "缓存命中放行", e.st.log, "cache.SetSession", 0)
	wantCount(t, "缓存命中放行", e.st.log, "cache.Incr", 1)
}

// TestVerifyPlaybackTokenUnsignedModeSkipsSignature dev/test 关闭签名时只校验会话有效期，
// 这是本地直连 MinIO 冒烟的刻意口子；生产由 svc.ServiceContext 拒绝这种配置。
func TestVerifyPlaybackTokenUnsignedModeSkipsSignature(t *testing.T) {
	expire := nowPlus(verExpireIn)
	e := newEnvUnsigned(t)
	seedActiveVerifySession(t, e, expire)

	reply, err := verify(t, e, verifyReq(ugcURI, ""))
	wantNoErr(t, "未签名模式", err)
	wantEQ(t, "未签名模式", "allow", reply.Allow, true)

	// 同一配置下会话到期仍然要拒绝：签名可关，有效期不可关。
	expiredEnv := newEnvUnsigned(t)
	seedActiveVerifySession(t, expiredEnv, nowPlus(-5))
	denied, err := verify(t, expiredEnv, verifyReq(ugcURI, ""))
	wantNoErr(t, "未签名模式会话到期", err)
	wantEQ(t, "未签名模式会话到期", "allow", denied.Allow, false)
	wantEQ(t, "未签名模式会话到期", "deny_reason", denied.DenyReason, denySessionExpired)
}

// TestVerifyPlaybackTokenGuestSessionAcceptsZeroUid 游客会话（mid=0）签发的 uid 段是 "0"，
// 校验必须与签发口径一致，不能把游客挡在门外。
func TestVerifyPlaybackTokenGuestSessionAcceptsZeroUid(t *testing.T) {
	expire := nowPlus(verExpireIn)
	e := newEnv(t)
	seedActiveVerifySession(t, e, expire)
	e.st.sessions.rows[verSessionID].Mid = 0

	reply, err := verify(t, e, verifyReq(ugcURI, authKeyFor(t, ugcURI, expire, "0", "a1b2c3")))

	wantNoErr(t, "游客会话放行", err)
	wantEQ(t, "游客会话放行", "allow", reply.Allow, true)
}
