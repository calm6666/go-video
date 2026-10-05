package logic

// guard_test.go 覆盖上架/建集前置校验的三条路径：通过、拒绝、客户端未配置。
// 全部使用 fake 客户端，不建立真实 gRPC 连接，也不依赖 MySQL/Redis。

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/catalog/internal/config"
	"go-video/services/catalog/internal/repository"
	"go-video/services/catalog/internal/svc"
	"go-video/services/catalog/model"
)

// fakeRights 是 repository.RightsClient 的可控实现。
type fakeRights struct {
	calls         int
	lastContentID int64
	lastType      repository.RightsContentType
	lastRegion    string
	result        *repository.RightsCheckResult
	err           error
}

func (f *fakeRights) CheckPlayable(_ context.Context, contentID int64, contentType repository.RightsContentType, region string) (*repository.RightsCheckResult, error) {
	f.calls++
	f.lastContentID = contentID
	f.lastType = contentType
	f.lastRegion = region
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

// fakeAsset 是 repository.AssetClient 的可控实现。
type fakeAsset struct {
	calls       int
	lastAssetID int64
	meta        *repository.AssetMeta
	err         error
}

func (f *fakeAsset) GetAsset(_ context.Context, assetID int64) (*repository.AssetMeta, error) {
	f.calls++
	f.lastAssetID = assetID
	if f.err != nil {
		return nil, f.err
	}
	return f.meta, nil
}

// newTestGuard 构造只带 fake 客户端的 guard，避免连接真实下游。
func newTestGuard(cfg config.Config, rights repository.RightsClient, asset repository.AssetClient) *guard {
	svcCtx := &svc.ServiceContext{Config: cfg, Rights: rights, Asset: asset}
	return newGuard(svcCtx, logx.WithContext(context.Background()))
}

func transcodedAsset() *repository.AssetMeta {
	return &repository.AssetMeta{AssetID: 77, Duration: 1200000, State: repository.AssetStateTranscoded}
}

func TestRightsWindowPasses(t *testing.T) {
	rights := &fakeRights{result: &repository.RightsCheckResult{Playable: true, WindowID: 9, EndTime: 2000}}
	g := newTestGuard(config.Config{DefaultRegion: "CN"}, rights, nil)

	if err := g.rightsWindow(context.Background(), 123, "CN"); err != nil {
		t.Fatalf("rightsWindow() = %v, want nil", err)
	}
	if rights.calls != 1 {
		t.Fatalf("CheckPlayable calls = %d, want 1", rights.calls)
	}
	if rights.lastContentID != 123 {
		t.Errorf("content_id = %d, want 123 (epid 作为 PGC 内容 ID)", rights.lastContentID)
	}
	if rights.lastType != repository.RightsContentTypePGC {
		t.Errorf("content_type = %d, want PGC(%d)", rights.lastType, repository.RightsContentTypePGC)
	}
}

func TestRightsWindowDenied(t *testing.T) {
	rights := &fakeRights{result: &repository.RightsCheckResult{Playable: false}}
	g := newTestGuard(config.Config{}, rights, nil)

	err := g.rightsWindow(context.Background(), 123, "US")
	if !errors.Is(err, model.ErrRightsWindowClosed) {
		t.Fatalf("rightsWindow() = %v, want ErrRightsWindowClosed", err)
	}
}

func TestRightsWindowClientNotConfigured(t *testing.T) {
	g := newTestGuard(config.Config{}, nil, nil)

	err := g.rightsWindow(context.Background(), 123, "CN")
	if !errors.Is(err, model.ErrRightsCheckerUnavailable) {
		t.Fatalf("rightsWindow() = %v, want ErrRightsCheckerUnavailable", err)
	}
}

func TestRightsWindowDownstreamErrorIsUnavailable(t *testing.T) {
	cause := errors.New("deadline exceeded")
	rights := &fakeRights{err: cause}
	g := newTestGuard(config.Config{}, rights, nil)

	err := g.rightsWindow(context.Background(), 123, "CN")
	if !errors.Is(err, model.ErrRightsCheckerUnavailable) {
		t.Fatalf("rightsWindow() = %v, want ErrRightsCheckerUnavailable", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("rightsWindow() = %v, want wrapped cause %v", err, cause)
	}
}

func TestRightsCheckDisabledSkipsCall(t *testing.T) {
	rights := &fakeRights{err: errors.New("must not be called")}
	g := newTestGuard(config.Config{DisableRightsCheck: true}, rights, nil)

	if err := g.rightsWindow(context.Background(), 123, ""); err != nil {
		t.Fatalf("rightsWindow() = %v, want nil when disabled", err)
	}
	if rights.calls != 0 {
		t.Fatalf("CheckPlayable calls = %d, want 0 when disabled", rights.calls)
	}
}

func TestResolveRegionPrecedence(t *testing.T) {
	g := newTestGuard(config.Config{DefaultRegion: "CN"}, nil, nil)

	if got, err := g.resolveRegion("TW"); err != nil || got != "TW" {
		t.Errorf("resolveRegion(TW) = %q, %v; want TW, nil", got, err)
	}
	if got, err := g.resolveRegion("  "); err != nil || got != "CN" {
		t.Errorf("resolveRegion(blank) = %q, %v; want CN(默认值)", got, err)
	}

	strict := newTestGuard(config.Config{}, nil, nil)
	if _, err := strict.resolveRegion(""); !errors.Is(err, model.ErrMissingRegion) {
		t.Errorf("resolveRegion(无默认地区) = %v, want ErrMissingRegion", err)
	}
}

func TestPrecheckPublishHappyPath(t *testing.T) {
	rights := &fakeRights{result: &repository.RightsCheckResult{Playable: true, WindowID: 5}}
	asset := &fakeAsset{meta: transcodedAsset()}
	g := newTestGuard(config.Config{}, rights, asset)
	e := &model.Episode{Epid: 123, SeasonID: 10, AssetID: 77, State: model.EpStateDraft}

	region, err := g.precheckPublish(context.Background(), e, "CN")
	if err != nil {
		t.Fatalf("precheckPublish() = %v, want nil", err)
	}
	if region != "CN" {
		t.Errorf("region = %q, want CN", region)
	}
	if asset.calls != 1 || asset.lastAssetID != 77 {
		t.Errorf("GetAsset calls = %d (asset_id=%d), want 1 次且查 77", asset.calls, asset.lastAssetID)
	}
	if rights.lastContentID != 123 {
		t.Errorf("rights content_id = %d, want epid 123", rights.lastContentID)
	}
}

func TestPrecheckPublishMissingRegionRejected(t *testing.T) {
	rights := &fakeRights{result: &repository.RightsCheckResult{Playable: true}}
	asset := &fakeAsset{meta: transcodedAsset()}
	g := newTestGuard(config.Config{}, rights, asset)
	e := &model.Episode{Epid: 123, AssetID: 77}

	if _, err := g.precheckPublish(context.Background(), e, ""); !errors.Is(err, model.ErrMissingRegion) {
		t.Fatalf("precheckPublish() = %v, want ErrMissingRegion", err)
	}
	if rights.calls != 0 {
		t.Errorf("CheckPlayable calls = %d, want 0（地区未知时不下发窗口校验）", rights.calls)
	}
}

func TestPrecheckPublishAssetNotTranscoded(t *testing.T) {
	rights := &fakeRights{result: &repository.RightsCheckResult{Playable: true}}
	asset := &fakeAsset{meta: &repository.AssetMeta{AssetID: 77, State: repository.AssetStateScanned}}
	g := newTestGuard(config.Config{}, rights, asset)
	e := &model.Episode{Epid: 123, AssetID: 77}

	if _, err := g.precheckPublish(context.Background(), e, "CN"); !errors.Is(err, model.ErrAssetNotReady) {
		t.Fatalf("precheckPublish() = %v, want ErrAssetNotReady", err)
	}
	if rights.calls != 0 {
		t.Errorf("CheckPlayable calls = %d, want 0（媒资未就绪时先拒绝，不再查窗口）", rights.calls)
	}
}

func TestAssetBindableAllowsScanned(t *testing.T) {
	asset := &fakeAsset{meta: &repository.AssetMeta{AssetID: 77, State: repository.AssetStateScanned}}
	g := newTestGuard(config.Config{}, nil, asset)

	if err := g.assetBindable(context.Background(), 77); err != nil {
		t.Fatalf("assetBindable(SCANNED) = %v, want nil", err)
	}
}

func TestAssetChecksRejectFailedAndUploaded(t *testing.T) {
	for _, state := range []repository.AssetState{repository.AssetStateUploaded, repository.AssetStateFailed} {
		asset := &fakeAsset{meta: &repository.AssetMeta{AssetID: 77, State: state}}
		g := newTestGuard(config.Config{}, nil, asset)

		if err := g.assetBindable(context.Background(), 77); !errors.Is(err, model.ErrAssetNotReady) {
			t.Errorf("assetBindable(%s) = %v, want ErrAssetNotReady", state, err)
		}
		if err := g.assetPublishable(context.Background(), 77); !errors.Is(err, model.ErrAssetNotReady) {
			t.Errorf("assetPublishable(%s) = %v, want ErrAssetNotReady", state, err)
		}
	}
}

func TestAssetNotFoundPropagates(t *testing.T) {
	// rights/asset 适配器把下游“不存在”归一化为 catalog 哨兵错误，guard 按 errors.Is 分叉。
	asset := &fakeAsset{err: fmt.Errorf("get: %w", model.ErrAssetNotFound)}
	g := newTestGuard(config.Config{}, nil, asset)

	if err := g.assetPublishable(context.Background(), 77); !errors.Is(err, model.ErrAssetNotFound) {
		t.Fatalf("assetPublishable() = %v, want ErrAssetNotFound", err)
	}
}

func TestAssetClientNotConfigured(t *testing.T) {
	g := newTestGuard(config.Config{}, nil, nil)

	if err := g.assetBindable(context.Background(), 77); !errors.Is(err, model.ErrAssetCheckerUnavailable) {
		t.Fatalf("assetBindable() = %v, want ErrAssetCheckerUnavailable", err)
	}
}

func TestAssetEmptyReplyIsRejected(t *testing.T) {
	// 适配器返回 (nil, nil) 时不能当作“媒资就绪”。
	asset := &fakeAsset{meta: nil}
	g := newTestGuard(config.Config{}, nil, asset)

	if err := g.assetPublishable(context.Background(), 77); !errors.Is(err, model.ErrAssetNotFound) {
		t.Fatalf("assetPublishable() = %v, want ErrAssetNotFound", err)
	}
}

func TestChecksDisabledSkipCalls(t *testing.T) {
	rights := &fakeRights{err: errors.New("must not be called")}
	asset := &fakeAsset{err: errors.New("must not be called")}
	g := newTestGuard(config.Config{DisableAssetCheck: true, DisableRightsCheck: true}, rights, asset)

	e := &model.Episode{Epid: 123, AssetID: 77}
	if _, err := g.precheckPublish(context.Background(), e, ""); err != nil {
		t.Fatalf("precheckPublish() = %v, want nil when both checks disabled", err)
	}
	if err := g.assetBindable(context.Background(), 77); err != nil {
		t.Fatalf("assetBindable() = %v, want nil when disabled", err)
	}
	if asset.calls != 0 || rights.calls != 0 {
		t.Fatalf("calls = asset %d / rights %d, want 0 when disabled", asset.calls, rights.calls)
	}
}
