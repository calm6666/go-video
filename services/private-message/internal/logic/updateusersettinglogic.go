package logic

import (
	"context"
	"fmt"

	"go-video/services/private-message/internal/svc"
	"go-video/services/private-message/model"
	"go-video/services/private-message/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpdateUserSettingLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpdateUserSettingLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateUserSettingLogic {
	return &UpdateUserSettingLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 更新反骚扰偏好。
//
// 实现要点：
//  1. 只改自己的偏好：mid 由 gateway 注入，本方法没有「替别人设置」的入口（operator 鉴权在网关侧）；
//  2. allow_from=UNSPECIFIED 表示不修改，显式取值必须落在 model.ValidAllowFrom；
//  3. 局部更新：三个 optional 布尔用 has_xxx 区分「未传」与「传 false」，
//     未传的保持基线行原值——把零值当成关闭写进去会静默改掉用户没碰过的开关；
//  4. 基线取 Settings.FindByMid，缺行时以站点级缺省（DefaultAllowFrom / RejectStrangerByDefault /
//     KeywordFilterEnabled）为基线再合并本次修改；
//  5. 写入用 Upsert（mid 主键 + ON DUPLICATE KEY UPDATE），单行天然幂等，无跨表副作用故不需要事务；
//  6. 缓存：偏好本身不进缓存（发送门禁每次从 MySQL 读这一行），
//     pair verdict 缓存的只是 social-graph 的关系事实，不受本表影响，因此无需失效；
//     未读汇总与免打扰无关，这里只失效一次角标以防投影口径抖动。
func (l *UpdateUserSettingLogic) UpdateUserSetting(in *rpc.UpdateUserSettingReq) (*rpc.UserSettingInfo, error) {
	ctx := l.ctx
	s := l.svcCtx

	mid := in.GetMid()
	if err := checkMid(mid); err != nil {
		return nil, err
	}
	row, err := loadSettingOrDefault(ctx, s, mid)
	if err != nil {
		return nil, err
	}
	normalizeSetting(s, row)

	if af := int32(in.GetAllowFrom()); af != 0 {
		if !model.ValidAllowFrom(af) {
			return nil, fmt.Errorf("%w: allow_from=%d", model.ErrInvalidAllowFrom, af)
		}
		row.AllowFrom = af
	}
	// optional 字段：nil 表示客户端没带这个开关，保持基线；非 nil 才覆盖（含显式 false）。
	if in.RejectStranger != nil {
		row.RejectStranger = boolToTiny(in.GetRejectStranger())
	}
	if in.KeywordFilter != nil {
		row.KeywordFilter = boolToTiny(in.GetKeywordFilter())
	}
	if in.MuteConversation != nil {
		row.MuteConversation = boolToTiny(in.GetMuteConversation())
	}
	// KeywordFilterEnabled=false 是压测/回放环境的站点级开关：不允许被单用户写回 1，
	// 否则「整体关掉过滤」的环境里会出现个别行仍标记已过滤的假事实。
	if !s.Config.PrivateMessage.KeywordFilterEnabled {
		row.KeywordFilter = 0
	}

	if err := s.Settings.Upsert(ctx, row); err != nil {
		return nil, err
	}
	invalidateUnreadCache(ctx, s, l.Logger, mid)

	fresh, err := s.Settings.FindByMid(ctx, mid)
	if err != nil {
		return nil, err
	}
	if fresh == nil {
		return nil, fmt.Errorf("private-message/logic: 偏好写入后回查不到 mid=%d", mid)
	}
	normalizeSetting(s, fresh)
	return settingInfo(fresh), nil
}
