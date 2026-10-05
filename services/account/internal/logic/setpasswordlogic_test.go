package logic

// setpasswordlogic_test.go 覆盖 SetPassword（repository/login.go:512-553 → logic/setpasswordlogic.go:29-35）。
//
// 被测判定链：FindActive(mid,1) → 有生效密钥则校验旧密码 → 校验新密码非空 →
// 事务内 [MarkHistory → Insert 新密钥] → RevokeAll(mid)（错误被 `_ =` 吞掉）。
//
// 钉住的关键事实：
//   - 没有生效密钥时旧密码**完全不校验**（首次设密分支）；
//   - 新密码没有任何强度/长度/trim 校验，" " 原样哈希入库（缺口 13）；
//   - 第二次改密必然失败：account_secret 的唯一键 uk_mid_type_status 把 status 也放进了
//     键里（deploy/migrations/account/000011_create_account_secret.sql:42），
//     MarkHistory 把当前行改成 status=1 时若已有历史行就撞 1062 → 事务回滚：
//     任何账号的密码一生只能改一次（缺口 3）；
//   - RevokeAll 的错误被吞：改密成功但会话全部存活（缺口 5）；
//   - 整条链路不碰任何缓存、不写任何日志：旧 token 在 tokenCacheTTL(600s) 内继续有效，
//     ip 入参没有落任何地方（缺口 7 / 9d / 15）；
//   - mid 不校验账号是否存在，可以给不存在的 mid 造出生效密钥（缺口 16）。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/account/model"
	"go-video/services/account/rpc"
)

func callSetPassword(t *testing.T, e *env, mid int64, oldPwd, newPwd, ip string) (*rpc.DelCacheReply, error) {
	t.Helper()
	return NewSetPasswordLogic(context.Background(), e.svcCtx).SetPassword(
		&rpc.SetPasswordReq{Mid: mid, OldPassword: oldPwd, NewPassword: newPwd, Ip: ip})
}

// activeSecret 回读某 mid 当前生效的密钥行（rowsOf 已经给的是值拷贝，静默）。
func activeSecret(t *testing.T, st *store, mid int64) *model.AccountSecret {
	t.Helper()
	for _, s := range st.secret.rowsOf(mid) {
		if s.Status == model.SecretStatusActive {
			return s
		}
	}
	t.Fatalf("mid=%d 没有生效密钥行", mid)
	return nil
}

