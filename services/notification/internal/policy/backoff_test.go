package policy

import (
	"strings"
	"testing"
	"time"

	"go-video/services/notification/model"
)

// 退避阶梯与幂等键（AGENTS.md §5：写接口必须幂等；重试必须有上限）。

func TestNormalizeBackoff(t *testing.T) {
	cases := []struct {
		name  string
		in    []int64
		want  []int64
		reuse bool // 是否等于默认阶梯
	}{
		{"nil 回落默认", nil, []int64{60, 300, 1800, 3600, 21600}, true},
		{"全非法回落默认", []int64{0, -5}, []int64{60, 300, 1800, 3600, 21600}, true},
		{"剔除非法值保留顺序", []int64{0, 10, -1, 20}, []int64{10, 20}, false},
	}
	for _, c := range cases {
		got := NormalizeBackoff(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("%s: got %v want %v", c.name, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%s: got %v want %v", c.name, got, c.want)
			}
		}
	}
	// 返回值必须是副本：调用方原地修改不能污染默认阶梯。
	got := NormalizeBackoff(nil)
	got[0] = 1
	if NormalizeBackoff(nil)[0] != 60 {
		t.Fatal("默认阶梯被外部修改污染")
	}
}

// TestBackoffDelaySequence：退避序列必须逐步拉长并在最后一级封顶，
// 否则要么把用户轰炸、要么永久不再提醒。
func TestBackoffDelaySequence(t *testing.T) {
	steps := NormalizeBackoff(nil)
	want := []time.Duration{
		time.Minute, 5 * time.Minute, 30 * time.Minute, time.Hour, 6 * time.Hour,
		6 * time.Hour, 6 * time.Hour, // 超出阶梯后复用末值
	}
	for i, w := range want {
		rc := int32(i + 1)
		if got := BackoffDelay(rc, steps); got != w {
			t.Errorf("第 %d 次重试等待 = %v, want %v", rc, got, w)
		}
	}
	// retryCount < 1 视为第 1 次，不能算出 0 退避导致忙轮询。
	if got := BackoffDelay(0, steps); got != time.Minute {
		t.Errorf("retryCount=0 应回落到第一级, got %v", got)
	}
	if got := BackoffDelay(-3, steps); got != time.Minute {
		t.Errorf("负数 retryCount 应回落到第一级, got %v", got)
	}
	// 自定义单级阶梯。
	if got := BackoffDelay(9, []int64{7}); got != 7*time.Second {
		t.Errorf("单级阶梯应复用该值, got %v", got)
	}
}

// TestNextRetryAt：next_retry_at 必须是“本次失败时刻 + 第 n 级退避”的 Unix 秒。
func TestNextRetryAt(t *testing.T) {
	steps := NormalizeBackoff(nil)
	now := time.Date(2026, 3, 14, 21, 26, 53, 0, time.UTC)
	cases := []struct {
		retryCount int32
		wantUnix   int64
	}{
		{1, now.Add(time.Minute).Unix()},
		{2, now.Add(5 * time.Minute).Unix()},
		{3, now.Add(30 * time.Minute).Unix()},
		{4, now.Add(time.Hour).Unix()},
		{5, now.Add(6 * time.Hour).Unix()},
		{6, now.Add(6 * time.Hour).Unix()},
	}
	for _, c := range cases {
		if got := NextRetryAt(now, c.retryCount, steps); got != c.wantUnix {
			t.Errorf("NextRetryAt(now,%d) = %d, want %d", c.retryCount, got, c.wantUnix)
		}
	}
	// 落在带时区偏移的时刻上：必须按绝对时间推进，与本机时区无关。
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skip("无 tzdata")
	}
	at := time.Date(2026, 3, 14, 5, 0, 0, 0, shanghai)
	if got := NextRetryAt(at, 1, steps); got != at.Add(time.Minute).Unix() {
		t.Errorf("Asia/Shanghai 时刻的退避计算漂移: %d vs %d", got, at.Add(time.Minute).Unix())
	}
}

