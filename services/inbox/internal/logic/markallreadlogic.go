package logic

import (
	"context"

	"go-video/services/inbox/internal/svc"
	"go-video/services/inbox/model"
	"go-video/services/inbox/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type MarkAllReadLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewMarkAllReadLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MarkAllReadLogic {
	return &MarkAllReadLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// MarkAllRead 幂等把某分类（CATEGORY_UNSPECIFIED 表示全部）标记已读。
// 已全读时 changed=0，不产生任何写放大。
func (l *MarkAllReadLogic) MarkAllRead(in *rpc.MarkAllReadReq) (*rpc.MarkAllReadReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	changed, err := l.svcCtx.Repository.MarkAllRead(l.ctx, in.Mid, convertCategory(in.Category))
	if err != nil {
		return nil, err
	}
	total, err := unreadTotal(l.ctx, l.svcCtx, in.Mid)
	if err != nil {
		return nil, err
	}
	return &rpc.MarkAllReadReply{Changed: int32(changed), UnreadTotal: total}, nil
}
