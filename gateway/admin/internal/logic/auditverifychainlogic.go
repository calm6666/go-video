// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	auditrpc "go-video/services/audit/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AuditVerifyChainLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 哈希链完整性自证（按 chain_key + seq 区间重放，可增量续验）
func NewAuditVerifyChainLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AuditVerifyChainLogic {
	return &AuditVerifyChainLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 校验哈希链：这是只读计算（不改数据），所以不要求 request_id。
// chain_key 必填（<动作域>/<yyyy-MM-dd UTC>）——不给链标识就无法界定重放范围。
// max_entries<=0 由 audit 按 AuditVerify.MaxEntriesPerCall 兜底，网关不另设上限；
// truncated=true 表示本段未验完，客户端要用 last_entry_hash 续验，网关不合并多次结果。
func (l *AuditVerifyChainLogic) AuditVerifyChain(req *types.ParamAuditVerifyChain) (resp *types.AuditChainVerifyResponse, err error) {
	if l.svcCtx.Audit == nil {
		return nil, errAuditServiceNotConfigured
	}
	if err := requireNonEmpty("chain_key", req.ChainKey); err != nil {
		return nil, err
	}
	callCtx, err := auditCallContext(l.ctx, req.Ctx, false)
	if err != nil {
		return nil, err
	}

	reply, err := l.svcCtx.Audit.VerifyAuditChain(l.ctx, &auditrpc.VerifyAuditChainReq{
		Ctx:        callCtx,
		ChainKey:   req.ChainKey,
		FromSeq:    req.FromSeq,
		ToSeq:      req.ToSeq,
		MaxEntries: req.MaxEntries,
	})
	if err != nil {
		l.Errorf("gateway/admin/auditVerifyChain: operator=%d chain_key=%s from=%d to=%d err=%v",
			callCtx.GetOperatorId(), req.ChainKey, req.FromSeq, req.ToSeq, err)
		return nil, err
	}
	return &types.AuditChainVerifyResponse{
		Code:    0,
		Message: "ok",
		Data: types.AuditChainVerifyData{
			Intact:             reply.GetIntact(),
			Checked:            reply.GetChecked(),
			FirstBrokenSeq:     reply.GetFirstBrokenSeq(),
			FirstBrokenEntryId: reply.GetFirstBrokenEntryId(),
			BrokenReason:       reply.GetBrokenReason(),
			LastEntryHash:      reply.GetLastEntryHash(),
			Truncated:          reply.GetTruncated(),
		},
		// intact=false 也是 200：校验失败是被审计事实而不是调用错误，改写它会销毁证据语义。
		TTL: 0,
	}, nil
}
