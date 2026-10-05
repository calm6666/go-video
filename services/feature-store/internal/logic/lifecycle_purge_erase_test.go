// 测试域 5：生命周期与隐私擦除（PurgeExpired / EraseEntityFeatures）。
//
// 这两个方法是本服务仅有的两条「删数据」路径，因此钉的都是删除的边界与计数的诚实性：
//  1. 删除范围由入参约束：截止时间/主体维度/主体标识/隐私下限，四条都不许多删一寸；
//  2. 删除计数来自 SQL 的影响行数，重放绝不重复删、也绝不把首次计数当现值；
//  3. 失败后的残留形态可见：哪些轮次已经删掉、回执是否让同一 request_id 还能接管；
//  4. 先选主键再按主键批删（不做范围 DELETE），因此「一次语句」是本包能断言的最小副作用单位。
//
// 替身纪律与生产一致：Cache 恒为 nil（README §9），所以「删完 DEL 缓存」这一段
// 只能钉到「DEL 的键集合与删除集合同构」的下界，即 DeleteByIDs 收到的 ID 集合；
// 真正的 Redis 命令由 helpers.go 的 nil 检查短路，不在此伪造。
package logic

import (
	"errors"
	"fmt"
	"testing"

	"go-video/services/feature-store/model"
	"go-video/services/feature-store/rpc"
)

// --- 造入参 ---

func purgeReq(requestID, operator string, limit int32, before int64) *rpc.PurgeExpiredReq {
	return &rpc.PurgeExpiredReq{RequestId: requestID, Operator: operator, Limit: limit, Before: before}
}

func callPurge(f *fixture, in *rpc.PurgeExpiredReq) (*rpc.PurgeExpiredReply, error) {
	return NewPurgeExpiredLogic(f.ctx, f.ServiceContext).PurgeExpired(in)
}

func eraseReq(entity *rpc.EntityRef, minPrivacy int32, requestID, operator, reason string) *rpc.EraseEntityFeaturesReq {
	return &rpc.EraseEntityFeaturesReq{
		Entity: entity, MinPrivacyLevel: rpc.PrivacyLevel(minPrivacy),
		RequestId: requestID, Operator: operator, Reason: reason,
	}
}

func callErase(f *fixture, in *rpc.EraseEntityFeaturesReq) (*rpc.EraseEntityFeaturesReply, error) {
	return NewEraseEntityFeaturesLogic(f.ctx, f.ServiceContext).EraseEntityFeatures(in)
}

func midRef(id string) *rpc.EntityRef {
	return &rpc.EntityRef{EntityScope: rpc.EntityScope_ENTITY_SCOPE_MID, EntityId: id}
}

// putRowAt 造一行「到期时刻 = 服务端当前时刻 + expireIn」的值（负数 = 已过期）。
// event_time 与 TTL 由生产构造函数配平（expire_at 恒等于 event_time + ttl_seconds），
// 所以这里得到的行是真 SQL 写得出的形态，而不是手填 expire_at 造出的脏数据。
func putRowAt(f *fixture, def *model.FeatureDefinition, version int32, entityID string,
	val, expireIn int64) *model.FeatureValue {
	f.t.Helper()
	ttl := int64(3600)
	if expireIn > 0 {
		ttl = expireIn + 3600
	}
	return f.putInt64(newDef(def.FeatureKey, version, withTTL(ttl),
		withScope(def.EntityScope), withPrivacy(def.PrivacyLevel)), version, entityID,
		val, f.nowUnix()+expireIn-ttl) // event_time 恒在过去
}

// seedEntityRows 给同一主体铺 n 行（每行一个版本，定义同时存在），
// 用于把「分批轮数保险」这条只有在行数超过 rounds×limit 时才可达的分支变成可达。
func seedEntityRows(f *fixture, key string, scope int32, entityID string, n int) {
	f.t.Helper()
	for i := 1; i <= n; i++ {
		d := f.putDef(newDef(key, int32(i), withScope(scope), withTTL(3600)))
		f.putInt64(d, int32(i), entityID, int64(i), f.nowUnix()-7200) // 全部已过期
	}
}

// --- PurgeExpired ---

