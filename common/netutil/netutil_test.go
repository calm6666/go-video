package netutil

import (
	"net"
	"testing"
)

func TestInetAtoN(t *testing.T) {
	cases := []struct {
		in   string
		want uint32
	}{
		{"0.0.0.0", 0},
		{"127.0.0.1", 0x7F000001},
		{"1.2.3.4", 0x01020304},
		{"255.255.255.255", 0xFFFFFFFF},
		{"10.0.0.1", 0x0A000001},
		{"192.168.1.1", 0xC0A80101},
		{"172.31.0.1", 0xAC1F0001},
		{"", 0},
		{"invalid", 0},
		{"::1", 0}, // 仅支持 IPv4，IPv6 返回 0
		{"256.1.1.1", 0},
		{"1.2.3", 0},
	}
	for _, c := range cases {
		if got := InetAtoN(c.in); got != c.want {
			t.Errorf("InetAtoN(%q) = %#x, want %#x", c.in, got, c.want)
		}
	}
}

func TestInetNtoA(t *testing.T) {
	cases := []struct {
		n    uint32
		want string
	}{
		{0, "0.0.0.0"},
		{0x7F000001, "127.0.0.1"},
		{0x01020304, "1.2.3.4"},
		{0xFFFFFFFF, "255.255.255.255"},
		{0x0A000001, "10.0.0.1"},
		{0xC0A80101, "192.168.1.1"},
		{0xAC1F0001, "172.31.0.1"},
		{1, "0.0.0.1"},
	}
	for _, c := range cases {
		if got := InetNtoA(c.n); got != c.want {
			t.Errorf("InetNtoA(%#x) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestInetRoundTrip(t *testing.T) {
	// IPv4 字符串 -> uint32 -> IPv4 字符串
	ips := []string{
		"0.0.0.0", "127.0.0.1", "1.2.3.4", "255.255.255.255",
		"10.0.0.1", "192.168.1.1", "172.16.0.1", "172.31.255.254",
		"8.8.8.8", "0.0.0.1",
	}
	for _, s := range ips {
		n := InetAtoN(s)
		if got := InetNtoA(n); got != s {
			t.Errorf("字符串往返失败：%s -> %#x -> %s", s, n, got)
		}
	}
	// uint32 -> IPv4 字符串 -> uint32
	nums := []uint32{0, 1, 0x7F000001, 0xFFFFFFFF, 0x0A000001, 0xC0A80101, 0xAC100001}
	for _, n := range nums {
		s := InetNtoA(n)
		if got := InetAtoN(s); got != n {
			t.Errorf("uint32 往返失败：%#x -> %s -> %#x", n, s, got)
		}
	}
}

func TestInternalIP(t *testing.T) {
	ip := InternalIP()
	if ip == "" {
		t.Skip("无可用非 loopback IPv4 网卡，跳过")
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		t.Fatalf("InternalIP() 返回非合法 IP：%q", ip)
	}
	if parsed.To4() == nil {
		t.Fatalf("InternalIP() 返回非 IPv4：%q", ip)
	}
	if parsed.IsLoopback() {
		t.Fatalf("InternalIP() 不应返回 loopback：%q", ip)
	}
}

func TestExternalIP(t *testing.T) {
	ips := ExternalIP()
	for _, ip := range ips {
		parsed := net.ParseIP(ip)
		if parsed == nil {
			t.Errorf("ExternalIP() 返回非合法 IP：%q", ip)
			continue
		}
		if parsed.To4() == nil {
			t.Errorf("ExternalIP() 返回非 IPv4：%q", ip)
			continue
		}
		if parsed.IsLoopback() {
			t.Errorf("ExternalIP() 不应返回 loopback：%q", ip)
		}
		if parsed.IsLinkLocalUnicast() || parsed.IsLinkLocalMulticast() {
			t.Errorf("ExternalIP() 不应返回链路本地地址：%q", ip)
		}
		ip4 := parsed.To4()
		switch {
		case ip4[0] == 10:
			t.Errorf("ExternalIP() 不应返回 10/8 私网：%q", ip)
		case ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31:
			t.Errorf("ExternalIP() 不应返回 172.16/12 私网：%q", ip)
		case ip4[0] == 192 && ip4[1] == 168:
			t.Errorf("ExternalIP() 不应返回 192.168/16 私网：%q", ip)
		}
	}
}
