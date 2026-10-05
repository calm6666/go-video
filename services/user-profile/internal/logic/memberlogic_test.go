package logic

// memberlogic_test.go 覆盖 Member 与 Members（基础 + 等级 + 官方认证的聚合读）。
//
// 聚合接口最容易出事的是「局部失败怎么算」，这里钉四条：
//  1. 单用户 Member：基础资料查无此人 → **ErrMemberNotExist 错误**，且不再读经验
//     （错误要早、代价要小）；经验读失败 → **降级为零值等级 + 不报错**，
//     基础资料与认证照给（资料页不能因为等级挂了整页打不开）。
//  2. 批量 Members：库里没有的 mid 补 mid=0 占位，**不跳过**（与 Member 的报错口径不对称，
//     这条不对称是有意为之，必须被钉住，改一处就要看到另一处红）。
//  3. 批量 Members 的经验整批失败 → 所有人 LevelInfo 为零值但不报错。
//  4. 官方认证只来自**构造期载入的内存快照**，每次请求都不得再查 user_official。

import (
	"context"
	"errors"
	"testing"

	"go-video/services/user-profile/internal/repository"
	"go-video/services/user-profile/model"
	"go-video/services/user-profile/rpc"
)

func TestMemberGuardInheritedFromBaseInfo(t *testing.T) {
	for _, tc := range []struct {
		label string
		mid   int64
	}{
		{"mid=0", 0},
		{"mid=-1", -1},
	} {
		e := newEnv(t)
		l := NewMemberLogic(context.Background(), e.svcCtx)
		reply, err := l.Member(&rpc.MemberMidReq{Mid: tc.mid})
		wantErrIs(t, tc.label, err, repository.ErrRequestErr)
		if reply != nil {
			t.Errorf("%s：拒绝后仍返回 %+v", tc.label, reply)
		}
		wantNoCall(t, tc.label, e.st, 0)
	}
}

func TestMemberUnknownUserFailsBeforeReadingExp(t *testing.T) {
	e := newEnv(t) // 缓存与库里都没有 60001

	l := NewMemberLogic(context.Background(), e.svcCtx)
	reply, err := l.Member(&rpc.MemberMidReq{Mid: 60001})
	wantErrIs(t, "查无此人", err, repository.ErrMemberNotExist)
	if reply != nil {
		t.Errorf("查无此人：reply = %+v, want nil", reply)
	}
	wantOps(t, "查无此人", e.ops(0), []string{
		"cache.GetJSON:bs_60001",
		"base.FindOne:60001",
		"cache.SetJSON:bs_60001/3600",
	})
	if n := e.st.log.countPrefix("exp."); n != 0 {
		t.Errorf("查无此人：仍然去读了经验 %d 次，want 0", n)
	}
}

func TestMemberAggregatesBaseLevelAndOfficial(t *testing.T) {
	e := newEnv(t, withOfficial(60002, model.OfficialRoleGov, "政务蓝V", "本地政务发布"))
	e.st.cache.warmJSON(keyBase(60002), baseCachePayload{Cached: true, baseCacheValue: baseCacheValue{
		Mid: 60002, Name: "蓝V用户", Sex: 2, Face: "https://i.example.com/60002.png",
		Sign: "权威发布", Rank: 20000, Birthday: 1600000000,
	}})
	e.st.cache.warmInt(keyExp(60002), 150000) // 1500 分 → 3 级

	l := NewMemberLogic(context.Background(), e.svcCtx)
	reply, err := l.Member(&rpc.MemberMidReq{Mid: 60002})
	wantNoErr(t, "全量聚合", err)
	wantOps(t, "全量聚合", e.ops(0), []string{"cache.GetJSON:bs_60002", "cache.GetInt:exp_60002"})

	base := reply.GetBaseInfo()
	wantEQ(t, "全量聚合", "base.mid", base.GetMid(), int64(60002))
	wantEQ(t, "全量聚合", "base.name", base.GetName(), "蓝V用户")
	wantEQ(t, "全量聚合", "base.sex", base.GetSex(), int64(2))
	wantEQ(t, "全量聚合", "base.face", base.GetFace(), "https://i.example.com/60002.png")
	wantEQ(t, "全量聚合", "base.sign", base.GetSign(), "权威发布")
	wantEQ(t, "全量聚合", "base.rank", base.GetRank(), int64(20000))
	wantEQ(t, "全量聚合", "base.birthday", base.GetBirthday(), int64(1600000000))

	level := reply.GetLevelInfo()
	wantEQ(t, "全量聚合", "level.cur", level.GetCur(), int32(3))
	wantEQ(t, "全量聚合", "level.min", level.GetMin(), int32(1500))
	wantEQ(t, "全量聚合", "level.now_exp（Member 走 Exp，带当前经验）", level.GetNowExp(), int32(1500))
	wantEQ(t, "全量聚合", "level.next_exp", level.GetNextExp(), int32(4500))

	official := reply.GetOfficialInfo()
	if official == nil {
		t.Fatal("全量聚合：official_info 为空，快照没生效")
	}
	wantEQ(t, "全量聚合", "official.role", official.GetRole(), int32(model.OfficialRoleGov))
	wantEQ(t, "全量聚合", "official.title", official.GetTitle(), "政务蓝V")
	wantEQ(t, "全量聚合", "official.desc", official.GetDesc(), "本地政务发布")

	// 认证只能来自内存快照：请求期不得再查 user_official。
	if n := e.st.log.countPrefix("official."); n != 0 {
		t.Errorf("全量聚合：请求期查了认证表 %d 次，want 0", n)
	}
}

