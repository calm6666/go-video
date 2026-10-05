package logic

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
)

// 本文件是 6 个 Webhook 方法共用的私有小工具（端点归属判定、日志用的地址脱敏、
// 事件正文的 PII 扫描）。
//
// 为什么这些判定必须在 logic 层而不是只靠 model：
//   - model 的 SQL 条件已经保证「未验证不投递」（ListMatching 过滤 verified_at/enabled/
//     deleted_at），但它不知道「谁在调这个接口」；归属与身份只有 logic 能裁决；
//   - 回调地址的完整串可能把 token 塞进查询参数，因此日志只允许出现 host
//     （AGENTS.md §7 隐私最小化的同一口径）；
//   - 投递正文会落 op_webhook_delivery.payload 并按 WebhookPayloadRetentionDays 清理，
//     但它仍然是「凭证与个人信息的第二存储地」，所以入队前的扫描与凭证门禁
//     （webhookevent.normalizeEventPayload）必须成对存在，缺一不可。

// errWebhookBizRefTooLong biz_type/biz_id 超出可审计引用的长度。
// 拒绝而不是截断：与 errReasonTooLong 同一口径（问责文案不能在最后一环失真）。
var errWebhookBizRefTooLong = errors.New("open-platform: webhook biz reference too long")

// errWebhookMidInvalid 事件的关联用户为负数。0 是「无归属用户」（平台级广播），
// 负数只能是脏参数；model 错误集里没有 mid 相关哨兵，本域也不该新增跨服务错误码。
var errWebhookMidInvalid = errors.New("open-platform: invalid related mid")

// errWebhookEndpointDisabled 端点存在、已验证，但被置为暂停投递（enabled=0）。
// 不复用 ErrWebhookUnverified：「没验证过」与「被主动关掉」在排障上是两件事，
// 前者要引导应用去验证，后者要问运营为什么停了。
var errWebhookEndpointDisabled = errors.New("open-platform: webhook endpoint is disabled")

// errWebhookPayloadCarriesPII 事件正文携带手机号/身份证明文。
// 与 errPayloadCarriesCredential 并列：前者挡「凭证外流」，后者挡「个人信息外流」，
// 两者都是「正文会被投递到公网端点并落库到保留期」这条链路上的一次性闸门。
var errWebhookPayloadCarriesPII = errors.New("open-platform: webhook payload must not carry personal data")

// webhookURLHostForLog 只取回调地址的 host:port 供日志使用。
// 解析失败回固定串而不是原样输出——非法地址本身就是攻击面，不能把原文写进日志。
func webhookURLHostForLog(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "unparsable"
	}
	return strings.ToLower(u.Host)
}

// webhookEndpointOfApp 读取端点并确认它属于该应用。
//
// 不存在、属于别的应用一律同一个 ErrWebhookNotFound：跨应用探测与「不存在」同口径，
// 不给「猜别人的 endpoint_id」留下可分辨信号。
// 注意本函数**不**排除软删行，这是两处契约的前提：
//   - DeleteWebhook 的重复删除要回幂等成功（proto:680「抑制未投递任务」+ 占位注释第 4 条）；
//   - ListWebhookDeliveries 要能列出已删端点的投递记录，否则死信复盘丢归属（占位注释第 5 条）。
func webhookEndpointOfApp(ctx context.Context, s *svc.ServiceContext,
	appID, endpointID int64) (*model.WebhookEndpoint, error) {
	if endpointID <= 0 {
		return nil, model.ErrWebhookNotFound
	}
	ep, err := s.WebhookEndpoints.FindByID(ctx, endpointID)
	if err != nil {
		return nil, err
	}
	if ep == nil || ep.AppID != appID {
		return nil, model.ErrWebhookNotFound
	}
	return ep, nil
}

// 手机号与证件号的识别模式。
//
// 刻意不做「整篇正文子串匹配」：本服务的事件正文携带视频标题等自由文本，
// 子串匹配会把 "iPhone" 里的 "phone"、把 11 位内容 ID 里恰好 1[3-9] 开头的数字
// 判成 PII，误杀等于拒绝合法投递（内容事件因此静默丢失比漏一条脱敏更糟）。
// 规则分三档，从「无歧义」到「需要键上下文」：
//  1. 18 位身份证形态（17 位数字 + 校验位）在任何字符串值上都不可能是内容 ID，直接命中；
//  2. 带 +86 前缀或分隔符的手机号写法（138-0013-8000 / +8613800138000）不是任何 ID 的形态，直接命中；
//  3. 裸 11 位数字既可能是手机号也可能是 mid/aid，只在键名本身是联系方式类字段时命中。
var (
	cnIDCardRe     = regexp.MustCompile(`^[0-9]{17}[0-9Xx]$`)
	cnPhoneFormRe  = regexp.MustCompile(`^(?:\+?86[-\s.]?)?1[3-9][0-9][-\s.][0-9]{4}[-\s.][0-9]{4}$`)
	cnPhoneBareRe  = regexp.MustCompile(`^1[3-9][0-9]{9}$`)
	piiKeyMarkers  = []string{"phone", "mobile", "telephone", "contact", "idcard", "id_card", "证件", "手机", "联系"}
	piiKeyExact    = []string{"tel", "phone", "mobile"}
	piiScanMaxNode = 512 // 递归节点上限：畸形深挂的正文不是 PII 扫描要伺候的对象
)

// payloadCarriesPII 扫描**已确认为合法 JSON** 的事件正文是否携带手机号/身份证明文。
//
// 调用方必须先过 normalizeEventPayload（JSON 合法性 + 字节上限 + 凭证键名），
// 解析失败一律回 false：本函数不是正文合法性门禁的第二道判据，
// 拿一个非法 JSON 报 PII 错误会把真实原因（格式错）掩盖掉。
func payloadCarriesPII(payload string) bool {
	var doc any
	if err := json.Unmarshal([]byte(payload), &doc); err != nil {
		return false
	}
	return nodeHasPII(doc, "", 0)
}

// nodeHasPII 递归遍历解码后的 JSON 树，parentKey 提供裸数字的字段上下文。
func nodeHasPII(v any, parentKey string, depth int) bool {
	if depth > piiScanMaxNode {
		return false
	}
	switch node := v.(type) {
	case string:
		return valueLooksPII(node, parentKey)
	case map[string]any:
		for key, child := range node {
			if nodeHasPII(child, strings.ToLower(key), depth+1) {
				return true
			}
		}
	case []any:
		for _, child := range node {
			if nodeHasPII(child, parentKey, depth+1) {
				return true
			}
		}
	}
	return false
}

// valueLooksPII 按上面三档规则判断单个字符串值。
func valueLooksPII(v, parentKey string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	if cnIDCardRe.MatchString(v) || cnPhoneFormRe.MatchString(v) {
		return true
	}
	return cnPhoneBareRe.MatchString(v) && keyLooksContact(parentKey)
}

// keyLooksContact 判断字段名是否是联系方式/证件类字段。
func keyLooksContact(key string) bool {
	if key == "" {
		return false
	}
	for _, exact := range piiKeyExact {
		if key == exact {
			return true
		}
	}
	for _, marker := range piiKeyMarkers {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return false
}
