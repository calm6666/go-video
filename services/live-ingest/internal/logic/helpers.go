// 本文件是 logic 包的手写扩展（入参校验、密钥材料与摘要、幂等去重、分页夹取、
// 协议与列宽归一），不是 goctl 生成产物。
//
// 分工（AGENTS.md §4/§5）：这里只放「不碰 SQL」的可测函数——校验、归一、判定、
// 摘要与引用构造。SQL、CAS 与事务边界一律留在 model。
//
// 密钥纪律（README「不保存长期明文推流密钥」）：明文只作为参数在签发/轮转的调用栈里
// 存活，本文件为它提供的每个函数都只产出「摘要 / 引用 / 末位辨认串」，
// 任何函数都不返回哈希以外可用于重建明文的材料，也绝不写日志。
package logic

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"go-video/common/idgen"
	"go-video/services/live-ingest/internal/svc"
	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"
)

// 列宽与长度约束（与 deploy/migrations/live-ingest/*.sql 逐字对齐）。
// 校验发生在 logic：model 只保证「不合法就别写」，logic 保证「别把不合法的请求带进事务」。
const (
	// maxIdempotencyKeyBytes 对应 request_id / report_id / nonce 的 VARCHAR(64)。
	maxIdempotencyKeyBytes = 64
	// maxTraceIDBytes 对应各表 trace_id VARCHAR(64)。
	maxTraceIDBytes = 64
	// maxStreamRefBytes 对应 stream_id / node_id VARCHAR(64)。
	maxStreamRefBytes = 64
	// maxStreamNameBytes 对应 stream_name VARCHAR(128)。
	maxStreamNameBytes = 128
	// maxReasonRunes 对应 reason / stop_detail VARCHAR(255)：
	// 留余量给多字节边界，超长直接拒绝而不是截断（审计必须与库值一致）。
	maxReasonRunes = 250
	// keyTailChars 是明文末 4 位辨认串的长度（key_tail VARCHAR(8)）。
	keyTailChars = 4
	// keyTailMaxBytes 是辨认串的字节上限：列宽 8 个字符，Base64URL 恒为 ASCII，
	// 但入参来源不受控，因此按字节截断而不是让整条写入失败。
	keyTailMaxBytes = 8
	// defaultListPageSize 是调用方未给 ps 时的每页条数（仍受 MaxListPageSize 夹取）。
	defaultListPageSize = 20
	// fallbackMaxPageSize 是配置缺失（<=0）时的每页上限，绝不能退化成「不限量」。
	fallbackMaxPageSize = 50
	// minKeyRandomBytes 是明文密钥的最小随机字节数（128 位强度）。
	// 低于它就拒签，而不是发一个更短的密钥（AGENTS.md §6）。
	minKeyRandomBytes = 16
	// publishURLApp 是 RTMP/WebRTC 推流地址里的固定应用名段。
	publishURLApp = "live"
	// defaultCallbackSkewSeconds 是配置缺失时允许的时钟偏差（5 分钟）。
	// 有偏差不等于拒绝，但超前服务端太多的时间戳一定有问题。
	defaultCallbackSkewSeconds = 300
	// floorReportedSeconds 是可信时间戳下限（2020-01-01）：早于它的 occurred_at
	// 一律按垃圾输入拒绝，而不是把状态时间轴拉到几十年前。
	floorReportedSeconds = 1577836800
	// maxRoomIDsPerQuery 是单次列表查询允许的房间数上限，
	// 超过就拒绝而不是静默截断（截断会让调用方以为「其余房间没有流」）。
	maxRoomIDsPerQuery = 50
)

