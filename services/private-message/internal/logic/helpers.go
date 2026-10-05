// 本文件是 logic 包的手写扩展（入参校验、游标编解码、脱敏摘要与占位文案、缓存键与计数器），
// 不是 goctl 生成产物。
//
// 分工（AGENTS.md §4/§5）：这里只放「不碰 SQL」的可测函数——校验、归一、纯判定与键构造。
// SQL、CAS 与事务边界一律留在 model；下游判定留在 gate.go；加解密留在 cipher.go。

package logic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go-video/services/private-message/internal/svc"
	"go-video/services/private-message/model"
	"go-video/services/private-message/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 列宽约束（与 deploy/migrations/private-message/*.sql 一致）。
// 校验发生在 logic：model 只保证「不合法就别写」，logic 保证「别把不合法的请求带进事务」。
const (
	// maxClientMsgIDRunes 对应 pm_message.client_msg_id VARCHAR(64)：幂等键超长会破坏重放。
	maxClientMsgIDRunes = 64
	// maxIdempotencyKeyRunes 对应 pm_report.handle_idempotency_key VARCHAR(128)。
	maxIdempotencyKeyRunes = 128
	// maxAuditEventIDRunes 对应 pm_message.audit_event_id VARCHAR(64)。
	maxAuditEventIDRunes = 64
	// maxReasonRunes 对应 pm_withdraw_log.reason VARCHAR(255)：留 5 个字符余量，
	// 超长直接拒绝而不是截断（审计文本必须与库值一致，截断会让回查对不上）。
	maxReasonRunes = 250
	// maxDescriptionRunes / maxNoteRunes 对应 pm_report.description / handle_note VARCHAR(500)：
	// 这两列按上限截断而不是拒绝——举报说明与处置备注是「补充材料」，
	// 因长度丢掉整笔举报反而丢了证据。
	maxDescriptionRunes = 500
	maxNoteRunes        = 500
	// maxMediaRefRunes 对应 pm_message.media_ref VARCHAR(128)。
	maxMediaRefRunes = 128
	// maxPreviewRunes 对应 pm_*_.last_preview / preview VARCHAR(255)。
	maxPreviewRunes = 255
	// maxTraceIDBytes 对应各表 trace_id VARCHAR(64)。
	maxTraceIDBytes = 64
	// minPreviewRunes 是摘要列的最小可用长度（保留省略号位置）。
	minPreviewRunes = 4
	// fallbackMaxTextLength 只在配置缺失（0/负数）时兜底，与 etc 默认值一致；
	// 正常路径永远走 PrivateMessage.MaxTextLength，不在此写死业务口径。
	fallbackMaxTextLength = 2000
)

// 占位文案：正文不可见（撤回/驳回/处置/留存到期）时对外返回的固定文本。
// 固定文案的价值在于「不泄露任何原文信息」——连「原文有多长」都不透露。
const (
	contentWithdrawn = "该消息已撤回"
	contentRejected  = "该消息未通过审核"
	contentDeleted   = "该消息已被处置删除"
	contentPurged    = "该消息正文已超过留存期并按规定删除"

	previewWithdrawn = "[消息已撤回]"
	previewRejected  = "[审核未通过]"
	previewDeleted   = "[内容已删除]"
	previewPurged    = "[正文已清理]"
)

// 非文本载体的固定摘要文案（README「正文加密与留存」：摘要必须脱敏）。
const (
	previewImage = "[图片]"
	previewAudio = "[语音]"
	previewVideo = "[短视频]"
	previewShare = "[分享]"
)

// 缓存键与 TTL：命名空间独立于其它服务（AGENTS.md §5 的 key 空间边界）。
const (
	cacheKeyPrefix          = "govideo:pm:"
	ttlUnreadSummary    int = 5     // 角标缓存：秒级，脏了指标准确性远不如脏了会话内容
	ttlPairVerdict      int = 30    // 黑名单/关注判定短缓存（真值在 social-graph）
	ttlSendRateWindow   int = 60    // 与 MaxPerUserPerMinute 对应的窗口
	ttlStrangerQuotaDay int = 86400 // 陌生人会话日配额
	keyFieldSep             = "|"
	// counterUnlimited 表示配置里 0/负数 = 不启用该项本地限额（限额语义：<=0 不设限）。
	quotaUnlimited = 0
)

// --- 入参校验 ---

func checkMid(mid int64) error {
	if mid <= 0 {
		return model.ErrInvalidMid
	}
	return nil
}