func TestMemberWithoutOfficialLeavesNilNotZeroStruct(t *testing.T) {
	e := newEnv(t, withOfficial(60003, model.OfficialRoleUp, "别人是认证号", "x"))
	e.st.cache.warmJSON(keyBase(60004), baseCachePayload{Cached: true, baseCacheValue: baseCacheValue{Mid: 60004, Name: "无名氏"}})
	e.st.cache.warmInt(keyExp(60004), 300)

	l := NewMemberLogic(context.Background(), e.svcCtx)
	reply, err := l.Member(&rpc.MemberMidReq{Mid: 60004})
	wantNoErr(t, "未认证", err)
	if reply.GetOfficialInfo() != nil {
		t.Errorf("未认证：official_info = %+v, want nil（零值结构体会被上游当成「已认证」）", reply.GetOfficialInfo())
	}
	wantEQ(t, "未认证", "base.name", reply.GetBaseInfo().GetName(), "无名氏")
	wantEQ(t, "未认证", "level.cur（3 分仍是 1 级）", reply.GetLevelInfo().GetCur(), int32(1))
	wantOps(t, "未认证", e.ops(0), []string{"cache.GetJSON:bs_60004", "cache.GetInt:exp_60004"})
}

func TestMemberDegradesWhenExpFails(t *testing.T) {
	e := newEnv(t, withOfficial(60005, model.OfficialRoleBusiness, "企业认证", "某某公司"))
	e.st.base.put(&model.UserBase{Mid: 60005, Name: "资料还在", Sex: 1, Sign: "签名", Rank: 30000, Birthday: 100})
	boom := errors.New("select from user_exp: deadlock")
	e.st.exp.failWith("FindOne", boom)

	l := NewMemberLogic(context.Background(), e.svcCtx)
	reply, err := l.Member(&rpc.MemberMidReq{Mid: 60005})
	wantNoErr(t, "经验故障降级", err)
	wantOps(t, "经验故障降级", e.ops(0), []string{
		"cache.GetJSON:bs_60005",
		"base.FindOne:60005",
		"cache.SetJSON:bs_60005/3600",
		"cache.GetInt:exp_60005",
		"exp.FindOne:60005",
	})
	wantEQ(t, "经验故障降级", "base.name 必须保住", reply.GetBaseInfo().GetName(), "资料还在")
	wantEQ(t, "经验故障降级", "base.rank", reply.GetBaseInfo().GetRank(), int64(30000))
	level := reply.GetLevelInfo()
	if level == nil {
		t.Fatal("经验故障降级：level_info 为 nil，上游 deref 会 panic")
	}
	wantEQ(t, "经验故障降级", "降级 cur", level.GetCur(), int32(0))
	wantEQ(t, "经验故障降级", "降级 min", level.GetMin(), int32(0))
	wantEQ(t, "经验故障降级", "降级 now_exp", level.GetNowExp(), int32(0))
	wantEQ(t, "经验故障降级", "降级 next_exp", level.GetNextExp(), int32(0))
	wantEQ(t, "经验故障降级", "official.title 不受影响", reply.GetOfficialInfo().GetTitle(), "企业认证")
}

