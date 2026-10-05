// 测试域 5：特征定义注册表 —— RegisterFeature / GetFeatureDefinition / ListFeatureDefinitions。
//
// 注册是本服务唯一的「口径入口」，三条纪律在这里可检查：
//  1. 新注册的版本一律 DRAFT：ACTIVE 只能由 UpdateFeatureState/SwitchFeatureVersion 达成，
//     注册即生效等于绕过评审；
//  2. 同一 (feature_key, version) 的重复注册只能「完全相同则复用」或「拒绝」，
//     绝不原地改写任何列（历史值必须由产出它的那一版解释）；
//  3. 入参非法时一次依赖都不许碰，且守卫的先后顺序本身可判别。
//
// 顺序类期望的事实源是 model 层 SQL 的 ORDER BY（本域是
// model/featuredefinition.go:468 的 `ORDER BY feature_key ASC, version ASC`），
// 不在测试里手抄一份「看起来合理的顺序」。
package logic

import (
	"errors"
	"strings"
	"testing"

	"go-video/services/feature-store/model"
	"go-video/services/feature-store/rpc"
)

// --- 本域共用的调用与断言小工具 ---

const regOperator = "admin:tester"

func regReq(operator, requestID string, def *rpc.FeatureDefinition) *rpc.RegisterFeatureReq {
	return &rpc.RegisterFeatureReq{Definition: def, Operator: operator, RequestId: requestID}
}

func callRegister(f *fixture, in *rpc.RegisterFeatureReq) (*rpc.RegisterFeatureReply, error) {
	return NewRegisterFeatureLogic(f.ctx, f.ServiceContext).RegisterFeature(in)
}

func callGetDef(f *fixture, key string, version int32) (*rpc.GetFeatureDefinitionReply, error) {
	return NewGetFeatureDefinitionLogic(f.ctx, f.ServiceContext).
		GetFeatureDefinition(&rpc.GetFeatureDefinitionReq{FeatureKey: key, Version: version})
}

func callListDefs(f *fixture, in *rpc.ListFeatureDefinitionsReq) (*rpc.ListFeatureDefinitionsReply, error) {
	return NewListFeatureDefinitionsLogic(f.ctx, f.ServiceContext).ListFeatureDefinitions(in)
}

// regProto 把一份 model 定义投影成注册入参。注册不接受调用方指定状态（一律以 DRAFT 入库），
// 所以这里显式清掉 state：若哪天有人传 ACTIVE，守卫必须拒。
func regProto(d *model.FeatureDefinition) *rpc.FeatureDefinition {
	out := defToProto(d)
	out.State = rpc.FeatureState_FEATURE_STATE_UNSPECIFIED
	return out
}

func withProtoState(d *rpc.FeatureDefinition, state int32) *rpc.FeatureDefinition {
	d.State = rpc.FeatureState(state)
	return d
}

func withProtoKey(d *rpc.FeatureDefinition, key string) *rpc.FeatureDefinition {
	d.FeatureKey = key
	return d
}

// assertTouchedNothing 断言「这一次调用一个依赖都没碰」：
// 被守卫挡下的请求不得留下回执行、事务、也不得读任何表。
func assertTouchedNothing(t *testing.T, f *fixture) {
	t.Helper()
	for _, part := range []struct {
		name  string
		calls []string
	}{
		{"definitions", f.defs.calls},
		{"activeVersions", f.pointers.calls},
		{"values", f.values.calls},
		{"switches", f.switches.calls},
		{"backfills", f.backfills.calls},
		{"receipts", f.receipts.calls},
	} {
		if len(part.calls) != 0 {
			t.Fatalf("%s was called %v: a rejected request must not touch any dependency",
				part.name, part.calls)
		}
	}
	if n := f.txBegins(); n != 0 {
		t.Fatalf("transactions opened=%d, want 0", n)
	}
}

// --- RegisterFeature：守卫与守卫顺序 ---

func TestRegisterFeatureGuardOrderTouchesNothing(t *testing.T) {
	validKey := "u_play_finish_7d"
	cases := []struct {
		name string
		in   *rpc.RegisterFeatureReq
		want error
	}{
		{"空操作人", regReq("   ", "req-op", regProto(newDef(validKey, 1))), model.ErrOperatorRequired},
		{"操作人超宽", regReq(strings.Repeat("a", maxOperatorLen+1), "req-op",
			regProto(newDef(validKey, 1))), model.ErrOperatorRequired},
		{"缺幂等键", regReq(regOperator, "  ", regProto(newDef(validKey, 1))), model.ErrRequestIdRequired},
		{"幂等键超宽", regReq(regOperator, strings.Repeat("r", maxRequestIDLen+1),
			regProto(newDef(validKey, 1))), model.ErrRequestIdRequired},
		{"没有定义", regReq(regOperator, "req-nil", nil), model.ErrFeatureKeyRequired},
		{"注册即生效被拒", regReq(regOperator, "req-state",
			withProtoState(regProto(newDef(validKey, 1)), model.FeatureStateActive)),
			model.ErrFeatureStateTransition},
		{"注册即下线被拒", regReq(regOperator, "req-state2",
			withProtoState(regProto(newDef(validKey, 1)), model.FeatureStateRetired)),
			model.ErrFeatureStateTransition},
		// 顺序判别①：操作人先于幂等键 —— 两个都缺时报的是操作人。
		{"操作人先于幂等键", regReq("", "", regProto(newDef(validKey, 1))), model.ErrOperatorRequired},
		// 顺序判别②：状态闸门先于 ValidateFeatureDefinition ——
		// 「越界状态 + 非法键形态（长度 1 不合 feature_key 规则）」报的是状态错。
		// 两道守卫一旦调换，这一条立刻变红。
		{"状态闸门先于定义校验", regReq(regOperator, "req-order",
			withProtoState(withProtoKey(regProto(newDef(validKey, 1)), "u"),
				model.FeatureStateRetired)), model.ErrFeatureStateTransition},
		// 对照：同一份非法键入参在状态合法时，报的才是键校验错。
		{"非法键由定义校验兜住", regReq(regOperator, "req-key",
			withProtoKey(regProto(newDef(validKey, 1)), "u")), model.ErrFeatureKeyRequired},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			_, err := callRegister(f, c.in)
			if !errors.Is(err, c.want) {
				t.Fatalf("err=%v, want %v", err, c.want)
			}
			assertTouchedNothing(t, f)
			if len(f.defs.rows) != 0 {
				t.Fatalf("definitions table got %d rows from a rejected request", len(f.defs.rows))
			}
		})
	}
}

