// 本文件覆盖不变量 2（推流密钥：明文只回显一次、库里只有摘要与引用、轮转共存窗口、
// 吊销后鉴权失败、重放不再发第二把密钥）与不变量 8 的「密钥不落日志」部分。

package logic

import (
	"fmt"
	"strings"
	"testing"

	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"
)

// issueAndAuth 签发一把密钥并回一份「可用来鉴权」的明文（签发后库里只剩摘要）。
func issueKey(t *testing.T, e *testEnv, roomID, anchor int64, requestID string) (*rpc.IssueStreamKeyReply, *model.StreamKey) {
	t.Helper()
	reply, err := NewIssueStreamKeyLogic(bg(), e.svc).IssueStreamKey(&rpc.IssueStreamKeyReq{
		RoomId: roomID, AnchorMid: anchor, SessionId: 0, RequestId: requestID,
	})
	wantOK(t, reply, err, "签发密钥")
	if reply.GetReplayed() {
		t.Fatalf("首次签发不该 replayed=true")
	}
	return reply, e.keyRow(t, reply.GetKeyId())
}

// --- 明文一次、库里只有摘要 ---

func TestIssueStreamKey_PlaintextAppearsOnceAndDbHoldsOnlyDigests(t *testing.T) {
	e := newTestEnv(t)
	before := e.effects()
	reply, row := issueKey(t, e, 101, 202, "issue-1")

	plaintext := reply.GetPlaintextKey()
	if len(plaintext) < minKeyRandomBytes {
		t.Fatalf("明文密钥强度不足：%d 字符（随机字节下限 %d）", len(plaintext), minKeyRandomBytes)
	}
	if reply.GetPublishUrl() == "" || !strings.Contains(reply.GetPublishUrl(), plaintext) {
		t.Fatalf("推流地址必须带上明文密钥：%q", reply.GetPublishUrl())
	}
	if row.KeyHash != sha256Hex(plaintext) {
		t.Fatalf("库里应存明文哈希：got=%s want=%s", row.KeyHash, sha256Hex(plaintext))
	}
	if len(row.KeyHash) != sha256HexLen {
		t.Fatalf("key_hash 应为 64 位 hex：%q", row.KeyHash)
	}
	if row.KeyTail != plaintext[len(plaintext)-keyTailChars:] {
		t.Fatalf("key_tail 应为明文末 %d 位：got=%q", keyTailChars, row.KeyTail)
	}
	if !strings.HasPrefix(row.KeyRef, "vault:secret/data/live-ingest/stream-key/") {
		t.Fatalf("key_ref 不是 Secret/Vault 引用形态：%q", row.KeyRef)
	}
	if !strings.Contains(row.KeyRef, "#v1") {
		t.Fatalf("首发密钥引用必须带代次 v1：%q", row.KeyRef)
	}
	// 明文一次都不入库：逐列扫（新增列时也照样有效）
	for name, value := range map[string]string{
		"key_hash": row.KeyHash, "key_ref": row.KeyRef, "key_tail": row.KeyTail,
		"stream_name": row.StreamName, "reason": row.Reason, "trace_id": row.TraceID,
		"request_id": row.RequestID,
	} {
		if strings.Contains(value, plaintext) {
			t.Fatalf("明文密钥出现在库列 %s 里", name)
		}
	}
	if row.State != model.KeyStateActive || row.Version != keyVersionFirst {
		t.Fatalf("首发密钥状态/代次不符：state=%d version=%d", row.State, row.Version)
	}
	// 签发是单表写：不该顺手建流、建租约或开事务
	e.requireDelta(t, before, [9]int{0: 1}, "签发的应有写入只有 1 行密钥")
}

