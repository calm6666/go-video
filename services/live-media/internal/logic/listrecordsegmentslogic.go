package logic

import (
	"context"

	"go-video/services/live-media/internal/svc"
	"go-video/services/live-media/model"
	"go-video/services/live-media/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListRecordSegmentsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListRecordSegmentsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListRecordSegmentsLogic {
	return &ListRecordSegmentsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// keyset 分页拉取切片（回放拼接与排障）
//
// 只读方法：不写库、不发事件；拼接方（Worker）据此判定区间可用性，
// 需要区间条件聚合时用 model.StatsInRange，不必自己遍历全表。
// 硬要求 record_id>0：切片表是只增不减的大表（一场 3 小时直播按 10s 分片 = 数千行），
// 没有 record_id 就是让它做全表扫，代价由下一个请求承担。
// 游标语义：after_seq<0 是传坏了（0 才是「从头」），拒绝而不是当 0 用——
// 静默归零会让客户端以为游标已推进，实际在同一批数据上死循环（ErrInvalidCursor）。
// limit 走 segmentPageLimit：<=0 取默认 200，越界夹到 MaxSegmentPageSize（500）。
// 这里夹取而不报错，是因为 keyset 的 limit 不改变扫描量级（命中 uniq_record_seq 区间扫，
// 扫到 limit 行就停），与 OFFSET 翻页的代价性质不同。
// 排序固定 seq ASC；has_more = 本页取满 limit（「可能还有」，宁可让客户端再要一次空页），
// next_after_seq 在空页时原样回传入游标，保证同一游标可稳定重试。
// total 是该 record_id 的切片总数（含 MISSING/CORRUPT 缺口行，proto 契约如此），
// 不随 state 过滤变化；两者口径不同属契约设计，不在此悄悄改写。
// 投影：bucket/object_key 原样返回相对路径引用，不签名、不拼 CDN 地址（AGENTS.md §6）。
func (l *ListRecordSegmentsLogic) ListRecordSegments(in *rpc.ListRecordSegmentsReq) (*rpc.ListRecordSegmentsReply, error) {
	cfg := l.svcCtx.Config.LiveMedia
	recordID := in.GetRecordId()
	if err := checkPositive("record_id", recordID, model.ErrRecordTaskNotFound); err != nil {
		return nil, err
	}
	afterSeq := in.GetAfterSeq()
	if err := checkAfterSeq(afterSeq); err != nil {
		return nil, err
	}
	state, err := filterState(int32(in.GetState()), checkSegmentState)
	if err != nil {
		return nil, err
	}
	limit := segmentPageLimit(cfg, in.GetLimit())

	rows, err := l.svcCtx.Segments.ListAfter(l.ctx, recordID, afterSeq, state, limit)
	if err != nil {
		return nil, err
	}
	total, err := l.svcCtx.Segments.CountByRecord(l.ctx, recordID)
	if err != nil {
		return nil, err
	}
	nextAfterSeq := afterSeq
	if n := len(rows); n > 0 {
		nextAfterSeq = rows[n-1].Seq
	}
	return &rpc.ListRecordSegmentsReply{
		Segments:     segmentInfos(rows),
		NextAfterSeq: nextAfterSeq,
		HasMore:      len(rows) == int(limit),
		Total:        total,
	}, nil
}
