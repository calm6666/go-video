// Code scaffolded by goctl. Safe to edit.

package logic

import (
	"context"

	"go-video/services/event-collector/internal/svc"
	"go-video/services/event-collector/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// healthWindowSeconds 健康度统计窗口：proto 字段名就叫 *_last_hour，写死 1 小时
// 而不是做成配置 —— 口径要和字段名一起变，否则名字会骗人。
const healthWindowSeconds = 3600

type GetCollectorHealthLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetCollectorHealthLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetCollectorHealthLogic {
	return &GetCollectorHealthLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 采集与投递健康度。
//
// 隐私（AGENTS.md §7）：本方法只输出「布尔 + 变量名 + 计数」——盐是否可读到（salt_available）、
// 盐版本、salt_ref（环境变量名），盐值本身永不出现；积压维度也只有 topic 与计数。
//
// 失败口径：无 ACTIVE 策略时本方法仍正常返回（健康检查的职责正是把这种缺失暴露出来），
// 但 active_salt_version 写 0 且不伪造策略版本；其余读库失败一律抛错，
// 不回一份「看起来一切正常」的空面板。本方法只读，绝不触发投递推进。
func (l *GetCollectorHealthLogic) GetCollectorHealth(in *rpc.GetCollectorHealthReq) (*rpc.GetCollectorHealthReply, error) {
	now := in.GetNow()
	if now <= 0 {
		now = timeNow()
	}
	since := now - healthWindowSeconds

	stats, err := l.svcCtx.Pending.TopicStats(l.ctx, since)
	if err != nil {
		return nil, err
	}
	deadOpen, err := l.svcCtx.DeadLetters.CountOpenByTopic(l.ctx)
	if err != nil {
		return nil, err
	}
	rejected, err := l.svcCtx.Batches.CountRejectedSince(l.ctx, since)
	if err != nil {
		return nil, err
	}
	// 限流两处来源相加才是完整口径：整批被限流时事件根本没进台账，只能从批次台账的
	// top_reason 取；逐条被限流才落在 ec_event_record.reason。
	rlBatches, err := l.svcCtx.Batches.CountTopReasonSince(l.ctx, rateLimitedReason(), since)
	if err != nil {
		return nil, err
	}
	rlRecords, err := l.svcCtx.Records.CountReasonSince(l.ctx, rateLimitedReason(), since)
	if err != nil {
		return nil, err
	}

	health := mergeTopicStats(stats, deadOpen, canonicalTopics(l.svcCtx))
	topics := make([]*rpc.TopicHealth, 0, len(health))
	for _, h := range health {
		topics = append(topics, &rpc.TopicHealth{
			Topic:              h.topic,
			Pending:            h.pending,
			Retrying:           h.retrying,
			DeadOpen:           h.deadOpen,
			SentLastHour:       h.sentLastHour,
			OldestPendingCtime: h.oldestPending,
		})
	}

	out := &rpc.GetCollectorHealthReply{
		ServerTime:              now,
		Topics:                  topics,
		BatchesRejectedLastHour: rejected,
		RateLimitedLastHour:     rlBatches + rlRecords,
		Version:                 buildVersion(),
	}
	if out.Version == "" {
		// 构建期没注入版本：留空并记日志，不写 "unknown" 伪装成「版本已确认」。
		l.Error("event-collector/logic: 读不到构建版本（-ldflags -X 未注入），health.version 为空")
	}

	p, err := cachedActivePolicy(l.ctx, l.svcCtx)
	switch {
	case err == nil:
		out.PolicyVersion = p.Version
		out.ActiveSaltVersion = p.SaltVersion
		out.SaltRef = p.SaltRef
		// 只报布尔：ensureSaltUsable 走的是采集侧同一个 resolveSalt 口径，
		// 「健康检查说盐可用、第一条上报就 ErrSaltMissing」这类错位由此消除。
		out.SaltAvailable = ensureSaltUsable(l.svcCtx, p) == nil
		if !out.SaltAvailable {
			l.Error("event-collector/logic: ACTIVE 策略声明的 salt_ref 取不到盐值，采集写入会被拒绝")
		}
	case errorsIsNoActivePolicy(err):
		out.ActiveSaltVersion = 0
		out.PolicyVersion = ""
		out.SaltRef = l.svcCtx.Config.Privacy.SaltRef
		_, saltErr := l.svcCtx.Salt()
		out.SaltAvailable = saltErr == nil
		l.Error("event-collector/logic: 无 ACTIVE 采样策略（active_salt_version=0，判定为不健康），" +
			"采集侧按 config 保守默认全量接收，请尽快 Upsert+Activate 一个策略版本")
	default:
		return nil, err
	}
	return out, nil
}
