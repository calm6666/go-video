// 本文件覆盖 RevokeStreamKeyLogic（吊销密钥，可选级联停流）。
//
// 要钉住的契约按重要性排序：
//  1. 幂等锚点是「密钥终态」而不是 request_id：REVOKED 不可逆，所以「谁第二次来吊销」
//     都命中同一分支。三态（不存在 / 已被自己吊销 / 已被别人吊销）必须能区分开，
//     而 reply.replayed 的 proto 注释（「true 表示命中 request_id」）与实现口径不符，按现状钉死。
//  2. 级联停流的边界：扫描上限来自密钥自身的 max_streams，排序来自 SQL 的 stream_id ASC，
//     两者共同决定「吊销之后还有几条流在推」——这件事响应里没有任何字段交代。
//  3. 审计落点：吊销写了哪些列、没写哪些列（谁吊销的、什么时候吊销的）。
//  4. 事务原子性：级联中途失败时「密钥还能用」必须是真的（整体回滚）。
//
// 与 README「已知缺口」的关系：CDN/媒体入口是 stub（缺口 1）不在这里重复登记；
// 本文件只登记吊销这一路新发现的口径问题。
package logic

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// --- 夹具 ---

// revokeFixture 是一把「可鉴权、绑定了一个节点、可以挂若干非终态流」的密钥。
type revokeFixture struct {
	key   *model.StreamKey
	plain string
}

// seedRevokeKey 只铺密钥（不碰节点行）。
// 一个用例里需要第二把密钥时必须用它：seedNode 是按 node_id 覆盖整行，
// 再走一遍 seedRevokeFixture 会把已经累计的 active_streams 抹回种子值，
// 级联退配额就会在第二条流上拿到「node quota already drained」并整体回滚。
func (e *testEnv) seedRevokeKey(t *testing.T, streamName string, maxStreams int32) revokeFixture {
	t.Helper()
	key, plain := seedAuthableKey(t, e, streamName, 71, 1001, func(k *model.StreamKey) {
		k.MaxStreams = maxStreams
	})
	return revokeFixture{key: key, plain: plain}
}

func (e *testEnv) seedRevokeFixture(t *testing.T, streamName string, maxStreams int32) revokeFixture {
	t.Helper()
	fx := e.seedRevokeKey(t, streamName, maxStreams)
	e.seedNode(t, &model.IngestNode{NodeID: "node-rev", ProtocolMask: model.ProtocolMaskRtmp,
		CapacityStreams: 10, HealthScore: 70})
	return fx
}

// attachStreamToKey 给密钥挂一条指定状态/seq 的流；withLease 时同时占住节点配额与生效租约。
//
// 租约与配额走真写路径（ReserveQuota + Insert）而不是手工改计数：
// 级联停流要「退配额」，种子若绕过 CAS 就会把「配额退成负数/退两次」这类缺陷掩盖掉。
func (e *testEnv) attachStreamToKey(t *testing.T, keyID int64, streamID string, state int32,
	seq int64, withLease bool) *model.Stream {
	t.Helper()
	row := e.seedStream(t, &model.Stream{
		StreamID: streamID, RoomID: 71, AnchorMid: 1001, KeyID: keyID, StreamName: "live_revoke_seed",
		Protocol: 1, State: state, Seq: seq,
	})
	if !withLease {
		return row
	}
	return e.attachLease(t, streamID, "node-rev", "assign-"+streamID)
}

// attachLease 给一条已落库的流补上「占着节点配额 + 一条生效租约」的形态（节点行须已存在）。
// 与 seedAssignedNode 的区别是不重建流与节点行：多条流共用同一个节点时，
// 重建节点会把 active_streams 抹回种子值，配额断言就成了自证。
func (e *testEnv) attachLease(t *testing.T, streamID, nodeID, requestID string) *model.Stream {
	t.Helper()
	row := e.streamRow(t, streamID)
	ok, err := e.repo.IngestNode.ReserveQuota(bg(), nil, nodeID)
	if err != nil || !ok {
		t.Fatalf("seed 占配额 %s：ok=%v err=%v", nodeID, ok, err)
	}
	e.db.streams[streamID].NodeID = nodeID
	if _, err := e.repo.NodeAssignment.Insert(bg(), nil, &model.NodeAssignment{
		RequestID: requestID, StreamID: streamID, RoomID: row.RoomID, KeyID: row.KeyID,
		NodeID: nodeID, Protocol: row.Protocol, State: model.AssignmentStateActive, AssignedAt: 1700000100,
	}); err != nil {
		t.Fatalf("seed 租约 %s：%v", requestID, err)
	}
	return e.streamRow(t, streamID)
}

// revokeWrites 是一次成功吊销（不含级联）在库里的最小写集：只有密钥行的两列。
var revokeNoCascadeLog = []string{
	"StreamKey.FindOne",  // 事务外预检（存在性 + 归属）
	"StreamKey.LockByID", // 事务内行锁
	"StreamKey.FindOne",  // 替身的 LockByID 走同一条 SELECT，留痕成对出现
	"StreamKey.TransitionState",
}

// --- 依赖缺席与守卫：必须零触库 ---

func TestRevokeStreamKey_NoRepositoryIsError(t *testing.T) {
	e := newTestEnv(t)
	mark, before := e.markCalls(), e.effects()
	reply, err := NewRevokeStreamKeyLogic(bg(), e.withoutRepository()).
		RevokeStreamKey(&rpc.RevokeStreamKeyReq{KeyId: 1, RequestId: "r-any", OperatorMid: 9001,
			Admin: true, Reason: "泄露"})
	wantFail(t, err, errNoRepository, "无 Repository")
	if reply != nil {
		t.Fatalf("无 Repository 时不得返回响应体：%+v", reply)
	}
	e.requireNoCallsAfter(t, mark, "无 Repository")
	e.requireSameEffects(t, before, "无 Repository")
}

