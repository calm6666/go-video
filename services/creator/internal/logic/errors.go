package logic

import "errors"

// 参数校验错误（参考 obc up 服务 request.proto 的 validate 规则）。
// 不使用 grpc/status 包装为业务码，直接返回错误由 gateway 统一处理。
var (
	errTooManyMids        = errors.New("creator: mids count exceeds 100")
	errInvalidGroupID     = errors.New("creator: invalid group_id")
	errInvalidMid         = errors.New("creator: invalid mid")
	errInvalidFrom        = errors.New("creator: invalid from, must be 0..3")
	errInvalidSwitchFrom  = errors.New("creator: invalid switch from, must be 0 or 1")
	errInvalidSwitchState = errors.New("creator: invalid switch state, must be 0 or 1")
)
