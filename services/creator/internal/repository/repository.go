package repository

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/creator/model"
)

// Repository 是 creator 服务的数据访问入口。
// 组合 5 个 model 与 Redis 缓存，为 logic 层提供统一接口。
type Repository struct {
	cache *Cache
	conn  sqlx.SqlConn

	specialModel  model.UpSpecialModel
	groupModel    model.UpGroupModel
	groupMemModel model.UpGroupMemberModel
	attrModel     model.UpAttrModel
	switchModel   model.UpSwitchModel
	signUpModel   model.SignUpModel
}

// New 构造 Repository。
func New(rds *redis.Redis, conn sqlx.SqlConn) *Repository {
	return &Repository{
		cache:         NewCache(rds),
		conn:          conn,
		specialModel:  model.NewUpSpecialModel(conn),
		groupModel:    model.NewUpGroupModel(conn),
		groupMemModel: model.NewUpGroupMemberModel(conn),
		attrModel:     model.NewUpAttrModel(conn),
		switchModel:   model.NewUpSwitchModel(conn),
		signUpModel:   model.NewSignUpModel(conn),
	}
}

// Ping 检查底层 Redis 连通性，供健康检查使用。
func (r *Repository) Ping(ctx context.Context) error {
	return r.cache.Ping(ctx)
}

// --- 特殊属性 ---

// UpSpecial 查询单个 mid 的特殊分组 ID 列表。
// 缓存→DB→回填；DB 不存在时缓存空标记防击穿。
func (r *Repository) UpSpecial(ctx context.Context, mid int64) ([]int64, error) {
	ids, err := r.cache.GetSpecial(ctx, mid)
	if err != nil {
		return nil, err
	}
	if ids != nil {
		return ids, nil
	}
	// miss 回源
	ids, err = r.specialModel.FindOne(ctx, mid)
	if err != nil {
		return nil, fmt.Errorf("UpSpecial FindOne: %w", err)
	}
	if ids == nil {
		ids = []int64{}
	}
	_ = r.cache.SetSpecial(ctx, mid, ids)
	return ids, nil
}

// UpsSpecial 批量查询；缺失 mid 返回空切片。
// 批量不使用缓存（参考仓库策略：单 mid 才走缓存，批量直接打 DB）。
func (r *Repository) UpsSpecial(ctx context.Context, mids []int64) (map[int64][]int64, error) {
	if len(mids) == 0 {
		return map[int64][]int64{}, nil
	}
	return r.specialModel.FindMany(ctx, mids)
}

// --- 特殊分组列表 ---

// UpGroups 查询全量特殊分组列表。
// 5 分钟内存缓存，DB JSON 字符串缓存。返回 mid→group 形式以便调用方使用。
func (r *Repository) UpGroups(ctx context.Context) (map[int64]*model.UpGroup, error) {
	payload, err := r.cache.GetGroups(ctx)
	if err != nil {
		return nil, err
	}
	if payload != "" {
		var groups map[int64]*model.UpGroup
		if err := json.Unmarshal([]byte(payload), &groups); err != nil {
			return nil, fmt.Errorf("UpGroups unmarshal cache: %w", err)
		}
		return groups, nil
	}
	groups, err := r.groupModel.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("UpGroups All: %w", err)
	}
	if groups == nil {
		groups = map[int64]*model.UpGroup{}
	}
	bs, _ := json.Marshal(groups)
	_ = r.cache.SetGroups(ctx, string(bs))
	return groups, nil
}

// UpGroupMids 分页查询某分组下的 mid 列表。
func (r *Repository) UpGroupMids(ctx context.Context, gid int64, pn, ps int32) ([]int64, int32, error) {
	// 分组列表短缓存 1 分钟
	payload, err := r.cache.GetGroupMids(ctx, gid, pn, ps)
	if err != nil {
		return nil, 0, err
	}
	if payload != "" {
		var resp struct {
			Mids  []int64 `json:"mids"`
			Total int32   `json:"total"`
		}
		if err := json.Unmarshal([]byte(payload), &resp); err != nil {
			return nil, 0, fmt.Errorf("UpGroupMids unmarshal cache: %w", err)
		}
		return resp.Mids, resp.Total, nil
	}
	mids, total, err := r.groupMemModel.FindMidsByGroup(ctx, gid, pn, ps)
	if err != nil {
		return nil, 0, fmt.Errorf("UpGroupMids FindMids: %w", err)
	}
	resp := struct {
		Mids  []int64 `json:"mids"`
		Total int32   `json:"total"`
	}{Mids: mids, Total: total}
	if resp.Mids == nil {
		resp.Mids = []int64{}
	}
	bs, _ := json.Marshal(resp)
	_ = r.cache.SetGroupMids(ctx, gid, pn, ps, string(bs))
	return mids, total, nil
}

// --- 身份属性 ---

// UpAttr 查询 UP 身份属性。
func (r *Repository) UpAttr(ctx context.Context, mid int64, from int32) (int32, error) {
	state, hit, err := r.cache.GetAttr(ctx, mid, from)
	if err != nil {
		return 0, err
	}
	if hit {
		return state, nil
	}
	a, err := r.attrModel.FindOne(ctx, mid, from)
	if err != nil {
		return 0, fmt.Errorf("UpAttr FindOne: %w", err)
	}
	if a == nil {
		_ = r.cache.SetAttrEmpty(ctx, mid, from)
		return 0, nil
	}
	_ = r.cache.SetAttr(ctx, mid, from, a.IsAuthor)
	return a.IsAuthor, nil
}

// --- 开关 ---

// UpSwitch 查询开关状态。
func (r *Repository) UpSwitch(ctx context.Context, mid int64, from int32) (int32, error) {
	state, hit, err := r.cache.GetSwitch(ctx, mid, from)
	if err != nil {
		return 0, err
	}
	if hit {
		return state, nil
	}
	s, err := r.switchModel.FindOne(ctx, mid, from)
	if err != nil {
		return 0, fmt.Errorf("UpSwitch FindOne: %w", err)
	}
	if s == nil {
		// 不存在视为默认关闭，不缓存（开关变更频率低）
		return 0, nil
	}
	_ = r.cache.SetSwitch(ctx, mid, from, s.State)
	return s.State, nil
}

// SetUpSwitch 设置开关状态（先 DB 后失效缓存）。
func (r *Repository) SetUpSwitch(ctx context.Context, mid, from, state int32) error {
	if err := r.switchModel.Upsert(ctx, mid, from, state); err != nil {
		return fmt.Errorf("SetUpSwitch Upsert: %w", err)
	}
	return r.cache.DelSwitch(ctx, int64(mid), from)
}

// --- 高能联盟签约 ---

// GetHighAllyUps 批量查询高能联盟签约信息。
// 不使用缓存（参考仓库策略：批量查 DB，单条可由调用方再做缓存）。
func (r *Repository) GetHighAllyUps(ctx context.Context, mids []int64) (map[int64]*model.SignUp, error) {
	if len(mids) == 0 {
		return map[int64]*model.SignUp{}, nil
	}
	return r.signUpModel.FindMany(ctx, mids)
}
