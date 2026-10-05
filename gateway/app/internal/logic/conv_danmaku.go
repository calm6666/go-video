// 本文件是 gateway/app 的手写转换扩展（非 goctl 生成产物）：danmaku RPC → 客户端投影。

package logic

import (
	"go-video/gateway/app/internal/types"
	danmakurpc "go-video/services/danmaku/rpc"
)

func danmakuInfoToAPI(d *danmakurpc.DanmakuInfo) types.DanmakuInfo {
	return types.DanmakuInfo{
		Dmid:       d.GetDmid(),
		Oid:        d.GetOid(),
		Aid:        d.GetAid(),
		Mid:        d.GetMid(),
		ProgressMs: d.GetProgressMs(),
		Mode:       d.GetMode(),
		Fontsize:   d.GetFontsize(),
		Color:      d.GetColor(),
		Content:    d.GetContent(),
		State:      d.GetState(),
		Pool:       d.GetPool(),
		SegNo:      d.GetSegNo(),
		Ctime:      d.GetCtime(),
	}
}

func danmakuInfoListToAPI(list []*danmakurpc.DanmakuInfo) []types.DanmakuInfo {
	out := make([]types.DanmakuInfo, 0, len(list))
	for _, d := range list {
		out = append(out, danmakuInfoToAPI(d))
	}
	return out
}

func danmakuSegmentCountsToAPI(list []*danmakurpc.SegmentCount) []types.DanmakuSegmentCount {
	out := make([]types.DanmakuSegmentCount, 0, len(list))
	for _, s := range list {
		out = append(out, types.DanmakuSegmentCount{SegNo: s.GetSegNo(), Count: s.GetCount()})
	}
	return out
}

func danmakuUserBlocksToAPI(list []*danmakurpc.UserBlockInfo) []types.DanmakuUserBlockInfo {
	out := make([]types.DanmakuUserBlockInfo, 0, len(list))
	for _, b := range list {
		out = append(out, types.DanmakuUserBlockInfo{
			Id:         b.GetId(),
			Mid:        b.GetMid(),
			Type:       b.GetType(),
			BlockedMid: b.GetBlockedMid(),
			Keyword:    b.GetKeyword(),
			State:      b.GetState(),
			Ctime:      b.GetCtime(),
		})
	}
	return out
}
