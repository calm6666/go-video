package dsn

import (
	"encoding"
	"net/url"
	"reflect"
	"runtime"
	"strconv"
	"strings"
)

const (
	_tagID       = "dsn"
	_queryPrefix = "query."
)

// InvalidBindError 表示传给 DecodeQuery 的参数无效（必须是非空指针）。
type InvalidBindError struct {
	Type reflect.Type
}

func (e *InvalidBindError) Error() string {
	if e.Type == nil {
		return "Bind(nil)"
	}

	if e.Type.Kind() != reflect.Ptr {
		return "Bind(non-pointer " + e.Type.String() + ")"
	}
	return "Bind(nil " + e.Type.String() + ")"
}

// BindTypeError 表示 query 值无法绑定到指定的 Go 类型字段。
type BindTypeError struct {
	Value string
	Type  reflect.Type
}

func (e *BindTypeError) Error() string {
	return "cannot decode " + e.Value + " into Go value of type " + e.Type.String()
}

// assignFunc 把已取到的原始值赋给 struct 字段。
type assignFunc func(v reflect.Value, to tagOpt) error

// stringsAssignFunc 构造把单个字符串赋给字段的赋值函数。
// 字段必须是可写的 string 类型，否则返回 BindTypeError。
func stringsAssignFunc(val string) assignFunc {
	return func(v reflect.Value, to tagOpt) error {
		if v.Kind() != reflect.String || !v.CanSet() {
			return &BindTypeError{Value: "string", Type: v.Type()}
		}
		if val == "" {
			v.SetString(to.Default)
		} else {
			v.SetString(val)
		}
		return nil
	}
}

// bindQuery 把 url.Values 解析到 v 指向的结构体。
// assignFuncs 用于注入内置键的赋值函数。返回未被消费的 query 参数。
func bindQuery(query url.Values, v any, assignFuncs map[string]assignFunc) (url.Values, error) {
	if assignFuncs == nil {
		assignFuncs = make(map[string]assignFunc)
	}
	d := decodeState{
		data:        query,
		used:        make(map[string]bool),
		assignFuncs: assignFuncs,
	}
	err := d.decode(v)
	ret := d.unused()
	return ret, err
}

// tagOpt 是解析后的 dsn tag 选项。
type tagOpt struct {
	Name    string
	Default string
}

// parseTag 解析 dsn tag，第二段及之后视为 default。
// default 中可包含逗号，以支持切片默认值如 "a,b,c"。
func parseTag(tag string) tagOpt {
	vs := strings.SplitN(tag, ",", 2)
	if len(vs) == 2 {
		return tagOpt{Name: vs[0], Default: vs[1]}
	}
	return tagOpt{Name: vs[0]}
}

type decodeState struct {
	data        url.Values
	used        map[string]bool
	assignFuncs map[string]assignFunc
}

// unused 返回 query 中未被使用的 key。
func (d *decodeState) unused() url.Values {
	ret := make(url.Values)
	for k, v := range d.data {
		if !d.used[k] {
			ret[k] = v
		}
	}
	return ret
}

// decode 入口，恢复 panic 为 error。
func (d *decodeState) decode(v any) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(runtime.Error); ok {
				panic(r)
			}
			err = r.(error)
		}
	}()
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return &InvalidBindError{reflect.TypeOf(v)}
	}
	return d.root(rv)
}

// root 处理顶层结构体，遍历字段并把内置键或 query.* 字段分发出去。
func (d *decodeState) root(v reflect.Value) error {
	var tu encoding.TextUnmarshaler
	tu, v = d.indirect(v)
	if tu != nil {
		return tu.UnmarshalText([]byte(d.data.Encode()))
	}
	// TODO 支持 map、slice 作为 root
	if v.Kind() != reflect.Struct {
		return &BindTypeError{Value: d.data.Encode(), Type: v.Type()}
	}
	tv := v.Type()
	for i := 0; i < tv.NumField(); i++ {
		fv := v.Field(i)
		field := tv.Field(i)
		to := parseTag(field.Tag.Get(_tagID))
		if to.Name == "-" {
			continue
		}
		if af, ok := d.assignFuncs[to.Name]; ok {
			if err := af(fv, tagOpt{}); err != nil {
				return err
			}
			continue
		}
		if !strings.HasPrefix(to.Name, _queryPrefix) {
			continue
		}
		to.Name = to.Name[len(_queryPrefix):]
		if err := d.value(fv, "", to); err != nil {
			return err
		}
	}
	return nil
}

