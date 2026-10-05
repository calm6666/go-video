// 本文件是 logic 包的手写扩展（入参校验、引用脱敏、状态机裁决、Outbox 追加、缓存键与投影），
// 不是 goctl 生成产物。
//
// 分工（AGENTS.md §4/§5）：
//   - 这里只放「不碰 SQL」的可测函数：校验、归一、幂等判定、状态机裁决、事件信封构造、投影映射；
//   - SQL、条件 UPDATE（CAS）与事务边界一律留在 model，logic 只负责决定「往哪迁移、失败怎么归因」；
//   - 任何可能含凭据的文本（拉流地址、对象 key、错误摘要）在离开口令之前先脱敏，
//     密钥/签名地址既不入库也不写日志（AGENTS.md §6）。

package logic

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"go-video/common/eventenvelope"
	"go-video/services/live-media/internal/config"
	"go-video/services/live-media/internal/svc"
	"go-video/services/live-media/model"
	"go-video/services/live-media/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 列宽约束（与 deploy/migrations/live-media/000001..000003 逐列一致。
// 这条一致性目前是人工比对（服务 README §7 记了逐表列数），本服务还没有
// 其它服务那种 model/migration_parity_test.go 自动门禁；校验发生在 logic：
// model 只保证「不合法就别写」，logic 保证「别把不合法的请求带进事务」）。
const (
	maxRequestIDRunes = 64  // request_id VARCHAR(64)（唯一索引，幂等键）
	maxEventIDRunes   = 64  // live_replay_asset_ref.last_event_id VARCHAR(64)
	maxTraceIDRunes   = 64  // trace_id VARCHAR(64)
	maxErrMsgRunes    = 512 // err_msg VARCHAR(512)
	maxBucketRunes    = 64  // bucket VARCHAR(64)
	maxObjectKeyRunes = 512 // object_key VARCHAR(512)
	maxPrefixRunes    = 255 // output_prefix VARCHAR(255)
	maxCdnRunes       = 191 // cdn_domain VARCHAR(191)
	maxSourceRefRunes = 512 // source_ref VARCHAR(512)
	maxChecksumRunes  = 64  // checksum CHAR(64)（sha256 hex）
	maxWorkerRunes    = 64  // worker_id VARCHAR(64)
	maxOperatorRunes  = 64  // operator VARCHAR(64)
	maxReasonRunes    = 255 // retention reason VARCHAR(255)
	maxTitleRunes     = 255 // replay title VARCHAR(255)
	maxDescRunes      = 2048
	maxBvidRunes      = 32   // bvid VARCHAR(32)
	maxRefRunes       = 4096 // 通用文本上限兜底（超长一律拒绝或截断，绝不静默截断事实列）

	// maxGapFillRows 单次补洞上限：seq 跳跃超过这个量级说明 Worker 传错了 record_id 或序号，
	// 继续补洞会一次性写入海量 MISSING 行，宁可拒绝并让它重投。
	maxGapFillRows = 1000
)

// 缓存键命名空间：本服务自有前缀，不与其他服务共用 key 空间（AGENTS.md §5）。
const (
	taskCachePrefix    = "govideo:livemedia:task:"
	outputListPrefix   = "govideo:livemedia:outputs:"
	outputGenSuffix    = ":gen"
	outputGenTTLSecond = 3600 // 代际键 TTL 必须远大于列表 TTL，否则代际回零会命中旧值
)

// taskCacheKey 的 kind 维度：三类任务共用一个前缀，靠 kind 区分主键空间
// （task_id / record_id / replay_id 都是各自的 AUTO_INCREMENT，数值会重叠）。
const (
	taskKindTranscode = "transcode"
	taskKindRecord    = "record"
	taskKindReplay    = "replay"
)

// secretMarkers 是「值必须脱敏」的键名片段：签名地址、临时凭据与口令都不允许进
// err_msg / 事件 payload / 日志（AGENTS.md §6）。匹配大小写不敏感。
var secretMarkers = []string{
	"x-amz-signature", "x-amz-credential", "x-amz-security-token", "x-amz-date",
	"x-oss-signature", "x-oss-credential", "x-oss-security-token",
	"awsaccesskeyid", "accesskeyid", "access_key_id", "access_key_secret",
	"signature", "security-token", "token", "secret", "password", "pwd",
	"authorization",
}

// --- 通用入参校验 ---

func checkRoomID(id int64) error {
	if id <= 0 {
		return model.ErrInvalidRoomID
	}
	return nil
}

func checkSessionID(id int64) error {
	if id <= 0 {
		return model.ErrInvalidSessionID
	}
	return nil
}

func checkTemplateID(id int64) error {
	if id <= 0 {
		return model.ErrInvalidTemplateID
	}
	return nil
}

func checkAssetID(id int64) error {
	if id <= 0 {
		return model.ErrInvalidAssetID
	}
	return nil
}

func checkAid(id int64) error {
	if id <= 0 {
		return model.ErrInvalidAid
	}
	return nil
}

func checkPositive(name string, id int64, notFound error) error {
	if id <= 0 {
		return fmt.Errorf("live-media: %s must be positive: %w", name, notFound)
	}
	return nil
}

// checkRequestID 校验写接口幂等键。空键一律拒绝：没有幂等键的写重放就是两次副作用。
// 长度超列宽同样拒绝（不截断）：截断后的键会与原始键不同义，重放时对不上。
func checkRequestID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return model.ErrEmptyRequestID
	}
	if runeLen(id) > maxRequestIDRunes {
		return fmt.Errorf("%w: request_id %d runes, max %d", model.ErrEmptyRequestID, runeLen(id), maxRequestIDRunes)
	}
	return nil
}

// checkEventID 校验驱动投影的事件 ID（幂等依据）。
func checkEventID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return model.ErrEmptyEventID
	}
	if runeLen(id) > maxEventIDRunes {
		return fmt.Errorf("%w: event_id %d runes, max %d", model.ErrEmptyEventID, runeLen(id), maxEventIDRunes)
	}
	return nil
}

// sanitizeTraceID 裁剪 trace_id 到列宽：trace 是关联句柄而非业务事实，
// 截断尾巴不影响正确性，因它让整个写入失败反而丢状态。
func sanitizeTraceID(id string) string {
	id = strings.TrimSpace(id)
	if runeLen(id) > maxTraceIDRunes {
		return truncateRunes(id, maxTraceIDRunes)
	}
	return id
}

