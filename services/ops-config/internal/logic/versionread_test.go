// 本文件钉住 ListConfigVersions（配置版本历史分页），四条真正的风险：
//
//  1. 排序事实源：ops_config_version.go:234 是
//     `WHERE config_id = ? ORDER BY version DESC LIMIT ? OFFSET ?`（没有第三级 tiebreaker，
//     uniq_config_version 保证同配置项内版本号唯一）。本文件的次序期望全部由这一句推出，
//     翻页拼接则与同一份全量逐条比对，不抄第二遍字面量。
//  2. 历史必须按 config_id 圈死：两个键的版本号会重叠（每个键各自从 1 递增），
//     所以「取错列 / 漏了 config_id」只有用重叠号码的种子才看得出来。
//  3. 键不存在是 fail-**soft**（后台筛选框打错字不该 500，listconfigversionslogic.go:44-47），
//     但「查键这件事失败」绝不能被吞成「这个键没有历史」——后者会把库故障伪装成一个空页。
//     两者在响应里长得一样，可区分的只有 ConfigVersion.ListByConfig 的调用次数。
//  4. 版本行是**证据**：值全量回（含超过当前 OpsValue.MaxBytes 的历史长值）、
//     audit_entry_id=0 原样可见、value_type 哪怕是 9 也照回——
//     同一个坏行在 RollbackConfig 那边是 fail-closed（publish_rollback_test.go:618）。
package logic

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/ops-config/internal/config"
	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"
)

var errVersionStoreDown = errors.New("read ops_config_version: no such table")

func (e *testEnv) listVersions(t *testing.T, req *rpc.ListConfigVersionsReq) (*rpc.ListConfigVersionsReply, error) {
	t.Helper()
	return NewListConfigVersionsLogic(bg(), e.svc).ListConfigVersions(req)
}

// versionNumbers 把投影压成版本号序列：次序与 config_id 隔离都只看这一列，
// 「取错列」（回成 version_id 或 config_id）会直接显形。
func versionNumbers(items []*rpc.ConfigVersion) []string {
	out := make([]string, 0, len(items))
	for _, v := range items {
		out = append(out, fmt.Sprint(v.GetVersion()))
	}
	return out
}

// requireVersionRowMatches 逐字段比对投影与库里的快照行（回读的行是唯一事实源）。
func requireVersionRowMatches(t *testing.T, got *rpc.ConfigVersion, row *model.ConfigVersion, label string) {
	t.Helper()
	if got.GetVersionId() != row.VersionID || got.GetConfigId() != row.ConfigID ||
		got.GetVersion() != row.Version || got.GetValue() != row.Value ||
		int32(got.GetValueType()) != row.ValueType || got.GetChangeType() != row.ChangeType ||
		got.GetRollbackFrom() != row.RollbackFrom || got.GetOperatorId() != row.OperatorID ||
		got.GetOperatorName() != row.OperatorName || got.GetReason() != row.Reason ||
		got.GetAuditEntryId() != row.AuditEntryID || got.GetRequestId() != row.RequestID ||
		got.GetPublishedAt() != row.PublishedAt || got.GetCtime() != row.Ctime {
		t.Fatalf("%s：投影与 ops_config_version 行不一致\n  got=%+v\n row=%+v", label, got, row)
	}
}

// seedVersionHistory 布一个配置项 + 若干条版本快照（走 model 写路径）。
// versions 的顺序就是插入顺序：故意不排序，才能证明读取次序来自 ORDER BY 而不是来自 map。
func (e *testEnv) seedVersionHistory(t *testing.T, cfgKey, scope string, versions ...int64) *model.ConfigItem {
	t.Helper()
	item := e.seedItem(t, cfgKey, scope, model.ValueTypeString, model.StateOn)
	for i, v := range versions {
		change := model.ChangeTypePublish
		if i == 0 {
			change = model.ChangeTypeCreate
		}
		e.seedVersion(t, item.ConfigID, v, fmt.Sprintf("%s@v%d", cfgKey, v), model.ValueTypeString,
			change, fmt.Sprintf("seed-%s-%d", cfgKey, v), "用例种子：造历史")
	}
	return e.itemRow(item.ConfigID)
}

