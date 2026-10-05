// 本文件覆盖不变量 3（CDN 回调验签：坏签名 / 时钟越窗 / nonce 重放都拒，
// 只有验过的回调才允许推进状态，回调路径本身不写状态）与不变量 6 的
// 「拿不到签名密钥就不留证、不消费 nonce」，并顺带钉住不变量 8 的摘要纪律。

package logic

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"
)

const (
	testSecretEnv = "LIVE_INGEST_TEST_CALLBACK_SECRET"
	testSecretVal = "vendor-hmac-key-0123456789"
	testDomain    = "push.example.com"
)

// signCallback 用**测试侧独立实现**算签名，而不是复用被测的 hmacHex/cdnCanonicalString：
// 否则「字段顺序被改坏」这种缺陷会被替身一起改掉，测试就成了自证。
// 字段顺序与分隔符按 README/proto 口径写死在这里。
func signCallback(t *testing.T, secret, domain, streamName, eventType, nonce string, timestamp int64) string {
	t.Helper()
	canonical := strings.Join([]string{
		strings.ToLower(strings.TrimSpace(domain)),
		strings.TrimSpace(streamName),
		strings.ToLower(strings.TrimSpace(eventType)),
		strings.TrimSpace(nonce),
		fmt.Sprintf("%d", timestamp),
	}, "\n")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(canonical))
	return hex.EncodeToString(mac.Sum(nil))
}

// callbackEnv 装一个「CDN 已配好、密钥已从环境变量注入」的服务现场。
func callbackEnv(t *testing.T, streamName string) *testEnv {
	t.Helper()
	e := newTestEnv(t)
	t.Setenv(testSecretEnv, testSecretVal)
	e.svc.Config.Cdn.CallbackSecretRef = "env:" + testSecretEnv
	if streamName != "" {
		key := e.seedKey(t, &model.StreamKey{
			StreamName: streamName, RoomID: 300, AnchorMid: 400, ProtocolMask: model.ProtocolMaskRtmp,
			Version: 1, State: model.KeyStateActive,
		})
		e.seedStream(t, &model.Stream{
			StreamID: "S-CB-" + streamName, StreamName: streamName, RoomID: 300, KeyID: key.KeyID,
			Protocol: 1, State: model.StreamStatePublishing, Seq: 1,
		})
	}
	return e
}

func callbackReq(streamName, nonce, eventType string, ts int64, signature string) *rpc.VerifyCdnCallbackReq {
	return &rpc.VerifyCdnCallbackReq{
		Domain: testDomain, StreamName: streamName, EventType: eventType,
		Timestamp: ts, Nonce: nonce, Signature: signature,
		ClientIp: "203.0.113.9", RawParamsDigest: sha256Hex("raw-params-" + nonce),
	}
}

// --- 正向：合法回调 ---

func TestVerifyCdnCallback_GoodSignaturePassesAndOnlySuggestsState(t *testing.T) {
	e := callbackEnv(t, "live_300_a")
	stream := e.streamRow(t, "S-CB-live_300_a")
	before := e.effects()

	ts := nowUnix()
	sig := signCallback(t, testSecretVal, testDomain, "live_300_a", "publish", "nonce-ok", ts)
	reply, err := e.verifyCallback(t, callbackReq("live_300_a", "nonce-ok", "publish", ts, sig))
	wantOK(t, reply, err, "合法回调")
	mustTrue(t, reply.GetAllowed(), "合法回调必须放行")
	if reply.GetSuggestState() != rpc.StreamState_STREAM_STATE_PUBLISHING {
		t.Fatalf("publish 事件的建议状态应为 PUBLISHING：%v", reply.GetSuggestState())
	}
	if reply.GetStreamId() != stream.StreamID || reply.GetRoomId() != stream.RoomID {
		t.Fatalf("回调未归属到已知流：%+v", reply)
	}

	// 关键契约：回调路径**只留证**，绝不就地改流状态（AGENTS.md §8）
	if got := e.streamRow(t, stream.StreamID); got.State != model.StreamStatePublishing || got.Seq != 1 {
		t.Fatalf("回调当场推进了流状态：state=%d seq=%d", got.State, got.Seq)
	}
	if n := len(e.db.events); n != 0 {
		t.Fatalf("回调路径直接写了 live_stream_event（%d 行），推进必须由入口带 report_id 走 ReportStreamState", n)
	}
	// 应有写入：只有 1 行留证
	e.requireDelta(t, before, [9]int{6: 1}, "合法回调只写一行留证")

	row := e.db.callbacks[reply.GetCallbackLogId()]
	if row.VerifyResult != model.CallbackResultPassed || row.Handled != model.CallbackUnhandled {
		t.Fatalf("留证判定不符：result=%d handled=%d", row.VerifyResult, row.Handled)
	}
	if row.SuggestState != model.StreamStatePublishing {
		t.Fatalf("建议状态未落库：%d", row.SuggestState)
	}
}

