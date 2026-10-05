package logic

import (
	"context"
	"errors"
	"testing"

	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	auditrpc "go-video/services/audit/rpc"

	"google.golang.org/grpc"
)

// 本文件只验证网关侧面向 audit 的口径：主体与会话归因、caller_service 固定值、
// 写接口 request_id 门槛、分页归一、proto 枚举互转、投影完整性与错误原样上抛。
// 时间跨度上限、必须带收窄维度、保留期合法性、哈希链重放算法全部由 audit 服务判定，
// 网关不复算（AGENTS.md §5/§9）。打桩方式为内嵌生成的 client 接口 + 覆盖所需方法，
// 不建 gRPC 连接、不碰数据库。

// auditAdminFake 是 audit RPC 的假客户端，只记录入参并返回预置响应。
type auditAdminFake struct {
	auditrpc.AuditClient

	err   error
	calls int

	listReq     *auditrpc.ListAuditEntriesReq
	listReply   *auditrpc.ListAuditEntriesReply
	getReq      *auditrpc.GetAuditEntryReq
	getReply    *auditrpc.GetAuditEntryReply
	verifyReq   *auditrpc.VerifyAuditChainReq
	verifyResp  *auditrpc.VerifyAuditChainReply
	createReq   *auditrpc.CreateAuditExportReq
	createResp  *auditrpc.CreateAuditExportReply
	exportReq   *auditrpc.GetAuditExportReq
	exportResp  *auditrpc.GetAuditExportReply
	runReq      *auditrpc.RunAuditExportTaskReq
	runResp     *auditrpc.RunAuditExportTaskReply
	retentReq   *auditrpc.SaveRetentionPolicyReq
	retentResp  *auditrpc.SaveRetentionPolicyReply
	archiveReq  *auditrpc.ArchiveAuditEntriesReq
	archiveReq2 *auditrpc.ListArchiveBatchesReq
	archiveResp *auditrpc.ArchiveAuditEntriesReply
}

func (f *auditAdminFake) ListAuditEntries(_ context.Context, in *auditrpc.ListAuditEntriesReq,
	_ ...grpc.CallOption) (*auditrpc.ListAuditEntriesReply, error) {
	f.calls++
	f.listReq = in
	return f.listReply, f.err
}

func (f *auditAdminFake) GetAuditEntry(_ context.Context, in *auditrpc.GetAuditEntryReq,
	_ ...grpc.CallOption) (*auditrpc.GetAuditEntryReply, error) {
	f.calls++
	f.getReq = in
	return f.getReply, f.err
}

func (f *auditAdminFake) VerifyAuditChain(_ context.Context, in *auditrpc.VerifyAuditChainReq,
	_ ...grpc.CallOption) (*auditrpc.VerifyAuditChainReply, error) {
	f.calls++
	f.verifyReq = in
	return f.verifyResp, f.err
}

func (f *auditAdminFake) CreateAuditExport(_ context.Context, in *auditrpc.CreateAuditExportReq,
	_ ...grpc.CallOption) (*auditrpc.CreateAuditExportReply, error) {
	f.calls++
	f.createReq = in
	return f.createResp, f.err
}

func (f *auditAdminFake) GetAuditExport(_ context.Context, in *auditrpc.GetAuditExportReq,
	_ ...grpc.CallOption) (*auditrpc.GetAuditExportReply, error) {
	f.calls++
	f.exportReq = in
	return f.exportResp, f.err
}

func (f *auditAdminFake) ListAuditExports(_ context.Context, in *auditrpc.ListAuditExportsReq,
	_ ...grpc.CallOption) (*auditrpc.ListAuditExportsReply, error) {
	f.calls++
	return &auditrpc.ListAuditExportsReply{Items: []*auditrpc.AuditExportTask{{TaskId: 1}}, Total: 1}, f.err
}

func (f *auditAdminFake) RunAuditExportTask(_ context.Context, in *auditrpc.RunAuditExportTaskReq,
	_ ...grpc.CallOption) (*auditrpc.RunAuditExportTaskReply, error) {
	f.calls++
	f.runReq = in
	return f.runResp, f.err
}

