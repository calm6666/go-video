package repository

// 本文件移植自参考仓库 service/base.go、service/member.go、service/exp.go、
// service/user_flag.go 与 dao/mysql.go、dao/memcache.go 的聚合逻辑。

import (
	"context"
	"errors"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/user-profile/model"
	"go-video/services/user-profile/rpc"
)

// 领域错误（logic 层据此映射 HTTP/RPC 错误信息）。
var (
	// ErrMemberNotExist 用户资料不存在。
	ErrMemberNotExist = errors.New("member not exist")
	// ErrMemberOverLimit 批量查询超过 100 个。
	ErrMemberOverLimit = errors.New("member over limit")
	// ErrUserNoMember 用户排名低于 10000，不允许经验值操作（参考 ecode.UserNoMember）。
	ErrUserNoMember = errors.New("user no member")
	// ErrNothingFound 官方认证信息不存在。
	ErrNothingFound = errors.New("nothing found")
	// ErrNoOfficialDoc 官方认证文档不存在。
	ErrNoOfficialDoc = errors.New("no official doc")
	// ErrRequestErr 请求参数错误。
	ErrRequestErr = errors.New("request error")
	// ErrSubmitOfficialDocFailed 提交认证文档失败。
	ErrSubmitOfficialDocFailed = errors.New("submit official doc failed")
)

// baseCachePayload 基础资料缓存结构（含 Cached 命中标记；Mid=0 为“不存在”哨兵，
// 参考仓库把 MemberNotExist 也写入缓存以防击穿）。
type baseCachePayload struct {
	Cached bool `json:"cached"`
	baseCacheValue
}

type baseCacheValue struct {
	Mid      int64  `json:"mid"`
	Name     string `json:"name"`
	Sex      int64  `json:"sex"`
	Face     string `json:"face"`
	Sign     string `json:"sign"`
	Rank     int64  `json:"rank"`
	Birthday int64  `json:"birthday"`
}

func toBaseReply(p *baseCacheValue) *rpc.BaseInfoReply {
	if p == nil {
		return nil
	}
	return &rpc.BaseInfoReply{
		Mid:      p.Mid,
		Name:     p.Name,
		Sex:      p.Sex,
		Face:     p.Face,
		Sign:     p.Sign,
		Rank:     p.Rank,
		Birthday: p.Birthday,
	}
}

// BaseInfo 查询单个用户基础资料（缓存→DB→异步回填，参考 service.BaseInfo）。
// 不存在时返回 mid=0 的 BaseInfoReply（防缓存击穿，与参考仓库一致）。
func (r *Repository) BaseInfo(ctx context.Context, mid int64) (*rpc.BaseInfoReply, error) {
	if mid < 1 {
		return nil, ErrRequestErr
	}
	cacheOK := true
	var payload baseCachePayload
	if err := r.cache.getJSON(ctx, keyBase(mid), &payload); err != nil {
		cacheOK = false
		logx.Errorf("user-profile/profile: get base cache mid=%d err=%v", mid, err)
	}
	if payload.Cached {
		return toBaseReply(&payload.baseCacheValue), nil
	}
	// cache miss 回源
	base, err := r.baseModel.FindOne(ctx, mid)
	if err != nil {
		return nil, err
	}
	payload = baseCachePayload{Cached: true}
	if base != nil {
		payload.baseCacheValue = baseCacheValue{
			Mid: base.Mid, Name: base.Name, Sex: base.Sex, Face: base.Face,
			Sign: base.Sign, Rank: base.Rank, Birthday: base.Birthday,
		}
	}
	if cacheOK {
		p := payload
		_ = r.async.Do(ctx, func(c context.Context) {
			r.cache.setJSON(c, keyBase(mid), p, cacheTTLBase)
		})
	}
	return toBaseReply(&payload.baseCacheValue), nil
}

