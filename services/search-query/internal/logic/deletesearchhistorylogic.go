package logic

import (
	"context"

	"go-video/services/search-query/internal/repository"
	"go-video/services/search-query/internal/svc"
	"go-video/services/search-query/model"
	"go-video/services/search-query/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type DeleteSearchHistoryLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDeleteSearchHistoryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteSearchHistoryLogic {
	return &DeleteSearchHistoryLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// DeleteSearchHistory 删除单个历史词（需 confirm=true，物理删除）。
//
// 语义（AGENTS.md 隐私要求）：
//   - confirm=false 直接返回 model.ErrConfirmRequired，不产生任何写入 —— 删除是破坏性
//     操作，网关 UI 必须二次确认后显式传 true；
//   - 物理 DELETE，不留软删行（隐私数据不做“可恢复”保留）；
//   - 幂等：按 (mid, keyword_hash) 精确匹配，重复调用返回 deleted=0 且仍为成功。
//
// 本接口不受 Search.HistoryEnabled 开关限制：擦除能力必须始终可用。
func (l *DeleteSearchHistoryLogic) DeleteSearchHistory(in *rpc.DeleteSearchHistoryReq) (*rpc.DeleteSearchHistoryReply, error) {
	if in == nil {
		return nil, model.ErrInvalidMid
	}
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if !in.Confirm {
		return nil, model.ErrConfirmRequired
	}
	keyword, err := repository.ValidateKeyword(in.Keyword, l.svcCtx.Repository.Conf().KeywordMaxLen)
	if err != nil {
		return nil, err
	}

	deleted, err := l.svcCtx.Repository.DeleteHistory(l.ctx, in.Mid, keyword)
	if err != nil {
		l.Errorw("delete search history failed",
			logx.Field("err", err),
			logx.Field("mid", in.Mid),
			logx.Field("keyword_hash", model.KeywordHash(keyword)),
			logx.Field("request_id", in.RequestId))
		return nil, err
	}
	l.Infow("search history deleted", logx.Field("mid", in.Mid),
		logx.Field("keyword_hash", model.KeywordHash(keyword)),
		logx.Field("deleted", deleted),
		logx.Field("request_id", in.RequestId))
	return &rpc.DeleteSearchHistoryReply{
		Deleted:     deleted,
		HardDeleted: true,
	}, nil
}
