package logic

// getuploadurl_test.go 钉 GetUploadUrl 的五类契约：
//  1. 入参门槛零依赖调用；
//  2. 终态门槛由**两层**把关：缓存命中直接判、不回源；缓存冷/坏时由库里状态判并回填；
//  3. INITIALIZED→UPLOADING 的推进只在第一次签发时发生（第二次不得再写）；
//  4. URL 是短期签名 PUT：过期时间由配置 TTL 决定（<=0 落 900 秒默认），
//     且绝不带 OSS 长期密钥；
//  5. 推进失败时不出 URL（宁可拒发，不给一个状态没落地的地址）。

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go-video/services/upload/internal/repository"
	"go-video/services/upload/model"
	rpc "go-video/services/upload/rpc"
)

const urlUpload = "u-url"

func urlReq(chunkNo int32) *rpc.GetUrlReq {
	return &rpc.GetUrlReq{UploadId: urlUpload, ChunkNo: chunkNo, ChunkSize: 5 << 20, Ip: "203.0.113.7"}
}

func TestGetUploadUrlGuardsRejectWithoutTouchingDependencies(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*rpc.GetUrlReq)
		want error
	}{
		{"upload_id 为空", func(r *rpc.GetUrlReq) { r.UploadId = "" }, model.ErrInvalidUploadID},
		{"chunk_no 为 0", func(r *rpc.GetUrlReq) { r.ChunkNo = 0 }, model.ErrInvalidChunkNo},
		{"chunk_no 为负", func(r *rpc.GetUrlReq) { r.ChunkNo = -1 }, model.ErrInvalidChunkNo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.seedSession(urlUpload, model.SessionStateUploading, 3, 5<<20) // 布好数据，证明拦住它的是门槛不是「查不到」
			req := urlReq(1)
			tc.mut(req)

			reply, err := f.getUploadUrl(t, req)
			mustErrIs(t, "门槛", err, tc.want)
			if reply != nil {
				t.Fatalf("门槛失败却签出了 URL：%+v", reply)
			}
			f.log.assertEmpty(t)
			if s := f.session(t, urlUpload); s.State != model.SessionStateUploading {
				t.Fatalf("门槛失败却把会话状态改成了 %d", s.State)
			}
		})
	}
}

// 第一次签发必须把会话推到 UPLOADING 并落库+刷缓存，然后才出 URL。
func TestGetUploadUrlPromotesInitializedSessionAndSigns(t *testing.T) {
	f := newFixture(t)
	f.seedSession(urlUpload, model.SessionStateInitialized, 3, 5<<20)
	before := time.Now().Unix()

	reply, err := f.getUploadUrl(t, urlReq(2))
	if err != nil {
		t.Fatalf("GetUploadUrl: %v", err)
	}
	f.log.assert(t,
		"cache.GetSession:"+urlUpload,
		"sessions.FindOne:"+urlUpload,
		fmt.Sprintf("sessions.UpdateState:%s=%d", urlUpload, model.SessionStateUploading),
		fmt.Sprintf("cache.SetSession:%s=%d", urlUpload, model.SessionStateUploading),
	)

	if reply.UploadId != urlUpload || reply.ChunkNo != 2 {
		t.Fatalf("回参回声不符：%+v", reply)
	}
	if reply.Method != "PUT" {
		t.Fatalf("method=%q，分片直传只能是 PUT", reply.Method)
	}
	assertUnixWindow(t, "expiration", reply.Expiration, before+testTTL, 5)
	wantURL := fmt.Sprintf("https://%s.%s/%s?uploadId=%s&partNumber=2&X-Amz-Expires=%d&signature=MOCK",
		testBucket, testEndpoint, fmt.Sprintf("uploads/1001/%s/demo.mp4", urlUpload), urlUpload, testTTL)
	if reply.Url != wantURL {
		t.Fatalf("URL 不符\n got=%s\nwant=%s", reply.Url, wantURL)
	}
	// 长期密钥不得出现在签发结果里（AGENTS.md §6）。
	if strings.Contains(strings.ToLower(reply.Url), "secret") || strings.Contains(reply.Url, "AccessKey") {
		t.Fatalf("URL 泄漏了长期凭证线索：%s", reply.Url)
	}
	// rpc/getUrlReply.headers 是契约字段，本实现从不回填：客户端拿不到任何必需 header。
	if len(reply.Headers) != 0 {
		t.Fatalf("headers=%v", reply.Headers)
	}

	stored := f.session(t, urlUpload)
	if stored.State != model.SessionStateUploading {
		t.Fatalf("库里 state=%d，推进没落地", stored.State)
	}
	if stored.Mtime <= stored.Ctime {
		t.Fatalf("mtime=%d 未随推进前进（ctime=%d）", stored.Mtime, stored.Ctime)
	}
}

