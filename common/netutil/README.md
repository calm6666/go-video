# common/netutil

IP 地址工具函数，提供本机网卡 IPv4 识别与 IPv4 字符串/uint32 互转。仅依赖标准库，不读取任何外部 IP 段数据文件，也不做地理位置解析。

## 职责

- 获取本机外部 IPv4（排除 loopback、链路本地与私网地址段）。
- 获取本机内部 IPv4（首个非 loopback 的 IPv4）。
- IPv4 字符串与 uint32 的大端序互转。

## 依赖

- 标准库 `net`、`strings`。

## API

| 函数 | 说明 |
|---|---|
| `ExternalIP() []string` | 返回本机外部 IPv4 列表，排除 loopback、链路本地、`10/8`、`172.16/12`、`192.168/16`；无可用地址返回 nil |
| `InternalIP() string` | 返回首个非 loopback 的本机 IPv4；无可用地址返回空串 |
| `InetAtoN(s string) uint32` | IPv4 字符串转 uint32（大端序，高位在前）；非法或非 IPv4 返回 0 |
| `InetNtoA(sum uint32) string` | uint32 转 IPv4 字符串（大端序） |

## 使用示例

```go
import "go-video/common/netutil"

n := netutil.InetAtoN("127.0.0.1")   // 0x7F000001
s := netutil.InetNtoA(0x7F000001)    // "127.0.0.1"
ip := netutil.InternalIP()           // 首个非 loopback IPv4
exts := netutil.ExternalIP()         // 外部 IPv4 列表
```

## 实现约定

- 仅使用标准库 `net` 与 `strings`，不依赖任何外部 IP 段数据文件。
- `ExternalIP`/`InternalIP` 在无网卡或无可用 IPv4 的环境下可能返回空，调用方需容忍。
- `InetAtoN` 仅支持 IPv4；IPv6 或非法输入返回 0，不报错。
- 排除私网地址段依据 RFC 1918：`10.0.0.0/8`、`172.16.0.0/12`、`192.168.0.0/16`。
