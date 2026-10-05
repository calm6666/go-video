package logic

import (
	"context"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type RegisterApplicationLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRegisterApplicationLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RegisterApplicationLogic {
	return &RegisterApplicationLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 注册应用（client_token 幂等），secret 仅此一次返回。
func (l *RegisterApplicationLogic) RegisterApplication(in *rpc.RegisterApplicationReq) (*rpc.RegisterApplicationReply, error) {
	ctx, s := l.ctx, l.svcCtx

	// 1. 参数形状校验先于任何写与读：非法入参必须零副作用。
	name, err := requireLen(in.Name, maxAppNameRunes, errNameRequired, errNameTooLong)
	if err != nil {
		return nil, err
	}
	description, err := optionalLen(in.Description, maxAppDescRunes, errDescriptionTooLong)
	if err != nil {
		return nil, err
	}
	if in.OwnerMid <= 0 {
		return nil, model.ErrOwnerRequired
	}
	clientToken, err := requireLen(in.ClientToken, maxClientTokenRunes,
		model.ErrClientTokenRequired, model.ErrClientTokenRequired)
	if err != nil {
		return nil, err
	}
	// 回调白名单：https only + 非内网字面地址（SSRF 护栏在注册期就要生效，
	// 脏地址一旦入库就是长期暴露面）。
	redirectURIs, err := normalizeRedirectURIs(s, in.RedirectUris)
	if err != nil {
		return nil, err
	}
	// 申请的 scope：先过商业化红线，再查目录（未知/停用都不落库）。
	scopes, err := normalizeScopes(in.Scopes)
	if err != nil {
		return nil, err
	}
	if err := rejectForbiddenScopes(scopes); err != nil {
		return nil, err
	}
	defs, err := requireKnownScopes(ctx, s, scopes)
	if err != nil {
		return nil, err
	}
	if err := requireWriteScopeConsent(defs, scopes); err != nil {
		return nil, err
	}

	// 2. 幂等回放：同 client_token 只回既有投影，client_secret 恒为空串。
	//    库中只有 HMAC(pepper, salt||明文)，物理上无法二次返回明文——
	//    这不是「选择不返回」，而是不可逆，调用方必须自己保存。
	if existing, err := s.Apps.FindByRegisterToken(ctx, clientToken); err != nil {
		return nil, err
	} else if existing != nil {
		app, err := appProjection(ctx, s, existing)
		if err != nil {
			return nil, err
		}
		logx.WithContext(ctx).Infof("open-platform: 注册幂等回放 app_id=%d owner_mid=%d", existing.AppID, existing.OwnerMid)
		return &rpc.RegisterApplicationReply{App: app, Replayed: true}, nil
	}

	// 3. 密钥材料前置闸门：没有 pepper 就不允许生成「看似有哈希、实际可离线爆破」的凭证。
	pepper, err := credentialPepper(s)
	if err != nil {
		return nil, err
	}
	appKey, err := uniqueAppKey(ctx, s)
	if err != nil {
		return nil, err
	}
	secret, err := secretCredential(pepper)
	if err != nil {
		return nil, err
	}

	// 5. 进程级写令牌桶：限流发生在写库之前，被限流的请求不留半成品。
	release, err := writePermit(ctx, s)
	if err != nil {
		return nil, err
	}
	defer release()

	// 4. 事务边界：应用行与首把密钥同生共死，不允许「有应用无密钥」（永久不可用身份）。
	now := nowUnix()
	expiresAt := secretExpiresAt(s.Config.OpenPlatform.SecretValidDays, now)
	app := &model.Application{
		AppKey:        appKey,
		Name:          name,
		Description:   description,
		OwnerMid:      in.OwnerMid,
		Status:        model.AppStatusPendingReview,
		RedirectURIs:  redirectURIs,
		RegisterToken: clientToken,
	}
	var (
		appID int64
	)
	err = s.DB.TransactCtx(ctx, func(tctx context.Context, session sqlx.Session) error {
		id, created, err := s.Apps.InsertTx(tctx, session, app)
		if err != nil {
			return err
		}
		if !created {
			// 并发下另一个请求已用同一 client_token 建行：整笔回滚，由上层重放分支处理。
			return model.ErrConcurrentUpdate
		}
		app.AppID = id
		if _, err := s.Secrets.InsertTx(tctx, session, &model.AppSecret{
			AppID:        id,
			Salt:         secret.salt,
			Hash:         secret.hash,
			ExpiresAt:    expiresAt,
			RotateReason: "initial issue",
			OperatorMid:  in.OwnerMid,
		}); err != nil {
			return err
		}
		appID = id
		return nil
	})
	if err != nil {
		return nil, err
	}

	// scope 申请登记在事务外：AppScopeModel.Request 不带 session（不推翻审批结论的幂等插入），
	//    失败只让「待审清单」缺一行，不影响应用与密钥的可用性，因此不回滚已提交的应用。
	if len(scopes) > 0 {
		if err := s.AppScopes.Request(ctx, appID, scopes, in.OwnerMid); err != nil {
			logx.WithContext(ctx).Errorf("open-platform: app_id=%d scope 申请登记失败: %v", appID, err)
		}
	}

	// 回读提交后的行再投影：事务里那个 app 结构没有 version/mtime（INSERT 侧写常量），
	// 直接投影会让响应里的乐观锁令牌恒为 0，而 UpdateApplication/applyProfile 对
	// expected_version<=0 直接判并发冲突——照本响应「注册→改资料」的调用方第一步必失败。
	// 回读失败时调用方用同一 client_token 重试会命中上面的幂等回放分支，拿到的是库里真值。
	stored, err := findApp(ctx, s, appID)
	if err != nil {
		return nil, err
	}
	info, err := appProjection(ctx, s, stored)
	if err != nil {
		return nil, err
	}
	// 7. 明文只经本响应出去一次；日志只记标识，绝不记 secret/salt/hash。
	logx.WithContext(ctx).Infof("open-platform: 应用注册成功 app_id=%d owner_mid=%d scopes=%d expires_at=%d",
		appID, in.OwnerMid, len(scopes), expiresAt)
	return &rpc.RegisterApplicationReply{
		App:             info,
		ClientSecret:    secret.plain,
		SecretExpiresAt: expiresAt,
		Replayed:        false,
	}, nil
}

// uniqueAppKey 生成公开标识并在撞 uniq_app_key 时重生成。
// 预检查而不是靠插入报错：唯一键冲突会把整笔注册事务判失败，
// 而 app_key 是 96bit 随机值，冲突概率极低，重生成比回滚更合适。
func uniqueAppKey(ctx context.Context, s *svc.ServiceContext) (string, error) {
	for i := 0; i < appKeyCollisionRetry; i++ {
		key, err := newAppKey()
		if err != nil {
			return "", err
		}
		existing, err := s.Apps.FindByAppKey(ctx, key)
		if err != nil {
			return "", err
		}
		if existing == nil {
			return key, nil
		}
	}
	return "", model.ErrConcurrentUpdate
}
