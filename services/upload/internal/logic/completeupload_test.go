package logic

// completeupload_test.go 钉 CompleteUpload 的七条契约：
//  1. 入参门槛（空 upload_id / 空清单）零依赖调用；
//  2. 判定链完全以库为准，**一次缓存都不读**（与 GetUploadUrl 的快路径相对）；
//  3. 清单必须与 total_chunks 逐条对齐：条数、覆盖、ETag 非空三重门槛；
//  4. 门槛失败不留任何半截写（一片都不标，状态不动）；
//  5. 成功后库里是「清单全标 + md5/asset_id 回填 + 会话 COMPLETED + 一行 media.task.v1 事件
//     + 缓存刷成 3」，顺序固定；asset_id 只能是占位值（真实 ID 由 asset 服务回填）；
//  6. 四写（md5 / asset_id / state / outbox）必须落在**同一次 TransactCtx** 的同一会话上，
//     任一写失败时该事务被标记回滚，且**不刷缓存**（AGENTS.md §5：业务写与 Outbox 同事务）；
//  7. 事件行的列与 payload 同源：event_id / aggregate_id==分区键==upload_id /
//     occurred_at 三处一致，payload 里不含任何预签名 URL 或 OSS 凭据。
//
// 仍在钉住的真实缺陷：从未上传过任何分片也能凭客户端自报 ETag 直接完成；
// 分片清单的逐片 MarkUploaded 仍在事务**之外**（README 缺口 8）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go-video/common/eventenvelope"
	"go-video/services/upload/model"
	rpc "go-video/services/upload/rpc"
)

const doneUpload = "u-done"

// etags 组 n 条「客户端自报」的分片清单。
func etags(n int32, from int32) []*rpc.ChunkPart {
	out := make([]*rpc.ChunkPart, 0, n)
	for i := from; i < from+n; i++ {
		out = append(out, &rpc.ChunkPart{ChunkNo: i, Etag: fmt.Sprintf("etag-%d", i)})
	}
	return out
}

func doneReq(n int32, md5 string) *rpc.CompleteUploadReq {
	return &rpc.CompleteUploadReq{UploadId: doneUpload, Parts: etags(n, 1), Md5: md5, Ip: "203.0.113.7"}
}

// mediaTaskOps 返回「事务内四写」的期望序列（md5 非空时的三条会话写 + 事件行）。
func mediaTaskOps(uploadID, md5 string) []string {
	ops := []string{}
	if md5 != "" {
		ops = append(ops, fmt.Sprintf("sessions.SetMd5Tx:%s=%s", uploadID, md5))
	}
	return append(ops,
		fmt.Sprintf("sessions.UpdateAssetIdTx:%s=asset-placeholder:%s", uploadID, uploadID),
		fmt.Sprintf("sessions.UpdateStateTx:%s=%d", uploadID, model.SessionStateCompleted),
		fmt.Sprintf("outbox.Insert:%s/%s", model.EventMediaTask, uploadID),
	)
}

func TestCompleteUploadGuardsRejectWithoutTouchingDependencies(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*rpc.CompleteUploadReq)
		want error
	}{
		{"upload_id 为空", func(r *rpc.CompleteUploadReq) { r.UploadId = "" }, model.ErrInvalidUploadID},
		{"清单为空", func(r *rpc.CompleteUploadReq) { r.Parts = nil }, model.ErrChunkMismatch},
		{"清单是空切片", func(r *rpc.CompleteUploadReq) { r.Parts = []*rpc.ChunkPart{} }, model.ErrChunkMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.seedSession(doneUpload, model.SessionStateUploading, 3, 5<<20)
			req := doneReq(3, "new-md5")
			tc.mut(req)

			reply, err := f.completeUpload(t, req)
			mustErrIs(t, "门槛", err, tc.want)
			if reply != nil {
				t.Fatalf("门槛失败却回了完成应答：%+v", reply)
			}
			f.log.assertEmpty(t)
			stored := f.session(t, doneUpload)
			if stored.State != model.SessionStateUploading || stored.AssetId != "" {
				t.Fatalf("门槛失败却改了库：%+v", stored)
			}
			// 门槛失败连事务都不该开：开了就是「什么都没写却要回滚一次」的空转。
			if f.conn.transactions != 0 {
				t.Fatalf("门槛失败却开了 %d 次事务", f.conn.transactions)
			}
			if len(f.outbox.rows) != 0 {
				t.Fatalf("门槛失败却写了事件行：%+v", f.outbox.rows[0])
			}
		})
	}
}

