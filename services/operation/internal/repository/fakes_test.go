package repository

// 本文件是测试夹具：内存版 model 与 Repository 构造器。
//
// 单测只覆盖纯逻辑（权限判定、口令与防爆破、乐观锁、状态机、审计脱敏），
// 一律不连 MySQL/Redis/gRPC（AGENTS.md §9）。做法是注入 model 接口的内存实现，
// 未实现的方法通过内嵌接口保持“被调用即 panic”，从而暴露意外的依赖泄漏。

import (
	"context"

	"go-video/services/operation/model"
)

// testTokenSecret 是测试用摘要密钥（只为让 newToken 可签发，不用于任何真实环境）。
var testTokenSecret = []byte("unit-test-admin-token-secret")

// newTestRepository 构造一个只含内存依赖的 Repository。
// conn 为 nil：本文件覆盖的用例都不应触达数据库，触达即 panic。
func newTestRepository() *Repository {
	return &Repository{
		cache: &Cache{st: noopStore{}},
		conn:  nil,
		cfg:   normalizeOptions(Options{}),
		login: LoginConf{MaxFail: defaultMaxFail, LockMinutes: defaultLockMinutes},
		session: SessionConf{
			TokenTTL: 7200,
			Issuer:   "op",
			Secret:   testTokenSecret,
		},
	}
}

// ---------------------------------------------------------------- 管理员账号

type guardCall struct {
	failCount   int32
	lockedUntil int64
	state       int32
}

type fakeAdminUserModel struct {
	model.AdminUserModel

	user       *model.AdminUser
	inserted   []*model.AdminUser
	insertErr  error
	guards     []guardCall
	touchTimes []int64
	updatedPw  []string
}

func (f *fakeAdminUserModel) Insert(_ context.Context, u *model.AdminUser) (int64, error) {
	if f.insertErr != nil {
		return 0, f.insertErr
	}
	f.inserted = append(f.inserted, u)
	return 101, nil
}

func (f *fakeAdminUserModel) FindByUsername(_ context.Context, username string) (*model.AdminUser, error) {
	if f.user == nil || f.user.Username != username {
		return nil, nil
	}
	return f.user, nil
}

func (f *fakeAdminUserModel) FindOne(_ context.Context, adminID int64) (*model.AdminUser, error) {
	if f.user == nil || f.user.AdminID != adminID {
		return nil, nil
	}
	return f.user, nil
}

func (f *fakeAdminUserModel) UpdateLoginGuard(_ context.Context, _ int64, failCount int32, lockedUntil int64, state int32) error {
	f.guards = append(f.guards, guardCall{failCount, lockedUntil, state})
	if f.user != nil {
		f.user.FailCount, f.user.LockedUntil, f.user.State = failCount, lockedUntil, state
	}
	return nil
}

func (f *fakeAdminUserModel) TouchLogin(_ context.Context, _ int64, at int64) error {
	f.touchTimes = append(f.touchTimes, at)
	if f.user != nil {
		f.user.LastLoginAt = at
		f.user.FailCount = 0
		f.user.LockedUntil = 0
		f.user.State = model.AdminStateNormal
	}
	return nil
}

func (f *fakeAdminUserModel) UpdatePassword(_ context.Context, _ int64, hash, _ string, _ int64) error {
	f.updatedPw = append(f.updatedPw, hash)
	return nil
}

// -------------------------------------------------------------------- 会话

type fakeSessionModel struct {
	model.AdminSessionModel

	issued     []*model.AdminSession
	revoked    []string
	revokedAll []int64
}

func (f *fakeSessionModel) Insert(_ context.Context, s *model.AdminSession) error {
	f.issued = append(f.issued, s)
	return nil
}

func (f *fakeSessionModel) Revoke(_ context.Context, token string) error {
	f.revoked = append(f.revoked, token)
	return nil
}

func (f *fakeSessionModel) RevokeAllByAdmin(_ context.Context, adminID int64) error {
	f.revokedAll = append(f.revokedAll, adminID)
	return nil
}

// -------------------------------------------------------------------- 角色

type fakeRoleModel struct {
	model.RoleModel

	grants       []*model.RoleGrant
	rolesToFind  map[int64]*model.Role
	replacedWith map[int64][]int64
}

func (f *fakeRoleModel) LoadAdminGrants(_ context.Context, _ int64) ([]*model.RoleGrant, error) {
	return f.grants, nil
}

func (f *fakeRoleModel) FindOne(_ context.Context, roleID int64) (*model.Role, error) {
	if r, ok := f.rolesToFind[roleID]; ok {
		return r, nil
	}
	return nil, nil
}

func (f *fakeRoleModel) ReplaceAdminRoles(_ context.Context, adminID int64, roleIDs []int64) error {
	if f.replacedWith == nil {
		f.replacedWith = map[int64][]int64{}
	}
	f.replacedWith[adminID] = append([]int64{}, roleIDs...)
	return nil
}

// -------------------------------------------------------------------- 审计

type fakeAuditModel struct {
	model.AuditIndexModel

	rows []*model.AuditIndex
}

