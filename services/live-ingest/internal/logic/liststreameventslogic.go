package logic

import (
	"context"
	"fmt"

	"go-video/services/live-ingest/internal/svc"
	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListStreamEventsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListStreamEventsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListStreamEventsLogic {
	return &ListStreamEventsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 事件位点：按 seq 游标拉取状态事件（live-room/live-media 补偿与对账）
//
// 游标是 (stream_id, seq)：seq 只由合法迁移递增，事件行 append-only 不可改，
// 所以「从 after_seq 之后重新拉」就是补偿的全部语义，不需要 also 传 offset。
// max_seq 单独查一次：消费方拿它和已应用的最大 seq 比就知道自己是否追平，
// 不必为了判断「还有没有」多拉一页。
func (l *ListStreamEventsLogic) ListStreamEvents(in *rpc.ListStreamEventsReq) (*rpc.ListStreamEventsReply, error) {
	cfg := l.svcCtx.Config.LiveIngest
	repo := l.svcCtx.Repository
	if repo == nil {
		return nil, errNoRepository
	}
	streamID, err := checkStreamID(in.StreamId)
	if err != nil {
		return nil, err
	}
	if in.AfterSeq < 0 {
		return nil, fmt.Errorf("%w: after_seq 不能为负", model.ErrInvalidStreamId)
	}
	// 流不存在要报错而不是返回空列表：对账任务把「查无此流」当成「事件已全部应用」
	// 是最难事后发现的一类静默不一致。
	s, err := repo.Stream.FindOne(l.ctx, streamID)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, model.ErrStreamNotFound
	}

	limit := clampLimit(in.Limit, cfg.MaxEventPageSize)
	rows, err := repo.StreamEvent.ListAfterSeq(l.ctx, streamID, in.AfterSeq, limit, in.Desc)
	if err != nil {
		return nil, err
	}
	maxSeq, err := repo.StreamEvent.MaxSeq(l.ctx, streamID)
	if err != nil {
		return nil, err
	}
	return &rpc.ListStreamEventsReply{
		Events:  eventInfos(rows),
		MaxSeq:  maxSeq,
		HasMore: eventsHaveMore(rows, limit, maxSeq, in.Desc),
	}, nil
}

// eventsHaveMore 判断游标之后是否还有事件：装满一页就可能有下一页；
// 升序未装满时，只要最后一条还没到该流最大 seq，说明中间确实还有没给出去的。
// desc 取的是「最新的一批」，未装满即表示 after_seq 之上已全部返回。
func eventsHaveMore(rows []*model.StreamEvent, limit int32, maxSeq int64, desc bool) bool {
	if int32(len(rows)) >= limit {
		return true
	}
	if desc || len(rows) == 0 {
		return false
	}
	return rows[len(rows)-1].Seq < maxSeq
}