func TestRegisterFeatureStoresDraftAndEnsuresPointerRow(t *testing.T) {
	f := newFixture(t)
	key := "u_play_finish_7d"

	reply, err := callRegister(f, regReq(regOperator, "req-new", regProto(newDef(key, 1))))
	if err != nil {
		t.Fatalf("RegisterFeature: %v", err)
	}
	if !reply.GetCreated() || reply.GetReused() {
		t.Fatalf("created=%v reused=%v, want a genuine first registration",
			reply.GetCreated(), reply.GetReused())
	}
	if got := reply.GetDefinition().GetState(); got != rpc.FeatureState_FEATURE_STATE_DRAFT {
		t.Errorf("reply state=%v, want DRAFT: 注册即生效等于绕过评审", got)
	}
	stored, ok := f.defs.rows[model.DefinitionKey{FeatureKey: key, Version: 1}]
	if !ok {
		t.Fatalf("definition %s@v1 was not stored", key)
	}
	if stored.State != model.FeatureStateDraft {
		t.Errorf("stored state=%d, want DRAFT(%d)", stored.State, model.FeatureStateDraft)
	}
	if stored.CreatedBy != regOperator {
		t.Errorf("created_by=%q, want the trimmed operator %q", stored.CreatedBy, regOperator)
	}
	// 摘要只能由 model 算：入库值必须与 model 复算的一致（logic 自己拼会算出对不上的值）。
	if stored.ImmutableDigest == "" || stored.ImmutableDigest != stored.ImmutableDigestOf() {
		t.Errorf("stored immutable_digest is not the model-computed one")
	}
	if stored.DefinitionDigest == "" || stored.DefinitionDigest != stored.DefinitionDigestOf() {
		t.Errorf("stored definition_digest is not the model-computed one")
	}
	// 注册新 key 必须幂等建指针行，否则读路径无法把「已注册」与「未注册」区分开。
	ptr, ok := f.pointers.rows[key]
	if !ok {
		t.Fatalf("no active pointer row for %s", key)
	}
	if ptr.ActiveVersion != model.NoActiveVersion {
		t.Errorf("pointer active_version=%d, want 0: a freshly registered version serves nothing",
			ptr.ActiveVersion)
	}
	// 读表顺序：先查同版本是否已存在，再插入。
	if got := f.defs.calls; len(got) != 2 || got[0] != "definitions.FindOne" ||
		got[1] != "definitions.Insert" {
		t.Errorf("definitions call trace=%v, want FindOne then Insert", got)
	}
	if n := countCalled(f.pointers.calls, "activeVersions.Ensure"); n != 1 {
		t.Errorf("activeVersions.Ensure calls=%d, want 1", n)
	}
	// Ensure 要求 session：脱离事务的写入与后续切换之间没有隔离边界。
	if begins, commits, rollbacks := f.db.pairs(); begins != 1 || commits != 1 || rollbacks != 0 {
		t.Errorf("pointer ensure tx = %d begin/%d commit/%d rollback, want 1/1/0",
			begins, commits, rollbacks)
	}
	if n := countCalled(f.receipts.calls, "receipts.MarkDone"); n != 1 {
		t.Errorf("MarkDone calls=%d, want 1", n)
	}
	if row := f.receipts.rows[receiptKey("req-new", model.ReceiptOpRegister)]; row.DetailKept != 1 ||
		row.AffectedRows != 1 {
		t.Errorf("register receipt = %+v, want detail kept with affected_rows=1", row)
	}
}

// --- RegisterFeature：同一版本的重复注册 ---

func TestRegisterIdenticalRepeatIsReusedWithoutSecondInsert(t *testing.T) {
	f := newFixture(t)
	key := "u_play_finish_7d"
	f.putDef(newDef(key, 1, withState(model.FeatureStateDraft)))
	f.putPointer(key, 1, 0) // 该 key 已上线过：复用分支不该再碰指针

	reply, err := callRegister(f, regReq(regOperator, "req-reuse", regProto(newDef(key, 1))))
	if err != nil {
		t.Fatalf("identical re-registration: %v", err)
	}
	if !reply.GetReused() || reply.GetCreated() {
		t.Errorf("reused=%v created=%v, want the caller to be able to tell a reuse from a creation",
			reply.GetReused(), reply.GetCreated())
	}
	if n := countCalled(f.defs.calls, "definitions.Insert"); n != 0 {
		t.Errorf("definitions.Insert calls=%d, want 0", n)
	}
	if n := countCalled(f.pointers.calls, "activeVersions.Ensure"); n != 0 {
		t.Errorf("Ensure calls=%d, want 0: the reuse path must not touch the pointer table", n)
	}
	if f.txBegins() != 0 {
		t.Errorf("transactions=%d, want 0", f.txBegins())
	}
	receipt := f.receipts.rows[receiptKey("req-reuse", model.ReceiptOpRegister)]
	if receipt.AffectedRows != 0 {
		t.Errorf("reuse receipt affected_rows=%d, want 0", receipt.AffectedRows)
	}
	// 回执里的快照必须说「没有新增」，否则重放会谎报 created。
	if !strings.Contains(receipt.ResultJSON, `"c":false`) {
		t.Errorf("receipt snapshot = %s, want created=false recorded", receipt.ResultJSON)
	}
}

