// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	liveingestrpc "go-video/services/live-ingest/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type LiveStreamKeyGetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 推流密钥元数据（只有末 4 位辨认串与 Vault 引用，永不含明文）
func NewLiveStreamKeyGetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveStreamKeyGetLogic {
	return &LiveStreamKeyGetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveStreamKeyGet 聚合 live-ingest GetStreamKey：metadata only。
//
// 本路由能成立的前提是契约本身：GetStreamKeyReply 唯一的消息是 StreamKeyInfo，
// 而 proto 注明它「永不回显明文或哈希」，可投影的只有 key_hint_tail（明文末 4 位，
// 不可用于鉴权）与 key_ref（Vault 引用）。因此这里不需要额外裁剪字段。
//
// 反向的口子刻意不开：IssueStreamKey / RotateStreamKey 的响应带 plaintext_key 与内嵌明文密钥的
// publish_url（仅此一次返回），后台一旦被接进去就成了「能进访问日志的一次性机密通道」；
// 密钥泄露的运营处置面是本域的 RevokeStreamKey + 主播端重新签发。
//
// GetStreamKeyReq 没有 operator/admin 位（按 key_id 或 stream_name 定位），门槛落在主体二选一。
func (l *LiveStreamKeyGetLogic) LiveStreamKeyGet(req *types.ParamLiveStreamKeyGet) (resp *types.LiveStreamKeyResponse, err error) {
	if l.svcCtx.LiveIngest == nil {
		return nil, errLiveIngestNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveKeySubjectGate(req.KeyId, req.StreamName); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveIngest.GetStreamKey(l.ctx, &liveingestrpc.GetStreamKeyReq{
		KeyId:      req.KeyId,
		StreamName: req.StreamName,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveStreamKeyGet: key_id=%d stream_name=%s err=%v", req.KeyId, req.StreamName, err)
		return nil, err
	}
	return &types.LiveStreamKeyResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveStreamKeyData{
			Key:    liveStreamKeyToAPI(reply.GetKey()),
			HasKey: reply.GetKey() != nil,
		},
		TTL: 0,
	}, nil
}
