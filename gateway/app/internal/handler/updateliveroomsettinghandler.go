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

// 主播面：更新直播配置（整段覆盖语义）
func updateLiveRoomSettingHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.ParamLiveRoomSettingUpdate
		if err := httpx.Parse(r, &req); err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}

		l := logic.NewUpdateLiveRoomSettingLogic(r.Context(), svcCtx)
		resp, err := l.UpdateLiveRoomSetting(&req)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
