package logic

import (
	"context"
	"encoding/json"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"

	"github.com/zeromicro/go-zero/core/logx"
)

// 写侧幂等键的缓存用法。
//
// 与 helpers.claimIdempotency 的分工：那个函数在缓存不可用时 fail closed，
// 用于「nonce 是唯一防重放屏障」的路径；本文件服务的是写接口的「同键重放返回首次结论」，
// 其硬幂等锚点始终是数据库唯一键与状态机条件（uniq_app_scope / uniq_event_endpoint /
// uniq_request_id / revoked_at=0），缓存只是省掉一次重算。
//
// 因此这里在 Cache 缺失或读写故障时降级为「按当前状态重算」而不是拒绝服务：
// 重算的结果与首次执行收敛（授予/回收/入队都是状态迁移，不产生新行），
// 但 nonce 类屏障降级等于放弃防重放，所以那类路径必须继续用 claimIdempotency/claimNonce。

// claimWriteOnce 占用写侧幂等键。
// 返回 (first, cachedResult, err)：first=false 且 cachedResult 非空时调用方应直接回缓存结论；
// first=false 且 cachedResult 为空表示「首次结论尚未落盘」，按当前状态重算同样收敛。
func claimWriteOnce(ctx context.Context, s *svc.ServiceContext, scope, key string) (bool, string, error) {
	if key == "" {
		return false, "", model.ErrIdempotencyKeyRequired
	}
	if _, err := requireLen(key, maxIdempotencyRunes,
		model.ErrIdempotencyKeyRequired, model.ErrIdempotencyKeyRequired); err != nil {
		return false, "", err
	}
	if s.Cache == nil {
		logx.WithContext(ctx).Errorf("open-platform: 幂等缓存不可用，scope=%s 降级为按当前状态重算", scope)
		return true, "", nil
	}
	first, cached, err := claimIdempotency(ctx, s, scope, key)
	if err != nil {
		logx.WithContext(ctx).Errorf("open-platform: 幂等键读写失败 scope=%s: %v，降级为重算", scope, err)
		return true, "", nil
	}
	return first, cached, nil
}

// storeWriteOnce 把首次结论写入幂等缓存（失败只降级，调用方不因此回错）。
func storeWriteOnce(ctx context.Context, s *svc.ServiceContext, scope, key string, reply any) {
	if key == "" || s.Cache == nil {
		return
	}
	raw, err := json.Marshal(reply)
	if err != nil {
		logx.WithContext(ctx).Errorf("open-platform: 幂等结论序列化失败 scope=%s: %v", scope, err)
		return
	}
	saveIdempotency(ctx, s, scope, key, string(raw))
}

// loadWriteOnce 反序列化缓存的幂等结论；失败按「未命中」处理（不伪造成功）。
func loadWriteOnce(cached string, reply any) bool {
	if cached == "" {
		return false
	}
	if err := json.Unmarshal([]byte(cached), reply); err != nil {
		return false
	}
	return true
}

// replayedReply 命中幂等缓存时回一句明确的日志，便于排查「为什么没生效」。
func logReplay(ctx context.Context, method, key string) {
	logx.WithContext(ctx).Infof("open-platform: %s 命中幂等重放 key=%s", method, key)
}
