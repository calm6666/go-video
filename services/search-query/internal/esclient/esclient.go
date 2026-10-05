// Package esclient 是 OpenSearch / Elasticsearch 的只读薄客户端。
//
// 依赖纪律：仓库不引入官方 SDK（禁止为此新增 go.mod 依赖），这里只用标准库
// net/http + encoding/json，覆盖查询侧需要的两个端点：
//
//	POST /{alias}/_search   关键词检索（Search / 联想冷启动回源）
//	POST /{alias}/_count    命中数统计
//
// 设计约束：
//   - 每个请求都带超时（Options.Timeout），并尊重调用方 ctx 的 deadline；
//   - 用 go-zero core/breaker 做熔断，连续失败后快速失败，避免故障放大；
//   - 所有错误都带上下文返回，不吞错误：引擎 4xx/5xx、响应解析失败、
//     别名缺失都会用不同错误码区分，便于运维定位；
//   - 本包不认识业务字段，只负责 HTTP 传输与响应解码；查询 DSL 由
//     internal/repository 构造（业务语义在那里）。
package esclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zeromicro/go-zero/core/breaker"
)

// 引擎侧错误码（logic/repository 通过 errors.Is 判定并映射为领域错误）。
var (
	// ErrNotConfigured Endpoints/Alias 缺失，引擎视为未启用。
	ErrNotConfigured = errors.New("esclient: opensearch not configured")
	// ErrUnavailable 引擎不可用：连接失败、超时、5xx、429。
	ErrUnavailable = errors.New("esclient: opensearch unavailable")
	// ErrCircuitOpen 熔断打开，快速失败。
	ErrCircuitOpen = errors.New("esclient: circuit breaker open")
	// ErrAliasMissing 查询别名/索引不存在（404），通常是索引尚未重建或别名未切换。
	ErrAliasMissing = errors.New("esclient: search alias/index not found")
	// ErrBadStatus 其它非 2xx 响应（DSL 语法错误、字段不存在等）。
	ErrBadStatus = errors.New("esclient: unexpected http status")
	// ErrBadResponse 响应不是合法 JSON、缺少必要字段或存在失败分片。
	ErrBadResponse = errors.New("esclient: malformed engine response")
	// ErrNilBody 请求体为空（调用方错误）。
	ErrNilBody = errors.New("esclient: nil request body")
)

// Options 客户端构造参数。
type Options struct {
	Endpoints       []string
	Alias           string
	Username        string
	Password        string
	Timeout         time.Duration
	MaxResultWindow int64
	BreakerName     string
}

// Client 只读 OpenSearch 客户端。nil 客户端表示“引擎未配置”，
// 所有方法对 nil 安全并返回 ErrNotConfigured。
type Client struct {
	opt  Options
	hc   *http.Client
	brk  breaker.Breaker
	next atomic.Uint64

	// circuitOpen 记录熔断器最近一次是否拒绝过请求（由 doJSON 维护）。
	// 它只是给上层一个“当前是否处于故障观察期”的只读信号，
	// 半开/恢复仍由 breaker 自己决定（上层必须继续发起调用才会触发）。
	circuitOpen atomic.Bool
}

// New 构造客户端。Endpoints 为空或 Alias 为空时返回 ErrNotConfigured，
// 调用方应保留 nil 客户端并在查询时走降级分支。
func New(opt Options) (*Client, error) {
	eps := make([]string, 0, len(opt.Endpoints))
	for _, e := range opt.Endpoints {
		e = strings.TrimRight(strings.TrimSpace(e), "/")
		if e == "" {
			continue
		}
		if !strings.Contains(e, "://") {
			e = "http://" + e
		}
		eps = append(eps, e)
	}
	if len(eps) == 0 || strings.TrimSpace(opt.Alias) == "" {
		return nil, ErrNotConfigured
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 1500 * time.Millisecond
	}
	if opt.MaxResultWindow <= 0 {
		opt.MaxResultWindow = 10000
	}
	opt.Endpoints = eps
	if opt.BreakerName == "" {
		opt.BreakerName = "esclient:" + opt.Alias
	}
	return &Client{
		opt: opt,
		hc:  newHTTPClient(opt.Timeout),
		brk: breaker.NewBreaker(breaker.WithName(opt.BreakerName)),
	}, nil
}

// newHTTPClient 独立 Transport：限制连接数与空闲回收，避免引擎故障时连接堆积。
func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: timeout}).DialContext,
			MaxIdleConns:        64,
			MaxIdleConnsPerHost: 32,
			IdleConnTimeout:     60 * time.Second,
		},
	}
}

// Alias 返回查询别名（日志与排查用）。
func (c *Client) Alias() string {
	if c == nil {
		return ""
	}
	return c.opt.Alias
}

// MaxResultWindow 返回引擎 index.max_result_window（nil 时为 0）。
func (c *Client) MaxResultWindow() int64 {
	if c == nil {
		return 0
	}
	return c.opt.MaxResultWindow
}

// Available 报告客户端是否已配置（nil 安全）。
// 只表示“配置存在”，不代表引擎此刻健康；健康判定用 Healthy。
func (c *Client) Available() bool { return c != nil }

