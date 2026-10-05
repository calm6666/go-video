// Code scaffolded by goctl. Safe to edit.

package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/spm/internal/svc"
	"go-video/services/spm/model"
	"go-video/services/spm/rpc"

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

// 死信留档查询（只读；重放属于 services/cron 的待接线项）
func (l *ListDeadLettersLogic) ListDeadLetters(in *rpc.ListDeadLettersReq) (*rpc.ListDeadLettersReply, error) {
	// 逻辑轮规划：分页读 spm_dead_letter（topic/state/since 过滤，ctime 倒序）；payload_preview 在写入侧已按数字串掩码脱敏，本方法不二次加工，也不提供原文重取能力（避免行为明细外泄）。
	done, err := acquireReadToken(l.ctx, l.svcCtx, l.Logger, "ListDeadLetters")
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
	filter := model.DeadLetterFilter{
		Topic:  topic,
		Limit:  size,
		Offset: offsetTo32(pageOffset(in.GetPn(), size)),
	}
	if state := strings.TrimSpace(in.GetState()); state != "" {
		// 留档状态是字符串列（open/replayed/ignored），非法值必须拒绝：
		// 当成「不限」返回会把「筛错了」显示成「所有死信」。
		if !model.ValidDeadLetterState(state) {
			return nil, fmt.Errorf("%w: state=%q", model.ErrInvalidDeadLetterState, state)
		}
		filter.State = state
	}
	if in.GetSince() < 0 {
		return nil, fmt.Errorf("%w: since=%d 不能为负", model.ErrInvalidDeadLetterState,
			in.GetSince())
	}
	filter.Since = in.GetSince()

	total, err := l.svcCtx.DeadLetters.Count(l.ctx, filter)
	if err != nil {
		return nil, err
	}
	reply := &rpc.ListDeadLettersReply{Total: total}
	if !pageFits(int64(filter.Offset), total, size) {
		return reply, nil
	}
	rows, err := l.svcCtx.DeadLetters.List(l.ctx, filter)
	if err != nil {
		return nil, err
	}
	items := make([]*rpc.ListDeadLettersReply_DeadLetter, 0, len(rows))
	for _, r := range rows {
		// payload_preview 原样回带：脱敏在写入侧完成，读侧二次加工（截断之外的一切改写）
		// 只会让「运维认消息」这件事变得更难；本接口也不提供取原文的入口。
		items = append(items, &rpc.ListDeadLettersReply_DeadLetter{
			Id:             r.ID,
			EventId:        r.EventID,
			EventType:      r.EventType,
			Topic:          r.Topic,
			PayloadDigest:  r.PayloadDigest,
			PayloadPreview: r.PayloadPreview,
			Reason:         r.Reason,
			State:          r.State,
			Ctime:          r.Ctime,
		})
	}
	reply.Items = items
	return reply, nil
}
