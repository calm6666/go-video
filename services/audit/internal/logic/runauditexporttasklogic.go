package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"go-video/services/audit/internal/svc"
	"go-video/services/audit/model"
	"go-video/services/audit/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RunAuditExportTaskLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRunAuditExportTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RunAuditExportTaskLogic {
	return &RunAuditExportTaskLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 推进一个导出任务（由 services/cron 或人工触发，本服务不内置 worker）
//
// 实现要点（规格 1~6）：
//  1. 用 Claim 的条件更新拿到推进权（代替分布式锁），未拿到就安静结束而不报错；
//  2. 按 entry_id 升序游标分批读热表（ScanAfter），每批写完立刻 AddProgress 推游标；
//  3. 累计行数超 Export.MaxRowsPerTask → Finish(failed) 并说明要按时间分片；
//  4. 全部批次写完后 Commit 对象 → 校验回传摘要 → Finish(succeeded, ObjectRef)；
//  5. 对象存储不可用（未配置或本构建未接客户端）→ Finish(failed)：
//     绝不允许「库里记成功、桶里没有文件」；
//  6. 收敛后写一条 audit.export.finish。
//
// 关于「running 任务续传」：规格写的是 Claim(pending|running → running)，但真正从
// last_seq 续传需要一个「可续写的对象」（multipart upload id 之类），
// 而 objectStore 接口与 audit_export_task 表都没有这个位置（README 契约缺口）。
// 已有游标的 running 任务因此显式返回 ErrExportAlreadyClaimed，
// 而不是把区间后半段写成一个看起来完整的新文件。
func (l *RunAuditExportTaskLogic) RunAuditExportTask(in *rpc.RunAuditExportTaskReq) (*rpc.RunAuditExportTaskReply, error) {
	if in == nil {
		return nil, model.ErrRequestRequired
	}
	cc := in.GetCtx()
	if err := validateCallContext(cc, true); err != nil {
		return nil, err
	}
	taskID := in.GetTaskId()
	if taskID <= 0 {
		return nil, fmt.Errorf("%w: task_id 必填", model.ErrRequestRequired)
	}
	d := buildDeps(l.svcCtx)

	task, err := d.exports.FindOne(l.ctx, taskID)
	if err != nil {
		l.Errorf("RunAuditExportTask 读任务失败 task_id=%d err=%v", taskID, err)
		return nil, err
	}
	owned, err := l.acquire(d, task)
	if err != nil {
		l.Errorf("RunAuditExportTask 拿推进权失败 task_id=%d state=%s err=%v", taskID, task.State, err)
		return nil, err
	}
	if !owned {
		// 被别的推进者持有：不报错也不重复写文件，cron 下一轮再来。
		l.Infof("RunAuditExportTask 任务已被占用 task_id=%d state=%s", taskID, task.State)
		return &rpc.RunAuditExportTaskReply{Task: exportTaskView(task)}, nil
	}
	return l.drive(d, cc, task, in.GetBatchRows())
}

// acquire 通过条件更新把任务推进到 running 并回报是否真正拿到了推进权。
// 返回 (false, nil) 表示「状态合法但被别人先了一步」，调用方安静结束。
func (l *RunAuditExportTaskLogic) acquire(d deps, task *model.ExportTask) (bool, error) {
	switch task.State {
	case model.ExportStatePending:
		ok, err := d.exports.Claim(l.ctx, task.TaskID, model.ExportStatePending)
		if err != nil || !ok {
			return false, err
		}
		task.State = model.ExportStateRunning
		return true, nil
	case model.ExportStateRunning:
		if task.LastSeq != 0 || task.RowCount != 0 {
			return false, fmt.Errorf("%w: task_id=%d 已在 last_seq=%d row_count=%d 处中断，"+
				"本期没有可续写的对象写入器，需人工以新 request_id 重新提交",
				model.ErrExportAlreadyClaimed, task.TaskID, task.LastSeq, task.RowCount)
		}
		// 刚置为 running、一行都没写出：接管是安全的，对象键确定且整文件重写。
		ok, err := d.exports.Claim(l.ctx, task.TaskID, model.ExportStateRunning)
		if err != nil || !ok {
			return false, err
		}
		return true, nil
	default:
		return false, fmt.Errorf("%w: task_id=%d state=%s 已结束，不能再推进",
			model.ErrTaskBadTransition, task.TaskID, task.State)
	}
}

