package logic

import (
	"context"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"

	"github.com/zeromicro/go-zero/core/logx"
)

// access token 判定链的唯一实现，IntrospectToken 与 AuthorizeRequest 共用。
//
// 为什么必须共用：这两个方法是同一个安全属性的两个入口（「这次调用能不能放行」）。
// 各写一套就会漂移——例如 Authorize 忘了比对 grant 位点，撤销便只在 introspect 面生效，
// 攻击者换一个入口即可绕过。因此本文件之外的地方一律不得再实现 token 判定。
//
// 判定一律以 DB 真值为准：Cache 只缓存 access_hash→token_id 的「定位结果」，
// 不缓存结论。这样缓存最多让一次拒绝晚 IntrospectCacheSeconds 秒到达，
// 绝不会让已撤销的凭证重新可用（Config.Validate 已锁 IntrospectCacheSeconds<=AccessTokenTTL）。

// accessCheck 判定链输出。active=false 时 deny 必为非空固定原因码。
type accessCheck struct {
	token   *model.Token
	grant   *model.Grant
	app     *model.Application
	active  bool
	deny    string
	scopes  []string
	expires int64
}

// denyReply 组装「不通过」结论（业务结果，不是 gRPC 错误）。
func (a *accessCheck) denyReply(reason string) *accessCheck {
	a.active = false
	a.deny = reason
	return a
}

// locateToken 按 access 明文（走定位缓存）或 token_id 取 token 行。
// 返回 (nil, nil) 表示「取不到」，调用方统一按 inactive 处理。
func locateToken(ctx context.Context, s *svc.ServiceContext, accessToken string,
	tokenID int64) (*model.Token, string, error) {
	if accessToken != "" {
		pepper, err := credentialPepper(s)
		if err != nil {
			return nil, "", err
		}
		hash, err := model.HashCredential(pepper, accessToken)
		if err != nil {
			return nil, "", err
		}
		if id := lookupTokenID(ctx, s, hash); id > 0 {
			tok, err := s.Tokens.FindByID(ctx, id)
			if err != nil {
				// 定位缓存指向已被清理的行时回到 DB 真值，不因此判失败。
				logx.WithContext(ctx).Errorf("open-platform: 定位缓存回读失败 token_id=%d: %v", id, err)
			} else if tok != nil && tok.AccessHash == hash {
				// 缓存值必须与重算哈希一致才可信：不一致说明 hash 撞了主键位点（物理上不可能，
				// 但这是缓存被外部改写后的唯一发现手段），一律回 DB 重查。
				return tok, hash, nil
			}
		}
		tok, err := s.Tokens.FindByAccessHash(ctx, hash)
		if err != nil {
			return nil, "", err
		}
		return tok, hash, nil
	}
	if tokenID > 0 {
		tok, err := s.Tokens.FindByID(ctx, tokenID)
		return tok, "", err
	}
	return nil, "", nil
}

// evaluateAccessToken 执行完整判定链。
//
// requiredScope 为空时跳过 scope 判定（纯 introspect）；非空时同时校验
// 「token 快照含该 scope」「应用当前仍获批该 scope」「写 scope 有用户同意」。
//
// 返回的 error 只表达依赖故障（DB/配置），业务不通过一律走 active=false。
func evaluateAccessToken(ctx context.Context, s *svc.ServiceContext, accessToken string,
	tokenID int64, requiredScope string) (*accessCheck, error) {
	res := &accessCheck{}
	if s.Tokens == nil {
		return nil, model.ErrSecretVerificationUnavailable
	}
	tok, accessHash, err := locateToken(ctx, s, accessToken, tokenID)
	if err != nil {
		return nil, err
	}
	if tok == nil {
		return res.denyReply(denyInactive), nil
	}
	res.token = tok
	res.scopes = tok.ScopeList()
	res.expires = tok.AccessExpiresAt

	// a. 行状态：REVOKED/ROTATED 一律 revoked；EXPIRED 只是惰性归档值，
	//    真判定在 b 的时间戳比对，这里仅回更显眼的原因码。
	switch tok.State {
	case model.TokenStateRevoked:
		return res.denyReply(denyRevoked), nil
	case model.TokenStateRotated:
		// 轮换后的旧 access 必须立即不可用：否则「refresh 已换新」的同时旧 access 还能打。
		return res.denyReply(denyRevoked), nil
	case model.TokenStateExpired:
		return res.denyReply(denyExpired), nil
	case model.TokenStateActive:
	default:
		return res.denyReply(denyInactive), nil
	}

	now := nowUnix()
	// b. 过期：以时间戳真值判定，不依赖归档任务是否跑过。
	if tok.AccessExpiresAt <= now {
		return res.denyReply(denyExpired), nil
	}

	// c. 撤销位点：必须读 DB（见文件头注释）。
	grant, err := s.Grants.FindByID(ctx, tok.GrantID)
	if err != nil {
		return nil, err
	}
	if grant == nil {
		// token 指向不存在的 grant 属于数据不一致，保守拒绝而不是放行。
		return res.denyReply(denyRevoked), nil
	}
	res.grant = grant
	if !grant.TokenAccepted(tok.Ctime) {
		return res.denyReply(denyRevoked), nil
	}

	// d. 应用状态：停用/下线应用的 token 立即不可用。
	app, err := s.Apps.FindByID(ctx, tok.AppID)
	if err != nil {
		return nil, err
	}
	if app == nil {
		return res.denyReply(denyInactive), nil
	}
	res.app = app
	if err := requireActiveApp(app); err != nil {
		return res.denyReply(denyAppSuspended), nil
	}

	// e. scope：token 快照 ∩ 应用当前获批集。审批被回收后，此前签发的 token 也必须失效。
	if requiredScope != "" {
		if !model.ContainsScope(res.scopes, requiredScope) {
			return res.denyReply(denyScopeMissing), nil
		}
		granted, err := s.AppScopes.FindGrantedScopes(ctx, tok.AppID, []string{requiredScope})
		if err != nil {
			return nil, err
		}
		if !model.ContainsScope(granted, requiredScope) {
			return res.denyReply(denyScopeMissing), nil
		}
		if err := requireScopeConsent(ctx, s, requiredScope, grant); err != nil {
			return res.denyReply(denyConsentMissing), nil
		}
	}

	res.active = true
	res.deny = denyNone
	if accessHash != "" {
		rememberTokenID(ctx, s, accessHash, tok.TokenID, tok.AccessExpiresAt, now)
	}
	return res, nil
}

// requireScopeConsent 写权限必须有用户同意记录（grant.consent_given=1）。
// 目录里该 scope 不是写权限时直接放行；目录缺项按未知处理（不放宽）。
func requireScopeConsent(ctx context.Context, s *svc.ServiceContext, scope string,
	grant *model.Grant) error {
	def, err := s.Scopes.FindByScope(ctx, scope)
	if err != nil {
		return err
	}
	if def == nil || !def.IsWrite() {
		return nil
	}
	if grant == nil || grant.ConsentGiven != 1 {
		return model.ErrScopeWriteRequiresConsent
	}
	return nil
}
