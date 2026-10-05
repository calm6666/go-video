package logic

// 本文件是网关聚合层的类型转换（video pb → gateway HTTP types）。
// 网关只做转换与聚合，不持有业务规则。

import (
	"go-video/gateway/app/internal/types"
	videorpc "go-video/services/video/rpc"
)

func toVideoSubmission(p *videorpc.Submission) types.VideoSubmission {
	if p == nil {
		return types.VideoSubmission{}
	}
	return types.VideoSubmission{
		Aid:    p.Aid,
		Mid:    p.Mid,
		Title:  p.Title,
		Desc:   p.Desc,
		Cover:  p.Cover,
		Typeid: p.Typeid,
		Tag:    p.Tag,
		State:  int32(p.State),
		Ctime:  p.Ctime,
		Mtime:  p.Mtime,
	}
}

func toVideoSubmissions(p []*videorpc.Submission) []types.VideoSubmission {
	out := make([]types.VideoSubmission, 0, len(p))
	for _, v := range p {
		if v == nil {
			continue
		}
		out = append(out, toVideoSubmission(v))
	}
	return out
}