// TestSetPasswordChangesExistingPassword 正常改密：四步顺序（读旧密钥 → 事务内置历史 →
// 插新密钥 → 全量吊销）、旧密钥行就地转历史、新密钥行 salt/hash 形态正确、
// 该 mid 的会话全部吊销、不写任何日志、不碰任何缓存。
func TestSetPasswordChangesExistingPassword(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	oldSeed := activeSecret(t, st, mid)
	sess := login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
	nextID := st.secret.peekID()
	st.log.reset()
	now := nowUnix()

	reply, err := callSetPassword(t, e, mid, "S3cret!", "N3w#Pass", "1.2.3.4")
	wantNoErr(t, "改密", err)
	if reply == nil {
		t.Fatalf("改密成功必须返回非 nil reply")
	}
	wantOps(t, "改密调用序列", e.ops(0), []string{
		"secret.FindActive:" + itoa(mid) + "/1",
		"secret.MarkHistory:" + itoa(mid) + "/1",
		"secret.Insert:" + itoa(nextID+1) + "/" + itoa(mid) + "/0",
		"session.RevokeAll:" + itoa(mid),
	})
	opened, rolledBack := st.conn.txStats()
	wantEQ(t, "改密开一个事务", "事务数", opened, 1)
	wantEQ(t, "改密不应回滚", "回滚数", rolledBack, 0)

	// 审计面与缓存面：整条链路既没有一步缓存、也没有一条日志（缺口 7 / 9d / 15）
	wantNoOpsWith(t, "改密链路", e.ops(0), "cache.")
	wantNoOpsWith(t, "改密链路", e.ops(0), "loginlog.Add")
	wantNoOpsWith(t, "改密链路", e.ops(0), "capturelog.Add")
	wantEQ(t, "改密不落任何登录日志", "loginlog 行数", st.loginLog.count(), 1) // 只剩 login() 那条
	wantEQ(t, "改密不新增账号", "account 行数", st.account.count(), 1)
	wantEQ(t, "改密不动凭证", "credential 行数", st.cred.count(), 1)

	// 旧密钥行转历史（不删行），新密钥行生效
	wantEQ(t, "改密后密钥行数", "行数", st.secret.count(), 2)
	history := st.secret.rowsOf(mid)[0]
	wantEQ(t, "原密钥行改为历史", "status", history.Status, model.SecretStatusHistory)
	wantEQ(t, "历史行的 salt 不变", "salt", history.Salt, oldSeed.Salt)
	wantEQ(t, "历史行的 hash 不变", "hash", history.Hash, oldSeed.Hash)

	fresh := activeSecret(t, st, mid)
	wantHexLen(t, "新密钥", "salt", fresh.Salt, 32)
	if fresh.Salt == oldSeed.Salt {
		t.Errorf("新密钥复用了旧 salt：%s", fresh.Salt)
	}
	wantEQ(t, "新 hash = MD5(pwd+盐+salt)", "hash", fresh.Hash, seedHash("N3w#Pass", fresh.Salt))
	wantTSWindow(t, "新密钥时间戳", "ctime", fresh.CTime, now, nowUnix()+5)
	wantEQ(t, "新密钥 mtime 与 ctime 同步", "mtime", fresh.MTime, fresh.CTime)

	// 全量吊销确实落库（该 mid 的会话一律 status=1，行不删）
	wantEQ(t, "改密后该 mid 无有效会话", "active", st.session.activeFor(mid), 0)
	wantEQ(t, "改密不删会话行", "session 行数", st.session.count(), 1)
	wantEQ(t, "原会话就地吊销", "status", sessionOf(t, e, sess.Token).Status, model.SessionStatusRevoked)

	// 口径闭环：旧密码不再能登录、新密码可以（此时才动 loginLog，后面不再断言）
	if _, err := callPasswordLogin(t, e, &rpc.LoginReq{Account: "alice", Password: "S3cret!",
		LoginType: int32(model.LoginTypePassword), Ip: "1.2.3.4"}); !errors.Is(err, ErrLoginPasswordWrong) {
		t.Errorf("旧密码 = %v, want ErrLoginPasswordWrong", err)
	}
	if got := login(t, e, "alice", "N3w#Pass", "1.2.3.4", "pc"); got.Mid != mid {
		t.Errorf("新密码登录 mid = %d, want %d", got.Mid, mid)
	}
}

// TestSetPasswordFirstTimeSetSkipsHistoryAndOldCheck 首次设密（验证码注册的中间态）：
// 没有生效密钥 → 旧密码一律不校验（传错的也放行），且不走 MarkHistory。
func TestSetPasswordFirstTimeSetSkipsHistoryAndOldCheck(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUserWithoutSecret(t, st, "carol", model.CredentialTypePhone)
	nextID := st.secret.peekID()

	_, err := callSetPassword(t, e, mid, "随便乱填的旧密码", "N3w#Pass", "1.2.3.4")
	wantNoErr(t, "首次设密（旧密码错误也放行）", err)
	wantOps(t, "首次设密调用序列", e.ops(0), []string{
		"secret.FindActive:" + itoa(mid) + "/1",
		"secret.Insert:" + itoa(nextID+1) + "/" + itoa(mid) + "/0",
		"session.RevokeAll:" + itoa(mid),
	})
	wantEQ(t, "首次设密不置历史", "MarkHistory 次数", countPrefix(e.ops(0), "secret.MarkHistory"), 0)
	fresh := activeSecret(t, st, mid)
	wantEQ(t, "新密钥 hash", "hash", fresh.Hash, seedHash("N3w#Pass", fresh.Salt))
	wantEQ(t, "首次设密只有一行密钥", "行数", st.secret.count(), 1)
}

