package repository

// 本文件覆盖 Outbox 发布器的投递逻辑：user.profile.updated 通过 account 的
// DelCache RPC 成功投递、RPC 失败时退避重试、超过最大次数标记失败。

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/user-profile/internal/config"
	"go-video/services/user-profile/model"
)

// fakeOutboxModel 内存实现 MemberOutboxModel，记录状态变更。
type fakeOutboxModel struct {
	mu      sync.Mutex
	events  []*model.MemberOutbox
	lastErr string
}

func (f *fakeOutboxModel) Insert(ctx context.Context, tx sqlx.Session, out *model.MemberOutbox) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, out)
	return nil
}

func (f *fakeOutboxModel) ListPending(ctx context.Context, now int64, limit int) ([]*model.MemberOutbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*model.MemberOutbox
	for _, e := range f.events {
		if e.Status == model.OutboxStatusPending && (e.NextRetryAt == 0 || e.NextRetryAt <= now) {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeOutboxModel) MarkPublished(ctx context.Context, id int64, publishedAt int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events[id-1].Status = model.OutboxStatusPublished
	return nil
}

func (f *fakeOutboxModel) MarkRetry(ctx context.Context, id int64, attempts int32, nextRetryAt int64, lastError string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events[id-1].Attempts = attempts
	f.events[id-1].NextRetryAt = nextRetryAt
	f.lastErr = lastError
	return nil
}

func (f *fakeOutboxModel) MarkFailed(ctx context.Context, id int64, lastError string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events[id-1].Status = model.OutboxStatusFailed
	f.lastErr = lastError
	return nil
}

// fakeAccountClient 内存实现 AccountCacheClient，记录调用并可注入错误。
type fakeAccountClient struct {
	mu    sync.Mutex
	calls []delCacheCall
	failN int // 前 N 次调用失败，之后成功
}

type delCacheCall struct {
	Mid    int64
	Action string
}

func (f *fakeAccountClient) DelCache(ctx context.Context, mid int64, action string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, delCacheCall{Mid: mid, Action: action})
	if f.failN > 0 {
		f.failN--
		return errInjected
	}
	return nil
}

var errInjected = &injectedError{}

type injectedError struct{}

func (e *injectedError) Error() string { return "injected error" }

func newEnvelopePayload(mid int64, action string) string {
	raw, _ := json.Marshal(map[string]any{"mid": mid, "action": action})
	return string(raw)
}

func newTestPublisher(fake *fakeOutboxModel, account AccountCacheClient) *OutboxPublisher {
	p := NewOutboxPublisher(fake, config.OutboxConf{
		PollIntervalSeconds: 10,
		BatchSize:           10,
		MaxAttempts:         3,
	}, account)
	p.Start()
	return p
}

// TestOutboxDeliverProfileUpdated 验证资料更新事件通过 DelCache RPC 成功投递。
func TestOutboxDeliverProfileUpdated(t *testing.T) {
	fake := &fakeOutboxModel{events: []*model.MemberOutbox{{
		ID: 1, EventType: model.EventProfileUpdated, Payload: newEnvelopePayload(123, ActUpdateFace),
		Status: model.OutboxStatusPending,
	}}}
	account := &fakeAccountClient{}
	p := newTestPublisher(fake, account)
	defer p.Close()

	p.publishBatch(context.Background())

	account.mu.Lock()
	defer account.mu.Unlock()
	if len(account.calls) != 1 || account.calls[0].Mid != 123 || account.calls[0].Action != ActUpdateFace {
		t.Errorf("account calls = %v, want mid=123 action=updateFace", account.calls)
	}
	if fake.events[0].Status != model.OutboxStatusPublished {
		t.Errorf("status = %d, want published", fake.events[0].Status)
	}
}

// TestOutboxDeliverRetryThenFail 验证 RPC 失败退避重试与最终失败标记。
func TestOutboxDeliverRetryThenFail(t *testing.T) {
	fake := &fakeOutboxModel{events: []*model.MemberOutbox{{
		ID: 1, EventType: model.EventProfileUpdated, Payload: newEnvelopePayload(123, ActUpdateFace),
		Status: model.OutboxStatusPending,
	}}}
	account := &fakeAccountClient{failN: 99} // 始终失败
	p := newTestPublisher(fake, account)
	defer p.Close()

	// 第 1、2 次：失败重试（attempts 1→2，next_retry_at 递增）
	p.publishBatch(context.Background())
	if fake.events[0].Attempts != 1 || fake.events[0].NextRetryAt == 0 {
		t.Fatalf("after 1st: attempts=%d next=%d, want retry", fake.events[0].Attempts, fake.events[0].NextRetryAt)
	}
	firstBackoff := fake.events[0].NextRetryAt
	fake.events[0].NextRetryAt = 0 // 允许立即重试
	p.publishBatch(context.Background())
	if fake.events[0].Attempts != 2 {
		t.Fatalf("after 2nd: attempts=%d, want 2", fake.events[0].Attempts)
	}
	if fake.events[0].NextRetryAt <= firstBackoff {
		t.Errorf("backoff should increase: first=%d second=%d", firstBackoff, fake.events[0].NextRetryAt)
	}

	// 第 3 次：达到 MaxAttempts → 标记失败
	fake.events[0].NextRetryAt = 0
	p.publishBatch(context.Background())
	if fake.events[0].Status != model.OutboxStatusFailed {
		t.Errorf("status = %d, want failed", fake.events[0].Status)
	}
	if fake.lastErr == "" {
		t.Error("lastErr should be recorded")
	}
}

// TestOutboxDeliverNoAccount 验证 account RPC 未配置时事件进入退避重试。
func TestOutboxDeliverNoAccount(t *testing.T) {
	fake := &fakeOutboxModel{events: []*model.MemberOutbox{{
		ID: 1, EventType: model.EventProfileUpdated, Payload: newEnvelopePayload(123, ActUpdateFace),
		Status: model.OutboxStatusPending,
	}}}
	p := newTestPublisher(fake, nil)
	defer p.Close()

	p.publishBatch(context.Background())
	if fake.events[0].Status != model.OutboxStatusPending || fake.events[0].Attempts != 1 {
		t.Errorf("status=%d attempts=%d, want pending with retry", fake.events[0].Status, fake.events[0].Attempts)
	}
}

// TestOutboxDeliverUnknownType 验证未知事件类型跳过（不阻塞）。
func TestOutboxDeliverUnknownType(t *testing.T) {
	fake := &fakeOutboxModel{events: []*model.MemberOutbox{{
		ID: 1, EventType: "unknown.event", Payload: "{}",
		Status: model.OutboxStatusPending,
	}}}
	p := newTestPublisher(fake, nil)
	defer p.Close()

	p.publishBatch(context.Background())
	if fake.events[0].Status != model.OutboxStatusPublished {
		t.Errorf("status = %d, want published (skip unknown)", fake.events[0].Status)
	}
}

// TestOutboxMoralNoticeBestEffort 验证通知事件为最佳努力投递（无消费者也标记完成）。
func TestOutboxMoralNoticeBestEffort(t *testing.T) {
	payload, _ := json.Marshal(noticePayload{Mid: 1, Title: "t", Message: "m", NoticeType: "2_1_3"})
	fake := &fakeOutboxModel{events: []*model.MemberOutbox{{
		ID: 1, EventType: model.EventMoralNotice, Payload: string(payload),
		Status: model.OutboxStatusPending,
	}}}
	p := newTestPublisher(fake, nil)
	defer p.Close()

	p.publishBatch(context.Background())
	if fake.events[0].Status != model.OutboxStatusPublished {
		t.Errorf("status = %d, want published (best effort)", fake.events[0].Status)
	}
}

// TestOutboxStartClose 验证发布器可正常启动与关闭（Repository.Close 单次调用约定）。
func TestOutboxStartClose(t *testing.T) {
	p := NewOutboxPublisher(&fakeOutboxModel{}, config.OutboxConf{PollIntervalSeconds: 1}, nil)
	p.Start()
	p.Close()
}
