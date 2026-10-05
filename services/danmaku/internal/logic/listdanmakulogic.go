package logic

import (
	"context"
	"fmt"
	"sort"

	"go-video/services/danmaku/internal/policy"
	"go-video/services/danmaku/internal/svc"
	"go-video/services/danmaku/model"
	"go-video/services/danmaku/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListDanmakuLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListDanmakuLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListDanmakuLogic {
	return &ListDanmakuLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListDanmaku 按 oid + 时间分段批量拉取弹幕。
//
// 窗口可用分段号（start_seg/end_seg）或时间轴毫秒（start_progress_ms/end_progress_ms）表达；
// 服务端先读段缓存，miss 的分段回源 MySQL 并逐段回填。
// 用户级屏蔽（屏蔽某人、屏蔽关键词）在读取侧过滤，不改动主表状态。
func (l *ListDanmakuLogic) ListDanmaku(in *rpc.ListDanmakuReq) (*rpc.ListDanmakuReply, error) {
	if in.Oid <= 0 {
		return nil, model.ErrInvalidOid
	}
	segSec := l.svcCtx.SegmentSeconds

	startSeg, endSeg, err := resolveSegWindow(in, segSec)
	if err != nil {
		return nil, err
	}
	maxWindow := l.svcCtx.Config.Danmaku.MaxSegWindow
	if maxWindow <= 0 {
		maxWindow = 60
	}
	if int(endSeg)-int(startSeg)+1 > maxWindow {
		return nil, fmt.Errorf("%w: %d > %d", model.ErrSegRangeTooLarge, int(endSeg)-int(startSeg)+1, maxWindow)
	}
	segs := policy.Segments(startSeg, endSeg, maxWindow)

	rows, cacheHits, err := l.svcCtx.Repository.ListVisibleBySegs(l.ctx, in.Oid, segs, in.Limit)
	if err != nil {
		l.Errorf("danmaku/ListDanmaku: list oid=%d segs=%d err=%v", in.Oid, len(segs), err)
		return nil, err
	}

	// 本人待审/折叠弹幕单独回源，不进入段缓存（段缓存是跨用户共享的下发包）。
	if in.WithSelfPending && in.ViewerMid > 0 {
		mine, err := l.svcCtx.Repository.ListMineBySegs(l.ctx, in.Oid, in.ViewerMid, segs)
		if err != nil {
			l.Errorf("danmaku/ListDanmaku: list mine oid=%d mid=%d err=%v", in.Oid, in.ViewerMid, err)
			return nil, err
		}
		rows = append(rows, mine...)
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].ProgressMs != rows[j].ProgressMs {
				return rows[i].ProgressMs < rows[j].ProgressMs
			}
			return rows[i].Dmid < rows[j].Dmid
		})
	}

	visibleRows, err := l.applyUserBlocks(rows, in.ViewerMid)
	if err != nil {
		l.Errorf("danmaku/ListDanmaku: user filter oid=%d viewer=%d err=%v", in.Oid, in.ViewerMid, err)
		return nil, err
	}

	counts, err := l.svcCtx.Repository.SegmentCounts(l.ctx, in.Oid, segs)
	if err != nil {
		// 段计数是辅助信息：失败降级为不下发计数，不阻断弹幕窗口拉取。
		l.Errorf("danmaku/ListDanmaku: segment counts oid=%d err=%v", in.Oid, err)
		counts = nil
	}

	return &rpc.ListDanmakuReply{
		Danmaku:        danmakuListToRPC(visibleRows),
		SegmentCounts:  segmentCountsToRPC(segs, counts),
		SegmentSeconds: segSec,
		NextSeg:        endSeg + 1,
		CacheHits:      cacheHits,
	}, nil
}

// resolveSegWindow 解析请求的时间轴窗口为分段号闭区间。
// progress_ms 优先于 seg_no，客户端不需要自己算分段。
func resolveSegWindow(in *rpc.ListDanmakuReq, segSec int32) (int32, int32, error) {
	var startSeg, endSeg int32
	switch {
	case in.StartProgressMs > 0 || in.EndProgressMs > 0:
		startSeg, endSeg = policy.SegRangeFromMs(in.StartProgressMs, in.EndProgressMs, segSec)
	default:
		startSeg, endSeg = in.StartSeg, in.EndSeg
		if endSeg == 0 && startSeg == 0 {
			endSeg = 0 // 未指定窗口时只取第 0 段
		}
	}
	if !policy.SegRangeValid(startSeg, endSeg) {
		return 0, 0, model.ErrInvalidSegRange
	}
	return startSeg, endSeg, nil
}

// applyUserBlocks 按查看者的屏蔽列表过滤弹幕。
// viewerMid<=0（游客）不做过滤，普通池数据可直接下发。
func (l *ListDanmakuLogic) applyUserBlocks(rows []*model.Danmaku, viewerMid int64) ([]*model.Danmaku, error) {
	if viewerMid <= 0 || len(rows) == 0 {
		return rows, nil
	}
	blockedMids, keywordFilter, err := l.svcCtx.Repository.UserBlockFilter(l.ctx, viewerMid)
	if err != nil {
		return nil, err
	}
	if len(blockedMids) == 0 && keywordFilter.Len() == 0 {
		return rows, nil
	}
	out := make([]*model.Danmaku, 0, len(rows))
	for _, d := range rows {
		if d.Dmid == 0 {
			continue
		}
		// 本人弹幕永远保留，否则用户会看不到自己刚发出的内容。
		if d.Mid == viewerMid {
			out = append(out, d)
			continue
		}
		if blockedMids[d.Mid] {
			continue
		}
		if _, hit := keywordFilter.Match(d.Content); hit {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// segmentCountsToRPC 按请求分段顺序输出段计数，保证客户端可按下标对齐。
func segmentCountsToRPC(segs []int32, counts map[int32]int32) []*rpc.SegmentCount {
	out := make([]*rpc.SegmentCount, 0, len(segs))
	for _, s := range segs {
		out = append(out, &rpc.SegmentCount{SegNo: s, Count: counts[s]})
	}
	return out
}
