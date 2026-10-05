package logic

// 会员读侧三个接口（GetMembership / ListGrants / ListExpiringMemberships）的用例级测试。
//
// 这三条分别服务三种调用方，各自最贵的错误也不同：
//   - GetMembership（终端「我的会员」页）：found 只表示「有没有身份行」，过期行照样返回，
//     而 granted_entitlements 是只读投影——过期身份一个码都不给，否则「曾有过」会换成今天的能力；
//   - ListGrants（运营面 + 用户面台账）：mid=0 才有跨用户语义，非法过滤枚举必须在触库前拒掉，
//     台账读失败不能退成 total=0（那会把「查不到」说成「没发生过」）；
//   - ListExpiringMemberships（cron）：只读、不改状态（README §4：expire_at 就是状态），
//     闭区间 + 满批游标是续费与置灰批次的推进依据。
//
// 手法与 catalog_read_test.go 一致：投影逐字段比对来源行；守卫是否先于触库用调用计数证明；
// 过滤/游标/裁剪的「落到 model 的形态」用入参快照证明；依赖错误按 errors.Is 比对哨兵值。
// 本轮三条路径都不碰缓存（svcCtx.Cache 无引用，README §7-1），因此用例改为钉住
// 「零事务、零台账写入、身份行一个字节都没被改动」——读接口写库就是越权。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/membership/model"
	"go-video/services/membership/rpc"
)

// --- 夹具 ---

// fullMembership 造一行字段齐全的会员身份：签约位、渠道、来源与付费月数都要能逐个溯源。
// expireOffset 是相对 now 的秒偏移，正数表示仍在生效。
func fullMembership(mid int64, vipType int32, expireOffset int64, autoRenew int32) *model.Membership {
	now := nowSec()
	channel := ""
	signedAt := int64(0)
	if autoRenew == 1 {
		channel = model.AllowedAutoRenewChannel
		signedAt = now - 15*testDay
	}
	return &model.Membership{
		Mid: mid, VipType: vipType,
		StartAt: now - 100*testDay, ExpireAt: now + expireOffset,
		AutoRenew: autoRenew, AutoRenewChannel: channel, AutoRenewSignedAt: signedAt,
		Source: model.GrantSourceSandboxPurchase, PaidMonthCount: 3,
		Version: 6, Ctime: now - 100*testDay, Mtime: now - testDay,
	}
}

// assertMembershipEchoed 逐个字段比对身份行投影；漏映射或换算错（0/1 与 bool）都会发红。
func assertMembershipEchoed(t *testing.T, got *rpc.MembershipInfo, row *model.Membership, where string) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s：没有身份行投影，期望来自 mid=%d vip_type=%d", where, row.Mid, row.VipType)
	}
	if got.GetMid() != row.Mid || got.GetVipType() != rpc.VipType(row.VipType) {
		t.Errorf("%s：主体 = %d/%v，期望 %d/%d", where, got.GetMid(), got.GetVipType(), row.Mid, row.VipType)
	}
	if got.GetStartAt() != row.StartAt {
		t.Errorf("%s：start_at = %d，期望 %d", where, got.GetStartAt(), row.StartAt)
	}
	if got.GetExpireAt() != row.ExpireAt {
		t.Errorf("%s：expire_at = %d，期望 %d（到期时间是判定依据，不能被改写）", where, got.GetExpireAt(), row.ExpireAt)
	}
	if got.GetAutoRenew() != (row.AutoRenew == 1) {
		t.Errorf("%s：auto_renew = %v，期望库里的 %d 折算", where, got.GetAutoRenew(), row.AutoRenew)
	}
	if got.GetAutoRenewChannel() != row.AutoRenewChannel {
		t.Errorf("%s：auto_renew_channel = %q，期望 %q", where, got.GetAutoRenewChannel(), row.AutoRenewChannel)
	}
	if got.GetAutoRenewSignedAt() != row.AutoRenewSignedAt {
		t.Errorf("%s：auto_renew_signed_at = %d，期望 %d", where, got.GetAutoRenewSignedAt(), row.AutoRenewSignedAt)
	}
	if got.GetSource() != rpc.GrantSource(row.Source) {
		t.Errorf("%s：source = %v，期望 %d", where, got.GetSource(), row.Source)
	}
	if got.GetPaidMonthCount() != row.PaidMonthCount {
		t.Errorf("%s：paid_month_count = %d，期望 %d", where, got.GetPaidMonthCount(), row.PaidMonthCount)
	}
	if got.GetVersion() != row.Version {
		t.Errorf("%s：version = %d，期望 %d（CAS 位必须回显，调用方要拿它去重试）", where, got.GetVersion(), row.Version)
	}
	if got.GetCtime() != row.Ctime || got.GetMtime() != row.Mtime {
		t.Errorf("%s：ctime/mtime = %d/%d，期望 %d/%d",
			where, got.GetCtime(), got.GetMtime(), row.Ctime, row.Mtime)
	}
}

// assertGrantEchoed 逐个字段比对台账行投影。
func assertGrantEchoed(t *testing.T, got *rpc.GrantInfo, row *model.Grant, where string) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s：缺少 grant_id=%d 的投影", where, row.GrantID)
	}
	if got.GetGrantId() != row.GrantID || got.GetMid() != row.Mid {
		t.Errorf("%s：标识 = %d/%d，期望 %d/%d", where, got.GetGrantId(), got.GetMid(), row.GrantID, row.Mid)
	}
	if got.GetVipType() != rpc.VipType(row.VipType) {
		t.Errorf("%s：vip_type = %v，期望 %d", where, got.GetVipType(), row.VipType)
	}
	if got.GetAction() != row.Action {
		t.Errorf("%s：action = %s，期望 %s", where, got.GetAction(), row.Action)
	}
	if got.GetDeltaDays() != row.DeltaDays {
		t.Errorf("%s：delta_days = %d，期望 %d（台账记的是请求意图，带符号）", where, got.GetDeltaDays(), row.DeltaDays)
	}
	if got.GetPlanId() != row.PlanID {
		t.Errorf("%s：plan_id = %d，期望 %d", where, got.GetPlanId(), row.PlanID)
	}
	if got.GetSource() != rpc.GrantSource(row.Source) {
		t.Errorf("%s：source = %v，期望 %d", where, got.GetSource(), row.Source)
	}
	if got.GetBizOrderNo() != row.BizOrderNo || got.GetPaymentNo() != row.PaymentNo {
		t.Errorf("%s：追溯位 = %s/%s，期望 %s/%s（退款回收与运营纠错靠它区分）",
			where, got.GetBizOrderNo(), got.GetPaymentNo(), row.BizOrderNo, row.PaymentNo)
	}
	if got.GetBeforeExpireAt() != row.BeforeExpireAt || got.GetAfterExpireAt() != row.AfterExpireAt {
		t.Errorf("%s：before/after_expire_at = %d/%d，期望 %d/%d",
			where, got.GetBeforeExpireAt(), got.GetAfterExpireAt(), row.BeforeExpireAt, row.AfterExpireAt)
	}
	if got.GetOperator() != row.Operator || got.GetRequestId() != row.RequestID {
		t.Errorf("%s：operator/request_id = %s/%s，期望 %s/%s",
			where, got.GetOperator(), got.GetRequestId(), row.Operator, row.RequestID)
	}
	if got.GetReason() != row.Reason {
		t.Errorf("%s：reason = %q，期望 %q", where, got.GetReason(), row.Reason)
	}
	if got.GetCtime() != row.Ctime {
		t.Errorf("%s：ctime = %d，期望 %d", where, got.GetCtime(), row.Ctime)
	}
}

