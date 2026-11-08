package media

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

func iptr(n int) *int   { return &n }
func bptr(b bool) *bool { return &b }
func falPayload(f falFamily, in falPayloadInput) map[string]any {
	return pyjson.Plain(falBuildVideoPayload(f, in)).(map[string]any)
}

func TestKlingV3StandardAndProPayloadShape(t *testing.T) {
	for _, fid := range []string{"kling-v3", "kling-v3-pro"} {
		meta := falFamilies[fid]
		if meta.ImageParamKey != "start_image_url" {
			t.Fatal(fid)
		}
		p := falPayload(meta, falPayloadInput{Prompt: "a mecha lands", Duration: iptr(7), AspectRatio: "16:9", Resolution: "1080p",
			NegativePrompt: "blurry", Audio: bptr(true), Seed: iptr(3)})
		want := map[string]any{"prompt": "a mecha lands", "aspect_ratio": "16:9", "duration": "7", "generate_audio": true, "negative_prompt": "blurry"}
		if !reflect.DeepEqual(p, want) {
			t.Fatal(fid, p)
		}
		p = falPayload(meta, falPayloadInput{Prompt: "animate it", ImageURL: "https://example.com/i.png", Duration: iptr(20),
			AspectRatio: "16:9", Resolution: "720p", Audio: bptr(false)})
		if p["start_image_url"] != "https://example.com/i.png" || p["duration"] != "15" || p["generate_audio"] != false {
			t.Fatal(fid, p)
		}
		if _, ok := p["image_url"]; ok {
			t.Fatal(fid, p)
		}
		if _, ok := p["aspect_ratio"]; ok {
			t.Fatal(fid, p)
		}
	}
}

func TestMinimaxH3IntDurationAndResolutionAlias(t *testing.T) {
	meta := falFamilies["minimax-h3"]
	p := falPayload(meta, falPayloadInput{Prompt: "x", Duration: iptr(7), AspectRatio: "16:9", Resolution: "720p", Audio: bptr(true)})
	if p["duration"] != 7 || p["resolution"] != "768P" || p["aspect_ratio"] != "16:9" {
		t.Fatal(p)
	}
	if _, ok := p["generate_audio"]; ok {
		t.Fatal(p)
	}
	hi := falPayload(meta, falPayloadInput{Prompt: "x", Duration: iptr(5), AspectRatio: "16:9", Resolution: "1080p"})
	if hi["resolution"] != "2K" {
		t.Fatal(hi)
	}
}

func TestH3MaxTurboStaticKeyAnd1080pAlias(t *testing.T) {
	meta := falFamilies["minimax-h3-max-turbo"]
	t2v := falPayload(meta, falPayloadInput{Prompt: "x", Duration: iptr(7), AspectRatio: "16:9", Resolution: "1080p", Seed: iptr(11)})
	if t2v["prompt_expansion_mode"] != "balanced" || t2v["resolution"] != "1080P" || t2v["duration"] != 7 || t2v["seed"] != 11 {
		t.Fatal(t2v)
	}
	i2v := falPayload(meta, falPayloadInput{Prompt: "x", ImageURL: "https://example.com/i.png", Duration: iptr(5), AspectRatio: "16:9", Resolution: "480p"})
	if i2v["prompt_expansion_mode"] != "balanced" || i2v["image_url"] != "https://example.com/i.png" {
		t.Fatal(i2v)
	}
	if _, ok := i2v["aspect_ratio"]; ok {
		t.Fatal(i2v)
	}
}

func TestImageDropKeysStripsAspectRatioOnI2v(t *testing.T) {
	for _, fid := range []string{"seedance-2.5", "minimax-h3", "grok-imagine-1.5"} {
		meta := falFamilies[fid]
		i2v := falPayload(meta, falPayloadInput{Prompt: "x", ImageURL: "https://example.com/i.png", Duration: iptr(5), AspectRatio: "16:9", Resolution: "480p"})
		if _, ok := i2v["aspect_ratio"]; ok {
			t.Fatal(fid, i2v)
		}
		t2v := falPayload(meta, falPayloadInput{Prompt: "x", Duration: iptr(5), AspectRatio: "16:9", Resolution: "480p"})
		if t2v["aspect_ratio"] != "16:9" {
			t.Fatal(fid, t2v)
		}
	}
}