func TestCompleteUploadHappyPathWritesManifestAssetAndState(t *testing.T) {
	before := time.Now().Unix()
	f := newFixture(t)
	f.seedSession(doneUpload, model.SessionStateUploading, 3, 5<<20)
	f.cache.warm(doneUpload, model.SessionStateInitialized) // 脏缓存：完成判定不该看它一眼

	reply, err := f.completeUpload(t, doneReq(3, "1q2w3e4r5t"))
	if err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	after := time.Now().Unix()
	f.log.assert(t,
		append([]string{
			"sessions.FindOne:" + doneUpload,
			"chunks.ListByUpload:" + doneUpload,
			fmt.Sprintf("chunks.MarkUploaded:%s=1", doneUpload),
			fmt.Sprintf("chunks.MarkUploaded:%s=2", doneUpload),
			fmt.Sprintf("chunks.MarkUploaded:%s=3", doneUpload),
		}, append(mediaTaskOps(doneUpload, "1q2w3e4r5t"),
			fmt.Sprintf("cache.SetSession:%s=%d", doneUpload, model.SessionStateCompleted))...)...,
	)

	if reply.UploadId != doneUpload || reply.State != rpc.UploadState_UPLOAD_STATE_COMPLETED {
		t.Fatalf("回参不符：%+v", reply)
	}
	if reply.Md5 != "1q2w3e4r5t" {
		t.Fatalf("md5=%q", reply.Md5)
	}
	if reply.Size != 15<<20 {
		t.Fatalf("size=%d", reply.Size)
	}
	if reply.ObjectKey != fmt.Sprintf("uploads/1001/%s/demo.mp4", doneUpload) {
		t.Fatalf("object_key=%q", reply.ObjectKey)
	}
	// asset_id 只能是占位串：真实 ID 由 asset 服务消费事件后回填。
	if reply.AssetId != "asset-placeholder:"+doneUpload {
		t.Fatalf("asset_id=%q，期望占位串", reply.AssetId)
	}

	stored := f.session(t, doneUpload)
	if stored.State != model.SessionStateCompleted || stored.Md5 != "1q2w3e4r5t" || stored.AssetId == "" {
		t.Fatalf("库里形态不符：%+v", stored)
	}
	for _, c := range f.chunks.rows(doneUpload) {
		if c.State != model.ChunkStateUploaded {
			t.Fatalf("chunk %d state=%d，完成后应全为 UPLOADED", c.ChunkNo, c.State)
		}
		if c.Etag != fmt.Sprintf("etag-%d", c.ChunkNo) {
			t.Fatalf("chunk %d etag=%q", c.ChunkNo, c.Etag)
		}
	}
	if s, ok := f.cache.lookup(doneUpload); !ok || s != model.SessionStateCompleted {
		t.Fatalf("完成后会话缓存 state=%d hit=%v", s, ok)
	}
	// 事件行：一次完成只写一行，且它是待发布状态（投递交给 publisher）。
	row := f.outbox.only(t)
	if row.State != model.OutboxStatePending || row.RetryCount != 0 || row.NextRetryAt != 0 {
		t.Errorf("事件行状态不符：%+v", row)
	}
	if row.EventID == "" || row.EventType != model.EventMediaTask || row.SchemaVersion != model.EventSchemaVersion {
		t.Errorf("事件行契约列不符：%+v", row)
	}
	if row.AggregateType != model.AggregateTypeUploadSession || row.AggregateID != doneUpload {
		t.Errorf("事件行聚合根不符：%+v", row)
	}
	// occurred_at 是信封的 Unix 秒，不是 ctime：下游按它排事件时效。
	assertUnixWindow(t, "事件 occurred_at", row.OccurredAt, before, after-before+2)
}

// TestCompleteUploadFourWritesShareOneTransaction 是缺口 9 的收口证据：
// md5 回填、asset_id 回填、状态推进与事件写入必须在同一次 TransactCtx 的同一会话上。
// 判据是「事务次数」加「带会话的写次数」，两者各自独立：
// 只把四写塞进一次事务但偷偷用 conn 直连，transactions=1 而 txWrites 对不上。
func TestCompleteUploadFourWritesShareOneTransaction(t *testing.T) {
	f := newFixture(t)
	f.seedSession(doneUpload, model.SessionStateUploading, 2, 1<<20)

	if _, err := f.completeUpload(t, doneReq(2, "abc")); err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	if f.conn.transactions != 1 {
		t.Fatalf("开了 %d 次事务，期望恰好 1 次（四写同事务）", f.conn.transactions)
	}
	if f.conn.rolledBack != 0 {
		t.Fatalf("成功路径事务被标记回滚 %d 次", f.conn.rolledBack)
	}
	// 三条会话写都拿到事务会话（SetMd5Tx / UpdateAssetIdTx / UpdateStateTx）。
	if f.sessions.txWrites != 3 {
		t.Fatalf("带事务会话的会话写=%d，期望 3", f.sessions.txWrites)
	}
	if !f.outbox.gotTx {
		t.Fatal("事件行不是用事务会话写的：状态与事件会分裂（已 COMPLETED 但没事件，或反之）")
	}
}

