// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	videorpc "go-video/services/video/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type DeleteSubmissionLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 删除稿件（软删，状态机推进到 DELETED）
func NewDeleteSubmissionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteSubmissionLogic {
	return &DeleteSubmissionLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// DeleteSubmission 聚合 video DeleteSubmission RPC：稿件软删只推进状态机到 DELETED，
// 网关不删媒资也不改其他域数据（AGENTS.md §8）。
func (l *DeleteSubmissionLogic) DeleteSubmission(req *types.ParamDeleteSubmission) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.Video == nil {
		return nil, errors.New("video service not configured")
	}
	if _, err = l.svcCtx.Video.DeleteSubmission(l.ctx, &videorpc.SubmissionReq{
		Aid: req.Aid,
		Mid: req.Mid,
		Ip:  req.IP,
	}); err != nil {
		l.Errorf("gateway/app/deleteSubmission: aid=%d mid=%d err=%v", req.Aid, req.Mid, err)
		return nil, err
	}
	return emptyResponse(), nil
}
