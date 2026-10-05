// Code scaffolded by goctl. Safe to edit.

package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/event-collector/internal/svc"
	"go-video/services/event-collector/model"
	"go-video/services/event-collector/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListDispatchPoliciesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListDispatchPoliciesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListDispatchPoliciesLogic {
	return &ListDispatchPoliciesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询策略版本。
//
// 只读：不做激活校验、不改变任何状态。输出必须包含 ARCHIVED 历史版本 ——
// 每条 ec_event_record 都写了自己的 policy_version，历史版本是把「当时按什么规则裁决」
// 解释清楚的唯一依据，所以既不过滤也不按版本号合并去重。
//
// 翻页口径：策略版本数量少（每次变更一版），用 id 倒序游标即可，不必像台账那样带 ctime；
// 游标仍是 "0_<id>" 形态，与其余 List 方法同一编解码器（model.EncodeCursor），
// 让 gateway 只维护一套翻页工具函数。
func (l *ListDispatchPoliciesLogic) ListDispatchPolicies(in *rpc.ListDispatchPoliciesReq) (*rpc.ListDispatchPoliciesReply, error) {
	size, err := clampPage(l.svcCtx.Config, in.GetPageSize())
	if err != nil {
		return nil, err
	}
	state := int32(in.GetState())
	if state != 0 && !model.ValidPolicyState(state) {
		return nil, fmt.Errorf("%w: state=%d 不是合法的 PolicyState（1 DRAFT / 2 ACTIVE / 3 ARCHIVED）",
			model.ErrInvalidStateTransition, state)
	}
	afterID, err := policyCursor(in.GetCursor())
	if err != nil {
		return nil, err
	}

	rows, err := l.svcCtx.Policies.List(l.ctx, state, afterID, fetchMoreLimit(size))
	if err != nil {
		return nil, err
	}
	list, hasMore := trimPage(rows, size)
	total, err := l.svcCtx.Policies.Count(l.ctx, state)
	if err != nil {
		return nil, err
	}
	next := ""
	if hasMore && len(list) > 0 {
		next = model.EncodeCursor(0, list[len(list)-1].ID)
	}
	out := make([]*rpc.DispatchPolicy, 0, len(list))
	for _, p := range list {
		v, err := policyToRPC(p)
		if err != nil {
			// 某一行解码失败就整体报错：返回一份「规则为空」的策略清单会让运营以为
			// 「没有采样、也没有禁止字段」，那是比失败更危险的结论。
			return nil, err
		}
		out = append(out, v)
	}
	return &rpc.ListDispatchPoliciesReply{List: out, NextCursor: next, HasMore: hasMore, Total: total}, nil
}

// policyCursor 解析策略翻页游标（只接受 "0_<id>"）。
//
// 带 ctime 的游标来自别的台账接口：两种游标混用会让「在批次页面复制的游标」
// 在策略页面静默退化成第一页，翻页重复计数就是这么来的。
func policyCursor(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	cur, err := model.ParseCursor(s)
	if err != nil {
		return 0, err
	}
	if cur.Ctime != 0 || cur.ID <= 0 {
		return 0, fmt.Errorf("%w: 策略翻页游标必须是 0_<id>（当前为 %q）", model.ErrInvalidCursor,
			fitColumn(s, 64))
	}
	return cur.ID, nil
}
