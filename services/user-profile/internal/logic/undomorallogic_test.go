package logic

// undomorallogic_test.go 覆盖 UndoMoral（logic/undomorallogic.go:28
// → repository/moral.go:190 → 复用 UpdateMoral(moral.go:87)）。
//
// 撤销是「节操域」里唯一一条**没有幂等键、也没有权限入参**的写链路，本文件把四件事钉住：
//   1. 撤销语义是「反向 delta 重放到**当前值**」，不是「回到当年的值」
//      （moral.go:239 Delta = from_moral - to_moral）：中间发生过别的变更时，撤销后拿不到原值。
//   2. 审计台账只有两条：原行被 MarkRevoked 改成 status=1（于是从 FindByMid 的
//      status=0 过滤里消失），新行是 UpdateMoral 写的**反向变更**，
//      它的 content 里没有任何字段指回被撤销的原 log_id（8 键集合见 moral.go:109-118）。
//      而 moral.go:208-217 那条「复制原日志、置 status=已撤销」的撤销台账
//      因为复用了同一个 (log_type, log_id)，在真库永远是 1062 并**被 logx 吞掉**
//      （deploy/migrations/user-profile/000009_create_member_log.sql:46 UNIQUE KEY uk_log_type_log_id）
//      —— 这条设计从未生效过。
//   3. 重复撤销没有幂等保护（FindByLogID 不过滤 status）：每撤销一次就再退回一次。
//   4. 撤销的 remark/operator 是调用方**自报**的自由文本，原样进 content 并经
//      MoralLog 的 content 出口（repository/util.go:50）端给调用方；
//      台账里的 IP 抄的是**被撤销那次变更**的 IP（moral.go:234），不是撤销者的 IP。
//
// 依赖错误口径：MarkRevoked / 撤销日志 Add 失败被吞；
// parse 与 UpdateMoral 的错误原样上抛（parse 分支还带 fmt.Errorf 包装）。

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/logx/logtest"

	"go-video/services/user-profile/internal/repository"
	"go-video/services/user-profile/model"
	"go-video/services/user-profile/rpc"
)

// moralUndoIP 是被撤销的那次变更的来源 IP（假值），用于验证撤销台账抄的是哪一个 IP。
const moralUndoIP = "203.0.113.7"

// moralCanonicalContent 复刻 AddMoral 落库的 8 键内容（moral.go:109-118）。
func moralCanonicalContent(mid, from, to, origin int64, reason string) map[string]string {
	return map[string]string{
		"from_moral": itoa(from), "to_moral": itoa(to), "origin": itoa(origin),
		"status": itoa(model.RevocableMoralStatus), "mid": itoa(mid),
		"remark": "例行巡查", "operater": "运营小A", "reason": reason,
	}
}

// undoSeedLog 静默布一条节操台账（不写轨迹，status=0 有效）。
func undoSeedLog(e *env, mid int64, logID string, content map[string]string) *model.MemberLog {
	return e.st.logs.put(model.LogTypeMoral, &model.UserLog{
		Mid: mid, IP: moralUndoIP, TS: time.Now().Unix(), LogID: logID, Content: content,
	}, model.LogStatusActive)
}

// undoReq 是撤销入参（log_id 之外只有两句自由文本）。
func undoReq(logID, remark, operator string) *rpc.UndoMoralReq {
	return &rpc.UndoMoralReq{LogId: logID, Remark: remark, Operator: operator}
}

// undoActiveContents 走真实读出口 MoralLog，返回 (log_id → content)。
func undoActiveContents(t *testing.T, e *env, mid int64) map[string]map[string]string {
	t.Helper()
	reply, err := NewMoralLogLogic(context.Background(), e.svcCtx).MoralLog(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "读节操台账", err)
	out := map[string]map[string]string{}
	for _, l := range reply.GetUserLogs() {
		out[l.GetLogId()] = l.GetContent()
	}
	return out
}