func TestWan30AudioToggleUsesFamilyKeyAndStartImageURL(t *testing.T) {
	kw := falPayloadInput{Prompt: "x", Duration: iptr(7), AspectRatio: "16:9", Resolution: "720p", Audio: bptr(true)}
	withImage := kw
	withImage.ImageURL = "https://i.png"
	p := falPayload(falFamilies["wan-3.0"], withImage)
	if p["audio"] != true || p["start_image_url"] != "https://i.png" || p["duration"] != 7 {
		t.Fatal(p)
	}
	if _, ok := p["generate_audio"]; ok {
		t.Fatal(p)
	}
	if falPayload(falFamilies["veo3.1"], kw)["generate_audio"] != true {
		t.Fatal("veo3.1 lost generate_audio")
	}
}

// Captured FAL submits with a fake module whose handles complete with a per endpoint result
type fakeFalVideo struct {
	mu    sync.Mutex
	calls []fakeFalCall
}

type fakeFalCall struct {
	endpoint  string
	arguments map[string]any
}

func (f *fakeFalVideo) last() fakeFalCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

// Direct mode FAL with FAL_KEY set, no managed gateway and a fake client module
func withFakeFal(t *testing.T, result func(endpoint string) map[string]any) *fakeFalVideo {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("FAL_KEY", "test")
	t.Setenv("FAL_VIDEO_MODEL", "")
	f := &fakeFalVideo{}
	mod := &falClientModule{Submit: func(_ context.Context, _ *tools.Env, endpoint string, arguments map[string]any, _ map[string]string) (*falRequestHandle, error) {
		f.mu.Lock()
		f.calls = append(f.calls, fakeFalCall{endpoint, arguments})
		f.mu.Unlock()
		return completedFalHandle(result(endpoint)), nil
	}}
	videoFalClientMu.Lock()
	prevMod := videoFalClient
	videoFalClient = mod
	videoFalClientMu.Unlock()
	managedFalVideoClientMu.Lock()
	managedFalVideoClient, managedFalVideoClientConfig, managedFalVideoClientConfigured = nil, [2]string{}, false
	managedFalVideoClientMu.Unlock()
	prevGW := resolveManagedFalVideoGateway
	resolveManagedFalVideoGateway = func(*tools.Env) (*tools.ManagedToolGatewayConfig, error) { return nil, nil }
	t.Cleanup(func() {
		resolveManagedFalVideoGateway = prevGW
		videoFalClientMu.Lock()
		videoFalClient = prevMod
		videoFalClientMu.Unlock()
	})
	return f
}

func TestTextOnlyJobErrorsCleanlyForI2vOnlyFamily(t *testing.T) {
	synthetic := newFalFamily("Synthetic i2v", "~1s", "cheap", "test", "", "example/i2v-only/image-to-video", func(f *falFamily) {
		f.Durations, f.DurationInt = []int{3, 10}, true
	})
	prevAvail, prevFamily, prevSubmit := falVideoAvailable, falResolveFamily, submitFalVideoRequest
	falVideoAvailable = func(*tools.Env) (bool, error) { return true, nil }
	falResolveFamily = func(context.Context, *tools.Env, string) (string, *falFamily, error) {
		return "synthetic", &synthetic, nil
	}
	submitFalVideoRequest = func(context.Context, *tools.Env, string, map[string]any) (*falRequestHandle, error) {
		t.Fatal("submitted to a None endpoint")
		return nil, nil
	}
	t.Cleanup(func() { falVideoAvailable, falResolveFamily, submitFalVideoRequest = prevAvail, prevFamily, prevSubmit })
	res, err := newFalVideoGenProvider().Generate(context.Background(), nil, VideoGenRequest{Prompt: "a dog running"})
	if err != nil || res["success"] != false || res["error_type"] != "modality_unsupported" {
		t.Fatal(res, err)
	}
}

