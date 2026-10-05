package logic

// 权益判定（CheckEntitlement / CheckEntitlements）的口径测试。
//
// 这里守的是全站最贵的一条不变量（AGENTS.md §1 资金语义）：
//   granted=true 只能来自 mb_membership 里真实存在、且 expire_at > now 的那一行
//   （那一行只能由 GrantMembership 的事务写入并留有 mb_grant 台账）；
//   读不出可信结论时（DB 故障）必须是 error，绝不能塌缩成「未开通」的正常结论——
//   否则播放端拿到 granted=false 会以为用户没买，而事实是数据库读不动。
//
// 判定与库之间的接缝是 svc.ServiceContext 的接口字段，因此这里注入内存假实现即可，
// 不需要数据库，也不需要放宽被测代码。

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go-video/services/membership/model"
	"go-video/services/membership/rpc"
)

// errCatalogDown / errMembersDown 是两个读路径各自的故障注入点。
// 用可辨识的哨兵值，断言「错误原样上抛」而不是「变成了一个 false」。
var (
	errCatalogDown = errors.New("fake: mb_entitlement read failed")
	errMembersDown = errors.New("fake: mb_membership read failed")
)

const (
	hdCode       = "vip.high_bitrate"
	ultraCode    = "vip.ultra_4k"
	disabledCode = "vip.legacy_offline"
	unseededCode = "vip.never_defined"
	testDay      = model.SecondsPerDay
)

// seedCatalog 放三个码：要求大会员的高码率、要求超级大会员的 4K、以及一个已下线的历史码。
func seedCatalog(t *testing.T, db *fakeDB) {
	t.Helper()
	now := nowSec()
	seedEntitlement(db, &model.Entitlement{
		Code: hdCode, Name: "高码率", MinVipType: model.VipTypePremium, Enabled: 1,
		Ctime: now - 100*testDay, Mtime: now - 100*testDay,
	})
	seedEntitlement(db, &model.Entitlement{
		Code: ultraCode, Name: "超清 4K", MinVipType: model.VipTypePremiumPlus, Enabled: 1,
		Ctime: now - 100*testDay, Mtime: now - 100*testDay,
	})
	seedEntitlement(db, &model.Entitlement{
		Code: disabledCode, Name: "已下线能力", MinVipType: model.VipTypePremium, Enabled: 0,
		Ctime: now - 100*testDay, Mtime: now - 100*testDay,
	})
}

// -- CheckEntitlement --

func TestCheckEntitlementGrantedOnlyFromStoredRow(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedCatalog(t, db)
	now := nowSec()
	held := seedMembership(db, &model.Membership{
		Mid: 9001, VipType: model.VipTypePremium,
		StartAt: now - 40*testDay, ExpireAt: now + 30*testDay,
		Source: model.GrantSourceSandboxPurchase, PaidMonthCount: 1,
	})

	got, err := NewCheckEntitlementLogic(context.Background(), svcCtx).CheckEntitlement(
		&rpc.CheckEntitlementReq{Mid: 9001, Code: hdCode})
	if err != nil {
		t.Fatalf("判定不该报错，得到 %v", err)
	}
	if !got.GetGranted() || got.GetReason() != rpc.EntitlementReason_ENTITLEMENT_GRANTED {
		t.Errorf("有生效行却判成 %v/%v，期望 GRANTED", got.GetReason(), got.GetGranted())
	}
	if got.GetExpireAt() != held.ExpireAt {
		t.Errorf("reply.expire_at = %d，期望回显判定依据行 %d", got.GetExpireAt(), held.ExpireAt)
	}
	if got.GetVipType() != rpc.VipType_VIP_TYPE_PREMIUM {
		t.Errorf("reply.vip_type = %v，期望 %v", got.GetVipType(), rpc.VipType_VIP_TYPE_PREMIUM)
	}

	// 同一档位没被开通的用户（库里 9002 一行都没有）必须是 NO_MEMBERSHIP，
	// 而不是与「过期」混成一类——两个结论引导的动作不同（开通 vs 续费）。
	got2, err := NewCheckEntitlementLogic(context.Background(), svcCtx).CheckEntitlement(
		&rpc.CheckEntitlementReq{Mid: 9002, Code: hdCode})
	if err != nil {
		t.Fatalf("未开通是结论不是错误: %v", err)
	}
	if got2.GetGranted() || got2.GetReason() != rpc.EntitlementReason_ENTITLEMENT_NO_MEMBERSHIP {
		t.Errorf("从未开通却判成 %v", got2.GetReason())
	}
	if got2.GetExpireAt() != 0 || got2.GetVipType() != rpc.VipType_VIP_TYPE_UNSPECIFIED {
		t.Errorf("从未开通却编造出依据 expire_at=%d vip_type=%v", got2.GetExpireAt(), got2.GetVipType())
	}
}