// TestRevokeStreamKey_GuardsTouchNothing 钉住「入参不合法就连一条 SQL 都不该发出去」。
// 吊销是风控按钮，密钥泄露时每一毫秒都在被推流，校验不能拖到事务里。
func TestRevokeStreamKey_GuardsTouchNothing(t *testing.T) {
	longReq := strings.Repeat("r", maxIdempotencyKeyBytes+1)
	longReason := strings.Repeat("泄", maxReasonRunes+1)
	cases := []struct {
		name string
		want error
		mut  func(*rpc.RevokeStreamKeyReq)
	}{
		{"key_id=0", model.ErrInvalidKeyId, func(in *rpc.RevokeStreamKeyReq) { in.KeyId = 0 }},
		{"key_id 为负", model.ErrInvalidKeyId, func(in *rpc.RevokeStreamKeyReq) { in.KeyId = -3 }},
		{"空 request_id", model.ErrIdempotencyKeyRequired, func(in *rpc.RevokeStreamKeyReq) { in.RequestId = "" }},
		{"request_id 全空格", model.ErrIdempotencyKeyRequired, func(in *rpc.RevokeStreamKeyReq) { in.RequestId = "  " }},
		{"request_id 超列宽", model.ErrIdempotencyKeyRequired, func(in *rpc.RevokeStreamKeyReq) { in.RequestId = longReq }},
		{"operator_mid=0", model.ErrOperatorRequired, func(in *rpc.RevokeStreamKeyReq) { in.OperatorMid = 0 }},
		{"operator_mid 为负", model.ErrOperatorRequired, func(in *rpc.RevokeStreamKeyReq) { in.OperatorMid = -9 }},
		// 吊销的 reason 是必填（与强制停流不同口径，见 closestream_test.go 的对照用例）。
		{"空 reason", model.ErrReasonRequired, func(in *rpc.RevokeStreamKeyReq) { in.Reason = "" }},
		{"reason 全空格", model.ErrReasonRequired, func(in *rpc.RevokeStreamKeyReq) { in.Reason = "   " }},
		{"reason 超长", model.ErrReasonTooLong, func(in *rpc.RevokeStreamKeyReq) { in.Reason = longReason }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv(t)
			fx := e.seedRevokeFixture(t, "live_guard_key", 2)
			e.attachStreamToKey(t, fx.key.KeyID, "S-REV-GUARD", model.StreamStatePublishing, 1, true)
			mark, before := e.markCalls(), e.effects()
			in := &rpc.RevokeStreamKeyReq{KeyId: fx.key.KeyID, RequestId: "r-ok", OperatorMid: 9001,
				Admin: true, Reason: "密钥泄露", StopStream: true}
			c.mut(in)
			_, err := NewRevokeStreamKeyLogic(bg(), e.svc).RevokeStreamKey(in)
			wantFail(t, err, c.want, c.name)
			e.requireNoCallsAfter(t, mark, c.name)
			e.requireSameEffects(t, before, c.name)
			e.requireNoTransaction(t, before, c.name)
			if k := e.keyRow(t, fx.key.KeyID); k.State != model.KeyStateActive {
				t.Fatalf("%s：入参被拒后密钥状态被改动 state=%d", c.name, k.State)
			}
		})
	}
}

// TestRevokeStreamKey_GuardOrderIsStable 钉住守卫次序（key_id → request → operator → reason）：
// 全非法的请求只能报出第一个错，调用方按提示改一次就能过。
func TestRevokeStreamKey_GuardOrderIsStable(t *testing.T) {
	e := newTestEnv(t)
	fx := e.seedRevokeFixture(t, "live_order_key", 1)
	mark := e.markCalls()
	call := func(keyID int64, reqID string, mid int64, reason string) error {
		_, err := NewRevokeStreamKeyLogic(bg(), e.svc).RevokeStreamKey(&rpc.RevokeStreamKeyReq{
			KeyId: keyID, RequestId: reqID, OperatorMid: mid, Reason: reason, Admin: true})
		return err
	}
	wantErr(t, call(0, "", 0, ""), model.ErrInvalidKeyId, "全非法")
	wantErr(t, call(fx.key.KeyID, "", 0, ""), model.ErrIdempotencyKeyRequired, "key 合法后看 request")
	wantErr(t, call(fx.key.KeyID, "r-1", 0, ""), model.ErrOperatorRequired, "request 合法后看 operator")
	wantErr(t, call(fx.key.KeyID, "r-1", 9001, ""), model.ErrReasonRequired, "最后才是 reason")
	e.requireNoCallsAfter(t, mark, "守卫次序")
}

// --- 预检：不存在与越权都必须停在第一次读上 ---

func TestRevokeStreamKey_KeyNotFoundHasNoTransaction(t *testing.T) {
	e := newTestEnv(t)
	mark, before := e.markCalls(), e.effects()
	_, err := e.revokeKey(t, 4242, "r-missing")
	wantFail(t, err, model.ErrStreamKeyNotFound, "密钥不存在")
	e.requireCallsFrom(t, mark, []string{"StreamKey.FindOne"}, "密钥不存在的触库形状")
	e.requireNoTransaction(t, before, "密钥不存在")
	e.requireSameEffects(t, before, "密钥不存在")
}

// TestRevokeStreamKey_OwnershipAdminDecidesNotOperatorIdentity 用判别对钉住归属模型：
// 同一个 operator_mid 去吊销别人的密钥，只有 admin=true 才放行；
// 主播本人（operator_mid == anchor_mid）不需要 admin。
func TestRevokeStreamKey_OwnershipAdminDecidesNotOperatorIdentity(t *testing.T) {
	e := newTestEnv(t)
	fx := e.seedRevokeFixture(t, "live_owner_key", 1)
	before := e.effects()

	mark := e.markCalls()
	_, err := e.revokeKey(t, fx.key.KeyID, "r-other", func(in *rpc.RevokeStreamKeyReq) {
		in.OperatorMid, in.Admin = 2002, false
	})
	wantFail(t, err, model.ErrOperatorRequired, "越权吊销")
	e.requireCallsFrom(t, mark, []string{"StreamKey.FindOne"}, "越权吊销的触库形状")
	e.requireSameEffects(t, before, "越权吊销")
	e.requireNoTransaction(t, before, "越权吊销")

	// 判别对 1：只把 admin 打开，其余不变 → 必须成功。
	e.mustRevokeKey(t, fx.key.KeyID, "r-admin", func(in *rpc.RevokeStreamKeyReq) {
		in.OperatorMid, in.Admin = 2002, true
	})
	if k := e.keyRow(t, fx.key.KeyID); k.State != model.KeyStateRevoked {
		t.Fatalf("admin=true 的吊销未生效：state=%d", k.State)
	}

	// 判别对 2：主播自吊销不需要 admin。
	own := e.seedRevokeFixture(t, "live_self_key", 1)
	e.mustRevokeKey(t, own.key.KeyID, "r-self", func(in *rpc.RevokeStreamKeyReq) {
		in.OperatorMid, in.Admin = own.key.AnchorMid, false
	})
	if k := e.keyRow(t, own.key.KeyID); k.State != model.KeyStateRevoked {
		t.Fatalf("主播自吊销未生效：state=%d", k.State)
	}
}

// --- 状态矩阵：哪些前置状态能被吊销 ---

