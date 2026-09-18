package repository

// 本文件实现 AGENTS.md §5 的 Outbox 模式：
// 业务写操作与事件记录（member_outbox 表）在同一事务内提交，独立发布器
// 轮询投递。当前投递目标：
//   - user.profile_updated → account 服务 DelCache RPC
//     （对应参考仓库 databus 的 MemberService-AccountNotify 主题，
//       消费者为 account 的缓存失效逻辑）；
//   - user.moral_notice    → notification 服务（待接入，配置 MessageURL 后投递，
//     未配置时按参考仓库的最佳努力语义记录日志后标记完成）。
// 消费者按 event_id 幂等；发布失败指数退避重试，超过上限标记失败转人工处理。

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/common/eventenvelope"
	"go-video/services/user-profile/internal/config"
	"go-video/services/user-profile/model"
)

// 资料变更动作常量（移植自参考仓库 model/base.go）。
const (
	ActUpdateByAdmin    = "updateByAdmin"
	ActUpdatePersonInfo = "updatePersonInfo"
	ActUpdateFace       = "updateFace"
	ActUpdateUname      = "updateUname"
)

// producer 事件生产者标识。
const producer = "user-profile"

// noticePayload 节操阈值通知事件负载。
type noticePayload struct {
	Mid        int64  `json:"mid"`
	Title      string `json:"title"`
	Message    string `json:"message"`
	NoticeType string `json:"notice_type"`
}

// enqueueProfileUpdated 在事务内写入资料更新事件（供 Set* 业务在事务中调用）。
func (r *Repository) enqueueProfileUpdatedTx(ctx context.Context, tx sqlx.Session, mid int64, action string) error {
	return r.enqueueEventTx(ctx, tx, model.EventProfileUpdated, mid, map[string]any{
		"mid":    mid,
		"action": action,
	})
}

// enqueueProfileUpdated 非事务场景（官方认证快照变化的后台任务）写入事件。
func (r *Repository) enqueueProfileUpdated(ctx context.Context, mid int64, action string) {
	if err := r.conn.TransactCtx(ctx, func(c context.Context, tx sqlx.Session) error {
		return r.enqueueProfileUpdatedTx(c, tx, mid, action)
	}); err != nil {
		logx.Errorf("user-profile/outbox: enqueue profile_updated mid=%d action=%s err=%v", mid, action, err)
	}
}

// enqueueMoralNotice 在事务内写入节操阈值通知事件。
func (r *Repository) enqueueMoralNoticeTx(ctx context.Context, tx sqlx.Session, p *noticePayload) error {
	return r.enqueueEventTx(ctx, tx, model.EventMoralNotice, p.Mid, p)
}

// enqueueEventTx 构造事件信封并写入 Outbox（须与业务写操作同一事务）。
func (r *Repository) enqueueEventTx(ctx context.Context, tx sqlx.Session, eventType string, mid int64, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("user-profile/outbox: marshal payload: %w", err)
	}
	env, err := eventenvelope.New(producer, eventType, "user", strconv.FormatInt(mid, 10), 1, raw, "")
	if err != nil {
		return fmt.Errorf("user-profile/outbox: build envelope: %w", err)
	}
	envJSON, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("user-profile/outbox: marshal envelope: %w", err)
	}
	return r.outboxModel.Insert(ctx, tx, &model.MemberOutbox{
		EventID:     env.EventID,
		EventType:   eventType,
		AggregateID: strconv.FormatInt(mid, 10),
		Payload:     string(envJSON),
		Status:      model.OutboxStatusPending,
	})
}

// OutboxPublisher 轮询 member_outbox 并投递事件的发布器。
type OutboxPublisher struct {
	model   model.MemberOutboxModel
	cfg     config.OutboxConf
	account AccountCacheClient

	stop chan struct{}
	done chan struct{}
}

