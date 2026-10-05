package logic

import (
	"context"

	"go-video/common/validation"
	"go-video/services/risk-control/internal/svc"
	"go-video/services/risk-control/model"
	"go-video/services/risk-control/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetListEntriesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetListEntriesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetListEntriesLogic {
	return &GetListEntriesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询名单条目（admin 审计与运营检索用）。
// list_type/target_type 为 UNSPECIFIED、target_value 为空表示对应维度不过滤；
// state=-1 不过滤，0/1 精确匹配。
// target_value 传明文 IP 时不返回结果也不报错，而是直接拒绝：
// 检索接口若接受明文 IP，等于给运营后台提供了一个 IP 反查工具。
func (l *GetListEntriesLogic) GetListEntries(in *rpc.GetListEntriesReq) (*rpc.GetListEntriesReply, error) {
	listType := int32(in.GetListType())
	if listType != 0 && !model.ValidListType(listType) {
		return nil, model.ErrInvalidListEntry
	}
	targetType := int32(in.GetTargetType())
	if targetType != 0 && !model.ValidTargetType(targetType) {
		return nil, model.ErrInvalidListEntry
	}
	state := in.GetState()
	if state != -1 && state != model.StateDisabled && state != model.StateEnabled {
		return nil, model.ErrInvalidListEntry
	}

	targetValue := ""
	if raw := in.GetTargetValue(); raw != "" {
		if targetType == 0 {
			// 不指定 target_type 时无法判定该按哪种规范化匹配，
			// 与其做模糊 LIKE（会引入跨类型误匹配）不如要求调用方明确维度。
			return nil, model.ErrInvalidTarget
		}
		targetValue = model.NormalizeTargetValue(targetType, raw)
		if targetValue == "" {
			return nil, model.ErrRawIPForbidden
		}
	}
	page := validation.NormalizePage(int(in.GetPn()), int(in.GetPs()), maxPageSize)

	rows, total, err := l.svcCtx.Repository.GetListEntries(l.ctx, listType, targetType, targetValue, state, page.Page, page.PageSize)
	if err != nil {
		l.Errorf("risk-control/GetListEntries: failed list_type=%d target_type=%d err=%v", listType, targetType, err)
		return nil, err
	}
	return &rpc.GetListEntriesReply{
		Entries: listEntriesToProto(rows),
		Total:   total,
		Pn:      int32(page.Page),
		Ps:      int32(page.PageSize),
	}, nil
}
