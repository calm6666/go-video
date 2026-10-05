package logic

// adminlogin_logic_test.go 覆盖 AdminLogin（后台登录）的安全关键路径。
//
// 本轮已核实的生产缺陷与处置（AGENTS.md §10：不放宽断言、不掩盖现状）：
//   - 缺陷 #1（已改生产代码）：services/operation/internal/logic/adminloginlogic.go:52
//     原来把 token 生命周期 7200 塞进 AdminLoginReply.Ttl，而 rpc/operation.proto:58 写明
//     「后台登录态固定 0，不建议缓存」，gateway/admin/internal/logic/adminloginlogic.go:77-78
//     也按 0 把它搬进 HTTP 信封 ttl。现固定为 0，到期时间仍由 expires_at 承载。
//     见 TestAdminLogin成功签发会话并留痕 里的 Ttl 断言。
//   - 缺陷 #2（钉现状）：services/operation/internal/repository/admin_user.go:214-218 —
//     存量 password_hash 损坏时 VerifyAdminPassword 返回错误，AdminLogin 直接 wrap 外传，
//     既不写审计（无 admin.login/error 行）也不累计失败计数。
//     后果：账号「永远登不进去」在审计与防爆破两条线上都完全隐形，
//     运维只能靠 gRPC 报文里的 "verify admin password" 字样发现；
//     同时该账号成为免费的拒绝服务靶（改坏一行散列即可让管理员失联且无留痕）。
//     修不修涉及「数据问题要不要留痕」的策略选择，不唯一确定，故只钉不改。
//     见 TestAdminLogin散列损坏不降级为口令错。
//   - 缺陷 #3（钉现状）：services/operation/internal/repository/admin_user.go:220-223 —
//     recordLoginFailure 的 UpdateLoginGuard 失败时提前 return，跳过 denyLogin，
//     因此这次拒绝没有任何审计行。后果同上（爆破事件留痕缺口）。
//     见 TestAdminLogin审计写失败 与 TestAdminLogin计数写失败不留痕。
//   - 缺陷 #4（钉现状）：services/operation/internal/repository/admin_user.go:271-273 —
//     带二次校验的账号「漏传 second_factor」返回 ErrSecondFactorRequired，
//     但它不在 ErrSecondFactorWrong 分支里，因此不计入防爆破计数。
//     后果：省略字段即可无限次试探短信码而不触发锁定（只有码值错才计数）。
//     修法涉及「缺码算不算一次失败」的策略选择（可能有意为之，避免用户未收到短信时被锁），
//     不唯一确定，故只钉不改。见 TestAdminLogin二次校验缺码不计入爆破计数。
//   - 缺陷 #5（钉现状）：services/operation/internal/repository/admin_user.go:289-292 —
//     denyLogin 复制 actor 时只覆盖 Username、从不回填 AdminID，而 op_audit_index
//     没有 username 列（model/audit.go:13 的列清单；用户名只能靠 List 的
//     LEFT JOIN op_admin_user ON admin_id 还原，见 model/audit.go:137-140），
//     因此所有 result=denied 的 admin.login 审计行 admin_id 一律为 0。
//     后果：口令错的审计与「账号不存在」的审计在表里完全同形，
//     按 admin_id 查某个管理员的爆破/被撞库记录永远返回空——
//     防爆破计数（op_admin_user.fail_count）在留痕侧没有任何对应证据。
//     修法要给 denied 行补 admin_id（并决定账号不存在时填 0 是否可接受），
//     涉及审计口径统一，不在本轮测试改动范围内，故只钉不改。
//     见 TestAdminLogin口令错与账号不存在同类错误/口令错会计数并留痕 与
//     TestAdminLogin拒绝审计无法归因到账号。
//   - 判定为「测试期望错、非缺陷」（本轮从守卫表移出，见
//     TestAdminLogin纯空白口令按口令错处理）：登录路径上的口令是**不透明凭证**，
//     repository/admin_user.go:188 只做 in.Password == "" 的传输级前置检查，
//     不套用创建侧策略 passwordStrengthOK（repository/password.go:50-53 拒绝首尾空白）。
//     两条理由：① model/errors.go:18 把 ErrAdminPasswordEmpty 的语义限定为
//     「创建/重置时未提供口令」，登录侧只是复用它挡空报文；
//     ② 若在登录侧按创建策略先拒空白口令，等于给「策略变更即让存量账号登录语义变化」
//     开门，而且纯空白口令本来就只能落进 ErrAdminPasswordWrong 这条反枚举路径。
//     因此本轮把该用例改为钉「按口令错处理并计数」的真实语义，而不是删掉断言。

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"go-video/services/operation/model"
	"go-video/services/operation/rpc"
)

// loginReq 是后台登录入参（IP/UA/trace/request 走真实脱敏路径，因此必须给全）。
func loginReq(username, password string) *rpc.AdminLoginReq {
	return &rpc.AdminLoginReq{
		Username:  username,
		Password:  password,
		Ip:        "203.0.113.9",
		UserAgent: "Mozilla/5.0 (Windows NT 10.0) AdminConsole/1.0",
		TraceId:   "trace-login-1",
		RequestId: "req-login-1",
	}
}

