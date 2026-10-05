package logic

// 读侧硬约束：查询窗口、分页夹取、留痕深度、以及「空结果 ≠ 成功」的区分。
//
// audit_entry 是只增大表，读接口的三条约束是接口契约而不是建议：
//  1. 必须给时间范围、跨度不超过 Query.MaxRangeDays、且至少一个能定位少量行的收窄维度
//     ——放开任何一条都等于放行千万级全表扫描；
//  2. ps 由服务端夹到上限、排序键固定在 model 侧，调用方无法指定；
//  3. 查询条件本身是敏感信息（「按手机号查审计」是第二条泄露通道），
//     因此摘要列只记 `字段=16hex`，不记原文；同时读留痕深度恒为 1。
//     例外见 TestKnownGapListTrailStoresRawTargetID：target_id 列仍被原文直填。
//
// 另外把「未命中」与「查不动」严格区分：前者回 found=false，后者必须报错，
// 绝不能把数据面故障降级成「没有这条记录」的空成功。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/audit/internal/repository"
	"go-video/services/audit/model"
	"go-video/services/audit/rpc"
)

// trailOn 取指定自审计动作域上的留痕行（按 seq 升序）。
func (f *fixture) trailOn(t *testing.T, domain string) []*model.AuditEntry {
	t.Helper()
	rows := f.chainOf(model.ChainKey(domain, f.now))
	if len(rows) == 0 {
		t.Fatalf("域 %s 上没有自审计留痕", domain)
	}
	return rows
}

// goodListReq 是一条合法的分页查询请求（收窄维度用 action_domain）。
func goodListReq(requestID string) *rpc.ListAuditEntriesReq {
	return &rpc.ListAuditEntriesReq{
		Ctx:          testCallContext(requestID),
		StartAt:      testNow - 3600,
		EndAt:        testNow,
		ActionDomain: "media",
		Pn:           1,
		Ps:           20,
	}
}

func TestListAuditEntriesRejectsBeforeQuerying(t *testing.T) {
	day := int64(86400)
	cases := []struct {
		name    string
		mutate  func(*rpc.ListAuditEntriesReq)
		wantErr error
	}{
		{"无 start_at", func(r *rpc.ListAuditEntriesReq) { r.StartAt = 0 }, model.ErrQueryRangeRequired},
		{"无 end_at", func(r *rpc.ListAuditEntriesReq) { r.EndAt = 0 }, model.ErrQueryRangeRequired},
		{"区间倒置", func(r *rpc.ListAuditEntriesReq) { r.StartAt, r.EndAt = testNow, testNow-1 },
			model.ErrQueryRangeRequired},
		{"区间相等", func(r *rpc.ListAuditEntriesReq) { r.EndAt = r.StartAt }, model.ErrQueryRangeRequired},
		{"跨度超上限", func(r *rpc.ListAuditEntriesReq) { r.StartAt = testNow - day*120 },
			model.ErrQueryRangeTooWide},
		{"只有时间没有收窄维度", func(r *rpc.ListAuditEntriesReq) { r.ActionDomain = "" },
			model.ErrQueryTooBroad},
		{"只有 result 不算收窄", func(r *rpc.ListAuditEntriesReq) {
			r.ActionDomain = ""
			r.Result = rpc.AuditResult_AUDIT_RESULT_OK
		}, model.ErrQueryTooBroad},
		{"只有 source_app 不算收窄", func(r *rpc.ListAuditEntriesReq) {
			r.ActionDomain = ""
			r.SourceApp = rpc.SourceApp_SOURCE_APP_ANDROID
		}, model.ErrQueryTooBroad},
		{"只有 actor_type 不算收窄", func(r *rpc.ListAuditEntriesReq) {
			r.ActionDomain = ""
			r.ActorType = rpc.ActorType_ACTOR_TYPE_ADMIN
		}, model.ErrQueryTooBroad},
		{"裸 target_id 不算收窄", func(r *rpc.ListAuditEntriesReq) {
			r.ActionDomain = ""
			r.TargetId = "work-1"
		}, model.ErrQueryTooBroad},
		{"未知 actor_type 不当成不过滤", func(r *rpc.ListAuditEntriesReq) {
			r.ActorType = rpc.ActorType(99)
		}, nil},
		{"未知 result 不当成不过滤", func(r *rpc.ListAuditEntriesReq) {
			r.Result = rpc.AuditResult(9)
		}, nil},
		{"未知 source_app 不当成不过滤", func(r *rpc.ListAuditEntriesReq) {
			r.SourceApp = rpc.SourceApp(77)
		}, nil},
		{"负页码", func(r *rpc.ListAuditEntriesReq) { r.Pn = -1 }, model.ErrInvalidPage},
		{"负每页", func(r *rpc.ListAuditEntriesReq) { r.Ps = -5 }, model.ErrInvalidPage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.use(t)
			in := goodListReq("req-list-" + strings.ReplaceAll(tc.name, " ", "-"))
			tc.mutate(in)
			reply, err := NewListAuditEntriesLogic(context.Background(), svcCtx()).ListAuditEntries(in)
			if reply != nil {
				t.Fatalf("被拒的查询不能回响应对象：%+v", reply)
			}
			if err == nil {
				t.Fatal("非法查询必须报错")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，期望 %v", err, tc.wantErr)
			}
			if tc.wantErr == nil {
				// 契约外枚举值必须被点名，不能被静默当成「不过滤」而放大扫描范围。
				if !strings.Contains(err.Error(), "unknown") {
					t.Fatalf("未定义枚举值必须报错并点名：%v", err)
				}
			}
			if len(f.entries.listF) != 0 {
				t.Fatalf("未通过约束的查询打到了库上：%+v", f.entries.listF[0])
			}
			if f.countAll() != 0 {
				t.Fatalf("被拒查询仍写了 %d 行（约束未过不查库，也不写命中数留痕）", f.countAll())
			}
		})
	}
}