func sanitizeWorkerID(id string) string {
	id = strings.TrimSpace(id)
	if runeLen(id) > maxWorkerRunes {
		return truncateRunes(id, maxWorkerRunes)
	}
	return id
}

// sanitizeOperator 校验人工写入口的操作人。
// 缺操作人与超列宽是两种失败：前者返回哨兵 ErrOperatorRequired 供调用方分支，
// 后者只拒绝不截断（截断后的名字可能是另一个人，审计归因一旦被伪造就找不回真实操作者）。
func sanitizeOperator(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", fmt.Errorf("live-media: operator required for audit attribution: %w", model.ErrOperatorRequired)
	}
	if runeLen(id) > maxOperatorRunes {
		return "", fmt.Errorf("live-media: operator too long: %d > %d", runeLen(id), maxOperatorRunes)
	}
	return id, nil
}

// optionalOperator 用于「operator 可选但必须留证」的入口（停止/取消）：截断到列宽并脱敏。
func optionalOperator(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	return truncateRunes(redactSecrets(id), maxOperatorRunes)
}

func checkAuditText(name, text string, required bool, maxRunes int) (string, error) {
	t := strings.TrimSpace(text)
	t = redactSecrets(t)
	if t == "" {
		if required {
			return "", fmt.Errorf("live-media: %s is required: %w", name, model.ErrRetentionReasonRequired)
		}
		return "", nil
	}
	if runeLen(t) > maxRunes {
		return "", fmt.Errorf("live-media: %s too long: %d > %d", name, runeLen(t), maxRunes)
	}
	return t, nil
}

// checkBoundedRunes 通用「非空 + 列宽」校验，用于不允许截断的引用列。
func checkBoundedRunes(name, v string, maxRunes int) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", fmt.Errorf("live-media: %s required: %w", name, model.ErrInvalidBucketRef)
	}
	if runeLen(v) > maxRunes {
		return "", fmt.Errorf("live-media: %s too long: %d > %d", name, runeLen(v), maxRunes)
	}
	return v, nil
}

// checkObjectRef 校验「桶 + 相对 key」引用：只存引用，签名地址与凭据一律拒绝。
// 允许 object_key 为空串（缺口切片/仅登记桶的场景由调用方决定），但给了就必须是相对路径。
func checkObjectRef(bucket, objectKey string, keyRequired bool, keyMax int) (string, string, error) {
	b, err := checkBucketRef(bucket, keyRequired)
	if err != nil {
		return "", "", err
	}
	k := strings.TrimSpace(objectKey)
	if k == "" {
		if keyRequired {
			return "", "", fmt.Errorf("live-media: object_key required: %w", model.ErrInvalidBucketRef)
		}
		return b, "", nil
	}
	if runeLen(k) > keyMax {
		return "", "", fmt.Errorf("live-media: object_key too long: %d > %d", runeLen(k), keyMax)
	}
	if strings.Contains(k, "://") || strings.HasPrefix(k, "/") {
		return "", "", fmt.Errorf("live-media: object_key must be a relative path without scheme: %w",
			model.ErrInvalidBucketRef)
	}
	if hasSecretMarker(k) {
		return "", "", fmt.Errorf("live-media: object_key must not carry signature or credentials: %w",
			model.ErrInvalidBucketRef)
	}
	return b, k, nil
}

// checkBucketRef 校验桶名；keyRequired=false（MISSING/CORRUPT 登记）时允许整体留空。
//
// 缺陷 #5（本轮修）：这里曾无条件走 checkBoundedRunes，空桶一律「bucket required」，
// 与三处契约相反 —— proto 的 ReportRecordSegmentReq.bucket 注释「MISSING 时可为空」、
// DDL 列注释「MISSING/CORRUPT 行为空串，表示产物不存在」（000002:89）、
// 以及 reportrecordsegmentlogic.go:99「缺口不带引用时清空 object_key」那段因此恒不成立的分支。
// 后果不是报错难看，而是缺口登记不出去：Worker 探测到时间轴空洞时往往连桶都未知，
// 要么被拒到无限重投（缺口永不落表，回放拼出一条看起来完整实则断档的录像），
// 要么为了过门禁随手编一个 bucket（一旦落库，回收任务会以为那里有可删对象）。
// keyRequired=true 的行为与此前逐字一致，其余五个调用点不受影响。
func checkBucketRef(bucket string, required bool) (string, error) {
	if !required && strings.TrimSpace(bucket) == "" {
		return "", nil
	}
	b, err := checkBoundedRunes("bucket", bucket, maxBucketRunes)
	if err != nil {
		return "", err
	}
	if strings.Contains(strings.ToLower(b), "://") || strings.ContainsAny(b, " \t/?=") {
		return "", fmt.Errorf("live-media: bucket %q must be a bare bucket name: %w", b, model.ErrInvalidBucketRef)
	}
	return b, nil
}

// checkSourceRef 校验拉流源引用：允许 rtmp/hls 等带 scheme 的短期地址，
// 但只要命中签名参数或凭据片段就拒绝（这类值一旦入库就等于长期泄漏）。
func checkSourceRef(ref string) (string, error) {
	v := strings.TrimSpace(ref)
	if v == "" {
		return "", fmt.Errorf("live-media: source_ref required: %w", model.ErrInvalidBucketRef)
	}
	if runeLen(v) > maxSourceRefRunes {
		return "", fmt.Errorf("live-media: source_ref too long: %d > %d", runeLen(v), maxSourceRefRunes)
	}
	if hasSecretMarker(v) || hasUserInfoCredential(v) {
		return "", fmt.Errorf("live-media: source_ref must not contain signed URL or credentials: %w",
			model.ErrInvalidBucketRef)
	}
	return v, nil
}

// checkCdnDomain 校验 CDN 域名：只存域名配置，不存完整播放地址。
func checkCdnDomain(domain string) (string, error) {
	v := strings.TrimSpace(domain)
	if v == "" {
		return "", nil
	}
	if runeLen(v) > maxCdnRunes {
		return "", fmt.Errorf("live-media: cdn_domain too long: %d > %d", runeLen(v), maxCdnRunes)
	}
	if strings.Contains(v, "://") || strings.ContainsAny(v, " \t/?@") || hasSecretMarker(v) {
		return "", fmt.Errorf("live-media: cdn_domain %q must be a bare host: %w", v, model.ErrInvalidBucketRef)
	}
	return v, nil
}

