package logic

// list_logic_test.go 覆盖名单域两个入口：UpsertListEntry / GetListEntries。
//
// risk_list 是本服务里唯一「不透明命中即拒」的安全控制：写进去的一条封禁如果不生效，
// 后果不是「少个功能」而是「攻击继续放行」，所以这里的断言集中在三类：
//  1. 唯一键与列宽：uniq_target = (list_type, target_type, target_value)（见
//     deploy/migrations/risk-control/000001_create_risk_control_decision_tables.sql:79），
//     target_value 是 VARCHAR(64)（同文件 :71）。规范化必须既「把同一实体的不同写法折回一行」
//     （mid 前导零、摘要大小写），又「不把两个实体折成一行」（黑/白名单同值必须两行、
//     超列宽必须拒绝而不是截断）—— 本轮为此改生产代码三处，见下方对应用例与文末缺陷清单。
//  2. 读侧口径：过滤条件必须逐字进 SQL 参数（不能取回后再筛）、分页有上限、
//     total=0 时不发第二条查询；已过期条目在运营审计视图里**必须**继续返回（与裁决读侧
//     FindActive 的 expire_at 过滤刻意不同，见 TestGetListEntriesKeepsExpiredRowsForAudit）。
//  3. 写侧语义：覆盖式 upsert 不换 ID、不动 ctime，只刷 reason/operator/expire_at/state/mtime；
//     主键与时间戳读回后再拼进期望序列。
//
// 替身与真实 SQL 的一处差异要如实说明：model/list.go:109 在 Upsert 之后还会发一条
// `FindOne` 读回整行，而 fakeListModel.Upsert 直接在内存里返回库存行副本，
// 所以这里的调用轨迹只有一条 `list.Upsert:*`，读回结果本身仍与真实实现一致（含 id/ctime）。
//
// 本轮发现、只钉住未修的缺陷（详见文件末尾清单与最终报告）：
//   - state 缺省语义：UpsertListEntryReq.state 是 proto3 int32，不传即 0=停用，
//     而 model/list.go:100 的 INSERT 显式绑定 state 列 ⇒ DDL 的 `DEFAULT 1` 永不生效。
//     与缺口 #12（处罚）不同，这里 0 是**合法值**（停用条目是唯一的下架手段，本服务没有
//     删除名单的 RPC），无法自动归一 ⇒ 只钉行为、记候选缺口 #19。
//   - target_value 非摘要时错误信息误导（getlistentrieslogic.go:54-57 把「规范化失败」一律
//     说成 raw ip；写入侧 upsertlistentrylogic.go:54-57 的文案已区分，不受此缺口影响），
//     记候选缺口 #20。
//   - GetListEntriesReq.state 用 -1 表达「不过滤」，而 proto3 的零值是 0=只查停用条目
//     （getlistentrieslogic.go:42-45 原样透传），调用方漏传就得到空列表，记候选缺口 #21。

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"go-video/services/risk-control/internal/policy"
	"go-video/services/risk-control/model"
	"go-video/services/risk-control/rpc"
)

// listReq 是一条合法的黑名单写入请求（封账号 42，永久，立即生效）。
func listReq(tType rpc.TargetType, value string) *rpc.UpsertListEntryReq {
	return &rpc.UpsertListEntryReq{
		ListType:    rpc.ListType_LIST_TYPE_BLACK,
		TargetType:  tType,
		TargetValue: value,
		Reason:      "批量投稿搬运，人工确认",
		Operator:    testOperator,
		State:       model.StateEnabled,
	}
}

func upsertListEntry(t *testing.T, st *store, in *rpc.UpsertListEntryReq) (*rpc.UpsertListEntryReply, error) {
	t.Helper()
	return NewUpsertListEntryLogic(context.Background(), st.svcCtx).UpsertListEntry(in)
}

func getListEntries(t *testing.T, st *store, in *rpc.GetListEntriesReq) (*rpc.GetListEntriesReply, error) {
	t.Helper()
	return NewGetListEntriesLogic(context.Background(), st.svcCtx).GetListEntries(in)
}

func upsertOp(listType, targetType int32, value string) string {
	return fmt.Sprintf("list.Upsert:%d/%d/%s", listType, targetType, value)
}

// listOp 复现 fakeListModel.List 的调用轨迹（model/list.go:157 的签名顺序）：
// list.List:<名单类型>/<实体类型>/<哈希>/<状态>/<offset>/<limit>。
func listOp(listType, targetType int32, value string, state int32, offset, limit int) string {
	return fmt.Sprintf("list.List:%d/%d/%s/%d/%d/%d", listType, targetType, value, state, offset, limit)
}

func idsOfEntries(reply *rpc.GetListEntriesReply) []int64 {
	out := make([]int64, 0, len(reply.Entries))
	for _, e := range reply.Entries {
		out = append(out, e.GetId())
	}
	return out
}

// listRow 读回库存行（爆炸半径与「落库原值」断言用）。
func (st *store) listRow(listType, targetType int32, value string) (model.RiskList, bool) {
	row, ok := st.list.rows[listKey(listType, targetType, value)]
	if !ok {
		return model.RiskList{}, false
	}
	return *row, true
}

