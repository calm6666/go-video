package repository

// 本文件是 operation 服务对下游领域服务的 RPC 客户端适配层。
//
// 数据所有权约束（AGENTS.md §5）：operation 只对自有表读写，对稿件下架、目录
// 上下架、版权窗口过期、申诉处理等**业务写操作**一律通过下游服务的公开 RPC 完成，
// 不 import 其它服务的 internal/model 包，也不直连它们的库表。
// 所有客户端在配置里都是 optional：未配置时字段为 nil，调用返回
// model.ErrDownstreamUnavailable，任务步骤被标记为 failed 而不是静默成功。

import (
	"context"
	"errors"

	"github.com/zeromicro/go-zero/zrpc"

	accountrpc "go-video/services/account/rpc"
	catalogrpc "go-video/services/catalog/rpc"
	moderationrpc "go-video/services/moderation-orchestrator/rpc"
	"go-video/services/operation/model"
	rightsrpc "go-video/services/rights/rpc"
	videorpc "go-video/services/video/rpc"
)

// VideoGateway 稿件运营动作（下架/删除由 video 服务的状态机裁决）。
type VideoGateway interface {
	// OfflineSubmission 把稿件推进到 OFFLINE；operator 必须是管理员用户名。
	OfflineSubmission(ctx context.Context, aid int64, operator, reason, ip string) error
}

// CatalogGateway 版权目录运营动作。
type CatalogGateway interface {
	// OfflineEpisode 下架某集。
	OfflineEpisode(ctx context.Context, episodeID int64) error
}

// RightsGateway 版权窗口运营动作。
type RightsGateway interface {
	// ExpireWindow 让版权窗口立即失效。
	ExpireWindow(ctx context.Context, windowID int64, ip string) error
}

// ModerationGateway 审核工作台运营动作。
type ModerationGateway interface {
	// RejectAppeal 以“维持驳回”的结论处理申诉。
	RejectAppeal(ctx context.Context, appealID, handlerAdminID int64, reason, ip string) error
}

// AccountGateway 复用 account 的验证码通道做管理员二次校验。
// 只调用其公开 RPC，绝不读写 account 的账号/会话表；
// 管理员 token 与用户 token 分属两套存储（op_admin_session vs account_session）。
type AccountGateway interface {
	// CheckSecondFactor 校验管理员登录的二次校验码（业务码沿用 account 的“登录”语义）。
	CheckSecondFactor(ctx context.Context, target, code string) error
}

// Downstream 聚合全部可选客户端；nil 字段表示对应服务未部署。
type Downstream struct {
	Video      VideoGateway
	Catalog    CatalogGateway
	Rights     RightsGateway
	Moderation ModerationGateway
	Account    AccountGateway
}

// videoGateway 是 VideoGateway 的 gRPC 实现。
type videoGateway struct {
	cli videorpc.VideoClient
}

// NewVideoGateway 构造 video 客户端适配器。
func NewVideoGateway(c zrpc.RpcClientConf) VideoGateway {
	return &videoGateway{cli: videorpc.NewVideoClient(zrpc.MustNewClient(c).Conn())}
}

func (g *videoGateway) OfflineSubmission(ctx context.Context, aid int64, operator, reason, ip string) error {
	_, err := g.cli.TransitionState(ctx, &videorpc.TransitionReq{
		Aid:      aid,
		Target:   videorpc.SubmissionState_STATE_OFFLINE,
		Operator: operator,
		Reason:   reason,
		Ip:       ip,
	})
	return err
}

// catalogGateway 是 CatalogGateway 的 gRPC 实现。
type catalogGateway struct {
	cli catalogrpc.CatalogClient
}

// NewCatalogGateway 构造 catalog 客户端适配器。
func NewCatalogGateway(c zrpc.RpcClientConf) CatalogGateway {
	return &catalogGateway{cli: catalogrpc.NewCatalogClient(zrpc.MustNewClient(c).Conn())}
}

func (g *catalogGateway) OfflineEpisode(ctx context.Context, episodeID int64) error {
	_, err := g.cli.OfflineEpisode(ctx, &catalogrpc.EpisodeReq{Epid: episodeID})
	return err
}

