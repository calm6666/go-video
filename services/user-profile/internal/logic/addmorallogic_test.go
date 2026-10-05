package logic

// addmorallogic_test.go 覆盖 AddMoral（变更单个用户的节操值）。
//
// Repository.UpdateMoral 是本批最复杂的写链路，也是唯一**有真守卫 + 走事务 + 带阈值通知**的：
//
//	origin 合法性 + NeedReason 守卫（在任何触库之前）
//	→ TransactCtx { TxFindOne → (缺失则 TxInit(7000)) → TxUpdate → (跌破基准则 TxUpdateRecoverDate)
//	                → memberLog.Add }
//	→ delMoralCache（错误被 logx.Errorf 吞掉）
//	→ moralNotice（reasonType 只有 1 弹幕 / 2 评论会触发；再各开一个事务写 user.moral.notice）
//
// 单位口径：节操值 1/100（基准 7000 = 70.00，上限 10000 = 100.00），
// 阈值 6000/3000 与扣减文案里的「%0.2f」都建立在这个单位上。
// 本文件重点钉：正负边界、越界是**钳制**而非拒绝、每一次变更恰好一行日志、
// 日志与变更同事务（日志失败则变更回滚）、重放会双记（无幂等键）、
// 失效失败被吞、以及阈值通知的触发/不触发全集。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go-video/services/user-profile/internal/repository"
	"go-video/services/user-profile/model"
	"go-video/services/user-profile/rpc"
)

// moralArg 是绝大多数用例共用的入参（origin=2 违规惩罚，reason 已填 → 过守卫）。
func moralArg(mid, delta int64) *rpc.UpdateMoralReq {
	return &rpc.UpdateMoralReq{
		Mid: mid, Delta: delta, Origin: model.PunishmentType,
		Reason: "发布违规弹幕", ReasonType: model.DMReasonType,
		Operator: "运营小A", Remark: "例行巡查", Status: 0, IsNotify: false,
		Ip: "203.0.113.7",
	}
}

func seedMoral(e *env, mid, moral int64) {
	e.st.moral.put(&model.UserMoral{Mid: mid, Moral: moral, Added: 100, Deducted: 200})
}

// TestAddMoralGuardTable 守卫表：origin 与 reason 两项校验必须发生在**任何触库/触缓存之前**。
func TestAddMoralGuardTable(t *testing.T) {
	cases := []struct {
		label  string
		origin int64
		reason string
	}{
		{"origin=0 未登记", 0, "有原因"},
		{"origin=7 未登记", 7, "有原因"},
		{"origin=-1 未登记", -1, "有原因"},
		{"origin=999 未登记", 999, "有原因"},
		{"origin=1 举报奖励缺原因", model.ReportRewardType, ""},
		{"origin=2 违规惩罚缺原因", model.PunishmentType, ""},
		{"origin=3 撤销奖励缺原因", model.CancelRewardType, ""},
		{"origin=4 撤销惩罚缺原因", model.CancelPunishType, ""},
		{"origin=5 自动恢复缺原因", model.ManualRecoveryType, ""},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			e := newEnv(t)
			seedMoral(e, 36000, 7500)
			l := NewAddMoralLogic(context.Background(), e.svcCtx)
			arg := moralArg(36000, -100)
			arg.Origin = tc.origin
			arg.Reason = tc.reason
			reply, err := l.AddMoral(arg)
			if !errors.Is(err, repository.ErrRequestErr) {
				t.Fatalf("%s：err = %v, want ErrRequestErr", tc.label, err)
			}
			if reply != nil {
				t.Errorf("%s：reply = %+v, want nil", tc.label, reply)
			}
			// 关键：守卫先于一切依赖调用（缓存/DB 一次都没碰）。
			wantNoCall(t, tc.label, e.st, 0)
			wantEQ(t, tc.label, "节操值未动", e.st.moral.get(36000).Moral, int64(7500))
			wantEQ(t, tc.label, "member_log 行数", e.st.logs.count(), 0)
		})
	}

	// origin=6 手动修改是唯一 NeedReason=false 的来源，空原因必须放行。
	t.Run("origin=6 手动修改允许空原因", func(t *testing.T) {
		e := newEnv(t)
		seedMoral(e, 36001, 7500)
		arg := moralArg(36001, -100)
		arg.Origin = model.ManualChangeType
		arg.Reason = ""
		l := NewAddMoralLogic(context.Background(), e.svcCtx)
		_, err := l.AddMoral(arg)
		wantNoErr(t, "origin=6 空原因", err)
		wantEQ(t, "origin=6 空原因", "确实落了库", e.st.moral.get(36001).Moral, int64(7400))
	})
}