// Healthy 报告此刻是否适合把请求真正发给引擎：已配置且熔断器未处于打开状态（nil 安全）。
//
// 语义边界：
//   - 熔断打开是“最近的临时故障”，不是配置缺失；上层可以据此把缓存命中
//     标记为降级返回，而不是伪装成正常命中（也不继续在故障期消耗引擎配额）。
//   - 该标志不会让上层永久绕开引擎：缓存条目不续期，过期后必然回到未命中分支，
//     再次调用本客户端，由 breaker 的半开机制决定恢复（成功即清零标志）。
func (c *Client) Healthy() bool { return c != nil && !c.circuitOpen.Load() }

// endpoint 轮询选择后端地址。
func (c *Client) endpoint() string {
	if c == nil {
		return ""
	}
	n := uint64(len(c.opt.Endpoints))
	if n == 0 {
		return ""
	}
	return c.opt.Endpoints[c.next.Add(1)%n]
}

// Search 执行 _search 并解码响应。
// 失败时返回包装后的引擎错误；调用方必须把它当作“查询失败”，
// 不得当作“零命中”返回给客户端。
func (c *Client) Search(ctx context.Context, body []byte) (*SearchResponse, error) {
	var resp SearchResponse
	if err := c.doJSON(ctx, "_search", body, &resp); err != nil {
		return nil, err
	}
	if resp.Shards.Failed > 0 {
		// 分片失败会让结果与总数不完整：按引擎异常处理，不返回可疑的部分结果。
		return nil, fmt.Errorf("%w: _search shards failed=%d/%d", ErrBadResponse, resp.Shards.Failed, resp.Shards.Total)
	}
	return &resp, nil
}

// Count 执行 _count 返回命中总数。
func (c *Client) Count(ctx context.Context, body []byte) (int64, error) {
	var resp struct {
		Count int64 `json:"count"`
	}
	if err := c.doJSON(ctx, "_count", body, &resp); err != nil {
		return 0, err
	}
	return resp.Count, nil
}

// doJSON 在熔断器保护下发起请求并解码 JSON。
// 传输错误与解码错误都会触发熔断拒绝，保证持续异常时能快速失败。
func (c *Client) doJSON(ctx context.Context, action string, body []byte, out any) error {
	if c == nil {
		return ErrNotConfigured
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return ErrNilBody
	}
	promise, err := c.brk.AllowCtx(ctx)
	if err != nil {
		c.circuitOpen.Store(true)
		return fmt.Errorf("%w: %v", ErrCircuitOpen, err)
	}
	raw, callErr := c.request(ctx, action, body)
	if callErr != nil {
		promise.Reject(snippet([]byte(callErr.Error())))
		return callErr
	}
	if err := json.Unmarshal(raw, out); err != nil {
		promise.Reject("decode response")
		return fmt.Errorf("%w: %s: %v", ErrBadResponse, action, err)
	}
	promise.Accept()
	// 请求确实跑通了：熔断观察期结束（无论此前是否被拒绝过）。
	c.circuitOpen.Store(false)
	return nil
}

// request 单次 HTTP 调用，返回原始响应体。
func (c *Client) request(ctx context.Context, action string, body []byte) ([]byte, error) {
	ep := c.endpoint()
	if ep == "" {
		return nil, ErrNotConfigured
	}
	url := fmt.Sprintf("%s/%s/%s", ep, c.opt.Alias, action)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("esclient: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.opt.Username != "" {
		req.SetBasicAuth(c.opt.Username, c.opt.Password)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		if isTimeout(err) {
			return nil, fmt.Errorf("%w: %s timeout: %v", ErrUnavailable, url, err)
		}
		return nil, fmt.Errorf("%w: %s: %v", ErrUnavailable, url, err)
	}
	defer func() {
		// 读完剩余少量字节再关闭，保证连接可复用；丢弃错误无业务含义。
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if readErr != nil {
		return nil, fmt.Errorf("%w: read body: %v", ErrUnavailable, readErr)
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return raw, nil
	case resp.StatusCode == http.StatusNotFound && bytes.Contains(raw, []byte("index_not_found")):
		return nil, fmt.Errorf("%w: alias=%s status=%d body=%s", ErrAliasMissing, c.opt.Alias, resp.StatusCode, snippet(raw))
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%w: status=%d body=%s", ErrBadStatus, resp.StatusCode, snippet(raw))
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		return nil, fmt.Errorf("%w: status=%d body=%s", ErrUnavailable, resp.StatusCode, snippet(raw))
	default:
		return nil, fmt.Errorf("%w: status=%d body=%s", ErrBadStatus, resp.StatusCode, snippet(raw))
	}
}

// maxResponseBytes 单次响应硬上限（32MB），防御异常巨型返回拖垮进程。
const maxResponseBytes = 32 << 20

// isTimeout 判定是否为网络/上下文超时。
func isTimeout(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// snippet 截取响应/错误片段用于日志，避免把整个结果集写进错误信息。
func snippet(b []byte) string {
	const max = 256
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "...(truncated)"
	}
	return s
}