// --- 反向：坏签名 / 越窗 / 重放 ---

func TestVerifyCdnCallback_BadSignatureIsRejectedAndNotAuthorizing(t *testing.T) {
	e := callbackEnv(t, "live_300_a")
	before := e.effects()

	reply, err := e.verifyCallback(t, callbackReq("live_300_a", "nonce-bad", "publish", nowUnix(),
		signCallback(t, "wrong-secret", testDomain, "live_300_a", "publish", "nonce-bad", nowUnix())))
	wantOK(t, reply, err, "坏签名回调")
	mustFalse(t, reply.GetAllowed(), "坏签名")
	if reply.GetReason() != reasonSignatureMismatch {
		t.Fatalf("原因码应为 signature_mismatch：%q", reply.GetReason())
	}
	// 未过签名的回调不得产出「建议状态」：否则伪造者能直接驱动下游投影
	if reply.GetSuggestState() != rpc.StreamState_STREAM_STATE_UNSPECIFIED {
		t.Fatalf("坏签名回调回了建议状态：%v", reply.GetSuggestState())
	}
	if reply.GetStreamId() != "" {
		t.Fatalf("坏签名回调解析并回显了归属流：%s", reply.GetStreamId())
	}
	row := e.db.callbacks[reply.GetCallbackLogId()]
	if row.VerifyResult != model.CallbackResultBadSignature || row.Handled != model.CallbackIgnored {
		t.Fatalf("坏签名留证不符：result=%d handled=%d", row.VerifyResult, row.Handled)
	}
	if got := e.streamRow(t, "S-CB-live_300_a").State; got != model.StreamStatePublishing {
		t.Fatalf("被拒回调改了流状态：%d", got)
	}
	// 被拒的回调也要留证，且**只**留证这一行
	e.requireDelta(t, before, [9]int{6: 1}, "坏签名回调的写入集合")

	// 签名逐位篡改（改掉末位并保证与原值不同）同样必须拒
	tapTs := nowUnix()
	valid := signCallback(t, testSecretVal, testDomain, "live_300_a", "publish", "nonce-tap", tapTs)
	tampered := valid[:len(valid)-1] + flipHexByte(valid[len(valid)-1:])
	if tampered == valid {
		t.Fatalf("篡改未生效，用例失去意义")
	}
	again, err := e.verifyCallback(t, callbackReq("live_300_a", "nonce-tap", "publish", tapTs, tampered))
	wantOK(t, again, err, "篡改后的签名")
	mustFalse(t, again.GetAllowed(), "篡改后的签名")
	if again.GetReason() != reasonSignatureMismatch {
		t.Fatalf("篡改签名的原因码应为 signature_mismatch：%q", again.GetReason())
	}
}

// flipHexByte 把一位十六进制翻成另一个确定不同的字符。
func flipHexByte(s string) string {
	if s == "f" {
		return "0"
	}
	return "f"
}

func TestVerifyCdnCallback_TimestampSkewIsRejected(t *testing.T) {
	e := callbackEnv(t, "live_300_a")
	skew := e.svc.Config.LiveIngest.CallbackSkewSeconds

	cases := []struct {
		name string
		ts   int64
	}{
		{"超前于时钟窗口", nowUnix() + skew + 120},
		{"时间戳缺失", 0},
		{"时间戳早于可信下限", 100000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 每个子用例独立 nonce：否则第二格会被 nonce 唯一键判成重放，越窗分支就测不到了
			nonce := fmt.Sprintf("nonce-skew-%d", len(e.db.callbacks))
			// 签名本身是对的：越窗必须单独被拒（这是「重放旧包」的主要形态）
			sig := signCallback(t, testSecretVal, testDomain, "live_300_a", "publish", nonce, c.ts)
			reply, err := e.verifyCallback(t, callbackReq("live_300_a", nonce, "publish", c.ts, sig))
			wantOK(t, reply, err, c.name)
			mustFalse(t, reply.GetAllowed(), c.name)
			if reply.GetReason() != reasonTimestampSkew {
				t.Fatalf("%s：reason 应为 timestamp_skew，实得 %q", c.name, reply.GetReason())
			}
			if got := e.db.callbacks[reply.GetCallbackLogId()].VerifyResult; got != model.CallbackResultTimestampSkew {
				t.Fatalf("留证判定应为 TIMESTAMP_SKEW，实得 %d", got)
			}
		})
	}
}

