// deletecontentdoc_test.go 覆盖 DeleteContentDoc：下架/删除只动投影，
// 且必须「先校验再 IO」「不顺手建索引」「降级要推进 revision 防复活」。
package logic

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go-video/services/search-indexer/internal/esclient"
	"go-video/services/search-indexer/internal/repository"
	"go-video/services/search-indexer/model"
	"go-video/services/search-indexer/rpc"
)

func deleteReq(contentID int64, ct rpc.ContentType, purge bool, reason string) *rpc.DeleteContentDocReq {
	return &rpc.DeleteContentDocReq{ContentId: contentID, ContentType: ct, Purge: purge, Reason: reason}
}

func newDeleteStore(t *testing.T) *testStore {
	t.Helper()
	s := newTestStore(t, defaultOptions())
	s.seedActiveIndex(testPrefix, curIndex, 7)
	s.es.seedRevision(curIndex, "1_88", 1000)
	return s
}

func storedDoc(t *testing.T, s *testStore, index, id string) *esclient.ContentDoc {
	t.Helper()
	raw, ok := s.es.sourceOf(index, id)
	if !ok {
		t.Fatalf("投影 %s/%s 不存在", index, id)
	}
	var d esclient.ContentDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("反序列化 _source 失败: %v", err)
	}
	return &d
}

// 入参边界必须在任何依赖调用之前拒绝（避免「按三种类型各删一遍」的误删）。
func TestDeleteContentDoc_ValidationRejectsBeforeAnyIO(t *testing.T) {
	cases := []struct {
		name    string
		req     *rpc.DeleteContentDocReq
		wantErr string
	}{
		{name: "content_id 为 0", req: deleteReq(0, rpc.ContentType_CONTENT_TYPE_UGC_VIDEO, false, "offline"), wantErr: "有效 content_id"},
		{name: "content_id 为负", req: deleteReq(-7, rpc.ContentType_CONTENT_TYPE_UGC_VIDEO, false, "offline"), wantErr: "有效 content_id"},
		{name: "content_type 未指定", req: deleteReq(88, rpc.ContentType_CONTENT_TYPE_UNSPECIFIED, false, "offline"), wantErr: "有效 content_type"},
		{name: "content_type 越界", req: deleteReq(88, rpc.ContentType(4), false, "offline"), wantErr: "有效 content_type"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newDeleteStore(t)
			reply, err := NewDeleteContentDocLogic(context.Background(), s.svcCtx).DeleteContentDoc(c.req)
			wantErr(t, err, "非法入参")
			wantContains(t, err.Error(), c.wantErr, "错误原因")
			if reply != nil {
				t.Fatalf("失败时响应必须为 nil, got %+v", reply)
			}
			wantOps(t, s.ops())
			// 投影必须完好：校验失败不能有任何副作用。
			wantEQ(t, storedDoc(t, s, curIndex, "1_88").DocRevision, int64(1000), "被误改的 doc_revision")
		})
	}
}

// 缓存命中分支：写入索引直接从缓存取，不再读 MySQL 登记表。
func TestDeleteContentDoc_CacheHitSkipsRegistryRead(t *testing.T) {
	s := newDeleteStore(t)
	s.seedCacheActive(testPrefix, curIndex)
	reply, err := NewDeleteContentDocLogic(context.Background(), s.svcCtx).DeleteContentDoc(
		deleteReq(88, rpc.ContentType_CONTENT_TYPE_UGC_VIDEO, false, "offline"))
	wantNoErr(t, err)
	wantEQ(t, reply.Index, curIndex, "reply.Index")
	wantOps(t, s.ops(),
		"cache.GetActiveIndex:"+testPrefix,
		"es.GetSource:"+curIndex+"/1_88",
		"es.UpdatePartial:"+curIndex+"/1_88",
	)
	wantNoCall(t, s.ops(), "version.FindActive")
}

// 缓存 miss 必须回填，否则同一别名的后续请求每次都打 MySQL。
func TestDeleteContentDoc_CacheMissRefillsActiveIndex(t *testing.T) {
	s := newDeleteStore(t)
	if _, err := NewDeleteContentDocLogic(context.Background(), s.svcCtx).DeleteContentDoc(
		deleteReq(88, rpc.ContentType_CONTENT_TYPE_UGC_VIDEO, false, "offline")); err != nil {
		t.Fatal(err)
	}
	wantOps(t, s.ops(),
		"cache.GetActiveIndex:"+testPrefix,
		"version.FindActive:"+testPrefix,
		"cache.SetActiveIndex:"+testPrefix,
		"es.GetSource:"+curIndex+"/1_88",
		"es.UpdatePartial:"+curIndex+"/1_88",
	)
	wantEQ(t, s.cache.active[testPrefix], curIndex, "回填后的缓存内容")

	// 第二次调用即命中缓存（不再读登记表）。
	s.resetOps()
	if _, err := NewDeleteContentDocLogic(context.Background(), s.svcCtx).DeleteContentDoc(
		deleteReq(88, rpc.ContentType_CONTENT_TYPE_UGC_VIDEO, false, "offline")); err != nil {
		t.Fatal(err)
	}
	wantNoCall(t, s.ops(), "version.FindActive")
}

