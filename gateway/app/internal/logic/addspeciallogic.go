// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	socialgraphrpc "go-video/services/social-graph/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AddSpecialLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 设为特别关注（须先关注）
func NewAddSpecialLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AddSpecialLogic {
	return &AddSpecialLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// AddSpecial 设为特别关注：聚合 social-graph AddSpecial RPC。
// 「特别关注须先关注」（ErrSpecialNeedFollow）与 special 计数维护均由 social-graph 判定，
// 网关不预查关注关系，避免检查与写入之间的竞态。
func (l *AddSpecialLogic) AddSpecial(req *types.ParamAddSpecial) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.SocialGraph == nil {
		return nil, errors.New("social-graph service not configured")
	}
	if _, err = l.svcCtx.SocialGraph.AddSpecial(l.ctx, &socialgraphrpc.SpecialReq{
		Mid:        req.Mid,
		SpecialMid: req.SpecialMid,
		RealIp:     req.IP,
	}); err != nil {
		l.Errorf("gateway/app/addSpecial: mid=%d special_mid=%d err=%v", req.Mid, req.SpecialMid, err)
		return nil, err
	}
	return emptyResponse(), nil
}