// TestAddMoralGuardsDoNotCoverMidOrStatus 钉「守卫的覆盖面就这两项」：
// mid、status、reason_type、delta 全都不校验。
//
// TODO(缺陷)：
//   - mid <= 0：照样进事务，给一个不存在的账号 TxInit 出一行 user_moral；
//   - status 不校验枚举（日志状态只有 0/1/2 有意义，这里可以塞任意 int64）；
//   - reason_type 不校验枚举：只有 1/2 会触发站内通知，传 3/999 会**静默不通知**，
//     运营以为通知发出去了其实没有；
//   - 审核结论的 owner 是 moderation-orchestrator（AGENTS.md §5），本服务不判定违规，
//     所以这里对 reason 只查「非空」，不查内容真伪。
func TestAddMoralGuardsDoNotCoverMidOrStatus(t *testing.T) {
	t.Run("mid=0 无守卫照样写", func(t *testing.T) {
		e := newEnv(t)
		l := NewAddMoralLogic(context.Background(), e.svcCtx)
		_, err := l.AddMoral(moralArg(0, -100))
		wantNoErr(t, "mid=0", err)
		// user_moral 里没有这一行 → TxInit 补建基准 7000，再扣 100。
		wantOpsLoose(t, "mid=0", e.ops(0), []string{
			"moral.TxFindOne:0", "moral.TxInit:0/7000", "moral.TxUpdate:0/-100/0/100",
			"moral.TxUpdateRecoverDate:0/~", "memberLog.Add:12/0/1", "cache.Del:moral_0",
		})
		row := e.st.moral.get(0)
		if row == nil {
			t.Fatal("mid=0：凭空补出的节操行不在库里")
		}
		wantEQ(t, "mid=0", "被补建成 70.00 起扣", row.Moral, int64(6900))
	})

	t.Run("status/reason_type 取任意值都原样入日志", func(t *testing.T) {
		e := newEnv(t)
		seedMoral(e, 36010, 7500)
		arg := moralArg(36010, -100)
		arg.Status = 77
		arg.ReasonType = 999
		l := NewAddMoralLogic(context.Background(), e.svcCtx)
		_, err := l.AddMoral(arg)
		wantNoErr(t, "非法 status/reason_type", err)
		c := decodeLogContent(t, "非法 status/reason_type", e.st.logs.row(0))
		wantEQ(t, "非法 status/reason_type", "status 原样入库", c["status"], "77")
		// reason_type 不进日志，只影响是否通知；999 不通知。
		wantEQ(t, "非法 status/reason_type", "Outbox 行数（reason_type=999 静默不通知）", e.st.outbox.count(), 0)
	})
}

