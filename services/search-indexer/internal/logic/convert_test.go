// convert_test.go 覆盖 rpc 契约 → 内部投影的映射规则（纯函数，不启动服务）。
package logic

import (
	"testing"

	"go-video/services/search-indexer/internal/esclient"
	"go-video/services/search-indexer/internal/repository"
	"go-video/services/search-indexer/model"
	"go-video/services/search-indexer/rpc"
)

func TestDocFromRPC_RequiresSnapshot(t *testing.T) {
	if _, err := docFromRPC(nil); err != model.ErrContentSnapshotRequired {
		t.Fatalf("空快照必须报错（本服务不猜字段），实际 %v", err)
	}
}

func TestDocFromRPC_FieldMapping(t *testing.T) {
	in := &rpc.ContentDoc{
		ContentId: 88, ContentType: rpc.ContentType_CONTENT_TYPE_PGC_EPISODE,
		Title: "第一集", Description: "简介", CoverUrl: "https://cdn/a.jpg",
		AuthorMid: 7, AuthorName: "版权方", Typeid: 29, TypeName: "番剧",
		Tags: []string{"番剧"}, DurationSec: 1440, PublishAt: 100, Ctime: 90,
		State:       rpc.ContentState_CONTENT_STATE_PUBLISHED,
		DocRevision: 12345, RightsExpireAt: 999, Language: "zh-CN",
		SubtitleLangs: []string{"zh-CN", "ja-JP"}, Sensitive: true,
		Heat: &rpc.HeatSnapshot{ViewCount: 10, LikeCount: 2, HeatScore: 60, HeatRevision: 12000},
	}
	doc, err := docFromRPC(in)
	if err != nil {
		t.Fatal(err)
	}
	if doc.ContentID != 88 || doc.ContentType != 2 || doc.State != esclient.StatePublished {
		t.Fatalf("关键字段映射异常: %+v", doc)
	}
	if doc.RightsExpireAt != 999 || doc.Language != "zh-CN" || len(doc.SubtitleLangs) != 2 {
		t.Fatalf("版权/字幕字段丢失: %+v", doc)
	}
	if doc.Heat.ViewCount != 10 || doc.Heat.HeatScore != 60 || doc.Heat.HeatRevision != 12000 {
		t.Fatalf("热度快照未透传: %+v", doc.Heat)
	}
	if doc.SchemaVersion != 1 {
		t.Fatalf("schema_version 未回填默认值 1: %d", doc.SchemaVersion)
	}
	if doc.ID() != "2_88" {
		t.Fatalf("文档主键 = %s, want 2_88", doc.ID())
	}

	// 上游显式给了结构版本（如 v2 灰度）时不得被默认值覆盖。
	// 这里重新构造最小快照而不是 `*in` 值复制：protobuf 消息内嵌 sync.Mutex，
	// 复制会被 go vet 判为 copylocks。
	explicit := &rpc.ContentDoc{
		ContentId: 88, ContentType: rpc.ContentType_CONTENT_TYPE_PGC_EPISODE,
		Title: "第一集", State: rpc.ContentState_CONTENT_STATE_PUBLISHED,
		DocRevision: 12345, SchemaVersion: 2,
	}
	doc2, err := docFromRPC(explicit)
	if err != nil {
		t.Fatalf("显式 schema_version 的合法快照被拒绝: %v", err)
	}
	if doc2.SchemaVersion != 2 {
		t.Fatalf("schema_version 应透传, 实际 %d", doc2.SchemaVersion)
	}
}

