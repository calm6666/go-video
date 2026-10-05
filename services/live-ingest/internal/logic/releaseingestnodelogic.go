package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/live-ingest/internal/svc"
	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// defaultReleaseReason 是调用方没给理由时写入分配记录的释放原因。
// 留空会让容量对账看不出「这条租约是被谁放掉的」，而编一个具体理由属于伪造审计，
// 所以用一个既不空也不假装具体的稳定值。
const defaultReleaseReason = "released_without_reason"

type ReleaseIngestNodeLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewReleaseIngestNodeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ReleaseIngestNodeLogic {
	return &ReleaseIngestNodeLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 释放流的节点占用（停流或运维摘流）
//
// 三条不可省的点：
//  1. 幂等锚点是「state=ACTIVE 的租约本身」，不是 request_id：
//     NodeAssignment.Release 是 state=ACTIVE 条件 UPDATE，两次释放只有一次改到行，
//     第二次必然落到 released=false。live_node_assignment.request_id 存的是「分配」的
//     幂等键（uniq_request_id），不能再拿来承载释放动作，因此本接口的 replayed 恒为
//     false——要如实回答「这次是不是重放」需要给分配记录加释放幂等列，见 README「已知缺口」；
//  2. 带 node_id 时它是护栏而不是过滤器：当前生效租约不在这台节点上就报
//     ErrLeaseNotHeld 且零副作用，绝不「顺手」把别人占的配额扣掉；
//  3. 分配记录、节点配额与流上的 node_id 指针三处必须同事务改：
//     少改任一处，就会出现「记录说没占用但节点计数还挂着」的容量漂移，
//     或「节点已空载但流还指向它」的错误下发地址。
//
// 已停的流通常在这里拿到 released=false——停流事务（applyStreamTransition）
// 已经归还过配额，这是正确终态而不是失败，不能让它以错误形式打扰调用方。
func (l *ReleaseIngestNodeLogic) ReleaseIngestNode(in *rpc.ReleaseIngestNodeReq) (*rpc.ReleaseIngestNodeReply, error) {
	repo := l.svcCtx.Repository
	if repo == nil {
		return nil, errNoRepository
	}
	streamID, err := checkStreamID(in.StreamId)
	if err != nil {
		return nil, err
	}
	requestID, err := checkRequestID(in.RequestId)
	if err != nil {
		return nil, err
	}
	nodeID := ""
	if trimmed := strings.TrimSpace(in.NodeId); trimmed != "" {
		if nodeID, err = checkNodeID(trimmed); err != nil {
			return nil, err
		}
	}
	reason, err := checkAssignmentReason(in.Reason)
	if err != nil {
		return nil, err
	}
	if reason == "" {
		reason = defaultReleaseReason
	}

	var (
		releasedNode string
		didRelease   bool
	)
	err = repo.Conn().TransactCtx(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		s, err := repo.Stream.LockByID(ctx, tx, streamID)
		if err != nil {
			return err
		}
		if s == nil {
			return model.ErrStreamNotFound
		}
		active, err := repo.NodeAssignment.FindActiveByStream(ctx, tx, streamID)
		if err != nil {
			return err
		}
		if active == nil {
			// 没有生效租约：幂等成功（可能已被停流事务或上一次释放归还），不动任何行。
			return nil
		}
		if nodeID != "" && nodeID != active.NodeID {
			return fmt.Errorf("%w: 当前生效租约属于另一个节点", model.ErrLeaseNotHeld)
		}
		at := nowUnix()
		ok, err := repo.NodeAssignment.Release(ctx, tx, active.AssignmentID,
			model.AssignmentStateReleased, at, reason, active.NodeID)
		if err != nil {
			return err
		}
		if !ok {
			// 锁内读到了 ACTIVE 行却没改到：同事务里被并发迁移/释放抢走，回滚重来。
			return model.ErrConcurrentUpdate
		}
		quota, err := repo.IngestNode.ReleaseQuota(ctx, tx, active.NodeID)
		if err != nil {
			return err
		}
		if !quota {
			// 租约生效说明配额当初占到过，退不动只能是节点行被删或计数已被清零：
			// 这是容量账本的断裂，必须让运维看见，而不是悄悄把账做平。
			return fmt.Errorf("%w: 节点配额无法归还（行不存在或计数已清零）", model.ErrNodeNotFound)
		}
		if s.NodeID != "" {
			updated, err := repo.Stream.SetNode(ctx, tx, streamID, "", s.NodeID)
			if err != nil {
				return err
			}
			if !updated {
				return model.ErrConcurrentUpdate
			}
		}
		if s.NodeID != "" && s.NodeID != active.NodeID {
			// 指针与租约不一致（历史脏数据或并发迁移留下的一帧）：留 warn 供对账。
			l.Logger.Errorw("node assignment drift between stream and lease",
				logx.Field("module", "live-ingest"), logx.Field("op", "release_ingest_node"),
				logx.Field("stream_id", streamID), logx.Field("lease_node", active.NodeID),
				logx.Field("request_id", requestID))
		}
		releasedNode, didRelease = active.NodeID, true
		return nil
	})
	if err != nil {
		return nil, err
	}
	if didRelease {
		l.Logger.Infow("release ingest node",
			logx.Field("module", "live-ingest"), logx.Field("op", "release_ingest_node"),
			logx.Field("stream_id", streamID), logx.Field("node_id", releasedNode),
			logx.Field("request_id", requestID))
	}
	return &rpc.ReleaseIngestNodeReply{
		Released: didRelease,
		NodeId:   releasedNode,
		// 无释放幂等列可依据，这里不谎报命中重放（见函数头与 README「已知缺口」）。
		Replayed: false,
	}, nil
}
