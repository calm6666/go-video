// 手写文件（不属于 goctl 生成产物）：/admin/membership 十条路由共用的身份渲染、
// 传输层门槛与 RPC→后台投影。
//
// 放在这里而不是各 logic 里重复一遍（AGENTS.md §4/§5）：
//   - operator 只能来自会话身份，且要满足 membership 侧 operator 列宽；
//   - 投影逐字段不丢，网关不发明判定：档位、时长、价格区间、币种、状态机迁移、
//     幂等指纹与「来源必须能回溯到支付流水」全部由 services/membership 判定；
//   - 分页/时间窗只挡形状问题（负数、from>to），超限裁剪由服务做并回显实际值，
//     网关照抄 reply 里的 page/size，绝不本地按自己的上限重算一遍。

package logic

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/types"
	membershiprpc "go-video/services/membership/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// membershipOperatorPrefix 与 cron/collector/recommend 同口径：operator 字段的前缀说明
// 「哪个入口提交的」（服务名，不是实例地址），后面接会话 admin_id，让 mb_grant /
// mb_plan_change_log 能回溯到人。
const membershipOperatorPrefix = "gateway/admin:"

// membershipMaxOperatorLength 是 mb_grant.operator / mb_plan_change_log.operator 的列宽
// （services/membership/model.MaxOperatorLength，按字符计）。
// 前缀 14 + int64 最多 19 位，正常永远碰不到；留着是因为一旦超限，服务报的是
// "operator exceeds 64 characters"，从后台错误里看不出这串是网关拼出来的。
const membershipMaxOperatorLength = 64

// errMembershipServiceNotConfigured 未配 MembershipRPC 时本域 10 条路由一律返回它。
// 不退化成空台账或「未开通」：那会让后台把「下游没接」读成「这个用户从来没买过会员」，
// 进而据此去手工发放——正是 §1 资金语义要避免的补数动作。
var errMembershipServiceNotConfigured = errors.New("gateway/admin: membership service client not configured")

// errMembershipRequestMissing 请求体缺失。goctl 生成的 handler 永远传非 nil 指针，
// 该分支只覆盖 logic 被直接复用的场景。
var errMembershipRequestMissing = errors.New("gateway/admin: request body required")

// errMembershipSessionRequired 受 AdminPermission 保护的写路由拿不到会话身份。
// 此时说明这条路由没被中间件保护（权限表/挂载漂移），一律 fail-closed。
var errMembershipSessionRequired = errors.New("gateway/admin: admin session identity required")

// membershipOperator 渲染下传给 membership 的 operator。
//
// claimed 是请求体里的 operator 位（.api 注释「必须 > 0」）：它**只**用于两件事——
// 按契约确认表单确实填了审计主体，以及在与会话不一致时留一条越权线索日志。
// 真正进台账的永远是会话 admin_id 渲染出的 gateway/admin:<id>，否则请求体就等于
// 能自称是任意后台账号（AGENTS.md §5：台账归 membership 持有，网关不能投喂未证实主体）。
//
// 日志不打 reason 正文与 idempotency_key：前者是人读文案、后者可被重放（§7 脱敏口径）。
func membershipOperator(ctx context.Context, route string, claimed int64) (string, error) {
	id, ok := middleware.AdminFromContext(ctx)
	if !ok {
		return "", errMembershipSessionRequired
	}
	if err := requireOperatorID(id.AdminID); err != nil {
		return "", err
	}
	if err := requireOperator("operator", claimed); err != nil {
		return "", err
	}
	if claimed != id.AdminID {
		logx.WithContext(ctx).Errorf("gateway/admin/%s: operator mismatch session=%d claimed=%d",
			route, id.AdminID, claimed)
	}
	operator := fmt.Sprintf("%s%d", membershipOperatorPrefix, id.AdminID)
	if utf8.RuneCountInString(operator) > membershipMaxOperatorLength {
		return "", fmt.Errorf("gateway/admin: rendered operator exceeds %d characters", membershipMaxOperatorLength)
	}
	logx.WithContext(ctx).Infof("gateway/admin/%s: admin_id=%d operator=%s", route, id.AdminID, operator)
	return operator, nil
}

// --- 传输层门槛 ---

// membershipNonNeg 只挡负数：0 在本域普遍是合法哨兵（page/size=0 用服务默认页、
// from_ts=0 不设下界、mid=0 跨用户查台账、plan_id=0 无套餐、limit=0 用批处理上限、
// expected_version=0 新建），负数没有任何对应语义，透传只会换来一次无意义往返。
// 上限（MaxPageSize、ExpireScanMaxLimit、MaxGrantDeltaDays、列宽）一律由服务夹取或拒绝。
func membershipNonNeg(field string, v int64) error {
	if v < 0 {
		return fmt.Errorf("gateway/admin: %s must be >= 0, got %d", field, v)
	}
	return nil
}