func TestListAuditEntriesClampsPagination(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	ctx := context.Background()
	cases := []struct {
		name   string
		pn, ps int32
		wantPN int32
		wantPS int32
	}{
		{"缺省分页", 0, 0, 1, int32(f.opts.MaxPageSize)},
		{"超大 ps 被夹回上限", 1, 100000, 1, int32(f.opts.MaxPageSize)},
		{"合法 ps 生效", 3, 7, 3, 7},
		{"ps=1 不被放大", 1, 1, 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := goodListReq("req-page-" + tc.name)
			in.Pn, in.Ps = tc.pn, tc.ps
			reply, err := NewListAuditEntriesLogic(ctx, svcCtx()).ListAuditEntries(in)
			if err != nil {
				t.Fatal(err)
			}
			if reply.GetPn() != tc.wantPN || reply.GetPs() != tc.wantPS {
				t.Fatalf("回参分页 = %d/%d，期望 %d/%d", reply.GetPn(), reply.GetPs(), tc.wantPN, tc.wantPS)
			}
			// 夹取后的值必须就是下发给 model 的 LIMIT：回参与实际查询不能两套口径。
			got := f.entries.listF[len(f.entries.listF)-1]
			if got.Pn != tc.wantPN || got.Ps != tc.wantPS {
				t.Fatalf("下发 model 的分页 = %+v，与回参不一致", got)
			}
		})
	}
	// 上限由配置改小时，夹取必须跟着配置走（而不是把 100 写死在代码里）。
	f.withOpts(func(o *repository.Options) { o.MaxPageSize = 20 })
	in := goodListReq("req-page-cfg")
	in.Ps = 999
	reply, err := NewListAuditEntriesLogic(ctx, svcCtx()).ListAuditEntries(in)
	if err != nil {
		t.Fatal(err)
	}
	if reply.GetPs() != 20 || f.entries.listF[len(f.entries.listF)-1].Ps != 20 {
		t.Fatalf("MaxPageSize=20 未生效：reply.ps=%d", reply.GetPs())
	}
}

