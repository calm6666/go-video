package repository

// rpcclient_test.go 验证适配器对下游契约的归一化行为：
// rights 的内容类型映射、asset 的“不存在”识别与状态转换。
// 通过内嵌生成的客户端接口只覆写用到的方法，不需要真实连接。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc"

	assetrpc "go-video/services/asset/rpc"
	"go-video/services/catalog/model"
	rightsrpc "go-video/services/rights/rpc"
)

// stubRightsRPC 实现 rightsrpc.RightsClient，只覆盖 CheckPlayable。
type stubRightsRPC struct {
	rightsrpc.RightsClient
	req   *rightsrpc.CheckReq
	reply *rightsrpc.CheckReply
	err   error
}

func (s *stubRightsRPC) CheckPlayable(_ context.Context, in *rightsrpc.CheckReq, _ ...grpc.CallOption) (*rightsrpc.CheckReply, error) {
	s.req = in
	if s.err != nil {
		return nil, s.err
	}
	return s.reply, nil
}

// stubAssetRPC 实现 assetrpc.AssetClient，只覆盖 GetAsset。
type stubAssetRPC struct {
	assetrpc.AssetClient
	req   *assetrpc.AssetReq
	reply *assetrpc.AssetReply
	err   error
}

func (s *stubAssetRPC) GetAsset(_ context.Context, in *assetrpc.AssetReq, _ ...grpc.CallOption) (*assetrpc.AssetReply, error) {
	s.req = in
	if s.err != nil {
		return nil, s.err
	}
	return s.reply, nil
}

func TestRightsAdapterMapsPGCAndResult(t *testing.T) {
	stub := &stubRightsRPC{reply: &rightsrpc.CheckReply{Playable: true, WindowId: 9, EndTime: 1234}}
	c := &rightsClient{cli: stub}

	res, err := c.CheckPlayable(context.Background(), 123, RightsContentTypePGC, "CN")
	if err != nil {
		t.Fatalf("CheckPlayable() error = %v", err)
	}
	if stub.req.GetContentType() != rightsrpc.ContentType_CONTENT_TYPE_PGC {
		t.Errorf("content_type = %v, want PGC", stub.req.GetContentType())
	}
	if res.WindowID != 9 || res.EndTime != 1234 || !res.Playable {
		t.Errorf("result = %+v, want {true 9 1234}", res)
	}
}

func TestRightsAdapterEmptyReplyIsError(t *testing.T) {
	c := &rightsClient{cli: &stubRightsRPC{reply: nil}}
	if _, err := c.CheckPlayable(context.Background(), 123, RightsContentTypePGC, "CN"); err == nil {
		t.Fatal("CheckPlayable() = nil error, want error（空回复不能当作窗口有效）")
	}
}

func TestAssetAdapterMapsNotFoundAndState(t *testing.T) {
	// 下游以错误表达“媒资不存在”，适配器归一化为 catalog 哨兵，供 guard 区分。
	notFound := &assetClient{cli: &stubAssetRPC{err: errors.New("rpc error: code = Unknown desc = asset: not found")}}
	_, err := notFound.GetAsset(context.Background(), 77)
	if !errors.Is(err, model.ErrAssetNotFound) {
		t.Fatalf("GetAsset() = %v, want wrapped ErrAssetNotFound", err)
	}

	// 传输类错误不得被误判为不存在。
	unavailable := &assetClient{cli: &stubAssetRPC{err: errors.New("rpc error: code = Unavailable desc = connection refused")}}
	_, err = unavailable.GetAsset(context.Background(), 77)
	if errors.Is(err, model.ErrAssetNotFound) {
		t.Fatalf("GetAsset() = %v, want 非 ErrAssetNotFound", err)
	}

	okStub := &stubAssetRPC{reply: &assetrpc.AssetReply{
		AssetId: 77, Duration: 1200000, State: assetrpc.AssetState_STATE_TRANSCODED,
	}}
	meta, err := (&assetClient{cli: okStub}).GetAsset(context.Background(), 77)
	if err != nil {
		t.Fatalf("GetAsset() error = %v", err)
	}
	if okStub.req.GetAssetId() != 77 {
		t.Errorf("下游 asset_id = %d, want 77", okStub.req.GetAssetId())
	}
	if meta.State != AssetStateTranscoded || meta.AssetID != 77 || meta.Duration != 1200000 {
		t.Errorf("meta = %+v, want {77 1200000 TRANSCODED}", meta)
	}
	if got := AssetStateFailed.String(); !strings.Contains(got, "FAILED") {
		t.Errorf("AssetStateFailed.String() = %q, want 含 FAILED", got)
	}
}