func TestRegisterChangedImmutableShapeIsRejected(t *testing.T) {
	f := newFixture(t)
	key := "u_play_finish_7d"
	f.putDef(newDef(key, 1, withState(model.FeatureStateDraft)))

	// 同一个 (key, version) 换来源：这是另一个特征，不是同一特征的又一次注册。
	changed := regProto(newDef(key, 1, withSource(model.SourceOfflineModel)))
	_, err := callRegister(f, regReq(regOperator, "req-immutable", changed))
	if !errors.Is(err, model.ErrFeatureDefinitionImmutable) {
		t.Fatalf("err=%v, want %v", err, model.ErrFeatureDefinitionImmutable)
	}
	if !strings.Contains(err.Error(), key+"@v1") {
		t.Errorf("err=%v must name which version is immutable", err)
	}
	stored := f.defs.rows[model.DefinitionKey{FeatureKey: key, Version: 1}]
	if stored.Source != model.SourceSpmMetric {
		t.Errorf("stored source=%d was rewritten to %d", model.SourceSpmMetric, stored.Source)
	}
	if n := countCalled(f.defs.calls, "definitions.Insert"); n != 0 {
		t.Errorf("Insert calls=%d, want 0", n)
	}
	// 失败必须落 MarkFailed：幂等键的意义是「同一件事只发生一次」，不是「一次失败永久占坑」。
	want := "req-immutable/" + model.ReceiptOpRegister + ":" + errorCodeOf(model.ErrFeatureDefinitionImmutable)
	if len(f.receipts.failed) != 1 || f.receipts.failed[0] != want {
		t.Errorf("failed receipts=%v, want [%s]", f.receipts.failed, want)
	}
	// 判别对照：同一形状再注册一次（不换来源）就必须是复用而不是不可变错误。
	if _, err := callRegister(f, regReq(regOperator, "req-immutable-2",
		regProto(newDef(key, 1)))); err != nil {
		t.Errorf("control: registering the unchanged shape again got %v, want reuse", err)
	}
}

func TestRegisterChangedTTLOrDefaultValueIsMetadataImmutable(t *testing.T) {
	f := newFixture(t)
	key := "u_play_finish_7d"
	f.putDef(newDef(key, 1, withState(model.FeatureStateDraft)))

	for _, c := range []struct {
		name string
		def  *model.FeatureDefinition
	}{
		{"换 TTL", newDef(key, 1, withTTL(7200))},
		{"换默认值", newDef(key, 1, withDefault("8"))},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := callRegister(f, regReq(regOperator, "req-meta-"+c.name, regProto(c.def)))
			if !errors.Is(err, model.ErrFeatureMetadataImmutable) {
				t.Fatalf("err=%v, want %v", err, model.ErrFeatureMetadataImmutable)
			}
		})
	}
	stored := f.defs.rows[model.DefinitionKey{FeatureKey: key, Version: 1}]
	if stored.TTLSeconds != 3600 || stored.DefaultValue != "7" {
		t.Errorf("stored ttl/default = %d/%q, want the original 3600/\"7\" untouched",
			stored.TTLSeconds, stored.DefaultValue)
	}
	// 判别对照：不可变五字段相同、只是 TTL 不同 —— 报的必须是 METADATA 而不是 DEFINITION。
	if errors.Is(model.ErrFeatureDefinitionImmutable, model.ErrFeatureMetadataImmutable) {
		t.Fatal("test premise broken: the two sentinels are the same error")
	}
}

func TestRegisterPrivacyDriftIsRejectedEvenThoughDigestMatches(t *testing.T) {
	f := newFixture(t)
	key := "u_play_finish_7d"
	f.putDef(newDef(key, 1, withState(model.FeatureStateDraft), withPrivacy(model.PrivacyPseudonymous)))

	// privacy_level 故意不进注册摘要（否则调过级别就再也无法幂等重放注册），
	// 因此这里必须靠摘要之外的独立比对：一次注册不能悄悄把特征升到更敏感的级别。
	higher := regProto(newDef(key, 1, withPrivacy(model.PrivacyUserProfile)))
	_, err := callRegister(f, regReq(regOperator, "req-privacy", higher))
	if !errors.Is(err, model.ErrFeatureMetadataImmutable) {
		t.Fatalf("err=%v, want %v", err, model.ErrFeatureMetadataImmutable)
	}
	if !strings.Contains(err.Error(), "UpdateFeaturePrivacy") {
		t.Errorf("err=%v must point at the audited path instead of failing silently", err)
	}
	if got := f.defs.rows[model.DefinitionKey{FeatureKey: key, Version: 1}].PrivacyLevel; got !=
		model.PrivacyPseudonymous {
		t.Errorf("stored privacy_level=%d was raised to %d by a registration", got,
			model.PrivacyUserProfile)
	}
	// 判别对照：同级别同形状的注册就是复用（证明上面这条拒绝只由 privacy 一项触发）。
	if _, err := callRegister(f, regReq(regOperator, "req-privacy-ok",
		regProto(newDef(key, 1)))); err != nil {
		t.Errorf("control: same-privacy repeat got %v, want reuse", err)
	}
}

