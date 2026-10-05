package registry

import (
	"context"
	"errors"
	"testing"

	"go-video/services/cron/model"
)

func okHandler(_ context.Context, in *Input) (*Output, error) {
	return &Output{ResultSummary: "attempt=" + string(rune('0'+in.Attempt))}, nil
}

func TestRegisterRejectsBadSpecs(t *testing.T) {
	r := New()
	cases := []struct {
		name string
		spec Spec
		want error
	}{
		{"empty name", Spec{handler: okHandler}, ErrHandlerEmpty},
		{"nil handler", Spec{Name: "demo noop"}, ErrHandlerNil},
		{"negative bound", Spec{Name: "demo.noop", handler: okHandler, SuggestedTimeout: -1}, ErrSpecConflict},
	}
	for _, tc := range cases {
		if err := r.Register(tc.spec); !errors.Is(err, tc.want) {
			t.Errorf("%s: Register() = %v, want %v", tc.name, err, tc.want)
		}
	}
	if r.Len() != 0 {
		t.Fatalf("非法注册不应进入注册表，当前 len=%d", r.Len())
	}
}

func TestRegisterRejectsNameWithSlashOrSpace(t *testing.T) {
	r := New()
	if err := r.Register(Spec{Name: "demo/noop", handler: okHandler}); err == nil {
		t.Error("handler 名带斜杠应被拒绝：lease_key/scope 用 / 作分隔，混进来会让审计对不上")
	}
}

func TestDuplicateRegisterDoesNotOverwrite(t *testing.T) {
	r := New()
	if err := r.Register(Spec{Name: "demo.noop", handler: okHandler, Description: "first"}); err != nil {
		t.Fatalf("首次注册失败: %v", err)
	}
	second := Spec{Name: "demo.noop", handler: func(context.Context, *Input) (*Output, error) {
		return &Output{}, nil
	}, Description: "second"}
	if err := r.Register(second); !errors.Is(err, ErrHandlerDuplicate) {
		t.Fatalf("重复注册必须显式失败，得到 %v", err)
	}
	spec, ok := r.Get("demo.noop")
	if !ok || spec.Description != "first" {
		t.Fatalf("重复注册不得覆盖既有 Spec，当前 %+v ok=%v", spec, ok)
	}
}

func TestResolveRequiresInjectedHandler(t *testing.T) {
	r := New()
	// 只有 Spec、没有 handler 的注册路径不存在（Register 会拒绝），
	// 因此这里验证「未注册」与「SetHandler 之后可解析」两种状态。
	if _, err := r.Resolve("demo.noop"); !errors.Is(err, model.ErrHandlerNotRegistered) {
		t.Fatalf("未注册 handler 必须映射到 ErrHandlerNotRegistered，得到 %v", err)
	}
	if err := r.Register(Spec{Name: "demo.noop", handler: okHandler}); err != nil {
		t.Fatal(err)
	}
	if !r.Has("demo.noop") {
		t.Fatal("注册后 Has 应为 true")
	}
	// SetHandler 只允许覆盖已有条目，不能凭空插入。
	if err := r.SetHandler("ghost.noop", okHandler); !errors.Is(err, ErrHandlerNotFound) {
		t.Errorf("SetHandler 对未知 handler 应报 ErrHandlerNotFound，得到 %v", err)
	}
}

func TestRunRejectsNilOutput(t *testing.T) {
	r := New()
	if err := r.Register(Spec{Name: "demo.nil", handler: func(context.Context, *Input) (*Output, error) {
		return nil, nil
	}}); err != nil {
		t.Fatal(err)
	}
	// 处理器既没报错也没给结果：按未实现处理，绝不计为成功（AGENTS.md §9）。
	if _, err := r.Run(context.Background(), "demo.nil", &Input{Attempt: 1}); !errors.Is(err, model.ErrNotImplemented) {
		t.Fatalf("空输出应判为未实现，得到 %v", err)
	}
}

