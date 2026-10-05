// 本文件钉住 GetUserSetting / UpdateUserSetting 这一对「反骚扰偏好」读写入口。
// 它们看着无害（就一行配置），但错法很具体：
//
//	- 读路径「顺手补一行缺省」会把 pm_user_setting 塞满从未设置过的用户，
//	  并让「未设置」与「显式设成默认」再也分不开（mtime=0 就是这条区分的唯一载体）；
//	- 写路径把 optional 布尔的零值当成「用户关掉了它」，会静默改掉用户没碰过的开关；
//	- allow_from 越界值直接落库，会让发送门禁读到 0 既不是「不修改」也不是任何合法档位；
//	- 站点级 KeywordFilterEnabled=false 时被单个用户写回 1，环境里就出现「个别行仍在过滤」的假事实。
//
// 因此这里每条都落到「库里的行」而不是只落到「接口返回值」：
// 返回值对了但库里写错，是本域最坏的一类通过。
//
// 隐私约束（AGENTS.md §7）：偏好在语义上是「某人是否收得到消息」的策略事实，
// 用例里的 mid 全是明显的假值（101/202/303/909），不引入手机号/身份证之类真实标识。

package logic

import (
	"strings"
	"testing"

	"go-video/services/private-message/model"
	"go-video/services/private-message/rpc"
)

// onb 取 bool 的地址：proto3 的 optional 布尔靠 nil / 非 nil 区分「没传」与「传了 false」，
// 没有这个助手就写不出「显式 false」这一类种子。
func onb(v bool) *bool { return &v }

func callGetSetting(t *testing.T, e *env, mid int64) (*rpc.UserSettingInfo, error) {
	t.Helper()
	return NewGetUserSettingLogic(bg(), e.svc).GetUserSetting(&rpc.GetUserSettingReq{Mid: mid})
}

func callUpdateSetting(t *testing.T, e *env, in *rpc.UpdateUserSettingReq) (*rpc.UserSettingInfo, error) {
	t.Helper()
	return NewUpdateUserSettingLogic(bg(), e.svc).UpdateUserSetting(in)
}

// updateSetting 写一次偏好并当场要求成功：本文件里「写失败」永远不是可接受的结果。
func updateSetting(t *testing.T, e *env, in *rpc.UpdateUserSettingReq, label string) *rpc.UserSettingInfo {
	t.Helper()
	got, err := callUpdateSetting(t, e, in)
	return wantOK(t, got, err, label)
}

// settingRow 回库里那一行，并当场要求它在（「读不到自己的写」不能算通过）。
func settingRow(t *testing.T, e *env, mid int64) model.UserSetting {
	t.Helper()
	row, ok := e.st.settings[mid]
	if !ok {
		t.Fatalf("库里没有 mid=%d 的偏好行（实际行数 %d）", mid, len(e.st.settings))
	}
	return row
}

// requireSettingInfo 把「返回值」与「库里的行」逐位对齐：
// 两个来源不一致时，说明投影函数与真实持久化状态漂移了。
func requireSettingInfo(t *testing.T, e *env, mid int64, got *rpc.UserSettingInfo, label string) {
	t.Helper()
	row := settingRow(t, e, mid)
	if got.Mid != mid {
		t.Fatalf("%s：返回的 mid=%d 与请求的 %d 不符", label, got.Mid, mid)
	}
	if int32(got.AllowFrom) != row.AllowFrom {
		t.Fatalf("%s：allow_from 返回值 %d 与库里 %d 不一致", label, got.AllowFrom, row.AllowFrom)
	}
	if got.RejectStranger != (row.RejectStranger != 0) {
		t.Fatalf("%s：reject_stranger 返回值 %v 与库里 %d 不一致", label, got.RejectStranger, row.RejectStranger)
	}
	if got.KeywordFilter != (row.KeywordFilter != 0) {
		t.Fatalf("%s：keyword_filter 返回值 %v 与库里 %d 不一致", label, got.KeywordFilter, row.KeywordFilter)
	}
	if got.MuteConversation != (row.MuteConversation != 0) {
		t.Fatalf("%s：mute_conversation 返回值 %v 与库里 %d 不一致", label, got.MuteConversation, row.MuteConversation)
	}
	if got.Mtime != row.UpdatedAt {
		t.Fatalf("%s：mtime %d 与库里 updated_at %d 不一致", label, got.Mtime, row.UpdatedAt)
	}
}

