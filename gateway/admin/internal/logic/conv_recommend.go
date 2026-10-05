// 本文件是 gateway/admin 的手写投影扩展（非 goctl 生成产物）。
//
// recommend-recall / recommend-rank RPC ↔ 管理后台投影 + 写入口门槛。
//
// 职责边界（AGENTS.md §4/§5/§7），与 conv_cron.go / conv_live.go 同一套口径：
//  1. 网关只做三件事：入参形态门槛（池寻址存在、幂等键非空、会话身份存在、数值非负）、调用下游、
//     逐字段投影。池键语法（model.ValidatePoolKey）、池版本状态机、版本可否回滚、keep_versions 下限、
//     模型/特征/实验的取值与状态迁移合法性全部由 services/recommend-recall、services/recommend-rank
//     判定，网关不复算，也不把下游错误改写成看起来成功的空结果；
//  2. **不在此实现任何推荐决策**：两个契约都没有「把某个 aid 顶到前面 / 加权 / 屏蔽」的入参
//     （recall.proto 与 rank.proto 的头注释即如此），运营能动的只有池数据与模型/特征/实验的登记；
//     在线热路径（RecallCandidates/RankCandidates）与按主体的分桶查询（GetExperimentAssignment）
//     刻意不开后台路由，理由逐条见 gateway/admin/README.md；
//  3. 后台面下发**全字段**投影：batch_id/generator/schema_version/operator/note/versions_digest/
//     result_digest/top_aids/过滤计数/降级原因这些正是排障与审计证据，裁掉就等于让后台靠猜。
//     反向边界是 subject_id（mid 字符串或设备 sha256 摘要）：它按契约回在响应体里，
//     但**永不进网关日志**（AGENTS.md §7 行为数据脱敏）；
//  4. 列表一律返回非 nil 切片：把 null 与 [] 区分给前端是多余的契约负担。

package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/types"
	rankrpc "go-video/services/recommend-rank/rpc"
	recallrpc "go-video/services/recommend-recall/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// recommendOperatorPrefix 与 cronOperatorPrefix 同口径：写进 operator 字段的身份前缀是
// 「哪个入口提交的」（服务名，不是实例地址），后面接会话 admin_id，保证版本行/实验行能回溯到人。
const recommendOperatorPrefix = "gateway/admin:"

// errRecallServiceNotConfigured：未配置 RecommendRecallRPC 时 recall 域路由一律返回它。
// 不退化成伪造空池——那会让后台把「下游没接」读成「池是空的」，进而误判推荐结果正常。
var errRecallServiceNotConfigured = errors.New("recommend-recall service not configured")

// errRankServiceNotConfigured：同上，rank 域。
var errRankServiceNotConfigured = errors.New("recommend-rank service not configured")

// errRecommendRequestMissing：请求体缺失。goctl 生成的 handler 永远传非 nil 指针，
// 该分支只覆盖 logic 被直接复用的场景。
var errRecommendRequestMissing = errors.New("gateway/admin: request body required")

// errRecommendSessionRequired：受 AdminPermission 保护的写路由拿不到会话身份。
// 此时说明这条路由没被中间件保护（权限表/挂载漂移），一律 fail-closed。
var errRecommendSessionRequired = errors.New("gateway/admin: admin session identity required")

// errRecommendPoolRefRequired 池寻址缺一半就没法定位：source=0（UNSPECIFIED）与空 pool_key
// 在服务侧分别落 ErrInvalidPoolKey 和「查无此池」，网关先给出可定位参数的错误消息。
var errRecommendPoolRefRequired = errors.New("gateway/admin: pool source and pool_key required")

// errRecallLogSubjectRequired GetRecallRequestLogReq 的 request_id / snapshot_id 二选一：
// 两个都空时下游会按空主键查一条不存在的日志，回给后台的是「未命中」而不是「你少传了参数」。
var errRecallLogSubjectRequired = errors.New("gateway/admin: request_id or snapshot_id required")

// errRankDecisionSubjectRequired 同上：GetRankDecisionReq 至少要给 decision_id 或 request_id。
var errRankDecisionSubjectRequired = errors.New("gateway/admin: decision_id or request_id required")

// recommendOperator 从会话渲染 operator（proto 里有 operator 位的写路由都用它）。
// 表单**不能**声明操作者：那等于让请求体自己说「我是某个后台账号」。
// 日志只打路由与 admin_id：reason 正文、模型工件引用、subject_id 等都不进日志。
func recommendOperator(ctx context.Context, route string) (string, error) {
	id, ok := middleware.AdminFromContext(ctx)
	if !ok {
		return "", errRecommendSessionRequired
	}
	if err := requireOperatorID(id.AdminID); err != nil {
		return "", err
	}
	logx.WithContext(ctx).Infof("gateway/admin/%s: admin_id=%d", route, id.AdminID)
	return fmt.Sprintf("%s%d", recommendOperatorPrefix, id.AdminID), nil
}

