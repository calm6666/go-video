package logic

import (
	"context"

	"go-video/services/live-media/internal/svc"
	"go-video/services/live-media/model"
	"go-video/services/live-media/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListReplayTasksLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListReplayTasksLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListReplayTasksLogic {
	return &ListReplayTasksLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询回放任务
//
// 只读方法：不写库、不发事件。
// 过滤语义（与 model.ReplayTaskFilter 一致）：room_id / live_session_id 非正数不过滤；
// state=UNSPECIFIED 不过滤，其余取值必须是已定义的 rpc.ReplayState（未知取值报错）。
// model 侧还支持的 RecordId/AnchorMid 过滤在 rpc 未暴露（契约缺口，见 README），
// 本方法不凭空接受未声明参数，二者恒传 0。
// 分页见 listPage（ps 越界夹取、pn 超深翻页窗口拒绝）；排序固定 replay_id DESC。
// 投影边界（必须如实表达，不得美化）：LiveReplayTaskInfo 不含 review_state——
// 稿件的事实状态只在 live_replay_asset_ref 做 video 侧只读投影，两处暴露同义字段
// 必然出现不一致；要按发布态筛选必须走 ListReplayAssetRefs。
func (l *ListReplayTasksLogic) ListReplayTasks(in *rpc.ListReplayTasksReq) (*rpc.ListReplayTasksReply, error) {
	cfg := l.svcCtx.Config.LiveMedia
	state, err := filterState(int32(in.GetState()), checkReplayState)
	if err != nil {
		return nil, err
	}
	pn, ps, err := listPage(cfg, in.GetPage().GetPn(), in.GetPage().GetPs())
	if err != nil {
		return nil, err
	}
	rows, total, err := l.svcCtx.ReplayTasks.List(l.ctx, model.ReplayTaskFilter{
		RoomId:      in.GetRoomId(),
		SessionId:   in.GetLiveSessionId(),
		State:       state,
		Pn:          pn,
		Ps:          ps,
		MaxPageSize: cfg.MaxListPageSize,
	})
	if err != nil {
		return nil, err
	}
	return &rpc.ListReplayTasksReply{Page: pageResult(total), Tasks: replayInfos(rows)}, nil
}
