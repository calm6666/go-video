package logic

// addsubtitle_test.go 覆盖写侧字幕登记 AddSubtitle：四条守卫的先后（lang 判在 bucket 之前）、
// 归属键 (asset_id, lang)、重复插入的行为。
//
// 重复插入是这里最要紧的一条：asset_subtitle 上有唯一键 uniq_asset_lang(asset_id, lang)
// （deploy/migrations/asset/000002_create_asset_attachment_tables.sql:30），
// 替身按真实 1062 建模，而 logic 与 model 都不做任何翻译 ⇒ 调用方拿到的是裸驱动错误文本，
// 既不是「已存在」业务码，也不是覆盖成功（README 缺口 8）。

import (
	"context"
	"errors"
	"testing"
	"time"

	"go-video/services/asset/model"
	"go-video/services/asset/rpc"
)

func validSubtitleReq(assetID int64, lang string) *rpc.AddSubtitleReq {
	return &rpc.AddSubtitleReq{
		AssetId:   assetID,
		Lang:      lang,
		Bucket:    fakeBucket,
		ObjectKey: fakeSubtitleKey(1),
	}
}

// domainSentinels 是本服务对外的全部业务错误；用来证明某个错误**没有**被翻译成其中之一。
var domainSentinels = []error{
	model.ErrAssetNotFound, model.ErrInvalidAssetID, model.ErrInvalidUploadID,
	model.ErrInvalidMid, model.ErrInvalidBucket, model.ErrInvalidObjectKey,
	model.ErrInvalidState, model.ErrInvalidLang, model.ErrInvalidTimestamp,
	model.ErrInvalidTransition, model.ErrPsTooLarge,
}

func TestAddSubtitleGuardsAssetIDThenLangThenBucketThenObjectKey(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(in *rpc.AddSubtitleReq)
		wantErr error
	}{
		{"四条全坏：先判 asset_id", func(in *rpc.AddSubtitleReq) { in.AssetId, in.Lang, in.Bucket, in.ObjectKey = 0, "", "", "" }, model.ErrInvalidAssetID},
		{"asset_id 负数", func(in *rpc.AddSubtitleReq) { in.AssetId = -fakeAssetID }, model.ErrInvalidAssetID},
		{"lang 与 bucket 同坏：先判 lang", func(in *rpc.AddSubtitleReq) { in.Lang, in.Bucket = "", "" }, model.ErrInvalidLang},
		{"lang 合规后 bucket 空", func(in *rpc.AddSubtitleReq) { in.Bucket = "" }, model.ErrInvalidBucket},
		{"只剩 object_key 空", func(in *rpc.AddSubtitleReq) { in.ObjectKey = "" }, model.ErrInvalidObjectKey},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newStore()
			seedAssetInState(t, st, fakeAssetID, model.StateUploaded)
			l := NewAddSubtitleLogic(context.Background(), newTestSvc(st))

			req := validSubtitleReq(fakeAssetID, "zh-CN")
			c.mutate(req)
			got, err := l.AddSubtitle(req)

			wantErrIdentity(t, "AddSubtitle", err, c.wantErr)
			if got != nil {
				t.Errorf("AddSubtitle(%s) 应答 = %v, want nil", c.name, got)
			}
			wantNoCall(t, "AddSubtitle 守卫", st, 0)
			wantEQ(t, "守卫拒绝", "库存字幕行数", len(st.sub.rows), 0)
		})
	}

	st := newStore()
	l := NewAddSubtitleLogic(context.Background(), newTestSvc(st))
	got, err := l.AddSubtitle(validSubtitleReq(fakeAssetID, "zh-CN"))
	wantNoErr(t, "AddSubtitle 合规入参", err)
	wantEQ(t, "合规入参", "库存字幕行数", len(st.sub.rows), 1)
	wantOps(t, "合规入参只发一条 INSERT", st.log.opsFrom(0), []string{opSubInsert(fakeAssetID, "zh-CN")})
	wantEQ(t, "合规入参", "落库业务键", st.sub.rowByLang(fakeAssetID, "zh-CN").SubID, got.GetSubId())
}

