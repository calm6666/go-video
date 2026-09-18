package dsn

import (
	"net/url"
	"reflect"
	"testing"
	"time"

	"go-video/common/timeutil"
)

// qcfg1 覆盖 string/[]int/float64 默认值与 query 多值。
type qcfg1 struct {
	Name     string  `dsn:"query.name"`
	Def      string  `dsn:"query.def,hello"`
	DefSlice []int   `dsn:"query.defslice,1,2,3,4"`
	Ignore   string  `dsn:"-"`
	FloatNum float64 `dsn:"query.floatNum"`
}

// qcfg2/qcfg4 覆盖 TextUnmarshaler 字段（timeutil.Duration）的 query 绑定与默认值。
type qcfg2 struct {
	Timeout timeutil.Duration `dsn:"query.timeout"`
}

type qcfg3 struct {
	Username string            `dsn:"username"`
	Timeout  timeutil.Duration `dsn:"query.timeout"`
}

type qcfg4 struct {
	Timeout timeutil.Duration `dsn:"query.timeout,1s"`
}

// TestDecodeQuery 覆盖 bindQuery 的核心场景：通用绑定、TextUnmarshaler、空值、默认值、内置键注入。
func TestDecodeQuery(t *testing.T) {
	type args struct {
		query       url.Values
		v           any
		assignFuncs map[string]assignFunc
	}
	tests := []struct {
		name    string
		args    args
		want    url.Values
		cfg     any
		wantErr bool
	}{
		{
			name: "test generic",
			args: args{
				query: url.Values{
					"name":     {"hello"},
					"Ignore":   {"test"},
					"floatNum": {"22.33"},
					"adb":      {"123"},
				},
				v: &qcfg1{},
			},
			want: url.Values{
				"Ignore": {"test"},
				"adb":    {"123"},
			},
			cfg: &qcfg1{
				Name:     "hello",
				Def:      "hello",
				DefSlice: []int{1, 2, 3, 4},
				FloatNum: 22.33,
			},
		},
		{
			name: "test textunmarshaler duration",
			args: args{
				query: url.Values{
					"timeout": {"1s"},
				},
				v: &qcfg2{},
			},
			want: url.Values{},
			cfg:  &qcfg2{timeutil.Duration(time.Second)},
		},
		{
			name: "test empty textunmarshaler duration",
			args: args{
				query: url.Values{},
				v:     &qcfg2{},
			},
			want: url.Values{},
			cfg:  &qcfg2{},
		},
		{
			name: "test textunmarshaler duration default",
			args: args{
				query: url.Values{},
				v:     &qcfg4{},
			},
			want: url.Values{},
			cfg:  &qcfg4{timeutil.Duration(time.Second)},
		},
		{
			name: "test build-in value",
			args: args{
				query: url.Values{
					"timeout": {"1s"},
				},
				v:           &qcfg3{},
				assignFuncs: map[string]assignFunc{"username": stringsAssignFunc("hello")},
			},
			want: url.Values{},
			cfg: &qcfg3{
				Timeout:  timeutil.Duration(time.Second),
				Username: "hello",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := bindQuery(tt.args.query, tt.args.v, tt.args.assignFuncs)
			if (err != nil) != tt.wantErr {
				t.Errorf("DecodeQuery() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("DecodeQuery() = %v, want %v", got, tt.want)
			}
			if !reflect.DeepEqual(tt.args.v, tt.cfg) {
				t.Errorf("DecodeQuery() = %+v, want %+v", tt.args.v, tt.cfg)
			}
		})
	}
}

// TestBindQueryNonPointer 验证非指针参数返回 InvalidBindError。
func TestBindQueryNonPointer(t *testing.T) {
	_, err := bindQuery(url.Values{}, qcfg1{}, nil)
	if err == nil {
		t.Fatal("expect InvalidBindError, got nil")
	}
	if _, ok := err.(*InvalidBindError); !ok {
		t.Fatalf("expect *InvalidBindError, got %T", err)
	}
}

// TestBindQueryNilPointer 验证 nil 指针参数返回 InvalidBindError。
func TestBindQueryNilPointer(t *testing.T) {
	var v *qcfg1
	_, err := bindQuery(url.Values{}, v, nil)
	if err == nil {
		t.Fatal("expect InvalidBindError, got nil")
	}
	if _, ok := err.(*InvalidBindError); !ok {
		t.Fatalf("expect *InvalidBindError, got %T", err)
	}
}

// TestBindQueryTypeError 验证类型不匹配返回 BindTypeError。
func TestBindQueryTypeError(t *testing.T) {
	type bad struct {
		Foo int `dsn:"query.foo"`
	}
	// 把字符串值绑给 int 字段会触发 BindTypeError
	_, err := bindQuery(url.Values{"foo": {"notanint"}}, &bad{}, nil)
	if err == nil {
		t.Fatal("expect BindTypeError, got nil")
	}
	if _, ok := err.(*BindTypeError); !ok {
		t.Fatalf("expect *BindTypeError, got %T", err)
	}
}