// checkChecksum 校验 sha256 hex：只当校验证据，不当凭据。
func checkChecksum(sum string) (string, error) {
	v := strings.TrimSpace(sum)
	if v == "" {
		return "", nil
	}
	if runeLen(v) > maxChecksumRunes {
		return "", fmt.Errorf("live-media: checksum too long: %d > %d: %w", runeLen(v), maxChecksumRunes,
			model.ErrInvalidSegmentRange)
	}
	return v, nil
}

// normalizedRequestID 归一幂等键：校验与落库用同一个值，
// 否则带空白的键会「校验通过但回读不到」。
func normalizedRequestID(id string) string { return strings.TrimSpace(id) }

// normalizedEventID 归一事件 ID（投影幂等键）。
func normalizedEventID(id string) string { return strings.TrimSpace(id) }

// --- 枚举取值校验（与 rpc 枚举编号一一对应，越界即拒绝） ---

func checkFailureReason(r int32) error {
	if r < model.ReasonUnspecified || r > model.ReasonDataGap {
		return fmt.Errorf("live-media: reason=%d out of rpc.FailureReason range: %w", r, model.ErrInvalidTransition)
	}
	return nil
}

func checkBitrateLevel(lvl int32) error {
	if lvl < int32(rpc.BitrateLevel_BITRATE_LEVEL_SOURCE) || lvl > int32(rpc.BitrateLevel_BITRATE_LEVEL_AUDIO) {
		return fmt.Errorf("live-media: bitrate_level=%d out of range: %w", lvl, model.ErrInvalidTransition)
	}
	return nil
}

func checkProtocol(p int32) error {
	if p < int32(rpc.StreamProtocol_STREAM_PROTOCOL_HLS) || p > int32(rpc.StreamProtocol_STREAM_PROTOCOL_ARTC) {
		return fmt.Errorf("live-media: protocol=%d out of range: %w", p, model.ErrInvalidTransition)
	}
	return nil
}

func checkTranscodeState(s int32) error {
	if s < model.TranscodeStatePending || s > model.TranscodeStateCancelled {
		return fmt.Errorf("live-media: state=%d out of LiveTranscodeState range: %w", s, model.ErrInvalidTransition)
	}
	return nil
}

func checkRecordState(s int32) error {
	if s < model.RecordStatePending || s > model.RecordStateCancelled {
		return fmt.Errorf("live-media: state=%d out of LiveRecordState range: %w", s, model.ErrInvalidTransition)
	}
	return nil
}

func checkSegmentState(s int32) error {
	if s < model.SegmentStateUploading || s > model.SegmentStateCorrupt {
		return fmt.Errorf("live-media: segment state=%d out of SegmentState range: %w", s, model.ErrInvalidTransition)
	}
	return nil
}

func checkReplayState(s int32) error {
	if s < model.ReplayStatePending || s > model.ReplayStateCancelled {
		return fmt.Errorf("live-media: replay state=%d out of ReplayState range: %w", s, model.ErrInvalidTransition)
	}
	return nil
}

func checkReviewState(s int32) error {
	if !model.ValidReviewState(s) {
		return fmt.Errorf("live-media: review_state=%d not a valid projection value (1..5): %w",
			s, model.ErrInvalidReviewState)
	}
	return nil
}

func checkRetentionTarget(kind int32) error {
	if !model.ValidRetentionTarget(kind) {
		return fmt.Errorf("live-media: target_kind=%d out of range: %w", kind, model.ErrInvalidTransition)
	}
	return nil
}

// checkReportedRetentionState 回收结果上报的目标态：PENDING 不是上报目标（那是登记态）。
func checkReportedRetentionState(s int32) error {
	if s < model.RetentionStateRunning || s > model.RetentionStateCancelled {
		return fmt.Errorf("live-media: retention state=%d is not a reportable state: %w",
			s, model.ErrInvalidTransition)
	}
	return nil
}

// --- 归一化（把「未提供」折算成配置快照，之后不随配置变化改写历史行） ---

func positiveInt32(v, fallback int32) int32 {
	if v <= 0 {
		return fallback
	}
	return v
}

func clampInt32(v, min, max int32) int32 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// segmentSeconds 归一分片时长：未提供取默认，超过上限直接拒绝
// （超长分片会让断点续录的粒度失效）。
func segmentSeconds(cfg config.LiveMediaConf, requested int32) (int32, error) {
	v := positiveInt32(requested, cfg.DefaultRecordSegmentSeconds)
	if v <= 0 || v > cfg.MaxRecordSegmentSeconds {
		return 0, fmt.Errorf("live-media: segment_seconds=%d exceeds max %d: %w",
			v, cfg.MaxRecordSegmentSeconds, model.ErrInvalidTransition)
	}
	return v, nil
}

// retentionBatchLimit 夹取回收批量上限（不报错：Worker 每批以此限流，越界夹取即可）。
func retentionBatchLimit(cfg config.LiveMediaConf, requested int32) int32 {
	v := positiveInt32(requested, cfg.DefaultRetentionBatchLimit)
	return clampInt32(v, 1, cfg.MaxRetentionBatchLimit)
}

// maxGapSegments 允许的缺口切片数：allow_gaps=false 时恒为 0（任何洞都拒绝）。
func maxGapSegments(cfg config.LiveMediaConf, allowGaps bool) int64 {
	if !allowGaps {
		return 0
	}
	return int64(clampInt32(cfg.MaxReplayGapSegments, 0, 1<<20))
}

// nextTimeoutAt 心跳超时时点：与 heartbeat 同源时钟（测试用 model.SetClock 注入）。
// timeoutSeconds<=0 表示不判超时，返回 0 让 ListTimedOut 跳过该行。
func nextTimeoutAt(heartbeatAt int64, timeoutSeconds int32) int64 {
	if heartbeatAt <= 0 || timeoutSeconds <= 0 {
		return 0
	}
	return heartbeatAt + int64(timeoutSeconds)
}

