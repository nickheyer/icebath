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

func TestManagedKreaRoutingSkipsNonKreaModel(t *testing.T) {
	env := imageGenEnv(t, map[string]any{"model": "fal-ai/flux-2/klein/9b"})
	if _, handled := maybeRouteManagedModel(context.Background(), env, "p", "square", imageSources{}, nil); handled {
		t.Fatal("non krea model routed")
	}
}

func TestManagedKreaRoutingReachesKreaPlugin(t *testing.T) {
	env := imageGenEnv(t, map[string]any{"model": "krea-2-large"})
	prev := resolveManagedKreaGatewayHook
	resolveManagedKreaGatewayHook = func(*tools.Env) *tools.ManagedToolGatewayConfig {
		return &tools.ManagedToolGatewayConfig{Vendor: "krea", GatewayOrigin: "https://krea-gateway.example.com", NousUserToken: "tok", ManagedMode: true}
	}
	t.Cleanup(func() { resolveManagedKreaGatewayHook = prev })
	krea := &fakeImageProvider{name: "krea", generate: func(ImageGenRequest) (map[string]any, error) {
		return map[string]any{"success": true, "image": "/tmp/x.png"}, nil
	}}
	usePluginProviders(t, krea)
	out, handled := maybeRouteManagedModel(context.Background(), env, "a cat", "portrait", imageSources{}, nil)
	if !handled || decodeResult(t, out)["success"] != true {
		t.Fatalf("%v %s", handled, out)
	}
	req := krea.calls[0]
	if req.Kwargs["model"] != "krea-2-large" || req.Prompt != "a cat" || req.AspectRatio != "portrait" {
		t.Fatalf("%+v", req)
	}
}

func TestManagedPortalRoutingReachesNousPlugin(t *testing.T) {
	env := imageGenEnv(t, map[string]any{"provider": "nous", "model": "openai/gpt-5.4-image-2"})
	nous := &fakeImageProvider{name: "nous", generate: func(ImageGenRequest) (map[string]any, error) {
		return map[string]any{"success": true, "image": "/tmp/x.png"}, nil
	}}
	usePluginProviders(t, nous)
	out, _ := maybeRouteManagedModel(context.Background(), env, "a cat", "square", imageSources{}, nil)
	if decodeResult(t, out)["success"] != true || nous.calls[0].Kwargs["model"] != "openai/gpt-5.4-image-2" {
		t.Fatal(out)
	}
}

func TestManagedPortalModelWithoutPluginErrorsInsteadOfBillingFal(t *testing.T) {
	env := imageGenEnv(t, map[string]any{"provider": "nous", "model": "openai/gpt-5.4-image-2"})
	usePluginProviders(t)
	out, handled := maybeRouteManagedModel(context.Background(), env, "a cat", "square", imageSources{}, nil)
	if !handled || decodeResult(t, out)["success"] != false {
		t.Fatal(out)
	}
}

// Runs the FAL path with a native result and a recording upscaler
func runUpscaleOptIn(t *testing.T, model string, upscale any, upscaled any) (map[string]any, []string) {
	t.Helper()
	t.Setenv("FAL_IMAGE_MODEL", model)
	t.Setenv("FAL_KEY", "test-key")
	prevGW, prevSubmit, prevUp := resolveManagedFalGatewayHook, submitFalRequestHook, upscaleImageHook
	t.Cleanup(func() {
		resolveManagedFalGatewayHook, submitFalRequestHook, upscaleImageHook = prevGW, prevSubmit, prevUp
	})
	resolveManagedFalGatewayHook = func(*tools.Env) (*tools.ManagedToolGatewayConfig, error) { return nil, nil }
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
	prev := resolveManagedFalGatewayHook
	resolveManagedFalGatewayHook = func(*tools.Env) (*tools.ManagedToolGatewayConfig, error) { return nil, nil }
	t.Cleanup(func() { resolveManagedFalGatewayHook = prev })
	if checkFalAPIKey(imageGenEnv(t, nil)) {
		t.Fatal("whitespace FAL_KEY counted as configured")
	}
}

