package logic

// profile3logic_test.go 覆盖 Profile3（logic/profile3logic.go:28-37 →
// repository.Profile internal/repository/account.go:126-144 → RawProfile raw.go:152-208）。
//
// 被测判定链：读 p3_<mid> → 命中即返回缓存值，**下游与本地两张表都不查** →
// 未命中按固定顺序聚合：user-profile.Member（昵称/头像/签名/排名/等级/生日/节操/官方认证）
// → user-profile.RealnameStatus（identification）→ 本地 account（is_tourist、join_time、
// status==1 才 silence=1）→ 本地 account_credential（**只看 status==0**：手机→tel_status、
// 邮箱→email_status）→ 回填 p3_<mid>。
//
// 钉住的事实：
//  1. 下游失败只丢下游字段，本地两张表的事实照样合并进响应（raw.go:156-158 只记日志）；
//  2. 凭证的 status 过滤在这里是**存在**的（raw.go:196），与 InfosByName3 的反查
//     SQL 没有 status 过滤（README 已知缺口 2）形成正反对；
//  3. vip / pendant / nameplate 恒为 nil —— AGENTS.md §1 商业化与装饰性字段降级；
//  4. join_time 走 int32（proto 字段 8）：2038-01-19 之后注册的账号时间会被截断，
//     见 TestProfile3JoinTimeIsInt32AtThe2038Boundary 与 README 缺口 21；
//  5. Profile 的 mid 取的是**下游回的 mid**（raw.go:159），缓存键却是请求的 mid，
//     见 TestProfile3AdoptsDownstreamMidUnderRequestedKey 与 README 缺口 20。
//
// 覆盖不到的分支（如实声明）：Profile3 的 `err != nil` 与 `profile == nil` 两支都不可达
// —— RawProfile 的唯一 return 是 (profile, nil)，Repository.Profile 的 err 只可能来自它
// （README 缺口 18）。

import (
	"context"
	"testing"

	"go-video/services/account/internal/repository"
	"go-video/services/account/model"
	"go-video/services/account/rpc"
)

func callProfile3(t *testing.T, e *env, mid int64) (*rpc.ProfileReply, error) {
	t.Helper()
	return NewProfile3Logic(context.Background(), e.svcCtx).Profile3(&rpc.MidReq{Mid: mid, RealIp: "1.2.3.4"})
}

func fullMember(mid int64) *repository.UserProfileMember {
	return &repository.UserProfileMember{
		UserProfileBase: repository.UserProfileBase{
			Mid: mid, Name: "阿莉", Sex: "女", Face: "https://cdn/f.png", Sign: "签名", Rank: 5,
		},
		Level:    6,
		Birthday: 631152000,
		Moral:    70,
		Official: &rpc.OfficialInfo{Role: 1, Title: "官方账号", Desc: "认证说明"},
	}
}

func phoneCred(mid int64, status int8) *model.AccountCredential {
	return &model.AccountCredential{Mid: mid, CredentialType: model.CredentialTypePhone, Identifier: "13800000000", Status: status}
}

func emailCred(mid int64, status int8) *model.AccountCredential {
	return &model.AccountCredential{Mid: mid, CredentialType: model.CredentialTypeEmail, Identifier: "a@b.com", Status: status}
}

func usernameCred(mid int64, status int8) *model.AccountCredential {
	return &model.AccountCredential{Mid: mid, CredentialType: model.CredentialTypeUsername, Identifier: "alice", Status: status}
}

// TestProfile3CacheHitSkipsDownstreamAndLocalTables 命中即短路：四路数据源一个都不许多查。
// 这正是「改了资料要等 DelCache 才生效」的根因（缓存失效面属 DelCache 轮，本文件不测）。
func TestProfile3CacheHitSkipsDownstreamAndLocalTables(t *testing.T) {
	const mid = int64(70001)
	cached := &rpc.Profile{Mid: mid, Name: "缓存里的名字", Level: 9, TelStatus: 1, Silence: 1, JoinTime: 123}
	e := newEnv(t)
	st := e.st
	st.cache.warmMsg(profileKey(mid), cached, 11) // TTL 故意不是 3600
	st.account.put(&model.Account{Mid: mid, Status: 0, CreatedAt: 999999})
	st.cred.put(phoneCred(mid, 0))
	st.log.reset()

	reply, err := callProfile3(t, e, mid)
	wantNoErr(t, "缓存命中的 Profile3", err)
	wantProto(t, "命中的响应", "profile", reply.GetProfile(), cached)
	wantOps(t, "命中只该读一次缓存", e.ops(0), []string{"cache.CacheProfile:" + profileKey(mid)})
	wantNoOpsWith(t, "命中路径", e.ops(0), "userProfile.")
	wantNoOpsWith(t, "命中路径", e.ops(0), "account.FindOne")
	wantNoOpsWith(t, "命中路径", e.ops(0), "cred.FindByMid")
	if ttl, ok := st.cache.ttlOf(profileKey(mid)); !ok || ttl != 11 {
		t.Errorf("命中路径改了 TTL：%d(存在=%v), want 保持 11", ttl, ok)
	}
}

