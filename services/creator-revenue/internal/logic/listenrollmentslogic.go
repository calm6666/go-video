package logic

import (
	"context"

	"go-video/services/creator-revenue/internal/svc"
	"go-video/services/creator-revenue/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListEnrollmentsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListEnrollmentsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListEnrollmentsLogic {
	return &ListEnrollmentsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 运营面：参与名单分页
//
// 判定口径同 ListRevenueRules：state=0 不过滤、page/size 有界并回显、
// 查询错误一律上抛（故障不能渲染成「名单是空的」）、空结果投影成非 nil 空数组。
func (l *ListEnrollmentsLogic) ListEnrollments(
	in *rpc.ListEnrollmentsReq,
) (*rpc.ListEnrollmentsReply, error) {
	if in == nil {
		in = &rpc.ListEnrollmentsReq{}
	}
	if err := l.svcCtx.Ready(); err != nil {
		return nil, err
	}
	if err := validEnrollmentStateFilter(int32(in.State)); err != nil {
		return nil, err
	}
	page := l.svcCtx.PageSize(in.Page, in.Size)
	state := int32(in.State)

	rows, err := l.svcCtx.Enrollments.List(l.ctx, state, page.Offset, page.Limit)
	if err != nil {
		l.Errorf("ListEnrollments failed, state=%d page=%d size=%d: %v", state, page.RequestedPage, page.RequestedSize, err)
		return nil, err
	}
	total, err := l.svcCtx.Enrollments.Count(l.ctx, state)
	if err != nil {
		l.Errorf("ListEnrollments count failed, state=%d: %v", state, err)
		return nil, err
	}
	return &rpc.ListEnrollmentsReply{
		Enrollments: enrollmentInfos(rows),
		Total:       total,
		Page:        page.RequestedPage,
		Size:        page.RequestedSize,
	}, nil
}