// TestCheckEntitlementReadFailureIsErrorNotFalse 是本轮最关键的一条：
// 「读不动」与「没开通」必须分家。任何把 DB 错误折叠成 granted=false 的写法
// 都会让播放端在数据库抖动时把付费用户当成免费用户。
func TestCheckEntitlementReadFailureIsErrorNotFalse(t *testing.T) {
	cases := []struct {
		name  string
		setup func(db *fakeDB)
		want  error
	}{
		{
			name: "权益码目录读失败",
			setup: func(db *fakeDB) {
				db.entErr = errCatalogDown
				db.memberErr = nil
			},
			want: errCatalogDown,
		},
		{
			name: "会员身份读失败",
			setup: func(db *fakeDB) {
				db.entErr = nil
				db.memberErr = errMembersDown
			},
			want: errMembersDown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svcCtx, db := newTestSvc(t)
			seedCatalog(t, db)
			activePremium(db, 9101)
			tc.setup(db)

			got, err := NewCheckEntitlementLogic(context.Background(), svcCtx).CheckEntitlement(
				&rpc.CheckEntitlementReq{Mid: 9101, Code: hdCode})
			if !errors.Is(err, tc.want) {
				t.Fatalf("错误必须原样上抛，得到 %v，期望 %v", err, tc.want)
			}
			if got != nil {
				t.Errorf("读失败时不得返回任何结论结构（否则调用方会当成正常判定），得到 %+v", got)
			}
		})
	}
}

// TestCheckEntitlementTierNotEnoughEchoesEvidence：档位不足是「有会员但不够」，
// 必须与「没会员」区分，并把实际持有档位与到期时间带回去，客户端才能引导升级。
func TestCheckEntitlementTierNotEnoughEchoesEvidence(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedCatalog(t, db)
	now := nowSec()
	held := seedMembership(db, &model.Membership{
		Mid: 9201, VipType: model.VipTypePremium,
		StartAt: now - 10*testDay, ExpireAt: now + 20*testDay,
	})

	got, err := NewCheckEntitlementLogic(context.Background(), svcCtx).CheckEntitlement(
		&rpc.CheckEntitlementReq{Mid: 9201, Code: ultraCode})
	if err != nil {
		t.Fatalf("档位不足是结论不是错误: %v", err)
	}
	if got.GetGranted() {
		t.Error("要求超级大会员的码被大会员判成了通过")
	}
	if got.GetReason() != rpc.EntitlementReason_ENTITLEMENT_TIER_NOT_ENOUGH {
		t.Errorf("reason = %v，期望 TIER_NOT_ENOUGH", got.GetReason())
	}
	if got.GetExpireAt() != held.ExpireAt || got.GetVipType() != rpc.VipType(held.VipType) {
		t.Errorf("未回显实际依据：expire_at=%d vip_type=%v，期望 %d / %v",
			got.GetExpireAt(), got.GetVipType(), held.ExpireAt, rpc.VipType(held.VipType))
	}
}

// TestCheckEntitlementHigherTierCoversLower：超级大会员拥有大会员全部权益（档位序为真）。
func TestCheckEntitlementHigherTierCoversLower(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedCatalog(t, db)
	now := nowSec()
	plus := seedMembership(db, &model.Membership{
		Mid: 9301, VipType: model.VipTypePremiumPlus,
		StartAt: now - 5*testDay, ExpireAt: now + 5*testDay,
	})

	got, err := NewCheckEntitlementLogic(context.Background(), svcCtx).CheckEntitlement(
		&rpc.CheckEntitlementReq{Mid: 9301, Code: hdCode})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !got.GetGranted() || got.GetVipType() != rpc.VipType_VIP_TYPE_PREMIUM_PLUS ||
		got.GetExpireAt() != plus.ExpireAt {
		t.Errorf("高档未覆盖低档权益：%+v", got)
	}
}

