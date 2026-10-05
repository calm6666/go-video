package logic

// listattachments_test.go 覆盖读侧附属查询 ListCovers / ListSubtitles：
// 两者的共同口径是「只按 asset_id 过滤、不缓存、不校验媒资是否存在」，
// 差别只在排序主键（cover_id ASC / sub_id ASC）与投影字段（字幕多一个 lang）。

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"go-video/services/asset/model"
	"go-video/services/asset/rpc"
)

func fakeCoverKey(idx int) string { return fmt.Sprintf("ugc-ut/fake/cover-%02d.jpg", idx) }

func fakeSubtitleKey(idx int) string { return fmt.Sprintf("ugc-ut/fake/subtitle-%02d.srt", idx) }

func seedCovers(t *testing.T, st *store, assetID int64, coverIDs []int64) {
	t.Helper()
	for i, id := range coverIDs {
		seedCover(t, st, &model.AssetCover{
			CoverID: id, AssetID: assetID,
			Bucket: fakeBucket, ObjectKey: fakeCoverKey(i),
			Width: int32(640 + i), Height: int32(360 + i), Ctime: fakeCtime + int64(i),
		})
	}
}

func seedSubtitles(t *testing.T, st *store, assetID int64, subIDs []int64, langs []string) {
	t.Helper()
	for i, id := range subIDs {
		seedSubtitle(t, st, &model.AssetSubtitle{
			SubID: id, AssetID: assetID, Lang: langs[i],
			Bucket: fakeBucket, ObjectKey: fakeSubtitleKey(i), Ctime: fakeCtime + int64(i),
		})
	}
}

func coverIDLine(r *rpc.CoverReply) string       { return "cover=" + itoa(r.GetCoverId()) }
func subtitleIDLine(r *rpc.SubtitleReply) string { return "sub=" + itoa(r.GetSubId()) }

func orderedIDLines(t *testing.T, clause string, ids []int64, prefix string) []string {
	t.Helper()
	return lines(orderIDs(t, clause, ids), func(id int64) string { return prefix + itoa(id) })
}

// --- ListCovers ---

func TestListCoversRejectsNonPositiveAssetID(t *testing.T) {
	for _, id := range []int64{0, -7} {
		st := newStore()
		seedCovers(t, st, 1, []int64{11}) // 有可读的行：失败只能来自守卫
		l := NewListCoversLogic(context.Background(), newTestSvc(st))

		got, err := l.ListCovers(&rpc.AssetReq{AssetId: id})
		wantErrIdentity(t, "ListCovers", err, model.ErrInvalidAssetID)
		if got != nil {
			t.Errorf("应答 = %v, want nil", got)
		}
		wantNoCall(t, "ListCovers 守卫", st, 0)
	}

	st := newStore()
	seedCovers(t, st, 1, []int64{11})
	l := NewListCoversLogic(context.Background(), newTestSvc(st))
	got, err := l.ListCovers(&rpc.AssetReq{AssetId: 1})
	wantNoErr(t, "ListCovers asset_id=1", err)
	wantEQ(t, "正向对照", "条数", len(got.GetItems()), 1)
}

// TestListCoversOrdersByCoverIDASC 钉顺序：期望由 orderByCoverList 现算，
// 布景刻意乱序，logic 层不重排（顺序唯一来源是 model 的 ORDER BY cover_id ASC）。
func TestListCoversOrdersByCoverIDASC(t *testing.T) {
	st := newStore()
	seedIDs := []int64{4007, 4003, 4011, 4001}
	if slices.Equal(seedIDs, orderIDs(t, orderByCoverList, seedIDs)) {
		t.Fatalf("布景顺序与期望一致，用例失去判别力")
	}
	seedCovers(t, st, fakeAssetID, seedIDs)
	l := NewListCoversLogic(context.Background(), newTestSvc(st))

	got, err := l.ListCovers(&rpc.AssetReq{AssetId: fakeAssetID})
	wantNoErr(t, "ListCovers", err)
	wantEQ(t, "无 LIMIT", "条数", len(got.GetItems()), len(seedIDs))
	wantSeq(t, "ORDER BY cover_id ASC", lines(got.GetItems(), coverIDLine),
		orderedIDLines(t, orderByCoverList, seedIDs, "cover="))
	// 顺序对了还要每行内容跟着主键走：逐行整字段比对库存行。
	wantSeq(t, "整行投影", lines(got.GetItems(), replyCoverLine),
		lines(orderIDs(t, orderByCoverList, seedIDs), func(id int64) string { return coverLine(st.cover.rows[id]) }))
	wantCount(t, "封面列表不缓存", st.log, "cache.", 0)
	wantOps(t, "只发一条 SELECT", st.log.opsFrom(0), []string{"cover.List:" + itoa(fakeAssetID)})
}

func TestListCoversFiltersByAssetAndDoesNotCheckAssetExists(t *testing.T) {
	st := newStore()
	seedCovers(t, st, fakeAssetID, []int64{4001, 4002})
	seedCovers(t, st, fakeAssetID+1, []int64{5001})
	l := NewListCoversLogic(context.Background(), newTestSvc(st))

	mine, err := l.ListCovers(&rpc.AssetReq{AssetId: fakeAssetID})
	wantNoErr(t, "ListCovers", err)
	wantSeq(t, "WHERE asset_id = ?", lines(mine.GetItems(), coverIDLine),
		orderedIDLines(t, orderByCoverList, []int64{4001, 4002}, "cover="))

	// 媒资不存在（asset_meta 里一条都没有）也返回空列表、不报错：
	// 这里**没有**存在性守卫，是 addcoverlogic.go:29 注释所述现状。
	ghost, err := l.ListCovers(&rpc.AssetReq{AssetId: 888888})
	wantNoErr(t, "ListCovers 未登记媒资", err)
	wantEQ(t, "未登记媒资", "条数", len(ghost.GetItems()), 0)
	// err 为 nil 本身就是结论：读侧不会因为媒资没登记就报 ErrAssetNotFound。
}

