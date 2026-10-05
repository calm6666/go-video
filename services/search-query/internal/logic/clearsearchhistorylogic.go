package logic

import (
	"context"

	"go-video/services/search-query/internal/svc"
	"go-video/services/search-query/model"
	"go-video/services/search-query/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ClearSearchHistoryLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewClearSearchHistoryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ClearSearchHistoryLogic {
	return &ClearSearchHistoryLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ClearSearchHistory 清空用户全部历史（需 confirm=true，物理删除）。
//
// 与单条删除同样的隐私语义：物理 DELETE、无软删保留、重复调用幂等
// （第二次调用返回 deleted=0 仍为成功）。不受 Search.HistoryEnabled 开关限制。
func (l *ClearSearchHistoryLogic) ClearSearchHistory(in *rpc.ClearSearchHistoryReq) (*rpc.ClearSearchHistoryReply, error) {
	if in == nil {
		return nil, model.ErrInvalidMid
	}
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if !in.Confirm {
		return nil, model.ErrConfirmRequired
	}

	deleted, err := l.svcCtx.Repository.ClearHistory(l.ctx, in.Mid)
	if err != nil {
		l.Errorw("clear search history failed",
			logx.Field("err", err),
			logx.Field("mid", in.Mid),
			logx.Field("request_id", in.RequestId))
		return nil, err
	}
	// 只记录条数，不记录被清空的关键词内容（擦除事件的日志本身也属敏感信息）。
	l.Infow("search history cleared", logx.Field("mid", in.Mid),
		logx.Field("deleted", deleted),
		logx.Field("request_id", in.RequestId))
	return &rpc.ClearSearchHistoryReply{
		Deleted:     deleted,
		HardDeleted: true,
	}, nil
}
