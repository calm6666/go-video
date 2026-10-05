package logic

import (
	"context"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type RefreshAccessTokenLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRefreshAccessTokenLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RefreshAccessTokenLogic {
	return &RefreshAccessTokenLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// refresh token 轮换（旧值重放即撤销整条 grant）。
//
// 契约依据：proto:328-340「旧 refresh 立即置 ROTATED，若旧值被再次使用则判定重放并撤销
// 整条 grant（保守失效）」、proto:333「scope 允许收窄，不允许扩大」。
// refresh 是否滑动续期：本期实现为「新行重新给满 RefreshTokenTTLSeconds」（README:100 同口径），
// 不做绝对寿命上限——那需要 grant 侧记录首次授权时间并新增判定，属契约外新增语义。
func (l *RefreshAccessTokenLogic) RefreshAccessToken(in *rpc.RefreshAccessTokenReq) (*rpc.RefreshAccessTokenReply, error) {
	ctx, s := l.ctx, l.svcCtx

	// 1. 参数与哈希闸门：pepper 缺失时 hashOfPlaintext 直接 fail closed，
	//    绝不退化成「拿明文当哈希去 LIKE」或「无 pepper 哈希」。
	if in.AppId <= 0 {
		return nil, model.ErrInvalidAppID
	}
	plain, err := requireLen(in.RefreshToken, credentialBytes*2, model.ErrTokenInvalid, model.ErrTokenInvalid)
	if err != nil {
		return nil, err
	}
	hash, err := hashOfPlaintext(s, plain)
	if err != nil {
		return nil, err
	}

	// 2. 定位：refresh 走 uniq_refresh_hash 按值反查。
	row, err := s.Tokens.FindByRefreshHash(ctx, hash)
	if err != nil {
		return nil, err
	}
	// 「不存在」与「不属于本应用」回同一个哨兵：可区分的答案等于一个 token 存在性探测接口，
	// 而 refresh 的有效期是 30 天，探测窗口足够长。
	if row == nil || row.AppID != in.AppId {
		return nil, model.ErrTokenInvalid
	}

	// 3. 重放判定先于一切可恢复错误：ROTATED 意味着这把 refresh 已经换过一次代，
	//    现在又回到签名方 —— 要么被截获，要么被恶意重试。唯一安全的处置是让整条 grant 失效。
	if row.State == model.TokenStateRotated {
		l.containReplay(row)
		return nil, model.ErrRefreshReused
	}
	switch row.State {
	case model.TokenStateActive:
	case model.TokenStateRevoked:
		return nil, model.ErrTokenRevoked
	case model.TokenStateExpired:
		return nil, model.ErrTokenExpired
	default:
		// 未知状态值：按不可用处理，不当成「状态没写就不限制」（AGENTS.md §9 不放宽）。
		return nil, model.ErrTokenInvalid
	}
	now := nowUnix()
	// 时间戳真值判定：state 的归档由 cron 惰性做，判定不能依赖它。
	if !row.RefreshUsable(now) {
		return nil, model.ErrTokenExpired
	}

	// 4. 撤销位点：必须读 DB（tokencheck.go 同一口径），缓存只能延后拒绝不能延后过期。
	grant, err := s.Grants.FindByID(ctx, row.GrantID)
	if err != nil {
		return nil, err
	}
	if grant == nil {
		// token 指向不存在的 grant 属于数据不一致，保守按已撤销处理而不是放行。
		return nil, model.ErrGrantRevoked
	}
	if !grant.TokenAccepted(row.Ctime) {
		return nil, model.ErrGrantRevoked
	}

	// 5. 应用门禁：停用/下线应用的 refresh 即使没过期也必须被拒（proto:39 APP_STATUS_SUSPENDED
	//    「token 与配额全部拒绝」）。
	app, err := findApp(ctx, s, row.AppID)
	if err != nil {
		return nil, err
	}
	if err := requireActiveApp(app); err != nil {
		return nil, err
	}

	// 6. scope 只能收窄。
	scopes, err := resolveRefreshScopes(ctx, s, in.Scope, row, grant)
	if err != nil {
		return nil, err
	}

	// 7. 轮换。
	issued, err := l.rotate(row, grant, scopes, now)
	if err != nil {
		return nil, err
	}

	// 8. 日志只记 ID 与结论：refresh 明文与哈希都不落（AGENTS.md §7）。
	logx.WithContext(ctx).Infof("open-platform: refresh 轮换 app_id=%d mid=%d grant_id=%d old_token_id=%d "+
		"new_token_id=%d scope=%d parent=%d", row.AppID, row.Mid, row.GrantID, row.TokenID,
		issued.tokenID, len(scopes), issued.row.ParentTokenID)
	return &rpc.RefreshAccessTokenReply{
		Token: projectTokenSet(issued.tokenID, issued.row.GrantID, issued.issuedAt,
			issued.accessExpiresAt, issued.refreshExpiresAt, scopes,
			issued.accessPlain, issued.refreshPlain),
		Rotated: true,
	}, nil
}

// rotate 单个事务内完成「旧行作废 → 新行签发 → 链头前移」。
//
// 三句必须原子：分开提交会留下「旧的已 ROTATED、新的没签发」的状态，
// 而旧行已不可再用，等于把用户整条授权打死（只能重新走 consent）。
// 并发防护是 Tokens.MarkRotated 的 state CAS（ACTIVE→ROTATED）：
// 同一把 refresh 并发刷新时只有一个事务能改到行，另一个 applied=false 整笔回滚，
// 因此「一把 refresh 只换出一代 token」是硬约束而不是概率事件。
func (l *RefreshAccessTokenLogic) rotate(row *model.Token, grant *model.Grant,
	scopes []string, now int64) (*issuedToken, error) {
	ctx, s := l.ctx, l.svcCtx
	if err := requireIssuable(s); err != nil {
		return nil, err
	}
	release, err := writePermit(ctx, s)
	if err != nil {
		return nil, err
	}
	defer release()

	var out *issuedToken
	err = s.DB.TransactCtx(ctx, func(tctx context.Context, session sqlx.Session) error {
		applied, err := s.Tokens.MarkRotated(tctx, session, row.TokenID, now, "rotated by refresh")
		if err != nil {
			return err
		}
		if !applied {
			return model.ErrConcurrentUpdate
		}
		token, err := issueToken(tctx, s, session, tokenSpec{
			grantID:   row.GrantID,
			appID:     row.AppID,
			mid:       row.Mid,
			grantType: model.GrantTypeRefreshToken,
			scopes:    scopes,
			// parent 指向被轮换的上一代：轮换链必须能从任一代往上追溯整条历史。
			parentTokenID: row.TokenID,
		})
		if err != nil {
			return err
		}
		head, err := s.Grants.TouchTokenHead(tctx, session, row.GrantID, grant.CurrentTokenID,
			token.tokenID, now)
		if err != nil {
			return err
		}
		if !head {
			// 链头已被并发的另一次签发/轮换前移：本次结论不确定，回滚让调用方重读后重试。
			return model.ErrConcurrentUpdate
		}
		out = token
		return nil
	})
	if err != nil {
		return nil, err
	}
	// 旧 access 的定位缓存主动失效（可选优化）：判定链读 DB，正确性不依赖这一步。
	dropIntrospection(ctx, s, row.AccessHash)
	return out, nil
}

// containReplay 检测到 refresh 重放后的收敛动作：撤销整条 grant（保守失效）。
//
// 为什么不能只拒绝这一次调用：重放说明这把 refresh 已经泄露，而持有它的人随时可以
// 再换一代新 token。唯一安全态是让该用户对该应用的全部签发历史同时失效。
// 双保险都写：逐条标记 op_token（含 ROTATED 链上的旧行）+ 前移 grant.revoked_at 位点，
// 位点保证并发漏网的 token 在 Introspect/Authorize 比对 Ctime<=RevokedAt 时被拒。
//
// 本函数的失败只写日志、不改变对调用方的答复：拒绝必须是确定结论，
// 「处置没做成」不能变成「refresh 看起来还能用」。
func (l *RefreshAccessTokenLogic) containReplay(row *model.Token) {
	ctx, s := l.ctx, l.svcCtx
	const reason = "refresh token reuse detected"
	now := nowUnix()

	release, err := writePermit(ctx, s)
	if err == nil {
		defer release()
		err = s.DB.TransactCtx(ctx, func(tctx context.Context, session sqlx.Session) error {
			if _, err := s.Tokens.RevokeByGrant(tctx, session, row.GrantID, now, reason); err != nil {
				return err
			}
			// operator=0：这是系统因重放证据触发的撤销，不是某个 mid 的操作（审计列如实区分）。
			if _, err := s.Grants.MarkRevoked(tctx, session, row.GrantID, 0, reason, now); err != nil {
				return err
			}
			return nil
		})
	}
	dropIntrospection(ctx, s, row.AccessHash)
	if err != nil {
		logx.WithContext(ctx).Errorf("open-platform: refresh 重放处置失败 grant_id=%d token_id=%d: %v",
			row.GrantID, row.TokenID, err)
		return
	}
	logx.WithContext(ctx).Errorf("open-platform: refresh 重放告警 app_id=%d mid=%d grant_id=%d token_id=%d "+
		"rotated_at=%d：整条 grant 已撤销", row.AppID, row.Mid, row.GrantID, row.TokenID, row.RotatedAt)
	// 通知订阅方（尽力而为，与 RevokeAuthorization 共用同一入队出口）。
	notifyGrantRevoked(ctx, s, row.AppID, row.GrantID, row.Mid, reason)
}

// resolveRefreshScopes 算出轮换后的 scope 快照：请求值必须是
// 「旧快照 ∩ 应用当前获批集」的子集；未传表示继承旧快照（proto:333）。
//
// 三条独立约束缺一不可：只比旧快照会漏掉「审批已回收的 scope 借刷新复活」；
// 只查获批集会漏掉「借刷新扩权」；目录侧的写权限同意校验则防止 grant 被改脏后
// 把写 scope 继续往下传。
func resolveRefreshScopes(ctx context.Context, s *svc.ServiceContext, want []string,
	row *model.Token, grant *model.Grant) ([]string, error) {
	prev := row.ScopeList()
	scopes, err := normalizeScopes(want)
	if err != nil {
		return nil, err
	}
	if len(scopes) == 0 {
		scopes = prev
	}
	if len(scopes) == 0 {
		// 旧快照为空说明这行本身就是脏数据：换出一把无权限点的 token 既无用也无法审计
		// 用户同意过什么，与 IssueAuthorizationCode 同一口径直接拒绝。
		return nil, errScopeRequiredForIssue
	}
	if err := rejectForbiddenScopes(scopes); err != nil {
		return nil, err
	}
	if !model.ScopesSubset(scopes, prev) {
		return nil, model.ErrScopeNarrowingDenied
	}
	if err := grantedSubset(ctx, s, row.AppID, scopes); err != nil {
		return nil, err
	}
	if grant.ConsentGiven != 1 {
		// 只在缺同意标记时才逐个查目录（正常链路 ConsentGiven=1，不产生 N 次读）。
		for _, sc := range scopes {
			if err := requireScopeConsent(ctx, s, sc, grant); err != nil {
				return nil, err
			}
		}
	}
	return scopes, nil
}
