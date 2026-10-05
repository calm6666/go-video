package logic

import (
	"context"
	"errors"
	"testing"

	"go-video/gateway/app/internal/svc"
	catalogrpc "go-video/services/catalog/rpc"
	pbcrpc "go-video/services/playback/rpc"
	transcoderpc "go-video/services/transcode/rpc"
	videorpc "go-video/services/video/rpc"

	"google.golang.org/grpc"
)

// 播放来源解析只依赖三个下游 RPC 客户端接口，这里用内嵌接口 + 覆盖所需方法的方式打桩，
// 不引入真实 gRPC 连接。

type fakeVideo struct {
	videorpc.VideoClient
	src  *videorpc.PlayableSourceReply
	err  error
	reqs []*videorpc.PlayableSourceReq
}

func (f *fakeVideo) GetPlayableSource(ctx context.Context, in *videorpc.PlayableSourceReq,
	opts ...grpc.CallOption) (*videorpc.PlayableSourceReply, error) {
	f.reqs = append(f.reqs, in)
	return f.src, f.err
}

type fakeCatalog struct {
	catalogrpc.CatalogClient
	ep  *catalogrpc.EpisodeReply
	err error
}

func (f *fakeCatalog) GetEpisode(ctx context.Context, in *catalogrpc.EpisodeReq,
	opts ...grpc.CallOption) (*catalogrpc.EpisodeReply, error) {
	return f.ep, f.err
}

type fakeTranscode struct {
	transcoderpc.TranscodeClient
	tasks     *transcoderpc.TasksReply
	templates map[int64]*transcoderpc.TemplateReply
	listErr   error
	tplErr    error
	listReq   *transcoderpc.ListReq
}

func (f *fakeTranscode) ListTasks(ctx context.Context, in *transcoderpc.ListReq,
	opts ...grpc.CallOption) (*transcoderpc.TasksReply, error) {
	f.listReq = in
	return f.tasks, f.listErr
}

func (f *fakeTranscode) GetTemplate(ctx context.Context, in *transcoderpc.TemplateReq,
	opts ...grpc.CallOption) (*transcoderpc.TemplateReply, error) {
	if f.tplErr != nil {
		return nil, f.tplErr
	}
	tpl, ok := f.templates[in.GetTemplateId()]
	if !ok {
		return nil, errors.New("template not found")
	}
	return tpl, nil
}

func newSvcCtx(v videorpc.VideoClient, c catalogrpc.CatalogClient, t transcoderpc.TranscodeClient) *svc.ServiceContext {
	return &svc.ServiceContext{Video: v, Catalog: c, Transcode: t}
}

func ugcSource(assetID string) *videorpc.PlayableSourceReply {
	return &videorpc.PlayableSourceReply{
		Aid:             42,
		Version:         &videorpc.VideoVersion{Aid: 42, Version: 7, AssetId: assetID},
		SubmissionState: videorpc.SubmissionState_STATE_PUBLISHED,
	}
}

func task(id, assetID, tplID int64, bucket, key string, state transcoderpc.TaskState) *transcoderpc.TaskReply {
	return &transcoderpc.TaskReply{
		TaskId: id, AssetId: assetID, TemplateId: tplID,
		OutputBucket: bucket, OutputKey: key, State: state,
	}
}

