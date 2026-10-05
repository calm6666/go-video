// 本文件是 logic 包的手写扩展（rpc ↔ model 投影），不是 goctl 生成产物。
//
// AGENTS.md §4/§5：领域服务不返回数据库原始对象。投影只搬运事实字段与做枚举/条件的
// 一一映射，业务判定一律留在 logic 用例里。
// 特别注意：AuditEntryView 永不出现明文 IP / 设备号（库里本来也没有这两列，
// 只有 ip_hash / device_hash），所以新增字段时不要顺手把入参原文带出来。

package logic

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"go-video/services/audit/model"
	"go-video/services/audit/rpc"
)

// --- 枚举映射 ---
//
// 三个枚举的取值在 model 与 rpc 两侧刻意保持一致（model/errors.go 的常量注释即写明
// 「取值与 audit.v1.ActorType 一致」），因此投影是直转 + 合法性判定，不是一张 switch 表。
// 这层关系由 projection_test.go 用「反向覆盖率」钉住：proto 新增枚举值时测试必失败。

// knownActorType 判定过滤入参是否是契约里定义过的发起者类型（0 = 不过滤）。
func knownActorType(v rpc.ActorType) (int32, error) {
	n := int32(v)
	if n == 0 {
		return 0, nil
	}
	if _, ok := rpc.ActorType_name[n]; !ok {
		return 0, fmt.Errorf("audit: unknown actor_type %d", n)
	}
	if !model.ValidActorType(n) {
		return 0, fmt.Errorf("audit: actor_type %d 不可用于写入", n)
	}
	return n, nil
}

// knownResult 判定结果过滤值（0 = 不过滤）。
func knownResult(v rpc.AuditResult) (int32, error) {
	n := int32(v)
	if n == 0 {
		return 0, nil
	}
	if _, ok := rpc.AuditResult_name[n]; !ok {
		return 0, fmt.Errorf("audit: unknown result %d", n)
	}
	if !model.ValidResult(n) {
		return 0, fmt.Errorf("audit: result %d 不可用于写入", n)
	}
	return n, nil
}

// knownSourceApp 判定来源端过滤值（0 = 不过滤）。
func knownSourceApp(v rpc.SourceApp) (int32, error) {
	n := int32(v)
	if n == 0 {
		return 0, nil
	}
	if _, ok := rpc.SourceApp_name[n]; !ok {
		return 0, fmt.Errorf("audit: unknown source_app %d", n)
	}
	if !model.ValidSourceApp(n) {
		return 0, fmt.Errorf("audit: source_app %d 不可用于写入", n)
	}
	return n, nil
}

func actorTypeName(v int32) string { return enumName(rpc.ActorType_name, v, "actor_type") }
func resultName(v int32) string    { return enumName(rpc.AuditResult_name, v, "result") }
func sourceAppName(v int32) string { return enumName(rpc.SourceApp_name, v, "source_app") }

func enumName(names map[int32]string, v int32, field string) string {
	if n, ok := names[v]; ok {
		return n
	}
	return "UNDEFINED_" + strconv.FormatInt(int64(v), 10)
}

// --- 条目投影 ---

// entryView 把 audit_entry 行投影为 rpc.AuditEntryView。
func entryView(e *model.AuditEntry) *rpc.AuditEntryView {
	if e == nil {
		return nil
	}
	return &rpc.AuditEntryView{
		EntryId:       e.EntryID,
		EventId:       e.EventID,
		SchemaVersion: e.SchemaVersion,
		ChainKey:      e.ChainKey,
		Seq:           e.Seq,
		ActorType:     rpc.ActorType(e.ActorType),
		ActorId:       e.ActorID,
		ActorName:     e.ActorName,
		Action:        e.Action,
		ActionDomain:  e.ActionDomain,
		TargetType:    e.TargetType,
		TargetId:      e.TargetID,
		Result:        rpc.AuditResult(e.Result),
		BeforeDigest:  e.BeforeDigest,
		AfterDigest:   e.AfterDigest,
		Reason:        e.Reason,
		SourceApp:     rpc.SourceApp(e.SourceApp),
		IpHash:        e.IPHash,
		DeviceHash:    e.DeviceHash,
		TraceId:       e.TraceID,
		RequestId:     e.RequestID,
		OccurredAt:    e.OccurredAt,
		PrevHash:      e.PrevHash,
		EntryHash:     e.EntryHash,
		Ctime:         e.Ctime,
		ArchivedAt:    e.ArchivedAt,
		CallerService: e.CallerService,
	}
}