func TestEveryFamilyHasRequiredMetadata(t *testing.T) {
	for _, fid := range falFamilyOrder {
		meta := falFamilies[fid]
		if meta.Display == "" || (meta.Tier != "cheap" && meta.Tier != "premium") || (meta.TextEndpoint == "" && meta.ImageEndpoint == "") {
			t.Fatal(fid)
		}
	}
	if len(falFamilyOrder) != len(falFamilies) {
		t.Fatal("family order and catalog disagree")
	}
}

func nativeResult(string) map[string]any {
	return map[string]any{"video": map[string]any{"url": "https://fake/out.mp4"}}
}

func TestFamilyRoutingTextToVideoRoutesToTextEndpoint(t *testing.T) {
	f := withFakeFal(t, nativeResult)
	res, err := newFalVideoGenProvider().Generate(context.Background(), nil, VideoGenRequest{Prompt: "a dog running", Model: "pixverse-v6"})
	if err != nil || res["success"] != true || res["modality"] != "text" {
		t.Fatal(res, err)
	}
	call := f.last()
	if call.endpoint != "fal-ai/pixverse/v6/text-to-video" || call.arguments["prompt"] != "a dog running" {
		t.Fatal(call)
	}
	if _, ok := call.arguments["image_url"]; ok {
		t.Fatal(call.arguments)
	}
}

func TestFamilyRoutingImageToVideoRoutesToImageEndpoint(t *testing.T) {
	f := withFakeFal(t, nativeResult)
	res, err := newFalVideoGenProvider().Generate(context.Background(), nil,
		VideoGenRequest{Prompt: "animate this dog", Model: "pixverse-v6", ImageURL: "https://example.com/dog.png"})
	if err != nil || res["success"] != true || res["modality"] != "image" {
		t.Fatal(res, err)
	}
	call := f.last()
	if call.endpoint != "fal-ai/pixverse/v6/image-to-video" || call.arguments["image_url"] != "https://example.com/dog.png" {
		t.Fatal(call)
	}
}

func TestFamilyRoutingDefaultFamilyTextRouting(t *testing.T) {
	f := withFakeFal(t, nativeResult)
	res, err := newFalVideoGenProvider().Generate(context.Background(), nil, VideoGenRequest{Prompt: "a dog"})
	if err != nil || res["success"] != true || f.last().endpoint != falFamilies[falVideoDefaultModel].TextEndpoint {
		t.Fatal(res, err)
	}
}

func TestFamilyRoutingUnknownFamilyFallsBackToDefault(t *testing.T) {
	f := withFakeFal(t, nativeResult)
	res, err := newFalVideoGenProvider().Generate(context.Background(), nil, VideoGenRequest{Prompt: "x", Model: "not-a-real-family"})
	if err != nil || res["success"] != true || f.last().endpoint != falFamilies[falVideoDefaultModel].TextEndpoint {
		t.Fatal(res, err)
	}
}

func TestFamilyKeyNormalizationFullEndpointPathsResolveToTheirOwnFamily(t *testing.T) {
	for _, fid := range falFamilyOrder {
		meta := falFamilies[fid]
		for _, ep := range []string{meta.TextEndpoint, meta.ImageEndpoint} {
			if ep != "" && falNormalizeFamilyKey(ep) != fid {
				t.Fatal(ep, falNormalizeFamilyKey(ep))
			}
		}
	}
}

func TestFamilyKeyNormalizationBareAndPrefixedIdsStillResolve(t *testing.T) {
	cases := map[string]string{"seedance-2.5": "seedance-2.5", "bytedance/seedance-2.5": "seedance-2.5", "  pixverse-v6  ": "pixverse-v6", "nonsense/thing": ""}
	for in, want := range cases {
		if got := falNormalizeFamilyKey(in); got != want {
			t.Fatal(in, got)
		}
	}
}

