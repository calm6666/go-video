package logic

// batchaddmorallogic_test.go 覆盖 BatchAddMoral（批量变更节操值）。
//
// Repository.BatchUpdateMoral 与单个 UpdateMoral 共用 updateMoralTx（读-改-写 + 钳制），
// 但口径有三点不同，本文件逐个钉死：
//  1. 整批**一个事务**（不是逐条提交）：任一行失败 → 整批失败、调用方拿到 nil map；
//  2. 事务后的失效/通知是 `for mid := range beforeMap` → **顺序随机**（Go map 迭代），
//     所以尾部轨迹只能用集合断言，同时钉「每个 mid 恰好失效一次」；
//  3. 批量日志的 content **少一个 mid 键**（单个变更 8 个键，批量只有 7 个）。
//
// 另外钉住两件事：**没有批量上限**（200 个 mid 照单全收，本服务不设数量守卫），
// 以及 mids 里有重复时既不去重也不报错——扣两次、日志两行，但 beforeMap 被后一次覆盖，
// 于是「跨档那一次」的通知按最后一次的前值判定，被吞掉。

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"go-video/services/user-profile/internal/repository"
	"go-video/services/user-profile/model"
	"go-video/services/user-profile/rpc"
)

// moralArgs 是批量入口的共用入参（origin=2 违规惩罚；reasonType 默认 TAG=不通知）。
func moralArgs(mids []int64, delta int64) *rpc.UpdateMoralsReq {
	return &rpc.UpdateMoralsReq{
		Mids: mids, Delta: delta, Origin: model.PunishmentType,
		Reason: "批量清理违规弹幕", ReasonType: model.TagReasonType,
		Operator: "运营小B", Remark: "批量巡查", Status: 0, IsNotify: false,
		Ip: "203.0.113.8",
	}
}

func sortedMids(m map[int64]int64) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// TestBatchAddMoralGuardTable 守卫表：与单个变更同一套 origin/reason 校验，
// 且必须发生在**任何触库之前**（整批一个 mid 都不碰，连事务都不开）。
func TestBatchAddMoralGuardTable(t *testing.T) {
	cases := []struct {
		label  string
		origin int64
		reason string
	}{
		{"origin=0 未登记", 0, "有原因"},
		{"origin=7 未登记", 7, "有原因"},
		{"origin=-1 未登记", -1, "有原因"},
		{"origin=1 举报奖励缺原因", model.ReportRewardType, ""},
		{"origin=2 违规惩罚缺原因", model.PunishmentType, ""},
		{"origin=5 自动恢复缺原因", model.ManualRecoveryType, ""},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			e := newEnv(t)
			mids := []int64{37001, 37002, 37003}
			for _, mid := range mids {
				seedMoral(e, mid, 7500)
			}
			arg := moralArgs(mids, -100)
			arg.Origin = tc.origin
			arg.Reason = tc.reason
			reply, err := NewBatchAddMoralLogic(context.Background(), e.svcCtx).BatchAddMoral(arg)
			if !errors.Is(err, repository.ErrRequestErr) {
				t.Fatalf("%s：err = %v, want ErrRequestErr", tc.label, err)
			}
			if reply != nil {
				t.Errorf("%s：reply = %+v, want nil", tc.label, reply)
			}
			wantNoCall(t, tc.label, e.st, 0)
			for _, mid := range mids {
				wantEQ(t, tc.label, "节操值未动 mid="+itoa(mid), e.st.moral.get(mid).Moral, int64(7500))
			}
			wantEQ(t, tc.label, "member_log 行数", e.st.logs.count(), 0)
			wantEQ(t, tc.label, "事务次数", e.st.conn.transactions, 0)
		})
	}

	// origin=6 手动修改是唯一 NeedReason=false 的来源，空原因必须放行。
	t.Run("origin=6 手动修改允许空原因", func(t *testing.T) {
		e := newEnv(t)
		seedMoral(e, 37010, 7500)
		arg := moralArgs([]int64{37010}, -100)
		arg.Origin = model.ManualChangeType
		arg.Reason = ""
		reply, err := NewBatchAddMoralLogic(context.Background(), e.svcCtx).BatchAddMoral(arg)
		wantNoErr(t, "批量 origin=6 空原因", err)
		wantEQ(t, "批量 origin=6 空原因", "确实落了库", reply.GetAfterMorals()[37010], int64(7400))
	})

	// TODO(缺陷)：mids 的元素本身不校验（mid<=0、重复、任意长的列表都照收），
	// 见 TestBatchAddMoralRejectsNothingPerMid 与 TestBatchAddMoralHasNoSizeCap。
}