// listRowsDump 把库存行逐个解引用后拼成文本。
// 直接 fmt.Sprintf("%+v", st.list.rows) 打印的是 map[string]*RiskList 的地址，
// 隐私断言会变成永真式，所以这里必须落到字段值。
func listRowsDump(st *store) string {
	parts := make([]string, 0, len(st.list.rows))
	for _, row := range st.list.rows {
		parts = append(parts, fmt.Sprintf("%+v", *row))
	}
	return strings.Join(parts, "|")
}

// --- UpsertListEntry ---

func TestUpsertListEntryWritesRowAndReadsBackStoredValues(t *testing.T) {
	st := newStore(t)
	before := time.Now().Unix()

	reply, err := upsertListEntry(t, st, listReq(rpc.TargetType_TARGET_TYPE_MID, "42"))
	wantNoErr(t, "写入名单", err)
	wantOps(t, "写入序列", st.log.all(), []string{upsertOp(model.ListTypeBlack, model.TargetTypeMid, "42")})

	wantEQ(t, "写入", "created", reply.Created, true)
	wantEQ(t, "写入", "list_type", int32(reply.Entry.GetListType()), int32(model.ListTypeBlack))
	wantEQ(t, "写入", "target_type", int32(reply.Entry.GetTargetType()), int32(model.TargetTypeMid))
	wantEQ(t, "写入", "target_value", reply.Entry.GetTargetValue(), "42")
	wantEQ(t, "写入", "state", reply.Entry.GetState(), model.StateEnabled)
	wantEQ(t, "写入", "operator", reply.Entry.GetOperator(), testOperator)
	wantEQ(t, "写入", "reason", reply.Entry.GetReason(), "批量投稿搬运，人工确认")
	// 主键必须回填真实自增 ID（不是 0）：调用方要靠它做后续对账。
	if reply.Entry.GetId() == 0 {
		t.Fatalf("写入响应未回填自增主键：%+v", reply.Entry)
	}
	after := time.Now().Unix()
	// duration_seconds 未传 → 永久：expire_at 必须是 0，不能被算成「当前秒」。
	wantEQ(t, "写入", "expire_at（永久）", reply.Entry.GetExpireAt(), int64(0))
	wantRangeInt64(t, "写入", "ctime", reply.Entry.GetCtime(), before, after)
	wantEQ(t, "写入", "mtime==ctime（新建）", reply.Entry.GetMtime(), reply.Entry.GetCtime())

	row, ok := st.listRow(model.ListTypeBlack, model.TargetTypeMid, "42")
	if !ok {
		t.Fatalf("库存里没有落库行：%s", listRowsDump(st))
	}
	wantEQ(t, "库存行", "rows 总数", len(st.list.rows), 1)
	wantEQ(t, "库存行", "id（主键回填）", row.ID, reply.Entry.GetId())
	wantEQ(t, "库存行", "operator", row.Operator, testOperator)
	// 名单不做任何裁剪/转义：reason 原样入库（列宽由守卫把关）。
	wantEQ(t, "库存行", "reason", row.Reason, "批量投稿搬运，人工确认")
	// 名单不走缓存：任何 Redis 动作都是多余的（CheckAction 每次都回源读名单）。
	if n := st.log.countPrefix("redis."); n != 0 {
		t.Fatalf("写入名单触碰 Redis %d 次（应为 0）：%v", n, st.log.all())
	}

	// 限期条目：expire_at = 服务端当前秒 + duration（调用方不能指定绝对时间）。
	limited, err := upsertListEntry(t, st, func() *rpc.UpsertListEntryReq {
		r := listReq(rpc.TargetType_TARGET_TYPE_IP_HASH, ipHashHex)
		r.DurationSeconds = 600
		return r
	}())
	wantNoErr(t, "限期条目", err)
	lo, hi := before+600, after+600
	wantRangeInt64(t, "限期条目", "expire_at", limited.Entry.GetExpireAt(), lo, hi)
	wantEQ(t, "限期条目", "rows 总数", len(st.list.rows), 2)
	// 名单表本身是 PII 边界：target_value 只允许受控标识，落库行里不得出现设备号/IP 原文。
	assertNoRawPII(t, "名单条目", listRowsDump(st))
}

