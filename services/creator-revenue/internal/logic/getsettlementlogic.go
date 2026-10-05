package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/creator-revenue/internal/svc"
	"go-video/services/creator-revenue/model"
	"go-video/services/creator-revenue/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetSettlementLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetSettlementLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetSettlementLogic {
	return &GetSettlementLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 结算单详情（含分项）
//
// 判定口径：
//   - settlement_no 必填且不超列宽（超长单号在入口就拒，别留给 DB 报截断）；
//   - mid 非 0 时校验归属：不一致回 ErrForbidden 而不是 found=false。
//     这是内部 RPC，网关已按会话鉴权，把「查到了但不是你的单」伪装成「没有这张单」
//     会让运营以为单号丢失并去重推一次出单，那才是真正的风险；
//   - 单号不存在 → found=false（正常业务结论）；DB 故障一律上抛，
//     绝不回 found=false 冒充「确实没这张单」——那会让调用方去重发结算，
//     而重发在 DRAFT 分支上是会改金额的（AGENTS.md §9 禁止伪成功）；
//   - VOIDED 单同样能查到，且带 void_reason：作废是审计证据，不是删除；
//   - 分项按 source_type 升序返回，空分项投影成非 nil 空数组。
func (l *GetSettlementLogic) GetSettlement(in *rpc.GetSettlementReq) (*rpc.GetSettlementReply, error) {
	if in == nil {
		in = &rpc.GetSettlementReq{}
	}
	if err := l.svcCtx.Ready(); err != nil {
		return nil, err
	}
	no := strings.TrimSpace(in.SettlementNo)
	if no == "" {
		return nil, fmt.Errorf("%w: settlement_no 不能为空", model.ErrSettlementNosRequired)
	}
	if err := checkText("settlement_no", no, model.MaxSettlementNoBytes); err != nil {
		return nil, err
	}
	if in.Mid < 0 {
		return nil, fmt.Errorf("%w: mid=%d（0 表示不校验归属）", model.ErrInvalidMid, in.Mid)
	}

	row, err := l.svcCtx.Settlements.FindOneByNo(l.ctx, no)
	if err != nil {
		l.Errorf("GetSettlement failed settlement_no=%s: %v", no, err)
		return nil, err
	}
	if row == nil {
		return &rpc.GetSettlementReply{Found: false}, nil
	}
	if in.Mid > 0 && row.Mid != in.Mid {
		l.Errorf("GetSettlement 归属校验失败 settlement_no=%s 单据 mid=%d 请求 mid=%d", no, row.Mid, in.Mid)
		return nil, fmt.Errorf("%w: 结算单 %s 属于 mid=%d，与请求 mid=%d 不一致",
			model.ErrForbidden, no, row.Mid, in.Mid)
	}

	items, err := l.svcCtx.SettlementItems.ListByNo(l.ctx, no)
	if err != nil {
		l.Errorf("GetSettlement 分项读取失败 settlement_no=%s: %v", no, err)
		return nil, err
	}
	return &rpc.GetSettlementReply{
		Found:      true,
		Settlement: settlementInfo(row),
		Items:      settlementItemInfos(items),
	}, nil
}
