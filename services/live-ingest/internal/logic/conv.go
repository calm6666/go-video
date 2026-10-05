// 本文件是 logic 包的手写投影层（model 行结构 -> rpc 消息），不是 goctl 生成产物。
//
// 存在理由：22 个用例里有 9 个是「读 + 投影」，投影口径必须只有一处实现，
// 否则同一个 StreamKey 会在 GetStreamKey / ListStreamKeys / RotateStreamKey 三处
// 给出不同字段，客户端就无从判断哪一份是真的。
//
// 密钥纪律在本文件的体现：keyInfo 只输出 key_hint_tail 与 key_ref，
// 永远不读 KeyHash 字段——行结构里带着它，但投影函数没有把它复制到任何响应消息，
// 因此新增字段时也请照此检查（StreamKeyInfo 里根本没有对应字段，编译期就是防线）。
package logic

import (
	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"
)

// --- 枚举映射：model 常量（与库列取值一致） <-> rpc 枚举 ---
//
// 用显式 switch 而不是 int32 直转：一旦两侧编号被改得对不上，
// 这里会退化到 UNSPECIFIED（调用方可见），而直转会把非法值原样发出去。

func rpcStreamState(v int32) rpc.StreamState {
	switch v {
	case model.StreamStateIdle:
		return rpc.StreamState_STREAM_STATE_IDLE
	case model.StreamStatePublishing:
		return rpc.StreamState_STREAM_STATE_PUBLISHING
	case model.StreamStateInterrupted:
		return rpc.StreamState_STREAM_STATE_INTERRUPTED
	case model.StreamStateStopped:
		return rpc.StreamState_STREAM_STATE_STOPPED
	default:
		return rpc.StreamState_STREAM_STATE_UNSPECIFIED
	}
}

func streamStateFromRPC(v rpc.StreamState) int32 {
	switch v {
	case rpc.StreamState_STREAM_STATE_IDLE:
		return model.StreamStateIdle
	case rpc.StreamState_STREAM_STATE_PUBLISHING:
		return model.StreamStatePublishing
	case rpc.StreamState_STREAM_STATE_INTERRUPTED:
		return model.StreamStateInterrupted
	case rpc.StreamState_STREAM_STATE_STOPPED:
		return model.StreamStateStopped
	default:
		return 0 // 0 不是合法状态，调用方据此拒绝
	}
}

func rpcKeyState(v int32) rpc.StreamKeyState {
	switch v {
	case model.KeyStateActive:
		return rpc.StreamKeyState_STREAM_KEY_STATE_ACTIVE
	case model.KeyStateRotating:
		return rpc.StreamKeyState_STREAM_KEY_STATE_ROTATING
	case model.KeyStateRetired:
		return rpc.StreamKeyState_STREAM_KEY_STATE_RETIRED
	case model.KeyStateExpired:
		return rpc.StreamKeyState_STREAM_KEY_STATE_EXPIRED
	case model.KeyStateRevoked:
		return rpc.StreamKeyState_STREAM_KEY_STATE_REVOKED
	default:
		return rpc.StreamKeyState_STREAM_KEY_STATE_UNSPECIFIED
	}
}

func keyStateFromRPC(v rpc.StreamKeyState) int32 {
	switch v {
	case rpc.StreamKeyState_STREAM_KEY_STATE_ACTIVE:
		return model.KeyStateActive
	case rpc.StreamKeyState_STREAM_KEY_STATE_ROTATING:
		return model.KeyStateRotating
	case rpc.StreamKeyState_STREAM_KEY_STATE_RETIRED:
		return model.KeyStateRetired
	case rpc.StreamKeyState_STREAM_KEY_STATE_EXPIRED:
		return model.KeyStateExpired
	case rpc.StreamKeyState_STREAM_KEY_STATE_REVOKED:
		return model.KeyStateRevoked
	default:
		return 0
	}
}

func rpcHealthState(v int32) rpc.HealthState {
	switch v {
	case model.HealthStateHealthy:
		return rpc.HealthState_HEALTH_STATE_HEALTHY
	case model.HealthStateDegraded:
		return rpc.HealthState_HEALTH_STATE_DEGRADED
	case model.HealthStateCritical:
		return rpc.HealthState_HEALTH_STATE_CRITICAL
	case model.HealthStateNoData:
		return rpc.HealthState_HEALTH_STATE_NO_DATA
	default:
		return rpc.HealthState_HEALTH_STATE_UNSPECIFIED
	}
}

