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

type UpsertPoolItemsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分批写入池条目到指定版本（is_last_batch 置 READY；幂等键命中重放不重复写）
func NewUpsertPoolItemsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpsertPoolItemsLogic {
	return &UpsertPoolItemsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// UpsertPoolItems 转发 recommend-recall UpsertPoolItems。
// 门槛：会话身份（AdminPermission 已放行才会挂上）+ 池寻址 + idempotency_key 非空且原样透传。
// 版本必须为正、同一 (pool,version) 只能属于一个 batch_id、单次条数上限 MaxBatchItems、
// pool_key 语法与 aid 是否存在全部由服务判定，网关不复算（AGENTS.md §5）。
// 契约缺口：UpsertPoolItemsReq 没有 operator 位——generator 描述「这批数据由哪个作业产出」，
// 不是操作者，所以网关保留表单的 generator 声明、只把会话 admin_id 落进日志；
// 版本行里看不到「谁点的写入」（补法是先给 proto 加运营主体字段，本轮不改 services/**，见 README）。
// deduplicated=true 是幂等重放的正常结论，网关不改写成错误也不重复计数。
func (l *UpsertPoolItemsLogic) UpsertPoolItems(req *types.ParamRecommendPoolItemUpsert) (resp *types.RecommendPoolItemUpsertResponse, err error) {
	if l.svcCtx.RecommendRecall == nil {
		return nil, errRecallServiceNotConfigured
	}
	if req == nil {
		return nil, errRecommendRequestMissing
	}
	if err := recommendSessionGate(l.ctx, "upsertPoolItems"); err != nil {
		return nil, err
	}
	pool, err := recommendPoolRef(req.Pool.Source, req.Pool.PoolKey)
	if err != nil {
		return nil, err
	}
	if err := recommendNonNeg("version", req.Version); err != nil {
		return nil, err
	}
	if err := recommendNonNeg("schema_version", int64(req.SchemaVersion)); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("batch_id", req.BatchId); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.RecommendRecall.UpsertPoolItems(l.ctx, &recallrpc.UpsertPoolItemsReq{
		Pool:           pool,
		Version:        req.Version,
		BatchId:        req.BatchId,
		Generator:      req.Generator,
		SchemaVersion:  req.SchemaVersion,
		Items:          recallPoolItemsForRPC(req.Items),
		IdempotencyKey: req.IdempotencyKey,
		IsLastBatch:    req.IsLastBatch,
	})
	if err != nil {
		// 日志只到批次粒度：条目里是 aid 集合，逐条打印既没意义也会把日志刷爆。
		l.Errorf("gateway/admin/upsertPoolItems: source=%d pool_key=%s version=%d batch_id=%s items=%d idempotency_key=%s err=%v",
			req.Pool.Source, req.Pool.PoolKey, req.Version, req.BatchId, len(req.Items), req.IdempotencyKey, err)
		return nil, err
	}
	return &types.RecommendPoolItemUpsertResponse{
		Code:    0,
		Message: "ok",
		Data: types.RecommendPoolItemUpsertData{
			Version:      reply.GetVersion(),
			Written:      reply.GetWritten(),
			ItemCount:    reply.GetItemCount(),
			State:        int32(reply.GetState()),
			Deduplicated: reply.GetDeduplicated(),
		},
		TTL: 0,
	}, nil
}
