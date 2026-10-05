package logic

// 保留期策略写侧（SaveRetentionPolicy）。
//
// 这张表决定「证据什么时候可以消失」，因此本文件钉的不是「返回了什么」，
// 而是三件只能在副作用序列上才看得出来的事：
//  1. 所有入参与自洽性判定都发生在碰库之前（拒绝不能留下半条策略）；
//  2. 「放宽物理清理窗口」的动机检查在写数据之前 —— 写在后面等于拒了但窗口真的放宽了；
//  3. 应答里的 version 来自回读而不是内存里的结构体（version 由 SQL 的 version+1 决定）。
//
// 失败用例断言的是假库里**真实的残留形态**（假实现不回滚），不是「应当回滚成什么」。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/audit/model"
	"go-video/services/audit/rpc"
)

// retentionReq 组一个合法的新建请求（各用例只覆盖自己关心的字段）。
func retentionReq(requestID, domain string) *rpc.SaveRetentionPolicyReq {
	return &rpc.SaveRetentionPolicyReq{
		Ctx:              testCallContext(requestID),
		ActionDomain:     domain,
		HotDays:          90,
		ArchiveAfterDays: 30,
		DeleteAfterDays:  0,
		State:            0,
		ExpectVersion:    0,
	}
}

func saveRetentionPolicy(t *testing.T, req *rpc.SaveRetentionPolicyReq) (*rpc.SaveRetentionPolicyReply, error) {
	t.Helper()
	return NewSaveRetentionPolicyLogic(context.Background(), svcCtx()).SaveRetentionPolicy(req)
}

// seedMediaPolicy 预置一条 media 策略（真库里由迁移种子建，测试里直接放行）。
func (f *fixture) seedMediaPolicy(hot, archive, deleteAfter int32, version int64) {
	f.putPolicy(&model.RetentionPolicy{
		PolicyID: 3, ActionDomain: "media",
		HotDays: hot, ArchiveAfterDays: archive, DeleteAfterDays: deleteAfter,
		State: model.StateEnable, Version: version, Remark: "初始策略",
	})
}

// lastEntryOf 取链上最后一条条目（自审计写在哪条链由 domain 决定）。
func (f *fixture) lastEntryOf(t *testing.T, chainKey string) *model.AuditEntry {
	t.Helper()
	rows := f.chainOf(chainKey)
	if len(rows) == 0 {
		t.Fatalf("链 %s 上一条条目都没有（自审计留痕缺失）", chainKey)
	}
	return rows[len(rows)-1]
}

// selfChainKey 是自审计落链的链名（domain 固定 system，日期取夹具时钟）。
var selfChainKey = model.ChainKey(selfDomainSystem, testNow)

// --- 1. 守卫：全部在碰库之前 ---

