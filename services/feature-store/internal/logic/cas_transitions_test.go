// 测试域 2（续）：版本切换 / 状态变更 / 隐私调整的 CAS 与原子性。
//
// 这三条路径都承诺同一件事：「条件更新 + 审计留痕」同生共死，
// 并发变更必须以显式冲突哨兵失败并整体回滚，绝不能留下
// 「指针动了但没人知道」或「审计写了但指针没动」的中间态。
// 并发窗口用替身的 lostUpdate 钩子 *制造*（它只引入一次竞争提交，不放宽任何校验）。
package logic

import (
	"errors"
	"testing"

	"go-video/services/feature-store/model"
	"go-video/services/feature-store/rpc"
)

func callSwitch(f *fixture, in *rpc.SwitchFeatureVersionReq) (*rpc.SwitchFeatureVersionReply, error) {
	return NewSwitchFeatureVersionLogic(f.ctx, f.ServiceContext).SwitchFeatureVersion(in)
}

func switchReq(key string, from, to, expected int32, requestID string) *rpc.SwitchFeatureVersionReq {
	return &rpc.SwitchFeatureVersionReq{
		FeatureKey: key, FromVersion: from, ToVersion: to, ExpectedFromVersion: expected,
		Operator: "admin:tester", Reason: "离线评估通过", RequestId: requestID,
	}
}

// --- 切换 ---

func TestSwitchMovesPointerAndAppendsAuditInOneTransaction(t *testing.T) {
	f := newFixture(t)
	f.registerActive(newDef("user_play_finish_7d", 1))
	f.putDef(newDef("user_play_finish_7d", 2))

	reply, err := callSwitch(f, switchReq("user_play_finish_7d", 1, 2, 1, "req-switch"))
	if err != nil {
		t.Fatalf("SwitchFeatureVersion: %v", err)
	}
	if !reply.GetSwitched() || reply.GetActiveVersion() != 2 {
		t.Fatalf("reply = switched %v active %d, want true/2", reply.GetSwitched(), reply.GetActiveVersion())
	}
	ptr := f.pointers.rows["user_play_finish_7d"]
	if ptr.ActiveVersion != 2 || ptr.PreviousVersion != 1 {
		t.Errorf("pointer = %d/%d, want active 2 previous 1", ptr.ActiveVersion, ptr.PreviousVersion)
	}
	if ptr.LastSwitchID != reply.GetSwitchId() {
		t.Errorf("pointer.last_switch_id=%d but the reply says %d: the two must be the same audit row",
			ptr.LastSwitchID, reply.GetSwitchId())
	}
	audit, ok := f.switches.rows[reply.GetSwitchId()]
	if !ok {
		t.Fatal("the reply names a switch_id that has no audit row")
	}
	if audit.SwitchType != model.SwitchTypeVersionSwitch || audit.FromVersion != 1 || audit.ToVersion != 2 {
		t.Errorf("audit = %s %d->%d, want version_switch 1->2", audit.SwitchType, audit.FromVersion, audit.ToVersion)
	}
	if audit.FromDigest == "" || audit.ToDigest == "" {
		t.Errorf("audit lost the definition digests: %q / %q", audit.FromDigest, audit.ToDigest)
	}
	if audit.RequestID != "req-switch" {
		t.Errorf("audit.request_id=%q, want the idempotency key it was made under", audit.RequestID)
	}
	if begins, commits := f.txBegins(), f.txCommits(); begins != 1 || commits != 1 {
		t.Errorf("begin/commit = %d/%d, want 1/1", begins, commits)
	}
	if len(f.receipts.done) != 1 {
		t.Errorf("receipt done marks=%d, want 1", len(f.receipts.done))
	}
}

