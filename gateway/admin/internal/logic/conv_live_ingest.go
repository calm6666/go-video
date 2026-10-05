// 本文件是 gateway/admin 的手写投影扩展（非 goctl 生成产物）。
//
// live-ingest RPC ↔ 管理后台投影 + 写入口门槛。
//
// 职责边界（AGENTS.md §4/§5/§8），与 conv_live.go（live-room）同一套口径：
//  1. 网关只做三件事：入参形态门槛（主体存在、幂等键非空、数值非负、标识非空）、调用下游、
//     逐字段投影。流状态机能否迁移、密钥是否属于该主播、节点容量与打分、事件重试批次上限、
//     分页与采样上限全部由 services/live-ingest 判定，网关不复算，
//     也不把下游错误改写成看起来成功的空结果；
//  2. 密钥面只投影元数据：live-ingest 的契约保证 StreamKeyInfo 不含明文与哈希
//     （key_hint_tail 是末 4 位辨认串、key_ref 是 Vault 引用），网关因此可以整段回显；
//     反向的边界是 Issue/Rotate 的 plaintext_key 与内嵌明文密钥的 publish_url——
//     那两条路由在 .api 里就没有 HTTP 面，本文件也不为它们留任何投影出口；
//  3. 派生字段只读不写：active_streams / last_heartbeat_at / ctime / mtime 由节点心跳与
//     分配记录派生，后台声明会被 live-ingest 覆盖或拒掉，因此 IngestNodeInput 里根本不出现，
//     组装请求时也保持零值，避免「后台以为改了、服务按上报值算」；
//  4. 列表一律返回非 nil 切片：把 null 与 [] 区分给前端是多余的契约负担。

package logic

import (
	"errors"

	"go-video/gateway/admin/internal/types"
	liveingestrpc "go-video/services/live-ingest/rpc"
)

// errLiveIngestSubjectRequired GetStreamStateReq 的 stream_id / room_id 二选一：
// 两个都不给时下游会去查 stream_id="" 的流，回给后台的是「没有记录」而不是「你少传了参数」。
var errLiveIngestSubjectRequired = errors.New("gateway/admin: stream_id or room_id required")

// errLiveIngestNotConfigured：未配置 LiveIngestRPC 时 live-ingest 域路由一律返回它。
// 不退化成伪造空列表——那会让后台把「下游没接」读成「没有人开播」，
// 进而误判「推流链路全都正常」。
var errLiveIngestNotConfigured = errors.New("live-ingest service not configured")

// liveIngestAdminScope 是本运营面对 ListStreamsReq.admin / ListStreamKeysReq.admin 的取值。
//
// 它是**网关断言，不是表单字段**：.api 里刻意不出现 admin，因为「我是运营」不能由请求自己声明
// （与 live-room CloseRoom.admin 同一先例）。差别只在写路由有 AdminPermission 兜底，
// 而这两条是只读路由、不挂中间件，因此本常量的正当性来自「这条路由只存在于 /admin/live 前缀下、
// 由 gateway/admin 单独部署」这一部署事实，而不是来自单次请求的鉴权结果。
//
// 为什么不能写 false：live-ingest 的实现会在 admin=false 时把结果收敛成
// anchor_mid == operator_mid 的行（普通主播只能看自己的密钥与流），
// 后台巡检页于是会拿到一份「看起来是空」的清单，比报错更难排查。
// 信任边界与残余风险记在 gateway/admin/README.md。
const liveIngestAdminScope = true

// liveRequiredText 拦住「必填字符串标识缺失」：live-ingest 的 GetStreamHealth / ListStreamEvents
// 只有 stream_id 一个主体位，传空串只会落到一条不存在的流上，回给后台的是空结果而不是
// 「你少传了参数」。值本身不做规范化——stream_id 是 ULID，任何改写都会让它对不上建档记录。
func liveRequiredText(field, v string) error {
	if v == "" {
		return errors.New("gateway/admin: " + field + " required")
	}
	return nil
}