// TestUndoMoralReplaysReverseDeltaOnCurrentValue 撤销的主结论：
// 退回的是「那条变更的幅度」，作用在**当前值**上，而不是恢复成当年的 to_moral。
func TestUndoMoralReplaysReverseDeltaOnCurrentValue(t *testing.T) {
	const mid = int64(36100)
	e := newEnv(t)
	seedMoral(e, mid, 6000) // 当前值 6000，早已被后续变更继续扣过
	original := moralCanonicalContent(mid, 7500, 6500, model.PunishmentType, "发布违规弹幕")
	row := undoSeedLog(e, mid, "log-origin-1", original)
	e.st.log.reset()

	reply, err := NewUndoMoralLogic(context.Background(), e.svcCtx).UndoMoral(undoReq(row.LogID, "误判撤销", "运营小B"))
	wantNoErr(t, "撤销一次扣减", err)
	wantEQ(t, "撤销一次扣减", "应答无字段", reply.String(), "")

	// 序列：查原日志 → 标撤销 → 写撤销日志（必然 1062，见下方独立用例）→
	// 一个事务里 { 读当前值 → 加回 delta → 写台账 } → 失效节操缓存。
	// 反向 delta = 7500-6500 = +1000，作用在 6000 上 → 7000（**不是** 7500）。
	wantOpsLoose(t, "撤销一次扣减", e.ops(0), []string{
		"memberLog.FindByLogID:12/log-origin-1",
		"memberLog.MarkRevoked:12/log-origin-1",
		"memberLog.Add:12/36100/~",
		"moral.TxFindOne:36100",
		"moral.TxUpdate:36100/1000/1000/0",
		"memberLog.Add:12/36100/~",
		"cache.Del:moral_36100",
	})
	got := e.st.moral.get(mid)
	wantEQ(t, "撤销一次扣减", "只退回幅度：6000→7000", got.Moral, int64(7000))
	wantEQ(t, "撤销一次扣减", "累加计数同步 +1000", got.Added, int64(1100))
	wantEQ(t, "撤销一次扣减", "累减计数不变", got.Deducted, int64(200))
	wantEQ(t, "撤销一次扣减", "只有一个事务（撤销日志那条不在事务里）", e.st.conn.transactions, 1)
	wantEQ(t, "撤销一次扣减", "撤销不发任何事件", e.st.outbox.count(), 0)

	// 台账两行：原行 status 被改成「已撤销」，新行是反向变更且 status=0。
	wantEQ(t, "撤销一次扣减", "台账行数", e.st.logs.count(), 2)
	first := e.st.logs.row(0)
	wantEQ(t, "撤销一次扣减", "原行 status=已撤销", int(first.Status), model.LogStatusRevoked)
	newRow := e.st.logs.row(1)
	wantEQ(t, "撤销一次扣减", "新行 status=有效", int(newRow.Status), model.LogStatusActive)
	if newRow.LogID == row.LogID {
		t.Errorf("撤销一次扣减：新行复用了原 log_id（现状应是新 UUID）：%s", newRow.LogID)
	}
	wantUUIDLike(t, "撤销一次扣减", "新行 log_id", newRow.LogID)
	wantEQ(t, "撤销一次扣减", "新行 IP 抄的是被撤销那次变更的 IP", newRow.IP, moralUndoIP)

	content := decodeLogContent(t, "撤销一次扣减", newRow)
	wantKeysExact(t, "撤销一次扣减", "新行 content 键集合", content,
		[]string{"from_moral", "to_moral", "origin", "status", "mid", "remark", "operater", "reason"})
	wantStringsEQ(t, "撤销一次扣减", "反向台账的数值",
		[]string{content["from_moral"], content["to_moral"], content["origin"], content["status"], content["mid"]},
		[]string{"6000", "7000", itoa(int64(model.PunishmentType)), itoa(int64(model.IrrevocableMoralStatus)), itoa(mid)})
	wantStringsEQ(t, "撤销一次扣减", "撤销的备注/操作人取自入参",
		[]string{content["remark"], content["operater"]}, []string{"误判撤销", "运营小B"})
	// reason 抄原日志，ReasonType 被强制成「管理系统」→ 用户收不到任何通知（下面 Outbox=0 已证）。
	wantEQ(t, "撤销一次扣减", "原因沿用原日志", content["reason"], "发布违规弹幕")

	// 读侧闭环：原行因 status=1 被 FindByMid 过滤掉 → 台账只剩反向那条。
	active := undoActiveContents(t, e, mid)
	wantEQ(t, "撤销一次扣减", "有效台账条数", len(active), 1)
	if _, ok := active[row.LogID]; ok {
		t.Errorf("撤销一次扣减：被撤销的原记录仍出现在有效台账里（现状应被 status=1 过滤）")
	}
}

