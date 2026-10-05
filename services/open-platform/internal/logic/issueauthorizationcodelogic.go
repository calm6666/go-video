package logic

import (
	"context"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type IssueAuthorizationCodeLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewIssueAuthorizationCodeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *IssueAuthorizationCodeLogic {
	return &IssueAuthorizationCodeLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 用户同意后签发短期授权码。
func (l *IssueAuthorizationCodeLogic) IssueAuthorizationCode(in *rpc.IssueAuthorizationCodeReq) (*rpc.IssueAuthorizationCodeReply, error) {
	ctx, s := l.ctx, l.svcCtx

	// 1. 用户意愿锚点放在最前面，先于任何 DB 读写：
	//    这是整条链路唯一记录「用户同意了吗」的地方，隐式同意一律拒绝。
	if !in.ConsentGiven {
		return nil, model.ErrConsentRequired
	}
	if in.Mid <= 0 {
		return nil, model.ErrOperatorRequired
	}

	// 2. scope 非空：空 scope 的授权无法审计「用户到底同意了什么」。
	scopes, err := normalizeScopes(in.Scope)
	if err != nil {
		return nil, err
	}
	if len(scopes) == 0 {
		return nil, errScopeRequiredForIssue
	}
	if err := rejectForbiddenScopes(scopes); err != nil {
		return nil, err
	}

	// 3. 应用存在且 ACTIVE。
	app, err := findApp(ctx, s, in.AppId)
	if err != nil {
		return nil, err
	}
	if err := requireActiveApp(app); err != nil {
		return nil, err
	}

	// 4. redirect_uri 必须逐字命中白名单：前缀/通配匹配会让白名单退化成开放重定向池。
	redirect, err := requireLen(in.RedirectUri, s.Config.OpenPlatform.MaxRedirectURIBytes,
		model.ErrInvalidRedirectURI, model.ErrInvalidRedirectURI)
	if err != nil {
		return nil, err
	}
	if !uriAllowed(app.RedirectURIs, redirect) {
		return nil, model.ErrRedirectURIMismatch
	}

	// 5. 最小权限边界：请求 scope ⊆ 应用已获批集，再逐个查目录（未知/停用都拒）。
	if err := grantedSubset(ctx, s, app.AppID, scopes); err != nil {
		return nil, err
	}
	defs, err := requireKnownScopes(ctx, s, scopes)
	if err != nil {
		return nil, err
	}
	// 写权限必须在目录里声明 requires_user_consent=1：
	// 「用户勾了一键同意全部」不能替代目录侧的显式同意声明。
	if err := requireWriteScopeConsent(defs, scopes); err != nil {
		return nil, err
	}
	state, err := optionalLen(in.State, maxStateRunes, errStateTooLong)
	if err != nil {
		return nil, err
	}

	// 6. 签发限频：防对同一应用批量刷码。命中限流时不写任何行。
	cfg := s.Config.OpenPlatform
	if cfg.AuthCodeMaxPerUserPerHour > 0 {
		now := nowUnix()
		recent, err := s.AuthCodes.CountRecentByMid(ctx, in.Mid, now-3600)
		if err != nil {
			return nil, err
		}
		if recent >= int64(cfg.AuthCodeMaxPerUserPerHour) {
			logx.WithContext(ctx).Errorf("open-platform: 授权码签发限频 mid=%d app_id=%d recent=%d",
				in.Mid, app.AppID, recent)
			return nil, model.ErrRateLimited
		}
	}

	// 7. 密钥材料：没有 pepper 就签不出可比对的哈希，禁止退化成明文或无 pepper 哈希入库。
	pepper, err := credentialPepper(s)
	if err != nil {
		return nil, err
	}
	if cfg.AuthCodeTTLSeconds <= 0 {
		return nil, errTTLNotConfigured
	}

	code, err := newCredential(pepper)
	if err != nil {
		return nil, err
	}
	release, err := writePermit(ctx, s)
	if err != nil {
		return nil, err
	}
	defer release()

	var grantID, codeID int64
	now := nowUnix()
	err = s.DB.TransactCtx(ctx, func(tctx context.Context, session sqlx.Session) error {
		// FindOrCreate 复用 uniq_app_mid 的同一行：覆盖 scope 快照、刷新同意时间并清空撤销位点，
		// 因此「用户重新授权」一定能覆盖历史撤销（否则撤销过一次的永久无法再授权）。
		id, _, err := s.Grants.FindOrCreate(tctx, session, app.AppID, in.Mid, scopes, true)
		if err != nil {
			return err
		}
		grantID = id
		row := &model.AuthCode{
			AppID:       app.AppID,
			Mid:         in.Mid,
			Salt:        code.salt,
			Hash:        code.hash,
			Scope:       model.JoinScopes(scopes),
			RedirectURI: redirect,
			State:       state,
			GrantID:     id,
			ExpiresAt:   now + cfg.AuthCodeTTLSeconds,
		}
		// grant_id 预建：撤销位点从签发起就连贯（换码前 grant 已确定，不用事后回填）。
		id, created, err := s.AuthCodes.InsertTx(tctx, session, row)
		if err != nil {
			return err
		}
		if !created {
			// code_id 哈希唯一键撞上既有行：整笔回滚，绝不允许把别人的授权码当自己签发的。
			return model.ErrConcurrentUpdate
		}
		codeID = id
		return nil
	})
	if err != nil {
		return nil, err
	}

	// 日志只记标识与结论：code 明文、salt、hash 一律不落日志（AGENTS.md §7）。
	logx.WithContext(ctx).Infof("open-platform: 授权码签发 app_id=%d mid=%d grant_id=%d code_id=%d scope=%d ttl=%d",
		app.AppID, in.Mid, grantID, codeID, len(scopes), cfg.AuthCodeTTLSeconds)
	return &rpc.IssueAuthorizationCodeReply{
		Code:      code.plain,
		ExpiresIn: cfg.AuthCodeTTLSeconds,
		Scope:     scopes,
		GrantId:   grantID,
	}, nil
}