// entryViews 批量投影，顺序与入参一致（BatchAppendAudit 契约要求逐条同序）。
func entryViews(rows []*model.AuditEntry) []*rpc.AuditEntryView {
	out := make([]*rpc.AuditEntryView, 0, len(rows))
	for _, r := range rows {
		if v := entryView(r); v != nil {
			out = append(out, v)
		}
	}
	return out
}

// --- 查询条件投影 ---

// listFilterOf 把 ListAuditEntries 入参投影为 model.EntryFilter，并回报是否给了收窄维度。
// 枚举非法一律报错而不是当作「不过滤」：把 99 当成 0 会让一次写错的查询静默扩大范围。
func listFilterOf(in *rpc.ListAuditEntriesReq, pn, ps int32) (model.EntryFilter, bool, error) {
	at, err := knownActorType(in.GetActorType())
	if err != nil {
		return model.EntryFilter{}, false, err
	}
	rs, err := knownResult(in.GetResult())
	if err != nil {
		return model.EntryFilter{}, false, err
	}
	sa, err := knownSourceApp(in.GetSourceApp())
	if err != nil {
		return model.EntryFilter{}, false, err
	}
	f := narrowedFilter{
		ActorType:    at,
		ActorID:      in.GetActorId(),
		Action:       strings.TrimSpace(in.GetAction()),
		ActionDomain: strings.TrimSpace(in.GetActionDomain()),
		TargetType:   strings.TrimSpace(in.GetTargetType()),
		TargetID:     strings.TrimSpace(in.GetTargetId()),
		TraceID:      strings.TrimSpace(in.GetTraceId()),
	}
	return model.EntryFilter{
		StartAt:      in.GetStartAt(),
		EndAt:        in.GetEndAt(),
		ActorType:    at,
		ActorID:      in.GetActorId(),
		Action:       f.Action,
		ActionDomain: f.ActionDomain,
		TargetType:   f.TargetType,
		TargetID:     f.TargetID,
		TraceID:      f.TraceID,
		Result:       rs,
		SourceApp:    sa,
		Pn:           pn,
		Ps:           ps,
	}, f.narrowed(), nil
}

// narrowedFilter 是「收窄维度」的判定入参（与 model.EntryFilter 分离，
// 因为 result / source_app 这类低基数列不足以把千万级表扫窄）。
type narrowedFilter struct {
	ActorType    int32
	ActorID      int64
	Action       string
	ActionDomain string
	TargetType   string
	TargetID     string
	TraceID      string
}

// narrowed 实现 README 的硬约束：至少要有一个能定位到少量行的维度。
// target_type 单独给出算收窄（按类型排查是有效维度），但 target_id 单独给出不算
// （不知道类型的裸主键无法走 idx_target 前缀）。
func (f narrowedFilter) narrowed() bool {
	if f.ActorID > 0 || f.Action != "" || f.ActionDomain != "" || f.TraceID != "" {
		return true
	}
	return f.TargetType != ""
}

// filterDims 把查询条件压成自审计摘要的维度映射（原文不进库）。
// 维度取自最终下发给 model 的 filter，而不是取自调用方原始入参：
// 留痕必须记录「实际生效的条件」，否则归一化掉的差异（如枚举 0）就成了盲区。
func filterDims(f model.EntryFilter) map[string]string {
	return map[string]string{
		"start_at":      strconv.FormatInt(f.StartAt, 10),
		"end_at":        strconv.FormatInt(f.EndAt, 10),
		"actor_type":    positiveOrEmpty(int64(f.ActorType)),
		"actor_id":      positiveOrEmpty(f.ActorID),
		"action":        f.Action,
		"action_domain": f.ActionDomain,
		"target_type":   f.TargetType,
		"target_id":     f.TargetID,
		"trace_id":      f.TraceID,
		"result":        positiveOrEmpty(int64(f.Result)),
		"source_app":    positiveOrEmpty(int64(f.SourceApp)),
		"page_size":     positiveOrEmpty(int64(f.Ps)),
	}
}

func positiveOrEmpty(v int64) string {
	if v <= 0 {
		return ""
	}
	return strconv.FormatInt(v, 10)
}

// --- 导出任务投影 ---

// exportTaskView 把 audit_export_task 行投影为 rpc.AuditExportTask。
func exportTaskView(t *model.ExportTask) *rpc.AuditExportTask {
	if t == nil {
		return nil
	}
	return &rpc.AuditExportTask{
		TaskId:        t.TaskID,
		RequestId:     t.RequestID,
		OperatorId:    t.OperatorID,
		CallerService: t.CallerService,
		FilterJson:    t.FilterJSON,
		Format:        t.Format,
		State:         t.State,
		RowCount:      t.RowCount,
		Bucket:        t.Bucket,
		ObjectKey:     t.ObjectKey,
		ObjectSize:    t.ObjectSize,
		ExpireAt:      t.ExpireAt,
		FileHash:      t.FileHash,
		ErrMsg:        t.ErrMsg,
		TraceId:       t.TraceID,
		Ctime:         t.Ctime,
		StartedAt:     t.StartedAt,
		FinishedAt:    t.FinishedAt,
	}
}