func TestPurgeExpiredDeletesOnlyRowsPastTheCutoffAndCountsRemainingHonestly(t *testing.T) {
	f := newFixture(t)
	d := f.registerActive(newDef("u_play_finish_7d", 1, withTTL(3600)))
	expired := putRowAt(f, d, 1, testMid, 11, -1) // 一秒前到期
	fresh := putRowAt(f, d, 1, "10087", 12, 7200)
	// expire_at=0 的脏行：真 SQL 的谓词是 expire_at > 0 AND expire_at < ?，
	// 没有到期时间的行不属于「过期」，一次漏传参数的清理不能把它当过期删掉。
	dirty := f.putRawValue(&model.FeatureValue{
		FeatureKey: d.FeatureKey, Version: 1, EntityScope: model.EntityScopeMid,
		EntityID: "10088", ValueType: model.ValueTypeInt64, Int64Value: 13,
		EventTime: f.nowUnix(), ExpireAt: 0,
	})

	reply, err := callPurge(f, purgeReq("req-purge-1", "system:cron", 100, f.nowUnix()))
	if err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	if reply.GetPurged() != 1 || reply.GetRemaining() != 0 || reply.GetReused() {
		t.Fatalf("reply purged=%d remaining=%d reused=%v, want 1/0/false",
			reply.GetPurged(), reply.GetRemaining(), reply.GetReused())
	}
	if got := f.values.deletes; len(got) != 1 || got[0] != expired.ValueID {
		t.Errorf("deleted ids=%v, want exactly the one expired row %d", got, expired.ValueID)
	}
	for _, tc := range []struct {
		name string
		key  model.ValueKey
		want bool
	}{{"过期行已删", expired.Key(), false}, {"未过期行仍在", fresh.Key(), true},
		{"无到期时间的脏行仍在", dirty.Key(), true}} {
		if _, ok := f.valueRow(tc.key); ok != tc.want {
			t.Errorf("%s: present=%v want=%v", tc.name, ok, tc.want)
		}
	}
	// 一次选择 + 一次按主键批删：不出现 SelectExpiredIDs 之外的第二条删除路径，
	// 也不出现范围删除需要的事务（本方法全程不 transact）。
	if n := countCalled(f.values.calls, "values.ListExpiredForPurge"); n != 1 {
		t.Errorf("ListExpiredForPurge calls=%d, want 1", n)
	}
	if called(f.values.calls, "values.SelectExpiredIDs") {
		t.Error("purge took the ids-only path: the DEL set would not be derivable from the deleted rows")
	}
	if n := countCalled(f.values.calls, "values.DeleteByIDs"); n != 1 {
		t.Errorf("DeleteByIDs calls=%d, want 1 batch", n)
	}
	if n := countCalled(f.values.calls, "values.CountExpired"); n != 1 {
		t.Errorf("CountExpired calls=%d, want 1 (remaining must be re-measured, not derived)", n)
	}
	if f.txBegins() != 0 {
		t.Errorf("transactions=%d, want 0: a bounded primary-key DELETE needs no surrounding tx",
			f.txBegins())
	}
	row, ok := f.receiptRow("req-purge-1", model.ReceiptOpPurge)
	if !ok || row.State != model.ReceiptStateDone || row.AffectedRows != 1 {
		t.Errorf("purge receipt = %+v ok=%v, want done with affected_rows=1", row, ok)
	}
}

func TestPurgeExpiredClampsTheRequestedLimitInsteadOfRefusing(t *testing.T) {
	f := newFixture(t)
	f.withPurgeLimit(2)
	d := f.registerActive(newDef("u_play_finish_7d", 1))
	for _, id := range []string{"10086", "10087", "10088"} {
		putRowAt(f, d, 1, id, 5, -1)
	}

	// 上游传一个远超服务端硬上限的批大小：本方法是「夹取并如实报告剩余量」，
	// 不是拒绝整轮，也不是把 99999 原样带进 DELETE ... IN。
	reply, err := callPurge(f, purgeReq("req-purge-cap", "system:cron", 99999, f.nowUnix()))
	if err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	if reply.GetPurged() != 2 || reply.GetRemaining() != 1 {
		t.Errorf("purged/remaining = %d/%d, want 2/1", reply.GetPurged(), reply.GetRemaining())
	}
	if len(f.values.expireSee) != 1 || f.values.expireSee[0] != 2 {
		t.Errorf("limits seen by the model=%v, want exactly [2] (the server cap, not the request)",
			f.values.expireSee)
	}
	// limit<=0 走服务端上限而不是「不限量」。
	if _, err := callPurge(f, purgeReq("req-purge-zero", "system:cron", 0, f.nowUnix())); err != nil {
		t.Fatalf("PurgeExpired with limit=0: %v", err)
	}
	if f.values.expireSee[1] != 2 {
		t.Errorf("limit for the zero request=%d, want the server cap 2", f.values.expireSee[1])
	}
	if reply2, err := callPurge(f, purgeReq("req-purge-zero", "system:cron", 0, f.nowUnix())); err != nil ||
		!reply2.GetReused() {
		t.Fatalf("replay of the zero-limit call: reused=%v err=%v", reply2.GetReused(), err)
	}
	if n := countCalled(f.values.calls, "values.ListExpiredForPurge"); n != 2 {
		t.Errorf("ListExpiredForPurge calls=%d, want 2: the replay must not select a third batch", n)
	}
}