// 健康指标的物理上限：超过这些值的上报一定是节点侧统计错误或伪造，
// 直接拒绝并回 ErrInvalidSampleMetrics，而不是让它污染聚合结果。
const (
	maxVideoBitrateBps      int64 = 200_000_000 // 200 Mbps
	maxAudioBitrateBps      int64 = 2_000_000   // 2 Mbps
	maxFpsX100              int32 = 24_000      // 240 fps
	maxPacketLossPpm        int32 = 1_000_000   // 100%
	maxRttMs                int64 = 60_000
	maxSampleWindowSeconds  int32 = 3_600
	maxStreamIDListForRetry int32 = 500
	// defaultHealthWindowSeconds 是配置缺失时聚合窗口的兜底值（10 秒）。
	defaultHealthWindowSeconds = 10
	// fallbackCountHardLimit 是配置缺失时的断流 total 统计上限。
	fallbackCountHardLimit = 10000
	// healthScoreMax 是节点健康分上限（列语义 0~100）。
	healthScoreMax = 100
	// healthInterruptConfirmSamples 是触发 INTERRUPTED 所需的连续 CRITICAL 采样数
	// （含本次）：单次抖动不停流，避免 OBS 卡一帧就把主播踢下线。
	healthInterruptConfirmSamples = 3
)

// 幂等键与接入鉴权的拒绝原因码（rpc 注释里给出的稳定口径）。
const (
	reasonKeyNotFound    = "key_not_found"
	reasonKeyRevoked     = "key_revoked"
	reasonKeyExpired     = "key_expired"
	reasonProtocolDenied = "protocol_denied"
	reasonQuotaExceeded  = "quota_exceeded"
	reasonRoomMismatch   = "room_mismatch"
	reasonMalformed      = "malformed_request"

	reasonSignatureMismatch = "signature_mismatch"
	reasonTimestampSkew     = "timestamp_skew"
	reasonReplayedNonce     = "replayed_nonce"
	reasonDomainUnbound     = "domain_unbound"
	reasonStreamNotFound    = "stream_not_found"
	reasonSignerUnavailable = "signer_unavailable"
)

func nowUnix() int64 { return time.Now().Unix() }

// errNoRepository 用于「ServiceContext 没装配 Repository」。生产路径不会发生
// （svc 一定注入），它挡住的是测试里手搓 ServiceContext 时误以为「查询返回空即成功」。
var errNoRepository = errors.New("live-ingest: repository not wired")

// 密钥代次与级联上限。
const (
	// keyVersionFirst 是首发密钥的 version（每轮转一次 +1）。
	keyVersionFirst = 1
	// keyCascadeMax 是单次吊销级联停流的流数上限，防止历史脏数据把事务拖长。
	keyCascadeMax = 50
)

// issueTtlSeconds 归一密钥有效期：<=0 取配置默认，超上限夹到上限（README「服务端夹取」）。
// 两侧都不可用时（默认值也是 0）返回 ErrInvalidTtl，而不是签发一个立刻过期的密钥。
func issueTtlSeconds(defaultTTL, maxTTL, requested int64) (int64, error) {
	ttl := requested
	if ttl <= 0 {
		ttl = defaultTTL
	}
	if ttl <= 0 {
		return 0, model.ErrInvalidTtl
	}
	if maxTTL > 0 && ttl > maxTTL {
		ttl = maxTTL
	}
	return ttl, nil
}

// positiveOrDefault 取「显式值优先、否则配置默认」；两者都非正时返回 0 交给调用方判定。
func positiveOrDefault(configured, requested int64) int64 {
	if requested > 0 {
		return requested
	}
	if configured > 0 {
		return configured
	}
	return 0
}

// maxStreamsPerKey 归一单密钥并发流上限：<=0 取配置默认，再夹到硬上限。
func maxStreamsPerKey(configured, requested int32) int32 {
	v := requested
	if v <= 0 {
		v = configured
	}
	if v <= 0 {
		return 1
	}
	if v > keyCascadeMax {
		return keyCascadeMax
	}
	return v
}

// newStreamID 生成推流会话 ID（ULID）。失败必须上抛：
// 用时间戳或随机短码兜底会造出可能与既有 stream_id 相撞的主键。
func newStreamID() (string, error) {
	id, err := idgen.ULID()
	if err != nil {
		return "", fmt.Errorf("live-ingest: generate stream id: %w", err)
	}
	return id, nil
}

// newEventID 生成 live.state.v1 的 event_id（与 live_stream_event.event_id 同源）。
func newEventID() (string, error) {
	id, err := idgen.ULID()
	if err != nil {
		return "", fmt.Errorf("live-ingest: generate event id: %w", err)
	}
	return id, nil
}