func TestMemberBaseFailurePropagates(t *testing.T) {
	e := newEnv(t)
	boom := errors.New("dial tcp 10.0.0.9:3306: refused")
	e.st.base.failWith("FindOne", boom)

	l := NewMemberLogic(context.Background(), e.svcCtx)
	reply, err := l.Member(&rpc.MemberMidReq{Mid: 60006})
	wantErrIs(t, "基础资料故障", err, boom)
	if reply != nil {
		t.Errorf("基础资料故障：reply = %+v, want nil", reply)
	}
	wantOps(t, "基础资料故障", e.ops(0), []string{"cache.GetJSON:bs_60006", "base.FindOne:60006"})
}

// === Members ===

func TestMembersGuards(t *testing.T) {
	t.Run("超过 100 个 mid 直接拒绝", func(t *testing.T) {
		e := newEnv(t)
		mids := make([]int64, 101)
		for i := range mids {
			mids[i] = int64(70000 + i)
		}
		l := NewMembersLogic(context.Background(), e.svcCtx)
		reply, err := l.Members(&rpc.MemberMidsReq{Mids: mids})
		wantErrIs(t, "101 个 mid", err, repository.ErrMemberOverLimit)
		if reply != nil {
			t.Errorf("101 个 mid：reply = %+v, want nil", reply)
		}
		wantNoCall(t, "101 个 mid", e.st, 0)
	})

	t.Run("空列表零调用", func(t *testing.T) {
		for _, tc := range []struct {
			label string
			mids  []int64
		}{
			{"nil", nil},
			{"空切片", []int64{}},
		} {
			e := newEnv(t)
			l := NewMembersLogic(context.Background(), e.svcCtx)
			reply, err := l.Members(&rpc.MemberMidsReq{Mids: tc.mids})
			wantNoErr(t, tc.label, err)
			if reply.GetMemberInfos() == nil {
				t.Errorf("%s：member_infos 为 nil", tc.label)
			}
			wantEQ(t, tc.label, "条数", len(reply.GetMemberInfos()), 0)
			wantNoCall(t, tc.label, e.st, 0)
		}
	})
}

