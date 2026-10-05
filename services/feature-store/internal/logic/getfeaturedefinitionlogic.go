package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go-video/services/feature-store/internal/svc"
	"go-video/services/feature-store/model"
	"go-video/services/feature-store/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetFeatureDefinitionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetFeatureDefinitionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetFeatureDefinitionLogic {
	return &GetFeatureDefinitionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询单个特征定义
//
// version=0 走 ACTIVE 指针，且**直读指针表而不走 fs:active 缓存**：
// 定义是元数据事实源，管理侧读到陈旧指针会让人误判「切换没生效」，
// 一次主键/唯一索引点查的成本远低于这个误判的代价。
func (l *GetFeatureDefinitionLogic) GetFeatureDefinition(
	in *rpc.GetFeatureDefinitionReq) (*rpc.GetFeatureDefinitionReply, error) {
	key := strings.TrimSpace(in.GetFeatureKey())
	if err := checkFeatureKey(key); err != nil {
		return nil, err
	}
	version := in.GetVersion()
	if version == 0 {
		pointer, err := l.svcCtx.ActiveVersions.FindOne(l.ctx, key)
		if err != nil {
			return nil, fmt.Errorf("feature-store: read active pointer: %w", err)
		}
		if !pointer.HasActive() {
			// 未注册与「注册了但没有生效版本」都表现为 found=false，不伪造口径。
			return &rpc.GetFeatureDefinitionReply{Found: false}, nil
		}
		version = pointer.ActiveVersion
	} else if err := checkVersion(version); err != nil {
		return nil, err
	}

	def, err := l.svcCtx.Definitions.FindOne(l.ctx, key, version)
	switch {
	case errors.Is(err, model.ErrFeatureNotFound):
		return &rpc.GetFeatureDefinitionReply{Found: false}, nil
	case err != nil:
		return nil, err
	}
	return &rpc.GetFeatureDefinitionReply{Definition: defToProto(def), Found: true}, nil
}