func TestListAuditEntriesProjectsRowsAndReportsRangeLimit(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	rows := seedChain(model.ChainKey("media", testNow), 3, 200)
	f.entries.listRows = []*model.AuditEntry{rows[2], rows[0], rows[1]} // 排序键在 model 侧，logic 不改序
	f.entries.listTotal = 999

	in := goodListReq("req-proj")
	reply, err := NewListAuditEntriesLogic(context.Background(), svcCtx()).ListAuditEntries(in)
	if err != nil {
		t.Fatal(err)
	}
	if reply.GetTotal() != 999 {
		t.Fatalf("total = %d", reply.GetTotal())
	}
	if got := len(reply.GetEntries()); got != 3 {
		t.Fatalf("条数 = %d，期望 3", got)
	}
	for i, v := range reply.GetEntries() {
		want := f.entries.listRows[i]
		if v.GetEntryId() != want.EntryID || v.GetSeq() != want.Seq || v.GetEntryHash() != want.EntryHash {
			t.Fatalf("第 %d 条投影与行不符：%+v vs %+v", i, v, want)
		}
	}
	if want := int64(f.opts.MaxRangeDays) * 86400; reply.GetMaxRangeSeconds() != want {
		t.Fatalf("max_range_seconds = %d，期望 %d", reply.GetMaxRangeSeconds(), want)
	}
	// 归档视图是运维侧条件：调用方无法通过查询参数看/藏已归档行（proto 里根本没有这一维）。
	if got := f.entries.listF[0].Archived; got != 0 {
		t.Fatalf("Archived 被调用方控制：%d", got)
	}
	// 空结果与查不动是两件事：这里 listRows 为空时必须回 0 条 + nil 错误。
	f.entries.listRows = nil
	f.entries.listTotal = 0
	empty, err := NewListAuditEntriesLogic(context.Background(), svcCtx()).ListAuditEntries(goodListReq("req-empty"))
	if err != nil || empty.GetTotal() != 0 || len(empty.GetEntries()) != 0 {
		t.Fatalf("空结果应回 0 条不报错：%+v %v", empty, err)
	}
}

func TestListAuditEntriesPropagatesStoreError(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	f.entries.listErr = errors.New("audit_entry List: Error 1146: table doesn't exist")
	reply, err := NewListAuditEntriesLogic(context.Background(), svcCtx()).ListAuditEntries(goodListReq("req-err"))
	if err == nil {
		t.Fatal("数据面故障必须报错，不能降级成空列表")
	}
	if reply != nil {
		t.Fatalf("报错时不得回响应对象：%+v", reply)
	}
	// 失败路径不写「命中 0 条」的留痕：那会把一次故障记成一次成功的查看。
	if got := len(f.chainOf(model.ChainKey(selfDomainDataAccess, testNow))); got != 0 {
		t.Fatalf("查询失败仍写了成功留痕：%d 行", got)
	}
}

func TestListAuditEntriesTrailHashesFilterValues(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	// 「按手机号查审计」这个查询条件本身就是敏感信息：允许查，但只允许以摘要形态留痕。
	const rawPhone = "13800138000"
	in := goodListReq("req-pii-trail")
	in.ActionDomain = ""
	in.TargetType = "member"
	in.TargetId = rawPhone
	in.TraceId = "trace-手机号-13800138000"
	if _, err := NewListAuditEntriesLogic(context.Background(), svcCtx()).ListAuditEntries(in); err != nil {
		t.Fatal(err)
	}
	row := f.trailOn(t, selfDomainDataAccess)[0]
	if row.Action != actionEntryList || row.CallerService != selfCallerService {
		t.Fatalf("留痕归因异常：%+v", row)
	}
	if !model.ValidDigest(row.BeforeDigest) {
		t.Fatalf("查询条件摘要不在白名单内：%s", row.BeforeDigest)
	}
	// 维度数 = 实际生效的过滤条件数（start_at/end_at/target_type/target_id/trace_id/page_size）。
	if n := len(strings.Split(row.BeforeDigest, ";")); n < 5 {
		t.Fatalf("留痕维度太少，条件被吞了：%s", row.BeforeDigest)
	}
	secrets := []string{rawPhone, "member"}
	// 摘要列一个字都不该有原文：digestPairs 把每个维度值压成 `键=16hex`。
	for _, s := range secrets {
		if strings.Contains(row.BeforeDigest, s) || strings.Contains(row.AfterDigest, s) {
			t.Fatalf("查询条件以原文进了摘要列 before=%q after=%q", row.BeforeDigest, row.AfterDigest)
		}
	}
	// 留痕行其余文本列也不得出现原文。target_id 是唯一的例外，且是**已知缺口**，
	// 单独由 TestKnownGapListTrailStoresRawTargetID 钉住（list 留痕把调用方给的
	// target_id 原文直填进 audit_entry.target_id 列）。
	fields := stringFields(row)
	delete(fields, "TargetID")
	assertNoRaw(t, "自审计行", fields, secrets...)
	// 留痕自身也是链上条目：可复算、序号从 1 起。
	if row.EntryHash != model.ComputeEntryHash(row) || row.Seq != 1 {
		t.Fatalf("留痕条目不在链上：%+v", row)
	}
	// 递归深度恒为 1：写留痕这条路径不得再触发任何读接口。
	if got := f.countAll(); got != 1 {
		t.Fatalf("一次查询写了 %d 行，自审计深度不再是 1", got)
	}
}

