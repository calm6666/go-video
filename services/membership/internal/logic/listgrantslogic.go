package logic

import (
	"context"
	"strings"

	"go-video/services/membership/internal/svc"
	"go-video/services/membership/model"
	"go-video/services/membership/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListGrantsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListGrantsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListGrantsLogic {
	return &ListGrantsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListGrants 授予台账分页（运营面与用户面共用）。
//
// 口径：
//  1. mid=0 才是跨用户查询（admin 面）；终端面必须由网关带上鉴权后的 mid，
//     本服务不猜调用方是谁——跨用户读台账的能力只在运营侧暴露（AGENTS.md §6 网关统一鉴权）。
//  2. from_ts/to_ts 是「事件时间」闭区间，落在 idx_mid_ctime 上；
//  3. page/size 超限裁剪并在 reply 回显实际值；
//  4. 台账只增不删，所以查不到就是没发生过，返回 total=0 而不是错误。
func (l *ListGrantsLogic) ListGrants(in *rpc.ListGrantsReq) (*rpc.ListGrantsReply, error) {
	if in.Mid < 0 {
		return nil, model.ErrInvalidMid
	}
	var vipType int32
	if in.VipType != rpc.VipType_VIP_TYPE_UNSPECIFIED {
		if err := requireVipType(in.VipType); err != nil {
			return nil, err
		}
		vipType = int32(in.VipType)
	}
	var source int32
	if in.Source != rpc.GrantSource_GRANT_SOURCE_UNSPECIFIED {
		if !model.ValidGrantSource(int32(in.Source)) {
			return nil, model.ErrGrantSourceRequired
		}
		source = int32(in.Source)
	}
	if in.FromTs > 0 && in.ToTs > 0 && in.FromTs > in.ToTs {
		return nil, model.ErrInvalidQueryFilter
	}
	if err := checkLen("biz_order_no", strings.TrimSpace(in.BizOrderNo), model.MaxBizNoLength); err != nil {
		return nil, err
	}

	page, size, offset := clampPage(l.svcCtx.Config.Membership, in.Page, in.Size)
	rows, total, err := l.svcCtx.Grant.List(l.ctx, model.GrantQuery{
		Mid:        in.Mid,
		VipType:    vipType,
		Source:     source,
		BizOrderNo: in.BizOrderNo,
		FromTs:     in.FromTs,
		ToTs:       in.ToTs,
		Offset:     offset,
		Limit:      size,
	})
	if err != nil {
		l.Errorf("membership/ListGrants: query mid=%d page=%d size=%d err=%v", in.Mid, page, size, err)
		return nil, err
	}
	return &rpc.ListGrantsReply{
		Grants: grantListToRPC(rows),
		Total:  total,
		Page:   page,
		Size:   size,
	}, nil
}
