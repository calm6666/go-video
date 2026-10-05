package svc

import (
	"testing"

	"go-video/services/recommend-rank/internal/config"
	"go-video/services/recommend-rank/model"
)

// TestRepoOptionsMapsEveryLimit 逐字段检查「配置 → 仓库参数」映射。
// 这些参数决定候选上限与数据保留期，接错一个字段不会编译失败，
// 只会在生产上表现成「保留期变成扫描上限」这类极难定位的行为。
func TestRepoOptionsMapsEveryLimit(t *testing.T) {
	opt := repoOptions(config.RankConf{
		DefaultModelKey:            "play_related_multi_gate",
		BucketCount:                2000,
		RunningExperimentScanLimit: 11,
		LayerVariantScanLimit:      12,
		AssignmentScanLimit:        13,
		MaxDigestAids:              14,
		ArchiveBatchSize:           15,
		DecisionRetentionDays:      16,
		AssignmentRetentionDays:    17,
	})
	want := map[string]int64{
		"BucketCount":                2000,
		"RunningExperimentScanLimit": 11,
		"LayerVariantScanLimit":      12,
		"AssignmentScanLimit":        13,
		"MaxDigestAids":              14,
		"ArchiveBatchSize":           15,
		"DecisionRetentionDays":      16,
		"AssignmentRetentionDays":    17,
	}
	got := map[string]int64{
		"BucketCount":                int64(opt.BucketCount),
		"RunningExperimentScanLimit": int64(opt.RunningExperimentScanLimit),
		"LayerVariantScanLimit":      int64(opt.LayerVariantScanLimit),
		"AssignmentScanLimit":        int64(opt.AssignmentScanLimit),
		"MaxDigestAids":              int64(opt.MaxDigestAids),
		"ArchiveBatchSize":           int64(opt.ArchiveBatchSize),
		"DecisionRetentionDays":      int64(opt.DecisionRetentionDays),
		"AssignmentRetentionDays":    int64(opt.AssignmentRetentionDays),
	}
	for name, w := range want {
		if g := got[name]; g != w {
			t.Errorf("%s 映射错误: got %d want %d", name, g, w)
		}
	}
	if opt.DefaultModelKey != "play_related_multi_gate" {
		t.Errorf("DefaultModelKey = %q", opt.DefaultModelKey)
	}
}

// TestRepoOptionsFallsBackOnZeroConfig 零值配置必须落到安全默认值：
// 保留期或扫描上限为 0 会让「清理不跑」或「读不到任何实验」，都是静默故障。
func TestRepoOptionsFallsBackOnZeroConfig(t *testing.T) {
	opt := repoOptions(config.RankConf{})
	if opt.DefaultModelKey != fallbackDefaultModelKey {
		t.Errorf("DefaultModelKey 未兜底: %q", opt.DefaultModelKey)
	}
	if opt.BucketCount != model.DefaultBucketCount {
		t.Errorf("BucketCount 未兜底: %d", opt.BucketCount)
	}
	for name, v := range map[string]int{
		"RunningExperimentScanLimit": opt.RunningExperimentScanLimit,
		"LayerVariantScanLimit":      opt.LayerVariantScanLimit,
		"AssignmentScanLimit":        opt.AssignmentScanLimit,
		"MaxDigestAids":              opt.MaxDigestAids,
		"ArchiveBatchSize":           opt.ArchiveBatchSize,
		"DecisionRetentionDays":      opt.DecisionRetentionDays,
		"AssignmentRetentionDays":    opt.AssignmentRetentionDays,
	} {
		if v <= 0 {
			t.Errorf("%s 非正值未被兜底: %d", name, v)
		}
	}
}

func TestPositiveOr(t *testing.T) {
	if got := positiveOr[int32](0, 100); got != 100 {
		t.Errorf("positiveOr(0)=%d", got)
	}
	if got := positiveOr[int32](-5, 100); got != 100 {
		t.Errorf("positiveOr(-5)=%d", got)
	}
	if got := positiveOr[int32](7, 100); got != 7 {
		t.Errorf("positiveOr(7)=%d", got)
	}
	if got := positiveOr[int64](0, 30); got != 30 {
		t.Errorf("positiveOr[int64](0)=%d", got)
	}
}