func TestListCoversDBFailurePropagatesRaw(t *testing.T) {
	st := newStore()
	seedCovers(t, st, fakeAssetID, []int64{4001})
	st.cover.failWith("ListByAsset", errBoom)
	l := NewListCoversLogic(context.Background(), newTestSvc(st))

	got, err := l.ListCovers(&rpc.AssetReq{AssetId: fakeAssetID})
	if got != nil {
		t.Errorf("应答 = %v, want nil", got)
	}
	wantErrIs(t, "封面查库失败上抛", err, errBoom)
	wantOps(t, "失败顺序", st.log.opsFrom(0), []string{"cover.List:" + itoa(fakeAssetID)})
}

// --- ListSubtitles ---

func TestListSubtitlesRejectsNonPositiveAssetID(t *testing.T) {
	for _, id := range []int64{0, -7} {
		st := newStore()
		seedSubtitles(t, st, 1, []int64{6001}, []string{"zh-CN"})
		l := NewListSubtitlesLogic(context.Background(), newTestSvc(st))

		got, err := l.ListSubtitles(&rpc.AssetReq{AssetId: id})
		wantErrIdentity(t, "ListSubtitles", err, model.ErrInvalidAssetID)
		if got != nil {
			t.Errorf("应答 = %v, want nil", got)
		}
		wantNoCall(t, "ListSubtitles 守卫", st, 0)
	}
}

// TestListSubtitlesOrdersBySubIDASCCarriesLang 钉顺序与投影：字幕比封面多一个 lang 字段，
// 顺序唯一来源是 model 的 ORDER BY sub_id ASC。
func TestListSubtitlesOrdersBySubIDASCCarriesLang(t *testing.T) {
	st := newStore()
	seedIDs := []int64{7005, 7002, 7009}
	langs := map[int64]string{7005: "zh-CN", 7002: "en-US", 7009: "ja-JP"}
	seedSubtitles(t, st, fakeAssetID, seedIDs, []string{langs[7005], langs[7002], langs[7009]})
	l := NewListSubtitlesLogic(context.Background(), newTestSvc(st))

	got, err := l.ListSubtitles(&rpc.AssetReq{AssetId: fakeAssetID})
	wantNoErr(t, "ListSubtitles", err)
	wantEQ(t, "无 LIMIT", "条数", len(got.GetItems()), len(seedIDs))
	wantSeq(t, "ORDER BY sub_id ASC", lines(got.GetItems(), subtitleIDLine),
		orderedIDLines(t, orderBySubtitleList, seedIDs, "sub="))
	wantSeq(t, "整行投影含 lang", lines(got.GetItems(), replySubtitleLine),
		lines(orderIDs(t, orderBySubtitleList, seedIDs), func(id int64) string { return subtitleLine(st.sub.rows[id]) }))
	// 语言跟着主键走（不是按入参顺序）：中间那条必须是 en-US。
	wantEQ(t, "lang 跟随主键", "第 1 条 lang", got.GetItems()[0].GetLang(), "en-US")
	wantCount(t, "字幕列表不缓存", st.log, "cache.", 0)
	wantOps(t, "只发一条 SELECT", st.log.opsFrom(0), []string{"sub.List:" + itoa(fakeAssetID)})
}

func TestListSubtitlesFiltersByAssetAndDoesNotCheckAssetExists(t *testing.T) {
	st := newStore()
	seedSubtitles(t, st, fakeAssetID, []int64{7001}, []string{"zh-CN"})
	seedSubtitles(t, st, fakeAssetID+1, []int64{8001}, []string{"ko-KR"})
	l := NewListSubtitlesLogic(context.Background(), newTestSvc(st))

	mine, err := l.ListSubtitles(&rpc.AssetReq{AssetId: fakeAssetID})
	wantNoErr(t, "ListSubtitles", err)
	wantSeq(t, "WHERE asset_id = ?", lines(mine.GetItems(), subtitleIDLine),
		orderedIDLines(t, orderBySubtitleList, []int64{7001}, "sub="))

	ghost, err := l.ListSubtitles(&rpc.AssetReq{AssetId: 888888})
	wantNoErr(t, "ListSubtitles 未登记媒资", err)
	wantEQ(t, "未登记媒资", "条数", len(ghost.GetItems()), 0)
	// 同上：未登记媒资只意味着空列表，不报错。
}

func TestListSubtitlesDBFailurePropagatesRaw(t *testing.T) {
	st := newStore()
	seedSubtitles(t, st, fakeAssetID, []int64{7001}, []string{"zh-CN"})
	st.sub.failWith("ListByAsset", errBoom)
	l := NewListSubtitlesLogic(context.Background(), newTestSvc(st))

	got, err := l.ListSubtitles(&rpc.AssetReq{AssetId: fakeAssetID})
	if got != nil {
		t.Errorf("应答 = %v, want nil", got)
	}
	wantErrIs(t, "字幕查库失败上抛", err, errBoom)
	wantOps(t, "失败顺序", st.log.opsFrom(0), []string{"sub.List:" + itoa(fakeAssetID)})
}