// TestProfile3CacheMissMergesFourSourcesInFixedOrder 未命中的完整聚合：
// 顺序、字段映射、回填内容与过期时间一起钉。
func TestProfile3CacheMissMergesFourSourcesInFixedOrder(t *testing.T) {
	const mid = int64(70001)
	e := newEnv(t, withDownstream(mid, Downstream{
		Member:   fullMember(mid),
		Realname: i32(1),
	}))
	st := e.st
	st.account.put(&model.Account{Mid: mid, Status: 0, IsTourist: 1, CreatedAt: 1700000000, RegIP: "10.0.0.1"})
	st.cred.put(usernameCred(mid, 0))
	st.cred.put(phoneCred(mid, 0))
	st.cred.put(emailCred(mid, 0))
	st.log.reset()

	want := &rpc.Profile{
		Mid: mid, Name: "阿莉", Sex: "女", Face: "https://cdn/f.png", Sign: "签名", Rank: 5,
		Level: 6, JoinTime: 1700000000, Moral: 70, Silence: 0,
		EmailStatus: 1, TelStatus: 1, Identification: 1,
		Official:  &rpc.OfficialInfo{Role: 1, Title: "官方账号", Desc: "认证说明"},
		Birthday:  631152000,
		IsTourist: 1,
		Vip:       nil, // 商业化范围外（AGENTS.md §1）：这三个字段恒不填
		Pendant:   nil,
		Nameplate: nil,
	}
	reply, err := callProfile3(t, e, mid)
	wantNoErr(t, "回源的 Profile3", err)
	wantProto(t, "四路聚合结果", "profile", reply.GetProfile(), want)
	wantOps(t, "Member→实名→account→credential→回填", e.ops(0), []string{
		"cache.CacheProfile:" + profileKey(mid),
		"userProfile.Member:" + itoa(mid),
		"userProfile.RealnameStatus:" + itoa(mid),
		"account.FindOne:" + itoa(mid),
		"cred.FindByMid:" + itoa(mid),
		"cache.AddCacheProfile:" + profileKey(mid),
	})
	got, ok := msgAs[*rpc.Profile](st.cache, profileKey(mid))
	if !ok {
		t.Fatalf("回填没落到 p3_ 键（当前 key：%v）", st.cache.keys())
	}
	wantProto(t, "回填内容", "p3_"+itoa(mid), got, want)
	if ttl, ok := st.cache.ttlOf(profileKey(mid)); !ok || ttl != 3600 {
		t.Errorf("回填 TTL = %d(存在=%v), want 3600", ttl, ok)
	}
}

