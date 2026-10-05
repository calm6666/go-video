package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/creator-revenue/internal/svc"
	"go-video/services/creator-revenue/model"
	"go-video/services/creator-revenue/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type ConfirmSettlementLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewConfirmSettlementLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ConfirmSettlementLogic {
	return &ConfirmSettlementLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 运营确认结算单（金额冻结）
//
// 判定口径：
//   - 空列表拒绝（ErrSettlementNosRequired）：空批量是调用方装配错误，不是「确认成功 0 条」；
//     超过 MaxConfirmBatch 同样拒绝，批量上限存在的意义就是别让一次点击锁住半个账期；
//   - 整批一个事务：CONFIRM 是 CAS(state=DRAFT AND void_seq=0)，逐单不命中不整批失败，
//     不满足条件的单号进 failed_nos 让运营看得见（proto 注释口径：状态不符/已作废）。
//     DB 故障则整批回滚，绝不允许「确认了一半、回复说全成功」；
//   - 幂等：cr_settlement 没有确认审计表，重放判定押在状态机上——已 CONFIRMED 且
//     confirmed_by 就是本次操作人时计入 confirmed（同一人重复确认没有二次后果，
//     超时重试不该被显示成失败）；被**别人**确认过的单进 failed_nos，
//     因为「谁承诺了这笔钱」必须能被区分出来；
//   - 确认 ≠ 打款：本方法只把 state 推到 CONFIRMED，payout_state 恒 NOT_PAYABLE，
//     本项目没有任何出金路径（AGENTS.md §1 范围外能力不得假成功）；
//   - 不重复判「周期是否收官」：出单时已按周期告警，且 cr_settlement 已把
//     period/confirmed_by/confirmed_at 落库，确认当月单的可解释性由数据本身承担。
//
// 已知缺口（已写入 README 与交付报告）：reason 与 request_id 无处落库
// ——本服务没有结算确认审计表，本期也不新增 DDL，二者只进服务日志。
func (l *ConfirmSettlementLogic) ConfirmSettlement(
	in *rpc.ConfirmSettlementReq,
) (*rpc.ConfirmSettlementReply, error) {
	if in == nil {
		in = &rpc.ConfirmSettlementReq{}
	}
	if err := l.svcCtx.Ready(); err != nil {
		return nil, err
	}
	if len(in.SettlementNos) == 0 {
		return nil, model.ErrSettlementNosRequired
	}
	if len(in.SettlementNos) > model.MaxConfirmBatch {
		return nil, fmt.Errorf("%w: 单次最多 %d 个单号，传入 %d",
			model.ErrBatchTooLarge, model.MaxConfirmBatch, len(in.SettlementNos))
	}
	nos, err := normalizeSettlementNos(in.SettlementNos)
	if err != nil {
		return nil, err
	}
	operator, err := requireOperator(in.Operator)
	if err != nil {
		return nil, err
	}
	reason, err := requireReason(in.Reason)
	if err != nil {
		return nil, err
	}
	requestID, err := requireRequestID(in.RequestId, model.MaxRequestIDBytes)
	if err != nil {
		return nil, err
	}

	var (
		confirmed int64
		failed    = make([]string, 0, len(nos))
	)
	err = l.svcCtx.Transact(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		settlements := model.NewSettlementModel(sqlx.NewSqlConnFromSession(tx))
		for _, no := range nos {
			ok, cerr := settlements.Confirm(ctx, no, operator)
			if cerr != nil {
				// 真故障：整批回滚。逐单吞掉错误会让运营以为「这批都冻结了」。
				return fmt.Errorf("确认结算单 %s: %w", no, cerr)
			}
			if ok {
				confirmed++
				continue
			}
			cur, ferr := settlements.FindOneByNo(ctx, no)
			if ferr != nil {
				return ferr
			}
			switch {
			case cur == nil:
				l.Errorf("ConfirmSettlement 单号不存在：%s（request_id=%s operator=%s）", no, requestID, operator)
				failed = append(failed, no)
			case cur.State == model.SettlementStateConfirmed && cur.ConfirmedBy == operator:
				// 同一操作人的重复确认：首次已生效，本次按重放计入成功，零写入。
				confirmed++
			case cur.State == model.SettlementStateVoided:
				l.Errorf("ConfirmSettlement %s 已作废（void_reason=%s），不能确认", no, cur.VoidReason)
				failed = append(failed, no)
			case cur.State == model.SettlementStateConfirmed:
				l.Errorf("ConfirmSettlement %s 已由 %s 确认，本次操作人 %s 不计入成功",
					no, cur.ConfirmedBy, operator)
				failed = append(failed, no)
			default:
				l.Errorf("ConfirmSettlement %s state=%d 不是 DRAFT，不能确认", no, cur.State)
				failed = append(failed, no)
			}
		}
		return nil
	})
	if err != nil {
		l.Errorf("ConfirmSettlement 整批回滚 operator=%s request_id=%s 单号数=%d: %v",
			operator, requestID, len(nos), err)
		return nil, err
	}

	// 确认阶段不再重复判「周期是否收官」：出单时已按周期告警（见 GenerateSettlement），
	// 而 cr_settlement 已把 period / confirmed_by / confirmed_at 落库，复核不依赖这条日志。
	// 确认原因与幂等键当前没有可落库的表（见函数头「已知缺口」），先保证日志里可查。
	l.Infof("ConfirmSettlement 完成 operator=%s request_id=%s reason=%q confirmed=%d failed=%v",
		operator, requestID, reason, confirmed, failed)

	return &rpc.ConfirmSettlementReply{Confirmed: confirmed, FailedNos: failed}, nil
}

// normalizeSettlementNos 去空白、去重并按列宽拒掉超长单号。
//
// 去重是必需的而不是优化：同一单号在一次批量里出现两次，第一次 CAS 成功后
// 第二次必然不命中，会被判成「已确认」分支——不去重就会把一张单计成两张，
// reply.confirmed 也就比真实冻结的单数多一个。
func normalizeSettlementNos(raw []string) ([]string, error) {
	out := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for i, v := range raw {
		no := strings.TrimSpace(v)
		if no == "" {
			return nil, fmt.Errorf("%w: 第 %d 个单号为空", model.ErrSettlementNosRequired, i)
		}
		if err := checkText("settlement_nos", no, model.MaxSettlementNoBytes); err != nil {
			return nil, err
		}
		if seen[no] {
			continue
		}
		seen[no] = true
		out = append(out, no)
	}
	if len(out) == 0 {
		return nil, model.ErrSettlementNosRequired
	}
	return out, nil
}
