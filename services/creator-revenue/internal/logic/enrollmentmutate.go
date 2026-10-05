// 本文件是 logic 包的手写扩展（参与关系状态机的共用推进），不是 goctl 生成产物。

package logic

import (
	"context"
	"fmt"

	"go-video/services/creator-revenue/model"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// enrollmentAction 是一次参与关系状态推进的意图。
//
// 参与关系没有独立的变更台账表（proto 未给，本轮也不新增），所以幂等性
// 不靠 request_id 唯一键，而是由状态机本身保证：
// 「已在目标状态」直接回 duplicated=true 且不做任何写入，重复调用不会
// 把 enrolled_at/left_at 推到今天，也不会覆盖当时的 operator/remark。
type enrollmentAction struct {
	Mid       int64
	ToState   int32
	AllowFrom []int32 // 允许的当前状态；不在集合内一律 ErrEnrollmentStateTransition
	// AgreedVersion > 0 时才写入；0 表示保持原值（Transition 的三态语义）。
	AgreedVersion int64
	// TouchEnrolledAt / TouchLeftAt 写时间戳；ClearLeftAt 清零上一次退出时间（重新参加）。
	TouchEnrolledAt bool
	TouchLeftAt     bool
	ClearLeftAt     bool
	Operator        string
	Remark          string
	// TransitionLabel 只出现在错误消息里，说明这是哪个动作，便于网关回给调用方。
	TransitionLabel string
}

// applyEnrollmentTransition 在事务会话内锁行、判状态、CAS 推进并回读真值。
//
// 返回 changed=false 表示「已经是目标状态」的幂等重复，此时 row 是当前行、没有写入。
// 锁行必须在判状态之前：不加锁的「先读后写」会让暂停与参加并发时后写者悄悄赢掉，
// 结果就是「运营刚暂停的账号又被自助恢复」这类无法解释的台账。
func applyEnrollmentTransition(
	ctx context.Context, tx sqlx.Session, a *enrollmentAction,
) (row *model.Enrollment, changed bool, err error) {
	enrollments := model.NewEnrollmentModel(sqlx.NewSqlConnFromSession(tx))

	cur, err := enrollments.LockByMid(ctx, a.Mid)
	if err != nil {
		return nil, false, err
	}
	if cur == nil {
		return nil, false, fmt.Errorf("%w: mid=%d", model.ErrEnrollmentNotFound, a.Mid)
	}
	if cur.State == a.ToState {
		return cur, false, nil
	}
	if !stateAllowed(a.AllowFrom, cur.State) {
		return nil, false, fmt.Errorf("%w: %s 不允许从 state=%d 推进到 state=%d",
			model.ErrEnrollmentStateTransition, a.TransitionLabel, cur.State, a.ToState)
	}

	var enrolledAt, leftAt int64
	if a.TouchEnrolledAt {
		enrolledAt = model.NowUnix()
	}
	switch {
	case a.ClearLeftAt:
		leftAt = -1
	case a.TouchLeftAt:
		leftAt = model.NowUnix()
	}

	ok, err := enrollments.Transition(ctx, a.Mid, cur.State, a.ToState,
		a.AgreedVersion, enrolledAt, leftAt, a.Operator, a.Remark)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		// 行锁下理论不会miss；真miss了说明另有会话绕过了锁（DDL/DBA 改数），
		// 回可重试错误而不是假装成功。
		return nil, false, fmt.Errorf("%w: mid=%d 参与状态推进未命中，请重试", model.ErrConcurrentUpdate, a.Mid)
	}
	after, err := enrollments.FindOne(ctx, a.Mid)
	if err != nil {
		return nil, false, err
	}
	if after == nil {
		return nil, false, fmt.Errorf("%w: mid=%d 提交后读不到", model.ErrEnrollmentNotFound, a.Mid)
	}
	return after, true, nil
}

// stateAllowed 判定当前状态是否在允许集合内。
func stateAllowed(allowed []int32, cur int32) bool {
	for _, s := range allowed {
		if s == cur {
			return true
		}
	}
	return false
}

// validEnrollmentStateFilter 判定列表接口的状态过滤值合法（0 表示不过滤）。
// 越界的状态码必须报错而不是回空名单：「筛选条件写错了」被渲染成「这个状态没人」，
// 运营会据此做加人/清场的决定。
func validEnrollmentStateFilter(v int32) error {
	if v < model.EnrollmentStateUnspecified || v > model.EnrollmentStateSuspended {
		return fmt.Errorf("%w: %d", model.ErrEnrollmentStateTransition, v)
	}
	return nil
}
