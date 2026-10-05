// 本文件覆盖读侧「密钥」的两条入口：ListStreamKeys（主播侧/运营侧密钥列表）与
// GetStreamKey（按 key_id 或 stream_name 查密钥元数据）。
//
// 与 streamquery_test.go 同一套读侧口径（越权收口下推、分页夹取回显、零副作用），
// 另外多两条密钥特有的契约：
//   - 投影里不许出现任何密钥材料：StreamKeyInfo 根本没有 key_hash/明文位，
//     所以「没回显」不能靠字段名断言，只能把整份响应序列化出来逐字节找摘要；
//   - 非法 stream_name 与不存在的 stream_name 必须同样按「未找到」处理：
//     两者错误码不同就等于给调用方一台枚举机（getstreamkeylogic.go 的设计注释）。
//
// 排序断言口径：真 SQL 是 ORDER BY key_id DESC（key_id 自增），所以「铺种子的先后」
// 就是确定的倒序，不需要依赖替身的兜底排序。

package logic

import (
	"fmt"
	"strings"
	"testing"

	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"
)

func listStreamKeys(e *testEnv, mut ...func(*rpc.ListStreamKeysReq)) (*rpc.ListStreamKeysReply, error) {
	in := &rpc.ListStreamKeysReq{OperatorMid: 1001}
	for _, m := range mut {
		m(in)
	}
	return NewListStreamKeysLogic(bg(), e.svc).ListStreamKeys(in)
}

func getStreamKey(e *testEnv, mut ...func(*rpc.GetStreamKeyReq)) (*rpc.GetStreamKeyReply, error) {
	in := &rpc.GetStreamKeyReq{}
	for _, m := range mut {
		m(in)
	}
	return NewGetStreamKeyLogic(bg(), e.svc).GetStreamKey(in)
}

// seedKeyOf 铺一把密钥并回库里那一行。KeyHash / RequestID 按「名称+状态」派生而不是
// 沿用 seedKey 的默认（默认按 stream_name 派生）：轮转链上同名密钥是合法形态，
// 直接沿用会让第二把撞 uniq_key_hash。
func (e *testEnv) seedKeyOf(t *testing.T, name string, room, anchor int64, state int32) *model.StreamKey {
	t.Helper()
	tag := fmt.Sprintf("seed-list-%s-%d", name, state)
	return e.seedKey(t, &model.StreamKey{
		StreamName: name, RoomID: room, AnchorMid: anchor, State: state,
		Version: 1, ProtocolMask: model.ProtocolMaskRtmp, ExpireAt: 1800,
		KeyHash: sha256Hex(tag), RequestID: tag,
	})
}

// keyFindOneCalls 采样 StreamKey.FindOne 的调用次数。seedKey 自己就走真 FindOne，
// 所以「走没走某条 SQL」只能按增量判定，绝对值等于把种子的调用算到被测代码头上。
func keyFindOneCalls(e *testEnv) int { return e.db.call("StreamKey.FindOne") }

func idsOfKeys(rows []*rpc.StreamKeyInfo) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.GetKeyId())
	}
	return out
}

func requireKeyIDs(t *testing.T, rows []*rpc.StreamKeyInfo, want []int64, label string) {
	t.Helper()
	got := idsOfKeys(rows)
	if len(got) != len(want) {
		t.Fatalf("%s：条数不符\n  got =%v\n want=%v", label, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s：第 %d 条不符\n  got =%v\n want=%v", label, i, got, want)
		}
	}
}

// --- 不变量 2 的读侧半边：越权收口下推到 SQL 条件 ---

