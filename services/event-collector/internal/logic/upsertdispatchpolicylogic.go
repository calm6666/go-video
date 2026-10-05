// Code scaffolded by goctl. Safe to edit.

package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/event-collector/internal/svc"
	"go-video/services/event-collector/model"
	"go-video/services/event-collector/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpsertDispatchPolicyLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpsertDispatchPolicyLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpsertDispatchPolicyLogic {
	return &UpsertDispatchPolicyLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 新建/修改采样与脱敏策略草稿。
//
// 生命周期（AGENTS.md §5）：Upsert 只碰 DRAFT，生效必须走 ActivateDispatchPolicy。
// 「同一版本既能改又能生效」等于允许事后改写历史批次的归因依据，重放会得出相反结论。
//
// 幂等：idempotency_key 走 withRoundDedup 回放首次结论；版本唯一性由
// ec_dispatch_policy.uniq_version 在库层保证，Redis 缺失时它继续兜住重复副作用。
// 校验全在 policy.go 的 policyFromRPC 里（含「白名单不得覆盖内置隐私底线」），
// 形状非法的请求在抢幂等键之前就拒绝，不占用执行权。
func (l *UpsertDispatchPolicyLogic) UpsertDispatchPolicy(in *rpc.UpsertDispatchPolicyReq) (*rpc.DispatchPolicyReply, error) {
	operator := strings.TrimSpace(in.GetOperator())
	if operator == "" || len(operator) > maxOperatorBytes {
		return nil, model.ErrOperatorRequired
	}
	key := strings.TrimSpace(in.GetIdempotencyKey())
	if key == "" || len(key) > maxIdemKeyBytes {
		return nil, model.ErrIdempotencyKeyRequired
	}
	draft, err := policyFromRPC(in.GetPolicy(), operator, l.svcCtx.Config)
	if err != nil {
		return nil, err
	}
	return withRoundDedup(l.ctx, l.svcCtx, rpcUpsertDispatchPolicy, key, l.Logger,
		func() *rpc.DispatchPolicyReply { return &rpc.DispatchPolicyReply{} },
		func() (*rpc.DispatchPolicyReply, error) {
			return l.writeDraft(draft, key, operator)
		})
}

// writeDraft 写入草稿并回读库里的真实行作为响应。
//
// 回读而非回带入参：响应必须是「库里实际存下来的那份」，否则调用方会把
// 被继承/收敛过的默认值误当成自己配的值，事后归因就对不上。
func (l *UpsertDispatchPolicyLogic) writeDraft(draft *model.DispatchPolicy, key, operator string) (*rpc.DispatchPolicyReply, error) {
	_, created, err := l.svcCtx.Policies.InsertDraft(l.ctx, nil, draft)
	if err != nil {
		return nil, err
	}
	if !created {
		exist, err := l.svcCtx.Policies.FindByVersion(l.ctx, draft.Version)
		if err != nil {
			return nil, err
		}
		switch exist.State {
		case model.PolicyStateActive:
			return nil, fmt.Errorf("%w: version=%s 正在生效，要改参数请新建版本再 Activate",
				model.ErrActivePolicyImmutable, exist.Version)
		case model.PolicyStateArchived:
			return nil, fmt.Errorf("%w: version=%s 已归档、只读：历史批次的归因依据不能事后改写，请新建版本",
				model.ErrPolicyNotDraft, exist.Version)
		}
		applied, err := l.svcCtx.Policies.UpdateDraft(l.ctx, nil, draft)
		if err != nil {
			return nil, err
		}
		if !applied {
			// 读到 DRAFT 与写入之间被并发改成 ACTIVE/ARCHIVED：不重试、不覆盖，
			// 让调用方重读版本状态再决定（乐观并发的正确姿势）。
			return nil, fmt.Errorf("%w: 草稿 %s 在写入瞬间已不是 DRAFT，请重读版本状态",
				model.ErrConcurrentUpdate, draft.Version)
		}
	}

	stored, err := l.svcCtx.Policies.FindByVersion(l.ctx, draft.Version)
	if err != nil {
		return nil, err
	}
	// ACTIVE 缓存失效（blueprint 第 6 条）。cachedActivePolicy 只会缓存 FindActive 的结果、
	// 读取时再校验 state==ACTIVE，所以草稿本身不可能被当成生效版本；
	// 这里仍然失效一次，把「策略表刚被写过」如实告诉缓存层，最坏代价只是一次回源。
	invalidateActivePolicyCache(l.ctx, l.svcCtx)

	policy, err := policyToRPC(stored)
	if err != nil {
		return nil, err
	}
	// 审计（AGENTS.md §8）：本库没有策略审计表（README 已知缺口），变更原因落在 note 列，
	// 这里补一条含「谁、哪次请求、新建还是修改」的日志，两者合起来才是一条完整证据链。
	l.Infof("event-collector/logic: 策略草稿写入 version=%s created=%t operator=%s idempotency_key=%s "+
		"salt_version=%d salt_ref=%s", stored.Version, created, fitColumn(operator, maxOperatorBytes),
		fitColumn(key, maxIdemKeyBytes), stored.SaltVersion, fitColumn(stored.SaltRef, maxSaltRefBytes))
	return &rpc.DispatchPolicyReply{Policy: policy, Created: created}, nil
}
