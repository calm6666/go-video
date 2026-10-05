// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	operationrpc "go-video/services/operation/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminLoginLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 管理员登录（PBKDF2 校验 + 防爆破锁定 + 可选二次校验），签发后台 token
func NewAdminLoginLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminLoginLogic {
	return &AdminLoginLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 管理员登录：只转发到 operation AdminLogin RPC，口令校验、防爆破锁定与二次校验
// 全部由 operation 判定（AGENTS.md §5：账号/会话主数据不在网关）。
// 网关只挡空值，且任何日志与错误消息都不允许出现口令、二次校验码或签发的 token。
func (l *AdminLoginLogic) AdminLogin(req *types.ParamAdminLogin) (resp *types.AdminLoginResponse, err error) {
	if l.svcCtx.Operation == nil {
		return nil, errors.New("operation service not configured")
	}
	if err := requireNonEmpty("username", req.Username); err != nil {
		return nil, err
	}
	// 口令只做存在性校验：长度与强度策略是 operation 的 domain 规则，网关不复述。
	if err := requireNonEmpty("password", req.Password); err != nil {
		return nil, err
	}
	// request_id 必填：登录会写后台会话与审计索引，必须可归因到一次点击。
	if err := requireNonEmpty("request_id", req.RequestId); err != nil {
		return nil, err
	}

	reply, err := l.svcCtx.Operation.AdminLogin(l.ctx, &operationrpc.AdminLoginReq{
		Username:     req.Username,
		Password:     req.Password,
		SecondFactor: req.SecondFactor,
		Ip:           req.Ip,
		UserAgent:    req.UserAgent,
		TraceId:      req.TraceId,
		RequestId:    req.RequestId,
	})
	if err != nil {
		// 失败可能是口令错误或账号锁定：只回显账号名与幂等键，其它字段一律不落日志。
		l.Errorf("gateway/admin/adminLogin: username=%q request_id=%s err=%v", req.Username, req.RequestId, err)
		return nil, err
	}
	l.Infof("gateway/admin/adminLogin: username=%q admin_id=%d roles=%d expires_at=%d",
		req.Username, reply.GetAdminId(), len(reply.GetRoles()), reply.GetExpiresAt())
	return &types.AdminLoginResponse{
		Code:    0,
		Message: "ok",
		Data: types.AdminLoginData{
			Token:     reply.GetToken(),
			AdminId:   reply.GetAdminId(),
			Username:  reply.GetUsername(),
			ExpiresAt: reply.GetExpiresAt(),
			Roles:     reply.GetRoles(),
		},
		// 后台登录态固定不建议客户端缓存（operation 侧 AdminLoginReply.ttl=0）。
		TTL: int64TTL(reply.GetTtl()),
	}, nil
}
