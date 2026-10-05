package logic

import (
	"context"

	"go-video/services/asset/internal/svc"
	"go-video/services/asset/model"
	"go-video/services/asset/rpc"

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

// TransitionState 推进 asset 状态机（UPLOADED→SCANNED→TRANSCODED）。
// 校验目标状态合法且符合状态机推进方向；非法推进返回 ErrInvalidTransition。
func (l *TransitionStateLogic) TransitionState(in *rpc.TransitionReq) (*rpc.AssetReply, error) {
	if in.AssetId <= 0 {
		return nil, model.ErrInvalidAssetID
	}
	if in.ToState == rpc.AssetState_STATE_UNSPECIFIED {
		return nil, model.ErrInvalidState
	}
	cur, err := l.svcCtx.Repository.GetAsset(l.ctx, in.AssetId)
	if err != nil {
		if err == model.ErrAssetNotFound {
			return nil, err
		}
		l.Errorf("asset/TransitionState load: asset_id=%d err=%v", in.AssetId, err)
		return nil, err
	}
	m, err := l.svcCtx.Repository.TransitionState(l.ctx, in.AssetId, cur.State, int32(in.ToState))
	if err != nil {
		if err == model.ErrInvalidTransition {
			return nil, err
		}
		l.Errorf("asset/TransitionState: asset_id=%d from=%d to=%v err=%v", in.AssetId, cur.State, in.ToState, err)
		return nil, err
	}
	return assetMetaToReply(m), nil
}
