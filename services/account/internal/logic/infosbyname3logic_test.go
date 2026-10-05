package logic

// infosbyname3logic_test.go 覆盖 InfosByName3（logic/infosbyname3logic.go:30-52 →
// repository.MidsByName internal/repository/mids_by_name.go:12-20 →
// model/account_credential.go:88-115 → repository.Infos account.go:33-66）。
//
// 被测判定链：names 截断到 100 → 用 credential_type=1（用户名）反查 name→mid →
// 拿到的 mid 列表整体交给 Infos（于是复用 i3_ 缓存分组回源那一整套口径）→ 结果
// **按 mid 归键**，不是按 name 归键。
//
// 钉住的事实：
//  1. 只有用户名凭证能被反查：手机号/邮箱即便在 account_credential 里也不参与
//     （mids_by_name.go:19 写死 CredentialTypeUsername）；
//  2. 100 条上限是**真的生效**（第 101 条不进 SQL 参数）；
//  3. 反查失败（DB 故障）是这批 8 个资料方法里唯一能离线触达的错误分支：
//     错误必须原样外传、响应为 nil、且**一步缓存/下游都不许走**；
//  4. name→mid 来自 Go map，因此 mid 列表顺序不可断言：多 mid 的用例只比集合。
//
// 与真实 SQL 的口径差（不夸大为已覆盖）：替身把**入参原样**记进轨迹
// （fakes_test.go:FindMidsByIdentifiers），真实 model 会先 lowercase + 去重再拼 IN
// （account_credential.go:92-101）。因此截断类用例统一用小写、互不重复的名字，
// 使两侧口径重合；大小写用例断言的是「能否解析出同一个 mid」这一语义结论
// （替身用 EqualFold 复刻 LOWER(identifier)，真实实现用 SQL 的 LOWER），
// 不宣称覆盖了 SQL 文本本身。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/account/internal/repository"
	"go-video/services/account/model"
	"go-video/services/account/rpc"
)

func callInfosByName3(t *testing.T, e *env, names ...string) (*rpc.InfosReply, error) {
	t.Helper()
	return NewInfosByName3Logic(context.Background(), e.svcCtx).InfosByName3(&rpc.NamesReq{Names: names, RealIp: "1.2.3.4"})
}

// seqNames 造 n 个小写、互不重复的名字（u001..u999），用于截断边界。
func seqNames(n int) []string {
	out := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, "u"+strings.Repeat("0", 3-len(itoa(int64(i))))+itoa(int64(i)))
	}
	return out
}

// TestInfosByName3ResolvesUsernameThenReadsInfos 一条名字 → 一个 mid → 完整四步链路：
// 反查、读缓存、回源、回填，一步不多一步不少。
func TestInfosByName3ResolvesUsernameThenReadsInfos(t *testing.T) {
	const mid = int64(70001)
	e := newEnv(t, withDownstream(mid, Downstream{
		Base: &repository.UserProfileBase{Mid: mid, Name: "alice", Sex: "保密", Rank: 3},
	}))
	st := e.st
	st.cred.put(&model.AccountCredential{Mid: mid, CredentialType: model.CredentialTypeUsername, Identifier: "alice", Status: 0})
	st.account.put(&model.Account{Mid: mid, Status: 0})
	st.log.reset()

	reply, err := callInfosByName3(t, e, "alice")
	wantNoErr(t, "InfosByName3", err)
	// 结果按 mid 归键（不是按 name），调用方拿到的是和 Infos3 同一形状的 map。
	wantEQ(t, "结果键", "infos 条数", len(reply.GetInfos()), 1)
	wantProto(t, "按 mid 归键的结果", "infos[mid]", reply.GetInfos()[mid],
		&rpc.Info{Mid: mid, Name: "alice", Sex: "保密", Rank: 3})
	if _, ok := reply.GetInfos()[0]; ok {
		t.Errorf("结果里出现了 mid=0 的条目，说明反查或归键走偏了：%v", reply.GetInfos())
	}
	wantOps(t, "反查→缓存→回源→回填", e.ops(0), []string{
		"cred.FindMidsByIdentifiers:1/alice",
		"cache.CacheInfo:" + infoKey(mid),
		"userProfile.Bases:" + itoa(mid),
		"cache.AddCacheInfo:" + infoKey(mid),
	})
}

