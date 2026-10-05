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

type PrunePoolVersionsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分批清理过期池版本（dry_run 先核数；keep_versions 下限由服务守护）
func NewPrunePoolVersionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PrunePoolVersionsLogic {
	return &PrunePoolVersionsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// PrunePoolVersions 转发 recommend-recall PrunePoolVersions。
// 这是本域唯一会**删数据**的路由，所以门槛比别处更实：会话身份（operator 由它渲染，
// 服务的 operator 必填正落在这里）+ 池寻址 + request_id 非空。
// keep_versions 的下限（MinKeepVersions，默认 2）与「哪些版本在保留窗口外」由服务判定，
// 网关不复制这个数——服务调默认值时后台不必跟着改代码。
// 契约缺口：PrunePoolVersionsReq 既没有 idempotency_key 也没有 trace_id，
// 所以 request_id 只能作为网关侧的防重与留痕键，无法传给服务做真正的重放去重；
// 删除本身是单调动作（已删的行不会再删一次），运维流程要求先 dry_run=true 核对 scanned_versions
// 再执行（见 README）。has_more=true 表示还有内容可清，需要继续分批，网关不自动循环。
func (l *PrunePoolVersionsLogic) PrunePoolVersions(req *types.ParamRecommendPoolVersionPrune) (resp *types.RecommendPoolVersionPruneResponse, err error) {
	if l.svcCtx.RecommendRecall == nil {
		return nil, errRecallServiceNotConfigured
	}
	if req == nil {
		return nil, errRecommendRequestMissing
	}
	operator, err := recommendOperator(l.ctx, "prunePoolVersions")
	if err != nil {
		return nil, err
	}
	pool, err := recommendPoolRef(req.Pool.Source, req.Pool.PoolKey)
	if err != nil {
		return nil, err
	}
	if err := recommendNonNeg("keep_versions", int64(req.KeepVersions)); err != nil {
		return nil, err
	}
	if err := recommendNonNeg("max_rows", req.MaxRows); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("request_id", req.RequestId); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.RecommendRecall.PrunePoolVersions(l.ctx, &recallrpc.PrunePoolVersionsReq{
		Pool:         pool,
		KeepVersions: req.KeepVersions,
		MaxRows:      req.MaxRows,
		DryRun:       req.DryRun,
		Operator:     operator,
	})
	if err != nil {
		l.Errorf("gateway/admin/prunePoolVersions: source=%d pool_key=%s keep_versions=%d max_rows=%d dry_run=%t operator=%s request_id=%s err=%v",
			req.Pool.Source, req.Pool.PoolKey, req.KeepVersions, req.MaxRows, req.DryRun, operator, req.RequestId, err)
		return nil, err
	}
	return &types.RecommendPoolVersionPruneResponse{
		Code:    0,
		Message: "ok",
		Data: types.RecommendPoolVersionPruneData{
			ScannedVersions: reply.GetScannedVersions(),
			DeletedRows:     reply.GetDeletedRows(),
			DryRun:          reply.GetDryRun(),
			HasMore:         reply.GetHasMore(),
		},
		TTL: 0,
	}, nil
}
