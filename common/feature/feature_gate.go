// Package feature 提供运行时特性开关（feature gate），支持按名称注册特性、
// 通过命令行 flag 或 map 启用/禁用特性，以及查询特性启用状态。
//
// 仅依赖标准库，不引入任何外部日志或错误包装库。并发安全：内部状态用
// atomic.Value 存储并在 mutex 保护下复制后整体替换。
package feature

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Feature 是特性名称。
type Feature string

const (
	flagName = "feature-gates"
)

var (
	// DefaultGate 是包级共享 Gate。
	DefaultGate = NewGate()
)

// Spec 描述特性的规格，目前仅含默认值。
type Spec struct {
	Default bool
}

// Gate 解析并存储已知特性的开关状态，支持从形如 "feature1=true,feature2=false" 的字符串解析。
type Gate interface {
	// AddFlag 在指定 FlagSet 上注册全局特性开关 flag。
	AddFlag(fs *flag.FlagSet)
	// Set 从形如 "feature1=true,feature2=false" 的字符串解析并存储已知特性的开关状态。
	Set(value string) error
	// SetFromMap 从 map[string]bool 存储已知特性的开关状态，未知 key 返回错误。
	SetFromMap(m map[string]bool) error
	// Enabled 返回指定特性是否启用；未显式设置时回退到默认值。
	Enabled(key Feature) bool
	// Add 向 featureGate 注册特性；AddFlag 调用后不可再注册。
	Add(features map[Feature]Spec) error
	// KnownFeatures 返回描述所有已知特性的字符串切片（排序）。
	KnownFeatures() []string
	// DeepCopy 返回 Gate 的深拷贝，便于在提交配置变更前校验开关状态。
	DeepCopy() Gate
}

// featureGate 实现 Gate 接口，同时实现 flag.Value 用于 flag 解析。
type featureGate struct {
	// lock 保护 known、enabled 的写操作以及 closed 的读写。
	lock sync.Mutex
	// known 存储 map[Feature]Spec。
	known atomic.Value
	// enabled 存储 map[Feature]bool。
	enabled atomic.Value
	// closed 在 AddFlag 调用后置 true，禁止后续 Add。
	closed bool
}

// Set、String、Type 实现 flag.Value 接口。
var _ flag.Value = &featureGate{}

// NewGate 创建一个特性开关。
func NewGate() *featureGate {
	known := map[Feature]Spec{}
	knownValue := atomic.Value{}
	knownValue.Store(known)

	enabled := map[Feature]bool{}
	enabledValue := atomic.Value{}
	enabledValue.Store(enabled)

	f := &featureGate{
		known:   knownValue,
		enabled: enabledValue,
	}
	return f
}

// Set 将 "key1=value1,key2=value2,..." 形式的字符串解析为已知 key 的 map[string]bool，解析失败返回错误。
// 同时实现 flag.Value.Set。
func (f *featureGate) Set(value string) error {
	f.lock.Lock()
	defer f.lock.Unlock()

	// 复制现有状态
	known := map[Feature]Spec{}
	for k, v := range f.known.Load().(map[Feature]Spec) {
		known[k] = v
	}
	enabled := map[Feature]bool{}
	for k, v := range f.enabled.Load().(map[Feature]bool) {
		enabled[k] = v
	}

	for _, s := range strings.Split(value, ",") {
		if len(s) == 0 {
			continue
		}
		arr := strings.SplitN(s, "=", 2)
		k := Feature(strings.TrimSpace(arr[0]))
		_, ok := known[k]
		if !ok {
			return fmt.Errorf("unrecognized key: %s", k)
		}
		if len(arr) != 2 {
			return fmt.Errorf("missing bool value for %s", k)
		}
		v := strings.TrimSpace(arr[1])
		boolValue, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("invalid value of %s: %s, err: %v", k, v, err)
		}
		enabled[k] = boolValue
	}

	// 持久化变更
	f.known.Store(known)
	f.enabled.Store(enabled)

	fmt.Fprintf(os.Stderr, "feature gates: %v", enabled)
	return nil
}

