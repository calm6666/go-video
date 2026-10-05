// upsertcontentdoc_test.go 覆盖 UpsertContentDoc 的投影写入语义。
//
// 这里测的是「真实 Repository 走完整链路后发生什么」：守卫判定发生在哪一步、
// 什么情况下一次 OpenSearch 调用都不发、失败是传播还是被吞。
package logic

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"go-video/services/search-indexer/internal/esclient"
	"go-video/services/search-indexer/internal/repository"
	"go-video/services/search-indexer/model"
	"go-video/services/search-indexer/rpc"
)

const curIndex = testPrefix + "_v1_100" // 已登记的 active 物理索引（固定名，可进精确序列）

// publishedRPCDoc 最小合法快照（UGC=1 → 文档主键 "1_<id>"）。
func publishedRPCDoc(contentID, revision int64) *rpc.ContentDoc {
	return &rpc.ContentDoc{
		ContentId: contentID, ContentType: rpc.ContentType_CONTENT_TYPE_UGC_VIDEO,
		Title: "标题", State: rpc.ContentState_CONTENT_STATE_PUBLISHED, DocRevision: revision,
	}
}

func upsertReq(doc *rpc.ContentDoc) *rpc.UpsertContentDocReq {
	return &rpc.UpsertContentDocReq{ContentId: doc.GetContentId(), Doc: doc, RequestId: "req-1", Source: "rpc"}
}

func newUpsertStore(t *testing.T) *testStore {
	t.Helper()
	s := newTestStore(t, defaultOptions())
	s.seedActiveIndex(testPrefix, curIndex, 7)
	return s
}

// 首次写入：索引里没有该文档 → outcome=created，并且真的落进了 active 索引。
func TestUpsertContentDoc_FirstWriteIsCreated(t *testing.T) {
	s := newUpsertStore(t)
	reply, err := NewUpsertContentDocLogic(context.Background(), s.svcCtx).UpsertContentDoc(
		upsertReq(publishedRPCDoc(88, 2000)))
	wantNoErr(t, err)
	wantEQ(t, reply.ContentId, int64(88), "reply.ContentId")
	wantEQ(t, reply.Index, curIndex, "reply.Index（必须写到登记中的 active 索引）")
	wantEQ(t, reply.Outcome, repository.OutcomeCreated, "reply.Outcome")
	wantEQ(t, reply.DocRevision, int64(2000), "reply.DocRevision")
	if reply.TookMs < 0 {
		t.Fatalf("TookMs = %d, 不该为负", reply.TookMs)
	}
	// 完整序列：DB 解析写入索引 → probe → 覆盖写。没有 cache.GetActiveIndex ——
	// 主写路径每行都要读一次 MySQL（已登记缺口）。
	wantOps(t, s.ops(),
		"version.FindActive:"+testPrefix,
		"es.GetSource:"+curIndex+"/1_88",
		"es.IndexDoc:"+curIndex+"/1_88",
	)
	// 读回真实 _source：字段由上游快照决定，本服务不猜。
	raw, ok := s.es.sourceOf(curIndex, "1_88")
	if !ok {
		t.Fatal("文档没落进索引")
	}
	var stored esclient.ContentDoc
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	wantEQ(t, stored.ContentID, int64(88), "落库 content_id")
	wantEQ(t, stored.Title, "标题", "落库 title")
	wantEQ(t, stored.State, esclient.StatePublished, "落库 state")
	wantEQ(t, stored.SchemaVersion, 1, "schema_version 默认回填 1")
}

// last-write-wins 守卫：更新/等号放行，迟到拒绝且不再发写请求。
func TestUpsertContentDoc_RevisionGuard(t *testing.T) {
	cases := []struct {
		name        string
		stored      int64
		incoming    int64
		wantOutcome string
		wantWritten int // es.IndexDoc 次数
		wantReplRev int64
	}{
		{name: "更新的事实覆盖旧值", stored: 1000, incoming: 2000, wantOutcome: "written", wantWritten: 1, wantReplRev: 2000},
		{name: "同版本幂等重写放行", stored: 2000, incoming: 2000, wantOutcome: "written", wantWritten: 1, wantReplRev: 2000},
		{name: "迟到事件被拒但不报错", stored: 3000, incoming: 2000, wantOutcome: "skipped_stale", wantWritten: 0, wantReplRev: 3000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newUpsertStore(t)
			s.es.seedRevision(curIndex, "1_88", c.stored)
			reply, err := NewUpsertContentDocLogic(context.Background(), s.svcCtx).UpsertContentDoc(
				upsertReq(publishedRPCDoc(88, c.incoming)))
			wantNoErr(t, err)
			wantEQ(t, reply.Outcome, c.wantOutcome, "reply.Outcome")
			// 迟到时上报的是「索引里现存的版本」，调用方据此判断自己的写入没生效。
			wantEQ(t, reply.DocRevision, c.wantReplRev, "reply.DocRevision")
			wantCount(t, s.ops(), "es.IndexDoc", c.wantWritten)
			raw, _ := s.es.sourceOf(curIndex, "1_88")
			var stored esclient.ContentDoc
			if err := json.Unmarshal(raw, &stored); err != nil {
				t.Fatal(err)
			}
			wantEQ(t, stored.DocRevision, c.wantReplRev, "索引里最终生效的版本")
		})
	}
}

