// switchalias_test.go 覆盖 SwitchAlias：零停机重建的最后一步。
//
// 断言重点是「顺序」与「缓存失效时机」：先在 OpenSearch 原子切别名，再更新登记表，
// 成功后必须立刻失效写入索引缓存；失败时不能把已生效的切换伪装成没发生。
package logic

import (
	"context"
	"testing"

	"go-video/services/search-indexer/internal/repository"
	"go-video/services/search-indexer/model"
	"go-video/services/search-indexer/rpc"
)

const (
	swAlias = "gc"
	idxOld  = "gc_v1_100" // 当前 active（查询别名指向它）
	idxNew  = "gc_v1_200" // 重建产出的目标索引
	idxMore = "gc_v1_300" // 历史多指向场景
)

const (
	lockKey    = "si:sw:gc"
	lockAcq    = "cache.AcquireLock:" + lockKey + "|60" // 60 = ttlAliasLock
	lockRel    = "cache.ReleaseLock:" + lockKey
	delCache   = "cache.DelActiveIndex:" + swAlias
	setCache   = "cache.SetActiveIndex:" + swAlias
	getCache   = "cache.GetActiveIndex:" + swAlias
	findActive = "version.FindActive:" + swAlias
	swOldToNew = "version.SwitchActive:gc->gc_v1_200(exp=gc_v1_100)"
	swFirst    = "version.SwitchActive:gc->gc_v1_200(exp=)"
)

func switchReq(alias, target, expected string, skipHealth bool) *rpc.SwitchAliasReq {
	return &rpc.SwitchAliasReq{Alias: alias, TargetIndex: target, ExpectedCurrent: expected, SkipHealthCheck: skipHealth, Operator: "admin-1"}
}

// newSwitchStore 组一个「重建已完成、等待切换」的场景：
// 老索引是 active 且被别名指向，目标索引由 runner 建好并登记为 retiring、已灌满数据。
func newSwitchStore(t *testing.T) *testStore {
	t.Helper()
	s := newTestStore(t, repository.Options{IndexPrefix: swAlias, SchemaVersion: "v1"})
	s.seedActiveIndex(swAlias, idxOld, 10)
	s.es.seedAlias(swAlias, idxOld)
	s.es.seedIndex(idxNew, 42)
	s.ver.seed(&model.SearchIndexVersion{Alias: swAlias, IndexName: idxNew, SchemaVersion: "v1",
		DocCount: 0, State: model.VersionStateRetiring, CreatedBy: "rebuild task-1", Ctime: 1, Mtime: 1})
	return s
}

func doSwitch(t *testing.T, s *testStore, alias, target, expected string, skip bool) (*rpc.SwitchAliasReply, error) {
	t.Helper()
	return NewSwitchAliasLogic(context.Background(), s.svcCtx).SwitchAlias(switchReq(alias, target, expected, skip))
}

