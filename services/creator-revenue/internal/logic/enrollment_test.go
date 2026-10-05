package logic

// 本文件锁参与关系（enrollmentmutate.go + EnrollCreator/LeavePlan/SetEnrollmentState 入口）：
// 合法前置状态、重复调用的幂等结论、时间戳三态（保持/写入/清零）、
// 确认版本只能单向前移，以及「暂停是运营处置、退出是合约关系」的分工。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/creator-revenue/model"
	"go-video/services/creator-revenue/rpc"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

func runEnrollTx(db *fakeDB, a *enrollmentAction) (*model.Enrollment, bool, error) {
	var (
		row     *model.Enrollment
		changed bool
	)
	err := fakeConn{db: db}.TransactCtx(context.Background(),
		func(ctx context.Context, tx sqlx.Session) error {
			var err error
			row, changed, err = applyEnrollmentTransition(ctx, tx, a)
			return err
		})
	return row, changed, err
}

// seedEnrollmentAt 放一条带明确时间戳的参与关系（便于断言「有没有被推后」）。
func seedEnrollmentAt(db *fakeDB, mid int64, state int32, agreed, enrolledAt, leftAt int64, remark string) *model.Enrollment {
	return db.addEnrollment(&model.Enrollment{
		Mid: mid, State: state, AgreedRuleVersion: agreed,
		EnrolledAt: enrolledAt, LeftAt: leftAt,
		Operator: selfOperator, Remark: remark,
		Ctime: enrolledAt, Mtime: enrolledAt,
	})
}

const (
	oldEnrolledAt = int64(1_700_000_000) // 2023-11 的一个固定参加时间，便于断言「没被推到今天」
	oldLeftAt     = int64(1_710_000_000) // 上一次退出时间
)

// ---------------------------------------------------------------- 状态推进本体

// 重新参加：enrolled_at 刷新、left_at 清零（IF(? < 0, 0, ...) 的三态语义）。
func TestApplyEnrollmentTransitionReEnrollClearsLeftAt(t *testing.T) {
	_, db := newTestSvc(t)
	seedEnrollmentAt(db, txMid, model.EnrollmentStateLeft, 3, oldEnrolledAt, oldLeftAt, "自己退出的")

	row, changed, err := runEnrollTx(db, &enrollmentAction{
		Mid: txMid, ToState: model.EnrollmentStateEnrolled,
		AllowFrom:     []int32{model.EnrollmentStateLeft},
		AgreedVersion: 9, TouchEnrolledAt: true, ClearLeftAt: true,
		Operator: selfOperator, TransitionLabel: "参加分成计划",
	})
	mustNoErr(t, err)
	if !changed {
		t.Fatal("LEFT→ENROLLED 是一次真实变更，必须 changed=true")
	}
	if row.State != model.EnrollmentStateEnrolled {
		t.Fatalf("状态未推进：%+v", row)
	}
	if row.EnrolledAt <= oldEnrolledAt {
		t.Fatalf("重新参加必须刷新 enrolled_at：%d -> %d", oldEnrolledAt, row.EnrolledAt)
	}
	if row.LeftAt != 0 {
		t.Fatalf("重新参加后 left_at 必须清零，否则「上次退出时间」会和新周期混在一起：%d", row.LeftAt)
	}
	if row.AgreedRuleVersion != 9 {
		t.Fatalf("重新确认的规则版本未写入：%+v", row)
	}
	if row.Remark != "" {
		t.Fatalf("本次没给 remark 却写了值：%q", row.Remark)
	}
}

// AgreedVersion=0 表示「保持原值」，TouchLeftAt 只写 left_at。
func TestApplyEnrollmentTransitionKeepsUntouchedColumns(t *testing.T) {
	_, db := newTestSvc(t)
	seedEnrollmentAt(db, txMid, model.EnrollmentStateEnrolled, 4, oldEnrolledAt, 0, "在计划内")

	row, changed, err := runEnrollTx(db, &enrollmentAction{
		Mid: txMid, ToState: model.EnrollmentStateLeft,
		AllowFrom:   []int32{model.EnrollmentStateEnrolled},
		TouchLeftAt: true, Operator: selfOperator, TransitionLabel: "退出分成计划",
	})
	mustNoErr(t, err)
	if !changed {
		t.Fatal("ENROLLED→LEFT 必须 changed=true")
	}
	if row.EnrolledAt != oldEnrolledAt {
		t.Fatalf("退出不能改写参加时间：%d", row.EnrolledAt)
	}
	if row.AgreedRuleVersion != 4 {
		t.Fatalf("AgreedVersion=0 必须保持原确认版本：%+v", row)
	}
	if row.LeftAt <= oldEnrolledAt {
		t.Fatalf("未写入退出时间：%+v", row)
	}
}

