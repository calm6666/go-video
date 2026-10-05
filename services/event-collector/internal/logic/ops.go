// 本文件是 logic 包的手写运营动作公共件（轮次幂等、错误文本脱敏、死信原因归类、
// topic 入参校验），不是 goctl 生成产物。
//
// 面向的方法都是「运营/兜底」入口（策略切换、死信重放、投递推进）：它们不采集事件，
// 但每一次写入都必须回答「谁、凭哪次请求、为什么」（AGENTS.md §5 §8）。
// 铁律（AGENTS.md §7）：本文件对外输出的任何字符串（last_error / reason_detail / 日志）
// 都必须先过 redactSensitive —— 下游 MQ/MySQL 的错误文本里可能带 broker 地址、
// 连接串里的账号甚至被上游拼进来的设备号，原文入库等于开一个隐私漏口。

package logic

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"go-video/services/event-collector/internal/svc"
	"go-video/services/event-collector/model"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// 需要轮次幂等的 RPC 名（rpcRetryPendingDelivery 已在 helpers.go 声明）。
const (
	rpcUpsertDispatchPolicy   = "UpsertDispatchPolicy"
	rpcActivateDispatchPolicy = "ActivateDispatchPolicy"
	rpcReplayDeadLetter       = "ReplayDeadLetter"
)

// maxPolicyListItems 策略里 JSON 数组列的条目上限。
// 没有这一条，一次「误粘 10 万行 drop_fields」的提交就会把 TEXT 列写成几十 MB，
// 之后每条上报都要重新解析它（采集路径被一次配置事故拖死）。
const maxPolicyListItems = 500

// withRoundDedup 给「运营写动作」套一层按 idempotency_key 的轮次幂等：
// 首次执行跑 run 并回填结论；同一 key 的重复提交直接回放首次结论。
//
// 为什么需要它（DB 幂等兜不住的那一段）：策略 Upsert 第二次跑会走 UpdateDraft，
// 回带 created=false —— 与首次的 created=true 相反，调用方据此判断「我没建成」，
// 于是又开始新建版本。缓存只是加速与还原首次结论，真值仍在 MySQL：
// Redis 不可用时 claimOpDedup 返回 first=true，唯一键/CAS 继续兜住重复副作用。
func withRoundDedup[T any](ctx context.Context, s *svc.ServiceContext, rpcName, key string,
	logger logx.Logger, fresh func() *T, run func() (*T, error)) (*T, error) {

	first, raw, err := claimOpDedup(ctx, s, rpcName, key)
	if err != nil {
		return nil, err
	}
	if !first {
		if strings.TrimSpace(raw) == "" {
			return nil, fmt.Errorf("%w: %s idempotency_key 首次结论尚未产出", model.ErrConcurrentUpdate, rpcName)
		}
		out := fresh()
		if err := json.Unmarshal([]byte(raw), out); err != nil {
			return nil, fmt.Errorf("%w: 回放 %s 首次结论失败: %v", model.ErrConcurrentUpdate, rpcName, err)
		}
		return out, nil
	}
	out, rerr := run()
	if rerr != nil {
		// 失败必须交还执行权：否则同一 key 在 TTL 内只会拿到「still running」，
		// 调用方连「按同一键安全重试」都做不到。
		releaseOpDedup(ctx, s, rpcName, key, logger)
		return nil, rerr
	}
	if msg, ok := any(out).(proto.Message); ok {
		saveOpDedupResult(ctx, s, rpcName, key, msg, logger)
	}
	return out, nil
}

