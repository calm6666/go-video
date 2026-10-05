package logic

// matchbyasset_test.go 覆盖 MatchByAssetLogic.MatchByAsset。
//
// 与 MatchByFingerprint 的分工：那条按 key 召回候选（生产是占位、恒空），
// 本条按 asset_id 读**该媒资自己的指纹事实**（本期实现为「自匹配」）。
// 因此本方法要紧的是：
//  1. asset_id 守卫在触库之前；
//  2. fp_type 三态语义：UNSPECIFIED=0 不限定类型（video+audio 都回），1/2 精确过滤，
//     未知枚举也落到 0（不报错、不限制）——这条最容易写反；
//  3. 只回本 asset 的行，绝不串到别的媒资（版权证据不能张冠李戴）；
//  4. 依赖失败必须是错误，不能降级成空命中。

import (
	"context"
	"errors"
	"testing"

	"go-video/services/content-fingerprint/model"
	"go-video/services/content-fingerprint/rpc"
)

func TestMatchByAssetGuardsTouchNothing(t *testing.T) {
	st := newStore()
	seedRecord(t, st, &model.FingerprintRecord{AssetID: 500, FpType: model.FpTypeVideo, Key: "K", Hash: "h"})
	l := NewMatchByAssetLogic(context.Background(), newTestSvc(st))

	for _, id := range []int64{0, -1, -500} {
		reply, err := l.MatchByAsset(&rpc.AssetReq{AssetId: id, FpType: rpc.FpType_FP_TYPE_VIDEO})
		wantErrIs(t, "asset_id 非正", err, model.ErrInvalidAssetID)
		wantEQ(t, "asset_id 非正", "不返回半截命中", reply == nil, true)
	}
	wantNoCall(t, "asset_id<=0 守卫", st, 0)
}

func TestMatchByAssetUnspecifiedTypeReturnsBothFingerprints(t *testing.T) {
	st := newStore()
	seedRecord(t, st, &model.FingerprintRecord{AssetID: 500, FpType: model.FpTypeVideo, Key: "vk-500", Hash: "vh-500"})
	seedRecord(t, st, &model.FingerprintRecord{AssetID: 500, FpType: model.FpTypeAudio, Key: "ak-500", Hash: "ah-500"})
	seedRecord(t, st, &model.FingerprintRecord{AssetID: 501, FpType: model.FpTypeVideo, Key: "vk-501", Hash: "vh-501"})
	l := NewMatchByAssetLogic(context.Background(), newTestSvc(st))
	before := st.log.snapshot()

	reply, err := l.MatchByAsset(&rpc.AssetReq{AssetId: 500})
	wantNoErr(t, "不限定类型", err)
	wantOps(t, "不限定类型", st.log.opsFrom(before), []string{"rec.FindByAsset:500/0"})
	wantStringsEQ(t, "不限定类型", "命中逐字段投影", itemLines(reply.GetItems()), []string{
		"asset=500/type=FP_TYPE_VIDEO/key=vk-500/hash=vh-500/score=0",
		"asset=500/type=FP_TYPE_AUDIO/key=ak-500/hash=ah-500/score=0",
	})
	wantEQ(t, "不限定类型", "不串到别的媒资", len(reply.GetItems()), 2)
	wantEQ(t, "不限定类型", "不读写缓存", st.log.countPrefix("cache."), 0)
	wantEQ(t, "不限定类型", "不写指纹事实", st.log.countPrefix("rec.Upsert"), 0)
}

