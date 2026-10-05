// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	auditrpc "go-video/services/audit/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AuditArchiveLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 归档一条哈希链的指定区间（先落 manifest 再标记热表，不物理删除）
func NewAuditArchiveLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AuditArchiveLogic {
	return &AuditArchiveLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 触发归档：一次只处理一条链的一个区间（proto 约束），request_id 幂等，
// 命中已有批次时 audit 回 reused=true 而不是重做。
// purge_hot 只做热表 archived_at 标记；物理清理由库外 DBA 作业按保留策略执行，
// 本契约不提供 DELETE 方法，网关也就不会假装能「删掉」审计（AGENTS.md §8 审计证据留存）。
func (l *AuditArchiveLogic) AuditArchive(req *types.ParamAuditArchive) (resp *types.AuditArchiveBatchResponse, err error) {
	if l.svcCtx.Audit == nil {
		return nil, errAuditServiceNotConfigured
	}
	if err := requireNonEmpty("chain_key", req.ChainKey); err != nil {
		return nil, err
	}
	if req.FromSeq < 0 || req.ToSeq < 0 {
		return nil, errors.New("gateway/admin: from_seq/to_seq must be >= 0")
	}
	callCtx, err := auditCallContext(l.ctx, req.Ctx, true)
	if err != nil {
		return nil, err
	}

	reply, err := l.svcCtx.Audit.ArchiveAuditEntries(l.ctx, &auditrpc.ArchiveAuditEntriesReq{
		Ctx:      callCtx,
		ChainKey: req.ChainKey,
		FromSeq:  req.FromSeq,
		ToSeq:    req.ToSeq,
		PurgeHot: req.PurgeHot,
	})
	if err != nil {
		l.Errorf("gateway/admin/auditArchive: operator=%d chain_key=%s from=%d to=%d request_id=%s err=%v",
			callCtx.GetOperatorId(), req.ChainKey, req.FromSeq, req.ToSeq, callCtx.GetRequestId(), err)
		return nil, err
	}
	return &types.AuditArchiveBatchResponse{
		Code:    0,
		Message: "ok",
		Data:    types.AuditArchiveBatchData{Batch: auditArchiveBatchToAPI(reply.GetBatch()), Reused: reply.GetReused()},
		TTL:     0,
	}, nil
}
