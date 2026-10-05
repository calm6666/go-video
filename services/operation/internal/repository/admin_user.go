package repository

// 本文件实现管理员账号生命周期：登录（口令 + 防爆破 + 可选二次校验）、
// 创建、更新、禁用与列表查询。
//
// 隐私与安全约束：
//   - AdminUserView 是唯一的账号出参形态，不含 password_hash / two_factor_target，
//     只回 second_factor_enabled 布尔（AGENTS.md §6：管理员字段不得混入客户端，
//     这里进一步保证连后台出参也不含凭证与手机号）；
//   - 登录失败统一收敛为 ErrAdminPasswordWrong（不区分“账号不存在”），
//     避免账号枚举；
//   - 口令散列强度见 password.go（PBKDF2-HMAC-SHA256，不低于用户口令的 MD5 方案）。

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go-video/services/operation/model"
)

const (
	// defaultMaxFail 连续失败锁定阈值默认值。
	defaultMaxFail = 5
	// defaultLockMinutes 锁定时长默认值（分钟）。
	defaultLockMinutes = 15
	// secondFactorDisableToken UpdateAdminUser 传入该值表示关闭二次校验。
	secondFactorDisableToken = "-"
	// secondFactorTargetMaxLen 与 op_admin_user.two_factor_target 列宽一致。
	secondFactorTargetMaxLen = 64
)

// usernamePattern 管理员账号名：小写字母开头，允许小写字母/数字/下划线/点，总长 3~32。
var usernamePattern = regexp.MustCompile(`^[a-z][a-z0-9_.]{2,31}$`)

// AdminUserView 管理员账号的安全投影（不含任何凭证形态字段）。
type AdminUserView struct {
	// AdminID 管理员 ID。
	AdminID int64
	// Username 账号名。
	Username string
	// State 1 正常、2 禁用、3 锁定。
	State int32
	// Remark 备注。
	Remark string
	// Operator 最后修改人 admin_id。
	Operator int64
	// LastLoginAt 最近登录时间（Unix 秒）。
	LastLoginAt int64
	// Ctime 创建时间。
	Ctime int64
	// Mtime 修改时间。
	Mtime int64
	// SecondFactorEnabled 是否启用二次校验（由 two_factor_target 是否非空推导，不外泄目标值）。
	SecondFactorEnabled bool
	// RoleIDs 已绑定角色 ID。
	RoleIDs []int64
	// RoleNames 已绑定角色名。
	RoleNames []string
}

// CreateAdminUserInput 创建管理员账号入参。
type CreateAdminUserInput struct {
	// Username 账号名。
	Username string
	// Password 初始口令（明文只在本函数栈内存在）。
	Password string
	// Remark 备注。
	Remark string
	// RoleIDs 初始角色（空表示无权限账号）。
	RoleIDs []int64
	// SecondFactorTarget 二次校验目标（手机号）；空表示不启用。
	SecondFactorTarget string
}

// UpdateAdminUserInput 更新管理员账号入参（零值语义见各字段注释）。
type UpdateAdminUserInput struct {
	// AdminID 目标管理员。
	AdminID int64
	// Remark 新备注；空表示不修改。
	Remark string
	// State 目标状态：0 不修改，1 恢复正常，2 禁用。
	State int32
	// NewPassword 重置口令；空表示不改。
	NewPassword string
	// SecondFactorTarget 覆盖二次校验目标；空表示不修改，"-" 表示关闭。
	SecondFactorTarget string
}

// LoginInput 管理员登录入参。
type LoginInput struct {
	// Username 账号名。
	Username string
	// Password 口令。
	Password string
	// SecondFactor 二次校验码（账号启用二次校验时必填）。
	SecondFactor string
	// Actor 来源上下文（IP/UA/trace/request），只用于审计与会话记录。
	Actor Actor
}

// LoginResult 登录成功结果。
type LoginResult struct {
	// Token 后台会话 token。
	Token string
	// AdminID 管理员 ID。
	AdminID int64
	// Username 账号名。
	Username string
	// ExpiresAt token 过期时间（Unix 秒）。
	ExpiresAt int64
	// Roles 角色名（并集）。
	Roles []string
	// TTL token 有效秒数。
	TTL int64
}

// checkAdminUsername 校验账号名格式（不查库）。
func checkAdminUsername(username string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(username))
	if !usernamePattern.MatchString(name) {
		return "", fmt.Errorf("operation: invalid username %q (need 3-32 chars, lowercase letter first, [a-z0-9_.])", name)
	}
	return name, nil
}