func rpcStopReason(v int32) rpc.StopReason {
	switch v {
	case model.StopReasonAnchorStop:
		return rpc.StopReason_STOP_REASON_ANCHOR_STOP
	case model.StopReasonNodeTimeout:
		return rpc.StopReason_STOP_REASON_NODE_TIMEOUT
	case model.StopReasonUnhealthy:
		return rpc.StopReason_STOP_REASON_UNHEALTHY
	case model.StopReasonRevoked:
		return rpc.StopReason_STOP_REASON_REVOKED
	case model.StopReasonAdmin:
		return rpc.StopReason_STOP_REASON_ADMIN
	case model.StopReasonKeyExpired:
		return rpc.StopReason_STOP_REASON_KEY_EXPIRED
	default:
		return rpc.StopReason_STOP_REASON_UNSPECIFIED
	}
}

func stopReasonFromRPC(v rpc.StopReason) int32 {
	switch v {
	case rpc.StopReason_STOP_REASON_ANCHOR_STOP:
		return model.StopReasonAnchorStop
	case rpc.StopReason_STOP_REASON_NODE_TIMEOUT:
		return model.StopReasonNodeTimeout
	case rpc.StopReason_STOP_REASON_UNHEALTHY:
		return model.StopReasonUnhealthy
	case rpc.StopReason_STOP_REASON_REVOKED:
		return model.StopReasonRevoked
	case rpc.StopReason_STOP_REASON_ADMIN:
		return model.StopReasonAdmin
	case rpc.StopReason_STOP_REASON_KEY_EXPIRED:
		return model.StopReasonKeyExpired
	default:
		return 0
	}
}

func rpcInterruptionEnd(v int32) rpc.InterruptionEndReason {
	switch v {
	case model.InterruptionEndReconnected:
		return rpc.InterruptionEndReason_INTERRUPTION_END_RECONNECTED
	case model.InterruptionEndTimeout:
		return rpc.InterruptionEndReason_INTERRUPTION_END_TIMEOUT
	case model.InterruptionEndClosed:
		return rpc.InterruptionEndReason_INTERRUPTION_END_CLOSED
	default:
		return rpc.InterruptionEndReason_INTERRUPTION_END_REASON_UNSPECIFIED
	}
}

func interruptionEndFromRPC(v rpc.InterruptionEndReason) int32 {
	switch v {
	case rpc.InterruptionEndReason_INTERRUPTION_END_RECONNECTED:
		return model.InterruptionEndReconnected
	case rpc.InterruptionEndReason_INTERRUPTION_END_TIMEOUT:
		return model.InterruptionEndTimeout
	case rpc.InterruptionEndReason_INTERRUPTION_END_CLOSED:
		return model.InterruptionEndClosed
	default:
		return 0
	}
}

func rpcNodeState(v int32) rpc.IngestNodeState {
	switch v {
	case model.NodeStateOnline:
		return rpc.IngestNodeState_INGEST_NODE_STATE_ONLINE
	case model.NodeStateDraining:
		return rpc.IngestNodeState_INGEST_NODE_STATE_DRAINING
	case model.NodeStateOffline:
		return rpc.IngestNodeState_INGEST_NODE_STATE_OFFLINE
	default:
		return rpc.IngestNodeState_INGEST_NODE_STATE_UNSPECIFIED
	}
}

func nodeStateFromRPC(v rpc.IngestNodeState) int32 {
	switch v {
	case rpc.IngestNodeState_INGEST_NODE_STATE_ONLINE:
		return model.NodeStateOnline
	case rpc.IngestNodeState_INGEST_NODE_STATE_DRAINING:
		return model.NodeStateDraining
	case rpc.IngestNodeState_INGEST_NODE_STATE_OFFLINE:
		return model.NodeStateOffline
	default:
		return 0
	}
}

func rpcAssignmentState(v int32) rpc.AssignmentState {
	switch v {
	case model.AssignmentStateActive:
		return rpc.AssignmentState_ASSIGNMENT_STATE_ACTIVE
	case model.AssignmentStateReleased:
		return rpc.AssignmentState_ASSIGNMENT_STATE_RELEASED
	case model.AssignmentStateMigrated:
		return rpc.AssignmentState_ASSIGNMENT_STATE_MIGRATED
	default:
		return rpc.AssignmentState_ASSIGNMENT_STATE_UNSPECIFIED
	}
}

