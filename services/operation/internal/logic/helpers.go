package logic

// 本文件是 logic 包的手写扩展（上下文解析与投影），不是 goctl 生成产物。
//
// 两条硬约束在此集中落地，避免每个 method 重复实现：
//  1. 写接口必须带可信 OpContext（operator_id > 0），否则拒绝——审计留痕不能缺主体；
//  2. 投影函数是唯一允许把 model 实体转成 RPC 消息的地方，
//     其中永不调用 PasswordHash / TwoFactorTarget 等凭证字段，
//     保证「管理员凭证与手机号不会出现在响应里」（AGENTS.md §6）。

import (
	"go-video/services/operation/internal/repository"
	"go-video/services/operation/model"
	"go-video/services/operation/rpc"
)

// actorFrom 解析并校验写接口的操作者上下文。
// gateway/admin 在调用前已完成 VerifyAdminPermission 并回填 operator_id；
// 这里只校验其存在性，缺失即视为非法请求。
func actorFrom(op *rpc.OpContext) (repository.Actor, error) {
	if op == nil || op.OperatorId <= 0 {
		return repository.Actor{}, model.ErrInvalidOperator
	}
	return repository.Actor{
		AdminID:   op.OperatorId,
		Username:  op.OperatorName,
		IP:        op.Ip,
		UserAgent: op.UserAgent,
		TraceID:   op.TraceId,
		RequestID: op.RequestId,
	}, nil
}

// reasonSessionInvalid 是会话非法的对外稳定原因。
// 权限不足与账号非正常态的原因由 repository.PermissionDecision.Reason 给出
// （repository.DenyReason*），避免两处定义漂移。
// 会话非法与权限不足都以 allowed=false 表达而不是 gRPC 错误：
// 拒绝是正常业务结果，故障才走 error 通道。
const reasonSessionInvalid = "admin_session_invalid"

// adminUserItem 投影管理员账号（入参已是脱敏视图，不含散列与手机号）。
func adminUserItem(v *repository.AdminUserView) *rpc.AdminUserItem {
	if v == nil {
		return nil
	}
	return &rpc.AdminUserItem{
		AdminId:             v.AdminID,
		Username:            v.Username,
		State:               v.State,
		Remark:              v.Remark,
		OperatorId:          v.Operator,
		LastLoginAt:         v.LastLoginAt,
		Ctime:               v.Ctime,
		Mtime:               v.Mtime,
		RoleIds:             v.RoleIDs,
		RoleNames:           v.RoleNames,
		SecondFactorEnabled: v.SecondFactorEnabled,
	}
}

// roleItem 投影角色。
func roleItem(v *repository.RoleView) *rpc.RoleItem {
	if v == nil {
		return nil
	}
	return &rpc.RoleItem{
		RoleId:        v.RoleID,
		Name:          v.Name,
		Title:         v.Title,
		State:         v.State,
		MemberCount:   v.MemberCount,
		PermissionIds: v.PermissionIDs,
		Ctime:         v.Ctime,
		Mtime:         v.Mtime,
	}
}

// permissionItem 投影权限点。
func permissionItem(v *repository.PermissionView) *rpc.PermissionItem {
	if v == nil {
		return nil
	}
	return &rpc.PermissionItem{
		PermissionId: v.PermissionID,
		Resource:     v.Resource,
		Action:       v.Action,
		Domain:       v.Domain,
		Description:  v.Description,
		Ctime:        v.Ctime,
	}
}

// menuItem 投影菜单节点。
func menuItem(v *repository.MenuView) *rpc.MenuItem {
	if v == nil {
		return nil
	}
	return &rpc.MenuItem{
		MenuId:             v.MenuID,
		ParentId:           v.ParentID,
		Name:               v.Name,
		Path:               v.Path,
		Icon:               v.Icon,
		Sort:               v.Sort,
		RequiredPermission: v.RequiredPermission,
		State:              v.State,
		Ctime:              v.Ctime,
		Mtime:              v.Mtime,
	}
}

