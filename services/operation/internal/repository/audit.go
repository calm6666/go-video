package repository

// 本文件负责管理操作审计**索引**的写入与查询。
//
// 与 services/audit 的边界（该服务本期未实现，见 README）：
//   - operation 只落“谁在何时对哪个聚合做了什么、结果如何”的索引 + trace_id；
//   - 请求正文、前后快照、审核证据由被操作的领域服务保留，
//     需要长期不可抵赖存证时由 audit 服务以 append-only 方式承接；
//   - operation 通过 trace_id / request_id 与两者关联，不复制大字段。
//
// 脱敏要求（AGENTS.md §7、docs/data-design.md §6）：审计行不写口令、token、
// 明文 IP；IP 只存不可逆短哈希，UA 先剥掉凭证片段再截断存储（见 redactUserAgent）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/operation/model"
)

// ipHashSalt 是 IP 哈希的前缀盐（固定常量，不构成密钥，只防彩虹表反查）。
const ipHashSalt = "go-video:operation:ip:"

// maxUserAgentLen 与 op_audit_index.user_agent 列宽一致。
const maxUserAgentLen = 255

// 审计动作标识（op_audit_index.action），命名统一为 <对象>.<动作>。
const (
	actionAdminLogin        = "admin.login"
	actionAdminLogout       = "admin.session_revoke"
	actionAdminUserCreate   = "admin_user.create"
	actionAdminUserUpdate   = "admin_user.update"
	actionAdminUserDisable  = "admin_user.disable"
	actionAdminUserResetPwd = "admin_user.reset_password"
	actionRoleCreate        = "role.create"
	actionRoleDelete        = "role.delete"
	actionRoleGrant         = "role.grant_permissions"
	actionAdminAssignRoles  = "admin_user.assign_roles"
	actionPermissionCreate  = "permission.create"
	actionMenuSave          = "menu.save"
	actionConfigSave        = "ops_config.save"
	actionTaskSubmit        = "admin_task.submit"
	actionTaskCancel        = "admin_task.cancel"
	actionTaskRun           = "admin_task.run"
)

// Actor 是一次管理操作的调用者上下文（由 logic 从 OpContext 转换而来）。
type Actor struct {
	// AdminID 操作管理员 ID。
	AdminID int64
	// Username 操作管理员用户名（冗余，便于日志阅读）。
	Username string
	// IP 来源 IP（只在内存中存在，落库前哈希）。
	IP string
	// UserAgent 客户端 UA（截断后落库）。
	UserAgent string
	// TraceID 链路 ID。
	TraceID string
	// RequestID 幂等/请求 ID。
	RequestID string
}

// ipHash 返回来源 IP 的不可逆短哈希（32 个 hex 字符）；空 IP 返回空串。
func ipHash(ip string) string {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(ipHashSalt + ip))
	return hex.EncodeToString(sum[:])[:32]
}

// truncate 按 rune 截断，避免多字节字符被切出半个字。
func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// uaSecretPattern 命中即丢弃后半段：正常浏览器/客户端 UA 里不会出现这些键名，
// 出现说明调用方把凭证塞进了 User-Agent（调试工具或改造过的客户端）。
// 审计表只记“谁做了什么”，不能变成凭证的第二份副本。
var uaSecretPattern = regexp.MustCompile(`(?i)(token|passwd|password|pwd|secret|authorization|bearer|cookie|session)[=:\s]`)

// uaRedactedMark 是被脱敏处的可见标记，保留“此处被截断”的线索给排障。
const uaRedactedMark = " [redacted]"

// redactUserAgent 先剥掉凭证片段，再按列宽截断（顺序不能反：截断后再匹配会漏掉边界）。
func redactUserAgent(ua string) string {
	if loc := uaSecretPattern.FindStringIndex(ua); loc != nil {
		return truncate(strings.TrimRight(ua[:loc[0]], " \t")+uaRedactedMark, maxUserAgentLen)
	}
	return truncate(ua, maxUserAgentLen)
}

// newAuditIndex 构造审计索引行：集中做脱敏，避免各调用点漏处理。
// 注意：入参里出现的口令、token 一律不进入返回结构。
func newAuditIndex(actor Actor, action, resourceType, resourceID, result string) *model.AuditIndex {
	return &model.AuditIndex{
		AdminID:      actor.AdminID,
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		Result:       result,
		IPHash:       ipHash(actor.IP),
		UserAgent:    redactUserAgent(actor.UserAgent),
		TraceID:      truncate(actor.TraceID, 64),
		RequestID:    truncate(actor.RequestID, 64),
		Ctime:        nowUnix(),
	}
}

// writeAudit 追加审计索引。审计写失败不改变已完成的业务结果（否则无法回滚下游动作），
// 但必须以 error 级别落日志，供运维按 trace_id 补偿；这是 AGENTS.md §10 记录的风险点。
func (r *Repository) writeAudit(ctx context.Context, actor Actor, action, resourceType, resourceID, result string) error {
	row := newAuditIndex(actor, action, resourceType, resourceID, result)
	if _, err := r.auditMd.Insert(ctx, row); err != nil {
		logx.Errorf("operation/audit: insert index action=%s admin=%d resource=%s/%s trace=%s err=%v",
			action, actor.AdminID, resourceType, resourceID, actor.TraceID, err)
		return fmt.Errorf("operation: write audit index failed: %w", err)
	}
	return nil
}

// AuditQuery 是审计索引的查询条件（logic 直接把 RPC 入参转成它）。
type AuditQuery struct {
	AdminID      int64
	Action       string
	ResourceType string
	ResourceID   string
	StartAt      int64
	EndAt        int64
	Pn           int32
	Ps           int32
}

// ListAuditIndex 分页查询审计索引，时间倒序。
func (r *Repository) ListAuditIndex(ctx context.Context, q AuditQuery) ([]*model.AuditIndex, int64, error) {
	if q.Pn < 1 {
		q.Pn = 1
	}
	if q.Ps < 1 {
		q.Ps = 20
	}
	if q.Ps > 100 {
		q.Ps = 100
	}
	if q.StartAt > 0 && q.EndAt > 0 && q.EndAt <= q.StartAt {
		return nil, 0, fmt.Errorf("operation: end_at must be greater than start_at")
	}
	return r.auditMd.List(ctx, model.AuditFilter{
		AdminID:      q.AdminID,
		Action:       q.Action,
		ResourceType: q.ResourceType,
		ResourceID:   q.ResourceID,
		StartAt:      q.StartAt,
		EndAt:        q.EndAt,
		Pn:           q.Pn,
		Ps:           q.Ps,
	})
}
