// convert.go 负责 rpc 契约与内部投影结构之间的转换。
//
// 放在 logic 包内（AGENTS.md §3：logic 是业务映射层），这样 esclient 与 repository
// 不必依赖生成代码，单测也能直接构造 *esclient.ContentDoc。
package logic

import (
	"errors"
	"fmt"

	"go-video/services/search-indexer/internal/esclient"
	"go-video/services/search-indexer/internal/repository"
	"go-video/services/search-indexer/model"
	"go-video/services/search-indexer/rpc"
)

// errContentIDMismatch 请求参数与文档主体的 content_id 不一致（调用方 bug，必须拒绝）。
var errContentIDMismatch = errors.New("search-indexer: content_id 与 doc.content_id 不一致")

// docFromRPC 把上游推送的事实快照转成索引文档。
//
// 约束（AGENTS.md §5）：本服务不补齐事实字段，doc 为空即报错；
// state 必须是 1..5 的已知状态，未知状态一律拒绝写入而不是按「发布」处理。
func docFromRPC(in *rpc.ContentDoc) (*esclient.ContentDoc, error) {
	if in == nil {
		return nil, model.ErrContentSnapshotRequired
	}
	state, err := stateOfContentState(in.State)
	if err != nil {
		return nil, err
	}
	doc := &esclient.ContentDoc{
		ContentID:      in.ContentId,
		ContentType:    int32(in.ContentType),
		Title:          in.Title,
		Description:    in.Description,
		CoverURL:       in.CoverUrl,
		AuthorMid:      in.AuthorMid,
		AuthorName:     in.AuthorName,
		Typeid:         in.Typeid,
		TypeName:       in.TypeName,
		Tags:           in.Tags,
		DurationSec:    in.DurationSec,
		PublishAt:      in.PublishAt,
		Ctime:          in.Ctime,
		State:          state,
		DocRevision:    in.DocRevision,
		RightsExpireAt: in.RightsExpireAt,
		Language:       in.Language,
		SubtitleLangs:  in.SubtitleLangs,
		Sensitive:      in.Sensitive,
		SchemaVersion:  int(in.SchemaVersion),
	}
	if in.Heat != nil {
		doc.Heat = esclient.Heat{
			ViewCount:     in.Heat.ViewCount,
			LikeCount:     in.Heat.LikeCount,
			FavoriteCount: in.Heat.FavoriteCount,
			ShareCount:    in.Heat.ShareCount,
			CommentCount:  in.Heat.CommentCount,
			DanmakuCount:  in.Heat.DanmakuCount,
			HeatScore:     in.Heat.HeatScore,
			HeatRevision:  in.Heat.HeatRevision,
		}
	}
	if doc.SchemaVersion == 0 {
		doc.SchemaVersion = 1
	}
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	return doc, nil
}

// stateOfContentState 把 rpc 状态转成 int32，未指定状态返回错误。
func stateOfContentState(s rpc.ContentState) (int32, error) {
	v := int32(s)
	if v < esclient.StatePending || v > esclient.StateDeleted {
		return 0, fmt.Errorf("search-indexer: 未知 content_state %d", v)
	}
	return v, nil
}

// taskToRPC 任务投影转换。dlqCount 是当前待处理死信总数（观测值，不是任务字段）。
func taskToRPC(t *model.SearchIndexTask, dlqCount int64) *rpc.RebuildTask {
	if t == nil {
		return nil
	}
	return &rpc.RebuildTask{
		TaskId:      t.TaskID,
		Scope:       t.Scope,
		ScopeValue:  t.ScopeValue,
		State:       t.State,
		CursorValue: t.CursorValue,
		Total:       t.Total,
		Processed:   t.Processed,
		Failed:      t.Failed,
		TargetIndex: t.TargetIndex,
		Alias:       t.Alias,
		Operator:    t.Operator,
		RequestId:   t.RequestID,
		Ctime:       t.Ctime,
		Mtime:       t.Mtime,
		StartedAt:   t.StartedAt,
		FinishedAt:  t.FinishedAt,
		LastError:   t.LastError,
		DlqCount:    dlqCount,
	}
}

// aliasHealthToRPC 健康巡检结果转换。
func aliasHealthToRPC(h repository.AliasHealth) *rpc.AliasStatus {
	return &rpc.AliasStatus{
		Alias:         h.Alias,
		ActiveIndex:   h.ActiveIndex,
		SchemaVersion: h.SchemaVersion,
		DocCount:      h.DocCount,
		IndexExists:   h.IndexExists,
		Health:        h.Health,
		State:         h.State,
	}
}

// overallState 汇总巡检结论：
//   - down      ：没有任何登记别名（索引从未建立）或有别名读不到物理索引；
//   - degraded  ：集群非 green 或 doc 数读不到，或存在待重试/死信积压；
//   - ok        ：其余情况。
func overallState(rows []repository.AliasHealth, retryPending, deadLetter int64) string {
	if len(rows) == 0 {
		return "down"
	}
	degraded := retryPending > 0 || deadLetter > 0
	for _, h := range rows {
		switch {
		case !h.IndexExists:
			return "down"
		case h.Health != "green":
			degraded = true
		case h.DocCount < 0:
			degraded = true
		}
	}
	if degraded {
		return "degraded"
	}
	return "ok"
}
