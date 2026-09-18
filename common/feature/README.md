# common/feature

运行时特性开关（feature gate），支持按名称注册特性、通过 flag 或 map 启用/禁用、查询启用状态与默认值。适用于灰度发布、实验特性开关与配置校验场景。

## 职责

- 注册特性及其默认值（`Spec.Default`）。
- 通过命令行 flag（`feature-gates=...`）或 `SetFromMap` 启用/禁用特性。
- 查询特性是否启用（`Enabled`），未显式设置时回退到默认值。
- 输出已知特性列表与当前启用状态字符串，支持深拷贝用于配置校验。

## 依赖

- 标准库 `flag`、`fmt`、`os`、`sort`、`strconv`、`strings`、`sync`、`sync/atomic`。

## API

| 符号 | 说明 |
|---|---|
| `type Feature string` | 特性名称 |
| `type Spec struct { Default bool }` | 特性规格，仅含默认值 |
| `type Gate interface` | 特性开关集合接口 |
| `func NewGate() *featureGate` | 创建独立 Gate（实现 `Gate` 与 `flag.Value`） |
| `var DefaultGate Gate` | 包级共享 Gate |
| `func (Gate) Add(features map[Feature]Spec) error` | 注册特性；`AddFlag` 后不可再调用 |
| `func (Gate) Set(value string) error` | 解析 `k=v,k=v` 字符串设置启用状态 |
| `func (Gate) SetFromMap(m map[string]bool) error` | 从 map 设置启用状态 |
| `func (Gate) Enabled(key Feature) bool` | 查询是否启用，未设置回退默认值 |
| `func (Gate) AddFlag(fs *flag.FlagSet)` | 注册 `feature-gates` flag；调用后冻结 `Add` |
| `func (Gate) KnownFeatures() []string` | 已知特性描述列表（排序） |
| `func (Gate) DeepCopy() Gate` | 深拷贝，用于配置变更前校验 |

## flag 格式

`feature-gates` 接受逗号分隔的 `key=value` 列表，value 为布尔字符串（`true`/`false`/`1`/`0`/...，按 `strconv.ParseBool` 解析）。空段被忽略。未注册的 key 返回错误。

```
-feature-gates=FeatureA=true,FeatureB=false
```

## 使用示例

```go
import (
    "flag"
    "go-video/common/feature"
)

var (
    StableFeature  feature.Feature = "stable-feature"
    StagingFeature feature.Feature = "staging-feature"
)

func init() {
    feature.DefaultGate.Add(map[feature.Feature]feature.Spec{
        StableFeature:  {Default: true},
        StagingFeature: {Default: false},
    })
    feature.DefaultGate.AddFlag(flag.CommandLine)
}

// 查询
if feature.DefaultGate.Enabled(StagingFeature) {
    // ...
}
```

## 实现约定

- 并发安全：`known`/`enabled` 用 `atomic.Value` 存储，写操作在 `lock` 保护下复制后整体替换。
- `AddFlag` 后 `closed` 置位，禁止再 `Add`，避免 flag 解析后注册新特性导致行为不确定。
- `Enabled` 读取无锁，依赖 `atomic.Value` 的原子加载；未注册的 key 返回 false（`Spec` 零值的 `Default` 为 false），调用方应只查询已注册特性以保证语义明确。
- `Set`/`SetFromMap` 成功后向 stderr 输出当前启用状态，便于运维排查。
- 仅使用标准库，不引入任何外部日志或错误包装库。