// TestCompleteUploadEventRowMatchesEnvelopeContract 把「三处同源」钉成硬约束：
// 列的 event_id / 分区键 / occurred_at 必须与 payload 信封逐项一致，
// 这正是 common/outbox.CheckRow 在投递前做的反查；这里在写入侧就拦住，
// 不留一行「注定被判死」的事件。
func TestCompleteUploadEventRowMatchesEnvelopeContract(t *testing.T) {
	f := newFixture(t)
	f.seedSession(doneUpload, model.SessionStateUploading, 1, 5<<20)

	if _, err := f.completeUpload(t, doneReq(1, "md5-final")); err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	row := f.outbox.only(t)

	var env eventenvelope.Envelope
	if err := json.Unmarshal([]byte(row.Payload), &env); err != nil {
		t.Fatalf("payload 不是合法信封（发布时会被 CheckRow 判死）：%v", err)
	}
	if err := env.Validate(); err != nil {
		t.Fatalf("信封不满足跨服务契约：%v", err)
	}
	if env.EventID != row.EventID {
		t.Errorf("payload event_id=%q 与列=%q 不一致", env.EventID, row.EventID)
	}
	if env.AggregateID != row.AggregateID || env.AggregateID != doneUpload {
		t.Errorf("payload aggregate_id=%q 与列=%q，期望都是 upload_id", env.AggregateID, row.AggregateID)
	}
	if env.EventType != row.EventType || env.SchemaVersion != int(row.SchemaVersion) {
		t.Errorf("信封类型/版本=%s/v%d，与列 %s/v%d 不同源",
			env.EventType, env.SchemaVersion, row.EventType, row.SchemaVersion)
	}
	if got := eventenvelope.Topic(env.EventType, env.SchemaVersion); got != "media.task.v1" {
		t.Errorf("派生 topic=%q，期望 media.task.v1（docs/api-and-events.md §5 登记的契约名）", got)
	}
	if env.Producer != model.Producer {
		t.Errorf("producer=%q，期望 %q", env.Producer, model.Producer)
	}
	// 分区键非空是顺序前提；这里顺带钉住「事件确实可按 upload_id 定位到这一次上传」。
	if row.AggregateID == "" {
		t.Fatal("分区键为空，同一次上传的事件顺序无从保证")
	}
}

