package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go-video/services/playback/internal/signurl"
	"go-video/services/playback/model"
	rpc "go-video/services/playback/rpc"
)

const (
	pgcSessionID    = "01PGCSESSION0000000000000AA"
	replaySessionID = "01REPLAYSESSION0000000000BB"
	ugcURI          = "/ugc/12/34/700.m3u8"
	pgcURI          = "/pgc/episode/9001/1.m3u8"
)

// tokenReq 返回一个最小可用的 UGC 签发请求。
func tokenReq() *rpc.GetPlaybackTokenReq {
	return &rpc.GetPlaybackTokenReq{
		ContentType: rpc.ContentType_CONTENT_TYPE_UGC,
		ContentId:   700,
		Vid:         "BV700",
		ObjectKey:   "ugc/12/34/700.m3u8", // 故意不带前导斜杠：normalize 后才入库
		Mid:         42,
		Platform:    rpc.Platform_PLATFORM_ANDROID,
		AppVersion:  "1.2.3",
		Region:      "CN",
		RequestId:   "req-new-1",
		TraceId:     "trace-1",
	}
}

// pgcTokenReq 返回一个 PGC 整片请求；版权窗口由用例通过 e.rights 布景。
func pgcTokenReq() *rpc.GetPlaybackTokenReq {
	return &rpc.GetPlaybackTokenReq{
		ContentType: rpc.ContentType_CONTENT_TYPE_PGC,
		ContentId:   9001,
		ObjectKey:   pgcURI,
		Mid:         42,
		Platform:    rpc.Platform_PLATFORM_IOS,
		AppVersion:  "1.2.3",
		Region:      "CN",
		RequestId:   "req-pgc-1",
		TraceId:     "trace-2",
	}
}

// TestGetPlaybackTokenRejectsInvalidRequests 逐条校验入参守卫，并钉住更要紧的一点：
// 守卫必须在**触库、触缓存、调 rights 之前**发生（否则非法请求可以消耗下游配额、留下脏数据）。
func TestGetPlaybackTokenRejectsInvalidRequests(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(in *rpc.GetPlaybackTokenReq)
		want   error
	}{
		{"内容类型未指定", func(in *rpc.GetPlaybackTokenReq) { in.ContentType = rpc.ContentType_CONTENT_TYPE_UNSPECIFIED }, model.ErrInvalidContentType},
		{"内容类型越界", func(in *rpc.GetPlaybackTokenReq) { in.ContentType = rpc.ContentType(99) }, model.ErrInvalidContentType},
		{"content_id 为 0", func(in *rpc.GetPlaybackTokenReq) { in.ContentId = 0 }, model.ErrInvalidContentID},
		{"content_id 为负", func(in *rpc.GetPlaybackTokenReq) { in.ContentId = -7 }, model.ErrInvalidContentID},
		{"平台未指定", func(in *rpc.GetPlaybackTokenReq) { in.Platform = rpc.Platform_PLATFORM_UNSPECIFIED }, model.ErrInvalidPlatform},
		{"平台越界（小程序不在范围）", func(in *rpc.GetPlaybackTokenReq) { in.Platform = rpc.Platform(9) }, model.ErrInvalidPlatform},
		{"缺少幂等键", func(in *rpc.GetPlaybackTokenReq) { in.RequestId = "" }, model.ErrMissingRequestID},
		{"对象 key 为空", func(in *rpc.GetPlaybackTokenReq) { in.ObjectKey = "" }, model.ErrInvalidObjectKey},
		{"对象 key 是绝对 URL", func(in *rpc.GetPlaybackTokenReq) { in.ObjectKey = "https://cdn.example.com/a.m3u8" }, model.ErrInvalidObjectKey},
		{"对象 key 带 query", func(in *rpc.GetPlaybackTokenReq) { in.ObjectKey = "/a.m3u8?auth_key=1" }, model.ErrInvalidObjectKey},
		{"对象 key 路径穿越", func(in *rpc.GetPlaybackTokenReq) { in.ObjectKey = "/ugc/../../etc/passwd" }, model.ErrInvalidObjectKey},
		{"PGC 缺 region", func(in *rpc.GetPlaybackTokenReq) {
			in.ContentType = rpc.ContentType_CONTENT_TYPE_PGC
			in.Region = ""
		}, model.ErrMissingRegion},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			in := tokenReq()
			tc.mutate(in)
			before := e.st.log.snapshot()

			_, err := NewGetPlaybackTokenLogic(context.Background(), e.svcCtx).GetPlaybackToken(in)

			wantErrIs(t, tc.name, err, tc.want)
			wantNoCall(t, tc.name, e.st, before)
		})
	}
}

