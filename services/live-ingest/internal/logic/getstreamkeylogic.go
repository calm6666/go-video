package logic

import (
	"context"

	"go-video/services/live-ingest/internal/svc"
	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetStreamKeyLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetStreamKeyLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetStreamKeyLogic {
	return &GetStreamKeyLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询密钥元数据（永不回显明文或哈希）
//
// 投影只走 keyInfo：StreamKeyInfo 里没有明文/哈希字段，因此「读了哪一列」在编译期就被约束住。
func (l *GetStreamKeyLogic) GetStreamKey(in *rpc.GetStreamKeyReq) (*rpc.GetStreamKeyReply, error) {
	repo := l.svcCtx.Repository
	if repo == nil {
		return nil, errNoRepository
	}

	var (
		k   *model.StreamKey
		err error
	)
	switch {
	case in.KeyId > 0:
		k, err = repo.StreamKey.FindOne(l.ctx, in.KeyId)
	case in.StreamName != "":
		var name string
		if name, err = checkStreamNameForLookup(in.StreamName); err != nil {
			return nil, err
		}
		k, err = repo.StreamKey.FindCurrentByStreamName(l.ctx, name)
	default:
		return nil, model.ErrInvalidKeyId
	}
	if err != nil {
		return nil, err
	}
	if k == nil {
		return nil, model.ErrStreamKeyNotFound
	}
	return &rpc.GetStreamKeyReply{Key: keyInfo(k)}, nil
}

// checkStreamNameForLookup 校验按流标识查询时的入参。
// 读路径不需要「拒绝并解释细节」，非法标识与不存在的标识同样按未找到处理，避免枚举。
func checkStreamNameForLookup(name string) (string, error) {
	trimmed, ok := normalizeStreamName(name)
	if !ok {
		return "", model.ErrStreamKeyNotFound
	}
	return trimmed, nil
}