func TestRetryExhausted(t *testing.T) {
	if RetryExhausted(3, 0) || RetryExhausted(1<<20, -1) {
		t.Error("max<=0 表示不限制，不应判定耗尽")
	}
	if RetryExhausted(2, 3) {
		t.Error("2/3 未达上限")
	}
	if !RetryExhausted(3, 3) {
		t.Error("3/3 应判定耗尽")
	}
}

// TestGroupBizKey：没有幂等键的写接口不允许存在。
func TestGroupBizKey(t *testing.T) {
	if got, err := GroupBizKey(" order-ship:SO-1 ", ""); err != nil || got != "order-ship:SO-1" {
		t.Errorf("biz_key 优先且需去空白: %q %v", got, err)
	}
	if got, err := GroupBizKey("", "idem-1"); err != nil || got != "idem-1" {
		t.Errorf("应回落到 idempotency_key: %q %v", got, err)
	}
	if _, err := GroupBizKey("  ", "   "); err == nil {
		t.Fatal("两个键都为空时必须拒绝")
	}
}

// TestRowBizKeyDeterministic：行级键是跨实例去重的唯一依据，
// 相同输入必须得到相同 64 位摘要，不同收件人/通道/设备必须不同。
func TestRowBizKeyDeterministic(t *testing.T) {
	key := func(g string, ch int32, mid int64, ref string) string {
		t.Helper()
		k, err := RowBizKey(g, ch, mid, ref)
		if err != nil {
			t.Fatalf("RowBizKey: %v", err)
		}
		return k
	}
	base := key("grp-1", model.ChannelPush, 1001, "")
	if len(base) != 64 {
		t.Errorf("行级键长度 = %d, want 64（对齐 biz_key CHAR(64)）", len(base))
	}
	if base != key("grp-1", model.ChannelPush, 1001, "") {
		t.Error("相同输入派生出的键不稳定")
	}
	diff := map[string]string{
		"不同请求级键":   key("grp-2", model.ChannelPush, 1001, ""),
		"不同通道":     key("grp-1", model.ChannelSMS, 1001, ""),
		"不同 mid":   key("grp-1", model.ChannelPush, 1002, ""),
		"带 target": key("grp-1", model.ChannelPush, 1001, "device:abc"),
	}
	for name, v := range diff {
		if v == base {
			t.Errorf("%s 的行级键与基线相同，幂等键无法区分任务", name)
		}
	}
	if _, err := RowBizKey("", model.ChannelPush, 1, ""); err == nil {
		t.Error("空请求级键应被拒绝")
	}
	if _, err := RowBizKey("g", 99, 1, ""); err == nil {
		t.Error("非法通道应被拒绝")
	}
}

func TestDigest(t *testing.T) {
	if Digest("a", "bc") == Digest("ab", "c") {
		t.Error("标题长度未参与摘要，(" + "a,bc" + ") 与 (" + "ab,c" + ") 撞车")
	}
	got := Digest("标题", "正文")
	if got != Digest("标题", "正文") {
		t.Error("摘要不稳定，无法用于回执比对")
	}
	if len(got) != 64 {
		t.Errorf("digest 长度 = %d, want 64", len(got))
	}
	if strings.Contains(got, " ") {
		t.Errorf("digest 必须是 hex: %q", got)
	}
}

func TestRecipientKey(t *testing.T) {
	if got := RecipientKey(7, "ignored"); got != "mid:7" {
		t.Errorf("mid 优先: %q", got)
	}
	if got := RecipientKey(0, " device-abc "); got != "ref:device-abc" {
		t.Errorf("回落 target_ref: %q", got)
	}
	if got := RecipientKey(0, "   "); got != "anonymous" {
		t.Errorf("两者皆空应为 anonymous: %q", got)
	}
	if got := RecipientKey(0, strings.Repeat("x", 200)); len(got) > 132 {
		t.Errorf("超长 ref 需截断: %d", len(got))
	}
}
