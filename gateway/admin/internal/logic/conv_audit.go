// 本文件是 gateway/admin 的手写投影扩展（非 goctl 生成产物）。
//
// audit RPC → 管理后台投影 + 调用上下文装配。
//
// 网关在这里只做三件事（AGENTS.md §4/§5）：
//  1. 装配 CallContext：caller_service 由网关固定为 gateway/admin，不接受客户端声明，
//     这样 audit 侧的「谁声称做了这件事」归因不会被后台表单伪造；operator_id 优先取
//     AdminPermission 中间件解析出的会话身份；
//  2. 卡门槛：写接口必须带 request_id（proto 注释「写接口必填」），分页口径与 audit 的
//     AuditQuery.MaxPageSize 对齐；时间跨度、必须带收窄维度、保留期合法性全部由 audit 判定；
//  3. 投影：把 RPC 消息原样搬成后台 types。审计条目的哈希字段（prev_hash/entry_hash）
//     必须逐字段透传，任何裁剪都会让运营无法用同一份数据重放校验。

package logic

import (
	"context"
	"errors"

	"go-video/common/validation"
	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/types"
	auditrpc "go-video/services/audit/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// auditCallerService 是网关在 audit CallContext 里的自报身份。
// 与 services/*/etc/*.yaml 的 Name 口径一致（服务名，不是实例地址）。
const auditCallerService = "gateway/admin"

// auditMaxPageSize 对齐 services/audit 的 AuditQuery.MaxPageSize 默认值（100）。
// 网关先收敛一次避免大页打爆下游，服务端仍会二次拒绝（两边不会漂移）。
const auditMaxPageSize = 100

// errAuditServiceNotConfigured：未配置 AuditRPC 时受影响的 audit 路由一律返回它，
// 与既有各域「服务未配置」的口径一致，logic 不退化成伪造空结果。
var errAuditServiceNotConfigured = errors.New("audit service not configured")

// errAuditExpectVersionInvalid 拦住负版本号：0 是「新建」的合法语义，负数没有对应状态，
// 透传给 audit 只会得到一次无意义的往返。
var errAuditExpectVersionInvalid = errors.New("gateway/admin: expect_version must be >= 0")

// normalizeAuditPage 复用 common/validation.NormalizePage（pn<=0→1、ps<=0→20、ps>100→100）。
func normalizeAuditPage(pn, ps int32) (int32, int32) {
	page := validation.NormalizePage(int(pn), int(ps), auditMaxPageSize)
	return int32(page.Page), int32(page.PageSize)
}

// auditCallContext 装配 audit.v1.CallContext。
//
// write=true 用于所有写接口（导出申请/推进、保留策略保存、归档触发）：request_id 必填。
// operator_id 以中间件会话身份为准；读路由不在中间件组内，此时按客户端声明的
// operator_id 走，由 audit 侧的 data_access 自审计留痕（见 proto ListAuditEntriesReq 注释）。
func auditCallContext(ctx context.Context, cc types.AuditCallContext, write bool) (*auditrpc.CallContext, error) {
	adminID := cc.OperatorId
	if id, ok := middleware.AdminFromContext(ctx); ok {
		if cc.OperatorId > 0 && cc.OperatorId != id.AdminID {
			// 与 operation 同一套口径：以会话为准并留痕，便于按 trace_id 追越权尝试。
			logx.WithContext(ctx).Errorf("gateway/admin/audit: operator_id mismatch session=%d claimed=%d",
				id.AdminID, cc.OperatorId)
		}
		adminID = id.AdminID
	}
	if err := requireOperatorID(adminID); err != nil {
		return nil, err
	}
	if write {
		if err := requireNonEmpty("ctx.request_id", cc.RequestId); err != nil {
			return nil, err
		}
	}
	return &auditrpc.CallContext{
		CallerService: auditCallerService,
		OperatorId:    adminID,
		TraceId:       cc.TraceId,
		RequestId:     cc.RequestId,
		Ip:            cc.Ip,
		UserAgent:     cc.UserAgent,
	}, nil
}

// auditTimeRange 校验时间戳非负并原样返回（含 start、不含 end 的语义由 audit 判定）。
// 只做「不能是负数」这一条：跨度上限（AuditQuery.MaxRangeDays）与必填维度都在服务端。
func auditTimeRange(startAt, endAt int64) error {
	if startAt < 0 || endAt < 0 {
		return errors.New("gateway/admin: start_at/end_at must be >= 0")
	}
	return nil
}

// auditEntryToAPI 投影单条审计。nil（found=false 时的空 entry）投影成零值而不是 panic。
func auditEntryToAPI(e *auditrpc.AuditEntryView) types.AuditEntryItem {
	if e == nil {
		return types.AuditEntryItem{}
	}
	return types.AuditEntryItem{
		EntryId:       e.GetEntryId(),
		EventId:       e.GetEventId(),
		SchemaVersion: e.GetSchemaVersion(),
		ChainKey:      e.GetChainKey(),
		Seq:           e.GetSeq(),
		ActorType:     int32(e.GetActorType()),
		ActorId:       e.GetActorId(),
		ActorName:     e.GetActorName(),
		Action:        e.GetAction(),
		ActionDomain:  e.GetActionDomain(),
		TargetType:    e.GetTargetType(),
		TargetId:      e.GetTargetId(),
		Result:        int32(e.GetResult()),
		BeforeDigest:  e.GetBeforeDigest(),
		AfterDigest:   e.GetAfterDigest(),
		Reason:        e.GetReason(),
		SourceApp:     int32(e.GetSourceApp()),
		IpHash:        e.GetIpHash(),
		DeviceHash:    e.GetDeviceHash(),
		TraceId:       e.GetTraceId(),
		RequestId:     e.GetRequestId(),
		OccurredAt:    e.GetOccurredAt(),
		PrevHash:      e.GetPrevHash(),
		EntryHash:     e.GetEntryHash(),
		Ctime:         e.GetCtime(),
		ArchivedAt:    e.GetArchivedAt(),
		CallerService: e.GetCallerService(),
	}
}

func auditEntriesToAPI(list []*auditrpc.AuditEntryView) []types.AuditEntryItem {
	out := make([]types.AuditEntryItem, 0, len(list))
	for _, e := range list {
		out = append(out, auditEntryToAPI(e))
	}
	return out
}

func auditExportToAPI(t *auditrpc.AuditExportTask) types.AuditExportTaskItem {
	if t == nil {
		return types.AuditExportTaskItem{}
	}
	return types.AuditExportTaskItem{
		TaskId:        t.GetTaskId(),
		RequestId:     t.GetRequestId(),
		OperatorId:    t.GetOperatorId(),
		CallerService: t.GetCallerService(),
		FilterJson:    t.GetFilterJson(),
		Format:        t.GetFormat(),
		State:         t.GetState(),
		RowCount:      t.GetRowCount(),
		Bucket:        t.GetBucket(),
		ObjectKey:     t.GetObjectKey(),
		ObjectSize:    t.GetObjectSize(),
		ExpireAt:      t.GetExpireAt(),
		FileHash:      t.GetFileHash(),
		ErrMsg:        t.GetErrMsg(),
		TraceId:       t.GetTraceId(),
		Ctime:         t.GetCtime(),
		StartedAt:     t.GetStartedAt(),
		FinishedAt:    t.GetFinishedAt(),
	}
}

func auditExportsToAPI(list []*auditrpc.AuditExportTask) []types.AuditExportTaskItem {
	out := make([]types.AuditExportTaskItem, 0, len(list))
	for _, t := range list {
		out = append(out, auditExportToAPI(t))
	}
	return out
}

func auditRetentionPolicyToAPI(p *auditrpc.RetentionPolicy) types.AuditRetentionPolicyItem {
	if p == nil {
		return types.AuditRetentionPolicyItem{}
	}
	return types.AuditRetentionPolicyItem{
		PolicyId:         p.GetPolicyId(),
		ActionDomain:     p.GetActionDomain(),
		HotDays:          p.GetHotDays(),
		ArchiveAfterDays: p.GetArchiveAfterDays(),
		DeleteAfterDays:  p.GetDeleteAfterDays(),
		State:            p.GetState(),
		Version:          p.GetVersion(),
		OperatorId:       p.GetOperatorId(),
		Remark:           p.GetRemark(),
		Ctime:            p.GetCtime(),
		Mtime:            p.GetMtime(),
	}
}

func auditRetentionPoliciesToAPI(list []*auditrpc.RetentionPolicy) []types.AuditRetentionPolicyItem {
	out := make([]types.AuditRetentionPolicyItem, 0, len(list))
	for _, p := range list {
		out = append(out, auditRetentionPolicyToAPI(p))
	}
	return out
}

func auditArchiveBatchToAPI(b *auditrpc.ArchiveBatch) types.AuditArchiveBatchItem {
	if b == nil {
		return types.AuditArchiveBatchItem{}
	}
	return types.AuditArchiveBatchItem{
		BatchId:       b.GetBatchId(),
		RequestId:     b.GetRequestId(),
		ChainKey:      b.GetChainKey(),
		FromSeq:       b.GetFromSeq(),
		ToSeq:         b.GetToSeq(),
		RowCount:      b.GetRowCount(),
		Bucket:        b.GetBucket(),
		ObjectKey:     b.GetObjectKey(),
		ManifestHash:  b.GetManifestHash(),
		LastEntryHash: b.GetLastEntryHash(),
		State:         b.GetState(),
		OperatorId:    b.GetOperatorId(),
		TraceId:       b.GetTraceId(),
		ErrMsg:        b.GetErrMsg(),
		Ctime:         b.GetCtime(),
		FinishedAt:    b.GetFinishedAt(),
	}
}

func auditArchiveBatchesToAPI(list []*auditrpc.ArchiveBatch) []types.AuditArchiveBatchItem {
	out := make([]types.AuditArchiveBatchItem, 0, len(list))
	for _, b := range list {
		out = append(out, auditArchiveBatchToAPI(b))
	}
	return out
}
