package logic

import (
	"context"
	"fmt"

	"go-video/services/search-query/internal/repository"
	"go-video/services/search-query/internal/svc"
	"go-video/services/search-query/model"
	"go-video/services/search-query/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ReportQueryLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewReportQueryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ReportQueryLogic {
	return &ReportQueryLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 上报字段的长度上限（与 deploy/migrations/search-query/000001_*.sql 的列宽一致）。
const (
	maxQueryIDLen     = 64
	maxOpaqueHashLen  = 64
	maxPlatformLen    = 32
	maxAppVersionLen  = 32
	maxResultStateLen = 16
)

// ReportQuery 上报查询行为（写 query_log 与 search_outbox，同事务）。
//
// 幂等：query_id 唯一，重复上报返回 deduplicated=true 且不会产生新行/新事件。
// 隐私：只接受脱敏后的标识（ip_hash / device_id_hash），本层拒绝可能塞进原始
// IP、User-Agent、明文设备号的取值；关键词按查询链路同一套规则规范化后入库，
// 保证热词聚合与缓存 key 能对齐（AGENTS.md §7：仅用于搜索分析与推荐）。
func (l *ReportQueryLogic) ReportQuery(in *rpc.ReportQueryReq) (*rpc.ReportQueryReply, error) {
	if in == nil {
		return nil, model.ErrInvalidQueryID
	}
	repo := l.svcCtx.Repository
	cfg := repo.Conf()

	queryID, err := validateOpaqueID(in.QueryId, maxQueryIDLen, "query_id")
	if err != nil {
		return nil, err
	}
	if queryID == "" {
		return nil, fmt.Errorf("%w: query_id is required", model.ErrInvalidQueryID)
	}
	keyword, err := repository.ValidateKeyword(in.Keyword, cfg.KeywordMaxLen)
	if err != nil {
		return nil, err
	}
	if in.Mid < 0 {
		return nil, model.ErrInvalidMid
	}
	if in.HitCount < 0 || in.LatencyMs < 0 {
		return nil, fmt.Errorf("%w: hit_count=%d latency_ms=%d must not be negative",
			model.ErrInvalidPage, in.HitCount, in.LatencyMs)
	}
	if !isSupportedPlatform(in.Platform) {
		return nil, model.ErrInvalidPlatform
	}
	platform := truncate(in.Platform, maxPlatformLen)
	appVersion, err := validateOpaqueID(in.AppVersion, maxAppVersionLen, "app_version")
	if err != nil {
		return nil, err
	}
	ipHash, err := validateOpaqueID(in.IpHash, maxOpaqueHashLen, "ip_hash")
	if err != nil {
		return nil, err
	}
	deviceIDHash, err := validateOpaqueID(in.DeviceIdHash, maxOpaqueHashLen, "device_id_hash")
	if err != nil {
		return nil, err
	}
	resultState, err := resultStateOf(in.ResultState)
	if err != nil {
		return nil, err
	}
	if len(resultState) > maxResultStateLen {
		return nil, fmt.Errorf("%w: result_state too long", model.ErrInvalidPage)
	}
	// 搜索类型只用于分析维度校验，不入库（本服务不拥有 doc 语义，见 README）。
	if _, err := docTypesOf(in.SearchType); err != nil {
		return nil, err
	}

	out, err := repo.ReportQuery(l.ctx, &model.SearchQueryLog{
		QueryId:     queryID,
		Mid:         in.Mid,
		Keyword:     keyword,
		KeywordHash: model.KeywordHash(keyword),
		HitCount:    in.HitCount,
		ResultState: resultState,
		LatencyMs:   in.LatencyMs,
		Platform:    platform,
		AppVersion:  appVersion,
		IpHash:      ipHash,
	}, deviceIDHash, in.TraceId)
	if err != nil {
		l.Errorw("report query failed",
			logx.Field("err", err),
			logx.Field("query_id", queryID),
			logx.Field("keyword_hash", model.KeywordHash(keyword)),
			logx.Field("trace_id", in.TraceId))
		return nil, err
	}
	if out.Deduplicated {
		l.Infow("duplicate query report ignored", logx.Field("query_id", queryID),
			logx.Field("event_id", out.EventID))
	}
	return &rpc.ReportQueryReply{
		Accepted:     out.Accepted,
		Deduplicated: out.Deduplicated,
		EventId:      out.EventID,
	}, nil
}

// validateOpaqueID 校验“不透明标识”类字段（query_id / 哈希 / 版本号）：
// 非空可选（空串表示未采集），去除首尾空白后不得含空白或控制字符，且长度受限。
//
// 目的：防止调用方把原始 IP（含“:”端口的分片尚可，但含空格的 UA 串不行）、
// User-Agent 或明文设备号塞进这些列 —— 表里只应出现脱敏后的定长标识。
// 需要更强约束（例如强制 sha256 hex）时应在网关侧统一，避免本服务猜格式。
func validateOpaqueID(s string, maxLen int, field string) (string, error) {
	cleaned := repository.StripControl(s)
	if cleaned != s {
		return "", fmt.Errorf("%w: %s contains control characters", model.ErrInvalidPage, field)
	}
	if s == "" {
		return "", nil
	}
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return "", fmt.Errorf("%w: %s must not contain whitespace", model.ErrInvalidPage, field)
		}
	}
	if len([]rune(s)) > maxLen {
		return "", fmt.Errorf("%w: %s length exceeds %d", model.ErrInvalidPage, field, maxLen)
	}
	return s, nil
}