func TestListStreamKeys_NonAdminAskingOtherRoomsGetsOnlyItsOwn(t *testing.T) {
	e := newTestEnv(t)
	mineA := e.seedKeyOf(t, "k.mine.a", 11, 1001, model.KeyStateActive)
	mineB := e.seedKeyOf(t, "k.mine.b", 12, 1001, model.KeyStateActive)
	theirs := e.seedKeyOf(t, "k.theirs", 21, 2002, model.KeyStateActive)

	// TODO(缺陷 #5)：非 admin 分支把 filter.RoomID 归零（只按 anchor 收敛），
	// 于是「按房间筛」的请求被丢掉条件、回全量自己的密钥；ListStreams 则是
	// room_ids AND anchor_mid 收敛（越权时得到空集）。两个列表入口的同一维度语义不同。
	// 这里按现状钉住：安全性（拿不到别人的）成立，但 room_id 对非 admin 无效。
	for _, room := range []int64{theirs.RoomID, mineA.RoomID, mineB.RoomID} {
		reply, err := listStreamKeys(e, func(r *rpc.ListStreamKeysReq) { r.RoomId = room })
		wantOK(t, reply, err, fmt.Sprintf("非 admin 按房间 %d 查", room))
		// 无论问哪个房间（含别人的），回的都是且仅是自己的全部密钥。
		requireKeyIDs(t, reply.GetKeys(), []int64{mineB.KeyID, mineA.KeyID},
			fmt.Sprintf("非 admin 问房间 %d：不泄露他人，也不按房间收窄", room))
		if reply.GetTotal() != 2 {
			t.Fatalf("非 admin 问房间 %d 的 total=%d want=2", room, reply.GetTotal())
		}
	}
}

func TestListStreamKeys_NonAdminCannotTargetAnotherAnchor(t *testing.T) {
	e := newTestEnv(t)
	mine := e.seedKeyOf(t, "k.mine", 11, 1001, model.KeyStateActive)
	theirs := e.seedKeyOf(t, "k.theirs", 21, 2002, model.KeyStateActive)

	// 显式把 anchor_mid 填成别人：非 admin 分支会丢掉它，改用 operator_mid。
	reply, err := listStreamKeys(e, func(r *rpc.ListStreamKeysReq) { r.AnchorMid = theirs.AnchorMid })
	wantOK(t, reply, err, "非 admin 指名别人的 anchor_mid")
	requireKeyIDs(t, reply.GetKeys(), []int64{mine.KeyID}, "非 admin 的请求被收敛回自己")
	if reply.GetTotal() != 1 {
		t.Fatalf("total=%d want=1", reply.GetTotal())
	}
}

func TestListStreamKeys_AdminUsesTheRequestedScopeVerbatim(t *testing.T) {
	e := newTestEnv(t)
	a1 := e.seedKeyOf(t, "k.a1", 11, 1001, model.KeyStateActive)
	a2 := e.seedKeyOf(t, "k.a2", 12, 1001, model.KeyStateActive)
	b1 := e.seedKeyOf(t, "k.b1", 21, 2002, model.KeyStateActive)

	t.Run("按主播", func(t *testing.T) {
		reply, err := listStreamKeys(e, func(r *rpc.ListStreamKeysReq) { r.Admin, r.AnchorMid = true, 2002 })
		wantOK(t, reply, err, "admin 按 anchor_mid")
		requireKeyIDs(t, reply.GetKeys(), []int64{b1.KeyID}, "admin 只看 2002 的密钥")
	})
	t.Run("按房间", func(t *testing.T) {
		reply, err := listStreamKeys(e, func(r *rpc.ListStreamKeysReq) { r.Admin, r.RoomId = true, 11 })
		wantOK(t, reply, err, "admin 按 room_id")
		requireKeyIDs(t, reply.GetKeys(), []int64{a1.KeyID}, "admin 只看房间 11")
	})
	t.Run("不限范围", func(t *testing.T) {
		reply, err := listStreamKeys(e, func(r *rpc.ListStreamKeysReq) { r.Admin = true })
		wantOK(t, reply, err, "admin 全量")
		// key_id DESC：最后铺的那把在最前。
		requireKeyIDs(t, reply.GetKeys(), []int64{b1.KeyID, a2.KeyID, a1.KeyID}, "admin 全量按 key_id 倒序")
		if reply.GetTotal() != 3 {
			t.Fatalf("total=%d want=3", reply.GetTotal())
		}
	})
}

// --- 状态维度：UNSPECIFIED 不限制（含终态），其余逐格 ---

