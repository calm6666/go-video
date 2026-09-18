package dsn

import (
	"net/url"
	"reflect"
	"testing"
	"time"

	"go-video/common/timeutil"
)

// config 综合了内置键与 query 绑定的多种字段类型。
type config struct {
	Network   string            `dsn:"network"`
	Addresses []string          `dsn:"address"`
	Username  string            `dsn:"username"`
	Password  string            `dsn:"password"`
	Timeout   timeutil.Duration `dsn:"query.timeout"`
	Sub       Sub               `dsn:"query.sub"`
	Def       string            `dsn:"query.def,hello"`
}

// Sub 用于验证嵌套 struct 绑定。
type Sub struct {
	Foo int `dsn:"query.foo"`
}

// TestBind 覆盖 Parse + Bind 的核心路径：内置键、query 参数、嵌套 struct、默认值、未消费参数透出。
func TestBind(t *testing.T) {
	var cfg config
	rawdsn := "tcp://root:toor@172.12.23.34,178.23.34.45?timeout=1s&sub.foo=1&hello=world"
	d, err := Parse(rawdsn)
	if err != nil {
		t.Fatal(err)
	}
	values, err := d.Bind(&cfg)
	if err != nil {
		t.Error(err)
	}
	if !reflect.DeepEqual(values, url.Values{"hello": {"world"}}) {
		t.Errorf("unexpect values get %v", values)
	}
	cfg2 := config{
		Network:   "tcp",
		Addresses: []string{"172.12.23.34", "178.23.34.45"},
		Password:  "toor",
		Username:  "root",
		Sub:       Sub{Foo: 1},
		Timeout:   timeutil.Duration(time.Second),
		Def:       "hello",
	}
	if !reflect.DeepEqual(cfg, cfg2) {
		t.Errorf("unexpect config get %+v, expect %+v", cfg, cfg2)
	}
}

// config2 用于验证 unix scheme 与 string 地址绑定。
type config2 struct {
	Network string            `dsn:"network"`
	Address string            `dsn:"address"`
	Timeout timeutil.Duration `dsn:"query.timeout"`
}

// TestBindUnix 覆盖 unix 系协议：Addresses 返回 [Path]，address 绑定到 string。
func TestBindUnix(t *testing.T) {
	var cfg config2
	rawdsn := "unix:///run/xxx.sock?timeout=1s&sub.foo=1&hello=world"
	d, err := Parse(rawdsn)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Bind(&cfg)
	if err != nil {
		t.Error(err)
	}
	cfg2 := config2{
		Network: "unix",
		Address: "/run/xxx.sock",
		Timeout: timeutil.Duration(time.Second),
	}
	if !reflect.DeepEqual(cfg, cfg2) {
		t.Errorf("unexpect config2 get %+v, expect %+v", cfg, cfg2)
	}
}

// TestAddresses 验证 Addresses() 对多种 scheme 的处理。
func TestAddresses(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{"tcp://127.0.0.1:3306,127.0.0.1:3307", []string{"127.0.0.1:3306", "127.0.0.1:3307"}},
		{"unix:///run/sock.sock", []string{"/run/sock.sock"}},
		{"unixgram:///run/gram.sock", []string{"/run/gram.sock"}},
		{"tcp://single:8080", []string{"single:8080"}},
	}
	for _, c := range cases {
		d, err := Parse(c.raw)
		if err != nil {
			t.Fatalf("parse %s: %v", c.raw, err)
		}
		if got := d.Addresses(); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Addresses(%s) = %v, want %v", c.raw, got, c.want)
		}
	}
}

// validConfig 用于验证 validator 校验失败的路径。
type validConfig struct {
	Network string `dsn:"network" validate:"required"`
	Offset  int    `dsn:"query.offset" validate:"gte=0"`
}

// TestBindValidationError 覆盖 validator 校验失败：offset 为负数违反 gte=0。
func TestBindValidationError(t *testing.T) {
	var cfg validConfig
	rawdsn := "tcp://root:toor@127.0.0.1:3306?offset=-1"
	d, err := Parse(rawdsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Bind(&cfg); err == nil {
		t.Fatal("expect validation error, got nil")
	}
}

// TestBindMissingRequired 验证 required 校验：未提供 network 应失败。
func TestBindMissingRequired(t *testing.T) {
	var cfg validConfig
	// scheme 缺失会得到空 network
	rawdsn := "//root:toor@127.0.0.1:3306?offset=1"
	d, err := Parse(rawdsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Bind(&cfg); err == nil {
		t.Fatal("expect required validation error, got nil")
	}
}