// normalizeSecondFactorTarget 归一化二次校验目标：
// 返回 (值, 是否修改)。空串表示“不修改”，"-" 表示“关闭”。
func normalizeSecondFactorTarget(raw string) (string, bool, error) {
	target := strings.TrimSpace(raw)
	if target == "" {
		return "", false, nil
	}
	if target == secondFactorDisableToken {
		return "", true, nil
	}
	if len([]rune(target)) > secondFactorTargetMaxLen {
		return "", false, fmt.Errorf("operation: second factor target too long (max %d)", secondFactorTargetMaxLen)
	}
	return target, true, nil
}

// loginBlocked 判断账号当前是否禁止登录（纯函数，便于单测）。
// 锁定期已过（locked_until <= now）视为可重试，登录成功时会清零计数。
func loginBlocked(u *model.AdminUser, now int64) bool {
	if u == nil {
		return false
	}
	switch u.State {
	case model.AdminStateDisable:
		return true
	case model.AdminStateLock:
		return u.LockedUntil > now
	default:
		return false
	}
}

// evalLoginFailure 计算一次登录失败后的防爆破字段（纯函数，便于单测）。
// 返回 (fail_count, locked_until, state)。达到阈值即进入锁定态。
func evalLoginFailure(u *model.AdminUser, now int64, cfg LoginConf) (int32, int64, int32) {
	maxFail := cfg.MaxFail
	if maxFail <= 0 {
		maxFail = defaultMaxFail
	}
	lockMinutes := cfg.LockMinutes
	if lockMinutes <= 0 {
		lockMinutes = defaultLockMinutes
	}
	fail := u.FailCount + 1
	if fail < maxFail {
		return fail, 0, model.AdminStateNormal
	}
	return fail, now + lockMinutes*60, model.AdminStateLock
}

// AdminLogin 校验口令（可选二次校验）并签发后台会话。
// 失败路径同样写审计索引（result=denied），成功写 result=ok。
func (r *Repository) AdminLogin(ctx context.Context, in LoginInput) (*LoginResult, error) {
	username, err := checkAdminUsername(in.Username)
	if err != nil {
		return nil, err
	}
	if in.Password == "" {
		return nil, model.ErrAdminPasswordEmpty
	}
	// 摘要密钥缺失属于部署缺陷：先于任何库写入拒绝，避免把账号标记为“刚登录成功”
	// 却发不出 token（TouchLogin 会清零失败计数）。
	if err := r.session.requireSecret(); err != nil {
		return nil, err
	}
	actor := in.Actor
	actor.Username = username

	u, err := r.adminMd.FindByUsername(ctx, username)
	if err != nil {
		return nil, err
	}
	if u == nil {
		// 与口令错误同一响应：不透露账号是否存在。
		return nil, r.denyLogin(ctx, actor, username, model.ErrAdminPasswordWrong)
	}
	now := time.Now().Unix()
	if loginBlocked(u, now) {
		if u.State == model.AdminStateDisable {
			return nil, r.denyLogin(ctx, actor, u.Username, model.ErrAdminDisabled)
		}
		return nil, r.denyLogin(ctx, actor, u.Username, model.ErrAdminLocked)
	}

	ok, err := VerifyAdminPassword(in.Password, u.PasswordHash)
	if err != nil {
		// 散列格式异常属于数据问题，不能当成“口令错”。
		return nil, fmt.Errorf("operation: verify admin password: %w", err)
	}
	if !ok {
		if err := r.recordLoginFailure(ctx, u, now); err != nil {
			return nil, err
		}
		return nil, r.denyLogin(ctx, actor, u.Username, model.ErrAdminPasswordWrong)
	}

	if err := r.checkSecondFactor(ctx, u, in.SecondFactor); err != nil {
		if errors.Is(err, model.ErrSecondFactorWrong) {
			// 校验码错同样计入防爆破计数，避免短信码被在线爆破。
			if recErr := r.recordLoginFailure(ctx, u, now); recErr != nil {
				return nil, recErr
			}
		}
		return nil, r.denyLogin(ctx, actor, u.Username, err)
	}

	if err := r.adminMd.TouchLogin(ctx, u.AdminID, now); err != nil {
		return nil, err
	}
	sess, err := r.issueSession(ctx, u.AdminID, actor)
	if err != nil {
		return nil, err
	}
	grants, err := r.roleMd.LoadAdminGrants(ctx, u.AdminID)
	if err != nil {
		return nil, err
	}
	snapshot := &rbacSnapshot{Grants: grants}
	result := &LoginResult{
		Token:     sess.Token,
		AdminID:   u.AdminID,
		Username:  u.Username,
		ExpiresAt: sess.Expires,
		Roles:     snapshot.roleNames(),
		TTL:       r.session.ttl(),
	}
	if err := r.writeAudit(ctx, Actor{
		AdminID: u.AdminID, Username: u.Username, IP: actor.IP,
		UserAgent: actor.UserAgent, TraceID: actor.TraceID, RequestID: actor.RequestID,
	}, actionAdminLogin, "admin_user", fmt.Sprintf("%d", u.AdminID), model.AuditResultOK); err != nil {
		return nil, err
	}
	return result, nil
}