// TestProfile3LocalTableBoundaries 本地两张表贡献的四个字段（silence/tel/email/join_time）
// 的边界对：每一对都只差在**一个**条件位上，条件写反或写漏都会红。
func TestProfile3LocalTableBoundaries(t *testing.T) {
	const mid = int64(70001)
	cases := []struct {
		name      string
		acc       *model.Account // nil = account 表没有这一行
		creds     []*model.AccountCredential
		wantSilen int32
		wantTel   int32
		wantEmail int32
		wantTour  int32
	}{
		{
			name:      "正常账号+手机邮箱均已绑",
			acc:       &model.Account{Mid: mid, Status: 0},
			creds:     []*model.AccountCredential{phoneCred(mid, 0), emailCred(mid, 0)},
			wantSilen: 0, wantTel: 1, wantEmail: 1, wantTour: 0,
		},
		{
			name:      "status=1 才算禁言",
			acc:       &model.Account{Mid: mid, Status: 1},
			creds:     []*model.AccountCredential{phoneCred(mid, 0)},
			wantSilen: 1, wantTel: 1, wantEmail: 0, wantTour: 0,
		},
		{
			name:      "status=2 注销中不算禁言",
			acc:       &model.Account{Mid: mid, Status: 2},
			creds:     []*model.AccountCredential{phoneCred(mid, 0)},
			wantSilen: 0, wantTel: 1, wantEmail: 0, wantTour: 0,
		},
		{
			name:      "已解绑的手机号不再算绑定",
			acc:       &model.Account{Mid: mid, Status: 0},
			creds:     []*model.AccountCredential{phoneCred(mid, 1), emailCred(mid, 0)},
			wantSilen: 0, wantTel: 0, wantEmail: 1, wantTour: 0,
		},
		{
			name:      "只有用户名凭证既不算手机也不算邮箱",
			acc:       &model.Account{Mid: mid, Status: 0},
			creds:     []*model.AccountCredential{usernameCred(mid, 0)},
			wantSilen: 0, wantTel: 0, wantEmail: 0, wantTour: 0,
		},
		{
			name:      "account 表没有行时三个字段全零",
			acc:       nil,
			creds:     []*model.AccountCredential{phoneCred(mid, 0)},
			wantSilen: 0, wantTel: 1, wantEmail: 0, wantTour: 0,
		},
		{
			name:      "游客账号",
			acc:       &model.Account{Mid: mid, Status: 0, IsTourist: 1},
			creds:     nil,
			wantSilen: 0, wantTel: 0, wantEmail: 0, wantTour: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, withDownstream(mid, Downstream{Member: fullMember(mid), Realname: i32(0)}))
			st := e.st
			if tc.acc != nil {
				st.account.put(tc.acc)
			}
			for _, c := range tc.creds {
				st.cred.put(c)
			}
			st.log.reset()

			reply, err := callProfile3(t, e, mid)
			wantNoErr(t, "Profile3", err)
			p := reply.GetProfile()
			wantEQ(t, "silence", "值", p.GetSilence(), tc.wantSilen)
			wantEQ(t, "tel_status", "值", p.GetTelStatus(), tc.wantTel)
			wantEQ(t, "email_status", "值", p.GetEmailStatus(), tc.wantEmail)
			wantEQ(t, "is_tourist", "值", p.GetIsTourist(), tc.wantTour)
			// 下游字段不受本地表影响，始终来自 Member。
			wantEQ(t, "name", "值", p.GetName(), "阿莉")
			if tc.acc == nil {
				wantEQ(t, "account 无行时 join_time", "值", p.GetJoinTime(), 0)
			}
		})
	}
}

// TestProfile3JoinTimeIsInt32AtThe2038Boundary join_time 在 proto 里是 int32
// （rpc/account.proto:42），RawProfile 直接 int32(acc.CreatedAt) 截断（raw.go:185）。
//
// TODO(缺陷)（README 缺口 21）：2038-01-19 03:14:07 之后注册的账号，join_time 会
// 变成负数（客户端按无符号或按 0 处理都会显示成错误时间）。此处钉住**当前**截断行为：
// 边界前一秒不变、后一秒翻负。真正修法是契约把 join_time 改 int64（本轮不动 services/**）。
func TestProfile3JoinTimeIsInt32AtThe2038Boundary(t *testing.T) {
	const mid = int64(70001)
	for _, tc := range []struct {
		name      string
		createdAt int64
		want      int32
	}{
		{"2038-01-19 03:14:07（int32 上限，不截断）", 2147483647, 2147483647},
		{"2038-01-19 03:14:08（溢出为负）", 2147483648, -2147483648},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, withDownstream(mid, Downstream{Member: fullMember(mid)}))
			e.st.account.put(&model.Account{Mid: mid, Status: 0, CreatedAt: tc.createdAt})
			e.st.log.reset()

			reply, err := callProfile3(t, e, mid)
			wantNoErr(t, "Profile3", err)
			wantEQ(t, "join_time", "值", reply.GetProfile().GetJoinTime(), tc.want)
		})
	}
}

