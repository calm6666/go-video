package logic

import (
	"context"
	"strings"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RevokeApplicationSecretLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRevokeApplicationSecretLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RevokeApplicationSecretLogic {
	return &RevokeApplicationSecretLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 吊销密钥（疑似泄露的应急处置）。
//
// 契约依据（权威是 proto，不是 README）：
//   - proto:244-246「吊销后所有依赖 secret 的签名调用立即被拒，已签发的 token 不受影响
//     （除非同时 RevokeAuthorization）」→ 本方法**绝不**触碰 op_token/op_grant；
//   - proto:252 reason 必填（审计）；proto:249 secret_id=0 表示吊销全部生效密钥；
//   - README:53 幂等锚点是 status 生效位的 CAS。
func (l *RevokeApplicationSecretLogic) RevokeApplicationSecret(in *rpc.RevokeApplicationSecretReq) (*rpc.RevokeApplicationSecretReply, error) {
	ctx, s := l.ctx, l.svcCtx

	// 1. 身份与审计原因先于任何写：应急路径也不能匿名（proto:250-252）。
	app, err := findApp(ctx, s, in.AppId)
	if err != nil {
		return nil, err
	}
	if err := requireOwnerOrOperator(app, in.OperatorMid, in.IsOperator); err != nil {
		return nil, err
	}
	if err := requireReason(in.Reason); err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(in.Reason)

	release, err := writePermit(ctx, s)
	if err != nil {
		return nil, err
	}
	defer release()

	now := nowUnix()

	// 2a. secret_id=0：吊销该应用全部生效密钥。
	//
	// 应急语义：无宽限期。MarkAllHistory 一条 UPDATE 同时把 status 置历史、expires_at 归零、
	// 并把 reason/operator 落到每一行，因此「谁在什么时候为什么下线了这些密钥」在库里可查。
	// 影响行数天然幂等：条件 status=ACTIVE，第二次调用影响 0 行，回 revoked=0 而不是报错。
	if in.SecretId == 0 {
		revoked, err := s.Secrets.MarkAllHistory(ctx, app.AppID, in.OperatorMid, reason)
		if err != nil {
			return nil, err
		}
		if _, err := s.Apps.NextVersion(ctx, app.AppID); err != nil {
			return nil, err
		}
		logx.WithContext(ctx).Infof("open-platform: 密钥吊销 app_id=%d scope=all revoked=%d "+
			"operator_mid=%d is_operator=%t reason=%q", app.AppID, revoked, in.OperatorMid, in.IsOperator, reason)
		return &rpc.RevokeApplicationSecretReply{Revoked: int32(revoked), EffectiveAt: now}, nil
	}

	// 2b. 指定版本：先确认这一把确实属于本应用（跨应用吊销等于越权）。
	target, err := s.Secrets.FindByID(ctx, in.SecretId)
	if err != nil {
		return nil, err
	}
	if target == nil {
		// 「这一把不存在」是参数错，与「这一把已吊销」（下面 revoked=0 的幂等分支）必须区分：
		// 把 typo 的 secret_id 当幂等成功，会让运营以为密钥已下线而它仍在生效。
		return nil, model.ErrSecretNotConfigured
	}
	if target.AppID != app.AppID {
		return nil, model.ErrOwnerRequired
	}

	// 终态不可逆：model 侧只有 ACTIVE→HISTORY 的单向迁移（AppSecretModel 没有任何复活方法），
	// 因此本方法无需「防止重复吊销」的保护性失败——重复调用只是 0 行变更。
	revoked := int32(0)
	if target.Usable(now) {
		revoked = 1
	}
	// expiresAt=0：立即置历史，不留任何宽限窗口（宽限只属于「计划内轮换」）。
	if err := s.Secrets.MarkHistory(ctx, target.SecretID, 0); err != nil {
		return nil, err
	}
	if _, err := s.Apps.NextVersion(ctx, app.AppID); err != nil {
		return nil, err
	}

	logx.WithContext(ctx).Infof("open-platform: 密钥吊销 app_id=%d scope=single secret_id=%d revoked=%d "+
		"operator_mid=%d is_operator=%t reason=%q", app.AppID, target.SecretID, revoked,
		in.OperatorMid, in.IsOperator, reason)
	return &rpc.RevokeApplicationSecretReply{Revoked: revoked, EffectiveAt: now}, nil
}