// TestBatchAddMoralWritesInSliceOrderInsideOneTransaction 事务内按入参顺序逐行处理，
// 整批只有一个事务；事务外的失效顺序随机，所以拆开断言。
//
// 这里故意用 reasonType=3（TAG）：moralNoticeType 只认 1 弹幕 / 2 评论，
// 于是尾部不掺通知事件，失效序列能被完整钉住。
func TestBatchAddMoralWritesInSliceOrderInsideOneTransaction(t *testing.T) {
	e := newEnv(t)
	mids := []int64{37101, 37102, 37103}
	for _, mid := range mids {
		seedMoral(e, mid, 7500)
	}
	before := time.Now().Unix()

	reply, err := NewBatchAddMoralLogic(context.Background(), e.svcCtx).BatchAddMoral(moralArgs(mids, -1000))
	wantNoErr(t, "批量三人各扣 10", err)

	// 逐字段投影：返回 map 的键集合与值都要精确。
	wantInt64EQ(t, "批量三人", "AfterMorals 键集合", sortedMids(reply.GetAfterMorals()), mids)
	for _, mid := range mids {
		wantEQ(t, "批量三人", "AfterMorals["+itoa(mid)+"]", reply.GetAfterMorals()[mid], int64(6500))
		wantEQ(t, "批量三人", "库里 mid="+itoa(mid), e.st.moral.get(mid).Moral, int64(6500))
	}

	ops := e.ops(0)
	// 事务内：入参顺序可断言（每 mid 一组 TxFindOne → TxUpdate → TxUpdateRecoverDate → Add）。
	wantOpsLoose(t, "批量三人：事务内序列", ops[:len(mids)*4], []string{
		"moral.TxFindOne:37101", "moral.TxUpdate:37101/-1000/0/1000",
		"moral.TxUpdateRecoverDate:37101/~", "memberLog.Add:12/37101/1",
		"moral.TxFindOne:37102", "moral.TxUpdate:37102/-1000/0/1000",
		"moral.TxUpdateRecoverDate:37102/~", "memberLog.Add:12/37102/2",
		"moral.TxFindOne:37103", "moral.TxUpdate:37103/-1000/0/1000",
		"moral.TxUpdateRecoverDate:37103/~", "memberLog.Add:12/37103/3",
	})
	// 事务外：失效集合精确（一人一次，不重不漏），但顺序不保证。
	wantOpsSameSet(t, "批量三人：失效序列", "cache.Del", ops[len(mids)*4:], []string{
		"cache.Del:moral_37101", "cache.Del:moral_37102", "cache.Del:moral_37103",
	})
	wantEQ(t, "批量三人", "失效次数", e.st.log.countPrefix("cache.Del:"), len(mids))
	wantEQ(t, "批量三人", "整批只有一个事务", e.st.conn.transactions, 1)
	wantEQ(t, "批量三人", "没有回滚", e.st.conn.rolledBack, 0)
	wantEQ(t, "批量三人", "member_log 行数（一人一行）", e.st.logs.count(), 3)
	wantEQ(t, "批量三人", "Outbox 行数（TAG 类型不通知）", e.st.outbox.count(), 0)

	// 同批共用一个 ts（BatchUpdateMoral 只取一次 time.Now），log_id 各不相同。
	ts0 := e.st.logs.row(0).TS
	for i, mid := range mids {
		label := "批量三人：台账第 " + itoa(int64(i+1)) + " 行"
		row := e.st.logs.row(i)
		wantEQ(t, label, "mid", row.Mid, mid)
		wantEQ(t, label, "log_type", int(row.LogType), int(model.LogTypeMoral))
		wantEQ(t, label, "status 列", int(row.Status), int(model.LogStatusActive))
		wantEQ(t, label, "ip 列", row.IP, "203.0.113.8")
		wantEQ(t, label, "整批同一 ts", row.TS, ts0)
		wantTSWindow(t, label, "ts", row.TS, before, time.Now().Unix())
		wantUUIDLike(t, label, "log_id", row.LogID)
	}
	if e.st.logs.row(0).LogID == e.st.logs.row(1).LogID {
		t.Error("批量三人：两行日志共用了 log_id")
	}
}

