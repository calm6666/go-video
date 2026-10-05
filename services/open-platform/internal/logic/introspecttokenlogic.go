package logic

import (
	"context"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type IntrospectTokenLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewIntrospectTokenLogic(ctx context.Context, svcCtx *svc.ServiceContext) *IntrospectTokenLogic {
	return &IntrospectTokenLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// token 校验（含 grant 撤销位点比对）。
//
// 契约依据：proto:363-382「校验顺序：token 行状态 → 过期时间 → grant 撤销位点 → 应用状态 →
// scope（若带 api_code）」，proto:19-25「access token 校验除自身状态外还必须比对
// op_grant.revoked_at」。
//
// 本方法不自己写判定链：全部委托 tokencheck.evaluateAccessToken，与 AuthorizeRequest 共用同一实现。
// 这两个方法是同一个安全属性（「这次调用能不能放行」）的两个入口，各写一套必然漂移——
// 例如这里忘了比位点，撤销就只在另一条路径生效，换个入口即可绕过。
// 定位缓存（IntrospectCacheSeconds）也在该函数内部处理：只缓存 access_hash→token_id 的定位结果，
// 不缓存结论，因此缓存最多延后「拒绝」的传播，绝不会让已撤销的凭证重新可用。
func (l *IntrospectTokenLogic) IntrospectToken(in *rpc.IntrospectTokenReq) (*rpc.IntrospectTokenReply, error) {
	ctx, s := l.ctx, l.svcCtx

	// 1. 参数：access_token 与 token_id 至少给一个（proto:366-367）。
	//    同时给出时以 access_token 为准——token_id 可能是调用方缓存的旧值，
	//    而明文凭证是调用方当场持有的、更可信的定位依据。该优先级在 locateToken 里唯一实现。
	if in.AccessToken == "" && in.TokenId <= 0 {
		return nil, model.ErrTokenInvalid
	}

	// 2. 判定链。api_code 不参与 scope 判定：仓库里没有「接口 → 所需权限点」的目录表，
	//    服务端无法从 api_code 推导出 required_scope（见 README 已知缺口与本轮交付报告），
	//    因此只认调用方显式给的 required_scope（proto:369）。缺目录时绝不「猜一个权限点」，
	//    也绝不因为 api_code 非空就当作「已经判过 scope」。
	res, err := evaluateAccessToken(ctx, s, in.AccessToken, in.TokenId, in.RequiredScope)
	if err != nil {
		// 只有依赖故障（DB/配置）才走 gRPC 错误；业务不通过一律 active=false + code=OK。
		return nil, err
	}

	// 3. 不通过：只回结论与固定原因码。
	//    不回 app_id/mid/grant_id/token_id/scope——「存在但无效」的凭证已经带着
	//    一条内部标识映射（谁的哪次授权），把它回给任意持票人是净增的泄露面。
	if !res.active || res.token == nil {
		deny := res.deny
		if deny == "" {
			// 判定链未标注原因时保守回 inactive，绝不因为「没有理由」就放行（AGENTS.md §9）。
			deny = denyInactive
		}
		return &rpc.IntrospectTokenReply{Active: false, DenyReason: deny}, nil
	}

	// 4. 观测：使用时间限频回写（同 token 距上次 >=60s 才写一次），失败只降级不影响结论。
	touchTokenUsed(ctx, s, res.token, nowUnix())

	// 5. 通过：响应字段严格按 proto:373-382 的投影，没有 salt/hash/明文的列。
	return &rpc.IntrospectTokenReply{
		Active:    true,
		AppId:     res.token.AppID,
		Mid:       res.token.Mid,
		Scope:     res.scopes,
		ExpiresAt: res.expires,
		GrantId:   res.token.GrantID,
		TokenId:   res.token.TokenID,
	}, nil
}
