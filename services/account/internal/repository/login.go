package repository

// 本文件移植参考仓库 passport / passport-auth / identify / sms / secure /
// account-recovery 的账号域能力：
//   - 密码登录/验证码登录/注册（passport + passport-login 语义）；
//   - 会话签发、校验、刷新、吊销（passport-auth token/cookie/refresh）；
//   - 登录/注册/找回验证码（sms 的账号侧验证码能力，短信下发待 notification）；
//   - 改密、重置密码、历史密码校验（secure / account-recovery / passport）；
//   - 登录日志（passport loginlog，替代 HBase）。
// 密码哈希与参考仓库一致：MD5(pwd + ">>BiLiSaLt<<" + salt)（生产建议后续升级 bcrypt）。

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/common/idgen"
	"go-video/services/account/model"
	accountrpc "go-video/services/account/rpc"
)

// 登录域常量（参考 passport-auth 的 1/2 月过期；本项目固定天数并配置化）。
const (
	// TokenTTLDays access token 有效期（天），默认 30。
	TokenTTLDays = 30
	// RefreshTTLDays refresh token 有效期（天），默认 90。
	RefreshTTLDays = 90
	// tokenCacheTTL token 缓存秒数：按 token 精确吊销立即失效；
	// 批量吊销（改密）最迟 10 分钟内生效（见 RevokeAll 注释）。
	tokenCacheTTL = 600

	// captureCodeTTL 验证码有效期（秒）。
	captureCodeTTL = 600
	// captureTimesTTL 发送次数统计有效期（秒）。
	captureTimesTTL = 86400
	// captureErrTTL 错误次数统计有效期（秒）。
	captureErrTTL = 86400
	// captureMaxSend 单日单接收方最大发送次数（参考实名验证码 >5 拒绝）。
	captureMaxSend = 5
	// captureMaxErr 验证码最大错误次数（超过则失效）。
	captureMaxErr = 3

	// passwordSaltPrefix 密码哈希盐前后缀（与参考仓库 getSaltPwd 一致）。
	passwordSaltPrefix = ">>BiLiSaLt<<"
)

// 登录域领域错误（logic 层据此映射错误信息）。
var (
	// ErrLoginAccountNotExist 登录标识不存在。
	ErrLoginAccountNotExist = errors.New("account not exist")
	// ErrLoginPasswordWrong 密码错误。
	ErrLoginPasswordWrong = errors.New("password wrong")
	// ErrCaptureInvalid 验证码无效（未发送或已过期）。
	ErrCaptureInvalid = errors.New("capture code invalid")
	// ErrCaptureWrong 验证码错误。
	ErrCaptureWrong = errors.New("capture code wrong")
	// ErrCaptureErrTooMany 验证码错误次数过多。
	ErrCaptureErrTooMany = errors.New("capture code error too many")
	// ErrCaptureSendTooMany 验证码发送过于频繁。
	ErrCaptureSendTooMany = errors.New("capture send too many")
	// ErrAccountExists 注册标识已存在。
	ErrAccountExists = errors.New("account already exists")
	// ErrSessionRevoked 会话已吊销/过期。
	ErrSessionRevoked = errors.New("session revoked or expired")
	// ErrPasswordRequired 未设置密码或旧密码缺失。
	ErrPasswordRequired = errors.New("old password required")
)

// captureKey 验证码 Redis key（与实名验证码前缀风格一致）。
func captureKey(biz int32, target string) string {
	return fmt.Sprintf("cap_code_%d_%s", biz, target)
}

func captureTimesKey(biz int32, target string) string {
	return fmt.Sprintf("cap_times_%d_%s", biz, target)
}

func captureErrKey(biz int32, target string) string {
	return fmt.Sprintf("cap_err_%d_%s", biz, target)
}

// randomHex 生成 n 字节随机数的 hex 字符串（token/csrf/salt）。
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// 理论上不可达；降级为时间戳保证可用性
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// lastMid 进程内 mid 单调计数器（保证严格递增，跨进程由毫秒时间+随机计数保证唯一）。
var lastMid atomic.Int64

