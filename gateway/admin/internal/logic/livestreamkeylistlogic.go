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

type LiveStreamKeyListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 推流密钥台账（key_id 倒序，含轮转链与吊销原因）
func NewLiveStreamKeyListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveStreamKeyListLogic {
	return &LiveStreamKeyListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveStreamKeyList 聚合 live-ingest ListStreamKeys。
//
// Admin 固定 true，且它**不是表单字段**：ParamLiveStreamKeyList 里没有 admin，请求无法声明。
// 依据是 ListStreamKeysReq 的既有语义——admin=false 时服务会把结果收敛成
// anchor_mid == operator_mid 的行（普通主播只能看自己的密钥），后台台账页于是会拿到一份
// 「看起来没人有过密钥」的清单。这条断言的正当性来自部署事实（本路由只在 /admin/live 运营面里），
// 不是来自单次请求的鉴权结果；残余风险记在 gateway/admin/README.md。
//
// 投影只回元数据（key_hint_tail / key_ref），密钥明文与哈希按契约不存在于响应里。
func (l *LiveStreamKeyListLogic) LiveStreamKeyList(req *types.ParamLiveStreamKeyList) (resp *types.LiveStreamKeyListResponse, err error) {
	if l.svcCtx.LiveIngest == nil {
		return nil, errLiveIngestNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := requireOperator("operator_mid", req.OperatorMid); err != nil {
		return nil, err
	}
	if err := liveNonNeg("room_id", req.RoomId); err != nil {
		return nil, err
	}
	if err := liveNonNeg("anchor_mid", req.AnchorMid); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("state", req.State); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("pn", req.Pn); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("ps", req.Ps); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveIngest.ListStreamKeys(l.ctx, &liveingestrpc.ListStreamKeysReq{
		RoomId:      req.RoomId,
		AnchorMid:   req.AnchorMid,
		State:       liveingestrpc.StreamKeyState(req.State),
		Pn:          req.Pn,
		Ps:          req.Ps,
		OperatorMid: req.OperatorMid,
		Admin:       liveIngestAdminScope,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveStreamKeyList: operator_mid=%d room_id=%d state=%d err=%v",
			req.OperatorMid, req.RoomId, req.State, err)
		return nil, err
	}
	return &types.LiveStreamKeyListResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveStreamKeyListData{
			List:  liveStreamKeysToAPI(reply.GetKeys()),
			Total: reply.GetTotal(),
			Pn:    reply.GetPn(),
			Ps:    reply.GetPs(),
		},
		TTL: 0,
	}, nil
}