// seedDirtySetting 先按合法路径写一行，再把 allow_from 直接改成非法值。
// 之所以要绕过 Upsert：Upsert 自己就拒绝非法枚举（model/pm_user_setting.go:134），
// 库里存在脏值只能是历史数据/DBA 手改的结果——这正是 normalizeSetting 存在的理由。
func seedDirtySetting(t *testing.T, e *env, mid int64, allowFrom int32) {
	t.Helper()
	row := defaultSettingFor(e.svc, mid)
	if err := e.svc.Settings.Upsert(bg(), row); err != nil {
		t.Fatalf("seed 偏好行：%v", err)
	}
	cur := settingRow(t, e, mid)
	cur.AllowFrom = allowFrom
	e.st.settings[mid] = cur
}

// TestGetUserSettingSynthesizesDefaultWithoutWriting 钉住「缺行不落库」这条设计：
// 从未设置过的用户拿到站点级缺省，而且库里一行都不多。
func TestGetUserSettingSynthesizesDefaultWithoutWriting(t *testing.T) {
	e := newEnv(t)
	before := e.probe()
	mark := e.markCalls()

	got, err := callGetSetting(t, e, bob)
	wantOK(t, got, err, "未设置用户读偏好")

	cfg := e.svc.Config.PrivateMessage
	if got.Mid != bob {
		t.Fatalf("返回 mid=%d，应为 %d", got.Mid, bob)
	}
	if got.AllowFrom != rpc.AllowFrom(cfg.DefaultAllowFrom) {
		t.Fatalf("缺行应按站点级默认回 %d，实得 %v", cfg.DefaultAllowFrom, got.AllowFrom)
	}
	if !got.RejectStranger {
		t.Fatal("RejectStrangerByDefault=true 时缺省行必须是拒收态：否则未设置用户比设置了的更容易被骚扰")
	}
	if !got.KeywordFilter {
		t.Fatal("KeywordFilterEnabled=true 时缺省行应显示为已启用过滤")
	}
	if got.MuteConversation {
		t.Fatalf("免打扰不该由站点级开关决定，实得 %v", got.MuteConversation)
	}
	// mtime=0 是「从未设置过」的唯一载体：读路径补写就会让它恒为正，两种状态再也分不开。
	if got.Mtime != 0 {
		t.Fatalf("未设置用户的 mtime 应为 0，实得 %d（读路径放大了写？）", got.Mtime)
	}
	requireOnlyHandleRead(t, mark, "Settings", "读偏好")
	if n := mark.callsOf("Settings.Upsert"); n != 0 {
		t.Fatalf("读偏好写了 %d 次 Upsert：读路径不得放大写", n)
	}
	if len(e.st.settings) != 0 {
		t.Fatalf("读偏好后库里多出 %d 行偏好", len(e.st.settings))
	}
	e.requireSameRowsAndWrites(t, before, "未设置用户读偏好")
}