// TestGetPlaybackTokenUGCCreatesSessionWithoutRights 钉住 UGC 口径与签发全链：
// UGC 不调 rights（§1 整片不允许普通投稿），会话按真实字段落库、进缓存，
// 返回的是签名地址而不是对象存储公共地址（§6）。
func TestGetPlaybackTokenUGCCreatesSessionWithoutRights(t *testing.T) {
	e := newEnv(t)
	now := time.Now().Unix()

	reply, err := NewGetPlaybackTokenLogic(context.Background(), e.svcCtx).GetPlaybackToken(tokenReq())
	wantNoErr(t, "UGC 签发", err)

	wantCount(t, "UGC 不查版权窗口", e.st.log, "rights.CheckPlayable", 0)
	wantCount(t, "会话只写一次", e.st.log, "session.Insert", 1)
	wantCount(t, "新会话预热缓存一次", e.st.log, "cache.SetSession", 1)

	row := e.st.sessions.only()
	if len(row.SessionId) != 26 {
		t.Errorf("session_id = %q, want 26 位 ULID", row.SessionId)
	}
	wantEQ(t, "会话落库", "request_id", row.RequestId, "req-new-1")
	wantEQ(t, "会话落库", "state", row.State, int32(model.SessionStateActive))
	wantEQ(t, "会话落库", "content_type", row.ContentType, int32(model.ContentTypeUGC))
	wantEQ(t, "会话落库", "content_id", row.ContentId, int64(700))
	wantEQ(t, "会话落库", "vid", row.Vid, "BV700")
	wantEQ(t, "会话落库", "mid", row.Mid, int64(42))
	wantEQ(t, "会话落库", "platform", row.Platform, int32(model.PlatformAndroid))
	wantEQ(t, "会话落库", "region", row.Region, "CN")
	wantEQ(t, "会话落库", "object_key", row.ObjectKey, "ugc/12/34/700.m3u8")
	wantEQ(t, "会话落库", "uri（签名口径要规范化）", row.Uri, ugcURI)
	wantEQ(t, "会话落库", "trace_id", row.TraceId, "trace-1")
	assertAround(t, "会话落库", "expire_at(now+TTL)", row.ExpireAt, now+testTokenTTL, 3)
	assertAround(t, "会话落库", "ctime", row.Ctime, now, 3)
	wantEQ(t, "会话落库", "mtime（新建即 ctime）", row.Mtime, row.Ctime)

	wantEQ(t, "响应", "session_id", reply.SessionId, row.SessionId)
	wantEQ(t, "响应", "expire_at", reply.ExpireAt, row.ExpireAt)
	wantEQ(t, "响应", "key_id", reply.KeyId, testKeyID)
	wantEQ(t, "响应", "play_url", reply.PlayUrl, testBaseURL+ugcURI+"?auth_key="+reply.AuthKey)
	if reply.Ttl <= 0 || reply.Ttl > testTokenTTL {
		t.Errorf("响应：ttl = %d, want (0, %d]", reply.Ttl, testTokenTTL)
	}
	tok, err := signurl.Parse(reply.AuthKey)
	wantNoErr(t, "响应 auth_key 可解析", err)
	wantEQ(t, "auth_key", "ts 必须等于授权到期时刻", tok.ExpireAt, reply.ExpireAt)
	wantEQ(t, "auth_key", "uid 段是观看者 mid", tok.UID, "42")

	cached := e.st.cache.cached(row.SessionId)
	if cached == nil {
		t.Fatalf("新会话没有预热到缓存（首个分片回源会打穿数据库）")
	}
	wantEQ(t, "缓存副本", "uri", cached.Uri, row.Uri)
	wantEQ(t, "缓存副本", "expire_at", cached.ExpireAt, row.ExpireAt)
}

