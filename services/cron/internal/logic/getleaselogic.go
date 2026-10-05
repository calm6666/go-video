package logic

import (
	"context"
	"strings"

	"go-video/services/cron/internal/svc"
	"go-video/services/cron/model"
	"go-video/services/cron/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetLeaseLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetLeaseLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetLeaseLogic {
	return &GetLeaseLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询单个任务级租约。
func (l *GetLeaseLogic) GetLease(in *rpc.GetLeaseReq) (*rpc.GetLeaseReply, error) {
	if in == nil {
		return nil, model.ErrTaskKeyEmpty
	}
	taskKey := strings.TrimSpace(in.TaskKey)
	if taskKey == "" {
		return nil, model.ErrTaskKeyEmpty
	}
	lease, err := l.svcCtx.Leases.FindOne(l.ctx, leaseKeyOf(taskKey, in.Scope))
	if err != nil {
		l.Errorf("GetLease failed, task_key=%s scope=%s", taskKey, in.Scope)
		return nil, err
	}
	if lease == nil {
		// 从未 claim 过：found=false 是正常答案，不是错误。
		return &rpc.GetLeaseReply{Found: false}, nil
	}
	// takeover_count 偏高是实例频繁崩溃的信号，直接透出给运营判断。
	return &rpc.GetLeaseReply{Lease: leaseInfo(lease), Found: true}, nil
}