// TestRevokeStreamKey_PrestartStates 给出「哪些状态可吊销」的完整答案：
// ACTIVE/ROTATING/EXPIRED 是 revokestreamkeylogic.go:82 的三个 fromStates；
// RETIRED（轮转终态）与脏数据状态位都只能拿到 ErrConcurrentUpdate。
// 注意 RETIRED 拿不到吊销不是「安全」而是「口径缺口」：RETIRED 的密钥在鉴权侧已经失效
// （AuthAllowedKeyState 只认 ACTIVE/ROTATING），所以「吊销」这个动作对它本来就是空操作，
// 但运维面板看到的报错是「并发冲突」而不是「已终态」。
func TestRevokeStreamKey_PrestartStates(t *testing.T) {
	cases := []struct {
		name      string
		state     int32
		wantRevok bool // true：真的被吊销；false：按现状只能拿到 ErrConcurrentUpdate
		replay    bool // true：REVOKED 走幂等回放
	}{
		{"ACTIVE 可吊销", model.KeyStateActive, true, false},
		{"ROTATING 可吊销", model.KeyStateRotating, true, false},
		{"EXPIRED 可吊销", model.KeyStateExpired, true, false},
		{"RETIRED 不可吊销", model.KeyStateRetired, false, false},
		{"REVOKED 走回放", model.KeyStateRevoked, false, true},
		{"状态位为 0 的脏数据不可吊销", 0, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv(t)
			fx := e.seedRevokeFixture(t, "live_state_key", 1)
			// 直接改内存行：Insert 会把 0 归一成 ACTIVE，脏数据只能造在种子之后。
			e.db.keys[fx.key.KeyID].State = c.state
			before, mark := e.effects(), e.markCalls()
			reply, err := e.revokeKey(t, fx.key.KeyID, "r-state")

			switch {
			case c.wantRevok:
				wantOK(t, reply, err, c.name)
				mustFalse(t, reply.GetReplayed(), c.name+" 不该报回放")
				// 写序判定必须排在 e.keyRow 之前：keyRow 自己就是一次 StreamKey.FindOne，
				// 放在后面会把这次断言读算进被测序列（requireCallsFrom 是整段逐名比对）。
				e.requireCallsFrom(t, mark, revokeNoCascadeLog, c.name+" 的写序")
				if k := e.keyRow(t, fx.key.KeyID); k.State != model.KeyStateRevoked {
					t.Fatalf("%s：吊销未生效 state=%d", c.name, k.State)
				}
			case c.replay:
				wantOK(t, reply, err, c.name)
				mustTrue(t, reply.GetReplayed(), c.name)
				if k := e.keyRow(t, fx.key.KeyID); k.State != model.KeyStateRevoked {
					t.Fatalf("%s：回放不该改状态：%d", c.name, k.State)
				}
				e.requireSameEffects(t, before, c.name)
			default:
				wantFail(t, err, model.ErrConcurrentUpdate, c.name)
				e.requireSameEffects(t, before, c.name)
				// 同上：触库形状判定排在断言读之前，否则 e.keyRow 的那次 FindOne 会混进序列。
				// CAS 未命中时序列必须止步于那次没命中的 UPDATE：一条都没改，所以也没有下一步。
				e.requireCallsFrom(t, mark, revokeNoCascadeLog, c.name+" 的触库形状")
				if k := e.keyRow(t, fx.key.KeyID); k.State != c.state {
					t.Fatalf("%s：CAS 未命中却改了状态：%d", c.name, k.State)
				}
			}
		})
	}
}

// --- 落库列与审计（本文件的核心发现） ---

// 缺陷:吊销不留任何「谁、什么时候、哪条链路」的可查痕迹
//
// 事实：吊销唯一的持久化写入是 live_stream_key 的 state + reason 两列
// （revokestreamkeylogic.go:81-88 → model/streamkey.go:206-228 的
// `SET state=?, reason=?, mtime=? WHERE key_id=? AND state IN(...)`）。
// 请求里的 operator_mid 与 trace_id 既不进任何列、也不写日志（本方法一次 Logger 调用都没有），
// 而 live_stream_key.request_id / trace_id 两列仍指向**签发时**那次写：
// 于是「这把密钥是谁在什么时候吊销的」在库里无处可查，
// 唯一线索是 reason 自由文本，而它可以被下一次状态变更再覆盖（例如 MarkExpired）。
// 对照：CloseStream 至少把幂等键写进 live_stream_event.report_id；
// RetryFailedEvents 至少把 operator_mid/request_id 打进结构化日志。
// 本用例按现状钉死四处可证伪事实（含替身不镜像 mtime 这一限制，见用例内注释）。
func TestRevokeStreamKey_LeavesNoAttributionOrTimestamp(t *testing.T) {
	e := newTestEnv(t)
	fx := e.seedRevokeFixture(t, "live_audit_key", 1)
	e.db.keys[fx.key.KeyID].TraceID = "trace-at-issuance"
	before := e.effects()

	const operatorMid = int64(9001)
	e.mustRevokeKey(t, fx.key.KeyID, "r-audit-1", func(in *rpc.RevokeStreamKeyReq) {
		in.OperatorMid = operatorMid
		in.TraceId = "trace-at-revoke"
		in.Reason = "密钥出现在公开仓库"
	})
	row := e.keyRow(t, fx.key.KeyID)

	// 唯一真正写入的两列。
	if row.State != model.KeyStateRevoked || row.Reason != "密钥出现在公开仓库" {
		t.Fatalf("吊销必须写 state=REVOKED + reason，实得 state=%d reason=%q", row.State, row.Reason)
	}
	// 归因三件套全部留在签发时的值上。
	if row.TraceID != "trace-at-issuance" {
		t.Fatalf("trace_id 不该被吊销改写（改写等于丢掉签发链路）：%s", row.TraceID)
	}
	if row.RequestID != fx.key.RequestID {
		t.Fatalf("request_id 仍是签发幂等键，实得 %q", row.RequestID)
	}
	// 库里没有任何一列记录「谁吊销的」：StreamKey 结构里没有 operator/revoked_at 字段。
	if strings.Contains(fmt.Sprintf("%+v", row), fmt.Sprint(operatorMid)) {
		t.Fatalf("密钥行里居然出现了 operator_mid，说明归因有落点，请同步删掉本用例的缺陷注释：%+v", row)
	}
	// 日志里也找不到这次吊销：request_id 与明文/哈希都不能出现。
	// 注意这条断言同时是缺陷的另一半——「查不到」既是隐私正确，也是审计缺失。
	logs := captureLogs(t)
	e.mustRevokeKey(t, fx.key.KeyID, "r-audit-2", func(in *rpc.RevokeStreamKeyReq) { in.Reason = "第二次" })
	all := logs.joined()
	for _, forbidden := range []string{"r-audit-1", "r-audit-2", fx.plain, fx.key.KeyHash, fx.key.KeyRef} {
		if strings.Contains(all, forbidden) {
			t.Fatalf("吊销日志里出现了敏感/关联字段 %q：%s", forbidden, all)
		}
	}
	// 两次吊销合计九张表零新增：第一次只把现有密钥行改成 REVOKED（UPDATE，不长行），
	// 第二次是纯回放（连一次 UPDATE 都没发出，TransitionState 不在序列里）。
	// 「零新增」正是本用例的缺陷证据——留痕需要的新行（审计/事件）一条都没有。
	e.requireDelta(t, before, [9]int{}, "吊销不新增任何一行（第二次调用连 UPDATE 都没有）")
	// 替身限制（不是缺陷）：fakeStreamKey.TransitionState 不镜像真 SQL 的 mtime=nowUnix()，
	// 所以这里不断言 mtime 变化——断言了就是在测替身。
}