// TestUpsertListEntryNormalizesWithoutCollapsingDistinctEntities 钉住「规范化」这条窄缝：
// 同一实体的不同写法必须折回同一行（否则封禁漏一半），
// 不同实体绝不能被折成一行（否则后写覆盖前写的封禁）。
func TestUpsertListEntryNormalizesWithoutCollapsingDistinctEntities(t *testing.T) {
	// mid 前导零：裁决读侧用 strconv.FormatInt(mid,10) 组 target_value（repository/facts.go:112），
	// 放过 "042" 原样入库会得到一行永远命中不了的黑名单 —— 黑名单必须 fail-closed，
	// 故 NormalizeTargetValue 归一到最简十进制（model/identity.go 本轮修改）。
	st := newStore(t)
	first, err := upsertListEntry(t, st, listReq(rpc.TargetType_TARGET_TYPE_MID, " 042 "))
	wantNoErr(t, "带前导零的 mid", err)
	wantEQ(t, "前导零归一", "target_value", first.Entry.GetTargetValue(), "42")
	wantEQ(t, "前导零归一", "落库值", st.list.rows[listKey(model.ListTypeBlack, model.TargetTypeMid, "42")].TargetValue, "42")

	second, err := upsertListEntry(t, st, listReq(rpc.TargetType_TARGET_TYPE_MID, "42"))
	wantNoErr(t, "标准写法的同一账号", err)
	wantOps(t, "同一账号两次写入", st.log.opsFrom(1), []string{upsertOp(model.ListTypeBlack, model.TargetTypeMid, "42")})
	wantEQ(t, "同一账号两次写入", "created（第二次是覆盖）", second.Created, false)
	wantEQ(t, "同一账号两次写入", "id 不变", second.Entry.GetId(), first.Entry.GetId())
	wantEQ(t, "同一账号两次写入", "rows 总数（没有第二行）", len(st.list.rows), 1)

	// 全零：规范成单个 0，仍是惰性条目（mid<=0 不进查询），但不产生畸形键。
	zero, err := upsertListEntry(t, st, listReq(rpc.TargetType_TARGET_TYPE_MID, "0000"))
	wantNoErr(t, "全零 mid", err)
	wantEQ(t, "全零 mid", "target_value", zero.Entry.GetTargetValue(), "0")

	// 摘要大小写：十六进制不区分大小写，折回同一行是正确的（同一实体）。
	dev := newStore(t)
	upper := listReq(rpc.TargetType_TARGET_TYPE_DEVICE, strings.ToUpper(devHash()))
	lower := listReq(rpc.TargetType_TARGET_TYPE_DEVICE, devHash())
	u1, err := upsertListEntry(t, dev, upper)
	wantNoErr(t, "大写摘要", err)
	u2, err := upsertListEntry(t, dev, lower)
	wantNoErr(t, "小写摘要", err)
	wantEQ(t, "摘要大小写折叠", "rows 总数", len(dev.list.rows), 1)
	wantEQ(t, "摘要大小写折叠", "落库为小写", u1.Entry.GetTargetValue(), devHash())
	wantEQ(t, "摘要大小写折叠", "id 不变", u2.Entry.GetId(), u1.Entry.GetId())

	// 黑/白名单同值必须两行：唯一键含 list_type，裁决按「黑名单优先」分流。
	white := listReq(rpc.TargetType_TARGET_TYPE_MID, "42")
	white.ListType = rpc.ListType_LIST_TYPE_WHITE
	wReply, err := upsertListEntry(t, dev, white)
	wantNoErr(t, "同值白名单", err)
	wantEQ(t, "黑白共存", "created（不是覆盖黑名单那一行）", wReply.Created, true)
	wantEQ(t, "黑白共存", "rows 总数", len(dev.list.rows), 2)

	// 三种目标类型互不折叠：值相同也只在同一 (list_type,target_type) 下互相覆盖。
	typed := newStore(t)
	for _, tp := range []rpc.TargetType{rpc.TargetType_TARGET_TYPE_MID, rpc.TargetType_TARGET_TYPE_DEVICE, rpc.TargetType_TARGET_TYPE_IP_HASH} {
		// mid 用 "42"，device/ip 用 64 位十六进制：这里只验证 (target_type) 进唯一键。
		v := "42"
		if tp != rpc.TargetType_TARGET_TYPE_MID {
			v = devHash()
		}
		if _, err := upsertListEntry(t, typed, listReq(tp, v)); err != nil {
			t.Fatalf("目标类型 %d 写入失败：%v", tp, err)
		}
	}
	wantEQ(t, "目标类型进唯一键", "rows 总数", len(typed.list.rows), 3)
}

// TestUpsertListEntryOverwriteKeepsIdentity 钉住覆盖语义：换 ID 会让审计与外部引用断链。
func TestUpsertListEntryOverwriteKeepsIdentity(t *testing.T) {
	st := newStore(t)
	first, err := upsertListEntry(t, st, listReq(rpc.TargetType_TARGET_TYPE_MID, "77"))
	wantNoErr(t, "首次写入", err)
	createdCtime := first.Entry.GetCtime()
	id := first.Entry.GetId()

	time.Sleep(1100 * time.Millisecond) // 让 mtime 与 ctime 落在不同秒，断言才有分辨力
	over := listReq(rpc.TargetType_TARGET_TYPE_MID, "077")
	over.Reason = "补充证据：搬运团伙"
	over.Operator = 8802
	over.DurationSeconds = 120
	over.State = model.StateDisabled
	second, err := upsertListEntry(t, st, over)
	wantNoErr(t, "覆盖写入", err)
	wantEQ(t, "覆盖", "created", second.Created, false)
	wantEQ(t, "覆盖", "id 不变（就地更新，不新建行）", second.Entry.GetId(), id)
	wantEQ(t, "覆盖", "ctime 保持首次值", second.Entry.GetCtime(), createdCtime)
	if second.Entry.GetMtime() <= createdCtime {
		t.Errorf("覆盖：mtime=%d 未刷新（ctime=%d）", second.Entry.GetMtime(), createdCtime)
	}
	wantEQ(t, "覆盖", "reason 被替换", second.Entry.GetReason(), "补充证据：搬运团伙")
	wantEQ(t, "覆盖", "operator 被替换", second.Entry.GetOperator(), int64(8802))
	wantEQ(t, "覆盖", "state 被替换", second.Entry.GetState(), model.StateDisabled)
	wantEQ(t, "覆盖", "rows 总数", len(st.list.rows), 1)

	row, _ := st.listRow(model.ListTypeBlack, model.TargetTypeMid, "77")
	wantEQ(t, "覆盖落库", "ctime", row.Ctime, createdCtime)
	wantEQ(t, "覆盖落库", "state", row.State, model.StateDisabled)
	if row.ExpireAt == 0 {
		t.Errorf("覆盖落库：expire_at 仍为永久，说明 duration_seconds 没进 ON DUPLICATE 更新列")
	}
}