func TestIssueStreamKey_ReplayYieldsNoSecondKey(t *testing.T) {
	e := newTestEnv(t)
	first, _ := issueKey(t, e, 102, 203, "issue-replay")
	before := e.effects()

	second, err := NewIssueStreamKeyLogic(bg(), e.svc).IssueStreamKey(&rpc.IssueStreamKeyReq{
		RoomId: 102, AnchorMid: 203, RequestId: "issue-replay",
	})
	wantOK(t, second, err, "重放签发")
	mustTrue(t, second.GetReplayed(), "重放标记")
	if second.GetPlaintextKey() != "" {
		t.Fatalf("重放回显了明文——明文只能出现一次")
	}
	if second.GetPublishUrl() != "" {
		t.Fatalf("重放回显了含明文的推流地址：%q", second.GetPublishUrl())
	}
	if second.GetKeyId() != first.GetKeyId() || second.GetStreamName() != first.GetStreamName() {
		t.Fatalf("重放未回放首次结果：key %d/%d", first.GetKeyId(), second.GetKeyId())
	}
	e.requireSameEffects(t, before, "重放签发必须零写入、零新事务")
	e.requireNoTransaction(t, before, "request_id 重放应在幂等预检处返回")
	if n := len(e.db.keys); n != 1 {
		t.Fatalf("重放发出了第二把密钥：库里 %d 行", n)
	}
}

func TestIssueStreamKey_InputValidationIsZeroSideEffect(t *testing.T) {
	e := newTestEnv(t)
	before := e.effects()

	cases := []struct {
		name   string
		in     *rpc.IssueStreamKeyReq
		target error
	}{
		{"缺 request_id", &rpc.IssueStreamKeyReq{RoomId: 1, AnchorMid: 2}, model.ErrIdempotencyKeyRequired},
		{"空白 request_id", &rpc.IssueStreamKeyReq{RoomId: 1, AnchorMid: 2, RequestId: "   "}, model.ErrIdempotencyKeyRequired},
		{"房间号非正", &rpc.IssueStreamKeyReq{RoomId: 0, AnchorMid: 2, RequestId: "r"}, model.ErrInvalidRoomId},
		{"主播号非正", &rpc.IssueStreamKeyReq{RoomId: 1, AnchorMid: 0, RequestId: "r"}, model.ErrInvalidMid},
		{"未知协议", &rpc.IssueStreamKeyReq{RoomId: 1, AnchorMid: 2, RequestId: "r",
			Protocols: []rpc.IngestProtocol{rpc.IngestProtocol(99)}}, model.ErrInvalidProtocol},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewIssueStreamKeyLogic(bg(), e.svc).IssueStreamKey(c.in)
			wantFail(t, err, c.target, c.name)
			e.requireSameEffects(t, before, c.name)
		})
	}
}

func TestIssueStreamKey_TtlIsClampedNotRejected(t *testing.T) {
	e := newTestEnv(t)
	maxTTL := e.svc.Config.LiveIngest.MaxIssueTtlSeconds

	reply, err := NewIssueStreamKeyLogic(bg(), e.svc).IssueStreamKey(&rpc.IssueStreamKeyReq{
		RoomId: 103, AnchorMid: 204, RequestId: "issue-ttl", TtlSeconds: maxTTL + 999999,
	})
	wantOK(t, reply, err, "超长 TTL 应被夹到上限而不是报错")
	if got := reply.GetExpireAt() - nowUnix(); got > maxTTL || got < maxTTL-5 {
		t.Fatalf("expire_at 未按上限夹取：剩 %d 秒，上限 %d 秒", got, maxTTL)
	}

	// 默认值：不给 ttl 用配置默认
	def, err := NewIssueStreamKeyLogic(bg(), e.svc).IssueStreamKey(&rpc.IssueStreamKeyReq{
		RoomId: 104, AnchorMid: 204, RequestId: "issue-ttl-default",
	})
	wantOK(t, def, err, "默认 TTL")
	if got := def.GetExpireAt() - nowUnix(); got > e.svc.Config.LiveIngest.IssueTtlSeconds || got < e.svc.Config.LiveIngest.IssueTtlSeconds-5 {
		t.Fatalf("未按 IssueTtlSeconds 取默认有效期：剩 %d 秒", got)
	}
}

