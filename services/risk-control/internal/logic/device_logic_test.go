package logic

// device_logic_test.go 覆盖设备画像域两个入口：UpsertDeviceProfile / GetDeviceProfile。
//
// risk_device_profile 是本服务里唯一带 PII 边界的表（设备号原文只允许存在 SHA-256 的那一瞬间），
// 同时 related_mid_count / risk_score 又都是「别的写入口负责的事实投影」，
// 所以断言集中在四类：
//  1. 唯一键 = uniq_device_hash(device_hash)（迁移 SQL 000002:57，列是 CHAR(64)）：
//     反复写入必须命中同一行，且 first_seen 取 LEAST、last_seen 取 GREATEST，
//     UPSERT 语句**不含** related_mid_count（model/device.go:121-123）——
//     否则一次「补个标签」就会把关联账号数清零，device_mid_count 指标跟着失真。
//  2. 列宽：labels VARCHAR(512)（同文件 :47）、source VARCHAR(32)（:52）。
//     本轮为此改生产代码两处：
//       - source 原由 sanitizeShortString 裁断到 32 字节，"login-<尾巴>" 会被裁成合法的
//         "login"（伪造审计来源字段），现改为拒绝（upsertdeviceprofilelogic.go 的 maxProfileSourceLen）；
//       - labels 是「库里已有 + 本次新增」合并后的串，logic 层的「≤20 条 × ≤32 字节」根本框不住它，
//         超 512 字节时非严格模式会静默截断在某个标签中间，现改为在写前拒绝
//         （repository.go 的 maxLabelsColumnLen）。
//  3. 风险分语义：>=0 覆盖、>100 收敛到 100、<0（含 -1 哨兵）不修改；新建时负数落到 0。
//     proto3 无法区分「未传」与 0 ⇒ 只改标签的请求会把 100 分设备清零，
//     本轮钉住现状并记候选缺口 #23（文档已写明 ">=0 覆盖"，改默认值等于改契约）。
//  4. 读侧不伪造：画像不存在 ⇒ found=false 且 Profile 为 nil（不是零值画像、不是错误）；
//     读接口不碰 Redis，也不回传设备号原文。
//
// 替身与真实 SQL 的一处差异要如实说明：model/device.go:135 在 Upsert 之后还会发一条
// `FindOne` 读回整行，而 fakeDeviceModel.Upsert 直接返回内存行副本，
// 所以这里的写入序列只有一条 `device.Upsert:*`，读回值本身与真实实现一致。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go-video/services/risk-control/model"
	"go-video/services/risk-control/rpc"
)

// devReq 是一次「网关登录回写」形态的画像写入（不带 mid、不带 risk_score）。
func devReq() *rpc.UpsertDeviceProfileReq {
	return &rpc.UpsertDeviceProfileReq{
		DeviceId: rawDeviceSN,
		Source:   "gateway",
	}
}

func upsertDevice(t *testing.T, st *store, in *rpc.UpsertDeviceProfileReq) (*rpc.UpsertDeviceProfileReply, error) {
	t.Helper()
	return NewUpsertDeviceProfileLogic(context.Background(), st.svcCtx).UpsertDeviceProfile(in)
}

func getDevice(t *testing.T, st *store, in *rpc.GetDeviceProfileReq) (*rpc.GetDeviceProfileReply, error) {
	t.Helper()
	return NewGetDeviceProfileLogic(context.Background(), st.svcCtx).GetDeviceProfile(in)
}

func devFindOp(hash string) string { return "device.FindOne:" + hash }

func devUpsertOp(hash string) string { return "device.Upsert:" + hash }

func relAddOp(hash string, mid int64) string { return fmt.Sprintf("mid.AddRelation:%s=%d", hash, mid) }

func relCountOp(hash string) string { return "mid.CountByDevice:" + hash }

func devSetCountOp(hash string, n int64) string {
	return fmt.Sprintf("device.UpdateRelatedCount:%s=%d", hash, n)
}

// devWriteSeq 是不带 mid 的写入序列；带 mid 时后面接关联登记 + 重算 + 写回投影三步。
func devWriteSeq(hash string) []string { return []string{devFindOp(hash), devUpsertOp(hash)} }

func devWriteWithMid(hash string, mid, count int64) []string {
	return append(devWriteSeq(hash), relAddOp(hash, mid), relCountOp(hash), devSetCountOp(hash, count))
}

// paddedLabels 生成 n 个互不相同、每个恰好 32 字节的标签（列宽边界用例用）。
func paddedLabels(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("%02d%s", i, strings.Repeat("x", 30)))
	}
	return out
}