// checkSecondFactor 账号未配置二次校验目标时直接放行；
// 配置了则必须带码，校验通道复用 account 的验证码 RPC（不读写 account 的表）。
func (r *Repository) checkSecondFactor(ctx context.Context, u *model.AdminUser, code string) error {
	if strings.TrimSpace(u.TwoFactorTarget) == "" {
		return nil
	}
	if strings.TrimSpace(code) == "" {
		return model.ErrSecondFactorRequired
	}
	gw, err := r.downstream.requireAccount()
	if err != nil {
		// 二次校验通道不可用时 fail-closed：宁可不登录，也不能跳过因子。
		return err
	}
	return gw.CheckSecondFactor(ctx, u.TwoFactorTarget, code)
}

// recordLoginFailure 累加失败计数并在达到阈值时锁定账号。
func (r *Repository) recordLoginFailure(ctx context.Context, u *model.AdminUser, now int64) error {
	fail, lockedUntil, state := evalLoginFailure(u, now, r.login)
	return r.adminMd.UpdateLoginGuard(ctx, u.AdminID, fail, lockedUntil, state)
}

// denyLogin 写“登录被拒”审计后原样返回错误（错误对象不含口令）。
func (r *Repository) denyLogin(ctx context.Context, actor Actor, username string, err error) error {
	auditActor := actor
	auditActor.Username = username
	if writeErr := r.writeAudit(ctx, auditActor, actionAdminLogin, "admin_user", "", model.AuditResultDenied); writeErr != nil {
		// 审计写失败不掩盖真正的拒绝原因。
		return err
	}
	return err
}

// CreateAdminUser 新建管理员账号并绑定初始角色。
func (r *Repository) CreateAdminUser(ctx context.Context, actor Actor, in CreateAdminUserInput) (*AdminUserView, error) {
	if err := r.requireOperator(actor); err != nil {
		return nil, err
	}
	username, err := checkAdminUsername(in.Username)
	if err != nil {
		return nil, err
	}
	if !passwordStrengthOK(in.Password) {
		return nil, model.ErrAdminPasswordWeak
	}
	remark := strings.TrimSpace(in.Remark)
	if len([]rune(remark)) > maxRemarkLen {
		return nil, fmt.Errorf("operation: remark too long (max %d)", maxRemarkLen)
	}
	target, enabled, err := normalizeSecondFactorTarget(in.SecondFactorTarget)
	if err != nil {
		return nil, err
	}
	if !enabled {
		target = ""
	}
	if err := r.validateRoleIDs(ctx, in.RoleIDs); err != nil {
		return nil, err
	}

	hash, algo, err := HashAdminPassword(in.Password)
	if err != nil {
		return nil, err
	}
	row := &model.AdminUser{
		Username:        username,
		PasswordHash:    hash,
		PwdAlgo:         algo,
		State:           model.AdminStateNormal,
		Remark:          remark,
		TwoFactorTarget: target,
		Operator:        actor.AdminID,
	}
	adminID, err := r.adminMd.Insert(ctx, row)
	if err != nil {
		if errors.Is(err, model.ErrAdminExists) {
			return nil, err
		}
		return nil, err
	}
	if len(in.RoleIDs) > 0 {
		if err := r.roleMd.ReplaceAdminRoles(ctx, adminID, in.RoleIDs); err != nil {
			// 角色绑定失败时账号已存在但无权限；由运营重试 AssignRoles（README 记录该缺口）。
			return nil, fmt.Errorf("operation: bind initial roles for admin %d: %w", adminID, err)
		}
		r.invalidateRBAC(ctx)
	}
	if err := r.writeAudit(ctx, actor, actionAdminUserCreate, "admin_user", fmt.Sprintf("%d", adminID), model.AuditResultOK); err != nil {
		return nil, err
	}
	return r.AdminUserView(ctx, adminID)
}

