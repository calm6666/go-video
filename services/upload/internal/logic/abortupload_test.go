package logic

// abortupload_test.go 钉 AbortUpload 的五条契约：
//  1. 只有 COMPLETED 会被拒（清单必须留作审计证据），ABORTED/FAILED 允许再取消；
//  2. 清理次序固定：OSS 侧（占位）→ 删分片清单 → 会话置 ABORTED → 刷缓存；
//  3. 删清单失败时状态与缓存都不动，库里保持完整可重试；
//  4. 取消不产出任何事件，也不开事务：取消对下游完全静默（README 缺口 17）；
//  5. 钉住**当前真实缺陷**：置状态失败会留下「清单已删、会话还活着」的形态，
//     之后 GetUploadUrl 照样签 URL，CompleteUpload 必然 ErrChunkMismatch。

import (
	"errors"
	"fmt"
	"testing"

	"go-video/services/upload/model"
	rpc "go-video/services/upload/rpc"
)

const killUpload = "u-abort"

func killReq() *rpc.AbortUploadReq {
	return &rpc.AbortUploadReq{UploadId: killUpload, Ip: "203.0.113.7"}
}

func TestAbortUploadGuardRejectsWithoutTouchingDependencies(t *testing.T) {
	f := newFixture(t)
	f.seedSession(killUpload, model.SessionStateUploading, 3, 5<<20)

	reply, err := f.abortUpload(t, &rpc.AbortUploadReq{})
	mustErrIs(t, "门槛", err, model.ErrInvalidUploadID)
	if reply != nil {
		t.Fatalf("门槛失败却回了应答：%+v", reply)
	}
	f.log.assertEmpty(t)
	if f.chunks.count(killUpload) != 3 {
		t.Fatal("门槛失败却删了清单")
	}
}

func TestAbortUploadDeletesManifestThenMarksAborted(t *testing.T) {
	f := newFixture(t)
	f.seedSession(killUpload, model.SessionStateUploading, 3, 5<<20)
	f.cache.warm(killUpload, model.SessionStateUploading)
	before := f.session(t, killUpload).Mtime

	reply, err := f.abortUpload(t, killReq())
	if err != nil {
		t.Fatalf("AbortUpload: %v", err)
	}
	if reply == nil {
		t.Fatal("成功必须回 EmptyReply 而不是 nil")
	}
	f.log.assert(t,
		"sessions.FindOne:"+killUpload,
		"chunks.DeleteByUpload:"+killUpload,
		fmt.Sprintf("sessions.UpdateState:%s=%d", killUpload, model.SessionStateAborted),
		fmt.Sprintf("cache.SetSession:%s=%d", killUpload, model.SessionStateAborted),
	)
	if n := f.chunks.count(killUpload); n != 0 {
		t.Fatalf("清单还剩 %d 行，取消应删干净", n)
	}
	stored := f.session(t, killUpload)
	if stored.State != model.SessionStateAborted {
		t.Fatalf("state=%d", stored.State)
	}
	if stored.Mtime <= before {
		t.Fatalf("mtime=%d 未推进（原 %d）", stored.Mtime, before)
	}
	// 取消不擦元数据：md5/object_key/size 要留给对账和留痕。
	if stored.Md5 != "stored-md5" || stored.ObjectKey == "" || stored.Size == 0 {
		t.Fatalf("取消把会话元数据也改了：%+v", stored)
	}
	// 取消也不产事件：upload_outbox 留空，连事务都不该开（README 缺口 17）。
	if len(f.outbox.rows) != 0 {
		t.Fatalf("取消写了 %d 行事件：%+v", len(f.outbox.rows), f.outbox.rows)
	}
	if f.conn.transactions != 0 {
		t.Fatalf("取消开了 %d 次事务，与「只有 CompleteUpload 走事务」不符", f.conn.transactions)
	}
	if s, ok := f.cache.lookup(killUpload); !ok || s != model.SessionStateAborted {
		t.Fatalf("会话缓存 state=%d hit=%v", s, ok)
	}
}

// 唯一被拒的终态是 COMPLETED：已完成的对象和清单必须可追溯。
func TestAbortUploadRejectsCompletedSessionAndKeepsEvidence(t *testing.T) {
	f := newFixture(t)
	f.seedSession(killUpload, model.SessionStateCompleted, 3, 5<<20)
	f.sessions.byID[killUpload].AssetId = "asset-placeholder:" + killUpload

	reply, err := f.abortUpload(t, killReq())
	mustErrIs(t, "终态门槛", err, model.ErrUploadCompleted)
	if reply != nil {
		t.Fatalf("已完成会话却被取消了：%+v", reply)
	}
	f.log.assert(t, "sessions.FindOne:"+killUpload)
	if n := f.chunks.count(killUpload); n != 3 {
		t.Fatalf("拒了还删清单，剩 %d 行", n)
	}
	if s := f.session(t, killUpload); s.State != model.SessionStateCompleted {
		t.Fatalf("state=%d", s.State)
	}
}