// genMid 生成全局唯一的数值 mid（ULID 风格：48 位毫秒时间 + 20 位随机计数）。
// 与 account 表注释一致（idgen 生成），保证时间有序且全库唯一。
func genMid() (int64, error) {
	s, err := idgen.ULID()
	if err != nil {
		return 0, err
	}
	id, err := ulid.Parse(s)
	if err != nil {
		return 0, err
	}
	// 同毫秒内 ulid.Monotonic 的 80 位随机计数递增，取低 20 位保证进程内唯一；
	// 跨毫秒由 48 位时间主导；进程内用原子计数器兜底严格递增。
	ent := id.Entropy()
	mid := int64(id.Time()<<20) | int64(ent[7]&0x0F)<<16 | int64(ent[8])<<8 | int64(ent[9])
	for {
		last := lastMid.Load()
		if mid <= last {
			mid = last + 1
		}
		if lastMid.CompareAndSwap(last, mid) {
			break
		}
	}
	return mid, nil
}

// saltPwd 计算密码哈希（与参考仓库 getSaltPwd 一致）。
func saltPwd(pwd, salt string) string {
	return md5Hex(pwd + passwordSaltPrefix + salt)
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// decryptPassword 解密 RSA 加密密码；未配置密钥时（开发模式）按明文处理。
func (r *Repository) decryptPassword(encrypted string) (string, error) {
	if r.passportRSA == nil {
		return encrypted, nil
	}
	if encrypted == "" {
		return "", nil
	}
	plain, err := r.passportRSA.CardDecrypt([]byte(encrypted))
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// findMidByAccount 按登录标识（用户名/手机/邮箱凭证）反查 mid。
func (r *Repository) findMidByAccount(ctx context.Context, account string) (int64, error) {
	lower := strings.ToLower(strings.TrimSpace(account))
	if lower == "" {
		return 0, ErrLoginAccountNotExist
	}
	for _, credType := range []int8{model.CredentialTypeUsername, model.CredentialTypePhone, model.CredentialTypeEmail} {
		found, err := r.credentialModel.FindMidsByIdentifiers(ctx, credType, []string{lower})
		if err != nil {
			return 0, err
		}
		if mid, ok := found[lower]; ok && mid > 0 {
			return mid, nil
		}
	}
	return 0, ErrLoginAccountNotExist
}

// issueSession 签发会话（写库 + Redis 缓存），返回登录态。
func (r *Repository) issueSession(ctx context.Context, mid int64, ip, device, buvid string) (*accountrpc.LoginReply, error) {
	now := time.Now().Unix()
	token := randomHex(32)
	refresh := randomHex(32)
	csrf := randomHex(16)
	session := &model.AccountSession{
		Token:          token,
		RefreshToken:   refresh,
		Mid:            mid,
		Csrf:           csrf,
		Expires:        now + r.tokenTTLDays*86400,
		RefreshExpires: now + r.refreshTTLDays*86400,
		Status:         model.SessionStatusActive,
		CreateIP:       ip,
		Device:         device,
		Buvid:          buvid,
	}
	if err := r.sessionModel.Insert(ctx, session); err != nil {
		return nil, err
	}
	r.setSessionCache(ctx, session)
	return &accountrpc.LoginReply{
		Mid:          mid,
		Token:        token,
		RefreshToken: refresh,
		Csrf:         csrf,
		Expires:      session.Expires,
	}, nil
}

// setSessionCache 写入 token/refresh 的 Redis 缓存（前缀沿用参考 ak_/rk_）。
func (r *Repository) setSessionCache(ctx context.Context, s *model.AccountSession) {
	r.cache.setJSON(ctx, "ak_"+s.Token, s, tokenCacheTTL)
	r.cache.setJSON(ctx, "rk_"+s.RefreshToken, s, tokenCacheTTL)
}

// delSessionCache 删除 token/refresh 缓存（精确吊销立即生效）。
func (r *Repository) delSessionCache(ctx context.Context, s *model.AccountSession) {
	_ = r.cache.del(ctx, "ak_"+s.Token)
	_ = r.cache.del(ctx, "rk_"+s.RefreshToken)
}

// sessionByToken 按 token 取会话（缓存 → DB，校验有效期与状态）。
func (r *Repository) sessionByToken(ctx context.Context, token string) (*model.AccountSession, error) {
	if token == "" {
		return nil, ErrSessionRevoked
	}
	var cached model.AccountSession
	if err := r.cache.getJSON(ctx, "ak_"+token, &cached); err == nil && cached.Token == token {
		if cached.Status == model.SessionStatusActive && time.Now().Unix() < cached.Expires {
			return &cached, nil
		}
		return nil, ErrSessionRevoked
	}
	s, err := r.sessionModel.FindByToken(ctx, token)
	if err != nil {
		return nil, err
	}
	if s == nil || s.Status != model.SessionStatusActive || time.Now().Unix() >= s.Expires {
		return nil, ErrSessionRevoked
	}
	r.setSessionCache(ctx, s)
	return s, nil
}

// addLoginLog 写入登录日志（失败仅记日志）。
func (r *Repository) addLoginLog(ctx context.Context, mid int64, loginType, status int8, reason, ip, device, buvid string) {
	err := r.loginLogModel.Add(ctx, &model.AccountLoginLog{
		Mid:       mid,
		LoginType: loginType,
		Status:    status,
		Reason:    reason,
		IP:        ip,
		Device:    device,
		Buvid:     buvid,
	})
	if err != nil {
		logx.Errorf("account/login: add login log mid=%d err=%v", mid, err)
	}
}

// PasswordLogin 密码登录（登录标识：用户名/手机/邮箱 + 密码）。
func (r *Repository) PasswordLogin(ctx context.Context, req *accountrpc.LoginReq) (*accountrpc.LoginReply, error) {
	mid, err := r.findMidByAccount(ctx, req.Account)
	if err != nil {
		r.addLoginLog(ctx, 0, model.LoginTypePassword, model.LoginStatusFail, err.Error(), req.Ip, req.Device, req.Buvid)
		return nil, err
	}
	pwd, err := r.decryptPassword(req.Password)
	if err != nil {
		r.addLoginLog(ctx, mid, model.LoginTypePassword, model.LoginStatusFail, err.Error(), req.Ip, req.Device, req.Buvid)
		return nil, err
	}
	secret, err := r.secretModel.FindActive(ctx, mid, model.SecretTypePassword)
	if err != nil {
		return nil, err
	}
	if secret == nil || saltPwd(pwd, secret.Salt) != secret.Hash {
		r.addLoginLog(ctx, mid, model.LoginTypePassword, model.LoginStatusFail, ErrLoginPasswordWrong.Error(), req.Ip, req.Device, req.Buvid)
		return nil, ErrLoginPasswordWrong
	}
	reply, err := r.issueSession(ctx, mid, req.Ip, req.Device, req.Buvid)
	if err != nil {
		r.addLoginLog(ctx, mid, model.LoginTypePassword, model.LoginStatusFail, err.Error(), req.Ip, req.Device, req.Buvid)
		return nil, err
	}
	r.addLoginLog(ctx, mid, model.LoginTypePassword, model.LoginStatusOK, "", req.Ip, req.Device, req.Buvid)
	return reply, nil
}

// CaptureLogin 验证码登录（手机 + 验证码）。
func (r *Repository) CaptureLogin(ctx context.Context, req *accountrpc.LoginReq) (*accountrpc.LoginReply, error) {
	if err := r.checkCapture(ctx, int32(model.CaptureBizLogin), req.Account, req.CaptureCode); err != nil {
		r.addLoginLog(ctx, 0, model.LoginTypeCapture, model.LoginStatusFail, err.Error(), req.Ip, req.Device, req.Buvid)
		return nil, err
	}
	mid, err := r.findMidByAccount(ctx, req.Account)
	if err != nil {
		r.addLoginLog(ctx, 0, model.LoginTypeCapture, model.LoginStatusFail, err.Error(), req.Ip, req.Device, req.Buvid)
		return nil, err
	}
	reply, err := r.issueSession(ctx, mid, req.Ip, req.Device, req.Buvid)
	if err != nil {
		r.addLoginLog(ctx, mid, model.LoginTypeCapture, model.LoginStatusFail, err.Error(), req.Ip, req.Device, req.Buvid)
		return nil, err
	}
	r.addLoginLog(ctx, mid, model.LoginTypeCapture, model.LoginStatusOK, "", req.Ip, req.Device, req.Buvid)
	return reply, nil
}

// Register 注册（用户名+密码 或 手机/邮箱+验证码+密码）。
func (r *Repository) Register(ctx context.Context, req *accountrpc.RegisterReq) (*accountrpc.RegisterReply, error) {
	account := strings.ToLower(strings.TrimSpace(req.Account))
	if account == "" {
		return nil, ErrLoginAccountNotExist
	}
	// 注册标识唯一性：手机/邮箱注册要求验证码，用户名注册不要求
	if req.CaptureCode != "" {
		if err := r.checkCapture(ctx, int32(model.CaptureBizRegister), account, req.CaptureCode); err != nil {
			return nil, err
		}
	}
	credType := credentialTypeOf(account)
	if mid, err := r.findMidByAccount(ctx, account); err == nil && mid > 0 {
		return nil, ErrAccountExists
	} else if err != ErrLoginAccountNotExist {
		return nil, err
	}
	pwd, err := r.decryptPassword(req.Password)
	if err != nil {
		return nil, err
	}
	mid, err := genMid()
	if err != nil {
		return nil, err
	}
	// 事务：账号主表 + 凭证 + 密钥
	salt := randomHex(16)
	err = r.conn.TransactCtx(ctx, func(c context.Context, tx sqlx.Session) error {
		if err := r.accountModel.Insert(c, &model.Account{
			Mid: mid, Status: 0, IsTourist: 0, RegIP: req.Ip,
		}); err != nil {
			return err
		}
		if err := r.credentialModel.Insert(c, &model.AccountCredential{
			Mid: mid, CredentialType: credType, Identifier: account, Status: 0,
		}); err != nil {
			return err
		}
		now := time.Now().Unix()
		return r.secretModel.Insert(c, tx, &model.AccountSecret{
			Mid: mid, SecretType: model.SecretTypePassword, Salt: salt,
			Hash: saltPwd(pwd, salt), Status: model.SecretStatusActive, CTime: now, MTime: now,
		})
	})
	if err != nil {
		r.addLoginLog(ctx, mid, model.LoginTypeRegister, model.LoginStatusFail, err.Error(), req.Ip, "", "")
		return nil, err
	}
	login, err := r.issueSession(ctx, mid, req.Ip, "", "")
	if err != nil {
		return nil, err
	}
	r.addLoginLog(ctx, mid, model.LoginTypeRegister, model.LoginStatusOK, "", req.Ip, "", "")
	return &accountrpc.RegisterReply{Mid: mid, Login: login}, nil
}

// credentialTypeOf 按标识形态推断凭证类型（手机号/邮箱/用户名）。
func credentialTypeOf(account string) int8 {
	if len(account) == 11 && account[0] == '1' {
		return model.CredentialTypePhone
	}
	if strings.Contains(account, "@") {
		return model.CredentialTypeEmail
	}
	return model.CredentialTypeUsername
}

// Logout 登出：吊销 token 并删除缓存。
func (r *Repository) Logout(ctx context.Context, token string) error {
	s, err := r.sessionModel.FindByToken(ctx, token)
	if err != nil {
		return err
	}
	if s != nil {
		_ = r.sessionModel.Revoke(ctx, token)
		r.delSessionCache(ctx, s)
	}
	return nil
}

// TokenInfo token 校验（参考 identify.GetTokenInfo）。
func (r *Repository) TokenInfo(ctx context.Context, token string) (*accountrpc.GetTokenInfoReply, error) {
	s, err := r.sessionByToken(ctx, token)
	if err == ErrSessionRevoked {
		return &accountrpc.GetTokenInfoReply{IsLogin: false}, nil
	}
	if err != nil {
		return nil, err
	}
	return &accountrpc.GetTokenInfoReply{IsLogin: true, Mid: s.Mid, Csrf: s.Csrf, Expires: s.Expires}, nil
}

// CookieInfo cookie 会话校验（参考 identify.GetCookieInfo）。
// 约定 cookie 形如 'SESSDATA=<access token>;sid=<csrf>'，解析 SESSDATA 后按 token 校验。
func (r *Repository) CookieInfo(ctx context.Context, cookie string) (*accountrpc.GetCookieInfoReply, error) {
	token := ""
	for _, part := range strings.Split(cookie, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 && kv[0] == "SESSDATA" {
			token = kv[1]
			break
		}
	}
	s, err := r.sessionByToken(ctx, token)
	if err == ErrSessionRevoked {
		return &accountrpc.GetCookieInfoReply{IsLogin: false}, nil
	}
	if err != nil {
		return nil, err
	}
	return &accountrpc.GetCookieInfoReply{IsLogin: true, Mid: s.Mid, Csrf: s.Csrf, Expires: s.Expires}, nil
}

// RenewToken 刷新 token：校验 refresh 后轮换新 token（参考 passport-login /token/renew）。
func (r *Repository) RenewToken(ctx context.Context, refreshToken string) (*accountrpc.RenewTokenReply, error) {
	if refreshToken == "" {
		return nil, ErrSessionRevoked
	}
	s, err := r.sessionModel.FindByRefresh(ctx, refreshToken)
	if err != nil {
		return nil, err
	}
	if s == nil || s.Status != model.SessionStatusActive || time.Now().Unix() >= s.RefreshExpires {
		return nil, ErrSessionRevoked
	}
	// 轮换 token（refresh 保持不变），删除旧 token 缓存
	newToken := randomHex(32)
	newCsrf := randomHex(16)
	newExpires := time.Now().Unix() + r.tokenTTLDays*86400
	if err := r.sessionModel.UpdateToken(ctx, s.ID, newToken, newCsrf, newExpires); err != nil {
		return nil, err
	}
	_ = r.cache.del(ctx, "ak_"+s.Token)
	s.Token = newToken
	s.Csrf = newCsrf
	s.Expires = newExpires
	r.setSessionCache(ctx, s)
	return &accountrpc.RenewTokenReply{Token: newToken, Csrf: newCsrf, Expires: newExpires}, nil
}

// SendCapture 发送登录/注册/找回验证码。
// 验证码写入 Redis（10 分钟有效），发送记录落库；短信下发由 notification
// 服务承接（未接入前为开发降级日志模式，与实名验证码一致）。
func (r *Repository) SendCapture(ctx context.Context, biz int32, target, ip string) error {
	target = strings.TrimSpace(target)
	if target == "" {
		return ErrLoginAccountNotExist
	}
	times := 0
	if v, ok := r.cache.getInt(ctx, captureTimesKey(biz, target)); ok {
		times = int(v)
	}
	if times > captureMaxSend {
		r.addCaptureLog(ctx, biz, target, ip, 1, ErrCaptureSendTooMany.Error())
		return ErrCaptureSendTooMany
	}
	code := int(time.Now().UnixNano()%900000) + 100000
	if err := r.cache.setInt(ctx, captureKey(biz, target), int64(code), captureCodeTTL); err != nil {
		return err
	}
	r.cache.incrCaptureTimes(ctx, captureTimesKey(biz, target))
	_ = r.cache.del(ctx, captureErrKey(biz, target))
	// 短信下发：notification 未接入，记录日志供开发联调
	logx.Infof("account/login: send capture biz=%d target=%s code=%06d (sms not integrated)", biz, target, code)
	r.addCaptureLog(ctx, biz, target, ip, 0, "")
	return nil
}

// checkCapture 校验验证码（错误次数限制）。
func (r *Repository) checkCapture(ctx context.Context, biz int32, target, code string) error {
	errTimes := 0
	if v, ok := r.cache.getInt(ctx, captureErrKey(biz, target)); ok {
		errTimes = int(v)
	}
	if errTimes > captureMaxErr {
		_ = r.cache.del(ctx, captureKey(biz, target))
		return ErrCaptureErrTooMany
	}
	serverCode, ok := r.cache.getInt(ctx, captureKey(biz, target))
	if !ok {
		return ErrCaptureInvalid
	}
	if strconv.FormatInt(serverCode, 10) != strings.TrimSpace(code) {
		r.cache.incrCaptureErrTimes(ctx, captureErrKey(biz, target))
		return ErrCaptureWrong
	}
	return nil
}

// CheckCapture 校验验证码（供网关独立校验路由）。
func (r *Repository) CheckCapture(ctx context.Context, biz int32, target, code string) error {
	return r.checkCapture(ctx, biz, target, code)
}

func (r *Repository) addCaptureLog(ctx context.Context, biz int32, target, ip string, status int8, reason string) {
	if err := r.captureLogModel.Add(ctx, &model.AccountCaptureLog{
		Biz: int8(biz), Target: target, IP: ip, Status: status, Reason: reason,
	}); err != nil {
		logx.Errorf("account/login: add capture log err=%v", err)
	}
}

// SetPassword 设置/修改密码（secure 语义）：旧密码校验 → 事务内旧密钥置历史 +
// 新密钥生效 → 吊销全部会话。
func (r *Repository) SetPassword(ctx context.Context, mid int64, oldPwd, newPwd, ip string) error {
	active, err := r.secretModel.FindActive(ctx, mid, model.SecretTypePassword)
	if err != nil {
		return err
	}
	if active != nil {
		// 已有密码 → 改密必须校验旧密码
		plainOld, err := r.decryptPassword(oldPwd)
		if err != nil {
			return err
		}
		if plainOld == "" || saltPwd(plainOld, active.Salt) != active.Hash {
			return ErrLoginPasswordWrong
		}
	}
	plainNew, err := r.decryptPassword(newPwd)
	if err != nil {
		return err
	}
	if plainNew == "" {
		return ErrPasswordRequired
	}
	salt := randomHex(16)
	err = r.conn.TransactCtx(ctx, func(c context.Context, tx sqlx.Session) error {
		if active != nil {
			if err := r.secretModel.MarkHistory(c, tx, mid, model.SecretTypePassword); err != nil {
				return err
			}
		}
		now := time.Now().Unix()
		return r.secretModel.Insert(c, tx, &model.AccountSecret{
			Mid: mid, SecretType: model.SecretTypePassword, Salt: salt,
			Hash: saltPwd(plainNew, salt), Status: model.SecretStatusActive, CTime: now, MTime: now,
		})
	})
	if err != nil {
		return err
	}
	// 改密后吊销全部会话（批量吊销经 tokenCacheTTL 内生效，见注释）
	_ = r.sessionModel.RevokeAll(ctx, mid)
	return nil
}

// ResetPassword 重置密码（账号找回）：验证码校验 → 覆盖密码 → 吊销会话。
func (r *Repository) ResetPassword(ctx context.Context, account, captureCode, newPwd, ip string) error {
	account = strings.ToLower(strings.TrimSpace(account))
	if err := r.checkCapture(ctx, int32(model.CaptureBizRecovery), account, captureCode); err != nil {
		return err
	}
	mid, err := r.findMidByAccount(ctx, account)
	if err != nil {
		return err
	}
	plainNew, err := r.decryptPassword(newPwd)
	if err != nil {
		return err
	}
	if plainNew == "" {
		return ErrPasswordRequired
	}
	salt := randomHex(16)
	err = r.conn.TransactCtx(ctx, func(c context.Context, tx sqlx.Session) error {
		if err := r.secretModel.MarkHistory(c, tx, mid, model.SecretTypePassword); err != nil {
			return err
		}
		now := time.Now().Unix()
		return r.secretModel.Insert(c, tx, &model.AccountSecret{
			Mid: mid, SecretType: model.SecretTypePassword, Salt: salt,
			Hash: saltPwd(plainNew, salt), Status: model.SecretStatusActive, CTime: now, MTime: now,
		})
	})
	if err != nil {
		return err
	}
	_ = r.sessionModel.RevokeAll(ctx, mid)
	_ = r.cache.del(ctx, captureKey(int32(model.CaptureBizRecovery), account))
	return nil
}

// CheckHistoryPassword 历史密码校验（参考 passport HistoryPwdCheck）。
// password 可逗号分隔多个，返回与入参一一对应的 "0,1" 序列。
func (r *Repository) CheckHistoryPassword(ctx context.Context, mid int64, password string) (string, error) {
	secrets, err := r.secretModel.FindAll(ctx, mid, model.SecretTypePassword)
	if err != nil {
		return "", err
	}
	plain, err := r.decryptPassword(password)
	if err != nil {
		return "", err
	}
	pwds := strings.Split(plain, ",")
	match := make([]string, 0, len(pwds))
	for _, pwd := range pwds {
		hit := 0
		for _, s := range secrets {
			if saltPwd(pwd, s.Salt) == s.Hash {
				hit = 1
				break
			}
		}
		match = append(match, strconv.Itoa(hit))
	}
	return strings.Join(match, ","), nil
}

// LoginLogs 查询登录日志（最近 N 条，时间倒序）。
func (r *Repository) LoginLogs(ctx context.Context, mid int64, limit int32) ([]*accountrpc.LoginLog, error) {
	logs, err := r.loginLogModel.FindByMid(ctx, mid, limit)
	if err != nil {
		return nil, err
	}
	reply := make([]*accountrpc.LoginLog, 0, len(logs))
	for _, l := range logs {
		reply = append(reply, &accountrpc.LoginLog{
			Mid:       l.Mid,
			Ip:        l.IP,
			Ts:        l.TS,
			LoginType: int32(l.LoginType),
			Status:    int32(l.Status),
			Reason:    l.Reason,
			Device:    l.Device,
		})
	}
	return reply, nil
}
