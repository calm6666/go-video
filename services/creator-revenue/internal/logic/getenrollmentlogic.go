package logic

import (
	"context"

	"go-video/services/creator-revenue/internal/svc"
	"go-video/services/creator-revenue/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetEnrollmentLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetEnrollmentLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetEnrollmentLogic {
	return &GetEnrollmentLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询参与状态
//
// 「没参加过」是合法答案，用 found=false + enrollment=nil 表达；
// 但「查不动」（未配置库、SQL 出错）必须是错误，不能伪装成未参加，
// 否则创作者端首页会把故障渲染成「你还没加入计划」。
func (l *GetEnrollmentLogic) GetEnrollment(in *rpc.GetEnrollmentReq) (*rpc.GetEnrollmentReply, error) {
	if err := l.svcCtx.Ready(); err != nil {
		return nil, err
	}
	mid, err := normalizeMid(in.Mid)
	if err != nil {
		return nil, err
	}
	row, err := l.svcCtx.Enrollments.FindOne(l.ctx, mid)
	if err != nil {
		l.Errorf("get enrollment mid=%d: %v", mid, err)
		return nil, err
	}
	if row == nil {
		return &rpc.GetEnrollmentReply{Found: false}, nil
	}
	return &rpc.GetEnrollmentReply{Found: true, Enrollment: enrollmentInfo(row)}, nil
}