func TestIssueStreamKey_NoPublishDomainFailsClosed(t *testing.T) {
	e := newTestEnv(t)
	e.svc.Config.Cdn.PublishDomains = nil
	before := e.effects()

	_, err := NewIssueStreamKeyLogic(bg(), e.svc).IssueStreamKey(&rpc.IssueStreamKeyReq{
		RoomId: 105, AnchorMid: 205, RequestId: "issue-nodomain",
	})
	wantFail(t, err, model.ErrCdnNotConfigured, "没有推流域名白名单必须拒签")
	e.requireSameEffects(t, before, "拒签必须零副作用（不能先落库再报错）")
}

// --- 轮转：新旧共存窗口按 proto 口径 ---

func TestRotateStreamKey_OldAndNewCoexistWithinGrace(t *testing.T) {
	e := newTestEnv(t)
	oldPlain := "OLDPLAINKEY-0123456789abcdef"
	key := e.seedKey(t, &model.StreamKey{
		StreamName: "live_106_a", RoomID: 106, AnchorMid: 206, ProtocolMask: model.ProtocolMaskRtmp,
		KeyHash: sha256Hex(oldPlain), State: model.KeyStateActive, Version: 1, MaxStreams: 1,
		ExpireAt: nowUnix() + 3600,
	})
	grace := e.svc.Config.LiveIngest.RotateGraceSeconds

	reply, err := NewRotateStreamKeyLogic(bg(), e.svc).RotateStreamKey(&rpc.RotateStreamKeyReq{
		KeyId: key.KeyID, OperatorMid: 206, RequestId: "rot-1",
	})
	wantOK(t, reply, err, "轮转")
	if reply.GetPlaintextKey() == "" || reply.GetPrevKeyId() != key.KeyID || reply.GetVersion() != 2 {
		t.Fatalf("轮转响应不符：%+v", reply)
	}
	if got := reply.GetGraceUntil() - nowUnix(); got > grace || got < grace-5 {
		t.Fatalf("宽限截止未按配置：剩 %d 秒，配置 %d 秒", got, grace)
	}

	rotated := e.keyRow(t, key.KeyID)
	if rotated.State != model.KeyStateRotating || rotated.RotateToKeyID != reply.GetKeyId() {
		t.Fatalf("旧密钥未进入 ROTATING 或未挂后继：state=%d to=%d", rotated.State, rotated.RotateToKeyID)
	}
	fresh := e.keyRow(t, reply.GetKeyId())
	if fresh.State != model.KeyStateActive || fresh.Version != 2 || fresh.PrevKeyID != key.KeyID {
		t.Fatalf("新密钥行不符：state=%d version=%d prev=%d", fresh.State, fresh.Version, fresh.PrevKeyID)
	}
	if fresh.StreamName != key.StreamName || fresh.RoomID != key.RoomID {
		t.Fatalf("轮转换了流标识/房间归属：%s/%s", fresh.StreamName, key.StreamName)
	}
	// 明文纪律：新行同样只有摘要
	if fresh.KeyHash == reply.GetPlaintextKey() || fresh.KeyHash != sha256Hex(reply.GetPlaintextKey()) {
		t.Fatalf("新密钥未按哈希入库")
	}

	// 共存窗口：新旧两把密钥在宽限期内都必须能过鉴权（OBS 不重启切换）
	for _, plain := range []string{oldPlain, reply.GetPlaintextKey()} {
		auth, err := e.verifyPublish(t, &rpc.VerifyPublishAuthReq{
			StreamName: key.StreamName, PlaintextKey: plain, RoomId: key.RoomID,
			Protocol: rpc.IngestProtocol_PROTOCOL_RTMP,
		})
		wantOK(t, auth, err, "宽限期内鉴权")
		if !auth.GetAllowed() {
			t.Fatalf("宽限期内密钥被拒：reason=%s", auth.GetReason())
		}
	}

	// 宽限期一过：旧密钥必须失效（RETIRED 由扫描器置，鉴权这边先拒）
	rotated = e.keyRow(t, key.KeyID)
	e.db.keys[key.KeyID].GraceUntil = nowUnix() - 1
	expired, err := e.verifyPublish(t, &rpc.VerifyPublishAuthReq{
		StreamName: key.StreamName, PlaintextKey: oldPlain, RoomId: key.RoomID,
		Protocol: rpc.IngestProtocol_PROTOCOL_RTMP,
	})
	wantOK(t, expired, err, "宽限期已过鉴权")
	mustFalse(t, expired.GetAllowed(), "宽限期已过的旧密钥")
	if expired.GetReason() != reasonKeyExpired {
		t.Fatalf("宽限期已过的原因码应为 key_expired，实得 %q", expired.GetReason())
	}
}

