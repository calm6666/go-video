package logic

import (
	"context"

	"go-video/services/content-fingerprint/internal/svc"
	"go-video/services/content-fingerprint/model"
	"go-video/services/content-fingerprint/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type MatchByAssetLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewMatchByAssetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MatchByAssetLogic {
	return &MatchByAssetLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// MatchByAsset 按 asset_id 查询其指纹的所有匹配。
// 本期占位：直接返回该 asset 的指纹记录列表（即"自匹配"），无相似度计算。
// TODO(后续)：以该 asset 的指纹为锚，调用检索引擎做相似度召回。
func (l *MatchByAssetLogic) MatchByAsset(in *rpc.AssetReq) (*rpc.MatchReply, error) {
	if in.AssetId <= 0 {
		return nil, model.ErrInvalidAssetID
	}
	records, err := l.svcCtx.Repository.MatchByAsset(l.ctx, in.AssetId, fpTypeToModel(in.FpType))
	if err != nil {
		l.Errorf("content-fingerprint/MatchByAsset: asset_id=%d err=%v", in.AssetId, err)
		return nil, err
	}
	reply := &rpc.MatchReply{Items: make([]*rpc.MatchItem, 0, len(records))}
	for _, r := range records {
		reply.Items = append(reply.Items, recordToItem(r))
	}
	return reply, nil
}