// TestGetUserSettingNormalizesDirtyAllowFromWithoutPersisting 钉住枚举归一：
// 库里 0/越界都要回成明确的 ALLOW_FROM_*，但**不回写**——归一是展示层口径，
// 把「读一次就顺手改库」当成自愈会让审计看不到数据何时被谁改过。
func TestGetUserSettingNormalizesDirtyAllowFromWithoutPersisting(t *testing.T) {
	for _, tc := range []struct {
		name  string
		dirty int32
	}{
		{name: "allow_from=0（未指定）", dirty: 0},
		{name: "allow_from=99（越界）", dirty: 99},
		{name: "allow_from=-1（负数）", dirty: -1},
	} {
		e := newEnv(t)
		seedDirtySetting(t, e, bob, tc.dirty)

		got, err := callGetSetting(t, e, bob)
		wantOK(t, got, err, tc.name)
		if got.AllowFrom != rpc.AllowFrom(model.AllowFromFollowed) {
			t.Fatalf("%s：应归一到站点级默认 2，实得 %v", tc.name, got.AllowFrom)
		}
		if int32(got.AllowFrom) == 0 {
			t.Fatalf("%s：把 UNSPECIFIED 透给了客户端", tc.name)
		}
		if stored := settingRow(t, e, bob); stored.AllowFrom != tc.dirty {
			t.Fatalf("%s：读路径把脏值回写成 %d（应为原样保留 %d）", tc.name, stored.AllowFrom, tc.dirty)
		}
		// 其余三位不能被归一顺手改掉：它们本来就是合法取值。
		if row := settingRow(t, e, bob); row.RejectStranger != 1 || row.KeywordFilter != 1 {
			t.Fatalf("%s：归一 allow_from 时改动了其它位：reject=%d keyword=%d",
				tc.name, row.RejectStranger, row.KeywordFilter)
		}
	}
}

// TestNormalizeSettingFallsBackWhenSiteDefaultIsInvalid 按纯函数钉住兜底的另一支：
// 站点级默认本身非法时（配置写错）必须回 AllowFromAnyone，而不是把非法值透出去。
func TestNormalizeSettingFallsBackWhenSiteDefaultIsInvalid(t *testing.T) {
	e := newEnv(t)
	e.svc.Config.PrivateMessage.DefaultAllowFrom = 7 // 不存在的档位
	row := &model.UserSetting{Mid: bob, AllowFrom: 0}
	normalizeSetting(e.svc, row)
	if row.AllowFrom != model.AllowFromAnyone {
		t.Fatalf("站点默认非法时应退到 ALLOW_FROM_ANYONE，实得 %d", row.AllowFrom)
	}
	// 合法值必须原样通过：归一不是「一律改成默认」。
	for _, v := range []int32{model.AllowFromAnyone, model.AllowFromFollowed, model.AllowFromMutual, model.AllowFromNone} {
		keep := &model.UserSetting{Mid: bob, AllowFrom: v}
		normalizeSetting(e.svc, keep)
		if keep.AllowFrom != v {
			t.Fatalf("合法档位 %d 被改成 %d", v, keep.AllowFrom)
		}
	}
}

