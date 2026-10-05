// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	privatemessagerpc "go-video/services/private-message/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type PrivateMessageRetentionPurgeLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 留存到期清理（先 dry_run 看影响面，purge=true 才真删正文，审计行保留）
func NewPrivateMessageRetentionPurgeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PrivateMessageRetentionPurgeLogic {
	return &PrivateMessageRetentionPurgeLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// PrivateMessageRetentionPurge 聚合 private-message PurgeExpiredMessages。
//
// 这是本组唯一**不可逆**的入口：物理删除私信正文。因此权限点单独一个 pm:retention:purge，
// 不与 pm:report:handle 合并——能处置举报的人不该默认能删证据。
//
// 网关的三条收紧：
//  1. operator 必填 >0。契约里 cron 触发时传 0，但那条调用走服务侧定时任务、不经 HTTP；
//     从后台点出来的清理必须能追到人（AGENTS.md §8「处置留痕」）。
//  2. dry_run 由表单显式决定，网关不默认 false：先看清 scanned/remaining 再删是这条路由的
//     正确使用方式，把默认值设成「直接删」等于把误操作成本压给一次点击。
//  3. before_time=0（由服务按留存窗口推算）与 batch_limit=0（服务端默认值）都是合法哨兵，
//     负数一律拒；上限与「留存窗口能不能比配置更短」由 private-message 判定。
//
// 返回的是本次扫描/清理的**计数**，不是「已释放多少空间」：审计行与被撤回消息的行都保留，
// 网关不把这些数换算成存储口径（那是 asset/对象存储的事实）。
func (l *PrivateMessageRetentionPurgeLogic) PrivateMessageRetentionPurge(req *types.ParamPrivateMessagePurge) (resp *types.PrivateMessagePurgeResponse, err error) {
	if l.svcCtx.PrivateMessage == nil {
		return nil, errPMServiceNotConfigured
	}
	if req == nil {
		return nil, errPMRequestMissing
	}
	if err := pmSessionGate(l.ctx, "privateMessageRetentionPurge", req.Operator); err != nil {
		return nil, err
	}
	if err := requireOperator("operator", req.Operator); err != nil {
		return nil, err
	}
	if err := pmNonNeg("before_time", req.BeforeTime); err != nil {
		return nil, err
	}
	if err := pmNonNeg("batch_limit", int64(req.BatchLimit)); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.PrivateMessage.PurgeExpiredMessages(l.ctx, &privatemessagerpc.PurgeExpiredMessagesReq{
		BeforeTime: req.BeforeTime,
		BatchLimit: req.BatchLimit,
		DryRun:     req.DryRun,
		Operator:   req.Operator,
		TraceId:    req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/privateMessageRetentionPurge: before_time=%d batch_limit=%d dry_run=%t operator=%d err=%v",
			req.BeforeTime, req.BatchLimit, req.DryRun, req.Operator, err)
		return nil, err
	}
	// 清理成功本身要落一条后台日志：这是「谁在什么时候删了到期正文」的唯一网关侧证据
	// （服务侧留的是自己的扫描事实，两边编号空间不同，不能互相替代）。
	l.Infof("gateway/admin/privateMessageRetentionPurge: expired_before=%d scanned=%d purged=%d remaining=%d dry_run=%t",
		reply.GetExpiredBefore(), reply.GetScanned(), reply.GetPurged(), reply.GetRemaining(), req.DryRun)
	return &types.PrivateMessagePurgeResponse{
		Code:    0,
		Message: "ok",
		Data: types.PrivateMessagePurgeData{
			ExpiredBefore: reply.GetExpiredBefore(),
			Scanned:       reply.GetScanned(),
			Purged:        reply.GetPurged(),
			Remaining:     reply.GetRemaining(),
		},
		TTL: 0,
	}, nil
}