func assignmentStateFromRPC(v rpc.AssignmentState) int32 {
	switch v {
	case rpc.AssignmentState_ASSIGNMENT_STATE_ACTIVE:
		return model.AssignmentStateActive
	case rpc.AssignmentState_ASSIGNMENT_STATE_RELEASED:
		return model.AssignmentStateReleased
	case rpc.AssignmentState_ASSIGNMENT_STATE_MIGRATED:
		return model.AssignmentStateMigrated
	default:
		return 0
	}
}

// protocolFromRaw 把库里存的协议枚举值还原成 rpc 枚举。
// 越界值退化为 UNSPECIFIED（调用方看得见），而不是直转成一个新的「看起来像 RTMP」的值。
func protocolFromRaw(v int32) rpc.IngestProtocol {
	if _, ok := model.ProtocolMask(v); !ok {
		return rpc.IngestProtocol_PROTOCOL_UNSPECIFIED
	}
	return rpc.IngestProtocol(v)
}

// maskToProtocols 把 protocol_mask 位图还原成协议枚举列表（响应口径）。
// 位图为 0 表示「没有允许协议」，返回空切片而不是补一个 RTMP。
func maskToProtocols(mask uint32) []rpc.IngestProtocol {
	protocols := make([]rpc.IngestProtocol, 0, 3)
	if mask&model.ProtocolMaskRtmp != 0 {
		protocols = append(protocols, rpc.IngestProtocol_PROTOCOL_RTMP)
	}
	if mask&model.ProtocolMaskSrt != 0 {
		protocols = append(protocols, rpc.IngestProtocol_PROTOCOL_SRT)
	}
	if mask&model.ProtocolMaskWebrtc != 0 {
		protocols = append(protocols, rpc.IngestProtocol_PROTOCOL_WEBRTC)
	}
	return protocols
}

// --- 行 -> 消息投影 ---

func keyInfo(k *model.StreamKey) *rpc.StreamKeyInfo {
	if k == nil {
		return nil
	}
	return &rpc.StreamKeyInfo{
		KeyId:           k.KeyID,
		StreamName:      k.StreamName,
		KeyHintTail:     k.KeyTail,
		KeyRef:          k.KeyRef,
		State:           rpcKeyState(k.State),
		Version:         k.Version,
		PrevKeyId:       k.PrevKeyID,
		Protocols:       maskToProtocols(k.ProtocolMask),
		RoomId:          k.RoomID,
		SessionId:       k.SessionID,
		AnchorMid:       k.AnchorMid,
		ExpireAt:        k.ExpireAt,
		GraceUntil:      k.GraceUntil,
		CurrentStreamId: k.CurrentStreamID,
		RotateToKeyId:   k.RotateToKeyID,
		Reason:          k.Reason,
		Ctime:           k.Ctime,
		Mtime:           k.Mtime,
	}
}

func keyInfos(rows []*model.StreamKey) []*rpc.StreamKeyInfo {
	out := make([]*rpc.StreamKeyInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, keyInfo(r))
	}
	return out
}

func streamInfo(s *model.Stream) *rpc.StreamInfo {
	if s == nil {
		return nil
	}
	return &rpc.StreamInfo{
		StreamId:                s.StreamID,
		KeyId:                   s.KeyID,
		StreamName:              s.StreamName,
		RoomId:                  s.RoomID,
		SessionId:               s.SessionID,
		AnchorMid:               s.AnchorMid,
		Protocol:                protocolFromRaw(s.Protocol),
		NodeId:                  s.NodeID,
		State:                   rpcStreamState(s.State),
		Seq:                     s.Seq,
		PublishStartedAt:        s.PublishStartedAt,
		StateChangedAt:          s.StateChangedAt,
		LastHeartbeatAt:         s.LastHeartbeatAt,
		InterruptedTotalSeconds: s.InterruptedTotalSecs,
		InterruptionCount:       s.InterruptionCount,
		StopReason:              rpcStopReason(s.StopReason),
		HealthState:             s.HealthState,
		HealthReportedAt:        s.HealthReportedAt,
		VideoBitrateBps:         s.VideoBitrateBps,
		AudioBitrateBps:         s.AudioBitrateBps,
		Fps:                     s.FpsX100,
		PacketLossPpm:           s.PacketLossPpm,
		Ctime:                   s.Ctime,
		Mtime:                   s.Mtime,
	}
}

func streamInfos(rows []*model.Stream) []*rpc.StreamInfo {
	out := make([]*rpc.StreamInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, streamInfo(r))
	}
	return out
}

