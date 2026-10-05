package logic

import (
	"context"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListApplicationsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListApplicationsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListApplicationsLogic {
	return &ListApplicationsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 开发者/运营侧应用分页列表。
func (l *ListApplicationsLogic) ListApplications(in *rpc.ListApplicationsReq) (*rpc.ListApplicationsReply, error) {
	ctx, s := l.ctx, l.svcCtx

	// 1. 分页归一：ps 超上限直接拒绝而不是静默裁剪（静默裁剪会让调用方以为拿到了全部）。
	ps, err := pageSize(s, in.Ps)
	if err != nil {
		return nil, err
	}
	// 多取一条判 has_more：这是「不重复不遗漏」位点语义的实现前提。
	cursorTime, cursorID, err := decodeCursor(in.Cursor)
	if err != nil {
		return nil, err
	}

	// 2. 权限分支：status 未指定（0）表示不过滤，但必须是合法值，未知值不当成「不过滤」。
	status := appStatusFromRPC(in.Status)
	if status != 0 && !model.ValidAppStatus(status) {
		return nil, errInvalidAppStatusFilter
	}
	var rows []*model.Application
	if in.Operator {
		// 运营全量分支：本 RPC 没有 operator_mid 字段，运营身份由 gateway 的权限位承载
		// （in.Operator 只能由网关按会话角色置位）。服务端保证的是「全表扫描有 ps 上界」。
		rows, err = s.Apps.ListAll(ctx, status, cursorTime, cursorID, ps+1)
	} else {
		if in.OwnerMid <= 0 {
			return nil, model.ErrOwnerRequired
		}
		rows, err = s.Apps.ListByOwner(ctx, in.OwnerMid, status, cursorTime, cursorID, ps+1)
	}
	if err != nil {
		return nil, err
	}

	// 3. 裁页 + 批量补齐派生列（禁止逐行 N+1）。
	page, next, hasMore := trimPage(rows, ps, func(a *model.Application) (int64, int64) {
		return a.Mtime, a.AppID
	})
	list, err := appProjections(ctx, s, page)
	if err != nil {
		return nil, err
	}
	logx.WithContext(ctx).Infof("open-platform: 应用列表 operator=%t owner_mid=%d status=%d n=%d",
		in.Operator, in.OwnerMid, status, len(list))
	// 4. 响应只含 ApplicationInfo：任何情况下都不出现 client_secret / salt / hash。
	return &rpc.ListApplicationsReply{List: list, NextCursor: next, HasMore: hasMore}, nil
}
