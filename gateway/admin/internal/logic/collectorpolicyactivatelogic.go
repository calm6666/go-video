// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	collectorrpc "go-video/services/event-collector/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CollectorPolicyActivateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 切换生效策略版本（旧 ACTIVE 转 ARCHIVED；expected_current_version 做乐观校验）
func NewCollectorPolicyActivateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CollectorPolicyActivateLogic {
	return &CollectorPolicyActivateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CollectorPolicyActivate 转发 event-collector ActivateDispatchPolicy。
// 与 /policy/upsert 分属两个权限点：登记一版还没生效的草稿只影响将来的自己，
// 切 ACTIVE 则立刻改变所有终端上报的采样与脱敏口径（进而改变下游 spm 特征完整性），
// 所以它是本域最重的一步动作，必须能被单独授予与单独收回。
//
// operator 由会话渲染、reason 与 idempotency_key 必填（切换的审计义务三件套）；
// expected_current_version 是 optional 的乐观锁位：给得越全越安全，但空值在契约里
// 明确表示「不管当前是哪版都切」，网关不替调用方补——补出来的 CAS 不是调用方的意图。
//
// 版本格式、盐能否取到（ErrSaltMissing 时拒绝激活，绝不退化成无盐哈希）、
// 事务内的 FOR UPDATE 串行化与 uniq_active 库层约束、旧版本自动 ARCHIVED
// 全部在服务侧；created 恒为 false（本方法不新建版本），如实投影不省略。
func (l *CollectorPolicyActivateLogic) CollectorPolicyActivate(req *types.ParamCollectorPolicyActivate) (resp *types.CollectorPolicyResponse, err error) {
	if l.svcCtx.EventCollector == nil {
		return nil, errCollectorServiceNotConfigured
	}
	if req == nil {
		return nil, errCollectorRequestMissing
	}
	operator, err := collectorOperator(l.ctx, "collectorPolicyActivate")
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("version", req.Version); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.EventCollector.ActivateDispatchPolicy(l.ctx, &collectorrpc.ActivateDispatchPolicyReq{
		Version:                req.Version,
		ExpectedCurrentVersion: req.ExpectedCurrentVersion,
		IdempotencyKey:         req.IdempotencyKey,
		Operator:               operator,
		Reason:                 req.Reason,
	})
	if err != nil {
		l.Errorf("gateway/admin/collectorPolicyActivate: version=%s expected=%s operator=%s err=%v",
			req.Version, req.ExpectedCurrentVersion, operator, err)
		return nil, err
	}
	return &types.CollectorPolicyResponse{
		Code:    0,
		Message: "ok",
		Data: types.CollectorPolicyData{
			Policy:  collectorPolicyToAPI(reply.GetPolicy()),
			Created: reply.GetCreated(),
		},
		TTL: 0,
	}, nil
}