// TestAddMoralBoundaryAndClamping 越界是**钳制**不是拒绝：
// 上限 10000、下限 0，钳制后的增量写进 TxUpdate，日志记的是钳制后的前后值。
// 注意最后两行：即使钳制后增量为 0，仍然会 UPDATE 一次并留下一条「from == to」的台账。
func TestAddMoralBoundaryAndClamping(t *testing.T) {
	cases := []struct {
		label       string
		mid         int64
		before      int64
		delta       int64
		wantAfter   int64
		wantUpdate  string // moral.TxUpdate 的期望轨迹段
		wantAdded   int64  // 布景 added=100 + 本次实际增量
		wantDeduct  int64  // 布景 deducted=200 + 本次实际减量
		wantFrom    string // 日志 from_moral
		wantTo      string // 日志 to_moral
		wantRecover bool   // 是否刷新 last_recover_date
	}{
		{
			label: "正常扣减：跌破基准 → 刷恢复时间", mid: 36020, before: 7500, delta: -1000,
			wantAfter: 6500, wantUpdate: "moral.TxUpdate:36020/-1000/0/1000",
			wantAdded: 100, wantDeduct: 1200, wantFrom: "7500", wantTo: "6500", wantRecover: true,
		},
		{
			label: "正常增加", mid: 36021, before: 7500, delta: 1000,
			wantAfter: 8500, wantUpdate: "moral.TxUpdate:36021/1000/1000/0",
			wantAdded: 1100, wantDeduct: 200, wantFrom: "7500", wantTo: "8500",
		},
		{
			label: "加到越界：钳到上限，增量被改成 500", mid: 36022, before: 9500, delta: 1000,
			wantAfter: 10000, wantUpdate: "moral.TxUpdate:36022/500/500/0",
			wantAdded: 600, wantDeduct: 200, wantFrom: "9500", wantTo: "10000",
		},
		{
			label: "已在上限再加 1：增量钳成 0，仍写库仍记台账", mid: 36023, before: 10000, delta: 1,
			wantAfter: 10000, wantUpdate: "moral.TxUpdate:36023/0/0/0",
			wantAdded: 100, wantDeduct: 200, wantFrom: "10000", wantTo: "10000",
		},
		{
			label: "扣到越界：钳到 0，减量被改成 -300", mid: 36024, before: 300, delta: -1000,
			wantAfter: 0, wantUpdate: "moral.TxUpdate:36024/-300/0/300",
			wantAdded: 100, wantDeduct: 500, wantFrom: "300", wantTo: "0",
		},
		{
			label: "恰好扣到 0：不钳制", mid: 36025, before: 500, delta: -500,
			wantAfter: 0, wantUpdate: "moral.TxUpdate:36025/-500/0/500",
			wantAdded: 100, wantDeduct: 700, wantFrom: "500", wantTo: "0",
		},
		{
			label: "恰好加到上限：不钳制", mid: 36026, before: 9000, delta: 1000,
			wantAfter: 10000, wantUpdate: "moral.TxUpdate:36026/1000/1000/0",
			wantAdded: 1100, wantDeduct: 200, wantFrom: "9000", wantTo: "10000",
		},
		{
			label: "delta=0：仍写库仍记台账（前后值相同）", mid: 36027, before: 7000, delta: 0,
			wantAfter: 7000, wantUpdate: "moral.TxUpdate:36027/0/0/0",
			wantAdded: 100, wantDeduct: 200, wantFrom: "7000", wantTo: "7000",
		},
		{
			label: "从 0 再扣：钳成 0（绝不会为负）", mid: 36028, before: 0, delta: -100,
			wantAfter: 0, wantUpdate: "moral.TxUpdate:36028/0/0/0",
			wantAdded: 100, wantDeduct: 200, wantFrom: "0", wantTo: "0",
		},
		{
			label: "7000 → 6999：恰好跌破基准要刷恢复时间", mid: 36029, before: 7000, delta: -1,
			wantAfter: 6999, wantUpdate: "moral.TxUpdate:36029/-1/0/1",
			wantAdded: 100, wantDeduct: 201, wantFrom: "7000", wantTo: "6999", wantRecover: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			e := newEnv(t)
			seedMoral(e, tc.mid, tc.before)

			l := NewAddMoralLogic(context.Background(), e.svcCtx)
			_, err := l.AddMoral(moralArg(tc.mid, tc.delta))
			wantNoErr(t, tc.label, err)

			row := e.st.moral.get(tc.mid)
			wantEQ(t, tc.label, "钳制后的节操值", row.Moral, tc.wantAfter)
			wantEQ(t, tc.label, "added 累加的是钳制后的实际增量", row.Added, tc.wantAdded)
			wantEQ(t, tc.label, "deducted 累加的是钳制后的实际减量", row.Deducted, tc.wantDeduct)

			wantEQ(t, tc.label, "台账恰好一行", e.st.logs.count(), 1)
			c := decodeLogContent(t, tc.label, e.st.logs.row(0))
			wantEQ(t, tc.label, "日志 from_moral 是钳制前的库存值", c["from_moral"], tc.wantFrom)
			wantEQ(t, tc.label, "日志 to_moral 是钳制后的值", c["to_moral"], tc.wantTo)

			// 完整顺序：读 → 写 →（按需）刷恢复时间 → 记台账 → 提交 → 失效。
			seq := []string{"moral.TxFindOne:" + itoa(tc.mid), tc.wantUpdate}
			if tc.wantRecover {
				seq = append(seq, "moral.TxUpdateRecoverDate:"+itoa(tc.mid)+"/~")
			}
			seq = append(seq, "memberLog.Add:12/"+itoa(tc.mid)+"/1", "cache.Del:moral_"+itoa(tc.mid))
			wantOpsLoose(t, tc.label, e.ops(0), seq)
		})
	}
}

// TestAddMoralRecoverDateRule 恢复时间只在「从基准值上方跌到下方」时刷新。
// 规则：before >= 7000 && after < 7000。四种组合都要分辨。
func TestAddMoralRecoverDateRule(t *testing.T) {
	cases := []struct {
		label    string
		before   int64
		delta    int64
		wantCall bool
	}{
		{"7000 → 6999 跨界：刷新", 7000, -1, true},
		{"7001 → 7000 未跨界：不刷新", 7001, -1, false},
		{"6999 → 5000 本来就在下方：不刷新", 6999, -1999, false},
		{"8000 → 7500 下跌但仍在基准上：不刷新", 8000, -500, false},
		{"6000 → 7000 涨回基准：不刷新", 6000, 1000, false},
		{"7500 → 0 跌穿：刷新", 7500, -99999, true},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			e := newEnv(t)
			// 布一个**旧的**恢复时间，用来证明「刷新」确实改写了这一列。
			e.st.moral.put(&model.UserMoral{Mid: 36100, Moral: tc.before, LastRecoverDate: 111})
			before := time.Now().Unix() - 1
			l := NewAddMoralLogic(context.Background(), e.svcCtx)
			_, err := l.AddMoral(moralArg(36100, tc.delta))
			wantNoErr(t, tc.label, err)
			after := time.Now().Unix() + 1

			row := e.st.moral.get(36100)
			if tc.wantCall {
				wantTSWindow(t, tc.label, "last_recover_date 被刷成 now", row.LastRecoverDate, before, after)
				if e.st.log.countPrefix("moral.TxUpdateRecoverDate:") != 1 {
					t.Errorf("%s：TxUpdateRecoverDate 次数 = %d, want 1（序列 %v）", tc.label, e.st.log.countPrefix("moral.TxUpdateRecoverDate:"), e.ops(0))
				}
			} else {
				wantEQ(t, tc.label, "last_recover_date 保持布景值", row.LastRecoverDate, int64(111))
				wantNoOpsWith(t, tc.label, e.ops(0), "TxUpdateRecoverDate")
			}
		})
	}
}