// TestSetPasswordWrongOldPasswordWritesNothing 旧密码不对：领域错误外传，
// 事务一次都没开，密钥与会话原封不动。
func TestSetPasswordWrongOldPasswordWritesNothing(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	before := activeSecret(t, st, mid)
	sess := login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
	st.log.reset()

	_, err := callSetPassword(t, e, mid, "wrong-old", "N3w#Pass", "1.2.3.4")
	wantErrIs(t, "旧密码不对", err, ErrLoginPasswordWrong)
	wantOps(t, "旧密码不对的调用序列", e.ops(0), []string{"secret.FindActive:" + itoa(mid) + "/1"})
	opened, rolledBack := st.conn.txStats()
	wantEQ(t, "校验失败不得开事务", "事务数", opened, 0)
	wantEQ(t, "校验失败不得回滚", "回滚数", rolledBack, 0)
	wantEQ(t, "校验失败后密钥没换", "hash", activeSecret(t, st, mid).Hash, before.Hash)
	wantEQ(t, "校验失败后行数不变", "行数", st.secret.count(), 1)
	wantEQ(t, "校验失败后会话仍有效", "active", st.session.activeFor(mid), 1)
	wantEQ(t, "校验失败后 token 仍可鉴权", "is_login", mustTokenInfo(t, e, sess.Token).IsLogin, true)
}

// TestSetPasswordEmptyOldPasswordCountsAsWrong 入参校验边界：有生效密钥时空旧密码
// 归为「密码错误」，不是「请输入密码」——与 ResetPassword 的口径不同。
func TestSetPasswordEmptyOldPasswordCountsAsWrong(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	before := activeSecret(t, st, mid)

	_, err := callSetPassword(t, e, mid, "", "N3w#Pass", "1.2.3.4")
	wantErrIs(t, "空旧密码", err, ErrLoginPasswordWrong)
	wantOps(t, "空旧密码的调用序列", e.ops(0), []string{"secret.FindActive:" + itoa(mid) + "/1"})
	wantEQ(t, "空旧密码不得写入密钥", "行数", st.secret.count(), 1)
	wantEQ(t, "空旧密码不得改 hash", "hash", activeSecret(t, st, mid).Hash, before.Hash)
}

// TestSetPasswordEmptyNewPasswordRejected 新密码为空：ErrPasswordRequired，
// 且是在开事务之前判掉的（旧密码此刻已经校验过了）。
func TestSetPasswordEmptyNewPasswordRejected(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	before := activeSecret(t, st, mid)

	_, err := callSetPassword(t, e, mid, "S3cret!", "", "1.2.3.4")
	wantErrIs(t, "空新密码", err, ErrPasswordRequired)
	wantOps(t, "空新密码的调用序列", e.ops(0), []string{"secret.FindActive:" + itoa(mid) + "/1"})
	opened, _ := st.conn.txStats()
	wantEQ(t, "空新密码不得开事务", "事务数", opened, 0)
	wantEQ(t, "空新密码不得吊销会话", "RevokeAll 次数", countPrefix(e.ops(0), "session.RevokeAll"), 0)
	wantEQ(t, "空新密码后 hash 不变", "hash", activeSecret(t, st, mid).Hash, before.Hash)
}

// TestSetPasswordAcceptsBlankPasswords 缺口 13 的实锤：新密码没有强度、长度、trim 校验——
// 单个空格按原样哈希入库并能用它登录；只有「严格等于空串」被拒。
func TestSetPasswordAcceptsBlankPasswords(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)

	_, err := callSetPassword(t, e, mid, "S3cret!", " ", "1.2.3.4")
	wantNoErr(t, "空格当新密码", err)
	fresh := activeSecret(t, st, mid)
	wantEQ(t, "空格密码原样入库（未 trim）", "hash", fresh.Hash, seedHash(" ", fresh.Salt))

	_, err = callSetPassword(t, e, mid, " ", "", "1.2.3.4")
	wantErrIs(t, "空串仍是唯一被拒的形态", err, ErrPasswordRequired)
}

