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

// 回放任务分页（房间/场次/状态过滤）
func liveMediaReplayListHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.ParamLiveMediaReplayList
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := logic.NewLiveMediaReplayListLogic(r.Context(), svcCtx)
		resp, err := l.LiveMediaReplayList(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