func TestPurgeExpiredWithoutACutoffUsesTheServerClock(t *testing.T) {
	f := newFixture(t)
	d := f.registerActive(newDef("u_play_finish_7d", 1))
	willExpire := putRowAt(f, d, 1, testMid, 7, 1799) // now+1799 到期

	// before=0 = 服务端当前时刻，此刻这一行还没过期。
	first, err := callPurge(f, purgeReq("req-purge-clock", "system:cron", 10, 0))
	if err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	if first.GetPurged() != 0 {
		t.Fatalf("purged=%d before the row expired, want 0", first.GetPurged())
	}
	if got := f.values.expireBeforeSeen[0]; got != f.nowUnix() {
		t.Errorf("cutoff=%d, want the server clock %d", got, f.nowUnix())
	}

	// 时钟前进后同一份「before=0」入参必须落到新的时刻：补位取的是当前时间而不是常量。
	f.advance(3600)
	second, err := callPurge(f, purgeReq("req-purge-clock-2", "system:cron", 10, 0))
	if err != nil {
		t.Fatalf("PurgeExpired after the clock advanced: %v", err)
	}
	if second.GetPurged() != 1 {
		t.Errorf("purged=%d after TTL elapsed, want 1", second.GetPurged())
	}
	if got := f.values.expireBeforeSeen[1]; got != f.nowUnix() {
		t.Errorf("cutoff=%d, want the advanced server clock %d", got, f.nowUnix())
	}
	if _, ok := f.valueRow(willExpire.Key()); ok {
		t.Error("the row that aged out is still in feature_value")
	}

	// 客户端给的历史截止时间按原值下传，不被悄悄抬到 now：
	// 否则「只清 N 天前的」会变成「清到此刻为止」。这一行的到期时刻落在
	// testNow 之后、当前时刻之前，正是「已被 now 判定过期、但不属于这次历史窗口」的形态。
	historical := putRowAt(f, d, 1, "10087", 8, -1800)
	_, err = callPurge(f, purgeReq("req-purge-past", "system:cron", 10, testNow))
	if err != nil {
		t.Fatalf("PurgeExpired with a historical cutoff: %v", err)
	}
	if got := f.values.expireBeforeSeen[2]; got != testNow {
		t.Errorf("cutoff=%d, want the caller's %d untouched", got, testNow)
	}
	if _, ok := f.valueRow(historical.Key()); !ok {
		t.Error("a row that expired after the requested cutoff was swept by an older cutoff")
	}
}

func TestPurgeExpiredRejectsAFutureCutoffBeforeTakingTheExecutionRight(t *testing.T) {
	f := newFixture(t)
	d := f.registerActive(newDef("u_play_finish_7d", 1))
	kept := putRowAt(f, d, 1, testMid, 9, -1)

	_, err := callPurge(f, purgeReq("req-purge-future", "system:cron", 10, f.nowUnix()+1))
	if !errors.Is(err, model.ErrCutoffRequired) {
		t.Fatalf("err=%v, want %v", err, model.ErrCutoffRequired)
	}
	// 未来截止时间等于把还没过期的值当过期删：这条闸门必须在取得执行权之前，
	// 否则一次被拒的请求也会在回执表里留下一个 failed 轮次。
	assertTouchedNothing(t, f)
	if _, ok := f.valueRow(kept.Key()); !ok {
		t.Error("a rejected cutoff still deleted rows")
	}
}

func TestPurgeExpiredReplayReportsTheFirstRunAndDeletesNothingAgain(t *testing.T) {
	f := newFixture(t)
	d := f.registerActive(newDef("u_play_finish_7d", 1))
	for _, id := range []string{"10086", "10087"} {
		putRowAt(f, d, 1, id, 3, -1)
	}
	req := purgeReq("req-purge-replay", "system:cron", 50, f.nowUnix())

	first, err := callPurge(f, req)
	if err != nil {
		t.Fatalf("first purge: %v", err)
	}
	if first.GetPurged() != 2 || first.GetRemaining() != 0 {
		t.Fatalf("first reply = %d/%d, want 2/0", first.GetPurged(), first.GetRemaining())
	}
	// 首次之后库里又攒出一条过期行（上游写入了更短 TTL 的值）。
	later := putRowAt(f, d, 1, "10088", 4, -1)

	replay, err := callPurge(f, req)
	if err != nil {
		t.Fatalf("replayed purge: %v", err)
	}
	if !replay.GetReused() {
		t.Error("reused=false for a replayed request_id")
	}
	if replay.GetPurged() != 2 {
		t.Errorf("replayed purged=%d, want the first run's 2", replay.GetPurged())
	}
	if n := countCalled(f.values.calls, "values.DeleteByIDs"); n != 1 {
		t.Errorf("DeleteByIDs calls=%d, want 1: a replay must not delete a second batch", n)
	}
	if _, ok := f.valueRow(later.Key()); !ok {
		t.Error("the replay deleted a row that the first run never saw")
	}
	// 当前真实剩余量是 1，回放给出的却是首次的 0：cron 按 remaining 判断是否再跑一轮时，
	// 一次重放就会让它提前收工（README §10 已登记）。
	if replay.GetRemaining() != 0 {
		t.Fatalf("replayed remaining=%d, want the pinned first-run value 0", replay.GetRemaining())
	}
	if n := countExpiredRows(t, f); n != 1 {
		t.Errorf("expired rows still in the table=%d, want 1: the reply's remaining is stale", n)
	}
}

