package logic

import (
	"context"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpsertQuotaPolicyLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpsertQuotaPolicyLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpsertQuotaPolicyLogic {
	return &UpsertQuotaPolicyLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 新增/更新配额规则（运营）。
//
// 规则本身是「限额真值」，op_quota_usage 只是投影，因此本方法是判定链的上游：
// 一处放宽会立即反映到所有走 AuthorizeRequest 的请求上，参数门禁全部失败关闭。
//
// 错误映射：ErrOperatorRequired→PermissionDenied；ErrInvalidAppID/ErrAppNotFound/
// errAPICodeRequired/ErrScopeUnknown/errInvalidPolicyID/ErrQuotaLimitInvalid→InvalidArgument；
// ErrWindowInvalid（窗口长度越界）、errReasonRequired（停用不带原因）→InvalidArgument；
// ErrForbiddenScopeCategory→InvalidArgument（§1 商业化红线）；SQL 失败→Internal。
func (l *UpsertQuotaPolicyLogic) UpsertQuotaPolicy(in *rpc.UpsertQuotaPolicyReq) (*rpc.UpsertQuotaPolicyReply, error) {
	ctx, s := l.ctx, l.svcCtx

	// 1. 身份：只有自然人运营可改限额。本方法不开放给应用 owner——owner 提高自己限额
	//    等于自己给自己放行。仓库约定 operator=0 只用于「无自然人主体的内部回写」
	//    （如 token 使用时间戳、投递计数），限额规则不属于内部回写，所以这里硬性要求 >0；
	//    cron 触发时也必须在请求里带上当初授权的运营 mid（proto:440「运营，必填 > 0」）。
	if err := requireOperator(in.OperatorMid); err != nil {
		return nil, err
	}

	// 2. 应用层级：app_id=0 是全局默认层（兜住所有没有自有规则的应用）；
	//    非 0 时必须真实存在——给不存在的应用配限额是纯配置事故：规则永远命中不了，
	//    却会在运营后台显示「已配置」，比直接报错更难发现。
	if in.AppId < 0 {
		return nil, model.ErrInvalidAppID
	}
	if in.AppId > 0 {
		if _, err := findApp(ctx, s, in.AppId); err != nil {
			return nil, err
		}
	}

	// 3. api_code：必填，"*" 是合法取值（该层级全部接口）；字符集与列宽对齐 op_quota_policy。
	apiCode, err := requireLen(in.ApiCode, maxAPICodeRunes, errAPICodeRequired, errAPICodeRequired)
	if err != nil {
		return nil, err
	}
	if err := validScopeToken(apiCode); err != nil {
		return nil, err
	}
	// 商业化红线（AGENTS.md §1）双保险：model.QuotaPolicyModel.Upsert 内有同一拦截，
	// 这里先拦是为了不占写令牌、也不留下「改过限额但其实没落库」的审计噪声。
	if err := rejectForbiddenScopes([]string{apiCode}); err != nil {
		return nil, err
	}

	// 4. 窗口长度：必须 >0（判定与重算都以 FLOOR(ctime/w)*w 取齐，0 会除零）；
	//    上界 maxQuotaWindowSeconds（30 天）挡住「把毫秒当秒传」这类数量级错误——
	//    一个 1e9 秒的窗口等价于永久占满额度，落库后只能靠人工删行收拾。
	if in.WindowSeconds <= 0 || in.WindowSeconds > maxQuotaWindowSeconds {
		return nil, model.ErrWindowInvalid
	}

	// 5. 限额：只拒负数。0 是合法且有意的取值：quota_limit=0 且 enabled=1 等价
	//    「禁用该接口」（QuotaPolicy.Denied()），判定路径按超限回而不会当成「无上限」。
	if in.Limit < 0 {
		return nil, model.ErrQuotaLimitInvalid
	}

	// 6. 停用不走本方法：本 RPC 没有 reason 字段，而「关掉一条限额」是有运营后果的动作，
	//    必须留下问责原因，因此统一走 QuotaPolicies.Disable（带 reason 并保留行以便重算解释）。
	//    这里失败关闭而不是替调用方补一个空原因。
	if !in.Enabled {
		return nil, errReasonRequired
	}

	// 7. 唯一键与幂等：写入依赖 uniq_app_api_window (app_id, api_code, window_seconds) 的
	//    INSERT ... ON DUPLICATE KEY UPDATE，所以「同规则重复提交」天然是最后一次生效，
	//    不需要额外幂等键（也不允许加一个：网关重试同一份规则时再引入幂等键反而会出现
	//    「两次提交都成功、结论不同」）。policy_id 只是可选定位辅助：0 表示按唯一键 upsert；
	//    非 0 时必须与唯一键算出的那一行是同一条，否则就是「以为在改 A 其实改了 B」。
	if in.PolicyId > 0 {
		target, err := s.QuotaPolicies.FindByID(ctx, in.PolicyId)
		if err != nil {
			return nil, err
		}
		if target == nil {
			return nil, model.ErrQuotaPolicyNotFound
		}
		if target.AppID != in.AppId || target.APICode != apiCode || target.WindowSeconds != in.WindowSeconds {
			return nil, errInvalidPolicyID
		}
	}

	policy := &model.QuotaPolicy{
		AppID:         in.AppId,
		APICode:       apiCode,
		WindowSeconds: in.WindowSeconds,
		QuotaLimit:    in.Limit,
		Enabled:       boolToInt8(true),
		Operator:      in.OperatorMid,
		// 规则变更本身由 operator_mid + mtime 追责，Reason 留给 Disable 通道写；
		// 这里保持空串，避免用一句「运营改了限额」把审计列填成噪声。
		Reason: "",
	}

	// 8. 生效层级可解释性：判定只取一个层级（model.NarrowPolicies），运营「改了限额却看不到效果」
	//    几乎都出在「新规则被更高层级整体遮蔽」。结论只进日志（响应契约里没有这个字段，也不该加），
	//    排障按 app_id/api_code grep 即可定位。
	previewQuotaTier(ctx, s, policy)

	// 9. 写阶段：与其它写路径共用进程级写令牌，规则写入频率极低，
	//    但这道门保证「误操作脚本」不会把限额表刷成 DB 压力源。
	release, err := writePermit(ctx, s)
	if err != nil {
		return nil, err
	}
	defer release()

	policyID, created, err := s.QuotaPolicies.Upsert(ctx, policy)
	if err != nil {
		return nil, err
	}

	// 10. 一致性：规则变更后不清理既有窗口投影——limit_snapshot 只是「记账时限额」的解释性快照，
	//     判定一律用当前生效规则重算，因此新限额下一个窗口边界即生效（最长等待 = 窗口长度）。
	//     这也是 ListQuotaUsage 的 limit 口径（回显当前生效限额）与快照列可以并存的原因。
	logx.WithContext(ctx).Infof("open-platform: 配额规则写入 policy_id=%d app_id=%d api_code=%s window=%ds "+
		"limit=%d operator_mid=%d created=%t", policyID, in.AppId, apiCode, in.WindowSeconds, in.Limit,
		in.OperatorMid, created)

	return &rpc.UpsertQuotaPolicyReply{PolicyId: policyID, Created: created}, nil
}

// previewQuotaTier 预判「这条新规则会落在哪一层、会不会被更高层级遮蔽」，只写日志。
//
// 层级判定直接调用 model.NarrowPolicies 本身（把待写入的行替换掉同唯一键的既有候选行后再算），
// 不在这里另写一套排序：预判逻辑一旦独立实现，就会在下次 NarrowPolicies 调整时与真值漂移，
// 变成「日志说生效、实际没生效」的第二种排障黑洞。
// 候选集只含 enabled=1 的规则（ListCandidates 的口径），与判定路径看到的集合完全一致；
// 已停用规则不参与遮蔽，所以本函数不对它们作判断。
// 预判失败只记 Error 不影响写入：可观测性缺一条日志不构成拒绝运营改限额的理由。
func previewQuotaTier(ctx context.Context, s *svc.ServiceContext, p *model.QuotaPolicy) {
	cands, err := s.QuotaPolicies.ListCandidates(ctx, p.AppID, p.APICode)
	if err != nil {
		logx.WithContext(ctx).Errorf("open-platform: 配额层级预判取候选失败 app_id=%d api_code=%s: %v",
			p.AppID, p.APICode, err)
		return
	}
	merged := make([]*model.QuotaPolicy, 0, len(cands)+1)
	for _, c := range cands {
		if c == nil {
			continue
		}
		if c.AppID == p.AppID && c.APICode == p.APICode && c.WindowSeconds == p.WindowSeconds {
			continue // upsert 会覆盖同唯一键行，预判时先剔除旧值
		}
		merged = append(merged, c)
	}
	merged = append(merged, p)
	tier := model.NarrowPolicies(merged, p.AppID, p.APICode)
	effective := false
	for _, t := range tier {
		if t == p {
			effective = true
			break
		}
	}
	if !effective {
		logx.WithContext(ctx).Infof("open-platform: 配额规则将被更高层级遮蔽 app_id=%d api_code=%s window=%ds "+
			"命中层级规则数=%d（本条不会参与判定）", p.AppID, p.APICode, p.WindowSeconds, len(tier))
		return
	}
	logx.WithContext(ctx).Infof("open-platform: 配额规则落层 app_id=%d api_code=%s window=%ds 同层级规则数=%d",
		p.AppID, p.APICode, p.WindowSeconds, len(tier))
}