// reportedAt 归一外部上报的时间戳：未提供用服务端时间；超前超过 skew 的判为伪造/时钟漂移。
//
// 「上报时间比上次迁移还早」这一条要拿库里的 state_changed_at 比，无法在本函数完成，
// 由各 logic 在事务前做预检（真正防乱序的是 ApplyTransition 的 state+seq 双条件 CAS）。
func reportedAt(reported, serverNow, skewSeconds int64) (int64, error) {
	if reported <= 0 {
		return serverNow, nil
	}
	skew := skewSeconds
	if skew <= 0 {
		skew = defaultCallbackSkewSeconds
	}
	if reported > serverNow+skew {
		return 0, fmt.Errorf("%w: reported time ahead of server clock", model.ErrCallbackTimestampSkew)
	}
	if reported < floorReportedSeconds {
		return 0, fmt.Errorf("%w: reported time out of range", model.ErrCallbackTimestampSkew)
	}
	return reported, nil
}

// --- 通用入参校验 ---

func checkRoomID(id int64) error {
	if id <= 0 {
		return model.ErrInvalidRoomId
	}
	return nil
}

func checkMid(id int64) error {
	if id <= 0 {
		return model.ErrInvalidMid
	}
	return nil
}

// checkOperator 校验运营/管理员身份：没有归因主体的写不可受理。
func checkOperator(mid int64) error {
	if mid <= 0 {
		return model.ErrOperatorRequired
	}
	return nil
}

// checkRequestID 校验写接口幂等键。空键一律拒绝：没有幂等键的写重放就是两次副作用。
func checkRequestID(id string) (string, error) {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return "", model.ErrIdempotencyKeyRequired
	}
	if len(trimmed) > maxIdempotencyKeyBytes {
		return "", fmt.Errorf("%w: request_id %d bytes, max %d", model.ErrIdempotencyKeyRequired,
			len(trimmed), maxIdempotencyKeyBytes)
	}
	return trimmed, nil
}

// checkReportID 与 checkRequestID 同语义，只换提示词（report_id 承载状态/健康上报幂等）。
func checkReportID(id string) (string, error) {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return "", model.ErrIdempotencyKeyRequired
	}
	if len(trimmed) > maxIdempotencyKeyBytes {
		return "", fmt.Errorf("%w: report_id %d bytes, max %d", model.ErrIdempotencyKeyRequired,
			len(trimmed), maxIdempotencyKeyBytes)
	}
	return trimmed, nil
}

// checkStreamID 校验流引用（ULID 主键）。
func checkStreamID(id string) (string, error) {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return "", model.ErrInvalidStreamId
	}
	if len(trimmed) > maxStreamRefBytes || strings.ContainsAny(trimmed, " /\t\n") {
		return "", fmt.Errorf("%w: stream_id 不是合法引用", model.ErrInvalidStreamId)
	}
	return trimmed, nil
}

// checkNodeID 校验节点引用。
func checkNodeID(id string) (string, error) {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return "", model.ErrNodeNotFound
	}
	if len(trimmed) > maxStreamRefBytes || strings.ContainsAny(trimmed, " /\t\n") {
		return "", fmt.Errorf("%w: node_id 不是合法引用", model.ErrNodeNotFound)
	}
	return trimmed, nil
}

// normalizeStreamName 归一流标识：空串或超长、含空白/斜杠的一律判非法。
// 返回 ok=false 时调用方按「拒绝但不泄露」处理（接入鉴权与回调归属都靠它）。
func normalizeStreamName(name string) (string, bool) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" || len(trimmed) > maxStreamNameBytes || strings.ContainsAny(trimmed, " /\t\n?&=") {
		return "", false
	}
	return trimmed, true
}

// checkReason 校验审计/事件文本。required=true 时空串即拒绝。
func checkReason(name, reason string, required bool) (string, error) {
	r := strings.TrimSpace(reason)
	if r == "" {
		if required {
			return "", fmt.Errorf("%w: %s", model.ErrReasonRequired, name)
		}
		return "", nil
	}
	if runeLen(r) > maxReasonRunes {
		return "", fmt.Errorf("%w: %s %d > %d", model.ErrReasonTooLong, name, runeLen(r), maxReasonRunes)
	}
	return r, nil
}