// UpdateAdminUser 更新备注/状态/二次校验目标，可选重置口令（重置即吊销全部会话）。
func (r *Repository) UpdateAdminUser(ctx context.Context, actor Actor, in UpdateAdminUserInput) (*AdminUserView, error) {
	if err := r.requireOperator(actor); err != nil {
		return nil, err
	}
	if in.AdminID <= 0 {
		return nil, model.ErrInvalidOperator
	}
	target, err := r.adminMd.FindOne(ctx, in.AdminID)
	if err != nil {
		return nil, err
	}
	if target == nil {
		return nil, model.ErrAdminNotFound
	}
	if in.State == model.AdminStateDisable && actor.AdminID == in.AdminID {
		return nil, model.ErrAdminSelfDisable
	}

	patch := &model.AdminUserPatch{Operator: actor.AdminID}
	if in.Remark != "" {
		remark := strings.TrimSpace(in.Remark)
		if len([]rune(remark)) > maxRemarkLen {
			return nil, fmt.Errorf("operation: remark too long (max %d)", maxRemarkLen)
		}
		patch.Remark = &remark
	}
	if next, ok, err := normalizeSecondFactorTarget(in.SecondFactorTarget); err != nil {
		return nil, err
	} else if ok {
		patch.TwoFactorTarget = &next
	}
	switch in.State {
	case 0:
	case model.AdminStateNormal, model.AdminStateDisable:
		state := in.State
		patch.State = &state
	default:
		return nil, fmt.Errorf("operation: invalid admin state %d (0 keep, 1 normal, 2 disable)", in.State)
	}

	if patch.Remark == nil && patch.TwoFactorTarget == nil && patch.State == nil && in.NewPassword == "" {
		return nil, fmt.Errorf("operation: nothing to update")
	}
	if _, err := r.adminMd.UpdateProfile(ctx, in.AdminID, patch); err != nil {
		return nil, err
	}

	if in.NewPassword != "" {
		if !passwordStrengthOK(in.NewPassword) {
			return nil, model.ErrAdminPasswordWeak
		}
		hash, algo, err := HashAdminPassword(in.NewPassword)
		if err != nil {
			return nil, err
		}
		if err := r.adminMd.UpdatePassword(ctx, in.AdminID, hash, algo, actor.AdminID); err != nil {
			return nil, err
		}
		// 重置口令后旧会话一律作废，避免“改密码但被盗会话仍在线”。
		if err := r.RevokeAllSessions(ctx, in.AdminID); err != nil {
			return nil, err
		}
		if err := r.writeAudit(ctx, actor, actionAdminUserResetPwd, "admin_user", fmt.Sprintf("%d", in.AdminID), model.AuditResultOK); err != nil {
			return nil, err
		}
	}
	if in.State == model.AdminStateDisable {
		if err := r.RevokeAllSessions(ctx, in.AdminID); err != nil {
			return nil, err
		}
	}
	if err := r.writeAudit(ctx, actor, actionAdminUserUpdate, "admin_user", fmt.Sprintf("%d", in.AdminID), model.AuditResultOK); err != nil {
		return nil, err
	}
	return r.AdminUserView(ctx, in.AdminID)
}

// DisableAdminUser 禁用账号并吊销全部会话（禁用不删数据，便于审计与恢复）。
func (r *Repository) DisableAdminUser(ctx context.Context, actor Actor, adminID int64, reason string) (*AdminUserView, error) {
	if err := r.requireOperator(actor); err != nil {
		return nil, err
	}
	if adminID <= 0 {
		return nil, model.ErrInvalidOperator
	}
	if actor.AdminID == adminID {
		return nil, model.ErrAdminSelfDisable
	}
	target, err := r.adminMd.FindOne(ctx, adminID)
	if err != nil {
		return nil, err
	}
	if target == nil {
		return nil, model.ErrAdminNotFound
	}
	state := model.AdminStateDisable
	if _, err := r.adminMd.UpdateProfile(ctx, adminID, &model.AdminUserPatch{
		State:    &state,
		Operator: actor.AdminID,
	}); err != nil {
		return nil, err
	}
	if err := r.RevokeAllSessions(ctx, adminID); err != nil {
		return nil, err
	}
	resourceID := fmt.Sprintf("%d%s", adminID, reasonSuffix(reason))
	if err := r.writeAudit(ctx, actor, actionAdminUserDisable, "admin_user", resourceID, model.AuditResultOK); err != nil {
		return nil, err
	}
	return r.AdminUserView(ctx, adminID)
}

