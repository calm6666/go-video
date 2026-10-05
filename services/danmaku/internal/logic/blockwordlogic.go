package logic

import (
	"context"
	"strings"
	"unicode/utf8"

	"go-video/services/danmaku/internal/svc"
	"go-video/services/danmaku/model"
	"go-video/services/danmaku/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// maxBlockWordLen 是屏蔽词最大字符数，与 danmaku_blockword.word 列宽一致。
const maxBlockWordLen = 64

type BlockWordLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewBlockWordLogic(ctx context.Context, svcCtx *svc.ServiceContext) *BlockWordLogic {
	return &BlockWordLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// BlockWord 运营侧屏蔽词管理（新增/停用/删除）。
//
// 权限：operator_mid 必须 > 0，由 gateway/admin 完成 RBAC 校验后注入；
// 本服务不解析管理员 token，但拒绝匿名写。
// 词库变更会立即失效 Redis 缓存，生效延迟受 BlockWordCacheTTLSeconds 约束。
func (l *BlockWordLogic) BlockWord(in *rpc.BlockWordReq) (*rpc.BlockWordReply, error) {
	if in.OperatorMid <= 0 {
		return nil, model.ErrOperatorRequired
	}
	word := strings.TrimSpace(in.Word)
	if word == "" || utf8.RuneCountInString(word) > maxBlockWordLen {
		return nil, model.ErrInvalidBlockWord
	}
	scope, oid, err := normalizeBlockScope(in.Scope, in.Oid)
	if err != nil {
		return nil, err
	}

	switch in.Action {
	case rpc.BlockWordAction_BLOCK_WORD_ADD:
		id, err := l.svcCtx.Repository.UpsertBlockWord(l.ctx, &model.BlockWord{
			Word:     word,
			Scope:    scope,
			Oid:      oid,
			State:    model.BlockWordEnabled,
			Operator: in.OperatorMid,
		})
		if err != nil {
			l.Errorf("danmaku/BlockWord: upsert word=%s operator=%d err=%v", word, in.OperatorMid, err)
			return nil, err
		}
		return &rpc.BlockWordReply{WordId: id, State: model.BlockWordEnabled}, nil

	case rpc.BlockWordAction_BLOCK_WORD_DISABLE:
		existing, err := l.svcCtx.Repository.GetBlockWord(l.ctx, word)
		if err != nil {
			return nil, err
		}
		if existing == nil {
			return nil, model.ErrBlockWordNotFound
		}
		ok, err := l.svcCtx.Repository.DisableBlockWord(l.ctx, word, in.OperatorMid, existing.Oid)
		if err != nil {
			l.Errorf("danmaku/BlockWord: disable word=%s err=%v", word, err)
			return nil, err
		}
		if !ok {
			return nil, model.ErrBlockWordNotFound
		}
		return &rpc.BlockWordReply{WordId: existing.WordID, State: model.BlockWordDisabled}, nil

	case rpc.BlockWordAction_BLOCK_WORD_DELETE:
		existing, err := l.svcCtx.Repository.GetBlockWord(l.ctx, word)
		if err != nil {
			return nil, err
		}
		if existing == nil {
			return nil, model.ErrBlockWordNotFound
		}
		ok, err := l.svcCtx.Repository.DeleteBlockWord(l.ctx, word, existing.Oid)
		if err != nil {
			l.Errorf("danmaku/BlockWord: delete word=%s err=%v", word, err)
			return nil, err
		}
		if !ok {
			return nil, model.ErrBlockWordNotFound
		}
		return &rpc.BlockWordReply{WordId: existing.WordID, State: model.BlockWordDisabled}, nil

	default:
		return nil, model.ErrInvalidBlockWord
	}
}

// normalizeBlockScope 校验屏蔽词作用域：分区词必须带 oid。
func normalizeBlockScope(scope rpc.BlockWordScope, oid int64) (int32, int64, error) {
	switch scope {
	case rpc.BlockWordScope_SCOPE_GLOBAL:
		return model.ScopeGlobal, 0, nil
	case rpc.BlockWordScope_SCOPE_OID:
		if oid <= 0 {
			return 0, 0, model.ErrInvalidOid
		}
		return model.ScopeOid, oid, nil
	default:
		return 0, 0, model.ErrInvalidBlockWord
	}
}
