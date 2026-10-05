package logic

import (
	"context"

	"go-video/services/live-media/internal/svc"
	"go-video/services/live-media/model"
	"go-video/services/live-media/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListRetentionTasksLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListRetentionTasksLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListRetentionTasksLogic {
	return &ListRetentionTasksLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询回收任务
//
// 只读方法：不写库、不发事件；Worker 领取待执行队列不经过本方法（那会退化成轮询全表，
// 队列入口是 model.ListByState，未在 rpc 暴露）。
// 过滤语义（与 model.RetentionFilter 一致）：target_kind / state 的 UNSPECIFIED 不过滤，
// 非 0 取值必须落在各自枚举区间内（未知取值报错，不答成「没有任务」）。
// room_id<=0 不过滤：0 同时是登记时「全局扫描任务」的合法列值，因此本入口无法表达
// 「只查全局任务」（要靠 ListByState 的内部队列，属契约缺口，见 README）。
// 分页见 listPage；排序固定 retention_id DESC。
// 投影：scanned/deleted/skipped 是 Worker 覆盖写（不是累加）的结果证据，
// purge=false 时 deleted 恒为 0——那是「只登记未真删」的预期结果，不是失败；
// 本服务不对计数做二次推断。reason/operator/trace_id 是审计链路（AGENTS.md §8），原样回传。
func (l *ListRetentionTasksLogic) ListRetentionTasks(in *rpc.ListRetentionTasksReq) (*rpc.ListRetentionTasksReply, error) {
	cfg := l.svcCtx.Config.LiveMedia
	targetKind, err := filterState(int32(in.GetTargetKind()), checkRetentionTarget)
	if err != nil {
		return nil, err
	}
	state, err := filterState(int32(in.GetState()), checkRetentionState)
	if err != nil {
		return nil, err
	}
	pn, ps, err := listPage(cfg, in.GetPage().GetPn(), in.GetPage().GetPs())
	if err != nil {
		return nil, err
	}
	rows, total, err := l.svcCtx.RetentionTasks.List(l.ctx, model.RetentionFilter{
		TargetKind:  targetKind,
		State:       state,
		RoomId:      in.GetRoomId(),
		Pn:          pn,
		Ps:          ps,
		MaxPageSize: cfg.MaxListPageSize,
	})
	if err != nil {
		return nil, err
	}
	return &rpc.ListRetentionTasksReply{Page: pageResult(total), Tasks: retentionInfos(rows)}, nil
}
