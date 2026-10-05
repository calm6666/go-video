package model

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// 本文件放排序审计所需的纯函数（哈希与摘要），不碰 IO，可离线单测。
//
// 为什么需要它们：排序结果只回传 aid 列表会丢掉「为什么是这些、按什么顺序」的证据。
// 本服务用 DigestAids 把有序候选压成一个 sha256 摘要写进 rank_decision_log，
// 运营/算法用同一函数重放即可判断结果是否被篡改或凭空多出 aid；
// BucketOf 则保证「同一主体对同一实验」在任意实例、任意时刻得到同一桶号。

// aidDigestSeparator 是 aid 序列的连接符。
// 契约（rpc/rank.proto RankCandidatesReply.result_digest）明确写死 "|"，改它等于改历史摘要口径。
const aidDigestSeparator = "|"

// DigestAids 计算有序 aid 列表的 sha256 摘要（hex，64 位）。
// 顺序敏感：[1,2] 与 [2,1] 摘要不同，因此能证明「排序结果」而不是「结果集合」。
// 空列表返回 sha256("")，与「未写入摘要」的 NULL/空串可区分（调用方按需判空）。
func DigestAids(aids []int64) string {
	var sb strings.Builder
	for i, aid := range aids {
		if i > 0 {
			sb.WriteString(aidDigestSeparator)
		}
		sb.WriteString(strconv.FormatInt(aid, 10))
	}
	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:])
}

// JoinAids 渲染 aid 序列为 csv，用于 rank_decision_log.top_aids（人工排障直接可读）。
// 截断（只存前 N 个）由调用方按配置 MaxDigestAids 决定，本函数不做策略。
func JoinAids(aids []int64) string {
	if len(aids) == 0 {
		return ""
	}
	parts := make([]string, 0, len(aids))
	for _, aid := range aids {
		parts = append(parts, strconv.FormatInt(aid, 10))
	}
	return strings.Join(parts, ",")
}

// SplitAids 拆分 top_aids csv（忽略空项与非法项），审计读回显用。
func SplitAids(s string) []int64 {
	if s == "" {
		return nil
	}
	out := make([]int64, 0, 8)
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		aid, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			continue
		}
		out = append(out, aid)
	}
	return out
}

// BucketOf 计算主体在某个哈希盐下的稳定桶号，返回 [0, bucketCount)。
//
// 输入按 "|subject_type|subject_id|exp_key|hash_seed" 拼接后取 sha256，
// 前 8 字节做无符号大端序取模，保证：
//   - 跨进程、跨语言可重放（不依赖 Go map 随机序或进程内哈希种子）；
//   - 同一主体对同一实验永远同桶（实验分桶可审计的前提）；
//   - 换 hash_seed 即整体重分桶（所以 RUNNING 变体的 seed 不可热改，见 RankExperimentModel）。
//
// bucketCount <= 0 时按 DefaultBucketCount（1000 分位）处理。
func BucketOf(subjectType int32, subjectID, expKey, hashSeed string, bucketCount int32) int32 {
	if bucketCount <= 0 {
		bucketCount = DefaultBucketCount
	}
	sum := sha256.Sum256([]byte(BucketMaterial(subjectType, subjectID, expKey, hashSeed)))
	// 大端取前 8 字节，避免 int64 符号位影响取模结果。
	v := uint64(sum[0])<<56 | uint64(sum[1])<<48 | uint64(sum[2])<<40 | uint64(sum[3])<<32 |
		uint64(sum[4])<<24 | uint64(sum[5])<<16 | uint64(sum[6])<<8 | uint64(sum[7])
	return int32(v % uint64(bucketCount))
}

// BucketMaterial 返回分桶哈希的原文，写进日志便于人工复算（不含任何用户敏感字段，
// subject_id 本身已是 mid 或设备 sha256 摘要）。
func BucketMaterial(subjectType int32, subjectID, expKey, hashSeed string) string {
	var sb strings.Builder
	sb.WriteString("rk.bucket|")
	sb.WriteString(strconv.Itoa(int(subjectType)))
	sb.WriteString(aidDigestSeparator)
	sb.WriteString(subjectID)
	sb.WriteString(aidDigestSeparator)
	sb.WriteString(expKey)
	sb.WriteString(aidDigestSeparator)
	sb.WriteString(hashSeed)
	return sb.String()
}

// ScaleBucket 把固定分桶空间（from，通常是落库口径 BucketCount=1000）的桶号
// 线性换算到另一空间（to），用于 rpc GetExperimentAssignmentReq.bucket_count 与
// 落库口径不一致时回显。落库永远存归一后的 from 空间桶号，避免同一主体产生多行记录。
func ScaleBucket(bucketNo, from, to int32) int32 {
	if bucketNo < 0 {
		bucketNo = 0
	}
	if from <= 0 {
		from = DefaultBucketCount
	}
	if to <= 0 || to == from {
		return bucketNo
	}
	scaled := int64(bucketNo) * int64(to) / int64(from)
	if scaled >= int64(to) {
		scaled = int64(to) - 1 // 兜底：bucketNo 越界时也不返回 out-of-range 桶号
	}
	return int32(scaled)
}

// NewDecisionID 生成排序决策审计 ID：32 位 hex（128 bit 随机）。
// 不用自增 ID 对外暴露，是为了让 decision_id 可以直接进客户端日志而不泄露流量规模。
// 随机源不可用时退化到 sha256(request_id|time)，同样能稳定回查，不影响可审计性。
func NewDecisionID(requestID string, at int64) string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err == nil {
		return hex.EncodeToString(buf[:])
	}
	sum := sha256.Sum256([]byte(requestID + aidDigestSeparator + strconv.FormatInt(at, 10)))
	return hex.EncodeToString(sum[:16])
}