// TestCompleteUploadEventPayloadCarriesOnlyMediaFacts 钉住 payload 的字段清单与红线：
// 下游接管媒资需要的事实必须齐（对象引用/文件规格/归属），
// 而预签名 URL、OSS 凭据、调用方 IP 一个都不能有（AGENTS.md §6、§7）。
func TestCompleteUploadEventPayloadCarriesOnlyMediaFacts(t *testing.T) {
	f := newFixture(t)
	f.seedSession(doneUpload, model.SessionStateUploading, 2, 5<<20)

	if _, err := f.completeUpload(t, doneReq(2, "md5-sum")); err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	row := f.outbox.only(t)

	var env eventenvelope.Envelope
	if err := json.Unmarshal([]byte(row.Payload), &env); err != nil {
		t.Fatalf("payload 解析失败：%v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(env.Payload, &body); err != nil {
		t.Fatalf("payload body 解析失败：%v", err)
	}
	wantKeys := []string{"upload_id", "mid", "filename", "size", "typeid", "md5", "bucket",
		"object_key", "asset_id", "chunk_size", "total_chunks", "completed_at"}
	for _, k := range wantKeys {
		if _, ok := body[k]; !ok {
			t.Errorf("payload 缺字段 %q，下游拿不到它就无法接管媒资：%v", k, body)
		}
	}
	if body["upload_id"] != doneUpload || body["object_key"] != fmt.Sprintf("uploads/1001/%s/demo.mp4", doneUpload) {
		t.Errorf("payload 定位字段不符：%v", body)
	}
	if body["md5"] != "md5-sum" {
		t.Errorf("payload md5=%v，期望本次回填的新值而不是库里的旧值", body["md5"])
	}
	if body["asset_id"] != "asset-placeholder:"+doneUpload {
		t.Errorf("payload asset_id=%v", body["asset_id"])
	}
	if body["total_chunks"] != float64(2) || body["chunk_size"] != float64(5<<20) || body["size"] != float64(10<<20) {
		t.Errorf("payload 文件规格不符：%v", body)
	}
	for _, forbidden := range []string{"signature=MOCK", "X-Amz", "AccessKey", "SecretKey", "203.0.113.7"} {
		if strings.Contains(row.Payload, forbidden) {
			t.Errorf("事件 payload 含 %q：预签名 URL、OSS 凭据与调用方 IP 都不得进跨服务契约", forbidden)
		}
	}
}

// 清单条数不等于 total_chunks：一条都不许标，状态不动。
func TestCompleteUploadRejectsShortPartListWithoutPartialWrites(t *testing.T) {
	f := newFixture(t)
	f.seedSession(doneUpload, model.SessionStateUploading, 3, 5<<20)

	reply, err := f.completeUpload(t, doneReq(2, "x"))
	mustErrIs(t, "条数门槛", err, model.ErrChunkMismatch)
	if reply != nil {
		t.Fatalf("清单不齐却回了完成应答：%+v", reply)
	}
	f.log.assert(t,
		"sessions.FindOne:"+doneUpload,
		"chunks.ListByUpload:"+doneUpload,
	)
	if s := f.session(t, doneUpload); s.State != model.SessionStateUploading {
		t.Fatalf("state=%d", s.State)
	}
	for _, c := range f.chunks.rows(doneUpload) {
		if c.State != model.ChunkStatePending || c.Etag != "" {
			t.Fatalf("chunk %d 被提前标记：%+v", c.ChunkNo, c)
		}
	}
	if len(f.outbox.rows) != 0 {
		t.Fatalf("清单不齐却写了事件行：%+v", f.outbox.rows[0])
	}
}

// 条数够但多报一条（chunk_no=4 不在清单里）：覆盖门槛拦住。
func TestCompleteUploadRejectsPartNotInManifest(t *testing.T) {
	f := newFixture(t)
	f.seedSession(doneUpload, model.SessionStateUploading, 3, 5<<20)

	req := &rpc.CompleteUploadReq{UploadId: doneUpload, Md5: "x", Parts: []*rpc.ChunkPart{
		{ChunkNo: 1, Etag: "a"}, {ChunkNo: 2, Etag: "b"}, {ChunkNo: 4, Etag: "c"},
	}}
	reply, err := f.completeUpload(t, req)
	mustErrIs(t, "覆盖门槛", err, model.ErrChunkMismatch)
	if reply != nil {
		t.Fatalf("多报了不存在的分片还完成了：%+v", reply)
	}
	// 第 1、2 片在遍历到第 3 片之前已被标记：这正是「校验边遍历边写」的形态。
	f.log.assert(t,
		"sessions.FindOne:"+doneUpload,
		"chunks.ListByUpload:"+doneUpload,
		fmt.Sprintf("chunks.MarkUploaded:%s=1", doneUpload),
		fmt.Sprintf("chunks.MarkUploaded:%s=2", doneUpload),
	)
	rows := f.chunks.rows(doneUpload)
	if rows[0].State != model.ChunkStateUploaded || rows[1].State != model.ChunkStateUploaded {
		t.Fatalf("前两片应已被标记：%+v %+v", rows[0], rows[1])
	}
	if rows[2].State != model.ChunkStatePending {
		t.Fatalf("第 3 片不该被标记：%+v", rows[2])
	}
	if s := f.session(t, doneUpload); s.State != model.SessionStateUploading {
		t.Fatalf("校验失败却推进了状态：%+v", s)
	}
}

// 重复上报同一片（清单 3 条但只覆盖 2 片）：靠「必须有分片没被覆盖」兜住。
func TestCompleteUploadRejectsDuplicatedChunkNo(t *testing.T) {
	f := newFixture(t)
	f.seedSession(doneUpload, model.SessionStateUploading, 3, 5<<20)

	req := &rpc.CompleteUploadReq{UploadId: doneUpload, Md5: "x", Parts: []*rpc.ChunkPart{
		{ChunkNo: 1, Etag: "a"}, {ChunkNo: 1, Etag: "dup"}, {ChunkNo: 2, Etag: "b"},
	}}
	reply, err := f.completeUpload(t, req)
	mustErrIs(t, "重复片号", err, model.ErrChunkMismatch)
	if reply != nil {
		t.Fatalf("重复清单条目也能完成：%+v", reply)
	}
}

// 空 ETag 等同没传：完成不允许凭空标识落库。
func TestCompleteUploadRejectsEmptyEtag(t *testing.T) {
	f := newFixture(t)
	f.seedSession(doneUpload, model.SessionStateUploading, 2, 1<<20)

	req := doneReq(2, "x")
	req.Parts[1].Etag = ""
	reply, err := f.completeUpload(t, req)
	mustErrIs(t, "空 ETag", err, model.ErrChunkMismatch)
	if reply != nil {
		t.Fatalf("空 ETag 也能完成：%+v", reply)
	}
	f.log.assert(t,
		"sessions.FindOne:"+doneUpload,
		"chunks.ListByUpload:"+doneUpload,
		fmt.Sprintf("chunks.MarkUploaded:%s=1", doneUpload),
	)
}

// 终态门槛：已完成/已取消/查不到，都只读一次会话就回绝。
func TestCompleteUploadRejectsTerminalAndUnknownSessions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state int32 // 0 表示不布会话
		want  error
	}{
		{"已完成", model.SessionStateCompleted, model.ErrUploadCompleted},
		{"已取消", model.SessionStateAborted, model.ErrUploadAborted},
		{"已失败", model.SessionStateFailed, nil}, // 失败态允许重试，见下一条
		{"查无会话", 0, model.ErrUploadNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if tc.state != 0 {
				f.seedSession(doneUpload, tc.state, 3, 5<<20)
			}

			reply, err := f.completeUpload(t, doneReq(3, "x"))
			if tc.want != nil {
				mustErrIs(t, "终态门槛", err, tc.want)
				if reply != nil {
					t.Fatalf("%s 的会话却完成了：%+v", tc.name, reply)
				}
				f.log.assert(t, "sessions.FindOne:"+doneUpload)
				return
			}
			if err != nil {
				t.Fatalf("FAILED 会话应可重新完成：%v", err)
			}
			if reply.State != rpc.UploadState_UPLOAD_STATE_COMPLETED {
				t.Fatalf("state=%v", reply.State)
			}
		})
	}
}