// 正常切换：完整顺序 + 登记表互斥 + 缓存立即失效（切换后写入必须落到新索引）。
func TestSwitchAlias_HappyPathInvalidatesWriteIndexCacheImmediately(t *testing.T) {
	s := newSwitchStore(t)
	s.seedCacheActive(swAlias, idxOld) // 30 秒 TTL 内的旧解析结果

	reply, err := doSwitch(t, s, swAlias, idxNew, idxOld, false)
	wantNoErr(t, err)
	wantEQ(t, reply.Alias, swAlias, "reply.Alias")
	wantEQ(t, reply.PreviousIndex, idxOld, "reply.PreviousIndex")
	wantEQ(t, reply.CurrentIndex, idxNew, "reply.CurrentIndex")
	wantEQ(t, reply.DocCount, int64(42), "reply.DocCount（取自 OpenSearch 实测）")
	wantEQ(t, reply.RecordState, model.VersionStateActive, "reply.RecordState")

	wantOps(t, s.ops(),
		lockAcq,
		"es.IndexExists:gc_v1_200",
		"es.Count:gc_v1_200",
		"es.AliasTargets:gc",
		"es.ApplyAliasActions:remove:gc_v1_100,add:gc_v1_200",
		"version.FindByIndexName:gc_v1_200",
		"version.UpdateDocCount:gc_v1_200",
		swOldToNew,
		delCache,
		lockRel,
	)
	// 登记表互斥：一个别名同时只能有一行 active。
	wantEQ(t, s.ver.activeRow(swAlias).IndexName, idxNew, "切换后的 active 行")
	wantEQ(t, s.ver.rowOf(idxOld).State, model.VersionStateRetiring, "被替换的索引转 retiring（观察期后转 history）")
	wantEQ(t, s.ver.rowOf(idxNew).State, model.VersionStateActive, "目标索引转 active")
	wantEQ(t, s.ver.rowOf(idxNew).DocCount, int64(42), "doc 数快照刷新")

	// 缓存必须真的空掉 —— 只断言「调了 Del」不够。
	if v, ok := s.cache.active[swAlias]; ok {
		t.Fatalf("切换后写入索引缓存仍是 %q（会继续写老索引）", v)
	}
	// 端到端：随后的下架写入必须落到新索引，而不是老索引。
	s.resetOps()
	s.es.seedRevision(idxNew, "1_88", 10)
	if _, err := NewDeleteContentDocLogic(context.Background(), s.svcCtx).DeleteContentDoc(
		deleteReq(88, rpc.ContentType_CONTENT_TYPE_UGC_VIDEO, false, "offline")); err != nil {
		t.Fatal(err)
	}
	wantOps(t, s.ops(), getCache, findActive, setCache,
		"es.GetSource:gc_v1_200/1_88", "es.UpdatePartial:gc_v1_200/1_88")
	wantNoCall(t, s.ops(), "gc_v1_100")
	// OpenSearch 侧别名指向也已原子切换。
	wantEQ(t, len(s.es.aliasTargets[swAlias]), 1, "别名指向数")
	wantEQ(t, s.es.aliasTargets[swAlias][0], idxNew, "别名指向")
}

// 首次挂载别名（OpenSearch 里还没有这个别名，目标索引也没登记）：
// 要求调用方显式传空 expected_current，并补登记目标索引。
func TestSwitchAlias_FirstTimeMountRegistersTargetIndex(t *testing.T) {
	s := newSwitchStore(t)
	delete(s.es.aliasTargets, swAlias) // 别名尚不存在
	s.ver.drop(idxNew)                 // 目标索引没走 runner 登记

	reply, err := doSwitch(t, s, swAlias, idxNew, "", false) // expected_current 空 = 接受首次挂载
	wantNoErr(t, err)
	wantEQ(t, reply.PreviousIndex, "", "首次挂载没有前序索引")
	wantOps(t, s.ops(),
		lockAcq,
		"es.IndexExists:gc_v1_200",
		"es.Count:gc_v1_200",
		"es.AliasTargets:gc", // 别名不存在 → 按「首次挂载」处理
		"es.ApplyAliasActions:add:gc_v1_200",
		"version.FindByIndexName:gc_v1_200",
		"version.Insert:gc_v1_200",
		swFirst,
		delCache,
		lockRel,
	)
	row := s.ver.rowOf(idxNew)
	if row == nil {
		t.Fatal("目标索引未补登记，切换后无人知道它存在")
	}
	wantEQ(t, row.CreatedBy, "switch", "补登记的 created_by")
	wantEQ(t, row.SchemaVersion, "v1", "schema_version")
	wantEQ(t, row.DocCount, int64(42), "登记时带上实测 doc 数")
	wantEQ(t, row.State, model.VersionStateActive, "最终状态")
	wantEQ(t, s.ver.rowOf(idxOld).State, model.VersionStateRetiring, "老索引转 retiring")

	// 已知行为（README 缺口 10）：PlanAliasSwitch 的注释声称「expected_current 为空表示
	// 首次挂载，current 必须为空」，但代码只反向校验（current 为空时要求 expected 为空）。
	// 于是省略可选字段 expected_current 就等于放弃乐观校验，直接摘掉别人的指向。
	// 这里钉住现状，改动时该断言会立刻变红。
	s2 := newSwitchStore(t)
	reply2, err := doSwitch(t, s2, swAlias, idxNew, "", false)
	wantNoErr(t, err, "当前实现不拒绝空 expected_current")
	wantEQ(t, reply2.PreviousIndex, idxOld, "老指向被静默摘掉")
	wantCount(t, s2.ops(), "es.ApplyAliasActions:remove:gc_v1_100,add:gc_v1_200", 1)
}

