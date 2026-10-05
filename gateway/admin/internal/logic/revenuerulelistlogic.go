// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	creatorrevenuerpc "go-video/services/creator-revenue/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RevenueRuleListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分成规则分页（后台可见全部状态；含 ARCHIVED，历史周期按它复核）
func NewRevenueRuleListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RevenueRuleListLogic {
	return &RevenueRuleListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// RevenueRuleList 转发 creator-revenue ListRevenueRules（分成规则检索，只读）。
//
// 只读路由，不挂 AdminPermission（见 admin.api 的 creator-revenue 只读面注释），因此这里
// 没有 operator 可言：网关一侧不发任何写、也不看会话。
//
// 网关只挡形状（非负），判定一条都不接管（§5 规则主数据归 creator-revenue）：
//   - state=0 是「不按状态过滤」——后台面必须能看到 ARCHIVED，历史周期是按哪版规则算的
//     只有留着旧行才复核得动；是否「这个状态编号存在」由服务判，网关不替它挑值；
//   - source_type=0 同理是「不按来源过滤」，非 0 是否已定义由服务回 ErrInvalidSourceType；
//   - page/size 越上限是**拒绝**而不是裁剪（裁剪会让运营以为「规则就这么多」，进而漏掉
//     一份还在生效的旧价规则），所以网关更不能自己夹一刀；
//   - 后台面与创作者端的差别（创作者端只查 ACTIVE）在服务的路由语义里，网关不加第二层过滤。
//
// total/page/size 照抄服务回显。空列表投影成 []，但「查不到」在这里不是错误：
// 服务真失败时回的是 error，那一路原样上抛，绝不折叠成空列表冒充成功。
func (l *RevenueRuleListLogic) RevenueRuleList(req *types.ParamRevenueRuleList) (resp *types.RevenueRuleListResponse, err error) {
	if l.svcCtx.CreatorRevenue == nil {
		return nil, errRevenueServiceNotConfigured
	}
	if req == nil {
		return nil, errRevenueRequestMissing
	}
	if err := revenueNonNeg("state", int64(req.State)); err != nil {
		return nil, err
	}
	if err := revenueNonNeg("source_type", int64(req.SourceType)); err != nil {
		return nil, err
	}
	if err := revenueNonNeg("page", req.Page); err != nil {
		return nil, err
	}
	if err := revenueNonNeg("size", req.Size); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.CreatorRevenue.ListRevenueRules(l.ctx, &creatorrevenuerpc.ListRevenueRulesReq{
		State:      creatorrevenuerpc.RuleState(req.State),
		SourceType: creatorrevenuerpc.RevenueSourceType(req.SourceType),
		Page:       req.Page,
		Size:       req.Size,
	})
	if err != nil {
		l.Errorf("gateway/admin/revenueRuleList: state=%d source_type=%d page=%d size=%d err=%v",
			req.State, req.SourceType, req.Page, req.Size, err)
		return nil, err
	}
	return &types.RevenueRuleListResponse{
		Code:    0,
		Message: "ok",
		Data: types.RevenueRuleListData{
			List:  revenueRulesToAPI(reply.GetRules()),
			Total: reply.GetTotal(),
			Page:  reply.GetPage(),
			Size:  reply.GetSize(),
		},
		TTL: 0,
	}, nil
}
