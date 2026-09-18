# common/deploy

部署环境标识，提供 region/zone/hostname/deploy_env/app_id/color 等运行时元信息。

## 职责

- 暴露部署期常量：`DeployEnvDev`/`DeployEnvFat1`/`DeployEnvUat`/`DeployEnvPre`/`DeployEnvProd`。
- 暴露全局运行时元信息：`Region`/`Zone`/`Hostname`/`DeployEnv`/`IP`/`AppID`/`Color`。
- 在 `init()` 阶段从环境变量与默认值生成 flag 注册，应用启动 `flag.Parse()` 后即可读取。
- 提供 `defaultString(env, value)` 工具，优先返回环境变量值，缺失时返回默认值。

## 启动顺序

1. `init()` 调用 `addFlag(flag.CommandLine)` 注册全局 flag，并尝试通过 `os.Hostname()` 解析 Hostname。
2. 应用 main 函数显式调用 `flag.Parse()`。
3. 此后所有包级变量（`Region`/`Zone`/`DeployEnv`/`AppID`/`Color`/`HTTPPort`/`GORPCPort`/`GRPCPort`）才可用。

## 环境变量与 flag 映射

| 全局变量 | flag | 环境变量 | 默认值 |
|---|---|---|---|
| `Region` | `region` | `REGION` | `sh` |
| `Zone` | `zone` | `ZONE` | `sh001` |
| `DeployEnv` | `deploy.env` | `DEPLOY_ENV` | `dev` |
| `AppID` | `appid` | `APP_ID` | 空 |
| `Color` | `deploy.color` | `DEPLOY_COLOR` | 空 |
| `HTTPPort` | `http.port` | `DISCOVERY_HTTP_PORT` | `8000` |
| `GORPCPort` | `gorpc.port` | `DISCOVERY_GORPC_PORT` | `8099` |
| `GRPCPort` | `grpc.port` | `DISCOVERY_GRPC_PORT` | `9000` |
| `IP` | （仅环境变量） | `POD_IP` | 空 |
| `Hostname` | （仅 init） | `HOSTNAME` | `os.Hostname()` |

flag 优先级高于环境变量；环境变量优先级高于默认值。

## 依赖

- 标准库：`flag`、`os`。

## API

| 符号 | 说明 |
|---|---|
| `const DeployEnvDev = "dev"` | 开发环境 |
| `const DeployEnvFat1 = "fat1"` | FAT1 环境 |
| `const DeployEnvUat = "uat"` | UAT 环境 |
| `const DeployEnvPre = "pre"` | 预发环境 |
| `const DeployEnvProd = "prod"` | 生产环境 |
| `var Region string` | region 标识 |
| `var Zone string` | zone 标识 |
| `var Hostname string` | 主机名 |
| `var DeployEnv string` | 部署环境标识 |
| `var IP string` | Pod IP（启动时从 `POD_IP` 取） |
| `var AppID string` | 应用全局唯一 ID |
| `var Color string` | 实验分组标识 |
| `var HTTPPort string` | HTTP 监听端口 |
| `var GORPCPort string` | Go RPC 监听端口 |
| `var GRPCPort string` | gRPC 监听端口 |
| `func defaultString(env, value string) string` | 优先返回环境变量值，缺失返回默认 |

## 使用示例

```go
package main

import (
    "flag"
    "fmt"

    _ "go-video/common/deploy"
)

func main() {
    flag.Parse()
    fmt.Println(deploy.AppID, deploy.DeployEnv, deploy.Region)
}
```

服务启动时显式 `flag.Parse()` 后，即可直接读取包级变量；不在主入口调用 `flag.Parse()` 时变量仍是默认值。

## 实现约定

- `init()` 只注册 flag 不解析；调用方负责 `flag.Parse()`。
- `IP` 在包加载时直接从 `POD_IP` 读取，避免 flag 与 env 顺序耦合。
- `Hostname` 先尝试 `os.Hostname()`，失败或为空时回退到 `HOSTNAME` 环境变量。
- 测试中通过新建 `flag.FlagSet` 调用 `addFlag`，避免污染 `flag.CommandLine`。
