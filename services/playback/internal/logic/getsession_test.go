package logic

import (
	"context"
	"errors"
	"testing"

	"go-video/services/playback/model"
	rpc "go-video/services/playback/rpc"
)

const gsSessionID = "01GETSESSION0000000000000DD"

// gsSession 给一个字段值互不相同的会话，方便逐字段核对投影（相同值会掩盖串字段）。
func gsSession() *model.PlaybackSession {
	return &model.PlaybackSession{
		SessionId: gsSessionID, ContentType: model.ContentTypePGC, ContentId: 9001,
		Vid: "BV9001", Mid: 4242, Platform: model.PlatformHarmony, AppVersion: "2.3.4",
		Region: "CN", ObjectKey: "pgc/episode/9001/1.m3u8", Uri: "/pgc/episode/9001/1.m3u8",
		RequestId: "req-gs-1", ExpireAt: 1_810_000_900, State: model.SessionStateRevoked,
		TraceId: "trace-gs", Ctime: 1_810_000_100, Mtime: 1_810_000_200,
	}
}

// TestGetSessionRejectsMissingSessionID 守卫在触库之前。
func TestGetSessionRejectsMissingSessionID(t *testing.T) {
	e := newEnv(t)
	before := e.st.log.snapshot()

	_, err := NewGetSessionLogic(context.Background(), e.svcCtx).GetSession(&rpc.GetSessionReq{})

	wantErrIs(t, "缺 session_id", err, model.ErrMissingSessionID)
	wantNoCall(t, "缺 session_id", e.st, before)
}

// TestGetSessionNotFoundIsAnErrorNotAnEmptyReply GetSession 是排障/运营读接口：
// 查无此会话用 gRPC 错误表达（与回源校验的 allow=false 口径不同），
// 且必须真的查过缓存和数据库。
func TestGetSessionNotFoundIsAnErrorNotAnEmptyReply(t *testing.T) {
	e := newEnv(t)

	reply, err := NewGetSessionLogic(context.Background(), e.svcCtx).GetSession(
		&rpc.GetSessionReq{SessionId: gsSessionID})

	wantErrIs(t, "会话不存在", err, model.ErrSessionNotFound)
	if reply != nil {
		t.Errorf("会话不存在却返回了 %+v", reply)
	}
	wantOps(t, "会话不存在的查询链", e.st.log.ops, []string{
		"cache.GetSession:" + keySession(gsSessionID),
		"session.FindOne:" + gsSessionID,
	})
}

