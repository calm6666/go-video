package logic

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"go-video/services/coin/internal/config"
	"go-video/services/coin/model"
	"go-video/services/coin/rpc"

	"github.com/zeromicro/go-zero/core/stores/redis"
)

// conv.go 是 10 个方法共用的唯一投影口径，纯函数部分必须单独证明：
// 「nil 行怎么回」「枚举外的值怎么塌」「列宽怎么收敛」「缓存未启用时是否回源」。

func TestAccountInfoEchoesLimitsEvenWithoutAccount(t *testing.T) {
	coin := testCoinConf()
	info := accountInfo(7, nil, 0, coin)
	if info == nil {
		t.Fatal("没有账户行也必须回一个结构，不能让客户端读 nil")
	}
	if info.Balance != 0 || info.TotalTossed != 0 || info.Version != 0 {
		t.Errorf("缺账户时余额/历史一律 0，不得凭空给初始币：%+v", info)
	}
	// 上限三项必须照抄生效配置：否则客户端会以为「没账户 = 没上限」。
	if info.TodayLimit != coin.DailyLimit || info.PerTargetLimit != coin.PerTargetLimit ||
		info.CancelWindowSeconds != coin.CancelWindowSeconds {
		t.Errorf("限额未回显：%+v", info)
	}
	if info.Mid != 7 {
		t.Errorf("mid 未回显：%d", info.Mid)
	}
}

func TestAccountInfoProjectsRowFields(t *testing.T) {
	coin := testCoinConf()
	acc := &model.Account{Mid: 7, Balance: 12, TotalTossed: 8, Version: 3, Ctime: 100, Mtime: 200}
	info := accountInfo(0, acc, 5, coin)
	if info.Mid != 7 {
		t.Errorf("行里的 mid 应覆盖入参占位 0，得到 %d", info.Mid)
	}
	if info.Balance != 12 || info.TotalTossed != 8 || info.Version != 3 ||
		info.Ctime != 100 || info.Mtime != 200 || info.TodayTossed != 5 {
		t.Errorf("字段投影不全：%+v", info)
	}
}

func TestTossAndFlowProjectionHandleNil(t *testing.T) {
	if got := tossInfo(nil); got != nil {
		t.Errorf("tossInfo(nil) 应回 nil（表示「没有记录」），得到 %+v", got)
	}
	if got := flowInfo(nil); got != nil {
		t.Errorf("flowInfo(nil) 应回 nil，得到 %+v", got)
	}
	// 空切片投影成非 nil 的空数组：JSON 序列化后是 []，不是 null。
	if got := tossInfos(nil); got == nil || len(got) != 0 {
		t.Errorf("tossInfos(nil) 应为空数组而不是 nil：%#v", got)
	}
	if got := flowInfos(nil); got == nil || len(got) != 0 {
		t.Errorf("flowInfos(nil) 应为空数组而不是 nil：%#v", got)
	}
}

func TestTossInfoProjectsAllContractFields(t *testing.T) {
	row := &model.Toss{ID: 9, Mid: 7, TargetAid: 21, Count: 3, State: model.TossStateCancelled,
		FirstTossedAt: 100, LastTossedAt: 200, CancelledAt: 300, LastRequestID: "req-9"}
	got := tossInfo(row)
	if got.TossId != 9 || got.Mid != 7 || got.TargetAid != 21 || got.Count != 3 ||
		got.State != rpc.TossState_TOSS_STATE_CANCELLED || got.FirstTossedAt != 100 ||
		got.LastTossedAt != 200 || got.CancelledAt != 300 || got.LastRequestId != "req-9" {
		t.Errorf("投币记录投影不符：%+v", got)
	}
}