// sanitizeTraceID 裁剪 trace_id 到列宽：trace 是关联句柄而非业务事实，
// 截断尾巴不影响正确性，而因它让整次写入失败反而丢状态。
func sanitizeTraceID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) > maxTraceIDBytes {
		return id[:maxTraceIDBytes]
	}
	return id
}

func runeLen(s string) int { return len([]rune(s)) }

// --- 分页与上限夹取 ---

// maxListPageSize 取配置的每页上限；缺失（<=0）时用兜底值而不是「不限量」。
func maxListPageSize(configured int32) int32 {
	if configured <= 0 {
		return fallbackMaxPageSize
	}
	return configured
}

// clampPageSize 归一每页条数：非正取默认，超上限夹到上限（README「服务端夹取」口径）。
func clampPageSize(ps, max int32) int32 {
	max = maxListPageSize(max)
	if ps <= 0 {
		return defaultListPageSize
	}
	if ps > max {
		return max
	}
	return ps
}

// clampPageNo 归一页码：非正视为第一页（客户端省略分页参数的常见形态）。
func clampPageNo(pn int32) int32 {
	if pn <= 1 {
		return 1
	}
	return pn
}

// pageOffset 把「页码 + 页大小」换成 SQL OFFSET。
func pageOffset(pn, ps int32) int32 {
	if pn <= 1 || ps <= 0 {
		return 0
	}
	return (pn - 1) * ps
}

// clampLimit 归一「limit」型查询上限（事件、断流、批量重试）。
func clampLimit(limit, max int32) int32 {
	max = maxListPageSize(max)
	if limit <= 0 {
		return defaultListPageSize
	}
	if limit > max {
		return max
	}
	return limit
}

// clampInt32 把任意值夹到 [lo, hi]（健康分、采样条数等）。
func clampInt32(v, lo, hi int32) int32 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// toInt32 把 int64 饱和收成 int32（rpc 里少数计数字段是 int32）。
// 饱和而不是强转：回绕后的负数丢包秒数会被下游当成数据异常。
func toInt32(v int64) int32 {
	const maxI32 = int64(^uint32(0) >> 1)
	if v > maxI32 {
		return int32(maxI32)
	}
	if v < -maxI32 {
		return -int32(maxI32)
	}
	return int32(v)
}

