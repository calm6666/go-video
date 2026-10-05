package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go-video/services/membership/internal/svc"
	"go-video/services/membership/model"
	"go-video/services/membership/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type SetAutoRenewLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSetAutoRenewLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SetAutoRenewLogic {
	return &SetAutoRenewLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// SetAutoRenew 自动续费签约位翻转（沙箱，不建真实代扣协议）。
//
// 口径：
//  1. on=true 的渠道只允许 SANDBOX（逐字节比对）：真实代扣协议一概不落库，
//     记一个永远不会生效、却会被续费 cron 当真的协议位比拒绝更危险；
//  2. on=false 允许带渠道（客户端常常原样回显），一律归一化成「无渠道」，
//     解约后 auto_renew_channel 必须为空，否则判读方会以为还在约中；
//  3. 只允许对生效中的身份签约：已过期还置签约位，等于给续费 cron 留一个
//     「凭空复活会员」的入口，返回 ErrAutoRenewUnsupported；
//  4. 签约位不是时长变更：不写 mb_grant（契约把 action 钉死在四种时长动作），
//     幂等走 mb_biz_request 的唯一主键 + 参数指纹，重放返回 duplicated=true；
//  5. 不动 source / paid_month_count：source 记录的是「最近一次授予来源」，
//     自助翻签约位改写它会把台账上的付费来源抹成别的值。
func (l *SetAutoRenewLogic) SetAutoRenew(in *rpc.SetAutoRenewReq) (*rpc.SetAutoRenewReply, error) {
	if err := requireMid(in.Mid); err != nil {
		return nil, err
	}
	if err := requireVipType(in.VipType); err != nil {
		return nil, err
	}
	if err := requireRequestId(in.RequestId); err != nil {
		return nil, err
	}
	if err := requireOperator(in.Operator); err != nil {
		return nil, err
	}
	if err := optionalReason(in.Reason); err != nil {
		return nil, err
	}

	vipType := int32(in.VipType)
	operator := strings.TrimSpace(in.Operator)
	channel, err := normalizeAutoRenewChannel(in.On, in.Channel)
	if err != nil {
		return nil, err
	}
	subject := membershipSubject(in.Mid, vipType)
	fp := model.Fingerprint(model.ApiSetAutoRenew, subject, fmt.Sprintf("%t", in.On), channel)
	now := model.NowUnix()

	// 幂等重放优先：同一 request_id 不再翻第二次（翻两次会改写 signed_at）。
	req, err := l.svcCtx.Request.FindByRequestIDTx(l.ctx, nil, in.RequestId)
	if err != nil {
		l.Errorf("membership/SetAutoRenew: idempotency lookup request_id=%s err=%v", in.RequestId, err)
		return nil, err
	}
	if req != nil {
		return l.replay(req, fp, subject, in.RequestId)
	}

	var applied *model.Membership
	err = l.svcCtx.DB.TransactCtx(l.ctx, func(ctx context.Context, session sqlx.Session) error {
		cur, err := l.svcCtx.Membership.FindOneTx(ctx, session, in.Mid, vipType)
		if err != nil {
			return err
		}
		if cur == nil {
			return model.ErrMembershipNotFound
		}
		if !cur.IsActive(now) {
			return fmt.Errorf("%w: membership expired at %d (now=%d)", model.ErrAutoRenewUnsupported, cur.ExpireAt, now)
		}
		// 先占幂等键再改行：键被并发抢走时整个事务回滚，不会留下无痕的改动。
		if err := l.svcCtx.Request.InsertTx(ctx, session, &model.BizRequest{
			RequestID:         in.RequestId,
			Api:               model.ApiSetAutoRenew,
			Subject:           subject,
			Mid:               in.Mid,
			VipType:           vipType,
			ResultID:          cur.MembershipID,
			ParamsFingerprint: fp,
			Operator:          operator,
			Ctime:             now,
		}); err != nil {
			return err
		}

		row := *cur
		row.AutoRenew = int32FromBool(in.On)
		row.AutoRenewChannel = channel
		row.AutoRenewSignedAt = now
		ok, err := l.svcCtx.Membership.UpdateTx(ctx, session, &row)
		if err != nil {
			return err
		}
		if !ok {
			return model.ErrConcurrentUpdate
		}
		applied = &row
		return nil
	})
	if err != nil {
		switch {
		case l.svcCtx.Request.IsDuplicate(err):
			rival, ferr := l.svcCtx.Request.FindByRequestIDTx(l.ctx, nil, in.RequestId)
			if ferr != nil {
				return nil, ferr
			}
			if rival != nil {
				return l.replay(rival, fp, subject, in.RequestId)
			}
			return nil, err
		case errors.Is(err, model.ErrMembershipNotFound):
			l.Infof("membership/SetAutoRenew: no identity row mid=%d vip_type=%d request_id=%s", in.Mid, vipType, in.RequestId)
			return nil, err
		default:
			l.Errorf("membership/SetAutoRenew: apply failed mid=%d vip_type=%d request_id=%s err=%v",
				in.Mid, vipType, in.RequestId, err)
			return nil, err
		}
	}

	l.Infof("membership/SetAutoRenew: mid=%d vip_type=%d on=%t channel=%s request_id=%s",
		in.Mid, vipType, in.On, channel, in.RequestId)
	return &rpc.SetAutoRenewReply{
		Duplicated: false,
		Membership: membershipToRPC(applied),
	}, nil
}