// ledgerRow 造一条台账行。ctime 显式给定，便于锁「事件时间闭区间」。
func ledgerRow(id, mid int64, vipType int32, action string, delta int32, ctime int64,
	source int32, bizOrder, requestID string) *model.Grant {
	return &model.Grant{
		GrantID: id, Mid: mid, VipType: vipType, Action: action, DeltaDays: delta,
		PlanID: 88, Source: source, BizOrderNo: bizOrder, PaymentNo: "PAY-" + bizOrder,
		BeforeExpireAt: ctime - 31*testDay, AfterExpireAt: ctime,
		Operator: "ops-1", RequestID: requestID, Reason: "台账摘要 " + action, Ctime: ctime,
	}
}

// --- GetMembership ---

// TestGetMembershipProjectsHeldRowAndServerNow：found=true 时每个字段都必须来自那一行，
// server_now 必须是服务端此刻（调用方拿它判过期，不能自带时钟），且整条路径零写入。
func TestGetMembershipProjectsHeldRowAndServerNow(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedCatalog(t, db)
	held := seedMembership(db, fullMembership(3001, model.VipTypePremiumPlus, 30*testDay, 1))

	before := nowSec()
	got, err := NewGetMembershipLogic(context.Background(), svcCtx).
		GetMembership(&rpc.GetMembershipReq{Mid: 3001})
	after := nowSec()
	mustNoError(t, err, "GetMembership 生效中")
	if !got.GetFound() {
		t.Fatalf("found=false，但库里确实有这一行")
	}
	assertMembershipEchoed(t, got.GetMembership(), held, "GetMembership")
	if got.GetServerNow() < before || got.GetServerNow() > after {
		t.Errorf("server_now = %d，不在 [%d,%d] 内：调用方无法据此判过期", got.GetServerNow(), before, after)
	}
	if db.memberListByMidCalls != 1 {
		t.Errorf("身份读了 %d 次，期望 1 次（一次列出全部档位，不按档位重复查）", db.memberListByMidCalls)
	}
	if db.txRuns != 0 || len(db.grants) != 0 || len(db.bizRequest) != 0 {
		t.Errorf("读接口有写入：tx=%d grant=%d bizRequest=%d", db.txRuns, len(db.grants), len(db.bizRequest))
	}
}

// TestGetMembershipGrantedEntitlementsAreTierSubset：只读投影按「档位达标 + 在用」过滤，
// 停用码与更高档的码都不能出现；档位序为真，所以超级大会员要拿到大会员的码。
func TestGetMembershipGrantedEntitlementsAreTierSubset(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedCatalog(t, db) // hd(premium,用) / ultra(plus,用) / legacy(premium,停用)
	now := nowSec()
	seedMembership(db, &model.Membership{Mid: 3101, VipType: model.VipTypePremium,
		StartAt: now - testDay, ExpireAt: now + 20*testDay})
	seedMembership(db, &model.Membership{Mid: 3102, VipType: model.VipTypePremiumPlus,
		StartAt: now - testDay, ExpireAt: now + 20*testDay})

	cases := []struct {
		mid       int64
		tier      rpc.VipType
		wantCodes []string
	}{
		{3101, rpc.VipType_VIP_TYPE_PREMIUM, []string{hdCode}},
		{3102, rpc.VipType_VIP_TYPE_PREMIUM_PLUS, []string{hdCode, ultraCode}},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("档位%d", int32(tc.tier)), func(t *testing.T) {
			db.entListCalls = 0
			got, err := NewGetMembershipLogic(context.Background(), svcCtx).
				GetMembership(&rpc.GetMembershipReq{Mid: tc.mid})
			mustNoError(t, err, "GetMembership 权益投影")
			if got.GetMembership().GetVipType() != tc.tier {
				t.Fatalf("判定行档位 = %v，期望 %v", got.GetMembership().GetVipType(), tc.tier)
			}
			if len(got.GetGrantedEntitlements()) != len(tc.wantCodes) {
				t.Fatalf("投影 %d 个码 %v，期望 %d 个 %v", len(got.GetGrantedEntitlements()),
					codesOf(got.GetGrantedEntitlements()), len(tc.wantCodes), tc.wantCodes)
			}
			for i, e := range got.GetGrantedEntitlements() {
				if e.GetCode() != tc.wantCodes[i] {
					t.Errorf("第 %d 个码 = %s，期望 %s", i, e.GetCode(), tc.wantCodes[i])
				}
				if !e.GetEnabled() {
					t.Errorf("%s 被投影出来却是停用态", e.GetCode())
				}
			}
			// 目录只读一次（含更高档的码由投影自己筛），且必须是「只看在用」的那次读。
			if db.entListCalls != 1 || !db.lastEntListEnabled {
				t.Fatalf("目录读了 %d 次（enabled_only=%v），期望 1 次且只读在用码",
					db.entListCalls, db.lastEntListEnabled)
			}
		})
	}
}

// TestGetMembershipExpiredIdentityMintsNoEntitlement：过期身份 found=true（详情页要显示到期时间），
// 但一个权益码都不给，而且根本不该去读目录——「曾有过」不能换成今天的能力。
func TestGetMembershipExpiredIdentityMintsNoEntitlement(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedCatalog(t, db)
	expired := seedMembership(db, fullMembership(3201, model.VipTypePremiumPlus, -2*testDay, 1))

	got, err := NewGetMembershipLogic(context.Background(), svcCtx).
		GetMembership(&rpc.GetMembershipReq{Mid: 3201})
	mustNoError(t, err, "GetMembership 已过期")
	if !got.GetFound() {
		t.Fatalf("有过但已过期必须是 found=true（README §3）")
	}
	assertMembershipEchoed(t, got.GetMembership(), expired, "GetMembership 已过期")
	if got.GetServerNow() <= expired.ExpireAt {
		t.Fatalf("server_now=%d 竟然没超过 expire_at=%d，用例边界失效", got.GetServerNow(), expired.ExpireAt)
	}
	if len(got.GetGrantedEntitlements()) != 0 {
		t.Errorf("过期身份仍投影出 %d 个权益码：%v",
			len(got.GetGrantedEntitlements()), codesOf(got.GetGrantedEntitlements()))
	}
	if db.entListCalls != 0 {
		t.Errorf("过期身份还去读了 %d 次权益目录：白读且给「曾有过」留了复活通道", db.entListCalls)
	}
}

