package logic

// checkhistorypasswordlogic_test.go 覆盖 CheckHistoryPassword（repository/login.go:595-617）。
//
// 被测判定链：secret.FindAll(mid, 口令类型) 一次取全量密钥（含历史，SQL 是
// WHERE mid=? AND secret_type=? ORDER BY id DESC，model/secret.go:82-90）
// → decryptPassword（未配 RSA 时按明文）→ 按 ',' 切分入参 → 每个候选逐行比
// MD5(候选 + ">>BiLiSaLt<<" + 行盐) → 命中标 1 否则 0 → ',' 拼回。
//
// 钉住的关键事实：
//   - 「一一对应」是真的：结果元素个数 = 入参逗号段数（含空串与尾逗号产生的空段），
//     空入参也照样回一个 "0" 而不是空串；
//   - 整批只查一次库（无 N+1），且只查口令类型那一列族（其它 secret_type 的行不得参与比对）；
//   - 历史行（status=1）与当前行（status=0）一视同仁地算命中——这正是「历史密码校验」的语义；
//   - 比对用的是**每行自己的盐**，命中即 break，不回显任何盐/哈希；
//   - 配置 RSA 时解密发生在切分**之前**：整串逗号列表是一段密文，解不开就原样报错；
//   - secret.FindAll 的 DB 故障原样外传（logic 层不做错误映射），应答为 nil。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/account/model"
	"go-video/services/account/rpc"
)

func callCheckHistoryPassword(t *testing.T, e *env, mid int64, password string) (*rpc.CheckHistoryPwdReply, error) {
	t.Helper()
	return NewCheckHistoryPasswordLogic(context.Background(), e.svcCtx).CheckHistoryPassword(
		&rpc.CheckHistoryPwdReq{Mid: mid, Password: password})
}

// seedHistorySecret 静默布一条**历史**密钥（status=1），返回其盐。
// 为什么必须有它：正常写路径（SetPassword/ResetPassword）在已有历史行时必然撞
// uk_mid_type_status（见 README 已知缺口 3），所以「两条以上历史行」这种库内状态
// 只能靠 put 直接布，否则多命中/逐行盐的用例根本进不去。
func seedHistorySecret(t *testing.T, st *store, mid int64, password, salt string) {
	t.Helper()
	st.secret.put(&model.AccountSecret{
		Mid: mid, SecretType: model.SecretTypePassword, Salt: salt,
		Hash: seedHash(password, salt), Status: model.SecretStatusHistory,
		CTime: nowUnix(), MTime: nowUnix(),
	})
}

// TestCheckHistoryPasswordOneFlagPerInputPassword 「0,1」序列与入参一一对应，
// 且顺序就是入参顺序（历史行在前、当前行在后都算命中）。
func TestCheckHistoryPasswordOneFlagPerInputPassword(t *testing.T) {
	e := newEnv(t)
	st := e.st
	const phone = capturePhone
	mid := seedUser(t, st, phone, "New#1", model.CredentialTypePhone) // 当前生效口令
	seedHistorySecret(t, st, mid, "Old#1", "salt-old-salt-old-salt-old-01")
	seedHistorySecret(t, st, mid, "Ancient#1", "salt-anc-salt-anc-salt-anc-02")
	st.log.reset()

	reply, err := callCheckHistoryPassword(t, e, mid, "Old#1,New#1,guess#1,Ancient#1")
	wantNoErr(t, "历史密码批量校验", err)
	wantEQ(t, "命中序列", "result", reply.Result, "1,1,0,1")
	wantEQ(t, "序列长度与入参段数一致", "段数", len(strings.Split(reply.Result, ",")), 4)
	wantOps(t, "整批只查一次库", e.ops(0), []string{"secret.FindAll:" + itoa(mid) + "/1"})

	// 单段入参不受批量口径影响
	reply, err = callCheckHistoryPassword(t, e, mid, "Old#1")
	wantNoErr(t, "单个历史口令", err)
	wantEQ(t, "命中序列", "result", reply.Result, "1")

	// 边界对：尾逗号会多出一个空段（客户端拼串时必须自己收口）
	reply, err = callCheckHistoryPassword(t, e, mid, "Old#1,")
	wantNoErr(t, "尾逗号入参", err)
	wantEQ(t, "尾逗号产生的空段也算一段", "result", reply.Result, "1,0")
}

// TestCheckHistoryPasswordEmptyInputAndEmptyHistory 空入参 / 无历史 / mid 不存在
// 三者的真实契约：一律回 "0"（或逐段 0），**不是**空串、也不是错误。
func TestCheckHistoryPasswordEmptyInputAndEmptyHistory(t *testing.T) {
	t.Run("空口令串仍算一段", func(t *testing.T) {
		e := newEnv(t)
		mid := seedUser(t, e.st, capturePhone, "New#1", model.CredentialTypePhone)
		reply, err := callCheckHistoryPassword(t, e, mid, "")
		wantNoErr(t, "空口令", err)
		wantEQ(t, "空串也算一段（strings.Split 不吞空段）", "result", reply.Result, "0")
	})

	t.Run("从未设过密码的账号", func(t *testing.T) {
		e := newEnv(t)
		mid := seedUserWithoutSecret(t, e.st, capturePhone, model.CredentialTypePhone)
		reply, err := callCheckHistoryPassword(t, e, mid, "a,b,c")
		wantNoErr(t, "无密钥账号", err)
		wantEQ(t, "没有任何历史可比 ⇒ 逐段 0", "result", reply.Result, "0,0,0")
		wantOps(t, "仍然只查一次库", e.ops(0), []string{"secret.FindAll:" + itoa(mid) + "/1"})
	})

	t.Run("mid 不存在（含 mid=0）", func(t *testing.T) {
		e := newEnv(t)
		seedUser(t, e.st, capturePhone, "New#1", model.CredentialTypePhone)
		reply, err := callCheckHistoryPassword(t, e, 999999, "New#1")
		wantNoErr(t, "陌生 mid", err)
		wantEQ(t, "陌生 mid 判定未命中", "result", reply.Result, "0")
		reply, err = callCheckHistoryPassword(t, e, 0, "New#1")
		wantNoErr(t, "mid=0 不校验入参", err)
		wantEQ(t, "mid=0 判定未命中", "result", reply.Result, "0")
	})
}