// TestGetSessionProjectsSessionAndProgress 逐字段核对会话与进度投影。
// 这里刻意不使用签名相关字段：auth_key 从不落库，因此也不可能从这里泄漏。
func TestGetSessionProjectsSessionAndProgress(t *testing.T) {
	e := newEnv(t)
	seedSession(t, e.st, gsSession())
	own := seedProgress(t, e.st, &model.PlaybackProgress{
		SessionId: gsSessionID, ContentType: model.ContentTypePGC, ContentId: 9001,
		Vid: "BV9001", Mid: 4242, PositionMs: 123_456, DurationMs: 2_400_000,
		BufferCount: 3, AvgBitrate: 5_000_000, LastError: 0,
		Ctime: 1_810_000_300, Mtime: 1_810_000_400,
	})

	reply, err := NewGetSessionLogic(context.Background(), e.svcCtx).GetSession(
		&rpc.GetSessionReq{SessionId: gsSessionID})
	wantNoErr(t, "GetSession", err)

	got := reply.GetSession()
	wantEQ(t, "会话投影", "session_id", got.GetSessionId(), gsSession().SessionId)
	wantEQ(t, "会话投影", "content_type", got.GetContentType(), int32(model.ContentTypePGC))
	wantEQ(t, "会话投影", "content_id", got.GetContentId(), int64(9001))
	wantEQ(t, "会话投影", "vid", got.GetVid(), "BV9001")
	wantEQ(t, "会话投影", "mid", got.GetMid(), int64(4242))
	wantEQ(t, "会话投影", "platform", got.GetPlatform(), int32(model.PlatformHarmony))
	wantEQ(t, "会话投影", "app_version", got.GetAppVersion(), "2.3.4")
	wantEQ(t, "会话投影", "region", got.GetRegion(), "CN")
	wantEQ(t, "会话投影", "object_key", got.GetObjectKey(), "pgc/episode/9001/1.m3u8")
	wantEQ(t, "会话投影", "uri", got.GetUri(), "/pgc/episode/9001/1.m3u8")
	wantEQ(t, "会话投影", "request_id", got.GetRequestId(), "req-gs-1")
	wantEQ(t, "会话投影", "expire_at", got.GetExpireAt(), int64(1_810_000_900))
	// 状态原样透出：撤销/过期会话正是后台要查的对象，不得在这里被隐藏成"不存在"。
	wantEQ(t, "会话投影", "state", got.GetState(), int32(model.SessionStateRevoked))
	wantEQ(t, "会话投影", "trace_id", got.GetTraceId(), "trace-gs")
	wantEQ(t, "会话投影", "ctime", got.GetCtime(), int64(1_810_000_100))
	wantEQ(t, "会话投影", "mtime", got.GetMtime(), int64(1_810_000_200))

	p := reply.GetProgress()
	wantEQ(t, "进度投影", "id", p.GetId(), own.ID)
	wantEQ(t, "进度投影", "session_id", p.GetSessionId(), gsSessionID)
	wantEQ(t, "进度投影", "content_type", p.GetContentType(), int32(model.ContentTypePGC))
	wantEQ(t, "进度投影", "content_id", p.GetContentId(), int64(9001))
	wantEQ(t, "进度投影", "vid", p.GetVid(), "BV9001")
	wantEQ(t, "进度投影", "mid", p.GetMid(), int64(4242))
	wantEQ(t, "进度投影", "position_ms", p.GetPositionMs(), int64(123_456))
	wantEQ(t, "进度投影", "duration_ms", p.GetDurationMs(), int64(2_400_000))
	wantEQ(t, "进度投影", "buffer_count", p.GetBufferCount(), int32(3))
	wantEQ(t, "进度投影", "avg_bitrate", p.GetAvgBitrate(), int64(5_000_000))
	wantEQ(t, "进度投影", "last_error", p.GetLastError(), int32(0))
	wantEQ(t, "进度投影", "ctime", p.GetCtime(), int64(1_810_000_300))
	wantEQ(t, "进度投影", "mtime", p.GetMtime(), int64(1_810_000_400))

	if reply.LatestProgress != nil {
		t.Errorf("只有本会话一条进度时 latest_progress 不该重复出现：%+v", reply.LatestProgress)
	}
	wantOps(t, "GetSession 的查询链", e.st.log.ops, []string{
		"cache.GetSession:" + keySession(gsSessionID),
		"session.FindOne:" + gsSessionID,
		"cache.SetSession:" + keySession(gsSessionID),
		"progress.FindOne:" + gsSessionID,
		"progress.FindLatestByMid:4242/2/9001",
	})
}

// TestGetSessionReturnsLatestProgressFromAnotherSession latest_progress 回答的是
// "为什么客户端没有从头播放"：同一观看者对同一内容的**另一个**会话的进度。
func TestGetSessionReturnsLatestProgressFromAnotherSession(t *testing.T) {
	e := newEnv(t)
	seedSession(t, e.st, gsSession())
	seedProgress(t, e.st, &model.PlaybackProgress{
		SessionId: gsSessionID, ContentType: model.ContentTypePGC, ContentId: 9001, Mid: 4242,
		PositionMs: 10_000, Ctime: 1_810_000_300, Mtime: 1_810_000_400,
	})
	older := &model.PlaybackProgress{
		SessionId: "01OTHERSESSION000000000000EE", ContentType: model.ContentTypePGC, ContentId: 9001,
		Mid: 4242, PositionMs: 55_000, Ctime: 1_810_000_100, Mtime: 1_810_000_200,
	}
	newest := seedProgress(t, e.st, &model.PlaybackProgress{
		SessionId: "01OTHERSESSION000000000000FF", ContentType: model.ContentTypePGC, ContentId: 9001,
		Mid: 4242, PositionMs: 77_000, Ctime: 1_810_000_500, Mtime: 1_810_000_600,
	})
	// 更早的一条与另一个用户的同内容进度都不得被选中。
	seedProgress(t, e.st, older)
	seedProgress(t, e.st, &model.PlaybackProgress{
		SessionId: "01OTHERMID0000000000000000AA", ContentType: model.ContentTypePGC, ContentId: 9001,
		Mid: 8888, PositionMs: 999_000, Ctime: 1_810_000_900, Mtime: 1_810_001_000,
	})

	reply, err := NewGetSessionLogic(context.Background(), e.svcCtx).GetSession(
		&rpc.GetSessionReq{SessionId: gsSessionID})
	wantNoErr(t, "latest_progress", err)

	if reply.LatestProgress == nil {
		t.Fatalf("latest_progress = nil, want 同一观看者另一会话的最新进度")
	}
	wantEQ(t, "latest_progress", "session_id", reply.LatestProgress.GetSessionId(), newest.SessionId)
	wantEQ(t, "latest_progress", "position_ms", reply.LatestProgress.GetPositionMs(), int64(77_000))
	wantEQ(t, "本会话进度", "position_ms 不被 latest 覆盖", reply.GetProgress().GetPositionMs(), int64(10_000))
}