// membershipPositive 给「没有 0 语义」的主体位设下界：mid=0 在读写侧都指不到任何人
// （服务 requireMid 直接拒），to_expire_at=0 与 plan_id=0 在服务侧同样是硬错误。
func membershipPositive(field string, v int64) error {
	if v <= 0 {
		return fmt.Errorf("gateway/admin: %s required (must be > 0, got %d)", field, v)
	}
	return nil
}

// membershipEnum 挡住 0（*_UNSPECIFIED）：vip_type / source / target_state / min_vip_type
// 的 0 在服务侧都是硬错误（requireVipType、ValidGrantSource、ValidPlanState），
// 没有一个对应「不指定」的合法含义。具体取值是否被允许、迁移是否合法仍由服务判定，
// 网关不复算状态机。
func membershipEnum(field string, v int32) error {
	if v <= 0 {
		return fmt.Errorf("gateway/admin: %s required (0 = *_UNSPECIFIED)", field)
	}
	return nil
}

// membershipTimeWindow 只挡「负数」与「倒着给」的窗口：服务侧 ListGrants 对
// from_ts>to_ts（两者都为正）返回 ErrInvalidQueryFilter，单边的 0 表示该侧不设界。
// 窗口跨度上限由服务判定，网关不复算。
func membershipTimeWindow(from, to int64) error {
	if err := membershipNonNeg("from_ts", from); err != nil {
		return err
	}
	if err := membershipNonNeg("to_ts", to); err != nil {
		return err
	}
	if from > 0 && to > 0 && from > to {
		return errors.New("gateway/admin: from_ts must be <= to_ts")
	}
	return nil
}

// membershipExpireWindow 到期扫描窗口：服务侧对 to_expire_at<=0 与 from>to 都是硬错误
// （ErrInvalidExpireRange），而 from<=0 会被按 1 处理以覆盖「从未记过到期时间」的历史行，
// 所以只有 from 一侧允许 0。limit 的上限（ExpireScanMaxLimit）由服务裁剪，网关不复算。
func membershipExpireWindow(from, to, limit int64) error {
	if err := membershipNonNeg("from_expire_at", from); err != nil {
		return err
	}
	if err := membershipNonNeg("limit", limit); err != nil {
		return err
	}
	if err := membershipPositive("to_expire_at", to); err != nil {
		return err
	}
	if from > to {
		return errors.New("gateway/admin: from_expire_at must be <= to_expire_at")
	}
	return nil
}

// --- api → rpc ---

// membershipPlatformsFromAPI 把后台表单的平台编号列表转成 protobuf 枚举切片。
// 逐个 cast、不去重、不排序、不校验取值：平台掩码的组合规则（哪些端能同时出现、
// 空掩码是否合法）在 membership 的 planPlatformsFromRPC 里。
func membershipPlatformsFromAPI(list []int32) []membershiprpc.PlanPlatform {
	out := make([]membershiprpc.PlanPlatform, 0, len(list))
	for _, v := range list {
		out = append(out, membershiprpc.PlanPlatform(v))
	}
	return out
}

// --- rpc → 后台 types ---

// membershipPlanToAPI 投影套餐。价格原样转达 *_minor：网关不做单位换算、
// 不比较促销价与原价（那是 membership 判定「改价必须新建草稿」的输入）。
func membershipPlanToAPI(p *membershiprpc.PlanInfo) types.MembershipPlanItem {
	if p == nil {
		return types.MembershipPlanItem{}
	}
	return types.MembershipPlanItem{
		PlanId:             p.GetPlanId(),
		PlanCode:           p.GetPlanCode(),
		Name:               p.GetName(),
		Description:        p.GetDescription(),
		VipType:            int32(p.GetVipType()),
		DurationDays:       p.GetDurationDays(),
		UnitCount:          p.GetUnitCount(),
		PriceMinor:         p.GetPriceMinor(),
		PromPriceMinor:     p.GetPromPriceMinor(),
		Currency:           p.GetCurrency(),
		Platforms:          membershipPlatformsToAPI(p.GetPlatforms()),
		AutoRenewSupported: p.GetAutoRenewSupported(),
		State:              int32(p.GetState()),
		Version:            p.GetVersion(),
		Ctime:              p.GetCtime(),
		Mtime:              p.GetMtime(),
		CreatedBy:          p.GetCreatedBy(),
		UpdatedBy:          p.GetUpdatedBy(),
	}
}

