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

// 暂停任务（可恢复；重复暂停 changed=false）
func cronPauseTaskHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.ParamCronPauseTask
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := logic.NewCronPauseTaskLogic(r.Context(), svcCtx)
		resp, err := l.CronPauseTask(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
