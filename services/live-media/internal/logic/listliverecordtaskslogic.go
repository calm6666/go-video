package logic

import (
	"context"

	"go-video/services/live-media/internal/svc"
	"go-video/services/live-media/model"
	"go-video/services/live-media/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListLiveRecordTasksLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListLiveRecordTasksLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListLiveRecordTasksLogic {
	return &ListLiveRecordTasksLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询录制任务
//
// 只读方法：不写库、不发事件、不推进状态。
// 过滤语义（与 model.RecordTaskFilter 一致）：room_id / live_session_id 非正数不过滤；
// state=UNSPECIFIED 不过滤，其余取值必须是已定义的 rpc.LiveRecordState（未知取值报错）。
// 「房间与场次都不给」的全表读仍允许（运营排障），扫描面靠 idx_room_state_ctime /
// idx_session_state 与 listPage 的深翻页窗口共同限住。
// 排序固定 record_id DESC；分页口径见 listPage。
// 投影逐行映射 LiveRecordTaskInfo，其中 segment_count / gap_count / recorded_duration_ms
// 是可由切片表重算的派生列（RefreshStats 修复），last_seq 才是续录锚点，
// 二者都按库中原值如实返回，本方法不做二次推算（读接口绝不写回，包括不顺手重算统计）。
func (l *ListLiveRecordTasksLogic) ListLiveRecordTasks(in *rpc.ListLiveRecordTasksReq) (*rpc.ListLiveRecordTasksReply, error) {
	cfg := l.svcCtx.Config.LiveMedia
	state, err := filterState(int32(in.GetState()), checkRecordState)
	if err != nil {
		return nil, err
	}
	pn, ps, err := listPage(cfg, in.GetPage().GetPn(), in.GetPage().GetPs())
	if err != nil {
		return nil, err
	}
	rows, total, err := l.svcCtx.RecordTasks.List(l.ctx, model.RecordTaskFilter{
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
	return &rpc.ListLiveRecordTasksReply{Page: pageResult(total), Tasks: recordInfos(rows)}, nil
}
