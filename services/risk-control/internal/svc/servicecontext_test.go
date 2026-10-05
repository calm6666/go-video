package svc

import (
	"testing"

	"go-video/services/risk-control/internal/config"
	"go-video/services/risk-control/internal/policy"
	"go-video/services/risk-control/model"
)

// policyConfig 是「配置 → 引擎语义」的唯一映射点：
// 写错一次就会让线上降级方向相反（该拒的放行 / 该放的拒绝），因此在装配层单独测。
func TestPolicyConfigMapsDegradeSwitch(t *testing.T) {
	base := config.RiskControlConf{
		ChallengeTTLSeconds:    300,
		MaxHitRulesPerDecision: 20,
		HighRiskActions:        []int64{int64(model.ActionSubmitVideo), int64(model.ActionLogin)},
		OnDbFailureDefault:     "allow",
		LocalFallbackPerWindow: 200,
	}

	allowCfg := policyConfig(base)
	if allowCfg.ChallengeTTLSeconds != 300 || allowCfg.MaxHitsPerDecision != 20 {
		t.Fatalf("引擎参数未从配置透传: %+v", allowCfg)
	}
	if allowCfg.Degrade.DecisionFor(model.ActionSubmitVideo) != model.DecisionBlock {
		t.Fatal("高危动作必须 BLOCK-on-error")
	}
	if allowCfg.Degrade.DecisionFor(model.ActionComment) != model.DecisionAllow {
		t.Fatal("OnDbFailureDefault=allow 时低危动作应 ALLOW-on-error")
	}
	if len(allowCfg.Degrade.HighRiskActions) != 2 || allowCfg.Degrade.HighRiskActions[0] != model.ActionSubmitVideo {
		t.Fatalf("int64 -> int32 转换异常: %v", allowCfg.Degrade.HighRiskActions)
	}
	if allowCfg.Degrade.LocalFallbackLimit != 200 {
		t.Fatalf("兜底阈值未透传: %d", allowCfg.Degrade.LocalFallbackLimit)
	}

	block := base
	block.OnDbFailureDefault = "block"
	blockCfg := policyConfig(block)
	for _, action := range []int32{model.ActionComment, model.ActionDanmaku, model.ActionFollow} {
		if blockCfg.Degrade.DecisionFor(action) != model.DecisionBlock {
			t.Fatalf("OnDbFailureDefault=block 时动作 %d 应全量拒绝", action)
		}
	}

	// 未配置高危动作时不得退化为「全部 ALLOW-on-error」，由引擎默认集合兜底。
	empty := config.RiskControlConf{OnDbFailureDefault: "allow"}
	emptyCfg := policyConfig(empty)
	if len(emptyCfg.Degrade.HighRiskActions) != 0 {
		t.Fatalf("配置为空时应交由引擎 WithDefaults 补默认集合，实际 %v", emptyCfg.Degrade.HighRiskActions)
	}
	resolved := emptyCfg.WithDefaults()
	if resolved.Degrade.DecisionFor(model.ActionSubmitVideo) != model.DecisionBlock {
		t.Fatal("默认高危集合缺失会让投稿在 DB 故障时被放行")
	}
	if resolved.Degrade.DecisionFor(model.ActionComment) != model.DecisionAllow {
		t.Fatal("默认低危动作应保持 ALLOW-on-error")
	}
	if resolved.ChallengeTTLSeconds <= 0 || resolved.MaxHitsPerDecision <= 0 {
		t.Fatalf("零值应回落到引擎默认: %+v", resolved)
	}
}

// 高危动作枚举必须与 model/proto 保持同步，否则配置里的数字会指向别的动作。
func TestHighRiskActionConstantsMatchProtoNumbering(t *testing.T) {
	want := map[int32]string{
		model.ActionSubmitVideo: "submit_video",
		model.ActionComment:     "comment",
		model.ActionDanmaku:     "danmaku",
		model.ActionFollow:      "follow",
		model.ActionLogin:       "login",
		model.ActionRename:      "rename",
		model.ActionLiveStart:   "live_start",
	}
	if len(want) != len(policy.DefaultHighRiskActions)+3 {
		t.Fatalf("受保护动作数量变化（默认高危 %d 个），请同步 README 与配置示例", len(policy.DefaultHighRiskActions))
	}
	for _, a := range policy.DefaultHighRiskActions {
		if _, ok := want[a]; !ok {
			t.Fatalf("默认高危动作 %d 不在受保护动作枚举内", a)
		}
	}
}
