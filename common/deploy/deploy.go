// Package deploy 提供应用部署环境标识。
//
// 包加载时在 init 阶段把 region/zone/deploy.env/appid/color/端口等信息注册到
// flag.CommandLine，并从环境变量填充 Hostname 与 IP。调用方在主入口显式
// flag.Parse() 之后即可读取包级变量。
package deploy

import (
	"flag"
	"os"
)

// 部署环境常量。
const (
	DeployEnvDev  = "dev"
	DeployEnvFat1 = "fat1"
	DeployEnvUat  = "uat"
	DeployEnvPre  = "pre"
	DeployEnvProd = "prod"
)

// 环境变量相关默认值。
const (
	_region    = "sh"
	_zone      = "sh001"
	_deployEnv = "dev"
)

// 端口默认值。
const (
	_httpPort  = "8000"
	_gorpcPort = "8099"
	_grpcPort  = "9000"
)

// 部署元信息，flag.Parse() 之后可读。
var (
	// Region 应用所在 region。
	Region string
	// Zone 应用所在 zone。
	Zone string
	// Hostname 主机名。
	Hostname string
	// DeployEnv 部署环境标识。
	DeployEnv string
	// IP Pod IP，启动时从 POD_IP 取。
	IP = os.Getenv("POD_IP")
	// AppID 应用全局唯一 ID，由服务注册体系分配。
	AppID string
	// Color 实验分组标识。
	Color string
)

// 端口变量，flag.Parse() 之后可读。
var (
	// HTTPPort HTTP 监听端口。
	HTTPPort string
	// GORPCPort go-zero 自定义 RPC 监听端口。
	GORPCPort string
	// GRPCPort gRPC 监听端口。
	GRPCPort string
)

func init() {
	var err error
	if Hostname, err = os.Hostname(); err != nil || Hostname == "" {
		Hostname = os.Getenv("HOSTNAME")
	}

	addFlag(flag.CommandLine)
}

// addFlag 把部署元信息与端口注册到指定 FlagSet。
// flag 默认值优先取环境变量，缺失时使用内置默认值。
func addFlag(fs *flag.FlagSet) {
	// 部署元信息
	fs.StringVar(&Region, "region", defaultString("REGION", _region), "avaliable region. or use REGION env variable, value: sh etc.")
	fs.StringVar(&Zone, "zone", defaultString("ZONE", _zone), "avaliable zone. or use ZONE env variable, value: sh001/sh002 etc.")
	fs.StringVar(&DeployEnv, "deploy.env", defaultString("DEPLOY_ENV", _deployEnv), "deploy env. or use DEPLOY_ENV env variable, value: dev/fat1/uat/pre/prod etc.")
	fs.StringVar(&AppID, "appid", os.Getenv("APP_ID"), "appid is global unique application id, register by service tree. or use APP_ID env variable.")
	fs.StringVar(&Color, "deploy.color", os.Getenv("DEPLOY_COLOR"), "deploy.color is the identification of different experimental group.")

	// 端口
	fs.StringVar(&HTTPPort, "http.port", defaultString("DISCOVERY_HTTP_PORT", _httpPort), "app listen http port, default: 8000")
	fs.StringVar(&GORPCPort, "gorpc.port", defaultString("DISCOVERY_GORPC_PORT", _gorpcPort), "app listen gorpc port, default: 8099")
	fs.StringVar(&GRPCPort, "grpc.port", defaultString("DISCOVERY_GRPC_PORT", _grpcPort), "app listen grpc port, default: 9000")
}

// defaultString 优先返回环境变量 env 的值，为空时返回 value 作为默认值。
func defaultString(env, value string) string {
	v := os.Getenv(env)
	if v == "" {
		return value
	}
	return v
}
