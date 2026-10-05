package repository

// 本文件实现管理员会话（token）的签发、校验与吊销。
//
// 方案与 account 的用户会话一致：随机 token + 落库为事实源 + Redis 短缓存，
// 但命名空间完全独立（token 前缀 adm_、缓存 key op:sess:、表 op_admin_session），
// 用户端 token 与后台 token 互不通用（AGENTS.md §6：管理员字段不得混入客户端）。
// token 末尾始终带 HMAC 摘要，摘要密钥只能来自配置指向的环境变量；密钥缺失时
// 拒绝签发（model.ErrTokenSecretMissing）而不是退化成无摘要的弱 token，
// 这样打库前的本地格式校验这条防线不会被配置疏漏绕过；真正的授权依据仍是库中会话。

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"go-video/services/operation/model"
)

const (
	// adminTokenPrefix 后台 token 前缀，与用户端 token 明确区分。
	adminTokenPrefix = "adm"
	// defaultIssuer 未配置 Issuer 时使用的标识。
	defaultIssuer = "op"
	// sessionCacheTTLSec 会话缓存秒数：吊销最迟该间隔内失效（禁用账号会同时吊销会话并置 state，
	// 权限快照另有 State 兜底，见 rbac.go 的 VerifyPermission）。
	sessionCacheTTLSec = 300
	// tokenRandomBytes 随机段长度。
	tokenRandomBytes = 24
	// tokenSigBytes 摘要截断长度。
	tokenSigBytes = 8
)

// SessionConf 后台会话配置（由 etc/AdminSession 映射而来）。
type SessionConf struct {
	// TokenTTL token 有效秒数。
	TokenTTL int64
	// Issuer 签发方标识，参与 token 前缀，便于多环境隔离。
	Issuer string
	// Secret 签名密钥；由 TokenSecretRef 指向的环境变量注入，不落配置文件。
	// 必填：为空时 newToken/parseToken 一律返回 model.ErrTokenSecretMissing。
	Secret []byte
}

// LoginConf 防爆破配置（由 etc/Login 映射而来）。
type LoginConf struct {
	// MaxFail 连续失败多少次后锁定。
	MaxFail int32
	// LockMinutes 锁定时长（分钟）。
	LockMinutes int64
}

// issuer 返回归一化后的签发方标识（不含下划线，避免破坏 token 分段）。
func (s SessionConf) issuer() string {
	iss := strings.NewReplacer("_", "-", " ", "").Replace(strings.TrimSpace(s.Issuer))
	if iss == "" {
		return defaultIssuer
	}
	return iss
}

// ttl 返回 token 有效秒数，缺省 2 小时（后台会话应显著短于用户端 30 天）。
func (s SessionConf) ttl() int64 {
	if s.TokenTTL > 0 {
		return s.TokenTTL
	}
	return 7200
}