func nodeInfo(n *model.IngestNode) *rpc.IngestNodeInfo {
	if n == nil {
		return nil
	}
	return &rpc.IngestNodeInfo{
		NodeId:          n.NodeID,
		Name:            n.Name,
		Region:          n.Region,
		Protocols:       maskToProtocols(n.ProtocolMask),
		EndpointRtmp:    n.EndpointRtmp,
		EndpointSrt:     n.EndpointSrt,
		EndpointWebrtc:  n.EndpointWebrtc,
		State:           rpcNodeState(n.State),
		CapacityStreams: n.CapacityStreams,
		ActiveStreams:   n.ActiveStreams,
		HealthScore:     n.HealthScore,
		LastHeartbeatAt: n.LastHeartbeatAt,
		Labels:          n.Labels,
		Ctime:           n.Ctime,
		Mtime:           n.Mtime,
	}
}

func nodeInfos(rows []*model.IngestNode) []*rpc.IngestNodeInfo {
	out := make([]*rpc.IngestNodeInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, nodeInfo(r))
	}
	return out
}

func assignmentInfo(a *model.NodeAssignment) *rpc.NodeAssignmentInfo {
	if a == nil {
		return nil
	}
	return &rpc.NodeAssignmentInfo{
		AssignmentId: a.AssignmentID,
		StreamId:     a.StreamID,
		RoomId:       a.RoomID,
		NodeId:       a.NodeID,
		Protocol:     protocolFromRaw(a.Protocol),
		State:        rpcAssignmentState(a.State),
		Score:        a.Score,
		PrevNodeId:   a.PrevNodeID,
		AssignedAt:   a.AssignedAt,
		ReleasedAt:   a.ReleasedAt,
		Reason:       a.Reason,
		TraceId:      a.TraceID,
	}
}

func assignmentInfos(rows []*model.NodeAssignment) []*rpc.NodeAssignmentInfo {
	out := make([]*rpc.NodeAssignmentInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, assignmentInfo(r))
	}
	return out
}

func eventInfo(e *model.StreamEvent) *rpc.StreamEventInfo {
	if e == nil {
		return nil
	}
	return &rpc.StreamEventInfo{
		EventId:            e.EventID,
		StreamId:           e.StreamID,
		RoomId:             e.RoomID,
		SessionId:          e.SessionID,
		Seq:                e.Seq,
		FromState:          rpcStreamState(e.FromState),
		ToState:            rpcStreamState(e.ToState),
		NodeId:             e.NodeID,
		InterruptionId:     e.InterruptionID,
		InterruptedSeconds: toInt32(e.InterruptedSeconds),
		StopReason:         rpcStopReason(e.StopReason),
		Reason:             e.Reason,
		OccurredAt:         e.OccurredAt,
		Ctime:              e.Ctime,
	}
}

func eventInfos(rows []*model.StreamEvent) []*rpc.StreamEventInfo {
	out := make([]*rpc.StreamEventInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, eventInfo(r))
	}
	return out
}

func interruptionInfo(i *model.StreamInterruption) *rpc.StreamInterruptionInfo {
	if i == nil {
		return nil
	}
	return &rpc.StreamInterruptionInfo{
		InterruptionId:    i.InterruptionID,
		StreamId:          i.StreamID,
		RoomId:            i.RoomID,
		EpisodeNo:         i.EpisodeNo,
		NodeId:            i.NodeID,
		StartedAt:         i.StartedAt,
		EndedAt:           i.EndedAt,
		DurationSeconds:   i.DurationSeconds,
		EndReason:         rpcInterruptionEnd(i.EndReason),
		ReconnectAttempts: i.ReconnectAttempts,
		StartEventId:      i.StartEventID,
		EndEventId:        i.EndEventID,
		Reason:            i.Reason,
	}
}

func interruptionInfos(rows []*model.StreamInterruption) []*rpc.StreamInterruptionInfo {
	out := make([]*rpc.StreamInterruptionInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, interruptionInfo(r))
	}
	return out
}

func healthSamples(rows []*model.StreamHealthReport) []*rpc.HealthSample {
	out := make([]*rpc.HealthSample, 0, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		out = append(out, &rpc.HealthSample{
			OccurredAt:      r.OccurredAt,
			VideoBitrateBps: r.VideoBitrateBps,
			AudioBitrateBps: r.AudioBitrateBps,
			FpsX100:         r.FpsX100,
			PacketLossPpm:   r.PacketLossPpm,
			RttMs:           r.RttMs,
		})
	}
	return out
}