// TestUpsertListEntryStateAndExpireAgainstDDL 逐条核对「显式绑定的列」与 DDL DEFAULT 的关系。
//
// risk_list 的 INSERT 显式绑定 expire_at 与 state（model/list.go:98-100），
// 所以 DDL 的 `expire_at DEFAULT 0`、`state DEFAULT 1` 永远不会生效：
// 落库值只可能来自入参。两个后果要分开看：
//   - expire_at：0=永久 与 DDL 默认一致，且 logic 已拒绝 duration<0 ⇒ 无缺口；
//   - state：入参缺省即 0=停用，与 DDL 的 1 相反 ⇒ 忘记传 state 的黑名单条目会「写入成功但不生效」，
//     而停用态本身是唯一的下架手段（本服务没有删除名单的 RPC），不能自动归一。
//     这里钉住现状并记候选缺口 #19（fail-open 方向）。
func TestUpsertListEntryStateAndExpireAgainstDDL(t *testing.T) {
	// 显式 state=1：条目立即参与裁决（黑名单优先，命中即 BLOCK）。
	on := newStore(t)
	enabled, err := upsertListEntry(t, on, listReq(rpc.TargetType_TARGET_TYPE_MID, "42"))
	wantNoErr(t, "生效条目", err)
	wantEQ(t, "state=1", "落库 state", enabled.Entry.GetState(), model.StateEnabled)

	before := on.log.snapshot()
	dec, err := checkAction(t, on, commentReq("req-blacklisted"))
	wantNoErr(t, "命中黑名单的裁决", err)
	wantEQ(t, "命中黑名单", "decision", int32(dec.Decision), int32(rpc.Decision_DECISION_BLOCK))
	wantEQ(t, "命中黑名单", "basis", dec.Basis, policy.BasisBlacklist)
	if n := on.log.countPrefixFrom(before, "list.FindActive"); n != 1 {
		t.Fatalf("裁决读取名单 %d 次（应为 1 次批量查询）：%v", n, on.log.opsFrom(before))
	}

	// 不传 state（proto3 零值）：写入成功但条目停用，裁决照放行 —— 现状哨兵。
	off := newStore(t)
	noState := listReq(rpc.TargetType_TARGET_TYPE_MID, "42")
	noState.State = 0
	reply, err := upsertListEntry(t, off, noState)
	wantNoErr(t, "不传 state 的写入", err)
	wantEQ(t, "候选缺口 #19 现状", "落库 state（不是 DDL 的 1）", reply.Entry.GetState(), model.StateDisabled)
	wantEQ(t, "候选缺口 #19 现状", "created（条目确实写进去了）", reply.Created, true)

	dec, err = checkAction(t, off, commentReq("req-disabled-entry"))
	wantNoErr(t, "停用条目的裁决", err)
	wantEQ(t, "候选缺口 #19 现状", "basis（停用条目不参与裁决）", dec.Basis, policy.BasisNoRule)
	wantEQ(t, "候选缺口 #19 现状", "decision", int32(dec.Decision), int32(rpc.Decision_DECISION_ALLOW))

	// 到期条目同样不再生效，但行保留（审计要能看到「什么时候封过、封到什么时候」）。
	expired := newStore(t)
	expired.seedListEntry(model.RiskList{
		ListType: model.ListTypeBlack, TargetType: model.TargetTypeMid, TargetValue: "42",
		Reason: "已到期", Operator: testOperator, ExpireAt: time.Now().Unix() - 1, State: model.StateEnabled,
	})
	dec, err = checkAction(t, expired, commentReq("req-expired-entry"))
	wantNoErr(t, "到期条目的裁决", err)
	wantEQ(t, "到期条目", "basis", dec.Basis, policy.BasisNoRule)
}

