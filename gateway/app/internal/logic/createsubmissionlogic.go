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

type CreateSubmissionLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 创建稿件（DRAFT 状态）
func NewCreateSubmissionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateSubmissionLogic {
	return &CreateSubmissionLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 创建稿件：聚合 video CreateSubmission RPC。
func (l *CreateSubmissionLogic) CreateSubmission(req *types.ParamCreateSubmission) (resp *types.VideoSubmissionResponse, err error) {
	if l.svcCtx.Video == nil {
		return nil, errors.New("video service not configured")
	}
	reply, err := l.svcCtx.Video.CreateSubmission(l.ctx, &videorpc.CreateSubmissionReq{
		Mid:    req.Mid,
		Title:  req.Title,
		Desc:   req.Desc,
		Cover:  req.Cover,
		Typeid: req.Typeid,
		Tag:    req.Tag,
		Ip:     req.IP,
	})
	if err != nil {
		l.Errorf("gateway/app/createSubmission: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.VideoSubmissionResponse{
		Code:    0,
		Message: "ok",
		Data:    types.VideoSubmissionData{Submission: toVideoSubmission(reply.GetSubmission())},
		TTL:     0,
	}, nil
}