// TestKnownGapListTrailStoresRawTargetID 钉住一个真实缺口（不是想要的行为，是现状）：
// ListAuditEntries 的自审计用 firstNonEmpty(filter.TargetID, ...) 直填留痕的 target_id
// 列，所以「按手机号查审计」会把手机号原文写进 audit_entry.target_id。
// 摘要列已经做了 16hex 脱敏，唯独这一列漏了 —— 而它恰恰是最容易被回看的一列。
//
// 这个用例故意断言「原文确实在列里」。修好那天它会失败，届时要同步：
//  1. 把 target_id 改为摘要（或新增 target_id_hash 列，与 ip_hash/device_hash 同构）；
//  2. 删掉本用例并把 TestListAuditEntriesTrailHashesFilterValues 里
//     delete(fields, "TargetID") 的豁免去掉；
//  3. 更新 services/audit/README.md 的「已知缺口」。
func TestKnownGapListTrailStoresRawTargetID(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	const rawPhone = "13800138000"
	in := goodListReq("req-known-gap-target-id")
	in.ActionDomain = ""
	in.TargetType = "member"
	in.TargetId = rawPhone
	if _, err := NewListAuditEntriesLogic(context.Background(), svcCtx()).ListAuditEntries(in); err != nil {
		t.Fatal(err)
	}
	row := f.trailOn(t, selfDomainDataAccess)[0]
	if row.TargetID != rawPhone {
		t.Fatalf("缺口行为已变化（当前实现把 target_id 原文写进留痕列）：实得 %q。"+
			"若是已修复，请按本用例注释同步删除豁免并更新 README。", row.TargetID)
	}
	// 同时钉住「另一条通道是干净的」：reason 与摘要列都没有原文。
	if strings.Contains(row.Reason, rawPhone) {
		t.Fatalf("reason 列也漏了：%s", row.Reason)
	}
}

func TestSelfAuditDepthStaysOneOnEveryReadPath(t *testing.T) {
	ctx := context.Background()
	t.Run("查单条", func(t *testing.T) {
		f := newFixture(t)
		f.use(t)
		f.putRows(seedChain(model.ChainKey("media", testNow), 1, 300)...)
		if _, err := NewGetAuditEntryLogic(ctx, svcCtx()).
			GetAuditEntry(&rpc.GetAuditEntryReq{Ctx: testCallContext("req-depth-get"), EntryId: 301}); err != nil {
			t.Fatal(err)
		}
		// 读一条存证只该多出「留痕」这一行（被读的那行业务存证不算）。
		if got := len(f.chainOf(model.ChainKey(selfDomainDataAccess, testNow))); got != 1 {
			t.Fatalf("读一条留痕写了 %d 行", got)
		}
		if got := f.countAll(); got != 2 {
			t.Fatalf("库里共 %d 行，期望 1 行业务存证 + 1 行留痕", got)
		}
	})
	t.Run("查列表", func(t *testing.T) {
		f := newFixture(t)
		f.use(t)
		if _, err := NewListAuditEntriesLogic(ctx, svcCtx()).ListAuditEntries(goodListReq("req-depth-list")); err != nil {
			t.Fatal(err)
		}
		if got := f.countAll(); got != 1 {
			t.Fatalf("读列表留痕写了 %d 行", got)
		}
	})
	t.Run("校验链", func(t *testing.T) {
		f := newFixture(t)
		f.use(t)
		key := model.ChainKey("media", testNow)
		rows := seedChain(key, 2, 400)
		f.putHead(key, 2, rows[1].EntryHash)
		f.entries.rangeRows = rows
		if _, err := NewVerifyAuditChainLogic(ctx, svcCtx()).
			VerifyAuditChain(&rpc.VerifyAuditChainReq{Ctx: testCallContext("req-depth-verify"), ChainKey: key}); err != nil {
			t.Fatal(err)
		}
		// 校验链本身只落在 system 域，且只有一条。
		if got := len(f.chainOf(model.ChainKey(selfDomainSystem, testNow))); got != 1 {
			t.Fatalf("一次校验写了 %d 条留痕", got)
		}
	})
}

