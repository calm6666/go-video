# common/chinese

中文简繁转换工具，基于 OpenCC 字典方案与 cedar Trie 前缀匹配。

## 职责

- 提供 `NewConverter(nameOrPath)` 构造转换器，支持内置方案名（如 `s2t`/`t2s`/`s2twp`/`tw2sp`/`s2hk`/`hk2s`/`s2tw`/`t2tw`/`t2hk`/`tw2s`）或本地 JSON 配置文件路径。
- 提供 `Converter.Convert(s) string` 应用转换链返回结果。
- 提供包级便捷函数 `ToSimplified(s)` 与 `ToTraditional(s)`，使用懒加载的单例转换器。
- 字典数据通过可替换的 `Loader` 加载，默认先尝试本地文件系统，再回退到远程 URL。

## 转换方案

| 方案 | 含义 |
|---|---|
| `s2t` | 简体到繁体 |
| `t2s` | 繁体到简体 |
| `s2tw` | 简体到台湾繁体 |
| `tw2s` | 台湾繁体到简体 |
| `s2hk` | 简体到香港繁体 |
| `hk2s` | 香港繁体到简体 |
| `s2twp` | 简体到台湾繁体（含短语） |
| `tw2sp` | 台湾繁体到简体（含短语） |
| `t2tw` | 繁体到台湾繁体 |
| `t2hk` | 繁体到香港繁体 |

## 依赖

- [`github.com/go-ego/cedar`](https://github.com/go-ego/cedar)：高性能 Double-Array Trie，用于字典前缀匹配。
- [`github.com/zeromicro/go-zero/core/logx`](https://github.com/zeromicro/go-zero)：日志输出。
- 标准库：`bytes`、`encoding/json`、`fmt`、`io`、`net/http`、`os`、`path/filepath`、`strings`、`sync`、`time`。

## API

| 符号 | 说明 |
|---|---|
| `func NewConverter(nameOrPath string) (*Converter, error)` | 按方案名或本地配置路径构造 Converter |
| `func (c *Converter) Convert(in string) string` | 应用转换链 |
| `func (c *Converter) Description() string` | 返回方案描述 |
| `func ToSimplified(in string) string` | 简化繁为简，使用 `tw2sp` 方案 |
| `func ToTraditional(in string) string` | 简化为繁，使用 `s2twp` 方案 |
| `func SetDictLoader(fn func(file string) ([]byte, error))` | 替换全局字典加载器，测试用 |
| `func SetDictRoot(root string)` | 设置本地字典目录，默认 `opencc` |
| `type Converter struct` | 转换器 |
| `type Group struct` | 一组字典，按顺序应用前缀匹配 |

## 字典加载

默认 `Loader` 行为：

1. 若 `DictRoot` 下存在名为 `file` 的文件，读取并返回其内容。
2. 否则回退到远程 URL `http://i0.hdslb.com/bfs/static/<file>`，最多重试 3 次。

生产环境应通过 `SetDictRoot` 指向打包好的本地字典目录，避免运行时依赖外网。测试可通过 `SetDictLoader` 注入内存字典数据，避免网络与磁盘依赖。

## 使用示例

```go
package main

import (
    "fmt"

    "go-video/common/chinese"
)

func main() {
    // 部署时把 OpenCC 字典文件放到 ./opencc 目录下，或通过 SetDictRoot 指定路径。
    chinese.SetDictRoot("/etc/go-video/opencc")

    fmt.Println(chinese.ToTraditional("中国")) // 中國
    fmt.Println(chinese.ToSimplified("中國"))   // 中国
}
```

直接构造指定方案的 Converter：

```go
c, err := chinese.NewConverter("s2t")
if err != nil {
    log.Fatal(err)
}
fmt.Println(c.Convert("中国"))
```

## 实现约定

- `Convert` 是只读操作，并发安全；多个 goroutine 可共享同一 `*Converter`。
- 包级 `ToSimplified`/`ToTraditional` 使用 `sync.Once` 懒加载单例，构造失败时记录日志并回退为返回原字符串。
- `Loader` 是包级变量，调用方应在初始化阶段替换，不应在运行时并发修改。
- 字典文件格式：每行 `key<TAB>value1[<TAB>value2...]`，少于 2 列的行被忽略。
- 转换链对每个 Group 应用前缀匹配；同一 Group 内按字典顺序取首个匹配，未匹配的字符原样透传。