// TestAddSubtitleWritesRowOwnedByAssetIDAndLang 钉归属键与前置检查位置：
// 落库行由 (asset_id, lang) 定位（sub_id 由 INSERT 分配），
// 整条序列只有这一条 INSERT——没有存在性 SELECT、没有按 lang 的先查、没碰缓存。
func TestAddSubtitleWritesRowOwnedByAssetIDAndLang(t *testing.T) {
	st := newStore()
	seedAssetInState(t, st, fakeAssetID, model.StateUploaded)
	before := time.Now().Unix()
	l := NewAddSubtitleLogic(context.Background(), newTestSvc(st))

	got, err := l.AddSubtitle(validSubtitleReq(fakeAssetID, "en-US"))
	after := time.Now().Unix()
	wantNoErr(t, "AddSubtitle", err)

	row := st.sub.row(got.GetSubId())
	if row == nil {
		t.Fatalf("应答 sub_id=%d 在库里不存在", got.GetSubId())
	}
	wantEQ(t, "归属键", "整行", replySubtitleLine(got), subtitleLine(row))
	wantEQ(t, "归属键", "asset_id", row.AssetID, fakeAssetID)
	wantEQ(t, "归属键", "lang", row.Lang, "en-US")
	wantEQ(t, "入参透传", "bucket", row.Bucket, fakeBucket)
	wantEQ(t, "入参透传", "object_key", row.ObjectKey, fakeSubtitleKey(1))
	wantUnixWindow(t, "登记时间", "ctime", row.Ctime, before, after)
	wantOps(t, "只发一条 INSERT", st.log.opsFrom(0), []string{opSubInsert(fakeAssetID, "en-US")})
	wantCount(t, "不校验媒资是否存在", st.log, "meta.", 0)
	wantCount(t, "不先按 lang 查", st.log, "sub.List", 0)
	wantCount(t, "附件写不动详情缓存", st.log, "cache.", 0)
}

// TestAddSubtitleDuplicateLangReturnsRawMySQL1062 钉重复插入的行为（README 缺口 8）：
// 同一 (asset_id, lang) 再登记一次 ⇒ 唯一键冲突被 model 层 %w 包一层直接上抛，
// logic 不识别 1062、不翻译成本域任何哨兵、也不改成覆盖语义。
// 三条残留断言：原行的引用列没被覆盖、库里仍只有一行、没有第二次写。
func TestAddSubtitleDuplicateLangReturnsRawMySQL1062(t *testing.T) {
	st := newStore()
	seedAssetInState(t, st, fakeAssetID, model.StateUploaded)
	original := seedSubtitle(t, st, &model.AssetSubtitle{
		SubID: 7001, AssetID: fakeAssetID, Lang: "zh-CN",
		Bucket: fakeBucket, ObjectKey: fakeSubtitleKey(1), Ctime: fakeCtime,
	})
	l := NewAddSubtitleLogic(context.Background(), newTestSvc(st))

	req := validSubtitleReq(fakeAssetID, "zh-CN")
	req.ObjectKey = fakeSubtitleKey(2) // 换一份字幕文件、同一语言：意图是「替换」
	got, err := l.AddSubtitle(req)

	if got != nil {
		t.Errorf("应答 = %v, want nil（撞唯一键绝不能报成功）", got)
	}
	wantErrIs(t, "重复语言登记", err, errDuplicateKey) // 裸 1062 文本一路透传
	for _, s := range domainSentinels {
		if errors.Is(err, s) {
			t.Errorf("撞唯一键被翻译成了业务哨兵 %v：现状是原样上抛，本用例与 README 缺口 8 要一起改", s)
		}
	}
	wantOps(t, "这次调用只发一条 INSERT", st.log.opsFrom(0), []string{opSubInsert(fakeAssetID, "zh-CN")})
	wantCount(t, "冲突后不再补写", st.log, "sub.Insert", 1)
	wantCount(t, "冲突后不覆盖原行", st.log, "sub.List", 0)
	wantEQ(t, "重复登记", "库存字幕行数", len(st.sub.rows), 1)
	kept := st.sub.rowByLang(fakeAssetID, "zh-CN")
	if kept == nil {
		t.Fatalf("原行不见了：业务键 (asset_id=%d, lang=zh-CN) 查不到", fakeAssetID)
	}
	wantEQ(t, "原行未被覆盖", "整行", subtitleLine(kept), subtitleLine(original))
}