func TestSaveRetentionPolicyGuardsRejectBeforeAnyDependency(t *testing.T) {
	longDomain := strings.Repeat("a", 33)
	longRemark := strings.Repeat("若", model.MaxRemarkBytes+1)
	cases := []struct {
		name  string
		want  error
		mut   func(*rpc.SaveRetentionPolicyReq)
		skipC bool
	}{
		{"域名为空", model.ErrActionDomainInvalid, func(r *rpc.SaveRetentionPolicyReq) { r.ActionDomain = "" }, false},
		{"域名大写", model.ErrActionDomainInvalid, func(r *rpc.SaveRetentionPolicyReq) { r.ActionDomain = "Media" }, false},
		{"域名带连字符", model.ErrActionDomainInvalid, func(r *rpc.SaveRetentionPolicyReq) { r.ActionDomain = "media-x" }, false},
		{"域名超 32 字符", model.ErrActionDomainInvalid, func(r *rpc.SaveRetentionPolicyReq) { r.ActionDomain = longDomain }, false},
		{"archive_after_days=0", model.ErrPolicyDaysInvalid, func(r *rpc.SaveRetentionPolicyReq) { r.ArchiveAfterDays = 0 }, false},
		{"hot_days 小于归档线", model.ErrPolicyDaysInvalid, func(r *rpc.SaveRetentionPolicyReq) { r.HotDays = 10 }, false},
		{"delete_after_days 为负", model.ErrPolicyDaysInvalid, func(r *rpc.SaveRetentionPolicyReq) { r.DeleteAfterDays = -1 }, false},
		{"delete 早于归档线", model.ErrPolicyDaysInvalid, func(r *rpc.SaveRetentionPolicyReq) { r.DeleteAfterDays = 29 }, false},
		{"未知 state", model.ErrPolicyDaysInvalid, func(r *rpc.SaveRetentionPolicyReq) { r.State = 3 }, false},
		{"expect_version 为负", model.ErrRequestRequired, func(r *rpc.SaveRetentionPolicyReq) { r.ExpectVersion = -1 }, false},
		{"remark 超列宽", model.ErrFieldTooLong, func(r *rpc.SaveRetentionPolicyReq) { r.Remark = longRemark }, false},
		{"remark 带手机号", model.ErrDigestLooksPII, func(r *rpc.SaveRetentionPolicyReq) { r.Remark = "联系 13800008000 复核" }, false},
		{"缺 request_id", model.ErrRequestIDRequired, nil, true},
		{"缺 caller_service", model.ErrCallerRequired, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.use(t)
			rec := f.traceAll()

			req := retentionReq("req-guard", "media")
			if tc.mut != nil {
				tc.mut(req)
			}
			switch tc.name {
			case "缺 request_id":
				req.Ctx.RequestId = ""
			case "缺 caller_service":
				req.Ctx.CallerService = "  "
			}

			reply, err := saveRetentionPolicy(t, req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("%s：错误 = %v，期望 %v", tc.name, err, tc.want)
			}
			if reply != nil {
				t.Fatalf("%s：拒绝仍返回 reply %+v", tc.name, reply)
			}
			rec.assertEmpty(t)
			if len(f.policies.byDomain) != 0 {
				t.Fatalf("%s：拒绝却写进了策略表：%v", tc.name, f.policies.inserts)
			}
		})
	}

	t.Run("nil 请求", func(t *testing.T) {
		f := newFixture(t)
		f.use(t)
		rec := f.traceAll()
		_, err := saveRetentionPolicy(t, nil)
		if !errors.Is(err, model.ErrRequestRequired) {
			t.Fatalf("nil 请求：错误 = %v，期望 ErrRequestRequired", err)
		}
		rec.assertEmpty(t)
	})
}

// --- 2. 新建 ---

func TestSaveRetentionPolicyCreateHappyPath(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	rec := f.traceAll()

	reply, err := saveRetentionPolicy(t, retentionReq("req-new", "media"))
	if err != nil {
		t.Fatal(err)
	}
	// 序列：插入 → 回读（库里才是事实）→ 上链留痕。回读必须在留痕之前，
	// 否则 after 摘要记的是内存里的猜测值。
	rec.assert(t, "policies.Insert:media", "policies.FindOne:media", "entries.Insert:"+actionRetentionSave)

	p := reply.GetPolicy()
	if p.GetVersion() != 1 || p.GetState() != model.StateEnable || p.GetActionDomain() != "media" {
		t.Fatalf("新建投影：version=%d state=%d domain=%s", p.GetVersion(), p.GetState(), p.GetActionDomain())
	}
	if p.GetOperatorId() != 7 || p.GetCtime() != testNow {
		t.Fatalf("新建归因/时间：operator=%d ctime=%d", p.GetOperatorId(), p.GetCtime())
	}
	row := f.policies.byDomain["media"]
	if row == nil || row.DeleteAfterDays != 0 || row.HotDays != 90 || row.ArchiveAfterDays != 30 {
		t.Fatalf("库里形态与入参不符：%+v", row)
	}
	entry := f.lastEntryOf(t, selfChainKey)
	assertDims(t, "新建痕 before", entry.BeforeDigest, map[string]string{"action_domain": "media"})
	assertDims(t, "新建痕 after", entry.AfterDigest, map[string]string{
		"action_domain": "media", "hot_days": "90", "archive_after_days": "30",
		"delete_after_days": "0", "state": "1", "version": "1",
	})
	if entry.TargetType != targetRetentionPolicy || entry.TargetID != "media" {
		t.Fatalf("留痕定位：target=%s/%s", entry.TargetType, entry.TargetID)
	}
}

