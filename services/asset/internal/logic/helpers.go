package logic

import (
	"go-video/services/asset/model"
	"go-video/services/asset/rpc"
)

// assetMetaToReply 把 model.AssetMeta 转换为 rpc.AssetReply。
// model.State 与 rpc.AssetState 数值一致（均按 proto 枚举值定义），可直接强转。
func assetMetaToReply(m *model.AssetMeta) *rpc.AssetReply {
	if m == nil {
		return nil
	}
	return &rpc.AssetReply{
		AssetId:   m.AssetID,
		UploadId:  m.UploadID,
		Mid:       m.Mid,
		Bucket:    m.Bucket,
		ObjectKey: m.ObjectKey,
		Size:      m.Size,
		Md5:       m.Md5,
		Duration:  m.Duration,
		Width:     m.Width,
		Height:    m.Height,
		Codec:     m.Codec,
		State:     rpc.AssetState(m.State),
		Ctime:     m.Ctime,
		Mtime:     m.Mtime,
	}
}