// 目标索引已在登记表（runner 建过）时只刷新快照，不重复登记、不覆盖 created_by。
func TestSwitchAlias_RegisteredTargetOnlyRefreshesDocCount(t *testing.T) {
	s := newSwitchStore(t)
	if _, err := doSwitch(t, s, swAlias, idxNew, idxOld, false); err != nil {
		t.Fatal(err)
	}
	wantCount(t, s.ops(), "version.UpdateDocCount", 1)
	wantNoCall(t, s.ops(), "version.Insert")
	row := s.ver.rowOf(idxNew)
	wantEQ(t, row.DocCount, int64(42), "doc 数快照刷新")
	wantEQ(t, row.CreatedBy, "rebuild task-1", "既有登记信息不得被覆盖（审计留痕）")
}

// 切换重放（幂等）：目标已是唯一指向时不再请求 _aliases，但登记表与缓存照常收敛。
func TestSwitchAlias_NoopWhenTargetAlreadySoleOwner(t *testing.T) {
	s := newTestStore(t, repository.Options{IndexPrefix: swAlias, SchemaVersion: "v1"})
	s.seedActiveIndex(swAlias, idxNew, 42)
	s.es.seedAlias(swAlias, idxNew)

	reply, err := doSwitch(t, s, swAlias, idxNew, "", false)
	wantNoErr(t, err)
	wantEQ(t, reply.CurrentIndex, idxNew, "reply.CurrentIndex")
	wantEQ(t, reply.PreviousIndex, idxNew, "Noop 时前序即目标")
	wantNoCall(t, s.ops(), "es.ApplyAliasActions") // 重复 remove 会因指向已变而报错
	wantOps(t, s.ops(),
		lockAcq,
		"es.IndexExists:gc_v1_200",
		"es.Count:gc_v1_200",
		"es.AliasTargets:gc",
		"version.FindByIndexName:gc_v1_200",
		"version.UpdateDocCount:gc_v1_200",
		"version.SwitchActive:gc->gc_v1_200(exp=)",
		delCache,
		lockRel,
	)
	wantEQ(t, s.ver.activeRow(swAlias).IndexName, idxNew, "active 行不变")
	wantEQ(t, s.es.aliasTargets[swAlias][0], idxNew, "别名不变")
}

// 别名多指向：一次 _aliases 里摘掉全部旧指向，add 必须放最后（查询侧看不到空指向瞬间）。
func TestSwitchAlias_RemovesAllStaleTargetsAndAddsLast(t *testing.T) {
	s := newSwitchStore(t)
	s.es.seedIndex(idxMore, 1)
	s.ver.seed(&model.SearchIndexVersion{Alias: swAlias, IndexName: idxMore, SchemaVersion: "v1",
		State: model.VersionStateRetiring, CreatedBy: "rebuild", Ctime: 1, Mtime: 1})
	s.es.seedAlias(swAlias, idxMore, idxOld) // 乱序指向，验证归一化按字典序
	if _, err := doSwitch(t, s, swAlias, idxNew, idxOld, false); err != nil {
		t.Fatal(err)
	}
	wantEQ(t, s.es.applied[0], "remove:gc_v1_100,remove:gc_v1_300,add:gc_v1_200", "动作顺序：remove 在前、add 最后")
	wantEQ(t, len(s.es.aliasTargets[swAlias]), 1, "切换后只剩目标索引")
}

// 三条纯参数守卫必须在抢锁之前拒绝：一次依赖调用都不发。
func TestSwitchAlias_GuardsRejectBeforeAnyIO(t *testing.T) {
	cases := []struct {
		name     string
		target   string
		expected string
		wantErr  error
		wantMsg  string
	}{
		{name: "目标为空", target: "", expected: idxOld, wantErr: model.ErrTargetIndexNotFound, wantMsg: ""},
		{name: "目标不属于该别名", target: "other_v1_200", expected: idxOld, wantErr: model.ErrIndexNameMismatch, wantMsg: "不属于别名"},
		{name: "expected_current 与 target 相同", target: idxNew, expected: idxNew, wantErr: model.ErrAliasMismatch, wantMsg: "expected_current 与 target_index 相同"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newSwitchStore(t)
			s.seedCacheActive(swAlias, idxOld)
			reply, err := doSwitch(t, s, swAlias, c.target, c.expected, false)
			wantErrIs(t, err, c.wantErr, "守卫错误类型")
			if c.wantMsg != "" {
				wantContains(t, err.Error(), c.wantMsg, "错误原因")
			}
			if reply != nil {
				t.Fatalf("失败时响应必须为 nil, got %+v", reply)
			}
			wantOps(t, s.ops()) // 连锁都不抢
			wantEQ(t, s.cache.active[swAlias], idxOld, "守卫失败不得动缓存")
		})
	}
}

