package logic

// matchbyfingerprint_test.go 覆盖 MatchByFingerprintLogic.MatchByFingerprint。
//
// 本域最要紧的不变量，按代价从高到低：
//  1. 守卫顺序：fp_key → fp_type → top_n，全部发生在触库之前；
//  2. top_n 的边界语义：<=0 回落默认 10，>50 直接拒绝，恰好 50 放行（防止一次拉爆检索）；
//  3. 生产实现目前是占位（model.FindByKey 不查库、恒返回空），所以「命中列表为空」是
//     **不判定为侵权**的降级方向；但依赖失败必须是错误，绝不能降级成 (空列表, nil)，
//     那等于把「检索挂了」上报成「这条内容干净」；
//  4. 命中列表的 Score 恒为 0：调用方不得据此判侵权，也不得以为已排序。
//
// 关于 searchEnabled：替身默认逐字复刻占位实现；置 true 只用于锁定 logic 的投影与顺序契约
// （见 fakes_test.go 覆盖边界），生产当前拿不到这些行。

import (
	"context"
	"errors"
	"testing"

	"go-video/services/content-fingerprint/model"
	"go-video/services/content-fingerprint/rpc"
)

func TestMatchByFingerprintGuardsTouchNothing(t *testing.T) {
	cases := []struct {
		name  string
		in    *rpc.MatchReq
		wantE error
	}{
		{"fp_key 为空", &rpc.MatchReq{FpKey: "", FpType: rpc.FpType_FP_TYPE_VIDEO}, model.ErrInvalidFpKey},
		{"fp_key 空 + fp_type 非法时先查 fp_key",
			&rpc.MatchReq{FpKey: "", FpType: rpc.FpType_FP_TYPE_UNSPECIFIED}, model.ErrInvalidFpKey},
		{"fp_key 空 + top_n 越界时先查 fp_key",
			&rpc.MatchReq{FpKey: "", FpType: rpc.FpType_FP_TYPE_VIDEO, TopN: 99}, model.ErrInvalidFpKey},
		{"fp_type UNSPECIFIED", &rpc.MatchReq{FpKey: "K", FpType: rpc.FpType_FP_TYPE_UNSPECIFIED}, model.ErrInvalidFpType},
		{"fp_type 未知枚举 99", &rpc.MatchReq{FpKey: "K", FpType: rpc.FpType(99)}, model.ErrInvalidFpType},
		{"fp_type 非法 + top_n 越界时先查 fp_type",
			&rpc.MatchReq{FpKey: "K", FpType: rpc.FpType(99), TopN: 51}, model.ErrInvalidFpType},
		{"top_n=51（上限 +1）", &rpc.MatchReq{FpKey: "K", FpType: rpc.FpType_FP_TYPE_AUDIO, TopN: 51}, model.ErrInvalidTopN},
		{"top_n=1000", &rpc.MatchReq{FpKey: "K", FpType: rpc.FpType_FP_TYPE_AUDIO, TopN: 1000}, model.ErrInvalidTopN},
		{"top_n 为负不受限（回落默认 10，见下一条用例）",
			&rpc.MatchReq{FpKey: "K", FpType: rpc.FpType_FP_TYPE_AUDIO, TopN: -1}, nil},
	}
	for _, tc := range cases {
		st := newStore()
		st.rec.searchEnabled = true // 让「守卫拦住了」与「放行后拿到候选」可区分
		seedRecord(t, st, &model.FingerprintRecord{AssetID: 7001, FpType: model.FpTypeVideo, Key: "K", Hash: "h1"})
		l := NewMatchByFingerprintLogic(context.Background(), newTestSvc(st))

		reply, err := l.MatchByFingerprint(tc.in)
		if tc.wantE == nil {
			// 这条是「不该被拒」的反向守卫：写成拒绝类断言会掩盖放行路径。
			wantNoErr(t, tc.name, err)
			if st.log.snapshot() == 0 {
				t.Errorf("%s：放行却没触达任何依赖", tc.name)
			}
			continue
		}
		wantErrIs(t, tc.name, err, tc.wantE)
		wantEQ(t, tc.name, "不返回半截命中", reply == nil, true)
		wantNoCall(t, tc.name, st, 0)
	}
}