// 真实退化路径：没有 Redis 时（生产 *Cache(nil)）每次解析都读 MySQL，不 panic 也不误命中。
func TestDeleteContentDoc_DegradesWithoutRedis(t *testing.T) {
	s := newTestStoreNoRedis(t, defaultOptions())
	s.ver.seed(&model.SearchIndexVersion{
		Alias: testPrefix, IndexName: curIndex, SchemaVersion: "v1",
		State: model.VersionStateActive, CreatedBy: "bootstrap", Ctime: 1, Mtime: 1,
	})
	s.es.seedIndex(curIndex, 1)
	s.es.seedRevision(curIndex, "1_88", 1000)
	for i := 0; i < 2; i++ {
		if _, err := NewDeleteContentDocLogic(context.Background(), s.svcCtx).DeleteContentDoc(
			deleteReq(88, rpc.ContentType_CONTENT_TYPE_UGC_VIDEO, false, "offline")); err != nil {
			t.Fatalf("无 Redis 时下架失败: %v", err)
		}
	}
	wantCount(t, s.ops(), "version.FindActive", 2) // 没有缓存层，逐次回源
	wantCount(t, s.ops(), "es.UpdatePartial", 2)
	wantNoCall(t, s.ops(), "cache.") // 真实 *Cache(nil) 靠判空退化，没有可观测的缓存动作
}

// 下架原因 → 索引状态映射（必须与查询侧的可检索判定一致）。
func TestDeleteContentDoc_ReasonMapsToState(t *testing.T) {
	cases := []struct {
		reason string
		want   int32
	}{
		{reason: "expired", want: esclient.StateExpired},
		{reason: "rights_expired", want: esclient.StateExpired},
		{reason: "offline", want: esclient.StateOffline},
		{reason: "deleted", want: esclient.StateDeleted},
		{reason: "copyright_takedown", want: esclient.StateDeleted},
		{reason: "", want: esclient.StateDeleted}, // 未知原因按最保守的「移除投影」处理
	}
	for _, c := range cases {
		t.Run("reason="+c.reason, func(t *testing.T) {
			s := newDeleteStore(t)
			reply, err := NewDeleteContentDocLogic(context.Background(), s.svcCtx).DeleteContentDoc(
				deleteReq(88, rpc.ContentType_CONTENT_TYPE_UGC_VIDEO, false, c.reason))
			wantNoErr(t, err)
			wantEQ(t, reply.Outcome, repository.OutcomeMarked, "reply.Outcome")
			if reply.Deleted {
				t.Fatal("非 purge 不得物理删除投影")
			}
			doc := storedDoc(t, s, curIndex, "1_88")
			wantEQ(t, doc.State, c.want, "降级后的 state")
			// 守卫字段必须一起推进，否则随后到达的旧版本事件会把它「复活」。
			if doc.DocRevision <= 1000 {
				t.Fatalf("doc_revision 未推进: %d", doc.DocRevision)
			}
			wantContains(t, s.es.patches[0], "retry=2", "RetryOnConflict 必须透传到 _update")
		})
	}
}

// revision = max(现值+1, 当前毫秒)：现值超前时不能用墙上时钟把它「降级」。
func TestDeleteContentDoc_RevisionIsMaxOfCurrentPlusOneAndNow(t *testing.T) {
	t.Run("索引现值超前于墙上时钟", func(t *testing.T) {
		s := newDeleteStore(t)
		ahead := time.Now().UnixMilli() + 1_000_000_000
		s.es.seedRevision(curIndex, "1_88", ahead)
		if _, err := NewDeleteContentDocLogic(context.Background(), s.svcCtx).DeleteContentDoc(
			deleteReq(88, rpc.ContentType_CONTENT_TYPE_UGC_VIDEO, false, "offline")); err != nil {
			t.Fatal(err)
		}
		wantEQ(t, storedDoc(t, s, curIndex, "1_88").DocRevision, ahead+1, "必须取现值+1")
	})
	t.Run("索引现值过旧时取当前毫秒", func(t *testing.T) {
		s := newDeleteStore(t)
		before := time.Now().UnixMilli()
		if _, err := NewDeleteContentDocLogic(context.Background(), s.svcCtx).DeleteContentDoc(
			deleteReq(88, rpc.ContentType_CONTENT_TYPE_UGC_VIDEO, false, "offline")); err != nil {
			t.Fatal(err)
		}
		after := time.Now().UnixMilli()
		got := storedDoc(t, s, curIndex, "1_88").DocRevision
		if got < before || got > after {
			t.Fatalf("doc_revision = %d, 应落在 [%d, %d]（当前毫秒）", got, before, after)
		}
	})
}