func TestRotateStreamKey_ReplayYieldsNoSecondKey(t *testing.T) {
	e := newTestEnv(t)
	key := e.seedKey(t, &model.StreamKey{
		StreamName: "live_107_a", RoomID: 107, AnchorMid: 207, ProtocolMask: model.ProtocolMaskRtmp,
		Version: 1, State: model.KeyStateActive,
	})
	first, err := NewRotateStreamKeyLogic(bg(), e.svc).RotateStreamKey(&rpc.RotateStreamKeyReq{
		KeyId: key.KeyID, OperatorMid: 207, RequestId: "rot-replay",
	})
	wantOK(t, first, err, "首次轮转")

	before := e.effects()
	second, err := NewRotateStreamKeyLogic(bg(), e.svc).RotateStreamKey(&rpc.RotateStreamKeyReq{
		KeyId: key.KeyID, OperatorMid: 207, RequestId: "rot-replay",
	})
	wantOK(t, second, err, "重放轮转")
	mustTrue(t, second.GetReplayed(), "重放标记")
	if second.GetPlaintextKey() != "" {
		t.Fatalf("重放轮转回显了明文")
	}
	if second.GetKeyId() != first.GetKeyId() {
		t.Fatalf("重放轮转发了另一把密钥：%d vs %d", first.GetKeyId(), second.GetKeyId())
	}
	// 宽限截止属于被替换的旧密钥：重放必须回查旧行如实回显，而不是回新行的 0
	if second.GetGraceUntil() != first.GetGraceUntil() {
		t.Fatalf("重放轮转未回显宽限截止：%d vs %d", second.GetGraceUntil(), first.GetGraceUntil())
	}
	e.requireSameEffects(t, before, "重放轮转必须零写入")
}

func TestRotateStreamKey_SecondRotationOnSameKeyRollsBackEntirely(t *testing.T) {
	e := newTestEnv(t)
	key := e.seedKey(t, &model.StreamKey{
		StreamName: "live_108_a", RoomID: 108, AnchorMid: 208, ProtocolMask: model.ProtocolMaskRtmp,
		Version: 1, State: model.KeyStateActive,
	})
	first, err := NewRotateStreamKeyLogic(bg(), e.svc).RotateStreamKey(&rpc.RotateStreamKeyReq{
		KeyId: key.KeyID, OperatorMid: 208, RequestId: "rot-a",
	})
	wantOK(t, first, err, "首次轮转")

	// 第二次轮转拿同一把已 ROTATING 的旧密钥：LinkRotation 必在 CAS 上失败并整事务回滚，
	// 库里不得留下「新密钥已可用但旧密钥还 ACTIVE」的双活窗口。
	before := e.effects()
	_, err = NewRotateStreamKeyLogic(bg(), e.svc).RotateStreamKey(&rpc.RotateStreamKeyReq{
		KeyId: key.KeyID, OperatorMid: 208, RequestId: "rot-b",
	})
	wantFail(t, err, model.ErrStreamKeyNotUsable, "对已轮转密钥再次轮转")
	e.requireSameEffects(t, before, "LinkRotation 失败必须回滚已插入的新密钥行")
	if got := e.keyRow(t, key.KeyID).State; got != model.KeyStateRotating {
		t.Fatalf("失败轮转改动了旧密钥状态：%d", got)
	}
}