// TestInfosByName3OnlyUsernameCredentialsResolve 手机号/邮箱标识查不到：
// 反查用的凭证类型写死为用户名，因此即使 account_credential 里有这行也不参与。
func TestInfosByName3OnlyUsernameCredentialsResolve(t *testing.T) {
	const mid = int64(70001)
	e := newEnv(t)
	st := e.st
	st.cred.put(&model.AccountCredential{Mid: mid, CredentialType: model.CredentialTypePhone, Identifier: "13800000000", Status: 0})
	st.cred.put(&model.AccountCredential{Mid: mid, CredentialType: model.CredentialTypeEmail, Identifier: "a@b.com", Status: 0})
	st.log.reset()

	reply, err := callInfosByName3(t, e, "13800000000", "a@b.com")
	wantNoErr(t, "非用户名标识", err)
	wantEQ(t, "非用户名标识不得出结果", "infos 条数", len(reply.GetInfos()), 0)
	wantOps(t, "只该有一次反查", e.ops(0), []string{"cred.FindMidsByIdentifiers:1/13800000000,a@b.com"})
	wantNoOpsWith(t, "反查为空的路径", e.ops(0), "cache.")
	wantNoOpsWith(t, "反查为空的路径", e.ops(0), "userProfile.")
}

// TestInfosByName3EmptyNamesDoesNotTouchDB 空 names 零调用：MidsByName 与 Infos
// 都在触 Redis/MySQL 之前就 return 了（mids_by_name.go:13、account.go:34）。
func TestInfosByName3EmptyNamesDoesNotTouchDB(t *testing.T) {
	for _, tc := range []struct {
		name  string
		names []string
	}{
		{"nil", nil},
		{"空切片", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			reply, err := NewInfosByName3Logic(context.Background(), e.svcCtx).InfosByName3(&rpc.NamesReq{Names: tc.names})
			wantNoErr(t, "InfosByName3", err)
			if reply.GetInfos() == nil {
				t.Errorf("infos = nil, want 非 nil 空 map")
			}
			wantEQ(t, "空入参", "infos 条数", len(reply.GetInfos()), 0)
			wantEQ(t, "空入参不得产生任何调用", "调用数", len(e.ops(0)), 0)
		})
	}
}

// TestInfosByName3TruncatesNamesTo100 100 条上限：第 101 个名字必须根本不进 SQL 参数，
// 100 个时又必须一个不丢（两侧都断，才不是一句「大概截了一下」）。
func TestInfosByName3TruncatesNamesTo100(t *testing.T) {
	const credStepPrefix = "cred.FindMidsByIdentifiers:1/"
	for _, tc := range []struct {
		name      string
		total     int
		wantIn    []string
		wantOut   []string
		wantSteps int
	}{
		{"正好 100 条不截", 100, []string{"u001", "u100"}, nil, 100},
		{"101 条截掉最后一条", 101, []string{"u001", "u100"}, []string{"u101"}, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			names := seqNames(tc.total)
			e := newEnv(t) // 不布凭证：反查必然为空，于是链路只有一步，轨迹最干净
			st := e.st
			st.log.reset()

			reply, err := callInfosByName3(t, e, names...)
			wantNoErr(t, "InfosByName3", err)
			wantEQ(t, "反查为空", "infos 条数", len(reply.GetInfos()), 0)
			ops := e.ops(0)
			wantEQ(t, "反查为空时不该再有第二步", "调用数", len(ops), 1)
			if !strings.HasPrefix(ops[0], credStepPrefix) {
				t.Fatalf("第一步 = %s, want 以 %s 开头", ops[0], credStepPrefix)
			}
			got := strings.Split(strings.TrimPrefix(ops[0], credStepPrefix), ",")
			wantEQ(t, "进 SQL 的名字条数", "len", len(got), tc.wantSteps)
			wantEQ(t, "进 SQL 的名字序列", "前 100", strings.Join(got, ","), strings.Join(names[:tc.wantSteps], ","))
			for _, n := range tc.wantIn {
				if !strings.Contains(ops[0], n) {
					t.Errorf("名字 %s 竟然没进查询参数", n)
				}
			}
			for _, n := range tc.wantOut {
				if strings.Contains(ops[0], n) {
					t.Errorf("名字 %s 不该进查询参数（截断没生效）：%s", n, ops[0])
				}
			}
		})
	}
}

