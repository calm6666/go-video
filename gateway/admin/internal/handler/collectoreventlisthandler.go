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

// 事件台账游标翻页（校验结论与投递状态是两个独立维度）
func collectorEventListHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.ParamCollectorEventList
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := logic.NewCollectorEventListLogic(r.Context(), svcCtx)
		resp, err := l.CollectorEventList(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