// timeoutBudget 取回「无心跳超时秒数」的登记快照。
// 表里没有独立秒数列，但 heartbeat_at 与 timeout_at 始终成对写入（登记、重试、心跳都一起写），
// 因此两者之差就是当时的 timeout_seconds；异常数据（差值非正）按 0 处理，即不再顺延超时。
func timeoutBudget(heartbeatAt, timeoutAt int64) int32 {
	if heartbeatAt <= 0 || timeoutAt <= heartbeatAt {
		return 0
	}
	return int32(timeoutAt - heartbeatAt)
}

// clampProgress 把上报进度夹到 0..100：直播健康度采样本身可能是越界的浮点转整，
// 不因此拒绝上报（拒绝只会让 Worker 无限重投）。
func clampProgress(p int32) (int32, bool) {
	if p < 0 {
		return 0, true
	}
	if p > 100 {
		return 100, true
	}
	return p, false
}

// normalizeReplayRange 归一拼接区间：from<=0 → 1；to<=0 → 录制水位 last_seq。
// 区间与水位不一致（to>lastSeq）时拒绝——尾部还没落库，拼接会产出缺口回放。
func normalizeReplayRange(fromSeq, toSeq, lastSeq int64) (int64, int64, error) {
	if lastSeq <= 0 {
		return 0, 0, fmt.Errorf("live-media: record has no segment yet: %w", model.ErrSegmentRangeNotRecorded)
	}
	from := fromSeq
	if from <= 0 {
		from = 1
	}
	to := toSeq
	if to <= 0 {
		to = lastSeq
	}
	if from > to {
		return 0, 0, fmt.Errorf("live-media: from_seq=%d > to_seq=%d: %w", from, to, model.ErrInvalidSegmentRange)
	}
	if to > lastSeq {
		return 0, 0, fmt.Errorf("live-media: to_seq=%d > last_seq=%d: %w", to, lastSeq, model.ErrInvalidSegmentRange)
	}
	return from, to, nil
}

// planMissingRows 为 seq 空洞生成 MISSING 行（显式记录，禁止静默跳过——
// 回放拼接必须知道时间轴上有洞）。时间轴按录制任务的 segment_seconds 与本次上报的
// 起点反推；缺时长配置时不补洞（由 logic 记 warn 并只发缺口事件）。
func planMissingRows(task *model.LiveRecordTask, fromSeq, toSeqExclusive int64, workerID, traceID string) []*model.LiveRecordSegment {
	if task == nil || toSeqExclusive <= fromSeq {
		return nil
	}
	if toSeqExclusive-fromSeq > maxGapFillRows {
		return nil
	}
	segSeconds := int64(task.SegmentSeconds)
	if segSeconds <= 0 {
		return nil
	}
	anchor := task.StartAt
	if anchor <= 0 {
		anchor = task.RecordStartAt
	}
	if anchor <= 0 {
		return nil
	}
	rows := make([]*model.LiveRecordSegment, 0, toSeqExclusive-fromSeq)
	for seq := fromSeq; seq < toSeqExclusive; seq++ {
		start := anchor + (seq-1)*segSeconds
		rows = append(rows, &model.LiveRecordSegment{
			RecordId:    task.RecordId,
			RoomId:      task.RoomId,
			LiveSession: task.LiveSession,
			Seq:         seq,
			StartAt:     start,
			EndAt:       start + segSeconds,
			DurationMs:  segSeconds * 1000,
			State:       model.SegmentStateMissing,
			WorkerId:    workerID,
			TraceId:     traceID,
		})
	}
	return rows
}

// durationToleranceMs 切片时长与时间轴的容差：一个分片长度 + 1 秒抖动。
// 越界只记 warn，不改写 Worker 上报的事实。
func durationToleranceMs(segmentSeconds int32) int64 {
	return int64(segmentSeconds)*1000 + 1000
}

// recordedGaps 预读「[1, expectedSeq] 还差多少片」，返回（缺口数, 已登记行数）。
//
// 必须在事务外调用：StatsInRange 走主连接，看不见本事务尚未提交的写入，
// 因此事件里的缺口数是「进入本次事务前」的已提交快照。真值恒在 live_record_task 的
// 派生列（RefreshStats 重算），事件只用于通知「有洞」与给排障一个量级。
func recordedGaps(ctx context.Context, m model.LiveRecordSegmentModel, recordID, expectedSeq int64) (int64, int64, error) {
	if recordID <= 0 || expectedSeq <= 0 {
		return 0, 0, nil
	}
	stats, err := m.StatsInRange(ctx, recordID, 1, expectedSeq)
	if err != nil {
		return 0, 0, err
	}
	return stats.Gaps(expectedSeq), stats.Registered, nil
}

// --- 读侧：分页、游标与枚举过滤归一 ---
//
// 读接口的口径统一在这里，七个列表方法不得各写一套：
// 分页越界的处理必须与 model.clampPage 一致（proto 的 PageParam 注释承诺「超限取默认」，
// 所以 ps 是夹取；pn 深到 OFFSET 扫不动时是报错），否则同一份契约会有两种解释。

// maxListOffset 是 OFFSET 翻页的深翻页保护窗口（同类服务里对应配置项名 MaxOffset）：
// offset=(pn-1)*ps 超过它即拒绝。OFFSET 的代价是线性扫描后被丢弃的行数，
// 页码乘上页大小就是扫描面；真要长期翻，得按 idx_room_state_ctime / idx_session_state
// 这类前缀做游标，而不是把第 N 万页拉出来（见 README 的表索引说明）。
const maxListOffset = 10000

// listPage 归一 pn/ps 并做深翻页保护，返回归一后的 (pn, ps)，调用方直接透传给 model。
// 与 model.clampPage 同口径，因此这里夹取出的 limit 与 SQL 实际用的 LIMIT 必然一致，
// 深翻页判定判的是真正会发生的那次扫描，而不是客户端传来的数字。
func listPage(cfg config.LiveMediaConf, pn, ps int32) (int32, int32, error) {
	maxPS := cfg.MaxListPageSize
	if maxPS <= 0 {
		maxPS = 50 // 与 model.clampPage 的兜底一致：配置缺失时也不让页大小失控
	}
	if pn < 1 {
		pn = 1 // proto：<=0 视为 1
	}
	limit := ps
	if ps <= 0 || ps > maxPS {
		limit = 20
		if limit > maxPS {
			limit = maxPS
		}
	}
	if offset := int64(pn-1) * int64(limit); offset > maxListOffset {
		return 0, 0, fmt.Errorf("live-media: pn=%d ps=%d would OFFSET %d rows (max %d): %w",
			pn, ps, offset, maxListOffset, model.ErrInvalidPage)
	}
	return pn, limit, nil
}