// TestInfosByName3DropsUnresolvableNames 一批名字里只有部分查得到：
// 查不到的既不报错也不占结果位（调用方按 mid 自行补默认值）。
// 两个 mid 的 mid 列表来自 Go map 迭代，顺序随机，故只断言「两条都在同一步里出现」。
func TestInfosByName3DropsUnresolvableNames(t *testing.T) {
	const (
		midAlice = int64(70001)
		midBob   = int64(70002)
	)
	e := newEnv(t,
		withDownstream(midAlice, Downstream{Base: &repository.UserProfileBase{Mid: midAlice, Name: "alice"}}),
		withDownstream(midBob, Downstream{Base: &repository.UserProfileBase{Mid: midBob, Name: "bob"}}),
	)
	st := e.st
	st.cred.put(&model.AccountCredential{Mid: midAlice, CredentialType: model.CredentialTypeUsername, Identifier: "alice", Status: 0})
	st.cred.put(&model.AccountCredential{Mid: midBob, CredentialType: model.CredentialTypeUsername, Identifier: "bob", Status: 0})
	st.account.put(&model.Account{Mid: midAlice, Status: 0})
	st.account.put(&model.Account{Mid: midBob, Status: 0})
	st.log.reset()

	reply, err := callInfosByName3(t, e, "alice", "bob", "ghost")
	wantNoErr(t, "部分名字查不到", err)
	wantEQ(t, "结果条数", "infos 条数", len(reply.GetInfos()), 2)
	wantProto(t, "alice", "infos[alice]", reply.GetInfos()[midAlice], &rpc.Info{Mid: midAlice, Name: "alice"})
	wantProto(t, "bob", "infos[bob]", reply.GetInfos()[midBob], &rpc.Info{Mid: midBob, Name: "bob"})

	ops := e.ops(0)
	// 1 次反查 + 2 次读缓存 + 1 次批量回源 + 2 次回填 = 6 步。
	wantEQ(t, "调用步数（反查 1 + 每 mid 读缓存/回源/回填）", "调用数", len(ops), 6)
	wantEQ(t, "第一步", "反查", ops[0], "cred.FindMidsByIdentifiers:1/alice,bob,ghost")
	// 「ghost」没有 mid，因此绝不会挤进 Bases 参数；两条 mid 的先后由 map 迭代决定，
	// 所以走 wantOpsArgSet：同一步内的参数按集合比（wantOpsSet 会把这一步再判红一次）。
	wantOpsArgSet(t, "两个 mid 的完整链路", ops[1:], []string{
		"cache.CacheInfo:" + infoKey(midAlice),
		"cache.CacheInfo:" + infoKey(midBob),
		"userProfile.Bases:" + itoa(midAlice) + "," + itoa(midBob),
		"cache.AddCacheInfo:" + infoKey(midAlice),
		"cache.AddCacheInfo:" + infoKey(midBob),
	})
}

// TestInfosByName3CredentialFaultPropagates DB 故障必须原样外传：响应为 nil、
// 一步缓存/下游都不许走。这是本批 8 个方法里唯一可离线触达的错误分支。
func TestInfosByName3CredentialFaultPropagates(t *testing.T) {
	e := newEnv(t)
	st := e.st
	boom := errors.New("account/db: credential lookup down")
	st.cred.failWith("FindMidsByIdentifiers", boom)
	st.log.reset()

	reply, err := callInfosByName3(t, e, "alice")
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v, want errors.Is(%v)（反查故障不许软化成空结果）", err, boom)
	}
	if reply != nil {
		t.Errorf("响应 = %+v, want nil（报错时不许带回半成品结果）", reply)
	}
	wantOps(t, "故障后的调用序列", e.ops(0), []string{"cred.FindMidsByIdentifiers:1/alice"})
	wantNoOpsWith(t, "故障路径", e.ops(0), "cache.")
	wantNoOpsWith(t, "故障路径", e.ops(0), "userProfile.")
}

