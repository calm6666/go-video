// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package handler

import (
	"net/http"

	"github.com/zeromicro/go-zero/rest/httpx"
	"go-video/gateway/app/internal/logic"
	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
)

// 发送弹幕（幂等，落库后待审核）
func postDanmakuHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.ParamDanmakuPost
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := logic.NewPostDanmakuLogic(r.Context(), svcCtx)
		resp, err := l.PostDanmaku(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
