// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

// Package middleware 存放 gateway/app 的 HTTP 中间件扩展。
// 中间件实现属于业务扩展，允许手写（骨架由 goctl 生成）。
package middleware

import (
	"net/http"
	"sync"

	"go-video/common/httpresponse"

	"github.com/zeromicro/go-zero/rest/httpx"
)

var (
	privacyAppKeyMu sync.RWMutex
	// privacyAppKeys 是 /account/privacy 接口允许的 appkey 白名单。
	// 空集合表示不启用校验（仅限开发环境），语义与参考仓库
	// filterByAppkey(conf.AppkeyFilter.Privacy) 一致。
	privacyAppKeys = map[string]struct{}{}
)

// SetPrivacyAppKeys 设置 /account/privacy 白名单 appkey。
// 由 ServiceContext 构造时注入；为空表示不启用 appkey 校验。
func SetPrivacyAppKeys(keys []string) {
	allowed := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		allowed[k] = struct{}{}
	}
	privacyAppKeyMu.Lock()
	privacyAppKeys = allowed
	privacyAppKeyMu.Unlock()
}

// AppkeyVerifyMiddleware 校验请求 query 中的 appkey 是否在白名单内。
// 拒绝时返回 HTTP 403 和统一响应信封（code=40300）。
type AppkeyVerifyMiddleware struct {
}

// NewAppkeyVerifyMiddleware 构造中间件。
func NewAppkeyVerifyMiddleware() *AppkeyVerifyMiddleware {
	return &AppkeyVerifyMiddleware{}
}

// Handle 是 go-zero rest.Middleware 的适配函数。
func (m *AppkeyVerifyMiddleware) Handle(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		privacyAppKeyMu.RLock()
		allowed := privacyAppKeys
		privacyAppKeyMu.RUnlock()

		// 未配置白名单时放行（仅限开发环境），避免本地联调被锁死。
		if len(allowed) == 0 {
			next(w, r)
			return
		}

		appkey := r.URL.Query().Get("appkey")
		if _, ok := allowed[appkey]; !ok {
			httpx.WriteJson(w, http.StatusForbidden, httpresponse.Envelope{
				Code:    httpresponse.CodeForbidden,
				Message: "appkey access denied",
				Data:    map[string]any{},
				TTL:     0,
			})
			return
		}
		next(w, r)
	}
}
