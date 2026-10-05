// 本文件是 logic 包的手写扩展（下游 social-graph / risk-control / moderation 的门禁适配），
// 不是 goctl 生成产物。
//
// 边界（AGENTS.md §5）：黑名单、关注关系与风控裁决的真值都不在本库，
// 这里只「取判定结果」，绝不复制对方主数据；未配置或调用失败一律显式失败（fail-closed），
// 绝不把「没接上」当成「已通过」。

package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	moderationrpc "go-video/services/moderation-orchestrator/rpc"
	"go-video/services/private-message/internal/svc"
	"go-video/services/private-message/model"
	riskrpc "go-video/services/risk-control/rpc"
	sgr "go-video/services/social-graph/rpc"
)

// ErrDownstreamUnavailable 下游客户端已配置但调用失败。
// 与 model.Err*NotConfigured 区分：前者是故障（可重试），后者是环境缺依赖（要配置）。
var ErrDownstreamUnavailable = errors.New("private-message: downstream rpc unavailable")

// moderationBusiness 是送审时与 submission_id 共同定位对象的业务名。
//
// 契约缺口（README 已登记）：moderation 的 ContentType 枚举本期只有
// VIDEO/CATALOG/COMMENT/DANMAKU/LIVE，没有私信；私信文本与评论同属「用户投稿文本」，
// 因此取 CONTENT_TYPE_COMMENT 并以 business 区分公司域，
// 等 moderation 契约新增私信类型后只改这里一处。
const moderationBusiness = "private_message"

const moderationContentTypeForPM = moderationrpc.ContentType_CONTENT_TYPE_COMMENT

// riskActionForSend 是发送私信上报给 risk-control 的受保护动作。
//
// 契约缺口（README 已登记）：GuardedAction 本期没有「发私信」，
// 语义最近的是 ACTION_COMMENT（用户投稿文本）。这里刻意不做「拿不到动作就跳过风控」，
// 而是照常 CheckAction——错在动作口径，而不是错在放行。
const riskActionForSend = riskrpc.GuardedAction_ACTION_COMMENT

func socialGraphClient(s *svc.ServiceContext) (sgr.SocialGraphClient, error) {
	cli, err := s.SocialGraphClient()
	if err != nil {
		return nil, err
	}
	return sgr.NewSocialGraphClient(cli.Conn()), nil
}

func riskControlClient(s *svc.ServiceContext) (riskrpc.RiskControlClient, error) {
	cli, err := s.RiskControlClient()
	if err != nil {
		return nil, err
	}
	return riskrpc.NewRiskControlClient(cli.Conn()), nil
}

func moderationClient(s *svc.ServiceContext) (moderationrpc.ModerationOrchestratorClient, error) {
	cli, err := s.ModerationClient()
	if err != nil {
		return nil, err
	}
	return moderationrpc.NewModerationOrchestratorClient(cli.Conn()), nil
}

// relationView 是「发送者与接收者之间」的关系快照（真值在 social-graph）。
type relationView struct {
	// Blocked 任一方拉黑另一方（拉黑是双向拒收：给已拉黑自己的人发、
	// 或发给已被自己拉黑的人，都不是用户想要的通信）。
	Blocked bool
	// ReceiverFollowsSender / SenderFollowsReceiver 关注方向，用于 allow_from 门槛。
	ReceiverFollowsSender bool
	SenderFollowsReceiver bool
	// Known false 表示关系数据没取全（下游故障），此时必须走保守判定。
	Known bool
}

func (r relationView) mutual() bool {
	return r.ReceiverFollowsSender && r.SenderFollowsReceiver
}

// loadRelation 读取双向黑名单与关注关系。
//
// 只读判定，不写任何对方数据；短缓存命中不了就回源，缓存永远不是事实源。
// 返回的 error 只有一种语义：social-graph 未配置（调用方必须把请求整体拒掉并回明确错误）。
// 下游「已配置但调用失败」不返回 error，而是把 Blocked 置真、Known 置假——
// 拒收是可恢复的保守结果，让用户在下游抖动时完全发不出私信才是要评审的问题。
func loadRelation(ctx context.Context, s *svc.ServiceContext, sender, receiver int64) (relationView, error) {
	sg, err := socialGraphClient(s)
	if err != nil {
		return relationView{}, err
	}
	view := relationView{Known: true}

	// 接收方是否拉黑了发送方。
	receiverBlocked, err := cachedPairVerdict(ctx, s, sg, "blk", receiver, sender)
	if err != nil {
		return relationView{Blocked: true, Known: false}, nil
	}
	// 发送方是否拉黑了接收方。
	senderBlocked, err := cachedPairVerdict(ctx, s, sg, "blk", sender, receiver)
	if err != nil {
		return relationView{Blocked: true, Known: false}, nil
	}
	view.Blocked = receiverBlocked || senderBlocked

	receiverFollows, err := cachedPairVerdict(ctx, s, sg, "fol", receiver, sender)
	if err != nil {
		view.Known = false
		return view, nil
	}
	senderFollows, err := cachedPairVerdict(ctx, s, sg, "fol", sender, receiver)
	if err != nil {
		view.Known = false
		return view, nil
	}
	view.ReceiverFollowsSender = receiverFollows
	view.SenderFollowsReceiver = senderFollows
	return view, nil
}

