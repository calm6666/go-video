// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	searchqueryrpc "go-video/services/search-query/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type HotKeywordsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 全站/分区热词快照
func NewHotKeywordsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *HotKeywordsLogic {
	return &HotKeywordsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *HotKeywordsLogic) HotKeywords(req *types.ParamHotKeywords) (resp *types.HotKeywordsResponse, err error) {
	if l.svcCtx.SearchQuery == nil {
		return nil, errors.New("search-query service not configured")
	}
	reply, err := l.svcCtx.SearchQuery.HotKeywords(l.ctx, &searchqueryrpc.HotKeywordsReq{
		Scope:     req.Scope,
		Limit:     req.Limit,
		ViewerMid: req.ViewerMid,
		Platform:  req.Platform,
	})
	if err != nil {
		l.Errorf("gateway/app/hotKeywords: scope=%s err=%v", req.Scope, err)
		return nil, err
	}
	return &types.HotKeywordsResponse{
		Code:    0,
		Message: "ok",
		Data: types.HotKeywordsData{
			Keywords:   hotKeywordsToAPI(reply.GetKeywords()),
			SnapshotAt: reply.GetSnapshotAt(),
			FromCache:  reply.GetFromCache(),
		},
		TTL: int64(reply.GetTtl()),
	}, nil
}