func TestListConfigVersionsRejectsIllegalInputsBeforeTouchingStorage(t *testing.T) {
	e := newTestEnv(t)
	e.seedVersionHistory(t, "home.guard", model.ScopeGlobal, 1, 2)
	before := e.effects()

	cases := []struct {
		name string
		req  *rpc.ListConfigVersionsReq
		want error
	}{
		// cfg_key 守卫排在分页之前（listconfigversionslogic.go:29-35）：
		// 没有键就分页等于回一份「不知道是谁的历史」。
		{"键为空", &rpc.ListConfigVersionsReq{}, model.ErrConfigKeyRequired},
		{"键只有空白", &rpc.ListConfigVersionsReq{CfgKey: "   "}, model.ErrConfigKeyRequired},
		{"键含大写与空格", &rpc.ListConfigVersionsReq{CfgKey: "Home Title"}, model.ErrConfigKeyInvalid},
		{"键含连字符（cfgKeyRe 只允许小写字母/数字/下划线/点）",
			&rpc.ListConfigVersionsReq{CfgKey: "home-title"}, model.ErrConfigKeyInvalid},
		{"pn 为负", &rpc.ListConfigVersionsReq{CfgKey: "home.guard", Pn: -1}, model.ErrInvalidPage},
		{"ps 为负", &rpc.ListConfigVersionsReq{CfgKey: "home.guard", Ps: -1}, model.ErrInvalidPage},
		// scope 守卫排在查库之前：非法 scope 走到 FindOne 只会得到一个空页。
		{"scope 未知", &rpc.ListConfigVersionsReq{CfgKey: "home.guard", Scope: "mini_program"}, model.ErrScopeUnknown},
		{"scope 大小写不符（库里是小写词）",
			&rpc.ListConfigVersionsReq{CfgKey: "home.guard", Scope: "GLOBAL"}, model.ErrScopeUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply, err := e.listVersions(t, tc.req)
			wantFail(t, err, tc.want, tc.name)
			if reply != nil {
				t.Fatalf("%s：出错还回了响应体：%+v", tc.name, reply)
			}
			if got := e.call("ConfigItem.FindOne"); got != 0 {
				t.Fatalf("%s：入参校验阶段就查了配置项：%d 次", tc.name, got)
			}
			if got := e.call("ConfigVersion.ListByConfig"); got != 0 {
				t.Fatalf("%s：入参校验阶段就查了历史：%d 次", tc.name, got)
			}
		})
	}
	// 守卫的先后本身也要钉：两个都非法时，先报哪一个就是先检查哪一个。
	if _, err := e.listVersions(t, &rpc.ListConfigVersionsReq{CfgKey: "BAD KEY", Pn: -1}); !errors.Is(err, model.ErrConfigKeyInvalid) {
		t.Fatalf("cfg_key 守卫没排在分页之前：%v", err)
	}
	if _, err := e.listVersions(t, &rpc.ListConfigVersionsReq{CfgKey: "home.guard", Pn: -1, Scope: "nope"}); !errors.Is(err, model.ErrInvalidPage) {
		t.Fatalf("分页守卫没排在 scope 之前（应先报分页）：%v", err)
	}
	// 合法的「不给 scope」= global：不能被当成非法值拒掉。
	// 顺带钉住 latest_version 的来源：本用例的配置项有两条历史却从未推进过指针，
	// 回 0 才说明它来自 item.latest_version，而不是 MAX(version) 或历史首行。
	legal, err := e.listVersions(t, &rpc.ListConfigVersionsReq{CfgKey: "home.guard"})
	rows := wantOK(t, legal, err, "不给 scope 的请求")
	if len(rows.GetItems()) != 2 || rows.GetLatestVersion() != 0 {
		t.Fatalf("合法请求口径不符：%v latest=%d", versionNumbers(rows.GetItems()), rows.GetLatestVersion())
	}
	// 键格式合法但库里没有（"weixin" 只在 scope 里被禁，不是 cfg_key 的保留词）：
	// 这是一次空查询，不是入参错误。
	if _, err := e.listVersions(t, &rpc.ListConfigVersionsReq{CfgKey: "home.title.weixin"}); err != nil {
		t.Fatalf("格式合法、内容不存在的键被当成了入参错误：%v", err)
	}
	e.requireSameEffects(t, before, "非法 ListConfigVersions 入参零副作用")
}