// TestGetMembershipPickRules 锁「判定依据行」的挑法：
// 未指定档位时生效中的最高档优先，全都过期才退回曾达到的最高档；指定档位只看那一档。
func TestGetMembershipPickRules(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedCatalog(t, db) // 投影要有码可投，否则「生效中却不给权益」这条断言会失去意义
	now := nowSec()
	// 3301：超级大会员已过期，大会员仍在生效——不能被「曾达到的更高档」顶替。
	seedMembership(db, &model.Membership{Mid: 3301, VipType: model.VipTypePremiumPlus,
		StartAt: now - 200*testDay, ExpireAt: now - testDay})
	activePremium := seedMembership(db, &model.Membership{Mid: 3301, VipType: model.VipTypePremium,
		StartAt: now - 10*testDay, ExpireAt: now + 5*testDay, PaidMonthCount: 1})
	// 3302：两档都过期，取曾达到的最高档（两行到期时刻不同，才能证明挑的是哪一行）。
	expiredPremium := seedMembership(db, &model.Membership{Mid: 3302, VipType: model.VipTypePremium,
		StartAt: now - 300*testDay, ExpireAt: now - 90*testDay})
	expiredPlus := seedMembership(db, &model.Membership{Mid: 3302, VipType: model.VipTypePremiumPlus,
		StartAt: now - 300*testDay, ExpireAt: now - 100*testDay})
	if expiredPlus.ExpireAt == expiredPremium.ExpireAt {
		t.Fatalf("夹具失效：两行到期时刻相同，无法区分挑了哪一行")
	}

	t.Run("生效中的低档胜过已过期的更高档", func(t *testing.T) {
		got, err := NewGetMembershipLogic(context.Background(), svcCtx).
			GetMembership(&rpc.GetMembershipReq{Mid: 3301})
		mustNoError(t, err, "GetMembership 3301")
		assertMembershipEchoed(t, got.GetMembership(), activePremium, "3301")
		if got.GetMembership().GetExpireAt() <= got.GetServerNow() {
			t.Errorf("挑出来的行按 server_now 已经不生效，投影将一个都不给权益")
		}
		if len(got.GetGrantedEntitlements()) == 0 {
			t.Errorf("生效中大会员却一个权益码都没投影到（目录里至少有 %s）", hdCode)
		}
	})

	t.Run("全过期时取曾达到的最高档", func(t *testing.T) {
		got, err := NewGetMembershipLogic(context.Background(), svcCtx).
			GetMembership(&rpc.GetMembershipReq{Mid: 3302})
		mustNoError(t, err, "GetMembership 3302")
		if !got.GetFound() {
			t.Fatalf("有过但过期必须 found=true")
		}
		if got.GetMembership().GetVipType() != rpc.VipType_VIP_TYPE_PREMIUM_PLUS {
			t.Errorf("依据行 = %v，期望曾达到的最高档 PREMIUM_PLUS", got.GetMembership().GetVipType())
		}
		// 到期时刻必须来自 PLUS 那一行（它比大会员行到期更早），否则就是挑错了行。
		if got.GetMembership().GetExpireAt() != expiredPlus.ExpireAt {
			t.Errorf("expire_at = %d，期望来自 PREMIUM_PLUS 行的 %d（大会员行是 %d）",
				got.GetMembership().GetExpireAt(), expiredPlus.ExpireAt, expiredPremium.ExpireAt)
		}
	})

	t.Run("指定档位只看那一档", func(t *testing.T) {
		db.memberListByMidCalls = 0
		got, err := NewGetMembershipLogic(context.Background(), svcCtx).
			GetMembership(&rpc.GetMembershipReq{Mid: 3301, VipType: rpc.VipType_VIP_TYPE_PREMIUM_PLUS})
		mustNoError(t, err, "GetMembership 指定已过期档位")
		// 那一档的行存在且已过期：found=true，且不得因为「另一档还在生效」给权益。
		if !got.GetFound() || got.GetMembership().GetVipType() != rpc.VipType_VIP_TYPE_PREMIUM_PLUS {
			t.Fatalf("指定档位拿到的行 = found=%v %+v", got.GetFound(), got.GetMembership())
		}
		if len(got.GetGrantedEntitlements()) != 0 {
			t.Errorf("指定档位的行已过期，仍投影出 %v", codesOf(got.GetGrantedEntitlements()))
		}
		if db.memberListByMidCalls != 1 {
			t.Errorf("按档位过滤又查了 %d 次库，期望一次列出全部档位后在内存里挑", db.memberListByMidCalls)
		}

		// 另一档生效、指定档位从未开通：found=false（那一档没有就是没有，不能拿别的档顶）。
		other, err := NewGetMembershipLogic(context.Background(), svcCtx).
			GetMembership(&rpc.GetMembershipReq{Mid: 3301, VipType: rpc.VipType_VIP_TYPE_PREMIUM})
		mustNoError(t, err, "GetMembership 指定存在档位")
		if !other.GetFound() || other.GetMembership().GetVipType() != rpc.VipType_VIP_TYPE_PREMIUM {
			t.Errorf("指定档位 = PREMIUM 应拿到大会员那行，实得 found=%v %+v", other.GetFound(), other.GetMembership())
		}
	})
}

// TestGetMembershipUnknownUserIsFoundFalseNotError：从未开通是正常结论，
// 但必须带 server_now 且不带任何编造出来的身份行。
func TestGetMembershipUnknownUserIsFoundFalseNotError(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedCatalog(t, db)
	seedMembership(db, fullMembership(3402, model.VipTypePremium, testDay, 0)) // 别人的行不算

	before := nowSec()
	got, err := NewGetMembershipLogic(context.Background(), svcCtx).
		GetMembership(&rpc.GetMembershipReq{Mid: 3401})
	mustNoError(t, err, "GetMembership 从未开通")
	if got.GetFound() {
		t.Errorf("从未开通却 found=true")
	}
	if got.GetMembership() != nil {
		t.Errorf("found=false 却带回身份投影 %+v（客户端会当成已开通）", got.GetMembership())
	}
	if len(got.GetGrantedEntitlements()) != 0 {
		t.Errorf("从未开通却给了 %d 个权益码", len(got.GetGrantedEntitlements()))
	}
	if got.GetServerNow() < before {
		t.Errorf("server_now = %d 早于调用时刻 %d", got.GetServerNow(), before)
	}
	if db.memberListByMidCalls != 1 || db.entListCalls != 0 {
		t.Errorf("读次数 = 身份 %d / 目录 %d，期望 1/0（没有依据行不该顺带读目录）",
			db.memberListByMidCalls, db.entListCalls)
	}
}