// TestUndoMoralRevokedAuditRowNeverLands 钉住「撤销台账」这条设计其实从未落地：
// moral.go:212 复用原 log_id 插新行，而 member_log 有
// UNIQUE KEY uk_log_type_log_id (log_type, log_id)（000009_create_member_log.sql:46）
// → 每次撤销必然 1062，且错误只进 logx.Errorf（moral.go:216），接口照样返回成功。
// 后果：库里查不到「谁在什么时候把这条撤销了」的独立记录——撤销人只活在
// 反向变更那条台账的 operater 字段里，而那个字段是调用方自报的。
//
// 缺陷：repository/moral.go:208-217 —— 撤销日志与原日志共用 log_id 且错误被吞，
// 那条 content.status=已撤销 的审计行永远写不进去 —— 修法方向：为撤销生成新 log_id，
// 并在 content 里加一列指回原 log_id。⚠ 改生产代码前不要动本用例的期望。
func TestUndoMoralRevokedAuditRowNeverLands(t *testing.T) {
	const mid = int64(36110)
	e := newEnv(t)
	seedMoral(e, mid, 6500)
	row := undoSeedLog(e, mid, "log-dup", moralCanonicalContent(mid, 7500, 6500, model.PunishmentType, "违规"))
	e.st.log.reset()
	logs := logtest.NewCollector(t)

	_, err := NewUndoMoralLogic(context.Background(), e.svcCtx).UndoMoral(undoReq(row.LogID, "撤销", "运营小B"))
	wantNoErr(t, "撤销日志冲突被吞", err)

	// 只有 2 行：被改状态的原行 + UpdateMoral 的反向行。第 3 行（撤销台账）不存在。
	wantEQ(t, "撤销日志冲突被吞", "台账行数", e.st.logs.count(), 2)
	// 先证明捕获通道工作，再钉「1062 只活在日志里」。
	if !strings.Contains(logs.String(), "add revoked log") {
		t.Fatalf("日志捕获通道失效，没收到撤销日志失败那行：%q", logs.String())
	}
	if !strings.Contains(logs.String(), "Duplicate entry") {
		t.Errorf("撤销日志冲突：期望日志里有 1062，实际=%q", logs.String())
	}
	// 设计里那条 content.status=1(已撤销) 的复制行，在库里查无实据。
	for i, r := range e.st.logs.rowsOf(model.LogTypeMoral, mid) {
		c := decodeLogContent(t, "撤销日志冲突被吞", r)
		if c["status"] == itoa(int64(model.RevokedMoralStatus)) {
			t.Errorf("撤销日志冲突被吞：第 %d 行竟带着 content.status=1（撤销台账其实没落地）", i)
		}
	}
	// 撤销台账的两次尝试都撞在同一把唯一键上（重放也不会有别的结果）。
	wantEQ(t, "撤销日志冲突被吞", "memberLog.Add 次数（一次被吞、一次成功）",
		e.st.log.countPrefix("memberLog.Add:"), 2)
}