func membershipPlansToAPI(list []*membershiprpc.PlanInfo) []types.MembershipPlanItem {
	out := make([]types.MembershipPlanItem, 0, len(list))
	for _, p := range list {
		out = append(out, membershipPlanToAPI(p))
	}
	return out
}

// membershipPlatformsToAPI 空掩码投影成 []（不是 null），让后台表单能直接遍历。
func membershipPlatformsToAPI(list []membershiprpc.PlanPlatform) []int32 {
	out := make([]int32, 0, len(list))
	for _, v := range list {
		out = append(out, int32(v))
	}
	return out
}

// membershipEntitlementToAPI 投影权益码目录项。min_vip_type/enabled 原样转达：
// 「档位够不够」是 membership 的 TierSufficient 结论，网关不自己比大小。
func membershipEntitlementToAPI(e *membershiprpc.EntitlementInfo) types.MembershipEntitlementItem {
	if e == nil {
		return types.MembershipEntitlementItem{}
	}
	return types.MembershipEntitlementItem{
		Code:        e.GetCode(),
		Name:        e.GetName(),
		Description: e.GetDescription(),
		MinVipType:  int32(e.GetMinVipType()),
		Enabled:     e.GetEnabled(),
		Version:     e.GetVersion(),
		Ctime:       e.GetCtime(),
		Mtime:       e.GetMtime(),
	}
}

func membershipEntitlementsToAPI(list []*membershiprpc.EntitlementInfo) []types.MembershipEntitlementItem {
	out := make([]types.MembershipEntitlementItem, 0, len(list))
	for _, e := range list {
		out = append(out, membershipEntitlementToAPI(e))
	}
	return out
}

// membershipMemberToAPI 投影会员身份行。expire_at 与 auto_renew 的「是否生效」
// 由调用方拿同一次响应里的 server_now 比较（契约把时钟交给了服务）。
func membershipMemberToAPI(m *membershiprpc.MembershipInfo) types.MembershipMemberItem {
	if m == nil {
		return types.MembershipMemberItem{}
	}
	return types.MembershipMemberItem{
		Mid:               m.GetMid(),
		VipType:           int32(m.GetVipType()),
		StartAt:           m.GetStartAt(),
		ExpireAt:          m.GetExpireAt(),
		AutoRenew:         m.GetAutoRenew(),
		AutoRenewChannel:  m.GetAutoRenewChannel(),
		AutoRenewSignedAt: m.GetAutoRenewSignedAt(),
		Source:            int32(m.GetSource()),
		PaidMonthCount:    m.GetPaidMonthCount(),
		Version:           m.GetVersion(),
		Ctime:             m.GetCtime(),
		Mtime:             m.GetMtime(),
	}
}

func membershipMembersToAPI(list []*membershiprpc.MembershipInfo) []types.MembershipMemberItem {
	out := make([]types.MembershipMemberItem, 0, len(list))
	for _, m := range list {
		out = append(out, membershipMemberToAPI(m))
	}
	return out
}

// membershipGrantToAPI 投影授予/变更台账。operator/request_id/reason 是审计三要素，
// 一个都不能裁：后台靠它区分「谁在什么时候为哪一单动了多少天」。
func membershipGrantToAPI(g *membershiprpc.GrantInfo) types.MembershipGrantItem {
	if g == nil {
		return types.MembershipGrantItem{}
	}
	return types.MembershipGrantItem{
		GrantId:        g.GetGrantId(),
		Mid:            g.GetMid(),
		VipType:        int32(g.GetVipType()),
		Action:         g.GetAction(),
		DeltaDays:      g.GetDeltaDays(),
		PlanId:         g.GetPlanId(),
		Source:         int32(g.GetSource()),
		BizOrderNo:     g.GetBizOrderNo(),
		PaymentNo:      g.GetPaymentNo(),
		BeforeExpireAt: g.GetBeforeExpireAt(),
		AfterExpireAt:  g.GetAfterExpireAt(),
		Operator:       g.GetOperator(),
		RequestId:      g.GetRequestId(),
		Reason:         g.GetReason(),
		Ctime:          g.GetCtime(),
	}
}

func membershipGrantsToAPI(list []*membershiprpc.GrantInfo) []types.MembershipGrantItem {
	out := make([]types.MembershipGrantItem, 0, len(list))
	for _, g := range list {
		out = append(out, membershipGrantToAPI(g))
	}
	return out
}