func (f *auditAdminFake) ListRetentionPolicies(_ context.Context, in *auditrpc.ListRetentionPoliciesReq,
	_ ...grpc.CallOption) (*auditrpc.ListRetentionPoliciesReply, error) {
	f.calls++
	return &auditrpc.ListRetentionPoliciesReply{
		Items: []*auditrpc.RetentionPolicy{{PolicyId: 7, ActionDomain: "default", Version: 3}},
		Total: 1,
	}, f.err
}

func (f *auditAdminFake) SaveRetentionPolicy(_ context.Context, in *auditrpc.SaveRetentionPolicyReq,
	_ ...grpc.CallOption) (*auditrpc.SaveRetentionPolicyReply, error) {
	f.calls++
	f.retentReq = in
	return f.retentResp, f.err
}

func (f *auditAdminFake) ArchiveAuditEntries(_ context.Context, in *auditrpc.ArchiveAuditEntriesReq,
	_ ...grpc.CallOption) (*auditrpc.ArchiveAuditEntriesReply, error) {
	f.calls++
	f.archiveReq = in
	return f.archiveResp, f.err
}

func (f *auditAdminFake) ListArchiveBatches(_ context.Context, in *auditrpc.ListArchiveBatchesReq,
	_ ...grpc.CallOption) (*auditrpc.ListArchiveBatchesReply, error) {
	f.calls++
	f.archiveReq2 = in
	return &auditrpc.ListArchiveBatchesReply{
		Items: []*auditrpc.ArchiveBatch{{BatchId: 9, ChainKey: "ops_config/2026-09-20", State: "verified"}},
		Total: 1,
	}, f.err
}

// auditCtx 返回一个「已通过 AdminPermission 中间件」的请求上下文，用于验证会话覆盖。
func auditCtx() context.Context {
	return middleware.WithAdmin(context.Background(), middleware.AdminIdentity{AdminID: 77})
}

func TestAuditListEntriesRequiresConfiguredService(t *testing.T) {
	l := NewAuditListEntriesLogic(context.Background(), &svc.ServiceContext{})
	_, err := l.AuditListEntries(&types.ParamAuditListEntries{})
	if err == nil || err.Error() != "audit service not configured" {
		t.Fatalf("未配置 audit 时应返回明确错误，实际: %v", err)
	}
}

func TestAuditListEntriesMapsAndNormalizes(t *testing.T) {
	fake := &auditAdminFake{listReply: &auditrpc.ListAuditEntriesReply{
		Entries: []*auditrpc.AuditEntryView{{
			EntryId: 5001, EventId: "ops-config.uuid", ChainKey: "ops_config/2026-09-20",
			ActorType: auditrpc.ActorType_ACTOR_TYPE_ADMIN, ActorId: 77,
			Action: "ops_config.publish", Result: auditrpc.AuditResult_AUDIT_RESULT_OK,
			SourceApp: auditrpc.SourceApp_SOURCE_APP_ADMIN_WEB, EntryHash: "abc",
		}},
		Total: 1, Pn: 2, Ps: 50, MaxRangeSeconds: 7948800,
	}}
	l := NewAuditListEntriesLogic(auditCtx(), &svc.ServiceContext{Audit: fake})

	resp, err := l.AuditListEntries(&types.ParamAuditListEntries{
		Ctx:          types.AuditCallContext{OperatorId: 1, CallerService: "spoofed", RequestId: "req-1"},
		StartAt:      1700000000,
		EndAt:        1700086400,
		ActorType:    int32(auditrpc.ActorType_ACTOR_TYPE_ADMIN),
		ActionDomain: "ops_config",
		Result:       int32(auditrpc.AuditResult_AUDIT_RESULT_OK),
		SourceApp:    int32(auditrpc.SourceApp_SOURCE_APP_ADMIN_WEB),
		Pn:           2,
		Ps:           5000, // 超过 audit 上限，网关先收敛到 100
	})
	if err != nil {
		t.Fatalf("合法请求不应失败: %v", err)
	}
	in := fake.listReq
	// caller_service 由网关固定，客户端声明值不采信。
	if in.GetCtx().GetCallerService() != "gateway/admin" {
		t.Fatalf("caller_service 应为 gateway/admin，实际 %q", in.GetCtx().GetCallerService())
	}
	// 会话身份覆盖客户端声明的 operator_id，审计读取无法冒名。
	if in.GetCtx().GetOperatorId() != 77 {
		t.Fatalf("operator_id 应取会话身份 77，实际 %d", in.GetCtx().GetOperatorId())
	}
	if in.ActorType != auditrpc.ActorType_ACTOR_TYPE_ADMIN || in.Result != auditrpc.AuditResult_AUDIT_RESULT_OK ||
		in.SourceApp != auditrpc.SourceApp_SOURCE_APP_ADMIN_WEB {
		t.Fatalf("枚举未按 proto 类型转换: %+v", in)
	}
	if in.Ps != 100 || in.Pn != 2 {
		t.Fatalf("分页未归一: pn=%d ps=%d", in.Pn, in.Ps)
	}
	if resp.Data.MaxRangeSeconds != 7948800 || resp.Data.Total != 1 {
		t.Fatalf("服务端回读的约束未透传: %+v", resp.Data)
	}
	item := resp.Data.Entries[0]
	if item.EntryHash != "abc" || item.ChainKey != "ops_config/2026-09-20" ||
		item.ActorType != int32(auditrpc.ActorType_ACTOR_TYPE_ADMIN) ||
		item.Result != int32(auditrpc.AuditResult_AUDIT_RESULT_OK) ||
		item.SourceApp != int32(auditrpc.SourceApp_SOURCE_APP_ADMIN_WEB) {
		t.Fatalf("条目投影丢失哈希/枚举字段: %+v", item)
	}
}