// 钉住**当前真实缺陷**：会话从没打过 GetUploadUrl、一片都没上传，
// 只要客户端自报 3 条 ETag 就能直接 COMPLETED 并拿到占位 asset_id，
// 连 media.task.v1 事件也会照样写出一行。
// ETag 全程来自请求，OSS 侧完成调用是占位（repository.go 的 completeMultipartUpload 恒 nil），
// 服务端没有任何一处能证明字节真的到过 OSS（README 已登记）。
func TestCompleteUploadAcceptsFabricatedEtagsWithoutAnyUpload(t *testing.T) {
	f := newFixture(t)
	f.seedSession(doneUpload, model.SessionStateInitialized, 3, 5<<20)

	reply, err := f.completeUpload(t, doneReq(3, "whatever"))
	if err != nil {
		t.Fatalf("当前实现不做上传事实校验（若本用例变红，说明已加校验，请同步 README）：%v", err)
	}
	if reply.State != rpc.UploadState_UPLOAD_STATE_COMPLETED {
		t.Fatalf("state=%v", reply.State)
	}
	stored := f.session(t, doneUpload)
	if stored.State != model.SessionStateCompleted {
		t.Fatalf("库里 state=%d", stored.State)
	}
	if stored.ChunkSize != 5<<20 || stored.Md5 != "whatever" {
		t.Fatalf("stored=%+v", stored)
	}
	for _, c := range f.chunks.rows(doneUpload) {
		if c.State != model.ChunkStateUploaded {
			t.Fatalf("从未上传的 chunk %d 被自报 ETag 标成 UPLOADED：%+v", c.ChunkNo, c)
		}
	}
	// 缺陷的另一半：伪造的完成同样会派生出一行待发布事件（下游会被唤起做一次转码）。
	if len(f.outbox.rows) != 1 {
		t.Fatalf("事件行数=%d，期望 1（当前实现不做上传事实校验）", len(f.outbox.rows))
	}
	// 完成判定一次都不读缓存（与 GetUploadUrl 的快路径相反）：只在末尾写一次。
	if strings.Contains(f.log.snapshot(), "cache.GetSession") {
		t.Fatalf("完成路径读了会话缓存，与本用例结论不符：\n%s", f.log.snapshot())
	}
}

