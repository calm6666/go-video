package esclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ErrNotFound 表示文档/索引/别名不存在（HTTP 404）。
var ErrNotFound = errors.New("esclient: resource not found")

// ErrEmptyEndpoints OpenSearch Endpoints 未配置，写读路径都不可用。
var ErrEmptyEndpoints = errors.New("esclient: opensearch endpoints are empty")

// ErrWriteGuarded 未配置密码且未显式允许匿名写：写操作被明确拒绝而不是静默成功。
var ErrWriteGuarded = errors.New("esclient: opensearch password is empty, 写操作被拒绝（请配置 OpenSearch.Password，" +
	"或在本地匿名集群显式设置 OpenSearch.AllowAnonymousWrites: true）")

// HTTPError 携带状态码与截断后的响应体片段，便于排障且不泄漏大对象。
type HTTPError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("esclient: %s %s -> HTTP %d: %s", e.Method, e.Path, e.Status, e.Body)
}

// Options 客户端构造参数（由 internal/config 的 OpenSearch 段转换而来）。
type Options struct {
	Endpoints            []string
	Username             string
	Password             string
	Timeout              time.Duration
	MaxRetries           int
	BulkActions          int
	AllowAnonymousWrites bool
	// Analyzer 建索引时写入的分词策略；留空即 DefaultAnalyzer（内置 cjk，无插件依赖）。
	Analyzer Analyzer
}

// Client 是本服务需要的 OpenSearch API 面（接口注入，便于离线单测与替换实现）。
type Client interface {
	// IndexDoc 以显式主键覆盖写入整篇文档（PUT /<index>/_doc/<id>，幂等）。
	IndexDoc(ctx context.Context, index, id string, src interface{}) error
	// GetSource 读取文档 _source 原文；found=false 表示不存在。
	GetSource(ctx context.Context, index, id string) (json.RawMessage, bool, error)
	// DeleteDoc 删除文档；返回 deleted=false 表示原本不存在（幂等）。
	DeleteDoc(ctx context.Context, index, id string) (bool, error)
	// UpdatePartial 合并更新部分字段（POST /<index>/_update/<id>），
	// docAsUpsert=false 时文档不存在返回 found=false 而不新建。
	UpdatePartial(ctx context.Context, index, id string, partial map[string]interface{}, retryOnConflict int) (bool, error)
	// Bulk 批量写入（POST /_bulk，NDJSON；所有 op 必须带显式 _id 才允许重试）。
	Bulk(ctx context.Context, index string, ops []BulkOp) (*BulkResult, error)
	// Refresh 让写入立即可搜（重建收尾时调用一次，逐条写入不做 refresh）。
	Refresh(ctx context.Context, index string) error
	// CreateIndex 创建物理索引；alreadyExists=true 表示索引已存在（幂等）。
	CreateIndex(ctx context.Context, index, schemaVersion string) (alreadyExists bool, err error)
	// IndexExists 判断索引或别名是否存在。
	IndexExists(ctx context.Context, index string) (bool, error)
	// Count 返回索引文档数；索引不存在返回 ErrNotFound。
	Count(ctx context.Context, index string) (int64, error)
	// AliasTargets 返回别名指向的物理索引列表；别名不存在返回 ErrNotFound。
	AliasTargets(ctx context.Context, alias string) ([]string, error)
	// ApplyAliasActions 原子执行别名增删（POST /_aliases）。
	ApplyAliasActions(ctx context.Context, actions []AliasAction) error
	// ReindexSlice 按 content_id 区间切片重建（POST /_reindex，不自动重试）。
	ReindexSlice(ctx context.Context, req ReindexSliceReq) (*ReindexResult, error)
	// ClusterHealth 返回索引级健康（green/yellow/red）；索引不存在返回 "missing"。
	ClusterHealth(ctx context.Context, index string) (string, error)
	// MaxBulkActions 单次 _bulk 的最大操作数（上层按此切分批次）。
	MaxBulkActions() int
	// Close 释放连接池。
	Close() error
}

// httpClient 是 Client 的标准库实现。
type httpClient struct {
	opts     Options
	client   *http.Client
	endpoint atomic.Uint64
}

// New 构造 OpenSearch HTTP 客户端。
// Endpoints 为空时返回 ErrEmptyEndpoints，调用方据此拒绝启动写路径而不是静默降级。
func New(opts Options) (Client, error) {
	eps := make([]string, 0, len(opts.Endpoints))
	for _, e := range opts.Endpoints {
		e = strings.TrimRight(strings.TrimSpace(e), "/")
		if e != "" {
			eps = append(eps, e)
		}
	}
	if len(eps) == 0 {
		return nil, ErrEmptyEndpoints
	}
	if _, err := url.Parse(eps[0]); err != nil {
		return nil, fmt.Errorf("esclient: invalid endpoint %q: %w", eps[0], err)
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Second
	}
	if opts.MaxRetries < 0 {
		opts.MaxRetries = 0
	}
	if opts.BulkActions <= 0 {
		opts.BulkActions = 500
	}
	analyzer, err := opts.Analyzer.normalize()
	if err != nil {
		return nil, err
	}
	opts.Analyzer = analyzer
	opts.Endpoints = eps
	return &httpClient{
		opts:   opts,
		client: &http.Client{Timeout: opts.Timeout},
	}, nil
}