// TestAddMoralInitializesMissingRow 节操行不存在时：先 TxFindOne 拿到 nil，
// 再 TxInit(基准 7000)，然后按 7000 起算——顺序必须钉住（少了 TxInit 就是给不存在的行做 UPDATE）。
func TestAddMoralInitializesMissingRow(t *testing.T) {
	e := newEnv(t)
	l := NewAddMoralLogic(context.Background(), e.svcCtx)
	_, err := l.AddMoral(moralArg(36200, -100))
	wantNoErr(t, "补建节操行", err)
	wantOpsLoose(t, "补建节操行", e.ops(0), []string{
		"moral.TxFindOne:36200",
		"moral.TxInit:36200/7000",
		"moral.TxUpdate:36200/-100/0/100",
		"moral.TxUpdateRecoverDate:36200/~",
		"memberLog.Add:12/36200/1",
		"cache.Del:moral_36200",
	})
	row := e.st.moral.get(36200)
	wantEQ(t, "补建节操行", "moral", row.Moral, int64(6900))
	wantEQ(t, "补建节操行", "TxInit 的 added/deducted 被后续 UPDATE 覆盖", row.Added, int64(0))
	wantEQ(t, "补建节操行", "deducted", row.Deducted, int64(100))
	wantEQ(t, "补建节操行", "事务次数", e.st.conn.transactions, 1)
	wantEQ(t, "补建节操行", "回滚次数", e.st.conn.rolledBack, 0)
}

// TestAddMoralWritesExactlyOneFullLogRow 每一次变更恰好一行完整审计日志。
// content 的 8 个键是对外契约（节操记录页直接读它），多一个少一个都要红。
func TestAddMoralWritesExactlyOneFullLogRow(t *testing.T) {
	e := newEnv(t)
	seedMoral(e, 36300, 7500)
	before := time.Now().Unix() - 1
	l := NewAddMoralLogic(context.Background(), e.svcCtx)
	_, err := l.AddMoral(moralArg(36300, -1000))
	wantNoErr(t, "节操日志", err)
	after := time.Now().Unix() + 1

	wantEQ(t, "节操日志", "一次变更恰好一行", e.st.logs.count(), 1)
	row := e.st.logs.row(0)
	wantEQ(t, "节操日志", "log_type（12 节操变更）", int(row.LogType), int(model.LogTypeMoral))
	wantEQ(t, "节操日志", "mid", row.Mid, int64(36300))
	wantUUIDLike(t, "节操日志", "log_id", row.LogID)
	wantTSWindow(t, "节操日志", "ts", row.TS, before, after)
	wantEQ(t, "节操日志", "ip 落在列里（不在 content 里）", row.IP, "203.0.113.7")
	wantEQ(t, "节操日志", "status 列恒为 0（model.Log.Add 里硬编码 LogStatusActive）", int(row.Status), int(model.LogStatusActive))

	c := decodeLogContent(t, "节操日志", row)
	wantKeysExact(t, "节操日志", "content 键集合", c, []string{
		"from_moral", "mid", "operater", "origin", "reason", "remark", "status", "to_moral",
	})
	wantEQ(t, "节操日志", "from_moral", c["from_moral"], "7500")
	wantEQ(t, "节操日志", "to_moral", c["to_moral"], "6500")
	wantEQ(t, "节操日志", "origin", c["origin"], "2")
	wantEQ(t, "节操日志", "status（content 里的 status 来自入参，可与列不同）", c["status"], "0")
	wantEQ(t, "节操日志", "mid", c["mid"], "36300")
	wantEQ(t, "节操日志", "remark", c["remark"], "例行巡查")
	wantEQ(t, "节操日志", "operater（既有拼写）", c["operater"], "运营小A")
	wantEQ(t, "节操日志", "reason", c["reason"], "发布违规弹幕")

	// 读侧闭环：MoralLog 能查到这一行，且 reason_type 不落库（查不回原因类型）。
	e.st.log.reset()
	ml, err := NewMoralLogLogic(context.Background(), e.svcCtx).MoralLog(&rpc.MemberMidReq{Mid: 36300})
	wantNoErr(t, "节操日志读回", err)
	wantOps(t, "节操日志读回", e.ops(0), []string{"memberLog.FindByMid:12/36300"})
	wantEQ(t, "节操日志读回", "行数", len(ml.GetUserLogs()), 1)
	wantEQ(t, "节操日志读回", "content 里读回的 to_moral", ml.GetUserLogs()[0].GetContent()["to_moral"], "6500")
}

