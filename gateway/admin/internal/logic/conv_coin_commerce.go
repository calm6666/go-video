// 手写文件（不属于 goctl 生成产物）：/admin/coin 四条路由共用的身份渲染、
// 传输层门槛与 RPC→后台投影。
//
// 放在这里而不是各 logic 里重复一遍（AGENTS.md §4/§5）：
//   - operator 只能来自会话身份，且要满足 coin 侧 operator 列宽；
//   - 硬币余额与投币记录**只属于 coin 服务**（§5）：日限、单片上限、取消窗口、
//     初始余额、|delta| 上限、余额会不会被扣成负数、幂等指纹是否一致，全部由服务判定，
//     网关一个都不复算，也不代为放宽；
//   - 硬币不是钱：本域与 payment 的现金余额是两套账，投影里不出现任何折算位（§1）；
//   - 分页三元组照抄 reply，不复算、不裁剪。
//
// 与 order/payment 同口径：读侧没配下游时一律报错，不回 found=false 也不回空台账——
// 那会让后台把「coin 没接」读成「这个用户一枚硬币都没有」，然后用 /grant 去补数。

package logic

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/types"
	coinrpc "go-video/services/coin/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// coinOperatorPrefix 与 membership/payment/order 同口径：operator 前缀说明「哪个入口提交的」
// （服务名，不是实例地址），后面接会话 admin_id，让 cn_flow.operator 能回溯到人。
const coinOperatorPrefix = "gateway/admin:"

// coinMaxOperatorLength 是 cn_flow.operator 的列宽（VARCHAR(64)；服务侧收口在
// services/coin/internal/logic/conv.go 的 maxIDLen，同样 64）。
// 前缀 14 + int64 最多 19 位，正常永远碰不到；留着是因为服务侧对超长是
// **clipID 按字节切**而不是拒绝，真超限时会把发放凭证的经办人切成半串（§5 审计证据）。
// 注意：coin 没有像 membership 的 model.MaxOperatorLength 那样导出这个常量，
// 只在 internal/logic 里写死 64，所以这里是本仓库第二处副本（已作为契约缺口上报）。
const coinMaxOperatorLength = 64

// errCoinServiceNotConfigured 未配 CoinRPC 时本域 4 条路由一律返回它。
var errCoinServiceNotConfigured = errors.New("gateway/admin: coin service client not configured")

// errCoinRequestMissing 请求体缺失。goctl 生成的 handler 永远传非 nil 指针，
// 该分支只覆盖 logic 被直接复用的场景。
var errCoinRequestMissing = errors.New("gateway/admin: request body required")

// errCoinSessionRequired 受 AdminPermission 保护的写路由拿不到会话身份。
// 此时说明这条路由没被中间件保护（权限表/挂载漂移），一律 fail-closed：
// /grant 是唯一能改硬币余额的后台口，没有主体就一个字都不写。
var errCoinSessionRequired = errors.New("gateway/admin: admin session identity required")