// checkPair 校验「发起人 + 对方」：私信不支持游客，也不允许自聊。
// 只服务于建档入口（GetOrCreateConversation）——那里的失败含义是「你要建一条自聊会话」；
// 发送入口不用它：发送侧的失败含义是「这一对之间没有可证明的成员行」（notMemberForPair）。
func checkPair(mid, peer int64) error {
	if err := checkMid(mid); err != nil {
		return err
	}
	if peer <= 0 {
		return model.ErrInvalidMid
	}
	if mid == peer {
		return model.ErrSelfConversation
	}
	return nil
}

func checkConversationID(id int64) error {
	if id <= 0 {
		return model.ErrConversationNotFound
	}
	return nil
}

func checkMsgID(id int64) error {
	if id <= 0 {
		return model.ErrMessageNotFound
	}
	return nil
}

// checkOperator 校验运营/管理员身份字段：没有归因主体的处置不可受理。
// 真正的管理员鉴权在 gateway/admin，本服务只拒绝「无主」写请求。
func checkOperator(mid int64) error {
	if mid <= 0 {
		return model.ErrOperatorRequired
	}
	return nil
}

// systemSubject 是「没有自然人主体」的归因值：proto 里 cron 与机审都以 0 表达
// （rpc/privatemessage.proto:398 触发者「cron 传 0」、:378 处理人「0 表示机审」）。
const systemSubject int64 = 0

// checkSubjectOrOperator 用于「机器主体也会调用」的运营侧入口（审核结论回写、留存到期清理）。
// 这类入口绝不能不校验主体：0 只允许是 systemSubject（cron/机审的契约取值），
// 负数既不是主体也不是系统，与「缺主体」同口径拒绝；>0 时退回 checkOperator。
func checkSubjectOrOperator(subject int64) error {
	if subject == systemSubject {
		return nil
	}
	return checkOperator(subject)
}

// checkClientMsgID 校验发送幂等键。空值一律拒绝：
// 服务端补随机值等于放弃幂等（model.MessageModel.Insert 同样拒绝空键，两层口径一致）。
func checkClientMsgID(id string) (string, error) {
	v := strings.TrimSpace(id)
	if v == "" {
		return "", model.ErrClientMsgIDRequired
	}
	if runeLen(v) > maxClientMsgIDRunes {
		return "", fmt.Errorf("%w: client_msg_id %d > %d", model.ErrClientMsgIDRequired, runeLen(v), maxClientMsgIDRunes)
	}
	return v, nil
}

func checkIdempotencyKey(key string) (string, error) {
	v := strings.TrimSpace(key)
	if v == "" {
		return "", model.ErrIdempotencyKeyRequired
	}
	if runeLen(v) > maxIdempotencyKeyRunes {
		return "", fmt.Errorf("%w: idempotency_key %d > %d", model.ErrIdempotencyKeyRequired, runeLen(v), maxIdempotencyKeyRunes)
	}
	return v, nil
}

func checkEventID(id string) (string, error) {
	v := strings.TrimSpace(id)
	if v == "" {
		return "", fmt.Errorf("%w: event_id required", model.ErrIdempotencyKeyRequired)
	}
	if runeLen(v) > maxAuditEventIDRunes {
		return "", fmt.Errorf("%w: event_id %d > %d", model.ErrIdempotencyKeyRequired, runeLen(v), maxAuditEventIDRunes)
	}
	return v, nil
}

// checkReasonText 校验审计文本（撤回原因、处置备注、审核结论说明）。
// required=true 时空串即拒绝。审计文本禁止携带私信正文。
func checkReasonText(name, reason string, required bool) (string, error) {
	v := strings.TrimSpace(reason)
	if v == "" {
		if required {
			return "", fmt.Errorf("%w: %s required", model.ErrInvalidStateTransition, name)
		}
		return "", nil
	}
	if runeLen(v) > maxReasonRunes {
		return "", fmt.Errorf("%w: %s %d > %d", model.ErrInvalidStateTransition, name, runeLen(v), maxReasonRunes)
	}
	return v, nil
}

// checkMediaRef 校验媒资引用：只允许 asset 主键这类不透明引用，
// 明确拒绝 URL——对象存储地址（尤其是可直连的公共读地址）不得入库（AGENTS.md §6）。
func checkMediaRef(ref string) (string, error) {
	v := strings.TrimSpace(ref)
	if v == "" {
		return "", model.ErrMediaRefRequired
	}
	if runeLen(v) > maxMediaRefRunes {
		return "", fmt.Errorf("%w: media_ref %d > %d", model.ErrMediaRefRequired, runeLen(v), maxMediaRefRunes)
	}
	low := strings.ToLower(v)
	if strings.Contains(low, "://") || strings.HasPrefix(low, "http:") {
		return "", fmt.Errorf("%w: 只允许 asset 主键或站内 object key", model.ErrMediaRefRequired)
	}
	if strings.ContainsAny(v, " \t\n") {
		return "", fmt.Errorf("%w: 不允许空白字符", model.ErrMediaRefRequired)
	}
	return v, nil
}