// TestGetPlaybackTokenPGCRequiresPlayableWindow 是本服务最要紧的版权口径：
// rights 判定不可播、或判定本身失败，都必须拒绝签发且**不留会话**。
// 尤其后者：RPC 故障绝不能退化成「默认可播」。
func TestGetPlaybackTokenPGCRequiresPlayableWindow(t *testing.T) {
	cases := []struct {
		name     string
		playable bool
		endTime  int64
		err      error
		want     error
	}{
		{"窗口不可播（撤权/地区不匹配/未开始）", false, 0, nil, model.ErrCopyrightWindowUnavailable},
		{"rights 调用失败", false, 0, model.ErrRightsUnavailable, model.ErrRightsUnavailable},
		{"rights 报未配置", false, 0, errors.New("dial timeout"), nil},
		{"窗口已结束（playable 但 end_time 在过去）", true, nowPlus(-10), nil, model.ErrCopyrightWindowUnavailable},
		{"窗口正好等于 now（边界仍不可签发）", true, nowPlus(0), nil, model.ErrCopyrightWindowUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.rights.playable = tc.playable
			e.rights.endTime = tc.endTime
			e.rights.err = tc.err
			before := e.st.log.snapshot()

			reply, err := NewGetPlaybackTokenLogic(context.Background(), e.svcCtx).GetPlaybackToken(pgcTokenReq())

			if reply != nil {
				t.Fatalf("%s：返回了签发结果 session=%s", tc.name, reply.SessionId)
			}
			if tc.want != nil {
				wantErrIs(t, tc.name, err, tc.want)
			}
			if err == nil {
				t.Fatalf("%s：错误 = nil, want 拒绝签发", tc.name)
			}
			// 拒绝必须只发生在 rights 这一步：不查幂等、不落库、不写缓存。
			wantOps(t, tc.name, e.st.log.opsFrom(before),
				[]string{fmt.Sprintf("rights.CheckPlayable:9001/%d/CN", model.ContentTypePGC)})
			if len(e.st.sessions.rows) != 0 {
				t.Errorf("%s：拒绝签发后仍写入了 %d 行会话", tc.name, len(e.st.sessions.rows))
			}
			if len(e.st.cache.sessions) != 0 {
				t.Errorf("%s：拒绝签发后仍写了缓存", tc.name)
			}
		})
	}
}

// TestGetPlaybackTokenPGCClampsExpireAtToWindowEnd 钉住「授权永不超过版权窗口」（§8）：
// 窗口早于 TTL 时以窗口封顶，并且封顶值直接落到会话、响应与 auth_key 三处。
func TestGetPlaybackTokenPGCClampsExpireAtToWindowEnd(t *testing.T) {
	windowEnd := nowPlus(120)
	e := newEnv(t)
	e.rights.playable = true
	e.rights.endTime = windowEnd

	reply, err := NewGetPlaybackTokenLogic(context.Background(), e.svcCtx).GetPlaybackToken(pgcTokenReq())
	wantNoErr(t, "PGC 签发", err)

	wantEQ(t, "授权到期", "reply.expire_at 必须正好是窗口结束（不是 now+TTL）", reply.ExpireAt, windowEnd)
	row := e.st.sessions.only()
	wantEQ(t, "授权到期", "落库 expire_at", row.ExpireAt, windowEnd)
	if row.ExpireAt >= row.Ctime+testTokenTTL {
		t.Errorf("授权到期 = %d 未早于 now+TTL = %d，封顶未生效", row.ExpireAt, row.Ctime+testTokenTTL)
	}
	tok, err := signurl.Parse(reply.AuthKey)
	wantNoErr(t, "auth_key 解析", err)
	wantEQ(t, "auth_key", "ts 段", tok.ExpireAt, windowEnd)
	if reply.Ttl > 120 {
		t.Errorf("响应：ttl = %d, want <=120（受窗口封顶）", reply.Ttl)
	}
	// PGC 必须把 region 与 playback 口径的 content_type 传给 rights。
	wantEQ(t, "rights 调用", "参数", strings.Join(e.rights.calls, ","),
		fmt.Sprintf("9001/%d/CN", model.ContentTypePGC))
}

