package logic

import (
	"context"

	"go-video/services/coin/internal/svc"
	"go-video/services/coin/model"
	"go-video/services/coin/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListTargetTossersLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListTargetTossersLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListTargetTossersLogic {
	return &ListTargetTossersLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 谁投了这条内容（运营/排障）。
//
// 只列 state=ACTIVE 的记录：已取消的投币不再算「投币人」，
// 但它们的行仍在 cn_toss/cn_flow 里，排障时可通过 ListCoinFlows 追溯（AGENTS.md §8 留痕）。
// 返回的是投币记录本身（含 mid、枚数、时间），不含任何用户资料 ——
// 昵称/头像必须由调用方走 user-profile，本服务不复制可变主资料（AGENTS.md §5）。
func (l *ListTargetTossersLogic) ListTargetTossers(in *rpc.ListTargetTossersReq) (*rpc.ListTargetTossersReply, error) {
	if in.TargetAid <= 0 {
		return nil, model.ErrInvalidTargetAid
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

	rows, err := l.svcCtx.Tosses.ListActiveByTarget(l.ctx, in.TargetAid, offset, size)
	if err != nil {
		return nil, err
	}
	total, err := l.svcCtx.Tosses.CountActiveByTarget(l.ctx, in.TargetAid)
	if err != nil {
		return nil, err
	}
	return &rpc.ListTargetTossersReply{
		Tosses: tossInfos(rows),
		Total:  total,
		Page:   page,
		Size:   int64(size),
	}, nil
}
