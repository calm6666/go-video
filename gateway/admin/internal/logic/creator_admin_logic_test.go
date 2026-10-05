package logic

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	creatorrpc "go-video/services/creator/rpc"

	"google.golang.org/grpc"
)

// 本文件只验证网关侧面向 creator 的口径：protobuf map 如何展平成稳定有序的数组、
// 分页与批量规模如何收敛到服务侧上限。分组语义、名册归属、签约状态判定全部留在 creator。
// 打桩方式为内嵌生成的 client 接口 + 覆盖所需方法，不建 gRPC 连接、不碰数据库（AGENTS.md §9）。

// fakeCreatorCli 只实现本文件用到的 3 个方法，其余继承接口（未调用即 panic）。
type fakeCreatorCli struct {
	creatorrpc.CreatorClient

	upGroupsReq    *creatorrpc.NoArgReq
	upGroupsRply   *creatorrpc.UpGroupsReply
	upGroupMidsReq *creatorrpc.UpGroupMidsReq
	upGroupMidsRly *creatorrpc.UpGroupMidsReply
	highAllyReq    *creatorrpc.HighAllyUpsReq
	highAllyRply   *creatorrpc.HighAllyUpsReply
	err            error
	calls          int
}

func (f *fakeCreatorCli) UpGroups(_ context.Context, in *creatorrpc.NoArgReq,
	_ ...grpc.CallOption) (*creatorrpc.UpGroupsReply, error) {
	f.calls++
	f.upGroupsReq = in
	return f.upGroupsRply, f.err
}

func (f *fakeCreatorCli) UpGroupMids(_ context.Context, in *creatorrpc.UpGroupMidsReq,
	_ ...grpc.CallOption) (*creatorrpc.UpGroupMidsReply, error) {
	f.calls++
	f.upGroupMidsReq = in
	return f.upGroupMidsRly, f.err
}

func (f *fakeCreatorCli) GetHighAllyUps(_ context.Context, in *creatorrpc.HighAllyUpsReq,
	_ ...grpc.CallOption) (*creatorrpc.HighAllyUpsReply, error) {
	f.calls++
	f.highAllyReq = in
	return f.highAllyRply, f.err
}

var creatorErr = errors.New("creator: rpc unavailable")

func TestCreatorUpGroupsFlattensMapByIDAscending(t *testing.T) {
	// map 遍历顺序随机，键故意乱序写入，断言响应必须按分组 ID 升序（前端可直接 diff）。
	fake := &fakeCreatorCli{upGroupsRply: &creatorrpc.UpGroupsReply{
		UpGroups: map[int64]*creatorrpc.UpGroup{
			30:  {Id: 30, Name: "高能联盟", Tag: "ally", ShortTag: "AL", FontColor: "#FFF", BgColor: "#000", Note: "n30"},
			7:   {Id: 7, Name: "认证UP", Tag: "cert", ShortTag: "CT"},
			101: {Id: 101, Name: "机构号", Tag: "org"},
			2:   {Id: 2, Name: "新手", Tag: "new"},
		},
	}}
	l := NewAdminUpGroupsLogic(context.Background(), &svc.ServiceContext{Creator: fake})

	resp, err := l.AdminUpGroups()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.calls != 1 || fake.upGroupsReq == nil {
		t.Fatalf("UpGroups 应以 NoArgReq 调用一次, calls=%d req=%+v", fake.calls, fake.upGroupsReq)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封 = code=%d message=%q ttl=%d", resp.Code, resp.Message, resp.TTL)
	}
	wantIDs := []int64{2, 7, 30, 101}
	got := resp.Data.Groups
	if len(got) != len(wantIDs) {
		t.Fatalf("条目数 = %d, want %d", len(got), len(wantIDs))
	}
	for i, id := range wantIDs {
		if got[i].Id != id {
			t.Fatalf("第 %d 项 ID = %d, want %d（map 必须按键升序展平）", i, got[i].Id, id)
		}
	}
	if got[2].Name != "高能联盟" || got[2].Tag != "ally" || got[2].ShortTag != "AL" ||
		got[2].FontColor != "#FFF" || got[2].BgColor != "#000" || got[2].Note != "n30" {
		t.Fatalf("分组字段投影不完整: %+v", got[2])
	}
}

