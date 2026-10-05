package logic

// initupload_test.go 钉 InitUpload 的四条契约：
//  1. 入参门槛在打任何依赖之前完成（零 SQL、零缓存）；
//  2. 一次初始化写的是「1 行会话 + n 行分片清单」，顺序固定，upload_id 由服务端生成；
//  3. 写侧故障的留痕形态：会话写失败什么都不留，清单写失败留下**没有清单的活会话**；
//  4. 秒传（rpc 契约里的 instant / md5 用途）本期完全不生效：相同 md5 重复初始化
//     只会得到两个互不相干的会话，且 instant 恒为 false。

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go-video/services/upload/model"
	rpc "go-video/services/upload/rpc"
)

const initMid int64 = 7001

// initReq 组一个四字段都合法的请求；chunkSize 传 0 表示「由服务端决定」，
// 此时 size 仍必须 >0，否则会先被 size 门槛拦掉。
func initReq(size int64, totalChunks int32, chunkSize int64) *rpc.InitUploadReq {
	return &rpc.InitUploadReq{
		Mid:         initMid,
		Filename:    "demo.mp4",
		Size:        size,
		Typeid:      16,
		Md5:         "d41d8cd98f00b204e9800998ecf8427e",
		ChunkSize:   chunkSize,
		TotalChunks: totalChunks,
		Ip:          "203.0.113.7",
	}
}

// 门槛顺序必须是 mid → filename → size → total_chunks：
// 客户端一次填错四个字段时，返回的第一个错误决定了它先改哪一处。
func TestInitUploadGuardsRejectWithoutTouchingDependencies(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*rpc.InitUploadReq)
		want  error
		first string // 期望首先命中的门槛
	}{
		{"mid 为 0", func(r *rpc.InitUploadReq) { r.Mid = 0 }, model.ErrInvalidMid, "mid"},
		{"mid 为负", func(r *rpc.InitUploadReq) { r.Mid = -1 }, model.ErrInvalidMid, "mid"},
		{"文件名为空", func(r *rpc.InitUploadReq) { r.Filename = "" }, model.ErrInvalidFilename, "filename"},
		{"大小为 0", func(r *rpc.InitUploadReq) { r.Size = 0 }, model.ErrInvalidSize, "size"},
		{"大小为负", func(r *rpc.InitUploadReq) { r.Size = -5 }, model.ErrInvalidSize, "size"},
		{"分片数为 0", func(r *rpc.InitUploadReq) { r.TotalChunks = 0 }, model.ErrInvalidTotalChunks, "total_chunks"},
		{"分片数为负", func(r *rpc.InitUploadReq) { r.TotalChunks = -3 }, model.ErrInvalidTotalChunks, "total_chunks"},
		// 多项同时错时只报第一个：钉住门槛次序，避免改动顺序时静默换错误码。
		{"mid+文件名+大小全错", func(r *rpc.InitUploadReq) {
			r.Mid, r.Filename, r.Size = 0, "", 0
		}, model.ErrInvalidMid, "mid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			req := initReq(15<<20, 3, 5<<20)
			tc.mut(req)

			reply, err := f.initUpload(t, req)
			mustErrIs(t, tc.first, err, tc.want)
			if reply != nil {
				t.Fatalf("门槛失败却回了应答：%+v", reply)
			}
			f.log.assertEmpty(t)
			if got := len(f.sessions.byID); got != 0 {
				t.Fatalf("门槛失败不该写会话，实际 %d 行", got)
			}
		})
	}
}

