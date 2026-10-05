package logic

import (
	"context"

	"go-video/services/danmaku/internal/policy"
	"go-video/services/danmaku/internal/svc"
	"go-video/services/danmaku/model"
	"go-video/services/danmaku/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ApplyModerationResultLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewApplyModerationResultLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ApplyModerationResultLogic {
	return &ApplyModerationResultLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ApplyModerationResult 回写审核结论并推进弹幕状态机。
//
// 本方法是 moderation.result.v1 的消费者入口：事件消费者（放在本服务
// internal/consumer，接入后补齐）拉取事件后调用这里，跨服务不允许直连审核库。
// 依据 AGENTS.md §8：
//   - 只有合法迁移会被接受，非法迁移返回 model.ErrInvalidStateTransition；
//   - 只有 VERDICT_PASS 才能让弹幕进入普通池对所有人可见；
//   - 带 event_id 的投递在同一事务内写 danmaku_op_log，唯一索引 uniq_event
//     保证重复/并发投递只生效一次（消费幂等）。
func (l *ApplyModerationResultLogic) ApplyModerationResult(in *rpc.ApplyModerationResultReq) (*rpc.ApplyModerationResultReply, error) {
	if in.Dmid <= 0 {
		return nil, model.ErrInvalidDmid
	}
	toState, toPool, ok := policy.TargetForVerdict(int32(in.Verdict))
	if !ok {
		return nil, model.ErrInvalidVerdict
	}

	d, err := l.svcCtx.Repository.GetDanmaku(l.ctx, in.Dmid)
	if err != nil {
		l.Errorf("danmaku/ApplyModerationResult: get dmid=%d err=%v", in.Dmid, err)
		return nil, err
	}
	if d == nil {
		return nil, model.ErrDanmakuNotFound
	}

	// 消费去重快查：唯一索引仍作为并发下的最终防线。
	if in.EventId != "" {
		consumed, err := l.svcCtx.Repository.EventConsumed(l.ctx, in.EventId)
		if err != nil {
			l.Errorf("danmaku/ApplyModerationResult: dedupe check dmid=%d event=%s err=%v", in.Dmid, in.EventId, err)
			return nil, err
		}
		if consumed {
			return &rpc.ApplyModerationResultReply{
				Dmid: d.Dmid, State: d.State, Pool: d.Pool, Applied: false, Message: "duplicate event",
			}, nil
		}
	}

	if d.State == toState {
		return &rpc.ApplyModerationResultReply{
			Dmid: d.Dmid, State: d.State, Pool: d.Pool, Applied: false, Message: "state already applied",
		}, nil
	}
	if !policy.CanTransition(d.State, toState) {
		l.Errorf("danmaku/ApplyModerationResult: illegal transition dmid=%d from=%d to=%d verdict=%d",
			d.Dmid, d.State, toState, int32(in.Verdict))
		return nil, model.ErrInvalidStateTransition
	}

	operatorRole := model.RoleSystem
	if in.Operator > 0 {
		operatorRole = model.RoleAdmin
	}
	log := &model.OpLog{
		Action:       model.ActionModeration,
		OperatorMid:  in.Operator,
		OperatorRole: operatorRole,
		Reason:       in.Reason,
		EventID:      in.EventId,
		TraceID:      in.TraceId,
	}
	applied, err := l.svcCtx.Repository.ApplyStateTransition(l.ctx, d, toState, toPool, in.TaskId, log)
	if err != nil {
		l.Errorf("danmaku/ApplyModerationResult: transition dmid=%d from=%d to=%d err=%v", d.Dmid, d.State, toState, err)
		return nil, err
	}
	if !applied {
		// 并发消费者已抢先推进：重读确认目标状态达成后按幂等成功返回。
		current, gerr := l.svcCtx.Repository.GetDanmaku(l.ctx, in.Dmid)
		if gerr != nil {
			return nil, gerr
		}
		if current == nil {
			return nil, model.ErrDanmakuNotFound
		}
		if current.State != toState {
			l.Errorf("danmaku/ApplyModerationResult: cas miss dmid=%d want=%d now=%d", current.Dmid, toState, current.State)
			return nil, model.ErrConcurrentUpdate
		}
		return &rpc.ApplyModerationResultReply{
			Dmid: current.Dmid, State: current.State, Pool: current.Pool,
			Applied: false, Message: "applied by concurrent consumer",
		}, nil
	}

	return &rpc.ApplyModerationResultReply{
		Dmid:  d.Dmid,
		State: toState,
		Pool:  toPool,
	}, nil
}
