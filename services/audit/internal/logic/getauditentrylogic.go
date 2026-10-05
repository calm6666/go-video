package logic

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"go-video/services/audit/internal/svc"
	"go-video/services/audit/model"
	"go-video/services/audit/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetAuditEntryLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetAuditEntryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetAuditEntryLogic {
	return &GetAuditEntryLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 按 entry_id / event_id 取单条。
//
// 语义（规格 1~4）：
//  1. 两个定位键都为空 → 明确报错，不猜调用方想查什么；
//  2. 未命中返回 found=false + 空 entry，而不是 gRPC NotFound，便于调用方批量核对；
//  3. 命中后写一条 action_domain=data_access、action=audit.entry.get 的自审计条目，
//     摘要只放被查条目的 entry_hash（「谁看过哪条存证」可追，且不回读内容本身）；
//  4. 自审计写入失败不改变读结果，但必须打 Error 级日志，让审计缺口可见而不是被静默吞掉。
func (l *GetAuditEntryLogic) GetAuditEntry(in *rpc.GetAuditEntryReq) (*rpc.GetAuditEntryReply, error) {
	if in == nil {
		return nil, model.ErrRequestRequired
	}
	cc := in.GetCtx()
	if err := validateCallContext(cc, false); err != nil {
		return nil, err
	}
	entryID := in.GetEntryId()
	// 定位键先 trim：一条全是空白的 event_id 不是「定位」，
	// 放过去会拿空白串去撞 uniq_event_id，回一个「没找到」把调用方的参数错误掩盖掉。
	eventID := strings.TrimSpace(in.GetEventId())
	if entryID <= 0 && eventID == "" {
		return nil, model.ErrEntryIDOrEventIDRequired
	}

	d := buildDeps(l.svcCtx)
	var (
		row *model.AuditEntry
		err error
	)
	if entryID > 0 {
		// entry_id 优先：它与 (chain_key, seq) 一一对应，比字符串幂等键更适合精确定位。
		row, err = d.entries.FindOne(l.ctx, entryID)
		if err != nil && errors.Is(err, model.ErrEntryNotFound) {
			row, err = nil, nil
		}
	} else {
		row, err = d.entries.FindByEventID(l.ctx, eventID)
	}
	if err != nil {
		l.Errorf("GetAuditEntry 查询失败 entry_id=%d event_id=%s err=%v", entryID, eventID, err)
		return nil, err
	}
	if row == nil {
		// 未命中同样留痕：连续探测不存在的存证本身就是值得回看的行为。
		selfAuditLogged(l.ctx, l.Logger, cc, d, selfAuditSpec{
			Action:     actionEntryGet,
			Domain:     selfDomainDataAccess,
			TargetType: targetAuditEntry,
			TargetID:   firstNonEmpty(eventID, strconv.FormatInt(entryID, 10)),
			Reason:     "读取存证条目：未命中",
		})
		return &rpc.GetAuditEntryReply{Found: false}, nil
	}

	selfAuditLogged(l.ctx, l.Logger, cc, d, selfAuditSpec{
		Action:     actionEntryGet,
		Domain:     selfDomainDataAccess,
		TargetType: targetAuditEntry,
		TargetID:   row.EventID,
		Reason:     "读取存证条目",
		After:      map[string]string{"entry_id": strconv.FormatInt(row.EntryID, 10)},
		// 摘要放被查条目的 entry_hash：既能回答「看的是哪条存证」，又不复制存证内容。
		Before: map[string]string{"entry_hash": row.EntryHash},
	})
	return &rpc.GetAuditEntryReply{Entry: entryView(row), Found: true}, nil
}