func TestUpsertListEntryGuardsRunBeforeAnyDependency(t *testing.T) {
	cases := []struct {
		name   string
		in     *rpc.UpsertListEntryReq
		want   error
		wantIn string
	}{
		{"名单类型未传", func() *rpc.UpsertListEntryReq {
			r := listReq(rpc.TargetType_TARGET_TYPE_MID, "42")
			r.ListType = rpc.ListType_LIST_TYPE_UNSPECIFIED
			return r
		}(), model.ErrInvalidListEntry, "list_type=0"},
		{"名单类型越界", func() *rpc.UpsertListEntryReq {
			r := listReq(rpc.TargetType_TARGET_TYPE_MID, "42")
			r.ListType = rpc.ListType(3)
			return r
		}(), model.ErrInvalidListEntry, "list_type=3"},
		{"目标类型未传", func() *rpc.UpsertListEntryReq {
			r := listReq(rpc.TargetType_TARGET_TYPE_UNSPECIFIED, "42")
			return r
		}(), model.ErrInvalidListEntry, "target_type=0"},
		{"目标类型越界", func() *rpc.UpsertListEntryReq {
			r := listReq(rpc.TargetType_TARGET_TYPE_IP_HASH+10, "42")
			r.TargetValue = "42"
			return r
		}(), model.ErrInvalidListEntry, "target_type=13"},
		{"空 target_value", listReq(rpc.TargetType_TARGET_TYPE_MID, "   "), model.ErrInvalidListEntry, "target_value rejected"},
		{"明文 IPv4", listReq(rpc.TargetType_TARGET_TYPE_IP_HASH, rawIPv4), model.ErrInvalidListEntry, "target_value rejected"},
		{"XFF 链形态", listReq(rpc.TargetType_TARGET_TYPE_IP_HASH, rawIPv4+", 10.0.0.1"), model.ErrInvalidListEntry, "target_value rejected"},
		{"非十六进制摘要", listReq(rpc.TargetType_TARGET_TYPE_DEVICE, "zzzzzzzzzzzzzzzz"), model.ErrInvalidListEntry, "target_value rejected"},
		{"过短摘要", listReq(rpc.TargetType_TARGET_TYPE_DEVICE, "abc123"), model.ErrInvalidListEntry, "target_value rejected"},
		{"mid 带符号", listReq(rpc.TargetType_TARGET_TYPE_MID, "-42"), model.ErrInvalidListEntry, "target_value rejected"},
		{"mid 超列宽（拒绝而非截断）", listReq(rpc.TargetType_TARGET_TYPE_MID, strings.Repeat("7", maxListTargetValueLen+1)), model.ErrInvalidListEntry, "too long"},
		{"负 duration", func() *rpc.UpsertListEntryReq {
			r := listReq(rpc.TargetType_TARGET_TYPE_MID, "42")
			r.DurationSeconds = -1
			return r
		}(), model.ErrInvalidListEntry, "duration_seconds"},
		{"state 越界", func() *rpc.UpsertListEntryReq {
			r := listReq(rpc.TargetType_TARGET_TYPE_MID, "42")
			r.State = 2
			return r
		}(), model.ErrInvalidListEntry, "state=2"},
		{"reason 超长", func() *rpc.UpsertListEntryReq {
			r := listReq(rpc.TargetType_TARGET_TYPE_MID, "42")
			r.Reason = strings.Repeat("r", maxReasonLen+1)
			return r
		}(), model.ErrInvalidListEntry, "reason too long"},
		{"无操作人", func() *rpc.UpsertListEntryReq {
			r := listReq(rpc.TargetType_TARGET_TYPE_MID, "42")
			r.Operator = 0
			return r
		}(), model.ErrOperatorRequired, "operator required"},
		{"操作人为负", func() *rpc.UpsertListEntryReq {
			r := listReq(rpc.TargetType_TARGET_TYPE_MID, "42")
			r.Operator = -9
			return r
		}(), model.ErrOperatorRequired, "operator required"},
	}
	for _, tc := range cases {
		st := newStore(t)
		_, err := upsertListEntry(t, st, tc.in)
		wantErrIs(t, tc.name, err, tc.want)
		if !strings.Contains(err.Error(), tc.wantIn) {
			t.Errorf("%s：错误文本 %q 未包含 %q", tc.name, err.Error(), tc.wantIn)
		}
		// 守卫必须先于任何依赖调用：拒绝不得留下读放大或半成品写入。
		wantNoCall(t, tc.name, st, 0)
		wantEQ(t, tc.name, "rows 总数", len(st.list.rows), 0)
	}

	// 边界：恰好列宽的 mid 串必须放行（拒绝只针对超限）。
	ok := newStore(t)
	wide := listReq(rpc.TargetType_TARGET_TYPE_MID, strings.Repeat("7", maxListTargetValueLen))
	if _, err := upsertListEntry(t, ok, wide); err != nil {
		t.Fatalf("恰好列宽的 target_value 被拒：%v", err)
	}
	wantEQ(t, "列宽边界", "rows 总数", len(ok.list.rows), 1)
	wantEQ(t, "列宽边界", "64 位摘要（device）", len(devHash()), maxListTargetValueLen)
}

