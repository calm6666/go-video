package dsn_test

import (
	"log"

	"go-video/common/dsn"
	"go-video/common/timeutil"
)

// Config 演示 DSN 绑定的典型用法：内置键 + query 参数 + validator 校验。
type Config struct {
	Network string   `dsn:"network" validate:"required"`
	Address []string `dsn:"address" validate:"required"`
	// Host 字段仅用于演示 validator required 校验，不会被 DSN 绑定。
	Host     string            `dsn:"-"`
	Username string            `dsn:"username" validate:"required"`
	Password string            `dsn:"password" validate:"required"`
	Timeout  timeutil.Duration `dsn:"query.timeout,1s"`
	Offset   int               `dsn:"query.offset" validate:"gte=0"`
}

func ExampleParse() {
	cfg := &Config{}
	d, err := dsn.Parse("tcp://root:toor@172.12.12.23:2233?timeout=10s")
	if err != nil {
		log.Fatal(err)
	}
	if _, err := d.Bind(cfg); err != nil {
		log.Fatal(err)
	}
	log.Printf("%+v", cfg)
}
