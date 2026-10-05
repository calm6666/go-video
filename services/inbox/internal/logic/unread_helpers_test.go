package logic

// 未读计数与状态推进用例的共用布景。
//
// inbox 的未读计数是三层结构：Redis 加速副本 -> inbox_unread_stat 快照 ->
// inbox_user_message 真值。本文件只负责把这三层布成一致状态，
// 「一致」本身就是断言的前提（快照与明细不一致时，重算必须把快照纠正过来）。

import (
	"fmt"
	"testing"

	"go-video/services/inbox/model"
)

// seedThreeUnread 给 Alice 布 3 条未读（系统 2 条、互动 1 条）与一致快照。
func seedThreeUnread(t *testing.T, st *store) {
	t.Helper()
	seedUnreadFor(t, st, midAlice, []int32{
		model.CategorySystem, model.CategorySystem, model.CategoryEngagement,
	})
}

// nextSeedMsgID 只给布景用：从 7300 起，避开 listmessageslogic_test.go 里显式给的 71xx。
var nextSeedMsgID = int64(7300)

// seedUnreadFor 给某用户布 n 条未读（分类按入参顺序），并写一份与明细一致的快照。
// 返回布下去的 msg_id 列表，顺序与 categories 一致。
func seedUnreadFor(t *testing.T, st *store, mid int64, categories []int32) []int64 {
	t.Helper()
	msgIDs := make([]int64, 0, len(categories))
	counts := map[int32]int64{}
	for i, category := range categories {
		nextSeedMsgID++
		msgID := nextSeedMsgID
		ctime := int64(1700000000 + i)
		seedMessage(t, st, &model.InboxMessage{
			MsgID: msgID, Category: category, MsgType: model.MsgTypeText,
			Title: fmt.Sprintf("分类%d的消息", category), Content: "正文",
			BizType: "seed", BizID: fmt.Sprintf("b-%d", msgID), Ctime: ctime,
		})
		seedRow(t, st, &model.InboxUserMessage{
			Mid: mid, MsgID: msgID, Category: category,
			ReadState: model.ReadStateUnread, DelState: model.DelStateNormal, Ctime: ctime,
		})
		counts[category]++
		msgIDs = append(msgIDs, msgID)
	}
	seedStat(t, st, mid, counts, 1700000500)
	return msgIDs
}