// BatchBaseInfo 批量查询基础资料（最多 100 个，参考 service.BatchBaseInfo）。
func (r *Repository) BatchBaseInfo(ctx context.Context, mids []int64) (map[int64]*rpc.BaseInfoReply, error) {
	if len(mids) > 100 {
		return nil, ErrMemberOverLimit
	}
	if len(mids) == 0 {
		return map[int64]*rpc.BaseInfoReply{}, nil
	}
	result := make(map[int64]*rpc.BaseInfoReply, len(mids))
	miss := make([]int64, 0, len(mids))
	for _, mid := range mids {
		var payload baseCachePayload
		if err := r.cache.getJSON(ctx, keyBase(mid), &payload); err != nil {
			logx.Errorf("user-profile/profile: batch base cache mid=%d err=%v", mid, err)
		}
		if payload.Cached {
			result[mid] = toBaseReply(&payload.baseCacheValue)
			continue
		}
		miss = append(miss, mid)
	}
	if len(miss) > 0 {
		dbResult, err := r.baseModel.FindMany(ctx, miss)
		if err != nil {
			return result, err
		}
		for _, mid := range miss {
			base := dbResult[mid]
			payload := baseCachePayload{Cached: true}
			if base != nil {
				payload.baseCacheValue = baseCacheValue{
					Mid: base.Mid, Name: base.Name, Sex: base.Sex, Face: base.Face,
					Sign: base.Sign, Rank: base.Rank, Birthday: base.Birthday,
				}
			}
			result[mid] = toBaseReply(&payload.baseCacheValue)
			p := payload
			_ = r.async.Do(ctx, func(c context.Context) {
				r.cache.setJSON(c, keyBase(mid), p, cacheTTLBase)
			})
		}
	}
	return result, nil
}

// Member 查询用户全量信息（基础+等级+官方认证，参考 service.Member）。
func (r *Repository) Member(ctx context.Context, mid int64) (*rpc.MemberInfoReply, error) {
	base, err := r.BaseInfo(ctx, mid)
	if err != nil {
		return nil, err
	}
	if base.Mid == 0 {
		return nil, ErrMemberNotExist
	}
	level, err := r.Exp(ctx, mid)
	if err != nil {
		logx.Errorf("user-profile/profile: exp mid=%d err=%v", mid, err)
		level = &rpc.LevelInfoReply{}
	}
	reply := &rpc.MemberInfoReply{
		BaseInfo:  base,
		LevelInfo: level,
	}
	officials := r.Officials()
	if officials != nil {
		if o := officials[mid]; o != nil {
			reply.OfficialInfo = &rpc.OfficialInfoReply{
				Role:  int32(o.Role),
				Title: o.Title,
				Desc:  o.Desc,
			}
		}
	}
	return reply, nil
}

// Members 批量查询全量信息（最多 100 个，参考 service.Members）。
func (r *Repository) Members(ctx context.Context, mids []int64) (map[int64]*rpc.MemberInfoReply, error) {
	if len(mids) > 100 {
		return nil, ErrMemberOverLimit
	}
	bases, err := r.BatchBaseInfo(ctx, mids)
	if err != nil {
		return nil, err
	}
	exps, err := r.Exps(ctx, mids)
	if err != nil {
		logx.Errorf("user-profile/profile: exps err=%v", err)
		exps = map[int64]*rpc.LevelInfoReply{}
	}
	officials := r.Officials()
	result := make(map[int64]*rpc.MemberInfoReply, len(bases))
	for mid, base := range bases {
		m := &rpc.MemberInfoReply{BaseInfo: base}
		if lv, ok := exps[mid]; ok && lv != nil {
			m.LevelInfo = lv
		} else {
			m.LevelInfo = &rpc.LevelInfoReply{}
		}
		if officials != nil {
			if o := officials[mid]; o != nil {
				m.OfficialInfo = &rpc.OfficialInfoReply{
					Role:  int32(o.Role),
					Title: o.Title,
					Desc:  o.Desc,
				}
			}
		}
		result[mid] = m
	}
	return result, nil
}

// setBaseTx 在事务内执行单个资料字段 UPSERT 并写入 Outbox 事件。
func (r *Repository) setBaseTx(ctx context.Context, mid int64, action string, fn func(ctx context.Context) error) error {
	err := r.conn.TransactCtx(ctx, func(c context.Context, tx sqlx.Session) error {
		if err := fn(c); err != nil {
			return err
		}
		return r.enqueueProfileUpdatedTx(c, tx, mid, action)
	})
	if err != nil {
		return err
	}
	if err := r.cache.delBaseCache(ctx, mid); err != nil {
		logx.Errorf("user-profile/profile: del base cache mid=%d err=%v", mid, err)
	}
	return nil
}

// SetRank 更新排名（参考 service.SetRank：写库→失效缓存→通知 account 失效缓存）。
func (r *Repository) SetRank(ctx context.Context, mid, rank int64) error {
	return r.setBaseTx(ctx, mid, ActUpdatePersonInfo, func(c context.Context) error {
		return r.baseModel.SetRank(c, mid, rank)
	})
}