// normalizeAutoRenewChannel 校验并归一化签约渠道。
// 空串在 on=false 时是正常值（解约后不保留渠道），在 on=true 时是缺参。
func normalizeAutoRenewChannel(on bool, raw string) (string, error) {
	channel := strings.TrimSpace(raw)
	if channel == "" {
		if on {
			return "", model.ErrAutoRenewChannelRequired
		}
		return "", nil
	}
	if err := checkLen("channel", channel, model.MaxAutoRenewChannelLength); err != nil {
		return "", err
	}
	if !on {
		// 解约带渠道：归一化掉，不影响幂等指纹（指纹用的是归一化后的值）。
		return "", nil
	}
	if channel != model.AllowedAutoRenewChannel {
		// 本服务不接任何真实代扣渠道，也不落「以后可能会生效」的协议位。
		return "", fmt.Errorf("%w: %s (only %s is supported)",
			model.ErrAutoRenewChannelRejected, channel, model.AllowedAutoRenewChannel)
	}
	return channel, nil
}

// replay 命中同一 request_id：指纹一致才把当前状态还给调用方。
func (l *SetAutoRenewLogic) replay(req *model.BizRequest, fp, subject, requestID string) (*rpc.SetAutoRenewReply, error) {
	if req.ParamsFingerprint != fp {
		l.Errorf("membership/SetAutoRenew: request_id=%s reused with different parameters", requestID)
		return nil, model.ErrRequestIdReused
	}
	var (
		mid     = req.Mid
		vipType = req.VipType
	)
	if mid <= 0 || vipType <= 0 {
		// 台账里主体信息与 subject 不一致说明数据被人工改过，不能伪称成功。
		l.Errorf("membership/SetAutoRenew: request_id=%s subject=%s has unusable mid/vip_type (%d,%d)",
			requestID, req.Subject, mid, vipType)
		return nil, model.ErrMembershipNotFound
	}
	row, err := l.svcCtx.Membership.FindOne(l.ctx, mid, vipType)
	if err != nil {
		l.Errorf("membership/SetAutoRenew: replay reread mid=%d vip_type=%d err=%v", mid, vipType, err)
		return nil, err
	}
	if row == nil {
		l.Errorf("membership/SetAutoRenew: request_id=%s has no membership row mid=%d vip_type=%d", requestID, mid, vipType)
		return nil, model.ErrMembershipNotFound
	}
	l.Infof("membership/SetAutoRenew: replay request_id=%s mid=%d vip_type=%d, signed_at not bumped twice",
		requestID, mid, vipType)
	return &rpc.SetAutoRenewReply{
		Duplicated: true,
		Membership: membershipToRPC(row),
	}, nil
}