// reasonSuffix 把禁用原因规范成审计 resource_id 的后缀（截断、去空白，不含换行）。
func reasonSuffix(reason string) string {
	reason = strings.TrimSpace(strings.ReplaceAll(reason, "\n", " "))
	if reason == "" {
		return ""
	}
	if len([]rune(reason)) > 64 {
		reason = string([]rune(reason)[:64])
	}
	return "|" + reason
}

// ListAdminUsers 分页查询账号（列表页一次性批量取回角色，避免 N+1）。
func (r *Repository) ListAdminUsers(ctx context.Context, state int32, keyword string, pn, ps int32) ([]*AdminUserView, int64, error) {
	pn, ps = pagePair(pn, ps)
	rows, total, err := r.adminMd.List(ctx, state, strings.TrimSpace(keyword), pn, ps)
	if err != nil {
		return nil, 0, err
	}
	if len(rows) == 0 {
		return nil, total, nil
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.AdminID)
	}
	bindings, err := r.roleMd.ListRoleBindings(ctx, ids)
	if err != nil {
		return nil, 0, err
	}
	roleIDs := make(map[int64][]int64, len(ids))
	roleNames := make(map[int64][]string, len(ids))
	for _, b := range bindings {
		roleIDs[b.AdminID] = append(roleIDs[b.AdminID], b.RoleID)
		roleNames[b.AdminID] = append(roleNames[b.AdminID], b.RoleName)
	}
	views := make([]*AdminUserView, 0, len(rows))
	for _, row := range rows {
		views = append(views, &AdminUserView{
			AdminID:             row.AdminID,
			Username:            row.Username,
			State:               row.State,
			Remark:              row.Remark,
			Operator:            row.Operator,
			LastLoginAt:         row.LastLoginAt,
			Ctime:               row.Ctime,
			Mtime:               row.Mtime,
			SecondFactorEnabled: strings.TrimSpace(row.TwoFactorTarget) != "",
			RoleIDs:             roleIDs[row.AdminID],
			RoleNames:           roleNames[row.AdminID],
		})
	}
	return views, total, nil
}

// AdminUserView 组装单个账号的安全投影（含角色 ID 与角色名）。
func (r *Repository) AdminUserView(ctx context.Context, adminID int64) (*AdminUserView, error) {
	u, err := r.adminMd.FindOne(ctx, adminID)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, model.ErrAdminNotFound
	}
	roleIDs, err := r.roleMd.ListRoleIDsByAdmin(ctx, adminID)
	if err != nil {
		return nil, err
	}
	view := &AdminUserView{
		AdminID:             u.AdminID,
		Username:            u.Username,
		State:               u.State,
		Remark:              u.Remark,
		Operator:            u.Operator,
		LastLoginAt:         u.LastLoginAt,
		Ctime:               u.Ctime,
		Mtime:               u.Mtime,
		SecondFactorEnabled: strings.TrimSpace(u.TwoFactorTarget) != "",
		RoleIDs:             roleIDs,
	}
	if len(roleIDs) > 0 {
		pairs, err := r.roleMd.ListRoleBindings(ctx, []int64{adminID})
		if err != nil {
			return nil, err
		}
		for _, p := range pairs {
			view.RoleNames = append(view.RoleNames, p.RoleName)
		}
	}
	return view, nil
}

// validateRoleIDs 校验角色存在且未停用，避免绑定出无效角色。
func (r *Repository) validateRoleIDs(ctx context.Context, roleIDs []int64) error {
	for _, id := range roleIDs {
		if id <= 0 {
			return fmt.Errorf("operation: invalid role_id %d", id)
		}
		role, err := r.roleMd.FindOne(ctx, id)
		if err != nil {
			return err
		}
		if role == nil {
			return model.ErrRoleNotFound
		}
		if role.State != model.StateEnable {
			return fmt.Errorf("operation: role %d is disabled", id)
		}
	}
	return nil
}

// requireOperator 写接口必须有可信操作者（网关已完成鉴权并回填 operator_id）。
func (r *Repository) requireOperator(actor Actor) error {
	if actor.AdminID <= 0 {
		return model.ErrInvalidOperator
	}
	return nil
}
