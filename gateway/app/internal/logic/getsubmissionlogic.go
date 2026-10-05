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

type GetSubmissionLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询稿件详情
func NewGetSubmissionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetSubmissionLogic {
	return &GetSubmissionLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 查询稿件详情：聚合 video GetSubmission RPC。
func (l *GetSubmissionLogic) GetSubmission(req *types.ParamVideoAid) (resp *types.VideoSubmissionResponse, err error) {
	if l.svcCtx.Video == nil {
		return nil, errors.New("video service not configured")
	}
	reply, err := l.svcCtx.Video.GetSubmission(l.ctx, &videorpc.SubmissionReq{
		Aid: req.Aid,
		Mid: req.Mid,
		Ip:  req.IP,
	})
	if err != nil {
		l.Errorf("gateway/app/getSubmission: aid=%d err=%v", req.Aid, err)
		return nil, err
	}
	return &types.VideoSubmissionResponse{
		Code:    0,
		Message: "ok",
		Data:    types.VideoSubmissionData{Submission: toVideoSubmission(reply.GetSubmission())},
		TTL:     0,
	}, nil
}