// TestUndoMoralRepeatIsNotIdempotent 重复撤销没有幂等保护：
// FindByLogID 不按 status 过滤（model/log.go:119-127），UndoMoral 也不看 useLog.Status，
// 于是同一个 log_id 撤销 N 次就退回 N 次，直到上限钳制为止。
func TestUndoMoralRepeatIsNotIdempotent(t *testing.T) {
	t.Run("同一份撤销连做两次：值涨两次、台账多两行", func(t *testing.T) {
		const mid = int64(36120)
		e := newEnv(t)
		seedMoral(e, mid, 6000)
		row := undoSeedLog(e, mid, "log-replay", moralCanonicalContent(mid, 7500, 6500, model.PunishmentType, "违规"))
		l := NewUndoMoralLogic(context.Background(), e.svcCtx)

		_, err := l.UndoMoral(undoReq(row.LogID, "第一次撤销", "运营小B"))
		wantNoErr(t, "第一次撤销", err)
		wantEQ(t, "第一次撤销", "值", e.st.moral.get(mid).Moral, int64(7000))
		_, err = l.UndoMoral(undoReq(row.LogID, "第二次撤销", "运营小C"))
		wantNoErr(t, "第二次撤销（现状不拒）", err)
		wantEQ(t, "第二次撤销", "再涨一次（无幂等键）", e.st.moral.get(mid).Moral, int64(8000))
		wantEQ(t, "第二次撤销", "台账行数（原行 + 两条反向行）", e.st.logs.count(), 3)
		wantEQ(t, "第二次撤销", "两个事务", e.st.conn.transactions, 2)
		// 第二条反向行的 from/to 是「当时的当前值」，与第一条完全接得上，但都指向同一次原始变更。
		second := decodeLogContent(t, "第二次撤销", e.st.logs.row(2))
		wantStringsEQ(t, "第二次撤销", "第二条反向行",
			[]string{second["from_moral"], second["to_moral"], second["operater"]},
			[]string{"7000", "8000", "运营小C"})
		// 原行状态没被再改坏（MarkRevoked 是幂等的 UPDATE）。
		wantEQ(t, "第二次撤销", "原行仍是已撤销", int(e.st.logs.row(0).Status), model.LogStatusRevoked)
	})

	t.Run("顶到上限后仍重放：delta 钳成 0，照样留一条空台账", func(t *testing.T) {
		const mid = int64(36121)
		e := newEnv(t)
		seedMoral(e, mid, 9500)
		undoSeedLog(e, mid, "log-clamp", moralCanonicalContent(mid, 10000, 9500, model.PunishmentType, "违规"))
		l := NewUndoMoralLogic(context.Background(), e.svcCtx)
		_, err := l.UndoMoral(undoReq("log-clamp", "撤销", "运营小B"))
		wantNoErr(t, "钳制前撤销", err)
		wantEQ(t, "钳制前撤销", "补满到上限", e.st.moral.get(mid).Moral, int64(model.MaxMoral))
		e.st.log.reset()

		_, err = l.UndoMoral(undoReq("log-clamp", "再撤销一次", "运营小B"))
		wantNoErr(t, "上限后再撤销", err)
		wantOpsLoose(t, "上限后再撤销", e.ops(0), []string{
			"memberLog.FindByLogID:12/log-clamp",
			"memberLog.MarkRevoked:12/log-clamp",
			"memberLog.Add:12/36121/~",
			"moral.TxFindOne:36121",
			"moral.TxUpdate:36121/0/0/0", // 钳成 0：什么都没加，却仍然写库
			"memberLog.Add:12/36121/~",
			"cache.Del:moral_36121",
		})
		wantEQ(t, "上限后再撤销", "值不动", e.st.moral.get(mid).Moral, int64(model.MaxMoral))
		last := decodeLogContent(t, "上限后再撤销", e.st.logs.row(e.st.logs.count()-1))
		wantEQ(t, "上限后再撤销", "空台账也记了一条 from==to", last["from_moral"] == last["to_moral"], true)
	})
}