func exportTaskViews(rows []*model.ExportTask) []*rpc.AuditExportTask {
	out := make([]*rpc.AuditExportTask, 0, len(rows))
	for _, r := range rows {
		if v := exportTaskView(r); v != nil {
			out = append(out, v)
		}
	}
	return out
}

// --- 归档批次投影 ---

// archiveBatchView 把 audit_archive_batch 行投影为 rpc.ArchiveBatch。
// manifest_hash / last_entry_hash 必须回出：运维靠它核对「已归档段尾哈希」与
// 「热表段首 prev_hash」是否衔接，衔接不上说明中间有段没归档或被截断。
func archiveBatchView(b *model.ArchiveBatch) *rpc.ArchiveBatch {
	if b == nil {
		return nil
	}
	return &rpc.ArchiveBatch{
		BatchId:       b.BatchID,
		RequestId:     b.RequestID,
		ChainKey:      b.ChainKey,
		FromSeq:       b.FromSeq,
		ToSeq:         b.ToSeq,
		RowCount:      b.RowCount,
		Bucket:        b.Bucket,
		ObjectKey:     b.ObjectKey,
		ManifestHash:  b.ManifestHash,
		LastEntryHash: b.LastEntryHash,
		State:         b.State,
		OperatorId:    b.OperatorID,
		TraceId:       b.TraceID,
		ErrMsg:        b.ErrMsg,
		Ctime:         b.Ctime,
		FinishedAt:    b.FinishedAt,
	}
}

func archiveBatchViews(rows []*model.ArchiveBatch) []*rpc.ArchiveBatch {
	out := make([]*rpc.ArchiveBatch, 0, len(rows))
	for _, r := range rows {
		if v := archiveBatchView(r); v != nil {
			out = append(out, v)
		}
	}
	return out
}

// --- 保留策略投影 ---

// retentionView 把 audit_retention_policy 行投影为 rpc.RetentionPolicy。
func retentionView(p *model.RetentionPolicy) *rpc.RetentionPolicy {
	if p == nil {
		return nil
	}
	return &rpc.RetentionPolicy{
		PolicyId:         p.PolicyID,
		ActionDomain:     p.ActionDomain,
		HotDays:          p.HotDays,
		ArchiveAfterDays: p.ArchiveAfterDays,
		DeleteAfterDays:  p.DeleteAfterDays,
		State:            p.State,
		Version:          p.Version,
		OperatorId:       p.Operator,
		Remark:           p.Remark,
		Ctime:            p.Ctime,
		Mtime:            p.Mtime,
	}
}

func retentionViews(rows []*model.RetentionPolicy) []*rpc.RetentionPolicy {
	out := make([]*rpc.RetentionPolicy, 0, len(rows))
	for _, r := range rows {
		if v := retentionView(r); v != nil {
			out = append(out, v)
		}
	}
	return out
}

// --- 导出文件行 ---

// exportRow 是导出文件的一行。字段全集等于 AuditEntryView 的可导出子集：
// 只搬运已脱敏事实（摘要/加盐短哈希），因此导出文件本身不构成新的泄露面。
type exportRow struct {
	EntryID       int64  `json:"entry_id"`
	EventID       string `json:"event_id"`
	SchemaVersion int32  `json:"schema_version"`
	ChainKey      string `json:"chain_key"`
	Seq           int64  `json:"seq"`
	ActorType     int32  `json:"actor_type"`
	ActorTypeName string `json:"actor_type_name"`
	ActorID       int64  `json:"actor_id"`
	ActorName     string `json:"actor_name"`
	Action        string `json:"action"`
	ActionDomain  string `json:"action_domain"`
	TargetType    string `json:"target_type"`
	TargetID      string `json:"target_id"`
	Result        int32  `json:"result"`
	ResultName    string `json:"result_name"`
	BeforeDigest  string `json:"before_digest"`
	AfterDigest   string `json:"after_digest"`
	Reason        string `json:"reason"`
	SourceApp     int32  `json:"source_app"`
	SourceAppName string `json:"source_app_name"`
	IPHash        string `json:"ip_hash"`
	DeviceHash    string `json:"device_hash"`
	TraceID       string `json:"trace_id"`
	RequestID     string `json:"request_id"`
	CallerService string `json:"caller_service"`
	OccurredAt    int64  `json:"occurred_at"`
	PrevHash      string `json:"prev_hash"`
	EntryHash     string `json:"entry_hash"`
	Ctime         int64  `json:"ctime"`
	ArchivedAt    int64  `json:"archived_at"`
}