// 空索引保护：拒绝把没有文档的索引上线，但紧急回滚可显式跳过。
func TestSwitchAlias_EmptyIndexRejectedUnlessSkipHealthCheck(t *testing.T) {
	t.Run("默认拒绝空索引", func(t *testing.T) {
		s := newSwitchStore(t)
		s.es.counts[idxNew] = 0
		reply, err := doSwitch(t, s, swAlias, idxNew, idxOld, false)
		wantErrIs(t, err, model.ErrTargetIndexNotFound, "空索引必须拒绝")
		wantContains(t, err.Error(), "文档数为 0", "错误原因")
		wantContains(t, err.Error(), "skip_health_check", "错误里要给出逃生阀")
		if reply != nil {
			t.Fatalf("失败时响应必须为 nil, got %+v", reply)
		}
		// 拒绝之后必须放锁，否则别名被永久锁死。
		wantOps(t, s.ops(), lockAcq, "es.IndexExists:gc_v1_200", "es.Count:gc_v1_200", lockRel)
		wantNoCall(t, s.ops(), "es.ApplyAliasActions")
		wantNoCall(t, s.ops(), "version.")
		if len(s.es.applied) != 0 {
			t.Fatal("被拒绝的切换仍改了别名")
		}
		wantEQ(t, s.ver.activeRow(swAlias).IndexName, idxOld, "登记表保持原样")
		if s.cache.locks[lockKey] {
			t.Fatal("互斥锁未释放")
		}
	})
	t.Run("skip_health_check 放行", func(t *testing.T) {
		s := newSwitchStore(t)
		s.es.counts[idxNew] = 0
		reply, err := doSwitch(t, s, swAlias, idxNew, idxOld, true)
		wantNoErr(t, err)
		wantEQ(t, reply.DocCount, int64(0), "reply.DocCount")
		wantCount(t, s.ops(), "es.ApplyAliasActions", 1)
		wantEQ(t, s.ver.activeRow(swAlias).IndexName, idxNew, "紧急回滚仍完成切换")
	})
	t.Run("_count 返回 404 也按空索引处理", func(t *testing.T) {
		s := newSwitchStore(t)
		s.es.seedIndexNoCount(idxNew) // 索引存在但 _count 报 not_found
		_, err := doSwitch(t, s, swAlias, idxNew, idxOld, false)
		wantErrIs(t, err, model.ErrTargetIndexNotFound, "count 404 → doc 数按 0")
		wantContains(t, err.Error(), "文档数为 0", "错误原因")
	})
}

// 互斥：抢不到锁的实例必须让路，且不能去释放别人的锁。
func TestSwitchAlias_LockBusySkipsAllIO(t *testing.T) {
	s := newSwitchStore(t)
	s.cache.locks[lockKey] = true // 另一实例正在切换
	s.seedCacheActive(swAlias, idxOld)
	reply, err := doSwitch(t, s, swAlias, idxNew, idxOld, false)
	wantErr(t, err, "并发切换必须有一方让路")
	wantContains(t, err.Error(), "正在被其它实例切换", "错误原因")
	if reply != nil {
		t.Fatalf("失败时响应必须为 nil, got %+v", reply)
	}
	wantOps(t, s.ops(), lockAcq) // 注意：没有 ReleaseLock，让路者不得释放持有者的锁
	wantNoCall(t, s.ops(), "es.")
	wantNoCall(t, s.ops(), "version.")
	wantEQ(t, s.cache.active[swAlias], idxOld, "缓存原样")
	if !s.cache.locks[lockKey] {
		t.Fatal("让路者把别人的锁删掉了")
	}
}

func TestSwitchAlias_LockErrorPropagates(t *testing.T) {
	s := newSwitchStore(t)
	s.fail("cache.AcquireLock", errOther)
	_, err := doSwitch(t, s, swAlias, idxNew, idxOld, false)
	wantErrIs(t, err, errOther, "Redis 故障不能当成「没抢到」静默放弃")
	wantOps(t, s.ops(), lockAcq)
	wantNoCall(t, s.ops(), "es.")
}

