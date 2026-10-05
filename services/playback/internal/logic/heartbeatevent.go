package logic

// 本文件手写 playback.heartbeat.v1 事件的 payload 组装（领域策略，非 goctl 生成产物）。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"go-video/services/playback/model"
	"go-video/services/playback/rpc"
)

// midHashDomain 是用户标识摘要的域分离前缀。
// 它不是密钥（真正的隐私控制是摘要不可逆），只保证同一 mid 在
// 播放链路里得到的 pseudonym 与别的链路不通用，避免跨域重识别。
const midHashDomain = "go-video:playback:mid:v1:"

// heartbeatPayload 是 playback.heartbeat.v1 的事件负载。
//
// 隐私与最小化（AGENTS.md §7、docs/data-design.md §6）：
//   - 不含客户端 IP、auth_key、播放签名地址等任何凭据；
//   - 用户标识只以 mid_hash（SHA-256 摘要前 32 位）出现；
//   - 内容主键（content_type/content_id/vid）保留，供 SPM 计算完播率与热度。
type heartbeatPayload struct {
	SessionID      string  `json:"session_id"`      // 播放会话 ID
	ContentType    int32   `json:"content_type"`    // 1 UGC、2 PGC
	ContentID      int64   `json:"content_id"`      // 内容主键
	Vid            string  `json:"vid,omitempty"`   // UGC bvid
	MidHash        string  `json:"mid_hash"`        // 观看者假名（摘要，非明文 mid）
	Guest          bool    `json:"guest"`           // 是否游客（mid<=0）
	Platform       int32   `json:"platform"`        // 客户端平台
	AppVersion     string  `json:"app_version"`     // 客户端版本
	Region         string  `json:"region"`          // 地区代码
	PositionMs     int64   `json:"position_ms"`     // 本次上报位置（毫秒）
	DurationMs     int64   `json:"duration_ms"`     // 内容总时长（毫秒）
	MaxPositionMs  int64   `json:"max_position_ms"` // 服务端累计最大位置（断点）
	BufferCount    int32   `json:"buffer_count"`    // 本次上报的累计卡顿次数
	AvgBitrate     int64   `json:"avg_bitrate"`     // 累计平均码率（bps）
	LastError      int32   `json:"last_error"`      // 最近一次播放错误码
	Completion     float64 `json:"completion"`      // 完播比例 [0,1]，SPM 直接用于热度特征
	SessionExpired bool    `json:"session_expired"` // 迟到的心跳：授权已过期但进度仍需保留
}

// hashMid 返回观看者的假名标识（SHA-256 十六进制前 32 位）。
// mid <= 0 表示游客，返回空串：游客本就没有可区分标识，
// 若对 0 取摘要会让所有游客共享同一个假名，反而误导下游做用户级去重。
func hashMid(mid int64) string {
	if mid <= 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(midHashDomain + strconv.FormatInt(mid, 10)))
	return hex.EncodeToString(sum[:])[:32]
}

// completionRatio 计算完播比例：总时长非正或位置超过总时长时收敛到 [0,1]。
func completionRatio(positionMs, durationMs int64) float64 {
	if durationMs <= 0 || positionMs <= 0 {
		return 0
	}
	if positionMs >= durationMs {
		return 1
	}
	return float64(positionMs) / float64(durationMs)
}

// buildHeartbeatPayload 组装并序列化事件负载。
// maxPositionMs 是幂等 upsert 之后服务端记录的最大位置，保证事件与库里一致。
func buildHeartbeatPayload(s *model.PlaybackSession, in *rpc.ReportHeartbeatReq,
	maxPositionMs int64, now int64) (json.RawMessage, error) {
	p := heartbeatPayload{
		SessionID:      s.SessionId,
		ContentType:    s.ContentType,
		ContentID:      s.ContentId,
		Vid:            s.Vid,
		MidHash:        hashMid(s.Mid),
		Guest:          s.Mid <= 0,
		Platform:       s.Platform,
		AppVersion:     s.AppVersion,
		Region:         s.Region,
		PositionMs:     in.GetPositionMs(),
		DurationMs:     in.GetDurationMs(),
		MaxPositionMs:  maxPositionMs,
		BufferCount:    in.GetBufferCount(),
		AvgBitrate:     in.GetAvgBitrate(),
		LastError:      in.GetLastError(),
		Completion:     completionRatio(in.GetPositionMs(), in.GetDurationMs()),
		SessionExpired: s.ExpireAt <= now,
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("playback/logic marshal heartbeat payload: %w", err)
	}
	return raw, nil
}
