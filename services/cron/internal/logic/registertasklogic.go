package logic

import (
	"context"
	"fmt"

	"go-video/services/cron/internal/svc"
	"go-video/services/cron/model"
	"go-video/services/cron/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type RegisterTaskLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRegisterTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RegisterTaskLogic {
	return &RegisterTaskLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 注册任务定义（task_key 唯一，重复注册幂等返回）。
//
// 幂等身份就是 uniq_task_key：注册是「声明期望状态」的动作，同 task_key 再次注册
// 只会读回既有行并给出 dedupe_reason，绝不覆盖线上调度（AGENTS.md §5）。
// idempotency_key 依然必填——它是调用方的动作标识，但契约没有给它落库列
// （README「契约缺口」），因此这里只做校验与审计留痕，不用它做去重键。
func (l *RegisterTaskLogic) RegisterTask(in *rpc.RegisterTaskReq) (*rpc.RegisterTaskReply, error) {
	if in == nil || in.Definition == nil {
		return nil, model.ErrTaskKeyEmpty
	}
	if err := requireIdempotencyKey(in.IdempotencyKey); err != nil {
		return nil, err
	}
	defaults := defaultsOf(l.svcCtx)
	// full=false：先取「裸」定义，让注册表兜底值（建议超时等）参与最终校验。
	d, err := defaults.definitionFromProto(in.Definition, false)
	if err != nil {
		return nil, err
	}
	state, err := registerState(in.Definition.State)
	if err != nil {
		return nil, err
	}
	d.State = state
	applyDefinitionDefaults(d, defaults)
	if d.TimeoutSeconds <= 0 {
		tmo, err := l.svcCtx.Registry.EffectiveTimeout(d)
		if err != nil {
			l.Errorf("RegisterTask effective timeout failed, task_key=%s handler=%s", d.TaskKey, d.Handler)
			return nil, err
		}
		d.TimeoutSeconds = tmo
	}
	// 两道校验分工明确：注册表管代码边界（SerialOnly、max_attempts 上限、租约下限、
	// handler 是否注册），model 管调度自洽性（表达式/间隔/重试/时区）。
	if err := l.svcCtx.Registry.ValidateDefinition(d); err != nil {
		return nil, err
	}
	if err := model.ValidateTaskDefinition(d); err != nil {
		return nil, err
	}
	// 调度指针由服务端现算，绝不接受调用方传入的 next_fire_at。
	nextFireAt, err := model.FirstFireAt(d, defaults.Timezone, l.svcCtx.ServerTime())
	if err != nil {
		return nil, err
	}
	d.NextFireAt = nextFireAt
	d.Operator = operatorOf(in.Operator, l.svcCtx)
	d.Version = 1

	created, existing, err := l.persist(d, in, defaults)
	if err != nil {
		l.Errorf("RegisterTask failed, task_key=%s handler=%s group=%s", d.TaskKey, d.Handler, d.TaskGroup)
		return nil, err
	}
	if created != nil {
		return &rpc.RegisterTaskReply{Definition: taskDefinitionInfo(created), Created: true}, nil
	}
	if existing == nil {
		// 插入说「已存在」却读不回：库不一致，必须显式失败而不是回零值冒充成功。
		l.Errorf("RegisterTask duplicate key without readable row, task_key=%s", d.TaskKey)
		return nil, model.ErrTaskNotFound
	}
	return &rpc.RegisterTaskReply{
		Definition: taskDefinitionInfo(existing),
		Created:    false,
		DedupeReason: fmt.Sprintf("uniq_task_key 命中：task_key=%s 已注册（version=%d, state=%s），"+
			"本次注册未覆盖线上调度；改配置请走 UpdateTask",
			existing.TaskKey, existing.Version, model.TaskStateName(existing.State)),
	}, nil
}

// persist 把「插入定义 + 写注册审计」放进同一事务。
// 返回值约定：(created 非 nil) 表示本次真的建了行；(existing 非 nil) 表示命中唯一键。
func (l *RegisterTaskLogic) persist(
	d *model.TaskDefinition, in *rpc.RegisterTaskReq, defaults ServiceDefaults,
) (created, existing *model.TaskDefinition, err error) {
	audit := &model.TaskAudit{
		TaskKey:  d.TaskKey,
		Action:   model.AuditActionRegister,
		ToState:  model.TaskStateName(d.State),
		Operator: d.Operator,
		TraceID:  in.TraceId,
		Detail: auditDetail(map[string]any{
			"handler":           d.Handler,
			"task_group":        d.TaskGroup,
			"schedule":          scheduleSummary(d),
			"next_fire_at":      d.NextFireAt,
			"timeout_seconds":   d.TimeoutSeconds,
			"max_attempts":      d.MaxAttempts,
			"concurrency_limit": d.ConcurrencyLimit,
			"lease_ttl_seconds": d.LeaseTTLSeconds,
			"misfire_policy":    d.MisfirePolicy,
			"params_bytes":      len(d.Params),
			"idempotency_key":   in.IdempotencyKey,
		}),
	}
	err = l.svcCtx.Transact(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		existed, err := l.svcCtx.TaskDefinitions.InsertTx(ctx, tx, d)
		if err != nil {
			return err
		}
		if !existed {
			created = d
			// 审计写失败必须整体回滚：不留「有调度却没痕迹」的行（AGENTS.md §8）。
			return l.svcCtx.Audits.Insert(ctx, tx, audit)
		}
		row, err := l.svcCtx.TaskDefinitions.FindOneTx(ctx, tx, d.TaskKey)
		if err != nil {
			return err
		}
		existing = row
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return created, existing, nil
}

// registerState 判定注册时的初始状态：缺省 ENABLED，只允许 ENABLED/PAUSED 两种声明。
// DISABLED 是「不再被支持」的终态声明，只能通过 DisableTask 达成。
func registerState(s rpc.TaskState) (int32, error) {
	state, err := taskStateValue(s, model.TaskStateEnabled)
	if err != nil {
		return 0, err
	}
	if state != model.TaskStateEnabled && state != model.TaskStatePaused {
		return 0, fmt.Errorf("%w: 注册只能声明 ENABLED/PAUSED，收到 %s；"+
			"停用是终态声明，请走 DisableTask", model.ErrStateTransition, model.TaskStateName(state))
	}
	return state, nil
}
