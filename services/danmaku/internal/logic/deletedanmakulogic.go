package logic

import (
	"context"

	"go-video/services/danmaku/internal/policy"
	"go-video/services/danmaku/internal/svc"
	"go-video/services/danmaku/model"
	"go-video/services/danmaku/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type DeleteDanmakuLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDeleteDanmakuLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteDanmakuLogic {
	return &DeleteDanmakuLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// DeleteDanmaku 删除弹幕（本人或管理员）。
//
// 依据 AGENTS.md §8：删除是状态推进而非物理删除，danmaku 行保留为审计证据，
// 同事务写 danmaku_op_log；删除成功后失效段缓存并回退段计数。
// 已是删除态时按幂等成功返回，避免客户端重试报错。
func (l *DeleteDanmakuLogic) DeleteDanmaku(in *rpc.DeleteDanmakuReq) (*rpc.EmptyReply, error) {
	if in.Dmid <= 0 {
		return nil, model.ErrInvalidDmid
	}
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	d, err := l.svcCtx.Repository.GetDanmaku(l.ctx, in.Dmid)
	if err != nil {
		l.Errorf("danmaku/DeleteDanmaku: get dmid=%d err=%v", in.Dmid, err)
		return nil, err
	}
	if d == nil {
		return nil, model.ErrDanmakuNotFound
	}
	if !in.Admin && d.Mid != in.Mid {
		return nil, model.ErrForbidden
	}
	if d.State == model.StateDeleted {
		return &rpc.EmptyReply{}, nil
	}
	if !policy.CanTransition(d.State, model.StateDeleted) {
		l.Errorf("danmaku/DeleteDanmaku: illegal transition dmid=%d from=%d", d.Dmid, d.State)
		return nil, model.ErrInvalidStateTransition
	}

	operatorRole := model.RoleSelf
	if in.Admin {
		operatorRole = model.RoleAdmin
	}
	log := &model.OpLog{
		Action:       model.ActionDelete,
		OperatorMid:  in.Mid,
		OperatorRole: operatorRole,
		Reason:       in.Reason,
		TraceID:      in.TraceId,
	}
	applied, err := l.svcCtx.Repository.ApplyStateTransition(
		l.ctx, d, model.StateDeleted, model.PoolBlock, 0, log)
	if err != nil {
		l.Errorf("danmaku/DeleteDanmaku: transition dmid=%d operator=%d err=%v", d.Dmid, in.Mid, err)
		return nil, err
	}
	if !applied {
		// CAS 未命中说明并发消费者已改动状态，调用方按幂等语义重试即可。
		l.Errorf("danmaku/DeleteDanmaku: cas miss dmid=%d from=%d", d.Dmid, d.State)
		return nil, model.ErrConcurrentUpdate
	}
	return &rpc.EmptyReply{}, nil
}