func TestRotateStreamKey_NonOwnerIsRejected(t *testing.T) {
	e := newTestEnv(t)
	key := e.seedKey(t, &model.StreamKey{
		StreamName: "live_109_a", RoomID: 109, AnchorMid: 209, ProtocolMask: model.ProtocolMaskRtmp,
		Version: 1, State: model.KeyStateActive,
	})
	before := e.effects()
	_, err := NewRotateStreamKeyLogic(bg(), e.svc).RotateStreamKey(&rpc.RotateStreamKeyReq{
		KeyId: key.KeyID, OperatorMid: 999, RequestId: "rot-x",
	})
	wantFail(t, err, model.ErrOperatorRequired, "非归属主播轮转")
	e.requireSameEffects(t, before, "越权轮转必须零副作用")

	_, err = NewRotateStreamKeyLogic(bg(), e.svc).RotateStreamKey(&rpc.RotateStreamKeyReq{
		KeyId: key.KeyID, OperatorMid: 0, RequestId: "rot-y",
	})
	wantFail(t, err, model.ErrOperatorRequired, "无归因主体的轮转")
	e.requireSameEffects(t, before, "无主体轮转必须零副作用")
}

// --- 接入鉴权：吊销 / 枚举防护 / 配额 / 建档幂等 ---

func seedAuthableKey(t *testing.T, e *testEnv, name string, room, anchor int64, mutate func(*model.StreamKey)) (*model.StreamKey, string) {
	t.Helper()
	plain := fmt.Sprintf("PLAIN-%s-0123456789abcdef", name)
	k := &model.StreamKey{
		StreamName: name, RoomID: room, AnchorMid: anchor, ProtocolMask: model.ProtocolMaskRtmp,
		KeyHash: sha256Hex(plain), State: model.KeyStateActive, Version: 1, MaxStreams: 1,
		ExpireAt: nowUnix() + 3600,
	}
	if mutate != nil {
		mutate(k)
	}
	return e.seedKey(t, k), plain
}

func TestVerifyPublishAuth_RevokedKeyIsDeniedWithoutSideEffects(t *testing.T) {
	e := newTestEnv(t)
	key, plain := seedAuthableKey(t, e, "live_110_a", 110, 210, func(k *model.StreamKey) {
		k.State = model.KeyStateRevoked
		k.Reason = "风控吊销"
	})
	before := e.effects()

	reply, err := e.verifyPublish(t, &rpc.VerifyPublishAuthReq{
		StreamName: key.StreamName, PlaintextKey: plain, RoomId: key.RoomID,
		Protocol: rpc.IngestProtocol_PROTOCOL_RTMP,
	})
	wantOK(t, reply, err, "吊销密钥鉴权")
	mustFalse(t, reply.GetAllowed(), "吊销密钥")
	if reply.GetReason() != reasonKeyRevoked {
		t.Fatalf("吊销后的原因码应为 key_revoked，实得 %q", reply.GetReason())
	}
	// 纯探测也不许留痕（鉴权失败连事件都不该写）
	e.requireSameEffects(t, before, "吊销密钥鉴权必须零副作用")
}

func TestVerifyPublishAuth_UnknownKeyAndWrongStreamShareOneReason(t *testing.T) {
	e := newTestEnv(t)
	key, _ := seedAuthableKey(t, e, "live_111_a", 111, 211, nil)
	before := e.effects()

	unknown, err := e.verifyPublish(t, &rpc.VerifyPublishAuthReq{
		StreamName: key.StreamName, PlaintextKey: "NEVER-ISSUED-0123456789abcdef", RoomId: key.RoomID,
		Protocol: rpc.IngestProtocol_PROTOCOL_RTMP,
	})
	wantOK(t, unknown, err, "未知密钥鉴权")
	otherStream, err := e.verifyPublish(t, &rpc.VerifyPublishAuthReq{
		StreamName: "live_999_z", PlaintextKey: key.KeyHash, RoomId: key.RoomID,
		Protocol: rpc.IngestProtocol_PROTOCOL_RTMP,
	})
	wantOK(t, otherStream, err, "密钥与流不匹配")

	if unknown.GetReason() != reasonKeyNotFound || otherStream.GetReason() != reasonKeyNotFound {
		t.Fatalf("「密钥不存在」与「密钥不归这条流」必须同码以免被枚举：%q / %q",
			unknown.GetReason(), otherStream.GetReason())
	}
	emptyKey, err := e.verifyPublish(t, &rpc.VerifyPublishAuthReq{
		StreamName: key.StreamName, RoomId: key.RoomID, Protocol: rpc.IngestProtocol_PROTOCOL_RTMP,
	})
	wantOK(t, emptyKey, err, "空密钥鉴权")
	if emptyKey.GetReason() != reasonKeyNotFound {
		t.Fatalf("空明文密钥应同码回 key_not_found：%q", emptyKey.GetReason())
	}
	e.requireSameEffects(t, before, "鉴权拒绝必须零副作用")
}