func TestAuditListEntriesRejectsNegativeTime(t *testing.T) {
	fake := &auditAdminFake{}
	l := NewAuditListEntriesLogic(context.Background(), &svc.ServiceContext{Audit: fake})
	_, err := l.AuditListEntries(&types.ParamAuditListEntries{
		Ctx: types.AuditCallContext{OperatorId: 77}, StartAt: -1, EndAt: 10,
	})
	if err == nil {
		t.Fatal("负时间戳应拒绝")
	}
	if fake.calls != 0 {
		t.Fatalf("入参非法时不应调用 audit，实际 %d 次", fake.calls)
	}
}

func TestAuditGetEntryRequiresLocator(t *testing.T) {
	fake := &auditAdminFake{}
	l := NewAuditGetEntryLogic(context.Background(), &svc.ServiceContext{Audit: fake})
	_, err := l.AuditGetEntry(&types.ParamAuditGetEntry{Ctx: types.AuditCallContext{OperatorId: 77}})
	if err == nil {
		t.Fatal("entry_id/event_id 全空时应拒绝，否则会退化成无条件取首条")
	}
	if fake.calls != 0 {
		t.Fatalf("入参非法时不应调用 audit，实际 %d 次", fake.calls)
	}
}

func TestAuditGetEntryKeepsFoundFalseAsSuccess(t *testing.T) {
	fake := &auditAdminFake{getReply: &auditrpc.GetAuditEntryReply{Found: false}}
	l := NewAuditGetEntryLogic(context.Background(), &svc.ServiceContext{Audit: fake})

	resp, err := l.AuditGetEntry(&types.ParamAuditGetEntry{
		Ctx: types.AuditCallContext{OperatorId: 77}, EventId: "audit.missing",
	})
	if err != nil {
		t.Fatalf("未命中不是错误: %v", err)
	}
	if resp.Data.Found {
		t.Fatal("found 应原样透传 false")
	}
	if resp.Data.Entry.EntryId != 0 {
		t.Fatalf("空条目应投影零值: %+v", resp.Data.Entry)
	}
}

func TestAuditVerifyChainRequiresChainKey(t *testing.T) {
	fake := &auditAdminFake{verifyResp: &auditrpc.VerifyAuditChainReply{Intact: false, BrokenReason: "seq_gap"}}
	l := NewAuditVerifyChainLogic(context.Background(), &svc.ServiceContext{Audit: fake})
	if _, err := l.AuditVerifyChain(&types.ParamAuditVerifyChain{
		Ctx: types.AuditCallContext{OperatorId: 77},
	}); err == nil {
		t.Fatal("chain_key 为空时应拒绝")
	}

	resp, err := l.AuditVerifyChain(&types.ParamAuditVerifyChain{
		Ctx: types.AuditCallContext{OperatorId: 77}, ChainKey: "ops_config/2026-09-20",
	})
	if err != nil {
		t.Fatalf("合法请求不应失败: %v", err)
	}
	// 链断了是被审计出来的事实，不是调用失败：必须 200 + intact=false 原样回传。
	if resp.Data.Intact || resp.Data.BrokenReason != "seq_gap" {
		t.Fatalf("校验结论被改写: %+v", resp.Data)
	}
}

