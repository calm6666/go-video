package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/ops-config/internal/svc"
	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListRolloutRulesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListRolloutRulesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListRolloutRulesLogic {
	return &ListRolloutRulesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 后台分页列出灰度规则（只读，不写审计）
func (l *ListRolloutRulesLogic) ListRolloutRules(in *rpc.ListRolloutRulesReq) (*rpc.ListRolloutRulesReply, error) {
	lim := newLimits(l.svcCtx.Config)
	pn, ps, err := pageOf(in.GetPn(), in.GetPs(), lim.maxPageSize)
	if err != nil {
		return nil, err
	}
	if in.GetVersion() < 0 {
		return nil, model.ErrVersionNotFound
	}
	state := in.GetState()
	if state != 0 {
		if err := checkState(state); err != nil {
			return nil, err
		}
	}
	var configID int64
	if key := strings.TrimSpace(in.GetCfgKey()); key != "" {
		// 给了 cfg_key 就必须解析成功：拼错 key 与「这个 key 没有规则」在后台是两件事，
		// 都回空列表的话前者会被当成后者，运营会以为自己已经清干净了灰度。
		if err := checkCfgKey(key, lim); err != nil {
			return nil, err
		}
		scope := normalizeScope(in.GetScope())
		if err := checkScope(scope); err != nil {
			return nil, err
		}
		item, ferr := l.svcCtx.Models.ConfigItem.FindOne(l.ctx, key, scope)
		if ferr != nil {
			return nil, ferr
		}
		if item == nil {
			return nil, fmt.Errorf("%w: cfg_key=%s scope=%s", model.ErrConfigNotFound, key, scope)
		}
		configID = item.ConfigID
	}

	rows, total, err := l.svcCtx.Models.RolloutRule.List(l.ctx, model.RolloutRuleFilter{
		ConfigID: configID,
		Version:  in.GetVersion(),
		State:    state,
		Pn:       pn,
		Ps:       ps,
	})
	if err != nil {
		return nil, err
	}
	// platforms / whitelist_mids 的存储串由 conv.go 还原成数组；
	// 白名单是用户 ID，只在后台接口回，绝不进运行时解析路径的缓存值。
	return &rpc.ListRolloutRulesReply{Items: rolloutRuleList(rows), Total: total}, nil
}
