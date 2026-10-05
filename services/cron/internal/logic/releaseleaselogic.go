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

type ReleaseLeaseLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewReleaseLeaseLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ReleaseLeaseLogic {
	return &ReleaseLeaseLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 释放租约并以 SKIPPED/CANCELED 终结执行记录。
//
// 分工必须清晰：这条路径只表达「这次没跑/不跑了」，跑完了的终态一律走 ReportTaskResult，
// 否则两条路径都能写 SUCCEEDED/FAILED，谁后写谁赢，历史轨迹就不可信了。
// 因此 final_state 只允许 SKIPPED/CANCELED，且要求同一事务里 run 与 lease 一起落地：
// 只释放锁不落状态会留下 RUNNING 僵尸，只落状态不释放锁会让别的实例白等到过期。
func (l *ReleaseLeaseLogic) ReleaseLease(in *rpc.ReleaseLeaseReq) (*rpc.EmptyReply, error) {
	if in == nil || in.RunId <= 0 {
		return nil, model.ErrRunNotFound
	}
	if in.Owner == "" {
		return nil, model.ErrLeaseOwnerRequired
	}
	toState, err := runStateValue(in.FinalState)
	if err != nil {
		return nil, err
	}
	if in.Reason == "" {
		// 「这次为什么没跑」必须能被回答，否则 SKIPPED 行等于凭空消失。
		return nil, fmt.Errorf("%w: release 必须给出 reason", model.ErrReasonRequired)
	}

	run, err := l.svcCtx.Runs.FindOne(l.ctx, in.RunId)
	if err != nil {
		l.Errorf("ReleaseLease read run failed, run_id=%d", in.RunId)
		return nil, err
	}
	if run == nil {
		return nil, model.ErrRunNotFound
	}
	if model.IsTerminalRunState(run.State) {
		// 幂等重入：已经是目标终态且锁已交回，什么都不改。
		if run.State == toState && run.LeaseExpireAt == 0 {
			l.Infof("cron/release: run_id=%d 已是 %s 且租约已释放，幂等返回", run.ID, model.RunStateName(toState))
			return &rpc.EmptyReply{}, nil
		}
		return nil, fmt.Errorf("%w: run_id=%d 终结于 %s，release 不能再改写终态",
			model.ErrRunAlreadyFinished, run.ID, model.RunStateName(run.State))
	}
	if run.FenceToken != in.FenceToken {
		return nil, fmt.Errorf("%w: run_id=%d 栅栏不一致（库内 %d，请求 %d），放弃收尾由接管实例继续",
			model.ErrLeaseLost, run.ID, run.FenceToken, in.FenceToken)
	}
	lease, err := l.svcCtx.Leases.FindByRun(l.ctx, in.RunId)
	if err != nil {
		l.Errorf("ReleaseLease read lease failed, run_id=%d", in.RunId)
		return nil, err
	}
	if lease == nil {
		return nil, fmt.Errorf("%w: run_id=%d 没有对应的 cron_task_lease 行，无法释放",
			model.ErrLeaseNotHeld, in.RunId)
	}

	durationMs := durationMs(run, l.svcCtx.ServerTime())
	err = l.svcCtx.Transact(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		ok, err := l.svcCtx.Runs.ReportTx(ctx, tx, run.ID, in.Owner, in.FenceToken, run.State, toState,
			"", in.Reason, 0, durationMs)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: run_id=%d 状态推进未命中（已被并发改写），放弃收尾",
				model.ErrLeaseNotHeld, run.ID)
		}
		ok, err = l.svcCtx.Leases.ReleaseTx(ctx, tx, lease.LeaseKey, in.Owner, in.FenceToken)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: 租约 %s 不属于 %s(fence=%d)",
				model.ErrLeaseNotHeld, lease.LeaseKey, in.Owner, in.FenceToken)
		}
		return nil
	})
	if err != nil {
		l.Errorf("ReleaseLease failed, run_id=%d task_key=%s final_state=%s",
			in.RunId, run.TaskKey, model.RunStateName(toState))
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
