package logic

import (
	"context"

	"go-video/services/inbox/internal/svc"
	"go-video/services/inbox/model"
	"go-video/services/inbox/rpc"
)

// convertCategory 把 rpc 枚举转成 model 的分类常量。
func convertCategory(c rpc.Category) int32 {
	return int32(c)
}

// convertMsgType 把 rpc 枚举转成 model 的载体类型。
func convertMsgType(t rpc.MsgType) int32 {
	return int32(t)
}

// toRPCMessage 把收件箱读模型投影成 rpc.Message。
// 领域服务不直接返回数据库对象（AGENTS.md §6）。
func toRPCMessage(row *model.MessageRow) *rpc.Message {
	if row == nil {
		return nil
	}
	return &rpc.Message{
		MsgId:     row.MsgID,
		Category:  rpc.Category(row.Category),
		MsgType:   rpc.MsgType(row.MsgType),
		Title:     row.Title,
		Content:   row.Content,
		SenderMid: row.SenderMid,
		BizType:   row.BizType,
		BizId:     row.BizID,
		Extra:     row.Extra,
		Ctime:     row.Ctime,
		ReadState: rpc.ReadState(row.ReadState),
	}
}

// toRPCMessages 批量投影，nil 元素被跳过。
func toRPCMessages(rows []*model.MessageRow) []*rpc.Message {
	out := make([]*rpc.Message, 0, len(rows))
	for _, row := range rows {
		if m := toRPCMessage(row); m != nil {
			out = append(out, m)
		}
	}
	return out
}

// toRPCUnread 把快照转成 rpc 的 by_category map。
func toRPCUnread(snapshot map[int32]int64) map[int32]int64 {
	out := make(map[int32]int64, len(snapshot))
	for category, n := range snapshot {
		out[category] = n
	}
	return out
}

// unreadTotal 读取用户当前未读总数。
// 写接口必须返回权威计数，因此这里不静默降级为 0（区别于列表接口的附加信息）。
func unreadTotal(ctx context.Context, svcCtx *svc.ServiceContext, mid int64) (int64, error) {
	snapshot, err := svcCtx.Repository.GetUnread(ctx, mid, false)
	if err != nil {
		return 0, err
	}
	return snapshot.Total, nil
}