// 逐片标记不在事务里：第 2 片失败时第 1 片已落库，会话停在 UPLOADING。
// 结论是「可重试」而不是「会脏」，所以同一条用例把重试续上跑完。
// 注意：分片侧的逐片写仍在事务外（README 缺口 8），事务只覆盖后面四写。
func TestCompleteUploadMarkFailureLeavesRetryablePartialManifest(t *testing.T) {
	f := newFixture(t)
	f.seedSession(doneUpload, model.SessionStateUploading, 3, 5<<20)
	boom := errors.New("upload_chunk MarkUploaded: deadlock")
	f.chunks.markErr = boom
	f.chunks.markErrCall = 2

	reply, err := f.completeUpload(t, doneReq(3, "x"))
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	if reply != nil {
		t.Fatalf("标记失败却回了完成应答：%+v", reply)
	}
	f.log.assert(t,
		"sessions.FindOne:"+doneUpload,
		"chunks.ListByUpload:"+doneUpload,
		fmt.Sprintf("chunks.MarkUploaded:%s=1", doneUpload),
		fmt.Sprintf("chunks.MarkUploaded:%s=2", doneUpload),
	)
	rows := f.chunks.rows(doneUpload)
	if rows[0].State != model.ChunkStateUploaded || rows[1].State != model.ChunkStatePending {
		t.Fatalf("半截形态不符：chunk1=%+v chunk2=%+v", rows[0], rows[1])
	}
	stored := f.session(t, doneUpload)
	if stored.State != model.SessionStateUploading || stored.AssetId != "" {
		t.Fatalf("失败路径不该回填 asset_id 或推进状态：%+v", stored)
	}
	// 标记阶段失败连事务都不该开，更不该留下事件行。
	if f.conn.transactions != 0 {
		t.Fatalf("分片标记失败却开了 %d 次事务", f.conn.transactions)
	}
	if len(f.outbox.rows) != 0 {
		t.Fatalf("未完成上传却写了事件行：%+v", f.outbox.rows[0])
	}

	// 同一条清单重试：从第 1 片重标（幂等 UPDATE），一路走到完成。
	f.chunks.markErr = nil
	f.log.ops = nil
	retry, err := f.completeUpload(t, doneReq(3, "x"))
	if err != nil {
		t.Fatalf("重试失败：%v", err)
	}
	if retry.State != rpc.UploadState_UPLOAD_STATE_COMPLETED {
		t.Fatalf("重试后 state=%v", retry.State)
	}
	f.log.assert(t,
		append([]string{
			"sessions.FindOne:" + doneUpload,
			"chunks.ListByUpload:" + doneUpload,
			fmt.Sprintf("chunks.MarkUploaded:%s=1", doneUpload),
			fmt.Sprintf("chunks.MarkUploaded:%s=2", doneUpload),
			fmt.Sprintf("chunks.MarkUploaded:%s=3", doneUpload),
		}, append(mediaTaskOps(doneUpload, "x"),
			fmt.Sprintf("cache.SetSession:%s=%d", doneUpload, model.SessionStateCompleted))...)...,
	)
	if len(f.outbox.rows) != 1 {
		t.Fatalf("重试后事件行=%d 条，期望恰好 1 条（第一次失败不该写过事件，重试也不该补写两行）",
			len(f.outbox.rows))
	}
}

// 清单项齐全但库里行数不足（InitUpload 清单写失败留下的孤儿会话）：
// 即使 parts 条数对得上，也必须在 chunks 行数上被拦住。
func TestCompleteUploadRejectsSessionWithoutManifestRows(t *testing.T) {
	f := newFixture(t)
	f.seedSession(doneUpload, model.SessionStateInitialized, 3, 5<<20)
	f.chunks.byUpload = map[string][]*model.UploadChunk{} // 模拟清单写失败的孤儿形态

	reply, err := f.completeUpload(t, doneReq(3, "x"))
	mustErrIs(t, "孤儿会话", err, model.ErrChunkMismatch)
	if reply != nil {
		t.Fatalf("无清单会话也能完成：%+v", reply)
	}
	f.log.assert(t,
		"sessions.FindOne:"+doneUpload,
		"chunks.ListByUpload:"+doneUpload,
	)
}

// md5 是可选的：不传就不写，库里原值保留（不能被空串抹掉）。
// 事件 payload 同样不该出现新 md5，而应带库里旧值之外的空字段（md5 是 omitempty）。
func TestCompleteUploadWithoutMd5KeepsStoredMd5(t *testing.T) {
	f := newFixture(t)
	f.seedSession(doneUpload, model.SessionStateUploading, 2, 1<<20)

	reply, err := f.completeUpload(t, doneReq(2, ""))
	if err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	if reply.Md5 != "stored-md5" {
		t.Fatalf("回参 md5=%q，应回库里原值", reply.Md5)
	}
	if s := f.session(t, doneUpload); s.Md5 != "stored-md5" {
		t.Fatalf("库里 md5 被空串覆盖：%+v", s)
	}
	f.log.assert(t,
		append([]string{
			"sessions.FindOne:" + doneUpload,
			"chunks.ListByUpload:" + doneUpload,
			fmt.Sprintf("chunks.MarkUploaded:%s=1", doneUpload),
			fmt.Sprintf("chunks.MarkUploaded:%s=2", doneUpload),
		}, append(mediaTaskOps(doneUpload, ""),
			fmt.Sprintf("cache.SetSession:%s=%d", doneUpload, model.SessionStateCompleted))...)...,
	)
	// 会话事务写只有两条（asset_id + state），事务仍然只开一次。
	if f.sessions.txWrites != 2 {
		t.Fatalf("带事务会话的写=%d，期望 2（未传 md5 不该发 SetMd5Tx）", f.sessions.txWrites)
	}
	// payload 里的 md5 是 omitempty：不传就整个键都不出现，而不是空串或库里旧值。
	var env eventenvelope.Envelope
	row := f.outbox.only(t)
	if err := json.Unmarshal([]byte(row.Payload), &env); err != nil {
		t.Fatalf("payload 解析失败：%v", err)
	}
	if strings.Contains(string(env.Payload), `"md5"`) {
		t.Errorf("未传 md5 时 payload 不该出现 md5 键：%s", env.Payload)
	}
}