// TestGetMembershipGuardsRunBeforeDB：mid 与档位非法必须在读身份之前被拒，
// 计数为 0 就是「没触库」的证据（不是靠日志推断）。
func TestGetMembershipGuardsRunBeforeDB(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedCatalog(t, db)
	seedMembership(db, fullMembership(3501, model.VipTypePremium, testDay, 0))

	cases := []struct {
		name string
		in   *rpc.GetMembershipReq
		want error
	}{
		{"mid 为 0", &rpc.GetMembershipReq{Mid: 0}, model.ErrInvalidMid},
		{"mid 为负", &rpc.GetMembershipReq{Mid: -7}, model.ErrInvalidMid},
		{"档位越界", &rpc.GetMembershipReq{Mid: 3501, VipType: rpc.VipType(11)}, model.ErrInvalidVipType},
		{"档位负数", &rpc.GetMembershipReq{Mid: 3501, VipType: rpc.VipType(-1)}, model.ErrInvalidVipType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewGetMembershipLogic(context.Background(), svcCtx).GetMembership(tc.in)
			mustErrIs(t, err, tc.want, "GetMembership "+tc.name)
			if got != nil {
				t.Errorf("守卫未过不得返回应答，实得 %+v", got)
			}
			if db.memberListByMidCalls != 0 || db.entListCalls != 0 {
				t.Fatalf("非法入参仍触库（身份 %d 次 / 目录 %d 次），守卫必须在读之前",
					db.memberListByMidCalls, db.entListCalls)
			}
		})
	}
}

// TestGetMembershipIdentityReadFailureIsNotUnopened：身份读失败必须是错误。
// 折叠成 found=false 等于在数据库抖动时把付费用户显示成「从未开通」。
func TestGetMembershipIdentityReadFailureIsNotUnopened(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedCatalog(t, db)
	seedMembership(db, fullMembership(3601, model.VipTypePremium, testDay, 0))
	db.memberErr = errIdentityDown

	got, err := NewGetMembershipLogic(context.Background(), svcCtx).
		GetMembership(&rpc.GetMembershipReq{Mid: 3601})
	if !errors.Is(err, errIdentityDown) {
		t.Fatalf("身份读失败必须原样上抛，实得 %v", err)
	}
	if got != nil {
		t.Errorf("读失败不得返回 found=false，实得 %+v", got)
	}
}

// 缺陷：getmembershiplogic.go:63-67 —— 权益目录读失败只在 l.Errorf 里记日志，随后仍
// return reply, nil，reply.GrantedEntitlements 保持 nil（用例
// TestGetMembershipEntitlementProjectionFailureDegradesToEmpty 钉住现状）。
// 失败方向是 fail-closed（不会凭空授权，比放行安全），但 GetMembershipReply 里没有任何字段
// 能让调用方区分「这个档位确实没有可用权益码」与「目录读挂了」——详情页会把故障显示成「你没有任何权益」。
// 修法方向：给 GetMembershipReply 加 entitlements_degraded 位（契约变更需先改 .proto 再生成），
// 或在 logic 里把该错误并入 ErrEntitlementProjectionFailed 让调用方按可重试错误处理。
func TestGetMembershipEntitlementProjectionFailureDegradesToEmpty(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedCatalog(t, db)
	held := seedMembership(db, fullMembership(3701, model.VipTypePremium, testDay, 0))
	db.entErr = errCatalogDown

	got, err := NewGetMembershipLogic(context.Background(), svcCtx).
		GetMembership(&rpc.GetMembershipReq{Mid: 3701})
	if err != nil {
		t.Fatalf("当前实现刻意不把目录读失败上抛（本用例钉住这个行为）：实得 %v", err)
	}
	if !got.GetFound() {
		t.Errorf("主结论被降级连带影响了：found=false")
	}
	assertMembershipEchoed(t, got.GetMembership(), held, "GetMembership 目录降级")
	if len(got.GetGrantedEntitlements()) != 0 {
		t.Errorf("目录读挂了却能投影出权益码：%v（说明投影没有真读依赖）", codesOf(got.GetGrantedEntitlements()))
	}
	// 对照组：同一入参、故障撤掉后立刻有码 —— 证明上面那个空列表是「被吞掉的故障」造成的，
	// 不是这个档位本来就没有可用码。缺陷的代价就在这里：应答里两种情形长得一样。
	db.entErr = nil
	healthy, err := NewGetMembershipLogic(context.Background(), svcCtx).
		GetMembership(&rpc.GetMembershipReq{Mid: 3701})
	mustNoError(t, err, "GetMembership 故障恢复后复查")
	if len(healthy.GetGrantedEntitlements()) == 0 {
		t.Fatalf("对照组也是空的（夹具失效，期望至少含 %s），本用例失去意义", hdCode)
	}
}

// TestGetMembershipNeverMutatesIdentity：读接口一次调用后，库里那行的每个字段必须原样，
// 幂等台账与授予台账都不该多出记录（读路径写库就是越权，AGENTS.md §5）。
func TestGetMembershipNeverMutatesIdentity(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	seedCatalog(t, db)
	before := seedMembership(db, fullMembership(3801, model.VipTypePremiumPlus, 10*testDay, 1))
	snap := db.snapshot()

	for i := 0; i < 3; i++ {
		if _, err := NewGetMembershipLogic(context.Background(), svcCtx).
			GetMembership(&rpc.GetMembershipReq{Mid: 3801}); err != nil {
			t.Fatalf("第 %d 次读不该报错：%v", i, err)
		}
	}
	after := db.members[memberKey(3801, model.VipTypePremiumPlus)]
	if *after != *before {
		t.Errorf("三次读之后身份行被改动：\n读前 %+v\n读后 %+v", *before, *after)
	}
	if len(db.grants) != len(snap.grants) || len(db.bizRequest) != len(snap.bizRequest) ||
		len(db.planLogs) != len(snap.planLogs) {
		t.Errorf("读接口写出了台账：grant=%d bizRequest=%d planLog=%d（读前 %d/%d/%d）",
			len(db.grants), len(db.bizRequest), len(db.planLogs),
			len(snap.grants), len(snap.bizRequest), len(snap.planLogs))
	}
	if db.txRuns != 0 {
		t.Errorf("读接口开了 %d 次事务", db.txRuns)
	}
}

// --- ListGrants ---

// TestListGrantsProjectsLedgerNewestFirst：台账按 grant_id 倒序（最新在前），
// 每个字段都要能溯源到那一行，total 是过滤后的总数。
func TestListGrantsProjectsLedgerNewestFirst(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	first := seedGrant(db, ledgerRow(0, 4001, model.VipTypePremium, model.ActionGrant, 31,
		now-62*testDay, model.GrantSourceSandboxPurchase, bizOrder, "g-old"))
	second := seedGrant(db, ledgerRow(0, 4001, model.VipTypePremium, model.ActionExtend, 31,
		now-31*testDay, model.GrantSourceSandboxAutoRenew, "SO-autorenew", "g-mid"))
	third := seedGrant(db, ledgerRow(0, 4001, model.VipTypePremium, model.ActionRevoke, -31,
		now-testDay, model.GrantSourceAdminOps, "SO-refund", "g-new"))

	got, err := NewListGrantsLogic(context.Background(), svcCtx).ListGrants(&rpc.ListGrantsReq{Mid: 4001})
	mustNoError(t, err, "ListGrants")
	if got.GetTotal() != 3 {
		t.Errorf("total = %d，期望 3", got.GetTotal())
	}
	rows := got.GetGrants()
	if len(rows) != 3 {
		t.Fatalf("本页 %d 行，期望 3", len(rows))
	}
	// 倒序：最新的 REVOKE 在前。
	assertGrantEchoed(t, rows[0], third, "ListGrants[0]")
	assertGrantEchoed(t, rows[1], second, "ListGrants[1]")
	assertGrantEchoed(t, rows[2], first, "ListGrants[2]")
	if rows[0].GetAction() != model.ActionRevoke {
		t.Errorf("首行 action = %s，期望最新一条 REVOKE", rows[0].GetAction())
	}
	if got.GetPage() != 1 || got.GetSize() != int64(testConf().DefaultPageSize) {
		t.Errorf("回显 page/size = %d/%d，期望 1/%d", got.GetPage(), got.GetSize(), testConf().DefaultPageSize)
	}
	if db.txRuns != 0 {
		t.Errorf("台账读接口开了 %d 次事务", db.txRuns)
	}
}

