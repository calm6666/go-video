package logic

import (
	"context"

	"go-video/services/live-media/internal/svc"
	"go-video/services/live-media/model"
	"go-video/services/live-media/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListLiveTranscodeTasksLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListLiveTranscodeTasksLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListLiveTranscodeTasksLogic {
	return &ListLiveTranscodeTasksLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询转码任务（房间/场次/状态/模板）
//
// 只读方法：不写库、不写事件、不推进状态；响应恒带 rpc.PageResult{total}。
// 过滤语义（与 model.TranscodeTaskFilter 一致）：room_id / live_session_id / template_id
// 非正数表示不参与过滤；state=UNSPECIFIED 不过滤，其余取值必须落在
// rpc.LiveTranscodeState 区间内——未知取值报错而不是退化成「恒空结果集」，
// 否则调用方的版本错误会被读成「这个房间没有任务」。
// 排序固定 task_id DESC（新任务优先），调用方无法传 order by 进 SQL。
// 分页见 listPage：ps 越界按 proto 契约夹取，pn 深到 OFFSET 超出保护窗口则拒绝。
// total=0 或越界页都返回空列表 + 正确 total，由客户端据此收敛，不当错误。
func (l *ListLiveTranscodeTasksLogic) ListLiveTranscodeTasks(in *rpc.ListLiveTranscodeTasksReq) (*rpc.ListLiveTranscodeTasksReply, error) {
	cfg := l.svcCtx.Config.LiveMedia
	state, err := filterState(int32(in.GetState()), checkTranscodeState)
	if err != nil {
		return nil, err
	}
	pn, ps, err := listPage(cfg, in.GetPage().GetPn(), in.GetPage().GetPs())
	if err != nil {
		return nil, err
	}
	rows, total, err := l.svcCtx.TranscodeTasks.List(l.ctx, model.TranscodeTaskFilter{
		RoomId:      in.GetRoomId(),
		SessionId:   in.GetLiveSessionId(),
		State:       state,
		TemplateId:  in.GetTemplateId(),
		Pn:          pn,
		Ps:          ps,
		MaxPageSize: cfg.MaxListPageSize,
	})
	if err != nil {
		return nil, err
	}
	return &rpc.ListLiveTranscodeTasksReply{Page: pageResult(total), Tasks: transcodeInfos(rows)}, nil
}