// cfg_key 的长度上限来自 cfgKeyRe（ops_config_item.go:22，`{1,64}`），
// 而不是 helpers.go:197 那句 `l.valueMaxBytes > 0 && len(key) > 64`：
// 正则已经封顶，那一支永远走不到；配置里的 OpsValue.MaxKeyLen 更是没有任何调用方读取。
// 本用例把「上限的真实来源」钉住：改 MaxKeyLen / MaxBytes 都不会改变长度判定。
// 现状哨兵：若将来把 MaxKeyLen 真正接进 checkCfgKey，或删掉那句死分支，本用例的
// 「收紧无效」这一段必须变红并被改写——不许反过来放宽。
func TestListConfigVersionsKeyLengthCapComesFromTheRegexNotTheKnob(t *testing.T) {
	e := newTestEnv(t)
	key64 := strings.Repeat("k", 64)
	e.seedVersionHistory(t, key64, model.ScopeGlobal, 1)

	if _, err := e.listVersions(t, &rpc.ListConfigVersionsReq{CfgKey: key64}); err != nil {
		t.Fatalf("64 字符的键被拒了：%v", err)
	}
	if _, err := e.listVersions(t, &rpc.ListConfigVersionsReq{CfgKey: key64 + "x"}); !errors.Is(err, model.ErrConfigKeyInvalid) {
		t.Fatalf("65 字符的键没被拒（cfgKeyRe 的 64 上限失效）：%v", err)
	}

	// 收紧 OpsValue.MaxKeyLen 与 MaxBytes 都不影响键长判定：两个旋钮都没接线。
	tight := newTestEnvWith(t, func(c *config.Config) {
		c.OpsValue.MaxKeyLen = 4
		c.OpsValue.MaxBytes = 4
	})
	k20 := strings.Repeat("a", 20) + ".hist"
	tight.seedVersionHistory(t, k20, model.ScopeGlobal, 1)
	reply, err := tight.listVersions(t, &rpc.ListConfigVersionsReq{CfgKey: k20})
	got := wantOK(t, reply, err, "MaxKeyLen=4 仍接受 25 字符的键")
	if len(got.GetItems()) != 1 {
		t.Fatalf("收紧的配置改变了键长判定（MaxKeyLen 本来就没被读取）：%+v", got)
	}
	// 值长度上限同样不作用在键上：MaxBytes=4 时 25 字符的键照样能查。
	if _, err := tight.listVersions(t, &rpc.ListConfigVersionsReq{CfgKey: "home.a"}); err != nil {
		t.Fatalf("MaxBytes 被误用成了键长上限：%v", err)
	}
}