// TestListGrantsFiltersReachQuery：档位/来源/单号/事件时间区间都要落到查询里；
// mid=0 才是跨用户（admin）语义；单号首尾空白由 model 的 TrimSpace 处理。
func TestListGrantsFiltersReachQuery(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	seedGrant(db, ledgerRow(0, 4101, model.VipTypePremium, model.ActionGrant, 31,
		now-10*testDay, model.GrantSourceSandboxPurchase, bizOrder, "g-a"))
	seedGrant(db, ledgerRow(0, 4101, model.VipTypePremium, model.ActionRevoke, -31,
		now-2*testDay, model.GrantSourceAdminOps, "SO-refund", "g-b"))
	seedGrant(db, ledgerRow(0, 4102, model.VipTypePremiumPlus, model.ActionGrant, 366,
		now-testDay, model.GrantSourceExperience, "", "g-c"))

	cases := []struct {
		name     string
		in       *rpc.ListGrantsReq
		wantQ    model.GrantQuery
		wantReqs []string
	}{
		{
			name:     "按单号查回收记录",
			in:       &rpc.ListGrantsReq{Mid: 0, BizOrderNo: "  SO-refund  "},
			wantQ:    model.GrantQuery{BizOrderNo: "  SO-refund  "},
			wantReqs: []string{"g-b"},
		},
		{
			name:     "跨用户 + 档位过滤",
			in:       &rpc.ListGrantsReq{VipType: rpc.VipType_VIP_TYPE_PREMIUM_PLUS},
			wantQ:    model.GrantQuery{VipType: model.VipTypePremiumPlus},
			wantReqs: []string{"g-c"},
		},
		{
			name:     "来源过滤只看运营手工",
			in:       &rpc.ListGrantsReq{Source: rpc.GrantSource_GRANT_SOURCE_ADMIN_OPS},
			wantQ:    model.GrantQuery{Source: model.GrantSourceAdminOps},
			wantReqs: []string{"g-b"},
		},
		{
			name:     "事件时间闭区间",
			in:       &rpc.ListGrantsReq{FromTs: now - 5*testDay, ToTs: now},
			wantQ:    model.GrantQuery{FromTs: now - 5*testDay, ToTs: now},
			wantReqs: []string{"g-c", "g-b"}, // grant_id 倒序：后写的 g-c 在前
		},
		{
			name:     "限定单个用户",
			in:       &rpc.ListGrantsReq{Mid: 4101},
			wantQ:    model.GrantQuery{Mid: 4101},
			wantReqs: []string{"g-b", "g-a"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewListGrantsLogic(context.Background(), svcCtx).ListGrants(tc.in)
			mustNoError(t, err, "ListGrants "+tc.name)
			q := db.lastGrantQuery
			if q.Mid != tc.wantQ.Mid || q.VipType != tc.wantQ.VipType || q.Source != tc.wantQ.Source ||
				q.BizOrderNo != tc.wantQ.BizOrderNo || q.FromTs != tc.wantQ.FromTs || q.ToTs != tc.wantQ.ToTs {
				t.Fatalf("落到 model 的查询条件 = %+v，期望 %+v", q, tc.wantQ)
			}
			if got.GetTotal() != int64(len(tc.wantReqs)) {
				t.Errorf("total = %d，期望 %d", got.GetTotal(), len(tc.wantReqs))
			}
			reqs := make([]string, 0, len(got.GetGrants()))
			for _, g := range got.GetGrants() {
				reqs = append(reqs, g.GetRequestId())
			}
			if strings.Join(reqs, ",") != strings.Join(tc.wantReqs, ",") {
				t.Errorf("request_id 序列 = %v，期望 %v（倒序）", reqs, tc.wantReqs)
			}
			if db.grantListCalls == 0 {
				t.Errorf("没有触库却有结果，假实现被绕过")
			}
		})
	}
}

// TestListGrantsGuardTableBeforeQuery：五类非法过滤各自返回**哪一个**哨兵，
// 并且全部发生在读台账之前（计数为 0）。
func TestListGrantsGuardTableBeforeQuery(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	seedGrant(db, ledgerRow(0, 4201, model.VipTypePremium, model.ActionGrant, 31,
		now, model.GrantSourceSandboxPurchase, bizOrder, "g-1"))

	cases := []struct {
		name string
		in   *rpc.ListGrantsReq
		want error
	}{
		{"mid 为负", &rpc.ListGrantsReq{Mid: -1}, model.ErrInvalidMid},
		{"档位越界", &rpc.ListGrantsReq{Mid: 4201, VipType: rpc.VipType(6)}, model.ErrInvalidVipType},
		{"来源越界", &rpc.ListGrantsReq{Source: rpc.GrantSource(9)}, model.ErrGrantSourceRequired},
		{"时间区间反了", &rpc.ListGrantsReq{FromTs: now, ToTs: now - testDay}, model.ErrInvalidQueryFilter},
		{"单号超列宽", &rpc.ListGrantsReq{BizOrderNo: strings.Repeat("S", model.MaxBizNoLength+1)}, model.ErrFieldTooLong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := db.grantListCalls
			got, err := NewListGrantsLogic(context.Background(), svcCtx).ListGrants(tc.in)
			mustErrIs(t, err, tc.want, "ListGrants "+tc.name)
			if got != nil {
				t.Errorf("守卫未过不得返回应答，实得 %+v", got)
			}
			if db.grantListCalls != before {
				t.Fatalf("非法过滤前已触库 %d 次，守卫必须先于查询", db.grantListCalls-before)
			}
		})
	}

	// 恰好等于列宽的单号必须放过（校验按字符数而不是字节数，见 guard.checkLen）。
	okReq := &rpc.ListGrantsReq{BizOrderNo: strings.Repeat("S", model.MaxBizNoLength)}
	if _, err := NewListGrantsLogic(context.Background(), svcCtx).ListGrants(okReq); err != nil {
		t.Errorf("%d 字符的单号被判非法：%v", model.MaxBizNoLength, err)
	}
	// 只给 FromTs（不给 ToTs）是合法开区间口径，不该被时间校验误杀。
	if _, err := NewListGrantsLogic(context.Background(), svcCtx).
		ListGrants(&rpc.ListGrantsReq{FromTs: now - testDay}); err != nil {
		t.Errorf("只给 from_ts 应合法：%v", err)
	}
	// UNSPECIFIED 的档位/来源是「不过滤」，不能被判成非法枚举。
	if _, err := NewListGrantsLogic(context.Background(), svcCtx).
		ListGrants(&rpc.ListGrantsReq{Mid: 4201}); err != nil {
		t.Errorf("缺省档位/来源应视为不过滤：%v", err)
	}
}