// TestBatchAddMoralLogContentOmitsMid 批量日志的 content 少一个 mid 键。
// 单个变更写 8 个键（含 "mid"），批量只写 7 个：
// 消费方（节操记录页、对账脚本）若按单条口径读 content["mid"]，批量产生的行会读到空串。
// TODO(缺陷)：两条路径的日志 schema 不一致，应统一到 8 键。
func TestBatchAddMoralLogContentOmitsMid(t *testing.T) {
	e := newEnv(t)
	seedMoral(e, 37201, 7500)
	_, err := NewBatchAddMoralLogic(context.Background(), e.svcCtx).BatchAddMoral(moralArgs([]int64{37201}, -1000))
	wantNoErr(t, "批量日志", err)
	batch := decodeLogContent(t, "批量日志", e.st.logs.row(0))
	wantKeysExact(t, "批量日志", "content 键集合", batch, []string{
		"from_moral", "operater", "origin", "reason", "remark", "status", "to_moral",
	})
	if _, ok := batch["mid"]; ok {
		t.Error("批量日志里出现了 mid 键（与本结论相反，请同步更新用例与 README）")
	}
	// 其余字段仍然逐字段正确，且 reason/remark/operater 来自批量入参。
	wantEQ(t, "批量日志", "from_moral", batch["from_moral"], "7500")
	wantEQ(t, "批量日志", "to_moral", batch["to_moral"], "6500")
	wantEQ(t, "批量日志", "origin", batch["origin"], "2")
	wantEQ(t, "批量日志", "status", batch["status"], "0")
	wantEQ(t, "批量日志", "reason", batch["reason"], "批量清理违规弹幕")
	wantEQ(t, "批量日志", "remark", batch["remark"], "批量巡查")
	wantEQ(t, "批量日志", "operater（既有拼写）", batch["operater"], "运营小B")

	// 对照：同一个仓库用单个入口写，就多了 mid 键。
	seedMoral(e, 37202, 7500)
	_, err = NewAddMoralLogic(context.Background(), e.svcCtx).AddMoral(moralArg(37202, -1000))
	wantNoErr(t, "单条日志", err)
	single := decodeLogContent(t, "单条日志", e.st.logs.row(1))
	wantKeysExact(t, "单条日志", "content 键集合", single, []string{
		"from_moral", "mid", "operater", "origin", "reason", "remark", "status", "to_moral",
	})
	wantEQ(t, "两条路径的差异", "单条有 mid 键", single["mid"], "37202")
}