func TestVerifyCdnCallback_ReplayedNonceNeverReauthorizes(t *testing.T) {
	e := callbackEnv(t, "live_300_a")
	ts := nowUnix()
	sig := signCallback(t, testSecretVal, testDomain, "live_300_a", "publish", "nonce-replay", ts)

	first, err := e.verifyCallback(t, callbackReq("live_300_a", "nonce-replay", "publish", ts, sig))
	wantOK(t, first, err, "首次回调")
	mustTrue(t, first.GetAllowed(), "首次回调应放行")

	before := e.effects()
	// 同一 nonce 第二次到达，且这次连流都不给了（换个流名）：也必须按首次判定回放
	second, err := e.verifyCallback(t, &rpc.VerifyCdnCallbackReq{
		Domain: testDomain, StreamName: "live_other", EventType: "publish_done",
		Timestamp: ts, Nonce: "nonce-replay", Signature: sig,
	})
	wantOK(t, second, err, "重放回调")
	mustFalse(t, second.GetAllowed(), "重放回调永不重复授权")
	mustTrue(t, second.GetReplayed(), "重放标记")
	if second.GetReason() != reasonReplayedNonce {
		t.Fatalf("重放原因码应为 replayed_nonce：%q", second.GetReason())
	}
	if second.GetCallbackLogId() != first.GetCallbackLogId() {
		t.Fatalf("重放未回指首次留证：%d vs %d", second.GetCallbackLogId(), first.GetCallbackLogId())
	}
	if second.GetSuggestState() != rpc.StreamState_STREAM_STATE_PUBLISHING {
		t.Fatalf("重放应按首次判定回显建议状态：%v", second.GetSuggestState())
	}
	e.requireSameEffects(t, before, "重放回调必须零写入（不再留证、不再消费 nonce）")
	if n := len(e.db.callbacks); n != 1 {
		t.Fatalf("重放回调多写了一份留证：%d 行", n)
	}
}

func TestVerifyCdnCallback_UnboundDomainWritesNothing(t *testing.T) {
	e := callbackEnv(t, "live_300_a")
	before := e.effects()
	ts := nowUnix()
	sig := signCallback(t, testSecretVal, "cdn.attacker.tld", "live_300_a", "publish", "nonce-evil", ts)

	reply, err := e.verifyCallback(t, &rpc.VerifyCdnCallbackReq{
		Domain: "CDN.Attacker.TLD", StreamName: "live_300_a", EventType: "publish",
		Timestamp: ts, Nonce: "nonce-evil", Signature: sig,
	})
	wantOK(t, reply, err, "未绑定域名的回调")
	mustFalse(t, reply.GetAllowed(), "未绑定域名")
	if reply.GetReason() != reasonDomainUnbound {
		t.Fatalf("reason 应为 domain_unbound：%q", reply.GetReason())
	}
	// 公网可达的接口：未鉴权流量若 able 随意写表就是自我 DoS
	e.requireSameEffects(t, before, "未绑定域名的回调必须一行都不写")
}

func TestVerifyCdnCallback_SignerUnavailableIsFailClosedAndKeepsNonce(t *testing.T) {
	e := callbackEnv(t, "live_300_a")
	// vault: 形态需要外部密钥服务，本仓库无客户端：必须显式报错而不是「跳过验签」
	e.svc.Config.Cdn.CallbackSecretRef = "vault:secret/data/live-ingest/cdn#callback_key"
	before := e.effects()
	ts := nowUnix()
	sig := signCallback(t, testSecretVal, testDomain, "live_300_a", "publish", "nonce-nosigner", ts)

	reply, err := e.verifyCallback(t, callbackReq("live_300_a", "nonce-nosigner", "publish", ts, sig))
	wantFail(t, err, model.ErrCallbackSignerUnavailable, "拿不到签名密钥")
	if reply != nil {
		t.Fatalf("验签不可用不能回一个看起来像结论的响应：%+v", reply)
	}
	// 关键：nonce 不能被消费掉，否则配置恢复后的合法重试会被判重放
	e.requireSameEffects(t, before, "signer 不可用必须既不写库也不消费 nonce")

	// 环境变量未注入同样算不可用（引用形态合法但取不到值）
	t.Setenv("LIVE_INGEST_TEST_MISSING_SECRET", "")
	e.svc.Config.Cdn.CallbackSecretRef = "env:LIVE_INGEST_TEST_MISSING_SECRET"
	_, err = e.verifyCallback(t, callbackReq("live_300_a", "nonce-nosigner", "publish", ts, sig))
	wantFail(t, err, model.ErrCallbackSignerUnavailable, "环境变量未注入")
	e.requireSameEffects(t, before, "取不到密钥同样必须零副作用")
}