// TestUndoMoralMarkRevokedFailureLeavesTwoActiveRows MarkRevoked 失败被吞（moral.go:199-201）：
// 结果原记录**仍是有效状态**，而反向变更又新写了一条有效记录 ——
// 用户台账里同时出现「扣 1000」和「退 1000」两条都算数的记录，
// 且这条被撤销的记录还能再撤销一次（幂等缺口被放大）。
func TestUndoMoralMarkRevokedFailureLeavesTwoActiveRows(t *testing.T) {
	const mid = int64(36130)
	e := newEnv(t)
	seedMoral(e, mid, 6000)
	row := undoSeedLog(e, mid, "log-mark", moralCanonicalContent(mid, 7500, 6500, model.PunishmentType, "违规"))
	e.st.logs.failWith("MarkRevoked", errors.New("deadlock, retry later"))
	e.st.log.reset()
	logs := logtest.NewCollector(t)

	reply, err := NewUndoMoralLogic(context.Background(), e.svcCtx).UndoMoral(undoReq(row.LogID, "撤销", "运营小B"))
	wantNoErr(t, "标记撤销失败被吞", err)
	wantEQ(t, "标记撤销失败被吞", "应答仍成功", reply.String(), "")
	if !strings.Contains(logs.String(), "mark revoked") {
		t.Fatalf("日志捕获通道失效，没收到 MarkRevoked 失败那行：%q", logs.String())
	}
	wantEQ(t, "标记撤销失败被吞", "原行 status 未变（仍有效）", int(e.st.logs.row(0).Status), model.LogStatusActive)
	wantEQ(t, "标记撤销失败被吞", "值仍被退回", e.st.moral.get(mid).Moral, int64(7000))
	active := undoActiveContents(t, e, mid)
	wantEQ(t, "标记撤销失败被吞", "有效台账出现两条（一扣一退）", len(active), 2)
}