// TestBatchAddMoralClampsEachMid 逐条钳制：上限 100.00、下限 0，与单个变更同一套规则；
// 缺行照样 TxInit 补 70.00（幻影账号）；已归零的用户再被扣，仍写一行 0→0 的台账。
func TestBatchAddMoralClampsEachMid(t *testing.T) {
	cases := []struct {
		label       string
		mid         int64
		hasRow      bool
		seed        int64
		delta       int64
		wantAfter   int64
		wantTxUpd   string
		wantRecover bool
		wantFrom    string
	}{
		{
			label: "正常扣减并跌破基准", mid: 37301, hasRow: true, seed: 7500, delta: -1000,
			wantAfter: 6500, wantTxUpd: "moral.TxUpdate:37301/-1000/0/1000", wantRecover: true, wantFrom: "7500",
		},
		{
			label: "加分撞上 100 上限（delta 被改成 500）", mid: 37302, hasRow: true, seed: 9500, delta: 5000,
			wantAfter: 10000, wantTxUpd: "moral.TxUpdate:37302/500/500/0", wantFrom: "9500",
		},
		{
			label: "恰好加满到 100", mid: 37303, hasRow: true, seed: 8000, delta: 2000,
			wantAfter: 10000, wantTxUpd: "moral.TxUpdate:37303/2000/2000/0", wantFrom: "8000",
		},
		{
			label: "扣减穿到 0 下限（delta 被改成 -500）", mid: 37304, hasRow: true, seed: 500, delta: -1000,
			wantAfter: 0, wantTxUpd: "moral.TxUpdate:37304/-500/0/500", wantFrom: "500",
		},
		{
			label: "已归零再扣：delta 被钳成 0，仍写一行 0→0 台账", mid: 37305, hasRow: true, seed: 0, delta: -100,
			wantAfter: 0, wantTxUpd: "moral.TxUpdate:37305/0/0/0", wantFrom: "0",
		},
		{
			label: "无行：补 70.00 后再扣", mid: 37306, hasRow: false, delta: -1000,
			wantAfter: 6000, wantTxUpd: "moral.TxUpdate:37306/-1000/0/1000", wantRecover: true, wantFrom: "7000",
		},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			e := newEnv(t)
			if tc.hasRow {
				seedMoral(e, tc.mid, tc.seed)
			}
			reply, err := NewBatchAddMoralLogic(context.Background(), e.svcCtx).BatchAddMoral(moralArgs([]int64{tc.mid}, tc.delta))
			wantNoErr(t, tc.label, err)
			wantEQ(t, tc.label, "返回值", reply.GetAfterMorals()[tc.mid], tc.wantAfter)
			wantEQ(t, tc.label, "库里", e.st.moral.get(tc.mid).Moral, tc.wantAfter)

			want := []string{"moral.TxFindOne:" + itoa(tc.mid)}
			if !tc.hasRow {
				want = append(want, "moral.TxInit:"+itoa(tc.mid)+"/7000")
			}
			want = append(want, tc.wantTxUpd)
			if tc.wantRecover {
				want = append(want, "moral.TxUpdateRecoverDate:"+itoa(tc.mid)+"/~")
			}
			want = append(want, "memberLog.Add:12/"+itoa(tc.mid)+"/1", "cache.Del:moral_"+itoa(tc.mid))
			wantOpsLoose(t, tc.label, e.ops(0), want)

			wantEQ(t, tc.label, "台账行数（钳制成 0 也留痕）", e.st.logs.count(), 1)
			c := decodeLogContent(t, tc.label, e.st.logs.row(0))
			wantEQ(t, tc.label, "from_moral", c["from_moral"], tc.wantFrom)
			wantEQ(t, tc.label, "to_moral", c["to_moral"], itoa(tc.wantAfter))
			if !tc.wantRecover {
				// 未跌破基准就不该刷新 last_recover_date（DDL 默认 -28800）。
				wantEQ(t, tc.label, "last_recover_date", e.st.moral.get(tc.mid).LastRecoverDate, model.DefaultTime)
			}
			// 真库 moral 是 BIGINT UNSIGNED：SQL 里没有 GREATEST，
			// 全靠这里的钳制兜住负值；钳制一旦被删，MySQL 会直接报 1690 越界。
		})
	}
}

