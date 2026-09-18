// Package netutil 提供 IP 地址工具函数，包含本机网卡 IPv4 识别与 IPv4 字符串/uint32 互转。
//
// 仅依赖标准库 net 与 strings，不读取任何外部 IP 段数据文件，也不做地理位置解析。
// ExternalIP/InternalIP 在无可用网卡的环境下可能返回空，调用方需自行容忍。
package netutil

import (
	"net"
	"strings"
)

// ExternalIP 返回本机外部 IPv4 地址列表。
// 排除以下地址：
//   - loopback
//   - 链路本地单播/组播（link-local）
//   - RFC 1918 私网地址段：10.0.0.0/8、172.16.0.0/12（172.16~172.31）、192.168.0.0/16
//
// 同时跳过以 "lo" 开头的网卡名。无可用地址时返回 nil。
func ExternalIP() (res []string) {
	inters, err := net.Interfaces()
	if err != nil {
		return
	}
	for _, inter := range inters {
		if !strings.HasPrefix(inter.Name, "lo") {
			addrs, err := inter.Addrs()
			if err != nil {
				continue
			}
			for _, addr := range addrs {
				if ipnet, ok := addr.(*net.IPNet); ok {
					if ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalMulticast() || ipnet.IP.IsLinkLocalUnicast() {
						continue
					}
					if ip4 := ipnet.IP.To4(); ip4 != nil {
						switch true {
						case ip4[0] == 10:
							continue
						case ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31:
							continue
						case ip4[0] == 192 && ip4[1] == 168:
							continue
						default:
							res = append(res, ipnet.IP.String())
						}
					}
				}
			}
		}
	}
	return
}

// InternalIP 返回本机内部 IPv4 地址（首个非 loopback 的 IPv4）。
// 跳过以 "lo" 开头的网卡名。无可用地址时返回空字符串。
func InternalIP() string {
	inters, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, inter := range inters {
		if !strings.HasPrefix(inter.Name, "lo") {
			addrs, err := inter.Addrs()
			if err != nil {
				continue
			}
			for _, addr := range addrs {
				if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
					if ipnet.IP.To4() != nil {
						return ipnet.IP.String()
					}
				}
			}
		}
	}
	return ""
}

// InetAtoN 将 IPv4 字符串转换为 uint32（大端序，高位在前）。
// 输入非法或非 IPv4 时返回 0。例如 "127.0.0.1" -> 0x7F000001。
func InetAtoN(s string) (sum uint32) {
	ip := net.ParseIP(s)
	if ip == nil {
		return
	}
	ip = ip.To4()
	if ip == nil {
		return
	}
	sum += uint32(ip[0]) << 24
	sum += uint32(ip[1]) << 16
	sum += uint32(ip[2]) << 8
	sum += uint32(ip[3])
	return sum
}

// InetNtoA 将 uint32 转换为 IPv4 字符串（大端序）。
// 例如 0x7F000001 -> "127.0.0.1"。
func InetNtoA(sum uint32) string {
	ip := make(net.IP, net.IPv4len)
	ip[0] = byte((sum >> 24) & 0xFF)
	ip[1] = byte((sum >> 16) & 0xFF)
	ip[2] = byte((sum >> 8) & 0xFF)
	ip[3] = byte(sum & 0xFF)
	return ip.String()
}