// guardOf 把 UpdateLoginGuard 的轨迹条目拆成 (admin_id, fail_count, locked_until, state)，
// 便于对不可复现的 locked_until 走时间窗断言而不是硬编码。
func guardOf(t *testing.T, op string) (adminID, fail, locked, state string) {
	t.Helper()
	const prefix = "admin_user.UpdateLoginGuard:"
	if !strings.HasPrefix(op, prefix) {
		t.Fatalf("不是防爆破写入的轨迹项：%q", op)
	}
	fields := strings.Split(strings.TrimPrefix(op, prefix), "/")
	if len(fields) != 4 {
		t.Fatalf("防爆破轨迹字段数 = %d, want 4（条目 %q）", len(fields), op)
	}
	return fields[0], fields[1], fields[2], fields[3]
}

// parseInt64 把轨迹里的数字字段读回成 int64，以便对不可复现的时间戳走时间窗断言。
func parseInt64(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("%q 不是合法整数：%v", s, err)
	}
	return n
}

// 断言成功登录的完整轨迹：口令比对 → TouchLogin → 会话落库 → 会话缓存回填 →
// 角色并集装载 → 审计 ok。次序本身就是结论（例如「缓存回填必须在落库之后」，
// 反了就会让未落库的 token 短暂可校验）。
func wantLoginSuccessOps(t *testing.T, adminID int64, token string, ops []string) {
	t.Helper()
	wantOps(t, "登录成功轨迹", ops, []string{
		"admin_user.TouchLogin:" + itoa(adminID),
		"admin_session.Insert:" + itoa(adminID),
		"cache.Setex:" + sessionCacheKey(token) + "/300",
		"role.LoadAdminGrants:" + itoa(adminID),
		"audit_index.Insert:admin.login/ok",
	})
}

func TestAdminLogin成功签发会话并留痕(t *testing.T) {
	e := newEnv(t)
	adminID := seedAdmin(t, e.st, "alice", testPassword, nil)
	enabled := seedGrant(t, e.st, adminID, "content_ops", "video:submission#read")
	// 停用角色不参与判定（model/role.go:328 的 JOIN 带 r.state = 1），
	// 登录响应里的角色名必须与判定口径一致，否则后台会展示「有权限」的假象。
	disabled := seedRole(t, e.st, "legacy_ops", func(r *model.Role) { r.State = model.StateDisable })
	e.st.role.putBindings(adminID, []int64{enabled, disabled})

	before := nowUnix()
	reply, err := loginCall(t, e, loginReq("alice", testPassword))
	after := nowUnix()
	wantNoErr(t, "AdminLogin 成功路径", err)
	if reply == nil {
		t.Fatalf("reply = nil, want 非 nil")
	}

	wantOps(t, "登录第一步（按账号名取用户）", e.ops(0)[:1], []string{"admin_user.FindByUsername:alice"})
	wantLoginSuccessOps(t, adminID, reply.Token, e.ops(1))

	// 出参字段来源必须逐个对上：token/过期时间来自会话行，角色名来自授权快照。
	sess := e.st.session.get(reply.Token)
	if sess == nil {
		t.Fatalf("op_admin_session 里没有 token=%s 的行", reply.Token)
	}
	wantEQ(t, "会话行", "admin_id", sess.AdminID, adminID)
	wantEQ(t, "会话行", "state", sess.State, model.SessionStateActive)
	wantEQ(t, "会话行", "expires 与出参 expires_at", reply.ExpiresAt, sess.Expires)
	wantTSWindow(t, "登录响应", "expires_at", reply.ExpiresAt,
		before+defaultTokenTTL, after+defaultTokenTTL)
	wantEQ(t, "登录响应", "admin_id", reply.AdminId, adminID)
	wantEQ(t, "登录响应", "username", reply.Username, "alice")
	wantEQ(t, "登录响应", "roles", strings.Join(reply.Roles, ","), "content_ops")
	// 缺陷 #1 的落点：契约要求后台登录态不建议缓存。
	wantEQ(t, "登录响应", "ttl（契约固定 0）", reply.Ttl, int32(0))

	// token 形态：adm_<issuer>_<随机 24B hex>_<HMAC 摘要 8B hex>，与用户端 token 隔离。
	parts := strings.Split(reply.Token, "_")
	if len(parts) != 4 || parts[0] != "adm" || parts[1] != "op" {
		t.Fatalf("token = %q, want adm_op_<random>_<sig>", reply.Token)
	}
	if len(parts[2]) != 48 || len(parts[3]) != 16 {
		t.Fatalf("token 随机段/摘要段长度 = %d/%d, want 48/16（段 %q）", len(parts[2]), len(parts[3]), reply.Token)
	}

	// 会话缓存：key 由 token 派生，TTL 是固定的短窗口（300s）而不是 token 生命周期，
	// 否则「吊销后最长 2 小时仍可用」。
	ttl, ok := e.st.cache.ttlOf(sessionCacheKey(reply.Token))
	if !ok {
		t.Fatalf("缓存里没有 %s", sessionCacheKey(reply.Token))
	}
	wantEQ(t, "会话缓存", "ttl", ttl, 300)
	raw, _ := e.st.cache.rawOf(sessionCacheKey(reply.Token))
	wantContains(t, "会话缓存内容", raw, itoa(adminID))
	wantNotContains(t, "会话缓存不落散列", e.st.cache.allText(), e.st.admin.get(adminID).PasswordHash)

	// 审计行：ok + 目标资源是本人，IP 只落哈希，UA 原样（未含凭证）。
	row := e.st.audit.only(t)
	wantEQ(t, "审计", "action", row.Action, "admin.login")
	wantEQ(t, "审计", "result", row.Result, model.AuditResultOK)
	wantEQ(t, "审计", "admin_id", row.AdminID, adminID)
	wantEQ(t, "审计", "resource_type", row.ResourceType, "admin_user")
	wantEQ(t, "审计", "resource_id", row.ResourceID, itoa(adminID))
	wantHex32(t, "审计", "ip_hash", row.IPHash)
	wantNotContains(t, "审计不落明文 IP", row.IPHash, "203.0.113.9")
	wantContains(t, "审计 user_agent", row.UserAgent, "AdminConsole/1.0")
	wantEQ(t, "审计", "trace_id", row.TraceID, "trace-login-1")
	wantTSWindow(t, "审计", "ctime", row.Ctime, before, after)

	// TouchLogin 的落库效果：最近登录时间写入，计数清零。
	stored := e.st.admin.get(adminID)
	wantTSWindow(t, "op_admin_user", "last_login_at", stored.LastLoginAt, before, after)
	wantEQ(t, "op_admin_user", "fail_count", stored.FailCount, int32(0))
	wantEQ(t, "op_admin_user", "state", stored.State, model.AdminStateNormal)
}