// clampInt64 把任意 int64 夹到 [lo, hi]（TTL、宽限秒数等）。
func clampInt64(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// toInt32Total 把 COUNT 的 int64 收成 int32（与 rpc 的 total 字段一致），饱和而不回绕。
func toInt32Total(v int64) int32 {
	if v > int64(^uint32(0)>>1) {
		return int32(^uint32(0) >> 1)
	}
	if v < -int64(^uint32(0)>>1) {
		return -int32(^uint32(0) >> 1)
	}
	return int32(v)
}

// --- 协议位图 <-> 枚举（model.ProtocolMask/ProtocolEnum 是唯一转换入口）---

// protocolsToMask 把请求里的协议枚举去重后转成 protocol_mask 位图。
// 未知枚举一律拒绝，绝不按 RTMP 兜底（AGENTS.md §9：不静默放宽）。
func protocolsToMask(protocols []rpc.IngestProtocol) (uint32, error) {
	var mask uint32
	for _, p := range protocols {
		m, ok := model.ProtocolMask(int32(p))
		if !ok {
			return 0, fmt.Errorf("%w: protocol=%d", model.ErrInvalidProtocol, int32(p))
		}
		mask |= m
	}
	return mask, nil
}

// protocolMaskOf 归一签发请求的协议集合：缺省按 RTMP（proto 注释口径）。
func protocolMaskOf(protocols []rpc.IngestProtocol) (uint32, error) {
	if len(protocols) == 0 {
		return model.ProtocolMaskRtmp, nil
	}
	mask, err := protocolsToMask(protocols)
	if err != nil {
		return 0, err
	}
	if mask == 0 {
		return 0, model.ErrInvalidProtocol
	}
	return mask, nil
}

// singleProtocolMask 把单个协议枚举转成位（节点分配匹配用）。
func singleProtocolMask(protocol rpc.IngestProtocol) (uint32, error) {
	mask, ok := model.ProtocolMask(int32(protocol))
	if !ok {
		return 0, fmt.Errorf("%w: protocol=%d", model.ErrInvalidProtocol, int32(protocol))
	}
	return mask, nil
}

// --- 密钥材料与摘要 ---

// newPlaintextKey 用配置的随机字节数生成明文推流密钥（Base64URL，无填充）。
// 随机源失败时返回错误：调用方必须拒签，绝不能退化成固定串、时间戳或其他可猜材料。
// 返回值只允许被拼进「本次响应的 plaintext_key / publish_url」，不得入库、不得写日志。
func newPlaintextKey(randomBytes int) (string, error) {
	if randomBytes < minKeyRandomBytes {
		return "", fmt.Errorf("%w: key random bytes below %d", model.ErrKeyRefUnresolvable, minKeyRandomBytes)
	}
	secret, err := idgen.Short(randomBytes)
	if err != nil {
		return "", fmt.Errorf("live-ingest: 生成密钥材料失败: %w", err)
	}
	if len(secret) < minKeyRandomBytes {
		return "", fmt.Errorf("%w: 密钥材料长度异常", model.ErrKeyRefUnresolvable)
	}
	return secret, nil
}

// sha256Hex 返回输入的 SHA-256 hex（64 字符），用于 key_hash、签名摘要、IP 摘要。
// 单向：拿到返回值无法还原输入，因此可以安全入库与回显摘要本身。
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// hmacHex 计算 HMAC-SHA256 的 hex。key 只在本调用栈内存在，绝不入日志。
func hmacHex(key []byte, canonical string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(canonical))
	return hex.EncodeToString(mac.Sum(nil))
}

// constantTimeEqualHex 常数时间比较两个 hex 摘要；大小写不敏感（厂商回签名常见大写）。
func constantTimeEqualHex(a, b string) bool {
	na, nb := strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	if na == "" || nb == "" {
		return false
	}
	return hmac.Equal([]byte(na), []byte(nb))
}

// keyTailOf 取明文末 4 位辨认串（仅供主播在多个密钥之间辨认，绝不参与鉴权）。
// 它本身就是明文的一小段，因此同样不写日志；长度与字符集都按列宽收紧。
func keyTailOf(plaintext string) string {
	if len(plaintext) <= keyTailChars {
		return truncateBytes(plaintext, keyTailMaxBytes)
	}
	return truncateBytes(plaintext[len(plaintext)-keyTailChars:], keyTailMaxBytes)
}

func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// deriveStreamName 生成流标识：room_id + 随机后缀。
// 流标识不是密钥（proto 注释「可下发客户端」），但它参与构造推流地址，必须不可猜。
func deriveStreamName(roomID int64) (string, error) {
	suffix, err := idgen.Short(6)
	if err != nil {
		return "", fmt.Errorf("live-ingest: 生成流标识失败: %w", err)
	}
	name := fmt.Sprintf("live_%d_%s", roomID, strings.TrimRight(suffix, "="))
	if len(name) > maxStreamNameBytes {
		return "", fmt.Errorf("%w: stream_name 超长", model.ErrKeyRefUnresolvable)
	}
	return name, nil
}

// keyRefFor 构造密钥的 Secret/Vault 引用（明文永不入库，本服务不写也不读该引用指向的内容）。
// 引用本身不是秘密，可回显与留档；要素缺失时拒签而不是留空——
// 空引用等于「没人知道这把密钥归属哪次签发」，审计链就断了。
func keyRefFor(streamName string, version int32) (string, error) {
	name, ok := normalizeStreamName(streamName)
	if !ok || version <= 0 {
		return "", model.ErrKeyRefUnresolvable
	}
	ref := fmt.Sprintf("vault:secret/data/live-ingest/stream-key/%s#v%d", name, version)
	if len(ref) > 191 { // live_stream_key.key_ref VARCHAR(191)
		return "", fmt.Errorf("%w: 引用超出列宽", model.ErrKeyRefUnresolvable)
	}
	return ref, nil
}