func TestGetAuditEntryRequiresLocator(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	ctx := context.Background()
	cases := []struct {
		name    string
		in      *rpc.GetAuditEntryReq
		wantErr error
	}{
		{"空请求", nil, model.ErrRequestRequired},
		{"两个键都不给", &rpc.GetAuditEntryReq{Ctx: testCallContext("r1")}, model.ErrEntryIDOrEventIDRequired},
		{"entry_id 为 0 且无 event_id", &rpc.GetAuditEntryReq{Ctx: testCallContext("r2"), EntryId: 0},
			model.ErrEntryIDOrEventIDRequired},
		{"entry_id 为负", &rpc.GetAuditEntryReq{Ctx: testCallContext("r3"), EntryId: -7},
			model.ErrEntryIDOrEventIDRequired},
		{"只有空白 event_id", &rpc.GetAuditEntryReq{Ctx: testCallContext("r4"), EventId: "   "},
			model.ErrEntryIDOrEventIDRequired},
		{"缺 caller 归因", &rpc.GetAuditEntryReq{Ctx: &rpc.CallContext{RequestId: "r5"}, EntryId: 1},
			model.ErrCallerRequired},
		{"ctx 整个缺失", &rpc.GetAuditEntryReq{EntryId: 1}, model.ErrRequestRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply, err := NewGetAuditEntryLogic(ctx, svcCtx()).GetAuditEntry(tc.in)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，期望 %v", err, tc.wantErr)
			}
			if reply != nil {
				t.Fatalf("拒绝时不得回响应：%+v", reply)
			}
		})
	}
}

func TestGetAuditEntryDistinguishesMissFromFailure(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	ctx := context.Background()
	key := model.ChainKey("media", testNow)
	f.putRows(seedChain(key, 2, 500)...)

	t.Run("未命中回 found=false", func(t *testing.T) {
		reply, err := NewGetAuditEntryLogic(ctx, svcCtx()).
			GetAuditEntry(&rpc.GetAuditEntryReq{Ctx: testCallContext("req-miss"), EntryId: 999})
		if err != nil {
			t.Fatalf("未命中不是调用失败：%v", err)
		}
		if reply.GetFound() || reply.GetEntry() != nil {
			t.Fatalf("未命中却回了条目：%+v", reply)
		}
		// 连续探测不存在的存证也要能回看，因此未命中同样留痕。
		trail := f.trailOn(t, selfDomainDataAccess)
		last := trail[len(trail)-1]
		if last.Action != actionEntryGet || last.TargetID != "999" {
			t.Fatalf("未命中留痕异常：%+v", last)
		}
	})

	t.Run("数据面故障必须报错", func(t *testing.T) {
		f.entries.findErr = errors.New("audit_entry FindOne: Error 1146: table doesn't exist")
		defer func() { f.entries.findErr = nil }()
		reply, err := NewGetAuditEntryLogic(ctx, svcCtx()).
			GetAuditEntry(&rpc.GetAuditEntryReq{Ctx: testCallContext("req-dberr"), EntryId: 501})
		if err == nil {
			t.Fatal("库故障必须报错，不能当成「没有这条存证」")
		}
		if reply != nil {
			t.Fatalf("报错时不得回响应：%+v", reply)
		}
	})

	t.Run("event_id 命中", func(t *testing.T) {
		reply, err := NewGetAuditEntryLogic(ctx, svcCtx()).
			GetAuditEntry(&rpc.GetAuditEntryReq{Ctx: testCallContext("req-event"), EventId: "ev-" + key + "-2"})
		if err != nil {
			t.Fatal(err)
		}
		if !reply.GetFound() || reply.GetEntry().GetEntryId() != 502 {
			t.Fatalf("event_id 定位失败：%+v", reply)
		}
	})

	t.Run("entry_id 优先于 event_id", func(t *testing.T) {
		reply, err := NewGetAuditEntryLogic(ctx, svcCtx()).GetAuditEntry(&rpc.GetAuditEntryReq{
			Ctx: testCallContext("req-both"), EntryId: 501, EventId: "不存在的键",
		})
		if err != nil {
			t.Fatal(err)
		}
		if !reply.GetFound() || reply.GetEntry().GetEntryId() != 501 {
			t.Fatalf("entry_id 未优先：%+v", reply)
		}
	})
}

