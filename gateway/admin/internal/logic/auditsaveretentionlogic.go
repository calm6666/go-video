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

type AuditSaveRetentionLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 新建/更新保留期策略（expect_version 乐观锁，0 表示新建）
func NewAuditSaveRetentionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AuditSaveRetentionLogic {
	return &AuditSaveRetentionLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 保存保留期策略：action_domain 命名规则（小写字母/数字/下划线、最长 32）、
// 「default 必须存在」、三个天数之间的先后关系与最小保留期全部由 audit 判定（AGENTS.md §5）。
// expect_version 原样透传：网关不猜版本号，冲突时由 audit 拒绝，运营重新拉取后再提交。
func (l *AuditSaveRetentionLogic) AuditSaveRetention(req *types.ParamAuditSaveRetention) (resp *types.AuditRetentionPolicyResponse, err error) {
	if l.svcCtx.Audit == nil {
		return nil, errAuditServiceNotConfigured
	}
	if err := requireNonEmpty("action_domain", req.ActionDomain); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("remark", req.Remark); err != nil {
		return nil, err
	}
	callCtx, err := auditCallContext(l.ctx, req.Ctx, true)
	if err != nil {
		return nil, err
	}
	if req.ExpectVersion < 0 {
		return nil, errAuditExpectVersionInvalid
	}

	reply, err := l.svcCtx.Audit.SaveRetentionPolicy(l.ctx, &auditrpc.SaveRetentionPolicyReq{
		Ctx:              callCtx,
		ActionDomain:     req.ActionDomain,
		HotDays:          req.HotDays,
		ArchiveAfterDays: req.ArchiveAfterDays,
		DeleteAfterDays:  req.DeleteAfterDays,
		State:            req.State,
		ExpectVersion:    req.ExpectVersion,
		Remark:           req.Remark,
	})
	if err != nil {
		l.Errorf("gateway/admin/auditSaveRetention: operator=%d domain=%s expect_version=%d err=%v",
			callCtx.GetOperatorId(), req.ActionDomain, req.ExpectVersion, err)
		return nil, err
	}
	return &types.AuditRetentionPolicyResponse{
		Code:    0,
		Message: "ok",
		Data:    types.AuditRetentionPolicyData{Policy: auditRetentionPolicyToAPI(reply.GetPolicy())},
		TTL:     0,
	}, nil
}
