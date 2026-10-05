// 本文件是 logic 包的手写策略扩展（回放/回收的状态机裁决与长文本归一），
// 与 helpers.go 同属「允许手写的非生成落点」，不是 goctl 生成产物。
//
// 分工（AGENTS.md §4/§8）：model 的迁移表负责「哪些边合法」，这里补的是
// model 词表覆盖不到的 logic 侧判据——投影态的越界/回退、上报字段的一致性、
// 以及「哪个状态才允许做某个动作」这类跨表前置条件。判据一律是纯函数，便于单测直接覆盖。

package logic

import (
	"fmt"
	"strings"

	"go-video/services/live-media/internal/config"
	"go-video/services/live-media/model"

	"github.com/zeromicro/go-zero/core/logx"
)

// reviewProjectionTransitions video→live-media 单向投影（live_replay_asset_ref.review_state）
// 的合法迁移表。语义与 rpc.ReviewState 注释一致（proto line 91-101）：
// 这一列是 video 事实状态在本地的只读投影，方向单一，绝不反向推进稿件。
//
// 与任务状态机的分工：投影可以来回（下架后可再上架），
// 但 live_replay_task 的 COMPLETED 一旦被驱动就不回退（replayTransitions 里没有出边）。
// DELETED 是投影侧的终局：video 侧已删除的行不存在「复活」，
// 允许它回到 PUBLISHED 等于给一条已删稿件重新发对外可用性。
var reviewProjectionTransitions = map[int32][]int32{
	model.ReviewStateUnsynced: {
		model.ReviewStateReviewing, model.ReviewStateRejected, model.ReviewStatePublished,
		model.ReviewStateOffline, model.ReviewStateDeleted,
	},
	model.ReviewStateReviewing: {
		model.ReviewStateReviewing, model.ReviewStateRejected, model.ReviewStatePublished,
		model.ReviewStateDeleted,
	},
	// 驳回后可重新送审（申诉链路）或被删除；不允许直接跳到已发布。
	model.ReviewStateRejected: {
		model.ReviewStateReviewing, model.ReviewStateDeleted,
	},
	// 已发布可被下架/删除；下架后可重新发布（video 侧重新上线）。
	model.ReviewStatePublished: {
		model.ReviewStateOffline, model.ReviewStateDeleted,
	},
	model.ReviewStateOffline: {
		model.ReviewStatePublished, model.ReviewStateDeleted,
	},
	// model.ReviewStateDeleted: 终态，无出边。
}

// isValidReviewProjectionTransition 判断投影 from→to 是否合法。
// from 为 0（未同步）表示首次同步，任何具体结论都可落。
// from 不在已知取值内时按「不允许」处理：宁可拒绝一个未知投影值，也不把它当成合法边写进去。
func isValidReviewProjectionTransition(from, to int32) bool {
	if !model.ValidReviewState(to) {
		return false
	}
	if from == to {
		// 同值重放：允许（用于刷新 review_state_at/published_at，事件级幂等由 last_event_id 挡）。
		return true
	}
	for _, allow := range reviewProjectionTransitions[from] {
		if allow == to {
			return true
		}
	}
	return false
}

// reviewProjectionSource 白名单：投影必须说明事实来源，否则无法判断是事件驱动还是人工刷新。
var reviewProjectionSource = map[string]bool{
	"content.published.v1": true,
	"video.rpc":            true,
	"manual":               true,
}

// allowBindAssetStates 允许绑定 asset/稿件 的回放状态：产物必须已经存在。
// PENDING/MERGING/UPLOADING 时绑定等于给不存在的产物建引用（proto line 82-85 的状态语义）。
// 允许 REGISTERED/REVIEW_SUBMITTED：aid 常常在送审动作返回后才拿到，
// 只放开 REGISTERED 会让「先送审后回填 aid」的正常链路走不通。
var allowBindAssetStates = map[int32]bool{
	model.ReplayStateRegistered:      true,
	model.ReplayStateReviewSubmitted: true,
}