func TestPurgeSameRequestIDDifferentCutoffIsReplayedAsTheSameRequest(t *testing.T) {
	f := newFixture(t)
	d := f.registerActive(newDef("u_play_finish_7d", 1))
	old := putRowAt(f, d, 1, "10086", 5, -3601)    // 落在第一次的窗口内
	recent := putRowAt(f, d, 1, "10087", 6, -3599) // 落在窗口外、但按 now 已经过期
	cutoff1 := f.nowUnix() - 3600

	if _, err := callPurge(f, purgeReq("req-purge-range", "system:cron", 50, cutoff1)); err != nil {
		t.Fatalf("first purge: %v", err)
	}
	if _, ok := f.valueRow(old.Key()); ok {
		t.Fatal("the row inside the first cutoff survived")
	}
	if _, ok := f.valueRow(recent.Key()); !ok {
		t.Fatal("the row outside the first cutoff was deleted")
	}

	// 同一 request_id、更宽的截止时间：回执摘要不含 before，第二次被判定为「同一件事」并回放。
	second, err := callPurge(f, purgeReq("req-purge-range", "system:cron", 50, f.nowUnix()))
	if err != nil {
		t.Fatalf("second purge with a wider cutoff: %v", err)
	}
	if !second.GetReused() || second.GetPurged() != 1 {
		t.Errorf("second reply reused=%v purged=%d, want a replay of the first run's 1",
			second.GetReused(), second.GetPurged())
	}
	if _, ok := f.valueRow(recent.Key()); !ok {
		t.Error("the wider cutoff deleted rows, so the pinning above describes the wrong behaviour")
	}
	if n := countCalled(f.values.calls, "values.ListExpiredForPurge"); n != 1 {
		t.Errorf("ListExpiredForPurge calls=%d, want 1: the wider cutoff was never evaluated", n)
	}
	// 对照：换 limit（参与摘要的 rowCount）就是另一件事，会被幂等冲突挡下而不是静默回放。
	if _, err := callPurge(f, purgeReq("req-purge-range", "system:cron", 49, f.nowUnix())); !errors.Is(err,
		model.ErrRequestIdReused) {
		t.Errorf("err=%v, want %v: row_count is covered by the digest", err, model.ErrRequestIdReused)
	}
}

func TestPurgeExpiredDeleteFailureKeepsTheRowsAndReleasesTheExecutionRight(t *testing.T) {
	f := newFixture(t)
	d := f.registerActive(newDef("u_play_finish_7d", 1))
	target := putRowAt(f, d, 1, testMid, 5, -1)
	f.values.errDeleteByIDs = errors.New("lock wait timeout")

	_, err := callPurge(f, purgeReq("req-purge-fail", "system:cron", 10, f.nowUnix()))
	if err == nil {
		t.Fatal("a failed DELETE must not be reported as success")
	}
	if _, ok := f.valueRow(target.Key()); !ok {
		t.Error("the failed purge lost the row it never deleted")
	}
	if len(f.values.deletes) != 1 {
		t.Errorf("deleted ids=%v, want the attempted batch recorded once", f.values.deletes)
	}
	// 失败必须收尾成 failed 并给出可枚举短码，否则同键重试会被挡成「正在进行」。
	if got := f.receipts.failed; len(got) != 1 || got[0] != "req-purge-fail/purge:INTERNAL" {
		t.Errorf("failed receipts=%v, want [req-purge-fail/purge:INTERNAL]", got)
	}
	row, ok := f.receiptRow("req-purge-fail", model.ReceiptOpPurge)
	if !ok || row.State != model.ReceiptStateFailed {
		t.Fatalf("receipt = %+v ok=%v, want state failed", row, ok)
	}

	f.values.errDeleteByIDs = nil
	retry, err := callPurge(f, purgeReq("req-purge-fail", "system:cron", 10, f.nowUnix()))
	if err != nil {
		t.Fatalf("retry after the failure: %v", err)
	}
	if retry.GetPurged() != 1 || retry.GetReused() {
		t.Errorf("retry purged=%d reused=%v, want 1/false (a failed receipt must allow re-execution)",
			retry.GetPurged(), retry.GetReused())
	}
}

// --- EraseEntityFeatures ---

