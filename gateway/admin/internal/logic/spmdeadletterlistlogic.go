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

type SpmDeadLetterListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 死信台账（只读；重放属 event-collector，本域没有重放口）
func NewSpmDeadLetterListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SpmDeadLetterListLogic {
	return &SpmDeadLetterListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// SpmDeadLetterList 转发 spm ListDeadLetters（本域消费失败的事件台账）。
//
// 只读是刻意的：死信的重放口在 event-collector（那边才有「按投递状态重放」的语义与幂等设计），
// spm 这边开一个重放按钮等于两处同时消费同一批事件。state 是字符串枚举
// （open/replayed/ignored），空串表示不限，网关不校验取值——拼错的 state 由服务拒，
// 网关若自己列一遍清单就会出现「服务加了新状态而网关不认识」的第二份口径。
// payload_preview 已是服务侧的脱敏前缀：网关不加工、不再落日志（§7 行为数据）。
func (l *SpmDeadLetterListLogic) SpmDeadLetterList(req *types.ParamSpmDeadLetterList) (resp *types.SpmDeadLetterListResponse, err error) {
	if l.svcCtx.Spm == nil {
		return nil, errSpmServiceNotConfigured
	}
	if req == nil {
		return nil, errSpmRequestMissing
	}
	if err := spmNonNeg("since", req.Since); err != nil {
		return nil, err
	}
	if err := spmPaging(req.Pn, req.Ps); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Spm.ListDeadLetters(l.ctx, &spmrpc.ListDeadLettersReq{
		Topic: req.Topic,
		State: req.State,
		Since: req.Since,
		Pn:    req.Pn,
		Ps:    req.Ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/spmDeadLetterList: topic=%s state=%s since=%d pn=%d ps=%d err=%v",
			req.Topic, req.State, req.Since, req.Pn, req.Ps, err)
		return nil, err
	}
	return &types.SpmDeadLetterListResponse{
		Code:    0,
		Message: "ok",
		Data: types.SpmDeadLetterListData{
			Items: spmDeadLettersToAPI(reply.GetItems()),
			Total: reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