// TestRegisterSilentlyDropsChangedNameAndDescription 是现状哨兵：
// name / description / change_note 既不在两个摘要里，也不在 reuseOrReject 的比对里，
// 于是「换了口径说明的重复注册」被当成幂等复用，新说明被就地丢弃，而服务里没有任何
// 更新这三列的接口。收严（要么拒、要么改）之后本用例必须变红。
func TestRegisterSilentlyDropsChangedNameAndDescription(t *testing.T) {
	f := newFixture(t)
	key := "u_play_finish_7d"
	f.putDef(newDef(key, 1, withState(model.FeatureStateDraft)))

	rewritten := regProto(newDef(key, 1))
	rewritten.Name = "近 7 日完播率（改口径后）"
	rewritten.Description = "现在按播放时长占比计算，与首版完全不同"
	rewritten.ChangeNote = "换算法"

	reply, err := callRegister(f, regReq(regOperator, "req-metadata-drift", rewritten))
	if err != nil {
		t.Fatalf("current behaviour: metadata drift is accepted as reuse, got %v", err)
	}
	if !reply.GetReused() {
		t.Errorf("reused=false, want the current reuse behaviour")
	}
	stored := f.defs.rows[model.DefinitionKey{FeatureKey: key, Version: 1}]
	if stored.Description == "现在按播放时长占比计算，与首版完全不同" {
		t.Errorf("description was rewritten in place: the immutability promise is gone")
	}
	if stored.Description != "近 7 日完播率分子" || stored.Name != key {
		t.Errorf("stored name/description = %q/%q, want the original pair", stored.Name,
			stored.Description)
	}
	if reply.GetDefinition().GetDescription() != "近 7 日完播率分子" {
		t.Errorf("the reply echoes %q instead of the row actually stored",
			reply.GetDefinition().GetDescription())
	}
	if n := countCalled(f.defs.calls, "definitions.Insert"); n != 0 {
		t.Errorf("Insert calls=%d, want 0", n)
	}
}

func TestRegisterReplayDoesNotInsertAgain(t *testing.T) {
	f := newFixture(t)
	key := "u_play_finish_7d"
	req := regReq(regOperator, "req-replay", regProto(newDef(key, 1)))

	first, err := callRegister(f, req)
	if err != nil || !first.GetCreated() {
		t.Fatalf("first registration = %+v, err %v", first, err)
	}
	// 首次注册之后该版本被评审上线：回放必须回答「现在库里是哪一份」，而不是快照里的旧状态。
	f.putDef(newDef(key, 1, withState(model.FeatureStateActive)))
	f.putPointer(key, 1, 0)
	before := len(f.defs.rows)

	replay, err := callRegister(f, req)
	if err != nil {
		t.Fatalf("replayed RegisterFeature: %v", err)
	}
	if !replay.GetReused() {
		t.Error("reused=false for a replayed request_id")
	}
	// 现状：created 取自首次回执快照（那次确实新建了版本），与 reused 同时为 true。
	// 二者不矛盾：reused 描述的是「这次调用没有再产生副作用」，created 描述的是「那件事的结果」。
	if !replay.GetCreated() {
		t.Errorf("replayed created=false, want the first run's fact replayed")
	}
	if got := replay.GetDefinition().GetState(); got != rpc.FeatureState_FEATURE_STATE_ACTIVE {
		t.Errorf("replayed definition state=%v, want the live row (ACTIVE) not a frozen snapshot", got)
	}
	if n := countCalled(f.defs.calls, "definitions.Insert"); n != 1 {
		t.Errorf("definitions.Insert calls=%d, want exactly 1", n)
	}
	if n := countCalled(f.pointers.calls, "activeVersions.Ensure"); n != 1 {
		t.Errorf("Ensure calls=%d, want exactly 1: a replay must not reopen the pointer step", n)
	}
	if n := countCalled(f.receipts.calls, "receipts.MarkDone"); n != 1 {
		t.Errorf("MarkDone calls=%d, want 1 (the replay must not finish a second time)", n)
	}
	if len(f.defs.rows) != before {
		t.Errorf("replay changed the definition table: %d -> %d rows", before, len(f.defs.rows))
	}
}

func TestRegisterEnsurePointerKeepsExistingActiveVersion(t *testing.T) {
	f := newFixture(t)
	key := "u_play_finish_7d"
	f.registerActive(newDef(key, 1))
	before := f.pointers.rows[key]

	reply, err := callRegister(f, regReq(regOperator, "req-v2", regProto(newDef(key, 2))))
	if err != nil {
		t.Fatalf("register v2: %v", err)
	}
	if !reply.GetCreated() {
		t.Fatal("created=false, want v2 registered as a new version")
	}
	if got := f.defs.rows[model.DefinitionKey{FeatureKey: key, Version: 2}].State; got !=
		model.FeatureStateDraft {
		t.Errorf("v2 state=%d, want DRAFT(%d)", got, model.FeatureStateDraft)
	}
	// Ensure 是 INSERT ... ON DUPLICATE KEY UPDATE mtime：绝不覆盖已有指针。
	after := f.pointers.rows[key]
	if after.ActiveVersion != before.ActiveVersion || after.PreviousVersion != before.PreviousVersion ||
		after.PointerID != before.PointerID {
		t.Errorf("pointer changed %v -> %v: registering a DRAFT version must not switch serving",
			before, after)
	}
	if n := countCalled(f.pointers.calls, "activeVersions.Ensure"); n != 1 {
		t.Errorf("Ensure calls=%d, want 1", n)
	}
}

