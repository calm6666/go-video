// Code scaffolded by goctl. Safe to edit.

package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go-video/services/event-collector/internal/svc"
	"go-video/services/event-collector/model"
	"go-video/services/event-collector/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type ActivateDispatchPolicyLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewActivateDispatchPolicyLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ActivateDispatchPolicyLogic {
	return &ActivateDispatchPolicyLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 切换生效策略版本（旧版本转 ARCHIVED，保留归因）。
//
// 原子性：整个切换跑在一个 DB.TransactCtx 里（model.Activate 强制 session 非空）——
// SELECT ... FOR UPDATE 锁住当前 ACTIVE → 归档（active_flag=NULL）→ 激活新版本。
// 分两次自动提交会留下「零个或两个 ACTIVE」，比切换失败严重得多；
// 「同一时刻只有一版生效」最终由 uniq_active(active_flag) 在库层保证，
// 并发切换的第二笔撞唯一键 → ErrConcurrentUpdate。
//
// 审计三件套（operator / reason / idempotency_key）必填：策略切换直接决定下游特征完整性
// （AGENTS.md §8 必须能回答「谁、凭哪次请求、为什么」）。
//
// 失败即不生效：盐取不到（ErrSaltMissing）时拒绝激活 —— 生效之后采集侧就要拿它给设备号
// 打哈希，宁可此刻失败，也绝不退化成无盐哈希（AGENTS.md §7）。
func (l *ActivateDispatchPolicyLogic) ActivateDispatchPolicy(in *rpc.ActivateDispatchPolicyReq) (*rpc.DispatchPolicyReply, error) {
	operator := strings.TrimSpace(in.GetOperator())
	if operator == "" || len(operator) > maxOperatorBytes {
		return nil, model.ErrOperatorRequired
	}
	reason := strings.TrimSpace(in.GetReason())
	if reason == "" {
		return nil, fmt.Errorf("%w: 切换生效策略必须给出 reason（为什么切换，审计义务）", model.ErrOperatorRequired)
	}
	key := strings.TrimSpace(in.GetIdempotencyKey())
	if key == "" || len(key) > maxIdemKeyBytes {
		return nil, model.ErrIdempotencyKeyRequired
	}
	version := strings.TrimSpace(in.GetVersion())
	if !validPolicyVersion(version) {
		return nil, fmt.Errorf("event-collector: version=%q 非法（形如 2026.09.20-1，<= %d 字节）",
			fitColumn(version, 64), maxVersionTagBytes)
	}
	expected := strings.TrimSpace(in.GetExpectedCurrentVersion())
	if len(expected) > maxPolicyVerBytes {
		return nil, fmt.Errorf("%w: expected_current_version 过长", model.ErrInvalidPage)
	}

	return withRoundDedup(l.ctx, l.svcCtx, rpcActivateDispatchPolicy, key, l.Logger,
		func() *rpc.DispatchPolicyReply { return &rpc.DispatchPolicyReply{} },
		func() (*rpc.DispatchPolicyReply, error) {
			return l.switchActive(version, expected, operator, reason, key)
		})
}

// switchActive 执行一次版本切换：先验盐，再在事务里 CAS 换 ACTIVE，最后失效缓存并回读新策略。
func (l *ActivateDispatchPolicyLogic) switchActive(version, expected, operator, reason, key string) (*rpc.DispatchPolicyReply, error) {
	target, err := l.svcCtx.Policies.FindByVersion(l.ctx, version)
	if err != nil {
		// 未知版本必须由调用方拿到明确的 ErrPolicyNotFound，不能回空 Reply + nil。
		return nil, err
	}
	// 已在生效且没有乐观锁冲突时，model.Activate 会幂等返回；盐检查对它同样适用：
	// 「生效但盐取不到」正是需要立刻被拦住的状态。
	if err := ensureSaltUsable(l.svcCtx, target); err != nil {
		return nil, fmt.Errorf("%w: 策略 %s 声明的 salt_ref=%s 取不到盐值，拒绝生效",
			err, target.Version, fitColumn(target.SaltRef, maxSaltRefBytes))
	}

	var previous string
	err = l.svcCtx.DB.TransactCtx(l.ctx, func(ctx context.Context, session sqlx.Session) error {
		prev, aerr := l.svcCtx.Policies.WithSession(session).Activate(ctx, session, version, expected, operator)
		previous = prev
		return aerr
	})
	if err != nil {
		if errors.Is(err, model.ErrConcurrentUpdate) && previous != "" {
			return nil, fmt.Errorf("%w: 当前生效版本是 %q，与 expected_current_version=%q 不符；"+
				"请重读生效版本后重试（两个运营基于同一份旧页面互相覆盖就是这么发生的）",
				err, previous, expected)
		}
		return nil, err
	}

	// 切换成功即失效 ACTIVE 缓存：新采集请求立刻按新版本裁决。
	// 已在途的批次仍按各自写入的 policy_version 归因，不受本次切换影响（blueprint 第 5 条）。
	invalidateActivePolicyCache(l.ctx, l.svcCtx)

	stored, err := l.svcCtx.Policies.FindByVersion(l.ctx, version)
	if err != nil {
		return nil, err
	}
	policy, err := policyToRPC(stored)
	if err != nil {
		return nil, err
	}
	l.Infof("event-collector/logic: 生效策略切换 previous=%s -> %s operator=%s idempotency_key=%s reason=%s",
		fitColumn(previous, maxPolicyVerBytes), stored.Version, fitColumn(operator, maxOperatorBytes),
		fitColumn(key, maxIdemKeyBytes), redactSensitive(reason))
	return &rpc.DispatchPolicyReply{Policy: policy, Created: false}, nil
}