func TestGetAuditEntryTrailRecordsHashNotContent(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	key := model.ChainKey("media", testNow)
	target := &model.AuditEntry{
		EntryID: 700, EventID: "ev-secret", SchemaVersion: model.SchemaVersion, ChainKey: key, Seq: 1,
		ActorType: model.ActorAdmin, ActorID: 7, ActorName: "运营小王", Action: "user.phone_change",
		ActionDomain: "user", TargetType: "member", TargetID: "13800138000", Result: model.ResultOK,
		SourceApp: model.SourceAdminWeb, Reason: "把手机号改成 13800138000 并通知本人",
		BeforeDigest: "phone=" + strings.Repeat("a", 16), AfterDigest: "phone=" + strings.Repeat("b", 16),
		OccurredAt: testNow, PrevHash: model.GenesisHash(key), Ctime: testNow,
	}
	target.EntryHash = model.ComputeEntryHash(target)
	f.putRows(target)

	reply, err := NewGetAuditEntryLogic(context.Background(), svcCtx()).
		GetAuditEntry(&rpc.GetAuditEntryReq{Ctx: testCallContext("req-trail"), EntryId: 700})
	if err != nil {
		t.Fatal(err)
	}
	if !reply.GetFound() {
		t.Fatal("应命中")
	}
	trail := f.trailOn(t, selfDomainDataAccess)[0]
	// 留痕记的是「看的是哪条存证」（entry_hash 的摘要），不是存证内容。
	if trail.BeforeDigest == "" || !model.ValidDigest(trail.BeforeDigest) {
		t.Fatalf("留痕缺少可核对的存证摘要：%q", trail.BeforeDigest)
	}
	if !strings.HasPrefix(trail.BeforeDigest, "entry_hash=") {
		t.Fatalf("留痕维度应是 entry_hash，实得 %q", trail.BeforeDigest)
	}
	if trail.BeforeDigest == "entry_hash="+trail.TargetID || strings.Contains(trail.BeforeDigest, target.EntryHash) {
		t.Fatalf("留痕把存证摘要按原文抄了一遍：%s", trail.BeforeDigest)
	}
	secrets := []string{target.Reason, "13800138000"}
	assertNoRaw(t, "读留痕", stringFields(trail), secrets...)
	// 被查条目本身一个字都没动。
	if after := *f.entries.byID[700]; after.EntryHash != target.EntryHash || after.ArchivedAt != 0 {
		t.Fatalf("读操作改写了存证：%+v", after)
	}
	// 读留痕也不得落进被查条目的业务链（否则「有没有人看过」会污染当日业务链尾）。
	if got := len(f.chainOf(key)); got != 1 {
		t.Fatalf("业务链被读留痕污染：%d 行", got)
	}
}

