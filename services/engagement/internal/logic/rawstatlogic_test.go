package logic

import (
	"context"
	"testing"

	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"
)

// TestRawStatRejectsGuardsBeforeTouchingDeps business → message_id 两条；origin_id 不设守卫。
func TestRawStatRejectsGuardsBeforeTouchingDeps(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.RawStatReq
		want error
	}{
		{"business 空", &rpc.RawStatReq{Business: "", OriginId: 1, MessageId: 101}, model.ErrInvalidBusiness},
		{"message_id 为 0", &rpc.RawStatReq{Business: likeBiz, OriginId: 1, MessageId: 0}, model.ErrInvalidMessage},
		{"message_id 为负", &rpc.RawStatReq{Business: likeBiz, OriginId: 1, MessageId: -101}, model.ErrInvalidMessage},
		{"business 优先于 message_id", &rpc.RawStatReq{Business: "", MessageId: 0}, model.ErrInvalidBusiness},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			seedStat(st, likeBiz, likeOrigin, likeMessage, 5, 2, 3, 1)
			before := st.log.snapshot()
			got, err := NewRawStatLogic(context.Background(), newTestSvc(st)).RawStat(tc.in)
			wantFail(t, tc.name, got, err, tc.want)
			wantNoCall(t, tc.name, st, before)
		})
	}
}

// TestRawStatProjectsAllSixFields 六个字段逐一核对（含运营修正位）——
// RawStat 是唯一把 like_change/dislike_change 暴露给调用方的读接口。
func TestRawStatProjectsAllSixFields(t *testing.T) {
	st := newStore()
	seedStat(st, likeBiz, likeOrigin, likeMessage, 5, 2, 3, 1)
	// 同 message_id 不同 origin_id 的行不得混进来（WHERE 带 origin_id）。
	seedStat(st, likeBiz, 999, likeMessage, 77, 88, 99, 100)

	got, err := NewRawStatLogic(context.Background(), newTestSvc(st)).
		RawStat(&rpc.RawStatReq{Business: likeBiz, OriginId: likeOrigin, MessageId: likeMessage})
	wantNoErr(t, "RawStat", err)
	wantOps(t, "RawStat", st.log.opsFrom(0), []string{"stat.FindOne:archive:1:101"})
	wantEQ(t, "RawStat", "OriginId", got.OriginId, likeOrigin)
	wantEQ(t, "RawStat", "MessageId", got.MessageId, likeMessage)
	wantEQ(t, "RawStat", "LikeNumber", got.LikeNumber, int64(5))
	wantEQ(t, "RawStat", "DislikeNumber", got.DislikeNumber, int64(2))
	wantEQ(t, "RawStat", "LikeChange", got.LikeChange, int64(3))
	wantEQ(t, "RawStat", "DislikeChange", got.DislikeChange, int64(1))
}

// TestRawStatMissingRowReturnsEchoedIDs 计数行不存在时返回**回显 id + 零计数**而不是错误：
// 新投稿还没人点赞是正常态。但注意调用方无法区分「0 赞」和「没这行」。
func TestRawStatMissingRowReturnsEchoedIDs(t *testing.T) {
	st := newStore()

	got, err := NewRawStatLogic(context.Background(), newTestSvc(st)).
		RawStat(&rpc.RawStatReq{Business: likeBiz, OriginId: likeOrigin, MessageId: likeMessage})
	wantNoErr(t, "无计数行", err)
	wantOps(t, "无计数行", st.log.opsFrom(0), []string{"stat.FindOne:archive:1:101"})
	wantEQ(t, "无计数行", "OriginId 仍回显", got.OriginId, likeOrigin)
	wantEQ(t, "无计数行", "MessageId 仍回显", got.MessageId, likeMessage)
	wantEQ(t, "无计数行", "LikeNumber", got.LikeNumber, int64(0))
	wantEQ(t, "无计数行", "DislikeNumber", got.DislikeNumber, int64(0))
	wantEQ(t, "无计数行", "LikeChange", got.LikeChange, int64(0))
	wantEQ(t, "无计数行", "DislikeChange", got.DislikeChange, int64(0))
}

// TestRawStatZeroOriginIsAllowedAndKeyedSeparately origin_id=0 参与唯一键，
// 必须按 (business,0,msg) 查，不能和 origin_id=1 的行串。
func TestRawStatZeroOriginIsAllowedAndKeyedSeparately(t *testing.T) {
	st := newStore()
	seedStat(st, likeBiz, 0, likeMessage, 4, 1, 0, 0)
	seedStat(st, likeBiz, 1, likeMessage, 77, 88, 0, 0)

	got, err := NewRawStatLogic(context.Background(), newTestSvc(st)).
		RawStat(&rpc.RawStatReq{Business: likeBiz, OriginId: 0, MessageId: likeMessage})
	wantNoErr(t, "origin 0", err)
	wantOps(t, "origin 0", st.log.opsFrom(0), []string{"stat.FindOne:archive:0:101"})
	wantEQ(t, "origin 0", "LikeNumber", got.LikeNumber, int64(4))
}

// TestRawStatPropagatesModelFailure 唯一依赖失败必须透出，不能返回「0 赞」的假成功，
// 否则运营会据此以为计数已经清零。
func TestRawStatPropagatesModelFailure(t *testing.T) {
	st := newStore()
	seedStat(st, likeBiz, likeOrigin, likeMessage, 5, 2, 3, 1)
	st.stat.failWith("FindOne", errBoom)

	got, err := NewRawStatLogic(context.Background(), newTestSvc(st)).
		RawStat(&rpc.RawStatReq{Business: likeBiz, OriginId: likeOrigin, MessageId: likeMessage})
	wantFail(t, "FindOne 失败", got, err, errBoom)
	wantOps(t, "FindOne 失败后的调用", st.log.opsFrom(0), []string{"stat.FindOne:archive:1:101"})
}

// TestRawStatOnlyReadsOwnDomain 读侧同样不越域：不得触碰收藏/分享/关系行，也不动缓存。
func TestRawStatOnlyReadsOwnDomain(t *testing.T) {
	st := newStore()
	seedStat(st, likeBiz, likeOrigin, likeMessage, 5, 2, 3, 1)
	seedFavItem(st, likeMid, likeMessage, 33, 2, 11, 0)

	_, err := NewRawStatLogic(context.Background(), newTestSvc(st)).
		RawStat(&rpc.RawStatReq{Business: likeBiz, OriginId: likeOrigin, MessageId: likeMessage})
	wantNoErr(t, "RawStat", err)
	wantCount(t, "越域检查", st.log, "favItem.", 0)
	wantCount(t, "越域检查", st.log, "share.", 0)
	wantCount(t, "越域检查", st.log, "like.Upsert", 0)
	wantCount(t, "越域检查", st.log, "cache.", 0)
}