// TestCheckEntitlementExpiredKeepsEvidence：已过期要报 EXPIRED 并把到期时间带回去，
// 让「我的会员」页能显示「已于 X 到期」，而不是伪装成从未开通。
func TestCheckEntitlementExpiredKeepsEvidence(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedCatalog(t, db)
	now := nowSec()
	expired := seedMembership(db, &model.Membership{
		Mid: 9401, VipType: model.VipTypePremiumPlus,
		StartAt: now - 100*testDay, ExpireAt: now - testDay,
	})

	got, err := NewCheckEntitlementLogic(context.Background(), svcCtx).CheckEntitlement(
		&rpc.CheckEntitlementReq{Mid: 9401, Code: hdCode})
	if err != nil {
		t.Fatalf("过期是结论不是错误: %v", err)
	}
	if got.GetGranted() || got.GetReason() != rpc.EntitlementReason_ENTITLEMENT_EXPIRED {
		t.Errorf("reason = %v granted = %v，期望 EXPIRED/false", got.GetReason(), got.GetGranted())
	}
	if got.GetExpireAt() != expired.ExpireAt {
		t.Errorf("已过期未回显到期时间：%d != %d", got.GetExpireAt(), expired.ExpireAt)
	}
}

// TestCheckEntitlementActiveLowerTierWinsOverExpiredHigherTier：
// 判定基准是「生效中的最高档」，过期的更高档不能压过生效的较低档，
// 也不能让过期行把结论依据的 expire_at 换掉。
func TestCheckEntitlementActiveLowerTierWinsOverExpiredHigherTier(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedCatalog(t, db)
	now := nowSec()
	active := seedMembership(db, &model.Membership{
		Mid: 9451, VipType: model.VipTypePremium, ExpireAt: now + 3*testDay, StartAt: now - 30*testDay,
	})
	seedMembership(db, &model.Membership{
		Mid: 9451, VipType: model.VipTypePremiumPlus, ExpireAt: now - 7*testDay, StartAt: now - 200*testDay,
	})

	got, err := NewCheckEntitlementLogic(context.Background(), svcCtx).CheckEntitlement(
		&rpc.CheckEntitlementReq{Mid: 9451, Code: hdCode})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !got.GetGranted() {
		t.Errorf("有生效的大会员却判成 %v", got.GetReason())
	}
	if got.GetExpireAt() != active.ExpireAt || got.GetVipType() != rpc.VipType_VIP_TYPE_PREMIUM {
		t.Errorf("依据行取错：expire_at=%d vip_type=%v，期望 %d / 大会员",
			got.GetExpireAt(), got.GetVipType(), active.ExpireAt)
	}
}

// TestMidInvalidIsAConclusionNotAnError：游客态是正常业务态。
// 除了结论必须是 MID_INVALID，还要证明它一次库都没读——
// 否则「未登录」会被放大成每帧一次 DB 扫描。
func TestMidInvalidIsAConclusionNotAnError(t *testing.T) {
	for _, mid := range []int64{0, -1, -9223372036854775808} {
		t.Run(fmt.Sprintf("mid=%d", mid), func(t *testing.T) {
			svcCtx, db := newTestSvc(t)
			seedCatalog(t, db)

			got, err := NewCheckEntitlementLogic(context.Background(), svcCtx).CheckEntitlement(
				&rpc.CheckEntitlementReq{Mid: mid, Code: hdCode})
			if err != nil {
				t.Fatalf("mid 非法必须是结论，得到错误 %v", err)
			}
			if got.GetGranted() || got.GetReason() != rpc.EntitlementReason_ENTITLEMENT_MID_INVALID {
				t.Errorf("reason = %v，期望 MID_INVALID", got.GetReason())
			}
			if db.entFindByCodeCalls != 0 || db.memberListByMidCalls != 0 {
				t.Errorf("mid 非法却读了库：目录 %d 次、身份 %d 次", db.entFindByCodeCalls, db.memberListByMidCalls)
			}
		})
	}
}