func TestListFiltersRejectUndefinedStates(t *testing.T) {
	ctx := context.Background()
	t.Run("导出任务状态", func(t *testing.T) {
		f := newFixture(t)
		f.use(t)
		for _, s := range []string{"done", "DONE", "succeded", "全部"} {
			reply, err := NewListAuditExportsLogic(ctx, svcCtx()).
				ListAuditExports(&rpc.ListAuditExportsReq{Ctx: testCallContext("req-exp-" + s), State: s})
			if err == nil || !errors.Is(err, model.ErrTaskBadTransition) {
				t.Fatalf("state=%q err = %v，期望 ErrTaskBadTransition", s, err)
			}
			if reply != nil {
				t.Fatalf("拒绝时不得回响应：%+v", reply)
			}
		}
		// 合法状态逐个放过，且拼出的 filter 与入参一致。
		for _, s := range []string{"", model.ExportStatePending, model.ExportStateRunning,
			model.ExportStateSucceeded, model.ExportStateFailed, model.ExportStateExpired, model.ExportStateCanceled} {
			if _, err := NewListAuditExportsLogic(ctx, svcCtx()).
				ListAuditExports(&rpc.ListAuditExportsReq{Ctx: testCallContext("req-exp-ok-" + s), State: s}); err != nil {
				t.Fatalf("合法状态 %q 被拒：%v", s, err)
			}
		}
		if got := f.exports.listF[len(f.exports.listF)-1].State; got != model.ExportStateCanceled {
			t.Fatalf("下发送 model 的 state = %q", got)
		}
		if _, err := NewListAuditExportsLogic(ctx, svcCtx()).
			ListAuditExports(&rpc.ListAuditExportsReq{Ctx: testCallContext("req-exp-neg"), OperatorId: -1}); !errors.Is(err, model.ErrActorIDInvalid) {
			t.Fatalf("负 operator_id err = %v", err)
		}
		if _, err := NewListAuditExportsLogic(ctx, svcCtx()).
			ListAuditExports(&rpc.ListAuditExportsReq{
				Ctx: testCallContext("req-exp-window"), StartAt: testNow, EndAt: testNow - 1,
			}); !errors.Is(err, model.ErrQueryRangeRequired) {
			t.Fatalf("ctime 区间倒置 err = %v", err)
		}
	})
	t.Run("归档批次状态与链名", func(t *testing.T) {
		f := newFixture(t)
		f.use(t)
		if _, err := NewListArchiveBatchesLogic(ctx, svcCtx()).
			ListArchiveBatches(&rpc.ListArchiveBatchesReq{Ctx: testCallContext("req-bad-state"), State: "archived"}); !errors.Is(err, model.ErrTaskBadTransition) {
			t.Fatalf("err = %v，期望 ErrTaskBadTransition", err)
		}
		// 只给动作域不当成 chain_key：前缀匹配会让运维以为整域都归档了。
		for _, ck := range []string{"media", "media/", "media/2026", "MEDIA/2026-03-05", "media/2026-03-05/x"} {
			_, err := NewListArchiveBatchesLogic(ctx, svcCtx()).
				ListArchiveBatches(&rpc.ListArchiveBatchesReq{Ctx: testCallContext("req-bad-chain"), ChainKey: ck})
			if !errors.Is(err, model.ErrChainKeyRequired) {
				t.Fatalf("chain_key=%q err = %v，期望 ErrChainKeyRequired", ck, err)
			}
		}
		if len(f.archives.listF) != 0 {
			t.Fatalf("非法 chain_key 仍下发查询：%+v", f.archives.listF[0])
		}
		for _, ck := range []string{"", model.ChainKey("media", testNow)} {
			if _, err := NewListArchiveBatchesLogic(ctx, svcCtx()).
				ListArchiveBatches(&rpc.ListArchiveBatchesReq{Ctx: testCallContext("req-ok-chain-" + ck), ChainKey: ck}); err != nil {
				t.Fatalf("合法 chain_key %q 被拒：%v", ck, err)
			}
		}
	})
	t.Run("保留策略状态", func(t *testing.T) {
		f := newFixture(t)
		f.use(t)
		for _, s := range []int32{3, -1, 99} {
			if _, err := NewListRetentionPoliciesLogic(ctx, svcCtx()).
				ListRetentionPolicies(&rpc.ListRetentionPoliciesReq{Ctx: testCallContext("req-pol"), State: s}); !errors.Is(err, model.ErrPolicyDaysInvalid) {
				t.Fatalf("state=%d err = %v，期望 ErrPolicyDaysInvalid", s, err)
			}
		}
		f.policies.listRows = []*model.RetentionPolicy{{PolicyID: 1, ActionDomain: model.DefaultPolicyDomain,
			HotDays: 90, ArchiveAfterDays: 30, State: model.StateEnable, Version: 1}}
		f.policies.listTotal = 1
		reply, err := NewListRetentionPoliciesLogic(ctx, svcCtx()).
			ListRetentionPolicies(&rpc.ListRetentionPoliciesReq{Ctx: testCallContext("req-pol-ok"), State: 0})
		if err != nil {
			t.Fatal(err)
		}
		if reply.GetTotal() != 1 || reply.GetItems()[0].GetActionDomain() != model.DefaultPolicyDomain {
			t.Fatalf("投影异常：%+v", reply)
		}
		// 读策略不写留痕（策略是参数不是存证内容），因此库里一行都不该有。
		if got := f.countAll(); got != 0 {
			t.Fatalf("读策略写了 %d 行留痕", got)
		}
	})
}

