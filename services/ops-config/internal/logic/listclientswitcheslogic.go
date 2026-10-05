package logic

import (
	"context"
	"strings"

	"go-video/services/ops-config/internal/svc"
	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListClientSwitchesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListClientSwitchesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListClientSwitchesLogic {
	return &ListClientSwitchesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 后台分页列出客户端开关（只读，不写审计）
//
// 这不是运行时读取路径：端上取「本端全部生效开关」应走
// ClientSwitch.ListForPlatform + Available(app_version)，
// 用本接口当运行时入口会撞上 ps 上限与全量拉取之间的矛盾（后台接口必须可分页）。
func (l *ListClientSwitchesLogic) ListClientSwitches(in *rpc.ListClientSwitchesReq) (*rpc.ListClientSwitchesReply, error) {
	lim := newLimits(l.svcCtx.Config)
	pn, ps, err := pageOf(in.GetPn(), in.GetPs(), lim.maxPageSize)
	if err != nil {
		return nil, err
	}
	// platform=UNSPECIFIED、switch_key=""、enabled=0 三条都是「不过滤」，
	// 由 model 侧同一段 build() 拼参数化 WHERE —— 不在 logic 里另拼一份 SQL 条件。
	if err := checkPlatformInt(int32(in.GetPlatform()), false); err != nil {
		return nil, err
	}
	enabled := in.GetEnabled()
	if enabled != 0 {
		if err := checkState(enabled); err != nil {
			return nil, err
		}
	}

	rows, total, err := l.svcCtx.Models.ClientSwitch.List(l.ctx, model.ClientSwitchFilter{
		Platform:  int32(in.GetPlatform()),
		SwitchKey: strings.TrimSpace(in.GetSwitchKey()),
		Enabled:   enabled,
		Pn:        pn,
		Ps:        ps,
	})
	if err != nil {
		return nil, err
	}
	// 排序固定 switch_key, platform, switch_id（model 侧实现），保证翻页稳定。
	// config_id 只回引用不回填 cfg_key：契约里没有承载 cfg_key 的字段（缺口见 README），
	// 更不可能回配置值——值在 ops_config_version，复制过来就是第二份真相。
	return &rpc.ListClientSwitchesReply{Items: switchList(rows), Total: lim.totalOf(total)}, nil
}
