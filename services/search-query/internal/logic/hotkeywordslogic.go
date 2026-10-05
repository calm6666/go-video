package logic

import (
	"context"

	"go-video/services/search-query/internal/svc"
	"go-video/services/search-query/model"
	"go-video/services/search-query/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type HotKeywordsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewHotKeywordsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *HotKeywordsLogic {
	return &HotKeywordsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// HotKeywords 全站/分区热词（读 DB 快照表 + Redis 缓存）。
//
// 数据来源是 search_hot_keyword 快照表，由离线/定时聚合任务从 search_query_log
// 刷新（本服务查询链路只读）。快照缺失时返回空列表且 snapshot_at=0，
// 由客户端展示“暂无热词”，服务端不编造榜单数据。
func (l *HotKeywordsLogic) HotKeywords(in *rpc.HotKeywordsReq) (*rpc.HotKeywordsReply, error) {
	if in == nil {
		in = &rpc.HotKeywordsReq{}
	}
	repo := l.svcCtx.Repository
	cfg := repo.Conf()

	if !isSupportedPlatform(in.Platform) {
		return nil, model.ErrInvalidPlatform
	}
	scope, err := model.NormalizeScope(in.Scope)
	if err != nil {
		return nil, err
	}
	limit, err := normalizeLimit(in.Limit, cfg.HotKeywordLimit, cfg.HotKeywordLimit)
	if err != nil {
		return nil, err
	}

	outcome, err := repo.HotKeywords(l.ctx, scope, limit)
	if err != nil {
		l.Errorw("hot keywords failed",
			logx.Field("err", err),
			logx.Field("scope", scope),
			logx.Field("trace_id", in.TraceId))
		return nil, err
	}

	items := make([]*rpc.HotKeyword, 0, len(outcome.Rows))
	for _, row := range outcome.Rows {
		items = append(items, &rpc.HotKeyword{
			Keyword:    row.Keyword,
			Score:      row.Score,
			SnapshotAt: row.SnapshotAt,
			Scope:      row.Scope,
		})
	}
	if len(items) == 0 {
		// 空快照不是错误，但必须能被调用方识别（snapshot_at=0）并留痕：
		// 若上线后长期为空，说明聚合任务未接入或已停更。
		l.Infow("hot keywords snapshot empty", logx.Field("scope", scope),
			logx.Field("trace_id", in.TraceId))
	}
	return &rpc.HotKeywordsReply{
		Keywords:   items,
		SnapshotAt: outcome.SnapshotAt,
		FromCache:  outcome.FromCache,
		Ttl:        hotKeywordsTTL(outcome.SnapshotAt, cfg.CacheTTLSeconds),
	}, nil
}

// hotKeywordsTTL 热词建议缓存秒数：无快照时缩短，让客户端更快重试到新快照。
func hotKeywordsTTL(snapshotAt int64, cacheTTLSeconds int) int32 {
	if snapshotAt == 0 {
		if cacheTTLSeconds <= 0 {
			return 0
		}
		if cacheTTLSeconds > 5 {
			return 5
		}
		return int32(cacheTTLSeconds)
	}
	if cacheTTLSeconds > 0 {
		return int32(cacheTTLSeconds)
	}
	return 0
}
