package repository

// 本文件是 danmaku 服务对 moderation-orchestrator 的 gRPC 客户端适配器。
// 依据 AGENTS.md §5，本服务不直连审核库、不引用审核服务的内部 model，
// 只通过公开 RPC 送审；审核结论由 ApplyModerationResult RPC 回传
// （moderation.result.v1 的消费者入口）。
//
// 未配置 ModerationRPC 时 svc 不构造客户端，PostDanmaku 在机审开关打开时
// 返回 model.ErrModerationNotConfigured，绝不伪造“已送审”。

import (
	"context"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/zrpc"

	moderrpc "go-video/services/moderation-orchestrator/rpc"
)

// ModerationClient 是审核编排服务在本服务视角下的最小契约。
type ModerationClient interface {
	// SubmitForReview 提交机审任务并返回 task_id。
	// dmid 作为 submission_id，business 固定为 danmaku。
	SubmitForReview(ctx context.Context, dmid, mid int64, reason string) (int64, error)
}

// moderationClient 通过 zrpc 调用 moderation-orchestrator。
type moderationClient struct {
	cli moderrpc.ModerationOrchestratorClient
}

// NewModerationClient 构造审核 RPC 客户端适配器。
// 配置非法或连接初始化失败时返回错误，由调用方决定降级策略。
func NewModerationClient(c zrpc.RpcClientConf) (ModerationClient, error) {
	conn, err := zrpc.NewClient(c)
	if err != nil {
		return nil, fmt.Errorf("danmaku: init moderation rpc client: %w", err)
	}
	return &moderationClient{cli: moderrpc.NewModerationOrchestratorClient(conn.Conn())}, nil
}

// SubmitForReview 实现 ModerationClient。
func (m *moderationClient) SubmitForReview(ctx context.Context, dmid, mid int64, reason string) (int64, error) {
	reply, err := m.cli.SubmitForReview(ctx, &moderrpc.SubmitReq{
		SubmissionId: dmid,
		ContentType:  moderrpc.ContentType_CONTENT_TYPE_DANMAKU,
		Mid:          mid,
		Business:     "danmaku",
		Reason:       reason,
	})
	if err != nil {
		return 0, fmt.Errorf("danmaku: submit moderation dmid=%d: %w", dmid, err)
	}
	if reply == nil || reply.GetTask() == nil || reply.GetTask().GetTaskId() <= 0 {
		return 0, errors.New("danmaku: moderation returned empty task")
	}
	return reply.GetTask().GetTaskId(), nil
}