// 投影不存在（热度先到 / 重复下架）：missing 且不发 _update。
func TestDeleteContentDoc_MissingProjectionSkipsUpdate(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	s.seedActiveIndex(testPrefix, curIndex, 7)
	reply, err := NewDeleteContentDocLogic(context.Background(), s.svcCtx).DeleteContentDoc(
		deleteReq(88, rpc.ContentType_CONTENT_TYPE_UGC_VIDEO, false, "offline"))
	wantNoErr(t, err)
	wantEQ(t, reply.Outcome, repository.OutcomeMissing, "reply.Outcome")
	wantEQ(t, reply.Deleted, false, "reply.Deleted")
	wantOps(t, s.ops(),
		"cache.GetActiveIndex:"+testPrefix,
		"version.FindActive:"+testPrefix,
		"cache.SetActiveIndex:"+testPrefix,
		"es.GetSource:"+curIndex+"/1_88",
	)
	wantNoCall(t, s.ops(), "es.UpdatePartial")
}

// _update 报「文档不存在」（applied=false）时同样归为 missing，而不是 marked。
func TestDeleteContentDoc_UpdateNotAppliedReportsMissing(t *testing.T) {
	s := newDeleteStore(t)
	s.es.updateForces = boolPtr(false)
	reply, err := NewDeleteContentDocLogic(context.Background(), s.svcCtx).DeleteContentDoc(
		deleteReq(88, rpc.ContentType_CONTENT_TYPE_UGC_VIDEO, false, "offline"))
	wantNoErr(t, err)
	wantEQ(t, reply.Outcome, repository.OutcomeMissing, "reply.Outcome")
	wantCount(t, s.ops(), "es.UpdatePartial", 1)
}

// purge=true 走物理删除：跳过读-改-写，不碰 probe。
func TestDeleteContentDoc_PurgeDeletesWithoutProbe(t *testing.T) {
	s := newDeleteStore(t)
	reply, err := NewDeleteContentDocLogic(context.Background(), s.svcCtx).DeleteContentDoc(
		deleteReq(88, rpc.ContentType_CONTENT_TYPE_UGC_VIDEO, true, "deleted"))
	wantNoErr(t, err)
	wantEQ(t, reply.Deleted, true, "reply.Deleted")
	wantEQ(t, reply.Outcome, repository.OutcomeDeleted, "reply.Outcome")
	wantOps(t, s.ops(),
		"cache.GetActiveIndex:"+testPrefix,
		"version.FindActive:"+testPrefix,
		"cache.SetActiveIndex:"+testPrefix,
		"es.DeleteDoc:"+curIndex+"/1_88",
	)
	wantNoCall(t, s.ops(), "es.GetSource")
	if _, ok := s.es.sourceOf(curIndex, "1_88"); ok {
		t.Fatal("purge 后投影仍在索引里")
	}
}

// purge 重放（文档已不在）：幂等成功，deleted=false / outcome=noop。
func TestDeleteContentDoc_PurgeReplayIsIdempotent(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	s.seedActiveIndex(testPrefix, curIndex, 7)
	reply, err := NewDeleteContentDocLogic(context.Background(), s.svcCtx).DeleteContentDoc(
		deleteReq(88, rpc.ContentType_CONTENT_TYPE_UGC_VIDEO, true, "deleted"))
	wantNoErr(t, err)
	wantEQ(t, reply.Deleted, false, "reply.Deleted")
	wantEQ(t, reply.Outcome, repository.OutcomeNoop, "reply.Outcome")
}

