package logic

import (
	"context"

	"go-video/services/content-fingerprint/internal/svc"
	"go-video/services/content-fingerprint/model"
	"go-video/services/content-fingerprint/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type MatchByFingerprintLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewMatchByFingerprintLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MatchByFingerprintLogic {
	return &MatchByFingerprintLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// MatchByFingerprint 按指纹 key 查询匹配的 asset 列表。
// 本期占位：未接入指纹检索引擎，返回空列表；Match 结果缓存 1 分钟。
// TODO(后续)：接入向量/倒排检索引擎（OpenSearch/自研索引）做 TopN 相似度召回。
func (l *MatchByFingerprintLogic) MatchByFingerprint(in *rpc.MatchReq) (*rpc.MatchReply, error) {
	if in.FpKey == "" {
		return nil, model.ErrInvalidFpKey
	}
	if fpTypeToModel(in.FpType) == 0 {
		return nil, model.ErrInvalidFpType
	}
	topN := in.TopN
	if topN <= 0 {
		topN = 10
	}
	if topN > 50 {
		return nil, model.ErrInvalidTopN
	}
	// 本期占位：Repository 内部 FindByKey 返回空列表
	records, err := l.svcCtx.Repository.MatchByFingerprint(l.ctx, in.FpKey, fpTypeToModel(in.FpType), topN)
	if err != nil {
		l.Errorf("content-fingerprint/MatchByFingerprint: fp_key=%s err=%v", in.FpKey, err)
		return nil, err
	}
	reply := &rpc.MatchReply{Items: make([]*rpc.MatchItem, 0, len(records))}
	for _, r := range records {
		reply.Items = append(reply.Items, recordToItem(r))
	}
	return reply, nil
}
