package logic

import (
	"context"
	"fmt"
	"time"

	"go-video/services/risk-control/internal/svc"
	"go-video/services/risk-control/model"
	"go-video/services/risk-control/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpsertListEntryLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpsertListEntryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpsertListEntryLogic {
	return &UpsertListEntryLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// maxListTargetValueLen 与 risk_list.target_value 的列宽（VARCHAR(64)，同时是 uniq_target 的组成列）
// 对齐。设备/IP 类摘要由 model.NormalizeIPHash 限定在 8-64，但 mid 类只保证「全是十进制数字」，
// 长度不设限：超过列宽时 MySQL 严格模式直接报 1406，非严格模式则**静默截断**——
// 截断后的值可能等于另一条真实条目的规范化结果，uniq_target 于是把两条不该相同的条目折叠成
// 一行（后写覆盖前写的封禁），所以这里只能拒绝，不能裁断（同缺口 #14 的幂等键）。
const maxListTargetValueLen = 64

// 新增/更新名单条目（唯一键 list_type + target_type + target_value，覆盖式写入）。
//
// 隐私约束：target_value 只接受账号 ID 十进制串、设备受控 ID（SHA-256 十六进制）
// 或调用方预哈希的 IP 摘要；传明文 IP 会返回 ErrRawIPForbidden 而不是由服务端代哈希 ——
// 服务端一旦代哈希，明文 IP 就已经进入了本服务的内存与日志链路。
// 审计约束：operator 必填 >0，名单是「不透明命中即拒」的能力，必须能追到写入人。
func (l *UpsertListEntryLogic) UpsertListEntry(in *rpc.UpsertListEntryReq) (*rpc.UpsertListEntryReply, error) {
	listType := int32(in.GetListType())
	targetType := int32(in.GetTargetType())
	if !model.ValidListType(listType) || !model.ValidTargetType(targetType) {
		return nil, fmt.Errorf("%w: list_type=%d target_type=%d", model.ErrInvalidListEntry, listType, targetType)
	}
	if in.GetDurationSeconds() < 0 {
		return nil, fmt.Errorf("%w: duration_seconds must be >= 0 (0 = permanent)", model.ErrInvalidListEntry)
	}
	if in.GetState() != model.StateDisabled && in.GetState() != model.StateEnabled {
		return nil, fmt.Errorf("%w: state=%d", model.ErrInvalidListEntry, in.GetState())
	}
	targetValue := model.NormalizeTargetValue(targetType, in.GetTargetValue())
	if targetValue == "" {
		return nil, fmt.Errorf("%w: target_value rejected (raw ip or non-hex digest not accepted)", model.ErrInvalidListEntry)
	}
	if len(targetValue) > maxListTargetValueLen {
		return nil, fmt.Errorf("%w: target_value too long, max %d", model.ErrInvalidListEntry, maxListTargetValueLen)
	}

	entry := &model.RiskList{
		ListType:    listType,
		TargetType:  targetType,
		TargetValue: targetValue,
		Reason:      in.GetReason(),
		Operator:    in.GetOperator(),
		State:       in.GetState(),
	}
	if len(entry.Reason) > maxReasonLen {
		return nil, fmt.Errorf("%w: reason too long", model.ErrInvalidListEntry)
	}
	if d := in.GetDurationSeconds(); d > 0 {
		entry.ExpireAt = time.Now().Unix() + d
	}

	saved, created, err := l.svcCtx.Repository.UpsertListEntry(l.ctx, entry)
	if err != nil {
		l.Errorf("risk-control/UpsertListEntry: rejected list_type=%d target_type=%d operator=%d err=%v",
			listType, targetType, in.GetOperator(), err)
		return nil, err
	}
	l.Infof("risk-control/UpsertListEntry: entry=%d list_type=%d target_type=%d created=%t operator=%d",
		saved.ID, saved.ListType, saved.TargetType, created, saved.Operator)
	return &rpc.UpsertListEntryReply{Entry: listEntryToProto(saved), Created: created}, nil
}