func TestVerifyCdnCallback_StreamNotFoundStillCountsAsVerified(t *testing.T) {
	e := callbackEnv(t, "")
	before := e.effects()
	ts := nowUnix()
	sig := signCallback(t, testSecretVal, testDomain, "live_unknown", "publish", "nonce-nostream", ts)

	reply, err := e.verifyCallback(t, callbackReq("live_unknown", "nonce-nostream", "publish", ts, sig))
	wantOK(t, reply, err, "无法归属的回调")
	// proto 口径：allowed = 「签名与时间窗是否通过」，找不到流只是归属失败，不是鉴权失败
	mustTrue(t, reply.GetAllowed(), "验签通过但流未知")
	if reply.GetReason() != reasonStreamNotFound {
		t.Fatalf("reason 应为 stream_not_found：%q", reply.GetReason())
	}
	row := e.db.callbacks[reply.GetCallbackLogId()]
	if row.VerifyResult != model.CallbackResultStreamNotFound || row.Handled != model.CallbackIgnored {
		t.Fatalf("归属失败的留证不符：result=%d handled=%d", row.VerifyResult, row.Handled)
	}
	if row.StreamID != "" || row.KeyID != 0 {
		t.Fatalf("归属失败却写了流/密钥引用：%s/%d", row.StreamID, row.KeyID)
	}
	e.requireDelta(t, before, [9]int{6: 1}, "只写留证一行")
}

// --- 回调入参形态：报错而不是伪造鉴权结论 ---

func TestVerifyCdnCallback_MalformedRequestsErrorOut(t *testing.T) {
	e := callbackEnv(t, "live_300_a")
	ts := nowUnix()
	good := signCallback(t, testSecretVal, testDomain, "live_300_a", "publish", "n-1", ts)

	cases := []struct {
		name   string
		mutate func(*rpc.VerifyCdnCallbackReq)
		target error
	}{
		{"缺 nonce", func(in *rpc.VerifyCdnCallbackReq) { in.Nonce = "  " }, model.ErrIdempotencyKeyRequired},
		{"nonce 含空白", func(in *rpc.VerifyCdnCallbackReq) { in.Nonce = "a b" }, model.ErrIdempotencyKeyRequired},
		{"缺域名", func(in *rpc.VerifyCdnCallbackReq) { in.Domain = "" }, model.ErrCallbackDomainUnbound},
		{"域名带路径", func(in *rpc.VerifyCdnCallbackReq) { in.Domain = "push.example.com/x" }, model.ErrCallbackDomainUnbound},
		{"缺事件名", func(in *rpc.VerifyCdnCallbackReq) { in.EventType = "" }, model.ErrCallbackSignatureMismatch},
		{"缺流标识", func(in *rpc.VerifyCdnCallbackReq) { in.StreamName = "" }, model.ErrCallbackSignatureMismatch},
		{"缺签名", func(in *rpc.VerifyCdnCallbackReq) { in.Signature = "" }, model.ErrCallbackSignatureMismatch},
		{"raw_params_digest 非摘要", func(in *rpc.VerifyCdnCallbackReq) { in.RawParamsDigest = "not-a-digest" },
			model.ErrCallbackSignatureMismatch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := e.effects()
			in := callbackReq("live_300_a", "n-1", "publish", ts, good)
			c.mutate(in)
			reply, err := e.verifyCallback(t, in)
			// 「请求本身没成形」必须报错：混进 allowed=false 会让入口以为厂商拒了我们
			wantFail(t, err, c.target, c.name)
			if reply != nil {
				t.Fatalf("%s：不该同时回结论：%+v", c.name, reply)
			}
			e.requireSameEffects(t, before, c.name)
		})
	}
}