// TestAddMoralReplayDoubleCounts 重放/并发没有幂等键：
// 同一笔惩罚调两次就是两次扣减、两行日志（log_id 不同）。
// 这就是「运营双击提交按钮」在生产上的后果——钳制只保证不会为负，不保证不双记。
func TestAddMoralReplayDoubleCounts(t *testing.T) {
	e := newEnv(t)
	seedMoral(e, 36400, 7500)
	l := NewAddMoralLogic(context.Background(), e.svcCtx)
	arg := moralArg(36400, -1000)

	_, err := l.AddMoral(arg)
	wantNoErr(t, "第一次惩罚", err)
	_, err = l.AddMoral(arg)
	wantNoErr(t, "第二次同样的惩罚", err)

	wantEQ(t, "重放", "节操被扣了两次", e.st.moral.get(36400).Moral, int64(5500))
	wantEQ(t, "重放", "日志两行（重复变更各留一行）", e.st.logs.count(), 2)
	r1, r2 := e.st.logs.row(0), e.st.logs.row(1)
	wantEQ(t, "重放", "第 1 行 from→to", decodeLogContent(t, "重放行1", r1)["to_moral"], "6500")
	wantEQ(t, "重放", "第 2 行 from→to", decodeLogContent(t, "重放行2", r2)["to_moral"], "5500")
	if r1.LogID == r2.LogID {
		t.Errorf("重放：两行日志共用了 log_id %s（应为每次新生成的 uuid）", r1.LogID)
	}
	wantOps(t, "重放的调用序列", e.ops(0), []string{
		"moral.TxFindOne:36400", "moral.TxUpdate:36400/-1000/0/1000",
		"moral.TxUpdateRecoverDate:36400/" + itoa(r1.TS), "memberLog.Add:12/36400/1",
		"cache.Del:moral_36400",
		// 第二次 6500→5500：before 已在基准值之下，所以不再刷新 last_recover_date；
		// 但这一笔跌破了 60 档，于是补一条通知事件（通知在自己的事务里）。
		"moral.TxFindOne:36400", "moral.TxUpdate:36400/-1000/0/1000",
		"memberLog.Add:12/36400/2",
		"cache.Del:moral_36400",
		"outbox.Insert:user.moral.notice/36400/1",
	})
	wantEQ(t, "重放", "事务次数（2 次变更 + 1 次通知）", e.st.conn.transactions, 3)
}