// segmentPageLimit 归一 keyset 拉取的单次上限：<=0 取默认，越界夹到配置上限。
// 与 pn/ps 不同：keyset 的 limit 不改变扫描量级（走 uniq_record_seq 的区间扫，
// 命中多少返回多少），所以夹取足够，不需要拒绝。
func segmentPageLimit(cfg config.LiveMediaConf, requested int32) int32 {
	maxLimit := positiveInt32(cfg.MaxSegmentPageSize, 500)
	return clampInt32(positiveInt32(requested, positiveInt32(cfg.DefaultSegmentPageSize, 200)), 1, maxLimit)
}

// checkAfterSeq 校验 keyset 游标：负数不是「从头」（那是 0），而是明确传坏了。
// 把 -1 静默当 0 会让客户端以为游标已经推进，实际在同一批数据上死循环。
func checkAfterSeq(seq int64) error {
	if seq < 0 {
		return fmt.Errorf("live-media: after_seq=%d must be >= 0 (0 means from start): %w",
			seq, model.ErrInvalidCursor)
	}
	return nil
}

// filterState 归一列表的枚举过滤值：0（UNSPECIFIED）表示不过滤，非 0 必须落在该枚举区间内。
// 未知取值一律报错，不退化成「恒空的结果集」——那会把调用方的版本/取值错误
// 伪装成业务上的「没有数据」，排障时看不出区别。
func filterState(v int32, check func(int32) error) (int32, error) {
	if v == 0 {
		return 0, nil
	}
	if err := check(v); err != nil {
		return 0, err
	}
	return v, nil
}

// checkRetentionState 列表过滤用的回收状态全区间（1..5）。
// 与 checkReportedRetentionState 的区别是必须的：那里 PENDING 不是上报目标态，
// 但作为过滤条件必须能选中「待执行」队列，否则运营看不到未跑的任务。
func checkRetentionState(s int32) error {
	if s < model.RetentionStatePending || s > model.RetentionStateCancelled {
		return fmt.Errorf("live-media: retention state=%d out of LiveRetentionState range: %w",
			s, model.ErrInvalidTransition)
	}
	return nil
}

// --- 幂等重放判定（纯函数，单测直接覆盖） ---

// transcodeReportReplay 判断一次进度上报是否只是重复投递：
// 状态与进度都与当前行一致时不需要推进状态，只需刷新心跳。
func transcodeReportReplay(cur, target, curProgress, newProgress int32) bool {
	return cur == target && curProgress == newProgress
}

// replayReportReplay 判断回放进度上报是否同值重放（状态与产物引用完全一致）。
func replayReportReplay(cur, target int32, curBucket, newBucket, curKey, newKey string) bool {
	return cur == target && curBucket == newBucket && curKey == newKey
}

// retentionReportReplay 判断回收结果上报是否同值重放（状态与三个计数一致）。
func retentionReportReplay(cur, target int32, cs, ns, cd, nd, ck, nk int32) bool {
	return cur == target && cs == ns && cd == nd && ck == nk
}

// retentionCountsConsistent 校验上报计数自洽：非负且 deleted+skipped<=scanned。
func retentionCountsConsistent(scanned, deleted, skipped int32) error {
	if scanned < 0 || deleted < 0 || skipped < 0 {
		return fmt.Errorf("live-media: retention counts negative: scanned=%d deleted=%d skipped=%d: %w",
			scanned, deleted, skipped, model.ErrInvalidTransition)
	}
	if deleted+skipped > scanned {
		return fmt.Errorf("live-media: retention counts inconsistent: deleted=%d skipped=%d > scanned=%d: %w",
			deleted, skipped, scanned, model.ErrInvalidTransition)
	}
	return nil
}

// --- 失败信息脱敏与截断 ---

// sanitizeErrMsg 生成可入库/可日志的错误摘要：先脱敏再按列宽截断。
func sanitizeErrMsg(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}
	msg = strings.ReplaceAll(msg, "\n", " ")
	msg = strings.ReplaceAll(msg, "\r", " ")
	msg = redactSecrets(msg)
	if runeLen(msg) > maxErrMsgRunes {
		msg = truncateRunes(msg, maxErrMsgRunes-1) + "…"
	}
	return msg
}

func redactSecrets(s string) string {
	low := strings.ToLower(s)
	out := []byte(s)
	for _, m := range secretMarkers {
		off := 0
		for off < len(low) {
			i := strings.Index(low[off:], m)
			if i < 0 {
				break
			}
			start := off + i + len(m)
			for start < len(out) && (out[start] == '=' || out[start] == ':' || out[start] == ' ') {
				start++
			}
			end := start
			for end < len(out) && out[end] < 0x80 && !isRedactDelim(out[end]) {
				end++
			}
			for k := start; k < end; k++ {
				out[k] = '*'
			}
			off = end
			if end == start {
				off = start + 1
			}
		}
	}
	return maskURLCredentials(string(out))
}

func isRedactDelim(c byte) bool {
	switch c {
	case ' ', '\t', '&', ';', ',', '"', '\'', ')', '}', ']', '?':
		return true
	}
	return false
}

// maskURLCredentials 处理 "scheme://user:pass@host" 形式的内嵌凭据。
func maskURLCredentials(s string) string {
	low := strings.ToLower(s)
	var sb strings.Builder
	from := 0
	for {
		i := strings.Index(low[from:], "://")
		if i < 0 {
			break
		}
		schemeEnd := from + i + 3
		segEnd := schemeEnd
		for segEnd < len(s) && !strings.ContainsRune(" \t\r\n\"'<>", rune(s[segEnd])) {
			segEnd++
		}
		host := s[schemeEnd:segEnd]
		at := strings.LastIndex(host, "@")
		if at >= 0 && strings.Contains(host[:at], ":") {
			sb.WriteString(s[from:schemeEnd])
			sb.WriteString("***:***@")
			sb.WriteString(host[at+1:])
			from = segEnd
			continue
		}
		// 无内嵌凭据的地址原样保留：必须先把已扫描过的前缀写进 sb，
		// 否则 from>0 之后 sb 只含最后一个 URL 之后的文本，
		// err_msg / 审核备注里 URL 之前的内容会被整段吃掉。
		sb.WriteString(s[from:segEnd])
		from = segEnd
	}
	if from == 0 {
		return s
	}
	sb.WriteString(s[from:])
	return sb.String()
}