func TestSwitchReplayDoesNotMoveThePointerTwice(t *testing.T) {
	f := newFixture(t)
	f.registerActive(newDef("user_play_finish_7d", 1))
	f.putDef(newDef("user_play_finish_7d", 2))
	req := switchReq("user_play_finish_7d", 1, 2, 1, "req-switch-replay")

	if _, err := callSwitch(f, req); err != nil {
		t.Fatalf("first switch: %v", err)
	}
	reply, err := callSwitch(f, req)
	if err != nil {
		t.Fatalf("replayed switch: %v", err)
	}
	if !reply.GetReused() {
		t.Error("reused=false for a replayed request_id")
	}
	if reply.GetActiveVersion() != 2 || reply.GetSwitchId() != 1 {
		t.Errorf("replay = active %d switch_id %d, want the first run's 2/1",
			reply.GetActiveVersion(), reply.GetSwitchId())
	}
	if n := len(f.switches.rows); n != 1 {
		t.Errorf("audit rows=%d, want 1: a replay must not append a second audit entry", n)
	}
	if n := len(f.pointers.switched); n != 1 {
		t.Errorf("pointer CAS successes=%d, want 1", n)
	}
	if f.txBegins() != 1 {
		t.Errorf("transactions=%d, want 1", f.txBegins())
	}
}

func TestSwitchWithStaleFromVersionIsAConflict(t *testing.T) {
	f := newFixture(t)
	f.registerActive(newDef("user_play_finish_7d", 1))
	f.putDef(newDef("user_play_finish_7d", 2))
	// 别的调用者已经切到 v2：这次提交带的还是「我以为当前是 v1」的旧视图。
	f.putPointer("user_play_finish_7d", 2, 1)

	_, err := callSwitch(f, switchReq("user_play_finish_7d", 1, 3, 0, "req-stale"))
	if !errors.Is(err, model.ErrVersionConflict) {
		t.Fatalf("err=%v, want %v", err, model.ErrVersionConflict)
	}
	if ptr := f.pointers.rows["user_play_finish_7d"]; ptr.ActiveVersion != 2 {
		t.Errorf("pointer active=%d, want the concurrent value 2 left intact", ptr.ActiveVersion)
	}
}

func TestSwitchOptimisticExpectedFromVersionIsChecked(t *testing.T) {
	f := newFixture(t)
	f.registerActive(newDef("user_play_finish_7d", 1))
	f.putDef(newDef("user_play_finish_7d", 2))

	_, err := callSwitch(f, switchReq("user_play_finish_7d", 1, 2, 9, "req-optimistic"))
	if !errors.Is(err, model.ErrVersionConflict) {
		t.Fatalf("err=%v, want %v: expected_from_version is the caller's optimistic lock",
			err, model.ErrVersionConflict)
	}
	if ptr := f.pointers.rows["user_play_finish_7d"]; ptr.ActiveVersion != 1 {
		t.Errorf("pointer moved to %d despite the rejected optimistic lock", ptr.ActiveVersion)
	}
}

func TestSwitchConcurrentBumpBetweenLockAndCasIsADetectedConflict(t *testing.T) {
	f := newFixture(t)
	f.registerActive(newDef("user_play_finish_7d", 1))
	f.putDef(newDef("user_play_finish_7d", 2))
	f.putDef(newDef("user_play_finish_7d", 3))
	// 在「FOR UPDATE 读到指针」与「条件 UPDATE 比较」之间，另一笔事务把指针切到了 v3。
	f.pointers.lostUpdate = func(key string) {
		row := f.pointers.rows[key]
		row.ActiveVersion, row.PreviousVersion = 3, 1
		f.pointers.rows[key] = row
	}

	_, err := callSwitch(f, switchReq("user_play_finish_7d", 1, 2, 1, "req-race"))
	if !errors.Is(err, model.ErrVersionConflict) {
		t.Fatalf("err=%v, want %v", err, model.ErrVersionConflict)
	}
	// 回滚必须把整张表还原到事务开始前：竞争者那次提交与我们的失败尝试都不该留下痕迹。
	ptr := f.pointers.rows["user_play_finish_7d"]
	if ptr.ActiveVersion != 1 {
		t.Errorf("pointer active=%d after rollback, want the pre-transaction 1", ptr.ActiveVersion)
	}
	if n := len(f.switches.rows); n != 0 {
		t.Errorf("audit rows=%d after rollback, want 0: a failed switch must not be audited as if it happened", n)
	}
	if len(f.receipts.failed) != 1 {
		t.Errorf("receipt failed marks=%d, want 1 (code %v)", len(f.receipts.failed), f.receipts.failed)
	}
	if f.txCommits() != 0 || f.txRollbacks() != 1 {
		t.Errorf("commit/rollback = %d/%d, want 0/1", f.txCommits(), f.txRollbacks())
	}
}

