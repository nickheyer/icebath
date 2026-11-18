package media

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Environment with an in memory image_gen section and a private home
func imageGenEnv(t *testing.T, imageGen map[string]any) *tools.Env {
	t.Helper()
	cfg := config.Tree{}
	if imageGen != nil {
		cfg["image_gen"] = imageGen
	}
	return &tools.Env{Home: t.TempDir(), Config: cfg}
}

// Decoded JSON object of a tool result
func decodeResult(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("%v in %q", err, raw)
	}
	return m
}

// Request handle whose status reads completed and whose result is the given body
func completedFalHandle(result map[string]any) *falRequestHandle {
	in := newFalClientInternals()
	in.MaybeRetryRequest = func(_ context.Context, _ *http.Client, _, rawURL string, _ falHTTPRequest) (*falResponse, error) {
		body := map[string]any{"status": "COMPLETED", "logs": nil}
		if !strings.HasSuffix(rawURL, "/status") {
			body = result
		}
		raw, _ := json.Marshal(body)
		return &falResponse{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: raw}, nil
	}
	return in.SyncRequestHandle("r1", "https://q/result", "https://q/status", "https://q/cancel", http.DefaultClient)
}

type mockHTTPStatusError struct{ status int }

func (e *mockHTTPStatusError) Error() string { return "http error" }
func (e *mockHTTPStatusError) ResponseStatus() (int, bool) {
	return e.status, e.status != 0
}

func TestExtractHTTPStatusMockShapes(t *testing.T) {
	if s, ok := extractHTTPStatus(&mockHTTPStatusError{403}); !ok || s != 403 {
		t.Fatalf("%d %v", s, ok)
	}
	if _, ok := extractHTTPStatus(&mockHTTPStatusError{}); ok {
		t.Fatal("response without status code must be none")
	}
}

func TestKreaModelNormalization(t *testing.T) {
	for _, id := range []string{"krea-2-medium", "krea-2-large", "krea-2-medium-turbo"} {
		if normalizeKreaModel(id) != id {
			t.Fatal(id)
		}
	}
	for _, id := range []any{"fal-ai/flux-2/klein/9b", "fal-ai/nano-banana-pro", nil, "", 123} {
		if normalizeKreaModel(id) != "" {
			t.Fatal(id)
		}
	}
}

// Runs the FAL path with a native result and a recording upscaler
func runUpscaleOptIn(t *testing.T, model string, upscale any, upscaled any) (map[string]any, []string) {
	t.Helper()
	t.Setenv("FAL_IMAGE_MODEL", model)
	t.Setenv("FAL_KEY", "test-key")
	prevSubmit, prevUp := submitFalRequestHook, upscaleImageHook
	t.Cleanup(func() {
		submitFalRequestHook, upscaleImageHook = prevSubmit, prevUp
	})
	submitFalRequestHook = func(context.Context, *tools.Env, string, map[string]any) (*falRequestHandle, error) {
		return completedFalHandle(map[string]any{"images": []any{map[string]any{"url": "https://fal/native.png", "width": 1024, "height": 768}}}), nil
	}
	var calls []string
	upscaleImageHook = func(_ context.Context, _ *tools.Env, u, _ string) (map[string]any, error) {
		calls = append(calls, u)
		if upscaled == nil {
			return nil, nil
		}
		return upscaled.(map[string]any), nil
	}
	out := decodeResult(t, imageGenerateTool(context.Background(), imageGenEnv(t, nil), imageToolArgs{Prompt: "a cat", Upscale: upscale}))
	return out, calls
}

func TestUpscaleOptIn(t *testing.T) {
	on, off := true, false
	up := map[string]any{"url": "https://fal/upscaled.png", "width": 2048, "height": 1536, "upscaled": true, "upscale_factor": 2}
	for _, tc := range []struct {
		model   string
		upscale *bool
		called  bool
	}{
		{"bytedance/seedream/v5/lite/text-to-image", &on, true},
		{"fal-ai/flux-2/klein/9b", &off, false},
		{"bytedance/seedream/v5/lite/text-to-image", nil, false},
	} {
		out, calls := runUpscaleOptIn(t, tc.model, optionalBool(tc.upscale), up)
		want := "https://fal/native.png"
		if tc.called {
			want = "https://fal/upscaled.png"
		}
		if out["success"] != true || (len(calls) > 0) != tc.called || out["upscaled"] != tc.called || out["image"] != want {
			t.Fatalf("%s %v", tc.model, out)
		}
	}
}

