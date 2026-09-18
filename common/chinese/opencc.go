// Package chinese 提供基于 OpenCC 字典方案的中文简繁转换能力。
//
// 转换器通过 cedar Trie 对输入字符串做前缀匹配，按转换链顺序应用各字典组。
// 字典数据通过包级 Loader 加载，默认优先读取本地文件，再回退到远程 URL，
// 生产环境应通过 SetDictRoot 指向本地打包目录以避免运行时外网依赖。
package chinese

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/zeromicro/go-zero/core/logx"
)

// defaultSimplifiedScheme 简体转换使用的默认方案。
const defaultSimplifiedScheme = "tw2sp"

// defaultTraditionalScheme 繁体转换使用的默认方案。
const defaultTraditionalScheme = "s2twp"

// Group 持有一组按顺序应用的字典。
type Group struct {
	Files []string
	Dicts []*dict
}

// String 返回 Group 引用的字典文件列表，便于调试。
func (g *Group) String() string {
	return fmt.Sprintf("%+v", g.Files)
}

// Converter 中文简繁转换器，封装一组转换链。
// 一次构造可多次并发调用 Convert。
type Converter struct {
	Conversion string
	desc       string
	DictGroup  []*Group
}

// NewConverter 根据方案名或本地 JSON 配置文件路径构造 Converter。
//
// nameOrPath 取值：
//   - 内置方案名：hk2s/s2hk/s2t/s2tw/s2twp/t2hk/t2s/t2tw/tw2s/tw2sp。
//   - 其他：按文件路径读取 JSON 配置（格式同内置方案）。
func NewConverter(nameOrPath string) (*Converter, error) {
	cfg, err := resolveConfig(nameOrPath)
	if err != nil {
		return nil, err
	}
	cc := &Converter{Conversion: cfg}
	if err := cc.load(); err != nil {
		return nil, err
	}
	return cc, nil
}

// Description 返回方案描述。
func (c *Converter) Description() string {
	return c.desc
}

// Convert 应用转换链返回结果。多个 goroutine 可并发调用。
func (c *Converter) Convert(in string) string {
	out, err := c.convert(in)
	if err != nil {
		logx.Errorf("chinese: convert %q err=%v", in, err)
		return in
	}
	return out
}

// resolveConfig 解析 nameOrPath 为方案 JSON 配置字符串。
func resolveConfig(nameOrPath string) (string, error) {
	if cfg, ok := schemes[nameOrPath]; ok {
		return cfg, nil
	}
	// 不是已知方案名时，按本地文件路径读取
	bs, err := Loader(nameOrPath)
	if err != nil {
		return "", fmt.Errorf("chinese: unknown scheme or unreadable config %q: %w", nameOrPath, err)
	}
	return string(bs), nil
}

// load 解析 Conversion JSON 并构建所有字典组。
func (c *Converter) load() error {
	var m map[string]any
	if err := json.Unmarshal([]byte(c.Conversion), &m); err != nil {
		return fmt.Errorf("chinese: parse config: %w", err)
	}
	desc, _ := m["name"].(string)
	c.desc = desc
	chain, ok := m["conversion_chain"].([]any)
	if !ok {
		return fmt.Errorf("chinese: config %q missing conversion_chain", c.Description())
	}
	for _, v := range chain {
		d, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("chinese: conversion_chain items must be objects")
		}
		dictMap, ok := d["dict"]
		if !ok {
			return fmt.Errorf("chinese: conversion_chain item missing dict")
		}
		dm, ok := dictMap.(map[string]any)
		if !ok {
			return fmt.Errorf("chinese: dict must be object")
		}
		group, err := buildGroup(dm)
		if err != nil {
			return err
		}
		c.DictGroup = append(c.DictGroup, group)
	}
	return nil
}