// 回填 md5 失败：事务被标记回滚，事件行一条都不写，缓存不刷。
// 替身不模拟 MySQL 的回滚，所以「分片已被标记」仍是真的（它们在事务之外）。
func TestCompleteUploadSetMd5FailureBlocksStateAdvance(t *testing.T) {
	f := newFixture(t)
	f.seedSession(doneUpload, model.SessionStateUploading, 2, 1<<20)
	boom := errors.New("upload_session SetMd5: too many connection proxies")
	f.sessions.setMd5Err = boom

	reply, err := f.completeUpload(t, doneReq(2, "new-md5"))
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	if reply != nil {
		t.Fatalf("回填失败却回了完成应答：%+v", reply)
	}
	f.log.assert(t,
		"sessions.FindOne:"+doneUpload,
		"chunks.ListByUpload:"+doneUpload,
		fmt.Sprintf("chunks.MarkUploaded:%s=1", doneUpload),
		fmt.Sprintf("chunks.MarkUploaded:%s=2", doneUpload),
		fmt.Sprintf("sessions.SetMd5Tx:%s=new-md5", doneUpload),
	)
	stored := f.session(t, doneUpload)
	if stored.State != model.SessionStateUploading || stored.AssetId != "" {
		t.Fatalf("失败形态应停在 UPLOADING 且不回填 asset_id：%+v", stored)
	}
	// 但两片已经 UPLOADED：分片侧的写已经生效，重试要能接上（另条已测）。
	for _, c := range f.chunks.rows(doneUpload) {
		if c.State != model.ChunkStateUploaded {
			t.Fatalf("chunk %d state=%d", c.ChunkNo, c.State)
		}
	}
	if f.conn.rolledBack != 1 {
		t.Errorf("事务未被标记回滚（rolledBack=%d）：状态与事件会各自落库", f.conn.rolledBack)
	}
	if len(f.outbox.rows) != 0 {
		t.Fatalf("状态推进失败却写了事件行：%+v", f.outbox.rows[0])
	}
	if strings.Contains(f.log.snapshot(), "cache.SetSession") {
		t.Fatalf("事务失败却刷了会话缓存，快路径会拒掉一个仍可重试的会话：\n%s", f.log.snapshot())
	}
}

// 回填 asset_id 失败：md5 那一写在同一事务里已经发过（替身不模拟回滚，所以仍可见），
// 但状态不推进、事件不写、缓存不刷，事务被标记回滚。
func TestCompleteUploadAssetIdFailureBlocksStateAdvance(t *testing.T) {
	f := newFixture(t)
	f.seedSession(doneUpload, model.SessionStateUploading, 1, 1<<20)
	boom := errors.New("upload_session UpdateAssetId: packet sequence")
	f.sessions.setAssetErr = boom

	if _, err := f.completeUpload(t, doneReq(1, "new-md5")); !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	f.log.assert(t,
		"sessions.FindOne:"+doneUpload,
		"chunks.ListByUpload:"+doneUpload,
		fmt.Sprintf("chunks.MarkUploaded:%s=1", doneUpload),
		fmt.Sprintf("sessions.SetMd5Tx:%s=new-md5", doneUpload),
		fmt.Sprintf("sessions.UpdateAssetIdTx:%s=asset-placeholder:%s", doneUpload, doneUpload),
	)
	if strings.Contains(f.log.snapshot(), "sessions.UpdateStateTx") {
		t.Fatalf("asset_id 回填失败却推进了状态：\n%s", f.log.snapshot())
	}
	if len(f.outbox.rows) != 0 {
		t.Fatalf("asset_id 回填失败却写了事件行：%+v", f.outbox.rows[0])
	}
	if f.conn.transactions != 1 || f.conn.rolledBack != 1 {
		t.Fatalf("事务口径=(%d,%d)，期望 (1,1)：四写同事务且失败时标记回滚",
			f.conn.transactions, f.conn.rolledBack)
	}
	// 库里 md5 已是新值：这是替身的边界，真实 MySQL 会把它随事务撤回，
	// 因此这里只钉「状态未推进 + 无事件行」这两条对外可见结论。
	stored := f.session(t, doneUpload)
	if stored.State != model.SessionStateUploading {
		t.Fatalf("state=%d，asset_id 回填失败不该完成会话", stored.State)
	}
}