// recommendSessionGate 用于 proto 里**没有** operator 位的写路由（UpsertPoolItems、PrunePoolVersions）：
// 会话身份照样必须是正的并留痕，只是没有可传给服务的操作者字段（缺口见 admin.api 注释与 README）。
func recommendSessionGate(ctx context.Context, route string) error {
	_, err := recommendOperator(ctx, route)
	return err
}

// recommendPoolRef 组装池寻址并做形态门槛：source 必须是正的枚举值、pool_key 非空。
// 键的**语法**与「这个组合是否被允许」（SOURCE_HOT 只接受 global / zone:<typeid> 等）由
// recommend-recall 判定，网关不解析 pool_key、不替它拼键。
// pool_key 只 TrimSpace 判空、不改写原值：截断或归一化会改变键本身。
func recommendPoolRef(source int32, poolKey string) (*recallrpc.PoolRef, error) {
	if source <= 0 || strings.TrimSpace(poolKey) == "" {
		return nil, errRecommendPoolRefRequired
	}
	return &recallrpc.PoolRef{Source: recallrpc.Source(source), PoolKey: poolKey}, nil
}

// recommendAtLeastOne 是「二选一主体」门槛：两个键都为空（只含空白也算）时没有任何可查目标，
// 下游会按空主键回「未命中」，后台因此看到的是假线索「这条记录不存在」。
// 判定用 TrimSpace，透传一律用原值——幂等/回放键的任何改写都会让它失去语义。
func recommendAtLeastOne(a, b string) bool {
	return strings.TrimSpace(a) != "" || strings.TrimSpace(b) != ""
}

// recommendNonNeg 传输层门槛：0 在本域普遍是「用服务默认值」的合法哨兵
// （version=0 读 CURRENT、ps=0 用默认页大小、max_rows=0 由服务取批量上限、mid=0 游客），
// 负数没有任何对应语义，透传只会换来一次无意义的往返。
// 上限（MaxPoolSnapshotPage/MaxVersionList/MaxRequestLogPage/MaxDecisionPage/MaxBatchItems/
// MinKeepVersions）一律由服务夹取或拒绝，网关不复制这些数字。
func recommendNonNeg(field string, v int64) error {
	if v < 0 {
		return errors.New("gateway/admin: " + field + " must be >= 0")
	}
	return nil
}

// recommendPositive 给「枚举目标位」设下界：0 = *_UNSPECIFIED，在两个服务里都没有对应语义
// （SetModelVersionStateReq 只接受 READY/ACTIVE/RETIRED、SetExperimentStateReq 只接受
// RUNNING/PAUSED/STOPPED），传 0 只会换来一次「目标状态非法」的往返。
// 具体哪个值合法、迁移是否允许仍由服务判定，网关不复算状态机。
func recommendPositive(field string, v int32) error {
	if v <= 0 {
		return errors.New("gateway/admin: " + field + " required (0 = UNSPECIFIED)")
	}
	return nil
}

// recommendPaging 是 pn/ps 两个分页位的合写（pn 从 1 起，ps 允许 0 = 服务默认）。
func recommendPaging(pn, ps int32) error {
	if pn < 0 {
		return errors.New("gateway/admin: pn must be >= 0")
	}
	return recommendNonNeg("ps", int64(ps))
}

// recommendTimeWindow 只挡住「倒着给」的时间窗：from>to 在两个服务里都查不到任何行，
// 但回的是空列表 + 一次无谓的全表条件扫描。窗口是否过大、跨天边界如何由服务判定。
func recommendTimeWindow(fromTime, toTime int64) error {
	if fromTime < 0 || toTime < 0 {
		return errors.New("gateway/admin: from_time and to_time must be >= 0")
	}
	if fromTime > 0 && toTime > 0 && fromTime > toTime {
		return errors.New("gateway/admin: from_time must be <= to_time")
	}
	return nil
}

// --- recall rpc → 后台 types 投影 ---

func recallPoolRefToAPI(p *recallrpc.PoolRef) types.RecommendPoolRef {
	if p == nil {
		return types.RecommendPoolRef{}
	}
	return types.RecommendPoolRef{Source: int32(p.GetSource()), PoolKey: p.GetPoolKey()}
}

// recallPoolEcho 回显池寻址：优先用服务回传的 pool（它才知道最终读的是哪个池、有没有做别名解析），
// 服务未回（nil）时退回请求入参，避免后台看到 source=0/pool_key="" 这种「查了但不知道查了谁」的空寻址。
// 这不是补数据：请求本身就是「我要读哪个池」的唯一事实来源。
func recallPoolEcho(p *recallrpc.PoolRef, requested types.RecommendPoolRef) types.RecommendPoolRef {
	if p == nil {
		return requested
	}
	return recallPoolRefToAPI(p)
}

