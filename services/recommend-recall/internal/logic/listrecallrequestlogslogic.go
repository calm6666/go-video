package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/recommend-recall/internal/svc"
	"go-video/services/recommend-recall/model"
	"go-video/services/recommend-recall/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListRecallRequestLogsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListRecallRequestLogsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListRecallRequestLogsLogic {
	return &ListRecallRequestLogsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查召回请求日志（运营/排障）
//
// 只读、无副作用；响应含 mid/scene，属运营面数据，不经 gateway/app 暴露给客户端（AGENTS.md §3）。
// 深翻有硬上限：offset 超过 model.MaxRequestLogOffset 直接 ErrPageTooDeep，
// 不放开上限 —— 日志表按 mid+时间窗口查就够用，允许任意深翻等于把一次 COUNT+OFFSET 变成慢查询。
func (l *ListRecallRequestLogsLogic) ListRecallRequestLogs(in *rpc.ListRecallRequestLogsReq) (*rpc.ListRecallRequestLogsReply, error) {
	if in == nil {
		return nil, fmt.Errorf("%w: ListRecallRequestLogsReq", model.ErrRequestRequired)
	}
	repo := l.svcCtx.Repository
	opts := repo.Options()
	scene, err := optionalText("scene", in.GetScene(), colScene)
	if err != nil {
		return nil, err
	}
	// 默认页大小取"单页上限"：本接口是排障列表，调用方翻页的目标是"看全这段时间的请求"。
	offset, limit, err := pageArgs(in.GetPn(), in.GetPs(), opts.MaxRequestLogPage,
		opts.MaxRequestLogPage, model.MaxRequestLogOffset)
	if err != nil {
		return nil, err
	}
	mid := in.GetMid()
	if mid < 0 {
		return nil, fmt.Errorf("%w: mid=%d", model.ErrRequestLogRequired, mid)
	}
	from, to := in.GetFromTime(), in.GetToTime()
	if from < 0 || to < 0 {
		return nil, fmt.Errorf("%w: from_time=%d to_time=%d", model.ErrRequestLogRequired, from, to)
	}
	// 时间区间倒挂在 model 侧也是错误，但在 logic 先判一次：
	// ErrPageTooDeep 的文本会带上进 SQL 的参数名，调用方拿到的是"你的窗口反了"而不是"分页太深"。
	if from > 0 && to > 0 && from > to {
		return nil, fmt.Errorf("%w: from_time=%d > to_time=%d", model.ErrPageTooDeep, from, to)
	}
	q := model.RequestLogQuery{
		Mid:    mid,
		Scene:  scene,
		From:   from,
		To:     to,
		Offset: offset,
		Limit:  limit,
	}
	total, err := repo.RequestLog.Count(l.ctx, q)
	if err != nil {
		return nil, err
	}
	rows, err := repo.RequestLog.List(l.ctx, q)
	if err != nil {
		return nil, err
	}
	entries := make([]*rpc.RecallRequestLogInfo, 0, len(rows))
	for _, row := range rows {
		entry, ierr := requestLogInfo(row)
		if ierr != nil {
			// 单行解析失败不能跳过：跳过等于把"某次请求的逐路统计丢了"从响应里抹掉，
			// 排障的人会以为那一次请求没有降级记录。整页报错更安全。
			return nil, ierr
		}
		entries = append(entries, entry)
	}
	return &rpc.ListRecallRequestLogsReply{
		Entries: entries,
		HasMore: int64(offset)+int64(len(entries)) < total,
	}, nil
}

var _ = strings.TrimSpace