// MaxBulkActions 返回配置的批量写切分阈值。
func (c *httpClient) MaxBulkActions() int { return c.opts.BulkActions }

// Close 关闭空闲连接。
func (c *httpClient) Close() error {
	c.client.CloseIdleConnections()
	return nil
}

// nextEndpoint 轮询端点，多节点时分散压力。
func (c *httpClient) nextEndpoint() string {
	i := c.endpoint.Add(1) % uint64(len(c.opts.Endpoints))
	return c.opts.Endpoints[i]
}

// guardWrite 写操作前置校验：密码为空且未显式允许匿名写时直接失败，
// 防止配置缺失导致「看起来成功了但索引没写」。
func (c *httpClient) guardWrite() error {
	if c.opts.Password == "" && !c.opts.AllowAnonymousWrites {
		return ErrWriteGuarded
	}
	return nil
}

// request 执行一次 HTTP 调用；idempotent=true 时对网络错误与 5xx/429 做退避重试。
// out 非空时反序列化 JSON 响应体；404 统一返回 ErrNotFound（除调用方显式处理）。
func (c *httpClient) request(ctx context.Context, method, path string, query url.Values, body []byte, contentType string, idempotent bool, out interface{}) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	attempts := 1
	if idempotent {
		attempts += c.opts.MaxRetries
	}

	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			// 指数退避 + 抖动，避免把故障的服务端打得更死。
			backoff := time.Duration(50*(1<<uint(i-1))) * time.Millisecond
			jitter := time.Duration(rand.Int63n(int64(20*i))) * time.Millisecond
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff + jitter):
			}
		}

		retryable, err := c.doOnce(ctx, method, path, query, body, contentType, out)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable || !idempotent {
			return err
		}
	}
	return fmt.Errorf("esclient: %s %s failed after %d attempts: %w", method, path, attempts, lastErr)
}

