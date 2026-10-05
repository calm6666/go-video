package logic

import (
	"context"

	"go-video/services/search-query/internal/svc"
	"go-video/services/search-query/model"
	"go-video/services/search-query/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListSearchHistoryLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListSearchHistoryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListSearchHistoryLogic {
	return &ListSearchHistoryLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListSearchHistory 用户搜索历史（按 mtime 倒序的 keyset cursor 分页）。
//
// mid 必须由网关从访问令牌注入并与调用方一致：历史是用户级隐私数据，
// 本服务不接受“查询他人历史”的能力（无 admin 分支）。
// 非正常状态（state != HistoryStateNormal）的行由 model 层过滤，不下发。
func (l *ListSearchHistoryLogic) ListSearchHistory(in *rpc.ListSearchHistoryReq) (*rpc.ListSearchHistoryReply, error) {
	if in == nil {
		return nil, model.ErrInvalidMid
	}
	repo := l.svcCtx.Repository
	cfg := repo.Conf()

	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if !isSupportedPlatform(in.Platform) {
		return nil, model.ErrInvalidPlatform
	}
	limit, err := normalizeLimit(in.Limit, cfg.HistoryLimit, cfg.HistoryLimit)
	if err != nil {
		return nil, err
	}
	if !cfg.HistoryEnabled {
		// 本部署关闭了历史能力：返回空页而不是报错，客户端无需区分“没有历史”和“不采集”。
		// 注意：删除/清空接口不受此开关影响（隐私擦除必须始终可用）。
		l.Infow("search history disabled by config", logx.Field("mid", in.Mid))
		return &rpc.ListSearchHistoryReply{}, nil
	}

	page, err := repo.ListHistory(l.ctx, in.Mid, in.Cursor, limit)
	if err != nil {
		l.Errorw("list search history failed",
			logx.Field("err", err),
			logx.Field("mid", in.Mid),
			logx.Field("trace_id", in.TraceId))
		return nil, err
	}

	items := make([]*rpc.SearchHistoryItem, 0, len(page.Rows))
	for _, row := range page.Rows {
		items = append(items, &rpc.SearchHistoryItem{
			Keyword:  row.Keyword,
			Ctime:    row.Ctime,
			Mtime:    row.Mtime,
			State:    row.State,
			Platform: row.Platform,
		})
	}
	// 无更多数据时清空游标，避免客户端把“末页游标”误用成下一页。
	next := ""
	if page.HasMore {
		next = page.NextCursor
	}
	return &rpc.ListSearchHistoryReply{
		Items:      items,
		NextCursor: next,
		HasMore:    page.HasMore,
	}, nil
}