// TestSetPasswordSecondChangeAlwaysFails 缺口 3 的正面证明：
// 唯一键 uk_mid_type_status 把 status 也放进键里，第一次改密留下一条 status=1 的历史行，
// 第二次改密的 MarkHistory 必然撞 1062 → 事务回滚、错误外传、会话一个都没吊销。
// 结论：任何账号的密码在有且只有一次可改（ResetPassword 同理）。
func TestSetPasswordSecondChangeAlwaysFails(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)

	_, err := callSetPassword(t, e, mid, "S3cret!", "Second1", "1.2.3.4")
	wantNoErr(t, "第一次改密", err)
	firstChange := activeSecret(t, st, mid)
	// 改密后用新密码重新登录一个会话：它必须在这次「失败的第二次改密」中存活
	sess := login(t, e, "alice", "Second1", "1.2.3.4", "pc")
	st.log.reset()

	_, err = callSetPassword(t, e, mid, "Second1", "Third1", "1.2.3.4")
	wantErr(t, "第二次改密必须失败（缺口 3）", err)
	if !strings.Contains(errText(err), "1062") || !strings.Contains(errText(err), "uk_mid_type_status") {
		t.Errorf("第二次改密的错误 = %v, want 唯一键 1062（uk_mid_type_status）", err)
	}
	wantOps(t, "第二次改密的调用序列", e.ops(0), []string{
		"secret.FindActive:" + itoa(mid) + "/1",
		"secret.MarkHistory:" + itoa(mid) + "/1",
	})
	wantCount(t, "失败路径不落新密钥", e.ops(0), "secret.Insert", 0)
	opened, rolledBack := st.conn.txStats()
	wantEQ(t, "两次改密共开两个事务", "事务数", opened, 2)
	wantEQ(t, "第二次整段回滚", "回滚数", rolledBack, 1)
	wantEQ(t, "回滚后密钥行数不变", "行数", st.secret.count(), 2)
	wantEQ(t, "回滚后生效密钥仍是第一次那个", "hash", activeSecret(t, st, mid).Hash, firstChange.Hash)
	wantEQ(t, "回滚后历史行仍是历史", "status", st.secret.rowsOf(mid)[0].Status, model.SecretStatusHistory)
	// 影响面：改密没成功，吊销那一步也没走到——现有会话与旧密码都还活着
	wantCount(t, "失败路径不吊销会话", e.ops(0), "session.RevokeAll", 0)
	wantEQ(t, "失败后现有会话仍有效", "active", st.session.activeFor(mid), 1)
	wantEQ(t, "失败后现有会话的 DB 行仍有效", "status",
		sessionOf(t, e, sess.Token).Status, model.SessionStatusActive)
	for _, s := range st.secret.rowsOf(mid) {
		wantNotContains(t, "回滚后不得留下 \"Third1\" 的哈希", s.Hash, seedHash("Third1", s.Salt))
	}
}

// TestSetPasswordMarkHistoryFailureRollbacksAndKeepsOldPassword MarkHistory 故障：
// 事务回滚、错误原样外传、旧密钥与会话都不受影响（缓存也没动）。
func TestSetPasswordMarkHistoryFailureRollbacksAndKeepsOldPassword(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	before := activeSecret(t, st, mid)
	boom := errors.New("account/db: mark history down")
	st.secret.failWith("MarkHistory", boom)
	st.log.reset()

	reply, err := callSetPassword(t, e, mid, "S3cret!", "N3w#Pass", "1.2.3.4")
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v, want %v", err, boom)
	}
	if reply != nil {
		t.Errorf("出错路径必须返回 nil reply，got %+v", reply)
	}
	wantOps(t, "MarkHistory 故障的调用序列", e.ops(0), []string{
		"secret.FindActive:" + itoa(mid) + "/1",
		"secret.MarkHistory:" + itoa(mid) + "/1",
	})
	wantNoOpsWith(t, "MarkHistory 故障路径", e.ops(0), "session.RevokeAll")
	opened, rolledBack := st.conn.txStats()
	wantEQ(t, "失败也开了一次事务", "事务数", opened, 1)
	wantEQ(t, "失败必须回滚", "回滚数", rolledBack, 1)
	wantEQ(t, "回滚后密钥行数", "行数", st.secret.count(), 1)
	wantEQ(t, "回滚后 salt 不变", "salt", activeSecret(t, st, mid).Salt, before.Salt)
	wantEQ(t, "旧密码仍可用（hash 未变）", "hash", activeSecret(t, st, mid).Hash, before.Hash)
}