// TestEnumProjectionCollapsesUnknown 库里出现枚举外的值意味着数据被绕过本服务写过，
// 必须塌成 UNSPECIFIED 让调用方看得见，而不是原样发一个不存在的状态。
func TestEnumProjectionCollapsesUnknown(t *testing.T) {
	cases := []struct {
		name            string
		toss            int32
		flow            int32
		wantUnspecified bool
	}{
		{"合法值原样映射", model.TossStateCancelled, model.FlowTypeOrderPack, false},
		{"零值（未写状态）", 0, 0, true},
		{"枚举外的脏数据", 7, 99, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := rpcTossState(tc.toss)
			ft := rpcFlowType(tc.flow)
			if _, ok := rpc.TossState_name[int32(st)]; !ok {
				t.Fatalf("投影出了枚举外的投币状态：%d", st)
			}
			if _, ok := rpc.CoinFlowType_name[int32(ft)]; !ok {
				t.Fatalf("投影出了枚举外的流水类型：%d", ft)
			}
			if tc.wantUnspecified {
				if st != rpc.TossState_TOSS_STATE_UNSPECIFIED || ft != rpc.CoinFlowType_COIN_FLOW_TYPE_UNSPECIFIED {
					t.Fatalf("脏数据应塌成 UNSPECIFIED 让调用方看得见：toss=%d flow=%d", st, ft)
				}
				return
			}
			if st != rpc.TossState_TOSS_STATE_CANCELLED || ft != rpc.CoinFlowType_COIN_FLOW_TYPE_ORDER_PACK {
				t.Fatalf("合法枚举被改写了：toss=%d flow=%d", st, ft)
			}
		})
	}
}

func TestValidFlowTypeCoversEnumOnly(t *testing.T) {
	for v := int32(model.FlowTypeToss); v <= model.FlowTypeExpire; v++ {
		if !validFlowType(v) {
			t.Errorf("flow_type=%d 是合法枚举却判为非法", v)
		}
	}
	for _, v := range []int32{0, -1, model.FlowTypeExpire + 1, 999} {
		if validFlowType(v) {
			t.Errorf("flow_type=%d 不该被当成合法值", v)
		}
	}
}

func TestInitialGrantFlowIsSelfDescribing(t *testing.T) {
	f := initialGrantFlow(7, 5)
	if f.Mid != 7 || f.Delta != 5 || f.BalanceAfter != 5 || f.FlowType != model.FlowTypeAdminGrant {
		t.Errorf("建仓流水不符：%+v", f)
	}
	// 三个标记必须能把「沙箱初始币」和「运营白送」「订单购买」区分开，否则对账分桶就是错的。
	if f.BizNo != model.InitialGrantBizNo || f.Operator != model.InitialGrantOperator ||
		f.RequestID != model.InitialGrantRequestID(7) {
		t.Errorf("建仓流水标识不符：%+v", f)
	}
	if f.TargetAid != 0 {
		t.Errorf("建仓流水带 target_aid 就伪装成投币了：%d", f.TargetAid)
	}
	want := f.RequestID
	if got := initialGrantFlow(7, 9).RequestID; got != want {
		t.Errorf("同一 mid 的建仓 request_id 必须唯一决定（靠唯一索引挡重复建仓），得到 %s / %s", got, want)
	}
	if got := initialGrantFlow(8, 5).RequestID; got == want {
		t.Errorf("不同 mid 的建仓 request_id 撞号：%s", got)
	}
}

// TestClipRemarkKeepsWholeRunes remark 是 VARCHAR(255) 按字符算，
// 截断必须落在字符边界上：留半个汉字写库就是 1366 错误。
func TestClipRemarkKeepsWholeRunes(t *testing.T) {
	long := strings.Repeat("硬币", 300) // 600 字符
	got := clipRemark(long)
	if n := len([]rune(got)); n != maxRemarkLen {
		t.Fatalf("应收敛到 %d 字符，实际 %d", maxRemarkLen, n)
	}
	if !utf8.ValidString(got) {
		t.Fatal("截断产生了非法 UTF-8")
	}
	if short := "退款说明"; clipRemark(short) != short {
		t.Errorf("未超长的文本不该被改动：%q", clipRemark(short))
	}
}

