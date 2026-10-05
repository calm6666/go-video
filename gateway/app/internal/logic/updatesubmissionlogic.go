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

type UpdateSubmissionLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 编辑稿件元信息（空字段表示不更新，须为所有者）
func NewUpdateSubmissionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateSubmissionLogic {
	return &UpdateSubmissionLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// UpdateSubmission 聚合 video UpdateSubmission RPC：空字符串/0 表示不更新的语义由网关
// 原样下发，是否为所有者、是否处于可编辑状态一律由 video 服务判定（AGENTS.md §5）。
func (l *UpdateSubmissionLogic) UpdateSubmission(req *types.ParamUpdateSubmission) (resp *types.VideoSubmissionResponse, err error) {
	if l.svcCtx.Video == nil {
		return nil, errors.New("video service not configured")
	}
	reply, err := l.svcCtx.Video.UpdateSubmission(l.ctx, &videorpc.UpdateSubmissionReq{
		Aid:    req.Aid,
		Mid:    req.Mid,
		Title:  req.Title,
		Desc:   req.Desc,
		Cover:  req.Cover,
		Typeid: req.Typeid,
		Tag:    req.Tag,
		Ip:     req.IP,
	})
	if err != nil {
		l.Errorf("gateway/app/updateSubmission: aid=%d mid=%d err=%v", req.Aid, req.Mid, err)
		return nil, err
	}
	return &types.VideoSubmissionResponse{
		Code:    0,
		Message: "ok",
		Data:    types.VideoSubmissionData{Submission: toVideoSubmission(reply.GetSubmission())},
		TTL:     0,
	}, nil
}