func TestSwitchAuditFailureRollsBackThePointer(t *testing.T) {
	f := newFixture(t)
	f.registerActive(newDef("user_play_finish_7d", 1))
	f.putDef(newDef("user_play_finish_7d", 2))
	f.switches.errAppend = errors.New("audit table full")

	_, err := callSwitch(f, switchReq("user_play_finish_7d", 1, 2, 1, "req-audit-fail"))
	if err == nil {
		t.Fatal("an audit append failure must fail the whole switch")
	}
	if ptr := f.pointers.rows["user_play_finish_7d"]; ptr.ActiveVersion != 1 {
		t.Errorf("pointer active=%d: the switch committed although its audit row failed — 「指针动了但没人知道」", ptr.ActiveVersion)
	}
	if f.txRollbacks() != 1 {
		t.Errorf("rollbacks=%d, want 1", f.txRollbacks())
	}
}

func TestSwitchBackToPreviousVersionIsAuditedAsRollback(t *testing.T) {
	f := newFixture(t)
	f.registerActive(newDef("user_play_finish_7d", 1))
	f.putDef(newDef("user_play_finish_7d", 2))

	forward, err := callSwitch(f, switchReq("user_play_finish_7d", 1, 2, 1, "req-fwd"))
	if err != nil {
		t.Fatalf("forward switch: %v", err)
	}
	back, err := callSwitch(f, switchReq("user_play_finish_7d", 2, 1, 2, "req-back"))
	if err != nil {
		t.Fatalf("rollback switch: %v", err)
	}
	rec := f.switches.rows[back.GetSwitchId()]
	if rec.SwitchType != model.SwitchTypeRollback {
		t.Errorf("second audit type=%q, want %q: 「切错了又切回去」必须在审计里成对出现",
			rec.SwitchType, model.SwitchTypeRollback)
	}
	if rec.RollbackSwitchID != forward.GetSwitchId() {
		t.Errorf("rollback_switch_id=%d, want it to reference %d", rec.RollbackSwitchID, forward.GetSwitchId())
	}
	if f.switches.rows[forward.GetSwitchId()].SwitchType != model.SwitchTypeVersionSwitch {
		t.Error("the forward move must stay in the audit trail: a rollback is not a delete")
	}
}

func TestSwitchTargetVersionMustBeActive(t *testing.T) {
	f := newFixture(t)
	f.registerActive(newDef("user_play_finish_7d", 1))
	f.putDef(newDef("user_play_finish_7d", 2, withState(model.FeatureStateDraft)))

	_, err := callSwitch(f, switchReq("user_play_finish_7d", 1, 2, 1, "req-draft-target"))
	if !errors.Is(err, model.ErrFeatureNotActive) {
		t.Fatalf("err=%v, want %v", err, model.ErrFeatureNotActive)
	}
	if ptr := f.pointers.rows["user_play_finish_7d"]; ptr.ActiveVersion != 1 {
		t.Errorf("pointer active=%d, want 1: a DRAFT must never become the served version", ptr.ActiveVersion)
	}
}

func TestSwitchTargetWithChangedImmutableFieldsIsRejected(t *testing.T) {
	f := newFixture(t)
	f.registerActive(newDef("user_play_finish_7d", 1))
	// 同一 key 换了值类型：口径在排序侧脚下漂移，必须换新 key。
	f.putDef(newDef("user_play_finish_7d", 2, withScalar(model.ValueTypeDouble)))

	_, err := callSwitch(f, switchReq("user_play_finish_7d", 1, 2, 1, "req-drift"))
	if !errors.Is(err, model.ErrImmutableFieldMismatch) {
		t.Fatalf("err=%v, want %v", err, model.ErrImmutableFieldMismatch)
	}
}