// checkContentText 归一并校验文本正文：按 rune 计长（列宽与多字节绕过都以字符数为准）。
// 非文本载体允许空正文（正文可为空的图片消息合法）。
func checkContentText(content string, maxRunes int32) (string, error) {
	c := strings.TrimSpace(content)
	if maxRunes <= 0 {
		maxRunes = fallbackMaxTextLength
	}
	if int32(runeLen(c)) > maxRunes {
		return "", fmt.Errorf("%w: %d > %d", model.ErrContentTooLong, runeLen(c), maxRunes)
	}
	return c, nil
}

// --- 分页与游标 ---

// clampPageSize 归一每页大小：0 取服务端默认，负数拒绝，超过上限拒绝。
// 这里不接受「超过上限就夹到上限」——静默夹会让客户端以为拿到了自己要求的页大小，
// 翻页游标语义随之失真，显式报错更安全。
func clampPageSize(ps, defaultPS, maxPS int32) (int32, error) {
	if ps < 0 {
		return 0, fmt.Errorf("%w: ps=%d", model.ErrInvalidPage, ps)
	}
	if ps == 0 {
		ps = defaultPS
	}
	if ps <= 0 {
		return 0, fmt.Errorf("%w: 服务端默认页大小非法 ps=%d", model.ErrInvalidPage, defaultPS)
	}
	if maxPS > 0 && ps > maxPS {
		return 0, fmt.Errorf("%w: ps=%d max=%d", model.ErrPsTooLarge, ps, maxPS)
	}
	return ps, nil
}

// encodeTimeIDCursor 会话列表游标：(last_msg_time, member_id) 双列位点。
// 单列时间戳在同秒多条会话之间无法稳定续翻，因此必须带上 id。
func encodeTimeIDCursor(msgTime, id int64) string {
	if msgTime <= 0 || id <= 0 {
		return ""
	}
	return strconv.FormatInt(msgTime, 10) + ":" + strconv.FormatInt(id, 10)
}

// decodeTimeIDCursor 解析会话列表游标。空串表示第一页；
// 不可解析一律报错——退化成「当作第一页」会让客户端在翻页抖动时重复拿到最新一页并以为没到底。
func decodeTimeIDCursor(cursor string) (int64, int64, error) {
	trimmed := strings.TrimSpace(cursor)
	if trimmed == "" {
		return 0, 0, nil
	}
	parts := strings.SplitN(trimmed, ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("%w: %q", model.ErrInvalidCursor, cursor)
	}
	msgTime, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	if err != nil || msgTime < 0 {
		return 0, 0, fmt.Errorf("%w: %q", model.ErrInvalidCursor, cursor)
	}
	id, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err != nil || id <= 0 {
		return 0, 0, fmt.Errorf("%w: %q", model.ErrInvalidCursor, cursor)
	}
	return msgTime, id, nil
}

func encodeIDCursor(id int64) string {
	if id <= 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}

func decodeIDCursor(cursor string) (int64, error) {
	trimmed := strings.TrimSpace(cursor)
	if trimmed == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || id < 0 {
		return 0, fmt.Errorf("%w: %q", model.ErrInvalidCursor, cursor)
	}
	return id, nil
}

// --- 文本工具 ---

func runeLen(s string) int { return len([]rune(s)) }

// timeNowUnix 统一取时钟：同一请求内的时间判定（撤回窗口、留存截止点、投影时间）
// 只读一次系统时钟，避免跨秒抖动让「窗口内」判定与落库时间互相矛盾。
func timeNowUnix() int64 { return time.Now().Unix() }

// truncateRunes 按 rune 截断（列宽是字符数，不是字节数）。
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

// sanitizeTraceID 裁剪 trace_id 到列宽：trace 是关联句柄而非业务事实，
// 截断尾巴不影响正确性，而因它让整次写入失败反而丢状态。
func sanitizeTraceID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) > maxTraceIDBytes {
		return id[:maxTraceIDBytes]
	}
	return id
}

