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

type TransitionStateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 推进稿件状态机（发布/删除等，校验合法转换）
func NewTransitionStateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *TransitionStateLogic {
	return &TransitionStateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 推进稿件状态机：聚合 video TransitionState RPC。
// 依据 AGENTS.md §8，仅推进合法状态转换，不能直接写入 PUBLISHED；
// 实际合法性由 video 服务自身状态机校验，网关不做业务规则判断。
func (l *TransitionStateLogic) TransitionState(req *types.ParamTransition) (resp *types.VideoSubmissionResponse, err error) {
	if l.svcCtx.Video == nil {
		return nil, errors.New("video service not configured")
	}
	reply, err := l.svcCtx.Video.TransitionState(l.ctx, &videorpc.TransitionReq{
		Aid:      req.Aid,
		Target:   videorpc.SubmissionState(req.Target),
		Operator: req.Operator,
		Reason:   req.Reason,
		Ip:       req.IP,
	})
	if err != nil {
		l.Errorf("gateway/app/transitionState: aid=%d target=%d err=%v", req.Aid, req.Target, err)
		return nil, err
	}
	return &types.VideoSubmissionResponse{
		Code:    0,
		Message: "ok",
		Data:    types.VideoSubmissionData{Submission: toVideoSubmission(reply.GetSubmission())},
		TTL:     0,
	}, nil
}