// 首条策略就带 delete_after_days>0 = 从「永不物理清理」变成「会清理」，
// 与更新时放宽同一类，必须给动机；不给则一行都不写。
func TestSaveRetentionPolicyCreateWithoutDeleteNeedsNoRemark(t *testing.T) {
	cases := []struct {
		name    string
		del     int32
		remark  string
		wantErr error
		action  string
	}{
		{"delete=0 不要求 remark", 0, "", nil, actionRetentionSave},
		{"delete>0 无 remark 直接拒绝", 365, "", model.ErrReasonRequired, ""},
		{"delete>0 空格 remark 也算没给", 365, "   ", model.ErrReasonRequired, ""},
		{"delete>0 给了 remark 记 loosen", 365, "法务窗口调整", nil, actionRetentionLoosen},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.use(t)
			rec := f.traceAll()

			req := retentionReq("req-new-del", "media")
			req.DeleteAfterDays, req.Remark = tc.del, tc.remark
			reply, err := saveRetentionPolicy(t, req)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("错误 = %v，期望 %v", err, tc.wantErr)
				}
				// 关键：拒绝发生在 Insert 之前，一条 SQL 都没打。
				rec.assertEmpty(t)
				if len(f.policies.byDomain) != 0 {
					t.Fatalf("拒绝后库里仍有策略行：%+v", f.policies.byDomain)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if reply.GetPolicy().GetDeleteAfterDays() != tc.del {
				t.Fatalf("delete_after_days = %d，期望 %d", reply.GetPolicy().GetDeleteAfterDays(), tc.del)
			}
			rec.assert(t, "policies.Insert:media", "policies.FindOne:media", "entries.Insert:"+tc.action)
			// remark 去空格后入库，回参也必须是被 trim 的那个值。
			if got := reply.GetPolicy().GetRemark(); got != strings.TrimSpace(tc.remark) {
				t.Fatalf("remark = %q，期望 trim 后 %q", got, strings.TrimSpace(tc.remark))
			}
		})
	}
}

// 幂等回放：同 domain 同内容 = 重复提交，返回既有行，不二次插入也不二次留痕。
func TestSaveRetentionPolicyCreateReplayReturnsExistingWithoutSecondWrite(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	f.seedMediaPolicy(90, 30, 0, 4)
	before := *f.policies.byDomain["media"]
	rec := f.traceAll()

	req := retentionReq("req-replay", "media")
	// remark 也在等价性判定里（samePolicy 比 remark）：不带它就成了一次「内容不同」的提交。
	req.Remark = "初始策略"
	reply, err := saveRetentionPolicy(t, req)
	if err != nil {
		t.Fatal(err)
	}
	rec.assert(t, "policies.Insert:media", "policies.FindOne:media")
	if got := reply.GetPolicy().GetVersion(); got != 4 {
		t.Fatalf("回放返回了改写后的 version=%d，期望既有行的 4", got)
	}
	if after := *f.policies.byDomain["media"]; after != before {
		t.Fatalf("回放改了库：before %+v after %+v", before, after)
	}
	if len(f.entries.byID) != 0 {
		t.Fatalf("重复提交又写了一条留痕（链上会凭空多一条“改策略”）：%d 条", len(f.entries.byID))
	}
}

// 同 domain 不同内容不能悄悄覆盖：那会盖掉别人刚收紧的窗口。
func TestSaveRetentionPolicyCreateSameDomainDifferentContentRejects(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	f.seedMediaPolicy(90, 30, 0, 4)
	before := *f.policies.byDomain["media"]
	rec := f.traceAll()

	req := retentionReq("req-conflict", "media")
	req.HotDays = 120 // 内容不等价（放宽热表窗口），但仍撞唯一键
	reply, err := saveRetentionPolicy(t, req)
	if !errors.Is(err, model.ErrPolicyExists) {
		t.Fatalf("错误 = %v，期望 ErrPolicyExists", err)
	}
	if reply != nil {
		t.Fatalf("冲突仍返回 reply %+v", reply)
	}
	rec.assert(t, "policies.Insert:media", "policies.FindOne:media")
	if after := *f.policies.byDomain["media"]; after != before {
		t.Fatalf("冲突却改了既有策略：before %+v after %+v", before, after)
	}
	if len(f.entries.byID) != 0 {
		t.Fatalf("失败的冲突提交写了留痕：%d 条", len(f.entries.byID))
	}
}

