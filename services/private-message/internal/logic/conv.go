// 本文件是 logic 包的手写扩展（model ↔ rpc 投影），不是 goctl 生成产物。
//
// AGENTS.md §4/§6：领域服务不返回数据库原始对象。投影只搬运事实字段，
// 不在此做业务判定；枚举按 model 常量回灌 rpc 枚举（取值已被 proto 锁死）。
//
// 隐私口径（本文件最重要的一条）：model.Message.ContentCipher 永远不会被搬运到响应里，
// 只有调用方在「成员身份 + 可见性门禁」之后解出的明文才进入 MessageInfo.Content。

package logic

import (
	"go-video/services/private-message/model"
	"go-video/services/private-message/rpc"
)

// conversationInfo 把「成员投影行 + 会话主体行」合成某个用户视角的会话。
//
// 未读数与隐藏状态属于成员行（用户侧），会话状态属于主体行（双方共享），
// 两者混在一行响应里是客户端渲染的需要，不是数据所有者边界的松动。
func conversationInfo(m *model.ConversationMember, c *model.Conversation) *rpc.ConversationInfo {
	if m == nil || c == nil {
		return nil
	}
	return &rpc.ConversationInfo{
		ConversationId: m.ConversationID,
		PeerMid:        m.PeerMid,
		State:          c.State,
		LastMsgId:      m.LastMsgID,
		LastSeq:        m.LastSeq,
		LastMsgType:    m.LastMsgType,
		LastPreview:    m.LastPreview,
		LastMsgTime:    m.LastMsgTime,
		ReadSeq:        m.ReadSeq,
		UnreadCount:    m.Unread(),
		Hidden:         m.HideState == model.HideStateHidden,
		Ctime:          c.CreatedAt,
	}
}

// messageInfo 把消息行投影为 rpc.MessageInfo。
//
// plaintext 只允许是「已通过成员与可见性门禁」的解密结果；
// 不可见的行由 contentPlaceholder 给出固定文案，media_ref 同步隐去
// （已撤回/驳回的消息仍回传 asset 主键，会让端上继续拉到媒体，等于绕过撤回）。
func messageInfo(msg *model.Message, plaintext string, hideMedia bool) *rpc.MessageInfo {
	if msg == nil {
		return nil
	}
	mediaRef := msg.MediaRef
	if hideMedia {
		mediaRef = ""
	}
	return &rpc.MessageInfo{
		MsgId:          msg.MsgID,
		ConversationId: msg.ConversationID,
		Seq:            msg.Seq,
		SenderMid:      msg.SenderMid,
		MsgType:        msg.MsgType,
		Content:        plaintext,
		MediaRef:       mediaRef,
		State:          msg.State,
		AuditTaskId:    msg.AuditTaskID,
		ClientMsgId:    msg.ClientMsgID,
		Ctime:          msg.Ctime,
		WithdrawTime:   msg.WithdrawTime,
	}
}

// reportInfo 举报记录的运营侧投影：只给主键、原因码、状态与时间，绝不给正文或摘要。
func reportInfo(r *model.Report) *rpc.ReportInfo {
	if r == nil {
		return nil
	}
	return &rpc.ReportInfo{
		ReportId:       r.ReportID,
		ConversationId: r.ConversationID,
		MsgId:          r.MsgID,
		ReporterMid:    r.ReporterMid,
		TargetMid:      r.TargetMid,
		Reason:         r.Reason,
		Description:    displayUserText(r.Description),
		State:          r.State,
		AuditTaskId:    r.AuditTaskID,
		Handler:        r.Handler,
		HandleNote:     r.HandleNote,
		Ctime:          r.Ctime,
		Mtime:          r.Mtime,
	}
}

func reportInfoList(rows []*model.Report) []*rpc.ReportInfo {
	out := make([]*rpc.ReportInfo, 0, len(rows))
	for _, r := range rows {
		if info := reportInfo(r); info != nil {
			out = append(out, info)
		}
	}
	return out
}

// settingInfo 偏好投影。row 必须已由调用方补齐（缺行时用 defaultSettingFor 合成），
// 因此 mtime=0 就代表「这一份是站点级缺省，用户从未设置过」，端上据此区分两种默认。
func settingInfo(row *model.UserSetting) *rpc.UserSettingInfo {
	if row == nil {
		return &rpc.UserSettingInfo{AllowFrom: rpc.AllowFrom_ALLOW_FROM_UNSPECIFIED}
	}
	return &rpc.UserSettingInfo{
		Mid:              row.Mid,
		AllowFrom:        rpc.AllowFrom(row.AllowFrom),
		RejectStranger:   row.RejectStranger != 0,
		KeywordFilter:    row.KeywordFilter != 0,
		MuteConversation: row.MuteConversation != 0,
		Mtime:            row.UpdatedAt,
	}
}
