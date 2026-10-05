// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	collectorrpc "go-video/services/event-collector/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CollectorDeadLetterReplayLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 重放投递死信（重新入队，不重新采样/脱敏；已是终态的计入 skipped）
func NewCollectorDeadLetterReplayLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CollectorDeadLetterReplayLogic {
	return &CollectorDeadLetterReplayLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CollectorDeadLetterReplay 转发 event-collector ReplayDeadLetter。
// 审计三件套齐备才算这次处置：operator 由会话渲染、reason 契约必填、idempotency_key 必填。
// 死信是「已经放弃投递」的终态台账，重放是唯一能把它推回在途的后台动作，
// 「谁、凭哪次请求、为什么」三列都落在 ec_dead_letter 行上（AGENTS.md §5/§8）。
//
// 网关只挡两件形状问题：ID 列表非空且每个元素为正（0/负数在库里没有对应行，
// 却会换回一个「处置了 0 条」的成功结论，是排障陷阱而不是失败）、reason/幂等键非空。
// 单次条数上限（MaxReplayPerRequest）、ID 归一化与去重、逐行状态迁移（只接 state=open）
// 与「dispatcher 未启用时在写入之前拒绝」全部在服务侧。
//
// skipped 与 failed_ids 都是结论而非错误：前者是「已是 replayed/discarded 终态」，
// 后者是「回队失败、仍留在 open 可按同一批 ID 原样再来一次」。网关不把任一折成 HTTP 失败，
// 也不重新采样：首次入库定型的 policy_version/sanitize_version 必须保持，否则归因对不上。
func (l *CollectorDeadLetterReplayLogic) CollectorDeadLetterReplay(req *types.ParamCollectorDeadLetterReplay) (resp *types.CollectorDeadLetterReplayResponse, err error) {
	if l.svcCtx.EventCollector == nil {
		return nil, errCollectorServiceNotConfigured
	}
	if req == nil {
		return nil, errCollectorRequestMissing
	}
	operator, err := collectorOperator(l.ctx, "collectorDeadLetterReplay")
	if err != nil {
		return nil, err
	}
	if len(req.DeadLetterIds) == 0 {
		return nil, errCollectorDeadLetterIDsRequired
	}
	for _, id := range req.DeadLetterIds {
		if id <= 0 {
			return nil, errors.New("gateway/admin: dead_letter_ids must be > 0")
		}
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.EventCollector.ReplayDeadLetter(l.ctx, &collectorrpc.ReplayDeadLetterReq{
		DeadLetterIds:  collectorInt64s(req.DeadLetterIds),
		IdempotencyKey: req.IdempotencyKey,
		Operator:       operator,
		Reason:         req.Reason,
	})
	if err != nil {
		l.Errorf("gateway/admin/collectorDeadLetterReplay: ids=%d operator=%s err=%v",
			len(req.DeadLetterIds), operator, err)
		return nil, err
	}
	return &types.CollectorDeadLetterReplayResponse{
		Code:    0,
		Message: "ok",
		Data: types.CollectorDeadLetterReplayData{
			Replayed:  reply.GetReplayed(),
			Skipped:   reply.GetSkipped(),
			FailedIds: collectorInt64s(reply.GetFailedIds()),
		},
		TTL: 0,
	}, nil
}
