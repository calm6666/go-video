// getindexhealth_test.go 覆盖 GetIndexHealth：巡检是排障入口，所以
// 「单个依赖读失败」要出现在结果里而不是让接口失败，
// 而「登记表读不到」必须直接失败（那时无可奉告比误报更安全）。
package logic

import (
	"context"
	"strings"
	"testing"

	"go-video/services/search-indexer/model"
	"go-video/services/search-indexer/rpc"
)

const (
	hAlias   = "gc"
	hIdxA    = "gc_v1_100"
	hIdxB    = "gc_v1_200"
	hIdxMiss = "gc_v1_404"

	hReadAll    = "version.ListAll"
	hReadByAlis = "version.ListByAlias:gc"
)

// hOps 拼出一次巡检的完整依赖序列：登记表读 → 逐项探测（可裁剪）→ 两条积压水位。
// 水位永远是最后两项，因为它们属于接口层而不是某个别名。
func hOps(registryRead string, probe ...string) []string {
	ops := append([]string{registryRead}, probe...)
	return append(ops, "offset.CountByState:retry", "dlq.Count:open")
}

// hProbeGreen 是「物理索引存在且 doc 数读得到」时的五项动作（含登记表快照回写）。
func hProbeGreen(index, alias string) []string {
	return []string{
		"es.IndexExists:" + index,
		"es.Count:" + index,
		"es.ClusterHealth:" + index,
		"version.UpdateDocCount:" + index,
		"es.AliasTargets:" + alias,
	}
}

func healthLogic(s *testStore) *GetIndexHealthLogic {
	return NewGetIndexHealthLogic(context.Background(), s.svcCtx)
}

// seedRegistered 静默登记一行索引版本（state 由调用方决定）。
func seedRegistered(s *testStore, alias, index, state string, docCount int64) *model.SearchIndexVersion {
	return s.ver.seed(&model.SearchIndexVersion{
		Alias: alias, IndexName: index, SchemaVersion: "v1",
		DocCount: docCount, State: state, CreatedBy: "bootstrap", Ctime: 100, Mtime: 100,
	})
}

func getHealth(s *testStore, alias string) (*rpc.GetIndexHealthReply, error) {
	return healthLogic(s).GetIndexHealth(&rpc.GetIndexHealthReq{Alias: alias})
}

// 一切正常：ok + 完整字段 + doc 数快照回写登记表。
func TestGetIndexHealth_AllGreenIsOk(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	s.es.seedIndex(hIdxA, 12345)
	s.es.seedAlias(hAlias, hIdxA)
	seedRegistered(s, hAlias, hIdxA, model.VersionStateActive, 0)

	reply, err := getHealth(s, "")
	wantNoErr(t, err)
	wantEQ(t, len(reply.Aliases), 1, "别名数")
	h := reply.Aliases[0]
	wantEQ(t, h.Alias, hAlias, "Alias")
	wantEQ(t, h.ActiveIndex, hIdxA, "ActiveIndex")
	wantEQ(t, h.SchemaVersion, "v1", "SchemaVersion")
	wantEQ(t, h.DocCount, int64(12345), "DocCount 用实测值，而不是登记表里的旧值")
	wantEQ(t, h.IndexExists, true, "IndexExists")
	wantEQ(t, h.Health, "green", "Health")
	wantEQ(t, h.State, model.VersionStateActive, "State")
	wantEQ(t, reply.RetryPending, int64(0), "RetryPending")
	wantEQ(t, reply.DeadLetter, int64(0), "DeadLetter")
	wantEQ(t, reply.OverallState, "ok", "OverallState")
	wantOps(t, s.ops(), hOps(hReadAll, hProbeGreen(hIdxA, hAlias)...)...)

	// 快照真的回写了：运维查库看到的 doc_count 与巡检一致。
	wantEQ(t, s.ver.rowOf(hIdxA).DocCount, int64(12345), "登记表 doc_count 快照")
}

