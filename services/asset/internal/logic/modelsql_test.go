package logic

// modelsql_test.go 用**源码文本对账**兜住三件替身在行为上测不到的事：
//
//  1. model 的 ORDER BY 方向：本包替身的排序、以及所有分页/顺序用例的期望序列，
//     都以这里抽出来的子句为唯一事实源（clauseLess 登记表与它一对一比对）。
//     SQL 一改，对账先红；把登记表改对新口径后，期望值自动跟着变，不需要手抄。
//  2. 生产 repository.Cache 的 key 拼装与 TTL：测试把 *repository.Cache 整个换成了内存替身，
//     真实 key 与 Setex 的 TTL 不可能从行为上被观察到，只能对账。
//  3. 「缓存值为空串按 miss 处理」「读穿回填失败就地吞掉」两条判定：同样被替身取代。
//
// 对账**不是**行为验证，所以 README 的已知缺口仍写着「缓存 key/TTL 无行为级覆盖」。
// 这里刻意只对本服务实际承诺的口径取值：把 60 改成别的数字、把 key 换个前缀，
// 都必须在同一轮改动里更新用例与文档。

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const (
	modelSourcePath      = "../../model/assetmodel.go"
	repositorySourcePath = "../repository/repository.go"

	// pinnedCacheTTL 是详情缓存的 TTL（秒）：读侧「命中即返回、不校验版本」的
	// 最大陈旧窗口就是它。见 TestGetAssetCacheHitServesStaleProbeResult。
	pinnedCacheTTL = 60
)

// orderByRE 抓 Go 源码字符串里的 ORDER BY 子句：列清单 + 方向，到 ASC/DESC 为止，
// 因此 "ORDER BY asset_id DESC LIMIT ? OFFSET ?" 抓到 "asset_id DESC"。
var orderByRE = regexp.MustCompile(`ORDER BY ([A-Za-z_]+(?:,\s*[A-Za-z_]+)*)\s+(ASC|DESC)`)

func readSource(t *testing.T, path string) string {
	t.Helper()
	bs, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败（路径变了就说明有人挪了 model/repository，先对齐用例）：%v", path, err)
	}
	return string(bs)
}

// TestModelOrderByClausesStillMatchFakes 要求 model 里出现的 ORDER BY 子句集合
// 与本包替身登记的集合**完全相等**（多一条少一条都判红）。
func TestModelOrderByClausesStillMatchFakes(t *testing.T) {
	src := readSource(t, modelSourcePath)

	fromModel := []string{}
	for _, m := range orderByRE.FindAllStringSubmatch(src, -1) {
		fromModel = append(fromModel, m[1]+" "+m[2])
	}
	slices.Sort(fromModel)

	fromFakes := make([]string, 0, len(clauseLess))
	for clause := range clauseLess {
		fromFakes = append(fromFakes, clause)
	}
	slices.Sort(fromFakes)

	// model 的 3 个列表/单行查询里只有 3 条 ORDER BY：List(asset_meta)、
	// ListByAsset(asset_cover)、ListByAsset(asset_subtitle)；FindOne/Insert/Update 无排序。
	want := []string{orderByAssetsList, orderByCoverList, orderBySubtitleList}
	slices.Sort(want)

	if strings.Join(fromModel, "|") != strings.Join(want, "|") {
		t.Errorf("model/assetmodel.go 的 ORDER BY 集合 = %v, want %v（用例替身的排序口径以它为准）",
			fromModel, want)
	}
	if strings.Join(fromFakes, "|") != strings.Join(want, "|") {
		t.Errorf("fakes_test.go 的 clauseLess 登记表 = %v, want %v（与 model 逐条对齐）",
			fromFakes, want)
	}
}

// TestProductionCacheKeyPatternStillMatchesFake 用生产源码里的 key 模板**实际格式化**一次，
// 与替身的 keyAsset 比对：只对模板字符串没有意义，必须真的拼出同一个 key。
func TestProductionCacheKeyPatternStillMatchesFake(t *testing.T) {
	src := readSource(t, repositorySourcePath)

	re := regexp.MustCompile(`(?s)const \(\s*prefixAsset\s*=\s*"([^"]*)"`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("没在 repository.go 里抓到 prefixAsset 常量：缓存 key 口径变了，先对齐替身与用例")
	}
	pattern := m[1]
	if n := strings.Count(pattern, "%d"); n != 1 {
		t.Fatalf("prefixAsset = %q：期望恰好一个 %%d 占位（asset_id），实得 %d 个", pattern, n)
	}
	for _, id := range []int64{1, 123, 9007199254740993} {
		if got, want := fmt.Sprintf(pattern, id), keyAsset(id); got != want {
			t.Errorf("key 拼装漂移：%q 格式化 %d = %q, 替身 keyAsset = %q", pattern, id, got, want)
		}
	}

	// TTL：读侧陈旧窗口的唯一来源。
	ttlRE := regexp.MustCompile(`cacheTTL\s*=\s*(\d+)`)
	tm := ttlRE.FindStringSubmatch(src)
	if tm == nil {
		t.Fatalf("没在 repository.go 里抓到 cacheTTL 常量")
	}
	if got, want := tm[1], fmt.Sprint(pinnedCacheTTL); got != want {
		t.Errorf("详情缓存 TTL = %s 秒, want %s 秒（README 与陈旧窗口用例都按这个值写）", got, want)
	}
}

// TestCacheMissAndBackfillJudgementsUnchanged 钉两条被替身取代的判定仍然成立：
//   - Cache.GetAsset 用 `bs != ""` 决定 hit ⇒ 空串是 miss（TestGetAssetEmptyPayloadIsTreatedAsMiss 依赖它）；
//   - Repository.GetAsset 的回填写的是 `_ = r.cache.SetAsset(...)` ⇒ 失败就地吞、连日志都没有
//     （TestGetAssetBackfillFailureIsSwallowed 依赖它）。
func TestCacheMissAndBackfillJudgementsUnchanged(t *testing.T) {
	cacheSrc := readSource(t, repositorySourcePath)
	if !strings.Contains(cacheSrc, `return bs, bs != "", nil`) {
		t.Errorf(`repository.go 里找不到 ` + "`return bs, bs != \"\", nil`" +
			`：空串不再等价 miss，替身与用例都得跟着改`)
	}
	if !strings.Contains(cacheSrc, `_ = r.cache.SetAsset(ctx, assetID, jsonMustMarshal(m))`) {
		t.Errorf(`repository.go 里找不到读穿回填的 ` + "`_ = r.cache.SetAsset(...)`" +
			`：回填错误开始影响读路径了，TestGetAssetBackfillFailureIsSwallowed 的结论要重写`)
	}
}