// fp_type 的三态映射：未知枚举与 UNSPECIFIED 一样落到 0（不限定），而不是报错。
func TestMatchByAssetFpTypeFilterMapping(t *testing.T) {
	cases := []struct {
		name      string
		in        rpc.FpType
		wantArg   int32
		wantLines []string
	}{
		{"UNSPECIFIED 不限定", rpc.FpType_FP_TYPE_UNSPECIFIED, 0, []string{
			"asset=500/type=FP_TYPE_VIDEO/key=vk-500/hash=vh-500/score=0",
			"asset=500/type=FP_TYPE_AUDIO/key=ak-500/hash=ah-500/score=0"}},
		{"VIDEO", rpc.FpType_FP_TYPE_VIDEO, model.FpTypeVideo, []string{
			"asset=500/type=FP_TYPE_VIDEO/key=vk-500/hash=vh-500/score=0"}},
		{"AUDIO", rpc.FpType_FP_TYPE_AUDIO, model.FpTypeAudio, []string{
			"asset=500/type=FP_TYPE_AUDIO/key=ak-500/hash=ah-500/score=0"}},
		{"未知枚举 99 回落不限定", rpc.FpType(99), 0, []string{
			"asset=500/type=FP_TYPE_VIDEO/key=vk-500/hash=vh-500/score=0",
			"asset=500/type=FP_TYPE_AUDIO/key=ak-500/hash=ah-500/score=0"}},
	}
	for _, tc := range cases {
		st := newStore()
		seedRecord(t, st, &model.FingerprintRecord{AssetID: 500, FpType: model.FpTypeVideo, Key: "vk-500", Hash: "vh-500"})
		seedRecord(t, st, &model.FingerprintRecord{AssetID: 500, FpType: model.FpTypeAudio, Key: "ak-500", Hash: "ah-500"})
		l := NewMatchByAssetLogic(context.Background(), newTestSvc(st))

		reply, err := l.MatchByAsset(&rpc.AssetReq{AssetId: 500, FpType: tc.in})
		wantNoErr(t, tc.name, err)
		wantOps(t, tc.name, st.log.ops, []string{"rec.FindByAsset:500/" + itoa(int64(tc.wantArg))})
		wantStringsEQ(t, tc.name, "命中", itemLines(reply.GetItems()), tc.wantLines)
	}
}

func TestMatchByAssetNoFingerprintAnswersEmptyNotNil(t *testing.T) {
	st := newStore()
	seedRecord(t, st, &model.FingerprintRecord{AssetID: 999, FpType: model.FpTypeVideo, Key: "K", Hash: "h"})
	l := NewMatchByAssetLogic(context.Background(), newTestSvc(st))

	reply, err := l.MatchByAsset(&rpc.AssetReq{AssetId: 500, FpType: rpc.FpType_FP_TYPE_VIDEO})
	wantNoErr(t, "该媒资还没有指纹", err)
	if reply.GetItems() == nil {
		t.Errorf("该媒资还没有指纹：items 是 nil")
	}
	wantStringsEQ(t, "该媒资还没有指纹", "命中", itemLines(reply.GetItems()), []string{})
}

// 疑似缺陷（只钉现状，不代表设计正确）：本方法对指纹事实本身**不做任何质量校验**，
// key/hash 为空串的行也会作为「命中证据」返回给 moderation-orchestrator。
// 空指纹不得被判为命中——收严时应在 Repository 侧过滤空 key/hash（或禁止写入）。
func TestMatchByAssetReturnsEmptyKeyRecordAsHit(t *testing.T) {
	st := newStore()
	seedRecord(t, st, &model.FingerprintRecord{AssetID: 500, FpType: model.FpTypeVideo, Key: "", Hash: ""})
	l := NewMatchByAssetLogic(context.Background(), newTestSvc(st))

	reply, err := l.MatchByAsset(&rpc.AssetReq{AssetId: 500, FpType: rpc.FpType_FP_TYPE_VIDEO})
	wantNoErr(t, "空指纹记录", err)
	wantStringsEQ(t, "空指纹记录", "空 key/hash 也被当成命中", itemLines(reply.GetItems()),
		[]string{"asset=500/type=FP_TYPE_VIDEO/key=/hash=/score=0"})
	wantEQ(t, "空指纹记录", "Score 仍是 0，调用方无法据分数排除", reply.GetItems()[0].GetScore(), float64(0))
}

func TestMatchByAssetDownstreamFailureIsNotEmptyHitList(t *testing.T) {
	st := newStore()
	seedRecord(t, st, &model.FingerprintRecord{AssetID: 500, FpType: model.FpTypeVideo, Key: "K", Hash: "h"})
	dbDown := errors.New("fingerprint_record FindByAsset: connection is dead")
	st.rec.failWith("FindByAsset", dbDown)
	l := NewMatchByAssetLogic(context.Background(), newTestSvc(st))

	reply, err := l.MatchByAsset(&rpc.AssetReq{AssetId: 500, FpType: rpc.FpType_FP_TYPE_VIDEO})
	wantErrIs(t, "读指纹事实失败", err, dbDown)
	if reply != nil {
		t.Errorf("读指纹事实失败：返回了 %v（等价于上报「这条内容干净」）", itemLines(reply.GetItems()))
	}
	wantOps(t, "读指纹事实失败", st.log.ops, []string{"rec.FindByAsset:500/1"})
}
