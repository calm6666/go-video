package logic

import (
	"context"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListScopesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListScopesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListScopesLogic {
	return &ListScopesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// scope 目录（含读写与风险级别声明）。
func (l *ListScopesLogic) ListScopes(in *rpc.ListScopesReq) (*rpc.ListScopesReply, error) {
	ctx, s := l.ctx, l.svcCtx

	// app_id>0 时先确认应用存在：授权页要显示「这个应用申请到哪一步了」，
	// 应用不存在时回空列表会被渲染成「该应用无权限点」，属于误导。
	var app *model.Application
	if in.AppId > 0 {
		var err error
		app, err = findApp(ctx, s, in.AppId)
		if err != nil {
			return nil, err
		}
	} else if in.AppId < 0 {
		return nil, model.ErrInvalidAppID
	}

	defs, err := s.Scopes.List(ctx, in.OnlyEnabled)
	if err != nil {
		return nil, err
	}
	if len(defs) == 0 {
		// 目录为空是 seed 未执行的故障，必须显式暴露：静默回空数组会被当成「平台没有权限点」，
		// 于是所有授权页都渲染成空白，而没人知道是迁移没跑。
		logx.WithContext(ctx).Errorf("open-platform: scope 目录为空，000001 seed 未执行？")
		return &rpc.ListScopesReply{List: []*rpc.ScopeInfo{}}, nil
	}

	// 审批关系一次取回（含待审批与已回收），逐 scope 映射，避免每行一次查询。
	var relations map[string]*model.AppScope
	if app != nil {
		relations, err = s.AppScopes.ListByApp(ctx, app.AppID)
		if err != nil {
			return nil, err
		}
	}

	list := make([]*rpc.ScopeInfo, 0, len(defs))
	for _, def := range defs {
		if def == nil {
			continue
		}
		if model.IsForbiddenScopeCategory(def.Scope) {
			// 红线类目出现在目录里只可能是有人手工改表（AGENTS.md §1：本项目不做会员/订单/
			// 支付/投币/分成/广告）。这里整次调用失败而不是「偷偷过滤掉那一条」：
			// 过滤会让运营以为目录是干净的，把一次数据污染变成永久性隐身故障。
			logx.WithContext(ctx).Errorf("open-platform: scope 目录含未开放类目 %s", def.Scope)
			return nil, model.ErrForbiddenScopeCategory
		}
		list = append(list, projectScope(def, grantedStateOf(relations[def.Scope])))
	}
	return &rpc.ListScopesReply{List: list}, nil
}

// grantedStateOf 把审批关系行映射成对外的 granted_state（0 未申请 / 1 待审批 / 2 已获批）。
//
// 已回收（AppScopeRevoked）映射回 0：回收后开发者应当能重新申请，
// 「已回收」是审计事实而不是开发者可用状态，留在 op_app_scope 行内与 reason 列里。
func grantedStateOf(row *model.AppScope) int32 {
	if row == nil {
		return 0
	}
	switch row.State {
	case model.AppScopePending:
		return 1
	case model.AppScopeGranted:
		return 2
	default:
		return 0
	}
}