// TestGetPlaybackTokenReplayIsIdempotent 同一 request_id 重放必须返回同一会话与同一
// 到期时刻（不延长授权），且不再产生任何写入。
func TestGetPlaybackTokenReplayIsIdempotent(t *testing.T) {
	e := newEnv(t)
	seed := &model.PlaybackSession{
		SessionId: replaySessionID, ContentType: model.ContentTypeUGC, ContentId: 700,
		Mid: 42, Platform: model.PlatformAndroid, Region: "CN",
		ObjectKey: "ugc/12/34/700.m3u8", Uri: ugcURI, RequestId: "req-new-1",
		ExpireAt: nowPlus(600), State: model.SessionStateActive, Ctime: nowPlus(-1200), Mtime: nowPlus(-1200),
	}
	seedSession(t, e.st, seed)
	before := e.st.log.snapshot()

	in := tokenReq()
	reply, err := NewGetPlaybackTokenLogic(context.Background(), e.svcCtx).GetPlaybackToken(in)
	wantNoErr(t, "幂等重放", err)

	wantEQ(t, "重放", "session_id", reply.SessionId, seed.SessionId)
	wantEQ(t, "重放", "expire_at 不得延长", reply.ExpireAt, seed.ExpireAt)
	wantEQ(t, "重放", "play_url 仍绑定原 URI", reply.PlayUrl, testBaseURL+ugcURI+"?auth_key="+reply.AuthKey)
	wantOps(t, "重放只读幂等键", e.st.log.opsFrom(before),
		[]string{"session.FindByRequest:req-new-1"})
	wantEQ(t, "重放", "库里仍是那一行", len(e.st.sessions.rows), 1)

	// auth_key 每次重新生成（rand 段变化），但到期时刻不变——CDN 才能防重放又不给客户端续命。
	reply2, err := NewGetPlaybackTokenLogic(context.Background(), e.svcCtx).GetPlaybackToken(in)
	wantNoErr(t, "二次重放", err)
	wantEQ(t, "二次重放", "expire_at", reply2.ExpireAt, seed.ExpireAt)
	if reply.AuthKey == reply2.AuthKey {
		t.Errorf("两次重放的 auth_key 完全相同：%q，rand 段应重新生成", reply.AuthKey)
	}
	tok1, err1 := signurl.Parse(reply.AuthKey)
	tok2, err2 := signurl.Parse(reply2.AuthKey)
	wantNoErr(t, "解析 auth_key 1", err1)
	wantNoErr(t, "解析 auth_key 2", err2)
	if tok1.Rand == tok2.Rand {
		t.Errorf("auth_key rand 段重复：%s", tok1.Rand)
	}
	wantEQ(t, "auth_key", "两次的 ts 一致", tok2.ExpireAt, tok1.ExpireAt)
}

// TestGetPlaybackTokenRejectsRequestIdCrossUse 幂等键串用必须拒绝：同一 request_id
// 换用户、换内容、换内容类型都算冲突（否则 A 拿到的 request_id 可以让 B 复用到 A 的会话）。
func TestGetPlaybackTokenRejectsRequestIdCrossUse(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(in *rpc.GetPlaybackTokenReq)
	}{
		{"换个用户", func(in *rpc.GetPlaybackTokenReq) { in.Mid = 4242 }},
		{"换个内容 id", func(in *rpc.GetPlaybackTokenReq) { in.ContentId = 701 }},
		{"换个内容类型", func(in *rpc.GetPlaybackTokenReq) {
			in.ContentType = rpc.ContentType_CONTENT_TYPE_PGC
		}},
		{"游客变登录用户", func(in *rpc.GetPlaybackTokenReq) { in.Mid = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			// PGC 类用例要先把版权窗口布成"可播"，才能测到归属冲突而不是被版权拦在前面；
			// UGC 用例不读 rights，布景无副作用。
			e.rights.playable = true
			e.rights.endTime = nowPlus(7200)
			seed := &model.PlaybackSession{
				SessionId: replaySessionID, ContentType: model.ContentTypeUGC, ContentId: 700,
				Mid: 42, Platform: model.PlatformAndroid, Uri: ugcURI, RequestId: "req-new-1",
				ExpireAt: nowPlus(600), State: model.SessionStateActive,
			}
			seedSession(t, e.st, seed)

			in := tokenReq()
			tc.mutate(in)
			reply, err := NewGetPlaybackTokenLogic(context.Background(), e.svcCtx).GetPlaybackToken(in)

			wantErrIs(t, tc.name, err, model.ErrSessionIDConflict)
			if reply != nil {
				t.Errorf("%s：冲突请求仍返回了会话 %s", tc.name, reply.SessionId)
			}
			wantCount(t, tc.name, e.st.log, "session.Insert", 0)
			wantCount(t, tc.name, e.st.log, "cache.SetSession", 0)
			wantEQ(t, tc.name, "原会话未被改动", e.st.sessions.state(replaySessionID), int32(model.SessionStateActive))
		})
	}
}