// TestAddSubtitleUniqueKeyIsScopedToAssetAndLang 是上一条的判别力对照：
// 唯一键只在 (asset_id, lang) 这个**组合**上生效——
// 同媒资换语言、同语言换媒资都必须成功，否则「重复被拒」就没有意义。
func TestAddSubtitleUniqueKeyIsScopedToAssetAndLang(t *testing.T) {
	st := newStore()
	seedAssetInState(t, st, fakeAssetID, model.StateUploaded)
	seedAssetInState(t, st, fakeAssetID+1, model.StateUploaded)
	l := NewAddSubtitleLogic(context.Background(), newTestSvc(st))

	// 同媒资、不同语言。
	zh, err := l.AddSubtitle(validSubtitleReq(fakeAssetID, "zh-CN"))
	wantNoErr(t, "登记 zh-CN", err)
	en, err := l.AddSubtitle(validSubtitleReq(fakeAssetID, "en-US"))
	wantNoErr(t, "同媒资换语言", err)
	if zh.GetSubId() == en.GetSubId() {
		t.Fatalf("两条语言字幕共用了 sub_id=%d", zh.GetSubId())
	}
	// 同语言、不同媒资。
	other, err := l.AddSubtitle(validSubtitleReq(fakeAssetID+1, "zh-CN"))
	wantNoErr(t, "同语言换媒资", err)
	if other.GetSubId() == zh.GetSubId() {
		t.Fatalf("不同媒资共用了 sub_id=%d", other.GetSubId())
	}

	wantEQ(t, "三条并存", "库存字幕行数", len(st.sub.rows), 3)
	wantOps(t, "三条 INSERT 的 lang 各不相同", st.log.opsFrom(0), []string{
		opSubInsert(fakeAssetID, "zh-CN"), opSubInsert(fakeAssetID, "en-US"), opSubInsert(fakeAssetID+1, "zh-CN"),
	})
	// 读侧确实按媒资切分：各自的列表只含自己的行。
	mine, err := NewListSubtitlesLogic(context.Background(), newTestSvc(st)).ListSubtitles(&rpc.AssetReq{AssetId: fakeAssetID})
	wantNoErr(t, "ListSubtitles", err)
	wantSeq(t, "按 asset_id 过滤", lines(mine.GetItems(), subtitleIDLine),
		orderedIDLines(t, orderBySubtitleList, []int64{zh.GetSubId(), en.GetSubId()}, "sub="))
}

// TestAddSubtitleDoesNotValidateOrNormalizeLang 钉守卫只看「是否为空串」：
// 纯空格的 lang 合法，语言代码的分隔符风格也不归一（zh_CN 与 zh-CN 是两行）。
// 不锁大小写变体：替身按字节比较，而 MySQL utf8mb4 默认排序规则大小写不敏感，
// 「ZH-CN 会不会撞上 zh-CN」在这里测不出来（见 README 缺口 8 的补充说明）。
func TestAddSubtitleDoesNotValidateOrNormalizeLang(t *testing.T) {
	st := newStore()
	l := NewAddSubtitleLogic(context.Background(), newTestSvc(st))

	space, err := l.AddSubtitle(validSubtitleReq(fakeAssetID, " "))
	wantNoErr(t, "纯空格 lang 被接受（现状）", err)
	wantEQ(t, "纯空格 lang", "库存原样", st.sub.row(space.GetSubId()).Lang, " ")

	underscore, err := l.AddSubtitle(validSubtitleReq(fakeAssetID, "zh_CN"))
	wantNoErr(t, "下划线写法被接受", err)
	dash, err := l.AddSubtitle(validSubtitleReq(fakeAssetID, "zh-CN"))
	wantNoErr(t, "连字符写法被接受", err)
	if underscore.GetSubId() == dash.GetSubId() {
		t.Fatalf("两种写法共用了 sub_id=%d：唯一键判定口径变了", underscore.GetSubId())
	}
	wantEQ(t, "两种写法并存（无归一）", "库存字幕行数", len(st.sub.rows), 3)
	// 落库的 lang 逐字保留（不 trim、不归一、不按语言表校验），顺序即登记顺序。
	wantStringsEQ(t, "库存 lang", "逐字保留",
		lines(st.sub.snapshotByPK(), func(s *model.AssetSubtitle) string { return s.Lang }),
		[]string{" ", "zh_CN", "zh-CN"})
}

func TestAddSubtitleInsertFailurePropagatesAndLeavesNoRow(t *testing.T) {
	st := newStore()
	seedAssetInState(t, st, fakeAssetID, model.StateUploaded)
	st.sub.failWith("Insert", errBoom)
	l := NewAddSubtitleLogic(context.Background(), newTestSvc(st))

	got, err := l.AddSubtitle(validSubtitleReq(fakeAssetID, "ja-JP"))
	if got != nil {
		t.Errorf("应答 = %v, want nil", got)
	}
	wantErrIs(t, "字幕 INSERT 失败上抛", err, errBoom)
	wantOps(t, "失败顺序", st.log.opsFrom(0), []string{opSubInsert(fakeAssetID, "ja-JP")})
	wantEQ(t, "失败残留", "库存字幕行数", len(st.sub.rows), 0)
	wantCount(t, "失败不碰缓存", st.log, "cache.", 0)
}