// --- 幂等：三种「已经吊销」的来路必须可区分 ---

// TestRevokeStreamKey_ReplayAnchoredOnTerminalStateNotRequestID 钉住重放判定：
// 第二次调用换一个**完全不同的** request_id、换一个**完全不同的** operator，
// 仍然拿到 replayed=true —— 判定锚点是终态，不是幂等键。
// 与 proto:201 的注释（「true 表示命中 request_id」）不符：按现状钉死。
func TestRevokeStreamKey_ReplayAnchoredOnTerminalStateNotRequestID(t *testing.T) {
	e := newTestEnv(t)
	fx := e.seedRevokeFixture(t, "live_replay_key", 2)
	first := e.mustRevokeKey(t, fx.key.KeyID, "r-first", func(in *rpc.RevokeStreamKeyReq) {
		in.Reason = "首发：风控吊销"
	})
	mustFalse(t, first.GetReplayed(), "首次吊销")

	// 另一个人、另一个幂等键、另一个理由：结果必须是「已吊销」。
	mark := e.markCalls()
	second := e.mustRevokeKey(t, fx.key.KeyID, "r-another-caller", func(in *rpc.RevokeStreamKeyReq) {
		in.OperatorMid, in.Admin, in.Reason = 2002, true, "另一个理由"
	})
	mustTrue(t, second.GetReplayed(), "他人重复吊销")
	if second.GetState() != rpc.StreamKeyState_STREAM_KEY_STATE_REVOKED {
		t.Fatalf("重放也要回 REVOKED，实得 %v", second.GetState())
	}
	if !strings.Contains(second.GetMessage(), "此前已吊销") {
		t.Fatalf("重放文案必须点明「未产生新变更」，实得 %q", second.GetMessage())
	}
	// 结构证明「重放路径上一条 UPDATE 都没发」：序列里没有 TransitionState。
	e.requireCallsFrom(t, mark, []string{"StreamKey.FindOne", "StreamKey.LockByID", "StreamKey.FindOne"},
		"重放吊销的触库形状")
	// 关键后果：第二次的理由文本被丢弃（库里仍是首次的理由）。这不是「幂等做得好」，
	// 而是「后来的调用方以为自己的理由被记录了」。
	if row := e.keyRow(t, fx.key.KeyID); row.Reason != "首发：风控吊销" {
		t.Fatalf("重放不得改写 reason（否则审计文本会被后来的调用方替换），实得 %q", row.Reason)
	}
	// 「不存在」与「已吊销」必须是两个不同哨兵，否则运维无法区分打错 ID 与重复操作。
	_, err := e.revokeKey(t, 999999, "r-notfound")
	wantFail(t, err, model.ErrStreamKeyNotFound, "不存在的密钥")
}

// TestRevokeStreamKey_ReplayStillCommitsAnEmptyTransaction 钉住重放路径的开销形状：
// 判定在事务内做（要先锁行才知道状态），所以重放也会开一次事务并提交。
// 这条本身不是缺陷（判定必须在锁后），但它是「重放不产生任何写」的唯一反证锚点：
// 若哪天把判定挪到事务外，本用例会以「事务次数变了」的形式报出来。
func TestRevokeStreamKey_ReplayStillCommitsAnEmptyTransaction(t *testing.T) {
	e := newTestEnv(t)
	fx := e.seedRevokeFixture(t, "live_replay_tx", 1)
	e.mustRevokeKey(t, fx.key.KeyID, "r-tx-1")
	before := e.effects()
	e.mustRevokeKey(t, fx.key.KeyID, "r-tx-2")
	if got := e.effects(); got.txs != before.txs+1 {
		t.Fatalf("重放吊销应开 1 次事务（前 %d 后 %d）", before.txs, got.txs)
	}
	e.requireSameEffects(t, before, "重放吊销不得产生数据痕迹")
}

// --- 不级联：默认形态 ---

// TestRevokeStreamKey_NoCascadeLeavesStreamRunningIsSilent 钉住 stop_stream=false 的真实后果：
// 密钥立刻不能再过鉴权（下一次连接被拒），但**正在推的流一条都不停**，
// 而且响应的 message 里明确写了这件事（这是调用方唯一的提示）。
// 写序整段锁死：只有 1 条 UPDATE。
func TestRevokeStreamKey_NoCascadeLeavesStreamRunningIsSilent(t *testing.T) {
	e := newTestEnv(t)
	fx := e.seedRevokeFixture(t, "live_nocascade_key", 3)
	live := e.attachStreamToKey(t, fx.key.KeyID, "S-REV-LIVE", model.StreamStatePublishing, 4, true)
	quotaBefore := e.nodeRow(t, "node-rev").ActiveStreams
	mark, before := e.markCalls(), e.effects()

	reply := e.mustRevokeKey(t, fx.key.KeyID, "r-nocascade")
	e.requireCallsFrom(t, mark, revokeNoCascadeLog, "不级联的吊销写序")
	e.requireDelta(t, before, [9]int{}, "不级联的吊销不新增任何行")

	if reply.GetState() != rpc.StreamKeyState_STREAM_KEY_STATE_REVOKED {
		t.Fatalf("状态回显不符：%v", reply.GetState())
	}
	if len(reply.GetStoppedStreamIds()) != 0 {
		t.Fatalf("未级联却回了被停的流：%v", reply.GetStoppedStreamIds())
	}
	if !strings.Contains(reply.GetMessage(), "进行中的流未停止") {
		t.Fatalf("不级联必须在 message 里说清楚，实得 %q", reply.GetMessage())
	}
	// 流侧一切照旧：状态、seq、租约、配额、活跃指针。
	row := e.streamRow(t, live.StreamID)
	if row.State != model.StreamStatePublishing || row.Seq != 4 {
		t.Fatalf("不级联时流被改动了：state=%d seq=%d", row.State, row.Seq)
	}
	if lease := e.assignmentRow(t, "assign-S-REV-LIVE"); lease.State != model.AssignmentStateActive {
		t.Fatalf("不级联时租约被释放：%+v", lease)
	}
	if got := e.nodeRow(t, "node-rev").ActiveStreams; got != quotaBefore {
		t.Fatalf("不级联时配额被归还：active=%d want=%d", got, quotaBefore)
	}
	if got := e.keyRow(t, fx.key.KeyID).CurrentStreamID; got != "S-REV-LIVE" {
		t.Fatalf("不级联时活跃指针被清空：%q", got)
	}
	// 「立即失效」的确切含义：下一次接入鉴权被拒，原因码是 key_revoked。
	auth, err := e.verifyPublish(t, &rpc.VerifyPublishAuthReq{
		StreamName: fx.key.StreamName, PlaintextKey: fx.plain, RoomId: fx.key.RoomID,
		Protocol: rpc.IngestProtocol_PROTOCOL_RTMP,
	})
	wantOK(t, auth, err, "吊销后鉴权")
	mustFalse(t, auth.GetAllowed(), "吊销后的密钥")
	if auth.GetReason() != reasonKeyRevoked {
		t.Fatalf("吊销后鉴权原因码应为 key_revoked，实得 %q", auth.GetReason())
	}
}