func TestListStreamKeys_StateFilterSelectsExactlyThatState(t *testing.T) {
	e := newTestEnv(t)
	rpcs := []rpc.StreamKeyState{
		rpc.StreamKeyState_STREAM_KEY_STATE_ACTIVE,
		rpc.StreamKeyState_STREAM_KEY_STATE_ROTATING,
		rpc.StreamKeyState_STREAM_KEY_STATE_RETIRED,
		rpc.StreamKeyState_STREAM_KEY_STATE_EXPIRED,
		rpc.StreamKeyState_STREAM_KEY_STATE_REVOKED,
	}
	states := []int32{model.KeyStateActive, model.KeyStateRotating, model.KeyStateRetired,
		model.KeyStateExpired, model.KeyStateRevoked}
	ids := make([]int64, 0, len(states))
	for i, st := range states {
		ids = append(ids, e.seedKeyOf(t, fmt.Sprintf("k.st%d", i), 31, 1001, st).KeyID)
	}

	all, err := listStreamKeys(e)
	wantOK(t, all, err, "不指定状态")
	// UNSPECIFIED 不限制：五种状态都要在（运营面要能翻到吊销记录）。
	requireKeyIDs(t, all.GetKeys(), []int64{ids[4], ids[3], ids[2], ids[1], ids[0]}, "UNSPECIFIED 不限制状态")

	for i, state := range rpcs {
		state, wantID := state, ids[i]
		reply, err := listStreamKeys(e, func(r *rpc.ListStreamKeysReq) { r.State = state })
		wantOK(t, reply, err, fmt.Sprintf("只查 %v", state))
		requireKeyIDs(t, reply.GetKeys(), []int64{wantID}, fmt.Sprintf("状态 %v 单命中", state))
		if reply.GetTotal() != 1 {
			t.Fatalf("状态 %v 的 total=%d want=1", state, reply.GetTotal())
		}
		if reply.GetKeys()[0].GetState() != state {
			t.Fatalf("状态 %v 回显成了 %v", state, reply.GetKeys()[0].GetState())
		}
	}
}

// --- 分页 ---

func TestListStreamKeys_PnPsAreClampedAndEchoedBack(t *testing.T) {
	e := newTestEnv(t)
	for i := 0; i < 3; i++ {
		e.seedKeyOf(t, fmt.Sprintf("k.pg%d", i), 41, 1001, model.KeyStateActive)
	}

	for _, tc := range []struct {
		name   string
		pn, ps int32
		wantPn int32
		wantPs int32
	}{
		{"省略分页参数", 0, 0, 1, defaultListPageSize},
		{"负页大小", 1, -1, 1, defaultListPageSize},
		{"页码 0", 0, 10, 1, 10},
		{"负页码", -3, 10, 1, 10},
		{"页大小超上限", 1, 51, 1, 50},
		{"巨大页大小同样夹到上限", 1, 99999, 1, 50},
	} {
		reply, err := listStreamKeys(e, func(r *rpc.ListStreamKeysReq) { r.Pn, r.Ps = tc.pn, tc.ps })
		wantOK(t, reply, err, tc.name)
		if reply.GetPn() != tc.wantPn || reply.GetPs() != tc.wantPs {
			t.Fatalf("%s：回显不符 pn=%d/%d ps=%d/%d", tc.name, reply.GetPn(), tc.wantPn, reply.GetPs(), tc.wantPs)
		}
	}
}

