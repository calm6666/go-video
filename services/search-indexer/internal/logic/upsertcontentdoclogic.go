package logic

import (
	"context"

	"go-video/services/search-indexer/internal/svc"
	"go-video/services/search-indexer/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpsertContentDocLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpsertContentDocLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpsertContentDocLogic {
	return &UpsertContentDocLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 按 content_id 写入/覆盖一条内容投影（上游显式触发或回填）。
//
// 一致性：写入前按 doc_revision 做 last-write-wins 守卫，旧版本返回 outcome=skipped_stale
// 而不是报错（迟到事件是预期路径）；OpenSearch/MySQL 失败一定返回错误，不伪装成功。
func (l *UpsertContentDocLogic) UpsertContentDoc(in *rpc.UpsertContentDocReq) (*rpc.UpsertContentDocReply, error) {
	doc, err := docFromRPC(in.Doc)
	if err != nil {
		return nil, err
	}
	// 请求体里的 content_id 与 doc.content_id 必须一致，避免「路由键与文档键不一致」的脏写。
	if in.ContentId != 0 && in.ContentId != doc.ContentID {
		return nil, errContentIDMismatch
	}

	res, err := l.svcCtx.Repository.UpsertDoc(l.ctx, doc, in.ForceOverwrite)
	if err != nil {
		l.Errorf("search-indexer/UpsertContentDoc: content_id=%d revision=%d source=%s request_id=%s err=%v",
			doc.ContentID, doc.DocRevision, in.Source, in.RequestId, err)
		return nil, err
	}
	l.Infof("search-indexer/UpsertContentDoc: content_id=%d outcome=%s index=%s source=%s request_id=%s",
		doc.ContentID, res.Outcome, res.Index, in.Source, in.RequestId)

	return &rpc.UpsertContentDocReply{
		ContentId:   doc.ContentID,
		Index:       res.Index,
		Outcome:     res.Outcome,
		DocRevision: res.DocRevision,
		TookMs:      res.TookMs,
	}, nil
}