// chunk_size=0 表示「由服务端决定」，服务端必须落到 5 MiB 并把清单一次写全。
func TestInitUploadWritesSessionAndChunkManifest(t *testing.T) {
	f := newFixture(t)
	before := time.Now().Unix()

	reply, err := f.initUpload(t, initReq(11<<20, 3, 0))
	if err != nil {
		t.Fatalf("InitUpload: %v", err)
	}
	id := reply.UploadId
	if id == "" {
		t.Fatal("upload_id 为空：应由服务端生成且非空")
	}
	f.log.assert(t,
		"sessions.Insert",
		fmt.Sprintf("chunks.InsertBatch:%s=3", id),
		fmt.Sprintf("cache.SetSession:%s=%d", id, model.SessionStateInitialized),
	)

	if reply.UploadProtocol != "multipart" {
		t.Fatalf("upload_protocol=%q", reply.UploadProtocol)
	}
	if reply.TotalChunks != 3 {
		t.Fatalf("total_chunks=%d", reply.TotalChunks)
	}
	if reply.ChunkSize != 5<<20 {
		t.Fatalf("chunk_size=%d，期望默认 5 MiB", reply.ChunkSize)
	}
	if reply.Bucket != testBucket {
		t.Fatalf("bucket=%q", reply.Bucket)
	}

	stored := f.session(t, id)
	if stored.State != model.SessionStateInitialized {
		t.Fatalf("state=%d，初始化应为 %d", stored.State, model.SessionStateInitialized)
	}
	if stored.Mid != initMid || stored.Filename != "demo.mp4" || stored.Typeid != 16 {
		t.Fatalf("归属字段被改写：%+v", stored)
	}
	if stored.ObjectKey != fmt.Sprintf("uploads/%d/%s/demo.mp4", initMid, id) {
		t.Fatalf("object_key=%q，未带上 mid 与 upload_id", stored.ObjectKey)
	}
	if stored.Md5 != "d41d8cd98f00b204e9800998ecf8427e" {
		t.Fatalf("md5=%q，客户端传指纹就该原样入库", stored.Md5)
	}
	assertUnixWindow(t, "ctime", stored.Ctime, before, 5)
	if stored.Mtime < stored.Ctime {
		t.Fatalf("mtime=%d < ctime=%d", stored.Mtime, stored.Ctime)
	}
	if reply.ObjectKey != stored.ObjectKey {
		t.Fatalf("应答 object_key=%q 与库里 %q 不一致", reply.ObjectKey, stored.ObjectKey)
	}

	chunks := f.chunks.rows(id)
	if len(chunks) != 3 {
		t.Fatalf("分片清单 %d 行", len(chunks))
	}
	for i, c := range chunks {
		want := int32(i + 1)
		if c.ChunkNo != want {
			t.Fatalf("第 %d 行 chunk_no=%d：必须从 1 连续编号", i, c.ChunkNo)
		}
		if c.State != model.ChunkStatePending {
			t.Fatalf("chunk %d state=%d，新建清单应为 PENDING", want, c.State)
		}
		if c.Size != 5<<20 {
			t.Fatalf("chunk %d size=%d", want, c.Size)
		}
		if c.UploadId != id {
			t.Fatalf("chunk %d upload_id=%q", want, c.UploadId)
		}
	}
	// 缓存里也要有初态，否则后续 GetUploadUrl 的快路径第一次必然打 DB。
	if s, ok := f.cache.lookup(id); !ok || s != model.SessionStateInitialized {
		t.Fatalf("会话缓存 state=%d hit=%v", s, ok)
	}
}

// 服务端接受任何正数 chunk_size，包括 5 MiB 以下。
// 这里钉的是**当前真实行为**：仓库注释把 5 MiB 写成「OSS/MinIO 分片上传最小分片」，
// 但没有任何一处校验它，客户端因此可以领到一份真传不上去的清单（README 已登记）。
func TestInitUploadAcceptsChunkSizeBelowDocumentedMinimum(t *testing.T) {
	f := newFixture(t)

	reply, err := f.initUpload(t, initReq(2<<10, 2, 1024))
	if err != nil {
		t.Fatalf("当前实现不校验最小分片（若本用例变红，说明已加校验，请同步 README）：%v", err)
	}
	if reply.ChunkSize != 1024 {
		t.Fatalf("chunk_size=%d", reply.ChunkSize)
	}
	for _, c := range f.chunks.rows(reply.UploadId) {
		if c.Size != 1024 {
			t.Fatalf("chunk %d size=%d", c.ChunkNo, c.Size)
		}
	}
}

// 会话写失败：什么都不留，错误原样上抛。
func TestInitUploadSessionInsertFailureLeavesNothing(t *testing.T) {
	f := newFixture(t)
	boom := errors.New("duplicate tablespace")
	f.sessions.insertErr = boom

	reply, err := f.initUpload(t, initReq(2<<20, 2, 1<<20))
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	if reply != nil {
		t.Fatalf("写失败却回了应答：%+v", reply)
	}
	f.log.assert(t, "sessions.Insert")
	if len(f.sessions.byID) != 0 || f.chunks.count("anything") != 0 {
		t.Fatal("会话插入失败后仍留下了行")
	}
}

// 清单写失败：会话已经落了库，留下一个 total_chunks>0 但一行清单都没有的孤儿会话。
// 后续 CompleteUpload 必然 ErrChunkMismatch，客户端只能整个重来：两步写没有包在事务里。
func TestInitUploadChunkBatchFailureLeavesOrphanSession(t *testing.T) {
	f := newFixture(t)
	boom := errors.New("deadlock on upload_chunk")
	f.chunks.insertErr = boom

	reply, err := f.initUpload(t, initReq(3<<20, 3, 1<<20))
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	if reply != nil {
		t.Fatalf("清单写失败却回了应答：%+v", reply)
	}
	if len(f.sessions.byID) != 1 {
		t.Fatalf("孤儿会话应恰好 1 行，实际 %d", len(f.sessions.byID))
	}
	orphan := f.sessions.all()[0] // upload_id 由服务端生成，只能从库里取
	f.log.assert(t,
		"sessions.Insert",
		fmt.Sprintf("chunks.InsertBatch:%s=3", orphan.UploadId),
	)
	stored := f.session(t, orphan.UploadId)
	if stored.TotalChunks != 3 || stored.State != model.SessionStateInitialized {
		t.Fatalf("孤儿会话形态不符：%+v", stored)
	}
	if f.chunks.count(orphan.UploadId) != 0 {
		t.Fatal("清单其实写成功了，本用例前提不成立")
	}
	// 缓存也没写：InitUpload 在 InsertBatch 出错时就 return 了。
	if _, ok := f.cache.lookup(orphan.UploadId); ok {
		t.Fatal("清单写失败却回填了会话缓存")
	}
}

