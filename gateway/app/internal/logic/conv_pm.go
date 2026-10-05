// 本文件是 gateway/app 的手写转换扩展（非 goctl 生成产物）：private-message RPC → 客户端投影。
//
// 投影原则（AGENTS.md §6）：网关只做 DTO→响应的字段搬运与裁剪，不在此判定可见性、
// 不解密、不改写正文占位文案；服务侧已经过滤过的字段（last_preview、content 占位）
// 原样透出。枚举一律降为 int32，客户端拿不到 protobuf 类型。

package logic

import (
	"fmt"

	"go-video/gateway/app/internal/types"
	privatemessagerpc "go-video/services/private-message/rpc"

	"google.golang.org/protobuf/proto"
)

// pmConversationsToAPI 会话列表投影。nil 元素由 protobuf getter 兜底成零值，
// 但列表本身必须返回非 nil 切片，避免端上把「空页」解析成 null。
func pmConversationsToAPI(list []*privatemessagerpc.ConversationInfo) []types.PmConversation {
	out := make([]types.PmConversation, 0, len(list))
	for _, c := range list {
		out = append(out, types.PmConversation{
			ConversationId: c.GetConversationId(),
			PeerMid:        c.GetPeerMid(),
			State:          c.GetState(),
			LastMsgId:      c.GetLastMsgId(),
			LastSeq:        c.GetLastSeq(),
			LastMsgType:    c.GetLastMsgType(),
			LastPreview:    c.GetLastPreview(),
			LastMsgTime:    c.GetLastMsgTime(),
			ReadSeq:        c.GetReadSeq(),
			UnreadCount:    c.GetUnreadCount(),
			Hidden:         c.GetHidden(),
			Ctime:          c.GetCtime(),
		})
	}
	return out
}

// pmMessagesToAPI 消息分页投影。seq 是端上做增量合并与游标续拉的稳定键，
// client_msg_id 原样回显以便客户端把幂等重发的本地消息与库内行对齐。
func pmMessagesToAPI(list []*privatemessagerpc.MessageInfo) []types.PmMessage {
	out := make([]types.PmMessage, 0, len(list))
	for _, m := range list {
		out = append(out, types.PmMessage{
			MsgId:          m.GetMsgId(),
			ConversationId: m.GetConversationId(),
			Seq:            m.GetSeq(),
			SenderMid:      m.GetSenderMid(),
			MsgType:        m.GetMsgType(),
			Content:        m.GetContent(),
			MediaRef:       m.GetMediaRef(),
			State:          m.GetState(),
			AuditTaskId:    m.GetAuditTaskId(),
			ClientMsgId:    m.GetClientMsgId(),
			Ctime:          m.GetCtime(),
			WithdrawTime:   m.GetWithdrawTime(),
		})
	}
	return out
}

func pmSettingToAPI(p *privatemessagerpc.UserSettingInfo) types.PmUserSetting {
	if p == nil {
		return types.PmUserSetting{}
	}
	return types.PmUserSetting{
		Mid:              p.GetMid(),
		AllowFrom:        int32(p.GetAllowFrom()),
		RejectStranger:   p.GetRejectStranger(),
		KeywordFilter:    p.GetKeywordFilter(),
		MuteConversation: p.GetMuteConversation(),
		Mtime:            p.GetMtime(),
	}
}

// pmTristate 把终端的三态开关（0 不修改、1 开启、2 关闭）映射到 proto3 optional bool。
// proto 用 optional 区分「未传」与「传 false」，HTTP form 无法表达，因此网关做显式编码；
// 非法取值直接拒绝，绝不静默当成「不修改」而丢掉用户的关闭操作。
func pmTristate(field string, v int32) (*bool, error) {
	switch v {
	case 0:
		return nil, nil
	case 1:
		return proto.Bool(true), nil
	case 2:
		return proto.Bool(false), nil
	default:
		return nil, fmt.Errorf("%s 取值非法：%d（0 不修改、1 开启、2 关闭）", field, v)
	}
}
