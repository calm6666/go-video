package logic

// client_secret 生命周期两个方法（轮换 / 吊销）的行为测试。
//
// 覆盖的契约点：
//   - proto:224-242 轮换的宽限期语义（grace_seconds=0 立即失效，>0 到点失效）、
//     README:52「本方法不幂等，以最后一次为准」；
//   - proto:244-259 吊销只影响签名调用、绝不影响已签发 token；
//   - proto:252 reason 必填（审计）；
//   - 明文只出现一次（只在新密钥的返回值里，库里只有 salt+hash）；
//   - 门禁失败与依赖故障一律零副作用。

import (
	"context"
	"testing"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"
)

func rotateFixture(t *testing.T) (*store, *svc.ServiceContext, int64) {
	t.Helper()
	db := newStore()
	seedApp(db, testAppID, testOwner, testScopeR)
	secret := seedSecret(t, db, testAppID, "old-secret-plaintext")
	return db, newTestSvc(db), secret.SecretID
}

func TestRotateApplicationSecret_GraceKeepsOldUsableUntilDeadline(t *testing.T) {
	db, s, oldID := rotateFixture(t)
	before := db.count("Secrets.InsertTx")
	vBefore := db.apps[testAppID].Version

	reply, err := NewRotateApplicationSecretLogic(context.Background(), s).
		RotateApplicationSecret(&rpc.RotateApplicationSecretReq{
			AppId: testAppID, OperatorMid: testOwner, Reason: "例行轮换", GraceSeconds: 3600,
		})
	wantOK(t, reply, err, "宽限期轮换")

	if reply.OldSecretId != oldID {
		t.Fatalf("OldSecretId=%d，期望被替换的是 %d", reply.OldSecretId, oldID)
	}
	if reply.ClientSecret == "" || len(reply.ClientSecret) != credentialBytes*2 {
		t.Fatalf("新明文密钥形态异常：len=%d", len(reply.ClientSecret))
	}
	// 旧密钥：仍是生效位，但到期时刻被前移到 now+grace（宽限期 = 旧密钥还能验签的窗口）。
	old := db.secrets[oldID]
	if old.Status != model.SecretStatusActive {
		t.Fatalf("宽限期内旧密钥 status=%d，应为 ACTIVE", old.Status)
	}
	if old.ExpiresAt <= reply.RotatedAt || old.ExpiresAt > reply.RotatedAt+3600 {
		t.Fatalf("旧密钥到期=%d，应落在 (%d, %d]", old.ExpiresAt, reply.RotatedAt, reply.RotatedAt+3600)
	}
	if reply.OldSecretExpiresAt != old.ExpiresAt {
		t.Fatalf("回显 OldSecretExpiresAt=%d 与库里 %d 不一致", reply.OldSecretExpiresAt, old.ExpiresAt)
	}
	// 新密钥：独立行、生效、审计列落到新行（谁在什么时候为什么换的）。
	wantCalls(t, db, "Secrets.InsertTx", before, 1, "轮换")
	newRow := db.secrets[reply.SecretId]
	if newRow == nil || newRow.AppID != testAppID || newRow.Status != model.SecretStatusActive {
		t.Fatalf("新密钥行未正确落库：%+v", newRow)
	}
	if newRow.RotateReason != "例行轮换" || newRow.OperatorMid != testOwner {
		t.Fatalf("新密钥审计列缺失：reason=%q operator=%d", newRow.RotateReason, newRow.OperatorMid)
	}
	// 库里绝不存明文：新行 hash 是掺 salt 的 HMAC，且明文无法由行内字段还原。
	if newRow.Hash == reply.ClientSecret || newRow.Salt == "" {
		t.Fatalf("新密钥行疑似存了明文或空 salt：salt=%q", newRow.Salt)
	}
	if want := mustHashSecret(t, newRow.Salt, reply.ClientSecret); want != newRow.Hash {
		t.Fatalf("新密钥哈希与返回明文不匹配：调用方拿到的密钥验不了签")
	}
	// 版本前移：让 token 校验侧看到「密钥集已变」（proto 声明的失效链路）。
	if got := db.apps[testAppID].Version; got != vBefore+1 {
		t.Fatalf("app.version=%d，期望 %d", got, vBefore+1)
	}
}

