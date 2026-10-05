package logic

import (
	"context"
	"time"

	"go-video/services/playback/internal/svc"
	"go-video/services/playback/model"
	"go-video/services/playback/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetSessionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetSessionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetSessionLogic {
	return &GetSessionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// GetSession 按 session_id 查询播放会话与进度，供管理后台与排障使用。
// 返回的是领域事实（不含签名串与私钥）：auth_key 从不落库，因此也无法从这里泄漏。
// latest_progress 是同一观看者对同一内容的最近一次进度，用于回答
// "为什么客户端没有从头播放"这类断点问题。
func (l *GetSessionLogic) GetSession(in *rpc.GetSessionReq) (*rpc.GetSessionReply, error) {
	if in.GetSessionId() == "" {
		return nil, model.ErrMissingSessionID
	}
	now := time.Now().Unix()
	s, err := l.svcCtx.Repository.FindSession(l.ctx, in.GetSessionId(), now)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, model.ErrSessionNotFound
	}
	progress, err := l.svcCtx.Repository.FindProgress(l.ctx, s.SessionId)
	if err != nil {
		return nil, err
	}
	reply := &rpc.GetSessionReply{
		Session:  sessionInfo(s),
		Progress: progressInfo(progress),
	}
	if s.Mid > 0 {
		latest, err := l.svcCtx.Repository.FindLatestProgress(l.ctx, s.Mid, s.ContentType, s.ContentId)
		if err != nil {
			// 排障辅助字段：取不到不影响主查询返回。
			l.Errorf("playback/GetSession latest progress session=%s err=%v", s.SessionId, err)
		} else if latest != nil && latest.SessionId != s.SessionId {
			reply.LatestProgress = progressInfo(latest)
		}
	}
	return reply, nil
}