func TestAdminLogin账号名归一化后查库(t *testing.T) {
	e := newEnv(t)
	seedAdmin(t, e.st, "alice", testPassword, nil)

	// 前后空白与大小写是复制粘贴常态：归一化在小写后才查库，
	// 因此轨迹里的 username 必须是归一化值（否则等价于开了第二条枚举通路）。
	reply, err := loginCall(t, e, loginReq("  Alice  ", testPassword))
	wantNoErr(t, "AdminLogin 归一化账号名", err)
	if reply == nil {
		t.Fatalf("reply = nil, want 非 nil")
	}
	wantOps(t, "归一化登录轨迹", e.ops(0)[:1], []string{"admin_user.FindByUsername:alice"})
	wantEQ(t, "登录响应", "username", reply.Username, "alice")
	wantCount(t, "登录轨迹", e.ops(0), "audit_index.Insert:admin.login/ok", 1)
}

func TestAdminLogin口令错与账号不存在同类错误(t *testing.T) {
	t.Run("口令错会计数并留痕", func(t *testing.T) {
		e := newEnv(t)
		adminID := seedAdmin(t, e.st, "alice", testPassword, nil)
		before := nowUnix()
		reply, err := loginCall(t, e, loginReq("alice", testPassword+"x"))
		after := nowUnix()
		wantErrIs(t, "口令错", err, ErrAdminPasswordWrong)
		if reply != nil {
			t.Fatalf("reply = %+v, want 失败时不返回响应体", reply)
		}
		wantOps(t, "口令错轨迹", e.ops(0), []string{
			"admin_user.FindByUsername:alice",
			"admin_user.UpdateLoginGuard:" + itoa(adminID) + "/1/0/1",
			"audit_index.Insert:admin.login/denied",
		})
		row := e.st.audit.only(t)
		wantEQ(t, "审计", "result", row.Result, model.AuditResultDenied)
		// TODO(缺陷 #5)：这一行期望的是 admin_id = adminID（口令错必须能归因到被爆破的账号），
		// 但 repository/admin_user.go:289-292 的 denyLogin 从不回填 AdminID，
		// 而 op_audit_index 没有 username 列（用户名靠 model/audit.go:137-140 的 LEFT JOIN 还原），
		// 于是 denied 行的主体信息整体丢失，与「账号不存在」完全同形。
		// 下面钉的是**现状**（0）而不是**应有值**：真正的后果由
		// TestAdminLogin拒绝审计无法归因到账号 那条哨兵用例守住；
		// 缺陷修好后这两处都必须翻红，不允许悄悄改回宽断言。
		wantEQ(t, "审计", "admin_id（缺陷 #5 现状：denied 不落 admin_id）", row.AdminID, int64(0))
		wantEQ(t, "审计", "resource_id", row.ResourceID, "") // 拒绝路径不带资源 ID
		wantTSWindow(t, "审计", "ctime", row.Ctime, before, after)
		wantEQ(t, "失败计数", "fail_count", e.st.admin.get(adminID).FailCount, int32(1))

		// 口令本身不得出现在任何对外文本里（错误报文、审计行、缓存）。
		wantNotContains(t, "错误报文不含口令", errText(err), testPassword)
		wantNotContains(t, "审计不含口令", fmt.Sprintf("%+v", row), testPassword)
		wantNotContains(t, "散列不进缓存", e.st.cache.allText(), e.st.admin.get(adminID).PasswordHash)
		wantEQ(t, "会话签发", "op_admin_session 行数", e.st.session.count(), 0)
	})

	t.Run("账号不存在既不计数也不签发", func(t *testing.T) {
		e := newEnv(t)
		seedAdmin(t, e.st, "alice", testPassword, nil)
		reply, err := loginCall(t, e, loginReq("nobody", testPassword))
		wantErrIs(t, "账号不存在", err, ErrAdminPasswordWrong)
		if reply != nil {
			t.Fatalf("reply = %+v, want 失败时不返回响应体", reply)
		}
		// 与口令错的唯一可观测差异：没有防爆破写入（账号行都不存在），
		// 对外错误类完全相同 → 不能靠报文区分「账号是否存在」。
		wantOps(t, "账号不存在轨迹", e.ops(0), []string{
			"admin_user.FindByUsername:nobody",
			"audit_index.Insert:admin.login/denied",
		})
		wantNoOpsWith(t, "账号不存在不得写凭证/计数", e.ops(0), "admin_session.Insert")
		wantNoOpsWith(t, "账号不存在不得清零登录时间", e.ops(0), "TouchLogin")
		row := e.st.audit.only(t)
		wantEQ(t, "审计", "admin_id（无主体）", row.AdminID, int64(0))
		wantEQ(t, "审计", "result", row.Result, model.AuditResultDenied)
	})

	// 两条路径的对外报文必须逐字相同：这是「不泄露账号存在性」最强的可断言形式。
	e1 := newEnv(t)
	seedAdmin(t, e1.st, "alice", testPassword, nil)
	_, wrongPwdErr := loginCall(t, e1, loginReq("alice", testPassword+"x"))
	_, noSuchErr := loginCall(t, e1, loginReq("ghost", testPassword))
	wantErr(t, "口令错应有错误", wrongPwdErr)
	wantErr(t, "账号不存在应有错误", noSuchErr)
	wantEQ(t, "账号枚举防护", "错误报文一致性", errText(noSuchErr), errText(wrongPwdErr))
}

