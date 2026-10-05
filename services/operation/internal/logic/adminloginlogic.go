package logic

import (
	"context"

	"go-video/services/operation/internal/repository"
	"go-video/services/operation/internal/svc"
	"go-video/services/operation/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminLoginLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminLoginLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminLoginLogic {
	return &AdminLoginLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// AdminLogin 管理员登录：PBKDF2 口令校验 + 防爆破锁定 + 可选二次校验，签发后台专用 token。
// 响应只含 token 与角色名，绝不含口令散列；失败文案对外统一，避免账号枚举。
func (l *AdminLoginLogic) AdminLogin(in *rpc.AdminLoginReq) (*rpc.AdminLoginReply, error) {
	res, err := l.svcCtx.Repository.AdminLogin(l.ctx, repository.LoginInput{
		Username:     in.Username,
		Password:     in.Password,
		SecondFactor: in.SecondFactor,
		Actor: repository.Actor{
			IP:        in.Ip,
			UserAgent: in.UserAgent,
			TraceID:   in.TraceId,
			RequestID: in.RequestId,
		},
	})
	if err != nil {
		// 不打印口令与来源 IP（IP 只做哈希落库，日志同样不落明文）。
		l.Errorf("operation/AdminLogin: username=%q err=%v", in.Username, err)
		return nil, err
	}
	return &rpc.AdminLoginReply{
		Token:     res.Token,
		AdminId:   res.AdminID,
		Username:  res.Username,
		ExpiresAt: res.ExpiresAt,
		Roles:     res.Roles,
		// Ttl 固定 0，不外传 res.TTL（token 生命周期）。
		// 契约依据：rpc/operation.proto:58 明确「建议客户端缓存秒数（后台登录态固定 0，
		// 不建议缓存）」，gateway/admin/internal/logic/adminloginlogic.go:77-78 也按
		// 「operation 侧 AdminLoginReply.ttl=0」把该字段原样搬进 HTTP 信封的 ttl。
		// 传 7200 等于命令后台控制台把「含 token 的登录响应」缓存 2 小时：
		// token 被禁用/改密吊销后控制台仍可能复用旧响应，且与自身契约相矛盾。
		// token 真实到期时间由 expires_at 单点承载（由会话行的 expires 读出），不受影响。
		Ttl: 0,
	}, nil
}
