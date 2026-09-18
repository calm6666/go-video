// Package idempotency 构造稳定的幂等键并跟踪写请求的生命周期，
// 使重试返回相同结果而非重复产生副作用。
//
// 该包有意不绑定存储：服务通过自己的 repository 层
// 将 Key/State 持久化到自有的幂等表或 Redis hash 中。
// 本包仅提供纯构造与校验工具。
package idempotency

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"go-video/common/idgen"
)

// State 表示幂等写请求的生命周期阶段。
type State string

const (
	// StatePending 表示请求正在执行中。
	// 并发调用方应等待或返回 409 Conflict，而不是启动第二次执行。
	StatePending State = "pending"
	// StateSucceeded 表示请求已完成；后续调用直接回放已存储的响应。
	StateSucceeded State = "succeeded"
	// StateFailed 表示执行出错；记录可被清除或更新，
	// 允许客户端重试。
	StateFailed State = "failed"
)

// ParseState 将存储中的字符串转换为 State。
// 未知值返回错误，以暴露 schema 漂移而非静默视为 failed。
func ParseState(s string) (State, error) {
	switch State(strings.ToLower(strings.TrimSpace(s))) {
	case StatePending:
		return StatePending, nil
	case StateSucceeded:
		return StateSucceeded, nil
	case StateFailed:
		return StateFailed, nil
	default:
		return "", fmt.Errorf("idempotency: unknown state %q", s)
	}
}

// Key 是基于业务字段哈希得到的稳定幂等键。
// 同时暴露 hash（用于持久化和查询）和原始 join（仅用于调试；
// 永远不要持久化原始值）。
type Key struct {
	raw  string
	hash string
}

// NewKey 基于有业务含义的 parts 构造 Key。
// 空 part 会被跳过，避免因尾部分隔符导致的意外碰撞。
// hash 为以 ':' 分隔的拼接 parts 的 SHA-256 小写 hex。
func NewKey(parts ...string) Key {
	nonEmpty := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		nonEmpty = append(nonEmpty, p)
	}
	raw := strings.Join(nonEmpty, ":")
	sum := sha256.Sum256([]byte(raw))
	return Key{raw: raw, hash: hex.EncodeToString(sum[:])}
}

// String 返回稳定的 hash。用此值作为持久化主键。
func (k Key) String() string { return k.hash }

// Raw 返回原始拼接 parts。仅用于日志；不要持久化或对外暴露。
func (k Key) Raw() string { return k.raw }

// GenerateKey 在客户端未提供幂等键时返回默认键。
// 格式为 "idem_<ULID>"。
func GenerateKey() string {
	id, err := idgen.Prefixed("idem")
	if err != nil {
		// idgen 仅在熵源耗尽时出错，属于不可恢复错误。
		// 回退到原始 ULID 以保证写入路径继续推进；
		// 幂等层在没有前缀的情况下仍可正常工作。
		id, _ = idgen.ULID()
		return id
	}
	return id
}

// Result 封装先前执行的结果，
// 调用方可在持有相同 Key 的后续请求中回放该结果。
type Result struct {
	hit        bool
	state      State
	response   []byte
	errCode    int
	errMessage string
}

// NewHit 构造一个表示存在已存储先前结果的 Result。
// 传入原始 state、响应体及可选的业务错误码/消息。
func NewHit(state State, response []byte, errCode int, errMessage string) Result {
	return Result{hit: true, state: state, response: response, errCode: errCode, errMessage: errMessage}
}

// NewMiss 构造表示无先前记录的 Result。
func NewMiss() Result { return Result{hit: false} }

// Hit 判断是否存在该 Key 对应的先前执行。
func (r Result) Hit() bool { return r.hit }

// State 返回先前执行的状态。Hit() 为 false 时返回 ""。
func (r Result) State() State { return r.state }

// Response 返回已存储的响应体（若原调用在产出响应前失败，可能为 nil）。
func (r Result) Response() []byte { return r.response }

// ErrCode 返回原始业务错误码（成功时为 0）。
func (r Result) ErrCode() int { return r.errCode }

// ErrMessage 返回原始错误消息（若有）。
func (r Result) ErrMessage() string { return r.errMessage }

// Err 在先前调用失败时将 Result 转换为 Go error，
// 调用方可通过标准 error 路径回放该结果。
// Hit() 为 false 或 State 为 succeeded 时返回 nil。
func (r Result) Err() error {
	if !r.hit || r.state == StateSucceeded {
		return nil
	}
	if r.errMessage != "" {
		return errors.New(r.errMessage)
	}
	return fmt.Errorf("idempotency: prior call ended in state %q", r.state)
}