// --- 隐私：库里与日志里都只有摘要 ---

func TestVerifyCdnCallback_StoresOnlyDigestsAndLogsNoSecrets(t *testing.T) {
	e := callbackEnv(t, "live_300_a")
	logs := captureLogs(t)
	ts := nowUnix()
	sig := signCallback(t, testSecretVal, testDomain, "live_300_a", "publish_done", "nonce-privacy", ts)

	reply, err := e.verifyCallback(t, callbackReq("live_300_a", "nonce-privacy", "publish_done", ts, sig))
	wantOK(t, reply, err, "回调")

	row := e.db.callbacks[reply.GetCallbackLogId()]
	if row.SignatureHash != sha256Hex(sig) {
		t.Fatalf("签名只应存 SHA-256 摘要：got=%s", row.SignatureHash)
	}
	if row.ClientIPHash != sha256Hex("203.0.113.9") {
		t.Fatalf("来源 IP 只应存摘要：got=%s", row.ClientIPHash)
	}
	for name, value := range map[string]string{
		"signature_hash": row.SignatureHash, "client_ip_hash": row.ClientIPHash,
		"raw_params_digest": row.RawParamsDigest, "reason": row.Reason, "domain": row.Domain,
	} {
		if strings.Contains(value, sig) || strings.Contains(value, "203.0.113.9") {
			t.Fatalf("明文材料进了库列 %s", name)
		}
	}

	text := logs.joined()
	requireAbsent(t, text, sig, "厂商签名原文")
	requireAbsent(t, text, testSecretVal, "回调签名密钥")
	requireAbsent(t, text, "203.0.113.9", "来源 IP 明文")
	requireAbsent(t, text, row.SignatureHash, "完整签名摘要（日志只允许前缀）")
	if !strings.Contains(text, "cdn callback verdict") {
		t.Fatalf("回调判定必须可从日志归因（否则排障时看不到结论）：\n%s", text)
	}
}

// --- 纯映射：事件名 → 建议状态（判定顺序是契约的一部分） ---

func TestSuggestStateFromCallbackEvent_StopClassBeatsPublishSubstring(t *testing.T) {
	cases := map[string]int32{
		"publish":             model.StreamStatePublishing,
		"PUBLISH":             model.StreamStatePublishing,
		"publish_done":        model.StreamStateStopped, // 含 "publish"，但它是终止类
		"unpublish":           model.StreamStateStopped, // 同上
		"stream_end":          model.StreamStateStopped,
		"offline":             model.StreamStateStopped,
		"node_disconnect":     model.StreamStateInterrupted,
		"publish_interrupted": model.StreamStateInterrupted,
		"resume":              model.StreamStatePublishing,
		"":                    0,
		"totally-unknown":     0,
	}
	for event, want := range cases {
		if got := suggestStateFromCallbackEvent(event); got != want {
			t.Fatalf("事件 %q 的建议状态应为 %s，实得 %s", event, streamStateName(want), streamStateName(got))
		}
	}
}

func TestCdnCanonicalStringIsFixedFieldOrder(t *testing.T) {
	// 待签串一旦改字段序或分隔符，厂商侧就会「签名永远不匹配」的静默故障。
	// 这里写死字面量，任何改动都必须先让这一格红。
	got := cdnCanonicalString("Push.Example.COM", " live_1_a ", " Publish ", "nonce-1", 1700000000)
	want := "push.example.com\nlive_1_a\npublish\nnonce-1\n1700000000"
	if got != want {
		t.Fatalf("待签串口径变了：\n got=%q\nwant=%q", got, want)
	}
}

func TestCallbackDomainBoundIsExactLowercaseMatch(t *testing.T) {
	e := callbackEnv(t, "")
	if !callbackDomainBound(e.svc, testDomain) {
		t.Fatalf("白名单内域名必须命中")
	}
	if callbackDomainBound(e.svc, "push.example.com.evil.tld") {
		t.Fatalf("后缀匹配会让 attacker 注册 live.example.com.evil.tld 直接通过")
	}
	e.svc.Config.Cdn.PublishDomains = nil
	if callbackDomainBound(e.svc, testDomain) {
		t.Fatalf("白名单为空时不接受任何回调（不能「谁都能打」）")
	}
	if callbackDomainBound(nil, testDomain) {
		t.Fatalf("ServiceContext 缺席时必须判未绑定")
	}
}