func TestRotateApplicationSecret_ZeroGraceMeansImmediate(t *testing.T) {
	db, s, oldID := rotateFixture(t)

	reply, err := NewRotateApplicationSecretLogic(context.Background(), s).
		RotateApplicationSecret(&rpc.RotateApplicationSecretReq{
			AppId: testAppID, OperatorMid: testOwner, Reason: "疑似泄露，立即下线", GraceSeconds: 0,
		})
	wantOK(t, reply, err, "grace=0 轮换")

	if db.secrets[oldID].Status != model.SecretStatusHistory {
		t.Fatalf("grace=0 时旧密钥必须当场进历史，实际 status=%d", db.secrets[oldID].Status)
	}
	// 回显必须是时间点而不是 0：0 在契约里表示「无到期」，回 0 会被读成「长期有效」。
	if reply.OldSecretExpiresAt != reply.RotatedAt {
		t.Fatalf("grace=0 时 OldSecretExpiresAt=%d，应等于 rotated_at=%d",
			reply.OldSecretExpiresAt, reply.RotatedAt)
	}
	// 立刻没有可验签的旧密钥：FindActive 只剩新密钥。
	list, err := s.Secrets.FindActive(context.Background(), testAppID, reply.RotatedAt+1)
	wantOK(t, list, err, "FindActive")
	if len(list) != 1 || list[0].SecretID != reply.SecretId {
		t.Fatalf("宽限期后生效密钥集=%+v，期望只剩新密钥", list)
	}
}

func TestRotateApplicationSecret_RejectsOfflineApp(t *testing.T) {
	db, s, _ := rotateFixture(t)
	db.apps[testAppID].Status = model.AppStatusOffline // proto:34 终态，无出边

	before := snapshotWrites(db)
	reply, err := NewRotateApplicationSecretLogic(context.Background(), s).
		RotateApplicationSecret(&rpc.RotateApplicationSecretReq{
			AppId: testAppID, OperatorMid: testOwner, Reason: "给死身份续密钥", GraceSeconds: 60,
		})
	wantFail(t, reply, err, model.ErrInvalidStateTransition, "下线应用轮换")
	wantNoWrites(t, db, before, "下线应用轮换")
}

func TestRotateApplicationSecret_GateFailuresAreSideEffectFree(t *testing.T) {
	cases := []struct {
		name  string
		in    *rpc.RotateApplicationSecretReq
		want  error
		tweak func(db *store, s *svc.ServiceContext)
	}{
		{"缺 reason", &rpc.RotateApplicationSecretReq{AppId: testAppID, OperatorMid: testOwner},
			errReasonRequired, nil},
		{"reason 超长", &rpc.RotateApplicationSecretReq{AppId: testAppID, OperatorMid: testOwner,
			Reason: string(make([]rune, maxReasonRunes+1))}, errReasonTooLong, nil},
		{"grace 负数", &rpc.RotateApplicationSecretReq{AppId: testAppID, OperatorMid: testOwner,
			Reason: "r", GraceSeconds: -1}, errGraceTooLong, nil},
		{"grace 超上界", &rpc.RotateApplicationSecretReq{AppId: testAppID, OperatorMid: testOwner,
			Reason: "r", GraceSeconds: maxRotationGraceSeconds + 1}, errGraceTooLong, nil},
		{"app_id 非法", &rpc.RotateApplicationSecretReq{OperatorMid: testOwner, Reason: "r"},
			model.ErrInvalidAppID, nil},
		{"应用不存在", &rpc.RotateApplicationSecretReq{AppId: 555, OperatorMid: testOwner, Reason: "r"},
			model.ErrAppNotFound, nil},
		{"非归属者", &rpc.RotateApplicationSecretReq{AppId: testAppID, OperatorMid: 424242, Reason: "r"},
			model.ErrOwnerRequired, nil},
		{"匿名运营", &rpc.RotateApplicationSecretReq{AppId: testAppID, IsOperator: true, Reason: "r"},
			model.ErrOwnerRequired, nil},
		{"缺 pepper 时不得签发弱哈希密钥", &rpc.RotateApplicationSecretReq{
			AppId: testAppID, OperatorMid: testOwner, Reason: "r"},
			model.ErrSecretVerificationUnavailable,
			func(_ *store, s *svc.ServiceContext) { s.Config.Security.CredentialPepper = "" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, s, _ := rotateFixture(t)
			if c.tweak != nil {
				c.tweak(db, s)
			}
			before := snapshotWrites(db)
			reply, err := NewRotateApplicationSecretLogic(context.Background(), s).
				RotateApplicationSecret(c.in)
			wantFail(t, reply, err, c.want, c.name)
			wantNoWrites(t, db, before, c.name)
		})
	}
}

