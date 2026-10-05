package logic

import (
	"context"
	"errors"

	"go-video/services/operation/internal/svc"
	"go-video/services/operation/model"
	"go-video/services/operation/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type VerifyAdminPermissionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewVerifyAdminPermissionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *VerifyAdminPermissionLogic {
	return &VerifyAdminPermissionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 权限校验（gateway/admin 每个受保护路由调用；结果带短缓存）
func (l *VerifyAdminPermissionLogic) VerifyAdminPermission(in *rpc.VerifyAdminPermissionReq) (*rpc.VerifyAdminPermissionReply, error) {
	repo := l.svcCtx.Repository
	adminID, err := repo.ResolveAdminID(l.ctx, in.Token, in.AdminId)
	if err != nil {
		if errors.Is(err, model.ErrSessionInvalid) {
			// 会话非法属于「拒绝」而非「故障」：网关据此直接 401，不需要重试。
			return &rpc.VerifyAdminPermissionReply{
				Allowed: false,
				AdminId: in.AdminId,
				Reason:  reasonSessionInvalid,
			}, nil
		}
		l.Errorf("operation/VerifyAdminPermission: resolve admin err=%v", err)
		return nil, err
	}

	decision, err := repo.VerifyPermission(l.ctx, adminID, in.Resource, in.Action)
	if err != nil {
		return nil, err
	}
	reply := &rpc.VerifyAdminPermissionReply{
		Allowed:      decision.Allowed,
		MatchedRoles: decision.MatchedRoles,
		AdminId:      decision.AdminID,
		Ttl:          decision.TTL,
		Reason:       decision.Reason,
	}
	if !decision.Allowed {
		l.Infof("operation/VerifyAdminPermission: denied admin=%d resource=%s action=%s reason=%s cache_hit=%v",
			decision.AdminID, in.Resource, in.Action, decision.Reason, decision.CacheHit)
	}
	return reply, nil
}
