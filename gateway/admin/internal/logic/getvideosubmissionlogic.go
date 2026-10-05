// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	videorpc "go-video/services/video/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetVideoSubmissionLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询稿件详情
func NewGetVideoSubmissionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetVideoSubmissionLogic {
	return &GetVideoSubmissionLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 查询稿件详情：聚合 video GetSubmission RPC。
// 管理端不携带投稿人身份，Mid 固定为 0；video 服务的 GetSubmission 按 aid 读取、
// 不做所有者校验，运营侧鉴权由网关统一鉴权中间件负责（AGENTS.md §6）。
func (l *GetVideoSubmissionLogic) GetVideoSubmission(req *types.ParamVideoAid) (resp *types.VideoSubmissionResponse, err error) {
	if l.svcCtx.Video == nil {
		return nil, errors.New("video service not configured")
	}
	reply, err := l.svcCtx.Video.GetSubmission(l.ctx, &videorpc.SubmissionReq{
		Aid: req.Aid,
		Mid: 0,
	})
	if err != nil {
		l.Errorf("gateway/admin/getVideoSubmission: aid=%d err=%v", req.Aid, err)
		return nil, err
	}
	submission := reply.GetSubmission()
	data := types.VideoSubmissionItem{
		Aid:    submission.GetAid(),
		Mid:    submission.GetMid(),
		Title:  submission.GetTitle(),
		Desc:   submission.GetDesc(),
		Cover:  submission.GetCover(),
		Typeid: submission.GetTypeid(),
		Tag:    submission.GetTag(),
		State:  int32(submission.GetState()),
		Ctime:  submission.GetCtime(),
		Mtime:  submission.GetMtime(),
	}
	return &types.VideoSubmissionResponse{
		Code:    0,
		Message: "ok",
		Data:    types.VideoSubmissionData{Submission: data},
		TTL:     0,
	}, nil
}