func TestSwitchToTheServingVersionIsAnIdempotentNoOp(t *testing.T) {
	f := newFixture(t)
	f.registerActive(newDef("user_play_finish_7d", 1))
	f.putDef(newDef("user_play_finish_7d", 2))

	reply, err := callSwitch(f, switchReq("user_play_finish_7d", 1, 1, 1, "req-noop"))
	if !errors.Is(err, model.ErrFeatureVersionRequired) {
		t.Fatalf("err=%v, want %v: from==to is a malformed request, not a no-op", err, model.ErrFeatureVersionRequired)
	}
	_ = reply

	// 指针已经是 v2（例如前一次切换的回放丢失）：目标已生效时不追加审计、不改指针。
	f.putPointer("user_play_finish_7d", 2, 1)
	again, err := callSwitch(f, switchReq("user_play_finish_7d", 2, 2, 2, "req-same"))
	if !errors.Is(err, model.ErrFeatureVersionRequired) {
		t.Fatalf("err=%v, want %v", err, model.ErrFeatureVersionRequired)
	}
	_ = again
	if n := len(f.switches.rows); n != 0 {
		t.Errorf("audit rows=%d, want 0", n)
	}
}

func TestSwitchWithoutPointerRowFailsClosed(t *testing.T) {
	f := newFixture(t)
	f.putDef(newDef("user_play_finish_7d", 1))
	f.putDef(newDef("user_play_finish_7d", 2))

	_, err := callSwitch(f, switchReq("user_play_finish_7d", 1, 2, 1, "req-noptr"))
	if !errors.Is(err, model.ErrActiveVersionMissing) {
		t.Fatalf("err=%v, want %v", err, model.ErrActiveVersionMissing)
	}
	if len(f.switches.rows) != 0 {
		t.Error("an audit row was appended for a switch that never happened")
	}
}

// --- 状态变更 ---

func callState(f *fixture, in *rpc.UpdateFeatureStateReq) (*rpc.UpdateFeatureStateReply, error) {
	return NewUpdateFeatureStateLogic(f.ctx, f.ServiceContext).UpdateFeatureState(in)
}

func stateReq(key string, version, state int32, requestID string) *rpc.UpdateFeatureStateReq {
	return &rpc.UpdateFeatureStateReq{
		FeatureKey: key, Version: version, State: rpc.FeatureState(state),
		Operator: "admin:tester", Reason: "评审通过", RequestId: requestID,
	}
}

func TestActivationPreconditionsFailWithExplicitReasons(t *testing.T) {
	t.Run("unfinished backfill job", func(t *testing.T) {
		f := newFixture(t)
		f.putDef(newDef("user_play_finish_7d", 1, withState(model.FeatureStateDraft)))
		f.putPointer("user_play_finish_7d", 0, 0)
		f.backfills.rows[7] = model.BackfillJob{
			JobID: 7, FeatureKey: "user_play_finish_7d", Version: 1,
			State: model.BackfillStateRunning, RequestID: "job-1", Operator: "system:worker",
			LeaseOwner: "pod#1", LeaseExpireAt: testNow + 60,
		}

		_, err := callState(f, stateReq("user_play_finish_7d", 1, model.FeatureStateActive, "req-act-1"))
		if !errors.Is(err, model.ErrFeatureStateTransition) {
			t.Fatalf("err=%v, want %v", err, model.ErrFeatureStateTransition)
		}
	})
	t.Run("another version already serves the key", func(t *testing.T) {
		f := newFixture(t)
		f.putDef(newDef("user_play_finish_7d", 2, withState(model.FeatureStateActive)))
		f.putDef(newDef("user_play_finish_7d", 1, withState(model.FeatureStateDraft)))
		f.putPointer("user_play_finish_7d", 2, 0)

		_, err := callState(f, stateReq("user_play_finish_7d", 1, model.FeatureStateActive, "req-act-2"))
		if !errors.Is(err, model.ErrMultipleActiveVersions) {
			t.Fatalf("err=%v, want %v", err, model.ErrMultipleActiveVersions)
		}
		if ptr := f.pointers.rows["user_play_finish_7d"]; ptr.ActiveVersion != 2 {
			t.Errorf("pointer active=%d: two versions would be live at once", ptr.ActiveVersion)
		}
	})
	t.Run("no values and no previous version", func(t *testing.T) {
		f := newFixture(t)
		f.putDef(newDef("user_play_finish_7d", 1, withState(model.FeatureStateDraft)))
		f.putPointer("user_play_finish_7d", 0, 0)

		_, err := callState(f, stateReq("user_play_finish_7d", 1, model.FeatureStateActive, "req-act-3"))
		if !errors.Is(err, model.ErrFeatureStateTransition) {
			t.Fatalf("err=%v, want %v", err, model.ErrFeatureStateTransition)
		}
	})
	t.Run("retired cannot be revived", func(t *testing.T) {
		f := newFixture(t)
		f.putDef(newDef("user_play_finish_7d", 1, withState(model.FeatureStateRetired)))
		f.putPointer("user_play_finish_7d", 0, 1)

		_, err := callState(f, stateReq("user_play_finish_7d", 1, model.FeatureStateActive, "req-act-4"))
		if !errors.Is(err, model.ErrFeatureStateTransition) {
			t.Fatalf("err=%v, want %v", err, model.ErrFeatureStateTransition)
		}
	})
}