// 已经是 UPLOADING 的会话再次签发：只读、不写。
// 与上一条构成边界对：推进是幂等的，重传/多分片不会反复打 UPDATE。
func TestGetUploadUrlOnUploadingSessionWritesNothing(t *testing.T) {
	f := newFixture(t)
	f.seedSession(urlUpload, model.SessionStateUploading, 3, 5<<20)

	if _, err := f.getUploadUrl(t, urlReq(1)); err != nil {
		t.Fatalf("GetUploadUrl: %v", err)
	}
	f.log.assert(t,
		"cache.GetSession:"+urlUpload,
		"sessions.FindOne:"+urlUpload,
	)
	if s := f.session(t, urlUpload); s.State != model.SessionStateUploading {
		t.Fatalf("state=%d", s.State)
	}
}

// 缓存命中终态：一次 DB 都不许打。这是这条快路径唯一可证的形态。
func TestGetUploadUrlCachedTerminalStateSkipsDbEntirely(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state int32
		want  error
	}{
		{"缓存说已完成", model.SessionStateCompleted, model.ErrUploadCompleted},
		{"缓存说已取消", model.SessionStateAborted, model.ErrUploadAborted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.seedSession(urlUpload, model.SessionStateInitialized, 2, 1<<20) // 库里故意留一个「还能签发」的状态
			f.cache.warm(urlUpload, tc.state)

			reply, err := f.getUploadUrl(t, urlReq(1))
			mustErrIs(t, "终态门槛", err, tc.want)
			if reply != nil {
				t.Fatalf("终态会话仍签出 URL：%+v", reply)
			}
			f.log.assert(t, "cache.GetSession:"+urlUpload)
			if s := f.session(t, urlUpload); s.State != model.SessionStateInitialized {
				t.Fatalf("快路径把库里状态改了：%+v", s)
			}
		})
	}
}

// 缓存冷/不可用时，判定回落到库里的状态，并把终态回填进缓存。
func TestGetUploadUrlDbTerminalStateRefillsCacheThenRejects(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state int32
		want  error
	}{
		{"库里已完成", model.SessionStateCompleted, model.ErrUploadCompleted},
		{"库里已取消", model.SessionStateAborted, model.ErrUploadAborted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.seedSession(urlUpload, tc.state, 2, 1<<20)

			reply, err := f.getUploadUrl(t, urlReq(1))
			mustErrIs(t, "终态门槛", err, tc.want)
			if reply != nil {
				t.Fatalf("终态会话仍签出 URL：%+v", reply)
			}
			f.log.assert(t,
				"cache.GetSession:"+urlUpload,
				"sessions.FindOne:"+urlUpload,
				fmt.Sprintf("cache.SetSession:%s=%d", urlUpload, tc.state),
			)
			if s, ok := f.cache.lookup(urlUpload); !ok || s != tc.state {
				t.Fatalf("终态没回填缓存：state=%d hit=%v", s, ok)
			}
		})
	}
}