// TestUndoMoralContentParseFailuresLeaveHalfState content 是台账里唯一的真相来源，
// 但它没有任何写入前的结构保证：UndoMoral 直接 ParseInt 三个键（moral.go:219-230），
// 失败时**已经改完原行状态**才返回 ——
// 于是这条记录既退不回（后续 UpdateMoral 没跑）也看不到（status=1 被读侧过滤），
// 重试还是同一个错误，形成永久半状态。
func TestUndoMoralContentParseFailuresLeaveHalfState(t *testing.T) {
	const mid = int64(36140)
	cases := []struct {
		label    string
		content  map[string]string
		wantErr  error
		wantWrap string
	}{
		{
			label:   "origin 未登记(999)：守卫在 UpdateMoral 里，但状态已改",
			content: moralCanonicalContent(mid, 7500, 6500, 999, "违规"),
			wantErr: repository.ErrRequestErr,
		},
		{
			label:    "origin 非数字：strconv 错误被包装上抛",
			content:  map[string]string{"from_moral": "7500", "to_moral": "6500", "origin": "举报", "mid": itoa(mid)},
			wantWrap: "parse origin:",
		},
		{
			label:    "from_moral 缺失：ParseInt(\"\") 失败",
			content:  map[string]string{"to_moral": "6500", "origin": itoa(int64(model.PunishmentType)), "mid": itoa(mid)},
			wantWrap: "parse from_moral:",
		},
		{
			label:    "to_moral 是小数（历史脏数据）",
			content:  map[string]string{"from_moral": "75.00", "to_moral": "65.00", "origin": itoa(int64(model.PunishmentType)), "mid": itoa(mid)},
			wantWrap: "parse from_moral:",
		},
		{
			label:    "content 是空对象",
			content:  map[string]string{},
			wantWrap: "parse origin:",
		},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			e := newEnv(t)
			seedMoral(e, mid, 6000)
			undoSeedLog(e, mid, "log-bad", tc.content)
			e.st.log.reset()

			reply, err := NewUndoMoralLogic(context.Background(), e.svcCtx).UndoMoral(undoReq("log-bad", "撤销", "运营小B"))
			if tc.wantErr != nil {
				wantErrIs(t, tc.label, err, tc.wantErr)
			} else {
				// 数字解析失败：错误种类仍可辨（*strconv.NumError → ErrSyntax），且带阶段前缀。
				if !errors.Is(err, strconv.ErrSyntax) {
					t.Fatalf("%s：err = %v, want 包着 strconv.ErrSyntax", tc.label, err)
				}
				if !strings.Contains(err.Error(), tc.wantWrap) {
					t.Errorf("%s：错误消息应含 %q，实际=%v", tc.label, tc.wantWrap, err)
				}
			}
			wantEQ(t, tc.label, "应答为 nil", reply == nil, true)
			wantEQ(t, tc.label, "节操值没退（半途而废）", e.st.moral.get(mid).Moral, int64(6000))
			wantEQ(t, tc.label, "没开事务（守卫/解析先于事务）", e.st.conn.transactions, 0)
			// 但原行已经被标成已撤销，并从有效台账里消失：这条再也查不到、也退不回。
			wantEQ(t, tc.label, "原行已被标撤销（半状态）", int(e.st.logs.row(0).Status), model.LogStatusRevoked)
			wantEQ(t, tc.label, "有效台账条数", len(undoActiveContents(t, e, mid)), 0)
			// 撤销日志那条在 parse 之前就已尝试（并撞 1062）。
			wantEQ(t, tc.label, "memberLog.Add 尝试过一次", e.st.log.countPrefix("memberLog.Add:"), 1)
			// 重试同一个 log_id：仍失败、仍不再改变任何值（MarkRevoked 重复无害）。
			_, err2 := NewUndoMoralLogic(context.Background(), e.svcCtx).UndoMoral(undoReq("log-bad", "再试", "运营小B"))
			if tc.wantErr != nil {
				wantErrIs(t, tc.label+" 重试", err2, tc.wantErr)
			} else {
				wantEQ(t, tc.label+" 重试", "错误不变", errors.Is(err2, strconv.ErrSyntax), true)
			}
			wantEQ(t, tc.label+" 重试", "值仍没退", e.st.moral.get(mid).Moral, int64(6000))
		})
	}
}

