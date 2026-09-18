package deploy

import (
	"flag"
	"fmt"
	"os"
	"testing"
)

// TestDefaultString 覆盖 defaultString 工具：环境变量缺失返回默认；存在则返回环境变量值。
func TestDefaultString(t *testing.T) {
	// 清理可能存在的环境变量，保证测试隔离。
	key := "DEPLOY_TEST_DEFAULT_STRING"
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	v := defaultString(key, "test")
	if v != "test" {
		t.Fatalf("v must be test, got %s", v)
	}
	if err := os.Setenv(key, "test1"); err != nil {
		t.Fatal(err)
	}
	defer os.Unsetenv(key)
	v = defaultString(key, "test")
	if v != "test1" {
		t.Fatalf("v must be test1, got %s", v)
	}
}

// TestEnv 覆盖 addFlag 在新建 FlagSet 上的行为：flag 设置、env 设置、默认值三种情形。
func TestEnv(t *testing.T) {
	tests := []struct {
		flag string
		env  string
		def  string
		val  *string
	}{
		{"region", "REGION", _region, &Region},
		{"zone", "ZONE", _zone, &Zone},
		{"deploy.env", "DEPLOY_ENV", _deployEnv, &DeployEnv},
		{"appid", "APP_ID", "", &AppID},
		{"http.port", "DISCOVERY_HTTP_PORT", _httpPort, &HTTPPort},
		{"gorpc.port", "DISCOVERY_GORPC_PORT", _gorpcPort, &GORPCPort},
		{"grpc.port", "DISCOVERY_GRPC_PORT", _grpcPort, &GRPCPort},
		{"deploy.color", "DEPLOY_COLOR", "", &Color},
	}
	for _, test := range tests {
		// flag 显式设置优先级最高
		t.Run(fmt.Sprintf("%s: flag set", test.env), func(t *testing.T) {
			fs := flag.NewFlagSet("", flag.ContinueOnError)
			addFlag(fs)
			if err := fs.Parse([]string{fmt.Sprintf("-%s=%s", test.flag, "test")}); err != nil {
				t.Fatal(err)
			}
			if *test.val != "test" {
				t.Fatalf("val must be test, got %s", *test.val)
			}
		})
		// flag 未设置、env 设置时取 env 值
		t.Run(fmt.Sprintf("%s: flag not set, env set", test.env), func(t *testing.T) {
			*test.val = ""
			if err := os.Setenv(test.env, "test2"); err != nil {
				t.Fatal(err)
			}
			defer os.Unsetenv(test.env)
			fs := flag.NewFlagSet("", flag.ContinueOnError)
			addFlag(fs)
			if err := fs.Parse([]string{}); err != nil {
				t.Fatal(err)
			}
			if *test.val != "test2" {
				t.Fatalf("val must be test2, got %s", *test.val)
			}
		})
		// flag 与 env 均未设置时取默认值
		t.Run(fmt.Sprintf("%s: flag not set, env not set", test.env), func(t *testing.T) {
			*test.val = ""
			if err := os.Setenv(test.env, ""); err != nil {
				t.Fatal(err)
			}
			defer os.Unsetenv(test.env)
			fs := flag.NewFlagSet("", flag.ContinueOnError)
			addFlag(fs)
			if err := fs.Parse([]string{}); err != nil {
				t.Fatal(err)
			}
			if *test.val != test.def {
				t.Fatalf("val must be %q, got %s", test.def, *test.val)
			}
		})
	}
}

// TestDeployEnvConstants 验证部署环境常量值，避免被误改。
func TestDeployEnvConstants(t *testing.T) {
	cases := map[string]string{
		"DeployEnvDev":  DeployEnvDev,
		"DeployEnvFat1": DeployEnvFat1,
		"DeployEnvUat":  DeployEnvUat,
		"DeployEnvPre":  DeployEnvPre,
		"DeployEnvProd": DeployEnvProd,
	}
	want := map[string]string{
		"DeployEnvDev":  "dev",
		"DeployEnvFat1": "fat1",
		"DeployEnvUat":  "uat",
		"DeployEnvPre":  "pre",
		"DeployEnvProd": "prod",
	}
	for k, v := range cases {
		if v != want[k] {
			t.Errorf("%s = %q, want %q", k, v, want[k])
		}
	}
}
