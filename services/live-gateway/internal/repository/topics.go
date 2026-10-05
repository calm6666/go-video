package repository

import (
	"fmt"
	"sort"
	"strings"

	"go-video/services/live-gateway/model"
)

// 订阅子通道词表（proto 只写了 danmaku/state/interaction 三个示例串，无枚举）。
// 这里是本服务唯一的词表事实源：拼错的 topic 必须**拒绝**而不是静默忽略——
// 静默忽略的表现是「客户端以为订了 anchor_tip 结果永远收不到」，排查成本极高。
const (
	TopicDanmaku     = "danmaku"
	TopicState       = "state"
	TopicInteraction = "interaction"
	TopicSystem      = "system"
	TopicModeration  = "moderation"
	TopicAnchorTip   = "anchor_tip"
)

// defaultTopics 是 topics 为空时的默认全集（观众态连接实际需要的三类）。
// 刻意不含 moderation / anchor_tip：这两个通道的订阅要按角色门禁，默认给等于绕过它。
var defaultTopics = []string{TopicDanmaku, TopicState, TopicInteraction}

// topicKinds 把子通道映射到广播类别，供订阅门禁与扇出过滤共用同一份口径。
var topicKinds = map[string]int32{
	TopicDanmaku:     model.KindDanmaku,
	TopicState:       model.KindRoomState,
	TopicInteraction: model.KindInteraction,
	TopicSystem:      model.KindSystem,
	TopicModeration:  model.KindModeration,
	TopicAnchorTip:   model.KindAnchorTip,
}

// DefaultTopics 返回默认订阅集合（副本，调用方可随意改）。
func DefaultTopics() []string {
	out := append([]string(nil), defaultTopics...)
	sort.Strings(out)
	return out
}

// ValidTopic 判断子通道是否在词表内。
func ValidTopic(topic string) bool {
	_, ok := topicKinds[strings.TrimSpace(topic)]
	return ok
}

// TopicKind 返回子通道对应的广播类别；未知 topic 返回 KindUnspecified。
func TopicKind(topic string) int32 {
	if k, ok := topicKinds[strings.TrimSpace(topic)]; ok {
		return k
	}
	return model.KindUnspecified
}

// NormalizeTopics 归一订阅/投递的子通道列表：
//  1. 去空白、小写、去重；
//  2. 空列表回落默认全集；
//  3. 数量超过 maxTopics 直接拒（防把 topics 当成广播放大器参数）；
//  4. 未知 topic 拒绝并指名是哪一个（回显 topic 名是安全的：它来自调用方自己的入参）。
func NormalizeTopics(topics []string, maxTopics int32) ([]string, error) {
	if len(topics) == 0 {
		return DefaultTopics(), nil
	}
	if maxTopics <= 0 {
		maxTopics = int32(len(defaultTopics))
	}
	if int32(len(topics)) > maxTopics {
		return nil, fmt.Errorf("%w: %d topics, max %d", ErrTooManyTopics, len(topics), maxTopics)
	}
	seen := make(map[string]struct{}, len(topics))
	out := make([]string, 0, len(topics))
	for _, raw := range topics {
		t := strings.ToLower(strings.TrimSpace(raw))
		if t == "" {
			return nil, fmt.Errorf("%w: empty topic in list", ErrUnknownTopic)
		}
		if !ValidTopic(t) {
			return nil, fmt.Errorf("%w: %q", ErrUnknownTopic, t)
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	sort.Strings(out)
	return out, nil
}

// TopicGateByRole 判定角色 role 是否有权订阅 topic：
// 门禁依据是「该类消息谁能发」（model.RoleAllowedToSend）——能发才谈得上能订，
// 否则任何观众都能订到 moderation 通道，把处置消息当成免费的情报流。
func TopicGateByRole(topic string, role int32) error {
	kind := TopicKind(topic)
	if kind == model.KindUnspecified {
		return fmt.Errorf("%w: %q", ErrUnknownTopic, topic)
	}
	if !model.RoleAllowedToSend(role, kind) {
		return fmt.Errorf("%w: role %d cannot subscribe topic %q", model.ErrPermissionDenied, role, topic)
	}
	return nil
}

// TopicsMatchingKind 从订阅集合里挑出属于该广播类别的子通道（扇出过滤用）。
// 返回空切片表示该连接没订这一类，调用方据此把它从投递目标里剔除。
func TopicsMatchingKind(topics []string, kind int32) []string {
	out := make([]string, 0, len(topics))
	for _, t := range topics {
		if TopicKind(t) == kind {
			out = append(out, t)
		}
	}
	return out
}
