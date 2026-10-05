package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/live-ingest/internal/repository"
	"go-video/services/live-ingest/internal/svc"
	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type AssignIngestNodeLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAssignIngestNodeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AssignIngestNodeLogic {
	return &AssignIngestNodeLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 为流分配接入节点（就近 + 配额 + 健康分打分；支持迁移）
//
// 五条不可省的点：
//  1. 分配记录与节点配额必须同事务：分开做「记录写了、配额没占」就是超卖，
//     「配额占了、记录没写」则是永久漏计（ListCandidates 只看 active_streams）；
//  2. request_id 是本接口唯一的重放锚点（live_node_assignment.uniq_request_id）。
//     命中已有分配直接回放，且要求它归属同一条流——同一个幂等键跨流复用必须拒；
//  3. 节点协议必须与流自身的接入协议一致：流是 RTMP 建连的，把它指向 SRT 端口
//     只会让主播看到「推流地址连不上」，所以这里硬判 ErrNodeProtocolMismatch；
//  4. 迁移顺序是「先抢新配额、后退旧配额」：反过来的话，新节点抢不到配额时
//     旧租约已经被释放，流会同时失去新旧两个节点的占用（更严重的超卖方向是漏计）；
//  5. 候选节点的配额抢占失败就换下一个，而不是回滚整次分配——只有全部候选都满
//     才报 ErrNoAvailableNode，这是并发分配的常态而不是异常。
//
// 硬约束与偏好：Cdn.DefaultAssignSameRegion=true 时期望区域是硬约束（候选直接剔除
// 异区域节点，而不是靠加分碰运气）；false 时只加分 25（见 nodeplacement.go）。
// prefer_node_id 是硬要求：指定的节点不可用就报错，绝不静默换节点——
// 入口拿「回到原节点」的承诺给客户端下发了地址，换节点会让那条地址白连一次。
func (l *AssignIngestNodeLogic) AssignIngestNode(in *rpc.AssignIngestNodeReq) (*rpc.AssignIngestNodeReply, error) {
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
	protocolMask, err := singleProtocolMask(in.Protocol)
	if err != nil {
		return nil, err
	}
	preferRegion, err := checkRegion(in.PreferRegion)
	if err != nil {
		return nil, err
	}
	preferNode := ""
	if trimmed := strings.TrimSpace(in.PreferNodeId); trimmed != "" {
		if preferNode, err = checkNodeID(trimmed); err != nil {
			return nil, err
		}
	}
	reason, err := checkAssignmentReason(in.Reason)
	if err != nil {
		return nil, err
	}
	traceID := sanitizeTraceID(in.TraceId)
	sameRegionRequired := l.svcCtx.Config.Cdn.DefaultAssignSameRegion && preferRegion != ""

	// 重放判定放在事务前：uniq_request_id 命中就是「同一个请求第二次到达」，
	// 此时不该再产生任何副作用，也不该占用新的配额。
	existing, err := repo.NodeAssignment.FindByRequest(l.ctx, requestID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if existing.StreamID != streamID {
			return nil, fmt.Errorf("%w: request_id 已用于另一条流的分配", model.ErrIdempotencyKeyRequired)
		}
		return l.assignmentReply(existing, true, "命中 request_id，返回首次分配结果")
	}

	// 候选集在事务外取快照：配额是 CAS 抢占的，快照旧了只会让本次多试一个节点，
	// 不会超卖；把候选查询放进事务反而会拉长 live_ingest_node 的锁范围。
	// 区域条件只在硬约束时才下推到 SQL——否则「偏好同区域」会变成「只能同区域」，
	// 异区域的健康节点被提前剔除，加分逻辑就没有意义了。
	scanRegion := ""
	if sameRegionRequired {
		scanRegion = preferRegion
	}
	candidates, err := repo.IngestNode.ListCandidates(l.ctx, protocolMask, scanRegion, assignCandidateScanLimit)
	if err != nil {
		return nil, err
	}
	order := rankIngestNodes(candidates, preferRegion, sameRegionRequired)
	if preferNode != "" {
		order, err = prioritizeNode(l.ctx, repo, order, preferNode, protocolMask)
		if err != nil {
			return nil, err
		}
	}

	var (
		out         *rpc.AssignIngestNodeReply
		chosenNode  *model.IngestNode
		chosenID    int64
		chosenScore int32
		movedFrom   string
	)
	err = repo.Conn().TransactCtx(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		s, err := repo.Stream.LockByID(ctx, tx, streamID)
		if err != nil {
			return err
		}
		if s == nil {
			return model.ErrStreamNotFound
		}
		if s.State == model.StreamStateStopped {
			return model.ErrTerminalStream
		}
		// 流的接入协议是建连时定死的：不一致说明调用方在拿另一条流的协议要节点。
		if s.Protocol != int32(in.Protocol) {
			return fmt.Errorf("%w: 流已按另一种协议接入，不能分配到该协议的节点", model.ErrNodeProtocolMismatch)
		}
		active, err := repo.NodeAssignment.FindActiveByStream(ctx, tx, streamID)
		if err != nil {
			return err
		}
		if active != nil && !in.ForceReassign {
			// 重连的常态：已有生效租约就直接回当前节点，一次分配只占一份配额。
			out, err = l.assignmentReply(active, false, "流已有生效分配，返回当前节点")
			return err
		}
		if len(order) == 0 {
			return model.ErrNoAvailableNode
		}

		at := nowUnix()
		prevNodeID := ""
		if active != nil {
			prevNodeID = active.NodeID
		}
		for _, cand := range order {
			if cand == nil || cand.NodeID == prevNodeID {
				// 迁移不选回原节点：那等于没迁移，还会把旧租约迁到自己身上。
				continue
			}
			reserved, err := repo.IngestNode.ReserveQuota(ctx, tx, cand.NodeID)
			if err != nil {
				return err
			}
			if !reserved {
				// 配额被并发抢先（或节点刚被置 OFFLINE）：换下一个候选。
				continue
			}
			if active != nil {
				released, err := repo.NodeAssignment.Release(ctx, tx, active.AssignmentID,
					model.AssignmentStateMigrated, at, reason, active.NodeID)
				if err != nil {
					return err
				}
				if !released {
					return model.ErrConcurrentUpdate
				}
				quota, err := repo.IngestNode.ReleaseQuota(ctx, tx, active.NodeID)
				if err != nil {
					return err
				}
				if !quota {
					// 旧租约刚被 CAS 释放却无配额可退：节点行被删了。
					// 回滚让运维看得见，而不是把节点占用悄悄记少（后续必然超卖）。
					return fmt.Errorf("%w: 迁移来源节点的配额已清空", model.ErrNodeNotFound)
				}
			}
			score := nodeAssignScore(cand, preferRegion, sameRegionRequired)
			id, err := repo.NodeAssignment.Insert(ctx, tx, &model.NodeAssignment{
				RequestID: requestID, StreamID: streamID, RoomID: s.RoomID, KeyID: s.KeyID,
				NodeID: cand.NodeID, Protocol: s.Protocol, State: model.AssignmentStateActive,
				Score: score, PrevNodeID: prevNodeID, Reason: assignReasonText(reason, cand, score),
				AssignedAt: at, TraceID: traceID,
			})
			if err != nil {
				return err
			}
			// node_id 用「读到的当前值」做 CAS 前置：并发迁移只能有一个成功改写流行。
			updated, err := repo.Stream.SetNode(ctx, tx, streamID, cand.NodeID, s.NodeID)
			if err != nil {
				return err
			}
			if !updated {
				return model.ErrConcurrentUpdate
			}
			chosenNode, chosenID, chosenScore, movedFrom = cand, id, score, prevNodeID
			return nil
		}
		return model.ErrNoAvailableNode
	})
	if err != nil {
		if model.IsDuplicate(err) {
			// uniq_request_id 被并发重放抢先：回放对手建出来的那条分配，
			// 绝不第二次占配额。
			winner, findErr := repo.NodeAssignment.FindByRequest(l.ctx, requestID)
			if findErr != nil {
				return nil, findErr
			}
			if winner != nil {
				return l.assignmentReply(winner, true, "并发重放：返回首次分配结果")
			}
		}
		return nil, err
	}
	if out != nil {
		return out, nil
	}
	if chosenNode == nil {
		// 事务提交但没落地分配：只可能是候选全部命中「迁移不选回原节点」的跳过分支。
		// 到这里不能回零值成功（入口会把流指向一个空节点）。
		return nil, model.ErrNoAvailableNode
	}

	message := "已分配接入节点"
	if movedFrom != "" {
		message = "已迁移到新的接入节点，旧节点配额已同事务归还"
	}
	reply, err := l.assignmentReply(&model.NodeAssignment{
		AssignmentID: chosenID,
		NodeID:       chosenNode.NodeID,
		Score:        chosenScore,
		PrevNodeID:   movedFrom,
	}, false, message)
	if err != nil {
		return nil, err
	}
	l.Logger.Infow("assign ingest node",
		logx.Field("module", "live-ingest"), logx.Field("op", "assign_ingest_node"),
		logx.Field("stream_id", streamID), logx.Field("node_id", chosenNode.NodeID),
		logx.Field("assignment_id", chosenID), logx.Field("prev_node_id", movedFrom),
		logx.Field("request_id", requestID))
	return reply, nil
}