// 排序事实源：ops_config_version.go:234 `WHERE config_id = ? ORDER BY version DESC`。
// 种子按 3,1,5,2,4 的顺序插入，期望次序 5,4,3,2,1 只能由 ORDER BY 给出。
func TestListConfigVersionsOrdersHistoryDescending(t *testing.T) {
	e := newTestEnv(t)
	item := e.seedVersionHistory(t, "home.sort", model.ScopeGlobal, 3, 1, 5, 2, 4)
	e.db.items[item.ConfigID].LatestVersion = 5

	all, err := e.listVersions(t, &rpc.ListConfigVersionsReq{CfgKey: "home.sort", Ps: 100})
	rows := wantOK(t, all, err, "全量一页")
	if got := strings.Join(versionNumbers(rows.GetItems()), ","); got != "5,4,3,2,1" {
		t.Fatalf("历史不是 version DESC：%s（期望 5,4,3,2,1）", got)
	}
	if rows.GetTotal() != 5 {
		t.Fatalf("total 应是该配置项的全部快照数：%d", rows.GetTotal())
	}
	if rows.GetLatestVersion() != 5 {
		t.Fatalf("latest_version 没回配置项指针：%d", rows.GetLatestVersion())
	}
	for i, row := range rows.GetItems() {
		requireVersionRowMatches(t, row, e.versionRow(item.ConfigID, row.GetVersion()),
			fmt.Sprintf("第 %d 行的字段比对", i+1))
	}
	// 一次请求只发两条 SQL：定位配置项 + 分页读历史。
	// 多出来的 MAX(version) / FindLatest 是一次无谓的回源（指针列就是答案）。
	if got := e.call("ConfigVersion.MaxVersion"); got != 0 {
		t.Fatalf("列表接口去算了 MAX(version)：%d 次", got)
	}
	if got := e.call("ConfigVersion.FindLatest"); got != 0 {
		t.Fatalf("列表接口去找了最新快照：%d 次", got)
	}

	// 翻页：拼接必须与上面那份全量逐条同序。
	p1, err := e.listVersions(t, &rpc.ListConfigVersionsReq{CfgKey: "home.sort", Pn: 1, Ps: 3})
	first := wantOK(t, p1, err, "第一页")
	p2, err := e.listVersions(t, &rpc.ListConfigVersionsReq{CfgKey: "home.sort", Pn: 2, Ps: 3})
	second := wantOK(t, p2, err, "第二页")
	if len(first.GetItems()) != 3 || len(second.GetItems()) != 2 {
		t.Fatalf("页大小没被执行：%d / %d", len(first.GetItems()), len(second.GetItems()))
	}
	joined := append(versionNumbers(first.GetItems()), versionNumbers(second.GetItems())...)
	if strings.Join(joined, ",") != strings.Join(versionNumbers(rows.GetItems()), ",") {
		t.Fatalf("翻页拼接与全量不一致：\n  got =%v\n want=%v", joined, versionNumbers(rows.GetItems()))
	}
	// 越界页：空 items + 保留 total/latest（后台据此才知道不是「历史被清空」）。
	far, err := e.listVersions(t, &rpc.ListConfigVersionsReq{CfgKey: "home.sort", Pn: 9, Ps: 3})
	out := wantOK(t, far, err, "越界页")
	if len(out.GetItems()) != 0 || out.GetTotal() != 5 || out.GetLatestVersion() != 5 {
		t.Fatalf("越界页口径不符：%d 条 total=%d latest=%d",
			len(out.GetItems()), out.GetTotal(), out.GetLatestVersion())
	}
	// 只读：零写、零缓存、零审计。
	if len(e.cache.sets) != 0 || len(e.cache.gets) != 0 || len(e.cache.dels) != 0 || len(e.audit.reqs) != 0 {
		t.Fatalf("ListConfigVersions 动了缓存或审计：sets=%v gets=%v dels=%v audit=%d",
			e.cache.sets, e.cache.gets, e.cache.dels, len(e.audit.reqs))
	}
}