func TestEraseGuardsRunBeforeTheExecutionRightAndTheAllowlistFailsClosed(t *testing.T) {
	cases := []struct {
		name      string
		entity    *rpc.EntityRef
		minPriv   int32
		requestID string
		operator  string
		reason    string
		want      error
	}{
		{"缺主体", nil, 0, "req-erase-guard", "privacy-job:x", "ticket-1",
			model.ErrEntityScopeRequired},
		{"主体维度未声明", &rpc.EntityRef{EntityScope: rpc.EntityScope_ENTITY_SCOPE_UNSPECIFIED,
			EntityId: testMid}, 0, "req-erase-guard", "privacy-job:x", "ticket-1",
			model.ErrEntityScopeRequired},
		{"明文设备号", &rpc.EntityRef{EntityScope: rpc.EntityScope_ENTITY_SCOPE_DEVICE,
			EntityId: plaintextDeviceSerial}, 0, "req-erase-guard", "privacy-job:x", "ticket-1",
			model.ErrEntityIDInvalid},
		{"原始 IP", &rpc.EntityRef{EntityScope: rpc.EntityScope_ENTITY_SCOPE_IP_HASH,
			EntityId: rawIPv4}, 0, "req-erase-guard", "privacy-job:x", "ticket-1",
			model.ErrEntityIDInvalid},
		// 顺序判别：主体形态先于幂等键 —— 两个都坏时报的是主体错，
		// 不合规的标识不会先被拿去算回执摘要（摘要输入含 entity_id）。
		{"主体先于幂等键", &rpc.EntityRef{EntityScope: rpc.EntityScope_ENTITY_SCOPE_MID,
			EntityId: "mid-10086"}, 0, "  ", "privacy-job:x", "ticket-1", model.ErrEntityIDInvalid},
		{"缺幂等键", midRef(testMid), 0, "  ", "privacy-job:x", "ticket-1",
			model.ErrRequestIdRequired},
		{"缺操作人", midRef(testMid), 0, "req-erase-guard", "  ", "ticket-1",
			model.ErrOperatorRequired},
		{"缺工单号", midRef(testMid), 0, "req-erase-guard", "privacy-job:x", "  ",
			model.ErrReasonRequired},
		{"操作人不在白名单", midRef(testMid), 0, "req-erase-guard", "admin:evil", "ticket-1",
			model.ErrPrivacyOperatorForbidden},
		{"隐私级别越界", midRef(testMid), 9, "req-erase-guard", "privacy-job:x", "ticket-1",
			model.ErrPrivacyUnsetNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			d := f.registerActive(newDef("u_play_finish_7d", 1))
			kept := putRowAt(f, d, 1, testMid, 5, 3600)
			_, err := callErase(f, eraseReq(tc.entity, tc.minPriv, tc.requestID,
				tc.operator, tc.reason))
			if !errors.Is(err, tc.want) {
				t.Fatalf("err=%v, want %v", err, tc.want)
			}
			assertTouchedNothing(t, f)
			if _, ok := f.valueRow(kept.Key()); !ok {
				t.Fatal("a rejected erase request deleted the subject's row")
			}
		})
	}

	// 空白名单 = 谁都拒绝（fail closed）：漏配不能让「抹数据」变成人人可调的接口。
	f := newFixture(t)
	f.withPrivacyPrefixes()
	d := f.registerActive(newDef("u_play_finish_7d", 1))
	putRowAt(f, d, 1, testMid, 5, 3600)
	if _, err := callErase(f, eraseReq(midRef(testMid), 0, "req-empty-list",
		"privacy-job:x", "ticket-1")); !errors.Is(err, model.ErrPrivacyOperatorForbidden) {
		t.Fatalf("err=%v, want %v with an empty allowlist", err, model.ErrPrivacyOperatorForbidden)
	}
	assertTouchedNothing(t, f)
	if n := len(f.values.rows); n != 1 {
		t.Errorf("feature_value rows=%d, want the row to survive a fail-closed rejection", n)
	}
}

