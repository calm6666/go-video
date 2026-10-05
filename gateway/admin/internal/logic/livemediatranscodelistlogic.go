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

type LiveMediaTranscodeListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 转码任务分页（房间/场次/状态/模板过滤）
func NewLiveMediaTranscodeListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveMediaTranscodeListLogic {
	return &LiveMediaTranscodeListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveMediaTranscodeList 聚合 live-media ListLiveTranscodeTasks。
//
// 四个过滤位（room_id/live_session_id/state/template_id）在服务侧都是「0 或 UNSPECIFIED = 不过滤」，
// 网关只拒负数（负值没有任何下游语义，见 conv_livemedia.go），不复算分页：pn/ps 的上限与夹取
// 由 live-media 决定（PageParam 注释 ps<=50）。空列表是合法结果，与「客户端未配置」不同——
// 后者在函数第一行就返回 errLiveMediaNotConfigured，不会被读成「这个房间没有转码任务」。
func (l *LiveMediaTranscodeListLogic) LiveMediaTranscodeList(req *types.ParamLiveMediaTranscodeList) (resp *types.LiveMediaTranscodeListResponse, err error) {
	if l.svcCtx.LiveMedia == nil {
		return nil, errLiveMediaNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	page, err := liveMediaPage(req.Pn, req.Ps)
	if err != nil {
		return nil, err
	}
	for _, f := range []struct {
		name string
		v    int64
	}{
		{"room_id", req.RoomId},
		{"live_session_id", req.SessionId},
		{"template_id", req.TemplateId},
	} {
		if err := liveNonNeg(f.name, f.v); err != nil {
			return nil, err
		}
	}
	if err := liveNonNeg32("state", req.State); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveMedia.ListLiveTranscodeTasks(l.ctx, &livemediarpc.ListLiveTranscodeTasksReq{
		RoomId:        req.RoomId,
		LiveSessionId: req.SessionId,
		State:         livemediarpc.LiveTranscodeState(req.State),
		TemplateId:    req.TemplateId,
		Page:          page,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveMediaTranscodeList: room_id=%d live_session_id=%d state=%d err=%v",
			req.RoomId, req.SessionId, req.State, err)
		return nil, err
	}
	return &types.LiveMediaTranscodeListResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveMediaTranscodeListData{
			Total: liveMediaPageTotal(reply.GetPage()),
			List:  liveMediaTranscodeTasksToAPI(reply.GetTasks()),
		},
		TTL: 0,
	}, nil
}