// 两个键的版本号刻意重叠（a 有 1..5、b 只有 2 与 4）：
// 少了 config_id 这一支 WHERE，读出来的就是「两家的历史混在一起」。
func TestListConfigVersionsIsolatesHistoryByConfigIDAndScope(t *testing.T) {
	e := newTestEnv(t)
	a := e.seedVersionHistory(t, "home.a", model.ScopeGlobal, 1, 2, 3, 4, 5)
	b := e.seedVersionHistory(t, "home.b", model.ScopeGlobal, 2, 4)
	// 同一个键的第二个 scope：FindOne 必须按 (cfg_key, scope) 唯一键定位。
	c := e.seedVersionHistory(t, "home.a", model.ScopeAndroid, 7)
	if a.ConfigID == b.ConfigID || a.ConfigID == c.ConfigID {
		t.Fatal("种子失效：三个配置项共用了 config_id")
	}

	for _, tc := range []struct {
		name      string
		req       *rpc.ListConfigVersionsReq
		want      string
		wantID    int64
		wantTotal int64
	}{
		{"home.a / global", &rpc.ListConfigVersionsReq{CfgKey: "home.a", Ps: 100}, "5,4,3,2,1", a.ConfigID, 5},
		{"home.b / global（号码与 a 重叠）", &rpc.ListConfigVersionsReq{CfgKey: "home.b", Ps: 100}, "4,2", b.ConfigID, 2},
		{"home.a / android（同键不同 scope）",
			&rpc.ListConfigVersionsReq{CfgKey: "home.a", Scope: model.ScopeAndroid, Ps: 100}, "7", c.ConfigID, 1},
		{"两端空白被裁掉后再查", &rpc.ListConfigVersionsReq{CfgKey: "  home.b  ", Ps: 100}, "4,2", b.ConfigID, 2},
		{"scope 空白归一为 global",
			&rpc.ListConfigVersionsReq{CfgKey: "home.a", Scope: "  ", Ps: 100}, "5,4,3,2,1", a.ConfigID, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := e.call("ConfigItem.FindOne")
			reply, err := e.listVersions(t, tc.req)
			got := wantOK(t, reply, err, tc.name)
			if s := strings.Join(versionNumbers(got.GetItems()), ","); s != tc.want {
				t.Fatalf("%s：历史不符\n  got =%s\n want=%s", tc.name, s, tc.want)
			}
			if got.GetTotal() != tc.wantTotal {
				t.Fatalf("%s：total=%d want=%d", tc.name, got.GetTotal(), tc.wantTotal)
			}
			for _, row := range got.GetItems() {
				if row.GetConfigId() != tc.wantID {
					t.Fatalf("%s：混进了别的配置项的快照（config_id=%d）", tc.name, row.GetConfigId())
				}
			}
			// 定位配置项恰好一次：多一次就是「先查再猜」的重复回源。
			if e.call("ConfigItem.FindOne") != reads+1 {
				t.Fatalf("%s：配置项定位查了 %d 次", tc.name, e.call("ConfigItem.FindOne")-reads)
			}
		})
	}

	// ps 越界夹到 Query.MaxPageSize(100)、缺省回落到上限，两者都不改变本页条数。
	for _, req := range []*rpc.ListConfigVersionsReq{{CfgKey: "home.a", Ps: 900}, {CfgKey: "home.a"}} {
		reply, err := e.listVersions(t, req)
		got := wantOK(t, reply, err, "ps 夹取")
		if len(got.GetItems()) != 5 {
			t.Fatalf("%+v：夹取改变了本页条数：%d", req, len(got.GetItems()))
		}
	}

	// Query.CountTotal=false → total 回 0，但 latest_version 与列表内容不受影响：
	// 指针列来自已定位的配置项，不依赖 COUNT。
	off := newTestEnvWith(t, func(c *config.Config) { c.Query.CountTotal = false })
	off.seedVersionHistory(t, "home.a", model.ScopeGlobal, 1, 2, 3)
	off.db.items[off.itemByName("home.a").ConfigID].LatestVersion = 3
	hidden, err := off.listVersions(t, &rpc.ListConfigVersionsReq{CfgKey: "home.a", Ps: 2})
	got := wantOK(t, hidden, err, "CountTotal=false")
	if got.GetTotal() != 0 {
		t.Fatalf("关掉总数开关后 total 必须是 0：%d", got.GetTotal())
	}
	if len(got.GetItems()) != 2 || got.GetLatestVersion() != 3 {
		t.Fatalf("关掉 total 连带砍了历史或指针：%d 条 latest=%d", len(got.GetItems()), got.GetLatestVersion())
	}
}