func TestReadPathsReportPaginationFromSameSeam(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.use(t)
	f.withOpts(func(o *repository.Options) { o.MaxPageSize = 50 })
	if _, err := NewListAuditExportsLogic(ctx, svcCtx()).
		ListAuditExports(&rpc.ListAuditExportsReq{Ctx: testCallContext("req-exp-page"), Ps: 5000}); err != nil {
		t.Fatal(err)
	}
	if got := f.exports.listF[0].Ps; got != 50 {
		t.Fatalf("导出列表 ps 未夹取：%d", got)
	}
	if _, err := NewListArchiveBatchesLogic(ctx, svcCtx()).
		ListArchiveBatches(&rpc.ListArchiveBatchesReq{Ctx: testCallContext("req-batch-page"), Ps: 5000}); err != nil {
		t.Fatal(err)
	}
	if got := f.archives.listF[0].Ps; got != 50 {
		t.Fatalf("归档列表 ps 未夹取：%d", got)
	}
	// 同一套夹取规则也作用于策略列表（三处共用 repository.ClampPage，不各写一份）。
	if _, err := NewListRetentionPoliciesLogic(ctx, svcCtx()).
		ListRetentionPolicies(&rpc.ListRetentionPoliciesReq{Ctx: testCallContext("req-pol-page"), Ps: 5000}); err != nil {
		t.Fatal(err)
	}
}

func TestMaxRangeBoundaryIsInclusive(t *testing.T) {
	f := newFixture(t)
	f.use(t)
	maxSpan := int64(f.opts.MaxRangeDays) * 86400
	// 每个收窄维度单独给出都必须算「收窄」，否则运营按类型排查会被拒。
	narrowing := []func(*rpc.ListAuditEntriesReq){
		func(r *rpc.ListAuditEntriesReq) { r.ActorId = 7 },
		func(r *rpc.ListAuditEntriesReq) { r.Action = "media.approve" },
		func(r *rpc.ListAuditEntriesReq) { r.ActionDomain = "media" },
		func(r *rpc.ListAuditEntriesReq) { r.TraceId = "t-1" },
		func(r *rpc.ListAuditEntriesReq) { r.TargetType = "work" },
		func(r *rpc.ListAuditEntriesReq) { r.TargetType, r.TargetId = "work", "work-1" },
	}
	for i, mut := range narrowing {
		in := goodListReq(fmt.Sprintf("req-narrow-%d", i))
		in.ActionDomain = ""
		mut(in)
		in.StartAt, in.EndAt = testNow-maxSpan, testNow // 正好等于上限必须放过
		if _, err := NewListAuditEntriesLogic(context.Background(), svcCtx()).ListAuditEntries(in); err != nil {
			t.Fatalf("第 %d 个收窄维度/边界跨度被误拒：%v", i, err)
		}
	}
	// 再宽一秒就要拒。
	in := goodListReq("req-too-wide")
	in.StartAt, in.EndAt = testNow-maxSpan-1, testNow
	if _, err := NewListAuditEntriesLogic(context.Background(), svcCtx()).ListAuditEntries(in); !errors.Is(err, model.ErrQueryRangeTooWide) {
		t.Fatalf("超出上限一秒必须被拒：%v", err)
	}
}