func TestFamilyKeyNormalizationTruncatedEndpointStemsResolve(t *testing.T) {
	cases := map[string]string{"bytedance/seedance-2.0/mini": "seedance-2.0-mini", "bytedance/seedance-2.0": "seedance-2.0", "minimax/h3": "minimax-h3",
		"xai/grok-imagine-video/v1.5": "grok-imagine-1.5", "google/gemini-omni-flash": "gemini-omni-flash", "blackforestlabs/flux-3": "flux-3"}
	for in, want := range cases {
		if got := falNormalizeFamilyKey(in); got != want {
			t.Fatal(in, got)
		}
	}
}

func TestFamilyKeyNormalizationCapabilitiesSpanLongestFamilyDuration(t *testing.T) {
	prev := falResolveFamily
	t.Cleanup(func() { falResolveFamily = prev })
	use := func(fid string) {
		meta := falFamilies[fid]
		falResolveFamily = func(context.Context, *tools.Env, string) (string, *falFamily, error) { return fid, &meta, nil }
	}
	use("seedance-2.5")
	if caps := newFalVideoGenProvider().Capabilities(); caps["max_duration"].(int) < 30 {
		t.Fatal(caps)
	}
	use("pixverse-v6")
	if caps := newFalVideoGenProvider().Capabilities(); caps["max_duration"] != falFamilies["pixverse-v6"].Durations[1] {
		t.Fatal(caps)
	}
	falResolveFamily = func(context.Context, *tools.Env, string) (string, *falFamily, error) {
		return "", nil, &imageValueError{"no config"}
	}
	caps := newFalVideoGenProvider().Capabilities()
	if caps["max_duration"].(int) < 30 || caps["min_duration"].(int) > 1 {
		t.Fatal(caps)
	}
}

func TestPayloadBuilderDropsUnsupportedKeys(t *testing.T) {
	p := falPayload(falFamilies["veo3.1"], falPayloadInput{Prompt: "x", Duration: iptr(12), AspectRatio: "16:9", Resolution: "720p",
		NegativePrompt: "ugly", Audio: bptr(true), Seed: iptr(42)})
	want := map[string]any{"prompt": "x", "duration": "8s", "aspect_ratio": "16:9", "resolution": "720p", "generate_audio": true, "negative_prompt": "ugly", "seed": 42}
	if !reflect.DeepEqual(p, want) {
		t.Fatal(p)
	}
}

func TestPayloadBuilderSeedDroppedForFamiliesWithoutSeedSupport(t *testing.T) {
	for _, fid := range []string{"seedance-2.0", "seedance-2.0-mini", "seedance-2.5", "minimax-h3", "flux-3", "grok-imagine-1.5", "gemini-omni-flash"} {
		t.Run(fid, func(t *testing.T) {
			p := falPayload(falFamilies[fid], falPayloadInput{Prompt: "x", ImageURL: "https://i.png", AspectRatio: "16:9", Resolution: "720p", Seed: iptr(42)})
			if _, ok := p["seed"]; ok {
				t.Fatal(p)
			}
		})
	}
}

func TestPayloadBuilderAudioOnlySentForFamiliesThatDeclareIt(t *testing.T) {
	for _, fid := range []string{"minimax-h3", "grok-imagine-1.5", "gemini-omni-flash"} {
		p := falPayload(falFamilies[fid], falPayloadInput{Prompt: "x", ImageURL: "https://i.png", AspectRatio: "16:9", Resolution: "720p",
			NegativePrompt: "ugly", Audio: bptr(true)})
		if _, ok := p["generate_audio"]; ok {
			t.Fatal(fid, p)
		}
		if _, ok := p["negative_prompt"]; ok {
			t.Fatal(fid, p)
		}
	}
}