// TestGetUserSettingIsScopedToRequestedMid 钉住「按 mid 切分」这层数据隔离，
// 同时钉住本入口的可信度缺口：mid 完全自报，服务侧不做任何身份校验。
//
// 缺陷：services/private-message/internal/logic/getusersettinglogic.go:35 —— mid 取自请求体，
// 同文件 :29 的注释声明「越权在网关拦截」，但 gateway/app/api/app.api:3118 起的私信路由
// 没有挂 jwt、mid 是 form 参数（gateway/app/internal/logic 各 pm logic 直接转发 req.Mid），
// 因此本服务在进程内的真实行为是「任何调用者都能读/写任意 mid 的私信偏好」：
// 读走别人的反骚扰档位，写则可以直接把对方的 allow_from 改成 ANYONE（等价于替别人关闭反骚扰）。
// 修法方向：mid 由网关按会话凭证注入并与之强校验，本服务只信 ctx；README 里那条「网关拦截」
// 的前提交付必须落到配置上（jwt 中间件）而不是注释。
//
// ⚠ 改生产代码前不要动这条断言的期望。
func TestGetUserSettingIsScopedToRequestedMid(t *testing.T) {
	e := newEnv(t)
	// bob 显式设置成「关闭私信」，alice 显式设置成「仅互关」。
	updateSetting(t, e, &rpc.UpdateUserSettingReq{Mid: bob, AllowFrom: rpc.AllowFrom_ALLOW_FROM_NONE}, "bob 设档位")
	updateSetting(t, e, &rpc.UpdateUserSettingReq{
		Mid: alice, AllowFrom: rpc.AllowFrom_ALLOW_FROM_MUTUAL, RejectStranger: onb(false),
	}, "alice 设档位")

	bobRow, err := callGetSetting(t, e, bob)
	wantOK(t, bobRow, err, "bob 读自己的偏好")
	if bobRow.AllowFrom != rpc.AllowFrom_ALLOW_FROM_NONE || bobRow.RejectStranger != true {
		t.Fatalf("bob 的偏好被 alice 的写入污染：%v reject=%v", bobRow.AllowFrom, bobRow.RejectStranger)
	}
	aliceRow, err := callGetSetting(t, e, alice)
	wantOK(t, aliceRow, err, "alice 读自己的偏好")
	if aliceRow.AllowFrom != rpc.AllowFrom_ALLOW_FROM_MUTUAL || aliceRow.RejectStranger != false {
		t.Fatalf("alice 的偏好与她的写入不符：%v reject=%v", aliceRow.AllowFrom, aliceRow.RejectStranger)
	}
	// 从未设置的第三方：既不是 bob 的档位也不是 alice 的，只能是站点级缺省。
	carolRow, err := callGetSetting(t, e, carol)
	wantOK(t, carolRow, err, "carol 读偏好")
	if carolRow.AllowFrom != rpc.AllowFrom_ALLOW_FROM_FOLLOWED || carolRow.Mtime != 0 {
		t.Fatalf("carol 拿到了别人的偏好：%v mtime=%d", carolRow.AllowFrom, carolRow.Mtime)
	}
	// 一行对应一人：三个 mid 只该有两行（carol 从未设置，读不落库）。
	if len(e.st.settings) != 2 {
		t.Fatalf("偏好行数应为 2（bob/alice），实得 %d", len(e.st.settings))
	}

	// 自报 mid 即可读走别人的策略事实（当前生产行为，非期望）。
	stolen, err := callGetSetting(t, e, bob)
	wantOK(t, stolen, err, "无凭证上下文自报 mid=bob 读偏好")
	if stolen.AllowFrom != rpc.AllowFrom_ALLOW_FROM_NONE || stolen.Mid != bob {
		t.Fatalf("自报 mid 的读取结果与 bob 的真值不同（%v/%d）：本用例前提已变化，需同步改期望",
			stolen.AllowFrom, stolen.Mid)
	}
	// 更坏的一支：同一个自报 mid 还能替 bob 把反骚扰门槛改成「所有人」。
	// 期望值钉的是「改得动」——一旦服务侧改成只信可信身份，这条必然红。
	hijacked := updateSetting(t, e, &rpc.UpdateUserSettingReq{
		Mid: bob, AllowFrom: rpc.AllowFrom_ALLOW_FROM_ANYONE, RejectStranger: onb(false),
	}, "自报 mid=bob 改写偏好")
	if hijacked.AllowFrom != rpc.AllowFrom_ALLOW_FROM_ANYONE || hijacked.RejectStranger {
		t.Fatalf("替别人改写偏好的结果与请求不符（%v reject=%v）：期望需随生产修复一起更新",
			hijacked.AllowFrom, hijacked.RejectStranger)
	}
	requireSettingInfo(t, e, bob, hijacked, "被改写后的 bob 偏好")
	// alice 那一行没被顺手改掉：越权面的大小就体现在「只有 mid 那一行动了」。
	if aliceStill := settingRow(t, e, alice); aliceStill.AllowFrom != model.AllowFromMutual || aliceStill.RejectStranger != 0 {
		t.Fatalf("改 bob 的偏好串到了 alice：allow_from=%d reject=%d", aliceStill.AllowFrom, aliceStill.RejectStranger)
	}
}

