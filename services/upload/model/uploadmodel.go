package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// nowUnix 返回当前 Unix 秒时间戳。
func nowUnix() int64 { return time.Now().Unix() }

// UploadSession 上传会话（upload_session 表）。
// 拥有字段：upload_id/mid/filename/size/typeid/bucket/object_key/state/
// chunk_size/total_chunks/md5/asset_id/ctime/mtime。
type UploadSession struct {
	UploadId    string `db:"upload_id"`    // 上传会话 ID
	Mid         int64  `db:"mid"`          // 用户 ID
	Filename    string `db:"filename"`     // 文件名
	Size        int64  `db:"size"`         // 文件总大小（字节）
	Typeid      int32  `db:"typeid"`       // 稿件类型 ID
	Bucket      string `db:"bucket"`       // OSS bucket
	ObjectKey   string `db:"object_key"`   // OSS 对象 key
	State       int32  `db:"state"`        // 会话状态：见 SessionState* 常量
	ChunkSize   int64  `db:"chunk_size"`   // 分片大小（字节）
	TotalChunks int32  `db:"total_chunks"` // 分片总数
	Md5         string `db:"md5"`          // 完整文件 MD5
	AssetId     string `db:"asset_id"`     // 关联 asset_id（asset 服务回填）
	Ctime       int64  `db:"ctime"`        // 创建时间（Unix 秒）
	Mtime       int64  `db:"mtime"`        // 修改时间（Unix 秒）
}

// UploadChunk 上传分片（upload_chunk 表）。
// 拥有字段：upload_id/chunk_no/size/etag/state/ctime/mtime。
type UploadChunk struct {
	Id       int64  `db:"id"`        // 主键 ID
	UploadId string `db:"upload_id"` // 上传会话 ID
	ChunkNo  int32  `db:"chunk_no"`  // 分片序号（从 1 开始）
	Size     int64  `db:"size"`      // 分片大小（字节）
	Etag     string `db:"etag"`      // 分片 ETag（OSS 返回）
	State    int32  `db:"state"`     // 分片状态：见 ChunkState* 常量
	Ctime    int64  `db:"ctime"`     // 创建时间（Unix 秒）
	Mtime    int64  `db:"mtime"`     // 修改时间（Unix 秒）
}

// UploadSessionModel upload_session 表查询与写入接口。
type UploadSessionModel interface {
	// Insert 新建上传会话。
	Insert(ctx context.Context, s *UploadSession) error
	// FindOne 按 upload_id 查询会话；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, uploadId string) (*UploadSession, error)
	// UpdateState 更新会话状态。
	UpdateState(ctx context.Context, uploadId string, state int32) error
	// UpdateAssetId 回填 asset_id。
	UpdateAssetId(ctx context.Context, uploadId, assetId string) error
	// SetMd5 更新完整文件 MD5。
	SetMd5(ctx context.Context, uploadId, md5 string) error
	// UpdateStateTx 是 UpdateState 的事务版：tx 非空时随调用方事务提交或回滚。
	// 完成上传的「md5 + asset_id + state + outbox 行」必须走这一组（AGENTS.md §5）。
	UpdateStateTx(ctx context.Context, tx sqlx.Session, uploadId string, state int32) error
	// UpdateAssetIdTx 是 UpdateAssetId 的事务版。
	UpdateAssetIdTx(ctx context.Context, tx sqlx.Session, uploadId, assetId string) error
	// SetMd5Tx 是 SetMd5 的事务版。
	SetMd5Tx(ctx context.Context, tx sqlx.Session, uploadId, md5 string) error
}

// ChunkPart 是完成上传时的分片清单条目（仓库层抽象，与 rpc 解耦）。
type ChunkPart struct {
	ChunkNo int32  // 分片序号
	Etag    string // OSS 返回的 ETag
}

// UploadChunkModel upload_chunk 表查询与写入接口。
type UploadChunkModel interface {
	// InsertBatch 批量新建分片清单（单事务）。
	InsertBatch(ctx context.Context, chunks []*UploadChunk) error
	// ListByUpload 查询会话全部分片，按 chunk_no 升序。
	ListByUpload(ctx context.Context, uploadId string) ([]*UploadChunk, error)
	// FindOne 查询单个分片；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, uploadId string, chunkNo int32) (*UploadChunk, error)
	// MarkUploaded 标记分片已上传并记录 ETag。
	MarkUploaded(ctx context.Context, uploadId string, chunkNo int32, etag string) error
	// DeleteByUpload 删除会话全部分片（取消上传时清理）。
	DeleteByUpload(ctx context.Context, uploadId string) error
}

type defaultUploadSessionModel struct {
	conn sqlx.SqlConn
}

// NewUploadSessionModel 创建 UploadSessionModel 实现。
func NewUploadSessionModel(conn sqlx.SqlConn) UploadSessionModel {
	return &defaultUploadSessionModel{conn: conn}
}

func (m *defaultUploadSessionModel) Insert(ctx context.Context, s *UploadSession) error {
	_, err := m.conn.ExecCtx(ctx,
		"INSERT INTO upload_session (upload_id, mid, filename, size, typeid, bucket, object_key, state, chunk_size, total_chunks, md5, asset_id, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		s.UploadId, s.Mid, s.Filename, s.Size, s.Typeid, s.Bucket, s.ObjectKey, s.State, s.ChunkSize, s.TotalChunks, s.Md5, s.AssetId, s.Ctime, s.Mtime)
	if err != nil {
		return fmt.Errorf("upload_session Insert: %w", err)
	}
	return nil
}

func (m *defaultUploadSessionModel) FindOne(ctx context.Context, uploadId string) (*UploadSession, error) {
	var s UploadSession
	query := "SELECT upload_id, mid, filename, size, typeid, bucket, object_key, state, chunk_size, total_chunks, md5, asset_id, ctime, mtime FROM upload_session WHERE upload_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &s, query, uploadId); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("upload_session FindOne: %w", err)
	}
	return &s, nil
}