func TestUpsertListEntryDependencyFailurePropagates(t *testing.T) {
	st := newStore(t)
	boom := errors.New("boom-list-upsert")
	st.list.failWith("Upsert", boom)

	reply, err := upsertListEntry(t, st, listReq(rpc.TargetType_TARGET_TYPE_MID, "42"))
	// 依赖错误原样透传：不得伪装成「条目不存在」或「参数非法」。
	wantErrIs(t, "写入故障", err, boom)
	for _, disguised := range []error{model.ErrListEntryNotFound, model.ErrInvalidListEntry, model.ErrOperatorRequired} {
		if errors.Is(err, disguised) {
			t.Errorf("写入故障被伪装成 %v：%v", disguised, err)
		}
	}
	if reply != nil {
		t.Errorf("写入故障：仍返回响应体 %+v", reply)
	}
	wantOps(t, "写入故障序列", st.log.all(), []string{upsertOp(model.ListTypeBlack, model.TargetTypeMid, "42")})
	wantEQ(t, "写入故障爆炸半径", "rows 总数", len(st.list.rows), 0)

	// 故障恢复后同一请求走通，且只有一行（覆盖式写入不会因重试翻倍）。
	st.list.clear("Upsert")
	got, err := upsertListEntry(t, st, listReq(rpc.TargetType_TARGET_TYPE_MID, "42"))
	wantNoErr(t, "故障恢复后重试", err)
	wantEQ(t, "故障恢复后重试", "created", got.Created, true)
	wantEQ(t, "故障恢复后重试", "rows 总数", len(st.list.rows), 1)
}

// --- GetListEntries ---

func TestGetListEntriesFiltersLandInSQLParameters(t *testing.T) {
	st := newStore(t)
	black := st.seedListEntry(model.RiskList{ListType: model.ListTypeBlack, TargetType: model.TargetTypeMid,
		TargetValue: "42", Operator: testOperator, State: model.StateEnabled})
	white := st.seedListEntry(model.RiskList{ListType: model.ListTypeWhite, TargetType: model.TargetTypeMid,
		TargetValue: "42", Operator: testOperator, State: model.StateEnabled})
	device := st.seedListEntry(model.RiskList{ListType: model.ListTypeBlack, TargetType: model.TargetTypeDevice,
		TargetValue: devHash(), Operator: testOperator, State: model.StateEnabled})
	off := st.seedListEntry(model.RiskList{ListType: model.ListTypeBlack, TargetType: model.TargetTypeIpHash,
		TargetValue: ipHashHex, Operator: testOperator, State: model.StateDisabled})
	wantInt64s(t, "布数据", "ids", []int64{black, white, device, off}, []int64{1, 2, 3, 4})

	// 不过滤：四条都在（含停用条目，state=-1 不进 WHERE）。
	all, err := getListEntries(t, st, &rpc.GetListEntriesReq{State: -1})
	wantNoErr(t, "不过滤", err)
	wantOps(t, "不过滤序列", st.log.all(), []string{listOp(0, 0, "", -1, 0, 20)})
	wantEQ(t, "不过滤", "total", all.Total, int32(4))
	wantInt64s(t, "不过滤", "ids", idsOfEntries(all), []int64{4, 3, 2, 1}) // id DESC

	// 名单类型过滤：结果与 total 都只剩黑名单。
	// state 必须显式传 -1：proto3 不传即 0，而 0 在这里是「只查停用条目」的精确匹配（见下方用例）。
	blackOnly, err := getListEntries(t, st, &rpc.GetListEntriesReq{ListType: rpc.ListType_LIST_TYPE_BLACK, State: -1})
	wantNoErr(t, "黑名单过滤", err)
	wantOps(t, "黑名单过滤序列", st.log.opsFrom(1), []string{listOp(model.ListTypeBlack, 0, "", -1, 0, 20)})
	wantEQ(t, "黑名单过滤", "total（SQL 侧计数，不是取回后再筛）", blackOnly.Total, int32(3))
	wantInt64s(t, "黑名单过滤", "ids", idsOfEntries(blackOnly), []int64{4, 3, 1})

	// 目标类型 + 值 + 状态三条件同时进参数。
	targeted, err := getListEntries(t, st, &rpc.GetListEntriesReq{
		ListType: rpc.ListType_LIST_TYPE_BLACK, TargetType: rpc.TargetType_TARGET_TYPE_MID,
		TargetValue: "42", State: model.StateEnabled,
	})
	wantNoErr(t, "三条件", err)
	wantOps(t, "三条件序列", st.log.opsFrom(2), []string{listOp(model.ListTypeBlack, model.TargetTypeMid, "42", model.StateEnabled, 0, 20)})
	wantInt64s(t, "三条件", "ids", idsOfEntries(targeted), []int64{1})

	// 只要停用条目：state=0 是精确匹配，不是「不过滤」。
	stopped, err := getListEntries(t, st, &rpc.GetListEntriesReq{State: model.StateDisabled})
	wantNoErr(t, "停用条目", err)
	wantOps(t, "停用条目序列", st.log.opsFrom(3), []string{listOp(0, 0, "", model.StateDisabled, 0, 20)})
	wantInt64s(t, "停用条目", "ids", idsOfEntries(stopped), []int64{4})

	// 检索值同样先规范化再进 WHERE：请求 " 042 " 必须落到 SQL 参数 "42"。
	// 如果归一化发生在读回之后，SQL 参数会是 " 042"，运营按前导零写法检索就会看到空结果。
	normalized, err := getListEntries(t, st, &rpc.GetListEntriesReq{
		TargetType: rpc.TargetType_TARGET_TYPE_MID, TargetValue: " 042 ", State: -1,
	})
	wantNoErr(t, "检索值归一", err)
	wantOps(t, "检索值归一系列", st.log.opsFrom(4), []string{listOp(0, model.TargetTypeMid, "42", -1, 0, 20)})
	wantEQ(t, "检索值归一", "total", normalized.Total, int32(2))
}