func TestListStreamKeys_TotalIsStableAcrossPagesAndOffsetSlices(t *testing.T) {
	e := newTestEnv(t)
	ids := make([]int64, 0, 5)
	for i := 0; i < 5; i++ {
		ids = append(ids, e.seedKeyOf(t, fmt.Sprintf("k-page-%d", i), 51, 1001, model.KeyStateActive).KeyID)
	}
	// key_id 自增，DESC 之后的期望序列就是铺种子的倒序。
	desc := []int64{ids[4], ids[3], ids[2], ids[1], ids[0]}

	first, err := listStreamKeys(e, func(r *rpc.ListStreamKeysReq) { r.Pn, r.Ps = 1, 2 })
	wantOK(t, first, err, "第 1 页")
	requireKeyIDs(t, first.GetKeys(), desc[0:2], "第 1 页（ps=2）")

	second, err := listStreamKeys(e, func(r *rpc.ListStreamKeysReq) { r.Pn, r.Ps = 2, 2 })
	wantOK(t, second, err, "第 2 页")
	requireKeyIDs(t, second.GetKeys(), desc[2:4], "第 2 页跳过前 2 条")

	tail, err := listStreamKeys(e, func(r *rpc.ListStreamKeysReq) { r.Pn, r.Ps = 3, 2 })
	wantOK(t, tail, err, "第 3 页（末页不满一页）")
	requireKeyIDs(t, tail.GetKeys(), desc[4:5], "第 3 页只剩第 5 条")

	beyond, err := listStreamKeys(e, func(r *rpc.ListStreamKeysReq) { r.Pn, r.Ps = 4, 2 })
	wantOK(t, beyond, err, "越过末页")
	requireKeyIDs(t, beyond.GetKeys(), nil, "越过末页返回空页")

	// total 必须是全量命中数：每页都一样，且不随 offset 掉下去。
	// 替身此前按「本页条数」数 total，正是这条断言当时测不出问题。
	for _, reply := range []*rpc.ListStreamKeysReply{first, second, tail, beyond} {
		if reply.GetTotal() != 5 {
			t.Fatalf("total 在某一页变成了 %d（应恒为全量命中数 5）", reply.GetTotal())
		}
	}
}

// --- 入参校验 ---

func TestListStreamKeys_InputValidationIsZeroSideEffect(t *testing.T) {
	e := newTestEnv(t)
	e.seedKeyOf(t, "k-1", 61, 1001, model.KeyStateActive)
	before := e.effects()

	cases := []struct {
		name   string
		mut    func(*rpc.ListStreamKeysReq)
		target error
	}{
		{"缺 operator", func(r *rpc.ListStreamKeysReq) { r.OperatorMid = 0 }, model.ErrOperatorRequired},
		{"负 operator", func(r *rpc.ListStreamKeysReq) { r.OperatorMid = -2 }, model.ErrOperatorRequired},
		{"负 room_id", func(r *rpc.ListStreamKeysReq) { r.RoomId = -1 }, model.ErrInvalidRoomId},
		// 负 anchor_mid 也报 ErrInvalidRoomId：两个不同的坏参数共用一个哨兵，
		// 见 README 缺陷 #3（这里只钉现状，不放宽也不改生产代码）。
		{"负 anchor_mid", func(r *rpc.ListStreamKeysReq) { r.AnchorMid = -1 }, model.ErrInvalidRoomId},
		// TODO(缺陷 #3)：非法状态枚举是「入参不合法」，却回 ErrStreamKeyNotUsable
		// （一把密钥的领域态）。调用方据此会把「自己传错了」理解成「密钥不可用」。
		{"非法状态枚举", func(r *rpc.ListStreamKeysReq) { r.State = rpc.StreamKeyState(9) }, model.ErrStreamKeyNotUsable},
	}
	for _, tc := range cases {
		mut := tc.mut
		t.Run(tc.name, func(t *testing.T) {
			_, err := listStreamKeys(e, mut)
			wantFail(t, err, tc.target, tc.name)
			e.requireSameEffects(t, before, tc.name+" 必须零写入")
			e.requireNoTransaction(t, before, tc.name+" 不该开事务")
		})
	}
	// room_id=0 / anchor_mid=0 是合法的「不限制」，不许被当成非法值拒掉。
	for _, tc := range []struct {
		name string
		mut  func(*rpc.ListStreamKeysReq)
	}{
		{"room_id 为 0 表示不限制", func(r *rpc.ListStreamKeysReq) { r.RoomId = 0 }},
		{"anchor_mid 为 0 表示不限制", func(r *rpc.ListStreamKeysReq) { r.AnchorMid = 0 }},
	} {
		mut := tc.mut
		reply, err := listStreamKeys(e, mut)
		wantOK(t, reply, err, tc.name)
		if reply.GetTotal() != 1 {
			t.Fatalf("%s：命中 %d 条 want=1", tc.name, reply.GetTotal())
		}
	}
}