// 乐观校验失败：OpenSearch 与登记表都必须保持原样（后到者不得静默摘掉前者刚上线的索引）。
func TestSwitchAlias_OptimisticMismatchLeavesEverythingUntouched(t *testing.T) {
	s := newSwitchStore(t)
	s.seedCacheActive(swAlias, idxOld)
	_, err := doSwitch(t, s, swAlias, idxNew, "gc_v1_999", false)
	wantErr(t, err, "expected_current 与实际指向不符")
	wantContains(t, err.Error(), "alias state mismatch", "错误定位到别名状态校验")
	wantOps(t, s.ops(),
		lockAcq,
		"es.IndexExists:gc_v1_200",
		"es.Count:gc_v1_200",
		"es.AliasTargets:gc",
		lockRel,
	)
	wantNoCall(t, s.ops(), "es.ApplyAliasActions")
	wantNoCall(t, s.ops(), "version.")
	wantEQ(t, s.ver.activeRow(swAlias).IndexName, idxOld, "登记表保持原样")
	wantEQ(t, s.es.aliasTargets[swAlias][0], idxOld, "别名保持原样")
	wantEQ(t, s.cache.active[swAlias], idxOld, "缓存保持原样")
}

// 别名指向读失败必须传播，绝不能被当成「别名不存在 → 首次挂载」。
func TestSwitchAlias_AliasTargetsFailurePropagates(t *testing.T) {
	s := newSwitchStore(t)
	s.fail("es.AliasTargets", errBoom)
	_, err := doSwitch(t, s, swAlias, idxNew, idxOld, false)
	wantErrIs(t, err, errBoom, "读失败必须传播")
	wantNoCall(t, s.ops(), "es.ApplyAliasActions")
	wantNoCall(t, s.ops(), "version.")
	wantOps(t, s.ops(), lockAcq, "es.IndexExists:gc_v1_200", "es.Count:gc_v1_200", "es.AliasTargets:gc", lockRel)
}

func TestSwitchAlias_TargetIndexMissingRejected(t *testing.T) {
	s := newSwitchStore(t)
	delete(s.es.exists, idxNew)
	_, err := doSwitch(t, s, swAlias, idxNew, idxOld, false)
	wantErrIs(t, err, model.ErrTargetIndexNotFound, "目标索引不存在")
	wantOps(t, s.ops(), lockAcq, "es.IndexExists:gc_v1_200", lockRel)
	wantNoCall(t, s.ops(), "es.Count")
	if len(s.es.applied) != 0 {
		t.Fatal("不存在的索引被挂上了别名")
	}
}

func TestSwitchAlias_CountFailurePropagates(t *testing.T) {
	s := newSwitchStore(t)
	s.fail("es.Count", errBoom)
	_, err := doSwitch(t, s, swAlias, idxNew, idxOld, false)
	wantErrIs(t, err, errBoom, "_count 失败必须传播")
	wantNoCall(t, s.ops(), "es.ApplyAliasActions")
	wantNoCall(t, s.ops(), "version.")
}

// 登记表更新失败：切换已在 OpenSearch 生效，错误里必须写明「需人工补偿」。
// 同时钉住当前实现的副作用：写入索引缓存**没有**被失效（见 README 缺口 8）。
func TestSwitchAlias_RegistryFailureKeepsESAppliedButStaleCache(t *testing.T) {
	s := newSwitchStore(t)
	s.seedCacheActive(swAlias, idxOld)
	s.fail(swOldToNew, errOther)
	reply, err := doSwitch(t, s, swAlias, idxNew, idxOld, false)
	wantErrIs(t, err, errOther, "登记表失败必须上报")
	wantContains(t, err.Error(), "需人工补偿", "错误里要给出补偿指引")
	wantContains(t, err.Error(), "已在 OpenSearch 生效", "错误里要说明爆炸半径")
	if reply != nil {
		t.Fatalf("失败时响应必须为 nil, got %+v", reply)
	}
	wantOps(t, s.ops(),
		lockAcq,
		"es.IndexExists:gc_v1_200",
		"es.Count:gc_v1_200",
		"es.AliasTargets:gc",
		"es.ApplyAliasActions:remove:gc_v1_100,add:gc_v1_200",
		"version.FindByIndexName:gc_v1_200",
		"version.UpdateDocCount:gc_v1_200",
		swOldToNew,
		lockRel,
	)
	// 查询侧已经指向新索引，但登记表还说老索引是 active —— 巡检必须能暴露它。
	wantEQ(t, s.es.aliasTargets[swAlias][0], idxNew, "OpenSearch 侧切换不回滚")
	wantEQ(t, s.ver.activeRow(swAlias).IndexName, idxOld, "登记表未推进")
	// 缺口 8：此时缓存仍把写入路由到已被查询侧摘掉的老索引（最长 30 秒的静默投影错位）。
	wantEQ(t, s.cache.active[swAlias], idxOld, "已知行为：缓存未失效")
	if _, ok := s.cache.active[swAlias]; !ok {
		t.Fatal("前提被破坏：缓存本应仍持有老索引")
	}
}