// TestUpdateUserSettingKeepsBaselineForUnsetFields 钉住局部更新：
// UNSPECIFIED / 未传的 optional 位必须保持基线，显式 false 才覆盖。
func TestUpdateUserSettingKeepsBaselineForUnsetFields(t *testing.T) {
	e := newEnv(t)

	// 第一步：只改 allow_from，其余三位应落到「站点级缺省」而不是零值。
	first, err := callUpdateSetting(t, e, &rpc.UpdateUserSettingReq{
		Mid: bob, AllowFrom: rpc.AllowFrom_ALLOW_FROM_NONE,
	})
	wantOK(t, first, err, "首次写偏好")
	requireSettingInfo(t, e, bob, first, "首次写偏好")
	if first.RejectStranger != true || first.KeywordFilter != true || first.MuteConversation != false {
		t.Fatalf("基线未按站点级缺省合成：reject=%v keyword=%v mute=%v",
			first.RejectStranger, first.KeywordFilter, first.MuteConversation)
	}
	if first.Mtime <= 0 {
		t.Fatalf("显式写过之后 mtime 必须为正，实得 %d", first.Mtime)
	}
	created := settingRow(t, e, bob).CreatedAt

	// 第二步：只开免打扰。allow_from=UNSPECIFIED 且另两位没传 → 全部保持。
	second, err := callUpdateSetting(t, e, &rpc.UpdateUserSettingReq{
		Mid: bob, MuteConversation: onb(true),
	})
	wantOK(t, second, err, "只改免打扰")
	requireSettingInfo(t, e, bob, second, "只改免打扰")
	if second.AllowFrom != rpc.AllowFrom_ALLOW_FROM_NONE {
		t.Fatalf("UNSPECIFIED 把 allow_from 改了：%v", second.AllowFrom)
	}
	if !second.RejectStranger || !second.KeywordFilter {
		t.Fatalf("未传的开关被零值覆盖：reject=%v keyword=%v（用户没碰过它们）", second.RejectStranger, second.KeywordFilter)
	}
	if !second.MuteConversation {
		t.Fatal("显式 true 没写进去")
	}

	// 第三步：显式 false 必须生效——「传 false」与「没传」是两件事。
	third, err := callUpdateSetting(t, e, &rpc.UpdateUserSettingReq{
		Mid: bob, RejectStranger: onb(false), KeywordFilter: onb(false),
	})
	wantOK(t, third, err, "显式关闭两个开关")
	requireSettingInfo(t, e, bob, third, "显式关闭两个开关")
	if third.RejectStranger || third.KeywordFilter {
		t.Fatalf("显式 false 被当成没传：reject=%v keyword=%v", third.RejectStranger, third.KeywordFilter)
	}
	if !third.MuteConversation || third.AllowFrom != rpc.AllowFrom_ALLOW_FROM_NONE {
		t.Fatalf("这一步动了上一步的位：mute=%v allow_from=%v", third.MuteConversation, third.AllowFrom)
	}
	// created_at 不动（ON DUPLICATE KEY UPDATE 不改插入时刻）：它区分「账号创建即有偏好」与「后来改的」。
	row := settingRow(t, e, bob)
	if row.CreatedAt != created {
		t.Fatalf("三次更新把 created_at 从 %d 改成了 %d", created, row.CreatedAt)
	}
	if len(e.st.settings) != 1 {
		t.Fatalf("三次更新应落在同一行上，实得 %d 行", len(e.st.settings))
	}
}