// top_n 的默认值与上限：0/负数回落 10，1 与 50 原样透传，51 以上被上一条用例拒绝。
func TestMatchByFingerprintTopNDefaultsAndBoundary(t *testing.T) {
	cases := []struct {
		name string
		in   int32
		want int32
	}{
		{"0 回落默认 10", 0, 10},
		{"负数回落默认 10", -5, 10},
		{"1 原样透传", 1, 1},
		{"恰好 50 放行", 50, 50},
	}
	for _, tc := range cases {
		st := newStore()
		l := NewMatchByFingerprintLogic(context.Background(), newTestSvc(st))
		_, err := l.MatchByFingerprint(&rpc.MatchReq{FpKey: "K", FpType: rpc.FpType_FP_TYPE_VIDEO, TopN: tc.in})
		wantNoErr(t, tc.name, err)
		wantOps(t, tc.name, st.log.ops, []string{"rec.FindByKey:K/1/" + itoa(int64(tc.want))})
	}
}

// 生产占位语义：检索未接入 ⇒ 命中列表为空且不是错误（= 不判定为侵权）。
// 同时钉住两个事实：Match 结果缓存未接线、指纹事实表只被读一次。
func TestMatchByFingerprintPlaceholderAnswersNoHit(t *testing.T) {
	st := newStore()
	seedRecord(t, st, &model.FingerprintRecord{AssetID: 7001, FpType: model.FpTypeVideo, Key: "K", Hash: "h-same"})
	l := NewMatchByFingerprintLogic(context.Background(), newTestSvc(st))
	before := st.log.snapshot()

	reply, err := l.MatchByFingerprint(&rpc.MatchReq{FpKey: "K", FpType: rpc.FpType_FP_TYPE_VIDEO, TopN: 10})
	wantNoErr(t, "检索占位", err)
	wantOps(t, "检索占位", st.log.opsFrom(before), []string{"rec.FindByKey:K/1/10"})
	if reply.GetItems() == nil {
		t.Errorf("检索占位：items 是 nil，客户端会按缺字段处理")
	}
	wantStringsEQ(t, "检索占位", "命中列表", itemLines(reply.GetItems()), []string{})
	wantEQ(t, "检索占位", "不读写 fp:match:* 缓存", st.log.countPrefix("cache."), 0)
}

func TestMatchByFingerprintProjectsEveryCandidateField(t *testing.T) {
	st := newStore()
	st.rec.searchEnabled = true
	seedRecord(t, st, &model.FingerprintRecord{AssetID: 7001, FpType: model.FpTypeAudio, Key: "OTHER", Hash: "h-a"})
	seedRecord(t, st, &model.FingerprintRecord{AssetID: 7003, FpType: model.FpTypeVideo, Key: "K", Hash: "h-7003"})
	seedRecord(t, st, &model.FingerprintRecord{AssetID: 7002, FpType: model.FpTypeVideo, Key: "K", Hash: "h-7002"})
	l := NewMatchByFingerprintLogic(context.Background(), newTestSvc(st))

	reply, err := l.MatchByFingerprint(&rpc.MatchReq{FpKey: "K", FpType: rpc.FpType_FP_TYPE_VIDEO, TopN: 10})
	wantNoErr(t, "命中投影", err)
	wantStringsEQ(t, "命中投影", "只回 fp_type=video 且 key=K 的两条", itemLines(reply.GetItems()), []string{
		"asset=7003/type=FP_TYPE_VIDEO/key=K/hash=h-7003/score=0",
		"asset=7002/type=FP_TYPE_VIDEO/key=K/hash=h-7002/score=0",
	})
	for idx, item := range reply.GetItems() {
		label := "命中投影 第 " + itoa(int64(idx+1)) + " 条"
		wantEQ(t, label, "Score 恒为 0（相似度未接入）", item.GetScore(), float64(0))
		wantEQ(t, label, "fp_type", item.GetFpType(), rpc.FpType_FP_TYPE_VIDEO)
		wantEQ(t, label, "fp_key", item.GetFpKey(), "K")
	}
}