// maskRedirection 打码正文里的引流形态（链接与长数字串）。
//
// 这不是「关键词词典」：本服务没有词典表（README 已知缺口），命中判定完全交给 moderation 机审。
// 这里只做一件事——保证会话列表的脱敏摘要不会变成引流广告位，
// 因为 last_preview 是明文入库并会出现在列表响应里。
func maskRedirection(s string) string {
	if s == "" {
		return s
	}
	lowered := strings.ToLower(s)
	switch {
	case strings.Contains(lowered, "://"), strings.Contains(lowered, "http:"),
		strings.Contains(lowered, "www."), strings.Contains(lowered, ".com"),
		strings.Contains(lowered, ".cn"):
		return "[含链接，已隐去]"
	}
	// 连续 7 位以上数字（手机号/QQ 等引流形态）整串隐去。
	if runsOfDigits(s) {
		return "[含联系方式，已隐去]"
	}
	return s
}

// displayUserText 处理「用户输入但会展示给运营/对方」的自由文本（举报说明等）：
// 先去引流形态再按列上限截断。这类文本不是私信正文，不涉解密授权，
// 但同样不能变成运营台里的广告位或超长溢出。
func displayUserText(s string) string {
	return truncateRunes(maskRedirection(strings.TrimSpace(s)), maxDescriptionRunes)
}

func runsOfDigits(s string) bool {
	run := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			run++
			if run >= 7 {
				return true
			}
			continue
		}
		run = 0
	}
	return false
}

// buildPreview 生成脱敏摘要（写入 pm_message.preview 与成员投影 last_preview）。
//
// 硬约束：摘要恒不等于正文原文——摘要会出现在会话列表与任何允许携带内容表示的地方，
// 而原文只允许出现在「成员 + 可见门禁」通过后的消息读取响应里。
// 因此无论正文多短，摘要都以省略号收尾；若原串本身以省略号结尾，再削一个字符，
// 保证 preview != content 恒成立。
func buildPreview(msgType int32, content string, maxRunes int32) string {
	if maxRunes < minPreviewRunes {
		maxRunes = minPreviewRunes
	}
	if msgType != model.MsgTypeText {
		return mediaPreviewLabel(msgType)
	}
	text := strings.TrimSpace(maskRedirection(content))
	if text == "" {
		return ""
	}
	keep := int(maxRunes) - 1
	r := []rune(text)
	if len(r) > keep {
		r = r[:keep]
	}
	out := string(r)
	for strings.HasSuffix(out, "…") || strings.HasSuffix(out, "...") {
		nb := []rune(out)
		if len(nb) == 0 {
			break
		}
		out = string(nb[:len(nb)-1])
	}
	return truncateRunes(out+"…", maxPreviewRunes)
}

func mediaPreviewLabel(msgType int32) string {
	switch msgType {
	case model.MsgTypeImage:
		return previewImage
	case model.MsgTypeAudio:
		return previewAudio
	case model.MsgTypeVideo:
		return previewVideo
	case model.MsgTypeShare:
		return previewShare
	default:
		return "[消息]"
	}
}

// contentPlaceholder 给出「这一条对外显示什么」的固定文案；
// 返回 ok=false 表示这一行应当原样解密返回（NORMAL / PENDING_REVIEW 且正文仍在）。
func contentPlaceholder(msg *model.Message) (string, bool) {
	if msg == nil {
		return "", true
	}
	switch msg.State {
	case model.MsgStateWithdrawn:
		return contentWithdrawn, true
	case model.MsgStateRejected:
		return contentRejected, true
	case model.MsgStateDeleted:
		return contentDeleted, true
	}
	if !msg.HasPlainContent() {
		// 留存到期：行还在、seq 还在、审计还在，只是正文没了——
		// 回占位文案而不是报错，用户看到的是「内容已按留存策略删除」而非故障。
		return contentPurged, true
	}
	return "", false
}

// previewForProjection 给出成员投影列在该消息进入终态后应显示的摘要。
func previewForProjection(msg *model.Message) string {
	if msg == nil {
		return ""
	}
	switch msg.State {
	case model.MsgStateWithdrawn:
		return previewWithdrawn
	case model.MsgStateRejected:
		return previewRejected
	case model.MsgStateDeleted:
		return previewDeleted
	}
	if !msg.HasPlainContent() {
		return previewPurged
	}
	return msg.Preview
}