// TestCheckEntitlementEmptyCodeStillUnknown：空码等价于未知码。
// 省一次查询可以，默认放行不行（少查一次之后 granted 仍是 false）。
func TestCheckEntitlementEmptyCodeStillUnknown(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedCatalog(t, db)
	activePremium(db, 9501)

	got, err := NewCheckEntitlementLogic(context.Background(), svcCtx).CheckEntitlement(
		&rpc.CheckEntitlementReq{Mid: 9501, Code: "   "})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if got.GetGranted() || got.GetReason() != rpc.EntitlementReason_ENTITLEMENT_CODE_UNKNOWN {
		t.Errorf("空码判成 %v/%v，期望 CODE_UNKNOWN/false", got.GetReason(), got.GetGranted())
	}
	if db.entFindByCodeCalls != 0 {
		t.Errorf("空码仍去打库 %d 次（应直接给结论）", db.entFindByCodeCalls)
	}
	if db.memberListByMidCalls != 1 {
		t.Errorf("身份读次数 = %d，期望 1（结论依据仍要如实回显）", db.memberListByMidCalls)
	}
}

// TestCheckEntitlementUnknownCodeBeatsMembership：目录里没有的码，即使有生效会员也不放行。
// 这条钉的是「未知码不得默认通过」——否则打错一个常量就等于赠送权益。
func TestCheckEntitlementUnknownCodeBeatsMembership(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedCatalog(t, db)
	activePremium(db, 9601)

	got, err := NewCheckEntitlementLogic(context.Background(), svcCtx).CheckEntitlement(
		&rpc.CheckEntitlementReq{Mid: 9601, Code: unseededCode})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if got.GetGranted() || got.GetReason() != rpc.EntitlementReason_ENTITLEMENT_CODE_UNKNOWN {
		t.Errorf("未知码判成 %v/%v", got.GetReason(), got.GetGranted())
	}
	// 未知码优先于「档位不足」：目录里没有就没有要求档，不能拿用户档位去猜。
	if got.GetVipType() == rpc.VipType_VIP_TYPE_UNSPECIFIED {
		t.Error("未知码也不该丢掉依据行档位（客户端仍要显示会员状态）")
	}
}

// TestCheckEntitlementDisabledCodeIsDistinct：下线码必须判成 CODE_DISABLED，
// 与 CODE_UNKNOWN 区分开——行必须留着（删了就退化成未知）。
func TestCheckEntitlementDisabledCodeIsDistinct(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedCatalog(t, db)
	activePremium(db, 9701)

	got, err := NewCheckEntitlementLogic(context.Background(), svcCtx).CheckEntitlement(
		&rpc.CheckEntitlementReq{Mid: 9701, Code: disabledCode})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if got.GetGranted() || got.GetReason() != rpc.EntitlementReason_ENTITLEMENT_CODE_DISABLED {
		t.Errorf("下线码判成 %v/%v", got.GetReason(), got.GetGranted())
	}
}

// TestCheckEntitlementTrimsCodeBeforeLookup：proto 传进来的码常带空白，
// 不 trim 就会把合法码判成未知码。
func TestCheckEntitlementTrimsCodeBeforeLookup(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedCatalog(t, db)
	activePremium(db, 9801)

	var looked string
	wrapped := spyEntitlementLookup{
		EntitlementModel: svcCtx.Entitlement,
		onCall:           func(code string) { looked = code },
	}
	svcCtx.Entitlement = wrapped

	got, err := NewCheckEntitlementLogic(context.Background(), svcCtx).CheckEntitlement(
		&rpc.CheckEntitlementReq{Mid: 9801, Code: "  " + hdCode + "  "})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !got.GetGranted() {
		t.Errorf("带空白的合法码判成 %v", got.GetReason())
	}
	if looked != hdCode {
		t.Errorf("查目录用了未裁剪的码 %q，期望 %q", looked, hdCode)
	}
}

// spyEntitlementLookup 只加一层「观察到查了哪个码」的探针，不改任何判定。
type spyEntitlementLookup struct {
	model.EntitlementModel
	onCall func(code string)
}

func (s spyEntitlementLookup) FindByCode(ctx context.Context, code string) (*model.Entitlement, error) {
	if s.onCall != nil {
		s.onCall(code)
	}
	return s.EntitlementModel.FindByCode(ctx, code)
}

// -- CheckEntitlements --