// TestGetPlaybackTokenReplayRejectsUnusableSession 幂等重放不能绕过会话状态：
// 已撤销的会话（版权撤回/风控）与已过期的会话都要显式报错，让客户端换新 request_id。
func TestGetPlaybackTokenReplayRejectsUnusableSession(t *testing.T) {
	cases := []struct {
		name  string
		state int32
		delta int64 // expire_at 相对 now 的偏移，布景与断言共用同一个值
		want  error
	}{
		{"已撤销（到期时刻仍在未来）", model.SessionStateRevoked, 600, model.ErrSessionRevoked},
		{"已过期", model.SessionStateExpired, -60, model.ErrSessionExpired},
		{"状态仍有效但到期时刻已过", model.SessionStateActive, -30, model.ErrSessionExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			expire := nowPlus(tc.delta)
			seedSession(t, e.st, &model.PlaybackSession{
				SessionId: replaySessionID, ContentType: model.ContentTypeUGC, ContentId: 700,
				Mid: 42, Platform: model.PlatformAndroid, Uri: ugcURI, RequestId: "req-new-1",
				ExpireAt: expire, State: tc.state,
			})

			_, err := NewGetPlaybackTokenLogic(context.Background(), e.svcCtx).GetPlaybackToken(tokenReq())
			wantErrIs(t, tc.name, err, tc.want)
			wantCount(t, tc.name, e.st.log, "session.Insert", 0)
			// 拒绝重放不得给会话续命，也不得顺手改写状态。
			row := e.st.sessions.get(replaySessionID)
			wantEQ(t, tc.name, "expire_at 未被延长", row.ExpireAt, expire)
			wantEQ(t, tc.name, "state 未被改写", row.State, tc.state)
		})
	}
}

// TestGetPlaybackTokenUsesExistingSessionOnInsertRace 模拟并发重放：两个请求同时读到
// request_id 未命中，后写的那个拿到唯一键冲突，此时必须复用先落库的会话而不是报错。
func TestGetPlaybackTokenUsesExistingSessionOnInsertRace(t *testing.T) {
	e := newEnv(t)
	raced := &model.PlaybackSession{
		SessionId: replaySessionID, ContentType: model.ContentTypeUGC, ContentId: 700,
		Mid: 42, Platform: model.PlatformAndroid, Uri: ugcURI, RequestId: "req-new-1",
		ExpireAt: nowPlus(900), State: model.SessionStateActive, Ctime: nowPlus(-1),
	}
	pending := *raced
	e.st.sessions.pendingReveal = &pending

	reply, err := NewGetPlaybackTokenLogic(context.Background(), e.svcCtx).GetPlaybackToken(tokenReq())
	wantNoErr(t, "并发重放", err)

	wantEQ(t, "并发重放", "复用先落库的会话", reply.SessionId, raced.SessionId)
	wantEQ(t, "并发重放", "expire_at", reply.ExpireAt, raced.ExpireAt)
	wantEQ(t, "并发重放", "URI", reply.PlayUrl, testBaseURL+ugcURI+"?auth_key="+reply.AuthKey)
	// 幂等键读两次（冲突前一次、冲突后一次），只写一次，冲突分支不再预热缓存。
	wantCount(t, "并发重放", e.st.log, "session.FindByRequest", 2)
	wantCount(t, "并发重放", e.st.log, "session.Insert", 1)
	wantCount(t, "并发重放", e.st.log, "cache.SetSession", 0)
	if e.st.sessions.get(replaySessionID) == nil {
		t.Errorf("并发重放：库里应有先落库那一行")
	}
}

