// 本文件是 gateway/admin 的手写投影扩展（非 goctl 生成产物）。
//
// spm RPC ↔ 管理后台投影 + 写入口门槛。
//
// 职责边界（AGENTS.md §5/§7），与 conv_recommend.go / conv_cron.go 同一套口径：
//  1. 网关只做三件事：入参形态门槛（主体存在、枚举位不为 UNSPECIFIED、幂等键非空、会话身份存在、
//     数值非负）、调用下游、逐字段投影。窗口左边界如何按粒度规整、口径版本能否使用、
//     事件类型是否在白名单、pn/ps 上限、作业状态机与「这次回填该不该受理」全部由 services/spm 判定，
//     网关不复算，也不把下游错误改写成看起来成功的空结果；
//  2. **不在此实现任何指标计算或推荐决策**：本域没有「把某个 aid 顶到前面」的入参，运营能动的只有
//     口径登记与作业触发；窗口指标写回（WriteMetricWindow）刻意不开后台路由——它是计算链路的专属通道，
//     理由见 admin.api 的 spm 段头注释与 gateway/admin/README.md；
//  3. 0 值哨兵一律原样下传：metric_version=0（当前 ACTIVE 版本）、window_start=0（最近闭合窗口）、
//     window_start_to=0（当前时间）、subject_type=0（全部主体）都是契约里的合法语义，
//     网关把它们换成「具体值」就等于读了一个可能根本没人算过的窗口；
//  4. 后台面**全字段**投影：found/reused/stale/windows_failed/last_error/payload_digest 这些
//     正是排障与审计证据，裁掉就等于让后台靠猜；
//  5. 反向边界是兴趣画像与主体标识：interest_key/subject_id 按契约回在响应体里，
//     但**永不进网关日志**（§7 行为数据脱敏；死信的 payload_preview 同样不重复落日志）；
//  6. 列表一律返回非 nil 切片：把 null 与 [] 区分给前端是多余的契约负担。

package logic

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/types"
	spmrpc "go-video/services/spm/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// spmOperatorPrefix 与 recommendOperatorPrefix 同口径：operator 字段记的是「哪个入口提交的」
// （服务名 + 会话 admin_id），保证口径行与作业行能回溯到人。
const spmOperatorPrefix = "gateway/admin:"

// errSpmServiceNotConfigured：未配置 SpmRPC 时 spm 域路由一律返回它。
// 不退化成空榜单——那会把「下游没接」读成「这条内容没人看」。
var errSpmServiceNotConfigured = errors.New("spm service not configured")

// errSpmRequestMissing：请求体缺失。goctl 生成的 handler 永远传非 nil 指针，
// 该分支只覆盖 logic 被直接复用的场景。
var errSpmRequestMissing = errors.New("gateway/admin: request body required")

// errSpmSessionRequired：受 AdminPermission 保护的路由拿不到会话身份，说明权限表/挂载漂移，
// 一律 fail-closed（个人画像读与所有写路由都在这一档）。
var errSpmSessionRequired = errors.New("gateway/admin: admin session identity required")

// spmOperator 从会话渲染 operator（proto 里有 operator 位的写路由与画像读都用它）。
// 表单**不能**声明操作者：那等于让请求体自己说「我是某个后台账号」。
// 日志只打路由与 admin_id，不带主体、口径与理由正文。
func spmOperator(ctx context.Context, route string) (string, error) {
	id, ok := middleware.AdminFromContext(ctx)
	if !ok {
		return "", errSpmSessionRequired
	}
	if err := requireOperatorID(id.AdminID); err != nil {
		return "", err
	}
	logx.WithContext(ctx).Infof("gateway/admin/%s: admin_id=%d", route, id.AdminID)
	return fmt.Sprintf("%s%d", spmOperatorPrefix, id.AdminID), nil
}

// spmPositive 给「枚举位」设下界：0 = *_UNSPECIFIED 在本域普遍没有语义
// （subject_type/window_type/job_type/state/cohort_type 出现即被服务拒），
// 传 0 只会换来一次「目标非法」的往返。具体哪个值合法、迁移是否允许仍由服务判定。
func spmPositive(field string, v int32) error {
	if v <= 0 {
		return errors.New("gateway/admin: " + field + " required (0 = UNSPECIFIED)")
	}
	return nil
}

// spmPositiveID 给「主体主键」设下界：mid/aid/zone_id 为 0 在契约里都是「未给主体」，
// 而 spm 的每一次读都要有一个确定的被读对象。
func spmPositiveID(field string, v int64) error {
	if v <= 0 {
		return errors.New("gateway/admin: " + field + " required")
	}
	return nil
}

// spmNonNeg 用于窗口起点/时间筛/分页位：0 在本域普遍是「用服务默认」的合法哨兵
// （window_start=0 最近闭合窗口、window_start_to=0 当前时间、since=0 不限时间、ps=0 默认页大小），
// 负数没有任何对应语义，透传只会多一次无意义往返。上限一律由服务夹取或拒绝。
func spmNonNeg(field string, v int64) error {
	if v < 0 {
		return errors.New("gateway/admin: " + field + " must be >= 0")
	}
	return nil
}