// TestGetSessionSkipsLatestForGuest 游客（mid<=0）没有跨会话断点可查，不能再打一次数据库。
func TestGetSessionSkipsLatestForGuest(t *testing.T) {
	e := newEnv(t)
	row := gsSession()
	row.Mid = 0
	seedSession(t, e.st, row)

	reply, err := NewGetSessionLogic(context.Background(), e.svcCtx).GetSession(
		&rpc.GetSessionReq{SessionId: gsSessionID})
	wantNoErr(t, "游客会话查询", err)

	wantCount(t, "游客会话查询", e.st.log, "progress.FindLatestByMid", 0)
	if reply.LatestProgress != nil {
		t.Errorf("游客会话不应有 latest_progress：%+v", reply.LatestProgress)
	}
}

// TestGetSessionToleratesLatestProgressFailure 断点辅助信息查不到时不能拖垮主查询：
// 排障接口首先要能拿到会话本身。
func TestGetSessionToleratesLatestProgressFailure(t *testing.T) {
	e := newEnv(t)
	seedSession(t, e.st, gsSession())
	e.st.progress.failWith("FindLatestByMid", errors.New("playback: mysql is gone"))

	reply, err := NewGetSessionLogic(context.Background(), e.svcCtx).GetSession(
		&rpc.GetSessionReq{SessionId: gsSessionID})

	wantNoErr(t, "latest 查询失败", err)
	wantEQ(t, "latest 查询失败", "会话仍在", reply.GetSession().GetSessionId(), gsSessionID)
	if reply.LatestProgress != nil {
		t.Errorf("latest 查询失败却带回了进度：%+v", reply.LatestProgress)
	}
}

// TestGetSessionPropagatesStoreErrors 主查询链上的失败一律报错，不返回半截结果。
func TestGetSessionPropagatesStoreErrors(t *testing.T) {
	boom := errors.New("playback: mysql is gone")

	t.Run("会话读取失败", func(t *testing.T) {
		e := newEnv(t)
		e.st.sessions.failWith("FindOne", boom)

		reply, err := NewGetSessionLogic(context.Background(), e.svcCtx).GetSession(
			&rpc.GetSessionReq{SessionId: gsSessionID})

		wantErrIs(t, "会话读取失败", err, boom)
		if reply != nil {
			t.Errorf("会话读取失败却返回了 %+v", reply)
		}
	})

	t.Run("进度读取失败", func(t *testing.T) {
		e := newEnv(t)
		seedSession(t, e.st, gsSession())
		e.st.progress.failWith("FindOne", boom)

		reply, err := NewGetSessionLogic(context.Background(), e.svcCtx).GetSession(
			&rpc.GetSessionReq{SessionId: gsSessionID})

		wantErrIs(t, "进度读取失败", err, boom)
		if reply != nil {
			t.Errorf("进度读取失败却返回了 %+v", reply)
		}
		wantCount(t, "进度读取失败", e.st.log, "progress.FindLatestByMid", 0)
	})
}

// TestGetSessionWithoutProgress 从未上报过心跳的会话：progress 字段缺省而不是零值对象，
// 客户端据此区分"没播过"和"播到 0 毫秒"。
func TestGetSessionWithoutProgress(t *testing.T) {
	e := newEnv(t)
	seedSession(t, e.st, gsSession())

	reply, err := NewGetSessionLogic(context.Background(), e.svcCtx).GetSession(
		&rpc.GetSessionReq{SessionId: gsSessionID})
	wantNoErr(t, "无进度会话", err)

	if reply.GetProgress() != nil {
		t.Errorf("无心跳时 progress = %+v, want nil", reply.GetProgress())
	}
	wantEQ(t, "无进度会话", "session_id", reply.GetSession().GetSessionId(), gsSessionID)
}

// TestGetSessionServesFromWarmCache 签发后预热的会话不该再打数据库。
func TestGetSessionServesFromWarmCache(t *testing.T) {
	e := newEnv(t)
	row := gsSession()
	seedSession(t, e.st, row)
	e.st.cache.warm(row)

	reply, err := NewGetSessionLogic(context.Background(), e.svcCtx).GetSession(
		&rpc.GetSessionReq{SessionId: gsSessionID})
	wantNoErr(t, "缓存命中查询", err)

	wantEQ(t, "缓存命中查询", "state", reply.GetSession().GetState(), int32(model.SessionStateRevoked))
	wantOps(t, "缓存命中查询", e.st.log.ops, []string{
		"cache.GetSession:" + keySession(gsSessionID),
		"progress.FindOne:" + gsSessionID,
		"progress.FindLatestByMid:4242/2/9001",
	})
}