// force（运维回填）必须跳过 probe：少一次读，也意味着守卫让位于人工判断。
func TestUpsertContentDoc_ForceOverwriteSkipsGuard(t *testing.T) {
	s := newUpsertStore(t)
	s.es.seedRevision(curIndex, "1_88", 999999) // 索引里的版本更新，正常一定被守卫拦下
	in := upsertReq(publishedRPCDoc(88, 100))
	in.ForceOverwrite = true
	reply, err := NewUpsertContentDocLogic(context.Background(), s.svcCtx).UpsertContentDoc(in)
	wantNoErr(t, err)
	wantEQ(t, reply.Outcome, "written", "force 必须真的覆盖")
	wantEQ(t, reply.DocRevision, int64(100), "force 后生效的是回填版本")
	wantOps(t, s.ops(),
		"version.FindActive:"+testPrefix,
		"es.IndexDoc:"+curIndex+"/1_88",
	)
	wantNoCall(t, s.ops(), "es.GetSource")
}

// 首次使用时才建索引并登记；索引名必须小写、带 schema、带 Unix 秒（可反复重建不撞名）。
func TestUpsertContentDoc_BootstrapsActiveIndexOnFirstUse(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	reply, err := NewUpsertContentDocLogic(context.Background(), s.svcCtx).UpsertContentDoc(
		upsertReq(publishedRPCDoc(88, 2000)))
	wantNoErr(t, err)
	wantEQ(t, reply.Outcome, "created", "reply.Outcome")

	created := s.es.created
	wantCount(t, s.ops(), "es.CreateIndex", 1)
	if len(created) != 1 {
		t.Fatalf("CreateIndex 参数未记录: %v", created)
	}
	name := created[0]
	if !regexp.MustCompile(`^` + testPrefix + `_v1_\d{10}$`).MatchString(name) {
		t.Fatalf("索引名必须是 <alias>_<schema>_<unix秒> 且全小写: %q", name)
	}
	wantEQ(t, reply.Index, name, "reply.Index")
	wantOpsMasked(t, s.ops(),
		"version.FindActive:"+testPrefix,
		"es.CreateIndex:"+testPrefix+"_v1_<ts>",
		"version.Insert:"+testPrefix+"_v1_<ts>",
		"cache.DelActiveIndex:"+testPrefix,
		"es.GetSource:"+testPrefix+"_v1_<ts>/1_88",
		"es.IndexDoc:"+testPrefix+"_v1_<ts>/1_88",
	)

	row := s.ver.rowOf(name)
	if row == nil {
		t.Fatal("新索引未登记到 search_index_version")
	}
	wantEQ(t, row.State, model.VersionStateActive, "登记状态")
	wantEQ(t, row.CreatedBy, "bootstrap", "created_by 留痕")
	wantEQ(t, row.SchemaVersion, "v1", "schema_version")
	wantEQ(t, row.DocCount, int64(0), "登记时的 doc 数快照")
	wantEQ(t, row.Alias, testPrefix, "alias")
	isRecentUnix(t, row.Ctime, "登记 ctime")
}

// 同秒重建撞名：必须换后缀重试一次，而不是把文档写进别人的索引。
func TestUpsertContentDoc_IndexNameCollisionRetriesWithSuffix(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	s.es.createFirst = boolPtr(true)
	_, err := NewUpsertContentDocLogic(context.Background(), s.svcCtx).UpsertContentDoc(
		upsertReq(publishedRPCDoc(88, 2000)))
	wantNoErr(t, err)
	if len(s.es.created) != 2 {
		t.Fatalf("撞名后应再建一次索引，实际 %v", s.es.created)
	}
	first, second := s.es.created[0], s.es.created[1]
	if second == first || !strings.HasPrefix(second, first+"_") {
		t.Fatalf("重试名必须是原名加后缀: %q -> %q", first, second)
	}
	if s.ver.rowOf(first) != nil {
		t.Fatalf("撞掉的第一个名字不该被登记: %+v", s.ver.rowOf(first))
	}
	row := s.ver.rowOf(second)
	if row == nil {
		t.Fatalf("重试后的索引未登记: %v", s.ver.rows)
	}
	wantEQ(t, row.State, model.VersionStateActive, "登记状态")
	wantOpsMasked(t, s.ops(),
		"version.FindActive:"+testPrefix,
		"es.CreateIndex:"+testPrefix+"_v1_<ts>",
		"es.CreateIndex:"+testPrefix+"_v1_<ts>",
		"version.Insert:"+testPrefix+"_v1_<ts>",
		"cache.DelActiveIndex:"+testPrefix,
		"es.GetSource:"+testPrefix+"_v1_<ts>/1_88",
		"es.IndexDoc:"+testPrefix+"_v1_<ts>/1_88",
	)
}