// TestListGrantsPagingIsClampedAndEchoed：超限 size 裁剪到上限、offset 按裁剪后的 size 算，
// reply 回显实际生效值（契约带了 page/size，客户端要能对上自己拿到的那一页）。
func TestListGrantsPagingIsClampedAndEchoed(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	for i := 1; i <= 5; i++ {
		seedGrant(db, ledgerRow(0, 4301, model.VipTypePremium, model.ActionExtend, 31,
			int64(i)*100+now, model.GrantSourceSandboxAutoRenew, fmt.Sprintf("SO-%02d", i), fmt.Sprintf("g-%d", i)))
	}

	cases := []struct {
		name       string
		page, size int64
		wantPage   int64
		wantSize   int64
		wantOffset int64
		wantRows   int
	}{
		{"超限 size 夹到 100", 1, 5000, 1, 100, 0, 5},
		{"负页码归 1", -3, 10, 1, 10, 0, 5},
		{"第二页偏移按 size 算", 2, 2, 2, 2, 2, 2},
		{"越界页是空页不是错误", 40, 2, 40, 2, 78, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewListGrantsLogic(context.Background(), svcCtx).
				ListGrants(&rpc.ListGrantsReq{Mid: 4301, Page: tc.page, Size: tc.size})
			mustNoError(t, err, "ListGrants "+tc.name)
			if got.GetPage() != tc.wantPage || got.GetSize() != tc.wantSize {
				t.Errorf("回显 page/size = %d/%d，期望 %d/%d", got.GetPage(), got.GetSize(), tc.wantPage, tc.wantSize)
			}
			if db.lastGrantQuery.Offset != tc.wantOffset || db.lastGrantQuery.Limit != tc.wantSize {
				t.Fatalf("落到 SQL 的 offset/limit = %d/%d，期望 %d/%d",
					db.lastGrantQuery.Offset, db.lastGrantQuery.Limit, tc.wantOffset, tc.wantSize)
			}
			if len(got.GetGrants()) != tc.wantRows {
				t.Errorf("本页 %d 行，期望 %d 行", len(got.GetGrants()), tc.wantRows)
			}
			if got.GetTotal() != 5 {
				t.Errorf("total = %d，期望 5（总数不随分页变化）", got.GetTotal())
			}
		})
	}
}

// TestListGrantsReadFailureAndEmptyLedger：台账读失败必须上抛；
// 真的没有台账才是 total=0 + 空列表（README §5：只增不删，查不到就是没发生过）。
func TestListGrantsReadFailureAndEmptyLedger(t *testing.T) {
	svcCtx, db := newTestSvc(t)

	got, err := NewListGrantsLogic(context.Background(), svcCtx).ListGrants(&rpc.ListGrantsReq{Mid: 4401})
	mustNoError(t, err, "ListGrants 空台账")
	if got.GetTotal() != 0 || got.GetGrants() == nil || len(got.GetGrants()) != 0 {
		t.Errorf("空台账应是 total=0 + 空列表，实得 total=%d grants=%v", got.GetTotal(), got.GetGrants())
	}

	db.grantErr = errLedgerDown
	reply, err := NewListGrantsLogic(context.Background(), svcCtx).ListGrants(&rpc.ListGrantsReq{Mid: 4401})
	if !errors.Is(err, errLedgerDown) {
		t.Fatalf("台账读失败必须上抛，实得 %v", err)
	}
	if reply != nil {
		t.Errorf("读失败不得返回 total=0 冒充「没有台账」，实得 %+v", reply)
	}
}

// --- ListExpiringMemberships ---

// TestListExpiringMembershipsClosedIntervalAscending：闭区间两端都含，区间外一律不出现，
// 按 expire_at 升序投喂（cron 靠这个顺序推进），不足一批时游标回 0 表示扫完。
func TestListExpiringMembershipsClosedIntervalAscending(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	from := nowSec()
	seedMembership(db, &model.Membership{Mid: 5001, VipType: model.VipTypePremium, ExpireAt: from - 1})
	onFrom := seedMembership(db, &model.Membership{Mid: 5002, VipType: model.VipTypePremium, ExpireAt: from})
	middle := seedMembership(db, &model.Membership{Mid: 5003, VipType: model.VipTypePremiumPlus, ExpireAt: from + 50})
	onTo := seedMembership(db, &model.Membership{Mid: 5004, VipType: model.VipTypePremium, ExpireAt: from + 100})
	seedMembership(db, &model.Membership{Mid: 5005, VipType: model.VipTypePremium, ExpireAt: from + 101})

	got, err := NewListExpiringMembershipsLogic(context.Background(), svcCtx).ListExpiringMemberships(
		&rpc.ListExpiringMembershipsReq{FromExpireAt: from, ToExpireAt: from + 100, Limit: 10})
	mustNoError(t, err, "ListExpiringMemberships")
	q := db.lastExpireScan
	if q.from != from || q.to != from+100 || q.limit != 10 {
		t.Fatalf("扫描参数 = %+v，期望 from=%d to=%d limit=10", q, from, from+100)
	}
	if db.lastExpireAutoRenew {
		t.Errorf("auto_renew_only 默认应为 false（置灰批次要扫全部到期行）")
	}
	want := []*model.Membership{onFrom, middle, onTo}
	if len(got.GetMemberships()) != len(want) {
		t.Fatalf("投喂 %d 行，期望 %d 行（闭区间含两端、区间外不出现）", len(got.GetMemberships()), len(want))
	}
	for i, m := range got.GetMemberships() {
		assertMembershipEchoed(t, m, want[i], fmt.Sprintf("ListExpiring[%d]", i))
	}
	if got.GetNextExpireAtCursor() != 0 {
		t.Errorf("不足一批仍给游标 %d，期望 0（0 表示本区间已扫完）", got.GetNextExpireAtCursor())
	}
}