// 幂等：已在目标状态时零写入 —— 重复调用不得把 enrolled_at/left_at 推到今天，
// 也不得覆盖当时的 operator/remark。
func TestApplyEnrollmentTransitionDuplicateIsNoOp(t *testing.T) {
	cases := []struct {
		name  string
		state int32
	}{
		{"重复参加", model.EnrollmentStateEnrolled},
		{"重复退出", model.EnrollmentStateLeft},
		{"重复暂停", model.EnrollmentStateSuspended},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, db := newTestSvc(t)
			seedEnrollmentAt(db, txMid, c.state, 3, oldEnrolledAt, oldLeftAt, "第一次的处置原因")
			before := len(db.calls)

			row, changed, err := runEnrollTx(db, &enrollmentAction{
				Mid: txMid, ToState: c.state,
				AllowFrom: []int32{model.EnrollmentStateEnrolled, model.EnrollmentStateLeft,
					model.EnrollmentStateSuspended},
				AgreedVersion: 99, TouchEnrolledAt: true, TouchLeftAt: true,
				Operator: "ops-77", Remark: "第二次的原因", TransitionLabel: "重复动作",
			})
			mustNoErr(t, err)
			if changed {
				t.Fatal("同状态重复调用必须 changed=false")
			}
			if row.Operator != selfOperator || row.Remark != "第一次的处置原因" {
				t.Fatalf("重复调用覆盖了当时的责任人/原因：%+v", row)
			}
			if row.EnrolledAt != oldEnrolledAt || row.LeftAt != oldLeftAt {
				t.Fatalf("重复调用推后了时间戳：enrolled=%d left=%d", row.EnrolledAt, row.LeftAt)
			}
			if row.AgreedRuleVersion != 3 {
				t.Fatalf("重复调用改动了确认版本：%+v", row)
			}
			// 只允许锁行读取，任何 UPDATE 都是多余的写入。
			if db.countCallsAfter(before, "upd:") != 0 {
				t.Fatalf("幂等路径写了主表：%v", db.calls[before:])
			}
		})
	}
}

// 前置状态白名单：不在集合内一律拒绝，且拒绝时零写入。
func TestApplyEnrollmentTransitionRejectsIllegalPriorState(t *testing.T) {
	cases := []struct {
		name      string
		cur       int32
		to        int32
		allowFrom []int32
	}{
		{"违规暂停中不能自助重新参加", model.EnrollmentStateSuspended, model.EnrollmentStateEnrolled,
			[]int32{model.EnrollmentStateLeft}},
		{"已退出的人不能被暂停", model.EnrollmentStateLeft, model.EnrollmentStateSuspended,
			[]int32{model.EnrollmentStateEnrolled}},
		{"退出只能从在计划内或暂停中发起", model.EnrollmentStateEnrolled, model.EnrollmentStateLeft,
			[]int32{model.EnrollmentStateSuspended}},
		{"白名单为空一律拒绝", model.EnrollmentStateEnrolled, model.EnrollmentStateSuspended, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, db := newTestSvc(t)
			seedEnrollmentAt(db, txMid, c.cur, 3, oldEnrolledAt, oldLeftAt, "原样")
			before := len(db.calls)

			_, _, err := runEnrollTx(db, &enrollmentAction{
				Mid: txMid, ToState: c.to, AllowFrom: c.allowFrom,
				AgreedVersion: 9, TouchEnrolledAt: true, ClearLeftAt: true,
				Operator: selfOperator, TransitionLabel: "参加分成计划",
			})
			mustErrIs(t, err, model.ErrEnrollmentStateTransition)
			if db.countCallsAfter(before, "upd:") != 0 {
				t.Fatal("状态机拒绝后仍有写入")
			}
			got := db.enrollmentByMid(txMid)
			if got.State != c.cur || got.EnrolledAt != oldEnrolledAt || got.LeftAt != oldLeftAt {
				t.Fatalf("被拒的动作改动了数据：%+v", got)
			}
		})
	}
}

