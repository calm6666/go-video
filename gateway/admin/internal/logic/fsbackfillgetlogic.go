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

type FsBackfillGetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 单个回填任务读（job_id 或 request_id 二选一；断点与 last_error 原样回）
func NewFsBackfillGetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *FsBackfillGetLogic {
	return &FsBackfillGetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// FsBackfillGet 转发 feature-store GetBackfillJob（按 job_id 或 request_id 查一个回填任务）。
//
// 二选一主体必须有一个：两者都没给时下游会按空主键查一条不存在的任务，
// 后台于是看到「found=false」并推断「我的任务丢了」——真实原因只是没给寻址位，
// 这类假线索值得在网关挡掉（fsJobSubject）。request_id 用 TrimSpace 判空但原值透传：
// 幂等键改一个字符等于换一次执行权。
// cursor_entity_id/entities_failed/last_error 是断点续跑与失败归因的证据，全字段回；
// last_error 已由服务截断且不含 SQL 与特征值原文，网关不再改写。
func (l *FsBackfillGetLogic) FsBackfillGet(req *types.ParamFsBackfillGet) (resp *types.FsBackfillGetResponse, err error) {
	if l.svcCtx.FeatureStore == nil {
		return nil, errFeatureStoreNotConfigured
	}
	if req == nil {
		return nil, errFsRequestMissing
	}
	if err := fsJobSubject(req.JobId, req.RequestId); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.FeatureStore.GetBackfillJob(l.ctx, &featurestorerpc.GetBackfillJobReq{
		JobId:     req.JobId,
		RequestId: req.RequestId,
	})
	if err != nil {
		l.Errorf("gateway/admin/fsBackfillGet: job_id=%d request_id=%s err=%v",
			req.JobId, req.RequestId, err)
		return nil, err
	}
	return &types.FsBackfillGetResponse{
		Code:    0,
		Message: "ok",
		Data: types.FsBackfillGetData{
			Found: reply.GetFound(),
			Job:   fsJobToAPI(reply.GetJob()),
		},
		TTL: 0,
	}, nil
}
