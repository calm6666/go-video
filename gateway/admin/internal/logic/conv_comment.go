// 本文件是 gateway/admin 的手写投影扩展（非 goctl 生成产物）：comment RPC → 运营后台投影。
//
// 网关在这里只做三件事（AGENTS.md §4/§5/§6）：
//  1. 枚举升维：后台 JSON 只有 int32，comment.v1.SortMode 是独立 Go 类型，必须显式转换，
//     并且不把未知值透传给下游（服务侧会把未知 sort 静默当成热度，后台会以为筛选失效）；
//  2. 分页口径对齐：comment 服务对 ps 是「越界即拒绝」（model.ErrPsTooLarge，上限 49 与 obc 一致），
//     因此网关先把 ps 归一进 [1,49]，让后台的页大小选择器不可能构造出必然失败的请求；
//  3. 投影：把 RPC 消息搬成后台 types，CommentInfo.state 在契约里本就是 int32，
//     网关原样透出，不解释「折叠/待审/驳回」的含义。
//
// 评论内容合法性、是否可删、能否置顶、计数如何统计全部留在 comment 服务；
// 运营删除的 admin=true 只代表「以管理员身份执行」，审计证据由服务侧落库（AGENTS.md §8）。

package logic

import (
	"fmt"

	"go-video/common/validation"
	"go-video/gateway/admin/internal/types"
	commentrpc "go-video/services/comment/rpc"
)

const (
	// commentMaxPageSize 与 services/comment 的 ListComments/ListReplies 的 ps 上限一致
	// （proto 注释「最大 49，与 obc 一致」），网关不替后台放大页大小。
	commentMaxPageSize = 49
	// commentDefaultPageSize 同服务侧与 admin.api（ps,default=20）的缺省页大小：
	// NormalizePage 在 ps<=0 时正是回落到 20，两边口径一致。
	commentDefaultPageSize = 20
)

// normalizeCommentPage 复用 common/validation.NormalizePage（pn<=0→1、ps<=0→20、ps>49→49）。
func normalizeCommentPage(pn, ps int32) (int32, int32) {
	page := validation.NormalizePage(int(pn), int(ps), commentMaxPageSize)
	return int32(page.Page), int32(page.PageSize)
}

// commentSortMode 校验排序枚举，只接受 comment.v1.SortMode 已登记的值。
// SORT_UNSPECIFIED(0) 与 SORT_HOT(1) 在服务侧同为热度序，因此允许后台传 0。
func commentSortMode(v int32) (commentrpc.SortMode, error) {
	if _, ok := commentrpc.SortMode_name[v]; !ok {
		return 0, fmt.Errorf("gateway/admin: invalid sort %d", v)
	}
	return commentrpc.SortMode(v), nil
}

// commentItemToAPI 投影单条评论。nil 时返回零值条目：pb getter 对 nil 安全，
// 网关不伪造 rpid，也不把「没数据」当成错误。
func commentItemToAPI(c *commentrpc.CommentInfo) types.AdminCommentItem {
	return types.AdminCommentItem{
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

// commentListToAPI 投影评论/回复列表，nil 输入返回空切片而不是 null，便于前端直接遍历。
func commentListToAPI(list []*commentrpc.CommentInfo) []types.AdminCommentItem {
	out := make([]types.AdminCommentItem, 0, len(list))
	for _, c := range list {
		out = append(out, commentItemToAPI(c))
	}
	return out
}