// randomHex 生成 n 字节随机数的 hex 字符串。
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("operation/session: crypto/rand unavailable: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// signTokenBody 计算 token 摘要段。
// 密钥缺失时返回空串，但 newToken/parseToken 都会先拒绝这种配置（见 requireSecret），
// 因此这里不再存在“无摘要 token”的合法路径。
func (s SessionConf) signTokenBody(body string) string {
	if len(s.Secret) == 0 {
		return ""
	}
	mac := hmac.New(sha256.New, s.Secret)
	mac.Write([]byte(adminTokenPrefix + "." + s.issuer() + "." + body))
	return hex.EncodeToString(mac.Sum(nil))[:tokenSigBytes*2]
}

// requireSecret 校验摘要密钥已配置。
// 密钥只能来自配置/环境变量（svc.loadTokenSecret），缺失时不签发不带摘要的弱 token，
// 直接返回 model.ErrTokenSecretMissing，让部署问题在登录第一次调用时就暴露。
func (s SessionConf) requireSecret() error {
	if len(s.Secret) == 0 {
		return model.ErrTokenSecretMissing
	}
	return nil
}

// newToken 生成后台 token：adm_<issuer>_<random>_<hmac 摘要>。
func (s SessionConf) newToken() (string, error) {
	if err := s.requireSecret(); err != nil {
		return "", err
	}
	body, err := randomHex(tokenRandomBytes)
	if err != nil {
		return "", err
	}
	return adminTokenPrefix + "_" + s.issuer() + "_" + body + "_" + s.signTokenBody(body), nil
}

// parseToken 拆解 token 并校验摘要；不查库，因此只能作为“明显非法”的快速拒绝路径。
// 未配置密钥时一律拒绝：既签发不了合法 token，也不接受任何来路不明的 token。
func (s SessionConf) parseToken(token string) (body string, ok bool) {
	if token == "" || s.requireSecret() != nil {
		return "", false
	}
	parts := strings.Split(token, "_")
	if len(parts) != 4 || parts[0] != adminTokenPrefix || parts[1] != s.issuer() {
		return "", false
	}
	want := s.signTokenBody(parts[2])
	if want == "" || !hmac.Equal([]byte(want), []byte(parts[3])) {
		return "", false
	}
	return parts[2], true
}

// issueSession 签发后台会话：写库（事实源）+ 回填缓存。
func (r *Repository) issueSession(ctx context.Context, adminID int64, actor Actor) (*model.AdminSession, error) {
	token, err := r.session.newToken()
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	sess := &model.AdminSession{
		Token:     token,
		AdminID:   adminID,
		Expires:   now + r.session.ttl(),
		State:     model.SessionStateActive,
		IPHash:    ipHash(actor.IP),
		UserAgent: redactUserAgent(actor.UserAgent),
		Ctime:     now,
	}
	if err := r.sessionMd.Insert(ctx, sess); err != nil {
		return nil, fmt.Errorf("operation: insert admin session: %w", err)
	}
	r.setSessionCache(ctx, sess)
	return sess, nil
}

// sessionCacheKey 由 token 派生缓存 key。
func sessionCacheKey(token string) string {
	return fmt.Sprintf(keySession, token)
}

// setSessionCache 回填会话缓存，TTL 不超过剩余有效期。
func (r *Repository) setSessionCache(ctx context.Context, sess *model.AdminSession) {
	remain := sess.Expires - nowUnix()
	ttl := sessionCacheTTLSec
	if remain > 0 && remain < int64(ttl) {
		ttl = int(remain)
	}
	if ttl <= 0 {
		return
	}
	r.cache.setJSON(ctx, sessionCacheKey(sess.Token), sess, ttl)
}

// delSessionCache 删除会话缓存（吊销立即生效）。
func (r *Repository) delSessionCache(ctx context.Context, token string) {
	r.cache.del(ctx, sessionCacheKey(token))
}

// sessionByToken 按 token 取有效会话：先做本地摘要校验，再查缓存，最后回源库。
func (r *Repository) sessionByToken(ctx context.Context, token string) (*model.AdminSession, error) {
	if _, ok := r.session.parseToken(token); !ok {
		return nil, model.ErrSessionInvalid
	}
	var cached model.AdminSession
	if r.cache.getJSON(ctx, sessionCacheKey(token), &cached) && cached.Token == token {
		if cached.State == model.SessionStateActive && nowUnix() < cached.Expires {
			return &cached, nil
		}
		return nil, model.ErrSessionInvalid
	}
	sess, err := r.sessionMd.FindByToken(ctx, token)
	if err != nil {
		return nil, err
	}
	if sess == nil || sess.State != model.SessionStateActive || nowUnix() >= sess.Expires {
		return nil, model.ErrSessionInvalid
	}
	r.setSessionCache(ctx, sess)
	return sess, nil
}

// ResolveAdminID 从 token 或显式 admin_id 解析出操作者身份。
// token 优先：网关已做过一次鉴权时可以直接传 admin_id，减少一次会话查询。
func (r *Repository) ResolveAdminID(ctx context.Context, token string, adminID int64) (int64, error) {
	if strings.TrimSpace(token) != "" {
		sess, err := r.sessionByToken(ctx, token)
		if err != nil {
			return 0, err
		}
		return sess.AdminID, nil
	}
	if adminID > 0 {
		return adminID, nil
	}
	return 0, model.ErrSessionInvalid
}

// RevokeSession 吊销单个会话（登出）。
func (r *Repository) RevokeSession(ctx context.Context, token string) error {
	if err := r.sessionMd.Revoke(ctx, token); err != nil {
		return err
	}
	r.delSessionCache(ctx, token)
	return nil
}

// RevokeAllSessions 吊销某管理员的全部会话（禁用账号、重置口令时调用），
// 并作废其权限快照，保证“改权限即断旧登录态”的收敛语义。
func (r *Repository) RevokeAllSessions(ctx context.Context, adminID int64) error {
	if err := r.sessionMd.RevokeAllByAdmin(ctx, adminID); err != nil {
		return err
	}
	r.invalidateRBAC(ctx)
	return nil
}
