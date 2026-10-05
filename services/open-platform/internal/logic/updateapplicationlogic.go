package logic

import (
	"context"
	"strings"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type UpdateApplicationLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpdateApplicationLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateApplicationLogic {
	return &UpdateApplicationLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 修改资料或推进状态机（乐观锁版本）。
func (l *UpdateApplicationLogic) UpdateApplication(in *rpc.UpdateApplicationReq) (*rpc.UpdateApplicationReply, error) {
	ctx, s := l.ctx, l.svcCtx

	app, err := findApp(ctx, s, in.AppId)
	if err != nil {
		return nil, err
	}

	// 1. 身份与通道分离（本方法最关键的一条门禁）。
	//    开发者只能改资料、不能自批状态；运营只推进状态、不替开发者改简介。
	//    混用会造成「运营顺手改了下线原因，把开发者的回调白名单也覆盖了」这类越权副作用。
	target := appStatusFromRPC(in.TargetStatus)
	if in.IsOperator {
		if target == 0 {
			return nil, errTargetStatusRequired
		}
		if in.Name != "" || in.Description != "" || len(in.RedirectUris) > 0 {
			return nil, model.ErrOwnerRequired
		}
		if err := requireOperator(in.OperatorMid); err != nil {
			return nil, err
		}
		if err := requireReason(in.Reason); err != nil {
			return nil, err
		}
		return l.applyStatus(app, in, target)
	}
	if target != 0 {
		// 开发者请求推进状态：拒绝而不是忽略——忽略等于把「我以为改了」变成静默无操作。
		return nil, model.ErrOwnerRequired
	}
	if err := requireOwnerOrOperator(app, in.OperatorMid, false); err != nil {
		return nil, err
	}
	return l.applyProfile(app, in)
}

// applyProfile owner 改资料：与注册同一套校验，空值表示「不修改」。
//
// 本方法不提供「清空」语义：清空回调白名单会让用户再也无法通过原地址走完撤销流程，
// 属于需要运营显式处置的动作（AGENTS.md §5 的可逆性要求）。
func (l *UpdateApplicationLogic) applyProfile(app *model.Application, in *rpc.UpdateApplicationReq) (*rpc.UpdateApplicationReply, error) {
	ctx, s := l.ctx, l.svcCtx
	if in.ExpectedVersion <= 0 {
		return nil, model.ErrConcurrentUpdate
	}
	name, err := optionalLen(in.Name, maxAppNameRunes, errNameTooLong)
	if err != nil {
		return nil, err
	}
	description, err := optionalLen(in.Description, maxAppDescRunes, errDescriptionTooLong)
	if err != nil {
		return nil, err
	}
	redirectURIs, err := normalizeRedirectURIs(s, in.RedirectUris)
	if err != nil {
		return nil, err
	}
	release, err := writePermit(ctx, s)
	if err != nil {
		return nil, err
	}
	defer release()

	// CAS：WHERE version=?，0 行即 ErrConcurrentUpdate；禁止「先查后改」的非原子写法。
	if err := s.Apps.UpdateProfile(ctx, app.AppID, name, description, redirectURIs, in.ExpectedVersion); err != nil {
		return nil, err
	}
	fresh, err := findApp(ctx, s, app.AppID)
	if err != nil {
		return nil, err
	}
	info, err := appProjection(ctx, s, fresh)
	if err != nil {
		return nil, err
	}
	logx.WithContext(ctx).Infof("open-platform: app_id=%d 资料更新 owner_mid=%d version=%d",
		app.AppID, in.OperatorMid, fresh.Version)
	return &rpc.UpdateApplicationReply{App: info, Changed: true}, nil
}

// applyStatus 运营推进状态机，并在转入不可用态时连带吊销凭证与抑制外呼。
func (l *UpdateApplicationLogic) applyStatus(app *model.Application, in *rpc.UpdateApplicationReq,
	to int32) (*rpc.UpdateApplicationReply, error) {
	ctx, s := l.ctx, l.svcCtx
	if !model.ValidAppStatus(to) {
		return nil, errInvalidTargetStatus
	}
	if !model.CanTransitionAppStatus(app.Status, to) {
		return nil, model.ErrInvalidStateTransition
	}
	// requireReason 已在入口处校验过非空与长度（含 errReasonTooLong），
	// 这里的 clipRunes 只是防「以后有人新增调用点绕过入口」，正常路径下是恒等变换。
	reason := clipRunes(in.Reason, maxReasonRunes)
	release, err := writePermit(ctx, s)
	if err != nil {
		return nil, err
	}
	defer release()

	// UpdateStatus 自带 WHERE status=from 的 CAS（0 行 → ErrConcurrentUpdate）。
	// 它走的是连接池而不是 session，因此无法与「token 批量吊销」放进同一事务，
	// 顺序上必须先推状态：状态是不可用性的真值，凭证吊销是收尾。
	// 反过来（先吊销后改状态）才会在中途失败时留下「应用还在但全被吊销」的假可用状态。
	if err := s.Apps.UpdateStatus(ctx, app.AppID, app.Status, to, in.OperatorMid, reason, 0); err != nil {
		return nil, err
	}

	if to == model.AppStatusSuspended || to == model.AppStatusOffline {
		if err := revokeAppCredentials(ctx, s, app.AppID, in.OperatorMid, reason); err != nil {
			// 状态已经推进，Introspect/AuthorizeRequest 的「应用必须 ACTIVE」门禁即刻生效，
			// 因此这里失败不会让被停用应用继续可用；但 op_token.state 仍留着 ACTIVE，
			// 必须告警交给重算/清理任务收敛，不能吞掉。
			logx.WithContext(ctx).Errorf("open-platform: app_id=%d 停用后凭证吊销失败: %v", app.AppID, err)
		}
		// 这里刻意不入队 WEBHOOK_EVENT_TYPE_GRANT_REVOKED：
		// 该事件的正文契约是「单个 grant_id 被撤销」（见 notifyGrantRevoked），
		// 而 Tokens.RevokeByApp 只回受影响行数、拿不到 grant_id 集合，
		// 造一条 grant_id=0 的通知会让订阅方无法与自己的授权表对账（比不发更糟）。
		// 缺口登记在 README「已知缺口」：需要 per-grant 通知就得给 model 增加「返回被撤销 grant_id 列表」的口径。
		logx.WithContext(ctx).Infof("open-platform: app_id=%d status=%d 凭证已吊销 operator=%d",
			app.AppID, to, in.OperatorMid)
	}

	fresh, err := findApp(ctx, s, app.AppID)
	if err != nil {
		return nil, err
	}
	info, err := appProjection(ctx, s, fresh)
	if err != nil {
		return nil, err
	}
	logx.WithContext(ctx).Infof("open-platform: app_id=%d 状态 %d→%d operator=%d",
		app.AppID, app.Status, to, in.OperatorMid)
	return &rpc.UpdateApplicationReply{App: info, Changed: true}, nil
}

// revokeAppCredentials 一次性作废该应用全部 ACTIVE/ROTATED token，并抑制未完成的回调任务。
//
// 两条集合化 UPDATE 而不是分页遍历：撤销的安全属性不能随数据规模退化。
func revokeAppCredentials(ctx context.Context, s *svc.ServiceContext, appID, operator int64,
	reason string) error {
	now := nowUnix()
	return s.DB.TransactCtx(ctx, func(tctx context.Context, session sqlx.Session) error {
		if _, err := s.Tokens.RevokeByApp(tctx, session, appID, now, reason); err != nil {
			return err
		}
		endpoints, err := s.WebhookEndpoints.ListByApp(tctx, appID, false)
		if err != nil {
			return err
		}
		for _, ep := range endpoints {
			if ep == nil {
				continue
			}
			// 逐端点抑制：SuppressByEndpoint 的入参是 endpoint_id，没有「按应用批量」的口径。
			// 端点数受 MaxWebhookEndpointsPerApp 约束，因此这里的循环规模有上界。
			if _, err := s.WebhookDeliveries.SuppressByEndpoint(tctx, session, ep.EndpointID,
				"application disabled", now); err != nil {
				return err
			}
		}
		return nil
	})
}

// requireReason 破坏性/审批操作的审计原因必填。
func requireReason(v string) error {
	if len(strings.TrimSpace(v)) == 0 {
		return errReasonRequired
	}
	if utf8Len(v) > maxReasonRunes {
		return errReasonTooLong
	}
	return nil
}