// --- 3. 更新 ---

func TestSaveRetentionPolicyUpdateMovesVersionAndDims(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	f.seedMediaPolicy(90, 30, 0, 1)
	rec := f.traceAll()

	req := retentionReq("req-upd", "media")
	req.ExpectVersion = 1
	req.HotDays, req.ArchiveAfterDays = 60, 20
	req.Remark = "存储成本压降"
	reply, err := saveRetentionPolicy(t, req)
	if err != nil {
		t.Fatal(err)
	}
	// 读旧值 → 乐观锁更新 → 回读 → 留痕。
	rec.assert(t, "policies.FindOne:media", "policies.UpdateWithVersion:media@1",
		"policies.FindOne:media", "entries.Insert:"+actionRetentionSave)
	// version 来自回读（SQL 的 version+1），不是内存里那个没被推进的 next。
	if got := reply.GetPolicy().GetVersion(); got != 2 {
		t.Fatalf("回参 version=%d，期望回读到的 2（说明应答取的是内存值）", got)
	}
	entry := f.lastEntryOf(t, selfChainKey)
	assertDims(t, "更新痕 before", entry.BeforeDigest, map[string]string{
		"action_domain": "media", "hot_days": "90", "archive_after_days": "30",
		"delete_after_days": "0", "state": "1", "version": "1",
	})
	assertDims(t, "更新痕 after", entry.AfterDigest, map[string]string{
		"action_domain": "media", "hot_days": "60", "archive_after_days": "20",
		"delete_after_days": "0", "state": "1", "version": "2",
	})
	if entry.Reason != "存储成本压降" {
		t.Fatalf("留痕 reason=%q，应取 remark", entry.Reason)
	}
	row := f.policies.byDomain["media"]
	if row.Operator != 7 || row.Remark != "存储成本压降" {
		t.Fatalf("更新后归因/remark 丢失：operator=%d remark=%q", row.Operator, row.Remark)
	}
}

// 收紧（含 365→0 = 取消物理清理）不需要 remark，也不该被记成 loosen。
func TestSaveRetentionPolicyTightenNeedsNoRemark(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	f.seedMediaPolicy(90, 30, 365, 1)
	rec := f.traceAll()

	req := retentionReq("req-tighten", "media")
	req.ExpectVersion, req.DeleteAfterDays = 1, 0
	if _, err := saveRetentionPolicy(t, req); err != nil {
		t.Fatal(err)
	}
	rec.assert(t, "policies.FindOne:media", "policies.UpdateWithVersion:media@1",
		"policies.FindOne:media", "entries.Insert:"+actionRetentionSave)
	if got := f.policies.byDomain["media"].DeleteAfterDays; got != 0 {
		t.Fatalf("收紧未落库：delete_after_days=%d", got)
	}
}

