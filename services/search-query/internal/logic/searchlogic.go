package logic

import (
	"context"

	"go-video/services/search-query/internal/repository"
	"go-video/services/search-query/internal/svc"
	"go-video/services/search-query/model"
	"go-video/services/search-query/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SearchLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSearchLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SearchLogic {
	return &SearchLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// Search 关键词搜索（cursor 优先分页；引擎不可用时返回明确错误码）。
//
// 流程：参数校验 -> 屏蔽词判定 -> 深分页保护 -> 结果缓存 -> OpenSearch 查询 ->
// 出口高亮映射 -> 登录用户写入搜索历史（尽力而为）。
//
// 失败语义：引擎未配置一律返回 model.ErrSearchUnavailable；引擎已配置但熔断打开时，
// 按 repository.Search 的降级顺序处理（未过期缓存 + DegradeEnabled 才兜底，并置 Degraded，
// 本函数据此把响应 ttl 归 0，见 replyTTL）。任何情况下都不返回空结果冒充成功；
// 命中屏蔽词是唯一的“成功但零结果”，并带 safe_filtered=true。
func (l *SearchLogic) Search(in *rpc.SearchReq) (*rpc.SearchReply, error) {
	if in == nil {
		return nil, model.ErrInvalidKeyword
	}
	repo := l.svcCtx.Repository
	cfg := repo.Conf()

	keyword, err := repository.ValidateKeyword(in.Keyword, cfg.KeywordMaxLen)
	if err != nil {
		return nil, err
	}
	if !isSupportedPlatform(in.Platform) {
		return nil, model.ErrInvalidPlatform
	}
	docTypes, err := docTypesOf(in.SearchType)
	if err != nil {
		return nil, err
	}
	durMin, durMax, err := durationRangeOf(in.Duration)
	if err != nil {
		return nil, err
	}
	if in.PublishedAfter < 0 || in.PublishedBefore < 0 {
		return nil, model.ErrInvalidPage
	}
	if in.PublishedAfter > 0 && in.PublishedBefore > 0 && in.PublishedAfter > in.PublishedBefore {
		return nil, model.ErrInvalidPage
	}
	sortMode := resolveSort(in.Sort, cfg.DefaultSort)
	sortFields, err := sortFieldsOf(in.SearchType, sortMode)
	if err != nil {
		return nil, err
	}
	ps := normalizePageSize(in.Ps, cfg)

	params := repository.SearchParams{
		Keyword:         keyword,
		DocTypes:        docTypes,
		ZoneID:          in.ZoneId,
		DurationMin:     durMin,
		DurationMax:     durMax,
		PublishedAfter:  in.PublishedAfter,
		PublishedBefore: in.PublishedBefore,
		SortFields:      sortFields,
		Size:            ps,
	}
	params.Fingerprint = repository.QueryFingerprint(params)

	// cursor 优先；pn/ps 仅为兼容旧端保留（两者同时出现时以 cursor 为准）。
	offset, err := repository.DecodeOffsetCursor(in.Cursor, params.Fingerprint)
	if err != nil {
		return nil, err
	}
	if in.Cursor == "" {
		offset, err = repository.PageToOffset(in.Pn, ps)
		if err != nil {
			return nil, err
		}
	}
	params.From = offset

	// 安全过滤：命中屏蔽词时不查询引擎，也不暴露命中数量。
	blocked, err := repo.IsBlockedKeyword(l.ctx, keyword)
	if err != nil {
		l.Errorf("search-query/Search: block word check keyword=%q err=%v", keyword, err)
		return nil, err
	}
	if blocked {
		l.Infow("search blocked keyword", logx.Field("keyword_hash", model.KeywordHash(keyword)),
			logx.Field("mid", in.ViewerMid), logx.Field("request_id", in.RequestId))
		return &rpc.SearchReply{
			Pn:           pageNo(offset, ps),
			Ps:           ps,
			SafeFiltered: true,
			Ttl:          0,
		}, nil
	}

	outcome, err := repo.Search(l.ctx, params)
	if err != nil {
		// 引擎侧错误必须原样上抛（附 trace_id 便于与索引/引擎日志对齐）。
		l.Errorw("search engine query failed",
			logx.Field("err", err),
			logx.Field("keyword_hash", model.KeywordHash(keyword)),
			logx.Field("alias", repo.EngineAlias()),
			logx.Field("offset", offset),
			logx.Field("request_id", in.RequestId),
			logx.Field("trace_id", in.TraceId))
		return nil, err
	}

	// 登录用户的搜索历史：失败不影响已成功的搜索，但必须留痕。
	if cfg.HistoryEnabled && in.ViewerMid > 0 {
		if herr := repo.RecordHistory(l.ctx, in.ViewerMid, keyword, in.Platform); herr != nil {
			l.Errorf("search-query/Search: record history mid=%d err=%v", in.ViewerMid, herr)
		}
	}

	reply := &rpc.SearchReply{
		Hits:     toSearchHits(outcome.Hits),
		Total:    outcome.Total,
		Pn:       pageNo(offset, ps),
		Ps:       ps,
		CacheHit: outcome.CacheHit,
		Ttl:      replyTTL(outcome, cfg.CacheTTLSeconds),
	}
	// 下一页游标：只有仍在允许的 offset 窗口内才下发，避免客户端翻到拒绝区。
	next := offset + int64(ps)
	hasMore := int64(len(outcome.Hits)) == int64(ps) && (outcome.Total == 0 || next < outcome.Total)
	if hasMore && (cfg.MaxOffset <= 0 || next <= int64(cfg.MaxOffset)) {
		cursor, cerr := repository.EncodeOffsetCursor(next, params.Fingerprint)
		if cerr != nil {
			l.Errorf("search-query/Search: encode cursor err=%v", cerr)
		} else {
			reply.NextCursor = cursor
		}
	} else {
		hasMore = false
	}
	reply.HasMore = hasMore
	return reply, nil
}

// pageNo 由 offset 反解页码（pn 从 1 开始）。
func pageNo(offset int64, ps int32) int32 {
	if ps <= 0 {
		return 1
	}
	return int32(offset/int64(ps)) + 1
}

// replyTTL 建议客户端缓存秒数：命中缓存时用剩余 TTL，否则用服务端缓存窗口。
// 降级兜底结果不给客户端缓存（避免故障期间放大陈旧副本）。
func replyTTL(outcome *repository.SearchOutcome, configured int) int32 {
	if outcome == nil || outcome.Degraded {
		return 0
	}
	if outcome.CacheHit {
		if outcome.CacheTTL > 0 {
			return int32(outcome.CacheTTL)
		}
		return 0
	}
	if configured > 0 {
		return int32(configured)
	}
	return 0
}
