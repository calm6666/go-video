// Code scaffolded by goctl. Safe to edit.

package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/event-collector/internal/svc"
	"go-video/services/event-collector/model"
	"go-video/services/event-collector/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListDeadLettersLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListDeadLettersLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListDeadLettersLogic {
	return &ListDeadLettersLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询投递死信。
//
// 只读：不改变死信状态，也不触发重新入队（处置入口是 ReplayDeadLetter，它要求
// operator + idempotency_key + reason 三件套，AGENTS.md §8 保留审计证据）。
//
// 输出只含摘要列（event_id/batch_id/topic/payload_digest/reason/attempts/state/operator，
// 见 conv.go deadLetterToRPC）：死信原文不入库，重放需要正文时按 event_id 回对象存储取件。
// reason_detail 是入库前就已脱敏的失败摘要（dispatch.go 的 errorText 口径），这里不补任何原文。
func (l *ListDeadLettersLogic) ListDeadLetters(in *rpc.ListDeadLettersReq) (*rpc.ListDeadLettersReply, error) {
	w, err := newPageWindow(l.svcCtx.Config, in.GetPageSize(), in.GetCursor(), in.GetCtimeFrom(), in.GetCtimeTo())
	if err != nil {
		return nil, err
	}
	topic := strings.TrimSpace(in.GetTopic())
	if topic != "" && !validTopicFilter(topic) {
		return nil, model.ErrInvalidPage
	}
	state := strings.TrimSpace(in.GetState())
	if state != "" && !model.ValidDeadLetterState(state) {
		// 写错字面量必须报错：静默按「无匹配」返回空列表，运营会当成「没有死信」而漏掉积压。
		return nil, fmt.Errorf("%w: state=%q 必须是 %s/%s/%s 之一（空串表示全部）",
			model.ErrInvalidStateTransition, state, model.DeadLetterOpen, model.DeadLetterReplayed,
			model.DeadLetterDiscarded)
	}
	// 死信表按 idx_ctime_id / idx_topic_state / idx_state_ctime 走游标；
	// 与其余读接口同一口径：没有任何条件时拒绝扫描整张表。
	if err := requireWindowOrFilter(topic != "" || state != "", w, "topic/state/ctime 区间"); err != nil {
		return nil, err
	}

	rows, err := l.svcCtx.DeadLetters.List(l.ctx, topic, state, w.from, w.to, w.cursor, w.fetchLimit())
	if err != nil {
		return nil, err
	}
	list, hasMore := trimPage(rows, w.pageSize)
	total, err := l.svcCtx.DeadLetters.Count(l.ctx, topic, state, w.from, w.to)
	if err != nil {
		return nil, err
	}
	var next string
	if hasMore && len(list) > 0 {
		last := list[len(list)-1]
		next = nextCursor(true, last.Ctime, last.ID)
	}
	return &rpc.ListDeadLettersReply{
		List:       deadLetterToRPCList(list),
		NextCursor: next,
		HasMore:    hasMore,
		Total:      total,
	}, nil
}