// 并发首次写入：对手实例抢先登记时，必须改用它选定的索引（不能两个写入目标并存）。
func TestUpsertContentDoc_ConcurrentRegistrationAdoptsOtherIndex(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	stolen := testPrefix + "_v1_777"
	s.es.seedIndex(stolen, 0)
	s.ver.collideWith = &model.SearchIndexVersion{
		Alias: testPrefix, IndexName: stolen, SchemaVersion: "v1",
		State: model.VersionStateActive, CreatedBy: "bootstrap", Ctime: 1000, Mtime: 1000,
	}
	reply, err := NewUpsertContentDocLogic(context.Background(), s.svcCtx).UpsertContentDoc(
		upsertReq(publishedRPCDoc(88, 2000)))
	wantNoErr(t, err)
	wantEQ(t, reply.Index, stolen, "必须落到对手登记的索引")
	wantOpsMasked(t, s.ops(),
		"version.FindActive:"+testPrefix,
		"es.CreateIndex:"+testPrefix+"_v1_<ts>",
		"version.Insert:"+testPrefix+"_v1_<ts>",
		"version.FindActive:"+testPrefix,
		"cache.DelActiveIndex:"+testPrefix, // 未采用自己建的名字 → 缓存必须作废
		"es.GetSource:"+stolen+"/1_88",
		"es.IndexDoc:"+stolen+"/1_88",
	)
}

// 登记行存在但不是 active：不能拿它当写入目标，必须报错由人工判断。
func TestUpsertContentDoc_RegisteredButNotActiveFailsFast(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	s.ver.collideWith = &model.SearchIndexVersion{
		Alias: testPrefix, IndexName: testPrefix + "_v1_777", SchemaVersion: "v1",
		State: model.VersionStateRetiring, CreatedBy: "rebuild", Ctime: 1000, Mtime: 1000,
	}
	reply, err := NewUpsertContentDocLogic(context.Background(), s.svcCtx).UpsertContentDoc(
		upsertReq(publishedRPCDoc(88, 2000)))
	wantErr(t, err, "登记为 retiring 时不能静默写入")
	wantContains(t, err.Error(), "registered but not active", "错误原因")
	if reply != nil {
		t.Fatalf("失败时响应必须为 nil, got %+v", reply)
	}
	wantNoCall(t, s.ops(), "es.GetSource")
	wantNoCall(t, s.ops(), "es.IndexDoc")
}

// 入参边界：全部在触碰任何依赖之前拒绝（序列必须为空）。
func TestUpsertContentDoc_RejectsInvalidInputBeforeAnyIO(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(d *rpc.ContentDoc)
		wantErr string
	}{
		{name: "内容类型未指定", mutate: func(d *rpc.ContentDoc) { d.ContentType = rpc.ContentType_CONTENT_TYPE_UNSPECIFIED }, wantErr: "invalid content_type"},
		{name: "内容类型越界", mutate: func(d *rpc.ContentDoc) { d.ContentType = rpc.ContentType(9) }, wantErr: "invalid content_type"},
		{name: "缺 doc_revision", mutate: func(d *rpc.ContentDoc) { d.DocRevision = 0 }, wantErr: "missing doc_revision"},
		{name: "负 doc_revision", mutate: func(d *rpc.ContentDoc) { d.DocRevision = -1 }, wantErr: "missing doc_revision"},
		{name: "content_id 为 0", mutate: func(d *rpc.ContentDoc) { d.ContentId = 0 }, wantErr: "invalid content_id"},
		{name: "已发布但标题为空", mutate: func(d *rpc.ContentDoc) { d.Title = "  " }, wantErr: "empty title for published doc"},
		{name: "状态未指定", mutate: func(d *rpc.ContentDoc) { d.State = rpc.ContentState_CONTENT_STATE_UNSPECIFIED }, wantErr: "未知 content_state"},
		{name: "状态越界", mutate: func(d *rpc.ContentDoc) { d.State = rpc.ContentState(99) }, wantErr: "未知 content_state"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newUpsertStore(t)
			doc := publishedRPCDoc(88, 2000)
			c.mutate(doc)
			in := upsertReq(doc)
			reply, err := NewUpsertContentDocLogic(context.Background(), s.svcCtx).UpsertContentDoc(in)
			wantErr(t, err, "非法入参")
			wantContains(t, err.Error(), c.wantErr, "错误原因")
			if reply != nil {
				t.Fatalf("失败时响应必须为 nil, got %+v", reply)
			}
			wantOps(t, s.ops()) // 一次依赖调用都不许发生
		})
	}
}

