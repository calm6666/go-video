package logic

import (
	"context"

	"go-video/services/coin/internal/svc"
	"go-video/services/coin/model"
	"go-video/services/coin/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListMyTossesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListMyTossesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListMyTossesLogic {
	return &ListMyTossesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 我的投币记录。
//
// state=UNSPECIFIED 表示不限状态（含已取消），此时 TossInfo.state 原样回显，
// 客户端才能区分「投过又取消了」和「没投过」；分页 size 超上限直接报错，
// 不静默截断（截断会让调用方以为已经拿全）。
func (l *ListMyTossesLogic) ListMyTosses(in *rpc.ListMyTossesReq) (*rpc.ListMyTossesReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.State > rpc.TossState_TOSS_STATE_CANCELLED {
		return nil, model.ErrInvalidStateFilter
	}
	size, err := l.svcCtx.PageSize(in.Size)
	if err != nil {
		return nil, err
	}
	page := in.Page
	if page <= 0 {
		page = 1
	}
	offset := l.svcCtx.Offset(page, size)

	rows, err := l.svcCtx.Tosses.ListByMid(l.ctx, in.Mid, int32(in.State), offset, size)
	if err != nil {
		return nil, err
	}
	total, err := l.svcCtx.Tosses.CountByMid(l.ctx, in.Mid, int32(in.State))
	if err != nil {
		return nil, err
	}
	return &rpc.ListMyTossesReply{
		Tosses: tossInfos(rows),
		Total:  total,
		Page:   page,
		Size:   int64(size),
	}, nil
}
