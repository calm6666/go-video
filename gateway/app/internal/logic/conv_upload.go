package logic

// 本文件是网关聚合层的类型转换（upload pb → gateway HTTP types）。
// 网关只做转换与聚合，不持有业务规则。

import (
	"go-video/gateway/app/internal/types"
	uploadrpc "go-video/services/upload/rpc"
)

func toUploadChunkParts(p []types.UploadChunkPart) []*uploadrpc.ChunkPart {
	out := make([]*uploadrpc.ChunkPart, 0, len(p))
	for _, v := range p {
		out = append(out, &uploadrpc.ChunkPart{
			ChunkNo: v.ChunkNo,
			Etag:    v.Etag,
		})
	}
	return out
}

func toUploadChunks(p []*uploadrpc.Chunk) []types.UploadChunk {
	out := make([]types.UploadChunk, 0, len(p))
	for _, v := range p {
		if v == nil {
			continue
		}
		out = append(out, types.UploadChunk{
			ChunkNo: v.ChunkNo,
			Size:    v.Size,
			Etag:    v.Etag,
			State:   int32(v.State),
			Ctime:   v.Ctime,
		})
	}
	return out
}

func toUploadInitData(p *uploadrpc.InitUploadReply) types.UploadInitData {
	if p == nil {
		return types.UploadInitData{}
	}
	return types.UploadInitData{
		UploadId:       p.GetUploadId(),
		Bucket:         p.GetBucket(),
		ObjectKey:      p.GetObjectKey(),
		UploadProtocol: p.GetUploadProtocol(),
		ChunkSize:      p.GetChunkSize(),
		TotalChunks:    p.GetTotalChunks(),
		Instant:        p.GetInstant(),
	}
}

func toUploadUrlData(p *uploadrpc.GetUrlReply) types.UploadUrlData {
	if p == nil {
		return types.UploadUrlData{}
	}
	return types.UploadUrlData{
		UploadId:   p.GetUploadId(),
		ChunkNo:    p.GetChunkNo(),
		Url:        p.GetUrl(),
		Method:     p.GetMethod(),
		Expiration: p.GetExpiration(),
		Headers:    p.GetHeaders(),
	}
}

func toUploadCompleteData(p *uploadrpc.CompleteUploadReply) types.UploadCompleteData {
	if p == nil {
		return types.UploadCompleteData{}
	}
	return types.UploadCompleteData{
		UploadId:  p.GetUploadId(),
		AssetId:   p.GetAssetId(),
		ObjectKey: p.GetObjectKey(),
		Size:      p.GetSize(),
		Md5:       p.GetMd5(),
		State:     int32(p.GetState()),
	}
}

func toUploadStatusData(p *uploadrpc.UploadStatusReply) types.UploadStatusData {
	if p == nil {
		return types.UploadStatusData{}
	}
	return types.UploadStatusData{
		UploadId:        p.GetUploadId(),
		State:           int32(p.GetState()),
		Size:            p.GetSize(),
		UploadedSize:    p.GetUploadedSize(),
		TotalChunks:     p.GetTotalChunks(),
		CompletedChunks: p.GetCompletedChunks(),
		Chunks:          toUploadChunks(p.GetChunks()),
		AssetId:         p.GetAssetId(),
	}
}