// TestListExpiringMembershipsFullBatchHandsOutCursor：满批才给游标，
// 且游标是「本批最后一条的 expire_at」——下一批从它起步会重复投喂边界那一条，
// 这是刻意的宁重不漏（README §5），去重由 ExpireMembership 的 EXPIRE 台账负责。
func TestListExpiringMembershipsFullBatchHandsOutCursor(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	for i, offset := range []int64{10, 20, 30, 40} {
		seedMembership(db, &model.Membership{Mid: int64(5100 + i), VipType: model.VipTypePremium,
			ExpireAt: now + offset})
	}

	first, err := NewListExpiringMembershipsLogic(context.Background(), svcCtx).ListExpiringMemberships(
		&rpc.ListExpiringMembershipsReq{FromExpireAt: now, ToExpireAt: now + 100, Limit: 2})
	mustNoError(t, err, "ListExpiring 满批")
	if len(first.GetMemberships()) != 2 {
		t.Fatalf("第一批 %d 行，期望 2 行", len(first.GetMemberships()))
	}
	cursor := first.GetNextExpireAtCursor()
	if cursor != now+20 {
		t.Fatalf("游标 = %d，期望本批最后一条的 expire_at=%d", cursor, now+20)
	}

	second, err := NewListExpiringMembershipsLogic(context.Background(), svcCtx).ListExpiringMemberships(
		&rpc.ListExpiringMembershipsReq{FromExpireAt: cursor, ToExpireAt: now + 100, Limit: 2})
	mustNoError(t, err, "ListExpiring 第二批")
	if len(second.GetMemberships()) != 2 {
		t.Fatalf("第二批 %d 行，期望 2 行", len(second.GetMemberships()))
	}
	// 闭区间游标必然重复投喂边界那一行：这条断言就是「宁重不漏」的可观察证据。
	if second.GetMemberships()[0].GetExpireAt() != cursor {
		t.Errorf("第二批首行 expire_at = %d，期望与游标 %d 相同（重复投喂由去重挡住）",
			second.GetMemberships()[0].GetExpireAt(), cursor)
	}
	if second.GetMemberships()[1].GetExpireAt() != now+30 {
		t.Errorf("第二批次行 expire_at = %d，期望 %d", second.GetMemberships()[1].GetExpireAt(), now+30)
	}
}

// 缺陷：listexpiringmembershipslogic.go:59-61 —— 游标只带 expire_at（秒级），而
// model/membership.go:149 的区间条件是 `expire_at BETWEEN ? AND ?`（闭区间、含游标本秒），
// model/membership.go:156 的排序是 (expire_at, membership_id)：同一秒内的到期行数 >= limit 时，
// 游标 = 该秒的 expire_at，下一批从同一秒起步又拿满 limit 行，游标原地不动 ——
// cron 按游标推进就会在这批行上死循环，区间里更晚的到期行永远扫不到（漏到期）。
// 契约（rpc/membership.proto:341-344）只有一个 next_expire_at_cursor，没有行级游标位，
// MembershipInfo 也不带 membership_id，调用方即使想自己按主键去重也拿不到依据。
// 修法方向：游标改成 (expire_at, membership_id) 复合（需先改 .proto 再生成），
// 或 reply 回显本批最后一条的 membership_id + 是否在区间内还有同秒行。
// 下面这条用例钉住当前（错误）行为：三行同秒、limit=2 时，第二批返回的是同一批前两行。
func TestListExpiringMembershipsCursorCannotAdvanceWithinSameSecond(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	sameSecond := now + 7*testDay
	for i := 0; i < 3; i++ {
		seedMembership(db, &model.Membership{Mid: int64(5200 + i), VipType: model.VipTypePremium,
			ExpireAt: sameSecond})
	}

	first, err := NewListExpiringMembershipsLogic(context.Background(), svcCtx).ListExpiringMemberships(
		&rpc.ListExpiringMembershipsReq{FromExpireAt: now, ToExpireAt: sameSecond, Limit: 2})
	mustNoError(t, err, "ListExpiring 同秒第一批")
	if len(first.GetMemberships()) != 2 {
		t.Fatalf("第一批 %d 行，期望满批 2 行", len(first.GetMemberships()))
	}
	if first.GetNextExpireAtCursor() != sameSecond {
		t.Fatalf("游标 = %d，期望等于同秒到期时刻 %d", first.GetNextExpireAtCursor(), sameSecond)
	}

	// 游标不前进：按游标续扫只会拿到同样的两行，第三行永远扫不到。
	second, err := NewListExpiringMembershipsLogic(context.Background(), svcCtx).ListExpiringMemberships(
		&rpc.ListExpiringMembershipsReq{FromExpireAt: first.GetNextExpireAtCursor(), ToExpireAt: sameSecond, Limit: 2})
	mustNoError(t, err, "ListExpiring 同秒第二批")
	if len(second.GetMemberships()) != 2 {
		t.Fatalf("续扫 %d 行，期望仍是 2 行", len(second.GetMemberships()))
	}
	for i := range first.GetMemberships() {
		if first.GetMemberships()[i].GetMid() != second.GetMemberships()[i].GetMid() {
			t.Fatalf("续扫竟然换了行：第一批 %d/%d，第二批 %d/%d —— 缺陷描述需更新",
				first.GetMemberships()[i].GetMid(), second.GetMemberships()[i].GetMid(), i, i)
		}
	}
	if second.GetNextExpireAtCursor() != sameSecond {
		t.Errorf("第二批游标 = %d，期望仍卡在 %d（这正是活锁的形态）", second.GetNextExpireAtCursor(), sameSecond)
	}
	// 三行同秒却只能反复看到两行：第三行的到期在游标推进下不可达。
	seen := map[int64]bool{}
	for _, m := range second.GetMemberships() {
		seen[m.GetMid()] = true
	}
	if !seen[5200] || !seen[5201] || seen[5202] {
		t.Errorf("续扫看到的 mid 集合 = %v，期望钉住 {5200,5201}（5202 被同秒游标挡在后面）", seen)
	}
}

// TestListExpiringMembershipsTrimsLimitInsteadOfFailing：批处理调用方不该被参数细节卡住
// （README §5）——超限与缺省的 limit 一律裁剪到 ExpireScanMaxLimit 并落到扫描里。
func TestListExpiringMembershipsTrimsLimitInsteadOfFailing(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	seedMembership(db, &model.Membership{Mid: 5301, VipType: model.VipTypePremium, ExpireAt: now + testDay})

	for _, in := range []int64{0, -5, 999999, 1 << 40} {
		got, err := NewListExpiringMembershipsLogic(context.Background(), svcCtx).ListExpiringMemberships(
			&rpc.ListExpiringMembershipsReq{FromExpireAt: now, ToExpireAt: now + 10*testDay, Limit: in})
		mustNoError(t, err, fmt.Sprintf("limit=%d 应被裁剪而不是报错", in))
		if db.lastExpireScan.limit != int64(testConf().ExpireScanMaxLimit) {
			t.Fatalf("limit=%d 落到扫描的是 %d，期望夹到配置上限 %d",
				in, db.lastExpireScan.limit, testConf().ExpireScanMaxLimit)
		}
		if len(got.GetMemberships()) != 1 {
			t.Errorf("裁剪后仍要正常投喂，实得 %d 行", len(got.GetMemberships()))
		}
	}

	// 上限本身来自配置：调小后必须按新上限走（不是写死 500）。
	svcCtx.Config.Membership.ExpireScanMaxLimit = 3
	if _, err := NewListExpiringMembershipsLogic(context.Background(), svcCtx).ListExpiringMemberships(
		&rpc.ListExpiringMembershipsReq{FromExpireAt: now, ToExpireAt: now + 10*testDay, Limit: 500}); err != nil {
		t.Fatalf("按配置裁剪不该报错：%v", err)
	}
	if db.lastExpireScan.limit != 3 {
		t.Errorf("limit = %d，期望按 ExpireScanMaxLimit=3 裁剪", db.lastExpireScan.limit)
	}
	// 比上限更小的批次要求必须尊重（cron 想要小批量灰度推进）。
	if _, err := NewListExpiringMembershipsLogic(context.Background(), svcCtx).ListExpiringMemberships(
		&rpc.ListExpiringMembershipsReq{FromExpireAt: now, ToExpireAt: now + 10*testDay, Limit: 1}); err != nil {
		t.Fatalf("小批量不该报错：%v", err)
	}
	if db.lastExpireScan.limit != 1 || db.lastExpireScan.from != now {
		t.Errorf("小批量被改写：limit=%d from=%d，期望 1/%d", db.lastExpireScan.limit, db.lastExpireScan.from, now)
	}
}