// 非 active 版本（retiring/history）不进巡检结论：它们不承接写入。
func TestGetIndexHealth_SkipsNonActiveRows(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	s.es.seedIndex(hIdxA, 10)
	seedRegistered(s, hAlias, hIdxA, model.VersionStateActive, 0)
	seedRegistered(s, hAlias, hIdxB, model.VersionStateRetiring, 5)
	seedRegistered(s, hAlias, hIdxMiss, model.VersionStateHistory, 7)

	reply, err := getHealth(s, "")
	wantNoErr(t, err)
	wantEQ(t, len(reply.Aliases), 1, "只报告 active")
	wantEQ(t, reply.Aliases[0].ActiveIndex, hIdxA, "ActiveIndex")
	wantCount(t, s.ops(), "es.IndexExists", 1)
	wantNoCall(t, s.ops(), "es.IndexExists:"+hIdxB)
	wantNoCall(t, s.ops(), "es.IndexExists:"+hIdxMiss)
	wantOps(t, s.ops(), hOps(hReadAll, hProbeGreen(hIdxA, hAlias)...)...)
}

// 同一别名出现两条 active（历史脏数据）：按 id 倒序取最新那条，且只报一次。
func TestGetIndexHealth_DuplicateActiveRowsDeduped(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	s.es.seedIndex(hIdxA, 10)
	s.es.seedIndex(hIdxB, 20)
	seedRegistered(s, hAlias, hIdxA, model.VersionStateActive, 0)
	seedRegistered(s, hAlias, hIdxB, model.VersionStateActive, 0)

	reply, err := getHealth(s, "")
	wantNoErr(t, err)
	wantEQ(t, len(reply.Aliases), 1, "一个别名只出一行")
	wantEQ(t, reply.Aliases[0].ActiveIndex, hIdxB, "取 id 最大的登记行（ORDER BY id DESC）")
	wantCount(t, s.ops(), "es.IndexExists", 1)
}

// 登记表说有、OpenSearch 没有：index_exists=false + health=missing，整体 down，
// 且 doc_count 用 -1 表示「读不到」而不是 0 条（0 会被误读成空索引）。
func TestGetIndexHealth_PhysicalIndexMissingIsDown(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	seedRegistered(s, hAlias, hIdxMiss, model.VersionStateActive, 99)

	reply, err := getHealth(s, "")
	wantNoErr(t, err, "巡检本身不该失败")
	h := reply.Aliases[0]
	wantEQ(t, h.IndexExists, false, "IndexExists")
	wantEQ(t, h.Health, "missing", "Health")
	wantEQ(t, h.DocCount, int64(-1), "读不到必须 -1")
	wantEQ(t, reply.OverallState, "down", "OverallState")
	wantOps(t, s.ops(), hOps(hReadAll,
		"es.IndexExists:"+hIdxMiss,
		"es.Count:"+hIdxMiss,
		"es.AliasTargets:"+hAlias,
	)...)
	wantNoCall(t, s.ops(), "es.ClusterHealth")
	wantNoCall(t, s.ops(), "version.UpdateDocCount")
}

// 探测物理索引失败：错误写进结果（health=error: …）而不是让接口失败，
// 后面的 AliasTargets 仍然继续采集——巡检要尽量多给出线索。
func TestGetIndexHealth_IndexProbeErrorIsReportedInResult(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	s.es.seedIndex(hIdxA, 10)
	seedRegistered(s, hAlias, hIdxA, model.VersionStateActive, 0)
	s.fail("es.IndexExists:"+hIdxA, errBoom)

	reply, err := getHealth(s, "")
	wantNoErr(t, err)
	h := reply.Aliases[0]
	wantEQ(t, h.IndexExists, false, "读失败时不能声称索引存在")
	wantContains(t, h.Health, "error:", "health 字段承载错误")
	wantContains(t, h.Health, errBoom.Error(), "错误原文要能排障")
	wantEQ(t, h.DocCount, int64(-1), "DocCount")
	wantEQ(t, reply.OverallState, "down", "读不到物理索引按 down 报，不能报 ok")
	wantOps(t, s.ops(), hOps(hReadAll,
		"es.IndexExists:"+hIdxA,
		"es.AliasTargets:"+hAlias,
	)...)
}