// exportCSVHeader 与 csvFields 的顺序必须严格一致（导出方按列名解析）。
var exportCSVHeader = []string{
	"entry_id", "event_id", "schema_version", "chain_key", "seq",
	"actor_type", "actor_type_name", "actor_id", "actor_name",
	"action", "action_domain", "target_type", "target_id",
	"result", "result_name", "before_digest", "after_digest", "reason",
	"source_app", "source_app_name", "ip_hash", "device_hash",
	"trace_id", "request_id", "caller_service", "occurred_at",
	"prev_hash", "entry_hash", "ctime", "archived_at",
}

func newExportRow(e *model.AuditEntry) exportRow {
	return exportRow{
		EntryID:       e.EntryID,
		EventID:       e.EventID,
		SchemaVersion: e.SchemaVersion,
		ChainKey:      e.ChainKey,
		Seq:           e.Seq,
		ActorType:     e.ActorType,
		ActorTypeName: actorTypeName(e.ActorType),
		ActorID:       e.ActorID,
		ActorName:     e.ActorName,
		Action:        e.Action,
		ActionDomain:  e.ActionDomain,
		TargetType:    e.TargetType,
		TargetID:      e.TargetID,
		Result:        e.Result,
		ResultName:    resultName(e.Result),
		BeforeDigest:  e.BeforeDigest,
		AfterDigest:   e.AfterDigest,
		Reason:        e.Reason,
		SourceApp:     e.SourceApp,
		SourceAppName: sourceAppName(e.SourceApp),
		IPHash:        e.IPHash,
		DeviceHash:    e.DeviceHash,
		TraceID:       e.TraceID,
		RequestID:     e.RequestID,
		CallerService: e.CallerService,
		OccurredAt:    e.OccurredAt,
		PrevHash:      e.PrevHash,
		EntryHash:     e.EntryHash,
		Ctime:         e.Ctime,
		ArchivedAt:    e.ArchivedAt,
	}
}

func (r exportRow) csvFields() []string {
	return []string{
		strconv.FormatInt(r.EntryID, 10), r.EventID, strconv.FormatInt(int64(r.SchemaVersion), 10),
		r.ChainKey, strconv.FormatInt(r.Seq, 10),
		strconv.FormatInt(int64(r.ActorType), 10), r.ActorTypeName, strconv.FormatInt(r.ActorID, 10), r.ActorName,
		r.Action, r.ActionDomain, r.TargetType, r.TargetID,
		strconv.FormatInt(int64(r.Result), 10), r.ResultName, r.BeforeDigest, r.AfterDigest, r.Reason,
		strconv.FormatInt(int64(r.SourceApp), 10), r.SourceAppName, r.IPHash, r.DeviceHash,
		r.TraceID, r.RequestID, r.CallerService, strconv.FormatInt(r.OccurredAt, 10),
		r.PrevHash, r.EntryHash, strconv.FormatInt(r.Ctime, 10), strconv.FormatInt(r.ArchivedAt, 10),
	}
}

// csvRecord 渲染一行 CSV（含换行）。用 encoding/csv 而不是手拼引号：
// reason 里出现逗号/引号/换行时，手拼一定会写出无法解析的文件。
func csvRecord(fields []string) (string, error) {
	var sb strings.Builder
	w := csv.NewWriter(&sb)
	if err := w.Write(fields); err != nil {
		return "", err
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// csvHeaderBytes 渲染导出文件的表头行（仅 csv 格式有表头）。
func csvHeaderBytes() ([]byte, error) {
	s, err := csvRecord(exportCSVHeader)
	if err != nil {
		return nil, err
	}
	return []byte(s), nil
}

// jsonLine 渲染 JSONL 的一行：一条存证一行，导出侧可以流式追加、
// 下游也能按行切分并发消费，不必为了「整个文件是合法 JSON 数组」而先数行数。
func jsonLine(e *model.AuditEntry) ([]byte, error) {
	b, err := json.Marshal(newExportRow(e))
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// writeExportRows 把一批条目按格式渲染成待写字节（csv 首行带表头，由调用方一次性写）。
func writeExportRows(w io.Writer, format string, rows []*model.AuditEntry) (int64, error) {
	var written int64
	for _, r := range rows {
		var (
			b   []byte
			err error
		)
		if format == "json" {
			b, err = jsonLine(r)
		} else {
			var s string
			s, err = csvRecord(newExportRow(r).csvFields())
			b = []byte(s)
		}
		if err != nil {
			return written, err
		}
		n, err := w.Write(b)
		written += int64(n)
		if err != nil {
			return written, err
		}
	}
	return written, nil
}