// TestListExpiringMembershipsAutoRenewOnly：续费批次只扫签约行（idx_auto_renew_expire_at），
// 置灰批次扫全部；这个开关必须落到扫描条件里而不是在 logic 里筛。
func TestListExpiringMembershipsAutoRenewOnly(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	signed := seedMembership(db, fullMembership(5401, model.VipTypePremium, 2*testDay, 1))
	seedMembership(db, fullMembership(5402, model.VipTypePremium, 2*testDay, 0))

	got, err := NewListExpiringMembershipsLogic(context.Background(), svcCtx).ListExpiringMemberships(
		&rpc.ListExpiringMembershipsReq{
			FromExpireAt: now, ToExpireAt: now + 10*testDay, AutoRenewOnly: true, Limit: 10,
		})
	mustNoError(t, err, "ListExpiring 只扫签约")
	if !db.lastExpireAutoRenew {
		t.Fatalf("auto_renew_only 没落到扫描条件（在 logic 里筛会白读未签约行）")
	}
	if len(got.GetMemberships()) != 1 {
		t.Fatalf("投喂 %d 行，期望只有那 1 行签约", len(got.GetMemberships()))
	}
	assertMembershipEchoed(t, got.GetMemberships()[0], signed, "ListExpiring 签约行")
	if !got.GetMemberships()[0].GetAutoRenew() {
		t.Errorf("签约行的 auto_renew 投影成 false 了")
	}
}

// TestListExpiringMembershipsRangeGuardsBeforeScan：区间非法必须在扫描之前拒掉，
// 两个守卫共用一个哨兵，靠报文里的字段名区分是哪一个；from<=0 是「从头扫」的合法口径，
// 归一成 1 后照常扫描（覆盖从未记过到期时间的历史行）。
func TestListExpiringMembershipsRangeGuardsBeforeScan(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	historical := seedMembership(db, &model.Membership{Mid: 5501, VipType: model.VipTypePremium, ExpireAt: 1})

	cases := []struct {
		name        string
		in          *rpc.ListExpiringMembershipsReq
		wantMsgFrag string
	}{
		{"to 为 0", &rpc.ListExpiringMembershipsReq{ToExpireAt: 0}, "to_expire_at=0"},
		{"to 为负", &rpc.ListExpiringMembershipsReq{ToExpireAt: -1}, "to_expire_at=-1"},
		{"from 大于 to", &rpc.ListExpiringMembershipsReq{FromExpireAt: now + 10, ToExpireAt: now}, "from="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := db.memberListExpCalls
			got, err := NewListExpiringMembershipsLogic(context.Background(), svcCtx).
				ListExpiringMemberships(tc.in)
			mustErrIs(t, err, model.ErrInvalidExpireRange, "ListExpiring "+tc.name)
			if !strings.Contains(err.Error(), tc.wantMsgFrag) {
				t.Errorf("错误报文 %q 没有指出触发的守卫（期望含 %q）", err.Error(), tc.wantMsgFrag)
			}
			if got != nil {
				t.Errorf("守卫未过不得返回应答，实得 %+v", got)
			}
			if db.memberListExpCalls != before {
				t.Fatalf("非法区间仍触库 %d 次，守卫必须先于扫描", db.memberListExpCalls-before)
			}
		})
	}

	// from 留空/负数：归一成 1，照常扫到「expire_at=1」的历史行。
	got, err := NewListExpiringMembershipsLogic(context.Background(), svcCtx).ListExpiringMemberships(
		&rpc.ListExpiringMembershipsReq{FromExpireAt: -3, ToExpireAt: now + testDay, Limit: 5})
	mustNoError(t, err, "ListExpiring from 留空")
	if db.lastExpireScan.from != 1 {
		t.Errorf("落到扫描的 from = %d，期望归一成 1（覆盖历史行）", db.lastExpireScan.from)
	}
	if len(got.GetMemberships()) != 1 || got.GetMemberships()[0].GetMid() != historical.Mid {
		t.Fatalf("没扫到那条 expire_at=1 的历史行，实得 %d 行", len(got.GetMemberships()))
	}
}

// TestListExpiringMembershipsReadFailurePropagatesAndNeverMutates：
// 扫描是只读的——身份行一个字节都不许变，读失败必须原样上抛而不是回「本批为空」。
func TestListExpiringMembershipsReadFailurePropagatesAndNeverMutates(t *testing.T) {
	svcCtx, db := newTestSvc(t)
	now := nowSec()
	seed := seedMembership(db, fullMembership(5601, model.VipTypePremium, -testDay, 1)) // 已到期，等 cron 置灰
	req := &rpc.ListExpiringMembershipsReq{FromExpireAt: now - 10*testDay, ToExpireAt: now, Limit: 5}

	got, err := NewListExpiringMembershipsLogic(context.Background(), svcCtx).ListExpiringMemberships(req)
	mustNoError(t, err, "ListExpiring 正常扫描")
	if len(got.GetMemberships()) != 1 {
		t.Fatalf("投喂 %d 行，期望 1 行", len(got.GetMemberships()))
	}
	after := db.members[memberKey(5601, model.VipTypePremium)]
	if *after != *seed {
		t.Errorf("扫描把身份行改了：读前 %+v 读后 %+v（置灰只能由 ExpireMembership 记账）", *seed, *after)
	}
	if db.txRuns != 0 || len(db.grants) != 0 {
		t.Errorf("扫描有写入痕迹：tx=%d grant=%d", db.txRuns, len(db.grants))
	}

	db.memberErr = errIdentityDown
	reply, err := NewListExpiringMembershipsLogic(context.Background(), svcCtx).ListExpiringMemberships(req)
	if !errors.Is(err, errIdentityDown) {
		t.Fatalf("扫描读失败必须上抛，实得 %v", err)
	}
	if reply != nil {
		t.Errorf("读失败不得返回空批冒充「这段时间没人到期」，实得 %+v", reply)
	}
}

// --- 小工具 ---

// codesOf 把权益码投影抽成列表，便于按顺序断言。
func codesOf(list []*rpc.EntitlementInfo) []string {
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, e.GetCode())
	}
	return out
}