// TestBatchAddMoralRejectsNothingPerMid 批量入口对 mids 元素零校验：
// mid<=0 会凭空补出一行节操值，重复 mid 会扣两次。
//
// TODO(缺陷)：没有「mid 必须 > 0」校验，也没有去重；
// 重复 mid 还会因为 beforeMap 被后一次覆盖而吞掉跨档通知。
func TestBatchAddMoralRejectsNothingPerMid(t *testing.T) {
	t.Run("mid=0 与负 mid 照样写", func(t *testing.T) {
		e := newEnv(t)
		reply, err := NewBatchAddMoralLogic(context.Background(), e.svcCtx).BatchAddMoral(moralArgs([]int64{0, -7}, -100))
		wantNoErr(t, "非法 mid 批量", err)
		wantInt64EQ(t, "非法 mid 批量", "返回的 mid 集合", sortedMids(reply.GetAfterMorals()), []int64{-7, 0})
		wantEQ(t, "非法 mid 批量", "0 号幻影账号被补出来", e.st.moral.get(0).Moral, int64(6900))
		wantEQ(t, "非法 mid 批量", "负号幻影账号被补出来", e.st.moral.get(-7).Moral, int64(6900))
		wantOpsLoose(t, "非法 mid 批量：两行都是凭空补的", e.ops(0)[:10], []string{
			"moral.TxFindOne:0", "moral.TxInit:0/7000", "moral.TxUpdate:0/-100/0/100",
			"moral.TxUpdateRecoverDate:0/~", "memberLog.Add:12/0/1",
			"moral.TxFindOne:-7", "moral.TxInit:-7/7000", "moral.TxUpdate:-7/-100/0/100",
			"moral.TxUpdateRecoverDate:-7/~", "memberLog.Add:12/-7/2",
		})
		wantEQ(t, "非法 mid 批量", "台账两行（非法 mid 也入账）", e.st.logs.count(), 2)
	})

	t.Run("重复 mid 会扣两次并吞掉跨档通知", func(t *testing.T) {
		e := newEnv(t)
		seedMoral(e, 37400, 6500)
		arg := moralArgs([]int64{37400, 37400}, -1000)
		arg.ReasonType = model.DMReasonType // 弹幕原因：本可触发「低于60」通知

		reply, err := NewBatchAddMoralLogic(context.Background(), e.svcCtx).BatchAddMoral(arg)
		wantNoErr(t, "重复 mid", err)

		// 扣两次：钳制只保证不为负，不保证不重复计数。
		wantEQ(t, "重复 mid", "最终节操 65-10-10", e.st.moral.get(37400).Moral, int64(4500))
		wantEQ(t, "重复 mid", "返回 map 按键去重", reply.GetAfterMorals()[37400], int64(4500))
		wantEQ(t, "重复 mid", "台账两行（重复变更各留一行）", e.st.logs.count(), 2)
		// 失效按 beforeMap 的键走，重复 mid 只留一个键 → 一次 Del（值是对的，不漏失效）。
		wantEQ(t, "重复 mid", "失效次数（键去重后一次）", e.st.log.countPrefix("cache.Del:moral_37400"), 1)
		wantEQ(t, "重复 mid", "整批仍是一个事务", e.st.conn.transactions, 1)

		// beforeMap 被后一次覆盖成 5500，于是按 5500→4500 判定：不跨 60 档、
		// 也不跨 30 档 → 一条通知都没有。而 TestAddMoralNoticeMatrix 证明
		// 同样的跨档用单个入口写就会通知。
		wantEQ(t, "重复 mid", "Outbox 行数（跨档通知被吞）", e.st.outbox.count(), 0)
		wantEQ(t, "重复 mid", "第 1 行确实跨了 60 档",
			decodeLogContent(t, "重复 mid 第 1 行", e.st.logs.row(0))["to_moral"], "5500")
	})
}