// 路由键与文档键不一致必须拒绝；未传 content_id 时以文档为准。
func TestUpsertContentDoc_ContentIDMismatchGuard(t *testing.T) {
	s := newUpsertStore(t)
	in := upsertReq(publishedRPCDoc(88, 2000))
	in.ContentId = 99
	reply, err := NewUpsertContentDocLogic(context.Background(), s.svcCtx).UpsertContentDoc(in)
	wantErrIs(t, err, errContentIDMismatch, "content_id 不一致")
	if reply != nil {
		t.Fatalf("失败时响应必须为 nil, got %+v", reply)
	}
	wantOps(t, s.ops()) // 守卫在任何 IO 之前

	s2 := newUpsertStore(t)
	zero := upsertReq(publishedRPCDoc(88, 2000))
	zero.ContentId = 0
	reply2, err := NewUpsertContentDocLogic(context.Background(), s2.svcCtx).UpsertContentDoc(zero)
	wantNoErr(t, err)
	wantEQ(t, reply2.ContentId, int64(88), "以 doc.content_id 为准")

	s3 := newUpsertStore(t)
	same := upsertReq(publishedRPCDoc(88, 2000))
	same.ContentId = 88
	if _, err := NewUpsertContentDocLogic(context.Background(), s3.svcCtx).UpsertContentDoc(same); err != nil {
		t.Fatalf("一致的 content_id 不该被拒绝: %v", err)
	}
}

// 依赖失败的爆炸半径：错误一律传播，不伪装成功、不留半成品。
func TestUpsertContentDoc_DependencyFailuresPropagate(t *testing.T) {
	t.Run("MySQL 读不到登记就不碰 OpenSearch", func(t *testing.T) {
		s := newUpsertStore(t)
		s.fail("version.FindActive", errOther)
		reply, err := NewUpsertContentDocLogic(context.Background(), s.svcCtx).UpsertContentDoc(
			upsertReq(publishedRPCDoc(88, 2000)))
		wantErrIs(t, err, errOther, "FindActive 失败必须传播")
		if reply != nil {
			t.Fatalf("失败时响应必须为 nil, got %+v", reply)
		}
		wantOps(t, s.ops(), "version.FindActive:"+testPrefix)
		wantNoCall(t, s.ops(), "es.") // 不能顺手建索引
		if len(s.es.created) != 0 {
			t.Fatalf("MySQL 故障时建了索引: %v", s.es.created)
		}
	})
	t.Run("probe 读失败不得当成不存在", func(t *testing.T) {
		s := newUpsertStore(t)
		s.fail("es.GetSource", errBoom)
		_, err := NewUpsertContentDocLogic(context.Background(), s.svcCtx).UpsertContentDoc(
			upsertReq(publishedRPCDoc(88, 2000)))
		wantErrIs(t, err, errBoom, "probe 失败必须传播")
		wantContains(t, err.Error(), "probe", "错误里要能看出是 probe 环节")
		wantNoCall(t, s.ops(), "es.IndexDoc") // 读不到版本就覆盖 = 可能覆盖新事实
	})
	t.Run("IndexDoc 失败原样上报", func(t *testing.T) {
		s := newUpsertStore(t)
		s.fail("es.IndexDoc", errBoom)
		reply, err := NewUpsertContentDocLogic(context.Background(), s.svcCtx).UpsertContentDoc(
			upsertReq(publishedRPCDoc(88, 2000)))
		wantErrIs(t, err, errBoom, "写失败必须传播")
		if reply != nil {
			t.Fatalf("失败时响应必须为 nil（不能上报 created）, got %+v", reply)
		}
	})
}

// _source 脏数据/mapping 漂移：必须报错交人工判断，不能当成「文档不存在」放过去。
func TestUpsertContentDoc_DirtySourceIsNotTreatedAsMissing(t *testing.T) {
	s := newUpsertStore(t)
	s.es.seedSource(curIndex, "1_88", json.RawMessage(`{"doc_revision":"不是数字"}`))
	_, err := NewUpsertContentDocLogic(context.Background(), s.svcCtx).UpsertContentDoc(
		upsertReq(publishedRPCDoc(88, 2000)))
	wantErr(t, err, "脏 _source")
	wantContains(t, err.Error(), "unmarshal probe", "错误定位")
	wantNoCall(t, s.ops(), "es.IndexDoc")
}
