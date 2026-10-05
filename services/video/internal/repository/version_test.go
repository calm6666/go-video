package repository

import (
	"context"
	"errors"
	"testing"

	"go-video/services/video/model"
)

// fakeVersionModel 只实现 ListByAid：LatestPlayableVersion 不写库，也不应触达连接。
type fakeVersionModel struct {
	rows []*model.VideoVersion
	err  error
	aid  int64
}

func (f *fakeVersionModel) Insert(ctx context.Context, v *model.VideoVersion) error {
	return errors.New("fakeVersionModel: Insert must not be called by read path")
}

func (f *fakeVersionModel) ListByAid(ctx context.Context, aid int64) ([]*model.VideoVersion, error) {
	f.aid = aid
	return f.rows, f.err
}

func TestLatestPlayableVersionPicksNewestWithAsset(t *testing.T) {
	fm := &fakeVersionModel{rows: []*model.VideoVersion{
		{Aid: 7, Version: 3, AssetID: "", State: model.StateTranscoding},
		{Aid: 7, Version: 2, AssetID: "asset-2", State: model.StatePublished},
		{Aid: 7, Version: 1, AssetID: "asset-1", State: model.StatePublished},
	}}
	got, err := (&Repository{verMd: fm}).LatestPlayableVersion(context.Background(), 7)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got == nil {
		t.Fatal("got nil version, want version 2")
	}
	if got.Version != 2 || got.AssetID != "asset-2" {
		t.Fatalf("got version=%d asset_id=%q, want version=2 asset_id=asset-2", got.Version, got.AssetID)
	}
	if fm.aid != 7 {
		t.Fatalf("queried aid=%d, want 7", fm.aid)
	}
}

func TestLatestPlayableVersionNoAssetAnywhere(t *testing.T) {
	fm := &fakeVersionModel{rows: []*model.VideoVersion{
		{Aid: 7, Version: 2, AssetID: ""},
		{Aid: 7, Version: 1, AssetID: ""},
	}}
	got, err := (&Repository{verMd: fm}).LatestPlayableVersion(context.Background(), 7)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != nil {
		t.Fatalf("got %+v, want nil (不可播放由 logic 判定，不在仓库伪造版次)", got)
	}
}

func TestLatestPlayableVersionEmptyList(t *testing.T) {
	got, err := (&Repository{verMd: &fakeVersionModel{}}).LatestPlayableVersion(context.Background(), 7)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != nil {
		t.Fatalf("got %+v, want nil", got)
	}
}

func TestLatestPlayableVersionPropagatesModelErr(t *testing.T) {
	sentinel := errors.New("query boom")
	_, err := (&Repository{verMd: &fakeVersionModel{err: sentinel}}).LatestPlayableVersion(context.Background(), 7)
	if !errors.Is(err, sentinel) {
		t.Fatalf("got %v, want %v", err, sentinel)
	}
}