// TestAddMoralNoticeMatrix 阈值通知矩阵：只有 reasonType 1/2 会通知，
// delta==0 不通知，三段阈值各命中一次，IsNotify 决定是否追加惩罚/奖励通知。
func TestAddMoralNoticeMatrix(t *testing.T) {
	cases := []struct {
		label      string
		before     int64
		delta      int64
		reasonType int64
		origin     int64
		operator   string
		isNotify   bool
		wantEvents []string // 期望的 notice title 全集（按发送顺序）
	}{
		{
			label: "跌到 60 以下（弹幕原因）", before: 6500, delta: -1000,
			reasonType: model.DMReasonType, origin: model.PunishmentType, operator: "小A",
			wantEvents: []string{"你的节操值已低于60"},
		},
		{
			label: "跌到 30 以下（评论原因）", before: 3500, delta: -1000,
			reasonType: model.ReplyReasonType, origin: model.PunishmentType, operator: "小A",
			wantEvents: []string{"你的节操值已低于30"},
		},
		{
			label: "恢复到 60 以上", before: 5000, delta: 1500,
			reasonType: model.DMReasonType, origin: model.CancelPunishType, operator: "小A",
			wantEvents: []string{"你的节操值已恢复至60以上"},
		},
		{
			label: "未跨任何阈值：不通知", before: 7500, delta: -100,
			reasonType: model.DMReasonType, origin: model.PunishmentType, operator: "小A",
			wantEvents: nil,
		},
		{
			// before=6000 / after=6000 恰好满足「before>=6000 && after<=6000 && after>=3000」，
			// 唯一的拦阻是 moralNotice 开头的 delta==0 短路——这条用例因此能分辨那行代码在不在。
			label: "delta=0：短路掉通知（阈值条件本来已满足）", before: 6000, delta: 0,
			reasonType: model.DMReasonType, origin: model.PunishmentType, operator: "小A",
			wantEvents: nil,
		},
		{
			label: "reasonType=TAG(3)：整类不通知", before: 6500, delta: -1000,
			reasonType: model.TagReasonType, origin: model.PunishmentType, operator: "小A",
			wantEvents: nil,
		},
		{
			label: "reasonType=管理系统(6)：整类不通知", before: 6500, delta: -1000,
			reasonType: model.SysReasonType, origin: model.PunishmentType, operator: "小A",
			wantEvents: nil,
		},
		{
			label: "惩罚 + IsNotify：阈值通知之外再补一条惩罚通知", before: 6500, delta: -1000,
			reasonType: model.DMReasonType, origin: model.PunishmentType, operator: "小A", isNotify: true,
			wantEvents: []string{"你的节操值已低于60", "你被举报处理扣除了10.00节操值"},
		},
		{
			label: "惩罚 + 操作人是「系统」：走系统文案", before: 6500, delta: -1000,
			reasonType: model.DMReasonType, origin: model.PunishmentType, operator: "系统", isNotify: true,
			wantEvents: []string{"你的节操值已低于60", "你被系统处理扣除了10.00节操值"},
		},
		{
			label: "奖励 + IsNotify（评论来源）", before: 5000, delta: 1500,
			reasonType: model.ReplyReasonType, origin: model.ReportRewardType, operator: "小A", isNotify: true,
			wantEvents: []string{"你的节操值已恢复至60以上", "你举报的评论已被处理"},
		},
		{
			label: "惩罚但不通知：只剩阈值通知", before: 6500, delta: -1000,
			reasonType: model.DMReasonType, origin: model.PunishmentType, operator: "小A", isNotify: false,
			wantEvents: []string{"你的节操值已低于60"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			e := newEnv(t)
			seedMoral(e, 36500, tc.before)
			arg := moralArg(36500, tc.delta)
			arg.ReasonType = tc.reasonType
			arg.Origin = tc.origin
			arg.Operator = tc.operator
			arg.IsNotify = tc.isNotify

			l := NewAddMoralLogic(context.Background(), e.svcCtx)
			_, err := l.AddMoral(arg)
			wantNoErr(t, tc.label, err)

			var titles []string
			for i := 0; i < e.st.outbox.count(); i++ {
				row := e.st.outbox.row(i)
				wantEQ(t, tc.label, "event_type", row.EventType, model.EventMoralNotice)
				env, pl := decodeOutboxView(t, tc.label, row)
				wantEQ(t, tc.label, "信封 event_type", env.EventType, model.EventMoralNotice)
				wantEQ(t, tc.label, "信封 aggregate_id", env.AggregateID, "36500")
				wantEQ(t, tc.label, "payload.mid", plInt(t, tc.label, pl, "mid"), int64(36500))
				wantEQ(t, tc.label, "payload.notice_type 非空", plStr(t, tc.label, pl, "notice_type") != "", true)
				wantEQ(t, tc.label, "通知 payload 键数量（mid/title/message/notice_type 四个，多一个都算泄漏）", len(pl), 4)
				titles = append(titles, plStr(t, tc.label, pl, "title"))
			}
			wantOps(t, tc.label+"：通知标题与顺序", titles, tc.wantEvents)

			// 通知是**另一个事务**（enqueueNotice 自带 TransactCtx），且失败也不影响主流程。
			wantEQ(t, tc.label, "事务次数 = 1 次变更 + N 次通知",
				e.st.conn.transactions, 1+len(tc.wantEvents))
			wantEQ(t, tc.label, "失效次数（只删 moral_<mid>，一次）",
				e.st.log.countPrefix("cache.Del:"), 1)
			// 顺序：先失效缓存、后排队通知（反过来会让消费者读到旧值）。
			ops := e.ops(0)
			delAt, insAt := -1, -1
			for i, o := range ops {
				if strings.HasPrefix(o, "cache.Del:") && delAt < 0 {
					delAt = i
				}
				if strings.HasPrefix(o, "outbox.Insert:") && insAt < 0 {
					insAt = i
				}
			}
			if len(tc.wantEvents) > 0 {
				if delAt < 0 || insAt < 0 || delAt > insAt {
					t.Errorf("%s：缓存失效(%d)应排在通知入队(%d)之前（序列 %v）", tc.label, delAt, insAt, ops)
				}
			}
		})
	}
}

// TestAddMoralNoticeCarriesNoRawViolationText 通知文案里只有模板 + 数值，
// 不含 reason / remark / operator / ip（AGENTS.md §6：PII 与举报原文不进事件）。
func TestAddMoralNoticeCarriesNoRawViolationText(t *testing.T) {
	e := newEnv(t)
	seedMoral(e, 36600, 6500)
	arg := moralArg(36600, -1000)
	arg.Reason = "骂人：张三 13800001111"
	arg.Remark = "举报人内部备注 身份证 11010119900307XXXX"
	arg.Operator = "运营小A"
	arg.IsNotify = true
	l := NewAddMoralLogic(context.Background(), e.svcCtx)
	_, err := l.AddMoral(arg)
	wantNoErr(t, "通知内容审计", err)
	wantEQ(t, "通知内容审计", "事件数", e.st.outbox.count(), 2)

	for i := 0; i < e.st.outbox.count(); i++ {
		payload := e.st.outbox.row(i).Payload
		for _, leak := range []string{"张三", "13800001111", "11010119900307", "运营小A", "203.0.113.7", "骂人"} {
			if strings.Contains(payload, leak) {
				t.Errorf("通知事件第 %d 条泄漏了 %q：%s", i+1, leak, payload)
			}
		}
	}
	// 举报原文只在 member_log.content 里（那是审计面，不是事件面）。
	for _, row := range []*model.MemberLog{e.st.logs.row(0)} {
		c := decodeLogContent(t, "节操日志", row)
		wantEQ(t, "节操日志", "reason 只进日志", c["reason"], "骂人：张三 13800001111")
	}
}

