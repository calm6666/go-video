package logic

// statemachine_test.go 锁定 catalog_episode 状态机的合法与非法迁移，
// 保证「回调/运营接口不能绕过校验直接写上架态」（AGENTS.md §8）。

import (
	"testing"

	"go-video/services/catalog/model"
)

func TestCanTransitionEpisodeLegal(t *testing.T) {
	legal := []struct {
		from, to int32
		desc     string
	}{
		{model.EpStateDraft, model.EpStateOnline, "草稿 → 上架"},
		{model.EpStateOnline, model.EpStateOffline, "上架 → 下架"},
		{model.EpStateOffline, model.EpStateOnline, "下架 → 重新上架"},
	}
	for _, c := range legal {
		if !canTransitionEpisode(c.from, c.to) {
			t.Errorf("canTransitionEpisode(%d,%d) = false, want true（%s）", c.from, c.to, c.desc)
		}
	}
}

func TestCanTransitionEpisodeIllegal(t *testing.T) {
	illegal := []struct {
		from, to int32
		desc     string
	}{
		{model.EpStateOnline, model.EpStateOnline, "已上架再上架走幂等分支，不算状态推进"},
		{model.EpStateOffline, model.EpStateOffline, "已下架幂等"},
		{model.EpStateDraft, model.EpStateOffline, "草稿不能直接下架"},
		{model.EpStateOnline, model.EpStateDraft, "上架不能退回草稿"},
		{model.EpStateOffline, model.EpStateDraft, "下架不能退回草稿"},
		{99, model.EpStateOnline, "未知来源态必须拒绝"},
		{model.EpStateDraft, 99, "未知目标态必须拒绝"},
	}
	for _, c := range illegal {
		if canTransitionEpisode(c.from, c.to) {
			t.Errorf("canTransitionEpisode(%d,%d) = true, want false（%s）", c.from, c.to, c.desc)
		}
	}
}

func TestPublishTransitionRequiresOnlineTarget(t *testing.T) {
	// 上架的唯一合法来源态是草稿与下架，其余一律拒绝。
	var allowed []int32
	for state := range legalEpisodeTransitions {
		if canTransitionEpisode(state, model.EpStateOnline) {
			allowed = append(allowed, state)
		}
	}
	if len(allowed) != 2 {
		t.Fatalf("可上架的来源态 = %v, want 草稿与下架共 2 个", allowed)
	}
}
