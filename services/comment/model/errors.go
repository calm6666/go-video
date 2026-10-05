package model

import "errors"

// 评论域错误。
var (
	ErrCommentNotFoundOrForbidden = errors.New("comment: not found or not owner")
	ErrInvalidTarget              = errors.New("comment: invalid target oid/tp")
	ErrInvalidMid                 = errors.New("comment: invalid mid")
	ErrContentEmpty               = errors.New("comment: content is empty")
	ErrPsTooLarge                 = errors.New("comment: ps exceeds 49")
)