func TestCheckEntitlementsDecisionsAlignWithInputCodes(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedCatalog(t, db)
	now := nowSec()
	seedMembership(db, &model.Membership{
		Mid: 1101, VipType: model.VipTypePremium, StartAt: now - 10*testDay, ExpireAt: now + 10*testDay,
	})

	codes := []string{hdCode, " " + ultraCode + " ", unseededCode, hdCode, disabledCode, ""}
	got, err := NewCheckEntitlementsLogic(context.Background(), svcCtx).CheckEntitlements(
		&rpc.CheckEntitlementsReq{Mid: 1101, Codes: codes})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if len(got.GetDecisions()) != len(codes) {
		t.Fatalf("decisions 数量 %d != codes 数量 %d（调用方按下标取结论，错位就是给错答案）",
			len(got.GetDecisions()), len(codes))
	}
	wantReasons := []rpc.EntitlementReason{
		rpc.EntitlementReason_ENTITLEMENT_GRANTED,         // 大会员满足高码率
		rpc.EntitlementReason_ENTITLEMENT_TIER_NOT_ENOUGH, // 带空白的 4K 码仍被裁剪后识别（不是未知）
		rpc.EntitlementReason_ENTITLEMENT_CODE_UNKNOWN,
		rpc.EntitlementReason_ENTITLEMENT_GRANTED, // 重复的码重复回答
		rpc.EntitlementReason_ENTITLEMENT_CODE_DISABLED,
		rpc.EntitlementReason_ENTITLEMENT_CODE_UNKNOWN, // 空码
	}
	for i, d := range got.GetDecisions() {
		if d.GetCode() != codes[i] {
			t.Errorf("第 %d 项 code = %q，期望原样回显 %q", i, d.GetCode(), codes[i])
		}
		if d.GetReason() != wantReasons[i] {
			t.Errorf("第 %d 项(%q) reason = %v，期望 %v", i, codes[i], d.GetReason(), wantReasons[i])
		}
		if d.GetGranted() != (wantReasons[i] == rpc.EntitlementReason_ENTITLEMENT_GRANTED) {
			t.Errorf("第 %d 项 granted=%v 与 reason %v 不自洽", i, d.GetGranted(), d.GetReason())
		}
	}
	// 批量读必须是「一次目录 + 一次身份」，不能退化成逐码查。
	if db.entListByCodesCalls != 1 {
		t.Errorf("目录读了 %d 次，期望 1 次（批量不能退化成逐码）", db.entListByCodesCalls)
	}
	if db.entFindByCodeCalls != 0 {
		t.Errorf("批量路径里出现了单码查询 %d 次", db.entFindByCodeCalls)
	}
	if db.memberListByMidCalls != 1 {
		t.Errorf("身份读了 %d 次，期望 1 次", db.memberListByMidCalls)
	}
}

// TestCheckEntitlementsReplyEchoesEvidence：整批的 expire_at/vip_type 取生效中的最高档，
// 与单项判定的取值口径一致（PickMembershipFor 只有一个实现）。
func TestCheckEntitlementsReplyEchoesEvidence(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedCatalog(t, db)
	now := nowSec()
	plus := seedMembership(db, &model.Membership{
		Mid: 1151, VipType: model.VipTypePremiumPlus, StartAt: now - 3*testDay, ExpireAt: now + 27*testDay,
	})
	seedMembership(db, &model.Membership{
		Mid: 1151, VipType: model.VipTypePremium, StartAt: now - 3*testDay, ExpireAt: now + 7*testDay,
	})

	got, err := NewCheckEntitlementsLogic(context.Background(), svcCtx).CheckEntitlements(
		&rpc.CheckEntitlementsReq{Mid: 1151, Codes: []string{hdCode}})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if got.GetVipType() != rpc.VipType_VIP_TYPE_PREMIUM_PLUS || got.GetExpireAt() != plus.ExpireAt {
		t.Errorf("整批依据取错档：%v / %d", got.GetVipType(), got.GetExpireAt())
	}
}