// releaseOpDedup 交还某个 idempotency_key 的执行权（首次执行失败时调用）。
//
// 前提：失败路径上的写入都已回滚，重试不会叠加副作用。删除失败只记日志——
// 轮次标记自带 TTL，最多让调用方在窗口内重试拿到一次 ErrConcurrentUpdate，
// 不能因为「清理没成功」再对外报错掩盖真正的失败原因。
func releaseOpDedup(ctx context.Context, s *svc.ServiceContext, rpcName, key string, logger logx.Logger) {
	if s.Cache == nil || rpcName == "" || key == "" {
		return
	}
	if _, err := s.Cache.DelCtx(ctx, opDedupKeyPrefix+rpcName+":"+key); err != nil {
		msg := fmt.Sprintf("event-collector/logic: 交还 %s 幂等键失败（TTL %ds 后自愈）: %v",
			rpcName, opDedupTTLSecs, err)
		if logger != nil {
			logger.Error(msg)
			return
		}
		logx.Error(msg)
	}
}

// --- 外发文本收敛 ---

var (
	// kvPattern 匹配 token=xxx / "password": "xxx" 这类键值对（键名覆盖内置底线字段）。
	kvPattern = regexp.MustCompile(`(?i)\b(access_token|refresh_token|session_token|auth_token|token|` +
		`authorization|cookie|password|passwd|pwd|sms_code|secret|salt|api_key|imei|idfa|oaid|caid|` +
		`android_id|device_id|deviceid|serial|mac_address|mac|phone|mobile|telephone|tel|email|` +
		`id_card|idcard|id_no|identity_no|passport|real_name|nickname)\b(\s*[=:]\s*)("[^"]*"|'[^']*'|[^\s,;}\)]+)`)
	// ipv4Pattern 明文出口 IPv4（错误文本里最常见的敏感值）。
	ipv4Pattern = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
	// ipv6Pattern 至少 4 段冒号分组的 IPv6 字面量（不误伤 "1:2" 这类端口写法）。
	ipv6Pattern = regexp.MustCompile(`\b(?:[0-9a-fA-F]{1,4}:){3,7}[0-9a-fA-F]{1,4}\b`)
	// longRunPattern 长随机串：设备号/签名/ULID 都可能落在错误文本里，一律收敛。
	longRunPattern = regexp.MustCompile(`\b[0-9a-zA-Z_+/=]{32,}\b`)
)

// redactSensitive 把任意第三方错误文本压成「可入库、可外发」的排障摘要。
//
// 顺序：键值对 → IPv4 → IPv6 → 长随机串，最后按字节收敛到列宽。
// 这里只做「不把可疑值带出去」，不做可逆封装：需要原值时看 trace_id 对应的
// 结构化日志（那一层由运维权限控制），台账和 RPC 响应里永远只有摘要。
func redactSensitive(s string) string {
	if s == "" {
		return ""
	}
	out := kvPattern.ReplaceAllStringFunc(s, func(m string) string {
		parts := kvPattern.FindStringSubmatch(m)
		if len(parts) < 3 {
			return "[redacted]"
		}
		return parts[1] + parts[2] + "[redacted]"
	})
	out = ipv4Pattern.ReplaceAllString(out, "[ipv4]")
	out = ipv6Pattern.ReplaceAllString(out, "[ipv6]")
	out = longRunPattern.ReplaceAllString(out, "[id]")
	return fitColumn(strings.TrimSpace(out), maxReasonBytes)
}

// errorText 统一的错误摘要口径：nil 归空串，非 nil 先脱敏再收敛。
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return redactSensitive(err.Error())
}

// --- 死信原因归类 ---

// 死信 reason 是稳定枚举串（ec_dead_letter.reason 注释）：运营按它筛选，
// 绝不能写成一次性拼的人读文案，否则「最近 mq_timeout 涨了」这种问题永远统计不出来。
const (
	deadReasonMQTimeout      = "mq_timeout"
	deadReasonMQAuth         = "mq_auth"
	deadReasonMQUnreachable  = "mq_unreachable"
	deadReasonMQPayloadBig   = "mq_payload_oversize"
	deadReasonMQCanceled     = "mq_canceled"
	deadReasonDispatcherGone = "dispatcher_missing"
	deadReasonInternal       = "delivery_internal"
)