func TestEraseCoversEveryVersionOfTheEntityAndStopsAtTheEntityBoundary(t *testing.T) {
	f := newFixture(t)
	// 同一 key 的三个版本：ACTIVE / DRAFT / RETIRED 各一行残值，都要擦掉。
	dActive := f.registerActive(newDef("u_play_finish_7d", 1))
	dDraft := f.putDef(newDef("u_play_finish_7d", 2, withState(model.FeatureStateDraft)))
	dRetired := f.putDef(newDef("u_play_finish_7d", 3, withState(model.FeatureStateRetired)))
	dOtherKey := f.registerActive(newDef("u_other_7d", 1))
	mine := []*model.FeatureValue{
		putRowAt(f, dActive, 1, testMid, 1, 3600),
		f.putInt64(dDraft, 2, testMid, 2, f.nowUnix()-60),
		// RETIRED 版本上的残值是真实现象（先写过值后下线），构造函数拒绝造它，直接入库。
		f.putRawValue(&model.FeatureValue{FeatureKey: dRetired.FeatureKey,
			Version: dRetired.Version, EntityScope: model.EntityScopeMid, EntityID: testMid,
			ValueType: model.ValueTypeInt64, Int64Value: 3,
			EventTime: f.nowUnix() - 60, ExpireAt: f.nowUnix() + 3600}),
		// 另一个特征 key 上同一主体的值：擦除的谓词里没有 feature_key，
		// 「擦这个主体」= 该主体在目录里所有口径下的残值一次擦干净。
		putRowAt(f, dOtherKey, 1, testMid, 6, 3600),
	}
	// 三道边界：别的主体、别的维度（entity_id 字符串相同）、无定义的脏行。
	otherMid := putRowAt(f, dActive, 1, "10087", 4, 3600)
	sameIDDifferentScope := f.putInt64(f.putDef(newDef("a_play_finish_7d", 1,
		withScope(model.EntityScopeAid))), 1, testMid, 5, f.nowUnix()-60)
	orphan := f.putRawValue(&model.FeatureValue{FeatureKey: "u_deleted_definition", Version: 1,
		EntityScope: model.EntityScopeMid, EntityID: testMid, ValueType: model.ValueTypeInt64,
		Int64Value: 7, EventTime: f.nowUnix() - 60, ExpireAt: f.nowUnix() + 3600})

	reply, err := callErase(f, eraseReq(midRef(testMid), 0, "req-erase-full",
		"privacy-job:gdpr-1", "ticket-2026-09"))
	if err != nil {
		t.Fatalf("EraseEntityFeatures: %v", err)
	}
	if reply.GetErasedRows() != 4 || reply.GetFeaturesTouched() != 2 || reply.GetReused() {
		t.Fatalf("reply erased=%d touched=%d reused=%v, want 4/2/false",
			reply.GetErasedRows(), reply.GetFeaturesTouched(), reply.GetReused())
	}
	for i, v := range mine {
		if _, ok := f.valueRow(v.Key()); ok {
			t.Errorf("row %d (%s@v%d) survived the erasure", i, v.FeatureKey, v.Version)
		}
	}
	for name, key := range map[string]model.ValueKey{"别的主体": otherMid.Key(),
		"同 ID 不同维度": sameIDDifferentScope.Key(), "无定义行": orphan.Key()} {
		if _, ok := f.valueRow(key); !ok {
			t.Errorf("%s 的行被越界删掉了", name)
		}
	}
	// 删除集合必须正好是那四行（缓存失效集合与它同构，见 README §10.9）。
	wantIDs := map[int64]bool{}
	for _, v := range mine {
		wantIDs[v.ValueID] = true
	}
	if len(f.values.deletes) != 4 {
		t.Fatalf("deleted ids=%v, want 4", f.values.deletes)
	}
	for _, id := range f.values.deletes {
		if !wantIDs[id] {
			t.Errorf("deleted id %d does not belong to the requested entity", id)
		}
	}
	// 定义、指针与审计一寸不动：擦的是个体值，不是特征口径（README §1 职责 5）。
	if len(f.defs.rows) != 5 || len(f.pointers.rows) != 2 {
		t.Errorf("definitions=%d pointers=%d, want 5/2: erasure must not rewrite the catalog",
			len(f.defs.rows), len(f.pointers.rows))
	}
	if len(f.switches.rows) != 0 || called(f.switches.calls, "switches.Append") {
		t.Errorf("audit rows=%d calls=%v, want none", len(f.switches.rows), f.switches.calls)
	}
	// 谓词按「本主体 + 本维度 + 请求下限 + 服务端上限」下传，收尾再复查一次。
	seen := f.values.listForEraseSeen
	if len(seen) < 2 {
		t.Fatalf("ListForErase calls=%d, want the rounds plus the closing probe", len(seen))
	}
	for i, c := range seen[:len(seen)-1] {
		if c.entityScope != model.EntityScopeMid || c.entityID != testMid || c.minPrivacy != 0 {
			t.Errorf("round %d predicate = %+v, want MID/%s/0", i, c, testMid)
		}
	}
	if last := seen[len(seen)-1]; last.limit != 1 {
		t.Errorf("closing probe limit=%d, want 1 (存在性复查，不搬整批)", last.limit)
	}
	if n := countCalled(f.values.calls, "values.DeleteByEntity"); n != 0 {
		t.Errorf("DeleteByEntity calls=%d, want 0: 范围删除会绕过缓存键集合", n)
	}
	row, ok := f.receiptRow("req-erase-full", model.ReceiptOpErase)
	if !ok || row.State != model.ReceiptStateDone || row.AffectedRows != 4 {
		t.Errorf("erase receipt = %+v ok=%v, want done with affected_rows=4", row, ok)
	}
}

func TestEraseWithNoPrivacyFloorSweepsNonIndividualRowsToo(t *testing.T) {
	f := newFixture(t)
	// 内容维度上的两个级别：2 = 内容属性（PrivacyIsIndividual=false），3 = 可关联到个体。
	aggregate := f.registerActive(newDef("a_item_heat_7d", 1, withScope(model.EntityScopeAid),
		withPrivacy(model.PrivacyContentAttribute)))
	individual := f.registerActive(newDef("a_item_owner_7d", 1, withScope(model.EntityScopeAid),
		withPrivacy(model.PrivacyPseudonymous)))
	aggRow := f.putInt64(aggregate, 1, "555", 1, f.nowUnix()-60)
	indRow := f.putInt64(individual, 1, "555", 2, f.nowUnix()-60)

	// 契约注释写的是「未填 = 全部个体特征」，实现的下限是 0 = 不加隐私条件：
	// 非个体级别的行也会被删掉（README §10 已登记）。
	reply, err := callErase(f, eraseReq(&rpc.EntityRef{EntityScope: rpc.EntityScope_ENTITY_SCOPE_AID,
		EntityId: "555"}, 0, "req-erase-all", "privacy-job:x", "ticket-1"))
	if err != nil {
		t.Fatalf("erase with min_privacy_level=0: %v", err)
	}
	if reply.GetErasedRows() != 2 || reply.GetFeaturesTouched() != 2 {
		t.Fatalf("reply erased=%d touched=%d, want 2/2",
			reply.GetErasedRows(), reply.GetFeaturesTouched())
	}
	if _, ok := f.valueRow(aggRow.Key()); ok {
		t.Error("the pinned behaviour changed: a content-attribute row now survives a min=0 erase")
	}
	if _, ok := f.valueRow(indRow.Key()); ok {
		t.Error("the individual row survived")
	}
	if c := f.values.listForEraseSeen[0]; c.minPrivacy != 0 {
		t.Errorf("min privacy passed down=%d, want 0 (无隐私条件)", c.minPrivacy)
	}
}

