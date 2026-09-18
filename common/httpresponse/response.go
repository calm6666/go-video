// Package httpresponse 定义所有 go-zero HTTP API 服务对外返回的统一响应信封。
// 该包不持有任何领域模型，仅负责响应结构和错误处理。
package httpresponse

import (
	"context"
	"errors"
	"net/http"

	"github.com/zeromicro/go-zero/rest/httpx"
)

// 响应业务码常量，与各服务自定义业务码区间不冲突。
const (
	CodeOK            = 0
	CodeBadRequest    = 40000
	CodeUnauthorized  = 40100
	CodeForbidden     = 40300
	CodeNotFound      = 40400
	CodeConflict      = 40900
	CodeInternalError = 50000
)

// Envelope 是统一的响应信封结构。错误响应和无类型成功响应直接使用本结构；
// 通过 .api 生成的成功响应使用同一组四字段，但 Data 字段为具体类型。
type Envelope struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
	TTL     int64  `json:"ttl"`
}

// ErrorHandler 将 go-zero 处理流程中的 error 转换为统一响应信封。
// HTTP 状态码仍保留语义供网关和代理识别；code 为稳定的业务码。
func ErrorHandler(ctx context.Context, err error) (int, any) {
	status := http.StatusBadRequest
	code := CodeBadRequest
	message := "request failed"
	if err == nil {
		return http.StatusOK, Envelope{Code: CodeOK, Message: "ok", Data: map[string]any{}, TTL: 0}
	}

	switch {
	case errors.Is(err, context.Canceled):
		status, code, message = 499, CodeBadRequest, "request canceled"
	case errors.Is(err, context.DeadlineExceeded):
		status, code, message = http.StatusGatewayTimeout, CodeInternalError, "request timeout"
	default:
		// 校验和领域错误可以安全暴露其 message；服务端错误不应直接暴露原始信息，
		// 后续可通过 typed error 注入领域业务码而无需调整信封结构。
		message = err.Error()
	}
	_ = ctx
	return status, Envelope{Code: code, Message: message, Data: map[string]any{}, TTL: 0}
}

// Install 注册进程级的 go-zero 错误响应处理器。
// 在每个服务手写的 handler 扩展处调用，禁止修改生成的 handler 文件。
func Install() {
	httpx.SetErrorHandlerCtx(ErrorHandler)
}
