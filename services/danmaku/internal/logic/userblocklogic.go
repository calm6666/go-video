package logic

import (
	"context"
	"strings"
	"unicode/utf8"

	"go-video/services/danmaku/internal/svc"
	"go-video/services/danmaku/model"
	"go-video/services/danmaku/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// maxUserKeywordLen 是用户自定义屏蔽关键词的最大字符数，
// 与 danmaku_user_block.keyword 列宽一致。
const maxUserKeywordLen = 64

type UserBlockLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUserBlockLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UserBlockLogic {
	return &UserBlockLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// UserBlock 用户级屏蔽：屏蔽某用户发送的全部弹幕，或屏蔽包含某关键词的弹幕。
//
// 该配置只影响读取侧过滤（ListDanmaku），不改动弹幕主表，
// 因此解除屏蔽无需回填历史数据。写幂等由唯一索引 uniq_mid_target 保证，
// 重复提交同一目标只会更新状态，不会产生新行。
func (l *UserBlockLogic) UserBlock(in *rpc.UserBlockReq) (*rpc.EmptyReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.BlockedMid == in.Mid && in.BlockedMid > 0 {
		return nil, model.ErrForbidden
	}

	blockType, blockedMid, keyword, err := normalizeUserBlock(in)
	if err != nil {
		return nil, err
	}

	state := model.UserBlockOn
	if in.Unblock {
		state = model.UserBlockOff
	}
	if _, err := l.svcCtx.Repository.UpsertUserBlock(l.ctx, &model.UserBlock{
		Mid:        in.Mid,
		Type:       blockType,
		BlockedMid: blockedMid,
		Keyword:    keyword,
		State:      state,
	}); err != nil {
		l.Errorf("danmaku/UserBlock: upsert mid=%d type=%d blocked=%d err=%v", in.Mid, blockType, blockedMid, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}

// normalizeUserBlock 校验屏蔽目标与类型的一致性。
func normalizeUserBlock(in *rpc.UserBlockReq) (blockType int32, blockedMid int64, keyword string, err error) {
	switch in.Type {
	case rpc.UserBlockType_USER_BLOCK_MID:
		if in.BlockedMid <= 0 {
			return 0, 0, "", model.ErrInvalidUserBlock
		}
		return model.UserBlockMid, in.BlockedMid, "", nil
	case rpc.UserBlockType_USER_BLOCK_KEYWORD:
		kw := strings.TrimSpace(in.Keyword)
		if kw == "" || utf8.RuneCountInString(kw) > maxUserKeywordLen {
			return 0, 0, "", model.ErrInvalidUserBlock
		}
		return model.UserBlockKeyword, 0, kw, nil
	default:
		return 0, 0, "", model.ErrInvalidUserBlock
	}
}