// 「键不存在」与「这次查询失败」在响应里长得一模一样（都是空 items），
// 可区分的只有是否发出过历史查询。这里同时钉住 fail-soft 与其边界。
func TestListConfigVersionsUnknownKeyIsAFailSoftEmptyPage(t *testing.T) {
	e := newTestEnv(t)
	e.seedVersionHistory(t, "home.real", model.ScopeGlobal, 1, 2)
	e.seedItem(t, "home.never", model.ScopeGlobal, model.ValueTypeString, model.StateOn)
	off := e.seedItem(t, "home.off", model.ScopeGlobal, model.ValueTypeString, model.StateOn)
	e.seedVersion(t, off.ConfigID, 1, "off-value", model.ValueTypeString,
		model.ChangeTypeCreate, "req-off-1", "用例种子：停用前发布")
	// 契约里没有「停用配置项」的 RPC（opsconfig.proto 的 OpsConfig service 只有发布/回滚/
	// 规则/专题/坑位/开关/刷新），所以库里出现 state=2 只有 DBA 手工收口这一条路。
	e.forceItemState(off.ConfigID, model.StateOff)
	before := e.effects()

	// 键不存在：空列表 + total=0 + latest=0，并且不报错。
	// 对照面是 ListRolloutRules：同样的现场它回 ErrConfigNotFound（fail-stop），
	// 因为「拼错 key」与「这个 key 没有规则」在后台是两件事（见 rolloutread_test.go）。
	misses := e.call("ConfigVersion.ListByConfig")
	reply, err := e.listVersions(t, &rpc.ListConfigVersionsReq{CfgKey: "home.typo", Ps: 100})
	got := wantOK(t, reply, err, "键不存在")
	if len(got.GetItems()) != 0 || got.GetTotal() != 0 || got.GetLatestVersion() != 0 {
		t.Fatalf("键不存在却回了内容：%+v", got)
	}
	if e.call("ConfigVersion.ListByConfig") != misses {
		t.Fatalf("键不存在还去查了历史（白扫一次 ops_config_version）")
	}

	// 存在但从未发布：历史为空，但确实发出过一次查询——与上面那条可区分。
	before2 := e.effects()
	never, err := e.listVersions(t, &rpc.ListConfigVersionsReq{CfgKey: "home.never"})
	empty := wantOK(t, never, err, "存在但无历史")
	if len(empty.GetItems()) != 0 || empty.GetTotal() != 0 || empty.GetLatestVersion() != 0 {
		t.Fatalf("无历史的配置项回了内容：%+v", empty)
	}
	if got := e.call("ConfigVersion.ListByConfig"); got != 1 {
		t.Fatalf("「键存在但无历史」应真的查一次历史，实得 %d 次", got)
	}
	e.requireSameEffects(t, before2, "无历史零副作用")

	// 停用的配置项仍列得出历史：下线不等于销毁证据。
	offReply, err := e.listVersions(t, &rpc.ListConfigVersionsReq{CfgKey: "home.off"})
	rows := wantOK(t, offReply, err, "停用配置项的历史")
	if len(rows.GetItems()) != 1 || rows.GetItems()[0].GetValue() != "off-value" {
		t.Fatalf("停用的配置项列不出历史：%+v", rows.GetItems())
	}
	e.requireSameEffects(t, before, "只读三个分支零副作用")
}