// TestUpdateUserSettingIsIdempotentSingleRow 钉住「同一份请求重放不产生第二行」：
// pm_user_setting 主键是 mid，重复提交必须收敛成一行且值等于最后一次。
func TestUpdateUserSettingIsIdempotentSingleRow(t *testing.T) {
	e := newEnv(t)
	in := &rpc.UpdateUserSettingReq{
		Mid: bob, AllowFrom: rpc.AllowFrom_ALLOW_FROM_MUTUAL, RejectStranger: onb(true), MuteConversation: onb(true),
	}
	mark := e.markCalls()
	txm := e.txMark()
	var last *rpc.UserSettingInfo
	for i := 0; i < 3; i++ {
		got, err := callUpdateSetting(t, e, in)
		wantOK(t, got, err, "重放偏好写入")
		last = got
	}
	if last.AllowFrom != rpc.AllowFrom_ALLOW_FROM_MUTUAL {
		t.Fatalf("重放后的档位不是最后一次的值：%v", last.AllowFrom)
	}
	if len(e.st.settings) != 1 {
		t.Fatalf("三次重放产生了 %d 行", len(e.st.settings))
	}
	if n := mark.callsOf("Settings.Upsert"); n != 3 {
		t.Fatalf("Upsert 次数应为 3（幂等靠唯一键而不是少写），实得 %d", n)
	}
	// 单行 upsert 不需要事务；出现事务说明这次写跨了表，那才需要原子性。
	if n := e.txsSince(txm); n != 0 {
		t.Fatalf("偏好写入不应开事务，实得 %d 个", n)
	}
	requireOnlyHandleRead(t, mark, "Settings", "偏好重放写入")
}

// TestUpdateUserSettingRejectsInvalidAllowFromWithoutWriting 钉住非法档位的拒绝点：
// 必须在任何一次 Upsert 之前，且错误是 ErrInvalidAllowFrom 而不是 model 层的兜底。
func TestUpdateUserSettingRejectsInvalidAllowFromWithoutWriting(t *testing.T) {
	e := newEnv(t)
	before := e.probe()

	for _, tc := range []struct {
		name string
		af   rpc.AllowFrom
	}{
		{name: "allow_from=7", af: rpc.AllowFrom(7)},
		{name: "allow_from=-1", af: rpc.AllowFrom(-1)},
	} {
		mark := e.markCalls()
		got, err := callUpdateSetting(t, e, &rpc.UpdateUserSettingReq{Mid: bob, AllowFrom: tc.af})
		wantErr(t, err, model.ErrInvalidAllowFrom, tc.name)
		if got != nil {
			t.Fatalf("%s：被拒却回了偏好体 %v", tc.name, got)
		}
		// 基线读取先发生（loadSettingOrDefault 在枚举校验之前），写一次都不许发生。
		if n := mark.callsOf("Settings.Upsert"); n != 0 {
			t.Fatalf("%s：非法档位仍写了 %d 次", tc.name, n)
		}
		if len(e.st.settings) != 0 {
			t.Fatalf("%s：非法档位仍落了 %d 行", tc.name, len(e.st.settings))
		}
		e.requireSameRowsAndWrites(t, before, tc.name)
	}
	// UNSPECIFIED（0）是「不修改」而不是「非法」：同一条 if 必须区分这两件事。
	got, err := callUpdateSetting(t, e, &rpc.UpdateUserSettingReq{Mid: bob, AllowFrom: rpc.AllowFrom_ALLOW_FROM_UNSPECIFIED})
	wantOK(t, got, err, "UNSPECIFIED 表示不修改")
	if got.AllowFrom != rpc.AllowFrom(e.svc.Config.PrivateMessage.DefaultAllowFrom) {
		t.Fatalf("UNSPECIFIED 应落到站点级默认，实得 %v", got.AllowFrom)
	}
	// 错误文案里带上被拒的取值：运维排障要能看出是哪个档位被拒（哨兵本身不含值）。
	_, err = callUpdateSetting(t, e, &rpc.UpdateUserSettingReq{Mid: bob, AllowFrom: rpc.AllowFrom(42)})
	if err == nil || !strings.Contains(err.Error(), "allow_from=42") {
		t.Fatalf("非法档位的错误里没有取值线索：%v", err)
	}
}