func TestPayloadBuilderDurationIsEmittedInTheFormFalDeclares(t *testing.T) {
	cases := []struct {
		fid  string
		want any
	}{{"minimax-h3", 7}, {"flux-3", 7}, {"grok-imagine-1.5", 7}, {"gemini-omni-flash", 7}, {"seedance-2.5", "7"},
		{"seedance-2.0-mini", "7"}, {"pixverse-v6", "7"}, {"veo3.1", "6s"}}
	for _, c := range cases {
		t.Run(c.fid, func(t *testing.T) {
			p := falPayload(falFamilies[c.fid], falPayloadInput{Prompt: "x", Duration: iptr(7), AspectRatio: "16:9", Resolution: "720p"})
			if p["duration"] != c.want || reflect.TypeOf(p["duration"]) != reflect.TypeOf(c.want) {
				t.Fatalf("%#v", p["duration"])
			}
		})
	}
}

func TestPayloadBuilderEveryFamilyDeclaresBothEndpoints(t *testing.T) {
	for _, fid := range falFamilyOrder {
		if falFamilies[fid].TextEndpoint == "" || falFamilies[fid].ImageEndpoint == "" {
			t.Fatal(fid)
		}
	}
}

func TestPayloadBuilderLtxOmitsDurationAspectResolution(t *testing.T) {
	p := falPayload(falFamilies["ltx-2.3"], falPayloadInput{Prompt: "x", Duration: iptr(8), AspectRatio: "16:9", Resolution: "720p",
		NegativePrompt: "ugly", Audio: bptr(true)})
	for _, k := range []string{"duration", "aspect_ratio", "resolution"} {
		if _, ok := p[k]; ok {
			t.Fatal(k, p)
		}
	}
	if p["generate_audio"] != true || p["negative_prompt"] != "ugly" {
		t.Fatal(p)
	}
}

func TestPayloadBuilderRangeFamiliesOmitDurationWhenUnspecified(t *testing.T) {
	for _, fid := range []string{"pixverse-v6", "seedance-2.0", "kling-v3-4k"} {
		p := falPayload(falFamilies[fid], falPayloadInput{Prompt: "x", AspectRatio: "16:9", Resolution: "720p"})
		if d, ok := p["duration"]; ok {
			t.Fatalf("%s: duration=None should omit the field, got %#v", fid, d)
		}
	}
}

func TestPayloadBuilderLtx25Payload(t *testing.T) {
	meta := falFamilies["ltx-2.5"]
	p := falPayload(meta, falPayloadInput{Prompt: "a drone shot", Duration: iptr(7), AspectRatio: "16:9", Resolution: "4k",
		NegativePrompt: "blurry", Audio: bptr(true), Seed: iptr(42)})
	if !reflect.DeepEqual(p, map[string]any{"prompt": "a drone shot", "aspect_ratio": "16:9", "resolution": "2160p", "duration": 6, "generate_audio": true}) {
		t.Fatal(p)
	}
	p = falPayload(meta, falPayloadInput{Prompt: "animate", ImageURL: "https://example.com/f.png", Duration: iptr(19), AspectRatio: "9:16",
		Resolution: "720p", Seed: iptr(7)})
	if !reflect.DeepEqual(p, map[string]any{"prompt": "animate", "image_url": "https://example.com/f.png", "aspect_ratio": "9:16", "resolution": "720p", "duration": 18}) {
		t.Fatal(p)
	}
	kw := func(d *int, res string) map[string]any {
		return falPayload(meta, falPayloadInput{Prompt: "x", AspectRatio: "16:9", Duration: d, Resolution: res})
	}
	if kw(iptr(18), "4k")["duration"] != 10 || kw(iptr(18), "2k")["duration"] != 10 || kw(iptr(18), "1080p")["duration"] != 18 {
		t.Fatal("resolution cap")
	}
	if _, ok := kw(nil, "4k")["duration"]; ok {
		t.Fatal("unspecified duration emitted")
	}
}

func upscaleResult(endpoint string) map[string]any {
	if len(endpoint) >= len("upscale/video") && endpoint[len(endpoint)-len("upscale/video"):] == "upscale/video" {
		return map[string]any{"video": map[string]any{"url": "https://fake/upscaled.mp4"}}
	}
	return map[string]any{"video": map[string]any{"url": "https://fake/native.mp4"}}
}

