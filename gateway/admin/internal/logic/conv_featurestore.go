// 本文件是 gateway/admin 的手写投影扩展（非 goctl 生成产物）。
//
// feature-store RPC ↔ 管理后台投影 + 写入口门槛。
//
// 职责边界（AGENTS.md §5/§7），与 conv_spm.go / conv_recommend.go 同一套口径：
//  1. 网关只做三件事：入参形态门槛（会话身份存在、幂等键非空、显式版本为正、主体引用齐备、
//     枚举位不为 UNSPECIFIED、数值非负）、调用下游、逐字段投影。特征口径是否自洽、隐私级别与
//     主体维度是否匹配、TTL/维度/默认值是否可用、不可变字段是否冲突、版本能否上线、
//     切换的乐观基线是否命中、作业该不该受理、pn/ps 上限全部由 services/feature-store 判定，
//     网关不复算，也不把下游错误改写成看起来成功的空结果；
//  2. **不在此实现任何特征计算或值写入**：WriteFeatures 与在线热路径读（GetFeature/
//     BatchGetFeatures）刻意不开后台路由——值只能由计算链路写入，排序的读配额不该被排障页消耗，
//     理由见 admin.api 的 feature-store 段头注释与 gateway/admin/README.md；
//  3. 0 值哨兵一律原样下传：version=0（按 ACTIVE 指针解析）、window_to=0（当前时间）、
//     entity_ids 为空（全量扫描）、expected_from_version/from_version=0（不做乐观校验）、
//     min/max_privacy_level=0（不限级别）、limit=0（服务上限）、before=0（当前时间）
//     都是契约里的合法语义，网关把它们换成「具体值」就等于替调用方做了一个它没做的决定；
//  4. 后台面**全字段**投影：found/reused/resolved_version/degradation/cursor_entity_id/
//     last_error 这些正是排障与审计证据，裁掉就等于让后台靠猜；
//  5. 反向边界是个体标识与特征值本身：entity_id 与 value 会回在响应体里（导出/擦除核对就是它的用途），
//     但**永不进网关日志**（§7 行为数据脱敏；服务的 checkEntity 注释同理不回显 entity_id）；
//  6. 列表一律返回非 nil 切片：把 null 与 [] 区分给前端是多余的契约负担。

package logic

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/types"
	featurestorerpc "go-video/services/feature-store/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// featureStoreOperatorPrefix 与 spmOperatorPrefix 同口径：operator 记的是「哪个入口提交的」
// （服务名 + 会话 admin_id），让定义行、切换审计行与作业行能回溯到人。
// 擦除入口特别依赖它：feature-store 侧的 Privacy.OperatorPrefixes 白名单要放行
// `gateway/admin` 前缀，否则本域擦除一律被服务拒绝（空白名单 = 谁都拒绝），网关不代替它放行。
const featureStoreOperatorPrefix = "gateway/admin:"

// errFeatureStoreNotConfigured：未配置 FeatureStoreRPC 时本域路由一律返回它。
// 不退化成空目录——那会把「下游没接」读成「这个环境一个特征都没注册」。
var errFeatureStoreNotConfigured = errors.New("gateway/admin: feature-store service client not configured")

// errFsRequestMissing：请求体缺失。goctl 生成的 handler 永远传非 nil 指针，
// 该分支只覆盖 logic 被直接复用的场景。
var errFsRequestMissing = errors.New("gateway/admin: request body required")

// errFsSessionRequired：受 AdminPermission 保护的路由拿不到会话身份，说明权限表/挂载漂移，
// 一律 fail-closed（定向个人导出与所有写路由都在这一档）。
var errFsSessionRequired = errors.New("gateway/admin: admin session identity required")

// fsOperator 从会话渲染 operator（表单里没有 operator 位，也不允许自报：
// 那等于让请求体自己说「我是某个后台账号」）。日志只打路由与 admin_id。
func fsOperator(ctx context.Context, route string) (string, error) {
	id, ok := middleware.AdminFromContext(ctx)
	if !ok {
		return "", errFsSessionRequired
	}
	if err := requireOperatorID(id.AdminID); err != nil {
		return "", err
	}
	logx.WithContext(ctx).Infof("gateway/admin/%s: admin_id=%d", route, id.AdminID)
	return fmt.Sprintf("%s%d", featureStoreOperatorPrefix, id.AdminID), nil
}

