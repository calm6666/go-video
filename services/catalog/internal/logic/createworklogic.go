package logic

import (
	"context"

	"go-video/services/catalog/internal/svc"
	"go-video/services/catalog/model"
	"go-video/services/catalog/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreateWorkLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCreateWorkLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateWorkLogic {
	return &CreateWorkLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// CreateWork 运营创建作品。
// 作品初始状态为草稿；上架需通过后续状态机推进。
func (l *CreateWorkLogic) CreateWork(in *rpc.CreateWorkReq) (*rpc.WorkReply, error) {
	if in.Title == "" {
		return nil, model.ErrInvalidTitle
	}
	if in.Typeid <= 0 {
		return nil, model.ErrInvalidType
	}
	w := &model.Work{
		Title:  in.Title,
		Cover:  in.Cover,
		TypeID: in.Typeid,
		Intro:  in.Intro,
		State:  model.StateDraft,
	}
	id, err := l.svcCtx.Repository.CreateWork(l.ctx, w)
	if err != nil {
		l.Errorf("catalog/CreateWork: title=%s typeid=%d operator=%s err=%v",
			in.Title, in.Typeid, in.Operator, err)
		return nil, err
	}
	return &rpc.WorkReply{
		SeasonId: id,
		Title:    w.Title,
		Cover:    w.Cover,
		Typeid:   w.TypeID,
		Intro:    w.Intro,
		State:    w.State,
	}, nil
}