// combinekey 拼接嵌套字段的完整 query key。
func combinekey(prefix string, to tagOpt) string {
	key := to.Name
	if prefix != "" {
		key = prefix + "." + key
	}
	return key
}

// value 按字段类型分发解析。
func (d *decodeState) value(v reflect.Value, prefix string, to tagOpt) (err error) {
	key := combinekey(prefix, to)
	d.used[key] = true
	var tu encoding.TextUnmarshaler
	tu, v = d.indirect(v)
	if tu != nil {
		if val, ok := d.data[key]; ok {
			return tu.UnmarshalText([]byte(val[0]))
		}
		if to.Default != "" {
			return tu.UnmarshalText([]byte(to.Default))
		}
		return
	}
	switch v.Kind() {
	case reflect.Bool:
		err = d.valueBool(v, prefix, to)
	case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Int:
		err = d.valueInt64(v, prefix, to)
	case reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uint:
		err = d.valueUint64(v, prefix, to)
	case reflect.Float32, reflect.Float64:
		err = d.valueFloat64(v, prefix, to)
	case reflect.String:
		err = d.valueString(v, prefix, to)
	case reflect.Slice:
		err = d.valueSlice(v, prefix, to)
	case reflect.Struct:
		err = d.valueStruct(v, prefix, to)
	case reflect.Ptr:
		if !d.hasKey(combinekey(prefix, to)) {
			break
		}
		if !v.CanSet() {
			break
		}
		nv := reflect.New(v.Type().Elem())
		v.Set(nv)
		err = d.value(nv, prefix, to)
	}
	return
}

// hasKey 判断 data 中是否存在以 key 为前缀或完整匹配的项。
func (d *decodeState) hasKey(key string) bool {
	for k := range d.data {
		if strings.HasPrefix(k, key+".") || k == key {
			return true
		}
	}
	return false
}

func (d *decodeState) valueBool(v reflect.Value, prefix string, to tagOpt) error {
	key := combinekey(prefix, to)
	val := d.data.Get(key)
	if val == "" {
		if to.Default == "" {
			return nil
		}
		val = to.Default
	}
	return d.setBool(v, val)
}

func (d *decodeState) setBool(v reflect.Value, val string) error {
	bval, err := strconv.ParseBool(val)
	if err != nil {
		return &BindTypeError{Value: val, Type: v.Type()}
	}
	v.SetBool(bval)
	return nil
}

func (d *decodeState) valueInt64(v reflect.Value, prefix string, to tagOpt) error {
	key := combinekey(prefix, to)
	val := d.data.Get(key)
	if val == "" {
		if to.Default == "" {
			return nil
		}
		val = to.Default
	}
	return d.setInt64(v, val)
}

func (d *decodeState) setInt64(v reflect.Value, val string) error {
	ival, err := strconv.ParseInt(val, 10, 64)
	if err != nil {
		return &BindTypeError{Value: val, Type: v.Type()}
	}
	v.SetInt(ival)
	return nil
}

func (d *decodeState) valueUint64(v reflect.Value, prefix string, to tagOpt) error {
	key := combinekey(prefix, to)
	val := d.data.Get(key)
	if val == "" {
		if to.Default == "" {
			return nil
		}
		val = to.Default
	}
	return d.setUint64(v, val)
}

func (d *decodeState) setUint64(v reflect.Value, val string) error {
	uival, err := strconv.ParseUint(val, 10, 64)
	if err != nil {
		return &BindTypeError{Value: val, Type: v.Type()}
	}
	v.SetUint(uival)
	return nil
}

