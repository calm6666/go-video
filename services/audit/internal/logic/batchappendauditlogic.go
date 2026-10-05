package logic

import (
	"context"
	"fmt"

	"go-video/services/audit/internal/svc"
	"go-video/services/audit/model"
	"go-video/services/audit/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type BatchAppendAuditLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewBatchAppendAuditLogic(ctx context.Context, svcCtx *svc.ServiceContext) *BatchAppendAuditLogic {
	return &BatchAppendAuditLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 批量追加（Outbox 重放 / 领域服务批量留痕；任一条非法整批拒绝）。
//
// 为什么整批拒绝而不是逐条回报：半批成功会让「这次动作到底有没有留痕」变成
// 不可回答的问题，而上游 Outbox 只在整批成功时才 ack；逐条部分成功等于
// 让上游以为都写入了却永久缺了几条证据。
func (l *BatchAppendAuditLogic) BatchAppendAudit(in *rpc.BatchAppendAuditReq) (*rpc.BatchAppendAuditReply, error) {
	if in == nil {
		return nil, model.ErrRequestRequired
	}
	cc := in.GetCtx()
	if err := validateCallContext(cc, true); err != nil {
		return nil, err
	}
	d := buildDeps(l.svcCtx)
	drafts := in.GetEntries()
	if len(drafts) == 0 {
		return nil, model.ErrBatchEmpty
	}
	maxBatch := d.opts.MaxBatchSize
	if maxBatch <= 0 {
		maxBatch = 1
	}
	if len(drafts) > maxBatch {
		return nil, fmt.Errorf("%w: got=%d max=%d", model.ErrBatchTooLarge, len(drafts), maxBatch)
	}

	rows := make([]*model.AuditEntry, 0, len(drafts))
	seen := make(map[string]int, len(drafts))
	for i, draft := range drafts {
		row, err := d.prepareEntry(cc, draft)
		if err != nil {
			// 点出下标，否则调用方只能对着一批同构的条目逐条盲猜哪条写坏了。
			return nil, fmt.Errorf("audit: entries[%d] 非法: %w", i, err)
		}
		if prev, dup := seen[row.EventID]; dup {
			return nil, fmt.Errorf("%w: entries[%d] 与 entries[%d] 同为 %s",
				model.ErrDuplicateEventID, i, prev, row.EventID)
		}
		seen[row.EventID] = i
		rows = append(rows, row)
	}

	out, accepted, reused, err := d.appendIdempotent(l.ctx, rows)
	if err != nil {
		l.Errorf("BatchAppendAudit 写入失败 caller=%s request_id=%s count=%d err=%v",
			cc.GetCallerService(), cc.GetRequestId(), len(rows), err)
		return nil, err
	}
	return &rpc.BatchAppendAuditReply{
		Entries:  entryViews(out),
		Accepted: accepted,
		Reused:   reused,
	}, nil
}
