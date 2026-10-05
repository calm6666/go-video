package logic

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"go-video/services/audit/internal/svc"
	"go-video/services/audit/model"
	"go-video/services/audit/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetAuditExportLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetAuditExportLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetAuditExportLogic {
	return &GetAuditExportLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询导出任务，必要时签发短期下载地址
//
// 实现要点（规格 1~5）：
//  1. task_id 与 request_id 二选一，都缺失明确报错；
//  2. 非 succeeded 只回元信息、地址留空：半成品/失败任务的文件根本不存在；
//  3. succeeded 且已过 expire_at → 先落 succeeded→expired 再回空地址，
//     让状态与「对象已不可下载」的事实一致，而不是继续给打不开的链接；
//  4. 地址按 Storage.PresignTTLSeconds 签发，file_hash 由任务元信息带回供下载方自证；
//  5. 每次成功取地址写一条 audit.export.download：本方法是「存证离开本系统」的唯一出口。
//
// fail-closed：留痕写失败时不返回地址。取地址是数据出口，
// 「链接给了而台账查不到」比「链接暂时给不出」严重得多。
//
// 幂等说明：本方法要求 request_id（契约里读接口唯一能区分「两次独立下载」的键）。
// 不带 request_id 时同一秒内的两次下载会派生出同一个 event_id，被 uniq_event_id 合成一条。
func (l *GetAuditExportLogic) GetAuditExport(in *rpc.GetAuditExportReq) (*rpc.GetAuditExportReply, error) {
	if in == nil {
		return nil, model.ErrRequestRequired
	}
	cc := in.GetCtx()
	if err := validateCallContext(cc, true); err != nil {
		return nil, err
	}
	taskID, requestID := in.GetTaskId(), strings.TrimSpace(in.GetRequestId())
	if taskID <= 0 && requestID == "" {
		return nil, model.ErrTaskIDOrRequestIDRequired
	}
	d := buildDeps(l.svcCtx)

	task, err := func() (*model.ExportTask, error) {
		if taskID > 0 {
			return d.exports.FindOne(l.ctx, taskID)
		}
		t, ferr := d.exports.FindByRequestID(l.ctx, requestID)
		if ferr != nil {
			return nil, ferr
		}
		if t == nil {
			return nil, model.ErrTaskNotFound
		}
		return t, nil
	}()
	if err != nil {
		l.Errorf("GetAuditExport 查询失败 task_id=%d request_id=%s err=%v", taskID, requestID, err)
		return nil, err
	}

	switch {
	case task.State == model.ExportStateSucceeded && task.ExpireAt > 0 && d.now() >= task.ExpireAt:
		// 对象已到期：先把状态收敛到 expired，再按元信息返回（地址留空）。
		ok, terr := d.exports.TransitionState(l.ctx, task.TaskID, model.ExportStateSucceeded, model.ExportStateExpired)
		if terr != nil {
			l.Errorf("GetAuditExport 收敛 expired 失败 task_id=%d err=%v", task.TaskID, terr)
			return nil, terr
		}
		if !ok {
			// 并发：别人已经推进过（可能又被重跑成别的状态），以库为准回读。
			fresh, ferr := d.exports.FindOne(l.ctx, task.TaskID)
			if ferr != nil {
				return nil, ferr
			}
			task = fresh
		} else {
			task.State = model.ExportStateExpired
		}
		l.Infof("GetAuditExport 任务已过期 task_id=%d state=%s", task.TaskID, task.State)
		return &rpc.GetAuditExportReply{Task: exportTaskView(task)}, nil

	case task.State != model.ExportStateSucceeded:
		// pending/running/failed/canceled/expired：没有可下载产物，只回元信息。
		return &rpc.GetAuditExportReply{Task: exportTaskView(task)}, nil
	}

	// 走到这里 state=succeeded 且未过期：元信息必须完整，否则产物不可自证。
	if task.Bucket == "" || task.ObjectKey == "" || !isHashHex(task.FileHash) {
		l.Errorf("GetAuditExport 成功任务缺产物自证信息 task_id=%d file_hash_len=%d", task.TaskID, len(task.FileHash))
		return nil, fmt.Errorf("%w: task_id=%d 缺少 bucket/object_key/file_hash，拒绝签发地址",
			model.ErrObjectStorageMissing, task.TaskID)
	}
	url, urlExpireAt, err := d.storage.PresignDownload(l.ctx, task.Bucket, task.ObjectKey, d.opts.PresignTTLSeconds)
	if err != nil {
		l.Errorf("GetAuditExport 签发地址失败 task_id=%d err=%v", task.TaskID, err)
		return nil, err
	}
	if strings.TrimSpace(url) == "" || urlExpireAt <= 0 {
		// 签出一个空链接等于没签发：显式失败，避免调用方拿到 URL 字段非空却打不开。
		return nil, fmt.Errorf("%w: 签名地址为空", model.ErrObjectStorageUnsupported)
	}
	if err := d.appendSelfAudit(l.ctx, cc, selfAuditSpec{
		Action:     actionExportDownload,
		Domain:     selfDomainDataAccess,
		TargetType: targetExportTask,
		TargetID:   strconv.FormatInt(task.TaskID, 10),
		Reason:     "签发导出文件下载地址",
		Before:     map[string]string{"file_hash": task.FileHash, "object_key": task.ObjectKey},
		After:      map[string]string{"url_expire_at": strconv.FormatInt(urlExpireAt, 10)},
	}); err != nil {
		l.Errorf("GetAuditExport 下载地址留痕失败 task_id=%d request_id=%s err=%v", task.TaskID, cc.GetRequestId(), err)
		return nil, fmt.Errorf("%w: %v", model.ErrDownloadTrailUnwritten, err)
	}
	return &rpc.GetAuditExportReply{Task: exportTaskView(task), DownloadUrl: url, UrlExpireAt: urlExpireAt}, nil
}
