// 本文件是 gateway/app 的手写转换扩展（非 goctl 生成产物）：comment RPC → 客户端投影。

package logic

import (
	"go-video/gateway/app/internal/types"
	commentrpc "go-video/services/comment/rpc"
)

// commentItemToAPI 投影单条评论。state 在契约中即 int32（参见 CommentInfo.state），
// 这里不重新解释语义：折叠/待审/驳回等判定全部来自 comment 服务。
func commentItemToAPI(c *commentrpc.CommentInfo) types.CommentItem {
	return types.CommentItem{
		Rpid:       c.GetRpid(),
		Oid:        c.GetOid(),
		Tp:         c.GetTp(),
		Root:       c.GetRoot(),
		Parent:     c.GetParent(),
		Mid:        c.GetMid(),
		Content:    c.GetContent(),
		State:      c.GetState(),
		Ctime:      c.GetCtime(),
		Mtime:      c.GetMtime(),
		LikeCount:  c.GetLikeCount(),
		ReplyCount: c.GetReplyCount(),
	}
}

// commentListToAPI 投影根评论/楼中楼列表；nil 入参返回空切片，
// 保证客户端拿到 [] 而不是 null，可直接做列表渲染。
func commentListToAPI(list []*commentrpc.CommentInfo) []types.CommentItem {
	out := make([]types.CommentItem, 0, len(list))
	for _, c := range list {
		out = append(out, commentItemToAPI(c))
	}
	return out
}