func TestAdminLogin连续失败到阈值后锁定(t *testing.T) {
	e := newEnv(t, withLogin(3, 15))
	adminID := seedAdmin(t, e.st, "alice", testPassword, nil)
	id := itoa(adminID)

	for i, fail := 1, []string{"1", "2", "3"}; i <= 3; i++ {
		_, err := loginCall(t, e, loginReq("alice", testPassword+"x"))
		wantErrIs(t, fmt.Sprintf("第 %d 次口令错", i), err, ErrAdminPasswordWrong)
		// 顺序不能反：必须先把本轮轨迹取出来，再清零做「逐次推进」的布景。
		// （上一轮把 reset 放在取轨迹之前，于是三条断言都在拿空序列比期望，
		// 把「这一次登录到底碰了哪些依赖」这个结论整个丢掉了。）
		ops := e.ops(0)
		e.st.log.reset()
		wantCount(t, fmt.Sprintf("第 %d 次登录", i), ops, "admin_user.FindByUsername:alice", 1)
		wantCount(t, fmt.Sprintf("第 %d 次登录", i), ops, "audit_index.Insert:admin.login/denied", 1)
		guards := []string{}
		for _, op := range ops {
			if strings.HasPrefix(op, "admin_user.UpdateLoginGuard:") {
				guards = append(guards, op)
			}
		}
		if len(guards) != 1 {
			t.Fatalf("第 %d 次的防爆破写入 = %v, want 恰好 1 条", i, guards)
		}
		gotID, gotFail, gotLocked, gotState := guardOf(t, guards[0])
		wantEQ(t, "防爆破写入", "admin_id", gotID, id)
		wantEQ(t, "防爆破写入", "fail_count", gotFail, fail[i-1])
		if i < 3 {
			wantEQ(t, "阈值前不得锁定", "locked_until", gotLocked, "0")
			wantEQ(t, "阈值前保持正常态", "state", gotState, "1")
		} else {
			wantEQ(t, "达阈值须进锁定态", "state", gotState, "3")
			wantTSWindow(t, "防爆破写入", "locked_until", parseInt64(t, gotLocked),
				nowUnix()+15*60-5, nowUnix()+15*60+5)
		}
	}
	stored := e.st.admin.get(adminID)
	wantEQ(t, "锁定态", "state", stored.State, model.AdminStateLock)
	wantEQ(t, "锁定态", "fail_count", stored.FailCount, int32(3))

	// 锁定期内即使口令正确也必须拒绝，且不再走口令比对之外的任何写：
	// 不计数（否则锁定变成无限延长）、不 TouchLogin（否则锁定被自己清零）、不签发。
	e.st.log.reset()
	reply, err := loginCall(t, e, loginReq("alice", testPassword))
	wantErrIs(t, "锁定期内正确口令", err, ErrAdminLocked)
	if reply != nil {
		t.Fatalf("reply = %+v, want 锁定期间不签发", reply)
	}
	wantOps(t, "锁定后轨迹", e.ops(0), []string{
		"admin_user.FindByUsername:alice",
		"audit_index.Insert:admin.login/denied",
	})
	wantEQ(t, "锁定不延长计数", "fail_count", e.st.admin.get(adminID).FailCount, int32(3))
	wantEQ(t, "锁定不签发会话", "op_admin_session 行数", e.st.session.count(), 0)
	wantNoOpsWith(t, "锁定不得清零", e.ops(0), "TouchLogin")
}

func TestAdminLogin锁定期已过且口令正确则清零(t *testing.T) {
	e := newEnv(t)
	past := nowUnix() - 60
	adminID := seedAdmin(t, e.st, "alice", testPassword, func(u *model.AdminUser) {
		u.State, u.FailCount, u.LockedUntil = model.AdminStateLock, 5, past
	})

	before := nowUnix()
	reply, err := loginCall(t, e, loginReq("alice", testPassword))
	after := nowUnix()
	wantNoErr(t, "锁定期已过应可登录", err)
	if reply == nil {
		t.Fatalf("reply = nil, want 非 nil")
	}
	wantLoginSuccessOps(t, adminID, reply.Token, e.ops(1))

	stored := e.st.admin.get(adminID)
	wantEQ(t, "登录成功须回正常态", "state", stored.State, model.AdminStateNormal)
	wantEQ(t, "登录成功须清零计数", "fail_count", stored.FailCount, int32(0))
	wantEQ(t, "登录成功须清锁定", "locked_until", stored.LockedUntil, int64(0))
	wantTSWindow(t, "op_admin_user", "last_login_at", stored.LastLoginAt, before, after)
}

