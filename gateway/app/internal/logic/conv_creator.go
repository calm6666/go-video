// 本文件是 gateway/app 的手写转换扩展（非 goctl 生成产物）：creator RPC → 客户端投影。

package logic

import (
	"go-video/gateway/app/internal/types"
	creatorrpc "go-video/services/creator/rpc"
)

// upSpecialToAPI 投影 UP 主特殊分组 ID 列表；未设置特殊属性时返回空切片而非 null。
func upSpecialToAPI(s *creatorrpc.UpSpecial) types.UpSpecialInfo {
	ids := s.GetGroupIds()
	out := types.UpSpecialInfo{GroupIds: make([]int64, 0, len(ids))}
	out.GroupIds = append(out.GroupIds, ids...)
	return out
}

// upsSpecialToAPI 投影 mid → 特殊属性 map（响应类型仍是 map，故无需排序；
// encoding/json 对 map key 的排序保证输出稳定）。
// 未命中的 mid 由 creator 服务省略，网关不补零值，避免未返回被误读成无分组。
func upsSpecialToAPI(m map[int64]*creatorrpc.UpSpecial) map[int64]types.UpSpecialInfo {
	out := make(map[int64]types.UpSpecialInfo, len(m))
	for mid, s := range m {
		out[mid] = upSpecialToAPI(s)
	}
	return out
}