// TestBatchAddMoralHasNoSizeCap 没有批量上限：200 个 mid 一次全收，
// 只有一个事务、200 行台账、200 次失效。
//
// TODO(缺陷)：单个 gRPC 请求即可改任意多个用户的节操值，本服务不设数量上限；
// 而且这是**无调用者身份约束**的运营写入口（AGENTS.md §5：违规判定应由
// moderation-orchestrator 给出，这里只接受调用方自称的 origin/reason）。
func TestBatchAddMoralHasNoSizeCap(t *testing.T) {
	const n = 200
	e := newEnv(t)
	mids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		mid := int64(38000 + i)
		seedMoral(e, mid, 7500)
		mids = append(mids, mid)
	}
	reply, err := NewBatchAddMoralLogic(context.Background(), e.svcCtx).BatchAddMoral(moralArgs(mids, -1000))
	wantNoErr(t, "200 人批量", err)
	wantEQ(t, "200 人批量", "返回条数", len(reply.GetAfterMorals()), n)
	wantEQ(t, "200 人批量", "第一条", reply.GetAfterMorals()[38000], int64(6500))
	wantEQ(t, "200 人批量", "最后一条", reply.GetAfterMorals()[38199], int64(6500))
	wantEQ(t, "200 人批量", "台账行数", e.st.logs.count(), n)
	wantEQ(t, "200 人批量", "读次数", e.st.log.countPrefix("moral.TxFindOne:"), n)
	wantEQ(t, "200 人批量", "写次数", e.st.log.countPrefix("moral.TxUpdate:"), n)
	wantEQ(t, "200 人批量", "失效次数（一人一次，不重不漏）", e.st.log.countPrefix("cache.Del:"), n)
	wantEQ(t, "200 人批量", "事务次数（不是逐条提交）", e.st.conn.transactions, 1)
	wantEQ(t, "200 人批量", "没有回滚", e.st.conn.rolledBack, 0)
}

// TestBatchAddMoralEmptyBatchIsNoOp 空列表：守卫放行、事务照开、什么都没写。
func TestBatchAddMoralEmptyBatchIsNoOp(t *testing.T) {
	cases := []struct {
		label string
		mids  []int64
	}{
		{"mids 为 nil", nil},
		{"mids 为空切片", []int64{}},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			e := newEnv(t)
			reply, err := NewBatchAddMoralLogic(context.Background(), e.svcCtx).BatchAddMoral(moralArgs(tc.mids, -1000))
			wantNoErr(t, tc.label, err)
			wantEQ(t, tc.label, "返回条数", len(reply.GetAfterMorals()), 0)
			wantNoCall(t, tc.label, e.st, 0)
			wantEQ(t, tc.label, "空批量也开了一个事务", e.st.conn.transactions, 1)
			wantEQ(t, tc.label, "台账行数", e.st.logs.count(), 0)
		})
	}
}

// wantBatchAborted 钉「整批失败」的四个共同事实：nil map、
// 只有一个事务且它回滚了、事务之后的失效/通知一次都没发生。
func wantBatchAborted(t *testing.T, e *env, label string, reply *rpc.UpdateMoralsReply) {
	t.Helper()
	if reply != nil {
		t.Errorf("%s：reply = %+v, want nil（失败时不能给出半截 after_morals）", label, reply)
	}
	wantEQ(t, label, "整批只有一个事务", e.st.conn.transactions, 1)
	wantEQ(t, label, "该事务回滚了", e.st.conn.rolledBack, 1)
	wantNoOpsWith(t, label, e.ops(0), "cache.Del")
	wantEQ(t, label, "Outbox 行数", e.st.outbox.count(), 0)
}