// SetFromMap 从 map[string]bool 存储已知特性的开关状态，未知 key 返回错误。
func (f *featureGate) SetFromMap(m map[string]bool) error {
	f.lock.Lock()
	defer f.lock.Unlock()

	// 复制现有状态
	known := map[Feature]Spec{}
	for k, v := range f.known.Load().(map[Feature]Spec) {
		known[k] = v
	}
	enabled := map[Feature]bool{}
	for k, v := range f.enabled.Load().(map[Feature]bool) {
		enabled[k] = v
	}

	for k, v := range m {
		k := Feature(k)
		_, ok := known[k]
		if !ok {
			return fmt.Errorf("unrecognized key: %s", k)
		}
		enabled[k] = v
	}

	// 持久化变更
	f.known.Store(known)
	f.enabled.Store(enabled)

	fmt.Fprintf(os.Stderr, "feature gates: %v", f.enabled)
	return nil
}

// String 返回所有已启用特性的状态字符串，格式为 "key1=value1,key2=value2,..."（按 key 排序）。
// 同时实现 flag.Value.String。
func (f *featureGate) String() string {
	pairs := []string{}
	enabled, ok := f.enabled.Load().(map[Feature]bool)
	if !ok {
		return ""
	}
	for k, v := range enabled {
		pairs = append(pairs, fmt.Sprintf("%s=%t", k, v))
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}

// Type 返回 flag.Value 的类型标识。
func (f *featureGate) Type() string {
	return "mapStringBool"
}

// Add 向 featureGate 注册特性；AddFlag 调用后不可再注册。
func (f *featureGate) Add(features map[Feature]Spec) error {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.closed {
		return fmt.Errorf("cannot add a feature gate after adding it to the flag set")
	}

	// 复制现有状态
	known := map[Feature]Spec{}
	for k, v := range f.known.Load().(map[Feature]Spec) {
		known[k] = v
	}

	for name, spec := range features {
		if existingSpec, found := known[name]; found {
			if existingSpec == spec {
				continue
			}
			return fmt.Errorf("feature gate %q with different spec already exists: %v", name, existingSpec)
		}

		known[name] = spec
	}

	// 持久化更新状态
	f.known.Store(known)

	return nil
}

// Enabled 返回指定特性是否启用；未显式设置时回退到 Spec.Default。
func (f *featureGate) Enabled(key Feature) bool {
	if v, ok := f.enabled.Load().(map[Feature]bool)[key]; ok {
		return v
	}
	return f.known.Load().(map[Feature]Spec)[key].Default
}

// AddFlag 在指定 FlagSet 上注册 feature-gates flag；调用后冻结 Add。
func (f *featureGate) AddFlag(fs *flag.FlagSet) {
	f.lock.Lock()
	f.closed = true
	f.lock.Unlock()

	known := f.KnownFeatures()
	fs.Var(f, flagName, ""+
		"一组 key=value 对，用于描述 alpha/实验特性的特性开关。"+
		"可选项：\n"+strings.Join(known, "\n"))
}

// KnownFeatures 返回描述所有已知特性的字符串切片（按 key 排序）。
func (f *featureGate) KnownFeatures() []string {
	var known []string
	for k, v := range f.known.Load().(map[Feature]Spec) {
		known = append(known, fmt.Sprintf("%s=true|false (default=%t)", k, v.Default))
	}
	sort.Strings(known)
	return known
}

// DeepCopy 返回 Gate 的深拷贝，便于在提交配置变更前校验开关状态。
func (f *featureGate) DeepCopy() Gate {
	// 复制现有状态
	known := map[Feature]Spec{}
	for k, v := range f.known.Load().(map[Feature]Spec) {
		known[k] = v
	}
	enabled := map[Feature]bool{}
	for k, v := range f.enabled.Load().(map[Feature]bool) {
		enabled[k] = v
	}

	// 将复制后的状态存入新的 atomic.Value。
	knownValue := atomic.Value{}
	knownValue.Store(known)
	enabledValue := atomic.Value{}
	enabledValue.Store(enabled)

	// 基于复制后的状态构造新的 featureGate，并保留原 closed 状态。
	return &featureGate{
		known:   knownValue,
		enabled: enabledValue,
		closed:  f.closed,
	}
}