func TestAdminLogin禁用账号拒绝登录(t *testing.T) {
	e := newEnv(t)
	adminID := seedAdmin(t, e.st, "alice", testPassword, func(u *model.AdminUser) {
		u.State = model.AdminStateDisable
	})
	reply, err := loginCall(t, e, loginReq("alice", testPassword))
	wantErrIs(t, "禁用账号（即使口令正确）", err, ErrAdminDisabled)
	if reply != nil {
		t.Fatalf("reply = %+v, want 禁用账号不签发", reply)
	}
	// 禁用是终态：不计失败次数（否则解锁语义会覆盖禁用态），也不比对口令后的任何写。
	wantOps(t, "禁用账号轨迹", e.ops(0), []string{
		"admin_user.FindByUsername:alice",
		"audit_index.Insert:admin.login/denied",
	})
	wantNoOpsWith(t, "禁用不得计数", e.ops(0), "UpdateLoginGuard")
	wantEQ(t, "禁用不得改状态", "state", e.st.admin.get(adminID).State, model.AdminStateDisable)
}

func TestAdminLogin二次校验缺码不计入爆破计数(t *testing.T) {
	e := newEnv(t)
	const target = "13800001111"
	adminID := seedAdmin(t, e.st, "alice", testPassword, func(u *model.AdminUser) {
		u.TwoFactorTarget = target
	})

	reply, err := loginCall(t, e, loginReq("alice", testPassword)) // 不带 second_factor
	wantErrIs(t, "启用二阶但未带码", err, ErrSecondFactorMissing)
	if reply != nil {
		t.Fatalf("reply = %+v, want 缺码不签发", reply)
	}
	// 缺陷 #4 现状：缺码路径不写 UpdateLoginGuard，也不调用 account 通道，
	// 因此可被无限重复（在线爆破短信码时不触发 15 分钟锁定）。
	wantOps(t, "缺码轨迹", e.ops(0), []string{
		"admin_user.FindByUsername:alice",
		"audit_index.Insert:admin.login/denied",
	})
	wantEQ(t, "缺码不得计数", "fail_count", e.st.admin.get(adminID).FailCount, int32(0))
	wantEQ(t, "缺码不得签发", "op_admin_session 行数", e.st.session.count(), 0)
	wantEQ(t, "op_admin_user", "two_factor_target 仍是唯一落点", e.st.admin.get(adminID).TwoFactorTarget, target)
}

func TestAdminLogin二次校验码错计入爆破计数(t *testing.T) {
	e := newEnv(t, withSecondFactorChecker(func(target, code string) error {
		if target == "13800001111" && code == "007007" {
			return nil
		}
		return ErrSecondFactorWrong
	}))
	adminID := seedAdmin(t, e.st, "alice", testPassword, func(u *model.AdminUser) {
		u.TwoFactorTarget = "13800001111"
	})

	in := loginReq("alice", testPassword)
	in.SecondFactor = "000000"
	reply, err := loginCall(t, e, in)
	wantErrIs(t, "二阶码错", err, ErrSecondFactorWrong)
	if reply != nil {
		t.Fatalf("reply = %+v, want 码错不签发", reply)
	}
	wantOps(t, "码错轨迹", e.ops(0), []string{
		"admin_user.FindByUsername:alice",
		"account.CheckSecondFactor:13800001111/000000",
		"admin_user.UpdateLoginGuard:" + itoa(adminID) + "/1/0/1",
		"audit_index.Insert:admin.login/denied",
	})
	wantEQ(t, "码错须签发零会话", "op_admin_session 行数", e.st.session.count(), 0)

	// 码对的分支用于确认上面的计数不是「二阶恒失败」造成的假象。
	e.st.log.reset()
	ok := loginReq("alice", testPassword)
	ok.SecondFactor = "007007"
	reply, err = loginCall(t, e, ok)
	wantNoErr(t, "二阶码对", err)
	if reply == nil {
		t.Fatalf("reply = nil, want 码对签发")
	}
	wantEQ(t, "码对须清零计数", "fail_count", e.st.admin.get(adminID).FailCount, int32(0))
	wantCount(t, "码对轨迹", e.ops(0), "audit_index.Insert:admin.login/ok", 1)
}

func TestAdminLogin二次校验通道不可用则不签发(t *testing.T) {
	e := newEnv(t, withoutAccountDownstream())
	adminID := seedAdmin(t, e.st, "alice", testPassword, func(u *model.AdminUser) {
		u.TwoFactorTarget = "13800001111"
	})
	in := loginReq("alice", testPassword)
	in.SecondFactor = "007007"
	reply, err := loginCall(t, e, in)
	// fail-closed：宁可不登录，也不能因为 account 不可达就跳过二次因子。
	wantErrIs(t, "二阶通道未配置", err, ErrDownstreamUnavail)
	if reply != nil {
		t.Fatalf("reply = %+v, want 通道不可用不签发", reply)
	}
	wantOps(t, "通道不可用轨迹", e.ops(0), []string{
		"admin_user.FindByUsername:alice",
		"audit_index.Insert:admin.login/denied",
	})
	wantNoOpsWith(t, "通道不可用不得落会话", e.ops(0), "admin_session.Insert")
	wantEQ(t, "通道不可用不得改登录时间", "last_login_at", e.st.admin.get(adminID).LastLoginAt, int64(0))
}