// 没有参与关系就没有「退出」这回事：硬造一行 LEFT 会让名单里凭空多出一个从没参加过的人。
func TestApplyEnrollmentTransitionRequiresExistingRow(t *testing.T) {
	_, db := newTestSvc(t)
	_, _, err := runEnrollTx(db, &enrollmentAction{
		Mid: txMid, ToState: model.EnrollmentStateLeft,
		AllowFrom: []int32{model.EnrollmentStateEnrolled}, TouchLeftAt: true,
		Operator: selfOperator, TransitionLabel: "退出分成计划",
	})
	mustErrIs(t, err, model.ErrEnrollmentNotFound)
	if len(db.enrollments) != 0 {
		t.Fatalf("找不到行却凭空建行：%+v", db.enrollments)
	}
}

// CAS 以 fromState 为条件：行锁下理论不会 miss，真 miss 说明有人绕锁改数，
// 必须回可重试错误而不是假装成功。
func TestApplyEnrollmentTransitionCasMissIsRetryable(t *testing.T) {
	_, db := newTestSvc(t)
	seedEnrollmentAt(db, txMid, model.EnrollmentStateEnrolled, 3, oldEnrolledAt, 0, "")
	db.noRowsFor["upd:cr_enrollment.transition"] = true

	_, _, err := runEnrollTx(db, &enrollmentAction{
		Mid: txMid, ToState: model.EnrollmentStateLeft,
		AllowFrom: []int32{model.EnrollmentStateEnrolled}, TouchLeftAt: true,
		Operator: selfOperator, TransitionLabel: "退出分成计划",
	})
	mustErrIs(t, err, model.ErrConcurrentUpdate)
	got := db.enrollmentByMid(txMid)
	if got.State != model.EnrollmentStateEnrolled || got.LeftAt != 0 {
		t.Fatalf("并发失败却留下了部分变更：%+v", got)
	}
}

func TestStateAllowedAndStateFilter(t *testing.T) {
	allow := []int32{model.EnrollmentStateEnrolled, model.EnrollmentStateLeft}
	if !stateAllowed(allow, model.EnrollmentStateEnrolled) {
		t.Fatal("白名单内的状态必须放行")
	}
	if stateAllowed(allow, model.EnrollmentStateSuspended) {
		t.Fatal("白名单外的状态必须拒绝")
	}
	if stateAllowed(nil, model.EnrollmentStateEnrolled) {
		t.Fatal("空白名单不能放行任何状态")
	}

	// 列表接口的过滤值：0 表示不过滤，枚举外的值必须报错而不是回空名单。
	for _, v := range []int32{model.EnrollmentStateUnspecified, model.EnrollmentStateEnrolled,
		model.EnrollmentStateLeft, model.EnrollmentStateSuspended} {
		if err := validEnrollmentStateFilter(v); err != nil {
			t.Fatalf("合法状态 %d 被拒：%v", v, err)
		}
	}
	for _, v := range []int32{-1, 4, 99} {
		mustErrIs(t, validEnrollmentStateFilter(v), model.ErrEnrollmentStateTransition)
	}
}

// ---------------------------------------------------------------- EnrollCreator 入口

func TestEnrollCreatorFirstEnrollInsertsRow(t *testing.T) {
	ctx, db := newTestSvc(t)
	l := NewEnrollCreatorLogic(context.Background(), ctx)

	reply, err := l.EnrollCreator(&rpc.EnrollCreatorReq{
		Mid: txMid, AgreedRuleVersion: 7, Operator: selfOperator, RequestId: "req-enroll",
	})
	mustNoErr(t, err)
	if reply.Duplicated {
		t.Fatal("首次参加必须 duplicated=false")
	}
	if reply.Enrollment.State != rpc.EnrollmentState(model.EnrollmentStateEnrolled) {
		t.Fatalf("参加后状态错误：%+v", reply.Enrollment)
	}
	row := db.enrollmentByMid(txMid)
	if row == nil {
		t.Fatal("参与关系未落库")
	}
	if row.AgreedRuleVersion != 7 {
		t.Fatalf("未记录确认的规则版本：参与者的「当时看到哪套单价」只能靠这个字段解释：%+v", row)
	}
	if row.EnrolledAt == 0 || row.Ctime == 0 || row.Mtime == 0 {
		t.Fatalf("时间戳缺失：%+v", row)
	}
	if row.LeftAt != 0 {
		t.Fatalf("新参加不该有 left_at：%+v", row)
	}
	if len(db.enrollments) != 1 {
		t.Fatalf("多出行：%d", len(db.enrollments))
	}
}