// 登记前置查询失败：同样发生在 OpenSearch 已生效之后，必须传播且不改登记表。
func TestSwitchAlias_EnsureRegisteredFailurePropagates(t *testing.T) {
	s := newSwitchStore(t)
	s.fail("version.FindByIndexName", errBoom)
	_, err := doSwitch(t, s, swAlias, idxNew, idxOld, false)
	wantErrIs(t, err, errBoom, "必须传播")
	wantNoCall(t, s.ops(), "version.SwitchActive")
	wantNoCall(t, s.ops(), "cache.DelActiveIndex")
	wantCount(t, s.ops(), "cache.ReleaseLock", 1)
	wantEQ(t, s.ver.activeRow(swAlias).IndexName, idxOld, "登记表保持原样")
}

// 别名归一化（大小写/空白）与默认别名回退。
func TestSwitchAlias_NormalizesAliasAndDefaults(t *testing.T) {
	s := newSwitchStore(t)
	reply, err := doSwitch(t, s, "  GC  ", idxNew, idxOld, false)
	wantNoErr(t, err)
	wantEQ(t, reply.Alias, swAlias, "reply.Alias 必须小写归一化")
	wantContains(t, s.ops()[0], "si:sw:gc|", "锁 key 用归一化后的别名: "+s.ops()[0])

	// 空别名 = 默认别名（IndexPrefix）。
	s2 := newSwitchStore(t)
	if _, err := doSwitch(t, s2, "", idxNew, idxOld, false); err != nil {
		t.Fatal(err)
	}
	wantEQ(t, s2.ver.activeRow(swAlias).IndexName, idxNew, "默认别名同样完成切换")
}

// 无 Redis 时退化为单实例语义：抢锁总是成功、没有缓存可失效，幂等仍由登记表条件更新保证。
func TestSwitchAlias_DegradesWithoutRedis(t *testing.T) {
	s := newTestStoreNoRedis(t, repository.Options{IndexPrefix: swAlias, SchemaVersion: "v1"})
	s.seedActiveIndex(swAlias, idxOld, 10)
	s.es.seedAlias(swAlias, idxOld)
	s.es.seedIndex(idxNew, 42)
	s.ver.seed(&model.SearchIndexVersion{Alias: swAlias, IndexName: idxNew, SchemaVersion: "v1",
		DocCount: 0, State: model.VersionStateRetiring, CreatedBy: "rebuild task-1", Ctime: 1, Mtime: 1})
	reply, err := doSwitch(t, s, swAlias, idxNew, idxOld, false)
	wantNoErr(t, err)
	wantEQ(t, reply.CurrentIndex, idxNew, "reply.CurrentIndex")
	wantOps(t, s.ops(),
		"es.IndexExists:gc_v1_200",
		"es.Count:gc_v1_200",
		"es.AliasTargets:gc",
		"es.ApplyAliasActions:remove:gc_v1_100,add:gc_v1_200",
		"version.FindByIndexName:gc_v1_200",
		"version.UpdateDocCount:gc_v1_200",
		swOldToNew,
	)
	// 没有任何缓存/锁动作 —— 互斥退化掉，但登记表 CAS 仍保证一个别名一行 active。
	wantEQ(t, s.ver.activeRow(swAlias).IndexName, idxNew, "active 行")
	wantEQ(t, s.ver.rowOf(idxOld).State, model.VersionStateRetiring, "老索引转 retiring")
}