func TestActivationMovesPointerAndAuditsTogether(t *testing.T) {
	f := newFixture(t)
	d := f.putDef(newDef("user_play_finish_7d", 1, withState(model.FeatureStateDraft)))
	f.putPointer("user_play_finish_7d", 0, 0)
	f.putInt64(d, 1, testMid, 5, testNow-60) // 有值才允许首次上线

	reply, err := callState(f, stateReq("user_play_finish_7d", 1, model.FeatureStateActive, "req-activate"))
	if err != nil {
		t.Fatalf("UpdateFeatureState: %v", err)
	}
	if reply.GetDefinition().GetState() != rpc.FeatureState_FEATURE_STATE_ACTIVE {
		t.Errorf("reply state=%v, want ACTIVE", reply.GetDefinition().GetState())
	}
	if got := f.defs.rows[model.DefinitionKey{FeatureKey: "user_play_finish_7d", Version: 1}].State; got != model.FeatureStateActive {
		t.Errorf("stored state=%d, want ACTIVE", got)
	}
	if ptr := f.pointers.rows["user_play_finish_7d"]; ptr.ActiveVersion != 1 {
		t.Errorf("pointer active=%d, want the newly activated 1", ptr.ActiveVersion)
	}
	if len(f.switches.rows) != 1 {
		t.Fatalf("audit rows=%d, want 1", len(f.switches.rows))
	}
	for _, r := range f.switches.rows {
		if r.SwitchType != model.SwitchTypeStateChange || r.FromValue != "1" || r.ToValue != "2" {
			t.Errorf("audit = %s %s->%s, want state_change DRAFT(1)->ACTIVE(2)", r.SwitchType, r.FromValue, r.ToValue)
		}
	}
}

func TestStateChangeIsIdempotentForTheSameTargetState(t *testing.T) {
	f := newFixture(t)
	f.putDef(newDef("user_play_finish_7d", 1, withState(model.FeatureStateActive)))
	f.putPointer("user_play_finish_7d", 1, 0)

	reply, err := callState(f, stateReq("user_play_finish_7d", 1, model.FeatureStateActive, "req-same-state"))
	if err != nil {
		t.Fatalf("UpdateFeatureState: %v", err)
	}
	if !reply.GetReused() {
		t.Error("reused=false for a repeat of the same state")
	}
	if n := countCalled(f.defs.calls, "definitions.UpdateState"); n != 0 {
		t.Errorf("UpdateState calls=%d, want 0: no noise write", n)
	}
	if len(f.switches.rows) != 0 {
		t.Errorf("audit rows=%d, want 0: from==to is not a change worth auditing", len(f.switches.rows))
	}
	if f.txBegins() != 0 {
		t.Errorf("transactions=%d, want 0", f.txBegins())
	}
}

func TestRetiringTheServingVersionClearsThePointerAndKeepsHistory(t *testing.T) {
	f := newFixture(t)
	f.registerActive(newDef("user_play_finish_7d", 1))

	if _, err := callState(f, stateReq("user_play_finish_7d", 1, model.FeatureStateRetired, "req-retire")); err != nil {
		t.Fatalf("UpdateFeatureState: %v", err)
	}
	ptr := f.pointers.rows["user_play_finish_7d"]
	if ptr.ActiveVersion != model.NoActiveVersion {
		t.Errorf("pointer active=%d, want 0: a retired feature must stop being served", ptr.ActiveVersion)
	}
	if ptr.PreviousVersion != 1 {
		t.Errorf("pointer previous=%d, want 1 so the read path still knows what was retired", ptr.PreviousVersion)
	}

	// 下线之后读它：明确 FEATURE_RETIRED，而不是默认值伪装成真实值。
	_, err := callGetFeature(f, "user_play_finish_7d", 1, false)
	if err != nil {
		t.Fatalf("GetFeature after retire: %v", err)
	}
}