func TestRegisterPropagatesRawDependencyErrors(t *testing.T) {
	key := "u_play_finish_7d"
	cases := []struct {
		name  string
		setup func(f *fixture)
		want  string
	}{
		{"查同版本失败", func(f *fixture) {
			f.defs.errFindOne = errors.New("dial tcp 127.0.0.1:3306: connection refused")
		}, "connection refused"},
		{"插入失败", func(f *fixture) {
			f.defs.errInsert = errors.New("deadlock found")
		}, "deadlock found"},
		{"建指针行失败", func(f *fixture) {
			f.pointers.errEnsure = errors.New("lock wait timeout")
		}, "lock wait timeout"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			c.setup(f)
			_, err := callRegister(f, regReq(regOperator, "req-raw-"+c.name, regProto(newDef(key, 1))))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err=%v, want the raw dependency error %q surfaced", err, c.want)
			}
			// 取得执行权之后的失败必须落 MarkFailed，否则同一 request_id 会被租约卡住。
			if n := countCalled(f.receipts.calls, "receipts.MarkFailed"); n != 1 {
				t.Errorf("MarkFailed calls=%d, want 1", n)
			}
			if n := countCalled(f.receipts.calls, "receipts.MarkDone"); n != 0 {
				t.Errorf("MarkDone calls=%d, want 0 on a failed run", n)
			}
		})
	}
}

func TestRegisterEnsureFailureRollsBackThePointerStep(t *testing.T) {
	f := newFixture(t)
	key := "u_play_finish_7d"
	f.pointers.errEnsure = errors.New("boom")

	_, err := callRegister(f, regReq(regOperator, "req-ensure-fail", regProto(newDef(key, 1))))
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err=%v, want the ensure failure surfaced", err)
	}
	if _, commits, rollbacks := f.db.pairs(); commits != 0 || rollbacks != 1 {
		t.Errorf("pointer tx = %d commit/%d rollback, want 0/1", commits, rollbacks)
	}
	if _, ok := f.pointers.rows[key]; ok {
		t.Error("a pointer row survived a failed Ensure transaction")
	}
	// 定义插入在事务之外，所以这一行按现状仍然留着；下一次注册会走复用分支。
	if _, ok := f.defs.rows[model.DefinitionKey{FeatureKey: key, Version: 1}]; !ok {
		t.Error("premise broken: the definition insert happens outside the pointer transaction")
	}
}

// --- GetFeatureDefinition ---

func TestGetFeatureDefinitionReadsPointerTableDirectly(t *testing.T) {
	f := newFixture(t)
	key := "u_play_finish_7d"
	f.registerActive(newDef(key, 3))
	f.putDef(newDef(key, 2)) // 上一版仍在库里：指针说谁是生效版本，读的就是谁

	reply, err := callGetDef(f, key, 0)
	if err != nil {
		t.Fatalf("GetFeatureDefinition: %v", err)
	}
	if !reply.GetFound() || reply.GetDefinition().GetVersion() != 3 {
		t.Fatalf("found=%v version=%d, want the ACTIVE pointer version 3",
			reply.GetFound(), reply.GetDefinition().GetVersion())
	}
	// 定义是元数据事实源：这里必须直读指针表，而不是走在线读的批量指针解析/缓存代理。
	if got := f.pointers.calls; len(got) != 1 || got[0] != "activeVersions.FindOne" {
		t.Errorf("pointer call trace=%v, want exactly one direct FindOne", got)
	}
	if got := f.defs.calls; len(got) != 1 || got[0] != "definitions.FindOne" {
		t.Errorf("definition call trace=%v, want exactly one point read", got)
	}
}

// TestGetFeatureDefinitionIgnoresPointerCacheTTLConfig 钉住「与缓存代理层解耦」这件事：
// ActivePointerCacheSeconds 关掉（=在线读不缓存指针）时，管理侧读定义的调用序列完全不变。
func TestGetFeatureDefinitionIgnoresPointerCacheTTLConfig(t *testing.T) {
	key := "u_play_finish_7d"
	for _, ttlOn := range []bool{true, false} {
		f := newFixture(t)
		if !ttlOn {
			f.withActivePointerTTLOff()
		}
		f.registerActive(newDef(key, 1))
		if _, err := callGetDef(f, key, 0); err != nil {
			t.Fatalf("ttlOn=%v: %v", ttlOn, err)
		}
		if got := f.pointers.calls; len(got) != 1 || got[0] != "activeVersions.FindOne" {
			t.Errorf("ttlOn=%v pointer call trace=%v, want the same direct FindOne", ttlOn, got)
		}
	}
}

func TestGetFeatureDefinitionWithoutActiveVersionIsNotFound(t *testing.T) {
	key := "u_play_finish_7d"
	cases := []struct {
		name  string
		setup func(f *fixture)
	}{
		// 未注册：连指针行都没有。
		{"无指针行", func(f *fixture) {}},
		// 注册了但没有任何生效版本（DRAFT 或全部 RETIRED）：指针行在，active_version=0。
		{"有指针行但无生效版本", func(f *fixture) {
			f.putDef(newDef(key, 1, withState(model.FeatureStateDraft)))
			f.putPointer(key, model.NoActiveVersion, 0)
		}},
		// 指针指向一个已被删掉的版本：也只能 found=false，不伪造口径。
		{"指针指向不存在的版本", func(f *fixture) {
			f.putPointer(key, 9, 0)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			c.setup(f)
			reply, err := callGetDef(f, key, 0)
			if err != nil {
				t.Fatalf("err=%v, want found=false not an error", err)
			}
			if reply.GetFound() || reply.GetDefinition() != nil {
				t.Errorf("reply=%+v, want found=false with no definition", reply)
			}
			if c.name == "有指针行但无生效版本" || c.name == "无指针行" {
				if n := countCalled(f.defs.calls, "definitions.FindOne"); n != 0 {
					t.Errorf("definitions.FindOne calls=%d, want 0: no serving version means no read", n)
				}
			}
		})
	}
}

