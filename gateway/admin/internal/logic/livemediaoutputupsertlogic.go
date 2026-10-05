// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	livemediarpc "go-video/services/live-media/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type LiveMediaOutputUpsertLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 登记/刷新一个码率档位的分发输出（(room,session,level,protocol) 唯一）
func NewLiveMediaOutputUpsertLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveMediaOutputUpsertLogic {
	return &LiveMediaOutputUpsertLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveMediaOutputUpsert 聚合 live-media UpsertStreamOutput。
//
// upsert 与 offline 是两个方向：这一条决定「某一档位在观众侧存在」，OfflineStreamOutput 决定它消失，
// 因此权限点分开（live:output:update / live:output:offline）。
//
// 门槛只到形态：bitrate_level/protocol 的 0 是 UNSPECIFIED（不知道刷新哪一路就不该写），
// bucket 与 object_key 必须成对（只给一半的引用不可用，服务也无从校验对象是否存在），
// 宽高/码率/帧率是**下发时刻的快照**，传 0 表示该项不声明，网关不拿 transcode 模板的值来补
// （模板主数据归 transcode，回写历史行会伪造当时的下发参数）。online_expire_at=0 表示由断流事件
// 下线，是有意的哨兵而不是缺参。
//
// (room,session,level,protocol) 的唯一约束与「同档位重复登记算更新还是算冲突」由 live-media 判定；
// 本方法没有 operator 位，谁登记的只能落在网关日志（缺口见 admin.api 与 README）。
func (l *LiveMediaOutputUpsertLogic) LiveMediaOutputUpsert(req *types.ParamLiveMediaOutputUpsert) (resp *types.LiveMediaOutputUpsertResponse, err error) {
	if l.svcCtx.LiveMedia == nil {
		return nil, errLiveMediaNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveMediaSessionGate(l.ctx, "liveMediaOutputUpsert"); err != nil {
		return nil, err
	}
	if err := liveMediaIdempotencyGate(req.RequestId); err != nil {
		return nil, err
	}
	if err := liveRequiredID("room_id", req.RoomId); err != nil {
		return nil, err
	}
	if err := liveMediaEnum("bitrate_level", req.BitrateLevel); err != nil {
		return nil, err
	}
	if err := liveMediaEnum("protocol", req.Protocol); err != nil {
		return nil, err
	}
	if err := liveMediaRefPair("bucket", "object_key", req.Bucket, req.ObjectKey); err != nil {
		return nil, err
	}
	for _, f := range []struct {
		name string
		v    int64
	}{
		{"live_session_id", req.SessionId},
		{"task_id", req.TaskId},
		{"online_expire_at", req.OnlineExpireAt},
	} {
		if err := liveNonNeg(f.name, f.v); err != nil {
			return nil, err
		}
	}
	for _, f := range []struct {
		name string
		v    int32
	}{
		{"width", req.Width},
		{"height", req.Height},
		{"bitrate_kbps", req.BitrateKbps},
		{"fps", req.Fps},
	} {
		if err := liveNonNeg32(f.name, f.v); err != nil {
			return nil, err
		}
	}
	info, err := l.svcCtx.LiveMedia.UpsertStreamOutput(l.ctx, &livemediarpc.UpsertStreamOutputReq{
		RoomId:         req.RoomId,
		LiveSessionId:  req.SessionId,
		TaskId:         req.TaskId,
		BitrateLevel:   livemediarpc.BitrateLevel(req.BitrateLevel),
		Protocol:       livemediarpc.StreamProtocol(req.Protocol),
		Bucket:         req.Bucket,
		ObjectKey:      req.ObjectKey,
		CdnDomain:      req.CdnDomain,
		Width:          req.Width,
		Height:         req.Height,
		BitrateKbps:    req.BitrateKbps,
		Fps:            req.Fps,
		OnlineExpireAt: req.OnlineExpireAt,
		RequestId:      req.RequestId,
		TraceId:        req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveMediaOutputUpsert: room_id=%d bitrate_level=%d protocol=%d request_id=%s err=%v",
			req.RoomId, req.BitrateLevel, req.Protocol, req.RequestId, err)
		return nil, err
	}
	return &types.LiveMediaOutputUpsertResponse{
		Code:    0,
		Message: "ok",
		Data:    liveMediaStreamOutputToAPI(info),
		TTL:     0,
	}, nil
}