// TestUpdateUserSettingHonorsSiteLevelKeywordFilterSwitch 钉住站点级开关的压制：
// KeywordFilterEnabled=false 的环境里，任何用户都写不回 1（含「显式 true」）。
func TestUpdateUserSettingHonorsSiteLevelKeywordFilterSwitch(t *testing.T) {
	e := newEnv(t)
	e.svc.Config.PrivateMessage.KeywordFilterEnabled = false

	got, err := callUpdateSetting(t, e, &rpc.UpdateUserSettingReq{Mid: bob, KeywordFilter: onb(true)})
	wantOK(t, got, err, "站点关闭过滤时显式写 true")
	if got.KeywordFilter {
		t.Fatal("站点级关掉过滤后被单用户写回 1：环境里出现「个别行仍在过滤」的假事实")
	}
	if row := settingRow(t, e, bob); row.KeywordFilter != 0 {
		t.Fatalf("库里 keyword_filter=%d，应为 0", row.KeywordFilter)
	}
	// 未设置用户的缺省也必须是 0：否则「关掉的过滤」在读路径上又活了。
	def, err := callGetSetting(t, e, alice)
	wantOK(t, def, err, "站点关闭过滤时读缺省")
	if def.KeywordFilter {
		t.Fatal("缺省行的 keyword_filter 在站点关闭过滤后仍为 true")
	}

	// 开关打开后不继承上一条假事实：同一个 mid 显式写 true 应当生效。
	e.svc.Config.PrivateMessage.KeywordFilterEnabled = true
	after, err := callUpdateSetting(t, e, &rpc.UpdateUserSettingReq{Mid: bob, KeywordFilter: onb(true)})
	wantOK(t, after, err, "重新打开过滤后写 true")
	if !after.KeywordFilter {
		t.Fatal("重新打开站点开关后仍写不进 1")
	}
}

// TestUpdateUserSettingRecheckFailureIsSurfaced 钉住那条只有真库才能出现的防御分支：
// Upsert 成功但回查不到（读到的是一致性副本库/行被并发删掉）。
// 这里必须报错而不是回一份「内存里合并出来的偏好」——那等于把一次没有落地证据的写报成成功。
func TestUpdateUserSettingRecheckFailureIsSurfaced(t *testing.T) {
	e := newEnv(t)
	// 第一次写正常落库，随后把「回查看不到」打开：命中 UpdateUserSetting 末尾的 FindByMid。
	updateSetting(t, e, &rpc.UpdateUserSettingReq{Mid: bob, AllowFrom: rpc.AllowFrom_ALLOW_FROM_MUTUAL}, "建立基线行")
	e.st.hideSettingRow = true
	got, err := callUpdateSetting(t, e, &rpc.UpdateUserSettingReq{Mid: bob, AllowFrom: rpc.AllowFrom_ALLOW_FROM_NONE})
	// 这条错误不是哨兵而是 fmt.Errorf（helpers 里同类「服务端自证失败」都这么做），
	// 因此只能按文案钉：mid 是排障唯一可用的定位信息（AGENTS.md §7：错误只带主键）。
	if err == nil {
		t.Fatalf("回查不到却报成功：%v", got)
	}
	if got != nil {
		t.Fatalf("失败时仍回了偏好体：%v", got)
	}
	if !strings.Contains(err.Error(), "mid=202") {
		t.Fatalf("回查失败的错误里没有 mid 线索：%v", err)
	}
	// 当前行为：这次写不回滚（单语句 upsert，无跨表原子性要求），行仍在。
	if row := settingRow(t, e, bob); row.AllowFrom != model.AllowFromNone {
		t.Fatalf("被报错的这次写入把库改成了 allow_from=%d（本用例的前提是「写成功只是读不到」）", row.AllowFrom)
	}
	e.st.hideSettingRow = false

	// 同一条路径的另一种形态：库里压根没这一行时 FindByMid 回 (nil,nil) 也走同一分支。
	// 用 hideSettingRow 已覆盖，这里改测「读偏好」侧：同一张表读不到时是缺省而不是错误。
	def, err := callGetSetting(t, e, carol)
	wantOK(t, def, err, "缺行时读偏好")
	if def.Mid != carol || def.Mtime != 0 {
		t.Fatalf("缺行应合成 mid=%d 的缺省（mtime=0），实得 mid=%d mtime=%d", carol, def.Mid, def.Mtime)
	}
}

