package logic

// getuploadstatus_test.go 钉 GetUploadStatus 的四条契约：
//  1. 读侧判定**只看库**（不读缓存），但每次成功都回填会话状态缓存供签发快路径用；
//  2. 进度口径是「state >= UPLOADED」：已校验(VERIFIED)也算完成，待上传不算；
//  3. 分片列表原样透出（序号、大小、ETag、状态、ctime），顺序随库的 chunk_no 升序；
//  4. 钉住**当前真实缺陷**：分片行按统一 chunk_size 记账，末片不反映余数，
//     所以 uploaded_size 可以大于会话声明的 size（客户端进度条会超过 100%）。

import (
	"errors"
	"fmt"
	"testing"

	"go-video/services/upload/model"
	rpc "go-video/services/upload/rpc"
)

const statusUpload = "u-status"

func statusReq() *rpc.UploadStatusReq {
	return &rpc.UploadStatusReq{UploadId: statusUpload, Ip: "203.0.113.7"}
}

func TestGetUploadStatusGuardRejectsWithoutTouchingDependencies(t *testing.T) {
	f := newFixture(t)
	f.seedSession(statusUpload, model.SessionStateUploading, 3, 5<<20)

	reply, err := f.getUploadStatus(t, &rpc.UploadStatusReq{})
	mustErrIs(t, "门槛", err, model.ErrInvalidUploadID)
	if reply != nil {
		t.Fatalf("门槛失败却回了状态：%+v", reply)
	}
	f.log.assertEmpty(t)
}

func TestGetUploadStatusCountsUploadedAndVerifiedOnly(t *testing.T) {
	f := newFixture(t)
	f.seedSession(statusUpload, model.SessionStateUploading, 4, 5<<20)
	// 覆写分片状态：1 已校验、2 已上传、3/4 待上传。
	f.chunks.byUpload[statusUpload][0].State = model.ChunkStateVerified
	f.chunks.byUpload[statusUpload][0].Etag = "etag-1"
	f.chunks.byUpload[statusUpload][1].State = model.ChunkStateUploaded
	f.chunks.byUpload[statusUpload][1].Etag = "etag-2"
	f.cache.warm(statusUpload, model.SessionStateInitialized) // 脏缓存：读侧不该看它

	reply, err := f.getUploadStatus(t, statusReq())
	if err != nil {
		t.Fatalf("GetUploadStatus: %v", err)
	}
	f.log.assert(t,
		"sessions.FindOne:"+statusUpload,
		"chunks.ListByUpload:"+statusUpload,
		fmt.Sprintf("cache.SetSession:%s=%d", statusUpload, model.SessionStateUploading),
	)

	if reply.State != rpc.UploadState_UPLOAD_STATE_UPLOADING {
		t.Fatalf("state=%v，应以库为准", reply.State)
	}
	if reply.CompletedChunks != 2 {
		t.Fatalf("completed_chunks=%d，UPLOADED/VERIFIED 各一", reply.CompletedChunks)
	}
	if reply.UploadedSize != 10<<20 {
		t.Fatalf("uploaded_size=%d，期望 2 片 × 5 MiB", reply.UploadedSize)
	}
	if reply.TotalChunks != 4 || reply.Size != 20<<20 {
		t.Fatalf("total_chunks=%d size=%d", reply.TotalChunks, reply.Size)
	}
	if reply.UploadId != statusUpload {
		t.Fatalf("upload_id=%q", reply.UploadId)
	}
	if reply.AssetId != "" {
		t.Fatalf("asset_id=%q，未完成不该有", reply.AssetId)
	}
	if len(reply.Chunks) != 4 {
		t.Fatalf("chunks=%d 行", len(reply.Chunks))
	}
	for i, c := range reply.Chunks {
		want := int32(i + 1)
		if c.ChunkNo != want {
			t.Fatalf("第 %d 项 chunk_no=%d，必须按序号升序透出", i, c.ChunkNo)
		}
		if c.Size != 5<<20 || c.Ctime == 0 {
			t.Fatalf("chunk %d 字段透出失真：%+v", want, c)
		}
		switch want {
		case 1, 2:
			if c.Etag != fmt.Sprintf("etag-%d", want) {
				t.Fatalf("chunk %d etag=%q", want, c.Etag)
			}
		default:
			if c.State != rpc.ChunkState_CHUNK_STATE_PENDING || c.Etag != "" {
				t.Fatalf("chunk %d 应为待上传：%+v", want, c)
			}
		}
	}
	if reply.Chunks[0].State != rpc.ChunkState_CHUNK_STATE_VERIFIED {
		t.Fatalf("chunk 1 state=%v", reply.Chunks[0].State)
	}
	if reply.Chunks[1].State != rpc.ChunkState_CHUNK_STATE_UPLOADED {
		t.Fatalf("chunk 2 state=%v", reply.Chunks[1].State)
	}
	// 成功读把库里的状态回填进缓存，供 GetUploadUrl 的终态快路径使用。
	if s, ok := f.cache.lookup(statusUpload); !ok || s != model.SessionStateUploading {
		t.Fatalf("回填后缓存 state=%d hit=%v", s, ok)
	}
}