// TestBatchAddMoralFailureAbortsWholeBatch 部分失败口径 = **整批回滚**（不是逐条提交）：
// 故障放在第二条上，第三条连读都没被读到。
//
// 诚实说明：内存替身无法撤销事务内已做的写，所以第一条在替身里仍是被改过的值、
// 台账里也还留着一行；真库由 InnoDB 回滚清掉。本用例断言的是可观测的硬事实：
// 错误传出、map 为 nil、事务数为 1、第三条零调用、失效零发生。
func TestBatchAddMoralFailureAbortsWholeBatch(t *testing.T) {
	boom := errors.New("boom-batch")

	t.Run("第二条读失败", func(t *testing.T) {
		e := newEnv(t)
		mids := []int64{37501, 37502, 37503}
		for _, mid := range mids {
			seedMoral(e, mid, 7500)
			e.st.cache.warmJSON(keyMoral(mid), model.UserMoral{Mid: mid, Moral: 7500})
		}
		e.st.moral.failOn("TxFindOne", 2, boom)

		reply, err := NewBatchAddMoralLogic(context.Background(), e.svcCtx).BatchAddMoral(moralArgs(mids, -1000))
		wantErrIs(t, "第二条读失败", err, boom)
		wantBatchAborted(t, e, "第二条读失败", reply)
		wantOpsLoose(t, "第二条读失败：序列", e.ops(0), []string{
			"moral.TxFindOne:37501", "moral.TxUpdate:37501/-1000/0/1000",
			"moral.TxUpdateRecoverDate:37501/~", "memberLog.Add:12/37501/1",
			"moral.TxFindOne:37502",
		})
		wantEQ(t, "第二条读失败", "第三条未被触碰", e.st.moral.get(37503).Moral, int64(7500))
		wantEQ(t, "第二条读失败", "第三条无台账", len(e.st.logs.rowsOf(model.LogTypeMoral, 37503)), 0)
		// 缓存原样留着：失败不扩大影响面（真库里回滚后旧值本来就是对的）。
		for _, mid := range mids {
			if _, ok := e.st.cache.jsons[keyMoral(mid)]; !ok {
				t.Errorf("第二条读失败：mid=%d 的 moral_ 缓存被顺手删了", mid)
			}
		}
	})

	t.Run("第二条写失败", func(t *testing.T) {
		e := newEnv(t)
		mids := []int64{37511, 37512, 37513}
		for _, mid := range mids {
			seedMoral(e, mid, 7500)
		}
		e.st.moral.failOn("TxUpdate", 2, boom)

		reply, err := NewBatchAddMoralLogic(context.Background(), e.svcCtx).BatchAddMoral(moralArgs(mids, -1000))
		wantErrIs(t, "第二条写失败", err, boom)
		wantBatchAborted(t, e, "第二条写失败", reply)
		wantOpsLoose(t, "第二条写失败：序列", e.ops(0), []string{
			"moral.TxFindOne:37511", "moral.TxUpdate:37511/-1000/0/1000",
			"moral.TxUpdateRecoverDate:37511/~", "memberLog.Add:12/37511/1",
			"moral.TxFindOne:37512", "moral.TxUpdate:37512/-1000/0/1000",
		})
		wantNoOpsWith(t, "第二条写失败", e.ops(0), "memberLog.Add:12/37512")
		wantEQ(t, "第二条写失败", "第二条没有台账", len(e.st.logs.rowsOf(model.LogTypeMoral, 37512)), 0)
	})

	t.Run("第二条台账写失败", func(t *testing.T) {
		e := newEnv(t)
		mids := []int64{37521, 37522, 37523}
		for _, mid := range mids {
			seedMoral(e, mid, 7500)
		}
		e.st.logs.failOn("Add", 2, boom)

		reply, err := NewBatchAddMoralLogic(context.Background(), e.svcCtx).BatchAddMoral(moralArgs(mids, -1000))
		wantErrIs(t, "第二条台账写失败", err, boom)
		wantBatchAborted(t, e, "第二条台账写失败", reply)
		// 变更与台账同事务：台账失败要连第一条的节操变更一起回滚（真库语义）。
		wantEQ(t, "第二条台账写失败", "台账行数（替身无法回滚，真库为 0）", e.st.logs.count(), 1)
		wantNoOpsWith(t, "第二条台账写失败", e.ops(0), "moral.TxFindOne:37523")
	})

	t.Run("第二条补行失败", func(t *testing.T) {
		e := newEnv(t)
		seedMoral(e, 37531, 7500)
		// 第二个 mid 不布行 → 走 TxInit，注入让所有 TxInit 失败。
		e.st.moral.failWith("TxInit", boom)

		reply, err := NewBatchAddMoralLogic(context.Background(), e.svcCtx).BatchAddMoral(moralArgs([]int64{37531, 37532}, -1000))
		wantErrIs(t, "第二条补行失败", err, boom)
		wantBatchAborted(t, e, "第二条补行失败", reply)
		wantOpsLoose(t, "第二条补行失败：序列", e.ops(0), []string{
			"moral.TxFindOne:37531", "moral.TxUpdate:37531/-1000/0/1000",
			"moral.TxUpdateRecoverDate:37531/~", "memberLog.Add:12/37531/1",
			"moral.TxFindOne:37532", "moral.TxInit:37532/7000",
		})
		if e.st.moral.get(37532) != nil {
			t.Error("第二条补行失败：TxInit 报错却仍补出了行")
		}
	})
}