// TestGetPlaybackTokenPropagatesStoreErrors 读/写库失败必须原样报错：
// 既不返回签发成功，也不退化成「当作重复请求」或「当作未播放过」。
func TestGetPlaybackTokenPropagatesStoreErrors(t *testing.T) {
	boom := errors.New("playback: mysql is gone")

	t.Run("幂等键读取失败", func(t *testing.T) {
		e := newEnv(t)
		e.st.sessions.failWith("FindByRequest", boom)

		_, err := NewGetPlaybackTokenLogic(context.Background(), e.svcCtx).GetPlaybackToken(tokenReq())

		wantErrIs(t, "幂等键读取失败", err, boom)
		wantCount(t, "幂等键读取失败", e.st.log, "session.Insert", 0)
		wantCount(t, "幂等键读取失败", e.st.log, "cache.SetSession", 0)
	})

	t.Run("写入失败", func(t *testing.T) {
		e := newEnv(t)
		e.st.sessions.failWith("Insert", boom)

		_, err := NewGetPlaybackTokenLogic(context.Background(), e.svcCtx).GetPlaybackToken(tokenReq())

		wantErrIs(t, "写入失败", err, boom)
		wantCount(t, "写入失败", e.st.log, "cache.SetSession", 0)
		if len(e.st.cache.sessions) != 0 {
			t.Errorf("写入失败仍预热了缓存")
		}
	})

	t.Run("PGC 在幂等查询前就失败", func(t *testing.T) {
		e := newEnv(t)
		e.rights.err = boom

		_, err := NewGetPlaybackTokenLogic(context.Background(), e.svcCtx).GetPlaybackToken(pgcTokenReq())

		wantErrIs(t, "PGC rights 失败", err, boom)
		wantCount(t, "PGC rights 失败", e.st.log, "session.FindByRequest", 0)
	})
}

// TestGetPlaybackTokenRefusesUnsignedAddressWithoutKey 生产口径（签名开启）下私钥缺失
// 绝不能返回一个"看起来能用"的裸地址：必须报错，让调用方知道服务端配置有问题（§6）。
// 同时如实记录这一路径的副作用——会话已经落库，只是地址没发出去。
func TestGetPlaybackTokenRefusesUnsignedAddressWithoutKey(t *testing.T) {
	e := newEnvSigner(t, "", true)

	reply, err := NewGetPlaybackTokenLogic(context.Background(), e.svcCtx).GetPlaybackToken(tokenReq())

	wantErrIs(t, "私钥缺失", err, signurl.ErrPrivateKeyRequired)
	if reply != nil {
		t.Errorf("私钥缺失仍返回签发结果：%+v", reply)
	}
	wantCount(t, "私钥缺失", e.st.log, "session.Insert", 1)
}

// TestGetPlaybackTokenUnsignedAddressOnlyInDevMode dev/test 关闭签名时地址不带 auth_key，
// 这是刻意的本地冒烟口子；用它跑生产会退化成公共地址分发（svc 层已禁止，见 ServiceContext）。
func TestGetPlaybackTokenUnsignedAddressOnlyInDevMode(t *testing.T) {
	e := newEnvUnsigned(t)

	reply, err := NewGetPlaybackTokenLogic(context.Background(), e.svcCtx).GetPlaybackToken(tokenReq())
	wantNoErr(t, "dev 模式未签名签发", err)

	wantEQ(t, "dev 模式", "auth_key 为空", reply.AuthKey, "")
	wantEQ(t, "dev 模式", "play_url 不带鉴权串", reply.PlayUrl, testBaseURL+ugcURI)
	if strings.Contains(reply.PlayUrl, "auth_key") {
		t.Errorf("dev 模式地址里出现了 auth_key：%s", reply.PlayUrl)
	}
}

// TestGetPlaybackTokenCacheWarmFailureStillSucceeds 会话已落库、缓存预热失败只是少一次加速，
// 不能把已经成立的授权报成错误（否则客户端会换 request_id 重发，产生重复会话）。
func TestGetPlaybackTokenCacheWarmFailureStillSucceeds(t *testing.T) {
	e := newEnv(t)
	e.st.cache.failWith("SetSession", errors.New("redis: connection refused"))

	reply, err := NewGetPlaybackTokenLogic(context.Background(), e.svcCtx).GetPlaybackToken(tokenReq())

	wantNoErr(t, "缓存预热失败", err)
	if e.st.sessions.get(reply.SessionId) == nil {
		t.Errorf("缓存预热失败时会话必须仍已落库")
	}
	wantCount(t, "缓存预热失败", e.st.log, "cache.SetSession", 1)
}
