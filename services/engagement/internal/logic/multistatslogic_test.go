package logic

import (
	"context"
	"testing"

	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"
)

// TestMultiStatsShortCircuitsAndEnforcesLimit 这张表覆盖 MultiStats 的全部入参分支：
// 它没有任何「非法即报错」的守卫，只有 nil 短路、空记录短跳、跳过 nil record 和 >100 超限。
// 每个用例都断言 callLog 为空或只含预期的查询，确保短路真的短路。
func TestMultiStatsShortCircuitsAndEnforcesLimit(t *testing.T) {
	recs := func(n int) []*rpc.MultiStatsReq_Record {
		out := make([]*rpc.MultiStatsReq_Record, 0, n)
		for i := range n {
			out = append(out, &rpc.MultiStatsReq_Record{OriginId: 1, MessageId: int64(1000 + i)})
		}
		return out
	}
	cases := []struct {
		name    string
		in      *rpc.MultiStatsReq
		wantErr error // 非 nil 表示这条必须报错；nil 表示必须短路成功
	}{
		{
			name:    "business map 为 nil",
			in:      &rpc.MultiStatsReq{Mid: 7, Business: nil},
			wantErr: nil,
		},
		{
			name:    "业务键存在但值为 nil",
			in:      &rpc.MultiStatsReq{Mid: 7, Business: map[string]*rpc.MultiStatsReq_Business{"archive": nil}},
			wantErr: nil,
		},
		{
			name: "业务键存在但 records 为空",
			in: &rpc.MultiStatsReq{Mid: 7, Business: map[string]*rpc.MultiStatsReq_Business{
				"archive": {Records: nil},
			}},
			wantErr: nil,
		},
		{
			name: "records 里只有 nil 元素",
			in: &rpc.MultiStatsReq{Mid: 7, Business: map[string]*rpc.MultiStatsReq_Business{
				"archive": {Records: []*rpc.MultiStatsReq_Record{nil, nil}},
			}},
			wantErr: nil,
		},
		{
			name: "单业务 101 条超限",
			in: &rpc.MultiStatsReq{Mid: 7, Business: map[string]*rpc.MultiStatsReq_Business{
				"archive": {Records: recs(101)},
			}},
			wantErr: model.ErrTooManyMessageIDs,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			seedStat(st, likeBiz, likeOrigin, 1000, 5, 2, 0, 0)
			before := st.log.snapshot()
			got, err := NewMultiStatsLogic(context.Background(), newTestSvc(st)).MultiStats(tc.in)
			if tc.wantErr != nil {
				wantFail(t, tc.name, got, err, tc.wantErr)
			} else {
				wantNoErr(t, tc.name, err)
				if got == nil {
					t.Fatalf("%s：短路成功也必须返回响应体", tc.name)
				}
			}
			// 无论成功还是超限，都不允许发生一次查询。
			wantNoCall(t, tc.name, st, before)
		})
	}

	t.Run("单业务刚好 100 条不超限", func(t *testing.T) {
		st := newStore()
		_, err := NewMultiStatsLogic(context.Background(), newTestSvc(st)).
			MultiStats(&rpc.MultiStatsReq{Mid: 7, Business: map[string]*rpc.MultiStatsReq_Business{
				"archive": {Records: recs(100)},
			}})
		wantNoErr(t, "100 条", err)
		wantCount(t, "100 条", st.log, "stat.FindOne:archive:", 100)
	})
}

// TestMultiStatsNilBusinessReturnsEmptyMap 「没问任何业务」是合法的空请求，
// 必须回一个**非 nil 的空 map**，不能回 nil（客户端会解引用）。
func TestMultiStatsNilBusinessReturnsEmptyMap(t *testing.T) {
	st := newStore()

	got, err := NewMultiStatsLogic(context.Background(), newTestSvc(st)).
		MultiStats(&rpc.MultiStatsReq{Mid: 7})
	wantNoErr(t, "nil business", err)
	if got == nil || got.Business == nil {
		t.Fatalf("MultiStats() 的 business map = %#v, want 非 nil 空 map", got)
	}
	wantEQ(t, "nil business", "业务数", len(got.Business), 0)
	wantNoErr(t, "nil business", err)
}