// spmPaging 是 pn/ps 两个分页位的合写（pn 从 1 起，ps 允许 0 = 服务默认）。
func spmPaging(pn, ps int32) error {
	if err := spmNonNeg("pn", int64(pn)); err != nil {
		return err
	}
	return spmNonNeg("ps", int64(ps))
}

// spmWindowRange 只挡住「倒着给」的窗口区间：from>to 在服务侧圈不出任何窗口，
// 回的是空作业或空结果 + 一次无谓扫描。区间是否过大由服务判（网关不复制窗口上限）。
func spmWindowRange(from, to int64) error {
	if err := spmNonNeg("window_start_from", from); err != nil {
		return err
	}
	if err := spmNonNeg("window_start_to", to); err != nil {
		return err
	}
	if from > 0 && to > 0 && from > to {
		return errors.New("gateway/admin: window_start_from must be <= window_start_to")
	}
	return nil
}

// spmJobSubject 是 GetAggregationJobReq 的「二选一主体」门槛：job_id=0 且 request_id 为空
// （只含空白也算没给）时下游会按空主键查一条不存在的作业，后台因此看到假线索「作业丢了」。
// 判定用 TrimSpace，透传一律用原值——request_id 是幂等键，改一个字符等于换一次执行权。
func spmJobSubject(jobID int64, requestID string) error {
	if jobID <= 0 && strings.TrimSpace(requestID) == "" {
		return errors.New("gateway/admin: job_id or request_id required")
	}
	return spmNonNeg("job_id", jobID)
}

// --- rpc → 后台 types 投影 ---

func spmMetricPointToAPI(p *spmrpc.MetricPoint) types.SpmMetricPoint {
	if p == nil {
		return types.SpmMetricPoint{}
	}
	return types.SpmMetricPoint{
		MetricKey:     p.GetMetricKey(),
		MetricVersion: p.GetMetricVersion(),
		SubjectType:   int32(p.GetSubjectType()),
		SubjectId:     p.GetSubjectId(),
		WindowType:    int32(p.GetWindowType()),
		WindowStart:   p.GetWindowStart(),
		Value:         p.GetValue(),
		Numerator:     p.GetNumerator(),
		Denominator:   p.GetDenominator(),
		SampleCount:   p.GetSampleCount(),
		EventTime:     p.GetEventTime(),
	}
}

func spmMetricPointsToAPI(list []*spmrpc.MetricPoint) []types.SpmMetricPoint {
	out := make([]types.SpmMetricPoint, 0, len(list))
	for _, e := range list {
		out = append(out, spmMetricPointToAPI(e))
	}
	return out
}

// spmMetricPointsFromMap 把 BatchGetMetricsReply 的 map<string, MetricPoint> 摊平成稳定序列表。
// 排序键取 point 自身三元组（metric_key, metric_version, window_start）而不是 map 的 key 字符串：
// 契约说缺数据的窗口不出现，key 只是索引，值本身才是事实。map 遍历序随机，不排序会让同一请求
// 两次刷新顺序不同，后台会误读成「数据变了」。
func spmMetricPointsFromMap(points map[string]*spmrpc.MetricPoint) []types.SpmMetricPoint {
	out := make([]types.SpmMetricPoint, 0, len(points))
	for _, p := range points {
		out = append(out, spmMetricPointToAPI(p))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MetricKey != out[j].MetricKey {
			return out[i].MetricKey < out[j].MetricKey
		}
		if out[i].MetricVersion != out[j].MetricVersion {
			return out[i].MetricVersion < out[j].MetricVersion
		}
		return out[i].WindowStart < out[j].WindowStart
	})
	return out
}

func spmHotSubjectsToAPI(list []*spmrpc.ListHotSubjectsReply_HotSubject) []types.SpmHotSubject {
	out := make([]types.SpmHotSubject, 0, len(list))
	for _, s := range list {
		out = append(out, types.SpmHotSubject{
			SubjectId:   s.GetSubjectId(),
			Value:       s.GetValue(),
			Numerator:   s.GetNumerator(),
			Denominator: s.GetDenominator(),
			Rank:        s.GetRank(),
		})
	}
	return out
}

func spmInterestsToAPI(list []*spmrpc.GetUserInterestReply_Interest) []types.SpmInterest {
	out := make([]types.SpmInterest, 0, len(list))
	for _, i := range list {
		out = append(out, types.SpmInterest{
			InterestKey: i.GetInterestKey(),
			Weight:      i.GetWeight(),
			SampleCount: i.GetSampleCount(),
			EventTime:   i.GetEventTime(),
		})
	}
	return out
}

func spmRetentionPointsToAPI(list []*spmrpc.GetRetentionReply_RetentionPoint) []types.SpmRetentionPoint {
	out := make([]types.SpmRetentionPoint, 0, len(list))
	for _, p := range list {
		out = append(out, types.SpmRetentionPoint{
			DayOffset:  p.GetDayOffset(),
			CohortSize: p.GetCohortSize(),
			Retained:   p.GetRetained(),
			Rate:       p.GetRate(),
		})
	}
	return out
}

