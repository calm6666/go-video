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

// 单个游标（未推进过返回 found=false 而不是错误）
func cronGetCheckpointHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.ParamCronGetCheckpoint
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := logic.NewCronGetCheckpointLogic(r.Context(), svcCtx)
		resp, err := l.CronGetCheckpoint(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