func joinedLen(n int) int { return 32*n + (n - 1) }

// deviceRow 读回库存画像行。
func (st *store) deviceRow(hash string) (model.RiskDeviceProfile, bool) {
	row, ok := st.device.rows[hash]
	if !ok {
		return model.RiskDeviceProfile{}, false
	}
	return *row, true
}

// deviceRowsDump 把库存画像行逐个解引用后拼成文本（隐私断言要看字段值，不是指针地址）。
func deviceRowsDump(st *store) string {
	parts := make([]string, 0, len(st.device.rows))
	for _, row := range st.device.rows {
		parts = append(parts, fmt.Sprintf("%+v", *row))
	}
	return strings.Join(parts, "|")
}

// --- UpsertDeviceProfile ---

func TestUpsertDeviceProfileCreateMergesLabelsAndClampsScore(t *testing.T) {
	st := newStore(t)
	before := time.Now().Unix()

	reply, err := upsertDevice(t, st, devReq())
	wantNoErr(t, "新建画像", err)
	hash := devHash()
	wantOps(t, "新建序列", st.log.all(), devWriteSeq(hash))
	wantEQ(t, "新建", "created", reply.Created, true)
	wantEQ(t, "新建", "device_hash 是摘要不是原文", reply.Profile.GetDeviceHash(), hash)
	wantEQ(t, "新建", "related_mid_count（UPSERT 不含此列）", reply.Profile.GetRelatedMidCount(), int64(0))
	// 不传 risk_score（proto3 零值）即「覆盖成 0」：文档写的是 ">=0 覆盖"，这里钉住读起来别扭但合法的口径。
	wantEQ(t, "新建", "risk_score 零值即覆盖", reply.Profile.GetRiskScore(), int32(0))
	after := time.Now().Unix()
	wantRangeInt64(t, "新建", "first_seen", reply.Profile.GetFirstSeen(), before, after)
	wantEQ(t, "新建", "last_seen==first_seen（新建）", reply.Profile.GetLastSeen(), reply.Profile.GetFirstSeen())
	wantRangeInt64(t, "新建", "ctime", reply.Profile.GetCtime(), before, after)
	if n := len(reply.Profile.GetLabels()); n != 0 {
		t.Errorf("新建画像凭空多出标签：%d 条 %v", n, reply.Profile.GetLabels())
	}

	row, ok := st.deviceRow(hash)
	if !ok {
		t.Fatalf("库存里没有画像行：%s", deviceRowsDump(st))
	}
	wantEQ(t, "库存行", "rows 总数", len(st.device.rows), 1)
	wantEQ(t, "库存行", "source", row.Source, "gateway")
	wantEQ(t, "库存行", "operator（系统写入允许 0）", row.Operator, int64(0))
	// 设备号原文不得出现在任何一列。
	assertNoRawPII(t, "设备画像", deviceRowsDump(st))

	// 标签合并：去重、去空、字典序（model/device.go 的 MergeLabels 保证同集合恒定同串）。
	second := devReq()
	second.Labels = []string{"zeta", "alpha", "alpha", "  ", "mid"}
	merged, err := upsertDevice(t, st, second)
	wantNoErr(t, "补标签", err)
	wantEQ(t, "合并标签", "created（同一行更新）", merged.Created, false)
	wantEQ(t, "合并标签", "labels 条数", len(merged.Profile.GetLabels()), 3)
	wantEQ(t, "合并标签", "labels 字典序去重", strings.Join(merged.Profile.GetLabels(), ","), "alpha,mid,zeta")
	wantEQ(t, "合并标签", "rows 总数（没多出行）", len(st.device.rows), 1)
	wantEQ(t, "合并标签", "自增序号", st.device.next, int64(1))

	// 风险分：>100 收敛到 100；负数是「不修改」的哨兵。
	high := devReq()
	high.RiskScore = 999
	h, err := upsertDevice(t, st, high)
	wantNoErr(t, "越界风险分", err)
	wantEQ(t, "风险分收敛", "999 ⇒ 100", h.Profile.GetRiskScore(), int32(100))
	keep := devReq()
	keep.RiskScore = -1
	k, err := upsertDevice(t, st, keep)
	wantNoErr(t, "保留风险分", err)
	wantEQ(t, "风险分收敛", "-1 ⇒ 保持 100", k.Profile.GetRiskScore(), int32(100))
	drop := devReq()
	drop.RiskScore = -5000 // 任何负数都是「不改」，不会因为小于 SMALLINT 下界而落库
	d, err := upsertDevice(t, st, drop)
	wantNoErr(t, "极端负数", err)
	wantEQ(t, "风险分收敛", "极负 ⇒ 仍是 100", d.Profile.GetRiskScore(), int32(100))
	row, _ = st.deviceRow(hash)
	wantEQ(t, "库存行", "risk_score 落库值", row.RiskScore, int32(100))

	// 候选缺口 #23 现状：只改标签、忘了带 risk_score 的调用方会把 100 分设备清零。
	// proto3 无 optional，-1 才是「不修改」；文档写了但无法自动归一（0 是合法分值）。
	blind := devReq()
	blind.Labels = []string{"just-a-label"}
	z, err := upsertDevice(t, st, blind)
	wantNoErr(t, "漏传 risk_score 的更新", err)
	wantEQ(t, "候选缺口 #23 现状", "风险分被清零", z.Profile.GetRiskScore(), int32(0))
	wantEQ(t, "候选缺口 #23 现状", "标签仍合并", strings.Join(z.Profile.GetLabels(), ","), "alpha,just-a-label,mid,zeta")
}

