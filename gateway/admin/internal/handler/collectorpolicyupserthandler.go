// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package handler

import (
	"net/http"

	"github.com/zeromicro/go-zero/rest/httpx"
	"go-video/gateway/admin/internal/logic"
	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
)

// 新建/修改策略草稿（state 不可声明，服务按 DRAFT 落；ACTIVE 不可原地改）
func collectorPolicyUpsertHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.ParamCollectorPolicyUpsert
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := logic.NewCollectorPolicyUpsertLogic(r.Context(), svcCtx)
		resp, err := l.CollectorPolicyUpsert(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
