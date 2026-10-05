package logic

import (
	"context"
	"strings"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// maxRevokeNoticePages 撤销通知枚举 grant 的分页上界。
//
// 为什么必须有上界：model 的集合化撤销（RevokeByMid）只回受影响行数、拿不到 grant_id 集合，
// 而 GRANT_REVOKED 通知的正文契约是「单个 grant_id」（见 webhookevent.go 的 notifyGrantRevoked）。
// 于是只能在撤销前有界地枚举一次。超上界时少发通知（记 WARN），
// 但撤销本身始终是完整的——通知是观测面，不能反过来把安全属性做成随数据规模退化。
const maxRevokeNoticePages = 10

type RevokeAuthorizationLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRevokeAuthorizationLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RevokeAuthorizationLogic {
	return &RevokeAuthorizationLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 撤销授权（写撤销位点，立即生效）。
//
// 契约依据：proto:342-361（RFC 7009 语义；生效方式=位点 + 逐条标记双保险）、
// proto:75-81（三种撤销目标，UNSPECIFIED 一律拒绝）。
func (l *RevokeAuthorizationLogic) RevokeAuthorization(in *rpc.RevokeAuthorizationReq) (*rpc.RevokeAuthorizationReply, error) {
	// 1. 目标与必填参数：proto:77 明确 UNSPECIFIED「服务端拒绝」，
	//    缺失参数也直接拒，不做「按能推就推」的宽松解释（猜出来的撤销范围比漏撤销更危险）。
	if !validRevokeTarget(in.Target) {
		return nil, errInvalidRevokeTarget
	}
	if in.OperatorMid <= 0 {
		// 撤销是破坏性操作，必须能落到具体的人（匿名撤销在审计上不可接受）。
		return nil, model.ErrOwnerRequired
	}
	// 运营代他人撤销必须有原因；用户撤销自己的授权可以省略（RFC 7009 不要求理由）。
	if in.IsOperator {
		if err := requireReason(in.Reason); err != nil {
			return nil, err
		}
	}
	switch in.Target {
	case rpc.RevokeTarget_REVOKE_TARGET_GRANT:
		if in.AppId <= 0 || in.Mid <= 0 {
			return nil, errInvalidRevokeTarget
		}
	case rpc.RevokeTarget_REVOKE_TARGET_USER_ALL:
		if in.Mid <= 0 {
			return nil, errInvalidRevokeTarget
		}
	case rpc.RevokeTarget_REVOKE_TARGET_TOKEN:
		if in.AppId <= 0 || (in.TokenId <= 0 && strings.TrimSpace(in.TokenHint) == "") {
			return nil, errInvalidRevokeTarget
		}
	}
	// 2. 身份门禁（GRANT/USER_ALL 可在读库前判定；TOKEN 要等定位到行才知道 mid）。
	if in.Target != rpc.RevokeTarget_REVOKE_TARGET_TOKEN && !in.IsOperator && in.Mid != in.OperatorMid {
		return nil, errOwnerMismatch
	}
	reason := revokeAuditReason(in.IsOperator, in.Reason)

	switch in.Target {
	case rpc.RevokeTarget_REVOKE_TARGET_TOKEN:
		return l.revokeOneToken(in, reason)
	case rpc.RevokeTarget_REVOKE_TARGET_GRANT:
		return l.revokeGrant(in, reason)
	default:
		return l.revokeUserAll(in, reason)
	}
}

// revokeOneToken 撤销单个 token（access 或 refresh）。
//
// 刻意不推翻 grant 位点：proto:78 的 TOKEN 语义是「撤销单个 token」，
// 动位点会把同一授权下其它仍在使用的凭证一起打死（用户侧表现为「莫名其妙被下线」）。
func (l *RevokeAuthorizationLogic) revokeOneToken(in *rpc.RevokeAuthorizationReq,
	reason string) (*rpc.RevokeAuthorizationReply, error) {
	ctx, s := l.ctx, l.svcCtx

	tok, err := locateRevokeToken(ctx, s, in.TokenId, in.TokenHint)
	if err != nil {
		return nil, err
	}
	// 跨应用/不存在的凭证回同一哨兵：可区分的回答等于一个「这个 token 值是否存在」探测接口。
	if tok == nil || tok.AppID != in.AppId {
		return nil, model.ErrTokenInvalid
	}
	if !in.IsOperator && tok.Mid != in.OperatorMid {
		return nil, errOwnerMismatch
	}

	release, err := writePermit(ctx, s)
	if err != nil {
		return nil, err
	}
	defer release()

	now := nowUnix()
	var applied bool
	err = s.DB.TransactCtx(ctx, func(tctx context.Context, session sqlx.Session) error {
		// MarkRevoked 的条件是 state IN (ACTIVE, ROTATED)：轮换链上的旧 refresh 也必须能作废，
		// 否则「已撤销」只覆盖当前代，重放窗口还留着。
		ok, err := s.Tokens.MarkRevoked(tctx, session, tok.TokenID, now, reason)
		if err != nil {
			return err
		}
		applied = ok
		return nil
	})
	if err != nil {
		return nil, err
	}

	// 幂等：第二次撤销 applied=false、tokens_revoked=0，仍回成功（撤销可重入，proto:342 RFC 7009）。
	// 审计留在首次的 op_token.revoke_reason/revoke 时间上，条件更新保证它不被重放覆盖。
	revoked := int32(0)
	if applied {
		revoked = 1
		// 主动失效定位缓存是可选优化：判定链读 DB，正确性只建立在 state 与位点比对上。
		dropIntrospection(ctx, s, tok.AccessHash)
	}
	logx.WithContext(ctx).Infof("open-platform: 撤销 token app_id=%d token_id=%d mid=%d grant_id=%d "+
		"revoked=%d operator_mid=%d is_operator=%t", in.AppId, tok.TokenID, tok.Mid, tok.GrantID,
		revoked, in.OperatorMid, in.IsOperator)
	return &rpc.RevokeAuthorizationReply{
		GrantsRevoked: 0,
		TokensRevoked: revoked,
		EffectiveAt:   now,
	}, nil
}

// revokeGrant 撤销「该用户对该应用」的全部授权：位点 + 逐条标记双保险同事务提交。
func (l *RevokeAuthorizationLogic) revokeGrant(in *rpc.RevokeAuthorizationReq,
	reason string) (*rpc.RevokeAuthorizationReply, error) {
	ctx, s := l.ctx, l.svcCtx

	grant, err := s.Grants.FindByAppMid(ctx, in.AppId, in.Mid)
	if err != nil {
		return nil, err
	}
	if grant == nil {
		// 「没有这条授权」是参数错（与「已撤销」区分：撤销可重入，不存在不可重入）。
		return nil, errGrantNotFound
	}
	if !in.IsOperator && grant.Mid != in.OperatorMid {
		return nil, errOwnerMismatch
	}

	release, err := writePermit(ctx, s)
	if err != nil {
		return nil, err
	}
	defer release()

	now := nowUnix()
	var (
		applied bool
		tokens  int64
	)
	err = s.DB.TransactCtx(ctx, func(tctx context.Context, session sqlx.Session) error {
		// 先逐条标记再写位点：token 侧先落可让并发中的 Introspect 更早看到 state=REVOKED，
		// 位点是第二道保险（覆盖并发漏网与将来新出现的读路径）。
		n, err := s.Tokens.RevokeByGrant(tctx, session, grant.GrantID, now, reason)
		if err != nil {
			return err
		}
		tokens = n
		ok, err := s.Grants.MarkRevoked(tctx, session, grant.GrantID, in.OperatorMid, reason, now)
		if err != nil {
			return err
		}
		applied = ok
		return nil
	})
	if err != nil {
		return nil, err
	}

	grants := int32(0)
	if applied {
		grants = 1
	}
	if applied || tokens > 0 {
		// 只在真的发生变更时通知：重复撤销不重发（event_id 固定为 grant-revoked:<id>，
		// 投递侧 uniq_event_endpoint 也会去重，这里只是省掉一次无谓的扫描）。
		notifyGrantRevoked(ctx, s, grant.AppID, grant.GrantID, grant.Mid, reason)
	}
	logx.WithContext(ctx).Infof("open-platform: 撤销授权 app_id=%d mid=%d grant_id=%d grants=%d tokens=%d "+
		"operator_mid=%d is_operator=%t", grant.AppID, grant.Mid, grant.GrantID, grants, tokens,
		in.OperatorMid, in.IsOperator)
	return &rpc.RevokeAuthorizationReply{
		GrantsRevoked: grants,
		TokensRevoked: int32(tokens),
		EffectiveAt:   now,
	}, nil
}

// revokeUserAll 撤销该用户的全部第三方授权（改密/风控一键下线）。
func (l *RevokeAuthorizationLogic) revokeUserAll(in *rpc.RevokeAuthorizationReq,
	reason string) (*rpc.RevokeAuthorizationReply, error) {
	ctx, s := l.ctx, l.svcCtx

	// 通知正文需要 grant_id 明细，而集合化撤销只回行数，因此撤销前先有界枚举一次。
	// 快照只用于「给谁发通知」，不参与撤销判定：撤销本身完全由 SQL 条件集合化完成，
	// 即使枚举为空也一个 token 都不会漏（这是本方法的安全属性与数据规模无关的前提）。
	targets, truncated := l.userGrantSnapshot(in.Mid)

	release, err := writePermit(ctx, s)
	if err != nil {
		return nil, err
	}
	defer release()

	now := nowUnix()
	var grants, tokens int64
	err = s.DB.TransactCtx(ctx, func(tctx context.Context, session sqlx.Session) error {
		n, err := s.Tokens.RevokeByMid(tctx, session, in.Mid, now, reason)
		if err != nil {
			return err
		}
		tokens = n
		g, err := s.Grants.RevokeByMid(tctx, session, in.Mid, in.OperatorMid, reason, now)
		if err != nil {
			return err
		}
		grants = g
		return nil
	})
	if err != nil {
		return nil, err
	}

	if grants > 0 {
		for _, g := range targets {
			notifyGrantRevoked(ctx, s, g.AppID, g.GrantID, g.Mid, reason)
		}
	}
	if truncated {
		logx.WithContext(ctx).Errorf("open-platform: mid=%d 授权数超过通知枚举上界，部分 GRANT_REVOKED 通知未发送"+
			"（撤销本身已完整执行）", in.Mid)
	}
	logx.WithContext(ctx).Infof("open-platform: 撤销用户全部第三方授权 mid=%d grants=%d tokens=%d "+
		"operator_mid=%d is_operator=%t", in.Mid, grants, tokens, in.OperatorMid, in.IsOperator)
	return &rpc.RevokeAuthorizationReply{
		GrantsRevoked: int32(grants),
		TokensRevoked: int32(tokens),
		EffectiveAt:   now,
	}, nil
}

// userGrantSnapshot 有界枚举该用户当前未撤销的授权（只为通知取 grant_id）。
// 返回 (列表, 是否因上界被截断)。
func (l *RevokeAuthorizationLogic) userGrantSnapshot(mid int64) ([]*model.Grant, bool) {
	ctx, s := l.ctx, l.svcCtx
	ps := s.Config.OpenPlatform.PageSize
	if ps <= 0 {
		ps = 20
	}
	var (
		out       []*model.Grant
		cursorT   int64
		cursorID  int64
		truncated bool
	)
	for page := 0; page < maxRevokeNoticePages; page++ {
		rows, err := s.Grants.ListByMid(ctx, mid, cursorT, cursorID, ps)
		if err != nil {
			// 通知枚举失败不影响撤销：这里只报「拿不到明细」，调用方继续执行主流程。
			logx.WithContext(ctx).Errorf("open-platform: mid=%d 授权明细枚举失败: %v", mid, err)
			return out, true
		}
		out = append(out, rows...)
		if int32(len(rows)) < ps {
			return out, false
		}
		last := rows[len(rows)-1]
		cursorT, cursorID = last.Mtime, last.GrantID
		truncated = true
	}
	return out, truncated
}

// locateRevokeToken 按 token_id 或 token_hint 定位待撤销的 token 行。
//
// token_hint 的明文只活在本请求栈内：算完 HMAC 就丢弃，不日志、不入事件、不回显（proto:350）。
// access 与 refresh 任一命中都算（FindAnyByHash）——只认 access 的话，
// 攻击者手上那把 refresh 就成了撤销不到的残留凭证。
func locateRevokeToken(ctx context.Context, s *svc.ServiceContext, tokenID int64,
	hint string) (*model.Token, error) {
	if tokenID > 0 {
		return s.Tokens.FindByID(ctx, tokenID)
	}
	plain := strings.TrimSpace(hint)
	if plain == "" {
		return nil, errInvalidRevokeTarget
	}
	hash, err := hashOfPlaintext(s, plain)
	if err != nil {
		return nil, err
	}
	return s.Tokens.FindAnyByHash(ctx, hash)
}

// validRevokeTarget 撤销目标枚举守卫：proto3 的 0 是「未指定」，必须显式拒绝，
// 否则一个忘记填 target 的调用会被当成某一种撤销执行。
func validRevokeTarget(v rpc.RevokeTarget) bool {
	switch v {
	case rpc.RevokeTarget_REVOKE_TARGET_TOKEN,
		rpc.RevokeTarget_REVOKE_TARGET_GRANT,
		rpc.RevokeTarget_REVOKE_TARGET_USER_ALL:
		return true
	default:
		return false
	}
}

// revokeAuditReason 归一撤销原因（审计列 VARCHAR(255)）。
// 用户自撤销允许省略理由，但库里不能落空串——否则「这次撤销是谁的什么操作」无法回答。
func revokeAuditReason(isOperator bool, raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		if isOperator {
			return "revoked by operator"
		}
		return "revoked by user"
	}
	return clipRunes(v, maxReasonRunes)
}
