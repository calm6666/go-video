package logic

import (
	"context"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetApplicationLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetApplicationLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetApplicationLogic {
	return &GetApplicationLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询应用（不回显密钥）。
func (l *GetApplicationLogic) GetApplication(in *rpc.GetApplicationReq) (*rpc.GetApplicationReply, error) {
	ctx, s := l.ctx, l.svcCtx

	// 1. app_id 与 app_key 至少一个；两者都给以 app_id 为准。
	//    这条优先级不是随手写的：签名模式下 app_key 是公开标识，
	//    允许它覆盖 app_id 就等于给「拿 app_key 探测他人应用」留了口子。
	if in.AppId <= 0 && in.AppKey == "" {
		return nil, model.ErrInvalidAppID
	}
	var (
		app *model.Application
		err error
	)
	if in.AppId > 0 {
		app, err = s.Apps.FindByID(ctx, in.AppId)
	} else {
		app, err = s.Apps.FindByAppKey(ctx, in.AppKey)
	}
	if err != nil {
		return nil, err
	}
	// 2. 不存在与「存在但无权看」回同一个错误：不区分就不给探测留信号。
	if app == nil {
		return nil, model.ErrAppNotFound
	}

	// 3. 越权判定：owner 看自己；运营可看全部状态；非运营非 owner 一律拒绝。
	//    非运营侧只允许看 ACTIVE —— 待审/驳回/停用应用的身份信息不该对外可见。
	if err := requireOwnerOrOperator(app, in.CallerMid, in.Operator); err != nil {
		return nil, err
	}
	if !in.Operator && !app.IsActive() {
		return nil, model.ErrAppNotFound
	}

	// 4. 投影：scopes 与 secret_state 都是派生值（真值在 op_app_scope / op_app_secret），
	//    本方法不返回 salt/hash/密钥材料，也不返回 register_token（幂等键等同凭证）。
	info, err := appProjection(ctx, s, app)
	if err != nil {
		return nil, err
	}
	return &rpc.GetApplicationReply{App: info}, nil
}
