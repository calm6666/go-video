// Code scaffolded by goctl. Safe to edit.

package svc

import (
	"fmt"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"github.com/zeromicro/go-zero/zrpc"

	"go-video/services/live-room/internal/config"
	"go-video/services/live-room/model"

	creatorrpc "go-video/services/creator/rpc"
	moderationrpc "go-video/services/moderation-orchestrator/rpc"
	riskrpc "go-video/services/risk-control/rpc"
)

// ServiceContext 是 live-room 的运行时上下文，承载跨请求共享的依赖。
//
// 装配原则（AGENTS.md §3、§5）：
//   - 只拥有 go_video_live_room 库的 8 张 live_* 表；任何跨域读取（主播身份、风控结论、
//     审核送审）都通过下面的下游 zrpc client 完成，绝不 import 其它服务的 internal/model，
//     也不直连它们的库表或 Redis key；
//   - 不 import live-ingest 的 rpc 包：流密钥/流状态的真值由 live-ingest 拥有，
//     live-room 侧只通过 ReportStreamState 入站接收投影（见 README「与 live-ingest 的边界」）；
//   - 下游 client 全部可选：Target/Etcd.Hosts/Endpoints 皆空时字段保持 nil，
//     调用点必须返回 model.ErrCreatorNotConfigured / ErrRiskControlNotConfigured /
//     ErrModerationNotConfigured 显式失败，而不是「当作检查通过」；
//   - CacheRedis 只放房间详情、分区树与 PrepareLive 结果等易失副本，
//     真值恒在 MySQL；缓存缺失只回源，不影响正确性；
//   - MySQL 与 CacheRedis 是硬依赖（状态机 CAS 与幂等都要落库），沿用 MustNewRedis：
//     连不上就启动失败，不带着半残依赖对外服务。
type ServiceContext struct {
	Config config.Config

	// DB 本服务自有库的连接（go_video_live_room），logic 里需要自己起事务时使用。
	DB sqlx.SqlConn

	// Cache 业务缓存；nil 表示未配置，调用方必须回落到 DB 而不是返回空结果。
	Cache *redis.Redis

	// --- 本服务自有表的数据访问（一轮交付：SQL 与 CAS 语义已定，logic 二轮接线）---
	Rooms       model.LiveRoomModel
	Settings    model.LiveRoomSettingModel
	Anchors     model.LiveRoomAnchorModel
	Sessions    model.LiveSessionModel
	Bans        model.LiveRoomBanModel
	Areas       model.LiveAreaModel
	StateLogs   model.LiveRoomStateLogModel
	Idempotency model.LiveRoomIdempotencyModel

	// --- 下游领域服务客户端（nil 表示本环境未接入）---

	// Creator 读主播身份/直播资格（PrepareLive 的 anchor_qualification 检查项）。
	Creator creatorrpc.CreatorClient
	// RiskControl 开播前风控判定（CheckAction ACTION_LIVE_START）与主播处罚查询。
	RiskControl riskrpc.RiskControlClient
	// Moderation 房间资料送审（SubmitForReview，ContentType=LIVE）。
	Moderation moderationrpc.ModerationOrchestratorClient
}

// NewServiceContext 构造上下文。
//
// 这里刻意不做「下游连不上就退出」的强校验：领域服务之间必须能在灰度/降级环境里
// 独立启动（NonBlock: true），缺依赖的后果推迟到调用点显式报错。
// 配置本身的完整性由 internal/config/config_load_test.go 在 CI 阶段拦住。
func NewServiceContext(c config.Config) *ServiceContext {
	conn := sqlx.NewMysql(c.DataSource)
	ctx := &ServiceContext{
		Config:      c,
		DB:          conn,
		Cache:       newCache(c),
		Rooms:       model.NewLiveRoomModel(conn),
		Settings:    model.NewLiveRoomSettingModel(conn),
		Anchors:     model.NewLiveRoomAnchorModel(conn),
		Sessions:    model.NewLiveSessionModel(conn),
		Bans:        model.NewLiveRoomBanModel(conn),
		Areas:       model.NewLiveAreaModel(conn),
		StateLogs:   model.NewLiveRoomStateLogModel(conn),
		Idempotency: model.NewLiveRoomIdempotencyModel(conn),
		Creator:     newCreatorClient(c.CreatorRPC),
		RiskControl: newRiskControlClient(c.RiskControlRPC),
		Moderation:  newModerationClient(c.ModerationRPC),
	}
	for _, note := range ctx.Notes() {
		logx.Infof("live-room/svc: %s", note)
	}
	return ctx
}

// newCache 构造业务缓存；Host 为空时返回 nil，由调用方回落到 MySQL。
func newCache(c config.Config) *redis.Redis {
	if c.CacheRedis.Host == "" {
		return nil
	}
	return redis.MustNewRedis(c.CacheRedis)
}