func TestCheckEntitlementsOverLimitIsRejectedNotTruncated(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedCatalog(t, db)
	activePremium(db, 1201)

	codes := make([]string, maxCodesPerCheck+1)
	for i := range codes {
		codes[i] = fmt.Sprintf("vip.code_%d", i)
	}
	got, err := NewCheckEntitlementsLogic(context.Background(), svcCtx).CheckEntitlements(
		&rpc.CheckEntitlementsReq{Mid: 1201, Codes: codes})
	if !errors.Is(err, model.ErrInvalidQueryFilter) {
		t.Fatalf("超限必须报错，得到 %v", err)
	}
	if got != nil {
		t.Errorf("超限却返回了结论（静默截断会让调用方以为没问的那几项不存在）：%+v", got)
	}
	if db.memberListByMidCalls != 0 || db.entListByCodesCalls != 0 {
		t.Error("超限请求仍然打了库，等于没挡住慢查")
	}

	// 边界：正好上限要通过，且通过的路径只读两次。
	ok, err := NewCheckEntitlementsLogic(context.Background(), svcCtx).CheckEntitlements(
		&rpc.CheckEntitlementsReq{Mid: 1201, Codes: codes[:maxCodesPerCheck]})
	if err != nil {
		t.Fatalf("恰好 %d 项应通过: %v", maxCodesPerCheck, err)
	}
	if len(ok.GetDecisions()) != maxCodesPerCheck {
		t.Errorf("结论数 %d != %d", len(ok.GetDecisions()), maxCodesPerCheck)
	}
	if db.memberListByMidCalls != 1 || db.entListByCodesCalls != 1 {
		t.Errorf("上限内的批读次数：身份 %d、目录 %d，期望各 1", db.memberListByMidCalls, db.entListByCodesCalls)
	}
}

// TestCheckEntitlementsMidInvalidAnswersEveryCode：游客态下也要逐项给结论，
// 一个码都不能少（否则客户端无法区分「没答」与「不通过」）。
func TestCheckEntitlementsMidInvalidAnswersEveryCode(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedCatalog(t, db)
	activePremium(db, 1301)

	codes := []string{hdCode, ultraCode, "", "   "}
	got, err := NewCheckEntitlementsLogic(context.Background(), svcCtx).CheckEntitlements(
		&rpc.CheckEntitlementsReq{Mid: -5, Codes: codes})
	if err != nil {
		t.Fatalf("游客态是结论不是错误: %v", err)
	}
	if len(got.GetDecisions()) != len(codes) {
		t.Fatalf("结论数 %d != %d", len(got.GetDecisions()), len(codes))
	}
	for i, d := range got.GetDecisions() {
		if d.GetReason() != rpc.EntitlementReason_ENTITLEMENT_MID_INVALID || d.GetGranted() {
			t.Errorf("第 %d 项 = %v/%v，期望 MID_INVALID/false", i, d.GetReason(), d.GetGranted())
		}
		if d.GetCode() != codes[i] {
			t.Errorf("第 %d 项没原样回显 code：%q", i, d.GetCode())
		}
	}
	if got.GetVipType() != rpc.VipType_VIP_TYPE_UNSPECIFIED || got.GetExpireAt() != 0 {
		t.Error("游客态却编造出了会员证据")
	}
	if db.memberListByMidCalls != 0 || db.entListByCodesCalls != 0 {
		t.Errorf("游客态读了库：身份 %d、目录 %d", db.memberListByMidCalls, db.entListByCodesCalls)
	}
}

// TestCheckEntitlementsReadFailureFailsWholeBatch：批量判定里任一读失败都必须上抛，
// 不能把失败的那几项折叠成 false 再混着正确答案返回。
func TestCheckEntitlementsReadFailureFailsWholeBatch(t *testing.T) {
	cases := []struct {
		name  string
		setup func(db *fakeDB)
		want  error
	}{
		{"目录批量读失败", func(db *fakeDB) { db.entErr = errCatalogDown }, errCatalogDown},
		{"身份读失败", func(db *fakeDB) { db.memberErr = errMembersDown }, errMembersDown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svcCtx, db := newTestSvc(t)
			seedCatalog(t, db)
			activePremium(db, 1401)
			tc.setup(db)

			got, err := NewCheckEntitlementsLogic(context.Background(), svcCtx).CheckEntitlements(
				&rpc.CheckEntitlementsReq{Mid: 1401, Codes: []string{hdCode, ultraCode}})
			if !errors.Is(err, tc.want) {
				t.Fatalf("错误必须上抛，得到 %v", err)
			}
			if got != nil {
				t.Errorf("读失败仍返回半批结论：%+v", got)
			}
		})
	}
}