// isVisibleTo 判定「这一行是否对该查看者存在」。
// PENDING_REVIEW 只对发送者本人可见（机审结论回写前不外露，AGENTS.md §8）；
// 其余状态都返回，由 contentPlaceholder 决定是否换成占位文案。
func isVisibleTo(msg *model.Message, viewer int64) bool {
	if msg == nil {
		return false
	}
	if msg.State == model.MsgStatePendingReview {
		return msg.SenderMid == viewer
	}
	return true
}

func isTerminalMsgState(state int32) bool {
	switch state {
	case model.MsgStateWithdrawn, model.MsgStateRejected, model.MsgStateDeleted:
		return true
	default:
		return false
	}
}

func boolToTiny(v bool) int8 {
	if v {
		return 1
	}
	return 0
}

func tinyToBool(v int8) bool { return v != 0 }

// --- 缓存（只加速，真值恒在 MySQL） ---

func unreadCacheKey(mid int64) string {
	return fmt.Sprintf("%sunread%s%d", cacheKeyPrefix, keyFieldSep, mid)
}

func pairVerdictCacheKey(kind string, owner, target int64) string {
	return fmt.Sprintf("%spair%s%s%s%d%s%d", cacheKeyPrefix, keyFieldSep, kind, keyFieldSep, owner, keyFieldSep, target)
}

func sendRateKey(mid int64) string {
	return fmt.Sprintf("%srate%s%d", cacheKeyPrefix, keyFieldSep, mid)
}

func strangerQuotaKey(mid int64, day string) string {
	return fmt.Sprintf("%ssq%s%d%s%s", cacheKeyPrefix, keyFieldSep, mid, keyFieldSep, day)
}

// invalidateUnreadCache 在所有影响未读投影的写路径末尾调用。
// 缓存缺失只回源，不影响正确性，因此失败只记日志（错误文本只带 mid）。
func invalidateUnreadCache(ctx context.Context, s *svc.ServiceContext, logger logx.Logger, mids ...int64) {
	if s == nil || s.Cache == nil {
		return
	}
	keys := make([]string, 0, len(mids))
	for _, m := range mids {
		if m <= 0 {
			continue
		}
		keys = append(keys, unreadCacheKey(m))
	}
	if len(keys) == 0 {
		return
	}
	if _, err := s.Cache.DelCtx(ctx, keys...); err != nil {
		logf(logger, "private-message/logic: 未读汇总缓存失效失败 mids=%v: %v", mids, err)
	}
}

func logf(logger logx.Logger, format string, args ...any) {
	if logger != nil {
		logger.Errorf(format, args...)
		return
	}
	logx.Errorf(format, args...)
}

// --- 偏好缺省 ---

// defaultSettingFor 为「从未写过偏好」的用户合成站点级缺省值（不落库：读路径不放大写）。
//
// RejectStranger 这一位由 PrivateMessage.RejectStrangerByDefault 决定，
// 因为 model.DefaultUserSetting 不知道站点级策略；默认「拒收非互关陌生人首条」时，
// 缺省行也必须是拒收态，否则未设置偏好的用户比设置了的更容易被骚扰。
func defaultSettingFor(s *svc.ServiceContext, mid int64) *model.UserSetting {
	row := model.DefaultUserSetting(mid, s.Config.PrivateMessage.DefaultAllowFrom)
	if s.Config.PrivateMessage.RejectStrangerByDefault {
		row.RejectStranger = 1
	}
	if !s.Config.PrivateMessage.KeywordFilterEnabled {
		// 只允许压测/回放环境整体关掉；线上开关状态在 KeywordFilter 列上有对应事实。
		row.KeywordFilter = 0
	}
	return row
}

// normalizeSetting 把库里的历史脏值（allow_from=0 或越界）归一到站点级默认，
// 保证对外枚举恒为明确的 ALLOW_FROM_*；就地修改，调用方持有的是本请求的行副本。
func normalizeSetting(s *svc.ServiceContext, row *model.UserSetting) {
	if row == nil || model.ValidAllowFrom(row.AllowFrom) {
		return
	}
	if def := s.Config.PrivateMessage.DefaultAllowFrom; model.ValidAllowFrom(def) {
		row.AllowFrom = def
	} else {
		row.AllowFrom = model.AllowFromAnyone
	}
}

// --- 撤回的共享事务步骤 ---

// withdrawRequest 是「把某条消息撤回」所需的全部事实，WithdrawMessage 与 HandleReport 共用。
type withdrawRequest struct {
	msg         *model.Message
	source      int32
	operator    int64
	reason      string
	auditTaskID int64
	reportID    int64
	at          int64
}