// 放宽而无动机：拒绝必须发生在 UPDATE 之前 —— 拒了但窗口已放宽是最坏形态。
func TestSaveRetentionPolicyLoosenWithoutRemarkLeavesRowUntouched(t *testing.T) {
	cases := []struct {
		name        string
		oldDelete   int32
		newDelete   int32
		wantUpdated int
	}{
		{"0（永不删）→ 365", 0, 365, 0},
		{"365 → 730（提前清理）", 365, 730, 0},
		{"365 → 365（同值不算放宽）", 365, 365, 1},
		{"730 → 365（收紧）", 730, 365, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.use(t)
			f.seedMediaPolicy(90, 30, tc.oldDelete, 1)
			rec := f.traceAll()

			req := retentionReq("req-loosen", "media")
			req.ExpectVersion, req.DeleteAfterDays = 1, tc.newDelete
			_, err := saveRetentionPolicy(t, req)
			loosening := tc.wantUpdated == 0
			if loosening {
				if !errors.Is(err, model.ErrReasonRequired) {
					t.Fatalf("错误 = %v，期望 ErrReasonRequired", err)
				}
				rec.assert(t, "policies.FindOne:media")
				row := f.policies.byDomain["media"]
				if row.DeleteAfterDays != tc.oldDelete || row.Version != 1 {
					t.Fatalf("拒绝后库里窗口已被放宽：delete=%d version=%d", row.DeleteAfterDays, row.Version)
				}
				if len(f.entries.byID) != 0 {
					t.Fatalf("“拒绝”还留了痕，链上会出现一次没发生的改动：%d 条", len(f.entries.byID))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := f.policies.byDomain["media"].DeleteAfterDays; got != tc.newDelete {
				t.Fatalf("delete_after_days=%d，期望 %d", got, tc.newDelete)
			}
		})
	}
}

func TestSaveRetentionPolicyUpdateGuardsOnExistingRows(t *testing.T) {
	t.Run("domain 不存在", func(t *testing.T) {
		f := newFixture(t)
		f.use(t)
		rec := f.traceAll()

		req := retentionReq("req-nosuch", "media")
		req.ExpectVersion = 3
		_, err := saveRetentionPolicy(t, req)
		if !errors.Is(err, model.ErrPolicyNotFound) {
			t.Fatalf("错误 = %v，期望 ErrPolicyNotFound", err)
		}
		rec.assert(t, "policies.FindOne:media")
		if len(f.policies.byDomain) != 0 {
			t.Fatalf("expect_version!=0 的更新路径却建了新行：%v", f.policies.inserts)
		}
	})

	t.Run("版本不符", func(t *testing.T) {
		f := newFixture(t)
		f.use(t)
		f.seedMediaPolicy(90, 30, 0, 5)
		rec := f.traceAll()

		req := retentionReq("req-ver", "media")
		req.ExpectVersion = 4
		_, err := saveRetentionPolicy(t, req)
		if !errors.Is(err, model.ErrPolicyVersionConflict) {
			t.Fatalf("错误 = %v，期望 ErrPolicyVersionConflict", err)
		}
		if !strings.Contains(err.Error(), "actual_version=5") {
			t.Fatalf("冲突原因必须带上库里的真实版本供调用方重读：%v", err)
		}
		rec.assert(t, "policies.FindOne:media")
	})

	// 读到的版本对得上，但 UPDATE 的 WHERE 未命中（另一个入口刚改过）：
	// 不能伪造成功，也不能留痕。
	t.Run("CAS 未命中", func(t *testing.T) {
		f := newFixture(t)
		f.use(t)
		f.seedMediaPolicy(90, 30, 0, 1)
		f.policies.forceMiss = true
		rec := f.traceAll()

		req := retentionReq("req-cas", "media")
		req.ExpectVersion, req.ArchiveAfterDays = 1, 45
		reply, err := saveRetentionPolicy(t, req)
		if !errors.Is(err, model.ErrPolicyVersionConflict) {
			t.Fatalf("错误 = %v，期望 ErrPolicyVersionConflict", err)
		}
		if reply != nil {
			t.Fatalf("CAS 未命中仍返回 reply %+v", reply)
		}
		rec.assert(t, "policies.FindOne:media", "policies.UpdateWithVersion:media@1")
		row := f.policies.byDomain["media"]
		if row.ArchiveAfterDays != 30 || row.Version != 1 {
			t.Fatalf("CAS 未命中却改了行：archive=%d version=%d", row.ArchiveAfterDays, row.Version)
		}
	})
}

// 停用是契约里唯一的「删除」语义（没有删策略的方法）。
func TestSaveRetentionPolicyDisableIsTheOnlyRetirement(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	f.seedMediaPolicy(90, 30, 0, 1)
	rec := f.traceAll()

	req := retentionReq("req-disable", "media")
	req.ExpectVersion, req.State = 1, model.StateDisable
	reply, err := saveRetentionPolicy(t, req)
	if err != nil {
		t.Fatal(err)
	}
	rec.assert(t, "policies.FindOne:media", "policies.UpdateWithVersion:media@1",
		"policies.FindOne:media", "entries.Insert:"+actionRetentionSave)
	if got := reply.GetPolicy().GetState(); got != model.StateDisable {
		t.Fatalf("state=%d，期望停用 2", got)
	}
	if _, ok := f.policies.byDomain["media"]; !ok {
		t.Fatal("停用被实现成了删行：策略表里的行必须保留（链上还得能追到它曾经存在）")
	}
}

// --- 4. 依赖失败的传播与残留 ---

func TestSaveRetentionPolicyReadAndWriteFailuresPropagate(t *testing.T) {
	t.Run("新建插入失败", func(t *testing.T) {
		f := newFixture(t)
		f.use(t)
		want := errors.New("dial tcp: connection refused")
		f.policies.insertErr = want
		rec := f.traceAll()

		_, err := saveRetentionPolicy(t, retentionReq("req-ins", "media"))
		if !errors.Is(err, want) {
			t.Fatalf("错误 = %v，期望 %v", err, want)
		}
		rec.assert(t, "policies.Insert:media")
	})

	t.Run("新建插入唯一键冲突原样上抛", func(t *testing.T) {
		f := newFixture(t)
		f.use(t)
		f.seedMediaPolicy(90, 30, 999, 7) // 同 domain 不同内容
		f.policies.findErr = model.ErrPolicyNotFound
		rec := f.traceAll()

		_, err := saveRetentionPolicy(t, retentionReq("req-exist", "media"))
		// 回查也失败时必须把两个错误都带出来（errors.Join），不能只留一个。
		if !errors.Is(err, model.ErrPolicyExists) || !errors.Is(err, model.ErrPolicyNotFound) {
			t.Fatalf("错误 = %v，期望同时含 ErrPolicyExists 与回查错误", err)
		}
		rec.assert(t, "policies.Insert:media", "policies.FindOne:media")
	})

	t.Run("读当前策略失败", func(t *testing.T) {
		f := newFixture(t)
		f.use(t)
		f.seedMediaPolicy(90, 30, 0, 1)
		want := errors.New("read timeout")
		f.policies.findErr = want
		rec := f.traceAll()

		req := retentionReq("req-read", "media")
		req.ExpectVersion = 1
		_, err := saveRetentionPolicy(t, req)
		if !errors.Is(err, want) {
			t.Fatalf("错误 = %v，期望 %v", err, want)
		}
		rec.assert(t, "policies.FindOne:media")
		if len(f.policies.updates) != 0 {
			t.Fatalf("读失败之后还去更新：%v", f.policies.updates)
		}
	})

	t.Run("更新失败", func(t *testing.T) {
		f := newFixture(t)
		f.use(t)
		f.seedMediaPolicy(90, 30, 0, 1)
		want := errors.New("deadlock found")
		f.policies.updateErr = want
		rec := f.traceAll()

		req := retentionReq("req-upderr", "media")
		req.ExpectVersion, req.HotDays = 1, 45
		_, err := saveRetentionPolicy(t, req)
		if !errors.Is(err, want) {
			t.Fatalf("错误 = %v，期望 %v", err, want)
		}
		rec.assert(t, "policies.FindOne:media", "policies.UpdateWithVersion:media@1")
		if f.policies.byDomain["media"].HotDays != 90 {
			t.Fatal("更新报错但假库被改了（替身写坏了，后续断言不可信）")
		}
	})

	// 写成功、回读失败：应答是错误，但改动**已经落库**。
	// 调用方拿不到 version，只能重读；这条用例钉住「不会谎报成功，也不会假装没改」。
	t.Run("写成功后回读失败", func(t *testing.T) {
		f := newFixture(t)
		f.use(t)
		f.seedMediaPolicy(90, 30, 0, 1)
		want := errors.New("result format inconsistent")
		f.policies.findErr, f.policies.findErrOnCall = want, 2
		rec := f.traceAll()

		req := retentionReq("req-reread", "media")
		req.ExpectVersion, req.HotDays = 1, 45
		reply, err := saveRetentionPolicy(t, req)
		if !errors.Is(err, want) {
			t.Fatalf("错误 = %v，期望 %v", err, want)
		}
		if reply != nil {
			t.Fatalf("回读失败仍返回 reply %+v", reply)
		}
		rec.assert(t, "policies.FindOne:media", "policies.UpdateWithVersion:media@1", "policies.FindOne:media")
		row := f.policies.byDomain["media"]
		if row.HotDays != 45 || row.Version != 2 {
			t.Fatalf("改动没真的落库（残留 %+v），本用例前提不成立", row)
		}
		if len(f.entries.byID) != 0 {
			t.Fatalf("回读失败后仍写了留痕：%d 条", len(f.entries.byID))
		}
	})

	// 新建路径同理：Insert 之后回读失败 → 行已存在但应答为错。
	t.Run("新建后回读失败", func(t *testing.T) {
		f := newFixture(t)
		f.use(t)
		want := errors.New("connection reset")
		f.policies.findErr = want
		rec := f.traceAll()

		_, err := saveRetentionPolicy(t, retentionReq("req-newread", "media"))
		if !errors.Is(err, want) {
			t.Fatalf("错误 = %v，期望 %v", err, want)
		}
		rec.assert(t, "policies.Insert:media", "policies.FindOne:media")
		if _, ok := f.policies.byDomain["media"]; !ok {
			t.Fatal("策略行不在库里，与生产残留形态不符")
		}
	})
}

// 留痕写失败被 selfAuditLogged 吞掉：策略已经改了，但链上一行都没有，应答仍是成功。
// 这里钉的是**当前真实行为**（README「已知缺口」里已列为待评审），不是「应当如此」。
// 对照：GetAuditExport 对「下载留痕写不上」是 fail-closed 的（ErrDownloadTrailUnwritten），
// 同一个 helper 在两个方向上的取舍正好相反，所以哪一类写「无痕就不该生效」需要评审定夺。
func TestSaveRetentionPolicySwallowsSelfAuditFailure(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	f.seedMediaPolicy(90, 30, 0, 1)
	f.entries.insertErr = errors.New("duplicate entry on audit chain")
	rec := f.traceAll()

	req := retentionReq("req-auditfail", "media")
	req.ExpectVersion, req.HotDays = 1, 45
	reply, err := saveRetentionPolicy(t, req)
	if err != nil {
		t.Fatalf("当前实现吞掉了留痕错误（若本用例变红，说明已改成上抛，请与 README 缺口一起收口）：%v", err)
	}
	if reply.GetPolicy().GetVersion() != 2 {
		t.Fatalf("回参 version=%d", reply.GetPolicy().GetVersion())
	}
	// 轨迹里能看到「尝试上链」这一步（tracedEntries 先记账再调假实现），
	// 但下面断言链上真的零条目：这一步失败了，调用方拿到的却仍是成功应答。
	rec.assert(t, "policies.FindOne:media", "policies.UpdateWithVersion:media@1",
		"policies.FindOne:media", "entries.Insert:"+actionRetentionSave)
	if len(f.entries.byID) != 0 {
		t.Fatalf("留痕其实写成功了，本用例前提不成立：%d 条", len(f.entries.byID))
	}
	if f.policies.byDomain["media"].HotDays != 45 {
		t.Fatal("策略改动未落库，与「已改成功但无痕」的形态不符")
	}
}

// 自审计的 event_id 由 (action, domain, target, request_id, now) 决定：
// 同一个 request_id 重放不会在链上留下两条「改了同一个策略」的痕。
func TestSaveRetentionPolicyReplayOfSameRequestDoesNotDoubleAudit(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	f.seedMediaPolicy(90, 30, 0, 1)

	req := retentionReq("req-twice", "media")
	req.ExpectVersion, req.HotDays = 1, 45
	if _, err := saveRetentionPolicy(t, req); err != nil {
		t.Fatal(err)
	}
	first := len(f.entries.byID)
	if first == 0 {
		t.Fatal("第一次调用没写留痕")
	}

	// 同 request_id 再打一次：expect_version 已被前一次抬到 2，
	// 重放方若不知道新值会撞乐观锁（而不是二次改动）。
	req2 := retentionReq("req-twice", "media")
	req2.ExpectVersion, req2.HotDays = 1, 45
	if _, err := saveRetentionPolicy(t, req2); !errors.Is(err, model.ErrPolicyVersionConflict) {
		t.Fatalf("重放旧版本的错误 = %v，期望乐观锁冲突", err)
	}
	if got := len(f.entries.byID); got != first {
		t.Fatalf("重放又多写一条留痕：%d 条（首发 %d 条）", got, first)
	}
	if fmt.Sprint(f.policies.byDomain["media"].Version) != "2" {
		t.Fatalf("重放把版本抬到了 %d", f.policies.byDomain["media"].Version)
	}
}