// TestSetPasswordInsertFailureRollbacksMarkHistory Insert 故障时，事务内已执行的
// MarkHistory 必须被回滚——否则旧密码会被降级成历史、账号直接失去密码。
func TestSetPasswordInsertFailureRollbacksMarkHistory(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	before := activeSecret(t, st, mid)
	nextID := st.secret.peekID()
	boom := errors.New("account/db: secret insert down")
	st.secret.failWith("Insert", boom)
	st.log.reset()

	_, err := callSetPassword(t, e, mid, "S3cret!", "N3w#Pass", "1.2.3.4")
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v, want %v", err, boom)
	}
	wantOps(t, "Insert 故障的调用序列", e.ops(0), []string{
		"secret.FindActive:" + itoa(mid) + "/1",
		"secret.MarkHistory:" + itoa(mid) + "/1",
		"secret.Insert:" + itoa(nextID+1) + "/" + itoa(mid) + "/0",
	})
	_, rolledBack := st.conn.txStats()
	wantEQ(t, "Insert 失败必须回滚", "回滚数", rolledBack, 1)
	wantEQ(t, "回滚后只剩原密钥行", "行数", st.secret.count(), 1)
	wantEQ(t, "回滚后原密钥仍是生效态", "status", activeSecret(t, st, mid).Status, model.SecretStatusActive)
	wantEQ(t, "回滚后原密钥 hash 不变", "hash", activeSecret(t, st, mid).Hash, before.Hash)
	wantNoOpsWith(t, "Insert 故障路径", e.ops(0), "session.RevokeAll")
	// 旧密码还能登（事务边界正确性的行为侧证据）
	login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
}

// TestSetPasswordRevokeAllFailureSwallowed login.go:551 的 `_ =`：密码已经改了，
// 但吊销失败被吞 → 调用方拿到成功、旧会话一个都没吊销（缺口 5）。
func TestSetPasswordRevokeAllFailureSwallowed(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	sess := login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
	nextID := st.secret.peekID()
	boom := errors.New("account/db: revoke all down")
	st.session.failWith("RevokeAll", boom)
	st.log.reset()

	_, err := callSetPassword(t, e, mid, "S3cret!", "N3w#Pass", "1.2.3.4")
	wantNoErr(t, "RevokeAll 故障不得让改密失败（错误被吞）", err)
	wantOps(t, "RevokeAll 故障的调用序列", e.ops(0), []string{
		"secret.FindActive:" + itoa(mid) + "/1",
		"secret.MarkHistory:" + itoa(mid) + "/1",
		"secret.Insert:" + itoa(nextID+1) + "/" + itoa(mid) + "/0",
		"session.RevokeAll:" + itoa(mid),
	})
	wantEQ(t, "改密成功但旧会话仍有效（缺口 5）", "active", st.session.activeFor(mid), 1)
	wantEQ(t, "旧 token 仍判定已登录", "is_login", mustTokenInfo(t, e, sess.Token).IsLogin, true)
	wantEQ(t, "新密码也已生效：同时存在「新密码 + 旧会话」", "行数", st.secret.count(), 2)
}

// TestSetPasswordLeavesSessionCacheAlive 改密全程不碰 Redis：
// 即使 RevokeAll 成功，缓存里的 ak_/rk_ 仍是有效会话，TokenInfo 命中缓存直接放行
// ——「改密后吊销」最迟要等 tokenCacheTTL(600s) 才真正生效（缺口 7）。
func TestSetPasswordLeavesSessionCacheAlive(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	sess := login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
	st.log.reset()

	_, err := callSetPassword(t, e, mid, "S3cret!", "N3w#Pass", "1.2.3.4")
	wantNoErr(t, "改密", err)
	wantNoOpsWith(t, "改密链路", e.ops(0), "cache.")
	wantEQ(t, "改密后 DB 会话已吊销", "active", st.session.activeFor(mid), 0)
	wantEQ(t, "改密后 refresh 缓存也还在", "rk_ 存在", st.cache.has("rk_"+sess.RefreshToken), true)

	st.log.reset()
	info, err := callTokenInfo(t, e, sess.Token)
	wantNoErr(t, "改密后鉴权", err)
	wantEQ(t, "改密后旧 token 仍可用（缺口 7）", "is_login", info.IsLogin, true)
	wantOps(t, "改密后走的是残留缓存，不回源", e.ops(0), []string{"cache.GetJSON:ak_" + sess.Token})
	var cached model.AccountSession
	if !st.cache.jsonOf("ak_"+sess.Token, &cached) {
		t.Fatalf("前置条件破坏：ak_ 缓存本应还在")
	}
	wantEQ(t, "缓存里仍是改密前那个会话", "token", cached.Token, sess.Token)
	wantEQ(t, "缓存里的会话仍是有效态", "status", cached.Status, model.SessionStatusActive)
}