func TestVerifyPublishAuth_DenialReasonsPerDimension(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*model.StreamKey)
		room    int64
		proto   rpc.IngestProtocol
		reason  string
		streamX func(name string) string
	}{
		{"房间不符", func(k *model.StreamKey) {}, 4242, rpc.IngestProtocol_PROTOCOL_RTMP, reasonRoomMismatch, nil},
		{"协议不符", func(k *model.StreamKey) { k.ProtocolMask = model.ProtocolMaskSrt }, 112, rpc.IngestProtocol_PROTOCOL_RTMP, reasonProtocolDenied, nil},
		{"已过期", func(k *model.StreamKey) { k.ExpireAt = nowUnix() - 1 }, 112, rpc.IngestProtocol_PROTOCOL_RTMP, reasonKeyExpired, nil},
		{"RETIED 终态", func(k *model.StreamKey) { k.State = model.KeyStateRetired }, 112, rpc.IngestProtocol_PROTOCOL_RTMP, reasonKeyExpired, nil},
		{"宽限期已过的 ROTATING", func(k *model.StreamKey) {
			k.State = model.KeyStateRotating
			k.GraceUntil = nowUnix() - 1
		}, 112, rpc.IngestProtocol_PROTOCOL_RTMP, reasonKeyExpired, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv(t)
			key, plain := seedAuthableKey(t, e, "live_112_a", 112, 212, c.mutate)
			before := e.effects()
			reply, err := e.verifyPublish(t, &rpc.VerifyPublishAuthReq{
				StreamName: key.StreamName, PlaintextKey: plain, RoomId: c.room, Protocol: c.proto,
			})
			wantOK(t, reply, err, c.name)
			mustFalse(t, reply.GetAllowed(), c.name)
			if reply.GetReason() != c.reason {
				t.Fatalf("%s：reason 应为 %q，实得 %q", c.name, c.reason, reply.GetReason())
			}
			e.requireSameEffects(t, before, c.name)
		})
	}
}

func TestVerifyPublishAuth_CreateStreamIsQuotaHardGate(t *testing.T) {
	e := newTestEnv(t)
	key, plain := seedAuthableKey(t, e, "live_113_a", 113, 213, nil)

	created, err := e.verifyPublish(t, &rpc.VerifyPublishAuthReq{
		StreamName: key.StreamName, PlaintextKey: plain, RoomId: key.RoomID,
		Protocol: rpc.IngestProtocol_PROTOCOL_RTMP, CreateStream: true, RequestId: "pub-1",
	})
	wantOK(t, created, err, "建档鉴权")
	mustTrue(t, created.GetAllowed(), "首次建档")
	if created.GetStreamId() == "" || created.GetStreamState() != rpc.StreamState_STREAM_STATE_IDLE {
		t.Fatalf("建档后应回 IDLE 流：%+v", created)
	}
	if got := e.keyRow(t, key.KeyID).CurrentStreamID; got != created.GetStreamId() {
		t.Fatalf("活跃指针未指向新建的流：%q", got)
	}
	// 建档不是状态迁移：IDLE 以 seq=0 落库，且不产生事件与信封
	row := e.streamRow(t, created.GetStreamId())
	if row.Seq != 0 || row.State != model.StreamStateIdle {
		t.Fatalf("IDLE 流的 seq/state 不符：seq=%d state=%d", row.Seq, row.State)
	}
	if n := len(e.db.events); n != 0 {
		t.Fatalf("建档不该产生 live.state.v1 事件：%d 行", n)
	}

	before := e.effects()
	// max_streams=1 且已有一条活流：第二路建档必须被 ClaimActiveStream 硬闸门挡下
	_, err = e.verifyPublish(t, &rpc.VerifyPublishAuthReq{
		StreamName: key.StreamName, PlaintextKey: plain, RoomId: key.RoomID,
		Protocol: rpc.IngestProtocol_PROTOCOL_RTMP, CreateStream: true, RequestId: "pub-2",
	})
	wantFail(t, err, model.ErrStreamQuotaExceeded, "超配额建档")
	e.requireSameEffects(t, before, "超配额建档必须零副作用（不能有假流）")

	// 纯探测（不建档）同一条件必须是结构化拒绝而不是错误
	probe, err := e.verifyPublish(t, &rpc.VerifyPublishAuthReq{
		StreamName: key.StreamName, PlaintextKey: plain, RoomId: key.RoomID,
		Protocol: rpc.IngestProtocol_PROTOCOL_RTMP,
	})
	wantOK(t, probe, err, "配额探测")
	mustFalse(t, probe.GetAllowed(), "配额探测")
	if probe.GetReason() != reasonQuotaExceeded {
		t.Fatalf("配额探测的 reason 应为 quota_exceeded：%q", probe.GetReason())
	}
	e.requireSameEffects(t, before, "探测必须零副作用")
}

