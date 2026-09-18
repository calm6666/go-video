package logic

import (
	"context"
	"encoding/json"

	"go-video/services/user-profile/internal/repository"
	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AddPropertyReviewLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAddPropertyReviewLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AddPropertyReviewLogic {
	return &AddPropertyReviewLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 添加用户属性变更审核。
// 参考 service.AddPropertyReview：旧值取自当前资料（头像取 URL path），
// 归档同属性待审核记录后新增审核行；extra 非空时校验 JSON 格式。
func (l *AddPropertyReviewLogic) AddPropertyReview(in *rpc.AddPropertyReviewReq) (*rpc.EmptyReply, error) {
	extra := in.Extra
	if extra != "" {
		var m map[string]any
		if err := json.Unmarshal([]byte(extra), &m); err != nil {
			l.Errorf("user-profile/AddPropertyReview: bad extra %q err=%v", extra, err)
			return nil, err
		}
	}
	err := l.svcCtx.Repository.AddPropertyReview(l.ctx, &repository.PropertyReviewArg{
		Mid:      in.Mid,
		New:      in.New,
		State:    int8(in.State),
		Property: int8(in.Property),
		Extra:    extra,
	})
	if err != nil {
		l.Errorf("user-profile/AddPropertyReview: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