// cachedPairVerdict 查询「owner 是否 target 的 <blk:拉黑 / fol:关注> 对象」，带秒级短缓存。
// 会话列表一页最多 MaxPageSize 行、每行两次判定，缓存把这一页的下游扇出压到必要最小。
func cachedPairVerdict(ctx context.Context, s *svc.ServiceContext, sg sgr.SocialGraphClient,
	kind string, owner, target int64) (bool, error) {
	if owner <= 0 || target <= 0 {
		return false, model.ErrInvalidMid
	}
	key := pairVerdictCacheKey(kind, owner, target)
	var cached bool
	if cacheGetJSON(ctx, s, key, &cached) {
		return cached, nil
	}
	var (
		val bool
		err error
	)
	switch kind {
	case "blk":
		var resp *sgr.RelationReply
		resp, err = sg.IsBlacked(ctx, &sgr.RelationReq{Mid: owner, Owner: target})
		val = err == nil && resp.GetFollowing()
	case "fol":
		var resp *sgr.RelationReply
		resp, err = sg.IsFollowing(ctx, &sgr.RelationReq{Mid: owner, Owner: target})
		val = err == nil && resp.GetFollowing()
	default:
		return false, fmt.Errorf("private-message: unknown pair verdict kind %q", kind)
	}
	if err != nil {
		// 只记主键，不记任何内容字段。
		return false, fmt.Errorf("%w: social-graph %s: %v", ErrDownstreamUnavailable, kind, err)
	}
	cacheSetJSON(ctx, s, key, val, ttlPairVerdict)
	return val, nil
}

// blockedByPeerErr 是「关系侧拒收」的统一出口，保证三条链路（发送/列表/读取）同口径。
func blockedByPeerErr() error { return model.ErrBlockedByPeer }

// receiverAcceptsSender 按接收方偏好判定首条消息是否可投递（README 门禁 b 项）。
//
// 语义（proto AllowFrom 注释）：
//   - ALLOW_FROM_NONE：完全关闭私信，任何新会话都拒；
//   - ALLOW_FROM_FOLLOWED：只收「我关注的人」；
//   - ALLOW_FROM_MUTUAL：只收互关；
//   - ALLOW_FROM_ANYONE：可收任何人，但 reject_stranger 打开时仍要求互关。
//
// 关系数据取不到（Known=false）时用 model.UserSetting.AcceptsUnknownSender 做保守判定：
// 它只在「任何人 + 不强拒陌生人」时为真，其它门槛一律拒收（fail-closed）。
func receiverAcceptsSender(setting *model.UserSetting, rel relationView, firstFromSender bool) error {
	if !firstFromSender {
		// 该发送者在这个会话里已有历史消息：门槛针对的是「陌生人首条消息」，
		// 已经收过一条就说明关系已被接收方的会话事实确认，不再复评。
		return nil
	}
	allowFrom := model.AllowFromAnyone
	rejectStranger := false
	if setting != nil {
		allowFrom = setting.AllowFrom
		rejectStranger = setting.RejectStranger != 0
	}
	if !model.ValidAllowFrom(allowFrom) {
		allowFrom = model.AllowFromAnyone
	}
	if allowFrom == model.AllowFromNone {
		return blockedByPeerErr()
	}
	if !rel.Known {
		if !setting.AcceptsUnknownSender() {
			return blockedByPeerErr()
		}
		return nil
	}
	if rel.Blocked {
		return blockedByPeerErr()
	}
	switch allowFrom {
	case model.AllowFromFollowed:
		if !rel.ReceiverFollowsSender {
			return blockedByPeerErr()
		}
	case model.AllowFromMutual:
		if !rel.mutual() {
			return blockedByPeerErr()
		}
	}
	if rejectStranger && !rel.mutual() {
		return blockedByPeerErr()
	}
	return nil
}