// doc 数读失败（非 404）：索引仍在，只是统计不出——degraded 而不是 down。
func TestGetIndexHealth_DocCountErrorIsDegraded(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	s.es.seedIndex(hIdxA, 10)
	seedRegistered(s, hAlias, hIdxA, model.VersionStateActive, 0)
	s.fail("es.Count:"+hIdxA, errBoom)

	reply, err := getHealth(s, "")
	wantNoErr(t, err)
	h := reply.Aliases[0]
	wantEQ(t, h.IndexExists, true, "索引存在的结论仍然成立")
	wantContains(t, h.Health, errBoom.Error(), "Health 记录统计错误")
	wantEQ(t, h.DocCount, int64(-1), "DocCount 读不到为 -1")
	wantEQ(t, reply.OverallState, "degraded", "OverallState")
	wantOps(t, s.ops(), hOps(hReadAll,
		"es.IndexExists:"+hIdxA,
		"es.Count:"+hIdxA,
		"es.AliasTargets:"+hAlias,
	)...)
	wantNoCall(t, s.ops(), "version.UpdateDocCount")
}

// 索引存在但 _count 返回 404：health=missing（区别于「物理索引不存在」的 down）。
func TestGetIndexHealth_CountNotFoundIsMissingButNotDown(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	s.es.seedIndexNoCount(hIdxA)
	seedRegistered(s, hAlias, hIdxA, model.VersionStateActive, 0)

	reply, err := getHealth(s, "")
	wantNoErr(t, err)
	h := reply.Aliases[0]
	wantEQ(t, h.Health, "missing", "Health")
	wantEQ(t, h.IndexExists, true, "别名指向的索引确实在")
	wantEQ(t, h.DocCount, int64(-1), "DocCount")
	wantEQ(t, reply.OverallState, "degraded", "degraded 而不是 down")
	wantNoCall(t, s.ops(), "es.ClusterHealth")
	wantNoCall(t, s.ops(), "version.UpdateDocCount")
}

// 集群分片黄：只 degraded（还能读写），不 up 到 down。
func TestGetIndexHealth_NonGreenClusterIsDegraded(t *testing.T) {
	for _, status := range []string{"yellow", "red"} {
		t.Run(status, func(t *testing.T) {
			s := newTestStore(t, defaultOptions())
			s.es.seedIndex(hIdxA, 10)
			s.es.seedHealth(hIdxA, status)
			seedRegistered(s, hAlias, hIdxA, model.VersionStateActive, 0)

			reply, err := getHealth(s, "")
			wantNoErr(t, err)
			wantEQ(t, reply.Aliases[0].Health, status, "Health 原样上报")
			wantEQ(t, reply.Aliases[0].DocCount, int64(10), "doc 数照常给出")
			wantEQ(t, reply.OverallState, "degraded", "OverallState")
		})
	}
}

// 集群健康读失败：错误进 health，且 doc 数快照仍然回写。
func TestGetIndexHealth_ClusterHealthErrorIsReported(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	s.es.seedIndex(hIdxA, 77)
	seedRegistered(s, hAlias, hIdxA, model.VersionStateActive, 0)
	s.fail("es.ClusterHealth:"+hIdxA, errBoom)

	reply, err := getHealth(s, "")
	wantNoErr(t, err)
	wantContains(t, reply.Aliases[0].Health, errBoom.Error(), "Health")
	wantEQ(t, reply.Aliases[0].DocCount, int64(77), "DocCount 已读到，不回退成 -1")
	wantEQ(t, reply.OverallState, "degraded", "OverallState")
	wantOps(t, s.ops(), hOps(hReadAll, hProbeGreen(hIdxA, hAlias)...)...)
	wantEQ(t, s.ver.rowOf(hIdxA).DocCount, int64(77), "快照仍然回写")
}