// assignmentReply 组装「已有/刚建分配」形态的响应。
// 节点行重新读一次：回显的是当前接入地址与实时占用，而不是候选快照上的旧计数。
func (l *AssignIngestNodeLogic) assignmentReply(a *model.NodeAssignment, replayed bool, message string) (*rpc.AssignIngestNodeReply, error) {
	if a == nil {
		return nil, model.ErrAssignmentNotFound
	}
	node, err := l.svcCtx.Repository.IngestNode.FindOne(l.ctx, a.NodeID)
	if err != nil {
		return nil, err
	}
	return &rpc.AssignIngestNodeReply{
		NodeId:       a.NodeID,
		Node:         nodeInfo(node),
		AssignmentId: a.AssignmentID,
		Score:        a.Score,
		PrevNodeId:   a.PrevNodeID,
		Replayed:     replayed,
		Message:      message,
	}, nil
}

// prioritizeNode 把调用方指定的节点提到候选队首。
// 它不在候选里时不静默忽略，而是回查节点行给出可归因的原因：
// 不在线/满配额/未注册是三种完全不同的运维动作，混成一条「没有可用节点」等于没报。
func prioritizeNode(
	ctx context.Context,
	repo *repository.Repository,
	order []*model.IngestNode,
	nodeID string,
	protocolMask uint32,
) ([]*model.IngestNode, error) {
	if findIngestNode(order, nodeID) != nil {
		return moveNodeToFront(order, nodeID), nil
	}
	node, err := repo.IngestNode.FindOne(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	if node == nil {
		return nil, model.ErrNodeNotFound
	}
	if node.ProtocolMask&protocolMask == 0 {
		return nil, model.ErrNodeProtocolMismatch
	}
	return nil, model.ErrNodeNotAssignable
}

// moveNodeToFront 把命中的节点移到队首，保持其余相对顺序（打分顺序不能被打乱）。
func moveNodeToFront(nodes []*model.IngestNode, nodeID string) []*model.IngestNode {
	out := make([]*model.IngestNode, 0, len(nodes))
	var picked *model.IngestNode
	for _, n := range nodes {
		if n != nil && n.NodeID == nodeID {
			picked = n
			continue
		}
		out = append(out, n)
	}
	if picked == nil {
		return nodes
	}
	return append([]*model.IngestNode{picked}, out...)
}

// assignReasonText 生成「为何选它」的可读归因文本，写入分配记录的 reason 列。
// 调用方给了原因就以它为准（审计要与操作者说的一致），没给才补打分快照。
func assignReasonText(reason string, node *model.IngestNode, score int32) string {
	if reason != "" {
		return reason
	}
	if node == nil {
		return ""
	}
	text := fmt.Sprintf("auto: node=%s score=%d health=%d load=%d%%",
		node.NodeID, score, node.HealthScore, nodeLoadPercent(node))
	if len(text) > maxReasonBytes {
		return text[:maxReasonBytes]
	}
	return text
}
