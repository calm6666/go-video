// Code scaffolded by goctl. Safe to edit.

package svc

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"go-video/services/notification/internal/config"
	"go-video/services/notification/internal/provider"
)

// buildProviders 按配置装配通道适配器注册表。
//
// 密钥口径（AGENTS.md §4、§6）：配置里只允许出现环境变量名（ApiKeyRef/SecretRef）
// 与 ${ENV_NAME} 形式的请求头占位，真实密钥由 Secret/Vault 注入到进程环境。
//
// 三种结果：
//   - Enabled=false：不注册该通道。后续该通道投递得到 ErrProviderNotConfigured，
//     由调度器按退避重试直至进死信留档——这是“未配置”唯一正确的表现，绝不伪造成功；
//     同时给出一条 note 让运维在启动日志里看得见哪些通道是空的。
//   - Enabled=true 且配置合法：注册适配器。
//   - Enabled=true 但端点/密钥/签名配置不合法：返回错误，进程启动即失败。
//     运维显式打开了通道却配错，让它安静地不发送比立刻报错更糟。
func buildProviders(c config.ProvidersConf) (*provider.Registry, []string, error) {
	entries := []struct {
		channel string
		label   string
		conf    config.ProviderConf
	}{
		{channel: provider.ChannelPush, label: "Providers.Push", conf: c.Push},
		{channel: provider.ChannelSMS, label: "Providers.Sms", conf: c.Sms},
		{channel: provider.ChannelEmail, label: "Providers.Email", conf: c.Email},
	}
	registry := provider.NewRegistry()
	var notes []string
	for _, e := range entries {
		if !e.conf.Enabled {
			notes = append(notes, fmt.Sprintf("%s.Enabled=false，通道 %s 未接入：该通道投递会按退避重试直至转死信", e.label, e.channel))
			continue
		}
		opt, err := providerOptions(e.channel, e.conf)
		if err != nil {
			return nil, notes, fmt.Errorf("notification/svc: %s: %w", e.label, err)
		}
		p, err := provider.NewHTTP(opt, nil)
		if err != nil {
			return nil, notes, fmt.Errorf("notification/svc: %s: %w", e.label, err)
		}
		if err := registry.Register(p); err != nil {
			return nil, notes, fmt.Errorf("notification/svc: %s: %w", e.label, err)
		}
	}
	return registry, notes, nil
}

// providerOptions 把配置项翻译成适配器构造参数，并从环境解析密钥与请求头占位。
func providerOptions(channel string, c config.ProviderConf) (provider.Options, error) {
	opt := provider.Options{
		Name:           strings.TrimSpace(c.Name),
		Channel:        channel,
		Endpoint:       strings.TrimSpace(c.Endpoint),
		Method:         strings.ToUpper(strings.TrimSpace(c.Method)),
		Timeout:        time.Duration(c.TimeoutMs) * time.Millisecond,
		MaxRetries:     int(c.MaxRetries),
		RetryDelay:     time.Duration(c.RetryDelayMs) * time.Millisecond,
		BodyTemplate:   c.BodyTemplate,
		SignMode:       strings.TrimSpace(c.SignMode),
		APIKeyHeader:   strings.TrimSpace(c.ApiKeyHeader),
		SuccessField:   strings.TrimSpace(c.SuccessField),
		SuccessValue:   c.SuccessValue,
		MessageIDField: strings.TrimSpace(c.MessageIDField),
	}
	if opt.SignMode == "" {
		opt.SignMode = provider.SignNone
	}
	headers := make(map[string]string, len(c.Headers))
	for k, v := range c.Headers {
		val, err := expandEnv(v)
		if err != nil {
			return opt, fmt.Errorf("header %s: %w", k, err)
		}
		headers[k] = val
	}
	if len(headers) > 0 {
		opt.Headers = headers
	}
	if ref := strings.TrimSpace(c.ApiKeyRef); ref != "" {
		v, err := requireEnv(ref)
		if err != nil {
			return opt, fmt.Errorf("ApiKeyRef: %w", err)
		}
		opt.APIKeyValue = v
	}
	if ref := strings.TrimSpace(c.SecretRef); ref != "" {
		v, err := requireEnv(ref)
		if err != nil {
			return opt, fmt.Errorf("SecretRef: %w", err)
		}
		opt.SecretValue = v
	}
	return opt, nil
}

// envRefRe 匹配 ${NAME} 形式的环境变量占位。
var envRefRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv 展开字符串里的 ${NAME} 占位；引用了未注入的变量时报错（不静默留空）。
func expandEnv(s string) (string, error) {
	var missing []string
	out := envRefRe.ReplaceAllStringFunc(s, func(ref string) string {
		name := envRefRe.FindStringSubmatch(ref)[1]
		v, ok := os.LookupEnv(name)
		if !ok || v == "" {
			missing = append(missing, name)
			return ref
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("environment %s is not set", strings.Join(missing, ","))
	}
	return out, nil
}

// requireEnv 读取密钥环境变量；留空即视为未注入。
func requireEnv(name string) (string, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return "", fmt.Errorf("environment %s is not set", name)
	}
	return v, nil
}