func (m *defaultUploadSessionModel) UpdateState(ctx context.Context, uploadId string, state int32) error {
	return m.UpdateStateTx(ctx, nil, uploadId, state)
}

func (m *defaultUploadSessionModel) UpdateStateTx(ctx context.Context, tx sqlx.Session, uploadId string, state int32) error {
	session := m.session(tx)
	res, err := session.ExecCtx(ctx,
		"UPDATE upload_session SET state = ?, mtime = ? WHERE upload_id = ?",
		state, nowUnix(), uploadId)
	if err != nil {
		return fmt.Errorf("upload_session UpdateState: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("upload_session UpdateState RowsAffected: %w", err)
	}
	if aff == 0 {
		return ErrUploadNotFound
	}
	return nil
}

func (m *defaultUploadSessionModel) UpdateAssetId(ctx context.Context, uploadId, assetId string) error {
	return m.UpdateAssetIdTx(ctx, nil, uploadId, assetId)
}

func (m *defaultUploadSessionModel) UpdateAssetIdTx(ctx context.Context, tx sqlx.Session, uploadId, assetId string) error {
	_, err := m.session(tx).ExecCtx(ctx,
		"UPDATE upload_session SET asset_id = ?, mtime = ? WHERE upload_id = ?",
		assetId, nowUnix(), uploadId)
	if err != nil {
		return fmt.Errorf("upload_session UpdateAssetId: %w", err)
	}
	return nil
}

func (m *defaultUploadSessionModel) SetMd5(ctx context.Context, uploadId, md5 string) error {
	return m.SetMd5Tx(ctx, nil, uploadId, md5)
}

func (m *defaultUploadSessionModel) SetMd5Tx(ctx context.Context, tx sqlx.Session, uploadId, md5 string) error {
	_, err := m.session(tx).ExecCtx(ctx,
		"UPDATE upload_session SET md5 = ?, mtime = ? WHERE upload_id = ?",
		md5, nowUnix(), uploadId)
	if err != nil {
		return fmt.Errorf("upload_session SetMd5: %w", err)
	}
	return nil
}

// session 让每个写方法都能在同一份 SQL 文本上选执行器：tx 非空即随调用方事务，
// 为空则走连接自动提交。复制 SQL 文本会让两条路径的列清单各自演化。
func (m *defaultUploadSessionModel) session(tx sqlx.Session) sqlx.Session {
	if tx != nil {
		return tx
	}
	return m.conn
}

type defaultUploadChunkModel struct {
	conn sqlx.SqlConn
}

// NewUploadChunkModel 创建 UploadChunkModel 实现。
func NewUploadChunkModel(conn sqlx.SqlConn) UploadChunkModel {
	return &defaultUploadChunkModel{conn: conn}
}

func (m *defaultUploadChunkModel) InsertBatch(ctx context.Context, chunks []*UploadChunk) error {
	if len(chunks) == 0 {
		return nil
	}
	return m.conn.TransactCtx(ctx, func(ctx context.Context, session sqlx.Session) error {
		stmt := "INSERT INTO upload_chunk (upload_id, chunk_no, size, etag, state, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?)"
		for _, c := range chunks {
			if _, err := session.ExecCtx(ctx, stmt, c.UploadId, c.ChunkNo, c.Size, c.Etag, c.State, c.Ctime, c.Mtime); err != nil {
				return fmt.Errorf("upload_chunk InsertBatch: %w", err)
			}
		}
		return nil
	})
}

func (m *defaultUploadChunkModel) ListByUpload(ctx context.Context, uploadId string) ([]*UploadChunk, error) {
	var rows []*UploadChunk
	query := "SELECT id, upload_id, chunk_no, size, etag, state, ctime, mtime FROM upload_chunk WHERE upload_id = ? ORDER BY chunk_no ASC"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, uploadId); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("upload_chunk ListByUpload: %w", err)
	}
	return rows, nil
}

func (m *defaultUploadChunkModel) FindOne(ctx context.Context, uploadId string, chunkNo int32) (*UploadChunk, error) {
	var c UploadChunk
	query := "SELECT id, upload_id, chunk_no, size, etag, state, ctime, mtime FROM upload_chunk WHERE upload_id = ? AND chunk_no = ?"
	if err := m.conn.QueryRowCtx(ctx, &c, query, uploadId, chunkNo); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("upload_chunk FindOne: %w", err)
	}
	return &c, nil
}

func (m *defaultUploadChunkModel) MarkUploaded(ctx context.Context, uploadId string, chunkNo int32, etag string) error {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE upload_chunk SET etag = ?, state = ?, mtime = ? WHERE upload_id = ? AND chunk_no = ?",
		etag, ChunkStateUploaded, nowUnix(), uploadId, chunkNo)
	if err != nil {
		return fmt.Errorf("upload_chunk MarkUploaded: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("upload_chunk MarkUploaded RowsAffected: %w", err)
	}
	if aff == 0 {
		return ErrChunkNotFound
	}
	return nil
}

func (m *defaultUploadChunkModel) DeleteByUpload(ctx context.Context, uploadId string) error {
	_, err := m.conn.ExecCtx(ctx,
		"DELETE FROM upload_chunk WHERE upload_id = ?",
		uploadId)
	if err != nil {
		return fmt.Errorf("upload_chunk DeleteByUpload: %w", err)
	}
	return nil
}
