// Package model 是 inbox 服务的数据库访问层，只操作 go_video_inbox 库自身的表
// （AGENTS.md §5：服务只能写自己的 schema）。
//
// 表清单与 deploy/migrations/inbox/*.sql 严格一致：
//
//	inbox_message          消息主体（一条消息一行）
//	inbox_user_message     收件明细（每收件人一行，已读/删除是用户侧状态）
//	inbox_unread_stat      未读数快照（Redis 缺失时的回落层，可由明细表重算）
//	inbox_consumer_offset  事件消费状态机（按 event_id 幂等）
//	inbox_dead_letter      死信留档（只存摘要与脱敏预览）
package model

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 消息分类，与 rpc.Category、inbox_message.category 一致。
const (
	CategoryAll        int32 = 0 // 仅用于查询：不限分类
	CategorySystem     int32 = 1 // 系统通知
	CategoryEngagement int32 = 2 // 互动消息
	CategoryContent    int32 = 3 // 内容消息
	CategoryLive       int32 = 4 // 直播消息
)

// AllCategories 返回全部合法分类，未读计数按此维度快照。
func AllCategories() []int32 {
	return []int32{CategorySystem, CategoryEngagement, CategoryContent, CategoryLive}
}

// ValidCategory 判断分类取值是否在 1..4 范围内。
func ValidCategory(category int32) bool {
	return category >= CategorySystem && category <= CategoryLive
}

// 消息载体类型，与 rpc.MsgType 一致。
const (
	MsgTypeText int32 = 1 // 纯文本
	MsgTypeLink int32 = 2 // 文本 + 跳转
	MsgTypeRich int32 = 3 // 结构化富文本
)

// 收件状态，与 rpc.ReadState 一致。
const (
	ReadStateUnread int32 = 1 // 未读
	ReadStateRead   int32 = 2 // 已读
)

// 用户侧删除状态。
const (
	DelStateNormal  int32 = 0 // 正常
	DelStateDeleted int32 = 1 // 本人已删除
)

// 消息主体状态。
const (
	MessageStateNormal    int32 = 0 // 正常
	MessageStateWithdrawn int32 = 1 // 已撤回（对所有收件人不可见）
)

// 事件消费状态机（docs/api-and-events.md §6）。
const (
	ConsumerStateReceived   = "received"    // 已收到，尚未开始处理
	ConsumerStateProcessing = "processing"  // 处理中（进程崩溃后按 stale 时间回收）
	ConsumerStateSucceeded  = "succeeded"   // 已成功投递，重复消息直接确认
	ConsumerStateRetry      = "retry"       // 失败退避中，next_retry_at 之前不重复执行
	ConsumerStateDeadLetter = "dead_letter" // 已判死，转入 inbox_dead_letter
)

// 死信记录状态。
const (
	DeadLetterStateOpen     = "open"     // 待处理
	DeadLetterStateReplayed = "replayed" // 已重放
	DeadLetterStateIgnored  = "ignored"  // 人工忽略
)

// inbox 域错误：logic 层直接返回，由 gRPC status 承载；
// 不泄漏 SQL 片段或原始 payload（AGENTS.md §6）。
var (
	// ErrInvalidMid mid 非法。
	ErrInvalidMid = errors.New("inbox: invalid mid")
	// ErrEmptyRecipients 接收人列表为空。
	ErrEmptyRecipients = errors.New("inbox: recipients is empty")
	// ErrTooManyRecipients 接收人超过单次上限（Inbox.MaxRecipients 配置）。
	ErrTooManyRecipients = errors.New("inbox: recipients exceeds max limit")
	// ErrEmptyContent 标题与正文都为空。
	ErrEmptyContent = errors.New("inbox: title and content are both empty")
	// ErrInvalidCategory 分类非法。
	ErrInvalidCategory = errors.New("inbox: invalid category")
	// ErrInvalidIdempotencyKey 幂等键为空或超长。
	ErrInvalidIdempotencyKey = errors.New("inbox: idempotency_key required (<=128 chars)")
	// ErrMessageNotFound 消息不存在或不属于该用户。
	ErrMessageNotFound = errors.New("inbox: message not found")
	// ErrInvalidCursor 游标无法解析。
	ErrInvalidCursor = errors.New("inbox: invalid cursor")
	// ErrPsTooLarge 每页大小超过服务端上限。
	ErrPsTooLarge = errors.New("inbox: ps exceeds server limit")
	// ErrEventIDEmpty event_id 为空。
	ErrEventIDEmpty = errors.New("inbox: event_id is empty")
	// ErrConflictProcessing 同一事件正在被其它处理器执行。
	ErrConflictProcessing = errors.New("inbox: event is being processed by another consumer")
)

// nowUnix 统一由 model 层取时间，避免调用方各自读时钟造成状态时间不一致。
func nowUnix() int64 { return time.Now().Unix() }

// execer 抽象 sqlx.SqlConn 与 sqlx.Session，使同一条 SQL 可在事务内外复用；
// session 传 nil 表示走连接自身（非事务）。
type execer interface {
	ExecCtx(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowCtx(ctx context.Context, v any, query string, args ...any) error
	QueryRowsCtx(ctx context.Context, v any, query string, args ...any) error
}

// pick 选择执行载体。
func pick(conn sqlx.SqlConn, session sqlx.Session) execer {
	if session != nil {
		return session
	}
	return conn
}
