package repository

import (
	"context"
	"errors"

	"go-video/services/live-ingest/internal/config"
)

// ErrCdnNotConfigured 表示外部 CDN/媒体入口适配器尚未接入。
// 调用方必须把它原样返回给上游，绝不能用「成功」的零值掩盖（AGENTS.md §9）。
var ErrCdnNotConfigured = errors.New("live-ingest: cdn ingest gateway not configured")

// StreamProbe 一次主动健康探测结果（ProbeStream 返回值）。
type StreamProbe struct {
	// Alive 边缘节点上该流当前是否仍在推流。
	Alive bool
	// VideoBitrateBps 探测到的视频码率（bps），未知为 0。
	VideoBitrateBps int64
	// FpsX100 探测到的帧率 ×100，未知为 0。
	FpsX100 int32
	// PacketLossPpm 丢包率（百万分比），未知为 0。
	PacketLossPpm int32
	// ProbedAt 探测时间（Unix 秒）。
	ProbedAt int64
	// Detail 厂商返回的原始状态摘要，已脱敏（不得包含密钥）。
	Detail string
}

// IngestGateway 抽象外部媒体入口/CDN 的管控面调用（README 依赖「CDN/媒体入口」）。
//
// 契约缺口：本仓库不内置真实 RTMP/SRT/WebRTC 服务与 CDN 厂商 SDK，
// 因此本接口只有显式 stub 实现（stubIngestGateway），所有方法返回 ErrCdnNotConfigured。
// 接线方式（后续轮）：
//  1. 在 internal/repository 下新增 <vendor>_gateway.go 实现本接口，
//     超时用 config.CdnConf.RequestTimeoutSeconds，密钥从 CdnConf.CallbackSecretRef
//     指向的 Secret/Vault 读取，绝不写进配置文件或日志；
//  2. 在 internal/svc/servicecontext.go 里按 Cdn.Enabled 替换 stub
//     （配置未打开时继续保持 stub，让「未接入」在测试与线上都是显式错误）；
//  3. 为厂商调用补超时、重试、取消与故障场景测试（AGENTS.md §9）。
//
// 注意：即使接入完成，本接口的失败也不能阻塞状态机推进——流状态以接入节点上报为准，
// 厂商管控面只用于踢流兜底与探测补偿。
type IngestGateway interface {
	// ProbeStream 主动探测某条流在指定节点上的健康状况（GetStreamHealth 的补偿路径）。
	ProbeStream(ctx context.Context, nodeID, streamID string) (*StreamProbe, error)
	// KickStream 让边缘节点立即断开指定推流（强制停流、密钥吊销级联）。
	// 幂等：已经断开的流返回 nil。
	KickStream(ctx context.Context, nodeID, streamID, reason string) error
	// BindCallbackDomain 把推流域名与回调地址绑定（新域名首次签发时）。
	BindCallbackDomain(ctx context.Context, domain string) error
}

// stubIngestGateway 是 IngestGateway 的显式未实现桩。
// 它不返回伪造结果：任何调用都得到 ErrCdnNotConfigured，便于上层明确降级。
type stubIngestGateway struct{}

// NewIngestGateway 按配置构造外部入口适配器。
// 本轮 Cdn.Enabled 恒为 false（示例配置也写死 false），返回的永远是 stub；
// 将来接入厂商 SDK 时在此按配置分支，其他代码不用改。
//
// 契约缺口：真实厂商实现未落地。
func NewIngestGateway(c config.CdnConf) IngestGateway {
	if !c.Enabled {
		return stubIngestGateway{}
	}
	// 即便配置打开了实现，本轮也没有可用适配器：继续返回 stub，
	// 避免「配置说已启用、代码却什么都没做」的静默失败。
	return stubIngestGateway{}
}

// ProbeStream 契约缺口：无厂商实现，明确返回未配置错误。
func (stubIngestGateway) ProbeStream(context.Context, string, string) (*StreamProbe, error) {
	return nil, ErrCdnNotConfigured
}

// KickStream 契约缺口：无厂商实现，明确返回未配置错误。
// 停流的权威路径仍是本服务的状态机（CloseStream/ReportStreamState），
// 厂商踢流只是兜底，缺失时不得阻止事件产出。
func (stubIngestGateway) KickStream(context.Context, string, string, string) error {
	return ErrCdnNotConfigured
}

// BindCallbackDomain 契约缺口：无厂商实现，明确返回未配置错误。
func (stubIngestGateway) BindCallbackDomain(context.Context, string) error {
	return ErrCdnNotConfigured
}