func TestListStreamKeys_ProjectionCarriesNoKeyMaterial(t *testing.T) {
	e := newTestEnv(t)
	row := e.seedKeyOf(t, "k-priv", 71, 1001, model.KeyStateActive)
	before := e.effects()

	reply, err := listStreamKeys(e, func(r *rpc.ListStreamKeysReq) { r.Admin = true })
	wantOK(t, reply, err, "运营侧密钥列表")
	e.requireSameEffects(t, before, "读侧密钥列表不得改任何表")
	e.requireNoTransaction(t, before, "读侧密钥列表不该开事务")
	if n := e.db.call("StreamKey.Insert"); n != 1 {
		t.Fatalf("读路径又写了密钥表：Insert 累计 %d 次（只有种子那 1 次）", n)
	}
	if n := e.db.call("StreamKey.TransitionState"); n != 0 {
		t.Fatalf("读路径改了密钥状态：TransitionState %d 次", n)
	}

	// 摘要与 Vault 引用是两回事：引用可下发（运营面靠它定位材料），摘要一个字节都不许出现。
	text := reply.String()
	if strings.Contains(text, row.KeyHash) {
		t.Fatalf("响应里出现了 key_hash 摘要")
	}
	if !strings.Contains(text, "vault:secret/data/live-ingest/stream-key/k-priv#v1") {
		t.Fatalf("响应丢了 key_ref 引用，运营面无法定位密钥材料：\n%s", text)
	}
}

func TestListStreamKeys_RejectsMissingRepository(t *testing.T) {
	svcCtx := withoutRepoEnv(t)
	_, err := NewListStreamKeysLogic(bg(), svcCtx).ListStreamKeys(&rpc.ListStreamKeysReq{OperatorMid: 1001})
	wantFail(t, err, errNoRepository, "未装配 Repository 的密钥列表")
}

// --- GetStreamKey ---

func TestGetStreamKey_ByKeyIdProjectsMetadataOnly(t *testing.T) {
	logs := captureLogs(t)
	e := newTestEnv(t)
	row := e.seedKey(t, &model.StreamKey{
		StreamName: "k.full", RoomID: 81, SessionID: 82, AnchorMid: 1001,
		State: model.KeyStateRotating, Version: 3, PrevKeyID: 7, RotateToKeyID: 9,
		CurrentStreamID: "S-CUR", MaxStreams: 2, KeyTail: "ab12",
		ProtocolMask: model.ProtocolMaskRtmp | model.ProtocolMaskSrt,
		ExpireAt:     1900, GraceUntil: 2000, Reason: "轮转宽限",
		RequestID: "req-full", TraceID: "tr-full", Ctime: 1500,
	})
	before := e.effects()

	reply, err := getStreamKey(e, func(r *rpc.GetStreamKeyReq) { r.KeyId = row.KeyID })
	wantOK(t, reply, err, "按 key_id 查元数据")
	k := reply.GetKey()
	if k == nil {
		t.Fatalf("查到了却不带 key 投影")
	}
	for _, tc := range []struct {
		name string
		got  int64
		want int64
	}{
		{"key_id", k.GetKeyId(), row.KeyID},
		{"room_id", k.GetRoomId(), 81},
		{"session_id", k.GetSessionId(), 82},
		{"anchor_mid", k.GetAnchorMid(), 1001},
		{"version", int64(k.GetVersion()), 3},
		{"prev_key_id", k.GetPrevKeyId(), 7},
		{"rotate_to_key_id", k.GetRotateToKeyId(), 9},
		{"expire_at", k.GetExpireAt(), 1900},
		{"grace_until", k.GetGraceUntil(), 2000},
		{"ctime", k.GetCtime(), 1500},
		{"mtime", k.GetMtime(), 1500},
	} {
		if tc.got != tc.want {
			t.Fatalf("投影字段 %s 不符：got=%d want=%d", tc.name, tc.got, tc.want)
		}
	}
	if k.GetStreamName() != "k.full" || k.GetCurrentStreamId() != "S-CUR" || k.GetReason() != "轮转宽限" {
		t.Fatalf("文本类投影不符：%s/%s/%s", k.GetStreamName(), k.GetCurrentStreamId(), k.GetReason())
	}
	if k.GetState() != rpc.StreamKeyState_STREAM_KEY_STATE_ROTATING {
		t.Fatalf("state 投影成 %v", k.GetState())
	}
	// 位图要还原成枚举列表，且顺序与 maskToProtocols 的固定展开一致。
	if got := k.GetProtocols(); len(got) != 2 ||
		got[0] != rpc.IngestProtocol_PROTOCOL_RTMP || got[1] != rpc.IngestProtocol_PROTOCOL_SRT {
		t.Fatalf("protocol_mask=%d 投影成 %v", row.ProtocolMask, got)
	}
	if !strings.HasPrefix(k.GetKeyRef(), "vault:secret/data/live-ingest/stream-key/") ||
		!strings.Contains(k.GetKeyRef(), "#v3") {
		t.Fatalf("key_ref 不是带代次的 Vault 引用形态：%q", k.GetKeyRef())
	}
	if k.GetKeyHintTail() != "ab12" {
		t.Fatalf("key_hint_tail 投影成了 %q（种子是 4 位辨认串 ab12）", k.GetKeyHintTail())
	}
	if len(k.GetKeyHintTail()) > keyTailChars {
		t.Fatalf("key_hint_tail 超过辨认位长度上限 %d：%q", keyTailChars, k.GetKeyHintTail())
	}

	e.requireSameEffects(t, before, "按 key_id 查元数据必须零写入")
	e.requireNoTransaction(t, before, "按 key_id 查元数据不该开事务")
	// 摘要一个字节都不许进响应/日志：字段表里没有 key_hash 只说明「没回显」，
	// 逐字节找摘要才挡得住「以后加一列并填上」。
	requireAbsent(t, logs.joined(), row.KeyHash, "密钥摘要进日志")
	if hash := row.KeyHash; hash != "" && strings.Contains(reply.String(), hash) {
		t.Fatalf("响应里出现了 key_hash 摘要")
	}
}