// classifyDeliveryError 把投递失败归到稳定枚举（未知一律 delivery_internal，
// 不返回错误原文：reason 列只有 64 字节，且原文里可能带敏感值）。
func classifyDeliveryError(err error) string {
	if err == nil {
		return deadReasonInternal
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "not configured"), strings.Contains(msg, "not wired"),
		strings.Contains(msg, "dispatcher"):
		return deadReasonDispatcherGone
	case strings.Contains(msg, "too large"), strings.Contains(msg, "oversize"),
		strings.Contains(msg, "message size"), strings.Contains(msg, "payload too"):
		return deadReasonMQPayloadBig
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline"),
		strings.Contains(msg, "i/o time"):
		return deadReasonMQTimeout
	case strings.Contains(msg, "auth"), strings.Contains(msg, "sasl"),
		strings.Contains(msg, "forbidden"), strings.Contains(msg, "unauthorized"),
		strings.Contains(msg, "permission"):
		return deadReasonMQAuth
	case strings.Contains(msg, "canceled"), strings.Contains(msg, "cancelled"):
		return deadReasonMQCanceled
	case strings.Contains(msg, "connection refused"), strings.Contains(msg, "no such host"),
		strings.Contains(msg, "unreachable"), strings.Contains(msg, "connect"),
		strings.Contains(msg, "eof"):
		return deadReasonMQUnreachable
	default:
		return deadReasonInternal
	}
}

// --- 入参形状校验 ---

// validTopicFilter 判定调用方给的 topic 过滤条件形状合法。
// topic 会进 SQL 参数（不是拼接），这里限形状主要挡住「把一整段人读文案当 topic」
// 造成永远查不到结果却看不出原因的排障陷阱。
func validTopicFilter(s string) bool {
	if s == "" || len(s) > maxTopicBytes {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// validSaltRef 判定 salt_ref 是「环境变量名」而不是盐值本身。
//
// 这是隐私底线：一旦有人把盐直接填进 salt_ref 列，盐值就随策略行入库、
// 随 RPC 响应外发（policyToRPC 会回带 salt_ref），轮换与审计同时失效。
func validSaltRef(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > maxSaltRefBytes {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		case r >= 'a' && r <= 'z' && i > 0:
			// 允许小写（部分部署用低峰命名），但首字符必须是字母/下划线。
		default:
			return false
		}
	}
	return !strings.Contains(s, "=") && !strings.ContainsAny(s, "$ \t\r\n")
}

// validPolicyVersion 判定策略版本号形状（形如 2026.09.20-1）。
// 版本串会写进每条台账做归因，也用于客户端缓存比对，禁止空白与路径分隔符。
func validPolicyVersion(s string) bool {
	if s == "" || len(s) > maxVersionTagBytes {
		return false
	}
	if strings.ContainsAny(s, " \t\r\n/\\%,;'\"") {
		return false
	}
	if s[0] == '.' || s[0] == '-' || s[len(s)-1] == '.' || s[len(s)-1] == '-' {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// validOperatorName 判定操作人标识可用（进审计列，<=64 且不含换行/空白分隔符）。
func validOperatorName(s string) bool {
	if s == "" || len(s) > maxOperatorBytes || strings.ContainsAny(s, " \t\r\n") {
		return false
	}
	return true
}

// clampListLen 校验字符串列表条目数与单项长度，返回归一化后的列表（去空白、原样保留大小写）。
func clampListItems(name string, list []string) ([]string, error) {
	if len(list) > maxPolicyListItems {
		return nil, fmt.Errorf("%w: %s 条目数 %d 超过上限 %d", model.ErrBatchLimitTooLarge, name,
			len(list), maxPolicyListItems)
	}
	out := make([]string, 0, len(list))
	seen := make(map[string]struct{}, len(list))
	for _, item := range list {
		s := strings.TrimSpace(item)
		if s == "" {
			continue
		}
		if len(s) > maxDropFieldItemBytes {
			return nil, fmt.Errorf("event-collector: %s 单项长度 %d 超过 %d 字节", name,
				len(s), maxDropFieldItemBytes)
		}
		key := normalizeFieldName(s)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, s)
	}
	return out, nil
}
