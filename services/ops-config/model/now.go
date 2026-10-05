package model

import (
	"strconv"
	"strings"
	"time"
)

func nowUnix() int64 { return time.Now().Unix() }

// NowUnix 暴露当前 Unix 秒，供 logic/repository 统一取时间（避免各处自行 time.Now 造成
// 同一次写入的 ctime/published_at 不一致）。
func NowUnix() int64 { return nowUnix() }

// placeholders 生成 "?, ?, ..."，列数来自代码常量而不是用户输入，无注入面。
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// PageOrDefault 归一化分页：pn 至少 1，ps 落在 [1, maxPs]。
// 与 audit 不同，这里对越界 ps 采取「夹到上限」而不是报错：后台列表页的 ps 常由
// 前端组件默认值带上来，报错只会让控制台白屏，而上限本身已经是安全边界。
func PageOrDefault(pn, ps int32, maxPs int) (int32, int32) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 {
		ps = int32(maxPs)
	}
	if ps > int32(maxPs) {
		ps = int32(maxPs)
	}
	return pn, ps
}

// IDListMaxLen 是引用列表（zone_ids/tag_ids/platforms/白名单 mid）的入库字符上限。
// 上限存在的两个理由：专题挂几百个分区通常是配置错误；这些列表列要参与
// LIKE 匹配（按引用 ID 反查专题），长度失控会同时拖慢查询。
const IDListMaxLen = 512

// 列表列的统一存储形态：",1,2,3,"（首尾各带一个逗号）。
// 这样「是否包含某 ID」可以用 LIKE '%,2,%' 精确判定，不会把 2 误匹配到 12 或 23。
// 读写只走本文件的函数，禁止在 logic 里手拼字符串——漏掉首尾逗号的数据
// 不会报错，只会让「按分区反查专题」静默漏行，那是最难查的一类 bug。

// IDListString 把 ID 列表序列化为存储形态（去零、去重、保序）。
// 调用方必须先经 ValidateIDList 得到归一化列表，再写回结构体，
// 保证同一集合在库里只有一种字节形态。
func IDListString(ids []int64) string {
	var b strings.Builder
	for _, id := range ids {
		if id == 0 {
			continue
		}
		b.WriteByte(',')
		b.WriteString(strconv.FormatInt(id, 10))
	}
	if b.Len() == 0 {
		return ""
	}
	b.WriteByte(',')
	return b.String()
}

// ParseIDList 解析库里的引用列表。无法解析的片段被丢弃而不是报错：
// 读路径必须对历史脏数据稳健，写路径已在落库前用 ValidateIDList 校验过。
func ParseIDList(s string) []int64 {
	if s == "" {
		return nil
	}
	parts := strings.Split(strings.Trim(s, ","), ",")
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		v, err := strconv.ParseInt(p, 10, 64)
		if err != nil || v == 0 {
			continue
		}
		out = append(out, v)
	}
	return out
}

// ValidateIDList 去零、去重、保序，并检查数量与入库长度上限。
func ValidateIDList(ids []int64, maxCount int) ([]int64, error) {
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if maxCount > 0 && len(out) > maxCount {
		return nil, ErrBatchTooLarge
	}
	if len(IDListString(out)) > IDListMaxLen {
		return nil, ErrTopicIDListTooLong
	}
	return out, nil
}

// IDListContains 判定存储形态的列表里是否含某 ID（内存侧等价于 LIKE '%,id,%'）。
// 单测用它锁住与 SQL 匹配口径的一致性。
func IDListContains(stored string, id int64) bool {
	if id <= 0 {
		return false
	}
	return strings.Contains(stored, ","+strconv.FormatInt(id, 10)+",")
}

// IDListLike 生成与 IDListContains 完全同口径的 LIKE 参数（SQL 侧过滤用）。
// 两者必须成对使用：内存判定与 SQL 判定口径一旦分叉，
// 「后台按分区筛出来的专题」和「运行时命中分区的专题」就会不是同一批数据。
// 返回值只作为绑定参数（?）使用，绝不拼进 SQL 文本。
func IDListLike(id int64) string {
	return "%," + strconv.FormatInt(id, 10) + ",%"
}