// TestClipIDIsCharacterBased 同理 operator/trace_id/biz_no 的 VARCHAR(64) 也是按字符算。
func TestClipIDIsCharacterBased(t *testing.T) {
	got := clipID(strings.Repeat("运营", 100))
	if n := len([]rune(got)); n != maxIDLen {
		t.Fatalf("应收敛到 %d 字符，实际 %d", maxIDLen, n)
	}
	if !utf8.ValidString(got) {
		t.Fatal("截断产生了非法 UTF-8：写库会报 1366，把已判定成功的交易打成失败")
	}
	// ASCII ID 的口径不能被打滑：仍是 64 字节。
	if a := clipID(strings.Repeat("a", 200)); len(a) != maxIDLen {
		t.Errorf("ASCII ID 应截到 %d 字节，实际 %d", maxIDLen, len(a))
	}
	if b := clipID("ORDER-1"); b != "ORDER-1" {
		t.Errorf("未超长的 ID 不该被改动：%q", b)
	}
}

func TestNormalizeAidsDedupsThenTrims(t *testing.T) {
	cases := []struct {
		name    string
		in      []int64
		max     int64
		want    []int64
		dropped int
	}{
		{"空输入", nil, 50, []int64{}, 0},
		{"去重保序", []int64{3, 1, 3, 2, 1}, 50, []int64{3, 1, 2}, 2},
		{"丢弃非正 aid", []int64{0, -5, 4}, 50, []int64{4}, 2},
		{"全部非法", []int64{0, -1}, 50, []int64{}, 2},
		{"裁剪到上限（不报错）", []int64{1, 2, 3, 4, 5}, 3, []int64{1, 2, 3}, 2},
		{"重复与超限合并计数", []int64{1, 1, 2, 3, 4}, 2, []int64{1, 2}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, dropped := normalizeAids(tc.in, tc.max)
			if len(got) != len(tc.want) {
				t.Fatalf("aids=%v，期待 %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("顺序敏感：第 %d 个是 %d，期待 %d", i, got[i], tc.want[i])
				}
			}
			if dropped != tc.dropped {
				t.Errorf("dropped=%d，期待 %d", dropped, tc.dropped)
			}
		})
	}
}

func TestSummaryOfAlwaysReturnsStruct(t *testing.T) {
	got := summaryOf(21, cachedSummary{})
	if got == nil || got.Aid != 21 || got.CoinCount != 0 || got.CoinUserCount != 0 {
		t.Fatalf("无投币也要回全 0 汇总，让详情页显示「0 币」：%+v", got)
	}
	if got.LikeCount != 0 {
		t.Errorf("点赞归 engagement 持有，本服务必须恒回 0：%d", got.LikeCount)
	}
	v := cachedSummary{CoinCount: 9, CoinUserCount: 3}
	if s := summaryOf(21, v); s.CoinCount != 9 || s.CoinUserCount != 3 {
		t.Errorf("汇总未投影：%+v", s)
	}
}

func TestSummaryCacheKeyIsNamespaced(t *testing.T) {
	if got := summaryCacheKey(21); got != "coin:sum:21" {
		t.Errorf("缓存键口径变了会命中不了既有数据：%s", got)
	}
	if summaryCacheKey(1) == summaryCacheKey(11) {
		t.Fatal("不同 aid 撞键")
	}
}