// TestCheckHistoryPasswordUsesRowSaltAndPasswordTypeOnly 两个口径同时钉死：
//   - 每行用的是自己的盐（同一口令在不同盐下都是命中）；
//   - FindAll 带 secret_type=1 过滤：别的密钥类型即使哈希对上也不许算命中。
func TestCheckHistoryPasswordUsesRowSaltAndPasswordTypeOnly(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, capturePhone, "New#1", model.CredentialTypePhone)
	seedHistorySecret(t, st, mid, "Reuse#1", "salt-aaa-salt-aaa-salt-aaa-0001")
	seedHistorySecret(t, st, mid, "Reuse#1", "salt-bbb-salt-bbb-salt-bbb-0002")
	// 别的密钥类型（2）用同一口令 + 同一盐：SQL 不该把它捞进来
	st.secret.put(&model.AccountSecret{
		Mid: mid, SecretType: 2, Salt: "salt-ccc-salt-ccc-salt-ccc-0003",
		Hash:   seedHash("OtherType#1", "salt-ccc-salt-ccc-salt-ccc-0003"),
		Status: model.SecretStatusActive, CTime: nowUnix(), MTime: nowUnix(),
	})
	st.log.reset()

	reply, err := callCheckHistoryPassword(t, e, mid, "Reuse#1,Reuse#1")
	wantNoErr(t, "同一口令两次校验", err)
	wantEQ(t, "逐行盐都要能命中", "result", reply.Result, "1,1")

	reply, err = callCheckHistoryPassword(t, e, mid, "OtherType#1")
	wantNoErr(t, "别的密钥类型的口令", err)
	wantEQ(t, "secret_type 过滤必须生效（跨类型哈希不得算命中）", "result", reply.Result, "0")

	// 应答只许是标志位序列：盐、哈希、口令原文混进来都是外泄
	if strings.Trim(reply.Result, "01,") != "" {
		t.Errorf("result = %q, want 只由 0/1/逗号组成（不得回显盐、哈希或口令原文）", reply.Result)
	}
	for _, s := range st.secret.rowsOf(mid) {
		wantNotContains(t, "应答不得含哈希", reply.Result, s.Hash)
		wantNotContains(t, "应答不得含盐", reply.Result, s.Salt)
	}
}

// TestCheckHistoryPasswordWithRSA decrypt 在 split **之前**：整串逗号列表是一段密文。
// 对照两个方向——配对密钥加密的 "P1,P2" 能拿到 "1,1"；把两段分别加密再拼（客户端常见误用）
// 则整串解不开，直接把底层错误原样外传。
func TestCheckHistoryPasswordWithRSA(t *testing.T) {
	e := newEnv(t, withPassportRSA())
	st := e.st
	mid := seedUser(t, st, capturePhone, "New#1", model.CredentialTypePhone)
	seedHistorySecret(t, st, mid, "Old#1", "salt-rsa-salt-rsa-salt-rsa-00001")
	st.log.reset()

	cipher, encErr := testPassportRSA.CardEncrypt([]byte("Old#1,New#1"))
	wantNoErr(t, "测试侧 RSA 加密", encErr)
	reply, err := callCheckHistoryPassword(t, e, mid, string(cipher))
	wantNoErr(t, "整串密文的历史口令校验", err)
	wantEQ(t, "解密后才切分", "result", reply.Result, "1,1")
	wantOps(t, "调用序列", e.ops(0), []string{"secret.FindAll:" + itoa(mid) + "/1"})

	// 对照：明文入参在配了 RSA 的装配下解不开 ⇒ 原样报错（不静默回 "0,0"）
	st.log.reset()
	bad, err := callCheckHistoryPassword(t, e, mid, "Old#1,New#1")
	wantErr(t, "明文入参在 RSA 装配下必须失败", err)
	if bad != nil {
		t.Errorf("应答 = %+v, want nil", bad)
	}
	wantNotContains(t, "错误报文不得回显口令", errText(err), "Old#1")
}

// TestCheckHistoryPasswordLookupFailurePropagatesRaw secret.FindAll 故障：
// 原样外传、应答为 nil，不许被折成「全未命中」的 "0"（那会让客户端以为口令可以重用）。
func TestCheckHistoryPasswordLookupFailurePropagatesRaw(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, capturePhone, "New#1", model.CredentialTypePhone)
	boom := errors.New("account/db: secret lookup down")
	st.secret.failWith("FindAll", boom)
	st.log.reset()

	reply, err := callCheckHistoryPassword(t, e, mid, "Old#1,New#1")
	if reply != nil {
		t.Errorf("应答 = %+v, want nil（故障绝不能伪装成「未命中」）", reply)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v, want 原样外传 %v", err, boom)
	}
	wantOps(t, "故障路径只查一次库", e.ops(0), []string{"secret.FindAll:" + itoa(mid) + "/1"})
}