// replayProgressMonotonic 校验 Worker 上报的进度字段与已记录值不矛盾。
// 返回需要写入的（segmentCount, gapCount, durationMs），nil 表示本次不提供该列（patch 不动它）。
//
// 判据：
//   - 负数一律拒绝（计数不可能是负的，出现即调用方算错或串了任务）；
//   - 上报值 + 区间长度的关系：segment_count/gap_count 都不能超过 [from_seq,to_seq] 的片数，
//     两者之和也不能超过（一段切片不可能既是有效素材又是缺口）；
//   - duration_ms 只增不减：一次拼接的产物时长是产物事实，出现更小的值只可能是
//     旧任务/错 replay_id 的回执，收下就会把已登记的产物时长改坏；
//   - 0 表示「本次不提供」，不覆盖已记录值（Worker 心跳常带零值）。
func replayProgressMonotonic(cur *model.LiveReplayTask, rangeLen, segmentCount, gapCount, durationMs int64,
) (*int64, *int64, *int64, error) {
	if cur == nil {
		return nil, nil, nil, fmt.Errorf("live-media: replay task row is required: %w", model.ErrReplayTaskNotFound)
	}
	for name, v := range map[string]int64{"segment_count": segmentCount, "gap_count": gapCount, "duration_ms": durationMs} {
		if v < 0 {
			return nil, nil, nil, fmt.Errorf("live-media: %s=%d must not be negative: %w", name, v, model.ErrInvalidTransition)
		}
	}
	if rangeLen > 0 {
		if segmentCount > rangeLen {
			return nil, nil, nil, fmt.Errorf("live-media: segment_count=%d exceeds replay range %d: %w",
				segmentCount, rangeLen, model.ErrInvalidSegmentRange)
		}
		if gapCount > rangeLen {
			return nil, nil, nil, fmt.Errorf("live-media: gap_count=%d exceeds replay range %d: %w",
				gapCount, rangeLen, model.ErrInvalidSegmentRange)
		}
		if segmentCount+gapCount > rangeLen {
			return nil, nil, nil, fmt.Errorf("live-media: segment_count=%d + gap_count=%d exceeds replay range %d: %w",
				segmentCount, gapCount, rangeLen, model.ErrInvalidSegmentRange)
		}
	}
	if durationMs > 0 && cur.DurationMs > 0 && durationMs < cur.DurationMs {
		return nil, nil, nil, fmt.Errorf("live-media: duration_ms=%d shrinks recorded %d: %w",
			durationMs, cur.DurationMs, model.ErrInvalidTransition)
	}

	var seg, gap, dur *int64
	// 0 一律按「本次不提供」处理：Worker 的心跳常带零值，把零值当事实会把 SubmitReplayTask
	// 落好的素材数与缺口数抹掉。要改写这些列必须显式给出新值（正数）。
	if segmentCount > 0 {
		seg = i64p(segmentCount)
	}
	if gapCount > 0 {
		gap = i64p(gapCount)
	}
	if durationMs > 0 {
		dur = i64p(durationMs)
	}
	return seg, gap, dur, nil
}

// progressKeepsRow 判断一次进度上报是否「什么都不改」：
// 状态与产物引用和行内值一致，且携带的计数/时长也与行内值相同
// （未携带 = 不提供，同样算不改）。
//
// 这是幂等重放与非法迁移的分界：什么都不改 → 回当前行（重投的回执）；
// 改了任何东西但状态不变 → replayTransitions 里没有自环，只能判 ErrInvalidTransition，
// 否则就等于给 Worker 开了一条「不推进状态也能改写产物事实」的后门。
func progressKeepsRow(cur *model.LiveReplayTask, target int32, bucket, objectKey string,
	segmentCount, gapCount, durationMs *int64) bool {
	if cur == nil {
		return false
	}
	if !replayReportReplay(cur.State, target, cur.OutputBucket, bucket, cur.OutputKey, objectKey) {
		return false
	}
	for _, c := range []struct {
		want *int64
		have int64
	}{
		{segmentCount, cur.SegmentCount},
		{gapCount, cur.GapCount},
		{durationMs, cur.DurationMs},
	} {
		if c.want != nil && *c.want != c.have {
			return false
		}
	}
	return true
}

// --- 长文本归一（只用于展示性字段；参与幂等/对账的引用列一律拒绝截断） ---

// checkReplayTitle 校验回放标题：只做长度与空白归一，不做内容审核（内容安全归 moderation-orchestrator）。
// 超长不截断：标题会透传给 video 建稿，截出来的标题与投稿标题就是两份事实。
func checkReplayTitle(cfg config.LiveMediaConf, title string) (string, error) {
	t := redactSecrets(strings.TrimSpace(title))
	if t == "" {
		return "", nil
	}
	maxRunes := maxTitleRunes
	if cfg.ReplayTitleMaxLength > 0 {
		maxRunes = int(cfg.ReplayTitleMaxLength)
	}
	if runeLen(t) > maxRunes {
		return "", fmt.Errorf("live-media: replay title %d runes, max %d: %w", runeLen(t), maxRunes,
			model.ErrTitleTooLong)
	}
	return t, nil
}

// clampAuditText 纯展示性长文本（回放简介）：脱敏后按上限截断并告警，不拒绝整次提交。
func clampAuditText(name, text string, maxRunes int, logger logx.Logger) string {
	t := redactSecrets(strings.TrimSpace(text))
	if t == "" {
		return ""
	}
	if runeLen(t) > maxRunes {
		logger.Errorf("livemedia: %s truncated from %d to %d runes", name, runeLen(t), maxRunes)
		return truncateRunes(t, maxRunes)
	}
	return t
}

// checkOptionalBoundedRef 可选引用列：给了就必须是完整且合法的取值（bvid 一类）。
// 这类值是跨服务主键，截断后既不等于原值也查不到，只能拒绝。
func checkOptionalBoundedRef(name, v string, maxRunes int) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	if runeLen(v) > maxRunes {
		return "", fmt.Errorf("live-media: %s too long: %d > %d: %w", name, runeLen(v), maxRunes,
			model.ErrInvalidAid)
	}
	return v, nil
}

// --- 小工具 ---

func firstPositive(values ...int64) int64 {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}
	return 0
}

func boolText(v bool) string {
	if v {
		return "true"
	}
	return "false"
}