// 缓存里的陈旧非终态值不得掩盖库里的终态：快路径只认「已完成/已取消」，
// 其余一律回源，所以缓存脏值最多浪费一次读，不会放过已完成会话。
func TestGetUploadUrlStaleCacheDoesNotMaskDbTerminalState(t *testing.T) {
	f := newFixture(t)
	f.seedSession(urlUpload, model.SessionStateCompleted, 2, 1<<20)
	f.cache.warm(urlUpload, model.SessionStateUploading) // 脏缓存：看起来还在传

	reply, err := f.getUploadUrl(t, urlReq(1))
	mustErrIs(t, "脏缓存回源", err, model.ErrUploadCompleted)
	if reply != nil {
		t.Fatalf("脏缓存把已完成会话又签了一次：%+v", reply)
	}
	f.log.assert(t,
		"cache.GetSession:"+urlUpload,
		"sessions.FindOne:"+urlUpload,
		fmt.Sprintf("cache.SetSession:%s=%d", urlUpload, model.SessionStateCompleted),
	)
	if s, _ := f.cache.lookup(urlUpload); s != model.SessionStateCompleted {
		t.Fatal("脏缓存值没被纠正，下一次还会白跑")
	}
}

// Redis 读失败不能放大成业务失败：判定交给 DB。
func TestGetUploadUrlCacheReadFailureFallsBackToDb(t *testing.T) {
	f := newFixture(t)
	f.seedSession(urlUpload, model.SessionStateUploading, 3, 5<<20)
	f.cache.getErr = errors.New("redis: connection refused")

	reply, err := f.getUploadUrl(t, urlReq(1))
	if err != nil {
		t.Fatalf("缓存故障被当成了业务错误：%v", err)
	}
	if !strings.Contains(reply.Url, "partNumber=1") {
		t.Fatalf("URL=%s", reply.Url)
	}
	f.log.assert(t,
		"cache.GetSession:"+urlUpload,
		"sessions.FindOne:"+urlUpload,
	)
}

// 会话不存在：只有读，没有任何写。
func TestGetUploadUrlUnknownSessionRejected(t *testing.T) {
	f := newFixture(t)

	reply, err := f.getUploadUrl(t, urlReq(1))
	mustErrIs(t, "查无会话", err, model.ErrUploadNotFound)
	if reply != nil {
		t.Fatalf("无会话也签出 URL：%+v", reply)
	}
	f.log.assert(t,
		"cache.GetSession:"+urlUpload,
		"sessions.FindOne:"+urlUpload,
	)
}

func TestGetUploadUrlSessionReadFailurePropagates(t *testing.T) {
	f := newFixture(t)
	f.seedSession(urlUpload, model.SessionStateInitialized, 2, 1<<20)
	boom := errors.New("upload_session FindOne: context deadline exceeded")
	f.sessions.findErr = boom

	reply, err := f.getUploadUrl(t, urlReq(1))
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	if reply != nil {
		t.Fatalf("读失败却回了应答：%+v", reply)
	}
	f.log.assert(t,
		"cache.GetSession:"+urlUpload,
		"sessions.FindOne:"+urlUpload,
	)
}

// 推进失败就不出 URL：否则客户端拿到一个「状态没落地的地址」，
// 后续 CompleteUpload 会因为会话还停在 INITIALIZED 而语义错位。
func TestGetUploadUrlPromotionFailureDoesNotSign(t *testing.T) {
	f := newFixture(t)
	f.seedSession(urlUpload, model.SessionStateInitialized, 2, 1<<20)
	boom := errors.New("lock wait timeout")
	f.sessions.updateStateErr = boom

	reply, err := f.getUploadUrl(t, urlReq(1))
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	if reply != nil {
		t.Fatalf("推进失败仍签出 URL：%+v", reply)
	}
	f.log.assert(t,
		"cache.GetSession:"+urlUpload,
		"sessions.FindOne:"+urlUpload,
		fmt.Sprintf("sessions.UpdateState:%s=%d", urlUpload, model.SessionStateUploading),
	)
	if s := f.session(t, urlUpload); s.State != model.SessionStateInitialized {
		t.Fatalf("UPDATE 失败库里却变了：%+v", s)
	}
	if _, ok := f.cache.lookup(urlUpload); ok {
		t.Fatal("推进失败却刷了缓存，下一次会按 UPLOADING 少写一次")
	}
}

