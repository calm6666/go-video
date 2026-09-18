package logic

import (
	"context"

	"go-video/services/account/internal/svc"
	"go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AddMoral3Logic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAddMoral3Logic(ctx context.Context, svcCtx *svc.ServiceContext) *AddMoral3Logic {
	return &AddMoral3Logic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// AddMoral3 增加道德值。
// 参考 service.AddMoral：透传给 user-profile 服务持久化。
// 业务规则（delta=moral*100、Operator 默认"系统"、RewardType/PunishmentType 选择）
// 由 user-profile 数据所有者负责落地；account 只负责失效缓存。
// 当 user-profile 尚未接入时返回 repository.ErrNotImplemented。
func (l *AddMoral3Logic) AddMoral3(in *rpc.MoralReq) (*rpc.MoralReply, error) {
	if err := l.svcCtx.Repository.AddMoral(l.ctx, in.Mid, in.Moral, in.Oper, in.Reason, in.Remark); err != nil {
		l.Errorf("account/AddMoral3: repository.AddMoral mid=%d moral=%v err=%v", in.Mid, in.Moral, err)
		return nil, err
	}
	return &rpc.MoralReply{}, nil
}
