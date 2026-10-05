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

type RenewLeaseLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRenewLeaseLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RenewLeaseLogic {
	return &RenewLeaseLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 心跳续租，栅栏令牌不一致时返回租约已失效。
//
// 这是防重复副作用的最后一道闸：两处过期时间（cron_task_run.lease_expire_at 与
// cron_task_lease.expire_at）必须在同一个事务里一起续，任何一处 CAS 未命中就整体回滚
// 并返回 model.ErrLeaseLost —— 调用方必须立刻停止向下游 RPC 写入。
// 「只续了一处」是最坏的结果：另一处先过期，别的实例就能合法接管，两个实例同时推进同一任务。
func (l *RenewLeaseLogic) RenewLease(in *rpc.RenewLeaseReq) (*rpc.RenewLeaseReply, error) {
	if in == nil || in.RunId <= 0 {
		return nil, model.ErrRunNotFound
	}
	if in.Owner == "" {
		return nil, model.ErrLeaseOwnerRequired
	}
	if in.FenceToken <= 0 {
		// 栅栏令牌只能由服务端签发（AcquireLease 从 1 起），非正值不可能是合法持有者。
		return nil, fmt.Errorf("%w: fence_token=%d 非法，必须回传 AcquireLease 返回的值",
			model.ErrLeaseNotHeld, in.FenceToken)
	}

	run, err := l.svcCtx.Runs.FindOne(l.ctx, in.RunId)
	if err != nil {
		l.Errorf("RenewLease read run failed, run_id=%d", in.RunId)
		return nil, err
	}
	if run == nil {
		return nil, model.ErrRunNotFound
	}
	ttl, err := l.resolveTTL(run, in.TtlSeconds)
	if err != nil {
		return nil, err
	}
	// 任务级租约行只能按 run_id 反查定位（请求里没有 task_key/scope，见 README「契约缺口」）。
	lease, err := l.svcCtx.Leases.FindByRun(l.ctx, in.RunId)
	if err != nil {
		l.Errorf("RenewLease read lease failed, run_id=%d", in.RunId)
		return nil, err
	}
	if lease == nil {
		return nil, fmt.Errorf("%w: run_id=%d 没有对应的 cron_task_lease 行，"+
			"说明它不是通过 AcquireLease 取得的锁", model.ErrLeaseNotHeld, in.RunId)
	}

	var expireAt int64
	err = l.svcCtx.Transact(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		ok, err := l.svcCtx.Runs.RenewTx(ctx, tx, run.ID, in.Owner, in.FenceToken, ttl)
		if err != nil {
			return err
		}
		if !ok {
			return l.leaseLost(run, in)
		}
		ok, err = l.svcCtx.Leases.RenewTx(ctx, tx, lease.LeaseKey, in.Owner, in.FenceToken, ttl)
		if err != nil {
			return err
		}
		if !ok {
			return l.leaseLost(run, in)
		}
		row, err := l.svcCtx.Leases.FindOneTx(ctx, tx, lease.LeaseKey)
		if err != nil {
			return err
		}
		if row == nil || row.ExpireAt <= l.svcCtx.ServerTime() {
			return l.leaseLost(run, in)
		}
		expireAt = row.ExpireAt
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &rpc.RenewLeaseReply{LeaseExpireAt: expireAt, ServerTime: l.svcCtx.ServerTime()}, nil
}

// leaseLost 统一生成「租约已易主」的错误：回滚事务，并留下足够排障的标识（不含参数）。
func (l *RenewLeaseLogic) leaseLost(run *model.TaskRun, in *rpc.RenewLeaseReq) error {
	l.Errorf("cron/renew: lease lost, run_id=%d task_key=%s owner=%s fence=%d db_owner=%s db_fence=%d state=%s",
		run.ID, run.TaskKey, in.Owner, in.FenceToken, run.LeaseOwner, run.FenceToken, model.RunStateName(run.State))
	return fmt.Errorf("%w: run_id=%d 当前持有者 %s(fence=%d)，请求方 %s(fence=%d)",
		model.ErrLeaseLost, run.ID, run.LeaseOwner, run.FenceToken, in.Owner, in.FenceToken)
}

// resolveTTL 决定续租时长：请求显式给的优先，否则回到任务定义的 lease_ttl_seconds。
// 两种取值都过同一套服务端上下界（svcCtx.LeaseTTL），避免「心跳把 TTL 撑成无限期」。
func (l *RenewLeaseLogic) resolveTTL(run *model.TaskRun, requested int32) (int64, error) {
	if requested > 0 {
		return l.svcCtx.LeaseTTL(requested)
	}
	def, err := l.svcCtx.TaskDefinitions.FindOne(l.ctx, run.TaskKey)
	if err != nil {
		l.Errorf("RenewLease read definition failed, task_key=%s", run.TaskKey)
		return 0, err
	}
	if def == nil {
		return 0, fmt.Errorf("%w: run_id=%d 的 task_key=%s 已无定义，无法确定续租时长",
			model.ErrTaskNotFound, run.ID, run.TaskKey)
	}
	return l.svcCtx.LeaseTTL(def.LeaseTTLSeconds)
}
