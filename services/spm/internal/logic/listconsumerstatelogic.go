// Code scaffolded by goctl. Safe to edit.

package logic

import (
	"context"
	"fmt"
	"time"

	"go-video/services/spm/internal/svc"
	"go-video/services/spm/model"
	"go-video/services/spm/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListConsumerStateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListConsumerStateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListConsumerStateLogic {
	return &ListConsumerStateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 消费状态汇总（位点与堆积）
func (l *ListConsumerStateLogic) ListConsumerState(in *rpc.ListConsumerStateReq) (*rpc.ListConsumerStateReply, error) {
	// 逻辑轮规划：对 spm_consumer_offset 按 (topic,state) 做有界聚合（COUNT + MAX(msg_offset) + MIN(ctime)），
	// 命中 idx_topic_state 取分组前缀、idx_state_ctime 取「最早一条的接收时间」；
	// msg_offset/ctime 不在这两个索引里，要回表，所以聚合必须带时间下界与 LIMIT（分组数受 ps 上限约束），不做全表扫描。
	done, err := acquireReadToken(l.ctx, l.svcCtx, l.Logger, "ListConsumerState")
	if err != nil {
		return nil, err
	}
	defer done()

	size, err := pageSize(l.svcCtx, in.GetPs())
	if err != nil {
		return nil, err
	}
	topic, err := checkTopic(in.GetTopic())
	if err != nil {
		return nil, err
	}
	states, err := consumerStateFilter(in.GetState())
	if err != nil {
		return nil, err
	}
	// 有界聚合的时间下界取行为事实的留存期：早于此的成功行本该已被 DeleteSettledBefore 清走，
	// 所以这条下界不会漏掉任何「还在库里」的堆积，却保证 GROUP BY 不用扫整张状态表。
	// 契约里 ListConsumerStateReq 没有 since 字段（README「契约缺口」），下界只能由服务端给。
	since := time.Now().Unix() - int64(l.svcCtx.Config.Spm.BehaviorRetentionDays)*86400
	offset := pageOffset(in.GetPn(), size)

	total, err := l.svcCtx.Offsets.CountByState(l.ctx, topic, states, since)
	if err != nil {
		return nil, err
	}
	reply := &rpc.ListConsumerStateReply{Total: total}
	if !pageFits(offset, total, size) {
		return reply, nil
	}
	rows, err := l.svcCtx.Offsets.Summarize(l.ctx, topic, states, since, offsetTo32(offset), size)
	if err != nil {
		return nil, err
	}
	items := make([]*rpc.ListConsumerStateReply_Row, 0, len(rows))
	for _, r := range rows {
		items = append(items, &rpc.ListConsumerStateReply_Row{
			Topic:         r.Topic,
			State:         consumerStateToRPC(r.State),
			Count:         r.RowCount,
			OldestCtime:   r.OldestCtime,
			LastMsgOffset: r.LastMsgOffset,
			LastEventTime: r.LastEventTime,
		})
	}
	reply.Rows = items
	return reply, nil
}

// consumerStateFilter 把契约的状态枚举换成 model 用的状态字符串集合。
// UNSPECIFIED = 不限（返回 nil，SQL 不加 state 条件）；未知枚举值拒绝而不是当成「不限」——
// 把「过滤条件写错了」表现成「全部状态都命中」是可观测面最坏的一种假成功。
func consumerStateFilter(state rpc.ConsumerState) ([]string, error) {
	s := int32(state)
	if s == int32(rpc.ConsumerState_CONSUMER_STATE_UNSPECIFIED) {
		return nil, nil
	}
	name := consumerStateName(s)
	if name == "" {
		return nil, fmt.Errorf("%w: state=%d", model.ErrInvalidConsumerState, s)
	}
	return []string{name}, nil
}

// consumerStateName 契约枚举 -> 落库字符串；越界返回空串。
func consumerStateName(state int32) string {
	switch state {
	case int32(rpc.ConsumerState_CONSUMER_STATE_RECEIVED):
		return model.ConsumerStateReceived
	case int32(rpc.ConsumerState_CONSUMER_STATE_PROCESSING):
		return model.ConsumerStateProcessing
	case int32(rpc.ConsumerState_CONSUMER_STATE_SUCCEEDED):
		return model.ConsumerStateSucceeded
	case int32(rpc.ConsumerState_CONSUMER_STATE_RETRY):
		return model.ConsumerStateRetry
	case int32(rpc.ConsumerState_CONSUMER_STATE_DEAD_LETTER):
		return model.ConsumerStateDeadLetter
	default:
		return ""
	}
}

// consumerStateToRPC 落库字符串 -> 契约枚举。库里出现状态机之外的值（人为改库）时给
// UNSPECIFIED：调用方看得见「这一行的状态我解释不了」，而不是被悄悄归到某个已知状态。
func consumerStateToRPC(name string) rpc.ConsumerState {
	switch name {
	case model.ConsumerStateReceived:
		return rpc.ConsumerState_CONSUMER_STATE_RECEIVED
	case model.ConsumerStateProcessing:
		return rpc.ConsumerState_CONSUMER_STATE_PROCESSING
	case model.ConsumerStateSucceeded:
		return rpc.ConsumerState_CONSUMER_STATE_SUCCEEDED
	case model.ConsumerStateRetry:
		return rpc.ConsumerState_CONSUMER_STATE_RETRY
	case model.ConsumerStateDeadLetter:
		return rpc.ConsumerState_CONSUMER_STATE_DEAD_LETTER
	default:
		return rpc.ConsumerState_CONSUMER_STATE_UNSPECIFIED
	}
}