func TestAuditWriteRoutesRequireRequestId(t *testing.T) {
	fake := &auditAdminFake{}
	cases := []struct {
		name string
		call func() error
	}{
		{"createExport", func() error {
			l := NewAuditCreateExportLogic(auditCtx(), &svc.ServiceContext{Audit: fake})
			_, err := l.AuditCreateExport(&types.ParamAuditCreateExport{
				Ctx: types.AuditCallContext{OperatorId: 77}, StartAt: 1, EndAt: 2, Reason: "合规核查",
			})
			return err
		}},
		{"runExport", func() error {
			l := NewAuditRunExportLogic(auditCtx(), &svc.ServiceContext{Audit: fake})
			_, err := l.AuditRunExport(&types.ParamAuditRunExport{
				Ctx: types.AuditCallContext{OperatorId: 77}, TaskId: 5,
			})
			return err
		}},
		{"saveRetention", func() error {
			l := NewAuditSaveRetentionLogic(auditCtx(), &svc.ServiceContext{Audit: fake})
			_, err := l.AuditSaveRetention(&types.ParamAuditSaveRetention{
				Ctx: types.AuditCallContext{OperatorId: 77}, ActionDomain: "ops_config", Remark: "延长留存",
			})
			return err
		}},
		{"archive", func() error {
			l := NewAuditArchiveLogic(auditCtx(), &svc.ServiceContext{Audit: fake})
			_, err := l.AuditArchive(&types.ParamAuditArchive{
				Ctx: types.AuditCallContext{OperatorId: 77}, ChainKey: "ops_config/2026-09-20",
			})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake.calls = 0
			if err := tc.call(); err == nil {
				t.Fatal("写接口缺 ctx.request_id 时应拒绝")
			}
			if fake.calls != 0 {
				t.Fatalf("入参非法时不应调用 audit，实际 %d 次", fake.calls)
			}
		})
	}
}

func TestAuditCreateExportRequiresReason(t *testing.T) {
	fake := &auditAdminFake{createResp: &auditrpc.CreateAuditExportReply{
		Task: &auditrpc.AuditExportTask{TaskId: 12, State: "pending", Format: "csv"}, Reused: true,
	}}
	l := NewAuditCreateExportLogic(auditCtx(), &svc.ServiceContext{Audit: fake})
	if _, err := l.AuditCreateExport(&types.ParamAuditCreateExport{
		Ctx: types.AuditCallContext{OperatorId: 77, RequestId: "req-1"}, StartAt: 1, EndAt: 2,
	}); err == nil {
		t.Fatal("导出动机 reason 为空时应拒绝")
	}

	resp, err := l.AuditCreateExport(&types.ParamAuditCreateExport{
		Ctx: types.AuditCallContext{OperatorId: 77, RequestId: "req-1"}, StartAt: 1, EndAt: 2, Reason: "年度合规核查",
	})
	if err != nil {
		t.Fatalf("合法请求不应失败: %v", err)
	}
	if fake.createReq.GetCtx().GetRequestId() != "req-1" {
		t.Fatalf("request_id 未透传: %s", fake.createReq.GetCtx().GetRequestId())
	}
	if !resp.Data.Reused || resp.Data.Task.TaskId != 12 || resp.Data.Task.State != "pending" {
		t.Fatalf("reused/task 投影错误: %+v", resp.Data)
	}
}