func TestRotateApplicationSecret_ReadFailureAndTxFailureLeaveNothingBehind(t *testing.T) {
	t.Run("读现存密钥失败", func(t *testing.T) {
		db, s, oldID := rotateFixture(t)
		db.failOn("Secrets.FindActive", errFakeDown)
		before := snapshotWrites(db)
		reply, err := NewRotateApplicationSecretLogic(context.Background(), s).
			RotateApplicationSecret(&rpc.RotateApplicationSecretReq{
				AppId: testAppID, OperatorMid: testOwner, Reason: "r", GraceSeconds: 60})
		if err == nil || !isNilPtr(reply) {
			t.Fatalf("依赖故障必须回错且无响应，实际 err=%v reply=%v", err, reply)
		}
		wantNoWrites(t, db, before, "读现存密钥失败")
		if db.secrets[oldID].Status != model.SecretStatusActive {
			t.Fatalf("失败后旧密钥被动过")
		}
	})

	t.Run("新密钥落库失败则整笔回滚", func(t *testing.T) {
		db, s, oldID := rotateFixture(t)
		db.failOn("Secrets.InsertTx", errFakeDown)
		vBefore := db.apps[testAppID].Version
		_, err := NewRotateApplicationSecretLogic(context.Background(), s).
			RotateApplicationSecret(&rpc.RotateApplicationSecretReq{
				AppId: testAppID, OperatorMid: testOwner, Reason: "r", GraceSeconds: 60})
		if err == nil {
			t.Fatalf("事务内写失败必须回错")
		}
		// 旧密钥必须还停在原到期时刻：MarkHistoryTx 即便先执行也被回滚。
		if db.secrets[oldID].Status != model.SecretStatusActive || db.secrets[oldID].ExpiresAt != 0 {
			t.Fatalf("回滚不彻底，旧密钥被改脏：%+v", db.secrets[oldID])
		}
		if len(db.secrets) != 1 {
			t.Fatalf("回滚后仍留下 %d 行密钥", len(db.secrets))
		}
		wantCalls(t, db, "Apps.NextVersion", 0, 0, "事务失败不得前移版本")
		if db.apps[testAppID].Version != vBefore {
			t.Fatalf("app.version 被改脏")
		}
	})

	t.Run("废旧密钥失败则整笔回滚", func(t *testing.T) {
		db, s, oldID := rotateFixture(t)
		db.failOn("Secrets.MarkHistory", errFakeDown)
		_, err := NewRotateApplicationSecretLogic(context.Background(), s).
			RotateApplicationSecret(&rpc.RotateApplicationSecretReq{
				AppId: testAppID, OperatorMid: testOwner, Reason: "r", GraceSeconds: 60})
		if err == nil {
			t.Fatalf("事务内写失败必须回错")
		}
		if len(db.secrets) != 1 {
			t.Fatalf("回滚不彻底：剩 %d 行密钥（新密钥未回滚 = 留下无人知道的密钥）", len(db.secrets))
		}
		if db.secrets[oldID].ExpiresAt != 0 {
			t.Fatalf("回滚不彻底：旧密钥到期被改成 %d", db.secrets[oldID].ExpiresAt)
		}
	})

	t.Run("写令牌耗尽时零副作用", func(t *testing.T) {
		db, _, _ := rotateFixture(t)
		s := limitedSvc(db)
		before := snapshotWrites(db)
		reply, err := NewRotateApplicationSecretLogic(context.Background(), s).
			RotateApplicationSecret(&rpc.RotateApplicationSecretReq{
				AppId: testAppID, OperatorMid: testOwner, Reason: "r", GraceSeconds: 60})
		wantFail(t, reply, err, model.ErrRateLimited, "限流")
		wantNoWrites(t, db, before, "限流")
	})
}