// TestAddMoralDownstreamFailures 逐个依赖注入错误：
// 事务内的失败必须回滚（事务计数 +1、回滚 +1）、不留缓存失效、不假装成功。
func TestAddMoralDownstreamFailures(t *testing.T) {
	t.Run("TxFindOne 失败：整个事务回滚，不失效不通知", func(t *testing.T) {
		e := newEnv(t)
		seedMoral(e, 36700, 7500)
		e.st.cache.warmJSON(keyMoral(36700), model.UserMoral{Mid: 36700, Moral: 7500})
		boom := errors.New("select moral from user_moral: boom")
		e.st.moral.failWith("TxFindOne", boom)

		l := NewAddMoralLogic(context.Background(), e.svcCtx)
		reply, err := l.AddMoral(moralArg(36700, -1000))
		wantErrIs(t, "TxFindOne 失败", err, boom)
		if reply != nil {
			t.Errorf("TxFindOne 失败：reply = %+v, want nil", reply)
		}
		wantOps(t, "TxFindOne 失败", e.ops(0), []string{"moral.TxFindOne:36700"})
		wantEQ(t, "TxFindOne 失败", "事务次数", e.st.conn.transactions, 1)
		wantEQ(t, "TxFindOne 失败", "回滚次数", e.st.conn.rolledBack, 1)
		wantEQ(t, "TxFindOne 失败", "member_log 行数", e.st.logs.count(), 0)
		wantEQ(t, "TxFindOne 失败", "Outbox 行数", e.st.outbox.count(), 0)
		if _, ok := e.st.cache.jsons[keyMoral(36700)]; !ok {
			t.Error("TxFindOne 失败：缓存被顺手删了")
		}
	})

	t.Run("TxInit 失败（行缺失）：回滚", func(t *testing.T) {
		e := newEnv(t)
		boom := errors.New("Error 1146: Table 'user_moral' doesn't exist")
		e.st.moral.failWith("TxInit", boom)
		l := NewAddMoralLogic(context.Background(), e.svcCtx)
		_, err := l.AddMoral(moralArg(36701, -100))
		wantErrIs(t, "TxInit 失败", err, boom)
		wantOps(t, "TxInit 失败", e.ops(0), []string{"moral.TxFindOne:36701", "moral.TxInit:36701/7000"})
		wantEQ(t, "TxInit 失败", "回滚次数", e.st.conn.rolledBack, 1)
	})

	t.Run("TxUpdate 失败：不留日志、不失效", func(t *testing.T) {
		e := newEnv(t)
		seedMoral(e, 36702, 7500)
		boom := errors.New("dead lock found on user_moral")
		e.st.moral.failWith("TxUpdate", boom)
		l := NewAddMoralLogic(context.Background(), e.svcCtx)
		_, err := l.AddMoral(moralArg(36702, -1000))
		wantErrIs(t, "TxUpdate 失败", err, boom)
		wantOps(t, "TxUpdate 失败", e.ops(0), []string{"moral.TxFindOne:36702", "moral.TxUpdate:36702/-1000/0/1000"})
		wantNoOpsWith(t, "TxUpdate 失败", e.ops(0), "memberLog.Add")
		wantNoOpsWith(t, "TxUpdate 失败", e.ops(0), "cache.Del")
		wantEQ(t, "TxUpdate 失败", "回滚次数", e.st.conn.rolledBack, 1)
	})

	t.Run("TxUpdateRecoverDate 失败：一并回滚（不给半截状态）", func(t *testing.T) {
		e := newEnv(t)
		seedMoral(e, 36703, 7500)
		boom := errors.New("update last_recover_date: boom")
		e.st.moral.failWith("TxUpdateRecoverDate", boom)
		l := NewAddMoralLogic(context.Background(), e.svcCtx)
		_, err := l.AddMoral(moralArg(36703, -1000))
		wantErrIs(t, "TxUpdateRecoverDate 失败", err, boom)
		wantNoOpsWith(t, "TxUpdateRecoverDate 失败", e.ops(0), "memberLog.Add")
		wantEQ(t, "TxUpdateRecoverDate 失败", "回滚次数", e.st.conn.rolledBack, 1)
	})

	// 这条是本批最有价值的失败断言：日志与变更**同事务**，所以日志写失败时
	// 节操变更也一起回滚——「有变更没台账」在这个实现里是不可能的。
	t.Run("memberLog.Add 失败：变更同事务回滚，错误传出", func(t *testing.T) {
		e := newEnv(t)
		seedMoral(e, 36704, 7500)
		e.st.cache.warmJSON(keyMoral(36704), model.UserMoral{Mid: 36704, Moral: 7500})
		boom := errors.New("insert into member_log: Error 1406 content too long")
		e.st.logs.failWith("Add", boom)

		l := NewAddMoralLogic(context.Background(), e.svcCtx)
		reply, err := l.AddMoral(moralArg(36704, -1000))
		wantErrIs(t, "日志写失败", err, boom)
		if reply != nil {
			t.Errorf("日志写失败：reply = %+v, want nil", reply)
		}
		wantOps(t, "日志写失败", e.ops(0), []string{
			"moral.TxFindOne:36704", "moral.TxUpdate:36704/-1000/0/1000",
			"moral.TxUpdateRecoverDate:36704/" + itoa(time.Now().Unix()),
			"memberLog.Add:12/36704/1",
		})
		wantEQ(t, "日志写失败", "事务回滚（内存替身无法撤销已做的 TxUpdate，真库由 InnoDB 保证）",
			e.st.conn.rolledBack, 1)
		wantNoOpsWith(t, "日志写失败", e.ops(0), "cache.Del")
		wantEQ(t, "日志写失败", "Outbox 行数（没台账也不会通知）", e.st.outbox.count(), 0)
		if _, ok := e.st.cache.jsons[keyMoral(36704)]; !ok {
			t.Error("日志写失败：缓存被顺手删了（失败不该扩大影响面）")
		}
	})

	t.Run("缓存失效失败被吞：接口成功但 moral_ 仍吐旧值", func(t *testing.T) {
		e := newEnv(t)
		seedMoral(e, 36705, 7500)
		e.st.cache.warmJSON(keyMoral(36705), model.UserMoral{Mid: 36705, Moral: 7500})
		e.st.cache.failWith("Del", errors.New("del moral_36705: redis down"))

		l := NewAddMoralLogic(context.Background(), e.svcCtx)
		reply, err := l.AddMoral(moralArg(36705, -1000))
		wantNoErr(t, "节操失效失败", err)
		if reply == nil {
			t.Fatal("节操失效失败：成功时 reply 必须非 nil")
		}
		wantEQ(t, "节操失效失败", "库里已是新值", e.st.moral.get(36705).Moral, int64(6500))
		var stale model.UserMoral
		if !e.st.cache.jsonOf(keyMoral(36705), &stale) {
			t.Fatal("节操失效失败：moral_ 缓存不见了")
		}
		wantEQ(t, "节操失效失败", "缓存仍是旧节操值", stale.Moral, int64(7500))

		e.st.log.reset()
		got, err := NewMoralLogic(context.Background(), e.svcCtx).Moral(&rpc.MemberMidReq{Mid: 36705})
		wantNoErr(t, "节操失效失败后读", err)
		wantOps(t, "节操失效失败后读", e.ops(0), []string{"cache.GetJSON:moral_36705"})
		wantEQ(t, "节操失效失败后读", "吐出的还是旧值", got.GetMoral(), int64(7500))
	})

	t.Run("通知事件写失败被吞：变更成功、接口仍返回成功", func(t *testing.T) {
		e := newEnv(t)
		seedMoral(e, 36706, 6500)
		boom := errors.New("insert into member_outbox: boom")
		e.st.outbox.failWith("Insert", boom)

		l := NewAddMoralLogic(context.Background(), e.svcCtx)
		reply, err := l.AddMoral(moralArg(36706, -1000))
		if err != nil {
			t.Fatalf("通知写失败被传出：%v（与本结论「被吞」不符，请同步更新用例与 README）", err)
		}
		if reply == nil {
			t.Fatal("通知写失败：成功时 reply 必须非 nil")
		}
		wantOps(t, "通知写失败被吞", e.ops(0), []string{
			"moral.TxFindOne:36706", "moral.TxUpdate:36706/-1000/0/1000",
			"memberLog.Add:12/36706/1",
			"cache.Del:moral_36706",
			"outbox.Insert:user.moral.notice/36706/1",
		})
		wantEQ(t, "通知写失败被吞", "节操已扣", e.st.moral.get(36706).Moral, int64(5500))
		wantEQ(t, "通知写失败被吞", "台账仍在", e.st.logs.count(), 1)
		wantEQ(t, "通知写失败被吞", "事件丢了（用户收不到低于 60 的告知）", e.st.outbox.count(), 0)
		wantEQ(t, "通知写失败被吞", "主事务未受影响（通知是独立事务）", e.st.conn.rolledBack, 1)
	})
}
