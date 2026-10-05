package logic

import (
	"context"

	"go-video/services/search-query/internal/repository"
	"go-video/services/search-query/internal/svc"
	"go-video/services/search-query/model"
	"go-video/services/search-query/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SuggestLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSuggestLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SuggestLogic {
	return &SuggestLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// Suggest 输入前缀联想（Redis ZSET 词典 + 冷启动回源索引前缀查询）。
//
// 去重在 repository.Suggest 内完成（按规范化小写键），本层只负责参数校验、
// 屏蔽词判定入口与响应映射。
// 失败语义：词典与引擎回源同时不可用时返回 model.ErrSearchUnavailable，
// 不返回“空列表但成功”，避免客户端把故障当成“该前缀无联想”。
func (l *SuggestLogic) Suggest(in *rpc.SuggestReq) (*rpc.SuggestReply, error) {
	if in == nil {
		return nil, model.ErrInvalidKeyword
	}
	repo := l.svcCtx.Repository
	cfg := repo.Conf()

	prefix, err := repository.ValidateKeyword(in.Keyword, cfg.KeywordMaxLen)
	if err != nil {
		return nil, err
	}
	if !isSupportedPlatform(in.Platform) {
		return nil, model.ErrInvalidPlatform
	}
	limit, err := normalizeLimit(in.Limit, cfg.SuggestLimit, cfg.SuggestLimit)
	if err != nil {
		return nil, err
	}
	docTypes, err := docTypesOf(in.SearchType)
	if err != nil {
		return nil, err
	}

	// 屏蔽词直接在前缀层拦下：命中时不查询任何数据源，也不透露词典内容。
	blocked, err := repo.IsBlockedKeyword(l.ctx, prefix)
	if err != nil {
		l.Errorf("search-query/Suggest: block word check err=%v", err)
		return nil, err
	}
	if blocked {
		l.Infow("suggest blocked prefix", logx.Field("keyword_hash", model.KeywordHash(prefix)),
			logx.Field("mid", in.ViewerMid), logx.Field("trace_id", in.TraceId))
		return &rpc.SuggestReply{}, nil
	}

	cands, err := repo.Suggest(l.ctx, repository.SuggestParams{
		Prefix:   prefix,
		Limit:    limit,
		Mid:      in.ViewerMid,
		DocTypes: docTypes,
	})
	if err != nil {
		l.Errorw("suggest failed",
			logx.Field("err", err),
			logx.Field("keyword_hash", model.KeywordHash(prefix)),
			logx.Field("mid", in.ViewerMid),
			logx.Field("trace_id", in.TraceId))
		return nil, err
	}

	items := make([]*rpc.SuggestItem, 0, len(cands))
	for _, c := range cands {
		items = append(items, &rpc.SuggestItem{
			Keyword:     c.Keyword,
			Weight:      c.Weight,
			Source:      c.Source,
			FromHistory: c.Source == repository.SourceHistory,
		})
	}
	return &rpc.SuggestReply{
		Items: items,
		Ttl:   suggestTTL(cfg.CacheTTLSeconds),
	}, nil
}

// suggestTTL 联想结果建议缓存秒数：比结果缓存略长（前缀集合变化慢），
// 但服务端关闭缓存时返回 0，让客户端不做本地缓存。
func suggestTTL(cacheTTLSeconds int) int32 {
	if cacheTTLSeconds <= 0 {
		return 0
	}
	return int32(cacheTTLSeconds)
}