func TestStateChangeCASConflictRollsBackEverything(t *testing.T) {
	f := newFixture(t)
	d := f.putDef(newDef("user_play_finish_7d", 1, withState(model.FeatureStateDraft)))
	f.putPointer("user_play_finish_7d", 0, 0)
	f.putInt64(d, 1, testMid, 5, testNow-60) // 有值才允许首次上线（README §5 的上线前置）
	// 另一事务在我们读完 DRAFT 之后把它改成了 RETIRED。
	f.defs.lostUpdate = func(key string, version int32, column string) {
		if column != "state" {
			return
		}
		k := model.DefinitionKey{FeatureKey: key, Version: version}
		row := f.defs.rows[k]
		row.State = model.FeatureStateRetired
		f.defs.rows[k] = row
	}

	_, err := callState(f, stateReq("user_play_finish_7d", 1, model.FeatureStateActive, "req-cas-state"))
	if !errors.Is(err, model.ErrFeatureStateTransition) {
		t.Fatalf("err=%v, want %v", err, model.ErrFeatureStateTransition)
	}
	got := f.defs.rows[model.DefinitionKey{FeatureKey: "user_play_finish_7d", Version: 1}]
	if got.State != model.FeatureStateDraft {
		t.Errorf("stored state=%d after rollback, want the pre-transaction DRAFT: a lost CAS must not half-apply", got.State)
	}
	if len(f.switches.rows) != 0 {
		t.Errorf("audit rows=%d, want 0", len(f.switches.rows))
	}
	if ptr := f.pointers.rows["user_play_finish_7d"]; ptr.ActiveVersion != 0 {
		t.Errorf("pointer active=%d, want 0", ptr.ActiveVersion)
	}
}

// --- 隐私调整 ---

func callPrivacy(f *fixture, in *rpc.UpdateFeaturePrivacyReq) (*rpc.UpdateFeaturePrivacyReply, error) {
	return NewUpdateFeaturePrivacyLogic(f.ctx, f.ServiceContext).UpdateFeaturePrivacy(in)
}

func privacyReq(key string, version, level int32, requestID string) *rpc.UpdateFeaturePrivacyReq {
	return &rpc.UpdateFeaturePrivacyReq{
		FeatureKey: key, Version: version, PrivacyLevel: rpc.PrivacyLevel(level),
		Operator: "admin:tester", Reason: "口径复核", RequestId: requestID,
	}
}

func TestPrivacyChangeUpdatesAndAuditsTogether(t *testing.T) {
	f := newFixture(t)
	f.registerActive(newDef("user_play_finish_7d", 1, withPrivacy(model.PrivacyPseudonymous)))

	reply, err := callPrivacy(f, privacyReq("user_play_finish_7d", 1, model.PrivacyUserProfile, "req-priv"))
	if err != nil {
		t.Fatalf("UpdateFeaturePrivacy: %v", err)
	}
	if got := reply.GetDefinition().GetPrivacyLevel(); got != rpc.PrivacyLevel_PRIVACY_LEVEL_USER_PROFILE {
		t.Errorf("reply privacy=%v, want USER_PROFILE", got)
	}
	stored := f.defs.rows[model.DefinitionKey{FeatureKey: "user_play_finish_7d", Version: 1}]
	if stored.PrivacyLevel != model.PrivacyUserProfile {
		t.Fatalf("stored privacy=%d, want 4", stored.PrivacyLevel)
	}
	if len(f.switches.rows) != 1 {
		t.Fatalf("audit rows=%d, want 1", len(f.switches.rows))
	}
	for _, r := range f.switches.rows {
		if r.SwitchType != model.SwitchTypePrivacyChange || r.FromValue != "3" || r.ToValue != "4" {
			t.Errorf("audit = %s %s->%s, want privacy_change 3->4", r.SwitchType, r.FromValue, r.ToValue)
		}
	}
}