// publishDomain 取首个允许的推流域名。白名单为空时返回显式错误：
// 没有入口域名就签发一个「只有密钥没有地址」的响应，只会让主播按错的地址推流。
func publishDomain(svcCtx *svc.ServiceContext) (string, error) {
	for _, d := range svcCtx.Config.Cdn.PublishDomains {
		if trimmed := strings.TrimSpace(d); trimmed != "" {
			return trimmed, nil
		}
	}
	return "", model.ErrCdnNotConfigured
}

// buildPublishURL 按主协议拼装推流地址。返回值含明文密钥，与 plaintext_key 同级：
// 只出现在签发/轮转那一次响应里，不得入库、不得写日志。
func buildPublishURL(domain, streamName, plaintext string, mask uint32) string {
	switch {
	case mask&model.ProtocolMaskRtmp != 0:
		return fmt.Sprintf("rtmp://%s/%s/%s?s=%s", domain, publishURLApp, streamName, plaintext)
	case mask&model.ProtocolMaskSrt != 0:
		return fmt.Sprintf("srt://%s?streamid=%s/%s", domain, streamName, plaintext)
	case mask&model.ProtocolMaskWebrtc != 0:
		return fmt.Sprintf("https://%s/%s/%s?s=%s", domain, publishURLApp, streamName, plaintext)
	default:
		return ""
	}
}

// resolveCallbackSecret 把 Cdn.CallbackSecretRef 指向的凭据解析成 HMAC 密钥。
//
// 支持的引用形态（与 notification/operation 的「配置只放引用名」口径一致）：
//   - env:NAME / ${NAME}：取环境变量；
//   - 其他形态（例如 vault:）需要 Vault 客户端，本仓库尚未接线，返回显式错误。
//
// 绝不返回空密钥：拿不到凭据就判「无法验证」，调用方必须按未通过处理（fail closed）。
func resolveCallbackSecret(ref string) ([]byte, error) {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" {
		return nil, model.ErrCallbackSignerUnavailable
	}
	name := ""
	switch {
	case strings.HasPrefix(trimmed, "env:"):
		name = strings.TrimSpace(strings.TrimPrefix(trimmed, "env:"))
	case strings.HasPrefix(trimmed, "${") && strings.HasSuffix(trimmed, "}"):
		name = strings.TrimSpace(trimmed[2 : len(trimmed)-1])
	default:
		// vault:/file:/kms: 等形态需要外部密钥服务，本仓库无客户端：不猜、不降级。
		return nil, model.ErrCallbackSignerUnavailable
	}
	if !envNameRe.MatchString(name) {
		return nil, fmt.Errorf("%w: 引用形态不受支持", model.ErrCallbackSignerUnavailable)
	}
	value, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(value) == "" {
		return nil, fmt.Errorf("%w: 环境变量未注入", model.ErrCallbackSignerUnavailable)
	}
	return []byte(value), nil
}

// envNameRe 是环境变量名的保守形态（只允许大写字母/数字/下划线，不以数字开头）。
var envNameRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// cdnCanonicalString 构造回调签名的待签串。
// 字段顺序与分隔符是本服务与厂商侧的固定口径，任何一侧改动都要递增 schema，
// 否则会出现「签名永远不匹配」的静默故障。
func cdnCanonicalString(domain, streamName, eventType, nonce string, timestamp int64) string {
	return strings.Join([]string{
		strings.ToLower(strings.TrimSpace(domain)),
		strings.TrimSpace(streamName),
		strings.ToLower(strings.TrimSpace(eventType)),
		strings.TrimSpace(nonce),
		fmt.Sprintf("%d", timestamp),
	}, "\n")
}

// 幂等承载点全部在数据库（唯一索引或不可逆终态），见 README「幂等承载点」。
// 这里刻意不放 Redis 兜底：Redis 里的重放标记会过期，而「标记过期后把重放当新请求
// 再执行一次副作用」正是密钥/配额类写接口最不能接受的失败模式。