// peerBlockedForRead 判定读取路径上「查看者与对方之间是否存在拉黑」。
// 命中即整条会话从列表/分页里剔除（与发送侧同一套口径，README「三条链路共用过滤」）。
// 返回 error 仅表示 social-graph 未配置：不能把「拿不到判定」当成「全部可见」放行整页。
func peerBlockedForRead(ctx context.Context, s *svc.ServiceContext, viewer, peer int64) (bool, error) {
	sg, err := socialGraphClient(s)
	if err != nil {
		return false, err
	}
	viewerBlocked, err := cachedPairVerdict(ctx, s, sg, "blk", viewer, peer)
	if err != nil {
		// 下游故障：这一行按命中处理（宁可不给看，也不给越权看），不整页失败。
		return true, nil
	}
	peerBlockedViewer, err := cachedPairVerdict(ctx, s, sg, "blk", peer, viewer)
	if err != nil {
		return true, nil
	}
	return viewerBlocked || peerBlockedViewer, nil
}

// riskAllowsSend 让 risk-control 裁决这一次发送（README 门禁 c 项）。
//
// 入参只带主键与载体类型：正文、摘要与内容指纹都不发给风控（避免 P4 内容跨服务扩散），
// 内容合规判定是 moderation 的职责。
func riskAllowsSend(ctx context.Context, s *svc.ServiceContext, sender, conversationID, receiver int64,
	msgType int32, clientMsgID, traceID string) error {
	rc, err := riskControlClient(s)
	if err != nil {
		return err
	}
	resp, err := rc.CheckAction(ctx, &riskrpc.CheckActionReq{
		Mid:    sender,
		Action: riskActionForSend,
		RequestContext: map[string]string{
			"business":        moderationBusiness,
			"conversation_id": strconv.FormatInt(conversationID, 10),
			"peer_mid":        strconv.FormatInt(receiver, 10),
			"msg_type":        strconv.FormatInt(int64(msgType), 10),
		},
		RequestId: clientMsgID,
		TraceId:   sanitizeTraceID(traceID),
	})
	if err != nil {
		return fmt.Errorf("%w: risk-control CheckAction mid=%d: %v", ErrDownstreamUnavailable, sender, err)
	}
	return riskVerdictError(resp)
}

// riskVerdictError 把风控裁决翻译成「放行 / 拒绝」（纯函数，便于单测覆盖全部分支）。
//
// 两条不容情的口径：
//   - degraded=true 时即使 decision=ALLOW 也拒：降级路径下的 ALLOW 不代表真的评估过；
//   - 未识别的裁决值（含 DECISION_UNSPECIFIED）一律拒，不按「默认放行」处理。
func riskVerdictError(resp *riskrpc.CheckActionReply) error {
	if resp == nil {
		return fmt.Errorf("%w: risk-control 返回空裁决", ErrDownstreamUnavailable)
	}
	if resp.GetDegraded() {
		return fmt.Errorf("%w: 风控降级未评估（basis=%s）", model.ErrRiskDenied, resp.GetBasis())
	}
	switch resp.GetDecision() {
	case riskrpc.Decision_DECISION_ALLOW, riskrpc.Decision_DECISION_REVIEW:
		return nil
	default:
		code := resp.GetActionCode()
		if pc := resp.GetPunishment().GetReasonCode(); pc != "" {
			code = pc
		}
		return fmt.Errorf("%w: 风控裁决 %d（reason_code=%s）", model.ErrRiskDenied, resp.GetDecision(), code)
	}
}

// submitForModeration 提交机审/人审并返回任务 ID。
// 未配置返回 ErrModerationNotConfigured；配置了但失败返回 ErrDownstreamUnavailable，
// 两者都不允许被调用方降级成「已送审」。
//
// 只提交主键与原因，正文走 moderation 侧的脱敏通道另行拉取（本期本服务不外发明文）。
func submitForModeration(ctx context.Context, s *svc.ServiceContext, submissionID, mid int64,
	reason string) (int64, error) {
	mo, err := moderationClient(s)
	if err != nil {
		return 0, err
	}
	resp, err := mo.SubmitForReview(ctx, &moderationrpc.SubmitReq{
		SubmissionId: submissionID,
		ContentType:  moderationContentTypeForPM,
		Mid:          mid,
		UpMid:        mid,
		Business:     moderationBusiness,
		Reason:       truncateRunes(reason, maxReasonRunes),
	})
	if err != nil {
		return 0, fmt.Errorf("%w: moderation submit submission_id=%d: %v", ErrDownstreamUnavailable, submissionID, err)
	}
	taskID := resp.GetTask().GetTaskId()
	if taskID <= 0 {
		return 0, fmt.Errorf("%w: moderation 未返回任务（submission_id=%d）", ErrDownstreamUnavailable, submissionID)
	}
	return taskID, nil
}