// withdrawInTx 在调用方事务内完成「改状态位 + 写审计流水 + 修未读与摘要投影」。
//
// 名字后缀 InTx 是硬约束也是授权契约：包内所有 *InTx 结尾的函数都只接收调用方的 session，
// 因此它们只能从 TransactCtx 闭包里被调用（枚举用例按这条规则做跨函数的事务判定）。
//
// 三件事必须同事务（AGENTS.md §8）：
//   - 只改状态不写流水 = 处置无证据，事后无法回答「谁在什么时候为什么撤了这条」；
//   - 只写流水不改状态 = 违规内容仍然可见；
//   - 不修投影 = 已撤回的未读消息仍挂在角标上。
//
// withdrawn=false 表示这一行已是终态或重复撤回（幂等），不写第二条流水、不报错。
func withdrawInTx(ctx context.Context, s *svc.ServiceContext, session sqlx.Session,
	req withdrawRequest) (withdrawn bool, state int32, at int64, err error) {
	if req.msg == nil {
		return false, 0, 0, model.ErrMessageNotFound
	}
	at = req.at
	if at <= 0 {
		at = timeNowUnix()
	}
	applied, err := s.Messages.MarkState(ctx, session, req.msg.MsgID,
		[]int32{model.MsgStateNormal, model.MsgStatePendingReview}, model.MsgStateWithdrawn, at, "")
	if err != nil {
		return false, req.msg.State, 0, err
	}
	if !applied {
		// 已是 WITHDRAWN/REJECTED/DELETED：重复撤回按幂等成功处理，不产生第二条审计。
		return false, req.msg.State, req.msg.WithdrawTime, nil
	}
	if err := s.WithdrawLogs.Insert(ctx, session, &model.WithdrawLog{
		MsgID:          req.msg.MsgID,
		ConversationID: req.msg.ConversationID,
		Seq:            req.msg.Seq,
		SenderMid:      req.msg.SenderMid,
		OperatorMid:    req.operator,
		Source:         req.source,
		Reason:         req.reason,
		AuditTaskID:    req.auditTaskID,
		ReportID:       req.reportID,
		Ctime:          at,
	}); err != nil {
		return false, 0, 0, err
	}
	if err := s.Members.DecrementUnreadIfUnread(ctx, session, req.msg.ConversationID, req.msg.Seq); err != nil {
		return false, 0, 0, err
	}
	if err := s.Members.RefreshPreview(ctx, session, req.msg.ConversationID, req.msg.MsgID, previewWithdrawn); err != nil {
		return false, 0, 0, err
	}
	return true, model.MsgStateWithdrawn, at, nil
}

// --- 参与者授权（每一条读与写私信数据的入口门禁） ---

// membership 是「某个 mid 对某个会话的参与者证明」：成员行（本方游标/未读/隐藏位）、
// 会话主体行（双方共享状态）与对方 mid。任何返回私信内容的方法都必须先拿到它。
//
// 这条证明是本服务唯一的授权模型（AGENTS.md §8）：没有管理员后门读，
// 运营面只读举报单与状态位（ListReports/HandleReport 不返回正文），
// 因此 pm_message 的明文只可能从「成员 + 可见性」两道门禁之后流出。
type membership struct {
	member *model.ConversationMember
	conv   *model.Conversation
	peer   int64
}

func (m *membership) conversationID() int64 { return m.conv.ConversationID }
func (m *membership) mid() int64            { return m.member.Mid }
func (m *membership) readSeq() int64        { return m.member.ReadSeq }

// notMember 是「不是成员」与「会话不存在」的统一口径。
// 两者区分会让未登录者用错误码枚举哪些 conversation_id 存在（越权探测的起点），
// 而 model 层的 Find 已经把「无行」归一成 (nil,nil)，这里不再往上抛 ErrConversationNotFound。
func notMember(conversationID, mid int64) error {
	return fmt.Errorf("%w: conversation_id=%d mid=%d", model.ErrNotConversationMember, conversationID, mid)
}

// notMemberForPair 是「按 peer_mid 发送，但这一对之间没有任何可证明的成员行」的口径。
//
// 这里刻意复用 ErrNotConversationMember 而不是 ErrSelfConversation：
// 参与者授权的唯一依据是成员行（README「参与者授权」一节），
// 「给自己发」在成员表里的表现与「陌生人没有会话」完全一样——一行都没有，
// 于是它与真正的越权尝试共用同一个错误码，调用方无法用错误码差别反推
// 「这个 mid 到底有没有会话」。ErrSelfConversation 仍归 GetOrCreateConversation 的建档语义。
func notMemberForPair(mid, peer int64) error {
	return fmt.Errorf("%w: mid=%d peer_mid=%d 没有可证明的会话成员行", model.ErrNotConversationMember, mid, peer)
}

