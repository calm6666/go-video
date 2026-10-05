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

type TransitionVideoSubmissionLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 推进稿件状态机（校验合法转换，禁止直接置为 PUBLISHED）
func NewTransitionVideoSubmissionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *TransitionVideoSubmissionLogic {
	return &TransitionVideoSubmissionLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 推进稿件状态机：只把目标状态与操作人透传给 video TransitionState RPC。
// 依据 AGENTS.md §5、§8，稿件状态机规则与非法转换校验归 video 服务所有，
// 网关不判断 DRAFT/UPLOADED/READY_FOR_REVIEW 等转换是否合法，也不直接置为 PUBLISHED。
func (l *TransitionVideoSubmissionLogic) TransitionVideoSubmission(req *types.ParamVideoTransition) (resp *types.VideoSubmissionResponse, err error) {
	if l.svcCtx.Video == nil {
		return nil, errors.New("video service not configured")
	}
	if err := adminActorGate(l.ctx, "transitionVideoSubmission", "operator", req.Operator); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Video.TransitionState(l.ctx, &videorpc.TransitionReq{
		Aid:      req.Aid,
		Target:   videorpc.SubmissionState(req.Target),
		Operator: req.Operator,
		Reason:   req.Reason,
	})
	if err != nil {
		l.Errorf("gateway/admin/transitionVideoSubmission: aid=%d target=%d operator=%s err=%v",
			req.Aid, req.Target, req.Operator, err)
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