// 钉住**当前真实行为**：重复取消不报错，第二次会整段重跑清理。
// 应答是 EmptyReply，调用方无法区分「第一次取消」和「第 N 次取消」。
func TestAbortUploadRepeatedCancelReRunsCleanup(t *testing.T) {
	f := newFixture(t)
	f.seedSession(killUpload, model.SessionStateAborted, 2, 1<<20)
	f.chunks.byUpload = map[string][]*model.UploadChunk{} // 已取消的会话本来就没有清单

	if _, err := f.abortUpload(t, killReq()); err != nil {
		t.Fatalf("当前实现允许重复取消（若本用例变红，说明已加终态门槛，请同步 README）：%v", err)
	}
	f.log.assert(t,
		"sessions.FindOne:"+killUpload,
		"chunks.DeleteByUpload:"+killUpload,
		fmt.Sprintf("sessions.UpdateState:%s=%d", killUpload, model.SessionStateAborted),
		fmt.Sprintf("cache.SetSession:%s=%d", killUpload, model.SessionStateAborted),
	)
}

// FAILED 也允许取消（清理残留分片）：状态机里 5→4 这条边是通行为。
func TestAbortUploadAllowsFailedSession(t *testing.T) {
	f := newFixture(t)
	f.seedSession(killUpload, model.SessionStateFailed, 2, 1<<20)

	if _, err := f.abortUpload(t, killReq()); err != nil {
		t.Fatalf("FAILED 会话应可取消：%v", err)
	}
	if s := f.session(t, killUpload); s.State != model.SessionStateAborted {
		t.Fatalf("state=%d", s.State)
	}
}

func TestAbortUploadUnknownSessionDoesNotDeleteAnything(t *testing.T) {
	f := newFixture(t)
	f.seedSession("other-session", model.SessionStateUploading, 2, 1<<20)

	reply, err := f.abortUpload(t, killReq())
	mustErrIs(t, "查无会话", err, model.ErrUploadNotFound)
	if reply != nil {
		t.Fatalf("无会话也回了成功：%+v", reply)
	}
	f.log.assert(t, "sessions.FindOne:"+killUpload)
	if f.chunks.count("other-session") != 2 {
		t.Fatal("误删了别的会话清单")
	}
}

func TestAbortUploadSessionReadFailurePropagates(t *testing.T) {
	f := newFixture(t)
	f.seedSession(killUpload, model.SessionStateUploading, 2, 1<<20)
	boom := errors.New("upload_session FindOne: connection refused")
	f.sessions.findErr = boom

	if _, err := f.abortUpload(t, killReq()); !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	f.log.assert(t, "sessions.FindOne:"+killUpload)
	if f.chunks.count(killUpload) != 2 {
		t.Fatal("读失败却删了清单")
	}
}

// 删清单失败：一切都还没发生（状态、缓存都不动），可以直接重试。
func TestAbortUploadManifestDeleteFailureKeepsEverything(t *testing.T) {
	f := newFixture(t)
	f.seedSession(killUpload, model.SessionStateUploading, 3, 5<<20)
	boom := errors.New("upload_chunk DeleteByUpload: lock wait timeout")
	f.chunks.deleteErr = boom

	reply, err := f.abortUpload(t, killReq())
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	if reply != nil {
		t.Fatalf("清理失败却回了成功：%+v", reply)
	}
	f.log.assert(t,
		"sessions.FindOne:"+killUpload,
		"chunks.DeleteByUpload:"+killUpload,
	)
	if f.chunks.count(killUpload) != 3 {
		t.Fatal("报错了清单反而少了")
	}
	if s := f.session(t, killUpload); s.State != model.SessionStateUploading {
		t.Fatalf("state=%d，清理没成功就推进了状态", s.State)
	}
	if _, ok := f.cache.lookup(killUpload); ok {
		t.Fatal("取消失败却刷了缓存，之后 60 秒内会按 ABORTED 误判")
	}
}

// 钉住**当前真实缺陷**：清单已删、置 ABORTED 失败 → 会话仍停在 UPLOADING。
// 于是这个会话既签得出新分片 URL，又永远完不成（清单行数对不上 total_chunks）。
func TestAbortUploadStateFailureLeavesLiveSessionWithoutManifest(t *testing.T) {
	f := newFixture(t)
	f.seedSession(killUpload, model.SessionStateUploading, 3, 5<<20)
	boom := errors.New("upload_session UpdateState: server has gone away")
	f.sessions.updateStateErr = boom

	if _, err := f.abortUpload(t, killReq()); !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	f.log.assert(t,
		"sessions.FindOne:"+killUpload,
		"chunks.DeleteByUpload:"+killUpload,
		fmt.Sprintf("sessions.UpdateState:%s=%d", killUpload, model.SessionStateAborted),
	)
	stored := f.session(t, killUpload)
	if stored.State != model.SessionStateUploading {
		t.Fatalf("state=%d", stored.State)
	}
	if f.chunks.count(killUpload) != 0 {
		t.Fatal("清单其实没删掉，本用例前提不成立")
	}

	// 后果 1：还能领 URL（状态仍是 UPLOADING，快路径和库判定都放行）。
	f.sessions.updateStateErr = nil
	f.log.ops = nil
	if _, err := f.getUploadUrl(t, &rpc.GetUrlReq{UploadId: killUpload, ChunkNo: 1}); err != nil {
		t.Fatalf("当前实现允许给无清单会话签 URL：%v", err)
	}
	// 后果 2：永远完不成。
	f.log.ops = nil
	reply, err := f.completeUpload(t, &rpc.CompleteUploadReq{
		UploadId: killUpload, Parts: etags(3, 1), Md5: "x",
	})
	mustErrIs(t, "无清单完成", err, model.ErrChunkMismatch)
	if reply != nil {
		t.Fatalf("无清单会话竟然完成：%+v", reply)
	}
}