// TestUpsertDeviceProfileKeepsSeenWindowsAndRelatedCount 钉住 ON DUPLICATE 的列清单：
// first_seen 只前进到更早、last_seen 只前进到更晚、related_mid_count 压根不在更新列里。
func TestUpsertDeviceProfileKeepsSeenWindowsAndRelatedCount(t *testing.T) {
	st := newStore(t)
	hash := devHash()
	now := time.Now().Unix()
	firstSeen := now - 7200
	lastSeen := now + 3600 // 未来值：GREATEST 必须保留它，不能被本次写入的 now 回退
	st.seedDevice(model.RiskDeviceProfile{DeviceHash: hash, Labels: "old",
		FirstSeen: firstSeen, LastSeen: lastSeen, RelatedMidCount: 7, RiskScore: 40,
		Source: "login", Ctime: firstSeen, Mtime: firstSeen})

	upd := devReq()
	upd.Labels = []string{"new"}
	upd.RiskScore = 55
	upd.Source = "system"
	reply, err := upsertDevice(t, st, upd)
	wantNoErr(t, "更新画像", err)
	wantOps(t, "更新序列", st.log.opsFrom(0), devWriteSeq(hash))
	wantEQ(t, "seen 窗口", "first_seen 保留最早值", reply.Profile.GetFirstSeen(), firstSeen)
	wantEQ(t, "seen 窗口", "last_seen 不被未来值回退", reply.Profile.GetLastSeen(), lastSeen)
	wantEQ(t, "seen 窗口", "related_mid_count 不被 UPSERT 清零", reply.Profile.GetRelatedMidCount(), int64(7))
	wantEQ(t, "seen 窗口", "risk_score 覆盖", reply.Profile.GetRiskScore(), int32(55))
	wantEQ(t, "seen 窗口", "labels 合并", strings.Join(reply.Profile.GetLabels(), ","), "new,old")

	// mid>0 才登记关联：AddRelation(INSERT IGNORE) → CountByDevice → UpdateRelatedCount 写回投影。
	withMid := devReq()
	withMid.Mid = 13
	withMid.RiskScore = -1
	st.seedRelation(hash, 11, 12)
	m, err := upsertDevice(t, st, withMid)
	wantNoErr(t, "登记关联", err)
	wantOps(t, "登记关联序列", st.log.opsFrom(2), devWriteWithMid(hash, 13, 3))
	wantEQ(t, "登记关联", "relation_added", m.RelationAdded, true)
	wantEQ(t, "登记关联", "related_mid_count 由关联表重算", m.Profile.GetRelatedMidCount(), int64(3))
	wantEQ(t, "登记关联", "risk_score 走哨兵未被改动", m.Profile.GetRiskScore(), int32(55))
	row, _ := st.deviceRow(hash)
	wantEQ(t, "登记关联落库", "related_mid_count", row.RelatedMidCount, int64(3))

	// 同一账号重复登录：INSERT IGNORE 影响 0 行 ⇒ relation_added=false，计数不变（幂等）。
	again, err := upsertDevice(t, st, withMid)
	wantNoErr(t, "重复登记", err)
	wantOps(t, "重复登记序列", st.log.opsFrom(7), devWriteWithMid(hash, 13, 3))
	wantEQ(t, "重复登记", "relation_added=false", again.RelationAdded, false)
	wantEQ(t, "重复登记", "related_mid_count 不翻倍", again.Profile.GetRelatedMidCount(), int64(3))
	wantEQ(t, "重复登记爆炸半径", "rows 总数", len(st.device.rows), 1)
	wantEQ(t, "重复登记爆炸半径", "关联表条数", int64(len(st.mid.rels[hash])), int64(3))

	// mid<=0 不登记、也不清零已有投影（读侧 device_mid_count 指标的事实源是关联表，不是这一列）。
	noMid := devReq()
	noMid.Mid = 0
	n, err := upsertDevice(t, st, noMid)
	wantNoErr(t, "无 mid 的写入", err)
	wantOps(t, "无 mid 序列", st.log.opsFrom(12), devWriteSeq(hash))
	wantEQ(t, "无 mid", "related_mid_count 保持", n.Profile.GetRelatedMidCount(), int64(3))
	wantEQ(t, "无 mid", "relation_added=false", n.RelationAdded, false)
}

