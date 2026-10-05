package logic

import (
	"context"

	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
)

// 凭证生成与「按值定位」哈希的统一出口。
//
// 两条不可商量的规则（README「哈希口径」）：
//  1. 明文只在本包返回值里存在一次，落库的一律是 hash+salt；任何投影/日志/事件都不给明文；
//  2. 授权码 / access / refresh 的读路径是 `WHERE xxx_hash = ?`，因此算式必须是
//     HMAC(pepper, 明文)（掺 salt 就无法由明文反查唯一键）；
//     client_secret 按 app_id 定位，才用 HMAC(pepper, salt||明文)。
//     salt 列仍然写随机值：用于 pepper 轮换期的双读迁移与审计。

// credential 一枚新生成的凭证：明文 + 入库盐 + 按值定位哈希。
type credential struct {
	plain string
	salt  string
	hash  string
}

// newCredential 生成 256bit 随机凭证。pepper 缺失时由 credentialPepper 直接 fail closed。
func newCredential(pepper string) (*credential, error) {
	plain, err := randomCredential()
	if err != nil {
		return nil, err
	}
	salt, err := randomSalt()
	if err != nil {
		return nil, err
	}
	hash, err := model.HashCredential(pepper, plain)
	if err != nil {
		return nil, err
	}
	return &credential{plain: plain, salt: salt, hash: hash}, nil
}

// secretCredential 生成 client_secret：明文只回给调用方一次，哈希掺 salt。
func secretCredential(pepper string) (*credential, error) {
	plain, err := randomCredential()
	if err != nil {
		return nil, err
	}
	salt, err := randomSalt()
	if err != nil {
		return nil, err
	}
	hash, err := model.HashSecret(pepper, salt, plain)
	if err != nil {
		return nil, err
	}
	return &credential{plain: plain, salt: salt, hash: hash}, nil
}

// hashOfPlaintext 对调用方给来的凭证值算「按值定位」哈希
// （换码、轮换、校验、撤销定位共用同一算式，绝不另写一套）。
func hashOfPlaintext(s *svc.ServiceContext, plain string) (string, error) {
	pepper, err := credentialPepper(s)
	if err != nil {
		return "", err
	}
	return model.HashCredential(pepper, plain)
}

// hashOfChallenge 端点验证挑战的哈希：challenge 由 endpoint_id 定位后比对，
// 但比对口径与授权码一致（按值定位），因此共用 HashCredential。
func hashOfChallenge(s *svc.ServiceContext, challenge string) (string, error) {
	return hashOfPlaintext(s, challenge)
}

// ---------------------------------------------------------------- token 签发

// tokenSpec 一次 token 签发的输入。
type tokenSpec struct {
	grantID, appID, mid int64
	grantType           int32
	scopes              []string
	parentTokenID       int64
}

// issuedToken 已落库的 token 行 + 只在本次响应出现的双明文字段。
type issuedToken struct {
	tokenID          int64
	row              *model.Token
	accessPlain      string
	refreshPlain     string
	issuedAt         int64
	accessExpiresAt  int64
	refreshExpiresAt int64
}

// requireIssuable 事务前的签发能力闸门：pepper 与 TTL 都是配置态，缺失时一票签发都不可能成功。
//
// 放在开事务之前而不是交给 issueToken 兜：issueToken 是事务里的第一句还是最后一句因方法而异，
// 而 refresh 的轮换事务先作废旧行再签发，配置没配好时会在库里留下「旧的已 ROTATED、新的没有」
// 的中间态（回滚能兜住数据，兜不住已消费的写令牌和行锁）。
func requireIssuable(s *svc.ServiceContext) error {
	if _, err := credentialPepper(s); err != nil {
		return err
	}
	cfg := s.Config.OpenPlatform
	if cfg.AccessTokenTTLSeconds <= 0 || cfg.RefreshTokenTTLSeconds <= 0 {
		return errTTLNotConfigured
	}
	return nil
}

// issueToken 生成 access+refresh、算哈希并落库。
//
// session 非空时必须在调用方的事务里执行：「新行落库」与「旧凭证作废/授权码消费」
// 必须原子生效，否则会出现「旧的已作废、新的没签发」的用户不可用状态。
// session 传 nil 时由 model 自行选择连接（本服务所有跨表写都走事务，nil 只出现在单测里）。
func issueToken(ctx context.Context, s *svc.ServiceContext, session sqlx.Session, spec tokenSpec) (*issuedToken, error) {
	pepper, err := credentialPepper(s)
	if err != nil {
		return nil, err
	}
	cfg := s.Config.OpenPlatform
	if cfg.AccessTokenTTLSeconds <= 0 || cfg.RefreshTokenTTLSeconds <= 0 {
		return nil, errTTLNotConfigured
	}
	access, err := newCredential(pepper)
	if err != nil {
		return nil, err
	}
	refresh, err := newCredential(pepper)
	if err != nil {
		return nil, err
	}
	now := nowUnix()
	row := &model.Token{
		GrantID:          spec.grantID,
		AppID:            spec.appID,
		Mid:              spec.mid,
		GrantType:        spec.grantType,
		AccessSalt:       access.salt,
		AccessHash:       access.hash,
		RefreshSalt:      refresh.salt,
		RefreshHash:      refresh.hash,
		Scope:            model.JoinScopes(spec.scopes),
		AccessExpiresAt:  now + cfg.AccessTokenTTLSeconds,
		RefreshExpiresAt: now + cfg.RefreshTokenTTLSeconds,
		State:            model.TokenStateActive,
		ParentTokenID:    spec.parentTokenID,
	}
	id, err := s.Tokens.InsertTx(ctx, session, row)
	if err != nil {
		return nil, err
	}
	row.TokenID = id
	return &issuedToken{
		tokenID:          id,
		row:              row,
		accessPlain:      access.plain,
		refreshPlain:     refresh.plain,
		issuedAt:         now,
		accessExpiresAt:  row.AccessExpiresAt,
		refreshExpiresAt: row.RefreshExpiresAt,
	}, nil
}

// tokenSet 由 helpers.projectTokenSet 统一组装，本文件不再另写投影，
// 以保证「明文只出现在一处」这条审计规则只有一处需要被审查。

// secretExpiresAt 新密钥的失效时间：SecretValidDays<=0 表示长期有效（靠轮换/吊销），
// 返回 0 让 AppSecret.Usable 走「无到期」分支，而不是伪造一个远期时间戳。
func secretExpiresAt(validDays int, now int64) int64 {
	if validDays <= 0 {
		return 0
	}
	return now + int64(validDays)*86400
}