func TestImageGenerateToolReturnsActionableErrorWhenNoBackend(t *testing.T) {
	t.Setenv("FAL_KEY", "")
	prev := resolveManagedFalGatewayHook
	resolveManagedFalGatewayHook = func(*tools.Env) (*tools.ManagedToolGatewayConfig, error) { return nil, nil }
	t.Cleanup(func() { resolveManagedFalGatewayHook = prev })
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

// Replaces the dispatch seams so a managed Krea error reaches the FAL fallback
func useKreaFallbackSeams(t *testing.T, kreaError map[string]any, enabled bool, fal func(imageToolArgs) string) {
	t.Helper()
	prevD, prevM, prevT, prevE := dispatchToPluginProviderHook, maybeRouteManagedModelHook, imageGenerateToolHook, kreaFallbackToFalEnabledHook
	t.Cleanup(func() {
		dispatchToPluginProviderHook, maybeRouteManagedModelHook, imageGenerateToolHook, kreaFallbackToFalEnabledHook = prevD, prevM, prevT, prevE
	})
	dispatchToPluginProviderHook = func(context.Context, *tools.Env, string, string, imageSources, map[string]any) (string, bool) {
		return "", false
	}
	maybeRouteManagedModelHook = func(context.Context, *tools.Env, string, string, imageSources, map[string]any) (string, bool) {
		raw, _ := json.Marshal(kreaError)
		return string(raw), true
	}
	imageGenerateToolHook = func(_ context.Context, _ *tools.Env, a imageToolArgs) string { return fal(a) }
	kreaFallbackToFalEnabledHook = func(*tools.Env) bool { return enabled }
}

func TestManagedKreaFailureFallsBackToFalOnlyWhenSafe(t *testing.T) {
	for _, tc := range []struct {
		eligible              bool
		imageURL              string
		enabled, falOK, falls bool
	}{
		{true, "", true, true, true},
		{false, "", true, true, false},
		{true, "https://x/ref.png", true, true, false},
		{true, "", false, true, false},
		{true, "", true, false, true},
	} {
		var falCalls []imageToolArgs
		useKreaFallbackSeams(t, map[string]any{"success": false, "image": nil, "error": "Krea connection error",
			"error_type": "connection_error", "fallback_eligible": tc.eligible}, tc.enabled, func(a imageToolArgs) string {
			falCalls = append(falCalls, a)
			if tc.falOK {
				return `{"success": true, "image": "/tmp/fal.png", "modality": "text", "upscaled": false}`
			}
			return `{"success": false, "image": null, "error": "FAL down", "error_type": "api_error"}`
		})
		args := map[string]any{"prompt": "draw cat", "aspect_ratio": "square"}
		if tc.imageURL != "" {
			args["image_url"] = tc.imageURL
		}
		out := decodeResult(t, handleImageGenerate(context.Background(), &tools.Call{Env: imageGenEnv(t, nil), Args: args}))
		if !tc.falls {
			if len(falCalls) != 0 || out["error"] != "Krea connection error" || out["fallback_from"] != nil {
				t.Fatalf("%+v %v", tc, out)
			}
			continue
		}
		note, _ := out["note"].(string)
		want := "also failed"
		if tc.falOK {
			want = "generated on FAL"
		}
		if len(falCalls) != 1 || falCalls[0].FalModel != falDefaultModel || out["fallback_from"] != "krea" ||
			out["fallback_reason"] != "Krea connection error" || out["success"] != tc.falOK || !strings.Contains(note, want) {
			t.Fatalf("%+v %v", tc, out)
		}
	}
}

func TestFallbackKeepsUpscaleAndNamesFalUpscaler(t *testing.T) {
	var falCalls []imageToolArgs
	useKreaFallbackSeams(t, map[string]any{"success": false, "image": nil, "error": "refused", "error_type": "connection_error",
		"fallback_eligible": true}, true, func(a imageToolArgs) string {
		falCalls = append(falCalls, a)
		return `{"success": true, "image": "/tmp/fal.png", "modality": "text", "upscaled": true}`
	})
	out := decodeResult(t, handleImageGenerate(context.Background(), &tools.Call{Env: imageGenEnv(t, nil),
		Args: map[string]any{"prompt": "draw cat", "aspect_ratio": "square", "upscale": true}}))
	note, _ := out["note"].(string)
	if falCalls[0].Upscale != true || !strings.Contains(note, upscalerModel) || !strings.Contains(note, "Krea Enhance") {
		t.Fatal(out)
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