// 推进 COMPLETED 的 UPDATE 失败：md5/asset_id 的写已在同一事务里发过，
// 事务被标记回滚，事件行不写，缓存不刷（缺口 9 收口前的形态是「三写各发各的」，
// 那时这个会话会停在「asset_id 已回填 + 无事件 + 无缓存」的死区）。
func TestCompleteUploadStateAdvanceFailureLeavesHalfDoneSession(t *testing.T) {
	f := newFixture(t)
	f.seedSession(doneUpload, model.SessionStateUploading, 1, 1<<20)
	boom := errors.New("upload_session UpdateState: lock wait timeout")
	f.sessions.updateStateErr = boom

	reply, err := f.completeUpload(t, doneReq(1, "final-md5"))
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	if reply != nil {
		t.Fatalf("状态推进失败却回了完成应答：%+v", reply)
	}
	f.log.assert(t,
		"sessions.FindOne:"+doneUpload,
		"chunks.ListByUpload:"+doneUpload,
		fmt.Sprintf("chunks.MarkUploaded:%s=1", doneUpload),
		fmt.Sprintf("sessions.SetMd5Tx:%s=final-md5", doneUpload),
		fmt.Sprintf("sessions.UpdateAssetIdTx:%s=asset-placeholder:%s", doneUpload, doneUpload),
		fmt.Sprintf("sessions.UpdateStateTx:%s=%d", doneUpload, model.SessionStateCompleted),
	)
	stored := f.session(t, doneUpload)
	if stored.State != model.SessionStateUploading {
		t.Fatalf("UPDATE 失败库里却成了 %d", stored.State)
	}
	if f.conn.transactions != 1 || f.conn.rolledBack != 1 {
		t.Fatalf("事务口径=(%d,%d)，期望 (1,1)", f.conn.transactions, f.conn.rolledBack)
	}
	if len(f.outbox.rows) != 0 {
		t.Fatalf("状态未推进却写了事件行：%+v（后果：下游为一次未完成的上传开工）", f.outbox.rows[0])
	}
	if _, ok := f.cache.lookup(doneUpload); ok {
		t.Fatal("状态没推进却刷了缓存")
	}
}

// 事件写入本身失败：三次会话写已在那一次事务里发过，事务必须被标记回滚，
// 且缓存不刷。这条钉住「Outbox 是事务的第 4 个写」而不是「成功后补写一行」。
func TestCompleteUploadEventWriteFailureRollsBackAndSkipsCache(t *testing.T) {
	f := newFixture(t)
	f.seedSession(doneUpload, model.SessionStateUploading, 1, 1<<20)
	boom := errors.New("upload_outbox Insert: duplicate entry")
	f.outbox.insErr = boom

	reply, err := f.completeUpload(t, doneReq(1, "md5-x"))
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	if reply != nil {
		t.Fatalf("事件写失败却回了完成应答：%+v", reply)
	}
	f.log.assert(t,
		append([]string{
			"sessions.FindOne:" + doneUpload,
			"chunks.ListByUpload:" + doneUpload,
			fmt.Sprintf("chunks.MarkUploaded:%s=1", doneUpload),
		}, mediaTaskOps(doneUpload, "md5-x")...)...,
	)
	if f.conn.transactions != 1 || f.conn.rolledBack != 1 {
		t.Fatalf("事务口径=(%d,%d)，期望 (1,1)：事件写失败要连着三次会话写一起回滚",
			f.conn.transactions, f.conn.rolledBack)
	}
	if strings.Contains(f.log.snapshot(), "cache.SetSession") {
		t.Fatalf("事件写失败却刷了完成缓存：\n%s", f.log.snapshot())
	}
}

// 事件组装失败（聚合根 ID 为空 → 信封契约不合法）时，一条写都不该发出：
// 会话留在 UPLOADING 可重试，而不是「已 COMPLETED 但永远没有事件」。
func TestCompleteUploadRejectsSessionThatCannotBuildEventBeforeAnyWrite(t *testing.T) {
	f := newFixture(t)
	f.seedSession(doneUpload, model.SessionStateUploading, 1, 1<<20)
	// upload_id 置空是替身层能造出的唯一「聚合根缺失」形态（真库里 upload_id 非空）。
	f.sessions.byID[doneUpload].UploadId = ""

	_, err := f.completeUpload(t, doneReq(1, "md5-x"))
	if err == nil || !strings.Contains(err.Error(), "media.task") {
		t.Fatalf("应按信封契约失败：%v", err)
	}
	if f.conn.transactions != 0 || f.sessions.txWrites != 0 {
		t.Fatalf("组装失败仍发出了写：transactions=%d txWrites=%d", f.conn.transactions, f.sessions.txWrites)
	}
	if len(f.outbox.rows) != 0 {
		t.Fatalf("组装失败却写了事件行：%+v", f.outbox.rows[0])
	}
	if strings.Contains(f.log.snapshot(), "cache.SetSession") {
		t.Fatalf("组装失败却刷了完成缓存：\n%s", f.log.snapshot())
	}
}