func TestAdminLogin摘要密钥缺失先于任何库写拒绝(t *testing.T) {
	e := newEnv(t, withoutTokenSecret())
	adminID := seedAdmin(t, e.st, "alice", testPassword, nil)

	reply, err := loginCall(t, e, loginReq("alice", testPassword))
	wantErrIs(t, "未注入签名密钥", err, ErrTokenSecretMissing)
	if reply != nil {
		t.Fatalf("reply = %+v, want 密钥缺失不签发", reply)
	}
	// 部署缺陷必须在「任何写入之前」暴露：否则 TouchLogin 会把失败计数清零，
	// 相当于用一次登不上换来一次免费爆破重置。
	wantZeroOps(t, "密钥缺失", e.ops(0))
	wantEQ(t, "密钥缺失不得落会话", "op_admin_session 行数", e.st.session.count(), 0)
	wantEQ(t, "密钥缺失不得留审计", "op_audit_index 行数", len(e.st.audit.all()), 0)
	wantEQ(t, "密钥缺失不得清零计数", "fail_count", e.st.admin.get(adminID).FailCount, int32(0))
}

func TestAdminLogin入参守卫零依赖调用(t *testing.T) {
	cases := []struct{ name, username, password string }{
		{"空账号名", "", testPassword},
		{"过短", "ab", testPassword},
		{"数字开头", "1alice", testPassword},
		{"含连字符", "a-lice_", testPassword},
		{"超长", strings.Repeat("a", 33), testPassword},
		{"仅空白", "   ", testPassword},
		{"空口令", "alice", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seedAdmin(t, e.st, "alice", testPassword, nil)
			reply, err := loginCall(t, e, loginReq(tc.username, tc.password))
			wantErr(t, "守卫 "+tc.name, err)
			if reply != nil {
				t.Fatalf("reply = %+v, want 守卫拒绝不返回响应", reply)
			}
			wantZeroOps(t, "守卫 "+tc.name+"（不得触达库/缓存/审计）", e.ops(0))
			wantNotContains(t, "守卫报文不回显口令", errText(err), testPassword)
		})
	}
}

// TestAdminLogin纯空白口令按口令错处理 守住上一条守卫表的**边界在哪**：
// repository/admin_user.go:188 的前置检查只看 in.Password == ""（传输级空报文），
// 不套用创建侧策略 passwordStrengthOK（repository/password.go:50-53 拒绝首尾空白）。
// 结论是「纯空白口令是一次真实的口令错」：照常归一化查库、照常计入防爆破、
// 照常留 denied 审计、照常不给会话。
// 为什么这不是缺陷（详见文件头判定）：
//   - 登录侧对口令做任何 trim/规范化，等于让「创建策略变更」直接改写存量账号的登录语义；
//   - 把它挡在守卫里反而会跳过防爆破计数，给调用方一条「零成本试口令」的通路；
//   - 对外错误与真实口令错逐字相同，不额外泄露任何信息。
func TestAdminLogin纯空白口令按口令错处理(t *testing.T) {
	e := newEnv(t)
	adminID := seedAdmin(t, e.st, "alice", testPassword, nil)

	reply, err := loginCall(t, e, loginReq("alice", "   "))
	wantErrIs(t, "纯空白口令", err, ErrAdminPasswordWrong)
	if reply != nil {
		t.Fatalf("reply = %+v, want 不签发", reply)
	}
	// 与「空口令被守卫挡在库外」相对：这里必须真的走了一次口令比对，
	// 并且**计入**防爆破计数（不能因为看起来像空值就跳过留痕）。
	wantOps(t, "纯空白口令轨迹", e.ops(0), []string{
		"admin_user.FindByUsername:alice",
		"admin_user.UpdateLoginGuard:" + itoa(adminID) + "/1/0/1",
		"audit_index.Insert:admin.login/denied",
	})
	wantEQ(t, "纯空白口令须计数", "fail_count", e.st.admin.get(adminID).FailCount, int32(1))
	wantEQ(t, "纯空白口令不得签发", "op_admin_session 行数", e.st.session.count(), 0)
	wantNotContains(t, "纯空白口令报文", errText(err), "   ")
	// 对外报文与真实口令错逐字相同（守卫路径的报文则完全不同，见上一条用例）。
	e2 := newEnv(t)
	seedAdmin(t, e2.st, "alice", testPassword, nil)
	_, wrongPwdErr := loginCall(t, e2, loginReq("alice", testPassword+"x"))
	_, blankErr := loginCall(t, e2, loginReq("alice", "   "))
	wantErr(t, "真实口令错应有错误", wrongPwdErr)
	wantEQ(t, "空白口令与口令错同类", "错误报文", errText(blankErr), errText(wrongPwdErr))
}