func TestGetFeatureDefinitionPointerReadFailureIsNotNotFound(t *testing.T) {
	f := newFixture(t)
	key := "u_play_finish_7d"
	f.registerActive(newDef(key, 1))
	f.pointers.errFindOne = errors.New("dial tcp: too many connections")

	reply, err := callGetDef(f, key, 0)
	if err == nil {
		t.Fatalf("err=nil reply=%+v, want a pointer read failure surfaced, not found=false", reply)
	}
	if !strings.Contains(err.Error(), "too many connections") {
		t.Errorf("err=%v, want the raw cause surfaced", err)
	}
	if !strings.Contains(err.Error(), "read active pointer") {
		t.Errorf("err=%v, want the failing step named", err)
	}
}

func TestGetFeatureDefinitionExplicitVersionIgnoresPointer(t *testing.T) {
	f := newFixture(t)
	key := "u_play_finish_7d"
	f.registerActive(newDef(key, 1))
	f.putDef(newDef(key, 2, withState(model.FeatureStateRetired)))

	reply, err := callGetDef(f, key, 2)
	if err != nil {
		t.Fatalf("explicit version read: %v", err)
	}
	if !reply.GetFound() || reply.GetDefinition().GetState() != rpc.FeatureState_FEATURE_STATE_RETIRED {
		t.Fatalf("found=%v state=%v, want the retired v2 as registered",
			reply.GetFound(), reply.GetDefinition().GetState())
	}
	if n := countCalled(f.pointers.calls, "activeVersions.FindOne"); n != 0 {
		t.Errorf("pointer reads=%d, want 0: an explicit version is not resolved through the pointer", n)
	}
	// 显式版本查不到同样是 found=false 而不是错误：台账视图按现状回答「没有这一版」。
	if r, err := callGetDef(f, key, 7); err != nil || r.GetFound() {
		t.Errorf("missing version reply=%+v err=%v, want found=false without error", r, err)
	}
}

func TestGetFeatureDefinitionGuardOrder(t *testing.T) {
	cases := []struct {
		name string
		key  string
		ver  int32
		want error
	}{
		{"空键", "   ", 1, model.ErrFeatureKeyRequired},
		{"键形态非法", "U_play", 1, model.ErrFeatureKeyRequired},
		{"键命中范围外名单", "user_vip_level", 1, model.ErrFeatureKeyForbidden},
		{"负版本号", "u_play_finish_7d", -1, model.ErrFeatureVersionRequired},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			_, err := callGetDef(f, c.key, c.ver)
			if !errors.Is(err, c.want) {
				t.Fatalf("err=%v, want %v", err, c.want)
			}
			assertTouchedNothing(t, f)
		})
	}
}

// --- ListFeatureDefinitions ---

func TestListFeatureDefinitionsGuardOrder(t *testing.T) {
	all := func() *rpc.ListFeatureDefinitionsReq {
		return &rpc.ListFeatureDefinitionsReq{Pn: 1, Ps: 10}
	}
	cases := []struct {
		name string
		in   *rpc.ListFeatureDefinitionsReq
		want error
	}{
		{"pn 从 0 起", func() *rpc.ListFeatureDefinitionsReq { r := all(); r.Pn = 0; return r }(),
			model.ErrLimitTooLarge},
		{"ps 越界", func() *rpc.ListFeatureDefinitionsReq { r := all(); r.Ps = model.MaxListPageSize + 1; return r }(),
			model.ErrLimitTooLarge},
		{"ps 为 0", func() *rpc.ListFeatureDefinitionsReq { r := all(); r.Ps = 0; return r }(),
			model.ErrLimitTooLarge},
		{"隐私级别越界", func() *rpc.ListFeatureDefinitionsReq {
			r := all()
			r.MaxPrivacyLevel = rpc.PrivacyLevel(model.PrivacyUserProfile + 1)
			return r
		}(), model.ErrPrivacyUnsetNotAllowed},
		{"主体维度越界", func() *rpc.ListFeatureDefinitionsReq {
			r := all()
			r.EntityScope = rpc.EntityScope(model.EntityScopeIPHash + 1)
			return r
		}(), model.ErrEntityScopeRequired},
		{"来源越界", func() *rpc.ListFeatureDefinitionsReq {
			r := all()
			r.Source = rpc.FeatureSource(model.SourceStaticConfig + 1)
			return r
		}(), model.ErrSourceRequired},
		{"状态越界", func() *rpc.ListFeatureDefinitionsReq {
			r := all()
			r.State = rpc.FeatureState(model.FeatureStateRetired + 1)
			return r
		}(), model.ErrFeatureStateTransition},
		// 守卫先后：分页先于隐私闸门。
		{"分页先于隐私", func() *rpc.ListFeatureDefinitionsReq {
			r := all()
			r.Ps = 0
			r.MaxPrivacyLevel = rpc.PrivacyLevel(model.PrivacyUserProfile + 1)
			return r
		}(), model.ErrLimitTooLarge},
		// 守卫先后：隐私先于主体维度，主体维度先于来源，来源先于状态。
		{"隐私先于主体维度", func() *rpc.ListFeatureDefinitionsReq {
			r := all()
			r.MaxPrivacyLevel = rpc.PrivacyLevel(model.PrivacyUserProfile + 1)
			r.EntityScope = rpc.EntityScope(model.EntityScopeIPHash + 1)
			return r
		}(), model.ErrPrivacyUnsetNotAllowed},
		{"主体维度先于来源", func() *rpc.ListFeatureDefinitionsReq {
			r := all()
			r.EntityScope = rpc.EntityScope(model.EntityScopeIPHash + 1)
			r.Source = rpc.FeatureSource(model.SourceStaticConfig + 1)
			return r
		}(), model.ErrEntityScopeRequired},
		{"来源先于状态", func() *rpc.ListFeatureDefinitionsReq {
			r := all()
			r.Source = rpc.FeatureSource(model.SourceStaticConfig + 1)
			r.State = rpc.FeatureState(model.FeatureStateRetired + 1)
			return r
		}(), model.ErrSourceRequired},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			_, err := callListDefs(f, c.in)
			if !errors.Is(err, c.want) {
				t.Fatalf("err=%v, want %v", err, c.want)
			}
			assertTouchedNothing(t, f)
		})
	}
}