func TestUpsertDeviceProfileLabelsOverflowIsRejectedNotTruncated(t *testing.T) {
	// labels VARCHAR(512)：15 个 32 字节标签 = 494 字节仍在列内，再加一个就是 527 字节。
	// 非严格模式下 MySQL 会静默截断在某个标签中间，下次 splitLabels 读回半个标签名 ——
	// 风险标签被改写成一个不存在的标签，所以必须拒绝（repository.go 的 maxLabelsColumnLen）。
	st := newStore(t)
	hash := devHash()
	st.seedDevice(model.RiskDeviceProfile{DeviceHash: hash, Labels: strings.Join(paddedLabels(15), ","),
		FirstSeen: 1, LastSeen: 2, RelatedMidCount: 1, RiskScore: 30, Source: "system"})
	before := st.log.snapshot()

	full := devReq()
	full.Labels = paddedLabels(16)[15:] // 第 16 个：合并后 527 字节
	_, err := upsertDevice(t, st, full)
	wantErrIs(t, "标签合并后超列宽", err, model.ErrInvalidTarget)
	if !strings.Contains(err.Error(), "labels exceed 512 bytes") {
		t.Errorf("错误文本未说明列宽：%v", err)
	}
	// 前置读发生了（合并才知道总长度），但写一条都不能有。
	wantOps(t, "超宽序列", st.log.opsFrom(before), []string{devFindOp(hash)})
	wantEQ(t, "超宽爆炸半径", "UPSERT 次数", st.log.countPrefixFrom(before, "device.Upsert"), 0)
	row, _ := st.deviceRow(hash)
	wantEQ(t, "超宽爆炸半径", "库内标签未被改写", row.Labels, strings.Join(paddedLabels(15), ","))
	wantEQ(t, "超宽爆炸半径", "库内风险分未被改写", row.RiskScore, int32(30))
	wantEQ(t, "超宽爆炸半径", "mtime 未被刷新", row.Mtime, int64(0))

	// 边界：合并后恰好落在列宽内（15 个共 494 字节）必须放行。
	ok := newStore(t)
	ok.seedDevice(model.RiskDeviceProfile{DeviceHash: hash, Labels: strings.Join(paddedLabels(14), ","),
		FirstSeen: 1, LastSeen: 2, RiskScore: 30, Source: "system"})
	got, err := upsertDevice(t, ok, func() *rpc.UpsertDeviceProfileReq {
		r := devReq()
		r.Labels = paddedLabels(15)[14:]
		r.RiskScore = -1
		return r
	}())
	wantNoErr(t, "列宽边界", err)
	joined := strings.Join(got.Profile.GetLabels(), ",")
	wantEQ(t, "列宽边界", "合并后字节数", len(joined), joinedLen(15))
	if len(joined) > 512 { // 512 = repository.maxLabelsColumnLen，与 labels VARCHAR(512) 同宽
		t.Errorf("列宽边界：合并后 %d 字节已越过 VARCHAR(512)", len(joined))
	}

	// 单标签超 32 字节：MergeLabels 刻意丢弃而不是报错（model/device.go:68-70，
	// 避免运营批量写入被单个脏值打断）。钉住「丢弃 + 响应如实回显剩余标签」。
	drop := newStore(t)
	d := devReq()
	d.Labels = []string{"good", strings.Repeat("L", 33)}
	gotDrop, err := upsertDevice(t, drop, d)
	wantNoErr(t, "超宽单标签", err)
	wantEQ(t, "超宽单标签", "被丢弃后只剩一条", strings.Join(gotDrop.Profile.GetLabels(), ","), "good")
	gotDrop2, err := upsertDevice(t, drop, func() *rpc.UpsertDeviceProfileReq {
		r := devReq()
		r.Labels = []string{strings.Repeat("E", 32)} // 恰好 32 字节：合法
		r.RiskScore = -1
		return r
	}())
	wantNoErr(t, "32 字节标签边界", err)
	wantEQ(t, "32 字节标签边界", "整条 32 字节标签原样入库",
		strings.Join(gotDrop2.Profile.GetLabels(), ","), strings.Repeat("E", 32)+",good")
}