// liveStreamSubjectGate：GetStreamStateReq 与 ListStreamInterruptionsReq 都是
// stream_id / room_id 二选一。两个都留空时下游按「空 stream_id」查询，
// 结果是「没有匹配记录」，后台会误读成「这个房间没开过播」。
func liveStreamSubjectGate(streamID string, roomID int64) error {
	if streamID == "" && roomID <= 0 {
		return errLiveIngestSubjectRequired
	}
	return liveNonNeg("room_id", roomID)
}

// liveKeySubjectGate：GetStreamKeyReq 同上（key_id / stream_name 二选一）。
func liveKeySubjectGate(keyID int64, streamName string) error {
	if keyID <= 0 && streamName == "" {
		return errors.New("gateway/admin: key_id or stream_name required")
	}
	return liveNonNeg("key_id", keyID)
}

// liveAssignmentSubjectGate：ListNodeAssignmentsReq 按流或按节点查，两者都不给就是全表扫，
// 服务不会替后台限定范围，因此这里就拒。
func liveAssignmentSubjectGate(streamID, nodeID string) error {
	if streamID == "" && nodeID == "" {
		return errors.New("gateway/admin: stream_id or node_id required")
	}
	return nil
}

// liveRequiredID32 是 liveRequiredID 的 int32 版本（scope 这类「必填枚举/主键」位）：
// 0 在这两处都是 UNSPECIFIED，没有任何下游语义，透传只会换来一次「查不到」的往返。
func liveRequiredID32(field string, v int32) error {
	if v <= 0 {
		return errors.New("gateway/admin: " + field + " required")
	}
	return nil
}

// --- rpc → 后台 types 投影 ---

func liveProtocolsToAPI(ps []liveingestrpc.IngestProtocol) []int32 {
	out := make([]int32, 0, len(ps))
	for _, p := range ps {
		out = append(out, int32(p))
	}
	return out
}

func liveProtocolsForRPC(ps []int32) []liveingestrpc.IngestProtocol {
	out := make([]liveingestrpc.IngestProtocol, 0, len(ps))
	for _, p := range ps {
		out = append(out, liveingestrpc.IngestProtocol(p))
	}
	return out
}

// liveStreamToAPI 投影流状态与累计指标。
// health_state 在 StreamInfo 里本来就是 int32（不是枚举），按值投影不二次解释；
// fps 是 ×100 后的整数、packet_loss_ppm 是百万分比，口径由 live-ingest 定义。
func liveStreamToAPI(p *liveingestrpc.StreamInfo) types.LiveStreamInfo {
	if p == nil {
		return types.LiveStreamInfo{}
	}
	return types.LiveStreamInfo{
		StreamId:                p.GetStreamId(),
		KeyId:                   p.GetKeyId(),
		StreamName:              p.GetStreamName(),
		RoomId:                  p.GetRoomId(),
		SessionId:               p.GetSessionId(),
		AnchorMid:               p.GetAnchorMid(),
		Protocol:                int32(p.GetProtocol()),
		NodeId:                  p.GetNodeId(),
		State:                   int32(p.GetState()),
		Seq:                     p.GetSeq(),
		PublishStartedAt:        p.GetPublishStartedAt(),
		StateChangedAt:          p.GetStateChangedAt(),
		LastHeartbeatAt:         p.GetLastHeartbeatAt(),
		InterruptedTotalSeconds: p.GetInterruptedTotalSeconds(),
		InterruptionCount:       p.GetInterruptionCount(),
		StopReason:              int32(p.GetStopReason()),
		HealthState:             p.GetHealthState(),
		HealthReportedAt:        p.GetHealthReportedAt(),
		VideoBitrateBps:         p.GetVideoBitrateBps(),
		AudioBitrateBps:         p.GetAudioBitrateBps(),
		Fps:                     p.GetFps(),
		PacketLossPpm:           p.GetPacketLossPpm(),
		Ctime:                   p.GetCtime(),
		Mtime:                   p.GetMtime(),
	}
}

