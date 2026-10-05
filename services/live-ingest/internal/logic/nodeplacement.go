// 本文件是 logic 包的手写扩展（接入节点打分的纯函数部分与节点侧列宽校验），
// 不是 goctl 生成产物。
//
// 为什么单独成文件：打分与排序是「为什么把这条流放到这个节点」的唯一解释口径，
// 必须可单测、可复算（同输入同结果），且不碰 SQL。SQL、CAS 与事务边界留在 model，
// 候选行的取数与配额抢占留在 AssignIngestNode。
//
// 打分刻意不使用随机扰动或时间项：一旦引入，重放同一请求可能拿到不同节点，
// 「同一 request_id 返回同一分配」的幂等承诺就失效了。
package logic

import (
	"fmt"
	"strings"

	"go-video/services/live-ingest/model"
)

// 节点相关列宽（与 deploy/migrations/live-ingest/000001_create_live_ingest_key_tables.sql 对齐）。
const (
	// maxRegionBytes 对应 live_ingest_node.region VARCHAR(16)。
	maxRegionBytes = 16
	// maxNodeNameBytes 对应 live_ingest_node.name VARCHAR(64)。
	maxNodeNameBytes = 64
	// maxLabelsBytes 对应 live_ingest_node.labels VARCHAR(255)。
	maxLabelsBytes = 255
	// maxEndpointBytes 对应 live_ingest_node.endpoint_* VARCHAR(191)。
	maxEndpointBytes = 191
	// maxReasonBytes 对应 live_node_assignment.reason / release_reason VARCHAR(255)。
	maxReasonBytes = 255
	// assignCandidateScanLimit 是单次分配最多打分的候选节点数：
	// 候选按健康分预排，前若干名足以定出结果，避免为一场分配扫全表。
	assignCandidateScanLimit int32 = 20
)

// 打分参数：三项相加即 score 的全部来源，任何调整都会改变调度结果，
// 因此常量而非魔法数字（并写进分配记录，事后能从 score 反推决策）。
const (
	// assignRegionBonus 命中调用方期望区域的加分（就近优先，但不凌驾健康分）。
	assignRegionBonus = 25
	// assignLoadPenaltyDivisor 负载率惩罚除数：负载率每 1% 扣 1/2 分，
	// 满负载（本不该出现在候选里）扣 50 分，足以让健康分 100 的满节点输给空载节点。
	assignLoadPenaltyDivisor = 2
	// assignFullLoadPenalty 配额信息缺失（capacity<=0）时按满负载计入惩罚。
	assignFullLoadPenalty = 100
)

// checkRegion 归一区域码：列宽 VARCHAR(16)，超长或含空白直接拒绝。
// 区域是分配条件，静默截断会让「CN-EAST-1」变成「CN-EAST」，从此永远匹配不上。
func checkRegion(region string) (string, error) {
	trimmed := strings.TrimSpace(region)
	if trimmed == "" {
		return "", nil
	}
	if len(trimmed) > maxRegionBytes || strings.ContainsAny(trimmed, " /\t\n") {
		return "", fmt.Errorf("%w: region 不是合法区域码", model.ErrNodeNotFound)
	}
	return trimmed, nil
}

// checkNodeTextField 归一节点侧自由文本（名称/标签/接入地址）到列宽内。
// 这些字段只用于展示与下发，超长按字节截断而不是让整次注册失败；
// 但截断在 logic 完成，保证「落库的就是回显的」。
func checkNodeTextField(name string, max int) string {
	trimmed := strings.TrimSpace(name)
	if len(trimmed) > max {
		return trimmed[:max]
	}
	return trimmed
}

// checkAssignmentReason 校验分配/释放原因文本（列宽 255，与 maxReasonRunes 的 250 取交集）。
func checkAssignmentReason(reason string) (string, error) {
	trimmed := strings.TrimSpace(reason)
	if trimmed == "" {
		return "", nil
	}
	if runeLen(trimmed) > maxReasonRunes {
		return "", fmt.Errorf("%w: reason %d > %d", model.ErrReasonTooLong,
			runeLen(trimmed), maxReasonRunes)
	}
	return trimmed, nil
}

// nodeLoadPercent 返回节点的负载率（0~100）。配额缺失或为 0 视为满负载：
// 没有容量声明的节点不该被优先选中，而不是被当成「空载」吸走全部流量。
func nodeLoadPercent(n *model.IngestNode) int32 {
	if n == nil || n.CapacityStreams <= 0 {
		return assignFullLoadPenalty
	}
	load := n.ActiveStreams * 100 / n.CapacityStreams
	if load < 0 {
		return 0
	}
	if load > 100 {
		// active_streams 是派生计数，允许与分配记录短暂偏差（可能瞬时超卖），
		// 打分按 100% 封顶，不给出负分这种无法解释的值。
		return 100
	}
	return load
}