func TestEnrollCreatorGates(t *testing.T) {
	ctx, db := newTestSvc(t)
	l := NewEnrollCreatorLogic(context.Background(), ctx)

	newReq := func(mut func(*rpc.EnrollCreatorReq)) *rpc.EnrollCreatorReq {
		r := &rpc.EnrollCreatorReq{Mid: txMid, AgreedRuleVersion: 7, Operator: selfOperator, RequestId: "req-1"}
		mut(r)
		return r
	}
	cases := []struct {
		name   string
		in     *rpc.EnrollCreatorReq
		target error
	}{
		// 没确认过条款的人不能开始产生应计。
		{"必须带已确认规则版本", newReq(func(r *rpc.EnrollCreatorReq) { r.AgreedRuleVersion = 0 }),
			model.ErrAgreedRuleVersionRequired},
		{"规则版本不能为负", newReq(func(r *rpc.EnrollCreatorReq) { r.AgreedRuleVersion = -3 }),
			model.ErrAgreedRuleVersionRequired},
		{"mid 必须是真实账号", newReq(func(r *rpc.EnrollCreatorReq) { r.Mid = 0 }), model.ErrInvalidMid},
		{"operator 必填", newReq(func(r *rpc.EnrollCreatorReq) { r.Operator = "" }), model.ErrOperatorRequired},
		{"request_id 必填", newReq(func(r *rpc.EnrollCreatorReq) { r.RequestId = "" }), model.ErrRequestIDRequired},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := l.EnrollCreator(c.in)
			mustErrIs(t, err, c.target)
		})
	}
	if len(db.enrollments) != 0 {
		t.Fatalf("校验类失败写了参与关系：%+v", db.enrollments)
	}

	// DB 故障必须上抛，不能退化成「参加失败但回一个空结论」。
	db.failOn["fake:cr_enrollment.Insert"] = errors.New("connection refused")
	if _, err := l.EnrollCreator(&rpc.EnrollCreatorReq{
		Mid: txMid, AgreedRuleVersion: 7, Operator: selfOperator, RequestId: "req-1",
	}); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("插入故障未上抛：%v", err)
	}
}

func TestEnrollCreatorResurrectFromLeft(t *testing.T) {
	ctx, db := newTestSvc(t)
	seedEnrollmentAt(db, txMid, model.EnrollmentStateLeft, 3, oldEnrolledAt, oldLeftAt, "自己退出的")
	l := NewEnrollCreatorLogic(context.Background(), ctx)

	reply, err := l.EnrollCreator(&rpc.EnrollCreatorReq{
		Mid: txMid, AgreedRuleVersion: 5, Operator: selfOperator, RequestId: "req-2",
	})
	mustNoErr(t, err)
	if reply.Duplicated {
		t.Fatal("退出后重新参加是一次新的参加，不能报 duplicated")
	}
	if reply.Enrollment.LeftAt != 0 {
		t.Fatalf("应答里仍带着上次退出时间：%+v", reply.Enrollment)
	}
	got := db.enrollmentByMid(txMid)
	if got.State != model.EnrollmentStateEnrolled || got.LeftAt != 0 || got.EnrolledAt <= oldEnrolledAt {
		t.Fatalf("重新参加未落库：%+v", got)
	}
	// 当前实现：真实变更会把 remark/operator 一起换成这次动作的（本表没有变更台账，
	// remark 语义是「最近一次动作的备注」），所以重新参加会擦掉上一次的退出原因。
	// 这是「参与关系无历史表」这一已知缺口的直接后果，已进交付报告。
	if got.Remark != "" {
		t.Fatalf("重新参加的 remark 写成了本次动作之外的值：%q", got.Remark)
	}
}