// fsPositive 给「枚举位与显式版本」设下界：0 = *_UNSPECIFIED 在本域普遍没有语义
// （entity_scope/value_type/source/privacy_level/state/backfill state 出现即被服务拒），
// 需要确切版本的写入口（state/privacy/switch/backfill 的 version）也不接受 0——
// 0 在那里是「按 ACTIVE 指针」，悄悄按指针落版本等于把改动落在一个调用方没选过的版本上。
// 具体哪个值合法、迁移是否允许仍由服务判定。
func fsPositive(field string, v int32) error {
	if v <= 0 {
		return errors.New("gateway/admin: " + field + " required (0 = UNSPECIFIED)")
	}
	return nil
}

// fsNonNeg 用于时间戳、窗口端点与批量位：0 在本域普遍是「用服务默认」的合法哨兵
// （window_to=0 当前时间、before=0 当前时间、limit=0 服务上限、since=0 不限时间），
// 负数没有任何对应语义，透传只会多一次无意义往返。上限与超限行为一律由服务判定。
func fsNonNeg(field string, v int64) error {
	if v < 0 {
		return errors.New("gateway/admin: " + field + " must be >= 0")
	}
	return nil
}

// fsPaging 是 pn/ps 两个分页位的门槛。与 spm 域的差别要写清楚：本服务契约里
// ps **没有**「0 = 默认页大小」的语义（model.ValidatePageSize 判 1..100，0 直接拒），
// 因此 0 也挡在这里——这不是复算领域规则，而是「必填位没给」；
// 上限 100 与超限是否夹取仍由服务判（网关不夹取也不改写）。
func fsPaging(pn, ps int32) error {
	if pn < 1 {
		return errors.New("gateway/admin: pn must start from 1")
	}
	if ps < 1 {
		return errors.New("gateway/admin: ps required (no server default page size in this contract)")
	}
	return nil
}

// fsWindowRange 只挡住「倒着给」的回填区间：from>to 在服务侧圈不出任何数据，
// 回的是空作业或空结果 + 一次无谓扫描。区间是否过大、before 是否晚于服务时钟由服务判
// （网关不拿自己的钟去替服务判「现在几点」）。
func fsWindowRange(from, to int64) error {
	if err := fsNonNeg("window_from", from); err != nil {
		return err
	}
	if err := fsNonNeg("window_to", to); err != nil {
		return err
	}
	if from > 0 && to > 0 && from > to {
		return errors.New("gateway/admin: window_from must be <= window_to")
	}
	return nil
}

// fsJobSubject 是 GetBackfillJobReq 的「二选一主体」门槛：job_id=0 且 request_id 为空
// （只含空白也算没给）时下游会按空主键查一条不存在的作业，后台因此看到假线索「作业丢了」。
// 判定用 TrimSpace，透传一律用原值——request_id 是幂等键，改一个字符等于换一次执行权。
func fsJobSubject(jobID int64, requestID string) error {
	if jobID <= 0 && strings.TrimSpace(requestID) == "" {
		return errors.New("gateway/admin: job_id or request_id required")
	}
	return fsNonNeg("job_id", jobID)
}

// fsEntity 组装主体引用并挡掉两个不可能形状：scope 缺失（0）与 entity_id 空白。
// entity_id 的**形态**一律不在此判：DEVICE/IP_HASH 只接受十六进制摘要、MID/AID 只接受正十进制串、
// 搜索词不得夹带空白与分隔符，那是 model.ValidEntityID 的隐私第一道闸（它同时保证
// 明文设备号与原始 IP 进不了 SQL），网关若先按十进制裁一遍就把哈希维度写错了。
// 透传用原值（TrimSpace 只用于判空），哈希摘要是大小写敏感的十六进制串。
func fsEntity(scope int32, id string) (*featurestorerpc.EntityRef, error) {
	if err := fsPositive("entity_scope", scope); err != nil {
		return nil, err
	}
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("gateway/admin: entity_id required")
	}
	return &featurestorerpc.EntityRef{
		EntityScope: featurestorerpc.EntityScope(scope),
		EntityId:    id,
	}, nil
}