// 快照回写失败：被静默吞掉——不影响巡检，但登记表就此变旧（已知缺口 9：连日志都没有）。
func TestGetIndexHealth_SnapshotWritebackFailureIsSwallowed(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	s.es.seedIndex(hIdxA, 500)
	seedRegistered(s, hAlias, hIdxA, model.VersionStateActive, 1)
	s.fail("version.UpdateDocCount:"+hIdxA, errOther)

	reply, err := getHealth(s, "")
	wantNoErr(t, err, "回写失败不该让巡检失败")
	wantEQ(t, reply.OverallState, "ok", "OverallState")
	wantEQ(t, reply.Aliases[0].DocCount, int64(500), "接口返回实测值")
	wantEQ(t, s.ver.rowOf(hIdxA).DocCount, int64(1), "库里仍是旧快照（失败没被重试）")
	wantOps(t, s.ops(), hOps(hReadAll, hProbeGreen(hIdxA, hAlias)...)...)
}

// 别名指向表读失败：只影响这一列辅助信息，索引存在性结论不变。
func TestGetIndexHealth_AliasTargetsFailureIsSwallowed(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	s.es.seedIndex(hIdxA, 10)
	seedRegistered(s, hAlias, hIdxA, model.VersionStateActive, 0)
	s.fail("es.AliasTargets:"+hAlias, errBoom)

	reply, err := getHealth(s, "")
	wantNoErr(t, err)
	wantEQ(t, reply.Aliases[0].Health, "green", "主结论不受影响")
	wantEQ(t, reply.OverallState, "ok", "OverallState")
	wantOps(t, s.ops(), hOps(hReadAll, hProbeGreen(hIdxA, hAlias)...)...)
}

// 登记表读不到：整体失败（没有可信事实可报），并且完全不碰 OpenSearch。
func TestGetIndexHealth_RegistryReadFailurePropagates(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	s.es.seedIndex(hIdxA, 10)
	seedRegistered(s, hAlias, hIdxA, model.VersionStateActive, 0)
	s.fail("version.ListAll", errOther)

	reply, err := getHealth(s, "")
	wantErrIs(t, err, errOther, "必须传播")
	if reply != nil {
		t.Fatalf("失败时响应必须为 nil, got %+v", reply)
	}
	wantOps(t, s.ops(), "version.ListAll")
	wantNoCall(t, s.ops(), "es.")
	wantNoCall(t, s.ops(), "offset.")
	wantNoCall(t, s.ops(), "dlq.")
}

// 积压水位：待重试或死信任一非零就 degraded（投影在漏事件）。
func TestGetIndexHealth_BacklogMakesItDegraded(t *testing.T) {
	for _, tc := range []struct {
		name       string
		retry, dlq int64
		want       string
	}{
		{"无积压", 0, 0, "ok"},
		{"只有待重试", 5, 0, "degraded"},
		{"只有死信", 0, 2, "degraded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t, defaultOptions())
			s.es.seedIndex(hIdxA, 10)
			seedRegistered(s, hAlias, hIdxA, model.VersionStateActive, 0)
			s.offset.seedCount(model.OffsetStateRetry, tc.retry)
			s.dlq.seedCount(model.DLQStateOpen, tc.dlq)

			reply, err := getHealth(s, "")
			wantNoErr(t, err)
			wantEQ(t, reply.RetryPending, tc.retry, "RetryPending")
			wantEQ(t, reply.DeadLetter, tc.dlq, "DeadLetter")
			wantEQ(t, reply.OverallState, tc.want, "OverallState")
			wantEQ(t, reply.Aliases[0].Health, "green", "索引本身仍是绿的")
		})
	}
}