// coinOperator 渲染下传给 coin 的 operator。
//
// claimed 是请求体里的 operator 位（.api 注释「必须 > 0」）：它**只**用于两件事——
// 按契约确认表单确实填了审计主体，以及在与会话不一致时留一条越权线索日志。
// 真正进台账的永远是会话 admin_id 渲染出的 gateway/admin:<id>，否则请求体就等于
// 能自称是任意后台账号（§5：余额归 coin 持有，网关不能投喂未证实主体）。
//
// 日志不打 reason 正文与 idempotency_key：前者是人读文案、后者可被重放（§7 脱敏口径）。
func coinOperator(ctx context.Context, route string, claimed int64) (string, error) {
	id, ok := middleware.AdminFromContext(ctx)
	if !ok {
		return "", errCoinSessionRequired
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
	operator := fmt.Sprintf("%s%d", coinOperatorPrefix, id.AdminID)
	if utf8.RuneCountInString(operator) > coinMaxOperatorLength {
		return "", fmt.Errorf("gateway/admin: rendered operator exceeds %d characters", coinMaxOperatorLength)
	}
	logx.WithContext(ctx).Infof("gateway/admin/%s: admin_id=%d operator=%s", route, id.AdminID, operator)
	return operator, nil
}

// --- 传输层门槛 ---

// coinNonNeg 只挡负数：0 在本域普遍是合法哨兵（page/size=0 用服务默认页、
// mid=0 跨用户查流水台账、flow_type=0 不按类型过滤、from_ts/to_ts=0 该侧不设界、
// biz_no="" 不按订单号过滤、reason/biz_no 空由服务按 flow_type 决定是否拒绝），
// 负数没有任何对应语义，透传只会换来一次无意义往返。
// 上限（MaxPageSize、MaxListOffset、MaxGrantDelta、列宽）一律由服务拒绝，网关不复算。
func coinNonNeg(field string, v int64) error {
	if v < 0 {
		return fmt.Errorf("gateway/admin: %s must be >= 0, got %d", field, v)
	}
	return nil
}

// coinPositive 给「没有 0 语义」的主体位设下界：/account/get 与 /grant 的 mid=0 在服务侧
// 都是硬错误（GetCoinAccount/GrantCoin 对 mid<=0 回 ErrInvalidMid，硬币账户没有游客号）。
func coinPositive(field string, v int64) error {
	if v <= 0 {
		return fmt.Errorf("gateway/admin: %s required (must be > 0, got %d)", field, v)
	}
	return nil
}

// coinDeltaNonZero 只管 delta 的「0 位」：.api 写明「不允许为 0」，服务侧
// ErrGrantDeltaInvalid 同样只拒 0（以及 |delta|>MaxGrantDelta）。
// 绝对值上限、会不会把余额扣成负数全部由服务判，网关一个都不复算——
// 尤其不做「负数看着不合理就拒掉」这种兜底：扣回本来就是同一条口的合法方向。
func coinDeltaNonZero(v int64) error {
	if v == 0 {
		return errors.New("gateway/admin: delta must not be 0 (positive grants, negative clawbacks)")
	}
	return nil
}

// coinGrantFlowType 只挡「这条后台口明确不接受的流水类型」。
//
// admin.api 与 coin.proto 在这一点上口径完全一致：GrantCoin 只接受 ORDER_PACK 与 ADMIN_GRANT。
// 这不是复算服务判定，而是本路由的身份声明：TOSS/CANCEL_TOSS 由终端投币链路自己写，
// 从后台塞进来就是伪造「谁喜欢这条内容」的信号（§7 会污染 spm 的互动特征）；
// EXPIRE 本项目未开启、恒不出现；UNSPECIFIED 与未知编号没有任何对应语义。
// 因此这里用**白名单**而不是「非负就放行」，并且：
//   - 不把非法值改写/降级成 ADMIN_GRANT（那就是伪造发放来源，服务侧的
//     ErrGrantTypeInvalid 也永远不会被触发，运营还以为自己发对了）；
//   - 不替调用方补默认档位：档位是「买来的」还是「白送的」的唯一区分位（§5 对账口径）。
func coinGrantFlowType(v int32) error {
	switch coinrpc.CoinFlowType(v) {
	case coinrpc.CoinFlowType_COIN_FLOW_TYPE_ORDER_PACK,
		coinrpc.CoinFlowType_COIN_FLOW_TYPE_ADMIN_GRANT:
		return nil
	default:
		return fmt.Errorf("gateway/admin: flow_type %d is not accepted on this route (only ORDER_PACK=3 / ADMIN_GRANT=4)", v)
	}
}

// coinTimeWindow 只挡「负数」与「倒着给」的窗口：服务侧 ListCoinFlows 对
// from_ts>to_ts（两者都为正）返回 ErrInvalidTimeRange，单边 0 表示该侧不设界。
// 「跨用户（mid=0）必须给时间窗或 biz_no」是 coin 的有界性判定
// （ErrUnboundedLedgerQuery），它才知道哪个点位能替代窗口，网关不复算。
func coinTimeWindow(from, to int64) error {
	if err := coinNonNeg("from_ts", from); err != nil {
		return err
	}
	if err := coinNonNeg("to_ts", to); err != nil {
		return err
	}
	if from > 0 && to > 0 && from > to {
		return errors.New("gateway/admin: from_ts must be <= to_ts")
	}
	return nil
}

// --- rpc → 后台 types ---

// coinAccountToAPI 投影硬币账户行。
//
// found 位在**本路由的 reply 上**（GetCoinAccountReply.found），所以「从未建过账户」
// 与「余额为 0」在后台是可区分的——这一点与 payment 的 GetWallet 不同（那个缺口已上报）。
// 服务侧对 found=false 也会回一份带三项限额的账户（限额来自生效配置，不是账户行），
// 因此这里照常逐字段转达，绝不因为「没账户」就把整行清零：那会把「上限是多少枚」
// 这个仍然成立的事实抹掉（见 services/coin/internal/logic/conv.go 的 accountInfo）。
func coinAccountToAPI(a *coinrpc.CoinAccountInfo) types.CoinAccountItem {
	if a == nil {
		return types.CoinAccountItem{}
	}
	return types.CoinAccountItem{
		Mid:                 a.GetMid(),
		Balance:             a.GetBalance(),
		TotalTossed:         a.GetTotalTossed(),
		TodayTossed:         a.GetTodayTossed(),
		TodayLimit:          a.GetTodayLimit(),
		PerTargetLimit:      a.GetPerTargetLimit(),
		CancelWindowSeconds: a.GetCancelWindowSeconds(),
		Version:             a.GetVersion(),
		Ctime:               a.GetCtime(),
		Mtime:               a.GetMtime(),
	}
}

// coinFlowToAPI 投影硬币流水。delta 的正负（正入负出）与 balance_after 是台账自身的
// 结论，网关不取绝对值、不校验前后余额自洽，也不把 flow_type 折叠成「反正都是发放」。
func coinFlowToAPI(f *coinrpc.CoinFlowInfo) types.CoinFlowItem {
	if f == nil {
		return types.CoinFlowItem{}
	}
	return types.CoinFlowItem{
		FlowId:       f.GetFlowId(),
		Mid:          f.GetMid(),
		FlowType:     int32(f.GetFlowType()),
		Delta:        f.GetDelta(),
		BalanceAfter: f.GetBalanceAfter(),
		TargetAid:    f.GetTargetAid(),
		BizNo:        f.GetBizNo(),
		Operator:     f.GetOperator(),
		RequestId:    f.GetRequestId(),
		Remark:       f.GetRemark(),
		Ctime:        f.GetCtime(),
	}
}

func coinFlowsToAPI(list []*coinrpc.CoinFlowInfo) []types.CoinFlowItem {
	out := make([]types.CoinFlowItem, 0, len(list))
	for _, f := range list {
		out = append(out, coinFlowToAPI(f))
	}
	return out
}

// coinTossConfigToAPI 逐位转达服务回显的生效参数。
//
// 这五位必须是**运行时真正生效**的那份（coin 的 GetTossConfig 读的是 svc 已收敛过的配置），
// 网关一个都不硬编码、不填「常见值」：写死会让「改了配置但后台还在读旧上限」变成常态故障
// （AGENTS.md §6），而 initial_balance 被美化成非 0 更是凭空捏造赠送规则。
func coinTossConfigToAPI(r *coinrpc.GetTossConfigReply) types.CoinTossConfigData {
	if r == nil {
		return types.CoinTossConfigData{}
	}
	return types.CoinTossConfigData{
		DailyLimit:          r.GetDailyLimit(),
		PerTargetLimit:      r.GetPerTargetLimit(),
		CancelWindowSeconds: r.GetCancelWindowSeconds(),
		MinBalanceToToss:    r.GetMinBalanceToToss(),
		InitialBalance:      r.GetInitialBalance(),
	}
}