// rpcConfigured 与 internal/config/config_load_test.go 共用同一判定，
// 保证「测试里认为可留空」与「svc 里是否真的构造客户端」不会各说各话。
func rpcConfigured(c zrpc.RpcClientConf) bool {
	return c.Target != "" || len(c.Etcd.Hosts) > 0 || len(c.Endpoints) > 0
}

func newCreatorClient(c zrpc.RpcClientConf) creatorrpc.CreatorClient {
	if !rpcConfigured(c) {
		return nil
	}
	return creatorrpc.NewCreatorClient(zrpc.MustNewClient(c).Conn())
}

func newRiskControlClient(c zrpc.RpcClientConf) riskrpc.RiskControlClient {
	if !rpcConfigured(c) {
		return nil
	}
	return riskrpc.NewRiskControlClient(zrpc.MustNewClient(c).Conn())
}

func newModerationClient(c zrpc.RpcClientConf) moderationrpc.ModerationOrchestratorClient {
	if !rpcConfigured(c) {
		return nil
	}
	return moderationrpc.NewModerationOrchestratorClient(zrpc.MustNewClient(c).Conn())
}

// Notes 输出装配期诊断信息，便于运维确认「哪些 RPC 在这个环境必然失败」。
func (s *ServiceContext) Notes() []string {
	type entry struct {
		name    string
		built   bool
		apis    string
		missing error
	}
	entries := []entry{
		{"CreatorRPC", s.Creator != nil, "PrepareLive(anchor_qualification)", model.ErrCreatorNotConfigured},
		{"RiskControlRPC", s.RiskControl != nil, "PrepareLive(risk_check)", model.ErrRiskControlNotConfigured},
		{"ModerationRPC", s.Moderation != nil, "CreateRoom/UpdateRoomInfo 送审", model.ErrModerationNotConfigured},
	}
	notes := make([]string, 0, len(entries)+2)
	for _, e := range entries {
		if e.built {
			notes = append(notes, fmt.Sprintf("%s 已接入（供 %s 使用）", e.name, e.apis))
			continue
		}
		notes = append(notes, fmt.Sprintf("%s 未配置：%s 将以 %v 显式失败，绝不绕过 RPC 直连下游库表",
			e.name, e.apis, e.missing))
	}
	if s.Cache == nil {
		notes = append(notes, "CacheRedis 未配置：房间详情/分区树/PrepareLive 结果全部回源 MySQL")
	}
	if s.Config.LiveRoom.BanExpirySweepEnabled {
		notes = append(notes, "BanExpirySweepEnabled=true 但本轮没有执行者（logic 未实现），到期禁播不会被自动解除")
	}
	return notes
}

// PageSize 把请求的每页大小收敛到本服务允许的范围。
// 语义：<=0 用默认值；> 上限直接拒绝，不静默截断——
// 截断会让调用方以为「还有下一页」而写出错误的分页循环。
func (s *ServiceContext) PageSize(requested int32) (int, error) {
	return s.pageSize(requested, s.Config.LiveRoom.MaxListPageSize,
		s.Config.LiveRoom.DefaultListPageSize, fallbackMaxListPageSize, fallbackDefaultListPageSize)
}

// AreaPageSize 是 ListAreas 专用上限：分区是运营维护的小表，允许比房间列表更大，
// 但仍必须有硬上限，否则一次全表拉取会打到所有下游列表页。
func (s *ServiceContext) AreaPageSize(requested int32) (int, error) {
	return s.pageSize(requested, s.Config.LiveRoom.MaxAreaPageSize,
		s.Config.LiveRoom.DefaultListPageSize, fallbackMaxAreaPageSize, fallbackDefaultListPageSize)
}

// pageSize 共用一分页收敛逻辑；max/default 配成非正数时退到本包兜底常量，
// 因为「无上限」和「无默认值」都不是可接受的运行时行为（列表查询必须带 LIMIT）。
func (s *ServiceContext) pageSize(requested, maxAllowed, def, fallbackMax, fallbackDefault int32) (int, error) {
	if maxAllowed <= 0 {
		maxAllowed = fallbackMax
	}
	if def <= 0 {
		def = fallbackDefault
	}
	if def > maxAllowed {
		def = maxAllowed
	}
	if requested <= 0 {
		return int(def), nil
	}
	if requested > maxAllowed {
		return 0, fmt.Errorf("%w: page_size=%d, max=%d", model.ErrPageSizeTooLarge, requested, maxAllowed)
	}
	return int(requested), nil
}

// 分页兜底常量：仅当配置被显式写成 0/负数时生效，与 etc yaml 的 default 取值一致。
const (
	fallbackDefaultListPageSize int32 = 20
	fallbackMaxListPageSize     int32 = 100
	fallbackMaxAreaPageSize     int32 = 200
)
