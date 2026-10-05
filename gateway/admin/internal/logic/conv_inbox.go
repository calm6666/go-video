// 本文件是 gateway/admin 的手写转换扩展（非 goctl 生成产物）：inbox RPC → 后台投影。

package logic

import (
	"sort"

	"go-video/gateway/admin/internal/types"
)

// inboxUnreadByCategoryToAPI 把 map<int32,int64> 展平成按分类升序的数组，
// 与 gateway/app 的 /inbox/unread 形状一致，便于后台与终端对账同一份未读数。
func inboxUnreadByCategoryToAPI(byCategory map[int32]int64) []types.AdminInboxUnreadItem {
	keys := make([]int, 0, len(byCategory))
	for k := range byCategory {
		keys = append(keys, int(k))
	}
	sort.Ints(keys)
	out := make([]types.AdminInboxUnreadItem, 0, len(keys))
	for _, k := range keys {
		out = append(out, types.AdminInboxUnreadItem{Category: int32(k), Count: byCategory[int32(k)]})
	}
	return out
}