// TestSetPasswordUnknownMidCreatesOrphanSecret 缺口 16：mid 不校验账号存在性，
// 给不存在的 mid 也能写出一条生效密钥并「成功」返回（全程一次账号表读取都没有）。
func TestSetPasswordUnknownMidCreatesOrphanSecret(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	const ghost int64 = 999999
	nextID := st.secret.peekID()
	st.log.reset()

	_, err := callSetPassword(t, e, ghost, "", "N3w#Pass", "1.2.3.4")
	wantNoErr(t, "不存在的 mid 设密", err)
	wantOps(t, "孤儿密钥写入序列", e.ops(0), []string{
		"secret.FindActive:999999/1",
		"secret.Insert:" + itoa(nextID+1) + "/999999/0",
		"session.RevokeAll:999999",
	})
	wantNoOpsWith(t, "SetPassword 路径", e.ops(0), "account.")
	fresh := activeSecret(t, st, ghost)
	wantEQ(t, "孤儿密钥的 mid", "mid", fresh.Mid, ghost)
	wantEQ(t, "账号表仍然只有 alice", "account 行数", st.account.count(), 1)
}

// TestSetPasswordFindActiveFaultPropagates 读密钥故障原样外传（不降级成领域错误），
// 且发生在开事务之前。
func TestSetPasswordFindActiveFaultPropagates(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	before := activeSecret(t, st, mid)
	boom := errors.New("account/db: secret lookup down")
	st.secret.failWith("FindActive", boom)
	st.log.reset()

	_, err := callSetPassword(t, e, mid, "S3cret!", "N3w#Pass", "1.2.3.4")
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v, want %v", err, boom)
	}
	wantOps(t, "读密钥故障的调用序列", e.ops(0), []string{"secret.FindActive:" + itoa(mid) + "/1"})
	wantNoOpsWith(t, "读密钥故障路径", e.ops(0), "secret.Insert")
	opened, _ := st.conn.txStats()
	wantEQ(t, "读密钥故障不得开事务", "事务数", opened, 0)
	wantEQ(t, "读密钥故障后 hash 不变", "hash", activeSecret(t, st, mid).Hash, before.Hash)
}

// TestSetPasswordWithRSAKeyMismatch 配了 RSA 密钥时旧/新密码都要解密：
// 错配公钥的密文把底层 crypto 错误原样抛给客户端（缺口 10 同源），零写入；
// 空旧密码不进解密就直接判密码错误。
func TestSetPasswordWithRSAKeyMismatch(t *testing.T) {
	e := newEnv(t, withPassportRSA())
	st := e.st
	mid := seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	before := activeSecret(t, st, mid)
	st.log.reset()

	_, err := callSetPassword(t, e, mid, encryptWithOtherKey(t, "S3cret!"), "N3w#Pass", "1.2.3.4")
	wantErr(t, "旧密码用错公钥加密", err)
	if errors.Is(err, ErrLoginPasswordWrong) || errors.Is(err, ErrPasswordRequired) {
		t.Errorf("RSA 解密失败应原样外传，不该被包装成领域错误：%v", err)
	}
	wantOps(t, "RSA 解密失败只走一步", e.ops(0), []string{"secret.FindActive:" + itoa(mid) + "/1"})

	// 新密码用错配公钥：旧密码校验已过，卡在解密这一步（同样零写入）
	st.log.reset()
	_, err = callSetPassword(t, e, mid, "S3cret!", encryptWithOtherKey(t, "N3w#Pass"), "1.2.3.4")
	wantErr(t, "新密码用错公钥加密", err)
	wantCount(t, "解密失败不得写密钥", e.ops(0), "secret.Insert", 0)
	wantEQ(t, "两次 RSA 失败后 hash 不变", "hash", activeSecret(t, st, mid).Hash, before.Hash)

	// 空旧密码在 decryptPassword 里对空串短路（login.go:148-150），不进 RSA 分支
	st.log.reset()
	_, err = callSetPassword(t, e, mid, "", "N3w#Pass", "1.2.3.4")
	wantErrIs(t, "配了 RSA 时空旧密码仍直接判密码错误", err, ErrLoginPasswordWrong)
	wantOps(t, "空旧密码的调用序列", e.ops(0), []string{"secret.FindActive:" + itoa(mid) + "/1"})
	wantEQ(t, "RSA 分支全程没写密钥", "行数", st.secret.count(), 1)
}
