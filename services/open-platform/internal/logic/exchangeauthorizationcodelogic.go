package logic

import (
	"context"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type ExchangeAuthorizationCodeLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewExchangeAuthorizationCodeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ExchangeAuthorizationCodeLogic {
	return &ExchangeAuthorizationCodeLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 授权码换 token（一次性消费 + 重放检测）。
func (l *ExchangeAuthorizationCodeLogic) ExchangeAuthorizationCode(in *rpc.ExchangeAuthorizationCodeReq) (*rpc.ExchangeAuthorizationCodeReply, error) {
	ctx, s := l.ctx, l.svcCtx

	code, err := requireLen(in.Code, credentialBytes*2, model.ErrAuthCodeInvalid, model.ErrAuthCodeInvalid)
	if err != nil {
		return nil, err
	}
	redirect, err := requireLen(in.RedirectUri, s.Config.OpenPlatform.MaxRedirectURIBytes,
		model.ErrRedirectURIMismatch, model.ErrRedirectURIMismatch)
	if err != nil {
		return nil, err
	}

	// 1. 定位：授权码按值反查唯一键，必须先有 pepper 才能算出同一个 HMAC。
	//    缺 pepper 时 fail closed——退化成「明文比对」或「无 pepper 哈希」等于把凭证存储降级。
	accessHash, err := hashOfPlaintext(s, code)
	if err != nil {
		return nil, err
	}
	row, err := s.AuthCodes.FindByHash(ctx, accessHash)
	if err != nil {
		return nil, err
	}
	// 2. 「不存在」与「不属于本应用」回同一个错误：
	//    可区分的文案等于一个授权码存在性探测接口（攻击者可批量验证截获到的码是否真存在）。
	if row == nil || row.AppID != in.AppId {
		return nil, model.ErrAuthCodeInvalid
	}
	// 3. 回调地址一致性：码与 redirect_uri 绑定，换回调地址即视为码被转手。
	if row.RedirectURI != redirect {
		return nil, model.ErrRedirectURIMismatch
	}

	now := nowUnix()
	// 4a. 时效：短期是防截获的核心手段（TTL 默认 60s，配置校验把上限锁在 600s 内）。
	if row.Expired(now) {
		return nil, model.ErrAuthCodeExpired
	}
	// 4b. 一次性：UsedAt!=0 说明同一枚码第二次被拿来兑换——要么被截获，要么客户端在重试。
	//     无论哪种都要留下计数（replay_count>0 是泄露告警的输入），且绝不签发第二组 token。
	if row.UsedAt != 0 {
		if err := s.AuthCodes.IncrReplay(ctx, row.CodeID); err != nil {
			// 重放计数写失败不能放行：凭证复用比少一条审计更严重。
			logx.WithContext(ctx).Errorf("open-platform: 授权码重放计数失败 code_id=%d: %v", row.CodeID, err)
		}
		logx.WithContext(ctx).Errorf("open-platform: 授权码重放告警 code_id=%d app_id=%d used_at=%d",
			row.CodeID, row.AppID, row.UsedAt)
		return nil, model.ErrAuthCodeUsed
	}

	// 5. 兑换时点仍然可用：应用 ACTIVE、grant 未被撤销、scope 仍被批准。
	//    签发到兑换之间可能发生了停用或审批回收，因此这一段必须在换码时重判。
	app, err := findApp(ctx, s, row.AppID)
	if err != nil {
		return nil, err
	}
	if err := requireActiveApp(app); err != nil {
		return nil, err
	}
	grant, err := s.Grants.FindByID(ctx, row.GrantID)
	if err != nil {
		return nil, err
	}
	if grant == nil {
		// 码指向不存在的 grant 属于数据不一致，按「已撤销」处理而不是放行。
		return nil, model.ErrGrantRevoked
	}
	if !grant.TokenAccepted(row.Ctime) {
		return nil, model.ErrGrantRevoked
	}
	scopes := model.SplitScopes(row.Scope)
	if err := grantedSubset(ctx, s, row.AppID, scopes); err != nil {
		return nil, err
	}

	// 6. 事务：新 token 落库 + 码消费 + grant 链头前移，三句同生共死。
	//    MarkUsed 的 CAS（used_at 0→非 0）是「一把 code 只换出一组 token」的唯一硬锚点：
	//    并发两次兑换时，后提交者拿到 applied=false，整个事务回滚，不会留下第二代 token。
	token, err := l.consume(row, grant, scopes, now)
	if err != nil {
		return nil, err
	}

	logx.WithContext(ctx).Infof("open-platform: 授权码兑换成功 app_id=%d mid=%d code_id=%d token_id=%d grant_id=%d",
		row.AppID, row.Mid, row.CodeID, token.tokenID, grant.GrantID)
	return &rpc.ExchangeAuthorizationCodeReply{
		Token: projectTokenSet(token.tokenID, token.row.GrantID, token.issuedAt,
			token.accessExpiresAt, token.refreshExpiresAt, scopes, token.accessPlain, token.refreshPlain),
	}, nil
}

// consume 在单个事务里换出 token 并消费授权码。
func (l *ExchangeAuthorizationCodeLogic) consume(code *model.AuthCode, grant *model.Grant,
	scopes []string, now int64) (*issuedToken, error) {
	ctx, s := l.ctx, l.svcCtx
	release, err := writePermit(ctx, s)
	if err != nil {
		return nil, err
	}
	defer release()

	var out *issuedToken
	err = s.DB.TransactCtx(ctx, func(tctx context.Context, session sqlx.Session) error {
		token, err := issueToken(tctx, s, session, tokenSpec{
			grantID:   grant.GrantID,
			appID:     code.AppID,
			mid:       code.Mid,
			grantType: model.GrantTypeAuthorizationCode,
			scopes:    scopes,
			// 授权码兑换是「新链」而不是旧链的延续：parent 留 0，
			// 使 refresh 轮换链能从每一代授权各自追溯，不会把两次独立授权串成一条。
			parentTokenID: 0,
		})
		if err != nil {
			return err
		}
		applied, err := s.AuthCodes.MarkUsed(tctx, session, code.CodeID, token.tokenID, now)
		if err != nil {
			return err
		}
		if !applied {
			// 并发兑换：先到的事务已经消费掉，本次整体回滚（新 token 不落库）。
			return model.ErrAuthCodeUsed
		}
		// 链头 CAS：from 用读到的 current_token_id（重新授权后可能已有上一代的头）。
		head, err := s.Grants.TouchTokenHead(tctx, session, grant.GrantID, grant.CurrentTokenID,
			token.tokenID, now)
		if err != nil {
			return err
		}
		if !head {
			// 链头被别人前移（并发的第二次兑换或轮换）：结论已不确定，回滚让调用方重试。
			return model.ErrConcurrentUpdate
		}
		if err := s.Grants.TouchLastCode(tctx, session, grant.GrantID, code.CodeID, now); err != nil {
			return err
		}
		out = token
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