// fsPrivacyFilter 用于 min/max_privacy_level 两个过滤位：0 = 不限是合法哨兵，
// 只有负数没有语义。级别是否声明、与 scope 是否自洽由服务判。
func fsPrivacyFilter(field string, v int32) error {
	return fsNonNeg(field, int64(v))
}

// fsInt32Range 挡住「表单里写了 int64、rpc 字段是 int32」的截断：后台 HTTP 用 int64 收，
// 服务契约用 int32（PurgeExpiredReq.limit）。直接 int32(v) 会把一个超范围的数截成别的数——
// 负数或 0 在 limit 位上恰好是「按服务端上限清理一整批」的哨兵语义，
// 于是「填了一个大到没意义的批大小」会静默变成「清到上限」，那是数据面后果而不是取值偏差。
// 这里只判「这个数在 int32 里存不存在」，真正的批大小上限与超限夹取仍由服务判定。
func fsInt32Range(field string, v int64) error {
	if v > math.MaxInt32 {
		return errors.New("gateway/admin: " + field + " out of int32 range")
	}
	return nil
}

// --- rpc → 后台 types 投影 ---

func fsDefinitionToAPI(d *featurestorerpc.FeatureDefinition) types.FsFeatureDefinition {
	if d == nil {
		return types.FsFeatureDefinition{}
	}
	return types.FsFeatureDefinition{
		FeatureKey:    d.GetFeatureKey(),
		Version:       d.GetVersion(),
		Name:          d.GetName(),
		ValueType:     int32(d.GetValueType()),
		EntityScope:   int32(d.GetEntityScope()),
		Source:        int32(d.GetSource()),
		PrivacyLevel:  int32(d.GetPrivacyLevel()),
		WindowSeconds: d.GetWindowSeconds(),
		TTLSeconds:    d.GetTtlSeconds(),
		DefaultValue:  d.GetDefaultValue(),
		Dimension:     d.GetDimension(),
		State:         int32(d.GetState()),
		Description:   d.GetDescription(),
		ChangeNote:    d.GetChangeNote(),
		CreatedBy:     d.GetCreatedBy(),
		Ctime:         d.GetCtime(),
		Mtime:         d.GetMtime(),
	}
}

func fsDefinitionsToAPI(list []*featurestorerpc.FeatureDefinition) []types.FsFeatureDefinition {
	out := make([]types.FsFeatureDefinition, 0, len(list))
	for _, d := range list {
		out = append(out, fsDefinitionToAPI(d))
	}
	return out
}

// fsDefinitionForRPC 把表单元组装成 rpc.FeatureDefinition：
// state/created_by/ctime/mtime 四位**不填**——注册一律以 DRAFT 入库、经办人与库时钟由服务渲染
// （网关自报经办人等于伪造审计主体，本地造时间等于造一个假版本）。
// 零值原样带过（window_seconds=0 是「无窗口的静态属性」，不是「未填」）。
func fsDefinitionForRPC(in types.FsFeatureDefinitionInput) *featurestorerpc.FeatureDefinition {
	return &featurestorerpc.FeatureDefinition{
		FeatureKey:    in.FeatureKey,
		Version:       in.Version,
		Name:          in.Name,
		ValueType:     featurestorerpc.FeatureValueType(in.ValueType),
		EntityScope:   featurestorerpc.EntityScope(in.EntityScope),
		Source:        featurestorerpc.FeatureSource(in.Source),
		PrivacyLevel:  featurestorerpc.PrivacyLevel(in.PrivacyLevel),
		WindowSeconds: in.WindowSeconds,
		TtlSeconds:    in.TTLSeconds,
		DefaultValue:  in.DefaultValue,
		Dimension:     in.Dimension,
		Description:   in.Description,
		ChangeNote:    in.ChangeNote,
	}
}