// 违规暂停期间不接受自助重新参加：否则「暂停」等价于自己重进一次计划就失效。
func TestEnrollCreatorRejectsSuspended(t *testing.T) {
	ctx, db := newTestSvc(t)
	seedEnrollmentAt(db, txMid, model.EnrollmentStateSuspended, 3, oldEnrolledAt, 0, "刷量处置")
	l := NewEnrollCreatorLogic(context.Background(), ctx)

	before := len(db.calls)
	_, err := l.EnrollCreator(&rpc.EnrollCreatorReq{
		Mid: txMid, AgreedRuleVersion: 8, Operator: selfOperator, RequestId: "req-3",
	})
	mustErrIs(t, err, model.ErrEnrollmentSuspended)
	if db.countCallsAfter(before, "upd:") != 0 || db.countCallsAfter(before, "ins:") != 0 {
		t.Fatal("被拒的自助参加仍尝试写入")
	}
	got := db.enrollmentByMid(txMid)
	if got.State != model.EnrollmentStateSuspended || got.AgreedRuleVersion != 3 {
		t.Fatalf("暂停期间被自助解除了：%+v", got)
	}
}

// 已在计划内：重复调用既不刷新参加时间，也只有「确认了更靠后的版本」才前移确认记录，
// 且前移是单向的。
func TestEnrollCreatorInPlanOnlyAdvancesAgreedVersion(t *testing.T) {
	ctx, db := newTestSvc(t)
	seedEnrollmentAt(db, txMid, model.EnrollmentStateEnrolled, 5, oldEnrolledAt, 0, "在计划内")
	l := NewEnrollCreatorLogic(context.Background(), ctx)

	// 同一个版本：零写入。
	reply, err := l.EnrollCreator(&rpc.EnrollCreatorReq{
		Mid: txMid, AgreedRuleVersion: 5, Operator: selfOperator, RequestId: "req-same",
	})
	mustNoErr(t, err)
	if !reply.Duplicated {
		t.Fatal("已在计划内必须 duplicated=true")
	}
	if reply.Enrollment.EnrolledAt != oldEnrolledAt {
		t.Fatalf("重复参加把参加时间推到今天的了：%+v", reply.Enrollment)
	}
	if len(db.enrollments) != 1 {
		t.Fatalf("重复参加多出一行：%d", len(db.enrollments))
	}

	// 更靠后的版本：前移确认记录，但仍不算新参加。
	reply2, err := l.EnrollCreator(&rpc.EnrollCreatorReq{
		Mid: txMid, AgreedRuleVersion: 8, Operator: selfOperator, RequestId: "req-next",
	})
	mustNoErr(t, err)
	if !reply2.Duplicated {
		t.Fatal("仅前移确认版本不算新参加")
	}
	got := db.enrollmentByMid(txMid)
	if got.AgreedRuleVersion != 8 {
		t.Fatalf("重新确认的新条款版本没记下：%+v", got)
	}
	if got.EnrolledAt != oldEnrolledAt || got.State != model.EnrollmentStateEnrolled {
		t.Fatalf("前移确认版本改动了参加事实：%+v", got)
	}

	// 更旧的版本：model 侧 agreed_rule_version < ? 守卫保证历史确认不会被降级覆盖。
	if _, err := l.EnrollCreator(&rpc.EnrollCreatorReq{
		Mid: txMid, AgreedRuleVersion: 2, Operator: selfOperator, RequestId: "req-old",
	}); err != nil {
		t.Fatalf("旧版本重放不该报错：%v", err)
	}
	if v := db.enrollmentByMid(txMid).AgreedRuleVersion; v != 8 {
		t.Fatalf("确认版本被降级覆盖：%d", v)
	}
}

// 并发首投：事务外读不到行，INSERT 撞 uniq_mid —— 必须按「已参加」重新读一次并保持幂等结论。
func TestEnrollCreatorConcurrentFirstInsertStaysIdempotent(t *testing.T) {
	ctx, db := newTestSvc(t)
	seedEnrollmentAt(db, txMid, model.EnrollmentStateEnrolled, 3, oldEnrolledAt, 0, "并发者先建的行")
	db.missNext["fake:enrollments.FindOne"] = 1 // 只有第一次快照读看不到
	l := NewEnrollCreatorLogic(context.Background(), ctx)

	reply, err := l.EnrollCreator(&rpc.EnrollCreatorReq{
		Mid: txMid, AgreedRuleVersion: 3, Operator: selfOperator, RequestId: "req-race",
	})
	mustNoErr(t, err)
	if !reply.Duplicated {
		t.Fatal("撞 uniq_mid 后必须按「已参加」给结论")
	}
	if reply.Enrollment.EnrolledAt != oldEnrolledAt || reply.Enrollment.Remark != "并发者先建的行" {
		t.Fatalf("重读后覆盖了并发者的行：%+v", reply.Enrollment)
	}
	if len(db.enrollments) != 1 {
		t.Fatalf("并发首投造出了第二行：%d", len(db.enrollments))
	}
}

