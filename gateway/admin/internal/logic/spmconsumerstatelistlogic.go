// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	spmrpc "go-video/services/spm/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SpmConsumerStateListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 消费链路状态汇总（回答「事件消费到哪了、有没有堆积」）
func NewSpmConsumerStateListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SpmConsumerStateListLogic {
	return &SpmConsumerStateListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// SpmConsumerStateList 转发 spm ListConsumerState（按 topic 汇总消费状态）。
//
// 这是纯排障读：堆积有多严重、RETRY 与 DEAD_LETTER 怎么换算、oldest_ctime 落后多少算异常，
// 全是服务与看盘人的判断，网关不复算任何派生指标（不自己算 lag、不把 count 相加当总量）。
// state=0 是「全部状态」，不代填「只看失败的」——那会让后台的「一切正常」变成筛选的结果。
func (l *SpmConsumerStateListLogic) SpmConsumerStateList(req *types.ParamSpmConsumerStateList) (resp *types.SpmConsumerStateListResponse, err error) {
	if l.svcCtx.Spm == nil {
		return nil, errSpmServiceNotConfigured
	}
	if req == nil {
		return nil, errSpmRequestMissing
	}
	if err := spmNonNeg("state", int64(req.State)); err != nil {
		return nil, err
	}
	if err := spmPaging(req.Pn, req.Ps); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Spm.ListConsumerState(l.ctx, &spmrpc.ListConsumerStateReq{
		Topic: req.Topic,
		State: spmrpc.ConsumerState(req.State),
		Pn:    req.Pn,
		Ps:    req.Ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/spmConsumerStateList: topic=%s state=%d pn=%d ps=%d err=%v",
			req.Topic, req.State, req.Pn, req.Ps, err)
		return nil, err
	}
	return &types.SpmConsumerStateListResponse{
		Code:    0,
		Message: "ok",
		Data: types.SpmConsumerStateListData{
			Rows:  spmConsumerRowsToAPI(reply.GetRows()),
			Total: reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