// README:52 声明本方法「不幂等，以最后一次为准」：契约里没有 request_id，
// 因此重放的定义只能是「再换一把」，第二次必须以第一次的新密钥为旧密钥。
// 这条测试同时锁住宽限期的叠加口径：旧密钥只被缩短，不会被延长。
func TestRotateApplicationSecret_TwiceInARowChainIsBounded(t *testing.T) {
	db, s, firstOld := rotateFixture(t)

	first, err := NewRotateApplicationSecretLogic(context.Background(), s).
		RotateApplicationSecret(&rpc.RotateApplicationSecretReq{
			AppId: testAppID, OperatorMid: testOwner, Reason: "第一次", GraceSeconds: 7200})
	wantOK(t, first, err, "第一次轮换")
	second, err := NewRotateApplicationSecretLogic(context.Background(), s).
		RotateApplicationSecret(&rpc.RotateApplicationSecretReq{
			AppId: testAppID, OperatorMid: testOwner, Reason: "第二次", GraceSeconds: 60})
	wantOK(t, second, err, "第二次轮换")

	if second.OldSecretId != first.SecretId {
		t.Fatalf("第二次应替换第一把新密钥：%d vs %d", second.OldSecretId, first.SecretId)
	}
	if first.ClientSecret == second.ClientSecret {
		t.Fatalf("两次轮换发出同一把明文密钥")
	}
	// 第一次那把的 deadline 被第二次统一到新的更短宽限期（只缩短，不延长）。
	if got := db.secrets[firstOld].ExpiresAt; got > second.RotatedAt+60 {
		t.Fatalf("最早那把的宽限期未被收敛：%d > %d", got, second.RotatedAt+60)
	}
	// 明文只出现一次：两次响应互不相同，库里两行哈希也互不相同。
	if db.secrets[first.SecretId].Hash == db.secrets[second.SecretId].Hash {
		t.Fatalf("两次轮换落库哈希相同（salt 或明文生成有缺陷）")
	}
	// 生效密钥数始终收敛到 1（新密钥）：宽限期内的旧密钥到期后不再出现。
	list, err := s.Secrets.FindActive(context.Background(), testAppID, second.RotatedAt+61)
	wantOK(t, list, err, "FindActive")
	if len(list) != 1 || list[0].SecretID != second.SecretId {
		t.Fatalf("宽限期结束后生效密钥=%+v，期望只剩最后一把", list)
	}
}

// ---------------------------------------------------------------- 吊销