// 读侧完全不看缓存：整个序列里没有一次 GetSession。
func TestGetUploadStatusNeverReadsCacheForJudgement(t *testing.T) {
	f := newFixture(t)
	f.seedSession(statusUpload, model.SessionStateCompleted, 2, 1<<20)
	f.cache.warm(statusUpload, model.SessionStateInitialized) // 故意放一个相反的脏值

	reply, err := f.getUploadStatus(t, statusReq())
	if err != nil {
		t.Fatalf("GetUploadStatus: %v", err)
	}
	if reply.State != rpc.UploadState_UPLOAD_STATE_COMPLETED {
		t.Fatalf("state=%v，被缓存脏值影响了", reply.State)
	}
	f.log.assert(t,
		"sessions.FindOne:"+statusUpload,
		"chunks.ListByUpload:"+statusUpload,
		fmt.Sprintf("cache.SetSession:%s=%d", statusUpload, model.SessionStateCompleted),
	)
	if s, _ := f.cache.lookup(statusUpload); s != model.SessionStateCompleted {
		t.Fatal("脏缓存没被纠正，下一次签发会白跑一次 DB")
	}
}

func TestGetUploadStatusUnknownSessionDoesNotTouchCache(t *testing.T) {
	f := newFixture(t)

	reply, err := f.getUploadStatus(t, statusReq())
	mustErrIs(t, "查无会话", err, model.ErrUploadNotFound)
	if reply != nil {
		t.Fatalf("无会话也回了状态：%+v", reply)
	}
	f.log.assert(t, "sessions.FindOne:"+statusUpload)
	if _, ok := f.cache.lookup(statusUpload); ok {
		t.Fatal("查无会话却写了状态缓存")
	}
}

func TestGetUploadStatusSessionReadFailurePropagates(t *testing.T) {
	f := newFixture(t)
	f.seedSession(statusUpload, model.SessionStateUploading, 2, 1<<20)
	boom := errors.New("upload_session FindOne: invalid connection")
	f.sessions.findErr = boom

	if _, err := f.getUploadStatus(t, statusReq()); !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	f.log.assert(t, "sessions.FindOne:"+statusUpload)
}

// 分片清单读失败：整个请求失败，且不回填缓存（否则会写进一个来路不明的状态）。
func TestGetUploadStatusManifestReadFailurePropagatesWithoutCache(t *testing.T) {
	f := newFixture(t)
	f.seedSession(statusUpload, model.SessionStateUploading, 2, 1<<20)
	boom := errors.New("upload_chunk ListByUpload: too many connection proxies")
	f.chunks.listErr = boom

	reply, err := f.getUploadStatus(t, statusReq())
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	if reply != nil {
		t.Fatalf("清单读失败却回了状态：%+v", reply)
	}
	f.log.assert(t,
		"sessions.FindOne:"+statusUpload,
		"chunks.ListByUpload:"+statusUpload,
	)
	if _, ok := f.cache.lookup(statusUpload); ok {
		t.Fatal("整个请求失败却回填了缓存")
	}
}

// 缓存写失败不影响读侧结论：进度照样回，错误不外泄。
func TestGetUploadStatusSurvivesCacheBackfillFailure(t *testing.T) {
	f := newFixture(t)
	f.seedSession(statusUpload, model.SessionStateUploading, 1, 1<<20)
	f.cache.setErr = errors.New("redis: MAXMEMORY policy reached")

	reply, err := f.getUploadStatus(t, statusReq())
	if err != nil {
		t.Fatalf("缓存回填失败被当成了业务错误：%v", err)
	}
	if reply.CompletedChunks != 0 || reply.UploadedSize != 0 {
		t.Fatalf("进度不符：%+v", reply)
	}
	f.log.assert(t,
		"sessions.FindOne:"+statusUpload,
		"chunks.ListByUpload:"+statusUpload,
		fmt.Sprintf("cache.SetSession:%s=%d", statusUpload, model.SessionStateUploading),
	)
}