// 钉住**当前真实行为**：签发前既不查分片清单是否存在，也不校验 chunk_no 上界。
// 3 片的会话可以给 chunk_no=9999 领到 URL（README 已登记）。
func TestGetUploadUrlSignsChunkNoBeyondManifest(t *testing.T) {
	f := newFixture(t)
	f.seedSession(urlUpload, model.SessionStateUploading, 3, 5<<20)

	reply, err := f.getUploadUrl(t, urlReq(9999))
	if err != nil {
		t.Fatalf("当前实现不校验 chunk_no 上界（若本用例变红，说明已加校验，请同步 README）：%v", err)
	}
	if !strings.Contains(reply.Url, "partNumber=9999") {
		t.Fatalf("URL=%s", reply.Url)
	}
	if strings.Contains(f.log.snapshot(), "chunks.") {
		t.Fatalf("居然查了分片清单，本用例前提不再成立：\n%s", f.log.snapshot())
	}
	f.log.assert(t,
		"cache.GetSession:"+urlUpload,
		"sessions.FindOne:"+urlUpload,
	)
}

// TTL：配置 >0 用配置，<=0 落 900 默认。两条一起测才能证明默认值不是巧合。
func TestGetUploadUrlTtlComesFromConfigWithDefaultFallback(t *testing.T) {
	t.Run("配置 60 秒", func(t *testing.T) {
		oss := repository.OSSConfig{Bucket: testBucket, Endpoint: testEndpoint, PresignTTLSeconds: 60}
		f := newFixtureOSS(t, oss)
		f.seedSession(urlUpload, model.SessionStateUploading, 2, 1<<20)
		before := time.Now().Unix()

		reply, err := f.getUploadUrl(t, urlReq(1))
		if err != nil {
			t.Fatalf("GetUploadUrl: %v", err)
		}
		assertUnixWindow(t, "expiration", reply.Expiration, before+60, 5)
		if !strings.Contains(reply.Url, "X-Amz-Expires=60") {
			t.Fatalf("URL 里的有效期没跟着配置走：%s", reply.Url)
		}
	})
	t.Run("配置为 0 落默认 900", func(t *testing.T) {
		oss := repository.OSSConfig{Bucket: testBucket, Endpoint: testEndpoint}
		f := newFixtureOSS(t, oss)
		f.seedSession(urlUpload, model.SessionStateUploading, 2, 1<<20)
		before := time.Now().Unix()

		reply, err := f.getUploadUrl(t, urlReq(1))
		if err != nil {
			t.Fatalf("GetUploadUrl: %v", err)
		}
		assertUnixWindow(t, "expiration", reply.Expiration, before+900, 5)
		if !strings.Contains(reply.Url, "X-Amz-Expires=900") {
			t.Fatalf("URL=%s", reply.Url)
		}
	})
	t.Run("配置为负也落默认", func(t *testing.T) {
		oss := repository.OSSConfig{Bucket: testBucket, Endpoint: testEndpoint, PresignTTLSeconds: -1}
		f := newFixtureOSS(t, oss)
		f.seedSession(urlUpload, model.SessionStateUploading, 2, 1<<20)

		reply, err := f.getUploadUrl(t, urlReq(1))
		if err != nil {
			t.Fatalf("GetUploadUrl: %v", err)
		}
		if !strings.Contains(reply.Url, "X-Amz-Expires=900") {
			t.Fatalf("URL=%s", reply.Url)
		}
	})
}

// 钉住**当前真实行为**：endpoint 带 scheme 时拼出双 scheme 的坏 URL。
// internal/config/config.go:64 的注释示例正是「https://oss-cn-hangzhou.aliyuncs.com」，
// 而 etc/upload.v1.yaml 用的是不带 scheme 的值：两处说法只有一处能拼对（README 已登记）。
func TestGetUploadUrlMalformsWhenEndpointCarriesScheme(t *testing.T) {
	oss := repository.OSSConfig{
		Bucket: testBucket, Endpoint: "https://" + testEndpoint, PresignTTLSeconds: testTTL,
	}
	f := newFixtureOSS(t, oss)
	f.seedSession(urlUpload, model.SessionStateUploading, 2, 1<<20)

	reply, err := f.getUploadUrl(t, urlReq(1))
	if err != nil {
		t.Fatalf("GetUploadUrl: %v", err)
	}
	if strings.Count(reply.Url, "https://") != 2 {
		t.Fatalf("当前实现会把带 scheme 的 endpoint 拼坏（若本用例变红，说明已归一化，请同步 README）：%s", reply.Url)
	}
}