func TestRevokeApplicationSecret_SingleIsTerminalAndLeavesTokens(t *testing.T) {
	db, s, secretID := rotateFixture(t)
	// 一条已签发的 token：proto:245-246 明确吊销密钥不影响它。
	grant := seedGrant(db, testAppID, testMid, []string{testScopeR}, 1)
	_, _, tok := seedToken(t, db, grant, []string{testScopeR}, 10, 3600, 2592000)

	before := snapshotWrites(db)
	reply, err := NewRevokeApplicationSecretLogic(context.Background(), s).
		RevokeApplicationSecret(&rpc.RevokeApplicationSecretReq{
			AppId: testAppID, SecretId: secretID, OperatorMid: testOwner, Reason: "泄露应急"})
	wantOK(t, reply, err, "吊销单把")
	if reply.Revoked != 1 {
		t.Fatalf("首次吊销 Revoked=%d，期望 1", reply.Revoked)
	}
	row := db.secrets[secretID]
	if row.Status != model.SecretStatusHistory || row.ExpiresAt != 0 {
		t.Fatalf("密钥未进终态：status=%d expires_at=%d", row.Status, row.ExpiresAt)
	}
	// 终态不可逆：model 侧没有任何复活方法，这里断言「不留宽限窗口」。
	if reply.EffectiveAt <= 0 {
		t.Fatalf("EffectiveAt 未回填")
	}
	wantCalls(t, db, "Secrets.MarkHistory", before["Secrets.MarkHistory"], 1, "吊销单把")
	for _, op := range []string{"Tokens.MarkRevoked", "Tokens.RevokeByGrant", "Grants.MarkRevoked"} {
		wantCalls(t, db, op, before[op], 0, "吊销密钥不得动 token/"+op)
	}
	if db.tokens[tok.TokenID].State != model.TokenStateActive {
		t.Fatalf("已签发 token 被密钥吊销波及：state=%d", db.tokens[tok.TokenID].State)
	}

	// 幂等重放：第二次 revoked=0 且仍回成功（撤销可重入），不报错。
	again, err := NewRevokeApplicationSecretLogic(context.Background(), s).
		RevokeApplicationSecret(&rpc.RevokeApplicationSecretReq{
			AppId: testAppID, SecretId: secretID, OperatorMid: testOwner, Reason: "重复提交"})
	wantOK(t, again, err, "重复吊销")
	if again.Revoked != 0 {
		t.Fatalf("重复吊销 Revoked=%d，期望 0（终态不可逆，第二次是 0 行变更）", again.Revoked)
	}
}

func TestRevokeApplicationSecret_AllTargetsOnlyThisApp(t *testing.T) {
	db, s, idA1 := rotateFixture(t)
	seedSecret(t, db, testAppID, "second-active-secret")
	other := seedSecret(t, db, testApp2, "another-app-secret")
	before := db.count("Secrets.MarkAllHistory")

	reply, err := NewRevokeApplicationSecretLogic(context.Background(), s).
		RevokeApplicationSecret(&rpc.RevokeApplicationSecretReq{
			AppId: testAppID, SecretId: 0, OperatorMid: testOwner, Reason: "紧急下架"})
	wantOK(t, reply, err, "吊销全部")
	if reply.Revoked != 2 {
		t.Fatalf("Revoked=%d，期望 2", reply.Revoked)
	}
	wantCalls(t, db, "Secrets.MarkAllHistory", before, 1, "吊销全部")
	if db.secrets[idA1].Status != model.SecretStatusHistory {
		t.Fatalf("本应用密钥未全部下线")
	}
	// 越界保护：另一个应用的密钥必须毫发无损（app_id 是 WHERE 条件）。
	if db.secrets[other.SecretID].Status != model.SecretStatusActive {
		t.Fatalf("越权改到了 app_id=%d 的密钥", testApp2)
	}
	// 应急路径的原因与操作者逐行可查（MarkAllHistory 会把审计列写进每一行）。
	if db.secrets[idA1].RotateReason != "紧急下架" || db.secrets[idA1].OperatorMid != testOwner {
		t.Fatalf("吊销审计列缺失：reason=%q operator=%d",
			db.secrets[idA1].RotateReason, db.secrets[idA1].OperatorMid)
	}
	// 重复调用：影响 0 行，仍回成功。
	again, err := NewRevokeApplicationSecretLogic(context.Background(), s).
		RevokeApplicationSecret(&rpc.RevokeApplicationSecretReq{
			AppId: testAppID, OperatorMid: testOwner, Reason: "重复提交"})
	wantOK(t, again, err, "重复吊销全部")
	if again.Revoked != 0 {
		t.Fatalf("重复吊销全部 Revoked=%d，期望 0", again.Revoked)
	}
}

