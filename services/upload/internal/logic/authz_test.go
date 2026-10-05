package logic

// authz_test.go 钉一条**当前真实行为**：本服务写路径的授权凭证只有 upload_id。
//
// rpc/upload.proto 里 AbortUploadReq / UploadStatusReq / CompleteUploadReq 只有
// upload_id + ip 两个身份相关字段，没有 mid、没有 token；logic 也只是把 upload_id
// 透传给仓库。因此：
//   - 任何拿到 upload_id 的调用方都能读走他人会话的文件名/object_key/md5，
//     也能取消或「完成」他人会话；
//   - 服务侧唯一的兜底是 upload_id 本身够随机（纳秒 + 12 字节随机，repository.go:389），
//     属于「不可猜测」而不是「已鉴权」。
// README.md 已登记该缺口，并说明为什么本期不改成 fail-closed（网关侧已有会话鉴权，
// 但服务面缺少可信 mid 的传递字段，属跨服务契约改动）。

import (
	"fmt"
	"strings"
	"testing"

	"go-video/services/upload/model"
	rpc "go-video/services/upload/rpc"
)

func TestUploadWritePathsAuthorizeByUploadIdOnly(t *testing.T) {
	f := newFixture(t)
	f.seedSession("victim-session", model.SessionStateUploading, 2, 5<<20)

	// 1) 读侧：请求里没有任何身份字段，照样读出他人会话的全部元数据。
	status, err := f.getUploadStatus(t, &rpc.UploadStatusReq{UploadId: "victim-session"})
	if err != nil {
		t.Fatalf("GetUploadStatus: %v", err)
	}
	if status.TotalChunks != 2 || status.Size != 10<<20 {
		t.Fatalf("读到的不是他人会话：%+v", status)
	}
	// object_key 里带着他人 mid 与文件名，属于可外带的信息。
	if !strings.Contains(f.session(t, "victim-session").ObjectKey, "uploads/1001/") {
		t.Fatal("布景的 object_key 不含 mid，本用例前提不成立")
	}

	// 2) 写侧：同一身份空缺的取消请求生效。
	if _, err := f.abortUpload(t, &rpc.AbortUploadReq{UploadId: "victim-session"}); err != nil {
		t.Fatalf("AbortUpload: %v", err)
	}
	after := f.session(t, "victim-session")
	if after.State != model.SessionStateAborted {
		t.Fatalf("state=%d，他人会话没被取消（若已改为拒绝，请同步 README）", after.State)
	}
	// 归属字段一个都没动：服务从没把 mid 当作判定输入。
	if after.Mid != 1001 {
		t.Fatalf("mid=%d", after.Mid)
	}
	if strings.Contains(f.log.snapshot(), "mid") {
		t.Fatalf("出现了按 mid 的归属查询，本用例前提不再成立：\n%s", f.log.snapshot())
	}

	// 3) 另一条写路径：换一个「攻击者」会话，凭自报清单直接完成他人会话。
	f2 := newFixture(t)
	f2.seedSession("another-victim", model.SessionStateUploading, 1, 5<<20)
	reply, err := f2.completeUpload(t, &rpc.CompleteUploadReq{
		UploadId: "another-victim", Parts: []*rpc.ChunkPart{{ChunkNo: 1, Etag: "mine"}},
	})
	if err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	if reply.AssetId != fmt.Sprintf("asset-placeholder:%s", "another-victim") {
		t.Fatalf("asset_id=%q", reply.AssetId)
	}
	if f2.session(t, "another-victim").State != model.SessionStateCompleted {
		t.Fatal("他人会话没被置完成")
	}
}
