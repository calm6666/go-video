package logic

import (
	"context"
	"strconv"

	"go-video/services/video/internal/svc"
	"go-video/services/video/model"
	"go-video/services/video/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type TransitionStateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewTransitionStateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *TransitionStateLogic {
	return &TransitionStateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// TransitionState 推进稿件状态机。
// 依据 AGENTS.md §8，严格校验合法转换，非法转换返回 ErrInvalidStateTransition；
// 禁止直接写入 PUBLISHED（必须经 APPROVED → SCHEDULED → PUBLISHED）。
func (l *TransitionStateLogic) TransitionState(in *rpc.TransitionReq) (*rpc.SubmissionReply, error) {
	if in.Aid <= 0 {
		return nil, model.ErrInvalidAid
	}
	target := stateFromRPC(in.Target)
	if target == 0 {
		return nil, model.ErrInvalidTargetState
	}
	sub, err := l.svcCtx.Repository.GetSubmission(l.ctx, in.Aid)
	if err != nil {
		l.Errorf("video/TransitionState Get: aid=%d err=%v", in.Aid, err)
		return nil, err
	}
	if sub == nil {
		return nil, model.ErrSubmissionNotFound
	}
	if !canTransition(sub.State, target) {
		l.Errorf("video/TransitionState invalid: aid=%d from=%d to=%d operator=%s",
			in.Aid, sub.State, target, in.Operator)
		return nil, model.ErrInvalidStateTransition
	}
	if err := l.svcCtx.Repository.TransitionState(l.ctx, in.Aid, sub.State, target, in.Operator, in.Reason); err != nil {
		l.Errorf("video/TransitionState: aid=%d from=%d to=%d err=%v",
			in.Aid, sub.State, target, err)
		return nil, err
	}
	_ = l.svcCtx.Repository.InvalidateSubmissionCache(l.ctx, in.Aid)
	updated, err := l.svcCtx.Repository.GetSubmission(l.ctx, in.Aid)
	if err != nil {
		return nil, err
	}
	return &rpc.SubmissionReply{Submission: submissionModelToRPC(updated)}, nil
}

// itoa 把 int64 转为字符串（用于审计 operator 拼接）。
func itoa(n int64) string { return strconv.FormatInt(n, 10) }
