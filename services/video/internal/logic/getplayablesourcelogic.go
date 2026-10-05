package logic

import (
	"context"

	"go-video/services/video/internal/svc"
	"go-video/services/video/model"
	"go-video/services/video/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetPlayableSourceLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetPlayableSourceLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetPlayableSourceLogic {
	return &GetPlayableSourceLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// GetPlayableSource 查询稿件当前可播放版次，供网关解析 aid → asset_id → object_key。
// 依据 AGENTS.md §8：只有 PUBLISHED 稿件可对外放流，SCHEDULED 在状态机推进前一律拒绝，
// 本方法只读，不推进任何状态。
func (l *GetPlayableSourceLogic) GetPlayableSource(in *rpc.PlayableSourceReq) (*rpc.PlayableSourceReply, error) {
	if in.GetAid() <= 0 {
		return nil, model.ErrInvalidAid
	}
	sub, err := l.svcCtx.Repository.GetSubmission(l.ctx, in.GetAid())
	if err != nil {
		l.Errorf("video/GetPlayableSource: aid=%d err=%v", in.GetAid(), err)
		return nil, err
	}
	if sub == nil {
		return nil, model.ErrSubmissionNotFound
	}
	if sub.State != model.StatePublished {
		l.Infof("video/GetPlayableSource: aid=%d state=%d requester_mid=%d 拒绝提供播放来源",
			in.GetAid(), sub.State, in.GetMid())
		return nil, model.ErrSubmissionNotPlayable
	}
	ver, err := l.svcCtx.Repository.LatestPlayableVersion(l.ctx, in.GetAid())
	if err != nil {
		l.Errorf("video/GetPlayableSource: aid=%d 版次查询失败 err=%v", in.GetAid(), err)
		return nil, err
	}
	if ver == nil {
		return nil, model.ErrSubmissionNotPlayable
	}
	return &rpc.PlayableSourceReply{
		Aid:             sub.Aid,
		Version:         versionModelToRPC(ver),
		SubmissionState: stateToRPC(sub.State),
	}, nil
}
