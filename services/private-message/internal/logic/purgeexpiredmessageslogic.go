package logic

import (
	"context"
	"fmt"

	"go-video/services/private-message/internal/svc"
	"go-video/services/private-message/model"
	"go-video/services/private-message/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type PurgeExpiredMessagesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewPurgeExpiredMessagesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PurgeExpiredMessagesLogic {
	return &PurgeExpiredMessagesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 留存到期清理（正文物理删除，审计保留）。
//
// 本方法的红线（写死在实现里，不靠调用方自觉）：
//  1. 截止点只能「不早于配置留存窗口」：before_time 显式传入时，超出 now-RetentionDays 的一律
//     ErrInvalidRetentionWindow —— 清理不可逆，截止点被写歪（例如误传 now+10 年）等于清空全部正文；
//  2. 批量上限：batch_limit 取 PurgeBatchSize，超过 ErrBatchLimitTooLarge，禁止无界扫描；
//  3. 只取主键：ListPurgeCandidates 不把密文读进内存，日志也不带任何内容字段；
//  4. 不删行、不动 pm_withdraw_log / pm_report、不改 state / preview：
//     seq 连续性、撤回与举报留证期都长于正文留存期（合规证据链优先）；
//  5. 幂等：PurgeByIDs 的 WHERE 带 content_purged=0，重跑或中断重来都不会产生额外写入；
//  6. dry_run 只统计（变更前评估影响面），purged 恒为 0。
//
// operator=0 表示 cron 触发（proto 里 PurgeExpiredMessagesReq.operator 写明「触发者（cron 传 0）」）；
// >0 时其管理员身份由 gateway/admin 侧鉴权，本服务只把「谁触发的」记进日志用于问责。
// 这一条主体校验不可省：清理是不可逆的物理删除，「没校验就放行」等于任何拿到 RPC 端口的一方
// 都能清空全站私信正文；负数既不是 cron 也不是管理员，与缺主体同口径拒绝。
func (l *PurgeExpiredMessagesLogic) PurgeExpiredMessages(in *rpc.PurgeExpiredMessagesReq) (*rpc.PurgeExpiredMessagesReply, error) {
	ctx := l.ctx
	s := l.svcCtx
	cfg := s.Config.PrivateMessage

	operator := in.GetOperator()
	if err := checkSubjectOrOperator(operator); err != nil {
		return nil, err
	}

	now := timeNowUnix()
	// 配置自检已保证 MessageRetentionDays > 0；这里据此算出「最早可清理」的界限。
	horizon := now - int64(cfg.MessageRetentionDays)*secondsPerDay
	cutoff := in.GetBeforeTime()
	if cutoff == 0 {
		cutoff = horizon
	}
	if cutoff <= 0 || cutoff > horizon || cutoff > now {
		return nil, fmt.Errorf("%w: before_time=%d 允许上限=%d（留存 %d 天）",
			model.ErrInvalidRetentionWindow, cutoff, horizon, cfg.MessageRetentionDays)
	}

	limit := in.GetBatchLimit()
	if limit == 0 {
		limit = cfg.PurgeBatchSize
	}
	if limit <= 0 {
		return nil, fmt.Errorf("%w: 服务端批量上限非法 %d", model.ErrInvalidPage, cfg.PurgeBatchSize)
	}
	if cfg.PurgeBatchSize > 0 && limit > cfg.PurgeBatchSize {
		return nil, fmt.Errorf("%w: batch_limit=%d max=%d", model.ErrBatchLimitTooLarge, limit, cfg.PurgeBatchSize)
	}

	remaining, err := s.Messages.CountPurgeCandidates(ctx, cutoff)
	if err != nil {
		return nil, err
	}
	if in.GetDryRun() {
		l.Infof("private-message/logic: 清理演练 cutoff=%d operator=%d remaining=%d",
			cutoff, operator, remaining)
		return &rpc.PurgeExpiredMessagesReply{
			ExpiredBefore: cutoff,
			Scanned:       remaining,
			Purged:        0,
			Remaining:     remaining,
		}, nil
	}

	ids, err := s.Messages.ListPurgeCandidates(ctx, cutoff, limit)
	if err != nil {
		return nil, err
	}
	purged, err := s.Messages.PurgeByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	l.Infof("private-message/logic: 留存到期清理完成 cutoff=%d operator=%d scanned=%d purged=%d",
		cutoff, operator, len(ids), purged)
	left := remaining - purged
	if left < 0 {
		// 计数与清理之间有新行到期或被其它实例清掉：估算值下限 0，不返回负数。
		left = 0
	}
	return &rpc.PurgeExpiredMessagesReply{
		ExpiredBefore: cutoff,
		Scanned:       int64(len(ids)),
		Purged:        purged,
		Remaining:     left,
	}, nil
}