// TestDocFromRPC_RejectsUnknownState 未知状态一律拒绝，
// 不能因为枚举扩展就把「不认识」当成「可检索」。
func TestDocFromRPC_RejectsUnknownState(t *testing.T) {
	base := func(s rpc.ContentState) *rpc.ContentDoc {
		return &rpc.ContentDoc{
			ContentId: 1, ContentType: rpc.ContentType_CONTENT_TYPE_UGC_VIDEO,
			Title: "T", State: s, DocRevision: 1,
		}
	}
	if _, err := docFromRPC(base(rpc.ContentState_CONTENT_STATE_UNSPECIFIED)); err == nil {
		t.Fatal("UNSPECIFIED 状态必须拒绝")
	}
	if _, err := docFromRPC(base(rpc.ContentState(99))); err == nil {
		t.Fatal("越界状态必须拒绝")
	}
	for _, s := range []rpc.ContentState{
		rpc.ContentState_CONTENT_STATE_PENDING, rpc.ContentState_CONTENT_STATE_PUBLISHED,
		rpc.ContentState_CONTENT_STATE_OFFLINE, rpc.ContentState_CONTENT_STATE_EXPIRED,
		rpc.ContentState_CONTENT_STATE_DELETED,
	} {
		if _, err := stateOfContentState(s); err != nil {
			t.Fatalf("合法状态 %d 被拒绝: %v", s, err)
		}
	}
	// 缺 doc_revision 的快照无法参与防旧覆盖新判定。
	if _, err := docFromRPC(base(rpc.ContentState_CONTENT_STATE_PUBLISHED)); err != nil {
		t.Fatal("测试前提被破坏")
	}
	noRev := base(rpc.ContentState_CONTENT_STATE_PUBLISHED)
	noRev.DocRevision = 0
	if _, err := docFromRPC(noRev); err == nil {
		t.Fatal("缺 doc_revision 必须拒绝")
	}
	// 未指定内容类型时 ES 主键无法定位。
	noType := base(rpc.ContentState_CONTENT_STATE_PUBLISHED)
	noType.ContentType = rpc.ContentType_CONTENT_TYPE_UNSPECIFIED
	if _, err := docFromRPC(noType); err == nil {
		t.Fatal("content_type 未指定必须拒绝")
	}
}

func TestTaskToRPC(t *testing.T) {
	if taskToRPC(nil, 0) != nil {
		t.Fatal("nil 任务应返回 nil")
	}
	task := &model.SearchIndexTask{
		TaskID: "sit_1", Scope: model.ScopeFull, State: model.TaskStateRunning,
		CursorValue: "100001", Processed: 5, Failed: 1, Total: 10,
		TargetIndex: "go_video_content_v1_2", Alias: "go_video_content",
		RequestID: "req-1", LastError: "boom",
	}
	got := taskToRPC(task, 7)
	if got.TaskId != "sit_1" || got.State != model.TaskStateRunning || got.CursorValue != "100001" {
		t.Fatalf("任务转换异常: %+v", got)
	}
	if got.Processed != 5 || got.Failed != 1 || got.Total != 10 {
		t.Fatalf("进度字段丢失: %+v", got)
	}
	// DlqCount 是观测值，必须由调用方注入而不是留在任务里。
	if got.DlqCount != 7 {
		t.Fatalf("DlqCount = %d, want 7", got.DlqCount)
	}
	if got.RequestId != "req-1" || got.LastError != "boom" {
		t.Fatalf("审计字段丢失: %+v", got)
	}
}

// TestOverallState 健康结论必须能被运维直接采信：
// 索引读不到就是 down，积压就是 degraded，不能一律报 ok。
func TestOverallState(t *testing.T) {
	green := repository.AliasHealth{Alias: "a", ActiveIndex: "a_v1_1", IndexExists: true, Health: "green", DocCount: 10, State: model.VersionStateActive}
	yellow := green
	yellow.Health = "yellow"
	missing := green
	missing.IndexExists = false
	unknownCount := green
	unknownCount.DocCount = -1

	cases := []struct {
		name         string
		rows         []repository.AliasHealth
		retryPending int64
		deadLetter   int64
		want         string
	}{
		{"无登记（索引从未建立）", nil, 0, 0, "down"},
		{"全部正常", []repository.AliasHealth{green}, 0, 0, "ok"},
		{"集群 yellow", []repository.AliasHealth{yellow}, 0, 0, "degraded"},
		{"active 索引已消失", []repository.AliasHealth{missing}, 0, 0, "down"},
		{"doc 数读不到", []repository.AliasHealth{unknownCount}, 0, 0, "degraded"},
		{"有重试积压", []repository.AliasHealth{green}, 3, 0, "degraded"},
		{"有死信", []repository.AliasHealth{green}, 0, 1, "degraded"},
		{"缺失索引优先于降级", []repository.AliasHealth{green, missing}, 5, 5, "down"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := overallState(c.rows, c.retryPending, c.deadLetter); got != c.want {
				t.Fatalf("overallState = %s, want %s", got, c.want)
			}
		})
	}
}

func TestAliasHealthToRPC(t *testing.T) {
	got := aliasHealthToRPC(repository.AliasHealth{
		Alias: "a", ActiveIndex: "a_v1_1", SchemaVersion: "v1", DocCount: 3,
		IndexExists: true, Health: "green", State: model.VersionStateActive, AliasTargets: []string{"a_v1_1"},
	})
	if got.Alias != "a" || got.ActiveIndex != "a_v1_1" || got.SchemaVersion != "v1" || got.DocCount != 3 ||
		!got.IndexExists || got.Health != "green" || got.State != model.VersionStateActive {
		t.Fatalf("巡检字段转换异常: %+v", got)
	}
}