// NewOutboxPublisher 构造发布器。account 为 account 缓存失效 RPC 客户端
// （可为 nil，对应事件将退避重试）。
func NewOutboxPublisher(m model.MemberOutboxModel, cfg config.OutboxConf, account AccountCacheClient) *OutboxPublisher {
	if cfg.PollIntervalSeconds <= 0 {
		cfg.PollIntervalSeconds = 2
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 10
	}
	return &OutboxPublisher{
		model:   m,
		cfg:     cfg,
		account: account,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// Start 启动轮询协程。
func (p *OutboxPublisher) Start() {
	go p.loop()
}

// Close 停止轮询并等待当前批次结束（只允许调用一次）。
func (p *OutboxPublisher) Close() {
	close(p.stop)
	<-p.done
}

func (p *OutboxPublisher) loop() {
	defer close(p.done)
	interval := time.Duration(p.cfg.PollIntervalSeconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			p.publishBatch(context.Background())
		}
	}
}

func (p *OutboxPublisher) publishBatch(ctx context.Context) {
	events, err := p.model.ListPending(ctx, time.Now().Unix(), p.cfg.BatchSize)
	if err != nil {
		logx.Errorf("user-profile/outbox: list pending err=%v", err)
		return
	}
	for _, ev := range events {
		if err := p.deliver(ctx, ev); err != nil {
			logx.Errorf("user-profile/outbox: deliver event_id=%s type=%s err=%v", ev.EventID, ev.EventType, err)
			attempts := ev.Attempts + 1
			if int(attempts) >= p.cfg.MaxAttempts {
				_ = p.model.MarkFailed(ctx, ev.ID, err.Error())
				logx.Errorf("user-profile/outbox: event_id=%s marked failed after %d attempts", ev.EventID, attempts)
				continue
			}
			// 指数退避：2^attempts 秒，上限 5 分钟
			backoff := time.Duration(1<<uint(attempts)) * time.Second
			if backoff > 5*time.Minute {
				backoff = 5 * time.Minute
			}
			_ = p.model.MarkRetry(ctx, ev.ID, attempts, time.Now().Add(backoff).Unix(), err.Error())
			continue
		}
		if err := p.model.MarkPublished(ctx, ev.ID, time.Now().Unix()); err != nil {
			logx.Errorf("user-profile/outbox: mark published event_id=%s err=%v", ev.EventID, err)
		}
	}
}

// deliver 按事件类型投递。返回 error 表示投递失败需要重试。
func (p *OutboxPublisher) deliver(ctx context.Context, ev *model.MemberOutbox) error {
	switch ev.EventType {
	case model.EventProfileUpdated:
		return p.deliverProfileUpdated(ctx, ev.Payload)
	case model.EventMoralNotice:
		// 通知为最佳努力投递（对齐参考仓库 SendMessage 的日志级失败语义），
		// 投递失败不阻塞 Outbox，避免通知消息堆积阻塞主事件。
		p.deliverMoralNotice(ctx, ev.Payload)
		return nil
	default:
		logx.Infof("user-profile/outbox: skip unknown event type=%s event_id=%s", ev.EventType, ev.EventID)
		return nil
	}
}

// deliverProfileUpdated 通过 account 的 DelCache RPC 失效 account 侧缓存。
func (p *OutboxPublisher) deliverProfileUpdated(ctx context.Context, payload string) error {
	if p.account == nil {
		return fmt.Errorf("account cache clear rpc not configured")
	}
	var pl struct {
		Mid    int64  `json:"mid"`
		Action string `json:"action"`
	}
	if err := json.Unmarshal([]byte(payload), &pl); err != nil {
		return fmt.Errorf("unmarshal payload: %w", err)
	}
	if err := p.account.DelCache(ctx, pl.Mid, pl.Action); err != nil {
		return fmt.Errorf("account DelCache rpc: %w", err)
	}
	return nil
}

// deliverMoralNotice 投递节操阈值通知（notification 服务待接入）。
func (p *OutboxPublisher) deliverMoralNotice(ctx context.Context, payload string) {
	var pl noticePayload
	if err := json.Unmarshal([]byte(payload), &pl); err != nil {
		logx.Errorf("user-profile/outbox: unmarshal notice payload err=%v", err)
		return
	}
	// notification 服务未接入：记录日志，保留 Outbox 事件作为可审计记录。
	logx.Infof("user-profile/outbox: moral notice mid=%d title=%q (notification service not integrated yet)",
		pl.Mid, pl.Title)
}
