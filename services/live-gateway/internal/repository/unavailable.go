package repository

import (
	"context"
	"fmt"
)

// unavailableLeaseStore 是 CacheRedis 未配置时的占位实现：每个方法都返回 ErrStoreUnavailable。
//
// 为什么不做「nil 时 logic 各自判空」：那会把 30 个判空点摊到 22 个 logic 文件里，
// 漏掉任何一个就是 nil map 写入 panic；而漏掉判空的后果是「返回假成功」——
// 对下发类接口，假成功等于告诉运营「消息发出去了」，实际上什么都没发生。
// 有了这个实现，logic 无需判空，且缺依赖时的表现是**每个请求显式失败**。
type unavailableLeaseStore struct{ reason string }

// NewUnavailableLeaseStore 构造占位实现；reason 进入错误文本，供运维定位「哪段配置没写」。
func NewUnavailableLeaseStore(reason string) LeaseStore {
	if reason == "" {
		reason = "CacheRedis not configured"
	}
	return &unavailableLeaseStore{reason: reason}
}

func (s *unavailableLeaseStore) err(op string) error {
	return fmt.Errorf("%w: %s: %s", ErrStoreUnavailable, op, s.reason)
}

func (s *unavailableLeaseStore) Acquire(context.Context, *LeaseRecord, int) (*LeaseRecord, error) {
	return nil, s.err("Acquire")
}
func (s *unavailableLeaseStore) Get(context.Context, string) (*LeaseRecord, error) {
	return nil, s.err("Get")
}
func (s *unavailableLeaseStore) Put(context.Context, *LeaseRecord) error { return s.err("Put") }
func (s *unavailableLeaseStore) Delete(context.Context, *LeaseRecord) error {
	return s.err("Delete")
}
func (s *unavailableLeaseStore) ListRoomLeases(context.Context, int64, int32) ([]*LeaseRecord, int32, bool, error) {
	return nil, 0, false, s.err("ListRoomLeases")
}
func (s *unavailableLeaseStore) ListUserLeases(context.Context, int64, int64, int32) ([]*LeaseRecord, error) {
	return nil, s.err("ListUserLeases")
}
func (s *unavailableLeaseStore) RoomConnectionCount(context.Context, int64, int32) (int32, error) {
	return 0, s.err("RoomConnectionCount")
}
func (s *unavailableLeaseStore) Subscribe(context.Context, string, int64, int64, []string, int) error {
	return s.err("Subscribe")
}
func (s *unavailableLeaseStore) Unsubscribe(context.Context, string, int64, int64, []string) ([]string, error) {
	return nil, s.err("Unsubscribe")
}
func (s *unavailableLeaseStore) ClaimMessage(context.Context, int64, string, int) (string, bool, error) {
	return "", false, s.err("ClaimMessage")
}
func (s *unavailableLeaseStore) RememberMessage(context.Context, int64, string, string, int) error {
	return s.err("RememberMessage")
}
func (s *unavailableLeaseStore) ClaimEvent(context.Context, int64, string, int) (string, bool, error) {
	return "", false, s.err("ClaimEvent")
}
func (s *unavailableLeaseStore) RememberEvent(context.Context, int64, string, string, int) error {
	return s.err("RememberEvent")
}
func (s *unavailableLeaseStore) ClaimRequest(context.Context, string, string, int) (string, bool, error) {
	return "", false, s.err("ClaimRequest")
}
func (s *unavailableLeaseStore) RememberRequest(context.Context, string, string, string, int) error {
	return s.err("RememberRequest")
}
func (s *unavailableLeaseStore) AllowRate(context.Context, string, int32, int) (bool, int32, error) {
	return false, 0, s.err("AllowRate")
}
func (s *unavailableLeaseStore) SetBan(context.Context, int64, int64, int64, bool) error {
	return s.err("SetBan")
}
func (s *unavailableLeaseStore) BanUntil(context.Context, int64, int64) (int64, error) {
	return 0, s.err("BanUntil")
}
func (s *unavailableLeaseStore) PutTicket(context.Context, string, string, int) error {
	return s.err("PutTicket")
}
func (s *unavailableLeaseStore) ConsumeTicket(context.Context, string) (string, bool, error) {
	return "", false, s.err("ConsumeTicket")
}
func (s *unavailableLeaseStore) DropTicket(context.Context, string) error { return s.err("DropTicket") }
func (s *unavailableLeaseStore) BumpRoomSeq(context.Context, int64) (int64, error) {
	return 0, s.err("BumpRoomSeq")
}
func (s *unavailableLeaseStore) MarkOffline(context.Context, int64, int64, int64, int64, int) error {
	return s.err("MarkOffline")
}
func (s *unavailableLeaseStore) OfflineView(context.Context, int64, int64) (int64, int64, error) {
	return 0, 0, s.err("OfflineView")
}
func (s *unavailableLeaseStore) CacheGet(context.Context, string, any) (bool, error) {
	return false, s.err("CacheGet")
}
func (s *unavailableLeaseStore) CacheSet(context.Context, string, any, int) error {
	return s.err("CacheSet")
}
func (s *unavailableLeaseStore) CacheDel(context.Context, ...string) error { return s.err("CacheDel") }
func (s *unavailableLeaseStore) QuotaEpoch(context.Context) (int32, error) {
	return 0, s.err("QuotaEpoch")
}
func (s *unavailableLeaseStore) BumpQuotaEpoch(context.Context) (int32, error) {
	return 0, s.err("BumpQuotaEpoch")
}

// 编译期断言：接口新增方法时，占位实现必须同步补齐（否则这里就报错，而不是运行期 panic）。
var (
	_ LeaseStore = (*redisLeaseStore)(nil)
	_ LeaseStore = (*unavailableLeaseStore)(nil)
)