// ---------------------------------------------------------------- LeavePlan 入口

func TestLeavePlanSelfExitAndOperatorReason(t *testing.T) {
	ctx, db := newTestSvc(t)
	seedEnrollmentAt(db, txMid, model.EnrollmentStateEnrolled, 3, oldEnrolledAt, 0, "")
	l := NewLeavePlanLogic(context.Background(), ctx)

	// 创作者自助退出：proto 不要求原因。
	reply, err := l.LeavePlan(&rpc.LeavePlanReq{Mid: txMid, Operator: selfOperator, RequestId: "req-leave"})
	mustNoErr(t, err)
	if reply.Duplicated {
		t.Fatal("首次退出必须 duplicated=false")
	}
	got := db.enrollmentByMid(txMid)
	if got.State != model.EnrollmentStateLeft || got.LeftAt <= oldEnrolledAt {
		t.Fatalf("退出未落库：%+v", got)
	}
	if got.EnrolledAt != oldEnrolledAt {
		t.Fatalf("退出改写了参加时间：%+v", got)
	}
	if got.Remark != "" {
		t.Fatalf("自助退出没给原因却写了 remark：%q", got.Remark)
	}

	// 重复退出：不刷新 left_at，否则「退出后还有没有收益」的边界会被事后改写。
	before := got.LeftAt
	again, err := l.LeavePlan(&rpc.LeavePlanReq{Mid: txMid, Operator: selfOperator, RequestId: "req-leave-2"})
	mustNoErr(t, err)
	if !again.Duplicated {
		t.Fatal("重复退出必须 duplicated=true")
	}
	if db.enrollmentByMid(txMid).LeftAt != before {
		t.Fatalf("重复退出把退出时间推后了：%+v", db.enrollmentByMid(txMid))
	}

	// 运营代操作：必须说明原因。
	ctx2, db2 := newTestSvc(t)
	seedEnrollmentAt(db2, txMid, model.EnrollmentStateEnrolled, 3, oldEnrolledAt, 0, "")
	l2 := NewLeavePlanLogic(context.Background(), ctx2)
	emptyErr := mustLeaveErr(l2, "ops-77", "")
	mustErrIs(t, emptyErr, model.ErrReasonRequired)
	if !strings.Contains(emptyErr.Error(), "运营代操作退出计划必须说明原因") {
		t.Fatalf("代操作缺原因的提示丢了：%v", emptyErr)
	}
	// 原因超长是「写了但太长」，不能被吞成「没写」：哨兵必须是 ErrTextTooLong。
	longErr := mustLeaveErr(l2, "ops-77", strings.Repeat("理", model.MaxReasonBytes+1))
	mustErrIs(t, longErr, model.ErrTextTooLong)
	if errors.Is(longErr, model.ErrReasonRequired) {
		t.Fatal("超长原因被误报成缺原因")
	}
	if e := db2.enrollmentByMid(txMid); e.State != model.EnrollmentStateEnrolled {
		t.Fatalf("被拒的代操作退出了计划：%+v", e)
	}

	// 代操作成功时原因落 remark，能被追问。
	reply3, err := l2.LeavePlan(&rpc.LeavePlanReq{
		Mid: txMid, Operator: "ops-77", RequestId: "req-3", Reason: "作者申诉后代为解约",
	})
	mustNoErr(t, err)
	if reply3.Duplicated {
		t.Fatal("代操作退出是真实变更")
	}
	if e := db2.enrollmentByMid(txMid); e.State != model.EnrollmentStateLeft ||
		e.Remark != "作者申诉后代为解约" || e.Operator != "ops-77" {
		t.Fatalf("代操作未留下原因：%+v", e)
	}
}

