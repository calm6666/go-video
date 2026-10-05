package logic

import (
	"context"
	"strings"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RegisterWebhookLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRegisterWebhookLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RegisterWebhookLogic {
	return &RegisterWebhookLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 注册回调端点（需验证后才投递）。
//
// 本方法是全服务唯一「把外部可控地址写进潜在外呼目标表」的入口，因此它的两条安全属性
// 必须在本服务内成立，不能指望 gateway 兜：
//  1. 回调地址必须是公网 https 地址（SSRF 护栏，脏 URL 一旦入库就是长期暴露面）；
//  2. 未验证的端点永不投递（verified_at=0 被 ListMatching 过滤掉）。
//
// 错误映射：ErrInvalidAppID/ErrAppNotFound/ErrInvalidEventType/ErrInvalidWebhookURL/
// errDescriptionTooLong→InvalidArgument；ErrOwnerRequired→PermissionDenied；
// errWebhookEndpointLimit、ErrSecretVerificationUnavailable、ErrWebhookAlreadyRegistered→
// FailedPrecondition；SQL 失败→Internal。
func (l *RegisterWebhookLogic) RegisterWebhook(in *rpc.RegisterWebhookReq) (*rpc.RegisterWebhookReply, error) {
	ctx, s := l.ctx, l.svcCtx

	// 1. 归属与身份先于任何写：owner 或运营之外不得为该应用登记外呼目标（proto:529-530）。
	app, err := findApp(ctx, s, in.AppId)
	if err != nil {
		return nil, err
	}
	if err := requireOwnerOrOperator(app, in.OperatorMid, in.IsOperator); err != nil {
		return nil, err
	}

	// 2. 事件类型：proto3 默认值 0（UNSPECIFIED）与未知值一律拒绝，
	//    绝不解释成「订阅全部事件」；目录里也不存在商业化事件位（AGENTS.md §1）。
	eventType := int32(in.EventType)
	if !model.ValidWebhookEventType(eventType) {
		return nil, model.ErrInvalidEventType
	}
	description, err := optionalLen(in.Description, maxWebhookDescRunes, errDescriptionTooLong)
	if err != nil {
		return nil, err
	}

	// 3. 回调地址：与 redirect_uri 共用同一套严格校验（helpers.validPublicURI）——
	//    只允许 https、必须有 host、禁止 userinfo 与 fragment、禁止 localhost/.internal/
	//    .local 一类内部主机名，以及回环/私网/链路本地/组播/CGNAT 与元数据网段的字面 IP
	//    （model.IsPublicHostAddr）。返回归一化后的串入库，避免「同一地址两种写法」绕过
	//    uniq_app_event_url 变成两个端点。
	//    本方法刻意不做 DNS 解析（helpers 文件头已记录该分工）：解析后的地址校验由投递
	//    worker 在发起连接时再做一次并固定 IP，否则注册期解析与投递期解析之间的
	//    DNS rebinding 窗口仍然在。
	callbackURL, err := validWebhookURL(s, in.Url)
	if err != nil {
		return nil, err
	}

	// 4. 签名派生根：signKey = HMAC-SHA256(WebhookMasterPepper, app_id || key_version)，
	//    密钥既不入库也不下发。没有派生根就不允许登记一个将来必然无法签名的端点
	//    （fail closed，与 credentialPepper 同一闸门）。库里只写版本号。
	if _, err := webhookPepper(s); err != nil {
		return nil, err
	}

	// 5. 条数上限（不含软删行，与 CountByApp 口径一致）。
	//    达到上限时先确认这不是「重复注册」：同 (应用, 事件, 地址) 的幂等回放不得新增行，
	//    因此回放必须仍然成功，否则应用满额后就再也查不到自己已登记的端点。
	cfg := s.Config.OpenPlatform
	if cfg.MaxWebhookEndpointsPerApp > 0 {
		cnt, err := s.WebhookEndpoints.CountByApp(ctx, app.AppID)
		if err != nil {
			return nil, err
		}
		if cnt >= int64(cfg.MaxWebhookEndpointsPerApp) {
			if ep, matched, err := findRegisteredEndpoint(ctx, s, app.AppID, eventType, callbackURL); err != nil {
				return nil, err
			} else if matched {
				return replayedWebhook(ep), nil
			}
			return nil, errWebhookEndpointLimit
		}
	}

	// 6. 验证挑战：明文只出现在本次响应里一次，库里存 HMAC 后的摘要与有效期。
	//    verified_at 由 model.Insert 固定写 0，所以新登记的端点不会被 ListMatching 挑中。
	//    回执入口（VerifyWebhook(endpoint_id, challenge)）在当前契约里还不存在，
	//    因此 verified_at 暂无合法写入路径（README「已知缺口」）——本方法不假装能验证。
	challenge, err := model.RandomHex(challengeBytes)
	if err != nil {
		return nil, err
	}
	challengeHash, err := hashOfChallenge(s, challenge)
	if err != nil {
		return nil, err
	}
	now := nowUnix()

	// 7. 写阶段：与其它写路径共用进程级写令牌；单条 INSERT 即完成登记，无跨表事务。
	release, err := writePermit(ctx, s)
	if err != nil {
		return nil, err
	}
	defer release()

	endpointID, created, err := s.WebhookEndpoints.Insert(ctx, &model.WebhookEndpoint{
		AppID:              app.AppID,
		EventType:          eventType,
		URL:                callbackURL,
		SignKeyVersion:     s.Config.Security.KeyVersion,
		Description:        description,
		ChallengeHash:      challengeHash,
		ChallengeExpiresAt: now + challengeTTLSeconds,
	})
	if err != nil {
		return nil, err
	}

	// 8. 幂等回放：uniq_app_event_url 命中时 model 回既有 ID 且 created=false。
	//    此时既不刷新挑战也不返回新明文——库中只存旧挑战的 HMAC，无法反推明文，
	//    重新生成一个「存不进这一行」的挑战只会让应用拿着无效值反复试。
	//    另一种命中是「同地址曾被软删」：那一行永远投不出去，把它的 endpoint_id 当成
	//    可用端点回给应用是骗人，必须显式报错让调用方知道要换地址或走运营清理。
	if !created {
		existing, err := s.WebhookEndpoints.FindByID(ctx, endpointID)
		if err != nil {
			return nil, err
		}
		if existing == nil {
			return nil, model.ErrWebhookNotFound
		}
		if existing.DeletedAt > 0 {
			logx.WithContext(ctx).Errorf("open-platform: 回调端点重复注册撞上已软删行 endpoint_id=%d app_id=%d "+
				"event_type=%d host=%s delete_reason=%q：唯一键仍占用，不返回已下线端点",
				endpointID, app.AppID, eventType, webhookURLHostForLog(callbackURL), existing.DeleteReason)
			return nil, model.ErrWebhookAlreadyRegistered
		}
		logx.WithContext(ctx).Infof("open-platform: RegisterWebhook 命中幂等回放 endpoint_id=%d app_id=%d "+
			"event_type=%d host=%s", endpointID, app.AppID, eventType, webhookURLHostForLog(callbackURL))
		return replayedWebhook(existing), nil
	}

	// 9. 日志只记标识与 host：完整 URL 的查询串可能就是应用自己的鉴权 token。
	logx.WithContext(ctx).Infof("open-platform: 回调端点注册 endpoint_id=%d app_id=%d event_type=%d host=%s "+
		"sign_key_version=%d challenge_expires_at=%d operator_mid=%d is_operator=%t verified_at=0",
		endpointID, app.AppID, eventType, webhookURLHostForLog(callbackURL), s.Config.Security.KeyVersion,
		now+challengeTTLSeconds, in.OperatorMid, in.IsOperator)

	return &rpc.RegisterWebhookReply{
		EndpointId:            endpointID,
		Created:               true,
		SignKeyVersion:        s.Config.Security.KeyVersion,
		VerificationChallenge: challenge,
	}, nil
}

// findRegisteredEndpoint 在「已达条数上限」分支里判断本 (事件, 地址) 是否早已登记。
// 读的是 ListByApp（含停用、不含软删），与 CountByApp 的行集口径一致；
// 地址比对用归一化后的串逐字比较，与 validPublicURI 写入时的规范化同一口径。
func findRegisteredEndpoint(ctx context.Context, s *svc.ServiceContext, appID int64,
	eventType int32, callbackURL string) (*model.WebhookEndpoint, bool, error) {
	rows, err := s.WebhookEndpoints.ListByApp(ctx, appID, true)
	if err != nil {
		return nil, false, err
	}
	for _, ep := range rows {
		if ep == nil || ep.EventType != eventType {
			continue
		}
		if strings.TrimSpace(ep.URL) == callbackURL {
			return ep, true, nil
		}
	}
	return nil, false, nil
}

// replayedWebhook 组装重复登记的响应：created=false、verification_challenge 恒为空串，
// sign_key_version 回库里那一行的真实版本（不是配置当前版本——历史端点仍按自己的版本签名）。
func replayedWebhook(ep *model.WebhookEndpoint) *rpc.RegisterWebhookReply {
	return &rpc.RegisterWebhookReply{
		EndpointId:     ep.EndpointID,
		Created:        false,
		SignKeyVersion: ep.SignKeyVersion,
		// 明文挑战只在首次登记时出现一次；库中只有它的 HMAC，无法二次下发。
		VerificationChallenge: "",
	}
}