// nodeAssignScore 计算单个候选节点的分配得分（可负，仅用于同批候选之间比较）。
//
// score = 健康分 - 负载率/2 + （命中期望区域 ? 25 : 0）
// sameRegionRequired 为 true 且给了期望区域时，区域不符的节点直接判不可用（返回极小值），
// 因为「强制就近」是硬约束而不是偏好；调用方据此过滤，而不是靠分数排序碰运气。
func nodeAssignScore(n *model.IngestNode, preferRegion string, sameRegionRequired bool) int32 {
	if n == nil {
		return 0
	}
	score := n.HealthScore - nodeLoadPercent(n)/assignLoadPenaltyDivisor
	if preferRegion == "" {
		return score
	}
	if strings.EqualFold(strings.TrimSpace(n.Region), preferRegion) {
		return score + assignRegionBonus
	}
	if sameRegionRequired {
		return assignScoreUnusable
	}
	return score
}

// assignScoreUnusable 是「硬约束不满足」的哨兵分：远小于任何正常打分，
// 排序后必然垫底，调用方另用 assignNodeUsable 直接剔除，不让它参与竞争。
const assignScoreUnusable = -1000

// assignNodeUsable 判断候选是否满足硬约束（当前只有「强制同区域且区域不符」）。
func assignNodeUsable(score int32) bool {
	return score > assignScoreUnusable
}

// scoredCandidate 是 rankIngestNodes 的排序元素：把「谁得几分」和「谁是谁」绑在一起，
// 避免两个并行切片在下标错位时产出静默错乱。
type scoredCandidate struct {
	node  *model.IngestNode
	score int32
}

// rankIngestNodes 按得分降序排定候选顺序，并保证同分时顺序确定：
// 得分 → 负载率升序（先放空载）→ node_id 升序（字典序兜底）。
// 不排序同分节点会让同一批请求在两个节点之间来回抖动，主播重连被踢到不同机房。
func rankIngestNodes(nodes []*model.IngestNode, preferRegion string, sameRegionRequired bool) []*model.IngestNode {
	ranked := make([]scoredCandidate, 0, len(nodes))
	for _, n := range nodes {
		if n == nil {
			continue
		}
		s := nodeAssignScore(n, preferRegion, sameRegionRequired)
		if !assignNodeUsable(s) {
			continue
		}
		ranked = append(ranked, scoredCandidate{node: n, score: s})
	}
	// 插入排序：候选数量被 assignCandidateScanLimit 夹在几十以内，
	// 不必引入 sort 之外的复杂度，且比较逻辑要和 model 的预排顺序保持显式一致。
	for i := 1; i < len(ranked); i++ {
		for j := i; j > 0 && lessCandidate(ranked[j-1], ranked[j]); j-- {
			ranked[j-1], ranked[j] = ranked[j], ranked[j-1]
		}
	}
	out := make([]*model.IngestNode, 0, len(ranked))
	for _, r := range ranked {
		out = append(out, r.node)
	}
	return out
}

// lessCandidate 返回 a 是否应排在 b 之后（用于把更优的往前冒泡）。
func lessCandidate(a, b scoredCandidate) bool {
	if a.score != b.score {
		return a.score < b.score
	}
	la, lb := nodeLoadPercent(a.node), nodeLoadPercent(b.node)
	if la != lb {
		return la > lb
	}
	return a.node.NodeID > b.node.NodeID
}

// findIngestNode 在候选里找指定节点（prefer_node_id 命中时优先返回它，
// 重连回到原节点可以避免重新建联与冷启动转码）。找不到说明该节点不满足
// 协议/在线/配额条件，由调用方决定是降级到打分还是直接报错。
func findIngestNode(nodes []*model.IngestNode, nodeID string) *model.IngestNode {
	if nodeID == "" {
		return nil
	}
	for _, n := range nodes {
		if n != nil && n.NodeID == nodeID {
			return n
		}
	}
	return nil
}

// nodeIDOf 安全取节点 ID（候选行可能为 nil，不能让调度路径 panic 拖垮整个 RPC）。
func nodeIDOf(n *model.IngestNode) string {
	if n == nil {
		return ""
	}
	return n.NodeID
}