// 从未建过索引：没有投影可删，视为幂等成功，且绝不顺手建索引。
func TestDeleteContentDoc_NoIndexAtAllIsNoopSuccess(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	reply, err := NewDeleteContentDocLogic(context.Background(), s.svcCtx).DeleteContentDoc(
		deleteReq(88, rpc.ContentType_CONTENT_TYPE_UGC_VIDEO, false, "offline"))
	wantNoErr(t, err)
	wantEQ(t, reply.Outcome, repository.OutcomeNoop, "reply.Outcome")
	wantEQ(t, reply.Index, "", "无索引时不该编造索引名")
	wantOps(t, s.ops(),
		"cache.GetActiveIndex:"+testPrefix,
		"version.FindActive:"+testPrefix,
	)
	wantNoCall(t, s.ops(), "es.")
	if len(s.es.created) != 0 {
		t.Fatalf("删除路径建了索引: %v", s.es.created)
	}
}

// 依赖失败必须传播：MySQL 读失败绝不能被误判成「没有投影可删」而静默成功。
func TestDeleteContentDoc_DependencyFailuresPropagate(t *testing.T) {
	t.Run("登记表读失败", func(t *testing.T) {
		s := newDeleteStore(t)
		s.fail("version.FindActive", errOther)
		reply, err := NewDeleteContentDocLogic(context.Background(), s.svcCtx).DeleteContentDoc(
			deleteReq(88, rpc.ContentType_CONTENT_TYPE_UGC_VIDEO, false, "offline"))
		wantErrIs(t, err, errOther, "必须传播而不是返回 noop")
		if reply != nil {
			t.Fatalf("失败时响应必须为 nil, got %+v", reply)
		}
		wantNoCall(t, s.ops(), "es.")
	})
	t.Run("probe 读失败", func(t *testing.T) {
		s := newDeleteStore(t)
		s.fail("es.GetSource", errBoom)
		_, err := NewDeleteContentDocLogic(context.Background(), s.svcCtx).DeleteContentDoc(
			deleteReq(88, rpc.ContentType_CONTENT_TYPE_UGC_VIDEO, false, "offline"))
		wantErrIs(t, err, errBoom, "probe 失败必须传播")
		wantNoCall(t, s.ops(), "es.UpdatePartial")
	})
	t.Run("脏 _source", func(t *testing.T) {
		s := newDeleteStore(t)
		s.es.seedSource(curIndex, "1_88", json.RawMessage(`{"doc_revision":[]}`))
		_, err := NewDeleteContentDocLogic(context.Background(), s.svcCtx).DeleteContentDoc(
			deleteReq(88, rpc.ContentType_CONTENT_TYPE_UGC_VIDEO, false, "offline"))
		wantErr(t, err, "脏数据")
		wantContains(t, err.Error(), "unmarshal probe", "错误定位")
	})
	t.Run("物理删除失败", func(t *testing.T) {
		s := newDeleteStore(t)
		s.fail("es.DeleteDoc", errBoom)
		_, err := NewDeleteContentDocLogic(context.Background(), s.svcCtx).DeleteContentDoc(
			deleteReq(88, rpc.ContentType_CONTENT_TYPE_UGC_VIDEO, true, "deleted"))
		wantErrIs(t, err, errBoom, "删除失败必须传播")
	})
}

// 不同内容类型的同 ID 是两篇不同投影：只动指定的那一类。
func TestDeleteContentDoc_OnlyTargetsGivenContentType(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	s.seedActiveIndex(testPrefix, curIndex, 7)
	s.es.seedRevision(curIndex, "1_88", 900)
	s.es.seedRevision(curIndex, "2_88", 500)
	reply, err := NewDeleteContentDocLogic(context.Background(), s.svcCtx).DeleteContentDoc(
		deleteReq(88, rpc.ContentType_CONTENT_TYPE_PGC_EPISODE, false, "offline"))
	wantNoErr(t, err)
	wantEQ(t, reply.Outcome, repository.OutcomeMarked, "reply.Outcome")
	wantOps(t, s.ops(),
		"cache.GetActiveIndex:"+testPrefix,
		"version.FindActive:"+testPrefix,
		"cache.SetActiveIndex:"+testPrefix,
		"es.GetSource:"+curIndex+"/2_88",
		"es.UpdatePartial:"+curIndex+"/2_88",
	)
	wantEQ(t, storedDoc(t, s, curIndex, "2_88").State, esclient.StateOffline, "2_88 应被降级")
	wantEQ(t, storedDoc(t, s, curIndex, "1_88").DocRevision, int64(900), "同 ID 的 UGC 投影不得被连带修改")
	if got := storedDoc(t, s, curIndex, "1_88").State; got == esclient.StateOffline {
		t.Fatalf("1_88 被误降级为 offline")
	}
}