// buildGroup 递归构建 group：type=group 时合并子 dicts，type=txt 时加载单个文件。
func buildGroup(d map[string]any) (*Group, error) {
	typ, ok := d["type"].(string)
	if !ok {
		return nil, fmt.Errorf("chinese: dict type must be string")
	}
	res := &Group{}
	switch typ {
	case "group":
		dicts, ok := d["dicts"].([]any)
		if !ok {
			return nil, fmt.Errorf("chinese: group dict missing dicts")
		}
		for _, sub := range dicts {
			subMap, ok := sub.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("chinese: group dicts items must be objects")
			}
			subGroup, err := buildGroup(subMap)
			if err != nil {
				return nil, err
			}
			res.Files = append(res.Files, subGroup.Files...)
			res.Dicts = append(res.Dicts, subGroup.Dicts...)
		}
	case "txt":
		file, ok := d["file"].(string)
		if !ok {
			return nil, fmt.Errorf("chinese: txt dict missing file")
		}
		da, err := buildFromFile(file)
		if err != nil {
			return nil, err
		}
		res.Files = append(res.Files, file)
		res.Dicts = append(res.Dicts, da)
	default:
		return nil, fmt.Errorf("chinese: unsupported dict type %q", typ)
	}
	return res, nil
}

// convert 按转换链顺序对每个 Group 做前缀匹配替换。
func (c *Converter) convert(in string) (string, error) {
	for _, group := range c.DictGroup {
		r := []rune(in)
		var tokens []string
		for i := 0; i < len(r); {
			rest := string(r[i:])
			matched, token, advance, err := c.matchGroup(group, rest)
			if err != nil {
				return "", err
			}
			if !matched {
				token = string(r[i])
				advance = 1
			}
			tokens = append(tokens, token)
			i += advance
		}
		in = strings.Join(tokens, "")
	}
	return in, nil
}

// matchGroup 在指定 group 内对 rest 做前缀匹配。
// 返回 (matched, token, advance, error)。
// advance 为匹配消耗的 rune 数；未匹配时返回 (false, "", 0, nil)。
// 同一 group 内按字典顺序取首个有匹配的字典，并在该字典所有匹配中取最长 key。
func (c *Converter) matchGroup(group *Group, rest string) (bool, string, int, error) {
	for _, d := range group.Dicts {
		ret, err := d.prefixMatch(rest)
		if err != nil {
			return false, "", 0, err
		}
		if len(ret) == 0 {
			continue
		}
		var (
			token   string
			bestKey string
			max     int
		)
		for k, v := range ret {
			if len(k) > max {
				max = len(k)
				bestKey = k
				if len(v) > 0 {
					token = v[0]
				} else {
					token = k
				}
			}
		}
		if bestKey == "" {
			continue
		}
		return true, token, len([]rune(bestKey)), nil
	}
	return false, "", 0, nil
}

// --- 包级单例：ToSimplified / ToTraditional ---

var (
	simplifiedOnce sync.Once
	simplifiedConv *Converter
	simplifiedErr  error

	traditionalOnce sync.Once
	traditionalConv *Converter
	traditionalErr  error
)

// ToSimplified 把输入字符串转为简体，使用 tw2sp 方案。
// 单例构造失败时记录日志并返回原字符串。
func ToSimplified(in string) string {
	simplifiedOnce.Do(func() {
		simplifiedConv, simplifiedErr = NewConverter(defaultSimplifiedScheme)
		if simplifiedErr != nil {
			logx.Errorf("chinese: init simplified converter %q: %v", defaultSimplifiedScheme, simplifiedErr)
		}
	})
	if simplifiedErr != nil || simplifiedConv == nil {
		return in
	}
	return simplifiedConv.Convert(in)
}

// ToTraditional 把输入字符串转为繁体，使用 s2twp 方案。
// 单例构造失败时记录日志并返回原字符串。
func ToTraditional(in string) string {
	traditionalOnce.Do(func() {
		traditionalConv, traditionalErr = NewConverter(defaultTraditionalScheme)
		if traditionalErr != nil {
			logx.Errorf("chinese: init traditional converter %q: %v", defaultTraditionalScheme, traditionalErr)
		}
	})
	if traditionalErr != nil || traditionalConv == nil {
		return in
	}
	return traditionalConv.Convert(in)
}
