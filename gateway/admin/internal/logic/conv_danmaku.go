// 本文件是 gateway/admin 的手写投影扩展（非 goctl 生成产物）：danmaku RPC → 运营后台投影。
//
// 枚举在本文件里统一降维成 int32 透传给后台（scope/state 取值见 admin.api 注释），
// 网关不解释语义、不做二次判定：屏蔽词是否命中、能否停用只由 danmaku 服务回答。

package logic

import (
	"errors"
	"fmt"

	"go-video/gateway/admin/internal/types"
	danmakurpc "go-video/services/danmaku/rpc"
)

const (
	// danmakuMaxBlockWordPageSize 与 danmaku model 的 ListBlockWords 分页上限一致，
	// 不在网关放大后台可请求的页大小。
	danmakuMaxBlockWordPageSize = 100
	// danmakuDefaultPageSize 同样是 danmaku 服务口径：页大小越界时回落值。
	danmakuDefaultPageSize = 20
)

// danmakuBlockWordAction 校验屏蔽词管理动作，只接受 ADD/DISABLE/DELETE。
// BLOCK_WORD_UNSPECIFIED 是漏传，服务端同样会拒绝，这里给出更明确的后台字段提示。
func danmakuBlockWordAction(v int32) (danmakurpc.BlockWordAction, error) {
	if _, ok := danmakurpc.BlockWordAction_name[v]; !ok {
		return 0, fmt.Errorf("gateway/admin: invalid action %d", v)
	}
	if v == int32(danmakurpc.BlockWordAction_BLOCK_WORD_UNSPECIFIED) {
		return 0, errors.New("gateway/admin: action must be 1/2/3")
	}
	return danmakurpc.BlockWordAction(v), nil
}

// danmakuBlockWordScope 校验屏蔽词作用域。
// allowUnspecified=true 只用于列表过滤（不按作用域过滤）；写操作必须显式 GLOBAL/OID。
func danmakuBlockWordScope(v int32, allowUnspecified bool) (danmakurpc.BlockWordScope, error) {
	if _, ok := danmakurpc.BlockWordScope_name[v]; !ok {
		return 0, fmt.Errorf("gateway/admin: invalid scope %d", v)
	}
	if v == int32(danmakurpc.BlockWordScope_SCOPE_UNSPECIFIED) && !allowUnspecified {
		return 0, errors.New("gateway/admin: scope must be 1/2")
	}
	return danmakurpc.BlockWordScope(v), nil
}

// normalizeDanmakuPage 复刻 danmaku 服务 pn/ps 的归一规则（越界回落 20 而不是截断），
// 这样后台看到的分页结果与直接调用 RPC 完全一致。
func normalizeDanmakuPage(pn, ps int32) (int32, int32) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > danmakuMaxBlockWordPageSize {
		ps = danmakuDefaultPageSize
	}
	return pn, ps
}

// blockWordToAPI 投影单条屏蔽词。nil 时返回零值条目：pb getter 对 nil 安全，
// 网关不伪造词 ID，也不把「没数据」当成错误。
func blockWordToAPI(w *danmakurpc.BlockWordInfo) types.DanmakuBlockWordItem {
	return types.DanmakuBlockWordItem{
		WordId:   w.GetWordId(),
		Word:     w.GetWord(),
		Scope:    w.GetScope(),
		Oid:      w.GetOid(),
		State:    w.GetState(),
		Operator: w.GetOperator(),
		Ctime:    w.GetCtime(),
		Mtime:    w.GetMtime(),
	}
}

// blockWordsToAPI 投影屏蔽词列表，nil 输入返回空切片而不是 null，便于前端直接遍历。
func blockWordsToAPI(list []*danmakurpc.BlockWordInfo) []types.DanmakuBlockWordItem {
	out := make([]types.DanmakuBlockWordItem, 0, len(list))
	for _, w := range list {
		out = append(out, blockWordToAPI(w))
	}
	return out
}