func hasSecretMarker(s string) bool {
	low := strings.ToLower(s)
	for _, m := range secretMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

func hasUserInfoCredential(s string) bool {
	low := strings.ToLower(s)
	i := strings.Index(low, "://")
	if i < 0 {
		return false
	}
	rest := s[i+3:]
	if j := strings.IndexAny(rest, " \t\r\n"); j >= 0 {
		rest = rest[:j]
	}
	at := strings.LastIndex(rest, "@")
	return at >= 0 && strings.Contains(rest[:at], ":")
}

func runeLen(s string) int { return len([]rune(s)) }

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// --- Outbox：业务写与事件同事务提交（AGENTS.md §5） ---

// appendOutboxEvent 在调用方事务里登记一条领域事件。
// payload 只放主键引用与状态编号：绝不放 bucket 凭据、签名地址或完整拉流地址。
func appendOutboxEvent(ctx context.Context, s *svc.ServiceContext, sess sqlx.Session,
	eventType, aggregateType string, aggregateID, roomID int64, payload map[string]any, traceID string) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("live-media/logic: marshal %s payload: %w", eventType, err)
	}
	env, err := eventenvelope.New(model.ProducerName, eventType, aggregateType,
		strconv.FormatInt(aggregateID, 10), model.EventSchemaVersion, json.RawMessage(raw), traceID)
	if err != nil {
		return fmt.Errorf("live-media/logic: build %s envelope: %w", eventType, err)
	}
	envelopeJSON, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("live-media/logic: marshal %s envelope: %w", eventType, err)
	}
	return s.Outbox.Insert(ctx, sess, &model.LiveMediaOutbox{
		EventId:       env.EventID,
		EventType:     env.EventType,
		SchemaVersion: model.EventSchemaVersion,
		AggregateType: env.AggregateType,
		AggregateId:   env.AggregateID,
		RoomId:        roomID,
		Payload:       string(envelopeJSON),
		State:         model.OutboxStatePending,
		OccurredAt:    model.NowUnix(),
		TraceId:       traceID,
	})
}

// --- 缓存（只加速读，真值恒在 MySQL） ---

func taskCacheKey(kind string, id int64) string {
	return taskCachePrefix + kind + ":" + strconv.FormatInt(id, 10)
}

// cacheTTL 只在「行已进终态」时启用：终态行不再变化，缓存不会掩盖续录/超时判据；
// 运行中的行一律直读主表（last_seq/heartbeat_at/version 是决策输入）。
func cacheTTL(cfg config.LiveMediaConf) int {
	if cfg.TaskCacheTTLSeconds <= 0 {
		return 0
	}
	return cfg.TaskCacheTTLSeconds
}

// taskDetailTTL 取任务详情缓存秒数：terminal=false 时恒为 0（既不发缓存也不写缓存）。
// 本服务的写路径（Stop/Retry/Cancel/Report）用条件 UPDATE 推进状态且不做缓存失效，
// 所以运行中的行必须直读主表：缓存到旧值等于让上报方拿旧 version 当 expected_version
// （永远撞版本冲突），或按旧 last_seq 续录（覆盖已登记切片）。
// 只有永不再变化的终态行才允许被缓存，TTL 到期即回源，无需失效动作。
func taskDetailTTL(cfg config.LiveMediaConf, terminal bool) int {
	if !terminal {
		return 0
	}
	return cacheTTL(cfg)
}

func readCachedInfo(ctx context.Context, s *svc.ServiceContext, key string, into any) bool {
	if s.Cache == nil || key == "" {
		return false
	}
	raw, err := s.Cache.GetCtx(ctx, key)
	if err != nil || raw == "" {
		return false
	}
	if err := json.Unmarshal([]byte(raw), into); err != nil {
		logx.Errorf("livemedia/logic: bad cache %s: %v", key, err)
		return false
	}
	return true
}

func writeCachedInfo(ctx context.Context, s *svc.ServiceContext, key string, ttl int, info any) {
	if s.Cache == nil || ttl <= 0 || key == "" {
		return
	}
	raw, err := json.Marshal(info)
	if err != nil {
		return
	}
	if err := s.Cache.SetexCtx(ctx, key, string(raw), ttl); err != nil {
		logx.Errorf("livemedia/logic: set cache %s: %v", key, err)
	}
}

// outputListCacheKey 带房间档位列表的代际号：写侧只递增代际键，
// 就能让该房间此前的列表键在 TTL 内自然失效，无需 SCAN 前缀（键空间不可枚举）。
func outputListCacheKey(roomID, sessionID int64, includeOffline bool, pn, ps int32, gen int64) string {
	io := 0
	if includeOffline {
		io = 1
	}
	return fmt.Sprintf("%s%d:g%d:%d|%d|%d|%d", outputListPrefix, roomID, gen, sessionID, io, pn, ps)
}

func outputGenKey(roomID int64) string {
	return outputListPrefix + strconv.FormatInt(roomID, 10) + outputGenSuffix
}

func readOutputGen(ctx context.Context, s *svc.ServiceContext, roomID int64) int64 {
	if s.Cache == nil {
		return 0
	}
	raw, err := s.Cache.GetCtx(ctx, outputGenKey(roomID))
	if err != nil || raw == "" {
		return 0
	}
	gen, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0
	}
	return gen
}

func bumpOutputGen(ctx context.Context, s *svc.ServiceContext, roomID int64) {
	if s.Cache == nil || roomID <= 0 {
		return
	}
	key := outputGenKey(roomID)
	if _, err := s.Cache.IncrCtx(ctx, key); err != nil {
		logx.Errorf("livemedia/logic: bump output gen %d: %v", roomID, err)
		return
	}
	if err := s.Cache.ExpireCtx(ctx, key, outputGenTTLSecond); err != nil {
		logx.Errorf("livemedia/logic: expire output gen %d: %v", roomID, err)
	}
}

// containsState 判断某状态是否在允许集合里（logic 侧的前置态判定，与 model 的 IN 条件同语义）。
func containsState(states []int32, v int32) bool {
	for _, s := range states {
		if s == v {
			return true
		}
	}
	return false
}