func TestRunPropagatesHandlerError(t *testing.T) {
	r := New()
	want := errors.New("downstream exploded")
	if err := r.Register(Spec{Name: "demo.bad", handler: func(context.Context, *Input) (*Output, error) {
		return nil, want
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), "demo.bad", &Input{Attempt: 1}); !errors.Is(err, want) {
		t.Fatalf("Run() = %v, want wrapped %v", err, want)
	}
}

func TestValidateDefinitionEnforcesCodeBoundaries(t *testing.T) {
	r := New()
	if err := r.Register(Spec{
		Name:                 "index.alias_patrol",
		handler:              okHandler,
		SerialOnly:           true,
		SuggestedMaxAttempts: 3,
		MinLeaseTTLSeconds:   120,
	}); err != nil {
		t.Fatal(err)
	}

	base := func() *model.TaskDefinition {
		return &model.TaskDefinition{
			TaskKey: "index.alias_patrol", Handler: "index.alias_patrol",
			ConcurrencyLimit: 1, MaxAttempts: 2, LeaseTTLSeconds: 300,
		}
	}
	if err := r.ValidateDefinition(base()); err != nil {
		t.Fatalf("合法定义被拒绝: %v", err)
	}

	parallel := base()
	parallel.ConcurrencyLimit = 2
	if err := r.ValidateDefinition(parallel); !errors.Is(err, ErrSpecConflict) {
		t.Errorf("SerialOnly 任务并发数 >1 必须被拒绝，得到 %v", err)
	}

	tooManyAttempts := base()
	tooManyAttempts.MaxAttempts = 4
	if err := r.ValidateDefinition(tooManyAttempts); !errors.Is(err, ErrSpecConflict) {
		t.Errorf("max_attempts 超过 Spec 上限必须被拒绝，得到 %v", err)
	}

	shortLease := base()
	shortLease.LeaseTTLSeconds = 60
	if err := r.ValidateDefinition(shortLease); !errors.Is(err, ErrSpecConflict) {
		t.Errorf("lease_ttl 低于 Spec 下限必须被拒绝，得到 %v", err)
	}

	unknown := base()
	unknown.Handler = "not.registered"
	if err := r.ValidateDefinition(unknown); !errors.Is(err, model.ErrHandlerNotRegistered) {
		t.Errorf("未注册 handler 必须映射到 ErrHandlerNotRegistered，得到 %v", err)
	}
}

func TestEffectiveTimeoutFallsBackToSpec(t *testing.T) {
	r := New()
	if err := r.Register(Spec{Name: "demo.noop", handler: okHandler, SuggestedTimeout: 45}); err != nil {
		t.Fatal(err)
	}
	got, err := r.EffectiveTimeout(&model.TaskDefinition{Handler: "demo.noop"})
	if err != nil || got != 45 {
		t.Fatalf("DB 未配置超时时应回落到 Spec 建议值，得到 (%d,%v)", got, err)
	}
	got, err = r.EffectiveTimeout(&model.TaskDefinition{Handler: "demo.noop", TimeoutSeconds: 10})
	if err != nil || got != 10 {
		t.Fatalf("DB 显式配置优先，得到 (%d,%v)", got, err)
	}
	if _, err := r.EffectiveTimeout(&model.TaskDefinition{Handler: "ghost"}); !errors.Is(err, model.ErrHandlerNotRegistered) {
		t.Errorf("未知 handler 应报 ErrHandlerNotRegistered，得到 %v", err)
	}
}

func TestNamesAreSortedAndUnique(t *testing.T) {
	r := New()
	for _, name := range []string{"b.task", "a.task", "c.task"} {
		if err := r.Register(Spec{Name: name, handler: okHandler}); err != nil {
			t.Fatal(err)
		}
	}
	got := r.Names()
	want := []string{"a.task", "b.task", "c.task"}
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names() 必须升序（健康度输出要可 diff）：%v", got)
		}
	}
}