func TestRevokeApplicationSecret_GateAndOwnership(t *testing.T) {
	cases := []struct {
		name  string
		in    *rpc.RevokeApplicationSecretReq
		want  error
		tweak func(db *store, s *svc.ServiceContext)
	}{
		{"缺 reason", &rpc.RevokeApplicationSecretReq{AppId: testAppID, OperatorMid: testOwner},
			errReasonRequired, nil},
		{"匿名撤销", &rpc.RevokeApplicationSecretReq{AppId: testAppID, Reason: "r"},
			model.ErrOwnerRequired, nil},
		{"非归属者", &rpc.RevokeApplicationSecretReq{AppId: testAppID, OperatorMid: 424242, Reason: "r"},
			model.ErrOwnerRequired, nil},
		{"应用不存在", &rpc.RevokeApplicationSecretReq{AppId: 555, OperatorMid: testOwner, Reason: "r"},
			model.ErrAppNotFound, nil},
		{"secret_id 不存在是参数错", &rpc.RevokeApplicationSecretReq{
			AppId: testAppID, SecretId: 9999, OperatorMid: testOwner, Reason: "r"},
			model.ErrSecretNotConfigured, nil},
		{"跨应用 secret_id 是越权", &rpc.RevokeApplicationSecretReq{
			AppId: testAppID, SecretId: 777, OperatorMid: testOwner, Reason: "r"},
			model.ErrOwnerRequired, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, s, _ := rotateFixture(t)
			if c.name == "跨应用 secret_id 是越权" {
				other := seedSecret(t, db, testApp2, "x")
				c.in.SecretId = other.SecretID
			}
			if c.tweak != nil {
				c.tweak(db, s)
			}
			before := snapshotWrites(db)
			reply, err := NewRevokeApplicationSecretLogic(context.Background(), s).
				RevokeApplicationSecret(c.in)
			wantFail(t, reply, err, c.want, c.name)
			wantNoWrites(t, db, before, c.name)
		})
	}
}

func TestRevokeApplicationSecret_FailClosedOnDownstream(t *testing.T) {
	t.Run("全部吊销读库失败", func(t *testing.T) {
		db, s, secretID := rotateFixture(t)
		db.failOn("Secrets.MarkAllHistory", errFakeDown)
		before := snapshotWrites(db)
		reply, err := NewRevokeApplicationSecretLogic(context.Background(), s).
			RevokeApplicationSecret(&rpc.RevokeApplicationSecretReq{
				AppId: testAppID, OperatorMid: testOwner, Reason: "r"})
		if err == nil || !isNilPtr(reply) {
			t.Fatalf("依赖故障必须回错，实际 err=%v", err)
		}
		if db.secrets[secretID].Status != model.SecretStatusActive {
			t.Fatalf("失败后密钥状态被改脏")
		}
		wantCalls(t, db, "Apps.NextVersion", before["Apps.NextVersion"], 0, "失败不得前移版本")
	})

	t.Run("指定吊销写库失败", func(t *testing.T) {
		db, s, secretID := rotateFixture(t)
		db.failOn("Secrets.MarkHistory", errFakeDown)
		_, err := NewRevokeApplicationSecretLogic(context.Background(), s).
			RevokeApplicationSecret(&rpc.RevokeApplicationSecretReq{
				AppId: testAppID, SecretId: secretID, OperatorMid: testOwner, Reason: "r"})
		if err == nil {
			t.Fatalf("写失败必须回错")
		}
		wantCalls(t, db, "Apps.NextVersion", 0, 0, "写失败不得前移版本")
	})

	t.Run("版本前移失败必须回错", func(t *testing.T) {
		db, s, _ := rotateFixture(t)
		db.failOn("Apps.NextVersion", errFakeDown)
		_, err := NewRevokeApplicationSecretLogic(context.Background(), s).
			RevokeApplicationSecret(&rpc.RevokeApplicationSecretReq{
				AppId: testAppID, OperatorMid: testOwner, Reason: "r"})
		if err == nil {
			t.Fatalf("NextVersion 失败必须回错（否则校验侧看不到密钥集已变）")
		}
	})
}

func mustHashSecret(t *testing.T, salt, plain string) string {
	t.Helper()
	h, err := model.HashSecret(testPepper, salt, plain)
	wantOK(t, h, err, "HashSecret")
	return h
}