// --- 条件 UPDATE 命中 0 行的归因 ---
//
// 0 行绝不能当成功：回读一次，区分「行不存在」「版本被抢先」「前置状态不允许」。

func classifyTranscodeZeroRow(ctx context.Context, m model.LiveTranscodeTaskModel, taskID, expectedVersion int64) error {
	cur, err := m.FindOne(ctx, taskID)
	if err != nil {
		return err
	}
	if cur == nil {
		return model.ErrTranscodeTaskNotFound
	}
	if expectedVersion > 0 && cur.Version != expectedVersion {
		return model.ErrVersionConflict
	}
	if model.IsTranscodeTerminal(cur.State) {
		return model.ErrTerminalState
	}
	return model.ErrInvalidTransition
}

func classifyRecordZeroRow(ctx context.Context, m model.LiveRecordTaskModel, recordID, expectedVersion int64) error {
	cur, err := m.FindOne(ctx, recordID)
	if err != nil {
		return err
	}
	if cur == nil {
		return model.ErrRecordTaskNotFound
	}
	if expectedVersion > 0 && cur.Version != expectedVersion {
		return model.ErrVersionConflict
	}
	if model.IsRecordTerminal(cur.State) {
		return model.ErrTerminalState
	}
	return model.ErrInvalidTransition
}

func classifyReplayZeroRow(ctx context.Context, m model.LiveReplayTaskModel, replayID, expectedVersion int64) error {
	cur, err := m.FindOne(ctx, replayID)
	if err != nil {
		return err
	}
	if cur == nil {
		return model.ErrReplayTaskNotFound
	}
	if expectedVersion > 0 && cur.Version != expectedVersion {
		return model.ErrVersionConflict
	}
	if model.IsReplayTerminal(cur.State) {
		return model.ErrTerminalState
	}
	return model.ErrInvalidTransition
}

func classifyRetentionZeroRow(ctx context.Context, m model.LiveRetentionTaskModel,
	retentionID, expectedVersion int64) error {
	cur, err := m.FindOne(ctx, retentionID)
	if err != nil {
		return err
	}
	if cur == nil {
		return model.ErrRetentionTaskNotFound
	}
	if expectedVersion > 0 && cur.Version != expectedVersion {
		return model.ErrVersionConflict
	}
	if model.IsRetentionTerminal(cur.State) {
		return model.ErrTerminalState
	}
	return model.ErrInvalidTransition
}

// --- 投影：model 行 → rpc Info（逐列映射，不做二次推断） ---

func transcodeInfo(t *model.LiveTranscodeTask) *rpc.LiveTranscodeTaskInfo {
	if t == nil {
		return nil
	}
	return &rpc.LiveTranscodeTaskInfo{
		TaskId:        t.TaskId,
		RoomId:        t.RoomId,
		LiveSessionId: t.LiveSession,
		TemplateId:    t.TemplateId,
		BitrateLevel:  rpc.BitrateLevel(t.BitrateLevel),
		Protocol:      rpc.StreamProtocol(t.Protocol),
		SourceRef:     t.SourceRef,
		AnchorMid:     t.AnchorMid,
		State:         rpc.LiveTranscodeState(t.State),
		Progress:      t.Progress,
		Attempt:       t.Attempt,
		MaxAttempts:   t.MaxAttempts,
		StartedAt:     t.StartedAt,
		StoppedAt:     t.StoppedAt,
		HeartbeatAt:   t.HeartbeatAt,
		TimeoutAt:     t.TimeoutAt,
		Version:       t.Version,
		Reason:        rpc.FailureReason(t.Reason),
		Errno:         t.Errno,
		ErrMsg:        t.ErrMsg,
		RequestId:     t.RequestId,
		TraceId:       t.TraceId,
		Ctime:         t.Ctime,
		Mtime:         t.Mtime,
	}
}

func recordInfo(t *model.LiveRecordTask) *rpc.LiveRecordTaskInfo {
	if t == nil {
		return nil
	}
	return &rpc.LiveRecordTaskInfo{
		RecordId:           t.RecordId,
		RoomId:             t.RoomId,
		LiveSessionId:      t.LiveSession,
		SourceTaskId:       t.SourceTaskId,
		State:              rpc.LiveRecordState(t.State),
		StartAt:            t.StartAt,
		EndAt:              t.EndAt,
		RecordStartAt:      t.RecordStartAt,
		RecordEndAt:        t.RecordEndAt,
		SegmentSeconds:     t.SegmentSeconds,
		LastSeq:            t.LastSeq,
		SegmentCount:       t.SegmentCount,
		GapCount:           t.GapCount,
		RecordedDurationMs: t.RecordedDuration,
		OutputBucket:       t.OutputBucket,
		OutputPrefix:       t.OutputPrefix,
		HeartbeatAt:        t.HeartbeatAt,
		TimeoutAt:          t.TimeoutAt,
		Version:            t.Version,
		Reason:             rpc.FailureReason(t.Reason),
		Errno:              t.Errno,
		ErrMsg:             t.ErrMsg,
		RequestId:          t.RequestId,
		TraceId:            t.TraceId,
		Ctime:              t.Ctime,
		Mtime:              t.Mtime,
	}
}

func segmentInfo(s *model.LiveRecordSegment) *rpc.RecordSegmentInfo {
	if s == nil {
		return nil
	}
	return &rpc.RecordSegmentInfo{
		Id:            s.Id,
		RecordId:      s.RecordId,
		RoomId:        s.RoomId,
		LiveSessionId: s.LiveSession,
		Seq:           s.Seq,
		StartAt:       s.StartAt,
		EndAt:         s.EndAt,
		DurationMs:    s.DurationMs,
		State:         rpc.SegmentState(s.State),
		Bucket:        s.Bucket,
		ObjectKey:     s.ObjectKey,
		SizeBytes:     s.SizeBytes,
		Checksum:      s.Checksum,
		WorkerId:      s.WorkerId,
		RegisteredAt:  s.RegisteredAt,
		Mtime:         s.Mtime,
	}
}

