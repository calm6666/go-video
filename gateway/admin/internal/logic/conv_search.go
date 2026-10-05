// 本文件是 gateway/admin 的手写投影扩展（非 goctl 生成产物）：search-indexer RPC → 运营后台投影。
//
// 索引是投影而不是事实源（AGENTS.md §5）：网关不写索引、不直连 OpenSearch，
// 只把重建任务/别名健康搬运给后台，进度与终态全部以 search-indexer 返回为准。

package logic

import (
	"go-video/gateway/admin/internal/types"
	searchindexerrpc "go-video/services/search-indexer/rpc"
)

const (
	// searchMaxRebuildLimit 与 search-indexer model 的任务表分页上限一致。
	searchMaxRebuildLimit = 100
	// searchDefaultRebuildLimit 是服务侧的回落页大小：越界不截断，直接回落到默认值。
	searchDefaultRebuildLimit = 20
)

// normalizeSearchLimit 复刻 search-indexer indextaskmodel.List 的 limit 归一逻辑，
// 保证后台传 500 时拿到的仍是 20 条，而不是网关擅自放大到 100。
func normalizeSearchLimit(limit int32) int32 {
	if limit <= 0 || limit > searchMaxRebuildLimit {
		return searchDefaultRebuildLimit
	}
	return limit
}

// rebuildTaskToAPI 投影重建任务。state/scope 是服务端字符串枚举，
// 网关不做映射，避免后台展示的进度与任务表不一致。
func rebuildTaskToAPI(t *searchindexerrpc.RebuildTask) types.SearchRebuildTaskItem {
	return types.SearchRebuildTaskItem{
		TaskId:      t.GetTaskId(),
		Scope:       t.GetScope(),
		ScopeValue:  t.GetScopeValue(),
		State:       t.GetState(),
		CursorValue: t.GetCursorValue(),
		Total:       t.GetTotal(),
		Processed:   t.GetProcessed(),
		Failed:      t.GetFailed(),
		TargetIndex: t.GetTargetIndex(),
		Alias:       t.GetAlias(),
		Operator:    t.GetOperator(),
		RequestId:   t.GetRequestId(),
		Ctime:       t.GetCtime(),
		Mtime:       t.GetMtime(),
		StartedAt:   t.GetStartedAt(),
		FinishedAt:  t.GetFinishedAt(),
		LastError:   t.GetLastError(),
		DlqCount:    t.GetDlqCount(),
	}
}

// rebuildTasksToAPI 投影任务列表，nil 输入返回空切片。
func rebuildTasksToAPI(list []*searchindexerrpc.RebuildTask) []types.SearchRebuildTaskItem {
	out := make([]types.SearchRebuildTaskItem, 0, len(list))
	for _, t := range list {
		out = append(out, rebuildTaskToAPI(t))
	}
	return out
}

// aliasStatusesToAPI 投影别名健康；doc_count 为 -1 表示服务端读不到，网关不改写成 0。
func aliasStatusesToAPI(list []*searchindexerrpc.AliasStatus) []types.SearchAliasStatusItem {
	out := make([]types.SearchAliasStatusItem, 0, len(list))
	for _, a := range list {
		out = append(out, types.SearchAliasStatusItem{
			Alias:         a.GetAlias(),
			ActiveIndex:   a.GetActiveIndex(),
			SchemaVersion: a.GetSchemaVersion(),
			DocCount:      a.GetDocCount(),
			IndexExists:   a.GetIndexExists(),
			Health:        a.GetHealth(),
			State:         a.GetState(),
		})
	}
	return out
}