// TestSummaryCacheDisabledPaths 未配置 Redis / TTL<=0 时必须整体回源，
// 且一次缓存命令都不许发出去（本测试不连网络）。
// 非 nil 但零值的 *redis.Redis 是刻意选的：go-zero 对未初始化的句柄直接回错误、
// 不会拨号，因此一旦短路条件写错、真去调命令，命中的是「缓存不可用」而不是静默成功。
func TestSummaryCacheDisabledPaths(t *testing.T) {
	if _, err := json.Marshal(cachedSummary{}); err != nil {
		t.Fatalf("缓存值结构异常：%v", err)
	}
	t.Run("Cache 为 nil", func(t *testing.T) {
		sc, _ := newTestSvc(t)
		if sc.Cache != nil {
			t.Fatal("测试装配本身不该带缓存")
		}
		if hit := readSummaryCache(context.Background(), sc, []int64{1, 2}); hit != nil {
			t.Errorf("未启用缓存却报命中：%+v", hit)
		}
		// 三个入口在 Cache==nil 时都必须静默返回（nil 句柄一旦被调用就是 panic）。
		writeSummaryCache(context.Background(), sc, map[int64]cachedSummary{1: {}})
		invalidateSummaryCache(context.Background(), sc, 1)
	})
	t.Run("空 aids 不发起 MGET", func(t *testing.T) {
		sc, _ := newTestSvc(t)
		sc.Cache = &redis.Redis{}
		if hit := readSummaryCache(context.Background(), sc, nil); hit != nil {
			t.Errorf("没有 aid 时不该发起 MGET：%+v", hit)
		}
		writeSummaryCache(context.Background(), sc, nil)
	})
	t.Run("TTL<=0 即视为关闭", func(t *testing.T) {
		sc, _ := newTestSvc(t)
		sc.Config.Coin.TargetSummaryCacheTTLSeconds = 0
		sc.Cache = &redis.Redis{}
		if hit := readSummaryCache(context.Background(), sc, []int64{1}); hit != nil {
			t.Errorf("TTL=0 时不该读缓存：%+v", hit)
		}
		writeSummaryCache(context.Background(), sc, map[int64]cachedSummary{1: {CoinCount: 1}})
	})
	t.Run("非法 aid 不清缓存", func(t *testing.T) {
		sc, _ := newTestSvc(t)
		sc.Cache = &redis.Redis{}
		invalidateSummaryCache(context.Background(), sc, 0) // aid<=0 直接返回，不发 DEL
	})
}

func TestCachedSummaryFieldNamesAreStable(t *testing.T) {
	// 缓存值跨版本复用：字段名或 tag 一变，旧值就会被解成 0，详情页凭空显示「0 币」。
	bs, err := json.Marshal(cachedSummary{CoinCount: 7, CoinUserCount: 2})
	if err != nil {
		t.Fatal(err)
	}
	if string(bs) != `{"c":7,"u":2}` {
		t.Errorf("缓存值结构漂移：%s", bs)
	}
	var back cachedSummary
	if err := json.Unmarshal([]byte(`{"c":5,"u":1}`), &back); err != nil || back.CoinCount != 5 || back.CoinUserCount != 1 {
		t.Errorf("缓存值反序列化不符：%+v %v", back, err)
	}
}

func TestPageSizeAndOffset(t *testing.T) {
	sc, _ := newTestSvc(t)
	if n, err := sc.PageSize(0); err != nil || n != int(sc.Config.Coin.DefaultPageSize) {
		t.Errorf("size<=0 应用默认值：%d %v", n, err)
	}
	if n, err := sc.PageSize(-9); err != nil || n != int(sc.Config.Coin.DefaultPageSize) {
		t.Errorf("负 size 应用默认值：%d %v", n, err)
	}
	if n, err := sc.PageSize(5); err != nil || n != 5 {
		t.Errorf("上限内应原样采纳：%d %v", n, err)
	}
	// 超上限必须报错而不是静默截断：截断会让调用方误判「没有下一页」。
	if _, err := sc.PageSize(int64(sc.Config.Coin.MaxPageSize) + 1); err == nil {
		t.Fatal("超上限应报错")
	} else {
		wantErrIs(t, err, model.ErrPageSizeTooLarge)
	}
	if got := sc.Offset(1, 20); got != 0 {
		t.Errorf("第 1 页 offset 应为 0：%d", got)
	}
	if got := sc.Offset(0, 20); got != 0 {
		t.Errorf("非法页码按第 1 页处理：%d", got)
	}
	if got := sc.Offset(3, 20); got != 40 {
		t.Errorf("offset 计算不符：%d", got)
	}
}

func TestAccountInfoUsesSanitizedFallback(t *testing.T) {
	// 配置被写成非正数时 Sanitize 会兜底；投影必须用兜底后的那份，
	// 否则「配置读起来没上限」而门禁其实在用兜底值。
	var raw config.CoinConf
	raw.Sanitize()
	info := accountInfo(1, nil, 0, raw)
	if info.TodayLimit <= 0 || info.PerTargetLimit <= 0 || info.CancelWindowSeconds <= 0 {
		t.Errorf("Sanitize 兜底后仍是非正数，限额判定会恒假：%+v", info)
	}
}