func TestUpscalePassUpscaleChainsSeedvr(t *testing.T) {
	f := withFakeFal(t, upscaleResult)
	res, err := newFalVideoGenProvider().Generate(context.Background(), nil,
		VideoGenRequest{Prompt: "a dog", Model: "pixverse-v6", Kwargs: map[string]any{"upscale": true}})
	if err != nil || res["success"] != true || res["video"] != "https://fake/upscaled.mp4" || res["upscaled"] != true || res["upscale_factor"] != 2 {
		t.Fatal(res, err)
	}
	if len(f.calls) != 2 || f.calls[0].endpoint != "fal-ai/pixverse/v6/text-to-video" || f.calls[1].endpoint != falUpscalerEndpoint {
		t.Fatal(f.calls)
	}
	if f.calls[1].arguments["video_url"] != "https://fake/native.mp4" || f.calls[1].arguments["upscale_mode"] != "factor" {
		t.Fatal(f.calls[1].arguments)
	}
}

func TestUpscalePassNoUpscaleByDefault(t *testing.T) {
	f := withFakeFal(t, upscaleResult)
	res, err := newFalVideoGenProvider().Generate(context.Background(), nil, VideoGenRequest{Prompt: "a dog", Model: "pixverse-v6"})
	if err != nil || res["success"] != true || res["video"] != "https://fake/native.mp4" || res["upscaled"] != false || len(f.calls) != 1 {
		t.Fatal(res, err, f.calls)
	}
}

func TestUpscalePassUpscaleFailureFallsBackToNative(t *testing.T) {
	withFakeFal(t, upscaleResult)
	prev := falUpscaleVideo
	falUpscaleVideo = func(context.Context, *tools.Env, string, string) string { return "" }
	t.Cleanup(func() { falUpscaleVideo = prev })
	res, err := newFalVideoGenProvider().Generate(context.Background(), nil,
		VideoGenRequest{Prompt: "a dog", Model: "pixverse-v6", Kwargs: map[string]any{"upscale": true}})
	if err != nil || res["success"] != true || res["video"] != "https://fake/native.mp4" || res["upscaled"] != false {
		t.Fatal(res, err)
	}
}

// Managed gateway resolved and the submit seam replaced by the given function
func withManagedVideoSubmit(t *testing.T, submit func(endpoint string, arguments map[string]any) (*falRequestHandle, error)) {
	t.Helper()
	prevGW, prevSubmit := resolveManagedFalVideoGateway, submitFalVideoRequest
	resolveManagedFalVideoGateway = func(*tools.Env) (*tools.ManagedToolGatewayConfig, error) {
		return &tools.ManagedToolGatewayConfig{}, nil
	}
	submitFalVideoRequest = func(_ context.Context, _ *tools.Env, endpoint string, arguments map[string]any) (*falRequestHandle, error) {
		return submit(endpoint, arguments)
	}
	t.Cleanup(func() { resolveManagedFalVideoGateway, submitFalVideoRequest = prevGW, prevSubmit })
}

func TestUpscalePassManagedUpscaleBindsTheSourceRequest(t *testing.T) {
	var captured map[string]any
	withManagedVideoSubmit(t, func(_ string, arguments map[string]any) (*falRequestHandle, error) {
		captured = arguments
		return completedFalHandle(map[string]any{"video": map[string]any{"url": "https://fake/upscaled.mp4"}}), nil
	})
	if got := falUpscaleVideo(context.Background(), nil, "https://fake/native.mp4", "source-request-1"); got != "https://fake/upscaled.mp4" {
		t.Fatal(got)
	}
	if captured["source_request_id"] != "source-request-1" {
		t.Fatal(captured)
	}
}

func TestUpscalePassManagedUpscaleWithoutSourceRequestFallsBack(t *testing.T) {
	called := false
	withManagedVideoSubmit(t, func(string, map[string]any) (*falRequestHandle, error) { called = true; return nil, nil })
	if got := falUpscaleVideo(context.Background(), nil, "https://fake/native.mp4", ""); got != "" {
		t.Fatal(got)
	}
	if called {
		t.Fatal("submitted without the source request")
	}
}