// TestInfosByName3MatchesIdentifierCaseInsensitively 库里存 alice、用 ALICE 查：
// 真实 SQL 走 LOWER(identifier) IN (?)，替身按 EqualFold 同口径。
// 结果 map 的键是小写化的库里标识（account_credential.go:110），本用例只关心它能否解析出 mid。
func TestInfosByName3MatchesIdentifierCaseInsensitively(t *testing.T) {
	const mid = int64(70001)
	e := newEnv(t, withDownstream(mid, Downstream{
		Base: &repository.UserProfileBase{Mid: mid, Name: "alice"},
	}))
	st := e.st
	st.cred.put(&model.AccountCredential{Mid: mid, CredentialType: model.CredentialTypeUsername, Identifier: "alice", Status: 0})
	st.account.put(&model.Account{Mid: mid, Status: 0})
	st.log.reset()

	for _, q := range []string{"ALICE", "Alice", "alice"} {
		reply, err := callInfosByName3(t, e, q)
		wantNoErr(t, "大小写反查 "+q, err)
		wantProto(t, "大小写不敏感命中同一 mid", "infos[mid]", reply.GetInfos()[mid], &rpc.Info{Mid: mid, Name: "alice"})
	}
	// 三次调用：第一次回源+回填，后两次应当只读缓存（缓存键按 mid，与查询大小写无关）。
	wantOpsSet(t, "三次调用的总序列", e.ops(0), []string{
		"cred.FindMidsByIdentifiers:1/ALICE",
		"cache.CacheInfo:" + infoKey(mid),
		"userProfile.Bases:" + itoa(mid),
		"cache.AddCacheInfo:" + infoKey(mid),
		"cred.FindMidsByIdentifiers:1/Alice",
		"cache.CacheInfo:" + infoKey(mid),
		"cred.FindMidsByIdentifiers:1/alice",
		"cache.CacheInfo:" + infoKey(mid),
	})
}

// TestInfosByName3ResolvesUnboundCredential 已解绑（status=1）的用户名仍然能被反查出来。
//
// TODO(缺陷)（批次1 已登记为 README 已知缺口 2，本轮补上可执行的哨兵）：
// model/account_credential.go:102 的 SQL 只有 `WHERE credential_type = ? AND
// LOWER(identifier) IN (?)`，没有 DDL 注释要求的 status=0 过滤，于是注销/解绑过的
// 标识照样解析出 mid 并返回其资料。此处钉住**当前**行为；补上过滤后本用例会红，
// 届时把期望改成「解析不出 mid、结果为空」。
func TestInfosByName3ResolvesUnboundCredential(t *testing.T) {
	const mid = int64(70001)
	e := newEnv(t, withDownstream(mid, Downstream{
		Base: &repository.UserProfileBase{Mid: mid, Name: "alice"},
	}))
	st := e.st
	st.cred.put(&model.AccountCredential{Mid: mid, CredentialType: model.CredentialTypeUsername, Identifier: "alice", Status: 1})
	st.account.put(&model.Account{Mid: mid, Status: 0})
	st.log.reset()

	reply, err := callInfosByName3(t, e, "alice")
	wantNoErr(t, "已解绑标识的反查", err)
	if reply.GetInfos()[mid] == nil {
		t.Fatalf("当前实现本应照样解析出已解绑标识，结果却为空：%v —— 前提已变，请复核 SQL 后改写本哨兵", reply.GetInfos())
	}
	wantProto(t, "已解绑标识照样返回资料", "infos[mid]", reply.GetInfos()[mid], &rpc.Info{Mid: mid, Name: "alice"})
	wantOps(t, "已解绑标识照样走完链路", e.ops(0), []string{
		"cred.FindMidsByIdentifiers:1/alice",
		"cache.CacheInfo:" + infoKey(mid),
		"userProfile.Bases:" + itoa(mid),
		"cache.AddCacheInfo:" + infoKey(mid),
	})
}