func TestCreatorUpGroupsToAPIFlattensKeysAndTolerantOfNilValue(t *testing.T) {
	if got := upGroupsToAPI(nil); got == nil || len(got) != 0 {
		t.Fatalf("nil map 应投影成空切片, got %#v", got)
	}
	if got := upGroupsToAPI(map[int64]*creatorrpc.UpGroup{}); got == nil {
		t.Fatal("空 map 也应返回非 nil 切片")
	}
	// 键存在但值为 nil：投影为零值条目（ID 由键补齐会伪造数据，因此保持 0）。
	got := upGroupsToAPI(map[int64]*creatorrpc.UpGroup{5: nil})
	if len(got) != 1 || got[0] != (types.AdminUpGroup{}) {
		t.Fatalf("nil 值应投影为零值条目, got %#v", got)
	}
}

func TestCreatorUpGroupMidsNormalizesPageAndCopiesMids(t *testing.T) {
	mids := []int64{9, 3, 7}
	fake := &fakeCreatorCli{upGroupMidsRly: &creatorrpc.UpGroupMidsReply{Mids: mids, Total: 1234}}
	l := NewAdminUpGroupMidsLogic(context.Background(), &svc.ServiceContext{Creator: fake})

	resp, err := l.AdminUpGroupMids(&types.ParamAdminUpGroupMids{GroupId: 30, Pn: 0, Ps: 0})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	in := fake.upGroupMidsReq
	if in.GetGroupId() != 30 || in.GetPn() != 1 {
		t.Fatalf("名册入参 = %+v, want group_id=30 pn=1", in)
	}
	// ps 缺省走路由口径 50（服务侧会回落 1000，后台不默认拉满）。
	if in.GetPs() != creatorUpGroupMidsDefaultPageSize {
		t.Fatalf("ps = %d, want %d", in.GetPs(), creatorUpGroupMidsDefaultPageSize)
	}
	// 服务侧顺序原样透出，网关不重排（重排会与下游分页偏移量不一致）。
	if !reflect.DeepEqual(resp.Data.Mids, []int64{9, 3, 7}) {
		t.Fatalf("mids = %v, want 原顺序 9,3,7", resp.Data.Mids)
	}
	if resp.Data.Total != 1234 {
		t.Fatalf("total = %d, want 1234", resp.Data.Total)
	}
	// 响应体必须与下游消息底层数组解耦，改响应不能污染 RPC 消息。
	resp.Data.Mids[0] = -1
	if mids[0] != 9 {
		t.Fatalf("mids 与 RPC 消息共享底层数组: %v", mids)
	}

	// ps 超上限截断到服务侧的 1000。
	if _, err := l.AdminUpGroupMids(&types.ParamAdminUpGroupMids{GroupId: 30, Pn: 2, Ps: 5000}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.upGroupMidsReq.GetPs() != creatorMaxUpGroupMidsPageSize || fake.upGroupMidsReq.GetPn() != 2 {
		t.Fatalf("超上限分页 = %+v", fake.upGroupMidsReq)
	}
}

func TestCreatorUpGroupMidsNilReplyReturnsEmptySlice(t *testing.T) {
	fake := &fakeCreatorCli{upGroupMidsRly: nil}
	l := NewAdminUpGroupMidsLogic(context.Background(), &svc.ServiceContext{Creator: fake})

	resp, err := l.AdminUpGroupMids(&types.ParamAdminUpGroupMids{GroupId: 1, Ps: 50})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Data.Mids == nil || len(resp.Data.Mids) != 0 {
		t.Fatalf("空回包应给出 []，后台不能拿到 null, got %#v", resp.Data.Mids)
	}
}

func TestCreatorHighAllyUpsFlattensSignUpMapByMidAscending(t *testing.T) {
	fake := &fakeCreatorCli{highAllyRply: &creatorrpc.HighAllyUpsReply{
		Lists: map[int64]*creatorrpc.SignUp{
			88:  {Mid: 88, State: 2, BeginDate: 100, EndDate: 200},
			5:   {Mid: 5, State: 1, BeginDate: 10, EndDate: 20},
			100: nil,
		},
	}}
	l := NewAdminHighAllyUpsLogic(context.Background(), &svc.ServiceContext{Creator: fake})

	resp, err := l.AdminHighAllyUps(&types.ParamAdminHighAllyUps{Mids: []int64{88, 5, 100, 7}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(fake.highAllyReq.GetMids(), []int64{88, 5, 100, 7}) {
		t.Fatalf("mids 未透传: %v", fake.highAllyReq.GetMids())
	}
	want := []types.AdminSignUpInfo{
		{Mid: 5, State: 1, BeginDate: 10, EndDate: 20},
		{Mid: 88, State: 2, BeginDate: 100, EndDate: 200},
	}
	if len(resp.Data.Ups) != 3 {
		t.Fatalf("条目数 = %d, want 3: %+v", len(resp.Data.Ups), resp.Data.Ups)
	}
	if !reflect.DeepEqual(resp.Data.Ups[:2], want) {
		t.Fatalf("签约信息 = %+v, want 按 mid 升序 %+v", resp.Data.Ups[:2], want)
	}
	// 键 100 存在但值为 nil：投影成零值条目排在最后（网关不替下游伪造 mid）。
	if resp.Data.Ups[2] != (types.AdminSignUpInfo{}) {
		t.Fatalf("nil 值应投影为零值条目: %+v", resp.Data.Ups[2])
	}
}

func TestCreatorSignUpsToAPINilMap(t *testing.T) {
	if got := signUpsToAPI(nil); got == nil || len(got) != 0 {
		t.Fatalf("nil map 应投影成空切片, got %#v", got)
	}
}

func TestCreatorHighAllyUpsMidsSizeGuard(t *testing.T) {
	fake := &fakeCreatorCli{}
	l := NewAdminHighAllyUpsLogic(context.Background(), &svc.ServiceContext{Creator: fake})

	// 空 mids 合法：服务侧直接返回空 map，不打 DB。
	if _, err := l.AdminHighAllyUps(&types.ParamAdminHighAllyUps{}); err != nil {
		t.Fatalf("空 mids 应放行, got %v", err)
	}
	if fake.calls != 1 || len(fake.highAllyReq.GetMids()) != 0 {
		t.Fatalf("空 mids 入参 = %+v calls=%d", fake.highAllyReq, fake.calls)
	}

	// 超上限必须在网关挡下，不放大成下游的超长 IN 查询。
	tooMany := make([]int64, creatorHighAllyMaxMids+1)
	if _, err := l.AdminHighAllyUps(&types.ParamAdminHighAllyUps{Mids: tooMany}); err == nil ||
		!strings.Contains(err.Error(), "mids") {
		t.Fatalf("超量 mids 应点名 mids 并拒绝, got %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("规模校验失败不得调用下游，实际调用 %d 次", fake.calls)
	}
	// 恰好等于上限可用。
	if _, err := l.AdminHighAllyUps(&types.ParamAdminHighAllyUps{Mids: tooMany[:creatorHighAllyMaxMids]}); err != nil {
		t.Fatalf("上限内的批次应放行, got %v", err)
	}
}

// 未配置下游与下游错误都必须显式失败，不伪造空列表骗过后台。
func TestCreatorLogicsWithoutClientAndPropagateError(t *testing.T) {
	empty := &svc.ServiceContext{}
	cases := []struct {
		name string
		call func() error
	}{
		{"groups-no-client", func() error {
			_, err := NewAdminUpGroupsLogic(context.Background(), empty).AdminUpGroups()
			return err
		}},
		{"mids-no-client", func() error {
			_, err := NewAdminUpGroupMidsLogic(context.Background(), empty).AdminUpGroupMids(&types.ParamAdminUpGroupMids{GroupId: 1})
			return err
		}},
		{"high-ally-no-client", func() error {
			_, err := NewAdminHighAllyUpsLogic(context.Background(), empty).AdminHighAllyUps(&types.ParamAdminHighAllyUps{Mids: []int64{1}})
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.call(); err == nil || !strings.Contains(err.Error(), "creator service not configured") {
				t.Fatalf("%s 未配置 creator 时应失败, got %v", c.name, err)
			}
		})
	}

	fake := &fakeCreatorCli{err: creatorErr}
	ctx := context.Background()
	calls := map[string]func() error{
		"groups": func() error {
			_, err := NewAdminUpGroupsLogic(ctx, &svc.ServiceContext{Creator: fake}).AdminUpGroups()
			return err
		},
		"mids": func() error {
			_, err := NewAdminUpGroupMidsLogic(ctx, &svc.ServiceContext{Creator: fake}).
				AdminUpGroupMids(&types.ParamAdminUpGroupMids{GroupId: 1})
			return err
		},
		"high-ally": func() error {
			_, err := NewAdminHighAllyUpsLogic(ctx, &svc.ServiceContext{Creator: fake}).
				AdminHighAllyUps(&types.ParamAdminHighAllyUps{Mids: []int64{1}})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name+"-downstream-error", func(t *testing.T) {
			if err := call(); !errors.Is(err, creatorErr) {
				t.Fatalf("%s 错误未原样传播: %v", name, err)
			}
		})
	}
}
