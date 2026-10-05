// 本文件是 gateway/admin 的手写投影扩展（非 goctl 生成产物）。
//
// operation RPC → 管理后台投影 + 操作者上下文装配 + 分页/规模口径对齐。
//
// 网关在这里只做三件事（AGENTS.md §4/§5）：
//  1. 组装 OpContext：审计主体以 AdminPermission 中间件解析出的会话 admin_id 为准，
//     客户端声明的 operator_id 只在没有中间件参与时兜底，二者不一致时按 Error 留痕；
//  2. 卡门槛：写接口必须带 request_id（proto 注释「写接口必填」）、任务步骤不超过
//     服务端上限、分页口径与 operation 的 pagePair 完全一致；
//  3. 投影：把 RPC 消息搬成后台 types，不做任何业务判定（角色是否可删、口令强度、
//     配置值类型、任务类型合法性全部由 operation 决定）。

package logic

import (
	"context"

	"go-video/common/validation"
	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/types"
	operationrpc "go-video/services/operation/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

const (
	// operationMaxPageSize 与 services/operation/internal/repository.maxPageSize 一致。
	operationMaxPageSize = 100
	// operationDefaultPageSize 对应服务侧 defaultPageSize（ps<=0 时回落 20）。
	operationDefaultPageSize = 20
	// operationMaxTaskSteps 对应服务侧 maxTaskSteps：单个管理任务的步骤上限。
	// 网关先挡一次，避免一条 10 万步的提交把下游 RPC 打爆（服务端仍会二次拒绝）。
	operationMaxTaskSteps = 1000
)

// normalizeOperationPage 复用 common/validation.NormalizePage，参数与 operation 的
// pagePair 同口径（pn<=0→1、ps<=0→20、ps>100→100），两边不会漂移。
func normalizeOperationPage(pn, ps int32) (int32, int32) {
	page := validation.NormalizePage(int(pn), int(ps), operationMaxPageSize)
	return int32(page.Page), int32(page.PageSize)
}

// operationOpContext 装配 RPC 的 OpContext。
//
// write=true 用于所有写接口：request_id 必填（operation 用它做幂等与审计关联），
// 读接口不强制（列表查询重复执行无副作用）。
// operator_id 取自 AdminPermission 中间件写入 context 的会话身份，
// 因此后台表单即使伪造 op.operator_id 也无法把审计写进别人的名下。
func operationOpContext(ctx context.Context, op types.AdminOpContext, write bool) (*operationrpc.OpContext, error) {
	adminID := op.OperatorId
	if id, ok := middleware.AdminFromContext(ctx); ok {
		if op.OperatorId > 0 && op.OperatorId != id.AdminID {
			// 客户端声明的审计主体与会话身份不一致：以会话为准并留痕，
			// 便于按 trace_id 追后台表单或脚本的越权尝试。
			logx.WithContext(ctx).Errorf("gateway/admin/operation: operator_id mismatch session=%d claimed=%d",
				id.AdminID, op.OperatorId)
		}
		adminID = id.AdminID
	}
	if err := requireOperatorID(adminID); err != nil {
		return nil, err
	}
	if write {
		// 错误消息带 op. 前缀，和请求体里的嵌套字段名一致，便于后台表单定位。
		if err := requireNonEmpty("op.request_id", op.RequestId); err != nil {
			return nil, err
		}
	}
	// 契约缺口：operation.v1.VerifyAdminPermissionReply 只回传 admin_id 与命中角色，
	// 没有 username，网关无法用会话身份覆盖 op.operator_name；
	// 该字段会原样进入 op_audit_index.username（服务侧注释「冗余，便于列表展示」），
	// 因此审计的用户名只能按客户端声明看待，权威主体是 operator_id。
	return &operationrpc.OpContext{
		OperatorId:   adminID,
		OperatorName: op.OperatorName,
		Ip:           op.Ip,
		UserAgent:    op.UserAgent,
		TraceId:      op.TraceId,
		RequestId:    op.RequestId,
	}, nil
}

// operationSteps 把请求体的步骤声明逐条投影成 RPC 消息，字段原样透传：
// target_type/target_id 的合法性（是否属于该任务类型、是否存在）由 operation 判定。
func operationSteps(specs []types.OperationTaskStepSpec) []*operationrpc.TaskStepSpec {
	out := make([]*operationrpc.TaskStepSpec, 0, len(specs))
	for _, s := range specs {
		out = append(out, &operationrpc.TaskStepSpec{
			TargetType: s.TargetType,
			TargetId:   s.TargetId,
		})
	}
	return out
}

// ==================== 投影 ====================

// adminUserToAPI 投影管理员账号（下游已剔除口令散列与二次校验目标明文，
// 这里只多一个 second_factor_enabled 布尔）。
func adminUserToAPI(u *operationrpc.AdminUserItem) types.OperationAdminUserItem {
	return types.OperationAdminUserItem{
		AdminId:             u.GetAdminId(),
		Username:            u.GetUsername(),
		State:               u.GetState(),
		Remark:              u.GetRemark(),
		OperatorId:          u.GetOperatorId(),
		LastLoginAt:         u.GetLastLoginAt(),
		Ctime:               u.GetCtime(),
		Mtime:               u.GetMtime(),
		RoleIds:             u.GetRoleIds(),
		RoleNames:           u.GetRoleNames(),
		SecondFactorEnabled: u.GetSecondFactorEnabled(),
	}
}

func adminUsersToAPI(list []*operationrpc.AdminUserItem) []types.OperationAdminUserItem {
	out := make([]types.OperationAdminUserItem, 0, len(list))
	for _, u := range list {
		out = append(out, adminUserToAPI(u))
	}
	return out
}

// roleToAPI 投影角色；member_count 是服务侧删除角色的保护依据，网关不改写。
func roleToAPI(r *operationrpc.RoleItem) types.OperationRoleItem {
	return types.OperationRoleItem{
		RoleId:        r.GetRoleId(),
		Name:          r.GetName(),
		Title:         r.GetTitle(),
		State:         r.GetState(),
		MemberCount:   r.GetMemberCount(),
		PermissionIds: r.GetPermissionIds(),
		Ctime:         r.GetCtime(),
		Mtime:         r.GetMtime(),
	}
}

func rolesToAPI(list []*operationrpc.RoleItem) []types.OperationRoleItem {
	out := make([]types.OperationRoleItem, 0, len(list))
	for _, r := range list {
		out = append(out, roleToAPI(r))
	}
	return out
}

// permissionToAPI 投影权限点（resource/action 可能是通配模式，原样透出）。
func permissionToAPI(p *operationrpc.PermissionItem) types.OperationPermissionItem {
	return types.OperationPermissionItem{
		PermissionId: p.GetPermissionId(),
		Resource:     p.GetResource(),
		Action:       p.GetAction(),
		Domain:       p.GetDomain(),
		Description:  p.GetDescription(),
		Ctime:        p.GetCtime(),
	}
}

func permissionsToAPI(list []*operationrpc.PermissionItem) []types.OperationPermissionItem {
	out := make([]types.OperationPermissionItem, 0, len(list))
	for _, p := range list {
		out = append(out, permissionToAPI(p))
	}
	return out
}

// menuToAPI 投影菜单节点：扁平返回，parent_id 表达层级，由前端组树。
func menuToAPI(m *operationrpc.MenuItem) types.OperationMenuItem {
	return types.OperationMenuItem{
		MenuId:             m.GetMenuId(),
		ParentId:           m.GetParentId(),
		Name:               m.GetName(),
		Path:               m.GetPath(),
		Icon:               m.GetIcon(),
		Sort:               m.GetSort(),
		RequiredPermission: m.GetRequiredPermission(),
		State:              m.GetState(),
		Ctime:              m.GetCtime(),
		Mtime:              m.GetMtime(),
	}
}

func menusToAPI(list []*operationrpc.MenuItem) []types.OperationMenuItem {
	out := make([]types.OperationMenuItem, 0, len(list))
	for _, m := range list {
		out = append(out, menuToAPI(m))
	}
	return out
}

// configToAPI 投影运营配置；cfg_value 是字符串承载，按 value_type 由前端解析。
func configToAPI(c *operationrpc.ConfigItem) types.OperationConfigItem {
	return types.OperationConfigItem{
		Id:         c.GetId(),
		CfgKey:     c.GetCfgKey(),
		CfgValue:   c.GetCfgValue(),
		ValueType:  c.GetValueType(),
		Scope:      c.GetScope(),
		Version:    c.GetVersion(),
		State:      c.GetState(),
		OperatorId: c.GetOperatorId(),
		Remark:     c.GetRemark(),
		Ctime:      c.GetCtime(),
		Mtime:      c.GetMtime(),
	}
}

// taskToAPI 投影管理任务（state/progress 以服务端为准，网关不推算进度）。
func taskToAPI(t *operationrpc.TaskInfo) types.OperationTaskItem {
	return types.OperationTaskItem{
		TaskId:     t.GetTaskId(),
		TaskType:   t.GetTaskType(),
		Params:     t.GetParams(),
		State:      t.GetState(),
		RequestId:  t.GetRequestId(),
		Total:      t.GetTotal(),
		Succeeded:  t.GetSucceeded(),
		Failed:     t.GetFailed(),
		Progress:   t.GetProgress(),
		OperatorId: t.GetOperatorId(),
		TraceId:    t.GetTraceId(),
		Ctime:      t.GetCtime(),
		Mtime:      t.GetMtime(),
		StartedAt:  t.GetStartedAt(),
		FinishedAt: t.GetFinishedAt(),
	}
}

func tasksToAPI(list []*operationrpc.TaskInfo) []types.OperationTaskItem {
	out := make([]types.OperationTaskItem, 0, len(list))
	for _, t := range list {
		out = append(out, taskToAPI(t))
	}
	return out
}

// taskStepToAPI 投影任务步骤；err_msg 已由 operation 脱敏（不含堆栈与密钥）。
func taskStepToAPI(s *operationrpc.TaskStepInfo) types.OperationTaskStepItem {
	return types.OperationTaskStepItem{
		Id:         s.GetId(),
		TaskId:     s.GetTaskId(),
		StepNo:     s.GetStepNo(),
		TargetType: s.GetTargetType(),
		TargetId:   s.GetTargetId(),
		State:      s.GetState(),
		Result:     s.GetResult(),
		ErrMsg:     s.GetErrMsg(),
		Ctime:      s.GetCtime(),
		Mtime:      s.GetMtime(),
	}
}

func taskStepsToAPI(list []*operationrpc.TaskStepInfo) []types.OperationTaskStepItem {
	out := make([]types.OperationTaskStepItem, 0, len(list))
	for _, s := range list {
		out = append(out, taskStepToAPI(s))
	}
	return out
}

// auditToAPI 投影审计索引（ip_hash 是不可逆摘要，网关不做二次处理）。
func auditToAPI(a *operationrpc.AuditIndexItem) types.OperationAuditItem {
	return types.OperationAuditItem{
		Id:           a.GetId(),
		AdminId:      a.GetAdminId(),
		Username:     a.GetUsername(),
		Action:       a.GetAction(),
		ResourceType: a.GetResourceType(),
		ResourceId:   a.GetResourceId(),
		Result:       a.GetResult(),
		IpHash:       a.GetIpHash(),
		UserAgent:    a.GetUserAgent(),
		TraceId:      a.GetTraceId(),
		RequestId:    a.GetRequestId(),
		Ctime:        a.GetCtime(),
	}
}

func auditsToAPI(list []*operationrpc.AuditIndexItem) []types.OperationAuditItem {
	out := make([]types.OperationAuditItem, 0, len(list))
	for _, a := range list {
		out = append(out, auditToAPI(a))
	}
	return out
}

// int64TTL 把 RPC 的 int32 建议缓存秒数搬进信封的 int64 ttl 字段。
// 负数（异常值）归零：ttl<0 会让后台前端把它当成过期数据反复回源。
func int64TTL(ttl int32) int64 {
	return normalizeTTL(int64(ttl))
}

// normalizeTTL 是 int64 版归零（operation.v1.GetMenuReply.ttl 本身就是 int64，
// 与其余 reply 的 int32 口径不同，不能强行套同一个转换）。
func normalizeTTL(ttl int64) int64 {
	if ttl <= 0 {
		return 0
	}
	return ttl
}
