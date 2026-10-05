package model

import "errors"

// rights 域错误。
var (
	ErrContractNotFound      = errors.New("rights: contract not found")
	ErrWindowNotFound        = errors.New("rights: window not found")
	ErrWindowExpired         = errors.New("rights: window already expired")
	ErrWindowNotActive       = errors.New("rights: window not active")
	ErrInvalidContractID     = errors.New("rights: invalid contract_id")
	ErrInvalidWindowID       = errors.New("rights: invalid window_id")
	ErrInvalidContentID      = errors.New("rights: invalid content_id")
	ErrInvalidOwnerID        = errors.New("rights: invalid owner_id")
	ErrInvalidRegion         = errors.New("rights: invalid region")
	ErrInvalidTitle          = errors.New("rights: invalid title")
	ErrInvalidDateRange      = errors.New("rights: end_date must be greater than start_date")
	ErrInvalidTimeRange      = errors.New("rights: end_time must be greater than start_time")
	ErrPsTooLarge            = errors.New("rights: ps exceeds limit")
	ErrContractNotActive     = errors.New("rights: contract not active")
	ErrExpiringWithinInvalid = errors.New("rights: within_seconds must be positive")
)

// 合同状态常量。
const (
	ContractStateUnspecified = 0
	ContractStateActive      = 1
	ContractStateTerminated  = 2
)

// 窗口状态常量。
const (
	WindowStateUnspecified = 0
	WindowStateActive      = 1
	WindowStateExpired     = 2
	WindowStateRevoked     = 3
)

// 内容类型常量。
const (
	ContentTypeUnspecified = 0
	ContentTypePGC         = 1
	ContentTypeUGC         = 2
)