// 积压统计读失败：上报 0，但巡检不失败，且索引状态照常返回。
// 注意：两条统计都要读——第一条失败不能短路掉第二条。
func TestGetIndexHealth_BacklogStatFailuresReportZeroButKeepLog(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	s.es.seedIndex(hIdxA, 10)
	s.es.seedAlias(hAlias, hIdxA)
	seedRegistered(s, hAlias, hIdxA, model.VersionStateActive, 0)
	s.offset.seedCount(model.OffsetStateRetry, 42)
	s.dlq.seedCount(model.DLQStateOpen, 42)
	s.fail("offset.CountByState", errOther)
	s.fail("dlq.Count", errOther)

	reply, err := getHealth(s, "")
	wantNoErr(t, err, "观测值失败不该让巡检失败")
	wantEQ(t, reply.RetryPending, int64(0), "retry_pending 上报 0")
	wantEQ(t, reply.DeadLetter, int64(0), "dead_letter 上报 0")
	wantEQ(t, reply.OverallState, "ok", "0 积压不构成 degraded（不能拿失败当健康）")
	wantEQ(t, len(reply.Aliases), 1, "别名状态照常返回")
	wantOps(t, s.ops(), hOps(hReadAll, hProbeGreen(hIdxA, hAlias)...)...)
}

// 一条统计失败、另一条成功：成功那条仍要如实上报。
func TestGetIndexHealth_OneStatFailureDoesNotHideTheOther(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	s.es.seedIndex(hIdxA, 10)
	seedRegistered(s, hAlias, hIdxA, model.VersionStateActive, 0)
	s.dlq.seedCount(model.DLQStateOpen, 3)
	s.fail("offset.CountByState", errOther)

	reply, err := getHealth(s, "")
	wantNoErr(t, err)
	wantEQ(t, reply.RetryPending, int64(0), "失败项上报 0")
	wantEQ(t, reply.DeadLetter, int64(3), "成功项照常上报")
	wantEQ(t, reply.OverallState, "degraded", "有死信就 degraded")
	wantCount(t, s.ops(), "offset.CountByState", 1)
	wantCount(t, s.ops(), "dlq.Count", 1)
}

// 从未建过索引：空别名列表 + down（不是 ok，也不是 nil 列表）。
func TestGetIndexHealth_EmptyRegistryIsDown(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	reply, err := getHealth(s, "")
	wantNoErr(t, err)
	if reply.Aliases == nil {
		t.Fatalf("空结果也应返回非 nil 切片")
	}
	wantEQ(t, len(reply.Aliases), 0, "别名数")
	wantEQ(t, reply.OverallState, "down", "没有别名就是 down")
	wantOps(t, s.ops(), "version.ListAll", "offset.CountByState:retry", "dlq.Count:open")
}

// 指定别名：走 ListByAlias（不是全表扫），并且别名要归一化。
func TestGetIndexHealth_AliasFilterNormalizesInput(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	s.es.seedIndex(hIdxA, 10)
	s.es.seedIndex("other_v1_1", 5)
	seedRegistered(s, hAlias, hIdxA, model.VersionStateActive, 0)
	seedRegistered(s, "other", "other_v1_1", model.VersionStateActive, 0)

	reply, err := getHealth(s, "  GC  ")
	wantNoErr(t, err)
	wantEQ(t, len(reply.Aliases), 1, "只返回指定别名")
	wantEQ(t, reply.Aliases[0].Alias, hAlias, "Alias")
	wantOps(t, s.ops(), hOps(hReadByAlis, hProbeGreen(hIdxA, hAlias)...)...)
	wantNoCall(t, s.ops(), "version.ListAll")
	wantNoCall(t, s.ops(), "es.IndexExists:other_v1_1")
}