// rightsGateway 是 RightsGateway 的 gRPC 实现。
type rightsGateway struct {
	cli rightsrpc.RightsClient
}

// NewRightsGateway 构造 rights 客户端适配器。
func NewRightsGateway(c zrpc.RpcClientConf) RightsGateway {
	return &rightsGateway{cli: rightsrpc.NewRightsClient(zrpc.MustNewClient(c).Conn())}
}

func (g *rightsGateway) ExpireWindow(ctx context.Context, windowID int64, ip string) error {
	_, err := g.cli.ExpireWindow(ctx, &rightsrpc.WindowReq{WindowId: windowID, Ip: ip})
	return err
}

// moderationGateway 是 ModerationGateway 的 gRPC 实现。
type moderationGateway struct {
	cli moderationrpc.ModerationOrchestratorClient
}

// NewModerationGateway 构造 moderation-orchestrator 客户端适配器。
func NewModerationGateway(c zrpc.RpcClientConf) ModerationGateway {
	return &moderationGateway{cli: moderationrpc.NewModerationOrchestratorClient(zrpc.MustNewClient(c).Conn())}
}

func (g *moderationGateway) RejectAppeal(ctx context.Context, appealID, handlerAdminID int64, reason, ip string) error {
	_, err := g.cli.ProcessAppeal(ctx, &moderationrpc.ProcessAppealReq{
		AppealId:     appealID,
		Handler:      handlerAdminID,
		FinalVerdict: moderationrpc.Verdict_VERDICT_REJECT,
		FinalReason:  reason,
		Ip:           ip,
	})
	return err
}

// accountGateway 是 AccountGateway 的 gRPC 实现。
type accountGateway struct {
	cli accountrpc.AccountClient
}

// NewAccountGateway 构造 account 客户端适配器。
func NewAccountGateway(c zrpc.RpcClientConf) AccountGateway {
	return &accountGateway{cli: accountrpc.NewAccountClient(zrpc.MustNewClient(c).Conn())}
}

// captureBizLogin 与 account 的验证码业务码保持一致：1 登录。
const captureBizLogin = 1

func (g *accountGateway) CheckSecondFactor(ctx context.Context, target, code string) error {
	if target == "" || code == "" {
		return model.ErrSecondFactorRequired
	}
	reply, err := g.cli.CheckCapture(ctx, &accountrpc.CheckCaptureReq{
		Biz:         captureBizLogin,
		Target:      target,
		CaptureCode: code,
	})
	if err != nil {
		// account 对“验证码错/超次/过期”返回不同错误；统一收敛为二次校验失败，
		// 避免把上游文案透传给后台造成枚举猜测。
		return model.ErrSecondFactorWrong
	}
	if reply == nil {
		return model.ErrSecondFactorWrong
	}
	return nil
}

// ErrNotConfigured 是 nil 客户端的显式错误，包装后由调用方转成下游不可用。
var ErrNotConfigured = errors.New("operation: downstream not configured")

// requireVideo 等辅助函数保证 nil 客户端不会引发 panic。
func (d *Downstream) requireVideo() (VideoGateway, error) {
	if d == nil || d.Video == nil {
		return nil, model.ErrDownstreamUnavailable
	}
	return d.Video, nil
}

func (d *Downstream) requireCatalog() (CatalogGateway, error) {
	if d == nil || d.Catalog == nil {
		return nil, model.ErrDownstreamUnavailable
	}
	return d.Catalog, nil
}

func (d *Downstream) requireRights() (RightsGateway, error) {
	if d == nil || d.Rights == nil {
		return nil, model.ErrDownstreamUnavailable
	}
	return d.Rights, nil
}

func (d *Downstream) requireModeration() (ModerationGateway, error) {
	if d == nil || d.Moderation == nil {
		return nil, model.ErrDownstreamUnavailable
	}
	return d.Moderation, nil
}

func (d *Downstream) requireAccount() (AccountGateway, error) {
	if d == nil || d.Account == nil {
		return nil, model.ErrDownstreamUnavailable
	}
	return d.Account, nil
}