// --- 级联停流 ---

// TestRevokeStreamKey_CascadeStopsEveryActiveStreamInStreamIDOrder 是级联的主用例：
// 一次吊销要停掉该密钥的**全部非终态流**（IDLE/PUBLISHING/INTERRUPTED），
// 顺序由 SQL 的 ORDER BY stream_id ASC 决定，且：
//   - 已 STOPPED 的流不在扫描结果里（state IN 条件）；
//   - 别的密钥的流一条都不许碰（key_id 条件）；
//   - 每条流的停流都走完整收尾（事件 + Outbox + 租约 + 配额 + 活跃指针）；
//   - 级联事件的 report_id 为空：吊销键不能进 report_id（唯一索引，第二条就撞）。
func TestRevokeStreamKey_CascadeStopsEveryActiveStreamInStreamIDOrder(t *testing.T) {
	e := newTestEnv(t)
	fx := e.seedRevokeFixture(t, "live_cascade_key", 3)
	interrupted := e.attachStreamToKey(t, fx.key.KeyID, "S-REV-A", model.StreamStateInterrupted, 3, true)
	open := e.openWindow(t, interrupted.StreamID, 71, "node-rev", 40)
	e.attachStreamToKey(t, fx.key.KeyID, "S-REV-B", model.StreamStatePublishing, 1, true)
	e.attachStreamToKey(t, fx.key.KeyID, "S-REV-C", model.StreamStateIdle, 0, true)
	// 同密钥的终态流：扫描必须按 state 挡掉。
	terminal := e.attachStreamToKey(t, fx.key.KeyID, "S-REV-D", model.StreamStateStopped, 9, false)
	// 另一把密钥的流：一条都不许动。用 seedRevokeKey 而不是 seedRevokeFixture，
	// 否则重复 seedNode 会把上面三次 ReserveQuota 的计数抹掉（见该 helper 注释）。
	other := e.seedRevokeKey(t, "live_other_key", 1)
	untouched := e.attachStreamToKey(t, other.key.KeyID, "S-OTHER", model.StreamStatePublishing, 2, true)

	quotaBefore := e.nodeRow(t, "node-rev").ActiveStreams
	mark, before := e.markCalls(), e.effects()

	reply := e.mustRevokeKey(t, fx.key.KeyID, "r-cascade", func(in *rpc.RevokeStreamKeyReq) {
		in.StopStream = true
		in.Reason = "泄露并级联切断"
	})

	got := e.callsFrom(mark)
	// 扫描发生在抢吊销位之后、且整段只扫一次。
	if got[0] != "StreamKey.FindOne" || got[3] != "StreamKey.TransitionState" ||
		got[4] != "Stream.ListActiveByKey" || e.db.call("Stream.ListActiveByKey") != 1 {
		t.Fatalf("级联顺序不符（先抢吊销位再扫流，且只扫一次）：%v", got)
	}
	// 三条流各自都要走完「迁移→收尾→事件→Outbox」，次数与顺序一起钉。
	// requireCallOrder 的口径是「列几次就要求几次」，所以这里按扫描顺序把三段各列一遍：
	// A 是 INTERRUPTED（停流前还要合上断流区间），B/C 没有区间可合。
	// 未列出的读（Stream.FindOne 等）允许夹在中间——放宽的只有读侧。
	e.requireCallOrder(t, mark, []string{
		"StreamKey.TransitionState",
		"Stream.ListActiveByKey",
		// A：INTERRUPTED
		"Stream.LockByID", "Stream.ApplyTransition",
		"StreamInterruption.FindOpenByStream", "StreamInterruption.Close", "Stream.CloseInterruption",
		"Stream.MarkStopped", "StreamKey.ReleaseActiveStream", "NodeAssignment.FindActiveByStream",
		"NodeAssignment.Release", "IngestNode.ReleaseQuota", "StreamEvent.Insert", "Outbox.Insert",
		// B：PUBLISHING
		"Stream.LockByID", "Stream.ApplyTransition", "Stream.MarkStopped",
		"StreamKey.ReleaseActiveStream", "NodeAssignment.FindActiveByStream",
		"NodeAssignment.Release", "IngestNode.ReleaseQuota", "StreamEvent.Insert", "Outbox.Insert",
		// C：IDLE
		"Stream.LockByID", "Stream.ApplyTransition", "Stream.MarkStopped",
		"StreamKey.ReleaseActiveStream", "NodeAssignment.FindActiveByStream",
		"NodeAssignment.Release", "IngestNode.ReleaseQuota", "StreamEvent.Insert", "Outbox.Insert",
	}, "级联三条流的写序（逐条复用单流收尾，而不是批量 UPDATE）")
	for _, name := range []string{"Stream.ApplyTransition", "Stream.MarkStopped", "NodeAssignment.Release",
		"IngestNode.ReleaseQuota", "StreamEvent.Insert", "Outbox.Insert"} {
		if c := e.db.call(name); c != 3 {
			t.Fatalf("%s 应恰好 3 次（三条流各一次），实得 %d", name, c)
		}
	}
	e.requireDelta(t, before, [9]int{0, 0, 0, 0, 3, 3, 0, 0, 0}, "级联写入增量")

	if got := reply.GetStoppedStreamIds(); len(got) != 3 || got[0] != "S-REV-A" || got[1] != "S-REV-B" ||
		got[2] != "S-REV-C" {
		t.Fatalf("stopped_stream_ids 必须是扫描顺序（stream_id 升序）的三条，实得 %v", got)
	}
	if reply.GetState() != rpc.StreamKeyState_STREAM_KEY_STATE_REVOKED {
		t.Fatalf("级联后状态回显不符：%v", reply.GetState())
	}
	mustFalse(t, reply.GetReplayed(), "级联首次吊销")

	wantSeq := map[string]int64{"S-REV-A": 4, "S-REV-B": 2, "S-REV-C": 1}
	for _, id := range []string{"S-REV-A", "S-REV-B", "S-REV-C"} {
		row := e.streamRow(t, id)
		if row.State != model.StreamStateStopped || row.Seq != wantSeq[id] {
			t.Fatalf("%s 未按预期停流：state=%d seq=%d want_seq=%d", id, row.State, row.Seq, wantSeq[id])
		}
		if row.StopReason != model.StopReasonRevoked {
			t.Fatalf("%s 的停流原因必须是 REVOKED，实得 %d", id, row.StopReason)
		}
		if row.StopDetail != "泄露并级联切断" {
			t.Fatalf("%s 的 stop_detail 必须带吊销原因，实得 %q", id, row.StopDetail)
		}
		if row.HealthState != model.HealthStateNoData {
			t.Fatalf("%s 的健康位未归位：%d", id, row.HealthState)
		}
		ev := e.eventRow(t, id, row.Seq)
		if ev.ReportID != "" {
			// 级联事件的 report_id 必须为空：吊销键进 report_id 会让第二条流撞唯一索引。
			t.Fatalf("%s 的级联事件带了 report_id=%q，吊销键不许写进事件幂等列", id, ev.ReportID)
		}
		if ev.StopReason != model.StopReasonRevoked || ev.Source != model.EventSourceAdmin ||
			ev.ToState != model.StreamStateStopped || ev.Reason != "泄露并级联切断" {
			t.Fatalf("%s 的级联事件口径不符：%+v", id, ev)
		}
		if lease := e.assignmentRow(t, "assign-"+id); lease.State != model.AssignmentStateReleased ||
			lease.ReleaseReason != "泄露并级联切断" {
			t.Fatalf("%s 的租约未按预期释放：%+v", id, lease)
		}
	}
	// 断流区间：INTERRUPTED 那条必须被合上，结束原因是「主动停流」而不是超时。
	win := e.db.interrupt[open.InterruptionID]
	if win.EndedAt <= 0 || win.EndReason != model.InterruptionEndClosed || win.DurationSeconds < 40 {
		t.Fatalf("级联停流未闭合断流区间：%+v", win)
	}
	if got := e.streamRow(t, interrupted.StreamID).InterruptedTotalSecs; got != win.DurationSeconds {
		t.Fatalf("流的 interrupted_total_secs 与区间不符：stream=%d window=%d", got, win.DurationSeconds)
	}
	// 终态流与别的密钥的流：一条都不许碰。
	if row := e.streamRow(t, terminal.StreamID); row.State != model.StreamStateStopped || row.Seq != 9 {
		t.Fatalf("已停的流被重复处理：state=%d seq=%d", row.State, row.Seq)
	}
	if row := e.streamRow(t, untouched.StreamID); row.State != model.StreamStatePublishing || row.Seq != 2 {
		t.Fatalf("动了别的密钥的流：state=%d seq=%d", row.State, row.Seq)
	}
	if got := e.nodeRow(t, "node-rev").ActiveStreams; got != quotaBefore-3 {
		t.Fatalf("配额归还数量不符：active=%d（停流前 %d，应退 3）", got, quotaBefore)
	}
	// 活跃指针：级联后必须为空（指针原本指向扫描到的第一条流）。
	if got := e.keyRow(t, fx.key.KeyID).CurrentStreamID; got != "" {
		t.Fatalf("级联后密钥仍占着活跃指针：%s", got)
	}
	if got := e.keyRow(t, other.key.KeyID).CurrentStreamID; got != "S-OTHER" {
		t.Fatalf("级联误清了别的密钥的活跃指针：%q", got)
	}
	// 三条 outbox 都要能对上自己的事件与 seq（发布器未接线是缺口 2，这里只钉「写了、且同源」）。
	rows, err := e.repo.Outbox.ListByState(bg(), model.OutboxStatePending, 10)
	wantOK(t, rows, err, "查询待发布")
	seen := map[string]int64{}
	for _, r := range rows {
		seen[r.StreamID] = r.Seq
	}
	if len(seen) != 3 || seen["S-REV-A"] != 4 || seen["S-REV-B"] != 2 || seen["S-REV-C"] != 1 {
		t.Fatalf("outbox 的 (stream_id, seq) 与流不符：%v", seen)
	}
}