func (f *fakeAuditModel) Insert(_ context.Context, a *model.AuditIndex) (int64, error) {
	f.rows = append(f.rows, a)
	return int64(len(f.rows)), nil
}

// ---------------------------------------------------------------- 运营配置

type fakeOpsConfigModel struct {
	model.OpsConfigModel

	current   *model.OpsConfig
	insertErr error
	// storedVersion 是 UpdateWithVersion 命中时写入的新版本；
	// mismatch=true 模拟“读后写之间被他人抢先推进”（乐观锁第二次失败）。
	mismatch    bool
	insertCalls int
	updateCalls int
	lastExpect  int64
	written     []*model.OpsConfig
}

func (f *fakeOpsConfigModel) Insert(_ context.Context, c *model.OpsConfig) (int64, error) {
	f.insertCalls++
	if f.insertErr != nil {
		return 0, f.insertErr
	}
	return 11, nil
}

func (f *fakeOpsConfigModel) FindOne(_ context.Context, cfgKey, _ string) (*model.OpsConfig, error) {
	if f.current == nil || f.current.CfgKey != cfgKey {
		return nil, nil
	}
	row := *f.current
	return &row, nil
}

func (f *fakeOpsConfigModel) UpdateWithVersion(_ context.Context, c *model.OpsConfig, expect int64) (*model.OpsConfig, bool, error) {
	f.updateCalls++
	f.lastExpect = expect
	if f.mismatch || f.current == nil || f.current.Version != expect {
		return nil, false, nil
	}
	out := *c
	out.Version = expect + 1
	f.written = append(f.written, &out)
	return &out, true, nil
}

// ---------------------------------------------------------------- 管理任务

type fakeTaskModel struct {
	model.AdminTaskModel

	task           *model.AdminTask
	inserted       []*model.AdminTask
	insertErr      error
	transitionCall int
	lastFrom       string
	lastTo         string
	// stateInDB 模拟库中真实状态：与 from 不符时 TransitionState 不命中（并发推进者）。
	stateInDB    string
	returnNotOK  bool
	addCounterOK int32
}

// Insert 记录待写入的任务；insertErr 用于模拟 uniq_request_id 冲突。
func (f *fakeTaskModel) Insert(_ context.Context, t *model.AdminTask) (int64, error) {
	if f.insertErr != nil {
		return 0, f.insertErr
	}
	f.inserted = append(f.inserted, t)
	f.task = t
	return 77, nil
}

func (f *fakeTaskModel) FindOne(_ context.Context, taskID int64) (*model.AdminTask, error) {
	if f.task == nil || f.task.TaskID != taskID {
		return nil, nil
	}
	row := *f.task
	return &row, nil
}

func (f *fakeTaskModel) FindByRequestID(_ context.Context, requestID string) (*model.AdminTask, error) {
	if f.task == nil || f.task.RequestID != requestID {
		return nil, nil
	}
	row := *f.task
	return &row, nil
}

func (f *fakeTaskModel) TransitionState(_ context.Context, _ int64, from, to string, _, _ int64) (bool, error) {
	f.transitionCall++
	f.lastFrom, f.lastTo = from, to
	if f.returnNotOK || (f.stateInDB != "" && f.stateInDB != from) {
		return false, nil
	}
	if f.task != nil {
		f.task.State = to
	}
	return true, nil
}

func (f *fakeTaskModel) AddCounters(_ context.Context, _ int64, succeeded, failed int32) error {
	f.addCounterOK += succeeded + failed
	return nil
}

type fakeStepModel struct {
	model.AdminTaskStepModel

	steps        []*model.AdminTaskStep
	cancelCalled int
	marked       []string
}

// InsertBatch 记录批量写入的步骤（内存实现，顺序即 step_no 顺序）。
func (f *fakeStepModel) InsertBatch(_ context.Context, steps []*model.AdminTaskStep) error {
	f.steps = append(f.steps, steps...)
	return nil
}

func (f *fakeStepModel) ListByTask(_ context.Context, _ int64) ([]*model.AdminTaskStep, error) {
	return f.steps, nil
}

func (f *fakeStepModel) ListExecutable(_ context.Context, _ int64, _ int32) ([]*model.AdminTaskStep, error) {
	var out []*model.AdminTaskStep
	for _, s := range f.steps {
		if s.State == model.StepStatePending {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeStepModel) MarkStep(_ context.Context, stepID int64, from, to, _, _ string) (bool, error) {
	f.marked = append(f.marked, from+"->"+to)
	for _, s := range f.steps {
		if s.ID == stepID {
			if s.State != from {
				return false, nil
			}
			s.State = to
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeStepModel) CancelPending(_ context.Context, _ int64) (int64, error) {
	f.cancelCalled++
	return 0, nil
}

func (f *fakeStepModel) CountByTask(_ context.Context, _ int64) (map[string]int32, error) {
	out := map[string]int32{}
	for _, s := range f.steps {
		out[s.State]++
	}
	return out, nil
}