// 版本行是证据：投影不许截断、不许美化、不许替调用方「修数据」。
func TestListConfigVersionsReturnsEveryHistoricalFieldVerbatim(t *testing.T) {
	e := newTestEnv(t)
	item := e.seedVersionHistory(t, "home.evidence", model.ScopeGlobal, 1)
	// 历史长值：写入侧没有长度校验（model.insertVersion 只校 config_id/version/request_id/
	// reason/change_type/value_type），OpsValue.MaxBytes 只在 PublishConfig 入口生效。
	// 因此把 MaxBytes 调小之后，库里已有的长值仍必须原样读出——
	// 这正是 internal/config/config.go 里「改小要在 README 说明历史长值怎么办」的答案。
	longValue := strings.Repeat("长", 3000) // 9000 字节 > MaxBytes(8192) < 列宽 TEXT(65535)
	e.seedVersion(t, item.ConfigID, 2, longValue, model.ValueTypeJSON,
		model.ChangeTypePublish, "seed-long", "超过当前 MaxBytes 的历史值")
	// 回滚行与审计缺口行：这两列没有对外的写入口子，只能照真写路径落库后改列。
	rb := e.seedVersion(t, item.ConfigID, 3, "回滚来的值", model.ValueTypeString,
		model.ChangeTypeRollback, "seed-rollback", "止血：回滚到 v1")
	e.db.versions[rb.VersionID].RollbackFrom = 1
	e.db.versions[rb.VersionID].AuditEntryID = 777
	// 审计没补上的那一行：audit_entry_id 保持 0（conv.go:90-92 的口径：缺口要可见）。
	noAudit := e.seedVersion(t, item.ConfigID, 4, "无存证", model.ValueTypeInt,
		model.ChangeTypePublish, "seed-noaudit", "审计写入待补偿")
	if noAudit.AuditEntryID != 0 {
		t.Fatalf("种子失效：这一行本该没有审计引用")
	}
	// 手工改坏的 value_type：读侧照原样回（不解释、不报错）。
	e.putRawVersion(&model.ConfigVersion{
		ConfigID: item.ConfigID, Version: 5, Value: "不是任何类型的值", ValueType: 9,
		ChangeType: model.ChangeTypePublish, RequestID: "seed-broken-type",
		Reason: "被改坏的历史", PublishedAt: model.NowUnix(),
	})

	reply, err := e.listVersions(t, &rpc.ListConfigVersionsReq{CfgKey: "home.evidence", Ps: 100})
	rows := wantOK(t, reply, err, "全量历史")
	if got := strings.Join(versionNumbers(rows.GetItems()), ","); got != "5,4,3,2,1" {
		t.Fatalf("历史次序不符：%s", got)
	}
	if rows.GetTotal() != 5 {
		t.Fatalf("total=%d want=5", rows.GetTotal())
	}
	for _, row := range rows.GetItems() {
		requireVersionRowMatches(t, row, e.versionRow(item.ConfigID, row.GetVersion()),
			fmt.Sprintf("v%d 字段比对", row.GetVersion()))
		switch row.GetVersion() {
		case 5:
			// 未知类型不做判定：改成「报错」或「按 string 兜底」都会销毁当时的证据。
			if int32(row.GetValueType()) != 9 {
				t.Fatalf("坏 value_type 被读侧改写了：%d", row.GetValueType())
			}
		case 4:
			if row.GetAuditEntryId() != 0 {
				t.Fatalf("待补偿的审计缺口被藏起来了：%d", row.GetAuditEntryId())
			}
		case 3:
			if row.GetChangeType() != model.ChangeTypeRollback || row.GetRollbackFrom() != 1 {
				t.Fatalf("回滚来源丢了：%+v", row)
			}
			if row.GetAuditEntryId() != 777 {
				t.Fatalf("已存证的审计引用没回给后台：%d", row.GetAuditEntryId())
			}
		case 2:
			if len(row.GetValue()) != len(longValue) || row.GetValue() != longValue {
				t.Fatalf("历史长值被截断/改写了：len=%d want=%d", len(row.GetValue()), len(longValue))
			}
			if int32(row.GetValueType()) != model.ValueTypeJSON {
				t.Fatalf("值类型快照丢了：%d", row.GetValueType())
			}
		}
	}
	// 幂等键也照原样回：重放排查全靠它。次序 5,4,3,2,1 → 索引 1 是 v4。
	if rows.GetItems()[1].GetRequestId() != "seed-noaudit" {
		t.Fatalf("request_id 没回给后台：%+v", rows.GetItems()[1])
	}
}