// TODO(缺陷 #5) TestAdminLogin拒绝审计无法归因到账号 是缺陷 #5 的哨兵用例：
// 它断言的是**现状后果**（两条拒绝路径的审计行在表里逐列同形、按 admin_id 查不到任何
// 失败登录），而不是理想行为。一旦 repository/admin_user.go:289-292 的 denyLogin
// 补上 admin_id，本用例会变红，逼改动方回来更新结论与 README。
func TestAdminLogin拒绝审计无法归因到账号(t *testing.T) {
	e := newEnv(t)
	adminID := seedAdmin(t, e.st, "alice", testPassword, nil)

	// 同一来源 IP 先后打两次：一次口令错（账号存在）、一次账号不存在。
	// request_id 各自唯一，只为让下面能认出「哪条是哪次」——它不是主体归因列。
	inWrong := loginReq("alice", testPassword+"x")
	inWrong.RequestId = "req-wrong-pwd"
	inGhost := loginReq("ghost", testPassword)
	inGhost.RequestId = "req-ghost-user"
	_, errWrong := loginCall(t, e, inWrong)
	_, errGhost := loginCall(t, e, inGhost)
	wantErr(t, "口令错应有错误", errWrong)
	wantErr(t, "账号不存在应有错误", errGhost)

	rows := e.st.audit.all()
	if len(rows) != 2 {
		t.Fatalf("op_audit_index 行数 = %d, want 2（两次拒绝各一条）：%v", len(rows), actionsOf(rows))
	}
	wrongRow, ghostRow := rows[0], rows[1]
	wantEQ(t, "布景", "第一条的 request_id", wrongRow.RequestID, "req-wrong-pwd")
	wantEQ(t, "布景", "第二条的 request_id", ghostRow.RequestID, "req-ghost-user")
	for i, row := range []*model.AuditIndex{wrongRow, ghostRow} {
		wantEQ(t, fmt.Sprintf("布景 第 %d 条", i+1), "action", row.Action, "admin.login")
		wantEQ(t, fmt.Sprintf("布景 第 %d 条", i+1), "result", row.Result, model.AuditResultDenied)
	}

	// 两条 denied 行在**全部主体列**上逐字相同 —— 这正是缺陷：op_audit_index 没有
	// username 列（model/audit.go:13），admin_id 又从不回填，
	// 于是表里没有任何一列能区分「alice 被人试了口令」与「有人试了个不存在的账号」。
	wantEQ(t, "缺陷 #5 现状", "两种拒绝的 admin_id 同形", ghostRow.AdminID, wrongRow.AdminID)
	wantEQ(t, "缺陷 #5 现状", "口令错的 admin_id 丢了主体", wrongRow.AdminID, int64(0))
	wantEQ(t, "缺陷 #5 现状", "两种拒绝的 resource_id 同形", ghostRow.ResourceID, wrongRow.ResourceID)
	wantEQ(t, "缺陷 #5 现状", "resource_id 为空", wrongRow.ResourceID, "")

	// 后果的直接证据：按 admin_id 查该管理员的登录失败记录，一条也查不到，
	// 而同一时刻 op_admin_user.fail_count 已经累加到 1。
	e.st.log.reset()
	found, total, err := e.st.audit.List(context.Background(), model.AuditFilter{
		AdminID: adminID, Action: "admin.login", Pn: 1, Ps: 20,
	})
	wantNoErr(t, "按 admin_id 查审计", err)
	wantEQ(t, "缺陷 #5 现状", "按 admin_id 能查到的登录条数", total, int64(0))
	if len(found) != 0 {
		t.Fatalf("按 admin_id=%d 查审计返回了 %d 行，缺陷 #5 的结论已变化，请同步 README", adminID, len(found))
	}
	wantEQ(t, "同一时刻", "op_admin_user.fail_count 已累加", e.st.admin.get(adminID).FailCount, int32(1))
}

func TestAdminLogin散列损坏不降级为口令错(t *testing.T) {
	e := newEnv(t)
	adminID := seedAdmin(t, e.st, "alice", testPassword, nil)
	e.st.admin.mutate(t, adminID, func(u *model.AdminUser) { u.PasswordHash = "pbkdf2_sha256$999$zz$" })

	reply, err := loginCall(t, e, loginReq("alice", testPassword))
	if reply != nil {
		t.Fatalf("reply = %+v, want 数据损坏不签发", reply)
	}
	wantErr(t, "散列损坏应报错", err)
	// 必须是「数据问题」而不是「口令错」：否则运维会以为管理员忘了密码，
	// 而真实原因是散列被写坏（可能是越权写库）。
	if errors.Is(err, ErrAdminPasswordWrong) {
		t.Fatalf("散列损坏被伪装成口令错：%v", err)
	}
	wantContains(t, "散列损坏报文", errText(err), "verify admin password")
	// 缺陷 #2 现状：既无审计行也不计数，管理员失联在留痕上完全隐形。
	wantOps(t, "散列损坏轨迹", e.ops(0), []string{"admin_user.FindByUsername:alice"})
	wantEQ(t, "散列损坏无审计", "op_audit_index 行数", len(e.st.audit.all()), 0)
	wantEQ(t, "散列损坏不计数", "fail_count", e.st.admin.get(adminID).FailCount, int32(0))
}