func (d *decodeState) valueFloat64(v reflect.Value, prefix string, to tagOpt) error {
	key := combinekey(prefix, to)
	val := d.data.Get(key)
	if val == "" {
		if to.Default == "" {
			return nil
		}
		val = to.Default
	}
	return d.setFloat64(v, val)
}

func (d *decodeState) setFloat64(v reflect.Value, val string) error {
	fval, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return &BindTypeError{Value: val, Type: v.Type()}
	}
	v.SetFloat(fval)
	return nil
}

func (d *decodeState) valueString(v reflect.Value, prefix string, to tagOpt) error {
	key := combinekey(prefix, to)
	val := d.data.Get(key)
	if val == "" {
		if to.Default == "" {
			return nil
		}
		val = to.Default
	}
	return d.setString(v, val)
}

func (d *decodeState) setString(v reflect.Value, val string) error {
	v.SetString(val)
	return nil
}

// valueSlice 解析切片字段，依据元素类型选择 setFunc。
// 默认值通过 ',' 分割成多值。
func (d *decodeState) valueSlice(v reflect.Value, prefix string, to tagOpt) error {
	key := combinekey(prefix, to)
	strs, ok := d.data[key]
	if !ok {
		strs = strings.Split(to.Default, ",")
	}
	if len(strs) == 0 {
		return nil
	}
	et := v.Type().Elem()
	var setFunc func(reflect.Value, string) error
	switch et.Kind() {
	case reflect.Bool:
		setFunc = d.setBool
	case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Int:
		setFunc = d.setInt64
	case reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uint:
		setFunc = d.setUint64
	case reflect.Float32, reflect.Float64:
		setFunc = d.setFloat64
	case reflect.String:
		setFunc = d.setString
	default:
		return &BindTypeError{Type: et, Value: strs[0]}
	}
	vals := reflect.MakeSlice(v.Type(), len(strs), len(strs))
	for i, str := range strs {
		if err := setFunc(vals.Index(i), str); err != nil {
			return err
		}
	}
	if v.CanSet() {
		v.Set(vals)
	}
	return nil
}

// valueStruct 处理嵌套 struct，子字段以父 to.Name 为前缀查找。
func (d *decodeState) valueStruct(v reflect.Value, prefix string, to tagOpt) error {
	tv := v.Type()
	for i := 0; i < tv.NumField(); i++ {
		fv := v.Field(i)
		field := tv.Field(i)
		fto := parseTag(field.Tag.Get(_tagID))
		if fto.Name == "-" {
			continue
		}
		if af, ok := d.assignFuncs[fto.Name]; ok {
			if err := af(fv, tagOpt{}); err != nil {
				return err
			}
			continue
		}
		if !strings.HasPrefix(fto.Name, _queryPrefix) {
			continue
		}
		fto.Name = fto.Name[len(_queryPrefix):]
		if err := d.value(fv, to.Name, fto); err != nil {
			return err
		}
	}
	return nil
}

// indirect 解引用指针、找到具体承载值的 reflect.Value。
// 若过程中遇到实现 encoding.TextUnmarshaler 的类型则提前返回。
func (d *decodeState) indirect(v reflect.Value) (encoding.TextUnmarshaler, reflect.Value) {
	v0 := v
	haveAddr := false

	if v.Kind() != reflect.Ptr && v.Type().Name() != "" && v.CanAddr() {
		haveAddr = true
		v = v.Addr()
	}
	for {
		if v.Kind() == reflect.Interface && !v.IsNil() {
			e := v.Elem()
			if e.Kind() == reflect.Ptr && !e.IsNil() && e.Elem().Kind() == reflect.Ptr {
				haveAddr = false
				v = e
				continue
			}
		}

		if v.Kind() != reflect.Ptr {
			break
		}

		if v.Elem().Kind() != reflect.Ptr && v.CanSet() {
			break
		}
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		if v.Type().NumMethod() > 0 {
			if u, ok := v.Interface().(encoding.TextUnmarshaler); ok {
				return u, reflect.Value{}
			}
		}
		if haveAddr {
			v = v0
			haveAddr = false
		} else {
			v = v.Elem()
		}
	}
	return nil, v
}
