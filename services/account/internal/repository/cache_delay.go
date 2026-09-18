package repository

// 本文件移植自参考仓库 openbilibili-go-common/app/service/main/account/service/cache_delay.go
// 的 cachedelayproc 消费逻辑：updateVip 变更通知先立即失效一次缓存，5 秒后再次失效并回温，
// 避免“先写库后失效”时序下回源读到旧数据重新填回缓存。参考实现为进程内优先级队列，
// 本项目沿用进程内队列（单实例语义），后续多实例部署可替换为 Redis 延迟队列。

import (
	"context"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
)

const (
	// ActionUpdateVip 是缓存失效动作：会员信息变更，需要延迟二次失效。
	// 对应参考仓库 service.DelCache 中 action=="updateVip" 的分支。
	ActionUpdateVip = "updateVip"

	// cacheDelayInterval 延迟队列的检查间隔和任务的延迟时长。
	cacheDelayInterval = 5 * time.Second
)

// DelCache 失效指定 mid 的全部缓存（Info/Card/Profile/Vip）。
// action 为资料变更动作：action==ActionUpdateVip 时额外入延迟队列，5 秒后
// 再次失效并回温，防止并发写库与失效竞态。返回各删除操作发生的错误。
// 参考 service.DelCache + cachedelayproc。
func (r *Repository) DelCache(ctx context.Context, mid int64, action string) []error {
	if action == ActionUpdateVip {
		r.delayQ.Put(mid, time.Now().Add(cacheDelayInterval))
	}
	errs := r.cache.DelCache(ctx, mid)
	r.reWarm(ctx, mid)
	return errs
}

// reWarm 异步回源重建缓存。参考 dao.DelCache 末尾通过 fanout 回温
// Info/Card/Vip/Profile 四个缓存键。
func (r *Repository) reWarm(ctx context.Context, mid int64) {
	_ = r.async.Do(ctx, func(c context.Context) {
		if _, err := r.Info(c, mid); err != nil {
			logx.Errorf("account/cache_delay: reWarm Info mid=%d err=%v", mid, err)
		}
		if _, err := r.Card(c, mid); err != nil {
			logx.Errorf("account/cache_delay: reWarm Card mid=%d err=%v", mid, err)
		}
		if _, err := r.Profile(c, mid); err != nil {
			logx.Errorf("account/cache_delay: reWarm Profile mid=%d err=%v", mid, err)
		}
		if _, err := r.Vip(c, mid); err != nil {
			logx.Errorf("account/cache_delay: reWarm Vip mid=%d err=%v", mid, err)
		}
	})
}

// cacheDelayProc 是延迟队列的消费协程，每 5 秒弹出到期任务并二次失效缓存。
// 参考 cachedelayproc：到期条件为“入队时间距今不小于 5 秒”，弹出后逐条 DelCache。
func (r *Repository) cacheDelayProc() {
	ticker := time.NewTicker(cacheDelayInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.delayStop:
			return
		case <-ticker.C:
			mids := r.delayQ.PopExpired(time.Now())
			if len(mids) == 0 {
				continue
			}
			logx.Infof("account/cache_delay: delete %d delayed cache item(s)", len(mids))
			// 使用独立 context 保证退出时排空任务不被取消。
			ctx := context.Background()
			for _, mid := range mids {
				errs := r.cache.DelCache(ctx, mid)
				for _, e := range errs {
					logx.Errorf("account/cache_delay: delayed del mid=%d err=%v", mid, e)
				}
				r.reWarm(ctx, mid)
			}
		}
	}
}
