// 本文件是 gateway/app 的手写转换扩展（非 goctl 生成产物）：notification RPC → 客户端投影。

package logic

import (
	"go-video/gateway/app/internal/types"
	notificationrpc "go-video/services/notification/rpc"
)

// dndPreferenceToAPI 把 Channel 枚举数组投影为 int32 数组（客户端按通道码渲染开关）。
// muted_channels 为空表示所有通道均允许，投影为 [] 而不是 null。
func dndPreferenceToAPI(p *notificationrpc.DndPreference) types.NotifyDndPreference {
	channels := p.GetMutedChannels()
	out := types.NotifyDndPreference{
		Mid:           p.GetMid(),
		MutedChannels: make([]int32, 0, len(channels)),
		QuietStart:    p.GetQuietStart(),
		QuietEnd:      p.GetQuietEnd(),
		Timezone:      p.GetTimezone(),
		Enabled:       p.GetEnabled(),
		Ctime:         p.GetCtime(),
		Mtime:         p.GetMtime(),
	}
	for _, ch := range channels {
		out.MutedChannels = append(out.MutedChannels, int32(ch))
	}
	return out
}

// dndChannelsFromParam 把客户端通道码转为 protobuf Channel 枚举。
// 通道码合法性（是否在枚举内）由 notification 服务判定，网关只做类型转换。
func dndChannelsFromParam(channels []int32) []notificationrpc.Channel {
	out := make([]notificationrpc.Channel, 0, len(channels))
	for _, ch := range channels {
		out = append(out, notificationrpc.Channel(ch))
	}
	return out
}