func TestGetStreamKey_UnknownKeyIdIsNotFoundNotInvalid(t *testing.T) {
	e := newTestEnv(t)
	e.seedKeyOf(t, "k-exist", 91, 1001, model.KeyStateActive)
	findOne := keyFindOneCalls(e)

	// 「参数不合法」与「查不到」必须是两个哨兵：把后者报成前者会让调用方以为
	// 自己的入参有问题，从而放弃一条本该走「已删除/未同步」分支的排查路径。
	_, err := getStreamKey(e, func(r *rpc.GetStreamKeyReq) { r.KeyId = 999999 })
	wantFail(t, err, model.ErrStreamKeyNotFound, "查不存在的 key_id")
	if got := keyFindOneCalls(e) - findOne; got != 1 {
		t.Fatalf("按 key_id 的查询走了 %d 次 FindOne（应恰好 1 次）", got)
	}
}

func TestGetStreamKey_ByStreamNameOnlyMatchesUsableKeys(t *testing.T) {
	e := newTestEnv(t)
	// 真 SQL：WHERE stream_name=? AND state IN (ACTIVE, ROTATING) ORDER BY key_id DESC LIMIT 1。
	// 铺成「ACTIVE 较旧、ROTATING 次新、RETIRED 最新」：
	//   - 只按 key_id DESC 而不筛状态 → 会命中 RETIRED；
	//   - 只筛状态而不按 key_id DESC → 会拿到更旧的 ACTIVE。
	// 两个条件都必须成立才回 ROTATING 这一把。
	e.seedKeyOf(t, "k-pick", 101, 1001, model.KeyStateActive)
	rotating := e.seedKeyOf(t, "k-pick", 101, 1001, model.KeyStateRotating)
	e.seedKeyOf(t, "k-pick", 101, 1001, model.KeyStateRetired)
	findOne := keyFindOneCalls(e)

	reply, err := getStreamKey(e, func(r *rpc.GetStreamKeyReq) { r.StreamName = "k-pick" })
	wantOK(t, reply, err, "按 stream_name 查当前密钥")
	if reply.GetKey().GetKeyId() != rotating.KeyID {
		t.Fatalf("命中的是 key %d，应为可用的最新一把 %d", reply.GetKey().GetKeyId(), rotating.KeyID)
	}
	if reply.GetKey().GetState() != rpc.StreamKeyState_STREAM_KEY_STATE_ROTATING {
		t.Fatalf("回显状态成了 %v", reply.GetKey().GetState())
	}
	// 名称查询不许顺手走 key_id 路径。
	if got := keyFindOneCalls(e) - findOne; got != 0 {
		t.Fatalf("按名称查询却走了 FindOne：%d 次", got)
	}
}

