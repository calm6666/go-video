// 本文件是 gateway/admin 的手写投影扩展（非 goctl 生成产物）：creator RPC → 运营后台投影。
//
// 本域唯一的口径问题是 map：creator.v1.UpGroupsReply.up_groups（分组 ID → 分组）与
// HighAllyUpsReply.lists（mid → 签约信息）都是 protobuf map，Go 的 map 遍历顺序随机，
// 因此这里统一展平成「按 map 键升序」的数组再回给后台（AGENTS.md §6：同一份数据必须
// 给出稳定响应，后台前端才能直接 diff 列表）。
//
// 网关不解释分组与签约含义：分组是否存在、签约状态取值、名册分页规模全部由 creator 服务判定；
// 这里只按服务侧口径归一 pn/ps，并且限制一次批量查询的 mid 数量，避免把后台的
// 一个输入框放大成下游的超长 IN 查询。

package logic

import (
	"errors"
	"sort"

	"go-video/gateway/admin/internal/types"
	creatorrpc "go-video/services/creator/rpc"
)

const (
	// creatorMaxUpGroupMidsPageSize 与 services/creator UpGroupMids 的 ps 上限一致（1000）。
	creatorMaxUpGroupMidsPageSize = 1000
	// creatorUpGroupMidsDefaultPageSize 取 admin.api 的 ps,default=50：
	// 服务侧在 ps<=0 时回落 1000，后台名册页不该默认拉满一页，因此网关按更小的路由缺省值收敛
	// （只会比服务侧更保守，不会放大规模）。
	creatorUpGroupMidsDefaultPageSize = 50
	// creatorHighAllyMaxMids 与 creator.proto 批量请求的口径一致（MidsReq「最多 100 个」，
	// 同域 UpsSpecialReq 也是 100，gateway/app 已按该值实现）。
	creatorHighAllyMaxMids = 100
)

// creatorHighAllyMidsGuard 限制一次查询的 mid 数量，超量批次在进入下游前拒绝。
func creatorHighAllyMidsGuard(mids []int64) error {
	if len(mids) > creatorHighAllyMaxMids {
		return errors.New("gateway/admin: mids exceeds 100")
	}
	return nil
}

// normalizeCreatorUpGroupMidsPage 与服务侧同口径地把 ps 截断到 1000（creator 的 UpGroupMids
// 同样把 >1000 夹到 1000），pn<1 归一为 1；唯一的差别是 ps<=0 时这里回落到路由声明的 50，
// 而不是服务侧的 1000，属于只收紧不放大。
func normalizeCreatorUpGroupMidsPage(pn, ps int32) (int32, int32) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 {
		ps = creatorUpGroupMidsDefaultPageSize
	}
	if ps > creatorMaxUpGroupMidsPageSize {
		ps = creatorMaxUpGroupMidsPageSize
	}
	return pn, ps
}

// upGroupToAPI 投影单个特殊分组（分组字典没有状态字段，运营改名字/配色由 creator 落库）。
func upGroupToAPI(g *creatorrpc.UpGroup) types.AdminUpGroup {
	return types.AdminUpGroup{
		Id:        g.GetId(),
		Name:      g.GetName(),
		Tag:       g.GetTag(),
		ShortTag:  g.GetShortTag(),
		FontColor: g.GetFontColor(),
		BgColor:   g.GetBgColor(),
		Note:      g.GetNote(),
	}
}

// upGroupsToAPI 把 map<int64,*UpGroup> 展平成按分组 ID 升序的数组，
// 保证同一份字典两次响应字段顺序一致；nil/空 map 返回空切片而不是 null。
func upGroupsToAPI(groups map[int64]*creatorrpc.UpGroup) []types.AdminUpGroup {
	ids := make([]int64, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]types.AdminUpGroup, 0, len(ids))
	for _, id := range ids {
		out = append(out, upGroupToAPI(groups[id]))
	}
	return out
}

// creatorInt64Ids 复制 protobuf 重复字段，避免响应体与消息内部切片共享底层数组。
// nil 输入返回空切片，后台拿到的是 [] 而不是 null；顺序按服务侧给定，网关不重排。
func creatorInt64Ids(ids []int64) []int64 {
	out := make([]int64, 0, len(ids))
	return append(out, ids...)
}

// signUpToAPI 投影单条高能联盟签约信息（state 是契约里的 int32，网关不解释签约状态含义）。
func signUpToAPI(s *creatorrpc.SignUp) types.AdminSignUpInfo {
	return types.AdminSignUpInfo{
		Mid:       s.GetMid(),
		State:     s.GetState(),
		BeginDate: s.GetBeginDate(),
		EndDate:   s.GetEndDate(),
	}
}

// signUpsToAPI 把 map<int64,*SignUp> 展平成按 mid 升序的数组。
// creator 服务只返回命中的 mid，未签约的 mid 会被省略，网关不补零值条目。
func signUpsToAPI(lists map[int64]*creatorrpc.SignUp) []types.AdminSignUpInfo {
	mids := make([]int64, 0, len(lists))
	for mid := range lists {
		mids = append(mids, mid)
	}
	sort.Slice(mids, func(i, j int) bool { return mids[i] < mids[j] })
	out := make([]types.AdminSignUpInfo, 0, len(mids))
	for _, mid := range mids {
		out = append(out, signUpToAPI(lists[mid]))
	}
	return out
}