// 取消后的会话仍然可查：清单已空，进度归零，但元数据保留。
func TestGetUploadStatusAfterAbortReportsEmptyManifest(t *testing.T) {
	f := newFixture(t)
	f.seedSession(statusUpload, model.SessionStateUploading, 3, 5<<20)
	f.chunks.byUpload[statusUpload][0].State = model.ChunkStateUploaded
	f.chunks.byUpload[statusUpload][0].Etag = "etag-1"

	if _, err := f.abortUpload(t, &rpc.AbortUploadReq{UploadId: statusUpload}); err != nil {
		t.Fatalf("AbortUpload: %v", err)
	}
	f.log.ops = nil
	reply, err := f.getUploadStatus(t, statusReq())
	if err != nil {
		t.Fatalf("GetUploadStatus: %v", err)
	}
	if reply.State != rpc.UploadState_UPLOAD_STATE_ABORTED {
		t.Fatalf("state=%v", reply.State)
	}
	if reply.CompletedChunks != 0 || reply.UploadedSize != 0 || len(reply.Chunks) != 0 {
		t.Fatalf("取消后仍报进度：%+v", reply)
	}
	if reply.TotalChunks != 3 {
		t.Fatalf("total_chunks=%d，会话元数据应保留", reply.TotalChunks)
	}
	f.log.assert(t,
		"sessions.FindOne:"+statusUpload,
		"chunks.ListByUpload:"+statusUpload,
		fmt.Sprintf("cache.SetSession:%s=%d", statusUpload, model.SessionStateAborted),
	)
}

// 完成后的会话把占位 asset_id 透出，客户端据此去 asset 侧接续。
func TestGetUploadStatusAfterCompleteReportsAssetPlaceholder(t *testing.T) {
	f := newFixture(t)
	f.seedSession(statusUpload, model.SessionStateUploading, 2, 1<<20)

	if _, err := f.completeUpload(t, &rpc.CompleteUploadReq{
		UploadId: statusUpload, Parts: etags(2, 1), Md5: "final",
	}); err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	f.log.ops = nil
	reply, err := f.getUploadStatus(t, statusReq())
	if err != nil {
		t.Fatalf("GetUploadStatus: %v", err)
	}
	if reply.State != rpc.UploadState_UPLOAD_STATE_COMPLETED {
		t.Fatalf("state=%v", reply.State)
	}
	if reply.AssetId != "asset-placeholder:"+statusUpload {
		t.Fatalf("asset_id=%q", reply.AssetId)
	}
	if reply.CompletedChunks != 2 || reply.UploadedSize != 2<<20 {
		t.Fatalf("进度不符：completed=%d uploaded=%d", reply.CompletedChunks, reply.UploadedSize)
	}
}

// 钉住**当前真实缺陷**：清单按统一 chunk_size 记账，末片不反映余数。
// 11 MiB 的会话领到 3 片 × 5 MiB 的清单，全传完后进度是 15 MiB > 11 MiB。
func TestGetUploadStatusUploadedSizeCanExceedDeclaredSize(t *testing.T) {
	f := newFixture(t)
	f.seedSession(statusUpload, model.SessionStateUploading, 3, 5<<20)
	f.sessions.byID[statusUpload].Size = 11 << 20 // 真实文件 11 MiB，末片应为 1 MiB

	if _, err := f.completeUpload(t, &rpc.CompleteUploadReq{
		UploadId: statusUpload, Parts: etags(3, 1),
	}); err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	f.log.ops = nil
	reply, err := f.getUploadStatus(t, statusReq())
	if err != nil {
		t.Fatalf("GetUploadStatus: %v", err)
	}
	if reply.Size != 11<<20 {
		t.Fatalf("size=%d", reply.Size)
	}
	if reply.UploadedSize != 15<<20 {
		t.Fatalf("uploaded_size=%d，当前实现按统一分片大小累加（若变 11 MiB 说明已修正，请同步 README）", reply.UploadedSize)
	}
	if reply.UploadedSize > reply.Size {
		// 显式把这个可观测后果钉住：客户端按这两个字段画进度条就会超 100%。
		t.Logf("已确认缺陷形态：uploaded_size(%d) > size(%d)", reply.UploadedSize, reply.Size)
	}
	if got := f.uploadedSize(statusUpload); got != reply.UploadedSize {
		t.Fatalf("库里分片大小之和=%d，应答=%d", got, reply.UploadedSize)
	}
}