// TestMultiStatsSkipsEmptyBusinessWithEmptyMap 空业务返回的是「键存在 + 空 map」，
// 而不是键缺失——客户端按业务名取记录时不应拿到 nil。
func TestMultiStatsSkipsEmptyBusinessWithEmptyMap(t *testing.T) {
	st := newStore()

	got, err := NewMultiStatsLogic(context.Background(), newTestSvc(st)).
		MultiStats(&rpc.MultiStatsReq{Mid: 7, Business: map[string]*rpc.MultiStatsReq_Business{
			"archive": {Records: []*rpc.MultiStatsReq_Record{{OriginId: 1, MessageId: 101}}},
			"dynamic": nil,
			"article": {Records: []*rpc.MultiStatsReq_Record{}},
		}})
	wantNoErr(t, "混合业务", err)
	wantEQ(t, "混合业务", "业务键数（三个都保留）", len(got.Business), 3)

	rec := got.Business["archive"]
	if rec == nil {
		t.Fatalf("archive 结果缺失")
	}
	wantEQ(t, "混合业务", "archive 记录数", len(rec.Records), 1)
	wantEQ(t, "混合业务", "dynamic 是空 map", len(got.Business["dynamic"].GetRecords()), 0)
	wantEQ(t, "混合业务", "article 是空 map", len(got.Business["article"].GetRecords()), 0)
	wantOps(t, "混合业务", st.log.opsFrom(0), []string{"stat.FindOne:archive:1:101"})
}

// TestMultiStatsProjectsPerRecordOriginId 逐字段投影：每条记录的 origin_id 各自回显，
// 缺失计数的对象仍产出一条 zero 记录（与 Stats 的行为不同，见 statslogic 用例）。
func TestMultiStatsProjectsPerRecordOriginId(t *testing.T) {
	st := newStore()
	seedStat(st, "archive", 1, 101, 5, 2, 0, 0)
	seedStat(st, "archive", 2, 102, 7, 8, 0, 0)
	// 干扰行：同 message_id 不同 origin
	seedStat(st, "archive", 9, 102, 99, 99, 0, 0)

	got, err := NewMultiStatsLogic(context.Background(), newTestSvc(st)).
		MultiStats(&rpc.MultiStatsReq{Mid: 7, Business: map[string]*rpc.MultiStatsReq_Business{
			"archive": {Records: []*rpc.MultiStatsReq_Record{
				{OriginId: 1, MessageId: 101},
				{OriginId: 2, MessageId: 102},
				{OriginId: 3, MessageId: 103}, // 无计数行
				nil,                           // 被跳过
			}},
		}})
	wantNoErr(t, "MultiStats", err)
	wantOpsUnordered(t, "MultiStats", st.log.opsFrom(0), []string{
		"stat.FindOne:archive:1:101", "stat.FindOne:archive:2:102", "stat.FindOne:archive:3:103",
	})

	recs := got.Business["archive"].Records
	wantEQ(t, "MultiStats", "记录数（nil 被跳过、缺失补零）", len(recs), 3)

	wantEQ(t, "MultiStats 101", "OriginId", recs[101].OriginId, int64(1))
	wantEQ(t, "MultiStats 101", "MessageId", recs[101].MessageId, int64(101))
	wantEQ(t, "MultiStats 101", "LikeNumber", recs[101].LikeNumber, int64(5))
	wantEQ(t, "MultiStats 101", "DislikeNumber", recs[101].DislikeNumber, int64(2))
	wantEQ(t, "MultiStats 101", "LikeState（本接口不带用户态）", recs[101].LikeState, rpc.LikeState_STATE_UNSPECIFIED)

	wantEQ(t, "MultiStats 102", "OriginId（各条用自己的）", recs[102].OriginId, int64(2))
	wantEQ(t, "MultiStats 102", "LikeNumber", recs[102].LikeNumber, int64(7))
	wantEQ(t, "MultiStats 102", "DislikeNumber", recs[102].DislikeNumber, int64(8))

	wantEQ(t, "MultiStats 103", "OriginId", recs[103].OriginId, int64(3))
	wantEQ(t, "MultiStats 103", "LikeNumber 补零", recs[103].LikeNumber, int64(0))
	wantEQ(t, "MultiStats 103", "DislikeNumber 补零", recs[103].DislikeNumber, int64(0))
}