// TestGetListEntriesKeepsExpiredRowsForAudit 钉住读侧可见性的**真实**口径：
// 列表接口只按 state 过滤，不按 expire_at 过滤，过期条目仍要返回。
//
// 这不是缺陷而是刻意：risk_list 没有任何删除/归档出口，运营查「这个账号上次被封到什么时候」
// 只能靠这张表，若沿用裁决侧 FindActive 的 `expire_at=0 OR expire_at>now`
// （model/list.go:145），到期条目就从审计视图里消失了。
// 两个口径的唯一差别是 expire_at 原样回传，由调用方判定是否仍生效。
func TestGetListEntriesKeepsExpiredRowsForAudit(t *testing.T) {
	st := newStore(t)
	alive := st.seedListEntry(model.RiskList{ListType: model.ListTypeBlack, TargetType: model.TargetTypeMid,
		TargetValue: "42", Operator: testOperator, State: model.StateEnabled, ExpireAt: time.Now().Unix() + 3600})
	gone := st.seedListEntry(model.RiskList{ListType: model.ListTypeBlack, TargetType: model.TargetTypeMid,
		TargetValue: "43", Operator: testOperator, State: model.StateEnabled, ExpireAt: time.Now().Unix() - 3600})
	perm := st.seedListEntry(model.RiskList{ListType: model.ListTypeBlack, TargetType: model.TargetTypeMid,
		TargetValue: "44", Operator: testOperator, State: model.StateEnabled, ExpireAt: 0})

	reply, err := getListEntries(t, st, &rpc.GetListEntriesReq{ListType: rpc.ListType_LIST_TYPE_BLACK, State: model.StateEnabled})
	wantNoErr(t, "到期条目可见", err)
	wantEQ(t, "到期条目可见", "total（三条都算，含已到期）", reply.Total, int32(3))
	wantInt64s(t, "到期条目可见", "ids", idsOfEntries(reply), []int64{perm, gone, alive})
	// expire_at 必须原样回传：调用方靠它区分永久(0)/在效/已到期。
	wantEQ(t, "到期条目可见", "永久条目 expire_at", reply.Entries[0].GetExpireAt(), int64(0))
	if reply.Entries[1].GetExpireAt() >= time.Now().Unix() {
		t.Errorf("到期条目 expire_at=%d 不是过去的秒值", reply.Entries[1].GetExpireAt())
	}
	if reply.Entries[2].GetExpireAt() <= time.Now().Unix() {
		t.Errorf("在效条目 expire_at=%d 不是未来的秒值", reply.Entries[2].GetExpireAt())
	}
}

func TestGetListEntriesPagingContractAndEmptyResult(t *testing.T) {
	st := newStore(t)
	for i := 1; i <= 5; i++ {
		st.seedListEntry(model.RiskList{ListType: model.ListTypeBlack, TargetType: model.TargetTypeMid,
			TargetValue: strconv.Itoa(i), Operator: testOperator, State: model.StateEnabled})
	}

	cases := []struct {
		name   string
		pn, ps int32
		op     string
		wantPn int32
		wantPs int32
	}{
		{"零值走默认", 0, 0, listOp(0, 0, "", -1, 0, 20), 1, 20},
		{"负数走默认", -5, -1, listOp(0, 0, "", -1, 0, 20), 1, 20},
		{"上限 50", 1, 999, listOp(0, 0, "", -1, 0, 50), 1, 50},
		{"恰为上限", 1, int32(maxPageSize), listOp(0, 0, "", -1, 0, 50), 1, int32(maxPageSize)},
		{"第二页", 2, 2, listOp(0, 0, "", -1, 2, 2), 2, 2},
	}
	for _, tc := range cases {
		before := st.log.snapshot()
		reply, err := getListEntries(t, st, &rpc.GetListEntriesReq{Pn: tc.pn, Ps: tc.ps, State: -1})
		wantNoErr(t, tc.name, err)
		wantOps(t, tc.name, st.log.opsFrom(before), []string{tc.op})
		wantEQ(t, tc.name, "pn 回显归一后的页码", reply.Pn, tc.wantPn)
		wantEQ(t, tc.name, "ps 回显归一后的页长", reply.Ps, tc.wantPs)
	}

	// 第二页 ps=2：id DESC 序取到 3、2。
	page2, err := getListEntries(t, st, &rpc.GetListEntriesReq{Pn: 2, Ps: 2, State: -1})
	wantNoErr(t, "第二页", err)
	wantInt64s(t, "第二页", "ids", idsOfEntries(page2), []int64{3, 2})
	wantEQ(t, "第二页", "total 仍是全量", page2.Total, int32(5))

	// 越界页：total 照旧回传，列表为空（客户端据此渲染「没有更多」）。
	empty, err := getListEntries(t, st, &rpc.GetListEntriesReq{Pn: 9, Ps: 2, State: -1})
	wantNoErr(t, "越界页", err)
	wantEQ(t, "越界页", "total", empty.Total, int32(5))
	wantEQ(t, "越界页", "len", len(empty.Entries), 0)

	// 无命中：total=0 时不得发第二条查询（model/list.go:184 与替身同口径）。
	before := st.log.snapshot()
	none, err := getListEntries(t, st, &rpc.GetListEntriesReq{TargetType: rpc.TargetType_TARGET_TYPE_MID, TargetValue: "999999", State: -1})
	wantNoErr(t, "无命中", err)
	wantOps(t, "无命中序列", st.log.opsFrom(before), []string{listOp(0, model.TargetTypeMid, "999999", -1, 0, 20)})
	wantEQ(t, "无命中", "total", none.Total, int32(0))
	wantEQ(t, "无命中", "len", len(none.Entries), 0)
}

