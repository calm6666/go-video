package logic

import (
	"fmt"
	"strings"

	"go-video/services/risk-control/internal/policy"
	"go-video/services/risk-control/model"
	"go-video/services/risk-control/rpc"
)

// 本文件落实「入参即不可信」的边界：
// 一切外部字符串在进入 model/policy 之前先规范化，
// 明文 IP、超长上下文、越界枚举都在这里被挡下，而不是留到 SQL 层报错。

// maxRequestContextEntries / maxRequestContextKeyLen / maxRequestContextValLen
// 限制 request_context 体积。本期该字段不参与规则评估（没有对应指标），
// 服务端也不落库其原值（见 model.RiskCheckLog 注释），
// 因此这里只做「拒绝异常大的入参」这一件事：
// 不加限制的话，调用方可以用它把任意大对象灌进 gRPC 帧与审计日志链路。
const (
	maxRequestContextEntries = 16
	maxRequestContextKeyLen  = 32
	maxRequestContextValLen  = 64
)

// checkRequestContext 校验 request_context 规模，超限直接拒绝而不是静默截断，
// 因为静默截断会让调用方误以为上下文参与了裁决。
func checkRequestContext(ctxMap map[string]string) error {
	if len(ctxMap) == 0 {
		return nil
	}
	if len(ctxMap) > maxRequestContextEntries {
		return fmt.Errorf("%w: request_context entries %d > %d", model.ErrInvalidTarget, len(ctxMap), maxRequestContextEntries)
	}
	for k, v := range ctxMap {
		if len(k) > maxRequestContextKeyLen || len(v) > maxRequestContextValLen {
			return fmt.Errorf("%w: request_context key/value too long", model.ErrInvalidTarget)
		}
	}
	return nil
}

// actionFromProto 把 proto 动作枚举转成 model 常量，越界即非法。
func actionFromProto(a rpc.GuardedAction) (int32, error) {
	action := int32(a)
	if !model.ValidAction(action) {
		return 0, model.ErrInvalidTarget
	}
	return action, nil
}

// ruleActionFromProto 规则适用动作（允许 0 表示全部动作）。
func ruleActionFromProto(a rpc.GuardedAction) (int32, error) {
	action := int32(a)
	if !model.ValidRuleAction(action) {
		return 0, model.ErrInvalidTarget
	}
	return action, nil
}

// deviceHashOf 计算设备受控 ID：优先用 device_id 现算，其次接受已受控的 device_hash。
// 二者都为空返回空串（画像与设备维度指标不可观测，但不算入参错误）。
func deviceHashOf(deviceID, deviceHash string) string {
	if h := model.DeviceHash(deviceID); h != "" {
		return h
	}
	return model.NormalizeIPHash(deviceHash) // device_hash 与 ip_hash 同规则：小写十六进制摘要
}

// normalizeIPHashOrErr 规范化调用方预哈希 IP。
// 传入裸 IP 直接拒绝（ErrRawIPForbidden），而不是偷偷在服务端哈希：
// 服务端一旦哈希，明文 IP 就已经进了内存/日志链路，违反 AGENTS.md §5。
func normalizeIPHashOrErr(ipHash string) (string, error) {
	v := strings.TrimSpace(ipHash)
	if v == "" {
		return "", nil
	}
	if model.LooksLikeRawIP(v) {
		return "", model.ErrRawIPForbidden
	}
	normalized := model.NormalizeIPHash(v)
	if normalized == "" {
		return "", model.ErrRawIPForbidden
	}
	return normalized, nil
}

// checkActionInput 把 CheckActionReq 转成引擎输入（脱敏后）。
func checkActionInput(in *rpc.CheckActionReq) (policy.Input, error) {
	if in == nil {
		return policy.Input{}, model.ErrInvalidTarget
	}
	action, err := actionFromProto(in.Action)
	if err != nil {
		return policy.Input{}, err
	}
	if err := checkRequestContext(in.RequestContext); err != nil {
		return policy.Input{}, err
	}
	ipHash, err := normalizeIPHashOrErr(in.IpHash)
	if err != nil {
		return policy.Input{}, err
	}
	return policy.Input{
		RequestID:  strings.TrimSpace(in.RequestId),
		Mid:        in.Mid,
		Action:     action,
		DeviceHash: model.DeviceHash(in.DeviceId),
		IPHash:     ipHash,
		Platform:   sanitizeShortString(in.Platform, 32),
		AppVersion: sanitizeShortString(in.AppVersion, 32),
	}, nil
}

// sanitizeShortString 裁剪外部短字符串并去掉控制符，避免超长值进入索引列或日志。
func sanitizeShortString(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if maxLen > 0 && len(s) > maxLen {
		s = s[:maxLen]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 {
			return -1
		}
		return r
	}, s)
}
