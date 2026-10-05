package repository

// 运营配置的乐观锁与取值校验用例（内存 model，不连数据库）。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/operation/model"
)

func newConfigRepository(current *model.OpsConfig) (*Repository, *fakeOpsConfigModel, *fakeAuditModel) {
	r := newTestRepository()
	cfgMd := &fakeOpsConfigModel{current: current}
	auditMd := &fakeAuditModel{}
	r.configMd = cfgMd
	r.auditMd = auditMd
	return r, cfgMd, auditMd
}

func storedConfig(version int64) *model.OpsConfig {
	return &model.OpsConfig{
		ID: 11, CfgKey: "moderation.auto_publish_threshold", CfgValue: "80",
		ValueType: ValueTypeInt, Scope: "global", Version: version, State: model.StateEnable,
	}
}

func TestSaveOpsConfigCreateWhenExpectVersionZero(t *testing.T) {
	ctx := context.Background()
	r, cfgMd, auditMd := newConfigRepository(nil)

	got, err := r.SaveOpsConfig(ctx, Actor{AdminID: 1, Username: "ops_lead"}, SaveConfigInput{
		CfgKey: "moderation.auto_publish_threshold", CfgValue: "80", ValueType: ValueTypeInt,
	})
	if err != nil {
		t.Fatalf("SaveOpsConfig: %v", err)
	}
	if cfgMd.insertCalls != 1 || cfgMd.updateCalls != 0 {
		t.Fatalf("insert/update calls = %d/%d, want 1/0", cfgMd.insertCalls, cfgMd.updateCalls)
	}
	if got.Version != 1 {
		t.Fatalf("created version = %d, want 1", got.Version)
	}
	if got.Scope != "global" || got.State != model.StateEnable {
		t.Fatalf("defaults not applied: %+v", got)
	}
	if len(auditMd.rows) != 1 || auditMd.rows[0].Action != actionConfigSave {
		t.Fatalf("audit rows = %+v", auditMd.rows)
	}
}

func TestSaveOpsConfigCreateOnExistingKeyRequiresExpectVersion(t *testing.T) {
	ctx := context.Background()
	r, _, _ := newConfigRepository(nil)
	// 唯一键冲突（并发下另一个操作者刚建了同名配置）→ 必须转成版本冲突，
	// 而不是静默覆盖或直接抛 driver 错误。
	r.configMd = &fakeOpsConfigModel{insertErr: model.ErrConfigExists}

	_, err := r.SaveOpsConfig(ctx, Actor{AdminID: 1}, SaveConfigInput{CfgKey: "a.b", CfgValue: "1"})
	if !errors.Is(err, model.ErrConfigVersionConflict) {
		t.Fatalf("err = %v, want ErrConfigVersionConflict", err)
	}
	if !strings.Contains(err.Error(), "expect_version") {
		t.Fatalf("error must tell the operator what to do: %v", err)
	}
}

func TestSaveOpsConfigVersionConflictFromExpectVersion(t *testing.T) {
	ctx := context.Background()
	r, cfgMd, auditMd := newConfigRepository(storedConfig(7))

	_, err := r.SaveOpsConfig(ctx, Actor{AdminID: 1}, SaveConfigInput{
		CfgKey:        "moderation.auto_publish_threshold",
		CfgValue:      "90",
		ValueType:     ValueTypeInt,
		ExpectVersion: 5, // 前端拿着旧版本编辑
	})
	if !errors.Is(err, model.ErrConfigVersionConflict) {
		t.Fatalf("err = %v, want ErrConfigVersionConflict", err)
	}
	// 冲突必须提示当前版本，运营才知道要重新拉取。
	if !strings.Contains(err.Error(), "7") {
		t.Fatalf("conflict error should carry the current version: %v", err)
	}
	if cfgMd.updateCalls != 0 {
		t.Fatal("must not write when the expected version is stale")
	}
	if len(auditMd.rows) != 0 {
		t.Fatal("failed write must not produce an ok audit row")
	}
}

func TestSaveOpsConfigVersionLostBetweenReadAndUpdate(t *testing.T) {
	ctx := context.Background()
	r, cfgMd, _ := newConfigRepository(storedConfig(7))
	// expect_version 与读到的行一致，但 UPDATE ... WHERE version = ? 未命中行：
	// 读后写之间被他人抢先推进，同样必须是版本冲突（不能返回“成功”）。
	cfgMd.mismatch = true

	_, err := r.SaveOpsConfig(ctx, Actor{AdminID: 1}, SaveConfigInput{
		CfgKey:        "moderation.auto_publish_threshold",
		CfgValue:      "90",
		ValueType:     ValueTypeInt,
		ExpectVersion: 7,
	})
	if !errors.Is(err, model.ErrConfigVersionConflict) {
		t.Fatalf("err = %v, want ErrConfigVersionConflict", err)
	}
	if cfgMd.updateCalls != 1 {
		t.Fatalf("update calls = %d, want 1", cfgMd.updateCalls)
	}
}