// requireMembership 证明 mid 是 conversationID 的成员，并带回会话主体行。
// 只读，不写：授权判定不能顺手改数据（否则越权探测本身就成了写入路径）。
func requireMembership(ctx context.Context, s *svc.ServiceContext, conversationID, mid int64) (*membership, error) {
	if err := checkConversationID(conversationID); err != nil {
		return nil, err
	}
	if err := checkMid(mid); err != nil {
		return nil, err
	}
	mem, err := s.Members.Find(ctx, conversationID, mid)
	if err != nil {
		return nil, err
	}
	if mem == nil {
		return nil, notMember(conversationID, mid)
	}
	conv, err := s.Conversations.FindByID(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	if conv == nil {
		// 成员行在、会话主体不在：投影脏数据。按「不是成员」处理，不外露内部不一致。
		return nil, notMember(conversationID, mid)
	}
	return &membership{member: mem, conv: conv, peer: peerOf(conv, mid, mem.PeerMid)}, nil
}

// membershipTarget 对「已定位到的会话」做一次成员证明，回会话主体与发送方的「对方」mid。
//
// 存在的唯一理由：发送入口有两条定位路径（conversation_id 主键、pair_key 唯一键），
// 定位方式不同不等于授权门槛不同。两条路径必须过同一份证明，
// 否则「只报 peer_mid」就是绕过成员表的第二条发送路径；
// 而 pair_key 命中既有会话时，接收方也恒由会话主体决定（不接受调用方自报）。
func membershipTarget(ctx context.Context, s *svc.ServiceContext, conversationID, sender int64) (
	*model.Conversation, int64, error) {
	ms, err := requireMembership(ctx, s, conversationID, sender)
	if err != nil {
		return nil, 0, err
	}
	if ms.peer == sender {
		// 会话主体与成员投影自相矛盾（「对方」指回自己）：这种行没有业务写路径能产出
		// （建档按 min/max 写两个不同 mid），按它发送等于给自己投一条「已发送」的假消息，
		// 因此按非成员拒，一行都不写。
		return nil, 0, notMember(conversationID, sender)
	}
	return ms.conv, ms.peer, nil
}

// peerOf 取会话中「对方」mid：以会话主体的 user_a/user_b 为准（那是建档时归一化的事实），
// 成员行的 peer_mid 只是投影，两者不一致时以事实为准并保留投影值兜底。
func peerOf(conv *model.Conversation, mid, fallback int64) int64 {
	if conv != nil {
		if conv.UserA == mid && conv.UserB > 0 {
			return conv.UserB
		}
		if conv.UserB == mid && conv.UserA > 0 {
			return conv.UserA
		}
	}
	if fallback > 0 {
		return fallback
	}
	return mid
}

// requireMessageAccess 定位消息并证明 caller 是该消息所属会话的成员。
// 举报、撤回、单条读取都走这里：拿到消息 ID 不等于能读这条消息。
//
// 消息不存在与「不是成员」对外口径不同（ErrMessageNotFound / ErrNotConversationMember），
// 但两者都在任何解密之前返回，越权者因此读不到正文，也无法用错误码差别反推会话内容。
func requireMessageAccess(ctx context.Context, s *svc.ServiceContext, msgID, caller int64) (*model.Message, *membership, error) {
	if err := checkMsgID(msgID); err != nil {
		return nil, nil, err
	}
	msg, err := s.Messages.FindByID(ctx, msgID)
	if err != nil {
		return nil, nil, err
	}
	if msg == nil {
		return nil, nil, model.ErrMessageNotFound
	}
	ms, err := requireMembership(ctx, s, msg.ConversationID, caller)
	if err != nil {
		return nil, nil, err
	}
	return msg, ms, nil
}

// --- 正文解密（只服务于已过授权门禁的读取路径） ---

// decryptContent 还原消息明文。密钥未注入返回 ErrCipherKeyMissing（禁止退化成把密文原样回传），
// 密文形态异常统一成 ErrDecryptFailed，错误文本只带 msg_id 不带内容。
func decryptContent(s *svc.ServiceContext, msg *model.Message) (string, error) {
	mc, err := newMessageCipher(s.Config)
	if err != nil {
		return "", err
	}
	plain, err := mc.decrypt(msg.ContentCipher)
	if err != nil {
		if errors.Is(err, ErrCipherBlobTruncated) {
			return "", fmt.Errorf("%w: msg_id=%d 密文长度异常", model.ErrDecryptFailed, msg.MsgID)
		}
		return "", fmt.Errorf("%w: msg_id=%d", err, msg.MsgID)
	}
	return plain, nil
}

// projectMessage 把一行消息投影成对外结构：先判可见性，再决定「占位文案 or 解密」。
// 返回 nil 表示这一行对该查看者不存在（PENDING_REVIEW 的非发送者），由调用方丢弃。
func projectMessage(s *svc.ServiceContext, msg *model.Message, viewer int64) (*rpc.MessageInfo, error) {
	if !isVisibleTo(msg, viewer) {
		return nil, nil
	}
	if text, replaced := contentPlaceholder(msg); replaced {
		// 不可见正文的行同时隐去 media_ref：留着 asset 引用等于绕过撤回继续拉媒体。
		return messageInfo(msg, text, true), nil
	}
	plain, err := decryptContent(s, msg)
	if err != nil {
		return nil, err
	}
	return messageInfo(msg, plain, false), nil
}

// --- 偏好读取（缺行即站点级缺省） ---

// loadSettingOrDefault 读取用户偏好；缺行时合成站点级缺省值而不落库
// （读路径放大写会让 pm_user_setting 被未设置用户填满，也让「从未设置」与「设置成默认」失去区分）。
func loadSettingOrDefault(ctx context.Context, s *svc.ServiceContext, mid int64) (*model.UserSetting, error) {
	row, err := s.Settings.FindByMid(ctx, mid)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return defaultSettingFor(s, mid), nil
	}
	return row, nil
}