// TestRevokeStreamKey_CascadeLimitIsKeyMaxStreams 直接单测纯函数 revokeCascadeLimit：
// 0/负数抬到 1（否则历史脏数据一条都停不掉），>50 压到 keyCascadeMax（避免长事务）。
func TestRevokeStreamKey_CascadeLimitIsKeyMaxStreams(t *testing.T) {
	cases := []struct{ in, want int32 }{
		{-5, 1}, {-1, 1}, {0, 1}, {1, 1}, {2, 2}, {49, 49}, {50, keyCascadeMax},
		{51, keyCascadeMax}, {1000, keyCascadeMax}, {1 << 30, keyCascadeMax},
	}
	for _, c := range cases {
		if got := revokeCascadeLimit(c.in); got != c.want {
			t.Fatalf("revokeCascadeLimit(%d)=%d want=%d", c.in, got, c.want)
		}
	}
	if keyCascadeMax != 50 {
		t.Fatalf("keyCascadeMax 变了要同步核对 etc/*.yaml 与 README：%d", keyCascadeMax)
	}
}

// 缺陷:级联扫描上限取密钥配额，越界时「吊销不干净」在响应里完全没有信号
//
// 事实：cascadeLimit = revokeCascadeLimit(k.MaxStreams)（revokestreamkeylogic.go:66），
// 而扫描是 ListActiveByKey(... ORDER BY stream_id ASC LIMIT ?)（model/stream.go:215-233）。
// 也就是说「一次吊销最多停几条流」是由**配额列**决定的，不是由实际活跃流数决定的。
// 可达性说明（不夸大）：正常写路径到不了越界态——建档前会先按 max_streams 判配额
// （verifypublishauthlogic.go:115-119）再用 ClaimActiveStream 抢硬闸（同文件 141-147）。
// 越界只可能来自数据迁移/手工修表/将来放开配额而不改级联逻辑，
// 但一旦发生，reply.stopped_stream_ids 会短于实际活跃流数，
// 而 message 仍是「密钥已吊销，后续接入鉴权一律拒绝」，RevokeStreamKeyReply 没有任何
// 「还剩 N 条未停」的字段（proto:198-203）——运营会以为已经切干净。
// 本用例按现状钉死「只停了 stream_id 最小的一条，另两条仍在推，且响应毫无提示」。
func TestRevokeStreamKey_CascadeCapLeavesSurvivorsWithoutSignal(t *testing.T) {
	e := newTestEnv(t)
	fx := e.seedRevokeFixture(t, "live_cap_key", 1) // max_streams=1 → 级联上限 1
	for _, id := range []string{"S-CAP-A", "S-CAP-B", "S-CAP-C"} {
		e.attachStreamToKey(t, fx.key.KeyID, id, model.StreamStatePublishing, 1, false)
	}
	mark := e.markCalls()
	reply := e.mustRevokeKey(t, fx.key.KeyID, "r-cap", func(in *rpc.RevokeStreamKeyReq) {
		in.StopStream = true
		in.Reason = "泄露"
	})
	if got := reply.GetStoppedStreamIds(); len(got) != 1 || got[0] != "S-CAP-A" {
		t.Fatalf("级联上限应恰好停掉 stream_id 升序的 1 条，实得 %v", got)
	}
	for _, id := range []string{"S-CAP-B", "S-CAP-C"} {
		if row := e.streamRow(t, id); row.State != model.StreamStatePublishing {
			t.Fatalf("%s 本应因上限而被漏停，实得 state=%d", id, row.State)
		}
	}
	// 响应侧「不干净」的三处静默证据。
	if !strings.Contains(reply.GetMessage(), "后续接入鉴权一律拒绝") {
		t.Fatalf("漏停时 message 仍是通用文案（运营无从发现），实得 %q", reply.GetMessage())
	}
	if len(reply.GetStoppedStreamIds()) == 3 {
		t.Fatalf("三条都停了，本用例的前提（上限截断）已不成立，请重新评估")
	}
	// 扫描确实只取了一页：ListActiveByKey 只调一次，且没有任何补扫。
	if c := e.db.call("Stream.ListActiveByKey"); c != 1 {
		t.Fatalf("级联只应扫一次（没有续扫逻辑），实得 %d", c)
	}
	e.requireCallOrder(t, mark, []string{"Stream.ListActiveByKey", "StreamEvent.Insert"}, "扫描后直接停流")
	if c := e.db.call("StreamEvent.Insert"); c != 1 {
		t.Fatalf("只停了 1 条流，事件也应只有 1 条，实得 %d", c)
	}
}

