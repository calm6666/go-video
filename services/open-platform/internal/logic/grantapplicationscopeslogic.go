package logic

import (
	"context"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type GrantApplicationScopesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGrantApplicationScopesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GrantApplicationScopesLogic {
	return &GrantApplicationScopesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 运营审批 scope 授予/回收。
func (l *GrantApplicationScopesLogic) GrantApplicationScopes(in *rpc.GrantApplicationScopesReq) (*rpc.GrantApplicationScopesReply, error) {
	ctx, s := l.ctx, l.svcCtx

	// 1. 身份：本方法只有运营通道。开发者自助获批等于放弃审批这个控制点。
	if err := requireOperator(in.OperatorMid); err != nil {
		return nil, err
	}
	app, err := findApp(ctx, s, in.AppId)
	if err != nil {
		return nil, err
	}
	// 2. 审计原因必填：授予与回收都是需要问责的结论，op_app_scope.reason 不允许空串。
	if err := requireReason(in.Reason); err != nil {
		return nil, err
	}
	// 3. 幂等键必填（长度不合规直接拒，不做截断）。
	if _, err := requireLen(in.IdempotencyKey, maxIdempotencyRunes,
		model.ErrIdempotencyKeyRequired, model.ErrIdempotencyKeyRequired); err != nil {
		return nil, err
	}

	grantSet, err := normalizeScopes(in.Grant)
	if err != nil {
		return nil, err
	}
	revokeSet, err := normalizeScopes(in.Revoke)
	if err != nil {
		return nil, err
	}
	if len(grantSet) == 0 && len(revokeSet) == 0 {
		return nil, errGrantOrRevokeRequired
	}
	// 同一 scope 同时出现在授予与回收里：这不是「先给后收」，而是调用方把自己的意图写矛盾了。
	// 顺序执行会产出一个依赖执行次序的结果，属于未定义行为，直接拒。
	if overlap := intersectScopes(grantSet, revokeSet); len(overlap) > 0 {
		return nil, model.ErrInvalidStateTransition
	}
	// 4. 红线类目整次拒绝（不给「先授予再隐藏」留口子）。
	if err := rejectForbiddenScopes(append(append([]string{}, grantSet...), revokeSet...)); err != nil {
		return nil, err
	}

	// 5. 幂等重放：命中首次结论直接回。硬锚点仍是 uniq_app_scope 上的状态迁移，
	//    因此缓存缺失时降级为重算也收敛（见 idem.go）。
	first, cached, err := claimWriteOnce(ctx, s, "scope-grant:"+app.AppKey, in.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	if !first {
		reply := &rpc.GrantApplicationScopesReply{}
		if loadWriteOnce(cached, reply) {
			reply.Replayed = true
			logReplay(ctx, "GrantApplicationScopes", in.IdempotencyKey)
			return reply, nil
		}
	}

	// 6. 目录校验：不存在的 scope 进 rejected（允许运营提交混合批次），
	//    已停用的同样进 rejected——但字符非法的在第 3 步 normalizeScopes 就整体失败了。
	defs, err := s.Scopes.FindByScopes(ctx, grantSet)
	if err != nil {
		return nil, err
	}
	accepted := make([]string, 0, len(grantSet))
	rejected := make([]string, 0)
	for _, sc := range grantSet {
		def := defs[sc]
		if def == nil || def.Enabled != 1 {
			rejected = append(rejected, sc)
			continue
		}
		accepted = append(accepted, sc)
	}
	// 高风险 scope 不可批量授予：一次误操作不能放开整片写权限。
	if err := rejectHighRiskBatch(ctx, defs, accepted); err != nil {
		return nil, err
	}
	// 写 scope 的目录侧前提：必须声明 requires_user_consent=1，否则「用户同意了」无从谈起。
	if err := requireWriteScopeConsent(defs, accepted); err != nil {
		return nil, err
	}

	version, effective, err := l.apply(app, accepted, revokeSet, in)
	if err != nil {
		return nil, err
	}

	reply := &rpc.GrantApplicationScopesReply{
		Granted:    accepted,
		Revoked:    effective,
		Rejected:   rejected,
		AppVersion: version,
	}
	storeWriteOnce(ctx, s, "scope-grant:"+app.AppKey, in.IdempotencyKey, reply)
	logx.WithContext(ctx).Infof("open-platform: scope 审批 app_id=%d operator=%d grant=%d revoke=%d reject=%d version=%d",
		app.AppID, in.OperatorMid, len(accepted), len(revokeSet), len(rejected), version)
	return reply, nil
}

// apply 一次事务内完成「审批结论 + 版本前移 + 回收连带撤销」。
// 回传的 revoked 是「确实对应已获批 scope」的有效回收集合，调用方必须用它做回执：
// 把请求原样回给运营，就等于宣称「这几条已经回收了」而库里其实什么都没动。
//
// 版本号必须与前两项同事务：Introspect/AuthorizeRequest 以 app.version 作为投影失效依据，
// 分开提交会留下「scope 已回收但版本号还是旧值」的窗口。
//
// 与 blueprint 的偏差（刻意为之）：回收的连带效应不用「Grants.ScopeOf 逐条比对现存 grant」，
// 而用 model 提供的集合化定点撤销（Grants/Tokens.RevokeByAppWithScopes，SQL 内 FIND_IN_SET 判定）。
// 逐条遍历会把扫描规模变成 grant 总数的函数，让「回收立即生效」的时间窗随数据量线性变长；
// 集合化 UPDATE 的规模只与本次回收的 scope 数有关。
func (l *GrantApplicationScopesLogic) apply(app *model.Application, granted, revoked []string,
	in *rpc.GrantApplicationScopesReq) (int32, []string, error) {
	ctx, s := l.ctx, l.svcCtx
	if len(granted) == 0 && len(revoked) == 0 {
		// 全部被目录拒掉：不做无意义写，也不前移版本（版本前移会让既有校验链路多一次冷启动）。
		return app.Version, revoked, nil
	}
	if len(revoked) > 0 {
		// 回收一个应用从未获批的 scope 没有破坏性，但也不该静默成功——回查一次，
		// 让运营知道「这条回收其实没对应任何已获批 scope」。
		current, err := s.AppScopes.ListGranted(ctx, app.AppID)
		if err != nil {
			return 0, nil, err
		}
		effective := make([]string, 0, len(revoked))
		for _, sc := range revoked {
			if model.ContainsScope(current, sc) {
				effective = append(effective, sc)
			}
		}
		revoked = effective
		if len(revoked) == 0 && len(granted) == 0 {
			return app.Version, revoked, nil
		}
	}

	release, err := writePermit(ctx, s)
	if err != nil {
		return 0, nil, err
	}
	defer release()

	now := nowUnix()
	reason := clipRunes(in.Reason, maxReasonRunes)
	err = s.DB.TransactCtx(ctx, func(tctx context.Context, session sqlx.Session) error {
		if err := s.AppScopes.Grant(tctx, session, app.AppID, granted, in.OperatorMid, reason); err != nil {
			return err
		}
		if err := s.AppScopes.Revoke(tctx, session, app.AppID, revoked, in.OperatorMid, reason); err != nil {
			return err
		}
		if len(revoked) > 0 {
			// 先撤销 token 再撤销 grant：两者都以「快照命中被回收 scope」为条件，顺序不影响集合，
			// 但 token 侧先落可让并发中的 Introspect 更早看到 state=REVOKED（grant 位点是第二道）。
			if _, err := s.Tokens.RevokeByAppWithScopes(tctx, session, app.AppID, revoked, now,
				"scope revoked: "+reason); err != nil {
				return err
			}
			if _, err := s.Grants.RevokeByAppWithScopes(tctx, session, app.AppID, revoked,
				in.OperatorMid, reason, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}

	// Apps.NextVersion 走连接池而不是 session（model 层签名如此），因此无法并入上面的事务。
	// 放在事务之后：事务失败时版本不动，最坏是「白重算一次投影」；
	// 反过来（先前移后改审批）会留下「版本号说已变更、审批结论还是旧的」的假失效。
	fresh, err := s.Apps.NextVersion(ctx, app.AppID)
	if err != nil {
		return 0, nil, err
	}

	// 回收的连带外呼通知刻意留空：WEBHOOK_EVENT_TYPE_GRANT_REVOKED 的正文契约是
	// 「单个 grant_id 被撤销」（见 notifyGrantRevoked），而集合化撤销只回受影响行数、
	// 拿不到 grant_id 集合，GrantModel 也没有「按 scope 枚举某应用未撤销 grant」的口径。
	// 造一条 grant_id=0 的通知会让订阅方无法与自己的授权表对账（比不发更糟）。
	// 已登记进 README「已知缺口」：需要 per-grant 通知就得给 model 增加「返回被撤销 grant_id 列表」。
	if len(revoked) > 0 {
		logx.WithContext(ctx).Infof("open-platform: scope 回收连带撤销 app_id=%d scopes=%v version=%d",
			app.AppID, revoked, fresh)
	}
	return fresh, revoked, nil
}

// intersectScopes 返回两个已规范化（升序去重）列表的交集。
func intersectScopes(a, b []string) []string {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	out := make([]string, 0, len(a))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out = append(out, a[i])
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return out
}

// rejectHighRiskBatch 高风险 scope 必须单独审批：同批只允许出现一条高风险 scope，
// 且该批不得混入其它 scope（一次误操作放开全部写权限是最难回滚的事故形态）。
func rejectHighRiskBatch(ctx context.Context, defs map[string]*model.Scope, granted []string) error {
	high := 0
	for _, sc := range granted {
		if def := defs[sc]; def != nil && def.RiskLevel == model.ScopeRiskHigh {
			high++
		}
	}
	if high > 1 || (high == 1 && len(granted) > 1) {
		logx.WithContext(ctx).Errorf("open-platform: 高风险 scope 批量授予被拒 count=%d batch=%d", high, len(granted))
		return errHighRiskScopeBatch
	}
	return nil
}