// doOnce 单次请求；retryable 表示该错误值得再试（仅幂等请求会被再试）。
func (c *httpClient) doOnce(ctx context.Context, method, path string, query url.Values, body []byte, contentType string, out interface{}) (retryable bool, err error) {
	full := c.nextEndpoint() + path
	if len(query) > 0 {
		full += "?" + query.Encode()
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, full, reader)
	if err != nil {
		return false, fmt.Errorf("esclient: build request: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	if c.opts.Username != "" || c.opts.Password != "" {
		req.SetBasicAuth(c.opts.Username, c.opts.Password)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		// 传输层错误（连接重置、超时）对幂等请求可安全重试。
		return true, fmt.Errorf("esclient: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return true, fmt.Errorf("esclient: read response %s %s: %w", method, path, err)
	}

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, ErrNotFound
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated:
		if out == nil || len(respBody) == 0 {
			return false, nil
		}
		if err := json.Unmarshal(respBody, out); err != nil {
			return false, fmt.Errorf("esclient: unmarshal response %s %s: %w", method, path, err)
		}
		return false, nil
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		return true, &HTTPError{Method: method, Path: path, Status: resp.StatusCode, Body: truncate(string(respBody), 512)}
	default:
		return false, &HTTPError{Method: method, Path: path, Status: resp.StatusCode, Body: truncate(string(respBody), 512)}
	}
}

// IndexDoc 覆盖写入单篇文档。
func (c *httpClient) IndexDoc(ctx context.Context, index, id string, src interface{}) error {
	if err := c.guardWrite(); err != nil {
		return err
	}
	body, err := marshalNoHTMLEscape(src)
	if err != nil {
		return fmt.Errorf("esclient: marshal doc %s: %w", id, err)
	}
	path := "/" + url.PathEscape(index) + "/_doc/" + url.PathEscape(id)
	return c.request(ctx, http.MethodPut, path, nil, body, "application/json", true, nil)
}

// getSourceResponse 是 GET /<index>/_doc/<id> 的响应结构。
type getSourceResponse struct {
	Found  bool            `json:"found"`
	Source json.RawMessage `json:"_source"`
}

// GetSource 读取文档原文。
func (c *httpClient) GetSource(ctx context.Context, index, id string) (json.RawMessage, bool, error) {
	path := "/" + url.PathEscape(index) + "/_doc/" + url.PathEscape(id)
	var out getSourceResponse
	if err := c.request(ctx, http.MethodGet, path, nil, nil, "", true, &out); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if !out.Found || len(out.Source) == 0 {
		return nil, false, nil
	}
	return out.Source, true, nil
}

type writeResponse struct {
	Result string `json:"result"` // created/updated/deleted/not_found/noop
}

// DeleteDoc 删除文档。
func (c *httpClient) DeleteDoc(ctx context.Context, index, id string) (bool, error) {
	if err := c.guardWrite(); err != nil {
		return false, err
	}
	path := "/" + url.PathEscape(index) + "/_doc/" + url.PathEscape(id)
	var out writeResponse
	if err := c.request(ctx, http.MethodDelete, path, nil, nil, "", true, &out); err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return out.Result == "deleted", nil
}

// UpdatePartial 部分字段合并更新。
func (c *httpClient) UpdatePartial(ctx context.Context, index, id string, partial map[string]interface{}, retryOnConflict int) (bool, error) {
	if err := c.guardWrite(); err != nil {
		return false, err
	}
	body, err := marshalNoHTMLEscape(map[string]interface{}{"doc": partial})
	if err != nil {
		return false, fmt.Errorf("esclient: marshal partial: %w", err)
	}
	q := url.Values{}
	if retryOnConflict > 0 {
		q.Set("retry_on_conflict", strconv.Itoa(retryOnConflict))
	}
	path := "/" + url.PathEscape(index) + "/_update/" + url.PathEscape(id)
	var out writeResponse
	// 服务端 retry_on_conflict 已处理并发冲突，这里不再整体重试，避免重复合并。
	if err := c.request(ctx, http.MethodPost, path, q, body, "application/json", false, &out); err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return out.Result == "updated" || out.Result == "noop", nil
}

// Bulk 批量写入。
func (c *httpClient) Bulk(ctx context.Context, index string, ops []BulkOp) (*BulkResult, error) {
	if err := c.guardWrite(); err != nil {
		return nil, err
	}
	body, err := BuildBulkNDJSON(index, ops)
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	// _bulk 响应需先取原始字节再展平 items（每个 item 的 key 就是动作名）。
	if err := c.request(ctx, http.MethodPost, "/_bulk", nil, body, "application/x-ndjson", true, &raw); err != nil {
		return nil, err
	}
	return parseBulkResponse(raw)
}

// Refresh 触发索引 refresh。
func (c *httpClient) Refresh(ctx context.Context, index string) error {
	if err := c.guardWrite(); err != nil {
		return err
	}
	path := "/" + url.PathEscape(index) + "/_refresh"
	return c.request(ctx, http.MethodPost, path, nil, nil, "", true, nil)
}

// CreateIndex 创建物理索引（PUT /<index>）。分词策略取自构造参数 Options.Analyzer，
// 因此进程与脚本（cmd/esmapping）产出的 mapping 只会有一份定义。
func (c *httpClient) CreateIndex(ctx context.Context, index, schemaVersion string) (bool, error) {
	if err := c.guardWrite(); err != nil {
		return false, err
	}
	body, err := IndexBody(schemaVersion, c.opts.Analyzer)
	if err != nil {
		return false, err
	}
	path := "/" + url.PathEscape(index)
	if err := c.request(ctx, http.MethodPut, path, nil, body, "application/json", true, nil); err != nil {
		// 已存在视为幂等成功：重建可能因进程重启重复执行。
		var he *HTTPError
		if errors.As(err, &he) && he.Status == http.StatusBadRequest && strings.Contains(he.Body, "resource_already_exists_exception") {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

// IndexExists 判断索引或别名是否存在（HEAD /<index>）。
func (c *httpClient) IndexExists(ctx context.Context, index string) (bool, error) {
	path := "/" + url.PathEscape(index)
	err := c.request(ctx, http.MethodHead, path, nil, nil, "", true, nil)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

type countResponse struct {
	Count int64 `json:"count"`
}

// Count 返回文档数。
func (c *httpClient) Count(ctx context.Context, index string) (int64, error) {
	path := "/" + url.PathEscape(index) + "/_count"
	var out countResponse
	if err := c.request(ctx, http.MethodGet, path, nil, nil, "", true, &out); err != nil {
		if errors.Is(err, ErrNotFound) {
			return 0, fmt.Errorf("%w: index %s", ErrNotFound, index)
		}
		return 0, err
	}
	return out.Count, nil
}

// ClusterHealth 返回索引级健康状态。
func (c *httpClient) ClusterHealth(ctx context.Context, index string) (string, error) {
	path := "/_cluster/health/" + url.PathEscape(index)
	var out struct {
		Status string `json:"status"`
	}
	if err := c.request(ctx, http.MethodGet, path, nil, nil, "", true, &out); err != nil {
		if errors.Is(err, ErrNotFound) {
			return "missing", nil
		}
		return "", err
	}
	if out.Status == "" {
		return "unknown", nil
	}
	return out.Status, nil
}
