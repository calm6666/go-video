package logic

import (
	"context"
	"fmt"

	"go-video/services/creator-revenue/internal/svc"
	"go-video/services/creator-revenue/model"
	"go-video/services/creator-revenue/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListRevenueRulesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListRevenueRulesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListRevenueRulesLogic {
	return &ListRevenueRulesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 规则列表（运营面可见全部，创作者端只查 ACTIVE）。
//
// 判定口径：
//   - state / source_type 为 UNSPECIFIED(0) 表示不过滤（列表接口的通用约定）；
//   - page/size 由 svc.PageSize 折进 [1, MaxPageSize]，实际生效值回显在 reply 里，
//     调用方看得见自己被截断了多少；
//   - 查询失败一律上抛错误。把 DB 故障折叠成「空规则列表」会让运营以为
//     「平台还没配规则」，属于伪成功（AGENTS.md §9）；
//   - 空结果投影成非 nil 空数组（reply.rules 恒可安全 range）。
func (l *ListRevenueRulesLogic) ListRevenueRules(
	in *rpc.ListRevenueRulesReq,
) (*rpc.ListRevenueRulesReply, error) {
	if in == nil {
		in = &rpc.ListRevenueRulesReq{}
	}
	if err := l.svcCtx.Ready(); err != nil {
		return nil, err
	}
	if in.SourceType != 0 && !model.SourceTypeValid(int32(in.SourceType)) {
		return nil, fmt.Errorf("%w: %d", model.ErrInvalidSourceType, in.SourceType)
	}
	page := l.svcCtx.PageSize(in.Page, in.Size)

	rows, err := l.svcCtx.Rules.List(l.ctx, int32(in.State), int32(in.SourceType), page.Offset, page.Limit)
	if err != nil {
		l.Errorf("ListRevenueRules failed, state=%d source_type=%d page=%d size=%d: %v",
			in.State, in.SourceType, page.RequestedPage, page.RequestedSize, err)
		return nil, err
	}
	total, err := l.svcCtx.Rules.Count(l.ctx, int32(in.State), int32(in.SourceType))
	if err != nil {
		l.Errorf("ListRevenueRules count failed, state=%d source_type=%d: %v", in.State, in.SourceType, err)
		return nil, err
	}
	return &rpc.ListRevenueRulesReply{
		Rules: ruleInfos(rows),
		Total: total,
		Page:  page.RequestedPage,
		Size:  page.RequestedSize,
	}, nil
}