// TestUndoMoralNoInputGuardAndNoStoreTouch 撤销没有任何入参守卫：
// log_id 不存在 → 走「查无」分支；log_id 为空串 → 同样只是查无（不在守卫层被拒）；
// 查库失败 → 原始错误直接上抛（生产口径见下方注释）。
func TestUndoMoralNoInputGuardAndNoStoreTouch(t *testing.T) {
	t.Run("log_id 不存在：ErrNothingFound 且零写入", func(t *testing.T) {
		const mid = int64(36150)
		e := newEnv(t)
		seedMoral(e, mid, 6000)
		e.st.log.reset()

		reply, err := NewUndoMoralLogic(context.Background(), e.svcCtx).UndoMoral(undoReq("log-missing", "撤销", "运营小B"))
		wantErrIs(t, "查无此日志", err, repository.ErrNothingFound)
		wantEQ(t, "查无此日志", "应答为 nil", reply == nil, true)
		wantOps(t, "查无此日志", e.ops(0), []string{"memberLog.FindByLogID:12/log-missing"})
		wantEQ(t, "查无此日志", "值未动", e.st.moral.get(mid).Moral, int64(6000))
		wantEQ(t, "查无此日志", "事务次数", e.st.conn.transactions, 0)
	})

	t.Run("空 log_id 不是守卫拒，而是查无", func(t *testing.T) {
		e := newEnv(t)
		undoSeedLog(e, 36151, "log-x", moralCanonicalContent(36151, 7500, 6500, model.PunishmentType, "违规"))
		e.st.log.reset()
		_, err := NewUndoMoralLogic(context.Background(), e.svcCtx).UndoMoral(undoReq("", "撤销", "运营小B"))
		wantErrIs(t, "空 log_id", err, repository.ErrNothingFound)
		wantOps(t, "空 log_id", e.ops(0), []string{"memberLog.FindByLogID:12/"})
	})

	t.Run("查日志失败：原始错误上抛，不换成哨兵", func(t *testing.T) {
		// 注意：生产 model/log.go:119-127 把 QueryRowCtx 的错误（含 sqlx.ErrNotFound）原样返回，
		// 所以「查无」在真库走的是本条路径，moral.go:195-197 的 ErrNothingFound 分支不可达
		// （替身在 fakes_test.go:1346 刻意保留 (nil,nil) 语义以便钉住那个分支）。
		const mid = int64(36152)
		e := newEnv(t)
		seedMoral(e, mid, 6000)
		boom := errors.New("Error 1146: Table 'member_log' doesn't exist")
		e.st.logs.failWith("FindByLogID", boom)
		e.st.log.reset()

		_, err := NewUndoMoralLogic(context.Background(), e.svcCtx).UndoMoral(undoReq("log-y", "撤销", "运营小B"))
		wantEQ(t, "查日志失败", "错误原样", errors.Is(err, boom), true)
		wantNoCall(t, "查日志失败", e.st, 1)
		wantEQ(t, "查日志失败", "值未动", e.st.moral.get(mid).Moral, int64(6000))
	})

	t.Run("撤销一条属于别人的日志：目标 mid 取自日志，无法指定", func(t *testing.T) {
		// 入参里没有 mid，所以不存在「用 A 的 log_id 扣 B 的分」；
		// 但反过来，任何人拿到 log_id 就能撤销（无权限入参、无会话校验）。
		const mid = int64(36153)
		e := newEnv(t)
		seedMoral(e, mid, 6500)
		row := undoSeedLog(e, mid, "log-owner", moralCanonicalContent(mid, 7500, 6500, model.PunishmentType, "违规"))
		if e.st.moral.get(999999) != nil {
			t.Fatal("布景本身不该有别人的节操行")
		}
		e.st.log.reset()

		_, err := NewUndoMoralLogic(context.Background(), e.svcCtx).UndoMoral(undoReq(row.LogID, "撤销", "运营小B"))
		wantNoErr(t, "mid 取自日志", err)
		wantNoOpsWith(t, "mid 取自日志", e.ops(0), "999999")
		wantEQ(t, "mid 取自日志", "只动了日志里那个 mid", e.st.moral.get(mid).Moral, int64(7500))
	})
}

// TestUndoMoralCreatesMoralRowForUserWithoutOne 撤销一个节操行已被清掉的用户：
// updateMoralTx 见 nil 就 TxInit(7000)（moral.go:257-261），于是凭空造出一行，
// 退回值 = 7000 + delta（与这个人历史真实值无关）。
func TestUndoMoralCreatesMoralRowForUserWithoutOne(t *testing.T) {
	const mid = int64(36160)
	e := newEnv(t)
	row := undoSeedLog(e, mid, "log-norow", moralCanonicalContent(mid, 7000, 6000, model.PunishmentType, "违规"))
	if e.st.moral.get(mid) != nil {
		t.Fatal("布景本身不该有这个 mid 的节操行")
	}
	e.st.log.reset()

	_, err := NewUndoMoralLogic(context.Background(), e.svcCtx).UndoMoral(undoReq(row.LogID, "撤销", "运营小B"))
	wantNoErr(t, "无节操行也能撤销", err)
	wantOpsLoose(t, "无节操行也能撤销", e.ops(0), []string{
		"memberLog.FindByLogID:12/log-norow",
		"memberLog.MarkRevoked:12/log-norow",
		"memberLog.Add:12/36160/~",
		"moral.TxFindOne:36160",
		"moral.TxInit:36160/7000", // 基准值起步，added/deducted 全 0
		"moral.TxUpdate:36160/1000/1000/0",
		"memberLog.Add:12/36160/~",
		"cache.Del:moral_36160",
	})
	got := e.st.moral.get(mid)
	wantEQ(t, "无节操行也能撤销", "造出的行落在 8000", got.Moral, int64(8000))
	wantEQ(t, "无节操行也能撤销", "累计退回只有这一笔", got.Added, int64(1000))
	wantEQ(t, "无节操行也能撤销", "累计扣减为 0", got.Deducted, int64(0))
	wantEQ(t, "无节操行也能撤销", "事务内拿到的确实是会话", e.st.moral.gotTx[mid], true)
}