// TestBatchAddMoralDownstreamFailuresAfterTx 事务成功之后的两类失败都被吞：
// 失效失败（整批缓存吐旧值）、通知失败（变更已成功、用户没收到告知）。
func TestBatchAddMoralDownstreamFailuresAfterTx(t *testing.T) {
	t.Run("缓存失效失败被吞：接口成功但整批缓存都是旧值", func(t *testing.T) {
		e := newEnv(t)
		mids := []int64{37601, 37602}
		for _, mid := range mids {
			seedMoral(e, mid, 7500)
			e.st.cache.warmJSON(keyMoral(mid), model.UserMoral{Mid: mid, Moral: 7500})
		}
		e.st.cache.failWith("Del", errors.New("del moral cache: redis down"))

		reply, err := NewBatchAddMoralLogic(context.Background(), e.svcCtx).BatchAddMoral(moralArgs(mids, -1000))
		wantNoErr(t, "批量失效失败", err)
		wantEQ(t, "批量失效失败", "接口仍返回新值", reply.GetAfterMorals()[37601], int64(6500))
		wantEQ(t, "批量失效失败", "库里已是新值", e.st.moral.get(37602).Moral, int64(6500))
		wantEQ(t, "批量失效失败", "两个 key 都尝试删过", e.st.log.countPrefix("cache.Del:moral_"), 2)

		e.st.log.reset()
		got, err := NewMoralLogic(context.Background(), e.svcCtx).Moral(&rpc.MemberMidReq{Mid: 37601})
		wantNoErr(t, "批量失效失败后读", err)
		wantOps(t, "批量失效失败后读", e.ops(0), []string{"cache.GetJSON:moral_37601"})
		wantEQ(t, "批量失效失败后读", "读到的仍是旧值（TTL 3600 内）", got.GetMoral(), int64(7500))
	})

	t.Run("通知事件写失败被吞：变更成功但用户收不到告知", func(t *testing.T) {
		e := newEnv(t)
		seedMoral(e, 37701, 6500)
		e.st.outbox.failWith("Insert", errors.New("insert into member_outbox: boom"))

		arg := moralArgs([]int64{37701}, -1000) // 6500 → 5500：跌破 60 档
		arg.ReasonType = model.DMReasonType
		reply, err := NewBatchAddMoralLogic(context.Background(), e.svcCtx).BatchAddMoral(arg)
		wantNoErr(t, "批量通知写失败", err)
		wantEQ(t, "批量通知写失败", "变更成功", reply.GetAfterMorals()[37701], int64(5500))
		wantEQ(t, "批量通知写失败", "台账仍在", e.st.logs.count(), 1)
		wantEQ(t, "批量通知写失败", "事件丢了", e.st.outbox.count(), 0)
		wantEQ(t, "批量通知写失败", "只有通知事务回滚", e.st.conn.rolledBack, 1)
		wantEQ(t, "批量通知写失败", "事务次数（1 主 + 1 通知）", e.st.conn.transactions, 2)
		wantOps(t, "批量通知写失败：序列", e.ops(0), []string{
			"moral.TxFindOne:37701", "moral.TxUpdate:37701/-1000/0/1000",
			"memberLog.Add:12/37701/1",
			"cache.Del:moral_37701",
			"outbox.Insert:user.moral.notice/37701/1",
		})
	})
}
