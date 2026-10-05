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

// 屏蔽/解除屏蔽某用户或某关键词的弹幕
func danmakuUserBlockHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.ParamDanmakuUserBlock
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := logic.NewDanmakuUserBlockLogic(r.Context(), svcCtx)
		resp, err := l.DanmakuUserBlock(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