func liveStreamsToAPI(list []*liveingestrpc.StreamInfo) []types.LiveStreamInfo {
	out := make([]types.LiveStreamInfo, 0, len(list))
	for _, s := range list {
		out = append(out, liveStreamToAPI(s))
	}
	return out
}

// liveStreamKeyToAPI 密钥元数据投影。契约保证上游不会填明文与哈希，
// 因此这里逐字段透传即是安全边界；新增字段时若涉及凭据必须回到本函数显式判断。
func liveStreamKeyToAPI(p *liveingestrpc.StreamKeyInfo) types.StreamKeyInfo {
	if p == nil {
		return types.StreamKeyInfo{}
	}
	return types.StreamKeyInfo{
		KeyId:           p.GetKeyId(),
		StreamName:      p.GetStreamName(),
		KeyHintTail:     p.GetKeyHintTail(),
		KeyRef:          p.GetKeyRef(),
		State:           int32(p.GetState()),
		Version:         p.GetVersion(),
		PrevKeyId:       p.GetPrevKeyId(),
		Protocols:       liveProtocolsToAPI(p.GetProtocols()),
		RoomId:          p.GetRoomId(),
		SessionId:       p.GetSessionId(),
		AnchorMid:       p.GetAnchorMid(),
		ExpireAt:        p.GetExpireAt(),
		GraceUntil:      p.GetGraceUntil(),
		CurrentStreamId: p.GetCurrentStreamId(),
		RotateToKeyId:   p.GetRotateToKeyId(),
		Reason:          p.GetReason(),
		Ctime:           p.GetCtime(),
		Mtime:           p.GetMtime(),
	}
}

func liveStreamKeysToAPI(list []*liveingestrpc.StreamKeyInfo) []types.StreamKeyInfo {
	out := make([]types.StreamKeyInfo, 0, len(list))
	for _, k := range list {
		out = append(out, liveStreamKeyToAPI(k))
	}
	return out
}

func liveHealthSamplesToAPI(list []*liveingestrpc.HealthSample) []types.LiveHealthSample {
	out := make([]types.LiveHealthSample, 0, len(list))
	for _, s := range list {
		if s == nil {
			out = append(out, types.LiveHealthSample{})
			continue
		}
		out = append(out, types.LiveHealthSample{
			OccurredAt:      s.GetOccurredAt(),
			VideoBitrateBps: s.GetVideoBitrateBps(),
			AudioBitrateBps: s.GetAudioBitrateBps(),
			FpsX100:         s.GetFpsX100(),
			PacketLossPpm:   s.GetPacketLossPpm(),
			RttMs:           s.GetRttMs(),
		})
	}
	return out
}

func liveIngestNodeToAPI(p *liveingestrpc.IngestNodeInfo) types.IngestNodeInfo {
	if p == nil {
		return types.IngestNodeInfo{}
	}
	return types.IngestNodeInfo{
		NodeId:          p.GetNodeId(),
		Name:            p.GetName(),
		Region:          p.GetRegion(),
		Protocols:       liveProtocolsToAPI(p.GetProtocols()),
		EndpointRtmp:    p.GetEndpointRtmp(),
		EndpointSrt:     p.GetEndpointSrt(),
		EndpointWebrtc:  p.GetEndpointWebrtc(),
		State:           int32(p.GetState()),
		CapacityStreams: p.GetCapacityStreams(),
		ActiveStreams:   p.GetActiveStreams(),
		HealthScore:     p.GetHealthScore(),
		LastHeartbeatAt: p.GetLastHeartbeatAt(),
		Labels:          p.GetLabels(),
		Ctime:           p.GetCtime(),
		Mtime:           p.GetMtime(),
	}
}

func liveIngestNodesToAPI(list []*liveingestrpc.IngestNodeInfo) []types.IngestNodeInfo {
	out := make([]types.IngestNodeInfo, 0, len(list))
	for _, n := range list {
		out = append(out, liveIngestNodeToAPI(n))
	}
	return out
}