func TestUpsertDeviceProfileGuardsRunBeforeAnyDependency(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(in *rpc.UpsertDeviceProfileReq)
		want   error
		wantIn string
	}{
		{"无设备标识", func(in *rpc.UpsertDeviceProfileReq) { in.DeviceId = "" }, model.ErrEmptyDeviceID, "device id required"},
		{"设备号只有空白", func(in *rpc.UpsertDeviceProfileReq) { in.DeviceId = "   " }, model.ErrEmptyDeviceID, "device id required"},
		{"device_hash 是裸 IPv4（仍算缺设备标识）", func(in *rpc.UpsertDeviceProfileReq) {
			in.DeviceId = ""
			in.DeviceHash = rawIPv4
		}, model.ErrEmptyDeviceID, "device id required"},
		{"device_hash 非十六进制", func(in *rpc.UpsertDeviceProfileReq) {
			in.DeviceId = ""
			in.DeviceHash = "zzzzzzzzzzzzzzzz"
		}, model.ErrEmptyDeviceID, "device id required"},
		{"device_hash 过短", func(in *rpc.UpsertDeviceProfileReq) {
			in.DeviceId = ""
			in.DeviceHash = "abc123"
		}, model.ErrEmptyDeviceID, "device id required"},
		{"未知来源", func(in *rpc.UpsertDeviceProfileReq) { in.Source = "crawler" }, model.ErrInvalidTarget, "source=crawler not allowed"},
		{"来源超长（裁断即伪装）", func(in *rpc.UpsertDeviceProfileReq) {
			in.Source = "login-" + strings.Repeat("x", 30) // 裁到 32 字节就成了合法的 "login"
		}, model.ErrInvalidTarget, "source too long"},
		{"人工来源无操作人", func(in *rpc.UpsertDeviceProfileReq) {
			in.Source = "operation"
			in.Operator = 0
		}, model.ErrOperatorRequired, "operator required"},
		{"人工来源操作人为负", func(in *rpc.UpsertDeviceProfileReq) {
			in.Source = "operation"
			in.Operator = -3
		}, model.ErrOperatorRequired, "operator required"},
		{"标签条数超限", func(in *rpc.UpsertDeviceProfileReq) { in.Labels = paddedLabels(21) }, model.ErrInvalidTarget, "too many labels, max 20"},
	}
	for _, tc := range cases {
		st := newStore(t)
		in := devReq()
		tc.mutate(in)
		reply, err := upsertDevice(t, st, in)
		wantErrIs(t, tc.name, err, tc.want)
		if !strings.Contains(err.Error(), tc.wantIn) {
			t.Errorf("%s：错误文本 %q 未包含 %q", tc.name, err.Error(), tc.wantIn)
		}
		wantNoCall(t, tc.name, st, 0) // 守卫先于一切依赖：不查库、不写库、不碰 Redis
		wantEQ(t, tc.name, "rows 总数", len(st.device.rows), 0)
		if reply != nil {
			t.Errorf("%s：仍返回响应体 %+v", tc.name, reply)
		}
	}

	// 边界一：条数守卫的上界是 20，但 20 条 32 字节标签合并后 659 字节 > 512，
	// 实际拦下它的是列宽守卫 —— 两道守卫各管一段，报错文本必须能区分。
	ok := newStore(t)
	okSnap := ok.log.snapshot()
	got, err := upsertDevice(t, ok, func() *rpc.UpsertDeviceProfileReq {
		r := devReq()
		r.Labels = paddedLabels(20) // 21 条才会撞条数守卫
		return r
	}())
	wantErrIs(t, "20 条超宽标签", err, model.ErrInvalidTarget)
	if got != nil {
		t.Errorf("超宽标签仍返回响应体 %+v", got)
	}
	if !strings.Contains(err.Error(), "labels exceed") {
		t.Errorf("20 条 32 字节标签应落在列宽守卫上，实际：%v", err)
	}
	// 列宽守卫发生在合并之后、写库之前：只有那一次前置读。
	wantOps(t, "20 条标签序列", ok.log.opsFrom(okSnap), []string{devFindOp(devHash())})

	// 少五条（15 条 = 494 字节）就落库成功：证明上面拦下的确实是宽度而不是条数。
	narrow := newStore(t)
	less := devReq()
	less.Labels = paddedLabels(15)
	res, err := upsertDevice(t, narrow, less)
	wantNoErr(t, "15 条 32 字节标签", err)
	wantEQ(t, "条数/宽度分界", "全部入库", len(res.Profile.GetLabels()), 15)
	row, _ := narrow.deviceRow(devHash())
	wantEQ(t, "条数/宽度分界", "库内标签未被裁断", len(row.Labels), joinedLen(15))

	// 边界二：来源长度守卫是「>32 字节才拒绝」。四个允许的来源都不足 32 字节，
	// 所以用成员校验的报错来证明长度守卫没有误伤 32 字节：
	// 恰好 32 字节的未知来源报 not allowed（说明它穿过了长度守卫），33 字节报 too long。
	guard := newStore(t)
	guardSnap := guard.log.snapshot()
	at32 := devReq()
	at32.Source = "login" + strings.Repeat("y", 27) // 恰好 32 字节
	if _, err := upsertDevice(t, guard, at32); err == nil {
		t.Fatalf("未登记的来源 %q 未被拒绝", at32.Source)
	} else {
		wantErrIs(t, "32 字节未知来源", err, model.ErrInvalidTarget)
		if !strings.Contains(err.Error(), "not allowed") {
			t.Errorf("32 字节来源应穿过长度守卫、落在成员校验上，实际：%v", err)
		}
	}
	over := devReq()
	over.Source = "login" + strings.Repeat("y", 28) // 33 字节
	if _, err := upsertDevice(t, guard, over); err == nil {
		t.Fatalf("33 字节来源未被拒绝")
	} else {
		wantErrIs(t, "33 字节来源", err, model.ErrInvalidTarget)
		if !strings.Contains(err.Error(), "source too long") {
			t.Errorf("33 字节来源应落在长度守卫上（而不是被裁成合法值），实际：%v", err)
		}
	}
	wantNoCall(t, "来源守卫先于一切依赖", guard, guardSnap)

	// operation 来源带 operator：允许，并且审计字段落库。
	op := newStore(t)
	human := devReq()
	human.Source = "operation"
	human.Operator = testOperator
	human.Labels = []string{"manual-tag"}
	if _, err := upsertDevice(t, op, human); err != nil {
		t.Fatalf("人工写入被拒：%v", err)
	}
	row, _ = op.deviceRow(devHash())
	wantEQ(t, "人工写入", "operator 落库", row.Operator, testOperator)
	wantEQ(t, "人工写入", "source 落库", row.Source, "operation")
}