func TestErasePrivacyFloorIsPassedDownAsItsOwnPredicate(t *testing.T) {
	f := newFixture(t)
	low := f.registerActive(newDef("u_low_7d", 1, withPrivacy(model.PrivacyPseudonymous)))
	high := f.registerActive(newDef("u_high_7d", 1, withPrivacy(model.PrivacyUserProfile)))
	lowRow := f.putInt64(low, 1, testMid, 1, f.nowUnix()-60)
	highRow := f.putInt64(high, 1, testMid, 2, f.nowUnix()-60)

	reply, err := callErase(f, eraseReq(midRef(testMid), model.PrivacyUserProfile,
		"req-erase-high", "privacy-job:x", "ticket-1"))
	if err != nil {
		t.Fatalf("erase with a privacy floor: %v", err)
	}
	if reply.GetErasedRows() != 1 || reply.GetFeaturesTouched() != 1 {
		t.Fatalf("reply = %d/%d, want 1/1", reply.GetErasedRows(), reply.GetFeaturesTouched())
	}
	if _, ok := f.valueRow(lowRow.Key()); !ok {
		t.Error("the row below the requested privacy floor was erased")
	}
	if _, ok := f.valueRow(highRow.Key()); ok {
		t.Error("the row at the requested privacy floor survived")
	}
	if c := f.values.listForEraseSeen[0]; c.minPrivacy != model.PrivacyUserProfile {
		t.Errorf("min privacy passed down=%d, want %d", c.minPrivacy, model.PrivacyUserProfile)
	}
}

func TestEraseBatchesAcrossRoundsAndRefusesToClaimSuccessWhenTheRoundCapTrips(t *testing.T) {
	// 分批：每轮只允许 1 行，三行必须走满三轮，收尾复查为 0。
	f := newFixture(t)
	f.withPurgeLimit(1)
	d := f.registerActive(newDef("u_play_finish_7d", 1))
	for i := 1; i <= 3; i++ {
		f.putInt64(d, 1, fmt.Sprintf("1008%d", i), int64(i), f.nowUnix()-60)
	}
	// 上面三行共用一个 (key, version, scope)，只有 entity_id 不同：擦一个主体只该删一行。
	one, err := callErase(f, eraseReq(midRef("10081"), 0, "req-erase-one",
		"privacy-job:x", "ticket-1"))
	if err != nil {
		t.Fatalf("erase of a single row: %v", err)
	}
	if one.GetErasedRows() != 1 {
		t.Fatalf("erased=%d, want 1", one.GetErasedRows())
	}
	// 一批取满了 limit，所以还要再走一轮确认「下一页是空」，最后收尾复查一次：3 次 SELECT。
	// 轮数保险不能被写成「一次 SELECT 全删」。
	if n := countCalled(f.values.calls, "values.ListForErase"); n != 3 {
		t.Errorf("ListForErase calls=%d, want 3 (一轮删除 + 空页确认 + 收尾复查)", n)
	}
	if n := countCalled(f.values.calls, "values.DeleteByIDs"); n != 1 {
		t.Errorf("DeleteByIDs calls=%d, want 1", n)
	}
	if len(f.values.rows) != 2 {
		t.Fatalf("rows left=%d, want the other subject's 2 rows untouched", len(f.values.rows))
	}

	// 行数超过 rounds×limit：必须报错，而不是「擦到这儿算完成」。
	f2 := newFixture(t)
	f2.withPurgeLimit(1)
	seedEntityRows(f2, "u_priv_wide", model.EntityScopeMid, testMid, maxEraseRounds+5)
	_, err = callErase(f2, eraseReq(midRef(testMid), 0, "req-erase-cap", "privacy-job:x", "ticket-2"))
	if !errors.Is(err, model.ErrTooManyRows) {
		t.Fatalf("err=%v, want %v", err, model.ErrTooManyRows)
	}
	if n := countCalled(f2.values.calls, "values.DeleteByIDs"); n != maxEraseRounds {
		t.Errorf("DeleteByIDs calls=%d, want exactly maxEraseRounds=%d", n, maxEraseRounds)
	}
	if n := countCalled(f2.values.calls, "values.ListForErase"); n != maxEraseRounds+1 {
		t.Errorf("ListForErase calls=%d, want %d (每轮一次 + 收尾复查)", n, maxEraseRounds+1)
	}
	// 已删掉的行不会被回滚（本方法不在事务里），残留形态必须可见。
	if n := len(f2.values.rows); n != 5 {
		t.Errorf("rows left=%d, want the 5 rows the round cap could not reach", n)
	}
	if got := f2.receipts.failed; len(got) != 1 || got[0] != "req-erase-cap/erase:TOO_MANY_ROWS" {
		t.Errorf("failed receipts=%v, want the erase marked failed with TOO_MANY_ROWS", got)
	}
	if row, ok := f2.receiptRow("req-erase-cap", model.ReceiptOpErase); !ok ||
		row.State != model.ReceiptStateFailed {
		t.Fatalf("receipt = %+v ok=%v, want state failed", row, ok)
	}
	// 同键重试可以接管失败的执行权，但只回它这一轮的计数：
	// 工单要证明「一共删了多少」必须自己把两次的 affected_rows 加起来。
	retry, err := callErase(f2, eraseReq(midRef(testMid), 0, "req-erase-cap",
		"privacy-job:x", "ticket-2"))
	if err != nil {
		t.Fatalf("retry after the round cap: %v", err)
	}
	if retry.GetErasedRows() != 5 || retry.GetReused() {
		t.Errorf("retry erased=%d reused=%v, want 5/false", retry.GetErasedRows(), retry.GetReused())
	}
	if n := len(f2.values.rows); n != 0 {
		t.Errorf("rows left=%d, want 0 after the retry finished the job", n)
	}
}

