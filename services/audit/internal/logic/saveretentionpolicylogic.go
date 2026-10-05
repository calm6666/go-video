package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go-video/services/audit/internal/svc"
	"go-video/services/audit/model"
	"go-video/services/audit/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SaveRetentionPolicyLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSaveRetentionPolicyLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SaveRetentionPolicyLogic {
	return &SaveRetentionPolicyLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 新建/更新保留期策略（expect_version 乐观锁）
//
// 实现要点（规格 1~5）：
//  1. request_id 必填；action_domain 字符集/长度由 model.ValidActionDomain 钉住；
//  2. 天数自洽性交给 model.CanPolicyDays（单点实现），不自洽直接拒绝而不纠正数值；
//  3. expect_version==0 走 Insert（uniq_action_domain 冲突 → ErrPolicyExists），
//     非 0 走 UpdateWithVersion（未命中 → ErrPolicyVersionConflict）；
//  4. 放宽 delete_after_days 时必须给 remark，并写 action=audit.retention.loosen；
//     这一判定在改数据之前完成 —— 放在写之后等于「拒绝」了却已经把窗口放宽，
//     还会因为提前 return 而一行留痕都不写；
//  5. 契约没有删除策略的方法：只能停用（state=2）。
//
// 幂等口径：新建路径没有 request_id 列可落（表里没这一列），
// 因此以「同 domain + 同内容」判定重复提交（samePolicy）——
// 内容不同而同 domain 仍报 ErrPolicyExists，让调用方改用 expect_version 更新，
// 而不是悄悄覆盖别人刚收紧的窗口。
func (l *SaveRetentionPolicyLogic) SaveRetentionPolicy(in *rpc.SaveRetentionPolicyReq) (*rpc.SaveRetentionPolicyReply, error) {
	if in == nil {
		return nil, model.ErrRequestRequired
	}
	cc := in.GetCtx()
	if err := validateCallContext(cc, true); err != nil {
		return nil, err
	}
	domain := strings.TrimSpace(in.GetActionDomain())
	if !model.ValidActionDomain(domain) {
		return nil, fmt.Errorf("%w: action_domain=%q", model.ErrActionDomainInvalid, domain)
	}
	if !model.CanPolicyDays(in.GetHotDays(), in.GetArchiveAfterDays(), in.GetDeleteAfterDays()) {
		return nil, fmt.Errorf("%w: hot_days=%d archive_after_days=%d delete_after_days=%d",
			model.ErrPolicyDaysInvalid, in.GetHotDays(), in.GetArchiveAfterDays(), in.GetDeleteAfterDays())
	}
	state, err := policyState(in.GetState())
	if err != nil {
		return nil, err
	}
	remark := strings.TrimSpace(in.GetRemark())
	expectVersion := in.GetExpectVersion()
	if expectVersion < 0 {
		return nil, fmt.Errorf("%w: expect_version=%d 不可为负", model.ErrRequestRequired, expectVersion)
	}
	d := buildDeps(l.svcCtx)
	if err := checkLen("remark", remark, model.MaxRemarkBytes); err != nil {
		return nil, err
	}
	if err := scanPIIValues("remark", remark); err != nil {
		return nil, err
	}
	// 放宽物理清理窗口是「让证据更早消失」，动机必须留在 remark 里（规格 4）。
	// 新建时没有旧值可比，不做放宽判定；首条策略就带 delete_after_days>0 也属放宽，
	// 由下面的新建分支单独识别。
	next := &model.RetentionPolicy{
		ActionDomain:     domain,
		HotDays:          in.GetHotDays(),
		ArchiveAfterDays: in.GetArchiveAfterDays(),
		DeleteAfterDays:  in.GetDeleteAfterDays(),
		State:            state,
		Operator:         cc.GetOperatorId(),
		Remark:           remark,
	}

	var (
		old     *model.RetentionPolicy
		created bool
	)
	// 放宽判定必须在改数据之前。放在写之后会有两层问题：
	//  1. 窗口已经真的放宽了却只回一个错误 —— 相当于「拒绝」没有拒掉任何东西；
	//  2. 直接 return 就跳过了 selfAuditLogged，改策略这件事一行留痕都没有。
	// 新建路径没有旧值可比，因此「首条策略就带 delete_after_days>0」
	// 按相对默认值（0 = 永不物理清理）放宽处理，与后面的 loosened 标记口径一致。
	requireRemark := func(loosened bool) error {
		if loosened && remark == "" {
			return fmt.Errorf("%w: 放宽物理清理窗口必须给 remark 说明动机", model.ErrReasonRequired)
		}
		return nil
	}
	if expectVersion == 0 {
		created = true
		if err := requireRemark(next.DeleteAfterDays > 0); err != nil {
			return nil, err
		}
		if _, err := d.policies.Insert(l.ctx, next); err != nil {
			if !errors.Is(err, model.ErrPolicyExists) {
				l.Errorf("SaveRetentionPolicy 新建策略失败 domain=%s request_id=%s err=%v",
					domain, cc.GetRequestId(), err)
				return nil, err
			}
			// 幂等回放：同 domain 同内容视为重复提交，返回既有行而不是二次报错。
			existing, ferr := d.policies.FindOne(l.ctx, domain)
			if ferr != nil {
				return nil, errors.Join(err, ferr)
			}
			if existing == nil || !samePolicy(existing, next) {
				return nil, err
			}
			return &rpc.SaveRetentionPolicyReply{Policy: retentionView(existing)}, nil
		}
	} else {
		cur, err := d.policies.FindOne(l.ctx, domain)
		if err != nil {
			l.Errorf("SaveRetentionPolicy 读当前策略失败 domain=%s err=%v", domain, err)
			return nil, err
		}
		if cur == nil {
			return nil, fmt.Errorf("%w: action_domain=%s（expect_version=%d 表示更新，请先用 0 新建）",
				model.ErrPolicyNotFound, domain, expectVersion)
		}
		if cur.Version != expectVersion {
			return nil, fmt.Errorf("%w: action_domain=%s expect_version=%d actual_version=%d",
				model.ErrPolicyVersionConflict, domain, expectVersion, cur.Version)
		}
		old = cur
		updated := *cur
		updated.HotDays = next.HotDays
		updated.ArchiveAfterDays = next.ArchiveAfterDays
		updated.DeleteAfterDays = next.DeleteAfterDays
		updated.State = next.State
		updated.Operator = next.Operator
		updated.Remark = next.Remark
		// 与新建分支同理：先判「放宽而未给动机」，再动数据。
		if err := requireRemark(retentionLoosened(cur, next)); err != nil {
			return nil, err
		}
		ok, err := d.policies.UpdateWithVersion(l.ctx, &updated, expectVersion)
		if err != nil {
			l.Errorf("SaveRetentionPolicy 更新策略失败 domain=%s err=%v", domain, err)
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%w: action_domain=%s expect_version=%d 未命中，请重读后再改",
				model.ErrPolicyVersionConflict, domain, expectVersion)
		}
		*next = updated
	}

	// 只用于挑选留痕的 action/reason：拒绝未给动机的放宽已在两个分支的写数据之前完成。
	loosened := retentionLoosened(old, next) || (created && next.DeleteAfterDays > 0)
	// 重读一遍回传：version 由 SQL 的 version+1 决定，只有库里才是事实。
	fresh, err := d.policies.FindOne(l.ctx, domain)
	if err != nil {
		return nil, err
	}
	if fresh == nil {
		return nil, model.ErrPolicyNotFound
	}
	action, reason := actionRetentionSave, "变更保留期策略"
	if loosened {
		action, reason = actionRetentionLoosen, "放宽物理清理窗口"
	}
	before := map[string]string{"action_domain": domain}
	if old != nil {
		before = policyDims(old)
	}
	selfAuditLogged(l.ctx, l.Logger, cc, d, selfAuditSpec{
		Action:     action,
		Domain:     selfDomainSystem,
		TargetType: targetRetentionPolicy,
		TargetID:   domain,
		Reason:     firstNonEmpty(remark, reason),
		Before:     before,
		After:      policyDims(fresh),
		Nonce:      fresh.Version,
	})
	return &rpc.SaveRetentionPolicyReply{Policy: retentionView(fresh)}, nil
}

// policyDims 把策略压成自审计摘要维度（数值本身就是非敏感的治理参数）。
func policyDims(p *model.RetentionPolicy) map[string]string {
	if p == nil {
		return nil
	}
	return map[string]string{
		"action_domain":      p.ActionDomain,
		"hot_days":           strconv.Itoa(int(p.HotDays)),
		"archive_after_days": strconv.Itoa(int(p.ArchiveAfterDays)),
		"delete_after_days":  strconv.Itoa(int(p.DeleteAfterDays)),
		"state":              strconv.Itoa(int(p.State)),
		"version":            strconv.FormatInt(p.Version, 10),
	}
}
