package logic

import (
	"context"
	"strings"

	"go-video/services/audit/internal/svc"
	"go-video/services/audit/model"
	"go-video/services/audit/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListRetentionPoliciesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListRetentionPoliciesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListRetentionPoliciesLogic {
	return &ListRetentionPoliciesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页列出保留期策略
//
// 实现要点（规格 1~4）：
//  1. clampPage 归一化分页，state=0 表示全部（其余只允许 1/2）；
//  2. 排序键固定在 model 侧：action_domain 升序，"default" 稳定排在最前便于人工核对；
//  3. 该表行数等于动作域数量（十位数），COUNT + 分页不构成大表风险；
//  4. 读操作不写审计：策略本身不是存证内容，回参也只有参数；
//     改策略（SaveRetentionPolicy）才留痕。
func (l *ListRetentionPoliciesLogic) ListRetentionPolicies(in *rpc.ListRetentionPoliciesReq) (*rpc.ListRetentionPoliciesReply, error) {
	if in == nil {
		return nil, model.ErrRequestRequired
	}
	cc := in.GetCtx()
	if err := validateCallContext(cc, false); err != nil {
		return nil, err
	}
	state, err := policyStateFilter(in.GetState())
	if err != nil {
		return nil, err
	}
	d := buildDeps(l.svcCtx)
	pn, ps, err := d.clampPage(in.GetPn(), in.GetPs())
	if err != nil {
		return nil, err
	}
	rows, total, err := d.policies.List(l.ctx, state, pn, ps)
	if err != nil {
		l.Errorf("ListRetentionPolicies 查询失败 caller=%s state=%d err=%v",
			strings.TrimSpace(cc.GetCallerService()), state, err)
		return nil, err
	}
	return &rpc.ListRetentionPoliciesReply{Items: retentionViews(rows), Total: total}, nil
}