// TestCheckEntitlementsAllBlankCodesSkipCatalog：全是空码时不构造 IN () 这种注定报错的查询。
func TestCheckEntitlementsAllBlankCodesSkipCatalog(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedCatalog(t, db)
	activePremium(db, 1501)

	got, err := NewCheckEntitlementsLogic(context.Background(), svcCtx).CheckEntitlements(
		&rpc.CheckEntitlementsReq{Mid: 1501, Codes: []string{"", "  "}})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if db.entListByCodesCalls != 0 {
		t.Errorf("全是空码仍去查目录 %d 次", db.entListByCodesCalls)
	}
	for i, d := range got.GetDecisions() {
		if d.GetReason() != rpc.EntitlementReason_ENTITLEMENT_CODE_UNKNOWN {
			t.Errorf("第 %d 项 = %v，期望 CODE_UNKNOWN", i, d.GetReason())
		}
	}
}

// TestCheckEntitlementsEmptyCodesYieldNoDecisions：codes 为空是合法调用（只想知道自己档位），
// 结论数必须是 0 而不是 nil  panic，也不该因此省掉身份读。
func TestCheckEntitlementsEmptyCodesYieldNoDecisions(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedCatalog(t, db)
	held := activePremium(db, 1601)

	got, err := NewCheckEntitlementsLogic(context.Background(), svcCtx).CheckEntitlements(
		&rpc.CheckEntitlementsReq{Mid: 1601})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if len(got.GetDecisions()) != 0 {
		t.Errorf("没问码却给出 %d 条结论", len(got.GetDecisions()))
	}
	if got.GetExpireAt() != held.ExpireAt {
		t.Errorf("整批到期回显 %d，期望 %d", got.GetExpireAt(), held.ExpireAt)
	}
	if db.entListByCodesCalls != 0 {
		t.Errorf("空 codes 仍查了目录")
	}
}

// TestSingleAndBatchNeverDisagree 是「共用同一个判定实现」的回归保险：
// 同一个 (mid, code) 经两个接口必须给出完全相同的答案，否则播放端与详情页会互相打脸。
func TestSingleAndBatchNeverDisagree(t *testing.T) {
	catalogCodes := []string{hdCode, ultraCode, disabledCode, unseededCode, "vip.x"}
	userCases := []struct {
		name  string
		setup func(db *fakeDB, mid int64)
	}{
		{"从未开通", func(db *fakeDB, mid int64) {}},
		{"生效大会员", func(db *fakeDB, mid int64) {
			now := nowSec()
			seedMembership(db, &model.Membership{Mid: mid, VipType: model.VipTypePremium,
				StartAt: now - testDay, ExpireAt: now + 30*testDay})
		}},
		{"生效超级大会员", func(db *fakeDB, mid int64) {
			now := nowSec()
			seedMembership(db, &model.Membership{Mid: mid, VipType: model.VipTypePremiumPlus,
				StartAt: now - testDay, ExpireAt: now + 30*testDay})
		}},
		{"两档都已过期", func(db *fakeDB, mid int64) {
			now := nowSec()
			seedMembership(db, &model.Membership{Mid: mid, VipType: model.VipTypePremium,
				StartAt: now - 90*testDay, ExpireAt: now - 30*testDay})
			seedMembership(db, &model.Membership{Mid: mid, VipType: model.VipTypePremiumPlus,
				StartAt: now - 90*testDay, ExpireAt: now - 10*testDay})
		}},
	}

	for _, uc := range userCases {
		for _, code := range catalogCodes {
			t.Run(uc.name+"/"+code, func(t *testing.T) {
				singleCtx, singleDB := newTestSvc(t)
				seedCatalog(t, singleDB)
				uc.setup(singleDB, 7001)

				batchCtx, batchDB := newTestSvc(t)
				seedCatalog(t, batchDB)
				uc.setup(batchDB, 7001)

				single, err := NewCheckEntitlementLogic(context.Background(), singleCtx).CheckEntitlement(
					&rpc.CheckEntitlementReq{Mid: 7001, Code: code})
				if err != nil {
					t.Fatalf("单项报错 %v", err)
				}
				batch, err := NewCheckEntitlementsLogic(context.Background(), batchCtx).CheckEntitlements(
					&rpc.CheckEntitlementsReq{Mid: 7001, Codes: []string{code}})
				if err != nil {
					t.Fatalf("批量报错 %v", err)
				}
				d := batch.GetDecisions()[0]
				if single.GetGranted() != d.GetGranted() || single.GetReason() != d.GetReason() {
					t.Errorf("单项 %v/%v 与批量 %v/%v 不一致",
						single.GetReason(), single.GetGranted(), d.GetReason(), d.GetGranted())
				}
			})
		}
	}
}
