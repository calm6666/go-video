// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	featurestorerpc "go-video/services/feature-store/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type FsBackfillListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 回填任务列表分页（可按 feature_key/state/since 过滤）
func NewFsBackfillListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *FsBackfillListLogic {
	return &FsBackfillListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// FsBackfillList 转发 feature-store ListBackfillJobs（回填任务分页）。
//
// state 与 since 的 0 都是「不限」哨兵（BACKFILL_STATE_UNSPECIFIED 在这里不是错误入参，
// 而是「不按状态过滤」），只挡负数；状态机是否允许从 PENDING 直接到 CANCELLED 一类判定在服务。
func (l *FsBackfillListLogic) FsBackfillList(req *types.ParamFsBackfillList) (resp *types.FsBackfillListResponse, err error) {
	if l.svcCtx.FeatureStore == nil {
		return nil, errFeatureStoreNotConfigured
	}
	if req == nil {
		return nil, errFsRequestMissing
	}
	if err := fsPaging(req.Pn, req.Ps); err != nil {
		return nil, err
	}
	if err := fsNonNeg("state", int64(req.State)); err != nil {
		return nil, err
	}
	if err := fsNonNeg("since", req.Since); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.FeatureStore.ListBackfillJobs(l.ctx, &featurestorerpc.ListBackfillJobsReq{
		FeatureKey: req.FeatureKey,
		State:      featurestorerpc.BackfillState(req.State),
		Since:      req.Since,
		Pn:         req.Pn,
		Ps:         req.Ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/fsBackfillList: feature_key=%s state=%d since=%d pn=%d ps=%d err=%v",
			req.FeatureKey, req.State, req.Since, req.Pn, req.Ps, err)
		return nil, err
	}
	return &types.FsBackfillListResponse{
		Code:    0,
		Message: "ok",
		Data: types.FsBackfillListData{
			Jobs:  fsJobsToAPI(reply.GetJobs()),
			Total: reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
