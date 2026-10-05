package logic

import (
	"context"

	"go-video/services/payment/internal/svc"
	"go-video/services/payment/model"
	"go-video/services/payment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListRechargesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListRechargesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListRechargesLogic {
	return &ListRechargesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListRecharges 充值台账分页。
//
// 有界性（AGENTS.md §9 不允许「查失败就当没有」）：
//   - mid>0 走 idx_mid_ctime；mid=0 是运营面跨用户查询，必须给完整时间窗且
//     窗口不超过 Payment.MaxListWindowSeconds，否则直接拒绝，不做全表扫；
//   - size 上限 Payment.MaxPageSize，翻页深度上限 Payment.MaxListOffset；
//   - 空结果是合法结果，返回空列表 + total=0；Count/List 任一失败都上抛错误，
//     绝不把故障折叠成空台账冒充「这个人没有充值记录」。
func (l *ListRechargesLogic) ListRecharges(in *rpc.ListRechargesReq) (*rpc.ListRechargesReply, error) {
	cfg := l.svcCtx.Config.Payment
	if in.Mid < 0 {
		return nil, model.ErrInvalidMid
	}
	if err := requireListBounds(cfg, in.Mid, in.FromTs, in.ToTs); err != nil {
		return nil, err
	}
	page, size, err := normalizePage(cfg, in.Page, in.Size)
	if err != nil {
		return nil, err
	}

	q := model.RechargeListQuery{
		Mid:    in.Mid,
		State:  int32(in.State),
		FromTs: in.FromTs,
		ToTs:   in.ToTs,
		Page:   listPage(page, size),
	}
	total, err := l.svcCtx.Models.Recharge.Count(l.ctx, q)
	if err != nil {
		l.Errorf("payment/ListRecharges: count mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	rows, err := l.svcCtx.Models.Recharge.List(l.ctx, q)
	if err != nil {
		l.Errorf("payment/ListRecharges: list mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.ListRechargesReply{
		Recharges: rechargeInfos(rows),
		Total:     total,
		Page:      page,
		Size:      size,
	}, nil
}