func TestGetListEntriesGuardsAndFailureSurface(t *testing.T) {
	// 枚举/state 越界、缺 target_type、明文 IP：全部先拒，不发任何查询。
	for _, tc := range []struct {
		name string
		in   *rpc.GetListEntriesReq
		want error
	}{
		{"名单类型越界", &rpc.GetListEntriesReq{ListType: rpc.ListType(9)}, model.ErrInvalidListEntry},
		{"目标类型越界", &rpc.GetListEntriesReq{TargetType: rpc.TargetType(9)}, model.ErrInvalidListEntry},
		{"state 越界", &rpc.GetListEntriesReq{State: 2}, model.ErrInvalidListEntry},
		{"state 负值", &rpc.GetListEntriesReq{State: -2}, model.ErrInvalidListEntry},
		{"检索值缺维度", &rpc.GetListEntriesReq{TargetValue: "42"}, model.ErrInvalidTarget},
		{"检索明文 IP", &rpc.GetListEntriesReq{TargetType: rpc.TargetType_TARGET_TYPE_IP_HASH, TargetValue: rawIPv4}, model.ErrRawIPForbidden},
		{"检索非摘要", &rpc.GetListEntriesReq{TargetType: rpc.TargetType_TARGET_TYPE_DEVICE, TargetValue: "not-hex"}, model.ErrRawIPForbidden},
	} {
		st := newStore(t)
		st.seedListEntry(model.RiskList{ListType: model.ListTypeBlack, TargetType: model.TargetTypeMid,
			TargetValue: "42", Operator: testOperator, State: model.StateEnabled})
		_, err := getListEntries(t, st, tc.in)
		wantErrIs(t, tc.name, err, tc.want)
		wantNoCall(t, tc.name, st, 0)
	}

	// 明文 IP 检索是「拒绝」而不是「空结果」：空结果会被运营误读成「这个 IP 没被拉黑」。
	// 反过来，非摘要形态同样落在 ErrRawIPForbidden 上，错误文本会误导（候选缺口 #20），
	// 这里钉住文案，避免有人以为可以顺手改成按类型区分却不修归一化入口。
	if _, err := getListEntries(t, newStore(t), &rpc.GetListEntriesReq{
		TargetType: rpc.TargetType_TARGET_TYPE_MID, TargetValue: "12ab",
	}); err == nil || !strings.Contains(err.Error(), "raw ip") {
		t.Errorf("候选缺口 #20 现状：非数字 mid 也被说成 raw ip，实际 err=%v", err)
	}

	// 查询故障原样上抛：静默返回空表会让运营以为「名单里没有这个人」。
	st := newStore(t)
	st.seedListEntry(model.RiskList{ListType: model.ListTypeBlack, TargetType: model.TargetTypeMid,
		TargetValue: "42", Operator: testOperator, State: model.StateEnabled})
	boom := errors.New("boom-list-list")
	st.list.failWith("List", boom)
	// 这里刻意不传 state：proto3 下「不传」等价于 state=0=只查停用条目，
	// 运营后台若忘了传 -1 就会看到「名单是空的」。钉住现状 = 候选缺口 #21
	// （接口用 -1 表达「不过滤」而 proto3 无法区分未传与 0；改默认值会改变已有调用方看到的结果）。
	reply, err := getListEntries(t, st, &rpc.GetListEntriesReq{})
	wantErrIs(t, "列表故障", err, boom)
	if reply != nil {
		t.Errorf("列表故障：仍返回响应体 %+v", reply)
	}
	wantOps(t, "列表故障序列", st.log.all(), []string{listOp(0, 0, "", model.StateDisabled, 0, 20)})

	// 名单读写都不碰 Redis：任何缓存动作都说明有人把名单塞进了缓存路径。
	if n := st.log.countPrefix("redis."); n != 0 {
		t.Fatalf("名单接口触碰 Redis %d 次（应为 0）：%v", n, st.log.all())
	}
}
