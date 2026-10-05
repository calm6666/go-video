package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/live-ingest/internal/svc"
	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type VerifyCdnCallbackLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewVerifyCdnCallbackLogic(ctx context.Context, svcCtx *svc.ServiceContext) *VerifyCdnCallbackLogic {
	return &VerifyCdnCallbackLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// CDN/入口回调鉴权：签名 + 时间窗 + nonce 防重放，只建议状态、由调用方走 ReportStreamState
//
// 判定顺序（每一步都在防不同的东西，顺序不能重排）：
//  1. 入参形态：nonce/event_type/域名/流标识缺失或超长 → 直接报错。这些是「请求本身没成形」，
//     不是鉴权结论，混进 allowed=false 会让入口以为「厂商拒了我们」；
//  2. 域名白名单（Cdn.PublishDomains，全等小写匹配）：不在名单内不写任何一行留证。
//     回调地址是公网可达的，未鉴权的流量若能随意写表就是自我 DoS；
//  3. 解析签名密钥（Cdn.CallbackSecretRef）：拿不到就返回 ErrCallbackSignerUnavailable 并且
//     **不写库**——fail closed。把 nonce 消费掉会让配置恢复后的合法重试被判成重放；
//  4. nonce 重放：uniq_nonce 是唯一锚点。命中就回放首次判定，且**永不重复授权**
//     （首次即便通过，第二次也是 allowed=false + replayed_nonce）；
//  5. 时间窗（CallbackSkewSeconds）：这是「签名有效但重放旧包」的主要形态；
//  6. HMAC-SHA256 比对：待签串是 cdnCanonicalString 的固定字段序（domain/stream_name/event/nonce/timestamp），
//     比对用常数时间；失败只回原因码，**绝不回显签名、密钥或期望值**；
//  7. 归属解析：只有过了签名与时间窗的回调才去查流（否则一个伪造的 stream_name
//     就能驱动我们的库查询并拿到 suggest_state）。
//
// allowed 的口径严格按 proto 注释＝「签名与时间窗是否通过」，因此 stream_not_found
// 是 allowed=true + 一个原因码（回调是真的，只是我们找不到对应的流）。
// 本方法从不当场改流状态：suggest_state 只是建议，推进必须由入口带 report_id 调
// ReportStreamState 走合法迁移矩阵（AGENTS.md §8）。
//
// 隐私：live_cdn_callback 只落 signature_hash / client_ip_hash / raw_params_digest
// 三个单向摘要；日志同样只带域名、流标识与判定结论（签名只留 hex 摘要前缀）。
func (l *VerifyCdnCallbackLogic) VerifyCdnCallback(in *rpc.VerifyCdnCallbackReq) (*rpc.VerifyCdnCallbackReply, error) {
	repo := l.svcCtx.Repository
	if repo == nil {
		return nil, errNoRepository
	}
	cfg := l.svcCtx.Config.LiveIngest

	nonce, err := checkCallbackNonce(in.Nonce)
	if err != nil {
		return nil, err
	}
	domain, err := checkCallbackDomain(in.Domain)
	if err != nil {
		return nil, err
	}
	eventType, err := checkCallbackEventType(in.EventType)
	if err != nil {
		return nil, err
	}
	streamName, err := checkCallbackStreamName(in.StreamName)
	if err != nil {
		return nil, err
	}
	clientIP, err := checkCallbackClientIP(in.ClientIp)
	if err != nil {
		return nil, err
	}
	signature := strings.TrimSpace(in.Signature)
	if signature == "" {
		return nil, fmt.Errorf("%w: 回调缺少签名", model.ErrCallbackSignatureMismatch)
	}
	rawDigest := strings.TrimSpace(in.RawParamsDigest)
	if rawDigest != "" && !isHexDigest(rawDigest) {
		// CHAR(64) 列：塞非摘要值会让留证写入失败，而这正是排障最需要留证的时刻。
		return nil, fmt.Errorf("%w: raw_params_digest 不是 SHA-256 hex 摘要", model.ErrCallbackSignatureMismatch)
	}

	if !callbackDomainBound(l.svcCtx, domain) {
		l.Logger.Errorw("reject cdn callback: domain not in publish whitelist",
			logx.Field("module", "live-ingest"), logx.Field("op", "verify_cdn_callback"),
			logx.Field("domain", domain), logx.Field("reason", reasonDomainUnbound))
		return &rpc.VerifyCdnCallbackReply{Allowed: false, Reason: reasonDomainUnbound}, nil
	}

	secret, err := resolveCallbackSecret(l.svcCtx.Config.Cdn.CallbackSecretRef)
	if err != nil {
		return nil, err
	}

	first, err := repo.CdnCallback.FindByNonce(l.ctx, nonce)
	if err != nil {
		return nil, err
	}
	if first != nil {
		return l.replayVerdict(first), nil
	}

	now := nowUnix()
	row := &model.CdnCallback{
		Nonce:           nonce,
		Domain:          domain,
		EventType:       eventType,
		StreamName:      streamName,
		SignatureHash:   sha256Hex(signature),
		RawParamsDigest: rawDigest,
		VerifiedAt:      now,
		TraceID:         sanitizeTraceID(in.TraceId),
	}
	if clientIP != "" {
		row.ClientIPHash = sha256Hex(clientIP)
	}

	// finish 是所有判定分支的唯一出口：留证写入与响应组装只有一份实现，
	// 避免「改了判定忘了改留证」这类只会在事故复盘时暴露的分叉。
	finish := func(result int32, reason string) (*rpc.VerifyCdnCallbackReply, error) {
		row.VerifyResult = result
		row.Reason = reason
		// 默认「已忽略」：只有判定通过的回调才留给入口去推进状态机（handled=0）。
		row.Handled = model.CallbackIgnored
		allowed := false
		if result == model.CallbackResultPassed {
			row.SuggestState = suggestStateFromCallbackEvent(eventType)
			s, err := repo.Stream.FindActiveByStreamName(l.ctx, streamName)
			if err != nil {
				return nil, err
			}
			allowed = true
			if s == nil {
				row.VerifyResult = model.CallbackResultStreamNotFound
				row.Reason = reasonStreamNotFound
				result = model.CallbackResultStreamNotFound
				reason = reasonStreamNotFound
			} else {
				row.StreamID = s.StreamID
				row.KeyID = s.KeyID
				row.RoomID = s.RoomID
				row.Handled = model.CallbackUnhandled
			}
		}
		id, err := repo.CdnCallback.Insert(l.ctx, row)
		if err != nil {
			if model.IsDuplicate(err) {
				// 并发同源回调抢在落库前完成：uniq_nonce 才是最终防线，回查首次判定。
				winner, findErr := repo.CdnCallback.FindByNonce(l.ctx, nonce)
				if findErr != nil {
					return nil, findErr
				}
				if winner != nil {
					return l.replayVerdict(winner), nil
				}
			}
			return nil, err
		}
		l.Logger.Infow("cdn callback verdict",
			logx.Field("module", "live-ingest"), logx.Field("op", "verify_cdn_callback"),
			logx.Field("domain", domain), logx.Field("stream_name", streamName),
			logx.Field("event_type", eventType), logx.Field("result", callbackResultName(result)),
			logx.Field("reason", reason), logx.Field("callback_log_id", id),
			logx.Field("signature_hash_prefix", prefixHex(row.SignatureHash)))
		return &rpc.VerifyCdnCallbackReply{
			Allowed:       allowed,
			Reason:        reason,
			StreamId:      row.StreamID,
			KeyId:         row.KeyID,
			RoomId:        row.RoomID,
			SuggestState:  rpcStreamState(row.SuggestState),
			CallbackLogId: id,
		}, nil
	}

	if in.Timestamp <= 0 {
		return finish(model.CallbackResultTimestampSkew, reasonTimestampSkew)
	}
	occurredAt, err := reportedAt(in.Timestamp, now, cfg.CallbackSkewSeconds)
	if err != nil {
		// reportedAt 的错误文本只描述时钟形态，判定本身是「越窗」这个结论。
		return finish(model.CallbackResultTimestampSkew, reasonTimestampSkew)
	}
	row.OccurredAt = occurredAt

	expect := hmacHex(secret, cdnCanonicalString(domain, streamName, eventType, nonce, in.Timestamp))
	if !constantTimeEqualHex(expect, signature) {
		return finish(model.CallbackResultBadSignature, reasonSignatureMismatch)
	}
	return finish(model.CallbackResultPassed, "")
}

// replayVerdict 组装重放判定：留证行已存在，因此不再写库，也不重复授权。
// 回带首次判定的归属与 suggest_state，是为了让入口能把「这条回调已经处理过」
// 归因到具体记录，而不是只拿到一个 false。
func (l *VerifyCdnCallbackLogic) replayVerdict(first *model.CdnCallback) *rpc.VerifyCdnCallbackReply {
	l.Logger.Infow("cdn callback replayed",
		logx.Field("module", "live-ingest"), logx.Field("op", "verify_cdn_callback"),
		logx.Field("domain", first.Domain), logx.Field("stream_name", first.StreamName),
		logx.Field("reason", reasonReplayedNonce), logx.Field("callback_log_id", first.ID),
		logx.Field("first_result", callbackResultName(first.VerifyResult)))
	return &rpc.VerifyCdnCallbackReply{
		Allowed:       false,
		Reason:        reasonReplayedNonce,
		StreamId:      first.StreamID,
		KeyId:         first.KeyID,
		RoomId:        first.RoomID,
		SuggestState:  rpcStreamState(first.SuggestState),
		CallbackLogId: first.ID,
		Replayed:      true,
	}
}

// checkCallbackStreamName 校验回调解析出的流标识。
// 超长（> VARCHAR(128)）直接拒绝而不是截断：截断后的值既查不到流，
// 也让留证里的 stream_name 与厂商侧永远对不上。
func checkCallbackStreamName(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", fmt.Errorf("%w: 回调缺少 stream_name", model.ErrCallbackSignatureMismatch)
	}
	if len(trimmed) > maxStreamNameBytes {
		return "", fmt.Errorf("%w: stream_name %d > %d", model.ErrCallbackSignatureMismatch,
			len(trimmed), maxStreamNameBytes)
	}
	return trimmed, nil
}

// prefixHex 取摘要前 8 位供日志辨认（完整摘要本身已足够被误当成可用材料）。
func prefixHex(digest string) string {
	if len(digest) <= 8 {
		return digest
	}
	return digest[:8]
}