func liveNodeAssignmentsToAPI(list []*liveingestrpc.NodeAssignmentInfo) []types.NodeAssignmentInfo {
	out := make([]types.NodeAssignmentInfo, 0, len(list))
	for _, a := range list {
		if a == nil {
			out = append(out, types.NodeAssignmentInfo{})
			continue
		}
		out = append(out, types.NodeAssignmentInfo{
			AssignmentId: a.GetAssignmentId(),
			StreamId:     a.GetStreamId(),
			RoomId:       a.GetRoomId(),
			NodeId:       a.GetNodeId(),
			Protocol:     int32(a.GetProtocol()),
			State:        int32(a.GetState()),
			Score:        a.GetScore(),
			PrevNodeId:   a.GetPrevNodeId(),
			AssignedAt:   a.GetAssignedAt(),
			ReleasedAt:   a.GetReleasedAt(),
			Reason:       a.GetReason(),
			TraceId:      a.GetTraceId(),
		})
	}
	return out
}

func liveStreamInterruptionsToAPI(list []*liveingestrpc.StreamInterruptionInfo) []types.StreamInterruptionInfo {
	out := make([]types.StreamInterruptionInfo, 0, len(list))
	for _, i := range list {
		if i == nil {
			out = append(out, types.StreamInterruptionInfo{})
			continue
		}
		out = append(out, types.StreamInterruptionInfo{
			InterruptionId:    i.GetInterruptionId(),
			StreamId:          i.GetStreamId(),
			RoomId:            i.GetRoomId(),
			EpisodeNo:         i.GetEpisodeNo(),
			NodeId:            i.GetNodeId(),
			StartedAt:         i.GetStartedAt(),
			EndedAt:           i.GetEndedAt(),
			DurationSeconds:   i.GetDurationSeconds(),
			EndReason:         int32(i.GetEndReason()),
			ReconnectAttempts: i.GetReconnectAttempts(),
			StartEventId:      i.GetStartEventId(),
			EndEventId:        i.GetEndEventId(),
			Reason:            i.GetReason(),
		})
	}
	return out
}

func liveStreamEventsToAPI(list []*liveingestrpc.StreamEventInfo) []types.StreamEventInfo {
	out := make([]types.StreamEventInfo, 0, len(list))
	for _, e := range list {
		if e == nil {
			out = append(out, types.StreamEventInfo{})
			continue
		}
		out = append(out, types.StreamEventInfo{
			EventId:            e.GetEventId(),
			StreamId:           e.GetStreamId(),
			RoomId:             e.GetRoomId(),
			SessionId:          e.GetSessionId(),
			Seq:                e.GetSeq(),
			FromState:          int32(e.GetFromState()),
			ToState:            int32(e.GetToState()),
			NodeId:             e.GetNodeId(),
			InterruptionId:     e.GetInterruptionId(),
			InterruptedSeconds: e.GetInterruptedSeconds(),
			StopReason:         int32(e.GetStopReason()),
			Reason:             e.GetReason(),
			OccurredAt:         e.GetOccurredAt(),
			Ctime:              e.GetCtime(),
		})
	}
	return out
}

// liveIngestNodeForRPC 把后台的节点可写字段白名单组装成 protobuf IngestNodeInfo。
// active_streams / last_heartbeat_at / ctime / mtime 保持零值：它们分别是节点上报观测量与
// 服务维护时钟，由 live-ingest 在 upsert 里按「不覆盖」语义处理（heartbeat_only=false 路径）。
func liveIngestNodeForRPC(in types.IngestNodeInput) *liveingestrpc.IngestNodeInfo {
	return &liveingestrpc.IngestNodeInfo{
		NodeId:          in.NodeId,
		Name:            in.Name,
		Region:          in.Region,
		Protocols:       liveProtocolsForRPC(in.Protocols),
		EndpointRtmp:    in.EndpointRtmp,
		EndpointSrt:     in.EndpointSrt,
		EndpointWebrtc:  in.EndpointWebrtc,
		State:           liveingestrpc.IngestNodeState(in.State),
		CapacityStreams: in.CapacityStreams,
		HealthScore:     in.HealthScore,
		Labels:          in.Labels,
	}
}