// drive 是拿到推进权之后的主流程。
func (l *RunAuditExportTaskLogic) drive(d deps, cc *rpc.CallContext, task *model.ExportTask,
	batchRows int32) (*rpc.RunAuditExportTaskReply, error) {
	startRows := task.RowCount

	// fail 把任务收敛到 failed 并原样回传原因：任何一步做不到都不能留 running。
	fail := func(what string, cause error) (*rpc.RunAuditExportTaskReply, error) {
		updated, ferr := d.exports.Finish(l.ctx, task.TaskID, model.ExportStateFailed,
			model.ObjectRef{TotalRows: startRows}, clampErrMsg(what+": "+cause.Error()))
		if ferr != nil {
			l.Errorf("RunAuditExportTask 置 failed 失败 task_id=%d err=%v（原始失败：%v）", task.TaskID, ferr, cause)
			return nil, errors.Join(cause, ferr)
		}
		if !updated {
			l.Errorf("RunAuditExportTask 任务已不在 running，不覆盖终态 task_id=%d", task.TaskID)
		}
		return nil, cause
	}

	filter, err := parseExportFilter(task.FilterJSON)
	if err != nil {
		return fail("导出条件快照不可解析", err)
	}
	bucket, err := d.bucket()
	if err != nil {
		return fail("对象存储未就绪", err)
	}
	objectKey := exportObjectKey(task.RequestID, task.Format)
	writer, err := d.storage.CreateObject(l.ctx, bucket, objectKey)
	if err != nil {
		return fail("创建导出对象失败", err)
	}
	if writer == nil {
		return fail("对象写入器为空", model.ErrObjectStorageUnsupported)
	}
	abort := func() {
		if aerr := writer.Abort(); aerr != nil {
			l.Errorf("RunAuditExportTask Abort 失败 task_id=%d err=%v", task.TaskID, aerr)
		}
	}
	if task.Format != "json" {
		header, herr := csvHeaderBytes()
		if herr != nil {
			abort()
			return fail("渲染 CSV 表头失败", herr)
		}
		if _, werr := writer.Write(header); werr != nil {
			abort()
			return fail("写导出对象表头失败", werr)
		}
	}

	batch := d.scanLimit(batchRows)
	scanFilter := filter.entryFilter(batch)
	total := startRows
	cursor := task.LastSeq
	for {
		rows, serr := d.entries.ScanAfter(l.ctx, scanFilter, cursor, batch)
		if serr != nil {
			abort()
			return fail("扫描热表失败", serr)
		}
		if len(rows) == 0 {
			break
		}
		total += int64(len(rows))
		if max := d.opts.ExportMaxRows; max > 0 && total > max {
			abort()
			return fail(fmt.Sprintf("导出行数超过单任务上限 %d，请按时间范围分片重新提交", max),
				model.ErrExportRowLimit)
		}
		if _, werr := writeExportRows(writer, task.Format, rows); werr != nil {
			abort()
			return fail("写导出对象失败", werr)
		}
		cursor = rows[len(rows)-1].EntryID
		if perr := d.exports.AddProgress(l.ctx, task.TaskID, int64(len(rows)), cursor); perr != nil {
			abort()
			return fail("更新导出游标失败", perr)
		}
		if int32(len(rows)) < batch {
			break
		}
	}
	loc, err := writer.Commit()
	if err != nil {
		abort()
		return fail("提交导出对象失败", err)
	}
	// 对象侧自证信息不齐就当作没落地：Finish(succeeded) 会把 file_hash 写成空值，
	// 之后下载方再也无法验证文件有没有被替换。
	if loc.ObjectKey != objectKey || !isHashHex(loc.SHA256Hex) {
		l.Errorf("RunAuditExportTask 对象回传自证不完整 task_id=%d object_key=%q hash_len=%d",
			task.TaskID, loc.ObjectKey, len(loc.SHA256Hex))
		return fail("对象存储未回传一致的 object_key 与 sha256hex", model.ErrObjectStorageUnsupported)
	}
	ref := model.ObjectRef{
		Bucket:    firstNonEmpty(loc.Bucket, bucket),
		ObjectKey: loc.ObjectKey,
		Size:      loc.Size,
		FileHash:  loc.SHA256Hex,
		ExpireAt:  d.now() + d.opts.ObjectTTLSeconds,
		TotalRows: total,
	}
	ok, err := d.exports.Finish(l.ctx, task.TaskID, model.ExportStateSucceeded, ref, "")
	if err != nil {
		l.Errorf("RunAuditExportTask 置 succeeded 失败 task_id=%d err=%v", task.TaskID, err)
		return nil, err
	}
	if !ok {
		l.Errorf("RunAuditExportTask 任务已离开 running，本次不覆盖终态 task_id=%d", task.TaskID)
	}
	fresh, ferr := d.exports.FindOne(l.ctx, task.TaskID)
	if ferr != nil {
		l.Errorf("RunAuditExportTask 回读任务失败 task_id=%d err=%v", task.TaskID, ferr)
		fresh = task
	}
	spec := selfAuditSpec{
		Action:     actionExportFinish,
		Domain:     selfDomainDataAccess,
		TargetType: targetExportTask,
		TargetID:   strconv.FormatInt(task.TaskID, 10),
		Reason:     "推进导出任务",
		Before:     filterDims(filter.entryFilter(0)),
		After: map[string]string{
			"state":     fresh.State,
			"row_count": strconv.FormatInt(fresh.RowCount, 10),
			"file_hash": ref.FileHash,
			"size":      strconv.FormatInt(ref.Size, 10),
		},
		Nonce: fresh.RowCount,
	}
	if fresh.State != model.ExportStateSucceeded {
		// 没成功也留痕：否则「跑了一轮而台账上什么都没有」无法解释。
		spec.Reason = "推进导出任务：未收敛到 succeeded"
		spec.Result = rpc.AuditResult_AUDIT_RESULT_ERROR
	}
	selfAuditLogged(l.ctx, l.Logger, cc, d, spec)
	return &rpc.RunAuditExportTaskReply{
		Task:         exportTaskView(fresh),
		ExportedRows: total - startRows,
		Finished:     exportStopped(fresh.State),
	}, nil
}