func TestPrivacySameLevelRepeatIsIdempotentAndUnaudited(t *testing.T) {
	f := newFixture(t)
	f.registerActive(newDef("user_play_finish_7d", 1, withPrivacy(model.PrivacyUserProfile)))

	reply, err := callPrivacy(f, privacyReq("user_play_finish_7d", 1, model.PrivacyUserProfile, "req-priv-same"))
	if err != nil {
		t.Fatalf("UpdateFeaturePrivacy: %v", err)
	}
	if !reply.GetReused() {
		t.Error("reused=false for a repeat of the same level")
	}
	if len(f.switches.rows) != 0 {
		t.Errorf("audit rows=%d, want 0", len(f.switches.rows))
	}
}

func TestPrivacyUnsetAndScopeInconsistentLevelsRejected(t *testing.T) {
	f := newFixture(t)
	f.registerActive(newDef("user_play_finish_7d", 1, withPrivacy(model.PrivacyPseudonymous)))

	if _, err := callPrivacy(f, privacyReq("user_play_finish_7d", 1, model.PrivacyUnspecified, "req-priv-0")); !errors.Is(err, model.ErrPrivacyUnsetNotAllowed) {
		t.Errorf("err=%v, want %v: 「不知道多敏感」的特征不允许存在", err, model.ErrPrivacyUnsetNotAllowed)
	}
	// MID 维度降到 PUBLIC_AGGREGATE：个体维度被标成聚合级就能绕过授权读画像。
	_, err := callPrivacy(f, privacyReq("user_play_finish_7d", 1, model.PrivacyPublicAggregate, "req-priv-scope"))
	if !errors.Is(err, model.ErrPrivacyScopeMismatch) {
		t.Fatalf("err=%v, want %v", err, model.ErrPrivacyScopeMismatch)
	}
	if n := countCalled(f.receipts.calls, "receipts.MarkFailed"); n == 0 {
		t.Error("the rejected privacy changes released no execution right: a failed change must be retryable")
	}
}

func TestPrivacyCASConflictRollsBackTheLevelAndTheAudit(t *testing.T) {
	f := newFixture(t)
	f.registerActive(newDef("user_play_finish_7d", 1, withPrivacy(model.PrivacyPseudonymous)))
	// 另一事务先把 3 改成了别的值，我们手上的 3 已经过期。
	f.defs.lostUpdate = func(key string, version int32, column string) {
		if column != "privacy_level" {
			return
		}
		k := model.DefinitionKey{FeatureKey: key, Version: version}
		row := f.defs.rows[k]
		row.PrivacyLevel = model.PrivacyUserProfile
		f.defs.rows[k] = row
	}

	_, err := callPrivacy(f, privacyReq("user_play_finish_7d", 1, model.PrivacyPseudonymous+1, "req-priv-race"))
	if !errors.Is(err, model.ErrVersionConflict) {
		t.Fatalf("err=%v, want %v", err, model.ErrVersionConflict)
	}
	stored := f.defs.rows[model.DefinitionKey{FeatureKey: "user_play_finish_7d", Version: 1}]
	if stored.PrivacyLevel != model.PrivacyPseudonymous {
		t.Errorf("stored privacy=%d after rollback, want the pre-transaction 3", stored.PrivacyLevel)
	}
	if len(f.switches.rows) != 0 {
		t.Errorf("audit rows=%d, want 0", len(f.switches.rows))
	}
}

func TestPrivacyAuditFailureRollsBackTheLevelChange(t *testing.T) {
	f := newFixture(t)
	f.registerActive(newDef("user_play_finish_7d", 1, withPrivacy(model.PrivacyPseudonymous)))
	f.switches.errAppend = errors.New("audit unavailable")

	_, err := callPrivacy(f, privacyReq("user_play_finish_7d", 1, model.PrivacyUserProfile, "req-priv-audit"))
	if err == nil {
		t.Fatal("an unaudited privacy change must not be reported as success")
	}
	stored := f.defs.rows[model.DefinitionKey{FeatureKey: "user_play_finish_7d", Version: 1}]
	if stored.PrivacyLevel != model.PrivacyPseudonymous {
		t.Errorf("stored privacy=%d: the level moved without its audit row", stored.PrivacyLevel)
	}
}
