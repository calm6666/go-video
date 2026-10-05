// Code scaffolded by goctl. Safe to edit.

package logic

import (
	"context"
	"strings"

	"go-video/services/event-collector/internal/svc"
	"go-video/services/event-collector/model"
	"go-video/services/event-collector/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListIngestBatchesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListIngestBatchesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListIngestBatchesLogic {
	return &ListIngestBatchesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询批次台账。
//
// 翻页口径（与 ListEventRecords / ListDeadLetters 共用 newPageWindow）：
//   - page_size 经 model.ClampPageSize(PageSize, MaxPageSize) 收敛，越界返回 ErrInvalidPage；
//   - cursor 解析失败返回 ErrInvalidCursor，绝不当成「第一页」（那会让运营翻页重复计数）；
//   - 按 (ctime, id) 倒序行比较翻页，不用 OFFSET；has_more 用 ps+1 探测，total 走 Count。
//
// 隐私（AGENTS.md §7）：输出只含脱敏列（device_hash / ip_segment / salt_version），
// 且过滤条件本身也只接受脱敏形态 —— 明文设备号/完整 IP 作为 SQL 参数会留在查询日志里。
// 另外强制「至少一个条件」（等值条件或 ctime 边界）：本表随采集流量线性增长，
// 无条件的 COUNT 会把排障接口变成打垮自身 MySQL 的入口。
func (l *ListIngestBatchesLogic) ListIngestBatches(in *rpc.ListIngestBatchesReq) (*rpc.ListIngestBatchesReply, error) {
	w, err := newPageWindow(l.svcCtx.Config, in.GetPageSize(), in.GetCursor(), in.GetCtimeFrom(), in.GetCtimeTo())
	if err != nil {
		return nil, err
	}
	source := int32(in.GetSource())
	if source != 0 && !model.ValidSource(source) {
		return nil, model.ErrInvalidPage
	}
	state := int32(in.GetState())
	if state != 0 && !model.ValidBatchState(state) {
		return nil, model.ErrInvalidStateTransition
	}
	deviceHash := strings.TrimSpace(in.GetDeviceHash())
	if deviceHash != "" && !validDeviceHashFilter(deviceHash) {
		return nil, model.ErrPrivacyFieldForbidden
	}
	ipSegment := strings.TrimSpace(in.GetIpSegment())
	if ipSegment != "" && !validIPSegmentFilter(ipSegment) {
		return nil, model.ErrPrivacyFieldForbidden
	}
	f := model.IngestBatchFilter{
		Source:     source,
		State:      state,
		Mid:        in.GetMid(),
		DeviceHash: deviceHash,
		IPSegment:  ipSegment,
		CtimeFrom:  w.from,
		CtimeTo:    w.to,
	}
	// mid=0 在 proto3 里与「未传」同值，所以未登录批次不能用 mid 筛（改用其余条件组合）。
	if err := requireWindowOrFilter(source != 0 || state != 0 || in.GetMid() != 0 ||
		deviceHash != "" || ipSegment != "", w,
		"source/state/mid/device_hash/ip_segment/ctime 区间"); err != nil {
		return nil, err
	}

	rows, err := l.svcCtx.Batches.List(l.ctx, f, w.cursor, w.fetchLimit())
	if err != nil {
		return nil, err
	}
	list, hasMore := trimPage(rows, w.pageSize)
	total, err := l.svcCtx.Batches.Count(l.ctx, f)
	if err != nil {
		return nil, err
	}
	var next string
	if hasMore && len(list) > 0 {
		last := list[len(list)-1]
		next = nextCursor(true, last.Ctime, last.ID)
	}
	return &rpc.ListIngestBatchesReply{
		List:       batchToRPCList(list),
		NextCursor: next,
		HasMore:    hasMore,
		Total:      total,
	}, nil
}
