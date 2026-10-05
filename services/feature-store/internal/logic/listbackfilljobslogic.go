package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/feature-store/internal/svc"
	"go-video/services/feature-store/model"
	"go-video/services/feature-store/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListBackfillJobsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListBackfillJobsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListBackfillJobsLogic {
	return &ListBackfillJobsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 回填任务列表（分页）
//
// 供运营与 services/cron 巡检回填进度：入参枚举先经 model.ValidBackfillState 校验，
// 分页经 model.ValidatePageSize 收在 model.MaxListPageSize 内（未认领作业的扫描另有
// ListClaimable 一条按 job_id 升序的专用路径，不共用这个对外分页）。
func (l *ListBackfillJobsLogic) ListBackfillJobs(
	in *rpc.ListBackfillJobsReq) (*rpc.ListBackfillJobsReply, error) {
	if err := model.ValidatePageSize(in.GetPn(), in.GetPs()); err != nil {
		return nil, err
	}
	key := strings.TrimSpace(in.GetFeatureKey())
	if key != "" {
		if err := checkFeatureKey(key); err != nil {
			return nil, err
		}
	}
	state := int32(in.GetState())
	if state != model.BackfillStateUnspecified && !model.ValidBackfillState(state) {
		return nil, fmt.Errorf("%w: state %d", model.ErrJobStateInvalid, state)
	}
	if in.GetSince() < 0 {
		return nil, model.ErrBackfillWindowInvalid
	}
	jobs, total, err := l.svcCtx.Backfills.List(l.ctx, model.BackfillJobFilter{
		FeatureKey: key,
		State:      state,
		Since:      in.GetSince(),
		Pn:         in.GetPn(),
		Ps:         in.GetPs(),
	})
	if err != nil {
		return nil, err
	}
	items := make([]*rpc.BackfillJob, 0, len(jobs))
	for _, j := range jobs {
		items = append(items, backfillToProto(j))
	}
	return &rpc.ListBackfillJobsReply{Jobs: items, Total: total}, nil
}