func replayInfo(t *model.LiveReplayTask) *rpc.LiveReplayTaskInfo {
	if t == nil {
		return nil
	}
	return &rpc.LiveReplayTaskInfo{
		ReplayId:      t.ReplayId,
		RoomId:        t.RoomId,
		LiveSessionId: t.LiveSession,
		RecordId:      t.RecordId,
		State:         rpc.ReplayState(t.State),
		FromSeq:       t.FromSeq,
		ToSeq:         t.ToSeq,
		SegmentCount:  t.SegmentCount,
		GapCount:      t.GapCount,
		StartAt:       t.StartAt,
		EndAt:         t.EndAt,
		DurationMs:    t.DurationMs,
		AllowGaps:     t.AllowGaps != 0,
		OutputBucket:  t.OutputBucket,
		OutputKey:     t.OutputKey,
		AssetId:       t.AssetId,
		Aid:           t.Aid,
		Bvid:          t.Bvid,
		AnchorMid:     t.AnchorMid,
		Title:         t.Title,
		Version:       t.Version,
		Reason:        rpc.FailureReason(t.Reason),
		Errno:         t.Errno,
		ErrMsg:        t.ErrMsg,
		RequestId:     t.RequestId,
		TraceId:       t.TraceId,
		Ctime:         t.Ctime,
		Mtime:         t.Mtime,
	}
}

func refInfo(r *model.LiveReplayAssetRef) *rpc.ReplayAssetRefInfo {
	if r == nil {
		return nil
	}
	return &rpc.ReplayAssetRefInfo{
		Id:             r.Id,
		RoomId:         r.RoomId,
		LiveSessionId:  r.LiveSession,
		ReplayId:       r.ReplayId,
		RecordId:       r.RecordId,
		AssetId:        r.AssetId,
		Aid:            r.Aid,
		Bvid:           r.Bvid,
		AnchorMid:      r.AnchorMid,
		Bucket:         r.Bucket,
		ObjectKey:      r.ObjectKey,
		DurationMs:     r.DurationMs,
		SegmentFromSeq: r.SegmentFromSeq,
		SegmentToSeq:   r.SegmentToSeq,
		GapCount:       r.GapCount,
		ReviewState:    rpc.ReviewState(r.ReviewState),
		ReviewStateAt:  r.ReviewStateAt,
		RetentionState: r.RetentionState,
		PublishedAt:    r.PublishedAt,
		Ctime:          r.Ctime,
		Mtime:          r.Mtime,
	}
}

func outputInfo(o *model.LiveStreamOutput) *rpc.StreamOutputInfo {
	if o == nil {
		return nil
	}
	return &rpc.StreamOutputInfo{
		OutputId:       o.OutputId,
		RoomId:         o.RoomId,
		LiveSessionId:  o.LiveSession,
		TaskId:         o.TaskId,
		BitrateLevel:   rpc.BitrateLevel(o.BitrateLevel),
		Protocol:       rpc.StreamProtocol(o.Protocol),
		Bucket:         o.Bucket,
		ObjectKey:      o.ObjectKey,
		CdnDomain:      o.CdnDomain,
		Width:          o.Width,
		Height:         o.Height,
		BitrateKbps:    o.BitrateKbps,
		Fps:            o.Fps,
		State:          o.State,
		OnlineAt:       o.OnlineAt,
		OfflineAt:      o.OfflineAt,
		OnlineExpireAt: o.OnlineExpire,
		RequestId:      o.RequestId,
		Ctime:          o.Ctime,
		Mtime:          o.Mtime,
		Reason:         rpc.FailureReason(o.OfflineReason),
	}
}

func retentionInfo(t *model.LiveRetentionTask) *rpc.LiveRetentionTaskInfo {
	if t == nil {
		return nil
	}
	return &rpc.LiveRetentionTaskInfo{
		RetentionId:  t.RetentionId,
		TargetKind:   rpc.RetentionTargetKind(t.TargetKind),
		RoomId:       t.RoomId,
		TargetId:     t.TargetId,
		ExpireBefore: t.ExpireBefore,
		Purge:        t.Purge != 0,
		BatchLimit:   t.BatchLimit,
		State:        rpc.RetentionState(t.State),
		Scanned:      t.Scanned,
		Deleted:      t.Deleted,
		Skipped:      t.Skipped,
		Reason:       t.Reason,
		Operator:     t.Operator,
		Version:      t.Version,
		FailReason:   rpc.FailureReason(t.FailReason),
		Errno:        t.Errno,
		ErrMsg:       t.ErrMsg,
		RequestId:    t.RequestId,
		TraceId:      t.TraceId,
		Ctime:        t.Ctime,
		Mtime:        t.Mtime,
	}
}

func transcodeInfos(rows []*model.LiveTranscodeTask) []*rpc.LiveTranscodeTaskInfo {
	out := make([]*rpc.LiveTranscodeTaskInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, transcodeInfo(r))
	}
	return out
}

func recordInfos(rows []*model.LiveRecordTask) []*rpc.LiveRecordTaskInfo {
	out := make([]*rpc.LiveRecordTaskInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, recordInfo(r))
	}
	return out
}

func segmentInfos(rows []*model.LiveRecordSegment) []*rpc.RecordSegmentInfo {
	out := make([]*rpc.RecordSegmentInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, segmentInfo(r))
	}
	return out
}

func replayInfos(rows []*model.LiveReplayTask) []*rpc.LiveReplayTaskInfo {
	out := make([]*rpc.LiveReplayTaskInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, replayInfo(r))
	}
	return out
}

func refInfos(rows []*model.LiveReplayAssetRef) []*rpc.ReplayAssetRefInfo {
	out := make([]*rpc.ReplayAssetRefInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, refInfo(r))
	}
	return out
}

func outputInfos(rows []*model.LiveStreamOutput) []*rpc.StreamOutputInfo {
	out := make([]*rpc.StreamOutputInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, outputInfo(r))
	}
	return out
}

func retentionInfos(rows []*model.LiveRetentionTask) []*rpc.LiveRetentionTaskInfo {
	out := make([]*rpc.LiveRetentionTaskInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, retentionInfo(r))
	}
	return out
}

func pageResult(total int32) *rpc.PageResult { return &rpc.PageResult{Total: total} }

func boolInt32(v bool) int32 {
	if v {
		return 1
	}
	return 0
}

// i32p / i64p / strp 给 Patch 的可选字段取地址（同 services/live-room 口径）。
func i32p(v int32) *int32   { return &v }
func i64p(v int64) *int64   { return &v }
func strp(v string) *string { return &v }