func mustLeaveErr(l *LeavePlanLogic, operator, reason string) error {
	_, err := l.LeavePlan(&rpc.LeavePlanReq{Mid: txMid, Operator: operator, RequestId: "req-x", Reason: reason})
	return err
}

// 暂停中也可以解约，但从未参加（无行）没有「退出」这回事。
func TestLeavePlanPriorStates(t *testing.T) {
	cases := []struct {
		name    string
		state   int32
		wantErr error
	}{
		{"在计划内可退出", model.EnrollmentStateEnrolled, nil},
		{"暂停中可退出", model.EnrollmentStateSuspended, nil},
		{"从未参加不可退出", 0, model.ErrEnrollmentNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, db := newTestSvc(t)
			if c.state != 0 {
				seedEnrollmentAt(db, txMid, c.state, 3, oldEnrolledAt, 0, "")
			}
			l := NewLeavePlanLogic(context.Background(), ctx)
			_, err := l.LeavePlan(&rpc.LeavePlanReq{Mid: txMid, Operator: selfOperator, RequestId: "req-1"})
			if c.wantErr != nil {
				mustErrIs(t, err, c.wantErr)
				if len(db.enrollments) != 0 {
					t.Fatalf("凭空造出一行 LEFT：%+v", db.enrollments)
				}
				return
			}
			mustNoErr(t, err)
			if got := db.enrollmentByMid(txMid); got.State != model.EnrollmentStateLeft {
				t.Fatalf("未退出：%+v", got)
			}
		})
	}
	// request_id 仍是必填（虽然本表没有变更台账，幂等押在状态机上）。
	ctx, db := newTestSvc(t)
	seedEnrollmentAt(db, txMid, model.EnrollmentStateEnrolled, 3, oldEnrolledAt, 0, "")
	l := NewLeavePlanLogic(context.Background(), ctx)
	_, err := l.LeavePlan(&rpc.LeavePlanReq{Mid: txMid, Operator: selfOperator})
	mustErrIs(t, err, model.ErrRequestIDRequired)
	if db.enrollmentByMid(txMid).State != model.EnrollmentStateEnrolled {
		t.Fatal("缺 request_id 的退出仍执行了")
	}
}

// ---------------------------------------------------------------- SetEnrollmentState 入口

func TestSetEnrollmentStateSuspendAndRestore(t *testing.T) {
	ctx, db := newTestSvc(t)
	seedEnrollmentAt(db, txMid, model.EnrollmentStateEnrolled, 3, oldEnrolledAt, 0, "")
	l := NewSetEnrollmentStateLogic(context.Background(), ctx)

	reply, err := l.SetEnrollmentState(&rpc.SetEnrollmentStateReq{
		Mid: txMid, TargetState: rpc.EnrollmentState(model.EnrollmentStateSuspended),
		Operator: "ops-77", RequestId: "req-suspend", Reason: "刷量核查",
	})
	mustNoErr(t, err)
	if reply.Duplicated {
		t.Fatal("首次暂停是真实变更")
	}
	got := db.enrollmentByMid(txMid)
	if got.State != model.EnrollmentStateSuspended || got.Operator != "ops-77" || got.Remark != "刷量核查" {
		t.Fatalf("暂停未落库或未留原因：%+v", got)
	}
	// 暂停是处置不是合约变更：两个时间戳都不该动。
	if got.EnrolledAt != oldEnrolledAt || got.LeftAt != 0 {
		t.Fatalf("暂停改动了时间戳：%+v", got)
	}

	// 重复暂停：duplicated=true 且保留第一次的处置原因。
	again, err := l.SetEnrollmentState(&rpc.SetEnrollmentStateReq{
		Mid: txMid, TargetState: rpc.EnrollmentState(model.EnrollmentStateSuspended),
		Operator: "ops-77", RequestId: "req-suspend-2", Reason: "第二次的原因",
	})
	mustNoErr(t, err)
	if !again.Duplicated {
		t.Fatal("重复暂停必须 duplicated=true")
	}
	if got := db.enrollmentByMid(txMid); got.Remark != "刷量核查" {
		t.Fatalf("重复暂停覆盖了第一次的处置原因：%q", got.Remark)
	}

	// 运营解除暂停：回到 ENROLLED，参加时间仍是最初那次。
	reply3, err := l.SetEnrollmentState(&rpc.SetEnrollmentStateReq{
		Mid: txMid, TargetState: rpc.EnrollmentState(model.EnrollmentStateEnrolled),
		Operator: "ops-77", RequestId: "req-restore", Reason: "核查完毕解除",
	})
	mustNoErr(t, err)
	if reply3.Duplicated {
		t.Fatal("解除暂停是真实变更")
	}
	if got := db.enrollmentByMid(txMid); got.State != model.EnrollmentStateEnrolled ||
		got.EnrolledAt != oldEnrolledAt || got.Remark != "核查完毕解除" {
		t.Fatalf("解除暂停结论错误：%+v", got)
	}
}

