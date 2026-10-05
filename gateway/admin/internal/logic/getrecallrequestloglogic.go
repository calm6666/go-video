// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	recallrpc "go-video/services/recommend-recall/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetRecallRequestLogLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 回放一次在线召回请求（按 request_id 或 snapshot_id）
func NewGetRecallRequestLogLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetRecallRequestLogLogic {
	return &GetRecallRequestLogLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// GetRecallRequestLog 转发 recommend-recall GetRecallRequestLog。
// request_id 与 snapshot_id 至少给一个（两个都空时下游按空主键查一条不存在的日志，
// 回「未命中」而不是「你少传了参数」），谁优先、格式是否合法都由服务判定。
// found 表达「服务有没有回这一条」：契约明确 entry 不存在时为 null，
// 网关不拿全零值的 entry 冒充命中（与 live 的 has_setting 同一口径）。
// 日志只打两个键：mid/scene/region 这些用户维度不进网关日志（AGENTS.md §7）。
func (l *GetRecallRequestLogLogic) GetRecallRequestLog(req *types.ParamRecommendRecallLogGet) (resp *types.RecommendRecallLogResponse, err error) {
	if l.svcCtx.RecommendRecall == nil {
		return nil, errRecallServiceNotConfigured
	}
	if req == nil {
		return nil, errRecommendRequestMissing
	}
	if !recommendAtLeastOne(req.RequestId, req.SnapshotId) {
		return nil, errRecallLogSubjectRequired
	}
	reply, err := l.svcCtx.RecommendRecall.GetRecallRequestLog(l.ctx, &recallrpc.GetRecallRequestLogReq{
		RequestId:  req.RequestId,
		SnapshotId: req.SnapshotId,
	})
	if err != nil {
		l.Errorf("gateway/admin/getRecallRequestLog: request_id=%s snapshot_id=%s err=%v",
			req.RequestId, req.SnapshotId, err)
		return nil, err
	}
	entry := reply.GetEntry()
	return &types.RecommendRecallLogResponse{
		Code:    0,
		Message: "ok",
		Data: types.RecommendRecallLogData{
			Entry: recallRequestLogToAPI(entry),
			Found: entry != nil,
		},
		TTL: 0,
	}, nil
}