// TestRevokeStreamKey_CascadeOrderMatchesStreamIDBecauseOfSQL 钉住「顺序是 SQL 给的」：
// 字典序与插入序不同（先种 Z 再种 A），回带必须是 A 在前。
// 若哪天有人按 map 迭代序或按主键序回带，这条会红。
func TestRevokeStreamKey_CascadeOrderMatchesStreamIDBecauseOfSQL(t *testing.T) {
	e := newTestEnv(t)
	fx := e.seedRevokeFixture(t, "live_order_key2", 3)
	e.attachStreamToKey(t, fx.key.KeyID, "S-ZZZ", model.StreamStatePublishing, 1, false)
	e.attachStreamToKey(t, fx.key.KeyID, "S-AAA", model.StreamStatePublishing, 1, false)
	e.attachStreamToKey(t, fx.key.KeyID, "S-MMM", model.StreamStatePublishing, 1, false)
	reply := e.mustRevokeKey(t, fx.key.KeyID, "r-order2", func(in *rpc.RevokeStreamKeyReq) {
		in.StopStream = true
	})
	got := reply.GetStoppedStreamIds()
	if len(got) != 3 || got[0] != "S-AAA" || got[1] != "S-MMM" || got[2] != "S-ZZZ" {
		t.Fatalf("级联顺序必须是 stream_id 升序，实得 %v", got)
	}
}

// --- 失败注入：级联中途失败时「密钥还能用」必须是真的 ---

// TestRevokeStreamKey_CascadeFailureRollsBackKeyAndStreams 在级联的第二个流上注入事件写失败：
// 必须整体回滚——密钥回到 ACTIVE 且 reason 不变，第一条被停的流也要退回原状态。
func TestRevokeStreamKey_CascadeFailureRollsBackKeyAndStreams(t *testing.T) {
	e := newTestEnv(t)
	fx := e.seedRevokeFixture(t, "live_rollback_key", 3)
	first := e.attachStreamToKey(t, fx.key.KeyID, "S-RB-A", model.StreamStatePublishing, 5, true)
	e.attachStreamToKey(t, fx.key.KeyID, "S-RB-B", model.StreamStatePublishing, 7, true)
	quotaBefore := e.nodeRow(t, "node-rev").ActiveStreams
	before, mark := e.effects(), e.markCalls()

	// 第 2 条流（seq=7 → 事件 seq=8）写入失败；第 1 条已经写完，靠回滚兜住。
	e.repo.StreamEvent = &secondInsertFails{fakeStreamEvent: &fakeStreamEvent{db: e.db}, failOnSeq: 8}
	_, err := e.revokeKey(t, fx.key.KeyID, "r-rb", func(in *rpc.RevokeStreamKeyReq) {
		in.StopStream = true
		in.Reason = "泄露"
	})
	if err == nil {
		t.Fatalf("级联中途写事件失败时吊销必须整体失败")
	}
	got := e.callsFrom(mark)
	if len(got) < 5 || got[3] != "StreamKey.TransitionState" {
		t.Fatalf("触库形状不符：%v", got)
	}
	// 密钥行退回原样（state 与 reason 都在同一事务里）。
	row := e.keyRow(t, fx.key.KeyID)
	if row.State != model.KeyStateActive || row.Reason != fx.key.Reason {
		t.Fatalf("回滚不完整：密钥 state=%d reason=%q（种子 reason=%q）", row.State, row.Reason, fx.key.Reason)
	}
	for id, wantSeq := range map[string]int64{"S-RB-A": 5, "S-RB-B": 7} {
		if s := e.streamRow(t, id); s.State != model.StreamStatePublishing || s.Seq != wantSeq {
			t.Fatalf("%s 回滚不完整：state=%d seq=%d want_seq=%d", id, s.State, s.Seq, wantSeq)
		}
	}
	first = e.streamRow(t, first.StreamID)
	if s := e.streamRow(t, first.StreamID); s.StopReason != 0 || s.StopDetail != "" {
		t.Fatalf("回滚不完整：第一条流带上了停流痕迹 stop_reason=%d stop_detail=%q", s.StopReason, s.StopDetail)
	}
	if lease := e.assignmentRow(t, "assign-S-RB-A"); lease.State != model.AssignmentStateActive {
		t.Fatalf("回滚不完整：租约被单独释放 %+v", lease)
	}
	if got := e.nodeRow(t, "node-rev").ActiveStreams; got != quotaBefore {
		t.Fatalf("回滚不完整：配额被单独归还 active=%d want=%d", got, quotaBefore)
	}
	if got := e.keyRow(t, fx.key.KeyID).CurrentStreamID; got != "S-RB-A" {
		t.Fatalf("回滚不完整：活跃指针被单独清空 %q", got)
	}
	e.requireSameEffects(t, before, "级联失败必须零残留")
	// 重放同一个请求（换掉失败的替身）必须能成功，且不会因为「上次写了 report_id」而撞唯一索引：
	// 级联事件的 report_id 一直是空。
	e.repo.StreamEvent = &fakeStreamEvent{db: e.db}
	again := e.mustRevokeKey(t, fx.key.KeyID, "r-rb", func(in *rpc.RevokeStreamKeyReq) {
		in.StopStream = true
		in.Reason = "泄露"
	})
	if len(again.GetStoppedStreamIds()) != 2 {
		t.Fatalf("回滚后重试应停掉两条流，实得 %v", again.GetStoppedStreamIds())
	}
}

