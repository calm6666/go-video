package logic

import (
	"context"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListQuotaPoliciesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListQuotaPoliciesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListQuotaPoliciesLogic {
	return &ListQuotaPoliciesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 配额规则分页（运营只读面，规则是限额真值）。
//
// 错误映射：ErrOperatorRequired→PermissionDenied、ErrInvalidAppID/ErrPsTooLarge/
// ErrInvalidPage/ErrInvalidCursor/errAPICodeRequired/ErrScopeUnknown→InvalidArgument、
// 其它（model 包装的 SQL 错误）→Internal。空结果回空数组而不是报错。
func (l *ListQuotaPoliciesLogic) ListQuotaPolicies(in *rpc.ListQuotaPoliciesReq) (*rpc.ListQuotaPoliciesReply, error) {
	ctx, s := l.ctx, l.svcCtx

	// 1. 身份：规则目录只服务运营。应用 owner 要看「自己还剩多少额度」走 ListQuotaUsage，
	//    这里不把规则集按应用切片外发（限额本身对客户端不敏感，但规则集含运营意图）。
	//    gateway 的权限位是第一道门，operator_mid>0 是本服务的兜底：网关漏配时
	//    宁可拒绝也不把全量规则目录暴露出去。
	if err := requireOperator(in.OperatorMid); err != nil {
		return nil, err
	}

	// 2. 过滤：app_id=0 是合法取值（只看全局默认层级，与 op_quota_policy 的 GlobalAppID 同口径），
	//    负数才是参数错；api_code 为空表示不过滤，而 "*" 是规则本身的取值（通配规则），
	//    绝不能当「不过滤」处理，否则「列出通配规则」这个查询无法表达。
	if in.AppId < 0 {
		return nil, model.ErrInvalidAppID
	}
	apiCode, err := optionalLen(in.ApiCode, maxAPICodeRunes, errAPICodeRequired)
	if err != nil {
		return nil, err
	}
	if apiCode != "" {
		if err := validScopeToken(apiCode); err != nil {
			return nil, err
		}
	}

	// 3. 分页：ps 归一与上限同 ListApplications（超上限拒绝而不是静默裁剪）；
	//    cursor 是 (mtime, policy_id) 倒序位点，与 QuotaPolicyModel.ListByApp 的 ORDER BY 完全一致，
	//    位点语义由 trimPage 统一实现（多取一条判 has_more）。
	ps, err := pageSize(s, in.Ps)
	if err != nil {
		return nil, err
	}
	cursorTime, cursorID, err := decodeCursor(in.Cursor)
	if err != nil {
		return nil, err
	}

	rows, err := s.QuotaPolicies.ListByApp(ctx, in.AppId, apiCode, cursorTime, cursorID, ps+1)
	if err != nil {
		return nil, err
	}
	page, next, hasMore := trimPage(rows, ps, func(p *model.QuotaPolicy) (int64, int64) {
		return p.Mtime, p.PolicyID
	})

	// 4. 投影：直接回规则行原值，不合成「生效层级」结论——层级由 AuthorizeRequest 侧的
	//    model.NarrowPolicies 决定，读面再实现一套排序就会与真值漂移；要看实际效果用 ListQuotaUsage。
	list := make([]*rpc.QuotaPolicyInfo, 0, len(page))
	for _, p := range page {
		if info := projectQuotaPolicy(p); info != nil {
			list = append(list, info)
		}
	}
	logx.WithContext(ctx).Infof("open-platform: 配额规则列表 operator_mid=%d app_id=%d api_code=%s n=%d",
		in.OperatorMid, in.AppId, apiCode, len(list))

	return &rpc.ListQuotaPoliciesReply{List: list, NextCursor: next, HasMore: hasMore}, nil
}