func TestUpsertDeviceProfileFailurePropagates(t *testing.T) {
	hash := devHash()

	// 前置读失败：一行都没写，错误不被伪装成「设备不存在」。
	st := newStore(t)
	boom := errors.New("boom-device-findone")
	st.device.failWith("FindOne", boom)
	_, err := upsertDevice(t, st, devReq())
	wantErrIs(t, "前置读故障", err, boom)
	if errors.Is(err, model.ErrDeviceNotFound) || errors.Is(err, model.ErrEmptyDeviceID) {
		t.Errorf("前置读故障被伪装成业务错误：%v", err)
	}
	wantOps(t, "前置读故障序列", st.log.all(), []string{devFindOp(hash)})
	wantEQ(t, "前置读故障爆炸半径", "rows 总数", len(st.device.rows), 0)

	// 画像写入失败：关联登记不能抢跑。
	st.device.clear("FindOne")
	writeBoom := errors.New("boom-device-upsert")
	st.device.failWith("Upsert", writeBoom)
	reply, err := upsertDevice(t, st, func() *rpc.UpsertDeviceProfileReq {
		r := devReq()
		r.Mid = 13
		return r
	}())
	wantErrIs(t, "画像写入故障", err, writeBoom)
	if reply != nil {
		t.Errorf("画像写入故障：仍返回响应体 %+v", reply)
	}
	wantOps(t, "画像写入故障序列", st.log.opsFrom(1), []string{devFindOp(hash), devUpsertOp(hash)})
	wantEQ(t, "画像写入故障爆炸半径", "rows 总数", len(st.device.rows), 0)
	wantEQ(t, "画像写入故障爆炸半径", "关联表未被写入", st.log.countPrefix("mid."), 0)

	// 画像已落库、关联登记失败：错误必须上抛，同时库里那一行是既成事实（爆炸半径要说清楚）。
	st.device.clear("Upsert")
	relBoom := errors.New("boom-mid-addrelation")
	st.mid.failWith("AddRelation", relBoom)
	_, err = upsertDevice(t, st, func() *rpc.UpsertDeviceProfileReq {
		r := devReq()
		r.Mid = 13
		r.RiskScore = 66
		return r
	}())
	wantErrIs(t, "关联登记故障", err, relBoom)
	wantOps(t, "关联登记故障序列", st.log.opsFrom(3),
		[]string{devFindOp(hash), devUpsertOp(hash), relAddOp(hash, 13)})
	row, ok := st.deviceRow(hash)
	if !ok {
		t.Fatalf("画像行应已落库：%s", deviceRowsDump(st))
	}
	wantEQ(t, "关联登记故障爆炸半径", "画像已写入", row.RiskScore, int32(66))
	wantEQ(t, "关联登记故障爆炸半径", "投影仍是旧值（未被半路改写）", row.RelatedMidCount, int64(0))

	// 关联已建、重算/写回失败：事实源与投影可以短暂不一致，但错误不能被吞。
	st.mid.clear("AddRelation")
	countBoom := errors.New("boom-mid-count")
	st.mid.failWith("CountByDevice", countBoom)
	if _, err := upsertDevice(t, st, func() *rpc.UpsertDeviceProfileReq {
		r := devReq()
		r.Mid = 13
		r.RiskScore = -1
		return r
	}()); err == nil {
		t.Fatalf("重算故障被吞掉了")
	} else {
		wantErrIs(t, "重算故障", err, countBoom)
	}
	wantOps(t, "重算故障序列", st.log.opsFrom(6),
		[]string{devFindOp(hash), devUpsertOp(hash), relAddOp(hash, 13), relCountOp(hash)})
	wantEQ(t, "重算故障爆炸半径", "关联已登记", int64(len(st.mid.rels[hash])), int64(1))
	wantEQ(t, "重算故障爆炸半径", "写回投影未发生", st.log.countPrefix("device.UpdateRelatedCount"), 0)

	st.mid.clear("CountByDevice")
	setBoom := errors.New("boom-device-setcount")
	st.device.failWith("UpdateRelatedCount", setBoom)
	if _, err := upsertDevice(t, st, func() *rpc.UpsertDeviceProfileReq {
		r := devReq()
		r.Mid = 14
		r.RiskScore = -1
		return r
	}()); err == nil {
		t.Fatalf("写回投影故障被吞掉了")
	} else {
		wantErrIs(t, "写回投影故障", err, setBoom)
	}
	wantEQ(t, "写回投影故障爆炸半径", "第二个关联仍登记成功", int64(len(st.mid.rels[hash])), int64(2))
	row, _ = st.deviceRow(hash)
	wantEQ(t, "写回投影故障爆炸半径", "投影未刷新", row.RelatedMidCount, int64(0))

	// 故障恢复后重试：幂等，画像还是一行。
	st.device.clear("UpdateRelatedCount")
	got, err := upsertDevice(t, st, func() *rpc.UpsertDeviceProfileReq {
		r := devReq()
		r.Mid = 14
		r.RiskScore = -1
		return r
	}())
	wantNoErr(t, "恢复后重试", err)
	wantEQ(t, "恢复后重试", "created（不新建行）", got.Created, false)
	wantEQ(t, "恢复后重试", "related_mid_count 追平", got.Profile.GetRelatedMidCount(), int64(2))
	wantEQ(t, "恢复后重试", "rows 总数", len(st.device.rows), 1)
	// 画像读写都不该碰 Redis（设备分与关联数每次都回源）。
	if n := st.log.countPrefix("redis."); n != 0 {
		t.Errorf("画像接口触碰 Redis %d 次（应为 0）：%v", n, st.log.all())
	}
}