// secondInsertFails 只让「seq 等于 failOnSeq」的事件写失败：
// 造的是「级联第二条流失败」而不是「第一条就失败」，这样才能证明回滚覆盖了前一条已完成的写。
type secondInsertFails struct {
	*fakeStreamEvent
	failOnSeq int64
}

func (f *secondInsertFails) Insert(_ context.Context, tx sqlx.Session, ev *model.StreamEvent) (int64, error) {
	f.db.bump("StreamEvent.Insert")
	if ev != nil && ev.Seq == f.failOnSeq {
		return 0, fmt.Errorf("live_stream_event 写入失败（注入）")
	}
	return f.fakeStreamEvent.Insert(bg(), tx, ev)
}

// TestRevokeStreamKey_TransitionCasLossRollsBackWithoutTouchingStreams 钉住
// 「抢不到吊销位就什么都不做」：级联停流排在吊销位之后（revokestreamkeylogic.go:80-92），
// 所以 CAS 失败时连流表都不该被扫。
func TestRevokeStreamKey_TransitionCasLossRollsBackWithoutTouchingStreams(t *testing.T) {
	e := newTestEnv(t)
	fx := e.seedRevokeFixture(t, "live_cas_key", 2)
	e.attachStreamToKey(t, fx.key.KeyID, "S-CAS-A", model.StreamStatePublishing, 1, true)
	// 替身的 TransitionState 只在状态不匹配时回 false：造一个并发把密钥改成 RETIRED 的现场。
	e.db.keys[fx.key.KeyID].State = model.KeyStateRetired
	before, mark := e.effects(), e.markCalls()

	_, err := e.revokeKey(t, fx.key.KeyID, "r-cas", func(in *rpc.RevokeStreamKeyReq) { in.StopStream = true })
	wantFail(t, err, model.ErrConcurrentUpdate, "吊销位被抢")
	e.requireCallsFrom(t, mark, append([]string(nil), revokeNoCascadeLog...), "CAS 失败后不得扫流")
	e.requireSameEffects(t, before, "CAS 失败")
	if row := e.streamRow(t, "S-CAS-A"); row.State != model.StreamStatePublishing {
		t.Fatalf("CAS 失败却改了流状态：%d", row.State)
	}
	if got := e.nodeRow(t, "node-rev").ActiveStreams; got != 1 {
		t.Fatalf("CAS 失败却退了配额：active=%d", got)
	}
}

// --- 网关与隐私 ---

// TestRevokeStreamKey_NeverTouchesIngestGateway 钉住「吊销不停厂商侧会话」这个事实：
// RevokeStreamKeyLogic 全程不引用 svcCtx.Gateway（revokestreamkeylogic.go 里没有任何调用点），
// 因此即使 stop_stream=true 把库里的流停干净了，也从未向接入节点下发过断开会话的指令。
// （厂商侧是 stub 属 README 缺口 1，这里登记的是「没有调用点」这件事本身：
// 接线之后仍然不会生效，除非 logic 里补上调用。）
func TestRevokeStreamKey_NeverTouchesIngestGateway(t *testing.T) {
	e := newTestEnv(t)
	fx := e.seedRevokeFixture(t, "live_gw_key", 2)
	e.attachStreamToKey(t, fx.key.KeyID, "S-GW-A", model.StreamStatePublishing, 1, true)
	e.attachStreamToKey(t, fx.key.KeyID, "S-GW-B", model.StreamStatePublishing, 1, true)

	reply := e.mustRevokeKey(t, fx.key.KeyID, "r-gw", func(in *rpc.RevokeStreamKeyReq) {
		in.StopStream = true
		in.Reason = "泄露"
	})
	if len(reply.GetStoppedStreamIds()) != 2 {
		t.Fatalf("级联应停两条流，实得 %v", reply.GetStoppedStreamIds())
	}
	if e.gw.kickCalls != 0 || e.gw.probeCall != 0 || e.gw.bindCall != 0 {
		t.Fatalf("吊销不该触达接入网关（当前实现确实不触达）：kick=%d probe=%d bind=%d",
			e.gw.kickCalls, e.gw.probeCall, e.gw.bindCall)
	}
}

// TestRevokeStreamKey_NeverLeaksKeyMaterial 覆盖吊销 + 级联整条路径的隐私纪律：
// 日志的任何一级（含结构化字段）都不得出现明文、哈希或 Vault 引用。
func TestRevokeStreamKey_NeverLeaksKeyMaterial(t *testing.T) {
	e := newTestEnv(t)
	fx := e.seedRevokeFixture(t, "live_leak_key", 2)
	e.attachStreamToKey(t, fx.key.KeyID, "S-LEAK-A", model.StreamStateInterrupted, 2, true)
	e.openWindow(t, "S-LEAK-A", 71, "node-rev", 15)
	logs := captureLogs(t)

	e.mustRevokeKey(t, fx.key.KeyID, "r-leak", func(in *rpc.RevokeStreamKeyReq) {
		in.StopStream = true
		in.Reason = "泄露"
	})
	all := logs.joined()
	for _, forbidden := range []string{fx.plain, fx.key.KeyHash, fx.key.KeyRef} {
		requireAbsent(t, all, forbidden, "吊销+级联")
	}
}
