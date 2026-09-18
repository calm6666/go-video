// Package dsn 提供数据源名称（DSN）的解析与 struct 绑定能力。
//
// DSN 采用类似 URI 的格式，便于把网络协议、地址、用户信息和 query 参数
// 一次性解析为可校验的配置结构。绑定阶段支持内置键（network/username/
// password/address）和 query.{name} 两种来源，并执行 validator 字段校验。
package dsn

import (
	"net/url"
	"reflect"
	"strings"

	"github.com/go-playground/validator/v10"
)

// _validator 是包级共享的校验器，在 init 阶段构造。
// 调用方在 struct 字段上使用 validate tag 即可启用规则校验。
var _validator *validator.Validate

func init() {
	_validator = validator.New()
}

// DSN 是解析后的数据源名称，底层复用 url.URL。
type DSN struct {
	*url.URL
}

// Bind 把 DSN 上的内置字段与 query 参数绑定到 v 指向的 struct，
// 并使用 validator 校验 v。返回的 url.Values 包含 query 中未被消费的参数。
//
// 字段绑定通过 struct tag `dsn:"<key>[,<default>]"` 控制：
//
//   - 内置键：network、username、password、address。
//     address 可绑定到 string（取首个）或 []string（取全部）。
//   - query 参数：使用 `dsn:"query.<name>"` 绑定到字段。
//   - 跳过字段：`dsn:"-"`。
//
// 默认值通过 tag 第二段提供，例如 `dsn:"query.timeout,1s"`。
// 切片默认值用 `,` 分割，例如 `dsn:"query.tags,a,b,c"`。
// 字段若实现 encoding.TextUnmarshaler 可自定义解析逻辑。
func (d *DSN) Bind(v any) (url.Values, error) {
	assignFuncs := make(map[string]assignFunc)
	if d.User != nil {
		username := d.User.Username()
		password, ok := d.User.Password()
		if ok {
			assignFuncs["password"] = stringsAssignFunc(password)
		}
		assignFuncs["username"] = stringsAssignFunc(username)
	}
	assignFuncs["address"] = addressesAssignFunc(d.Addresses())
	assignFuncs["network"] = stringsAssignFunc(d.Scheme)
	query, err := bindQuery(d.Query(), v, assignFuncs)
	if err != nil {
		return nil, err
	}
	return query, _validator.Struct(v)
}

// addressesAssignFunc 构造把地址列表绑定到字段的赋值函数。
// string 类型取首个地址；[]string 类型保留全部地址。
func addressesAssignFunc(addresses []string) assignFunc {
	return func(v reflect.Value, to tagOpt) error {
		if v.Kind() == reflect.String {
			if addresses[0] == "" && to.Default != "" {
				v.SetString(to.Default)
			} else {
				v.SetString(addresses[0])
			}
			return nil
		}
		if !(v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.String) {
			return &BindTypeError{Value: strings.Join(addresses, ","), Type: v.Type()}
		}
		vals := reflect.MakeSlice(v.Type(), len(addresses), len(addresses))
		for i, address := range addresses {
			vals.Index(i).SetString(address)
		}
		if v.CanSet() {
			v.Set(vals)
		}
		return nil
	}
}

// Addresses 返回 DSN 的地址列表。
// network 为 unix 系协议时返回 [Path]；其余按 host 中的 ',' 切分。
func (d *DSN) Addresses() []string {
	switch d.Scheme {
	case "unix", "unixgram", "unixpacket":
		return []string{d.Path}
	}
	return strings.Split(d.Host, ",")
}

// Parse 把 rawdsn 解析为 *DSN。
func Parse(rawdsn string) (*DSN, error) {
	u, err := url.Parse(rawdsn)
	return &DSN{URL: u}, err
}