// TestSettingGuardsAndPropagatesDependencyErrors 钉住两个入口的守卫顺序与错误上抛。
func TestSettingGuardsAndPropagatesDependencyErrors(t *testing.T) {
	e := newEnv(t)
	for _, name := range []string{"GetUserSetting", "UpdateUserSetting"} {
		for _, mid := range []int64{0, -3} {
			mark := e.markCalls()
			var err error
			if name == "GetUserSetting" {
				_, err = callGetSetting(t, e, mid)
			} else {
				_, err = callUpdateSetting(t, e, &rpc.UpdateUserSettingReq{Mid: mid, AllowFrom: rpc.AllowFrom_ALLOW_FROM_ANYONE})
			}
			wantErr(t, err, model.ErrInvalidMid, name+" 非法 mid")
			// 守卫先于任何一次表访问：非法 mid 连「合成缺省」的读都不该发生。
			if n := mark.handleCallsOf("Settings"); n != 0 {
				t.Fatalf("%s：mid=%d 仍在守卫之后读了偏好表 %d 次", name, mid, n)
			}
		}
	}

	// 依赖错误原样上抛（两个入口都走 Settings.FindByMid）。
	e.st.failTx["Settings.FindByMid"] = errInjected
	_, err := callGetSetting(t, e, bob)
	wantFail(t, err, errInjected, "GetUserSetting 读偏好失败")
	_, err = callUpdateSetting(t, e, &rpc.UpdateUserSettingReq{Mid: bob, AllowFrom: rpc.AllowFrom_ALLOW_FROM_ANYONE})
	wantFail(t, err, errInjected, "UpdateUserSetting 取基线失败")
	if len(e.st.settings) != 0 {
		t.Fatalf("基线读取失败后仍落了行：%d", len(e.st.settings))
	}
	delete(e.st.failTx, "Settings.FindByMid")

	// 写入失败原样上抛，且不回查、不假装成功。
	e.st.failTx["Settings.Upsert"] = errInjected
	upsertMark := e.markCalls()
	got, err := callUpdateSetting(t, e, &rpc.UpdateUserSettingReq{Mid: bob, AllowFrom: rpc.AllowFrom_ALLOW_FROM_ANYONE})
	wantFail(t, err, errInjected, "UpdateUserSetting 写入失败")
	if got != nil {
		t.Fatalf("写入失败仍回了偏好体：%v", got)
	}
	if n := upsertMark.callsOf("Settings.Upsert"); n != 1 {
		t.Fatalf("失败路径应只尝试一次写入，实得 %d 次", n)
	}
	if len(e.st.settings) != 0 {
		t.Fatalf("被拒的写入仍落了 %d 行", len(e.st.settings))
	}
	delete(e.st.failTx, "Settings.Upsert")

	// 撤销注入后一次正常写入必须落地：证明上面的失败序列没把库改坏、也没留半成品。
	fresh := updateSetting(t, e, &rpc.UpdateUserSettingReq{Mid: bob, AllowFrom: rpc.AllowFrom_ALLOW_FROM_NONE}, "注入撤销后写偏好")
	requireSettingInfo(t, e, bob, fresh, "注入撤销后的偏好")
	if fresh.MuteConversation {
		t.Fatal("重放路径把没传过的免打扰位带上了 true")
	}
}