func spmDefinitionToAPI(p *spmrpc.MetricDefinition) types.SpmMetricDefinition {
	if p == nil {
		return types.SpmMetricDefinition{}
	}
	return types.SpmMetricDefinition{
		MetricKey:        p.GetMetricKey(),
		MetricVersion:    p.GetMetricVersion(),
		Name:             p.GetName(),
		Formula:          p.GetFormula(),
		Unit:             p.GetUnit(),
		SupportedWindows: spmWindowTypesToAPI(p.GetSupportedWindows()),
		SourceEventTypes: p.GetSourceEventTypes(),
		State:            int32(p.GetState()),
		Description:      p.GetDescription(),
		CreatedBy:        p.GetCreatedBy(),
		Ctime:            p.GetCtime(),
		Mtime:            p.GetMtime(),
	}
}

func spmDefinitionsToAPI(list []*spmrpc.MetricDefinition) []types.SpmMetricDefinition {
	out := make([]types.SpmMetricDefinition, 0, len(list))
	for _, e := range list {
		out = append(out, spmDefinitionToAPI(e))
	}
	return out
}

// spmDefinitionForRPC 把表单元组装成 rpc.MetricDefinition：created_by/ctime/mtime 三位**不填**，
// 由服务按会话身份与库时钟渲染（网关自报经办人等于伪造审计主体，本地造时间等于造一个假版本）。
func spmDefinitionForRPC(in types.SpmMetricDefinitionInput) *spmrpc.MetricDefinition {
	return &spmrpc.MetricDefinition{
		MetricKey:        in.MetricKey,
		MetricVersion:    in.MetricVersion,
		Name:             in.Name,
		Formula:          in.Formula,
		Unit:             in.Unit,
		SupportedWindows: spmWindowTypesForRPC(in.SupportedWindows),
		SourceEventTypes: in.SourceEventTypes,
		State:            spmrpc.DefinitionState(in.State),
		Description:      in.Description,
	}
}

func spmWindowTypesToAPI(list []spmrpc.WindowType) []int32 {
	out := make([]int32, 0, len(list))
	for _, w := range list {
		out = append(out, int32(w))
	}
	return out
}

func spmWindowTypesForRPC(list []int32) []spmrpc.WindowType {
	out := make([]spmrpc.WindowType, 0, len(list))
	for _, w := range list {
		out = append(out, spmrpc.WindowType(w))
	}
	return out
}

func spmJobToAPI(p *spmrpc.AggregationJob) types.SpmAggregationJob {
	if p == nil {
		return types.SpmAggregationJob{}
	}
	return types.SpmAggregationJob{
		JobId:           p.GetJobId(),
		JobType:         int32(p.GetJobType()),
		State:           int32(p.GetState()),
		SubjectType:     int32(p.GetSubjectType()),
		SubjectId:       p.GetSubjectId(),
		MetricKey:       p.GetMetricKey(),
		MetricVersion:   p.GetMetricVersion(),
		WindowType:      int32(p.GetWindowType()),
		WindowStartFrom: p.GetWindowStartFrom(),
		WindowStartTo:   p.GetWindowStartTo(),
		WindowsTotal:    p.GetWindowsTotal(),
		WindowsDone:     p.GetWindowsDone(),
		WindowsFailed:   p.GetWindowsFailed(),
		RequestId:       p.GetRequestId(),
		Operator:        p.GetOperator(),
		Reason:          p.GetReason(),
		LastError:       p.GetLastError(),
		Ctime:           p.GetCtime(),
		Mtime:           p.GetMtime(),
		FinishedAt:      p.GetFinishedAt(),
	}
}

func spmJobsToAPI(list []*spmrpc.AggregationJob) []types.SpmAggregationJob {
	out := make([]types.SpmAggregationJob, 0, len(list))
	for _, e := range list {
		out = append(out, spmJobToAPI(e))
	}
	return out
}

func spmConsumerRowsToAPI(list []*spmrpc.ListConsumerStateReply_Row) []types.SpmConsumerStateRow {
	out := make([]types.SpmConsumerStateRow, 0, len(list))
	for _, r := range list {
		out = append(out, types.SpmConsumerStateRow{
			Topic:         r.GetTopic(),
			State:         int32(r.GetState()),
			Count:         r.GetCount(),
			OldestCtime:   r.GetOldestCtime(),
			LastMsgOffset: r.GetLastMsgOffset(),
			LastEventTime: r.GetLastEventTime(),
		})
	}
	return out
}

func spmDeadLettersToAPI(list []*spmrpc.ListDeadLettersReply_DeadLetter) []types.SpmDeadLetter {
	out := make([]types.SpmDeadLetter, 0, len(list))
	for _, d := range list {
		out = append(out, types.SpmDeadLetter{
			Id:             d.GetId(),
			EventId:        d.GetEventId(),
			EventType:      d.GetEventType(),
			Topic:          d.GetTopic(),
			PayloadDigest:  d.GetPayloadDigest(),
			PayloadPreview: d.GetPayloadPreview(),
			Reason:         d.GetReason(),
			State:          d.GetState(),
			Ctime:          d.GetCtime(),
		})
	}
	return out
}