func TestGetStreamKey_OnlyUnusableKeysUnderNameYieldsNotFound(t *testing.T) {
	e := newTestEnv(t)
	revoked := e.seedKeyOf(t, "k-dead", 111, 1001, model.KeyStateRevoked)
	e.seedKeyOf(t, "k-dead", 111, 1001, model.KeyStateExpired)
	before := e.effects()

	_, err := getStreamKey(e, func(r *rpc.GetStreamKeyReq) { r.StreamName = "k-dead" })
	wantFail(t, err, model.ErrStreamKeyNotFound, "名下只剩不可用密钥时按未找到处理")

	// 但按 key_id 仍能查到：审计与对账要看吊销记录，「不可用」不等于「不存在」。
	byID, err := getStreamKey(e, func(r *rpc.GetStreamKeyReq) { r.KeyId = revoked.KeyID })
	wantOK(t, byID, err, "按 key_id 查已吊销的密钥")
	if byID.GetKey().GetState() != rpc.StreamKeyState_STREAM_KEY_STATE_REVOKED {
		t.Fatalf("已吊销密钥按 ID 查回来是 %v", byID.GetKey().GetState())
	}
	e.requireSameEffects(t, before, "名称未命中不得改密钥表")
}

func TestGetStreamKey_KeyRevokedByNameLookupStopsResolving(t *testing.T) {
	e := newTestEnv(t)
	// 走真写路径把一把 ACTIVE 密钥吊销，再看名称查询是否立刻停发：
	// 「吊销生效」这条不变量的读侧半边就落在这一格。
	first := e.seedKeyOf(t, "k-revoked", 121, 1001, model.KeyStateActive)
	ok, err := e.repo.StreamKey.TransitionState(bg(), nil, first.KeyID,
		model.KeyStateRevoked, "风控吊销", model.KeyStateActive)
	wantOK(t, ok, err, "把种子密钥迁移到 REVOKED")
	mustTrue(t, ok, "种子迁移没生效，这条用例等于什么都没测")

	_, err = getStreamKey(e, func(r *rpc.GetStreamKeyReq) { r.StreamName = "k-revoked" })
	wantFail(t, err, model.ErrStreamKeyNotFound, "吊销后按名称查不到当前密钥")
}

func TestGetStreamKey_IllegalStreamNameIsReportedAsNotFound(t *testing.T) {
	e := newTestEnv(t)
	e.seedKeyOf(t, "k-ok", 131, 1001, model.KeyStateActive)
	before := e.effects()
	findOne := keyFindOneCalls(e)

	long := "k" + strings.Repeat("x", maxStreamNameBytes)
	for _, tc := range []struct {
		name string
		val  string
	}{
		{"含斜杠", "a/b"},
		{"含空格", "a b"},
		{"含问号", "a?b"},
		{"含等号", "a=b"},
		{"含 &", "a&b"},
		{"含制表符", "a\tb"},
		{"含换行", "a\nb"},
		{"纯空白", "   "},
		{"超出列宽", long},
	} {
		val := tc.val
		_, err := getStreamKey(e, func(r *rpc.GetStreamKeyReq) { r.StreamName = val })
		// 非法名与不存在的名同一个哨兵：不同就是枚举机。
		wantFail(t, err, model.ErrStreamKeyNotFound, tc.name+" 按未找到处理")
	}
	e.requireSameEffects(t, before, "非法名称探测必须零写入")
	e.requireNoTransaction(t, before, "非法名称探测不该开事务")
	if got := keyFindOneCalls(e) - findOne; got != 0 {
		t.Fatalf("非法名称却走了 key_id 路径：FindOne 多调 %d 次", got)
	}
}