// --- 限额用的时间位点 ---

// epochDay 返回 UTC 日序号（陌生人会话日配额的计数桶）。
// 按 epoch 天数而不是日期字符串算，避免时区/格式化差异让配额在跨日抖动中被重置。
func epochDay(now int64) int64 {
	if now <= 0 {
		now = timeNowUnix()
	}
	return now / secondsPerDay
}

const secondsPerDay = 86400

// lowHigh 返回升序的两个 mid：pair_key 与 user_a/user_b 的写入顺序必须由同一规则决定。
func lowHigh(a, b int64) (int64, int64) {
	if a > b {
		return b, a
	}
	return a, b
}

func cacheGetJSON(ctx context.Context, s *svc.ServiceContext, key string, into any) bool {
	if s == nil || s.Cache == nil || key == "" || into == nil {
		return false
	}
	raw, err := s.Cache.GetCtx(ctx, key)
	if err != nil || raw == "" {
		return false
	}
	if err := json.Unmarshal([]byte(raw), into); err != nil {
		logx.Errorf("private-message/logic: 缓存 %s 反序列化失败: %v", key, err)
		return false
	}
	return true
}

func cacheSetJSON(ctx context.Context, s *svc.ServiceContext, key string, val any, ttlSeconds int) {
	if s == nil || s.Cache == nil || key == "" || ttlSeconds <= 0 {
		return
	}
	raw, err := json.Marshal(val)
	if err != nil {
		return
	}
	if err := s.Cache.SetexCtx(ctx, key, string(raw), ttlSeconds); err != nil {
		logx.Errorf("private-message/logic: 写缓存 %s 失败: %v", key, err)
	}
}

// incrCounter 计数并首次写入时设 TTL；返回累计值。
//
// 失败时返回 error，调用方必须 fail-closed：限额算不出来就等于「没有证明没超限」，
// 把「拿不到计数」当成「没超限」放行会让频控与陌生人配额在 Redis 故障期间完全失效。
func incrCounter(ctx context.Context, s *svc.ServiceContext, key string, ttlSeconds int) (int64, error) {
	if s.Cache == nil {
		return 0, errors.New("private-message: cache unavailable for rate counter")
	}
	n, err := s.Cache.IncrCtx(ctx, key)
	if err != nil {
		return 0, err
	}
	if n == 1 && ttlSeconds > 0 {
		if err := s.Cache.ExpireCtx(ctx, key, ttlSeconds); err != nil {
			// 没设上 TTL 的计数器会永久累加，最终把用户永久锁死；
			// 这里删键重来比留着错值安全（宁可少计一次，也不永久拒）。
			logx.Errorf("private-message/logic: 计数器 %s 设置 TTL 失败: %v", key, err)
			if _, delErr := s.Cache.DelCtx(ctx, key); delErr != nil {
				return 0, delErr
			}
			return 0, errors.New("private-message: rate counter ttl unavailable")
		}
	}
	return n, nil
}
