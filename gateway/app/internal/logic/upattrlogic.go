// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	creatorrpc "go-video/services/creator/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpAttrLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询 UP 主身份属性
func NewUpAttrLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpAttrLogic {
	return &UpAttrLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// UpAttr 聚合 creator UpAttr RPC：判定 mid 在指定来源下是否具备 UP 主身份。
// from 的取值语义（稿件作者/移动投稿/直播/直播白名单）由 creator 服务解释，网关只透传。
func (l *UpAttrLogic) UpAttr(req *types.ParamUpAttr) (resp *types.UpAttrResponse, err error) {
	if l.svcCtx.Creator == nil {
		return nil, errors.New("creator service not configured")
	}
	reply, err := l.svcCtx.Creator.UpAttr(l.ctx, &creatorrpc.UpAttrReq{
		Mid:  req.Mid,
		From: req.From,
	})
	if err != nil {
		l.Errorf("gateway/app/upAttr: mid=%d from=%d err=%v", req.Mid, req.From, err)
		return nil, err
	}
	return &types.UpAttrResponse{
		Code:    0,
		Message: "ok",
		Data: types.UpAttrData{
			IsAuthor: reply.GetIsAuthor(),
		},
		TTL: 0,
	}, nil
}