// --- GetDeviceProfile ---

func TestGetDeviceProfileReturnsStoredTruthOrNotFound(t *testing.T) {
	st := newStore(t)
	hash := devHash()
	now := time.Now().Unix()
	st.seedDevice(model.RiskDeviceProfile{ID: 9, DeviceHash: hash, Labels: "beta,alpha",
		FirstSeen: now - 100, LastSeen: now, RelatedMidCount: 4, RiskScore: 77,
		Source: "operation", Operator: testOperator, Ctime: now - 100, Mtime: now})

	got, err := getDevice(t, st, &rpc.GetDeviceProfileReq{DeviceId: rawDeviceSN})
	wantNoErr(t, "查询画像", err)
	wantOps(t, "查询序列", st.log.all(), []string{devFindOp(hash)})
	wantEQ(t, "查询画像", "found", got.Found, true)
	wantEQ(t, "查询画像", "device_hash", got.Profile.GetDeviceHash(), hash)
	wantEQ(t, "查询画像", "labels 字典序回显", strings.Join(got.Profile.GetLabels(), ","), "alpha,beta")
	wantEQ(t, "查询画像", "risk_score", got.Profile.GetRiskScore(), int32(77))
	wantEQ(t, "查询画像", "first_seen", got.Profile.GetFirstSeen(), now-100)
	wantEQ(t, "查询画像", "last_seen", got.Profile.GetLastSeen(), now)
	wantEQ(t, "查询画像", "related_mid_count", got.Profile.GetRelatedMidCount(), int64(4))
	wantEQ(t, "查询画像", "ctime", got.Profile.GetCtime(), now-100)
	wantEQ(t, "查询画像", "mtime", got.Profile.GetMtime(), now)
	// 画像原文不出网：DeviceProfile 里没有 source/operator 字段，响应文本也不许出现设备号原文。
	// （proto 消息的 %v 走生成的 String()，打印的是文本格式的字段值，不是指针地址。）
	if got.Profile == nil {
		t.Fatalf("found=true 却没带回画像：%+v", got)
	}
	assertNoRawPII(t, "画像响应", fmt.Sprintf("%+v", got.Profile))

	// 不存在 ⇒ found=false、Profile 为 nil：既不伪造一条合法画像，也不把它当调用方错误。
	missing, err := getDevice(t, st, &rpc.GetDeviceProfileReq{DeviceId: "never-seen-device"})
	wantNoErr(t, "未登记的设备", err)
	wantEQ(t, "未登记", "found", missing.Found, false)
	if missing.Profile != nil {
		t.Errorf("未登记设备被伪造出画像：%+v", missing.Profile)
	}
	wantOps(t, "未登记序列", st.log.opsFrom(1), []string{devFindOp(model.DeviceHash("never-seen-device"))})
	wantEQ(t, "未登记爆炸半径", "rows 总数（读接口不写库）", len(st.device.rows), 1)

	// device_id 优先于 device_hash：两个都给时必须按 device_id 现算的摘要查。
	other := model.DeviceHash("another-device-sn")
	prec, err := getDevice(t, st, &rpc.GetDeviceProfileReq{DeviceId: rawDeviceSN, DeviceHash: other})
	wantNoErr(t, "两者优先级", err)
	wantOps(t, "两者优先级序列", st.log.opsFrom(2), []string{devFindOp(hash)})
	wantEQ(t, "两者优先级", "found", prec.Found, true)

	// 只给已受控 ID（大写形态）：按小写规范化后检索，命中同一行 —— 运营后台按摘要粘贴即可查。
	upper, err := getDevice(t, st, &rpc.GetDeviceProfileReq{DeviceHash: strings.ToUpper(hash)})
	wantNoErr(t, "大写摘要检索", err)
	wantOps(t, "大写摘要序列", st.log.opsFrom(3), []string{devFindOp(hash)})
	wantEQ(t, "大写摘要检索", "found", upper.Found, true)

	// 空标识：守卫先于依赖。
	before := st.log.snapshot()
	if _, err := getDevice(t, st, &rpc.GetDeviceProfileReq{}); err == nil {
		t.Fatalf("空设备标识未被拒绝")
	} else {
		wantErrIs(t, "空设备标识", err, model.ErrEmptyDeviceID)
	}
	wantEQ(t, "空设备标识爆炸半径", "新增查询次数", st.log.countPrefixFrom(before, "device."), 0)
	// 裸 IP 当 device_hash：拒绝，且不提供「按 IP 反查画像」的能力。
	if _, err := getDevice(t, st, &rpc.GetDeviceProfileReq{DeviceHash: rawIPv4}); err == nil {
		t.Errorf("裸 IP 当 device_hash 未被拒绝")
	} else {
		wantErrIs(t, "裸 IP 摘要", err, model.ErrEmptyDeviceID)
	}

	// 查询故障原样上抛：静默返回 found=false 会让调用方以为「这设备没问题」。
	failed := newStore(t)
	failed.seedDevice(model.RiskDeviceProfile{DeviceHash: hash, RiskScore: 10, Source: "system"})
	boom := errors.New("boom-device-get")
	failed.device.failWith("FindOne", boom)
	reply, err := getDevice(t, failed, &rpc.GetDeviceProfileReq{DeviceId: rawDeviceSN})
	wantErrIs(t, "画像查询故障", err, boom)
	if reply != nil {
		t.Errorf("画像查询故障：仍返回响应体 %+v", reply)
	}
	if errors.Is(err, model.ErrDeviceNotFound) {
		t.Errorf("查询故障被伪装成「画像不存在」：%v", err)
	}
}