func TestListConfigVersionsPropagatesStorageErrorsVerbatim(t *testing.T) {
	e := newTestEnv(t)
	e.seedVersionHistory(t, "home.db", model.ScopeGlobal, 1, 2)
	before := e.effects()

	// 第一类：原始错误上抛。定位配置项失败绝不能被吞成「这个键没有历史」——
	// 那等于把一次库故障伪装成一个空页，运营会以为自己从没发布过。
	e.db.readErrs["ConfigItem.FindOne"] = errVersionStoreDown
	findOneCalls := e.call("ConfigItem.FindOne")
	listCalls := e.call("ConfigVersion.ListByConfig")
	reply, err := e.listVersions(t, &rpc.ListConfigVersionsReq{CfgKey: "home.db"})
	wantFail(t, err, errVersionStoreDown, "ConfigItem.FindOne 故障")
	if reply != nil {
		t.Fatalf("依赖故障却回了响应体（空历史是一种伪装）：%+v", reply)
	}
	if errors.Is(err, model.ErrConfigNotFound) {
		t.Fatalf("依赖错误被改写成了「配置项不存在」：%v", err)
	}
	if e.call("ConfigItem.FindOne") != findOneCalls+1 || e.call("ConfigVersion.ListByConfig") != listCalls {
		t.Fatalf("故障路径的查询次数不符（FindOne %d→%d / List %d→%d）",
			findOneCalls, e.call("ConfigItem.FindOne"), listCalls, e.call("ConfigVersion.ListByConfig"))
	}
	delete(e.db.readErrs, "ConfigItem.FindOne")

	// 同一个哨兵：历史查询本身失败也不能退成「已定位到配置项 + 空历史」。
	// latest_version 在这里必须是拿不到的：整页都被拒，而不是半真半假。
	e.db.readErrs["ConfigVersion.ListByConfig"] = errVersionStoreDown
	bad, err := e.listVersions(t, &rpc.ListConfigVersionsReq{CfgKey: "home.db"})
	wantFail(t, err, errVersionStoreDown, "ConfigVersion.ListByConfig 故障")
	if bad != nil {
		t.Fatalf("历史读失败却回了响应体（latest=%d）：%+v", bad.GetLatestVersion(), bad)
	}
	delete(e.db.readErrs, "ConfigVersion.ListByConfig")

	ok, err := e.listVersions(t, &rpc.ListConfigVersionsReq{CfgKey: "home.db"})
	back := wantOK(t, ok, err, "故障恢复后")
	if len(back.GetItems()) != 2 || back.GetLatestVersion() != 0 {
		t.Fatalf("恢复后读不到历史：%v latest=%d", versionNumbers(back.GetItems()), back.GetLatestVersion())
	}
	e.requireSameEffects(t, before, "ListConfigVersions 只读零副作用")
}

// 本读路径不依赖任何外部服务：AuditRPC 与 Redis 都不挂也不影响整页。
// 与写侧的 fail-closed 面（failclosed_test.go）对照，登记「读侧没有这道门禁」。
func TestListConfigVersionsHasNoExternalDependencyGate(t *testing.T) {
	e := newTestEnv(t)
	e.seedVersionHistory(t, "home.nodep", model.ScopeGlobal, 1, 2, 3)
	full, err := e.listVersions(t, &rpc.ListConfigVersionsReq{CfgKey: "home.nodep", Ps: 100})
	want := wantOK(t, full, err, "基准读取")

	e.svc.Audit = nil
	e.svc.Cache = nil
	blind, err := e.listVersions(t, &rpc.ListConfigVersionsReq{CfgKey: "home.nodep", Ps: 100})
	got := wantOK(t, blind, err, "摘掉审计与缓存之后")
	if len(got.GetItems()) != len(want.GetItems()) || got.GetTotal() != want.GetTotal() ||
		strings.Join(versionNumbers(got.GetItems()), ",") != strings.Join(versionNumbers(want.GetItems()), ",") {
		t.Fatalf("外部依赖缺失改变了整页答案：%v vs %v",
			versionNumbers(got.GetItems()), versionNumbers(want.GetItems()))
	}
	// 缓存为 nil 也不许偷偷建投影：历史分页永远读 MySQL。
	if len(e.cache.sets) != 0 || len(e.cache.gets) != 0 {
		t.Fatalf("历史读取写了/读了投影：sets=%v gets=%v", e.cache.sets, e.cache.gets)
	}
}
