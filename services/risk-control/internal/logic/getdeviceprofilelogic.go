package logic

import (
	"context"

	"go-video/services/risk-control/internal/svc"
	"go-video/services/risk-control/model"
	"go-video/services/risk-control/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetDeviceProfileLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetDeviceProfileLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetDeviceProfileLogic {
	return &GetDeviceProfileLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询设备画像。
// device_id 与 device_hash 二者取先者：device_id 优先（现算受控 ID），
// 已受控的 device_hash 可直接查询，便于运营后台按摘要检索。
// 画像不存在时返回 found=false 而不是报错：读接口不伪造画像，
// 也不把「没登记过」当成调用方错误。
func (l *GetDeviceProfileLogic) GetDeviceProfile(in *rpc.GetDeviceProfileReq) (*rpc.GetDeviceProfileReply, error) {
	hash := deviceHashOf(in.GetDeviceId(), in.GetDeviceHash())
	if hash == "" {
		return nil, model.ErrEmptyDeviceID
	}
	profile, err := l.svcCtx.Repository.GetDeviceProfile(l.ctx, hash)
	if err != nil {
		l.Errorf("risk-control/GetDeviceProfile: failed device=%s err=%v", model.TruncateHash(hash, 12), err)
		return nil, err
	}
	if profile == nil {
		return &rpc.GetDeviceProfileReply{Found: false}, nil
	}
	return &rpc.GetDeviceProfileReply{Profile: deviceToProto(profile), Found: true}, nil
}