func TestAuditGetExportRequiresLocatorAndReturnsUnsignedUrl(t *testing.T) {
	fake := &auditAdminFake{exportResp: &auditrpc.GetAuditExportReply{
		Task: &auditrpc.AuditExportTask{TaskId: 12, State: "succeeded"}, UrlExpireAt: 1700000900,
	}}
	l := NewAuditGetExportLogic(context.Background(), &svc.ServiceContext{Audit: fake})
	if _, err := l.AuditGetExport(&types.ParamAuditGetExport{
		Ctx: types.AuditCallContext{OperatorId: 77},
	}); err == nil {
		t.Fatal("task_id/request_id 全空时应拒绝")
	}

	resp, err := l.AuditGetExport(&types.ParamAuditGetExport{
		Ctx: types.AuditCallContext{OperatorId: 77}, TaskId: 12,
	})
	if err != nil {
		t.Fatalf("合法请求不应失败: %v", err)
	}
	// 任务未就绪时 audit 返回空地址，网关不伪造也不缓存（TTL 固定 0）。
	if resp.Data.DownloadUrl != "" || resp.TTL != 0 || resp.Data.UrlExpireAt != 1700000900 {
		t.Fatalf("下载地址口径错误: %+v ttl=%d", resp.Data, resp.TTL)
	}
}

func TestAuditSaveRetentionValidatesVersion(t *testing.T) {
	fake := &auditAdminFake{retentResp: &auditrpc.SaveRetentionPolicyReply{
		Policy: &auditrpc.RetentionPolicy{PolicyId: 7, ActionDomain: "ops_config", Version: 4},
	}}
	l := NewAuditSaveRetentionLogic(auditCtx(), &svc.ServiceContext{Audit: fake})
	if _, err := l.AuditSaveRetention(&types.ParamAuditSaveRetention{
		Ctx: types.AuditCallContext{OperatorId: 77, RequestId: "r"}, ActionDomain: "ops_config",
		Remark: "x", ExpectVersion: -1,
	}); !errors.Is(err, errAuditExpectVersionInvalid) {
		t.Fatalf("负版本号应拒绝，实际: %v", err)
	}

	resp, err := l.AuditSaveRetention(&types.ParamAuditSaveRetention{
		Ctx: types.AuditCallContext{OperatorId: 77, RequestId: "r"}, ActionDomain: "ops_config",
		HotDays: 365, ArchiveAfterDays: 730, DeleteAfterDays: 0, ExpectVersion: 3, Remark: "永久保留",
	})
	if err != nil {
		t.Fatalf("合法请求不应失败: %v", err)
	}
	if fake.retentReq.ExpectVersion != 3 || fake.retentReq.DeleteAfterDays != 0 {
		t.Fatalf("乐观锁/永久保留语义被改写: %+v", fake.retentReq)
	}
	if resp.Data.Policy.Version != 4 {
		t.Fatalf("新版本号未回传: %+v", resp.Data.Policy)
	}
}

func TestAuditArchivePassesPurgeHotAndReused(t *testing.T) {
	fake := &auditAdminFake{archiveResp: &auditrpc.ArchiveAuditEntriesReply{
		Batch: &auditrpc.ArchiveBatch{BatchId: 3, ChainKey: "admin/2026-09-01", State: "verified", ManifestHash: "m"},
	}}
	l := NewAuditArchiveLogic(auditCtx(), &svc.ServiceContext{Audit: fake})
	resp, err := l.AuditArchive(&types.ParamAuditArchive{
		Ctx:      types.AuditCallContext{OperatorId: 77, RequestId: "r-1"},
		ChainKey: "admin/2026-09-01", ToSeq: 1000, PurgeHot: true,
	})
	if err != nil {
		t.Fatalf("合法请求不应失败: %v", err)
	}
	if !fake.archiveReq.GetPurgeHot() || fake.archiveReq.GetToSeq() != 1000 {
		t.Fatalf("归档参数未透传: %+v", fake.archiveReq)
	}
	if resp.Data.Batch.ManifestHash != "m" || resp.Data.Reused {
		t.Fatalf("归档批次投影错误: %+v", resp.Data)
	}
}