// TestMultiStatsIgnoresMid MultiStats 收了 mid 但从不使用：既不像 Stats 那样查用户态，
// 也不做登录判定。所以传 mid 也拿不到 LikeState（缺陷 #9：契约字段无实现）。
func TestMultiStatsIgnoresMid(t *testing.T) {
	st := newStore()
	seedStat(st, likeBiz, likeOrigin, likeMessage, 5, 2, 0, 0)
	seedLike(st, likeBiz, likeMid, likeUpMid, likeOrigin, likeMessage, model.LikeStateLike, 1_700_000_500)

	got, err := NewMultiStatsLogic(context.Background(), newTestSvc(st)).
		MultiStats(&rpc.MultiStatsReq{Mid: likeMid, Business: map[string]*rpc.MultiStatsReq_Business{
			likeBiz: {Records: []*rpc.MultiStatsReq_Record{{OriginId: likeOrigin, MessageId: likeMessage}}},
		}})
	wantNoErr(t, "带 mid 的 MultiStats", err)
	wantEQ(t, "缺陷 #9", "LikeState 恒为未指定",
		got.Business[likeBiz].Records[likeMessage].LikeState, rpc.LikeState_STATE_UNSPECIFIED)
	wantOps(t, "带 mid 的 MultiStats", st.log.opsFrom(0), []string{"stat.FindOne:archive:1:101"})
	wantCount(t, "缺陷 #9", st.log, "like.FindStates", 0)
}

// TestMultiStatsDeduplicatesByMessageId 同一业务内重复 message_id 只产出一个键（后写覆盖）。
// 这条锁定「记录按 message_id 索引」而不是按入参下标，客户端拿到的一定是 map。
func TestMultiStatsDeduplicatesByMessageId(t *testing.T) {
	st := newStore()
	seedStat(st, likeBiz, likeOrigin, likeMessage, 5, 2, 0, 0)

	got, err := NewMultiStatsLogic(context.Background(), newTestSvc(st)).
		MultiStats(&rpc.MultiStatsReq{Business: map[string]*rpc.MultiStatsReq_Business{
			likeBiz: {Records: []*rpc.MultiStatsReq_Record{
				{OriginId: likeOrigin, MessageId: likeMessage},
				{OriginId: 2, MessageId: likeMessage},
			}},
		}})
	wantNoErr(t, "重复 message_id", err)
	recs := got.Business[likeBiz].Records
	wantEQ(t, "重复 message_id", "记录数", len(recs), 1)
	wantEQ(t, "重复 message_id", "后写的 origin 胜出", recs[likeMessage].OriginId, int64(2))
	wantCount(t, "重复 message_id", st.log, "stat.FindOne:", 2)
}

// TestMultiStatsPropagatesQueryFailure 任一对象查询失败 ⇒ 整个请求失败（无部分结果），
// 已查出来的业务也不返回。
func TestMultiStatsPropagatesQueryFailure(t *testing.T) {
	st := newStore()
	st.stat.failWith("FindOne", errBoom)

	got, err := NewMultiStatsLogic(context.Background(), newTestSvc(st)).
		MultiStats(&rpc.MultiStatsReq{Business: map[string]*rpc.MultiStatsReq_Business{
			likeBiz: {Records: []*rpc.MultiStatsReq_Record{{OriginId: likeOrigin, MessageId: likeMessage}}},
		}})
	wantFail(t, "FindOne 失败", got, err, errBoom)
	wantOps(t, "FindOne 失败后的调用", st.log.opsFrom(0), []string{"stat.FindOne:archive:1:101"})
}

// TestMultiStatsIsNPlusOne 现象固化（缺陷 #10）：MultiStats 对每条记录单独发一次
// FindOne，而不是按业务批量 FindMany。100 条记录 = 100 次 SELECT。
// 本用例不断言性能，只把「每记录一次查询」钉成可观察事实，将来改批量时它会红。
func TestMultiStatsIsNPlusOne(t *testing.T) {
	st := newStore()
	recs := make([]*rpc.MultiStatsReq_Record, 0, 8)
	for i := range 8 {
		recs = append(recs, &rpc.MultiStatsReq_Record{OriginId: likeOrigin, MessageId: int64(200 + i)})
	}
	_, err := NewMultiStatsLogic(context.Background(), newTestSvc(st)).
		MultiStats(&rpc.MultiStatsReq{Business: map[string]*rpc.MultiStatsReq_Business{likeBiz: {Records: recs}}})
	wantNoErr(t, "8 条记录", err)
	wantCount(t, "缺陷 #10", st.log, "stat.FindOne:", 8)
	wantCount(t, "缺陷 #10", st.log, "stat.FindMany:", 0)
}