// TestRegisterDuplicateKeyRaceStillRejudges 覆盖「查不到 → 插入撞唯一键 → 回查赢家再判一次」
// 这条并发分支：赢家的行必须被按同一套幂等规则复核，而不是直接落第二条。
func TestRegisterDuplicateKeyRaceStillRejudges(t *testing.T) {
	key := "u_play_finish_7d"

	t.Run("同形状则复用赢家的行", func(t *testing.T) {
		f := newFixture(t)
		f.defs.raceWinner = newDef(key, 1, withState(model.FeatureStateDraft))

		reply, err := callRegister(f, regReq(regOperator, "req-race", regProto(newDef(key, 1))))
		if err != nil {
			t.Fatalf("register racing an existing row: %v", err)
		}
		if !reply.GetReused() || reply.GetCreated() {
			t.Errorf("reused=%v created=%v, want the loser told it reused the winner's row",
				reply.GetReused(), reply.GetCreated())
		}
		if n := countCalled(f.defs.calls, "definitions.FindOne"); n != 2 {
			t.Errorf("FindOne calls=%d, want 2: pre-check plus the re-judge after the unique-key hit", n)
		}
		if n := countCalled(f.pointers.calls, "activeVersions.Ensure"); n != 0 {
			t.Errorf("Ensure calls=%d, want 0: reuse must not create a pointer row", n)
		}
		if f.txBegins() != 0 {
			t.Errorf("transactions=%d, want 0", f.txBegins())
		}
		if len(f.defs.rows) != 1 {
			t.Fatalf("definitions rows=%d, want exactly the winner's 1", len(f.defs.rows))
		}
	})

	t.Run("赢家换了不可变字段则拒绝", func(t *testing.T) {
		f := newFixture(t)
		f.defs.raceWinner = newDef(key, 1, withState(model.FeatureStateDraft),
			withSource(model.SourceOfflineModel))

		_, err := callRegister(f, regReq(regOperator, "req-race-bad", regProto(newDef(key, 1))))
		if !errors.Is(err, model.ErrFeatureDefinitionImmutable) {
			t.Fatalf("err=%v, want %v", err, model.ErrFeatureDefinitionImmutable)
		}
		if len(f.receipts.done) != 0 {
			t.Errorf("receipts marked done=%v, want none on a rejected race", f.receipts.done)
		}
	})
}

// --- ListFeatureDefinitions ---

func TestListFeatureDefinitionsPagesFollowSQLOrder(t *testing.T) {
	f := newFixture(t)
	// 期望顺序的事实源：model/featuredefinition.go:468
	//   ORDER BY feature_key ASC, version ASC LIMIT ? OFFSET ?
	// 逐字节序里 '_'(0x5F) < 'a'(0x61)，所以 a_first 在 aa_both 之前。
	want := []string{"a_first|1", "a_first|2", "aa_both|3", "b_second|1", "c_third|1"}
	// 故意按倒序播种：map 遍历是随机化的，若排序结果依赖插入顺序这条用例会飘红。
	for _, seed := range []struct {
		key string
		ver int32
	}{
		{"c_third", 1}, {"b_second", 1}, {"aa_both", 3}, {"a_first", 2}, {"a_first", 1},
	} {
		f.putDef(newDef(seed.key, seed.ver))
	}

	var got []string
	for pn := int32(1); pn <= 4; pn++ {
		reply, err := callListDefs(f, &rpc.ListFeatureDefinitionsReq{Pn: pn, Ps: 2})
		if err != nil {
			t.Fatalf("page %d: %v", pn, err)
		}
		// total 是过滤后的总数，与本页是否空无关：调用方靠它判断还要不要翻页。
		if reply.GetTotal() != int64(len(want)) {
			t.Fatalf("page %d total=%d, want %d", pn, reply.GetTotal(), len(want))
		}
		for _, d := range reply.GetDefinitions() {
			item := d.GetFeatureKey() + "|" + int32Text(d.GetVersion())
			got = append(got, item)
			// 分页拼接结果与库里回读的行逐项比对，而不是只比对键名。
			row, ok := f.defs.rows[model.DefinitionKey{FeatureKey: d.GetFeatureKey(),
				Version: d.GetVersion()}]
			if !ok || row.Description != d.GetDescription() || row.State != model.FeatureStateActive {
				t.Errorf("%s projected a row that does not match the stored one: %+v vs %+v",
					item, d, row)
			}
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("pages concatenated to %v, want %v", got, want)
	}
	// 第四页越界：空页而不是回绕，也不是报错。
	last, err := callListDefs(f, &rpc.ListFeatureDefinitionsReq{Pn: 4, Ps: 2})
	if err != nil {
		t.Fatalf("page past the end: %v", err)
	}
	if len(last.GetDefinitions()) != 0 {
		t.Errorf("page past the end returned %d rows, want 0", len(last.GetDefinitions()))
	}
}

func TestListFeatureDefinitionsPassesFiltersDown(t *testing.T) {
	f := newFixture(t)
	f.putDef(newDef("u_play_finish_7d", 1))
	f.putDef(newDef("u_play_finish_7d", 2, withState(model.FeatureStateDraft)))
	f.putDef(newDef("u_watch_time_7d", 1))
	f.putDef(newDef("d_device_open_cnt_7d", 1, withScope(model.EntityScopeDevice),
		withPrivacy(model.PrivacyPseudonymous)))

	// 前缀过滤：入参两侧空白被裁掉后原样下传（LIKE 的参数化前缀匹配，不做形态校验）。
	if _, err := callListDefs(f, &rpc.ListFeatureDefinitionsReq{
		FeatureKeyPrefix: "  u_play  ", Pn: 2, Ps: 3}); err != nil {
		t.Fatalf("ListFeatureDefinitions: %v", err)
	}
	seen := f.defs.lastFilter
	if seen.KeyPrefix != "u_play" || seen.Pn != 2 || seen.Ps != 3 {
		t.Errorf("filter down-transferred as %+v, want key_prefix trimmed and pn/ps kept verbatim",
			seen)
	}

	cases := []struct {
		name  string
		in    *rpc.ListFeatureDefinitionsReq
		want  []string
		total int64
	}{
		{"只看 DRAFT", &rpc.ListFeatureDefinitionsReq{
			State: rpc.FeatureState_FEATURE_STATE_DRAFT, Pn: 1, Ps: 10}, []string{"u_play_finish_7d|2"}, 1},
		{"只看 ACTIVE", &rpc.ListFeatureDefinitionsReq{
			State: rpc.FeatureState_FEATURE_STATE_ACTIVE, Pn: 1, Ps: 10},
			[]string{"d_device_open_cnt_7d|1", "u_play_finish_7d|1", "u_watch_time_7d|1"}, 3},
		{"只看设备维度", &rpc.ListFeatureDefinitionsReq{
			EntityScope: rpc.EntityScope_ENTITY_SCOPE_DEVICE, Pn: 1, Ps: 10},
			[]string{"d_device_open_cnt_7d|1"}, 1},
		{"只看 spm 留存来源（一个都没有）", &rpc.ListFeatureDefinitionsReq{
			Source: rpc.FeatureSource_FEATURE_SOURCE_SPM_RETENTION, Pn: 1, Ps: 10}, nil, 0},
		{"前缀 + 状态组合", &rpc.ListFeatureDefinitionsReq{
			FeatureKeyPrefix: "u_play", State: rpc.FeatureState_FEATURE_STATE_DRAFT,
			Pn: 1, Ps: 10}, []string{"u_play_finish_7d|2"}, 1},
		{"前缀不匹配任何行", &rpc.ListFeatureDefinitionsReq{
			FeatureKeyPrefix: "zz", Pn: 1, Ps: 10}, nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reply, err := callListDefs(f, c.in)
			if err != nil {
				t.Fatalf("ListFeatureDefinitions: %v", err)
			}
			var got []string
			for _, d := range reply.GetDefinitions() {
				got = append(got, d.GetFeatureKey()+"|"+int32Text(d.GetVersion()))
			}
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("rows=%v, want %v", got, c.want)
			}
			if reply.GetTotal() != c.total {
				t.Errorf("total=%d, want %d", reply.GetTotal(), c.total)
			}
		})
	}
}