func TestAuditListReadRoutesProjectCollections(t *testing.T) {
	fake := &auditAdminFake{}
	entriesLogic := NewAuditListExportsLogic(context.Background(), &svc.ServiceContext{Audit: fake})
	exportsResp, err := entriesLogic.AuditListExports(&types.ParamAuditListExports{
		Ctx: types.AuditCallContext{OperatorId: 77}, State: "succeeded",
	})
	if err != nil || len(exportsResp.Data.Items) != 1 || exportsResp.Data.Total != 1 {
		t.Fatalf("导出列表投影错误: resp=%+v err=%v", exportsResp, err)
	}

	retLogic := NewAuditListRetentionLogic(context.Background(), &svc.ServiceContext{Audit: fake})
	retResp, err := retLogic.AuditListRetention(&types.ParamAuditListRetention{
		Ctx: types.AuditCallContext{OperatorId: 77},
	})
	if err != nil || retResp.Data.Items[0].ActionDomain != "default" || retResp.Data.Items[0].Version != 3 {
		t.Fatalf("保留策略投影错误: resp=%+v err=%v", retResp, err)
	}

	archLogic := NewAuditListArchivesLogic(context.Background(), &svc.ServiceContext{Audit: fake})
	archResp, err := archLogic.AuditListArchives(&types.ParamAuditListArchives{
		Ctx: types.AuditCallContext{OperatorId: 77}, ChainKey: "ops_config/2026-09-20",
	})
	if err != nil || archResp.Data.Items[0].State != "verified" || archResp.Data.Items[0].BatchId != 9 {
		t.Fatalf("归档批次投影错误: resp=%+v err=%v", archResp, err)
	}
	if fake.archiveReq2.GetChainKey() != "ops_config/2026-09-20" {
		t.Fatalf("chain_key 过滤未透传: %+v", fake.archiveReq2)
	}
}

func TestAuditLogicPropagatesDownstreamError(t *testing.T) {
	sentinel := errors.New("audit: time range exceeds max 92 days")
	fake := &auditAdminFake{err: sentinel}
	cases := []func() error{
		func() error {
			l := NewAuditListEntriesLogic(context.Background(), &svc.ServiceContext{Audit: fake})
			_, e := l.AuditListEntries(&types.ParamAuditListEntries{
				Ctx: types.AuditCallContext{OperatorId: 77}, StartAt: 1, EndAt: 2, Action: "admin.login",
			})
			return e
		},
		func() error {
			l := NewAuditGetEntryLogic(context.Background(), &svc.ServiceContext{Audit: fake})
			_, e := l.AuditGetEntry(&types.ParamAuditGetEntry{Ctx: types.AuditCallContext{OperatorId: 77}, EntryId: 1})
			return e
		},
		func() error {
			l := NewAuditVerifyChainLogic(context.Background(), &svc.ServiceContext{Audit: fake})
			_, e := l.AuditVerifyChain(&types.ParamAuditVerifyChain{
				Ctx: types.AuditCallContext{OperatorId: 77}, ChainKey: "admin/2026-09-01",
			})
			return e
		},
		func() error {
			l := NewAuditListExportsLogic(context.Background(), &svc.ServiceContext{Audit: fake})
			_, e := l.AuditListExports(&types.ParamAuditListExports{Ctx: types.AuditCallContext{OperatorId: 77}})
			return e
		},
		func() error {
			l := NewAuditListRetentionLogic(context.Background(), &svc.ServiceContext{Audit: fake})
			_, e := l.AuditListRetention(&types.ParamAuditListRetention{Ctx: types.AuditCallContext{OperatorId: 77}})
			return e
		},
		func() error {
			l := NewAuditListArchivesLogic(context.Background(), &svc.ServiceContext{Audit: fake})
			_, e := l.AuditListArchives(&types.ParamAuditListArchives{Ctx: types.AuditCallContext{OperatorId: 77}})
			return e
		},
	}
	for i, call := range cases {
		if err := call(); !errors.Is(err, sentinel) {
			t.Fatalf("case %d: 下游错误应原样上抛，实际: %v", i, err)
		}
	}
}

// 未配置 operator_id（既无会话也未声明）时，读路由也必须拒绝：audit 要求所有方法带主体。
func TestAuditReadRejectsMissingOperator(t *testing.T) {
	fake := &auditAdminFake{}
	l := NewAuditGetEntryLogic(context.Background(), &svc.ServiceContext{Audit: fake})
	if _, err := l.AuditGetEntry(&types.ParamAuditGetEntry{EntryId: 1}); err == nil {
		t.Fatal("缺少 ctx.operator_id 时应拒绝")
	}
	if fake.calls != 0 {
		t.Fatalf("入参非法时不应调用 audit，实际 %d 次", fake.calls)
	}
}