func TestSaveOpsConfigHappyPathBumpsVersion(t *testing.T) {
	ctx := context.Background()
	r, cfgMd, auditMd := newConfigRepository(storedConfig(7))

	got, err := r.SaveOpsConfig(ctx, Actor{AdminID: 1}, SaveConfigInput{
		CfgKey:        "moderation.auto_publish_threshold",
		CfgValue:      "90",
		ValueType:     ValueTypeInt,
		ExpectVersion: 7,
		Remark:        "提高自动发布阈值",
	})
	if err != nil {
		t.Fatalf("SaveOpsConfig: %v", err)
	}
	if got.Version != 8 {
		t.Fatalf("version = %d, want 8 (expect + 1)", got.Version)
	}
	if cfgMd.lastExpect != 7 {
		t.Fatalf("update issued with expect_version = %d, want 7", cfgMd.lastExpect)
	}
	if len(auditMd.rows) != 1 || !strings.Contains(auditMd.rows[0].ResourceID, "v=8") {
		t.Fatalf("audit must record the new version: %+v", auditMd.rows)
	}
}

func TestSaveOpsConfigMissingRowAndOperatorGuards(t *testing.T) {
	ctx := context.Background()
	r, _, _ := newConfigRepository(nil) // 库里没有这一行
	_, err := r.SaveOpsConfig(ctx, Actor{AdminID: 1}, SaveConfigInput{
		CfgKey: "nope", CfgValue: "1", ExpectVersion: 3,
	})
	if !errors.Is(err, model.ErrConfigNotFound) {
		t.Fatalf("err = %v, want ErrConfigNotFound", err)
	}

	if _, err := r.SaveOpsConfig(ctx, Actor{}, SaveConfigInput{CfgKey: "a"}); !errors.Is(err, model.ErrInvalidOperator) {
		t.Fatalf("err = %v, want ErrInvalidOperator", err)
	}
	if _, err := r.SaveOpsConfig(ctx, Actor{AdminID: 1}, SaveConfigInput{CfgValue: "1"}); !errors.Is(err, model.ErrConfigKeyEmpty) {
		t.Fatalf("err = %v, want ErrConfigKeyEmpty", err)
	}
	if _, err := r.SaveOpsConfig(ctx, Actor{AdminID: 1}, SaveConfigInput{
		CfgKey: "a", CfgValue: "1", ExpectVersion: -1,
	}); err == nil || !strings.Contains(err.Error(), "expect_version") {
		t.Fatalf("negative expect_version must be refused, got %v", err)
	}
}

func TestValidateConfigValue(t *testing.T) {
	cases := []struct {
		valueType string
		value     string
		wantErr   bool
	}{
		{ValueTypeString, "任意文本", false},
		{ValueTypeInt, " 42 ", false},
		{ValueTypeInt, "4.2", true},
		{ValueTypeInt, "", true},
		{ValueTypeBool, "true", false},
		{ValueTypeBool, "yes", true},
		{ValueTypeJSON, `{"a":1}`, false},
		{ValueTypeJSON, `{"a":1`, true},
		{ValueTypeJSON, `[1,2]`, false},
		{"unknown_type", "1", true},
		{ValueTypeString, strings.Repeat("x", maxConfigValueLen+1), true},
	}
	for _, c := range cases {
		err := validateConfigValue(c.valueType, c.value)
		if c.wantErr && err == nil {
			t.Fatalf("validateConfigValue(%q, %q) must fail", c.valueType, c.value)
		}
		if !c.wantErr && err != nil {
			t.Fatalf("validateConfigValue(%q, %q) = %v, want nil", c.valueType, c.value, err)
		}
		if err != nil && !errors.Is(err, model.ErrConfigValueInvalid) {
			t.Fatalf("error must wrap ErrConfigValueInvalid, got %v", err)
		}
	}
}

func TestNormalizeConfigScopeAndValueType(t *testing.T) {
	if got, err := normalizeConfigScope(""); err != nil || got != defaultScope {
		t.Fatalf("empty scope = %q/%v, want %q", got, err, defaultScope)
	}
	if got, err := normalizeConfigScope(" Android "); err != nil || got != "android" {
		t.Fatalf("scope = %q/%v, want android", got, err)
	}
	if _, err := normalizeConfigScope("bad scope!"); err == nil {
		t.Fatal("invalid scope chars must be refused")
	}
	if _, err := normalizeConfigScope(strings.Repeat("a", 33)); err == nil {
		t.Fatal("over-long scope must be refused")
	}
	if got, err := normalizeValueType(""); err != nil || got != ValueTypeString {
		t.Fatalf("empty value_type = %q/%v, want string", got, err)
	}
	if _, err := normalizeValueType("float"); err == nil {
		t.Fatal("unsupported value_type must be refused")
	}
}

func TestGetOpsConfigNotFoundAndCacheGuard(t *testing.T) {
	ctx := context.Background()
	r, _, _ := newConfigRepository(nil)
	if _, _, err := r.GetOpsConfig(ctx, "  ", "global", false); !errors.Is(err, model.ErrConfigKeyEmpty) {
		t.Fatalf("err = %v, want ErrConfigKeyEmpty", err)
	}
	if _, _, err := r.GetOpsConfig(ctx, "missing.key", "global", true); !errors.Is(err, model.ErrConfigNotFound) {
		t.Fatalf("err = %v, want ErrConfigNotFound", err)
	}
	row := storedConfig(3)
	r.configMd = &fakeOpsConfigModel{current: row}
	got, hit, err := r.GetOpsConfig(ctx, "moderation.auto_publish_threshold", "global", false)
	if err != nil || hit || got.Version != 3 {
		t.Fatalf("get = %+v hit=%v err=%v", got, hit, err)
	}
}