func TestEraseReplayDoesNotReExecuteAndCannotBeReusedForAnotherSubject(t *testing.T) {
	f := newFixture(t)
	d := f.registerActive(newDef("u_play_finish_7d", 1))
	mine := putRowAt(f, d, 1, testMid, 1, 3600)
	theirs := putRowAt(f, d, 1, "10087", 2, 3600)
	req := eraseReq(midRef(testMid), 0, "req-erase-replay", "privacy-job:x", "ticket-1")

	first, err := callErase(f, req)
	if err != nil {
		t.Fatalf("first erase: %v", err)
	}
	if _, ok := f.valueRow(mine.Key()); ok {
		t.Fatal("the first erase did not delete the target row")
	}
	before := countCalled(f.values.calls, "values.DeleteByIDs")

	replay, err := callErase(f, req)
	if err != nil {
		t.Fatalf("replayed erase: %v", err)
	}
	if !replay.GetReused() || replay.GetErasedRows() != first.GetErasedRows() ||
		replay.GetFeaturesTouched() != first.GetFeaturesTouched() {
		t.Errorf("replay = %d/%d reused=%v, want the first run's %d/%d with reused=true",
			replay.GetErasedRows(), replay.GetFeaturesTouched(), replay.GetReused(),
			first.GetErasedRows(), first.GetFeaturesTouched())
	}
	if n := countCalled(f.values.calls, "values.DeleteByIDs"); n != before {
		t.Errorf("DeleteByIDs calls=%d (was %d): a replay must not delete again", n, before)
	}
	// 同一幂等键换主体：entity_id 参与摘要，必须冲突而不是把第二个主体也抹了。
	if _, err := callErase(f, eraseReq(midRef("10087"), 0, "req-erase-replay",
		"privacy-job:x", "ticket-1")); !errors.Is(err, model.ErrRequestIdReused) {
		t.Errorf("err=%v, want %v", err, model.ErrRequestIdReused)
	}
	if _, ok := f.valueRow(theirs.Key()); !ok {
		t.Error("the second subject's row was erased through a replayed request_id")
	}
}

func TestEraseCountsTouchedFromSelectedRowsAndErasedFromRowsAffected(t *testing.T) {
	f := newFixture(t)
	low := f.registerActive(newDef("u_two_a_7d", 1))
	high := f.registerActive(newDef("u_two_b_7d", 1))
	f.putInt64(low, 1, testMid, 1, f.nowUnix()-60)
	f.putInt64(high, 1, testMid, 2, f.nowUnix()-60)
	// 真库里 DELETE ... IN 的 RowsAffected 可以小于选中的 ID 数（并发清理先删了一步）。
	f.values.deleteShortfall = 1

	reply, err := callErase(f, eraseReq(midRef(testMid), 0, "req-erase-shortfall",
		"privacy-job:x", "ticket-1"))
	if err != nil {
		t.Fatalf("erase: %v", err)
	}
	// 两个计数来自两个集合：touched 按「本次选中的行」数 distinct feature_key，
	// erased 按 SQL 影响行数（README §10.7 声称前者来自「真正擦除的行」，与实现不符）。
	if reply.GetErasedRows() != 1 || reply.GetFeaturesTouched() != 2 {
		t.Fatalf("erased=%d touched=%d, want the pinned 1/2", reply.GetErasedRows(),
			reply.GetFeaturesTouched())
	}
	// 少报的这一次同样被写进回执：重放给出的还是 1，工单无法从任何一侧看出「少了 1」。
	row, ok := f.receiptRow("req-erase-shortfall", model.ReceiptOpErase)
	if !ok || row.State != model.ReceiptStateDone || row.AffectedRows != 1 {
		t.Fatalf("receipt = %+v ok=%v, want done with affected_rows=1", row, ok)
	}
	replay, err := callErase(f, eraseReq(midRef(testMid), 0, "req-erase-shortfall",
		"privacy-job:x", "ticket-1"))
	if err != nil || !replay.GetReused() || replay.GetErasedRows() != 1 {
		t.Errorf("replay erased=%d reused=%v err=%v, want the short 1 replayed as if complete",
			replay.GetErasedRows(), replay.GetReused(), err)
	}
	if n := len(f.values.rows); n != 0 {
		t.Errorf("rows left=%d, want 0 (the rows are gone, only the count is short)", n)
	}
}

// countExpiredRows 直接按谓词数库里的过期行，用于判别响应里的 remaining 是否还是现值。
func countExpiredRows(t *testing.T, f *fixture) int {
	t.Helper()
	n := 0
	for _, r := range f.values.rows {
		if r.ExpireAt > 0 && r.ExpireAt < f.nowUnix() {
			n++
		}
	}
	return n
}