// TestUndoMoralPropagatesTransactionFailures 事务内两步失败的口径：
// 值与台账同事务（回滚由 fakeConn 计数证明），错误原样上抛，缓存不失效。
func TestUndoMoralPropagatesTransactionFailures(t *testing.T) {
	t.Run("TxFindOne 失败：错误传出、原行状态已改（半状态）", func(t *testing.T) {
		const mid = int64(36170)
		e := newEnv(t)
		seedMoral(e, mid, 6000)
		undoSeedLog(e, mid, "log-tx1", moralCanonicalContent(mid, 7500, 6500, model.PunishmentType, "违规"))
		boom := errors.New("read user_moral: connection refused")
		e.st.moral.failWith("TxFindOne", boom)
		e.st.log.reset()

		_, err := NewUndoMoralLogic(context.Background(), e.svcCtx).UndoMoral(undoReq("log-tx1", "撤销", "运营小B"))
		wantEQ(t, "TxFindOne 失败", "错误原样", errors.Is(err, boom), true)
		wantOpsLoose(t, "TxFindOne 失败", e.ops(0), []string{
			"memberLog.FindByLogID:12/log-tx1", "memberLog.MarkRevoked:12/log-tx1",
			"memberLog.Add:12/36170/~", "moral.TxFindOne:36170",
		})
		wantEQ(t, "TxFindOne 失败", "值未动", e.st.moral.get(mid).Moral, int64(6000))
		wantEQ(t, "TxFindOne 失败", "原行已被标撤销", int(e.st.logs.row(0).Status), model.LogStatusRevoked)
		wantNoOpsWith(t, "TxFindOne 失败", e.ops(0), "cache.Del")
	})

	t.Run("memberLog.Add 失败：整个事务回滚，值退回被撤销", func(t *testing.T) {
		const mid = int64(36171)
		e := newEnv(t)
		seedMoral(e, mid, 6000)
		undoSeedLog(e, mid, "log-tx2", moralCanonicalContent(mid, 7500, 6500, model.PunishmentType, "违规"))
		boom := errors.New("Error 1406: Data too long for column 'content'")
		// 用 failWith 而不是 failOn：替身把「唯一键冲突」判定放在错误注入**之前**
		// （fakes_test.go:1274-1280），所以撤销日志那次 Add 永远拿不到注入的错误，
		// 这里注入的 boom 只可能命中事务内的反向台账那条 Add。
		e.st.logs.failWith("Add", boom)
		e.st.log.reset()

		_, err := NewUndoMoralLogic(context.Background(), e.svcCtx).UndoMoral(undoReq("log-tx2", "撤销", "运营小B"))
		wantEQ(t, "反向台账写失败", "错误原样", errors.Is(err, boom), true)
		wantEQ(t, "反向台账写失败", "事务回滚了一次", e.st.conn.rolledBack, 1)
		// 口径与 addmorallogic_test.go:619 一致：内存替身无法撤销已做的 TxUpdate，
		// 真库由 InnoDB 回滚保证，所以这里只断言「回滚发生了」+ 失败没有扩大影响面。
		wantEQ(t, "反向台账写失败", "台账仍只有原那一条（反向行没落地）", e.st.logs.count(), 1)
		wantNoOpsWith(t, "反向台账写失败", e.ops(0), "cache.Del")
	})
}