// TestProfile3DownstreamFaultKeepsLocalFacts user-profile 挂掉（Member 与 RealnameStatus
// 都不通）时：昵称/等级等下游字段退回零值，但本地 account/credential 的事实必须仍在，
// 而且整条链路照样回填。
func TestProfile3DownstreamFaultKeepsLocalFacts(t *testing.T) {
	const mid = int64(70001)
	e := newEnv(t) // 不布下游 → Member / RealnameStatus 都返回 errDownstreamWired
	st := e.st
	st.account.put(&model.Account{Mid: mid, Status: 1, IsTourist: 1, CreatedAt: 1600000000})
	st.cred.put(phoneCred(mid, 0))
	st.log.reset()

	want := &rpc.Profile{
		Mid: mid, Silence: 1, TelStatus: 1, JoinTime: 1600000000, IsTourist: 1,
	}
	reply, err := callProfile3(t, e, mid)
	wantNoErr(t, "下游故障的 Profile3 必须降级不报错", err)
	wantProto(t, "只剩本地事实", "profile", reply.GetProfile(), want)
	wantOps(t, "下游故障照样走完四路", e.ops(0), []string{
		"cache.CacheProfile:" + profileKey(mid),
		"userProfile.Member:" + itoa(mid),
		"userProfile.RealnameStatus:" + itoa(mid),
		"account.FindOne:" + itoa(mid),
		"cred.FindByMid:" + itoa(mid),
		"cache.AddCacheProfile:" + profileKey(mid),
	})
	// 缺口 17 的 Profile 形态：这份「只有本地事实」的残缺资料会带 3600 秒 TTL 进缓存。
	got, ok := msgAs[*rpc.Profile](st.cache, profileKey(mid))
	if !ok {
		t.Fatalf("前提已变：残缺资料竟然没进缓存，请复核 account.go:138-142")
	}
	wantProto(t, "进缓存的残缺资料", "p3_"+itoa(mid), got, want)
}

// TestProfile3AdoptsDownstreamMidUnderRequestedKey profile.mid 取的是 Member 回的值
// （raw.go:159），缓存键却是请求的 mid（account.go:140）。
//
// TODO(缺陷)（README 缺口 20）：一旦 user-profile 回错 mid（串号/迁移期双写），本服务会
// 把「A 的资料」挂在 B 的缓存键上并供一小时，且响应里的 mid 与请求的 mid 不一致，
// 调用方（网关）无从发现。此处钉住当前无校验的行为。
func TestProfile3AdoptsDownstreamMidUnderRequestedKey(t *testing.T) {
	const (
		requested = int64(70001)
		returned  = int64(999)
	)
	member := fullMember(returned) // 故意让下游回另一个 mid
	e := newEnv(t, withDownstream(requested, Downstream{Member: member, Realname: i32(1)}))
	st := e.st
	st.account.put(&model.Account{Mid: requested, Status: 0, CreatedAt: 1500000000})
	st.log.reset()

	reply, err := callProfile3(t, e, requested)
	wantNoErr(t, "Profile3", err)
	p := reply.GetProfile()
	wantEQ(t, "响应 mid 跟着下游跑", "值", p.GetMid(), returned)
	wantEQ(t, "本地 join_time 仍按请求 mid 查到的行填", "值", p.GetJoinTime(), 1500000000)
	// 回填的键是请求 mid，值是下游 mid 的资料 —— 键与载荷不一致（缺口 20 的实质）。
	got, ok := msgAs[*rpc.Profile](st.cache, profileKey(requested))
	if !ok {
		t.Fatalf("前置条件破坏：p3_%d 没被写", requested)
	}
	wantEQ(t, "p3_70001 里挂着的 mid", "值", got.GetMid(), returned)
	wantNoOpsWith(t, "回源路径", e.ops(0), "account.FindOne:"+itoa(returned))
}

// TestProfile3MidZeroAndOneUseDifferentCacheKeys 与 Info3 同口径：Profile3 不校验 mid，
// p3_0 与 p3_1 是两条独立缓存。
func TestProfile3MidZeroAndOneUseDifferentCacheKeys(t *testing.T) {
	for _, mid := range []int64{0, 1} {
		t.Run(itoa(mid), func(t *testing.T) {
			e := newEnv(t, withDownstream(mid, Downstream{
				Member:   &repository.UserProfileMember{UserProfileBase: repository.UserProfileBase{Mid: mid, Name: "mid" + itoa(mid)}},
				Realname: i32(0),
			}))
			reply, err := callProfile3(t, e, mid)
			wantNoErr(t, "Profile3", err)
			wantEQ(t, "mid", "值", reply.GetProfile().GetMid(), mid)
			wantEQ(t, "name", "值", reply.GetProfile().GetName(), "mid"+itoa(mid))
			wantOps(t, "缓存键按 mid 派生", e.ops(0), []string{
				"cache.CacheProfile:" + profileKey(mid),
				"userProfile.Member:" + itoa(mid),
				"userProfile.RealnameStatus:" + itoa(mid),
				"account.FindOne:" + itoa(mid),
				"cred.FindByMid:" + itoa(mid),
				"cache.AddCacheProfile:" + profileKey(mid),
			})
		})
	}
}