// 顺序契约：logic 不重排、不按 score 排序（score 全是 0），调用方拿到的就是依赖返回的顺序。
// 接上相似度检索后必须改成「按 score 降序」并连带改本用例。
func TestMatchByFingerprintKeepsDependencyOrderWithoutRanking(t *testing.T) {
	st := newStore()
	st.rec.searchEnabled = true
	seedRecord(t, st, &model.FingerprintRecord{AssetID: 9003, FpType: model.FpTypeVideo, Key: "K", Hash: "h3"})
	seedRecord(t, st, &model.FingerprintRecord{AssetID: 9001, FpType: model.FpTypeVideo, Key: "K", Hash: "h1"})
	seedRecord(t, st, &model.FingerprintRecord{AssetID: 9002, FpType: model.FpTypeVideo, Key: "K", Hash: "h2"})
	l := NewMatchByFingerprintLogic(context.Background(), newTestSvc(st))

	reply, err := l.MatchByFingerprint(&rpc.MatchReq{FpKey: "K", FpType: rpc.FpType_FP_TYPE_VIDEO})
	wantNoErr(t, "命中顺序", err)
	wantStringsEQ(t, "命中顺序", "既不按 asset_id 也不按 hash 排", itemLines(reply.GetItems()), []string{
		"asset=9003/type=FP_TYPE_VIDEO/key=K/hash=h3/score=0",
		"asset=9001/type=FP_TYPE_VIDEO/key=K/hash=h1/score=0",
		"asset=9002/type=FP_TYPE_VIDEO/key=K/hash=h2/score=0",
	})
}

// 疑似缺陷（只钉现状，不代表设计正确）：本服务对 fp_key 只判「非空」，
// 长度/字符集完全不设防，1 个字符的指纹也照样进检索。
// 空指纹/极短指纹在真实检索引擎里几乎必然产生误命中或全表扫描，
// 收严时应加最短长度阈值（连同本用例一起改）。
func TestMatchByFingerprintAcceptsSingleCharKey(t *testing.T) {
	st := newStore()
	l := NewMatchByFingerprintLogic(context.Background(), newTestSvc(st))

	_, err := l.MatchByFingerprint(&rpc.MatchReq{FpKey: "a", FpType: rpc.FpType_FP_TYPE_VIDEO})
	wantNoErr(t, "单字符 fp_key", err)
	wantOps(t, "单字符 fp_key", st.log.ops, []string{"rec.FindByKey:a/1/10"})

	// 空白串同样直通：守卫用的是 == ""，不是 strings.TrimSpace。
	_, err = l.MatchByFingerprint(&rpc.MatchReq{FpKey: "   ", FpType: rpc.FpType_FP_TYPE_AUDIO})
	wantNoErr(t, "空白 fp_key", err)
	wantOps(t, "空白 fp_key", st.log.ops, []string{
		"rec.FindByKey:a/1/10",
		"rec.FindByKey:   /2/10",
	})
}

// 依赖失败必须是错误，且不得被降级成「空命中 + nil」。
func TestMatchByFingerprintDownstreamFailureIsNotEmptyHitList(t *testing.T) {
	st := newStore()
	st.rec.searchEnabled = true
	seedRecord(t, st, &model.FingerprintRecord{AssetID: 7001, FpType: model.FpTypeVideo, Key: "K", Hash: "h"})
	timeout := errors.New("fingerprint_record FindByKey: context deadline exceeded")
	st.rec.failWith("FindByKey", timeout)
	l := NewMatchByFingerprintLogic(context.Background(), newTestSvc(st))

	reply, err := l.MatchByFingerprint(&rpc.MatchReq{FpKey: "K", FpType: rpc.FpType_FP_TYPE_VIDEO})
	wantErrIs(t, "检索超时", err, timeout)
	if reply != nil {
		t.Errorf("检索超时：返回了 %v（等价于上报「这条内容干净」）", itemLines(reply.GetItems()))
	}
	wantOps(t, "检索超时", st.log.ops, []string{"rec.FindByKey:K/1/10"})
	wantEQ(t, "检索超时", "不写任何缓存", st.log.countPrefix("cache."), 0)
	wantEQ(t, "检索超时", "不写指纹事实", st.log.countPrefix("rec.Upsert"), 0)
}