func TestGetStreamKey_KeyIdSelectorWinsOverStreamName(t *testing.T) {
	e := newTestEnv(t)
	target := e.seedKeyOf(t, "k-target", 141, 1001, model.KeyStateActive)
	e.seedKeyOf(t, "k-other", 142, 1001, model.KeyStateActive)
	findOne := keyFindOneCalls(e)

	reply, err := getStreamKey(e, func(r *rpc.GetStreamKeyReq) {
		r.KeyId = target.KeyID
		r.StreamName = "k-other"
	})
	wantOK(t, reply, err, "两个选择子同时给")
	if reply.GetKey().GetKeyId() != target.KeyID {
		t.Fatalf("以 key_id 为准却查回 %d", reply.GetKey().GetKeyId())
	}
	if got := keyFindOneCalls(e) - findOne; got != 1 {
		t.Fatalf("FindOne 多走了 %d 次（名称被忽略时应恰好 1 次）", got)
	}
}

func TestGetStreamKey_NoUsableSelectorIsInvalidKeyId(t *testing.T) {
	e := newTestEnv(t)
	e.seedKeyOf(t, "k-any", 151, 1001, model.KeyStateActive)
	before := e.effects()

	for _, tc := range []struct {
		name string
		mut  func(*rpc.GetStreamKeyReq)
	}{
		{"两个选择子都空", func(r *rpc.GetStreamKeyReq) {}},
		{"key_id 为负且无名称", func(r *rpc.GetStreamKeyReq) { r.KeyId = -5 }},
	} {
		mut := tc.mut
		_, err := getStreamKey(e, mut)
		wantFail(t, err, model.ErrInvalidKeyId, tc.name)
	}
	e.requireSameEffects(t, before, "选择子非法必须零写入")
	// 空白名称走的是「名称非法」那一格，哨兵是未找到而不是无效 ID：
	// 两者的边界钉住，日后顺手合并会让枚举防护失效。
	_, err := getStreamKey(e, func(r *rpc.GetStreamKeyReq) { r.StreamName = "  " })
	wantFail(t, err, model.ErrStreamKeyNotFound, "空白名称按未找到处理")
}

// TODO(缺陷 #4)：GetStreamKey / GetStreamState 的入参里没有 operator，
// 因此单查路径没有任何越权收口：任何内部调用方按 key_id 就能读到别人名下的元数据
// （room_id、anchor_mid、current_stream_id）。列表侧有收口、单查侧没有。
// 这一条按现状钉住，缺口登记在 README「已知缺口」，新增 operator 字段时必须同时补判。
func TestGetStreamKey_CurrentBehaviorHasNoOperatorScope(t *testing.T) {
	e := newTestEnv(t)
	theirs := e.seedKeyOf(t, "k-victim", 161, 2002, model.KeyStateActive)

	reply, err := getStreamKey(e, func(r *rpc.GetStreamKeyReq) { r.KeyId = theirs.KeyID })
	wantOK(t, reply, err, "无 operator 的单查")
	if reply.GetKey().GetAnchorMid() != 2002 || reply.GetKey().GetRoomId() != 161 {
		t.Fatalf("现状断言破了：查回来的归属是 %d/%d", reply.GetKey().GetAnchorMid(), reply.GetKey().GetRoomId())
	}
	// 契约里确实没有可用来收口的主体字段（有就说明缺陷已修，本登记要重评）。
	if (&rpc.GetStreamKeyReq{}).ProtoReflect().Descriptor().Fields().ByName("operator_mid") != nil {
		t.Fatalf("GetStreamKeyReq 已带上 operator_mid，缺陷 #4 的登记需要重新评估")
	}
}

func TestGetStreamKey_RejectsMissingRepository(t *testing.T) {
	svcCtx := withoutRepoEnv(t)
	_, err := NewGetStreamKeyLogic(bg(), svcCtx).GetStreamKey(&rpc.GetStreamKeyReq{KeyId: 1})
	wantFail(t, err, errNoRepository, "未装配 Repository 的密钥单查")
}
