package logic

import (
	"context"

	"go-video/services/membership/internal/svc"
	"go-video/services/membership/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListEntitlementsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListEntitlementsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListEntitlementsLogic {
	return &ListEntitlementsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListEntitlements 权益码目录读取（admin 写、内部读）。
//
// 下线的码默认仍返回（enabled=false）：判定侧要能区分「码不存在」与「码被运营关掉」，
// 前者是调用方传错（CODE_UNKNOWN），后者是本地开关（CODE_DISABLED）。
func (l *ListEntitlementsLogic) ListEntitlements(in *rpc.ListEntitlementsReq) (*rpc.ListEntitlementsReply, error) {
	rows, err := l.svcCtx.Entitlement.List(l.ctx, in.EnabledOnly)
	if err != nil {
		l.Errorf("membership/ListEntitlements: read failed enabled_only=%v err=%v", in.EnabledOnly, err)
		return nil, err
	}
	return &rpc.ListEntitlementsReply{Entitlements: entitlementListToRPC(rows)}, nil
}