// configItem 投影运营配置。
func configItem(c *model.OpsConfig) *rpc.ConfigItem {
	if c == nil {
		return nil
	}
	return &rpc.ConfigItem{
		Id:         c.ID,
		CfgKey:     c.CfgKey,
		CfgValue:   c.CfgValue,
		ValueType:  c.ValueType,
		Scope:      c.Scope,
		Version:    c.Version,
		State:      c.State,
		OperatorId: c.Operator,
		Remark:     c.Remark,
		Ctime:      c.Ctime,
		Mtime:      c.Mtime,
	}
}

// taskInfo 投影管理任务。
func taskInfo(t *model.AdminTask) *rpc.TaskInfo {
	if t == nil {
		return nil
	}
	return &rpc.TaskInfo{
		TaskId:     t.TaskID,
		TaskType:   t.TaskType,
		Params:     t.Params,
		State:      t.State,
		RequestId:  t.RequestID,
		Total:      t.Total,
		Succeeded:  t.Succeeded,
		Failed:     t.Failed,
		Progress:   t.Progress,
		OperatorId: t.Operator,
		TraceId:    t.TraceID,
		Ctime:      t.Ctime,
		Mtime:      t.Mtime,
		StartedAt:  t.StartedAt,
		FinishedAt: t.FinishedAt,
	}
}

// stepInfo 投影任务步骤。
func stepInfo(s *model.AdminTaskStep) *rpc.TaskStepInfo {
	if s == nil {
		return nil
	}
	return &rpc.TaskStepInfo{
		Id:         s.ID,
		TaskId:     s.TaskID,
		StepNo:     s.StepNo,
		TargetType: s.TargetType,
		TargetId:   s.TargetID,
		State:      s.State,
		Result:     s.Result,
		ErrMsg:     s.ErrMsg,
		Ctime:      s.Ctime,
		Mtime:      s.Mtime,
	}
}

// stepInfos 批量投影步骤。
func stepInfos(rows []*model.AdminTaskStep) []*rpc.TaskStepInfo {
	out := make([]*rpc.TaskStepInfo, 0, len(rows))
	for _, s := range rows {
		out = append(out, stepInfo(s))
	}
	return out
}

// auditItem 投影审计索引（ip_hash 已是不可逆摘要，无明文 IP/口令/token）。
func auditItem(a *model.AuditIndex) *rpc.AuditIndexItem {
	if a == nil {
		return nil
	}
	return &rpc.AuditIndexItem{
		Id:           a.ID,
		AdminId:      a.AdminID,
		Username:     a.Username,
		Action:       a.Action,
		ResourceType: a.ResourceType,
		ResourceId:   a.ResourceID,
		Result:       a.Result,
		IpHash:       a.IPHash,
		UserAgent:    a.UserAgent,
		TraceId:      a.TraceID,
		RequestId:    a.RequestID,
		Ctime:        a.Ctime,
	}
}

// auditItems 批量投影审计索引。
func auditItems(rows []*model.AuditIndex) []*rpc.AuditIndexItem {
	out := make([]*rpc.AuditIndexItem, 0, len(rows))
	for _, a := range rows {
		out = append(out, auditItem(a))
	}
	return out
}

// adminUserItems / roleItems / permissionItems / menuItems 批量投影。
func adminUserItems(rows []*repository.AdminUserView) []*rpc.AdminUserItem {
	out := make([]*rpc.AdminUserItem, 0, len(rows))
	for _, v := range rows {
		out = append(out, adminUserItem(v))
	}
	return out
}

func roleItems(rows []*repository.RoleView) []*rpc.RoleItem {
	out := make([]*rpc.RoleItem, 0, len(rows))
	for _, v := range rows {
		out = append(out, roleItem(v))
	}
	return out
}

func permissionItems(rows []*repository.PermissionView) []*rpc.PermissionItem {
	out := make([]*rpc.PermissionItem, 0, len(rows))
	for _, v := range rows {
		out = append(out, permissionItem(v))
	}
	return out
}

func menuItems(rows []*repository.MenuView) []*rpc.MenuItem {
	out := make([]*rpc.MenuItem, 0, len(rows))
	for _, v := range rows {
		out = append(out, menuItem(v))
	}
	return out
}

func taskInfos(rows []*model.AdminTask) []*rpc.TaskInfo {
	out := make([]*rpc.TaskInfo, 0, len(rows))
	for _, t := range rows {
		out = append(out, taskInfo(t))
	}
	return out
}