func TestSetEnrollmentStateGates(t *testing.T) {
	ctx, db := newTestSvc(t)
	seedEnrollmentAt(db, txMid, model.EnrollmentStateEnrolled, 3, oldEnrolledAt, 0, "")
	l := NewSetEnrollmentStateLogic(context.Background(), ctx)

	// 暂停/恢复是违规处置：被处置者不能自己解除处置。
	_, err := l.SetEnrollmentState(&rpc.SetEnrollmentStateReq{
		Mid: txMid, TargetState: rpc.EnrollmentState(model.EnrollmentStateSuspended),
		Operator: selfOperator, RequestId: "req-1", Reason: "自己解除自己",
	})
	mustErrIs(t, err, model.ErrForbidden)

	// LEFT 属合约关系，走 LeavePlan/EnrollCreator；本接口只处理 ENROLLED↔SUSPENDED。
	for _, target := range []rpc.EnrollmentState{
		rpc.EnrollmentState(model.EnrollmentStateLeft),
		rpc.EnrollmentState(model.EnrollmentStateUnspecified),
		rpc.EnrollmentState(9),
	} {
		_, err := l.SetEnrollmentState(&rpc.SetEnrollmentStateReq{
			Mid: txMid, TargetState: target, Operator: "ops-77", RequestId: "req-2", Reason: "越界目标",
		})
		mustErrIs(t, err, model.ErrEnrollmentStateTransition)
	}
	_, err = l.SetEnrollmentState(&rpc.SetEnrollmentStateReq{
		Mid: txMid, TargetState: rpc.EnrollmentState(model.EnrollmentStateSuspended),
		Operator: "ops-77", RequestId: "req-3",
	})
	mustErrIs(t, err, model.ErrReasonRequired)
	_, err = l.SetEnrollmentState(&rpc.SetEnrollmentStateReq{
		Mid: txMid, TargetState: rpc.EnrollmentState(model.EnrollmentStateSuspended),
		Operator: "ops-77", Reason: "缺幂等键",
	})
	mustErrIs(t, err, model.ErrRequestIDRequired)
	_, err = l.SetEnrollmentState(&rpc.SetEnrollmentStateReq{
		Mid: 0, TargetState: rpc.EnrollmentState(model.EnrollmentStateSuspended),
		Operator: "ops-77", RequestId: "req-4", Reason: "账号非法",
	})
	mustErrIs(t, err, model.ErrInvalidMid)

	if got := db.enrollmentByMid(txMid); got.State != model.EnrollmentStateEnrolled || got.Remark != "" {
		t.Fatalf("被拒的处置动作改动了数据：%+v", got)
	}

	// 已 LEFT 的人不能被「暂停」：白名单只有 ENROLLED。
	ctx2, db2 := newTestSvc(t)
	seedEnrollmentAt(db2, txMid, model.EnrollmentStateLeft, 3, oldEnrolledAt, oldLeftAt, "退出的")
	l2 := NewSetEnrollmentStateLogic(context.Background(), ctx2)
	_, err = l2.SetEnrollmentState(&rpc.SetEnrollmentStateReq{
		Mid: txMid, TargetState: rpc.EnrollmentState(model.EnrollmentStateSuspended),
		Operator: "ops-77", RequestId: "req-5", Reason: "对已退出者补处置",
	})
	mustErrIs(t, err, model.ErrEnrollmentStateTransition)
	if got := db2.enrollmentByMid(txMid); got.State != model.EnrollmentStateLeft || got.LeftAt != oldLeftAt {
		t.Fatalf("被拒的处置改动了合约状态：%+v", got)
	}
}