// 秒传契约（rpc/upload.proto:51「md5 用于秒传判断」、:65「instant=true 时无需上传」）
// 本期完全不生效：同一 md5 再初始化一次，得到的是第二个全新会话。
// 这里钉的是**当前真实行为**，缺口登记在 README。
func TestInitUploadNeverDeduplicatesByMd5(t *testing.T) {
	f := newFixture(t)

	first, err := f.initUpload(t, initReq(2<<20, 2, 1<<20))
	if err != nil {
		t.Fatalf("InitUpload#1: %v", err)
	}
	if first.Instant {
		t.Fatal("当前实现恒回 instant=false（若变 true 说明秒传已接上，请同步 README）")
	}
	second, err := f.initUpload(t, initReq(2<<20, 2, 1<<20))
	if err != nil {
		t.Fatalf("InitUpload#2: %v", err)
	}
	if second.Instant {
		t.Fatal("第二次也没查指纹")
	}
	if second.UploadId == first.UploadId {
		t.Fatal("两次初始化回同一个 upload_id：与「不做指纹去重」的结论矛盾")
	}
	if len(f.sessions.byID) != 2 {
		t.Fatalf("会话数=%d，期望 2（重复初始化不去重）", len(f.sessions.byID))
	}
	f.log.assert(t,
		"sessions.Insert",
		fmt.Sprintf("chunks.InsertBatch:%s=2", first.UploadId),
		fmt.Sprintf("cache.SetSession:%s=1", first.UploadId),
		"sessions.Insert",
		fmt.Sprintf("chunks.InsertBatch:%s=2", second.UploadId),
		fmt.Sprintf("cache.SetSession:%s=1", second.UploadId),
	)
	// 整个序列里没有一次读：本服务连「查一下有没有同指纹会话」都没做。
	if strings.Contains(f.log.snapshot(), "FindOne") {
		t.Fatalf("出现了指纹查询调用，本用例前提不再成立：\n%s", f.log.snapshot())
	}
}

// 迁移把 md5 定成 CHAR(32)、filename 定成 VARCHAR(255)，object_key 定成 VARCHAR(512)
// （deploy/migrations/upload/000001_create_upload_session.sql:3,5,11,16）。
// logic 与仓库都只判「空不空」，不判长度与字符集，指纹和文件名原样交给 SQL 层，
// 超限后是报错还是截断取决于 sql_mode（README 缺口登记）。
func TestInitUploadDoesNotValidateMd5OrFilenameShape(t *testing.T) {
	t.Run("40 位 sha1 当 md5 照收", func(t *testing.T) {
		f := newFixture(t)
		req := initReq(2<<20, 2, 1<<20)
		req.Md5 = "356a192b7913b04c54574d18c28d46e77542a729"

		reply, err := f.initUpload(t, req)
		if err != nil {
			t.Fatalf("当前实现不校验指纹长度/字符集：%v", err)
		}
		if got := f.session(t, reply.UploadId).Md5; got != req.Md5 {
			t.Fatalf("库里 md5=%q，与入参不一致", got)
		}
	})
	t.Run("空指纹也照收", func(t *testing.T) {
		f := newFixture(t)
		req := initReq(2<<20, 2, 1<<20)
		req.Md5 = ""

		reply, err := f.initUpload(t, req)
		if err != nil {
			t.Fatalf("InitUpload: %v", err)
		}
		if got := f.session(t, reply.UploadId).Md5; got != "" {
			t.Fatalf("库里 md5=%q", got)
		}
	})
	t.Run("300 字符文件名照收并进了 object_key", func(t *testing.T) {
		f := newFixture(t)
		req := initReq(2<<20, 2, 1<<20)
		req.Filename = strings.Repeat("x", 300) + ".mp4"

		reply, err := f.initUpload(t, req)
		if err != nil {
			t.Fatalf("当前实现不校验文件名长度（VARCHAR(255)）：%v", err)
		}
		stored := f.session(t, reply.UploadId)
		if !strings.Contains(stored.ObjectKey, strings.Repeat("x", 300)) {
			t.Fatal("object_key 没带上原始文件名，本用例前提不成立")
		}
		if len(stored.ObjectKey) < 300 {
			t.Fatalf("object_key 长度=%d", len(stored.ObjectKey))
		}
	})
}