func recallSourcesToAPI(list []recallrpc.Source) []int32 {
	out := make([]int32, 0, len(list))
	for _, s := range list {
		out = append(out, int32(s))
	}
	return out
}

func recallInt64s(list []int64) []int64 {
	out := make([]int64, 0, len(list))
	out = append(out, list...)
	return out
}

func recallPoolItemsToAPI(list []*recallrpc.PoolItem) []types.RecommendPoolItem {
	out := make([]types.RecommendPoolItem, 0, len(list))
	for _, i := range list {
		out = append(out, types.RecommendPoolItem{
			Aid:     i.GetAid(),
			Score:   i.GetScore(),
			Version: i.GetVersion(),
			Ctime:   i.GetCtime(),
		})
	}
	return out
}

func recallPoolItemsForRPC(list []types.RecommendPoolItemInput) []*recallrpc.PoolItemInput {
	out := make([]*recallrpc.PoolItemInput, 0, len(list))
	for _, i := range list {
		out = append(out, &recallrpc.PoolItemInput{Aid: i.Aid, Score: i.Score})
	}
	return out
}

func recallPoolVersionsToAPI(list []*recallrpc.PoolVersionInfo) []types.RecommendPoolVersionInfo {
	out := make([]types.RecommendPoolVersionInfo, 0, len(list))
	for _, v := range list {
		out = append(out, types.RecommendPoolVersionInfo{
			Pool:          recallPoolRefToAPI(v.GetPool()),
			Version:       v.GetVersion(),
			BatchId:       v.GetBatchId(),
			Generator:     v.GetGenerator(),
			SchemaVersion: v.GetSchemaVersion(),
			ItemCount:     v.GetItemCount(),
			State:         int32(v.GetState()),
			PublishedAt:   v.GetPublishedAt(),
			Operator:      v.GetOperator(),
			Note:          v.GetNote(),
			Ctime:         v.GetCtime(),
			Mtime:         v.GetMtime(),
		})
	}
	return out
}

func recallSourceStatsToAPI(list []*recallrpc.SourceStat) []types.RecommendSourceStat {
	out := make([]types.RecommendSourceStat, 0, len(list))
	for _, s := range list {
		out = append(out, types.RecommendSourceStat{
			Source:      int32(s.GetSource()),
			Planned:     s.GetPlanned(),
			Returned:    s.GetReturned(),
			PoolVersion: s.GetPoolVersion(),
			BatchId:     s.GetBatchId(),
			Degraded:    s.GetDegraded(),
			ErrorCode:   s.GetErrorCode(),
		})
	}
	return out
}

func recallRequestLogToAPI(p *recallrpc.RecallRequestLogInfo) types.RecommendRequestLog {
	if p == nil {
		return types.RecommendRequestLog{}
	}
	return types.RecommendRequestLog{
		RequestId:        p.GetRequestId(),
		SnapshotId:       p.GetSnapshotId(),
		Mid:              p.GetMid(),
		Scene:            p.GetScene(),
		Platform:         int32(p.GetPlatform()),
		AppVersion:       p.GetAppVersion(),
		Region:           p.GetRegion(),
		RequestedSources: recallSourcesToAPI(p.GetRequestedSources()),
		PerSource:        recallSourceStatsToAPI(p.GetPerSource()),
		CandidateCount:   p.GetCandidateCount(),
		ReturnedCount:    p.GetReturnedCount(),
		Degraded:         p.GetDegraded(),
		Reason:           int32(p.GetReason()),
		CostMs:           p.GetCostMs(),
		VersionsDigest:   p.GetVersionsDigest(),
		TraceId:          p.GetTraceId(),
		Ctime:            p.GetCtime(),
	}
}

func recallRequestLogsToAPI(list []*recallrpc.RecallRequestLogInfo) []types.RecommendRequestLog {
	out := make([]types.RecommendRequestLog, 0, len(list))
	for _, e := range list {
		out = append(out, recallRequestLogToAPI(e))
	}
	return out
}

func recallPoolStatusesToAPI(list []*recallrpc.PoolStatus) []types.RecommendPoolStatus {
	out := make([]types.RecommendPoolStatus, 0, len(list))
	for _, s := range list {
		out = append(out, types.RecommendPoolStatus{
			Pool:           recallPoolRefToAPI(s.GetPool()),
			CurrentVersion: s.GetCurrentVersion(),
			BatchId:        s.GetBatchId(),
			ItemCount:      s.GetItemCount(),
			PublishedAt:    s.GetPublishedAt(),
			Stale:          s.GetStale(),
		})
	}
	return out
}