// TestListFeatureDefinitionsPrivacyFilterIsCallerDeclared 钉住本方法的隐私闸门语义：
// max_privacy_level 未填（0）在定义列表这一视图里是「不设限」，而不是套用
// Privacy.ExportMaxPrivacyLevel —— 与 ListEntityFeatures 的服务端封顶形成对照。
// 现状哨兵：收严成「0 = 取服务端封顶」后，本用例第一条断言必须变红。
func TestListFeatureDefinitionsPrivacyFilterIsCallerDeclared(t *testing.T) {
	f := newFixture(t).withExportMaxPrivacy(model.PrivacyPseudonymous)
	f.putDef(newDef("u_pseudo_7d", 1, withPrivacy(model.PrivacyPseudonymous)))
	f.putDef(newDef("u_profile_7d", 1, withPrivacy(model.PrivacyUserProfile)))

	unrestricted, err := callListDefs(f, &rpc.ListFeatureDefinitionsReq{Pn: 1, Ps: 10})
	if err != nil {
		t.Fatalf("ListFeatureDefinitions: %v", err)
	}
	if unrestricted.GetTotal() != 2 {
		t.Errorf("total=%d with max_privacy_level unset, want 2: 现状是「不填即不设限」",
			unrestricted.GetTotal())
	}
	capped, err := callListDefs(f, &rpc.ListFeatureDefinitionsReq{
		Pn: 1, Ps: 10, MaxPrivacyLevel: rpc.PrivacyLevel(model.PrivacyPseudonymous)})
	if err != nil {
		t.Fatalf("ListFeatureDefinitions capped: %v", err)
	}
	if capped.GetTotal() != 1 || capped.GetDefinitions()[0].GetFeatureKey() != "u_pseudo_7d" {
		t.Errorf("capped rows=%+v, want only the PSEUDONYMOUS definition", capped.GetDefinitions())
	}
	// 判别对照：级别 4 只有在调用方显式要求时才出现。
	if _, ok := f.defs.rows[model.DefinitionKey{FeatureKey: "u_profile_7d", Version: 1}]; !ok {
		t.Fatal("test premise broken: the USER_PROFILE definition must still be stored")
	}
}

func TestListFeatureDefinitionsPropagatesRawError(t *testing.T) {
	f := newFixture(t)
	boom := errors.New("dial tcp 127.0.0.1:3306: connectex: no connection")
	f.defs.errList = boom

	reply, err := callListDefs(f, &rpc.ListFeatureDefinitionsReq{Pn: 1, Ps: 10})
	// 三类下游故障之一：原始错误就地原样上抛（不换成不透明哨兵、也不吞成空列表）。
	if err != boom {
		t.Fatalf("err=%v, want the identical raw error surfaced", err)
	}
	if reply != nil {
		t.Errorf("reply=%+v, want nil alongside the error", reply)
	}
	if n := countCalled(f.defs.calls, "definitions.List"); n != 1 {
		t.Errorf("definitions.List calls=%d, want 1 (no silent retry)", n)
	}
}