func TestMembersAggregatesInBaseThenExpOrder(t *testing.T) {
	e := newEnv(t, withOfficial(70101, model.OfficialRoleMedia, "媒体认证号", "某某传媒"))
	// 70101 基础资料命中缓存、经验 miss 回源；70102 双向都 miss；70103 库里没有。
	e.st.cache.warmJSON(keyBase(70101), baseCachePayload{Cached: true, baseCacheValue: baseCacheValue{
		Mid: 70101, Name: "传媒号", Sex: 2, Rank: 25000, Birthday: 1700000000}})
	e.st.base.put(&model.UserBase{Mid: 70102, Name: "回源号", Sex: 1, Rank: 5000, Birthday: -28800})
	e.st.exp.put(70101, 20000)   // 200 分 → 2 级
	e.st.exp.put(70102, 2880000) // 28800 分 → 满级

	l := NewMembersLogic(context.Background(), e.svcCtx)
	reply, err := l.Members(&rpc.MemberMidsReq{Mids: []int64{70101, 70102, 70103}})
	wantNoErr(t, "批量聚合", err)
	wantOps(t, "批量聚合", e.ops(0), []string{
		"cache.GetJSON:bs_70101",
		"cache.GetJSON:bs_70102",
		"cache.GetJSON:bs_70103",
		"base.FindMany:70102,70103",
		"cache.SetJSON:bs_70102/3600",
		"cache.SetJSON:bs_70103/3600",
		"cache.GetInt:exp_70101",
		"cache.GetInt:exp_70102",
		"cache.GetInt:exp_70103",
		"exp.FindMany:70101,70102,70103",
		"cache.SetInt:exp_70101/86400",
		"cache.SetInt:exp_70102/86400",
		"cache.SetInt:exp_70103/86400",
	})
	wantInt64EQ(t, "批量聚合", "基础资料只回源 miss", e.st.base.lastMany, []int64{70102, 70103})
	wantInt64EQ(t, "批量聚合", "经验整批都回源（批量侧不做逐个缓存读之外的裁剪）", e.st.exp.lastMany, []int64{70101, 70102, 70103})

	members := reply.GetMemberInfos()
	wantEQ(t, "批量聚合", "条数", len(members), 3)

	one := members[70101]
	wantEQ(t, "批量聚合", "70101 name", one.GetBaseInfo().GetName(), "传媒号")
	wantEQ(t, "批量聚合", "70101 cur", one.GetLevelInfo().GetCur(), int32(2))
	wantEQ(t, "批量聚合", "70101 min", one.GetLevelInfo().GetMin(), int32(200))
	wantEQ(t, "批量聚合", "70101 now_exp", one.GetLevelInfo().GetNowExp(), int32(200))
	wantEQ(t, "批量聚合", "70101 next_exp", one.GetLevelInfo().GetNextExp(), int32(1500))
	wantEQ(t, "批量聚合", "70101 official.role", one.GetOfficialInfo().GetRole(), int32(model.OfficialRoleMedia))
	wantEQ(t, "批量聚合", "70101 official.title", one.GetOfficialInfo().GetTitle(), "媒体认证号")
	wantEQ(t, "批量聚合", "70101 official.desc", one.GetOfficialInfo().GetDesc(), "某某传媒")

	two := members[70102]
	wantEQ(t, "批量聚合", "70102 name", two.GetBaseInfo().GetName(), "回源号")
	wantEQ(t, "批量聚合", "70102 满级 cur", two.GetLevelInfo().GetCur(), int32(6))
	wantEQ(t, "批量聚合", "70102 满级 min", two.GetLevelInfo().GetMin(), int32(28800))
	wantEQ(t, "批量聚合", "70102 满级 next_exp = -1", two.GetLevelInfo().GetNextExp(), int32(model.LevelMax))
	if two.GetOfficialInfo() != nil {
		t.Errorf("批量聚合：70102 不该有认证信息，got %+v", two.GetOfficialInfo())
	}

	// 与单用户 Member 的**不对称**口径：批量侧库里没有也返回占位条目，不报 ErrMemberNotExist。
	three, ok := members[70103]
	if !ok {
		t.Fatal("批量聚合：70103 被跳过了，上游按下标取会缺项")
	}
	wantEQ(t, "批量聚合", "70103 占位 base.mid", three.GetBaseInfo().GetMid(), int64(0))
	wantEQ(t, "批量聚合", "70103 占位 name", three.GetBaseInfo().GetName(), "")
	wantEQ(t, "批量聚合", "70103 等级 cur", three.GetLevelInfo().GetCur(), int32(0))
	wantEQ(t, "批量聚合", "70103 等级 next_exp", three.GetLevelInfo().GetNextExp(), int32(1))
}

func TestMembersDegradesWhenExpBatchFails(t *testing.T) {
	e := newEnv(t, withOfficial(70201, model.OfficialRoleUp, "UP主", "个人UP"))
	e.st.cache.warmJSON(keyBase(70201), baseCachePayload{Cached: true, baseCacheValue: baseCacheValue{Mid: 70201, Name: "甲", Rank: 10000}})
	e.st.cache.warmJSON(keyBase(70202), baseCachePayload{Cached: true, baseCacheValue: baseCacheValue{Mid: 70202, Name: "乙", Rank: 10001}})
	boom := errors.New("select from user_exp: broken pipe")
	e.st.exp.failWith("FindMany", boom)

	l := NewMembersLogic(context.Background(), e.svcCtx)
	reply, err := l.Members(&rpc.MemberMidsReq{Mids: []int64{70201, 70202}})
	wantNoErr(t, "经验整批故障", err)
	wantOps(t, "经验整批故障", e.ops(0), []string{
		"cache.GetJSON:bs_70201",
		"cache.GetJSON:bs_70202",
		"cache.GetInt:exp_70201",
		"cache.GetInt:exp_70202",
		"exp.FindMany:70201,70202",
	})
	members := reply.GetMemberInfos()
	wantEQ(t, "经验整批故障", "条数不受影响", len(members), 2)
	wantEQ(t, "经验整批故障", "70201 name 保住", members[70201].GetBaseInfo().GetName(), "甲")
	wantEQ(t, "经验整批故障", "70201 等级降级 cur", members[70201].GetLevelInfo().GetCur(), int32(0))
	wantEQ(t, "经验整批故障", "70201 等级降级 next_exp（不是 -1，是真零值）", members[70201].GetLevelInfo().GetNextExp(), int32(0))
	wantEQ(t, "经验整批故障", "70202 等级降级", members[70202].GetLevelInfo().GetCur(), int32(0))
	wantEQ(t, "经验整批故障", "认证信息仍在", members[70201].GetOfficialInfo().GetTitle(), "UP主")
}