// SetSex 更新性别。
func (r *Repository) SetSex(ctx context.Context, mid, sex int64) error {
	return r.setBaseTx(ctx, mid, ActUpdatePersonInfo, func(c context.Context) error {
		return r.baseModel.SetSex(c, mid, sex)
	})
}

// SetName 更新昵称（动作 updateUname，参考 service.SetName）。
func (r *Repository) SetName(ctx context.Context, mid int64, name string) error {
	return r.setBaseTx(ctx, mid, ActUpdateUname, func(c context.Context) error {
		return r.baseModel.SetName(c, mid, name)
	})
}

// SetSign 更新签名。
func (r *Repository) SetSign(ctx context.Context, mid int64, sign string) error {
	return r.setBaseTx(ctx, mid, ActUpdatePersonInfo, func(c context.Context) error {
		return r.baseModel.SetSign(c, mid, sign)
	})
}

// SetBirthday 更新生日。
func (r *Repository) SetBirthday(ctx context.Context, mid, birthday int64) error {
	return r.setBaseTx(ctx, mid, ActUpdatePersonInfo, func(c context.Context) error {
		return r.baseModel.SetBirthday(c, mid, birthday)
	})
}

// SetFace 更新头像（动作 updateFace，参考 service.SetFace）。
func (r *Repository) SetFace(ctx context.Context, mid int64, face string) error {
	return r.setBaseTx(ctx, mid, ActUpdateFace, func(c context.Context) error {
		return r.baseModel.SetFace(c, mid, face)
	})
}

// SetBase 整体更新基础资料（参考 service.SetBase：仅失效缓存，不发通知）。
// 头像整体更新时置空；rank/birthday 为零时使用默认值。
func (r *Repository) SetBase(ctx context.Context, base *model.UserBase) error {
	base.Face = ""
	if base.Rank == 0 {
		base.Rank = model.DefaultRank
	}
	if base.Birthday == 0 {
		base.Birthday = model.DefaultTime
	}
	if err := r.baseModel.SetBase(ctx, base); err != nil {
		return err
	}
	return r.cache.delBaseCache(ctx, base.Mid)
}

// NickUpdated 查询是否已首次修改昵称（参考 service.NickUpdated）。
func (r *Repository) NickUpdated(ctx context.Context, mid int64) (bool, error) {
	return r.flagModel.HasAttr(ctx, mid, model.NickUpdated)
}

// SetNickUpdated 标记已首次修改昵称（参考 service.SetNickUpdated）。
func (r *Repository) SetNickUpdated(ctx context.Context, mid int64) error {
	return r.flagModel.SetAttr(ctx, mid, model.NickUpdated)
}

// Exp 查询经验等级信息（含当前经验，参考 service.Exp）。
func (r *Repository) Exp(ctx context.Context, mid int64) (*rpc.LevelInfoReply, error) {
	count, err := r.exp(ctx, mid)
	if err != nil {
		return nil, err
	}
	cur, min, nowExp, nextExp := model.BuildLevel(count, true)
	return &rpc.LevelInfoReply{Cur: cur, Min: min, NowExp: nowExp, NextExp: nextExp}, nil
}

// Level 查询等级信息（不含当前经验，参考 service.Level）。
func (r *Repository) Level(ctx context.Context, mid int64) (*rpc.LevelInfoReply, error) {
	count, err := r.exp(ctx, mid)
	if err != nil {
		return nil, err
	}
	cur, min, _, nextExp := model.BuildLevel(count, false)
	return &rpc.LevelInfoReply{Cur: cur, Min: min, NowExp: 0, NextExp: nextExp}, nil
}

// Exps 批量查询等级信息（参考 service.Exps）。
func (r *Repository) Exps(ctx context.Context, mids []int64) (map[int64]*rpc.LevelInfoReply, error) {
	exps, err := r.exps(ctx, mids)
	if err != nil {
		return nil, err
	}
	result := make(map[int64]*rpc.LevelInfoReply, len(exps))
	for mid, exp := range exps {
		cur, min, nowExp, nextExp := model.BuildLevel(exp, true)
		result[mid] = &rpc.LevelInfoReply{Cur: cur, Min: min, NowExp: nowExp, NextExp: nextExp}
	}
	return result, nil
}

// exp 单用户经验值（缓存→DB，参考 dao.Exp）。
func (r *Repository) exp(ctx context.Context, mid int64) (int64, error) {
	if v, ok := r.cache.getInt(ctx, keyExp(mid)); ok {
		return v, nil
	}
	exp, err := r.expModel.FindOne(ctx, mid)
	if err != nil {
		return 0, err
	}
	r.cache.setInt(ctx, keyExp(mid), exp, cacheTTLExp)
	return exp, nil
}