// recallSwitchData 把 Publish/Rollback 两种同形回包投影成同一个后台结构：
// switched=false + deduplicated=true 是幂等重放的正常结论，网关不把它折叠成错误。
func recallSwitchData(switched bool, previous, current int64, deduplicated bool, eventID string) types.RecommendPoolVersionSwitchData {
	return types.RecommendPoolVersionSwitchData{
		Switched:        switched,
		PreviousVersion: previous,
		CurrentVersion:  current,
		Deduplicated:    deduplicated,
		EventId:         eventID,
	}
}

// --- rank rpc → 后台 types 投影 ---

func rankFilterStatToAPI(p *rankrpc.FilterStat) types.RankFilterStat {
	if p == nil {
		return types.RankFilterStat{}
	}
	return types.RankFilterStat{
		SafetyFiltered:    p.GetSafetyFiltered(),
		FrequencyFiltered: p.GetFrequencyFiltered(),
		DedupFiltered:     p.GetDedupFiltered(),
		DiversifiedMoved:  p.GetDiversifiedMoved(),
		Truncated:         p.GetTruncated(),
	}
}

func rankDecisionToAPI(p *rankrpc.RankDecisionInfo) types.RankDecisionInfo {
	if p == nil {
		return types.RankDecisionInfo{}
	}
	return types.RankDecisionInfo{
		DecisionId:           p.GetDecisionId(),
		RequestId:            p.GetRequestId(),
		TraceId:              p.GetTraceId(),
		SnapshotId:           p.GetSnapshotId(),
		Mid:                  p.GetMid(),
		SubjectType:          int32(p.GetSubjectType()),
		SubjectId:            p.GetSubjectId(),
		Scene:                p.GetScene(),
		Platform:             int32(p.GetPlatform()),
		AppVersion:           p.GetAppVersion(),
		ExpKey:               p.GetExpKey(),
		VariantKey:           p.GetVariantKey(),
		BucketNo:             p.GetBucketNo(),
		ModelKey:             p.GetModelKey(),
		ModelVersion:         p.GetModelVersion(),
		FeatureConfigVersion: p.GetFeatureConfigVersion(),
		InputCount:           p.GetInputCount(),
		ReturnedCount:        p.GetReturnedCount(),
		ResultDigest:         p.GetResultDigest(),
		TopAids:              recallInt64s(p.GetTopAids()),
		Degraded:             p.GetDegraded(),
		Reason:               int32(p.GetReason()),
		Fallback:             int32(p.GetFallback()),
		Filters:              rankFilterStatToAPI(p.GetFilters()),
		CostMs:               p.GetCostMs(),
		Ctime:                p.GetCtime(),
	}
}

func rankDecisionsToAPI(list []*rankrpc.RankDecisionInfo) []types.RankDecisionInfo {
	out := make([]types.RankDecisionInfo, 0, len(list))
	for _, e := range list {
		out = append(out, rankDecisionToAPI(e))
	}
	return out
}

func rankExperimentToAPI(p *rankrpc.ExperimentInfo) types.RankExperimentInfo {
	if p == nil {
		return types.RankExperimentInfo{}
	}
	return types.RankExperimentInfo{
		ExpKey:               p.GetExpKey(),
		VariantKey:           p.GetVariantKey(),
		LayerKey:             p.GetLayerKey(),
		BucketStart:          p.GetBucketStart(),
		BucketEnd:            p.GetBucketEnd(),
		ModelKey:             p.GetModelKey(),
		ModelVersion:         p.GetModelVersion(),
		FeatureConfigVersion: p.GetFeatureConfigVersion(),
		Overrides:            p.GetOverrides(),
		State:                int32(p.GetState()),
		Revision:             p.GetRevision(),
		StartAt:              p.GetStartAt(),
		EndAt:                p.GetEndAt(),
		Operator:             p.GetOperator(),
		Reason:               p.GetReason(),
		Ctime:                p.GetCtime(),
		Mtime:                p.GetMtime(),
	}
}

func rankExperimentsToAPI(list []*rankrpc.ExperimentInfo) []types.RankExperimentInfo {
	out := make([]types.RankExperimentInfo, 0, len(list))
	for _, e := range list {
		out = append(out, rankExperimentToAPI(e))
	}
	return out
}

func rankObjectiveWeightsForRPC(list []types.RankObjectiveWeight) []*rankrpc.ObjectiveWeight {
	out := make([]*rankrpc.ObjectiveWeight, 0, len(list))
	for _, w := range list {
		out = append(out, &rankrpc.ObjectiveWeight{Objective: w.Objective, Weight: w.Weight})
	}
	return out
}

// rankStrings 保证 repeated string 投影出来是非 nil 切片（空清单也要回 []，不能回 null）。
func rankStrings(list []string) []string {
	out := make([]string, 0, len(list))
	out = append(out, list...)
	return out
}