func TestMembersPropagatesBaseFailure(t *testing.T) {
	e := newEnv(t)
	boom := errors.New("select from user_base: syntax error")
	e.st.base.failWith("FindMany", boom)

	l := NewMembersLogic(context.Background(), e.svcCtx)
	reply, err := l.Members(&rpc.MemberMidsReq{Mids: []int64{70301, 70302}})
	wantErrIs(t, "基础资料批量故障", err, boom)
	if reply != nil {
		t.Errorf("基础资料批量故障：reply = %+v, want nil", reply)
	}
	// 基础资料阶段就挂了：经验一次都不许读。
	wantOps(t, "基础资料批量故障", e.ops(0), []string{
		"cache.GetJSON:bs_70301",
		"cache.GetJSON:bs_70302",
		"base.FindMany:70301,70302",
	})
}

func TestMembersUsesOnlyCachedExpWhenAllHit(t *testing.T) {
	e := newEnv(t)
	for _, mid := range []int64{70401, 70402} {
		e.st.cache.warmJSON(keyBase(mid), baseCachePayload{Cached: true, baseCacheValue: baseCacheValue{
			Mid: mid, Name: "命中", Rank: 5000, Birthday: -28800}})
		e.st.cache.warmInt(keyExp(mid), 450000) // 4500 分 → 4 级
	}
	l := NewMembersLogic(context.Background(), e.svcCtx)
	reply, err := l.Members(&rpc.MemberMidsReq{Mids: []int64{70401, 70402}})
	wantNoErr(t, "全员命中", err)
	wantOps(t, "全员命中", e.ops(0), []string{
		"cache.GetJSON:bs_70401",
		"cache.GetJSON:bs_70402",
		"cache.GetInt:exp_70401",
		"cache.GetInt:exp_70402",
	})
	wantEQ(t, "全员命中", "70402 cur", reply.GetMemberInfos()[70402].GetLevelInfo().GetCur(), int32(4))
	wantEQ(t, "全员命中", "70402 now_exp", reply.GetMemberInfos()[70402].GetLevelInfo().GetNowExp(), int32(4500))
}

// TestColdStartSnapshotEnqueuesProfileUpdatedPerMid 钉住 Member 快照来源的装配期行为：
// NewWithDeps / New 会把 officials 先初始化成**空而非 nil** 的 map，于是第一次
// loadOfficial 把每一条生效认证都判成「新增」，各写一条 user.profile.updated 进 Outbox。
// 生产后果：每次重启给全部认证号发一次 account 缓存失效风暴。
// 这是缺陷登记（见 README「已知缺口」），修好后本用例必须改成断言零条事件。
func TestColdStartSnapshotEnqueuesProfileUpdatedPerMid(t *testing.T) {
	st := newRawStore(withOfficial(80001, model.OfficialRoleUp, "甲", "a"))
	wantOps(t, "冷启动快照", st.log.opsFrom(0), []string{
		"official.All",
		"outbox.Insert:user.profile.updated/80001/1",
	})
	wantEQ(t, "冷启动快照", "Outbox 行数（缺陷现状：每个认证号一条）", len(st.outbox.rows), 1)
	// 快照本身必须可用：Member 只能从这里拿认证信息。
	wantEQ(t, "冷启动快照", "快照条数", len(st.repo.Officials()), 1)
}
