package logic

import (
	"context"
	"fmt"

	"go-video/services/search-indexer/internal/svc"
	"go-video/services/search-indexer/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type DeleteContentDocLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDeleteContentDocLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteContentDocLogic {
	return &DeleteContentDocLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 下架/删除时移除或降级投影，并说明 CDN/搜索投影一致性处理。
//
// purge=false（默认）只把 state 置为不可检索并推进 doc_revision，
// 保证随后到达的旧版本事件不会把它「复活」；purge=true 才物理删除文档。
// 缓存与 CDN 失效由拥有事实的服务负责（README「下架链路」）。
func (l *DeleteContentDocLogic) DeleteContentDoc(in *rpc.DeleteContentDocReq) (*rpc.DeleteContentDocReply, error) {
	if in.ContentId <= 0 {
		return nil, fmt.Errorf("search-indexer: delete 需要有效 content_id，收到 %d", in.ContentId)
	}
	contentType := int32(in.ContentType)
	if contentType < 1 || contentType > 3 {
		// 文档主键是 <content_type>_<content_id>，没有类型就无法定位投影；
		// 这里报错而不是「三种类型都试一遍」，避免误删其它内容域的文档。
		return nil, fmt.Errorf("search-indexer: delete 需要有效 content_type（1/2/3），收到 %d", contentType)
	}

	res, err := l.svcCtx.Repository.DeleteContent(l.ctx, in.ContentId, contentType, in.Purge, in.Reason)
	if err != nil {
		l.Errorf("search-indexer/DeleteContentDoc: content_id=%d type=%d purge=%v reason=%s err=%v",
			in.ContentId, contentType, in.Purge, in.Reason, err)
		return nil, err
	}
	l.Infof("search-indexer/DeleteContentDoc: content_id=%d type=%d purge=%v reason=%s outcome=%s index=%s",
		in.ContentId, contentType, in.Purge, in.Reason, res.Outcome, res.Index)

	return &rpc.DeleteContentDocReply{
		Deleted: res.Deleted,
		Index:   res.Index,
		Outcome: res.Outcome,
	}, nil
}