func TestVerifyPublishAuth_ReplayedRequestIdYieldsNoSecondStream(t *testing.T) {
	e := newTestEnv(t)
	key, plain := seedAuthableKey(t, e, "live_114_a", 114, 214, func(k *model.StreamKey) { k.MaxStreams = 5 })
	first, err := e.verifyPublish(t, &rpc.VerifyPublishAuthReq{
		StreamName: key.StreamName, PlaintextKey: plain, RoomId: key.RoomID,
		Protocol: rpc.IngestProtocol_PROTOCOL_RTMP, CreateStream: true, RequestId: "pub-replay",
	})
	wantOK(t, first, err, "首次建档")

	before := e.effects()
	second, err := e.verifyPublish(t, &rpc.VerifyPublishAuthReq{
		StreamName: key.StreamName, PlaintextKey: plain, RoomId: key.RoomID,
		Protocol: rpc.IngestProtocol_PROTOCOL_RTMP, CreateStream: true, RequestId: "pub-replay",
	})
	wantOK(t, second, err, "重放建档")
	mustTrue(t, second.GetReplayed(), "重放标记")
	mustTrue(t, second.GetAllowed(), "重放仍是放行（同一个建连请求）")
	if second.GetStreamId() != first.GetStreamId() {
		t.Fatalf("重放建出了第二条流：%s vs %s", first.GetStreamId(), second.GetStreamId())
	}
	e.requireSameEffects(t, before, "重放建档必须零写入、零新事务")
	e.requireNoTransaction(t, before, "publish_request_id 重放应在幂等预检处返回")

	// create_stream=true 却不给幂等键：没有幂等键的重放就是两次副作用，必须拒
	_, err = e.verifyPublish(t, &rpc.VerifyPublishAuthReq{
		StreamName: key.StreamName, PlaintextKey: plain, RoomId: key.RoomID,
		Protocol: rpc.IngestProtocol_PROTOCOL_RTMP, CreateStream: true,
	})
	wantFail(t, err, model.ErrIdempotencyKeyRequired, "建档缺幂等键")
	// 非法流标识（含斜杠/空白）在解析层就该拒：一次查库都不许发生。
	hashHitsBefore := e.call("StreamKey.FindByKeyHash")
	_, err = e.verifyPublish(t, &rpc.VerifyPublishAuthReq{
		StreamName: "bad name/with slash", PlaintextKey: plain, RoomId: key.RoomID,
		Protocol: rpc.IngestProtocol_PROTOCOL_RTMP,
	})
	wantFail(t, err, model.ErrPublishDenied, "非法流标识不得进查库")
	if got := e.call("StreamKey.FindByKeyHash"); got != hashHitsBefore {
		t.Fatalf("非法流标识的鉴权仍查了密钥表：calls 从 %d 涨到 %d", hashHitsBefore, got)
	}
}

// e.call 的包装：方法调用计数落在 fakeDB 上。
func (e *testEnv) call(name string) int { return e.db.call(name) }