func TestAdminLogin审计写失败不回滚业务结果(t *testing.T) {
	errAuditDown := errors.New("op_audit_index Insert: connection refused")

	t.Run("成功路径审计失败必须外传", func(t *testing.T) {
		e := newEnv(t)
		adminID := seedAdmin(t, e.st, "alice", testPassword, nil)
		e.st.audit.failWith("Insert", errAuditDown)

		reply, err := loginCall(t, e, loginReq("alice", testPassword))
		wantErrIs(t, "审计写失败须外传", err, errAuditDown)
		if reply != nil {
			t.Fatalf("reply = %+v, want 审计缺留痕时不给调用方发凭证", reply)
		}
		// 会话已落库且缓存已回填（README 记录的补偿风险）：断言这个既成事实，
		// 免得将来有人「顺手」在这里加回滚而改变语义。
		wantCount(t, "审计失败轨迹", e.ops(0), "admin_session.Insert:"+itoa(adminID), 1)
		wantCount(t, "审计失败轨迹", e.ops(0), "audit_index.Insert:", 1)
		wantEQ(t, "审计失败不得回滚会话", "op_admin_session 行数", e.st.session.count(), 1)
		wantContains(t, "审计失败报文", errText(err), "write audit index failed")
	})

	t.Run("拒绝路径审计失败不得掩盖拒绝原因", func(t *testing.T) {
		e := newEnv(t)
		adminID := seedAdmin(t, e.st, "alice", testPassword, nil)
		e.st.audit.failWith("Insert", errAuditDown)

		_, err := loginCall(t, e, loginReq("alice", testPassword+"x"))
		// 真正的结论是「口令错」，不能被审计故障顶替（repository/admin_user.go:292-296）。
		wantErrIs(t, "拒绝原因优先于审计故障", err, ErrAdminPasswordWrong)
		wantNotContains(t, "拒绝报文不含审计故障", errText(err), "audit")
		wantCount(t, "拒绝+审计故障轨迹", e.ops(0), "admin_user.UpdateLoginGuard:"+itoa(adminID)+"/1/0/1", 1)
	})
}

func TestAdminLogin计数写失败不留痕(t *testing.T) {
	e := newEnv(t)
	adminID := seedAdmin(t, e.st, "alice", testPassword, nil)
	errGuardDown := errors.New("op_admin_user UpdateLoginGuard: deadlinex exceeded")
	e.st.admin.failWith("UpdateLoginGuard", errGuardDown)

	_, err := loginCall(t, e, loginReq("alice", testPassword+"x"))
	wantErrIs(t, "计数写失败须外传", err, errGuardDown)
	// 缺陷 #3 现状：UpdateLoginGuard 失败会提前 return，这次拒绝没有任何审计行。
	wantOps(t, "计数写失败轨迹", e.ops(0), []string{
		"admin_user.FindByUsername:alice",
		"admin_user.UpdateLoginGuard:" + itoa(adminID) + "/1/0/1",
	})
	wantEQ(t, "计数写失败无审计", "op_audit_index 行数", len(e.st.audit.all()), 0)
}

func TestAdminLogin凭证与二次校验目标不外泄(t *testing.T) {
	e := newEnv(t, withSecondFactorChecker(func(target, code string) error {
		return nil
	}))
	const target = "13800001111"
	adminID := seedAdmin(t, e.st, "alice", testPassword, func(u *model.AdminUser) {
		u.TwoFactorTarget = target
	})
	in := loginReq("alice", testPassword)
	in.SecondFactor = "007007"
	// 调用方把凭证塞进 UA 时，落库前必须截断（审计与会话两张表都不能成为第二份凭证）。
	in.UserAgent = "AdminConsole/1.0 Authorization=Bearer-abc123xyz"
	in.Ip = "198.51.100.7"
	reply, err := loginCall(t, e, in)
	wantNoErr(t, "带二阶的登录", err)
	if reply == nil {
		t.Fatalf("reply = nil, want 签发")
	}

	stored := e.st.admin.get(adminID)
	sess := e.st.session.get(reply.Token)
	if sess == nil {
		t.Fatalf("op_admin_session 里没有 token=%s 的行", reply.Token)
	}
	// 凭证字段的唯一落点是 op_admin_user.password_hash / two_factor_target；
	// 其余任何一处（出参、会话行、会话缓存、审计）出现即等于多复制了一份凭证。
	outward := map[string]string{
		"登录响应": reply.String(),
		"会话行":  fmt.Sprintf("%+v", sess),
		"会话缓存": e.st.cache.allText(),
		"审计":   auditText(e.st.audit.all()),
	}
	for name, text := range outward {
		wantNotContains(t, name+" 不含明文口令", text, testPassword)
		wantNotContains(t, name+" 不含口令散列", text, stored.PasswordHash)
		wantNotContains(t, name+" 不含二次校验码", text, "007007")
		wantNotContains(t, name+" 不含二次校验目标（手机号）", text, target)
		wantNotContains(t, name+" 不含明文 IP", text, "198.51.100.7")
		wantNotContains(t, name+" 不含 UA 里夹带的凭证", text, "Bearer-abc123xyz")
	}
	// token 必须与库里那一行逐字相同：出参凭证以库为事实源，而不是 logic 自造。
	wantEQ(t, "登录响应", "token 与 op_admin_session 逐字相同", reply.Token, sess.Token)
	wantEQ(t, "op_admin_session", "admin_id", sess.AdminID, adminID)
	// 手机号在 op_admin_user 里确实存在（否则上面的「不外泄」会因为根本没启用二阶而空转）。
	wantContains(t, "手机号仍只落 op_admin_user", fmt.Sprintf("%+v", stored), target)
	wantContains(t, "审计 UA 脱敏留痕", outward["审计"], "[redacted]")
}

// auditText 把全部审计行拼成一段文本，供「凭证不外泄」的整体兜底断言。
func auditText(rows []*model.AuditIndex) string {
	var sb strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&sb, "%+v ", r)
	}
	return sb.String()
}
