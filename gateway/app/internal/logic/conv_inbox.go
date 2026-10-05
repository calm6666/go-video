// 本文件是 gateway/app 的手写转换扩展（非 goctl 生成产物）：inbox RPC → 客户端投影。

package logic

import (
	"sort"

	"go-video/gateway/app/internal/types"
	inboxrpc "go-video/services/inbox/rpc"
)

func inboxMessagesToAPI(list []*inboxrpc.Message) []types.InboxMessage {
	out := make([]types.InboxMessage, 0, len(list))
	for _, m := range list {
		out = append(out, types.InboxMessage{
			MsgId:     m.GetMsgId(),
			Category:  int32(m.GetCategory()),
			MsgType:   int32(m.GetMsgType()),
			Title:     m.GetTitle(),
			Content:   m.GetContent(),
			SenderMid: m.GetSenderMid(),
			BizType:   m.GetBizType(),
			BizId:     m.GetBizId(),
			Extra:     m.GetExtra(),
			Ctime:     m.GetCtime(),
			ReadState: int32(m.GetReadState()),
		})
	}
	return out
}

// inboxUnreadByCategoryToAPI 把 map<int32,int64> 展平成按分类升序的数组，
// 保证同一份数据在不同响应里字段顺序稳定（客户端可直接做 diff 渲染）。
func inboxUnreadByCategoryToAPI(byCategory map[int32]int64) []types.InboxCategoryUnread {
	keys := make([]int, 0, len(byCategory))
	for k := range byCategory {
		keys = append(keys, int(k))
	}
	sort.Ints(keys)
	out := make([]types.InboxCategoryUnread, 0, len(keys))
	for _, k := range keys {
		out = append(out, types.InboxCategoryUnread{Category: int32(k), Count: byCategory[int32(k)]})
	}
	return out
}