func TestResolvePlayableSourcePicksHighestQuality(t *testing.T) {
	tr := &fakeTranscode{
		tasks: &transcoderpc.TasksReply{Tasks: []*transcoderpc.TaskReply{
			task(1, 99, 11, "out-bucket", "v/11/720p.m3u8", transcoderpc.TaskState_TASK_STATE_SUCCEEDED),
			task(2, 99, 12, "out-bucket", "v/12/1080p.m3u8", transcoderpc.TaskState_TASK_STATE_SUCCEEDED),
			task(3, 99, 13, "out-bucket", "", transcoderpc.TaskState_TASK_STATE_SUCCEEDED),
			task(4, 99, 14, "out-bucket", "v/14/4k.m3u8", transcoderpc.TaskState_TASK_STATE_FAILED),
		}},
		templates: map[int64]*transcoderpc.TemplateReply{
			11: {TemplateId: 11, Name: "720P", Height: 720, Bitrate: 2000},
			12: {TemplateId: 12, Name: "1080P", Height: 1080, Bitrate: 6000},
		},
	}
	src, err := resolvePlayableSource(context.Background(),
		newSvcCtx(&fakeVideo{src: ugcSource("99")}, nil, tr),
		contentTypeUGC, 42, 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src.templateID != 12 || src.quality != "1080P" {
		t.Fatalf("expected highest template 1080P, got %+v", src)
	}
	if src.objectKey != "out-bucket/v/12/1080p.m3u8" {
		t.Fatalf("object_key must be bucket/key for CDN base URL, got %q", src.objectKey)
	}
	if src.assetID != 99 {
		t.Fatalf("assetID = %d, want 99", src.assetID)
	}
	if tr.listReq.AssetId != 99 || tr.listReq.State != transcoderpc.TaskState_TASK_STATE_SUCCEEDED {
		t.Fatalf("ListTasks filter = %+v, want succeeded tasks of asset 99", tr.listReq)
	}
}

func TestResolvePlayableSourceMatchesTemplateExactly(t *testing.T) {
	tr := &fakeTranscode{
		tasks: &transcoderpc.TasksReply{Tasks: []*transcoderpc.TaskReply{
			task(1, 99, 11, "out", "v/11/720p.m3u8", transcoderpc.TaskState_TASK_STATE_SUCCEEDED),
			task(2, 99, 12, "out", "v/12/1080p.m3u8", transcoderpc.TaskState_TASK_STATE_SUCCEEDED),
		}},
		templates: map[int64]*transcoderpc.TemplateReply{
			11: {TemplateId: 11, Name: "720P", Height: 720},
			12: {TemplateId: 12, Name: "1080P", Height: 1080},
		},
	}
	src, err := resolvePlayableSource(context.Background(),
		newSvcCtx(&fakeVideo{src: ugcSource("99")}, nil, tr),
		int32(pbcrpc.ContentType_CONTENT_TYPE_UGC), 42, 0, 11)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src.templateID != 11 || src.quality != "720P" {
		t.Fatalf("requested template 11 not honoured: %+v", src)
	}
}

func TestResolvePlayableSourceWithoutTemplateDetail(t *testing.T) {
	// 模板详情查不到时不应整体失败：退回候选顺序，quality 留空由客户端按 template_id 展示。
	tr := &fakeTranscode{
		tasks: &transcoderpc.TasksReply{Tasks: []*transcoderpc.TaskReply{
			task(1, 99, 11, "out", "v/11/a.m3u8", transcoderpc.TaskState_TASK_STATE_SUCCEEDED),
			task(2, 99, 12, "out", "v/12/b.m3u8", transcoderpc.TaskState_TASK_STATE_SUCCEEDED),
		}},
		tplErr: errors.New("template rpc down"),
	}
	src, err := resolvePlayableSource(context.Background(),
		newSvcCtx(&fakeVideo{src: ugcSource("99")}, nil, tr),
		contentTypeUGC, 42, 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src.templateID != 11 || src.quality != "" {
		t.Fatalf("fallback selection = %+v", src)
	}
}

func TestResolvePlayableSourceNoTranscodedVersion(t *testing.T) {
	tr := &fakeTranscode{
		tasks: &transcoderpc.TasksReply{Tasks: []*transcoderpc.TaskReply{
			task(1, 99, 11, "out", "", transcoderpc.TaskState_TASK_STATE_SUCCEEDED),
			task(2, 99, 12, "out", "v/12/b.m3u8", transcoderpc.TaskState_TASK_STATE_PROCESSING),
		}},
	}
	if _, err := resolvePlayableSource(context.Background(),
		newSvcCtx(&fakeVideo{src: ugcSource("99")}, nil, tr),
		contentTypeUGC, 42, 0, 0); !errors.Is(err, ErrNoPlayableVersion) {
		t.Fatalf("err = %v, want ErrNoPlayableVersion", err)
	}
}

func TestResolvePlayableSourceRejectsUnparsableAssetID(t *testing.T) {
	tr := &fakeTranscode{}
	if _, err := resolvePlayableSource(context.Background(),
		newSvcCtx(&fakeVideo{src: ugcSource("not-a-number")}, nil, tr),
		contentTypeUGC, 42, 0, 0); !errors.Is(err, ErrNoPlayableVersion) {
		t.Fatalf("err = %v, want ErrNoPlayableVersion", err)
	}
	if tr.listReq != nil {
		t.Fatal("must not query transcode when asset id is unusable")
	}
}

func TestResolvePlayableSourcePGCRequiresPublishedEpisode(t *testing.T) {
	cat := &fakeCatalog{ep: &catalogrpc.EpisodeReply{Epid: 7, State: 2, AssetId: 99}}
	if _, err := resolvePlayableSource(context.Background(),
		newSvcCtx(nil, cat, &fakeTranscode{}),
		contentTypePGC, 0, 7, 0); !errors.Is(err, ErrNoPlayableVersion) {
		t.Fatalf("unpublished episode err = %v, want ErrNoPlayableVersion", err)
	}

	cat = &fakeCatalog{ep: &catalogrpc.EpisodeReply{Epid: 7, State: catalogEpisodePublished, AssetId: 99}}
	tr := &fakeTranscode{
		tasks: &transcoderpc.TasksReply{Tasks: []*transcoderpc.TaskReply{
			task(1, 99, 11, "pgc-out", "e/11/1080p.m3u8", transcoderpc.TaskState_TASK_STATE_SUCCEEDED),
		}},
		templates: map[int64]*transcoderpc.TemplateReply{11: {TemplateId: 11, Name: "1080P", Height: 1080}},
	}
	src, err := resolvePlayableSource(context.Background(),
		newSvcCtx(nil, cat, tr), contentTypePGC, 0, 7, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src.objectKey != "pgc-out/e/11/1080p.m3u8" {
		t.Fatalf("objectKey = %q", src.objectKey)
	}
}

func TestResolvePlayableSourceValidatesContentAndConfig(t *testing.T) {
	ctx := context.Background()
	if _, err := resolvePlayableSource(ctx, &svc.ServiceContext{}, contentTypeUGC, 42, 0, 0); !errors.Is(err, ErrSourceNotConfigured) {
		t.Fatalf("err = %v, want ErrSourceNotConfigured", err)
	}
	// UGC 缺 aid / PGC 缺 epid / 未知内容类型都是请求非法，不应打到下游。
	good := &fakeTranscode{}
	cases := []struct {
		name        string
		contentType int32
		aid, epid   int64
	}{
		{"ugc without aid", contentTypeUGC, 0, 0},
		{"pgc without epid", contentTypePGC, 0, 0},
		{"unknown content type", 99, 42, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := resolvePlayableSource(ctx, newSvcCtx(&fakeVideo{src: ugcSource("99")}, &fakeCatalog{}, good),
				c.contentType, c.aid, c.epid, 0); !errors.Is(err, ErrInvalidContent) {
				t.Fatalf("err = %v, want ErrInvalidContent", err)
			}
		})
	}
}

func TestResolvePlayableSourcePropagatesDownstreamError(t *testing.T) {
	sentinel := errors.New("video rpc unavailable")
	if _, err := resolvePlayableSource(context.Background(),
		newSvcCtx(&fakeVideo{err: sentinel}, nil, &fakeTranscode{}),
		contentTypeUGC, 42, 0, 0); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want downstream error propagated", err)
	}
}