// fsFeatureValueToAPI 按 value_type 原样搬运全部取值位（含列表）：
// 网关不代为挑选「哪一位才是真的」，多余填充是服务的 payload 校验范围。
func fsFeatureValueToAPI(v *featurestorerpc.FeatureValue) types.FsFeatureValue {
	if v == nil {
		// 无值也回非 nil 列表：整行零值时后台拿到的仍是 []，而不是需要判 null 的字段。
		return types.FsFeatureValue{Int64List: []int64{}, DoubleList: []float64{}}
	}
	out := types.FsFeatureValue{
		ValueType:   int32(v.GetValueType()),
		Int64Value:  v.GetInt64Value(),
		DoubleValue: v.GetDoubleValue(),
		BoolValue:   v.GetBoolValue(),
		StringValue: v.GetStringValue(),
	}
	// 列表位一律回非 nil（与其它列表同口径），长度按服务原值，不补零也不裁剪。
	out.Int64List = make([]int64, 0, len(v.GetInt64List()))
	out.Int64List = append(out.Int64List, v.GetInt64List()...)
	out.DoubleList = make([]float64, 0, len(v.GetDoubleList()))
	out.DoubleList = append(out.DoubleList, v.GetDoubleList()...)
	return out
}

func fsEntryToAPI(e *featurestorerpc.FeatureEntry) types.FsFeatureEntry {
	if e == nil {
		return types.FsFeatureEntry{Value: fsFeatureValueToAPI(nil)}
	}
	return types.FsFeatureEntry{
		FeatureKey:     e.GetFeature().GetFeatureKey(),
		FeatureVersion: e.GetFeature().GetVersion(),
		EntityScope:    int32(e.GetEntity().GetEntityScope()),
		EntityId:       e.GetEntity().GetEntityId(),
		Value:          fsFeatureValueToAPI(e.GetValue()),
		// resolved_version/degradation 是「服务实际给了什么、为什么给」，一个都不省。
		ResolvedVersion: e.GetResolvedVersion(),
		Degradation:     int32(e.GetDegradation()),
		EventTime:       e.GetEventTime(),
		ExpireAt:        e.GetExpireAt(),
		SourceMetricKey: e.GetSourceMetricKey(),
		TTLSeconds:      e.GetTtlSeconds(),
	}
}

func fsEntriesToAPI(list []*featurestorerpc.FeatureEntry) []types.FsFeatureEntry {
	out := make([]types.FsFeatureEntry, 0, len(list))
	for _, e := range list {
		out = append(out, fsEntryToAPI(e))
	}
	return out
}

func fsSwitchRecordsToAPI(list []*featurestorerpc.ListVersionSwitchesReply_SwitchRecord) []types.FsVersionSwitchRecord {
	out := make([]types.FsVersionSwitchRecord, 0, len(list))
	for _, r := range list {
		out = append(out, types.FsVersionSwitchRecord{
			SwitchId:    r.GetSwitchId(),
			FeatureKey:  r.GetFeatureKey(),
			FromVersion: r.GetFromVersion(),
			ToVersion:   r.GetToVersion(),
			Operator:    r.GetOperator(),
			Reason:      r.GetReason(),
			RequestId:   r.GetRequestId(),
			Ctime:       r.GetCtime(),
		})
	}
	return out
}

func fsJobToAPI(j *featurestorerpc.BackfillJob) types.FsBackfillJob {
	if j == nil {
		return types.FsBackfillJob{}
	}
	return types.FsBackfillJob{
		JobId:          j.GetJobId(),
		FeatureKey:     j.GetFeatureKey(),
		Version:        j.GetVersion(),
		EntityScope:    int32(j.GetEntityScope()),
		Source:         int32(j.GetSource()),
		State:          int32(j.GetState()),
		WindowFrom:     j.GetWindowFrom(),
		WindowTo:       j.GetWindowTo(),
		EntitiesTotal:  j.GetEntitiesTotal(),
		EntitiesDone:   j.GetEntitiesDone(),
		EntitiesFailed: j.GetEntitiesFailed(),
		CursorEntityId: j.GetCursorEntityId(),
		AutoSwitch:     j.GetAutoSwitch(),
		RequestId:      j.GetRequestId(),
		Operator:       j.GetOperator(),
		Reason:         j.GetReason(),
		LastError:      j.GetLastError(),
		Ctime:          j.GetCtime(),
		Mtime:          j.GetMtime(),
		FinishedAt:     j.GetFinishedAt(),
	}
}

func fsJobsToAPI(list []*featurestorerpc.BackfillJob) []types.FsBackfillJob {
	out := make([]types.FsBackfillJob, 0, len(list))
	for _, j := range list {
		out = append(out, fsJobToAPI(j))
	}
	return out
}