// 指定别名但从未登记：空列表 + down（与「全表为空」同一结论）。
func TestGetIndexHealth_UnknownAliasFilterIsDown(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	s.es.seedIndex(hIdxA, 10)
	seedRegistered(s, hAlias, hIdxA, model.VersionStateActive, 0)

	reply, err := getHealth(s, "nope")
	wantNoErr(t, err)
	wantEQ(t, len(reply.Aliases), 0, "别名数")
	wantEQ(t, reply.OverallState, "down", "OverallState")
	wantOps(t, s.ops(), "version.ListByAlias:nope", "offset.CountByState:retry", "dlq.Count:open")
}

// 多别名汇总：down 优先于 degraded（有一处写不进去就不能报「勉强能用」）。
func TestGetIndexHealth_DownTakesPriorityOverDegraded(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	s.es.seedIndex("a1_v1_1", 10)
	seedRegistered(s, "a1", "a1_v1_1", model.VersionStateActive, 0)
	s.es.seedHealth("a1_v1_1", "yellow")
	seedRegistered(s, "a2", hIdxMiss, model.VersionStateActive, 0) // 物理索引不存在

	reply, err := getHealth(s, "")
	wantNoErr(t, err)
	wantEQ(t, len(reply.Aliases), 2, "两个别名都报告")
	wantEQ(t, reply.Aliases[0].Alias, "a1", "别名按字典序（列表稳定，便于比对）")
	wantEQ(t, reply.Aliases[0].Health, "yellow", "a1 是 degraded 级别问题")
	wantEQ(t, reply.Aliases[1].Health, "missing", "a2 索引不存在")
	wantEQ(t, reply.OverallState, "down", "down 优先")
}

// 巡检是只读操作：绝不建索引、不改别名、不碰写索引缓存。
func TestGetIndexHealth_IsReadOnly(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	s.es.seedIndex(hIdxA, 10)
	s.es.seedAlias(hAlias, hIdxA)
	seedRegistered(s, hAlias, hIdxA, model.VersionStateActive, 0)

	_, err := getHealth(s, "")
	wantNoErr(t, err)
	wantNoCall(t, s.ops(), "es.CreateIndex")
	wantNoCall(t, s.ops(), "es.ApplyAliasActions")
	wantNoCall(t, s.ops(), "es.IndexDoc")
	wantNoCall(t, s.ops(), "cache.")
	wantEQ(t, s.es.applied == nil, true, "没有下发过别名动作")

	// 空登记表也不会被巡检「顺手补出来」。
	s2 := newTestStore(t, defaultOptions())
	_, err = getHealth(s2, "")
	wantNoErr(t, err)
	wantEQ(t, len(s2.ver.rows), 0, "登记表仍为空")
	wantCount(t, s2.ops(), "es.CreateIndex", 0)
}

// Health 的返回结构里带别名指向表（rpc 未暴露，但 repository 事实要正确）。
func TestHealth_ExposesAliasTargets(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	s.es.seedIndex(hIdxA, 10)
	s.es.seedAlias(hAlias, hIdxA, hIdxB)
	seedRegistered(s, hAlias, hIdxA, model.VersionStateActive, 0)

	rows, err := s.repo.Health(context.Background(), hAlias)
	wantNoErr(t, err)
	wantEQ(t, len(rows), 1, "行数")
	wantEQ(t, strings.Join(rows[0].AliasTargets, ","), hIdxA+","+hIdxB, "AliasTargets 如实带出")

	// 指向表读不到时留空，而不是报错（别名还没建）。
	s2 := newTestStore(t, defaultOptions())
	s2.es.seedIndex(hIdxA, 10)
	seedRegistered(s2, hAlias, hIdxA, model.VersionStateActive, 0)
	rows2, err := s2.repo.Health(context.Background(), "")
	wantNoErr(t, err)
	wantEQ(t, len(rows2), 1, "行数")
	wantEQ(t, len(rows2[0].AliasTargets), 0, "别名不存在时指向表为空")
	wantEQ(t, rows2[0].Health, "green", "主结论不受影响")
}