// exps 批量经验值（缓存→DB→异步回填，参考 dao.Exps）。
func (r *Repository) exps(ctx context.Context, mids []int64) (map[int64]int64, error) {
	result := make(map[int64]int64, len(mids))
	miss := make([]int64, 0, len(mids))
	for _, mid := range mids {
		if v, ok := r.cache.getInt(ctx, keyExp(mid)); ok {
			result[mid] = v
		} else {
			miss = append(miss, mid)
		}
	}
	if len(miss) == 0 {
		return result, nil
	}
	dbResult, err := r.expModel.FindMany(ctx, miss)
	if err != nil {
		return result, err
	}
	for _, mid := range miss {
		exp := dbResult[mid]
		result[mid] = exp
		e := exp
		_ = r.async.Do(ctx, func(c context.Context) {
			r.cache.setInt(c, keyExp(mid), e, cacheTTLExp)
		})
	}
	return result, nil
}

// checkExpMember 经验值操作前置校验：排名 >= 10000（参考 service.SetExp/UpdateExp）。
func (r *Repository) checkExpMember(ctx context.Context, mid int64) error {
	base, err := r.BaseInfo(ctx, mid)
	if err != nil {
		return err
	}
	if base.Mid == 0 {
		return ErrMemberNotExist
	}
	if base.Rank < 10000 {
		return ErrUserNoMember
	}
	return nil
}

// SetExp 直接设置经验值（仅运营，参考 service.SetExp）。
func (r *Repository) SetExp(ctx context.Context, mid int64, count float64, operate, reason, ip string) error {
	if err := r.checkExpMember(ctx, mid); err != nil {
		return err
	}
	exp, err := r.exp(ctx, mid)
	if err != nil {
		return err
	}
	target := int64(count * model.ExpMulti)
	if _, err := r.expModel.Set(ctx, mid, target); err != nil {
		return err
	}
	r.addExpLog(ctx, mid, exp/model.ExpMulti, target/model.ExpMulti, operate, reason, ip)
	return r.cache.delExpCache(ctx, mid)
}

// UpdateExp 增加经验值（参考 service.UpdateExp：count=0 直接返回）。
func (r *Repository) UpdateExp(ctx context.Context, mid int64, count float64, operate, reason, ip string) error {
	if err := r.checkExpMember(ctx, mid); err != nil {
		return err
	}
	if count == 0 {
		return nil
	}
	exp, err := r.exp(ctx, mid)
	if err != nil {
		return err
	}
	delta := int64(count * model.ExpMulti)
	if exp == 0 {
		if _, err := r.expModel.Set(ctx, mid, delta); err != nil {
			return err
		}
	} else {
		if _, err := r.expModel.Incr(ctx, mid, delta); err != nil {
			return err
		}
	}
	r.addExpLog(ctx, mid, exp/model.ExpMulti, (exp+delta)/model.ExpMulti, operate, reason, ip)
	return r.cache.delExpCache(ctx, mid)
}

// addExpLog 写入经验变更日志（参考 dao.AddExplog：异步失败仅记日志）。
func (r *Repository) addExpLog(ctx context.Context, mid, fromExp, toExp int64, operate, reason, ip string) {
	ul := &model.UserLog{
		Mid:   mid,
		IP:    ip,
		TS:    time.Now().Unix(),
		LogID: uuid4(),
		Content: map[string]string{
			"from_exp": formatInt(fromExp),
			"to_exp":   formatInt(toExp),
			"operater": operate,
			"reason":   reason,
		},
	}
	if _, err := r.logModel.Add(ctx, model.LogTypeExp, ul); err != nil {
		logx.Errorf("user-profile/profile: add exp log mid=%d err=%v", mid, err)
	}
}

// ExpLog 查询经验变更日志（最近 7 天，参考 service.ExpLog）。
func (r *Repository) ExpLog(ctx context.Context, mid int64) ([]*rpc.UserLogReply, error) {
	return r.memberLogs(ctx, model.LogTypeExp, mid)
}

// Stat 查询当日经验奖励统计（参考 service.Stat）。
func (r *Repository) Stat(ctx context.Context, mid int64) (*rpc.ExpStatReply, error) {
	login, watch, share, coin, err := r.cache.statCache(ctx, mid, int64(time.Now().Day()))
	if err != nil {
		return nil, err
	}
	return &rpc.ExpStatReply{Login: login, Watch: watch, Coin: coin, Share: share}, nil
}