func TestUpscaleFailureFallsBackToNative(t *testing.T) {
	on := true
	out, _ := runUpscaleOptIn(t, "fal-ai/flux-2/klein/9b", &on, nil)
	if out["success"] != true || out["image"] != "https://fal/native.png" || out["upscaled"] != false {
		t.Fatal(out)
	}
}

func TestUpscaleDispatchForwarding(t *testing.T) {
	on := true
	for _, upscale := range []*bool{&on, nil} {
		env := imageGenEnv(t, map[string]any{"provider": "krea"})
		krea := &fakeImageProvider{name: "krea", generate: func(ImageGenRequest) (map[string]any, error) {
			return map[string]any{"success": true, "image": "/tmp/x.png"}, nil
		}}
		usePluginProviders(t, krea)
		out, _ := dispatchToPluginProvider(context.Background(), env, "a cat", "square", imageSources{Upscale: upscale}, nil)
		if decodeResult(t, out)["success"] != true {
			t.Fatal(out)
		}
		v, has := krea.calls[0].Kwargs["upscale"]
		if (upscale != nil) != has || (upscale != nil && v != true) {
			t.Fatalf("upscale %v %v", v, has)
		}
	}
}

func TestFalKeyWhitespaceIsUnset(t *testing.T) {
	t.Setenv("FAL_KEY", "   ")
	if checkFalAPIKey(imageGenEnv(t, nil)) {
		t.Fatal("whitespace FAL_KEY counted as configured")
	}
}

func TestImageGenerateToolReturnsActionableErrorWhenNoBackend(t *testing.T) {
	t.Setenv("FAL_KEY", "")
	out := decodeResult(t, imageGenerateTool(context.Background(), imageGenEnv(t, nil), imageToolArgs{Prompt: "a cat"}))
	if out["success"] != false || out["error"] == "" || out["error"] == nil {
		t.Fatal(out)
	}
}

func TestHandlerForwardsOnlyDeclaredCreativeControls(t *testing.T) {
	env := imageGenEnv(t, map[string]any{"provider": "recorder"})
	rec := &fakeImageProvider{name: "recorder", caps: map[string]any{"modalities": []any{"text"}, "creative_controls": []any{"intensity"}},
		generate: func(req ImageGenRequest) (map[string]any, error) {
			return map[string]any{"success": true, "image": "/tmp/recorder.png", "model": "m", "prompt": req.Prompt,
				"aspect_ratio": req.AspectRatio, "provider": "recorder"}, nil
		}}
	usePluginProviders(t, rec)
	out := decodeResult(t, handleImageGenerate(context.Background(), &tools.Call{Env: env,
		Args: map[string]any{"prompt": "draw cat", "aspect_ratio": "square", "intensity": 80, "creativity": "raw"}}))
	if out["success"] != true || rec.calls[0].Kwargs["intensity"] != 80 {
		t.Fatal(out)
	}
	if _, has := rec.calls[0].Kwargs["creativity"]; has {
		t.Fatal("undeclared control forwarded")
	}
}

func TestDeepinfraKeyAloneDoesNotSelectImageBackend(t *testing.T) {
	t.Setenv("DEEPINFRA_API_KEY", "sk-test")
	t.Setenv("FAL_KEY", "")
	if _, handled := dispatchToPluginProvider(context.Background(), imageGenEnv(t, nil), "a cat", "square", imageSources{}, nil); handled {
		t.Fatal("deepinfra key selected a backend")
	}
}

func TestRequirementsIgnoreUnselectedPaidPlugin(t *testing.T) {
	prev := checkFalAPIKeyHook
	checkFalAPIKeyHook = func(*tools.Env) bool { return false }
	t.Cleanup(func() { checkFalAPIKeyHook = prev })
	if checkImageGenerationRequirements(imageGenEnv(t, nil)) {
		t.Fatal("unselected plugin satisfied requirements")
	}
}
