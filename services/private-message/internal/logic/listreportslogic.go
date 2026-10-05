package logic

import (
	"context"
	"fmt"

	"go-video/services/private-message/internal/svc"
	"go-video/services/private-message/model"
	"go-video/services/private-message/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListReportsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListReportsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListReportsLogic {
	return &ListReportsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 运营侧举报分页。
//
// 隐私口径（本方法最重要的一条）：返回的 ReportInfo 只有主键、原因码、状态、处理人与时间，
// **不含消息正文也不含摘要** —— 举报单本身就是处置依据，正文经 moderation 的脱敏通道取阅，
// 本服务不给运营面开「读用户私信」的能力（AGENTS.md §8：信箱/私信正文是用户隐私）。
//
// 其余口径：operator_mid 必须 >0（无主查询不可受理，真正的管理员鉴权在 gateway/admin）；
// ps 落配置上下限；cursor 是 report_id 倒序位点，禁止 offset 翻页；
// 只读，不改状态、不写审计；待处理队列取件走 ReportModel.ListPending。
func (l *ListReportsLogic) ListReports(in *rpc.ListReportsReq) (*rpc.ListReportsReply, error) {
	ctx := l.ctx
	s := l.svcCtx
	cfg := s.Config.PrivateMessage

	if err := checkOperator(in.GetOperatorMid()); err != nil {
		return nil, err
	}
	state := int32(in.GetState())
	if state != 0 && (state < model.ReportStatePending || state > model.ReportStateDismissed) {
		return nil, fmt.Errorf("%w: 举报状态过滤 state=%d", model.ErrInvalidReportAction, state)
	}
	ps, err := clampPageSize(in.GetPs(), cfg.PageSize, cfg.MaxPageSize)
	if err != nil {
		return nil, err
	}
	cursorID, err := decodeIDCursor(in.GetCursor())
	if err != nil {
		return nil, err
	}

	rows, err := s.Reports.ListByCursor(ctx, int64(state), in.GetTargetMid(), cursorID, ps+1)
	if err != nil {
		return nil, err
	}
	hasMore := len(rows) > int(ps)
	if hasMore {
		rows = rows[:ps]
	}
	nextCursor := ""
	if hasMore && len(rows) > 0 {
		nextCursor = encodeIDCursor(rows[len(rows)-1].ReportID)
		if nextCursor == "" {
			hasMore = false
		}
	}
	return &rpc.ListReportsReply{
		List:       reportInfoList(rows),
		NextCursor: nextCursor,
		HasMore:    hasMore,
	}, nil
}
