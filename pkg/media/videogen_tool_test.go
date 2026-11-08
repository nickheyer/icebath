package media

import (
	"context"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Parsed JSON object of a tool result
func parseToolResult(t *testing.T, raw string) map[string]any {
	t.Helper()
	parsed, err := pyjson.Loads(raw)
	if err != nil {
		t.Fatal(raw, err)
	}
	m, _ := pyjson.Plain(parsed).(map[string]any)
	return m
}

// Runs the handler with the configured provider name stubbed and plugin discovery disabled
func runVideoDispatch(t *testing.T, args map[string]any, configured string) map[string]any {
	t.Helper()
	prevCfg, prevDiscover := readConfiguredVideoProvider, ensurePluginsDiscoveredHook
	readConfiguredVideoProvider = func(context.Context, *tools.Env) string { return configured }
	ensurePluginsDiscoveredHook = func(context.Context, bool) {}
	defer func() { readConfiguredVideoProvider, ensurePluginsDiscoveredHook = prevCfg, prevDiscover }()
	env := imageEnv(t, config.Tree{})
	t.Setenv("ICEBATH_HOME", env.Home)
	return parseToolResult(t, handleVideoGenerate(context.Background(), &tools.Call{Name: "video_generate", Args: args, Env: env}))
}

func TestUnifiedDispatchNoProviderReturnsClearError(t *testing.T) {
	resetVideoRegistry(t)
	res := runVideoDispatch(t, map[string]any{"prompt": "a dog"}, "")
	if res["success"] != false || res["error_type"] != "no_provider_configured" {
		t.Fatal(res)
	}
}

func TestUnifiedDispatchUnknownProviderReturnsClearError(t *testing.T) {
	resetVideoRegistry(t)
	res := runVideoDispatch(t, map[string]any{"prompt": "a dog"}, "ghost")
	if res["success"] != false || res["error_type"] != "provider_not_registered" {
		t.Fatal(res)
	}
}

func TestUnifiedDispatchUpscaleInSchemaAndForwarded(t *testing.T) {
	resetVideoRegistry(t)
	p := &fakeVideoProvider{name: "fake", models: []map[string]any{{"id": "model-a"}}, caps: map[string]any{"modalities": []any{"text", "image"}}}
	RegisterVideoGenProvider(p, "")
	res := runVideoDispatch(t, map[string]any{"prompt": "a dog", "upscale": true}, "fake")
	if res["success"] != true || p.calls[len(p.calls)-1].Kwargs["upscale"] != true {
		t.Fatal(res, p.calls)
	}
	runVideoDispatch(t, map[string]any{"prompt": "a dog"}, "fake")
	if _, ok := p.calls[len(p.calls)-1].Kwargs["upscale"]; ok {
		t.Fatal(p.calls[len(p.calls)-1])
	}
}

var videoCapabilityAxes = []string{"modalities", "aspect_ratios", "resolutions", "max_duration", "min_duration", "supports_audio",
	"supports_negative_prompt", "supports_seed", "supports_upscale", "max_reference_images"}

func TestFleetCapabilityCoverageEveryProviderDeclaresEveryAxis(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	resetDeepInfraCatalogCache(t)
	or := newOpenRouterVideoGenProvider()
	or.catalog = func(context.Context) []map[string]any { return openRouterFallbackCatalog() }
	providers := map[string]VideoGenProvider{"fal": newFalVideoGenProvider(), "xai": newXAIVideoGenProvider(),
		"deepinfra": newDeepInfraVideoGenProvider(), "openrouter": or}
	checked := 0
	for name, p := range providers {
		caps := p.Capabilities()
		for _, axis := range videoCapabilityAxes {
			if _, ok := caps[axis]; !ok {
				t.Fatalf("%s missing %s", name, axis)
			}
		}
		checked++
	}
	if checked < 4 {
		t.Fatal(checked)
	}
}

func TestFleetCapabilityCoverageAbcDefaultFailsClosed(t *testing.T) {
	caps := newOpenAICompatibleVideoGenProvider(openAICompatibleVideoGenProvider{providerID: "t"}).Capabilities()
	if !pyrepr.Equal(caps["modalities"], []any{"text"}) {
		t.Fatal(caps)
	}
	for _, axis := range []string{"supports_audio", "supports_negative_prompt", "supports_seed", "supports_upscale"} {
		if pyTruthy(caps[axis]) {
			t.Fatal(axis)
		}
	}
	for _, axis := range videoCapabilityAxes {
		if _, ok := caps[axis]; !ok {
			t.Fatal("ABC default missing", axis)
		}
	}
	if d := defaultVideoCapabilities(); !pyrepr.Equal(d["modalities"], []any{"text"}) {
		t.Fatal(d)
	}
}

func TestFleetCapabilityCoverageEveryFalFamilyDeclaresPerFamilyKeys(t *testing.T) {
	for _, fid := range falFamilyOrder {
		t.Run(fid, func(t *testing.T) {
			meta := falFamilies[fid]
			if meta.TextEndpoint == "" && meta.ImageEndpoint == "" {
				t.Fatalf("%s declares no endpoint", fid)
			}
			if meta.Audio && meta.AudioNative {
				t.Fatalf("%s: audio toggle and always-on are mutually exclusive", fid)
			}
			if len(meta.Durations) != 0 && len(meta.Durations) != 2 {
				t.Fatalf("%s durations is not a range", fid)
			}
		})
	}
}

// Dynamic schema for a provider with the given capabilities and m1 catalog entry
func schemaWith(t *testing.T, caps map[string]any, modelMeta map[string]any) map[string]any {
	t.Helper()
	entry := map[string]any{"id": "m1"}
	for k, v := range modelMeta {
		entry[k] = v
	}
	p := &fakeVideoProvider{name: "fake", caps: caps, models: []map[string]any{entry}}
	prevResolve, prevModel := videoResolveActiveProvider, readConfiguredVideoModel
	videoResolveActiveProvider = func(context.Context, *tools.Env) VideoGenProvider { return p }
	readConfiguredVideoModel = func(context.Context, *tools.Env) string { return "m1" }
	defer func() { videoResolveActiveProvider, readConfiguredVideoModel = prevResolve, prevModel }()
	return buildDynamicVideoSchema(nil)
}

func TestFleetCapabilityCoverageAlwaysOnAudioSurfacesInDescriptionNotParam(t *testing.T) {
	schema := schemaWith(t, map[string]any{"modalities": []any{"text", "image"}, "supports_audio": false, "audio_always_on": true,
		"supports_negative_prompt": false, "supports_seed": false, "supports_upscale": false, "max_reference_images": 0}, nil)
	if _, ok := schemaProps(schema)["audio"]; ok {
		t.Fatal(schemaProps(schema))
	}
	if !strings.Contains(schema["description"].(string), "always on") {
		t.Fatal(schema["description"])
	}
}

func TestDynamicParamGatingFullFeaturedBackendGetsAllParams(t *testing.T) {
	props := schemaProps(schemaWith(t, map[string]any{"modalities": []any{"text", "image"}, "aspect_ratios": []any{"16:9"},
		"resolutions": []any{"720p"}, "min_duration": 2, "max_duration": 12, "supports_audio": true, "supports_negative_prompt": true,
		"supports_seed": true, "supports_upscale": true, "max_reference_images": 7}, nil))
	for _, p := range []string{"image_url", "reference_image_urls", "negative_prompt", "audio", "seed", "upscale"} {
		if _, ok := props[p]; !ok {
			t.Fatal(p)
		}
	}
	if props["reference_image_urls"].(map[string]any)["maxItems"] != 7 {
		t.Fatal(props["reference_image_urls"])
	}
	d := props["duration"].(map[string]any)
	if !pyrepr.Equal(d["minimum"], 2) || !pyrepr.Equal(d["maximum"], 12) {
		t.Fatal(d)
	}
	if !pyrepr.Equal(props["aspect_ratio"].(map[string]any)["enum"], []any{"16:9"}) {
		t.Fatal(props["aspect_ratio"])
	}
}

func TestDynamicParamGatingMinimalBackendGetsBareParams(t *testing.T) {
	schema := schemaWith(t, map[string]any{"modalities": []any{"text"}, "supports_audio": false, "supports_negative_prompt": false,
		"supports_seed": false, "supports_upscale": false, "max_reference_images": 0}, nil)
	for _, p := range []string{"image_url", "reference_image_urls", "negative_prompt", "audio", "seed", "upscale"} {
		if _, ok := schemaProps(schema)[p]; ok {
			t.Fatal(p)
		}
	}
	if !strings.Contains(schema["description"].(string), "text-to-video only") {
		t.Fatal(schema["description"])
	}
}

func TestDynamicParamGatingI2vOnlyModelOverridesBackendUnion(t *testing.T) {
	schema := schemaWith(t, map[string]any{"modalities": []any{"text", "image"}, "max_reference_images": 0, "supports_audio": false,
		"supports_negative_prompt": false, "supports_seed": false, "supports_upscale": false}, map[string]any{"modalities": []any{"image"}})
	if _, ok := schemaProps(schema)["image_url"]; !ok {
		t.Fatal(schemaProps(schema))
	}
	if !strings.Contains(schema["description"].(string), "image-to-video only") {
		t.Fatal(schema["description"])
	}
}

func TestDynamicParamGatingNoProviderServesPromptOnly(t *testing.T) {
	prev := videoResolveActiveProvider
	videoResolveActiveProvider = func(context.Context, *tools.Env) VideoGenProvider { return nil }
	defer func() { videoResolveActiveProvider = prev }()
	var keys []string
	for k := range schemaProps(buildDynamicVideoSchema(nil)) {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "prompt" {
		t.Fatal(keys)
	}
}

func TestDynamicSchemaBuilderBuilderWiredIntoRegistry(t *testing.T) {
	entry := tools.Default.Get("video_generate")
	if entry == nil || entry.DynamicSchema == nil {
		t.Fatal("video_generate not registered with a dynamic schema")
	}
	resetVideoRegistry(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	prev := ensurePluginsDiscoveredHook
	ensurePluginsDiscoveredHook = func(context.Context, bool) {}
	defer func() { ensurePluginsDiscoveredHook = prev }()
	if _, ok := entry.DynamicSchema(nil)["description"]; !ok {
		t.Fatal("no description")
	}
}

// Home whose config.yaml holds the tree, with plugin discovery disabled
func cfgHome(t *testing.T, cfg config.Tree) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := config.Save(h, cfg); err != nil {
		t.Fatal(err)
	}
	prev := ensurePluginsDiscoveredHook
	ensurePluginsDiscoveredHook = func(context.Context, bool) {}
	t.Cleanup(func() { ensurePluginsDiscoveredHook = prev })
}

func TestDynamicSchemaBuilderBothModalitiesModelClaimsBoth(t *testing.T) {
	resetVideoRegistry(t)
	RegisterVideoGenProvider(&fakeVideoProvider{name: "both", models: []map[string]any{{"id": "family-a", "modalities": []any{"text", "image"}}},
		caps: map[string]any{"modalities": []any{"text", "image"}, "aspect_ratios": []any{"16:9", "9:16"}, "resolutions": []any{"720p", "1080p"},
			"min_duration": 1, "max_duration": 15, "supports_audio": true, "supports_negative_prompt": true, "max_reference_images": 0}}, "")
	cfgHome(t, config.Tree{"video_gen": map[string]any{"provider": "both", "model": "family-a"}})
	props := schemaProps(buildDynamicVideoSchema(nil))
	if _, ok := props["image_url"]; !ok {
		t.Fatal(props)
	}
	d := props["duration"].(map[string]any)
	if !pyrepr.Equal(d["minimum"], 1) || !pyrepr.Equal(d["maximum"], 15) {
		t.Fatal(d)
	}
}

func TestDynamicSchemaBuilderI2vOnlyModelDoesNotClaimTextToVideo(t *testing.T) {
	resetVideoRegistry(t)
	RegisterVideoGenProvider(&fakeVideoProvider{name: "dual-i2v",
		models: []map[string]any{{"id": "gemini-like", "modalities": []any{"image"}, "min_duration": 3, "max_duration": 10}},
		caps:   map[string]any{"modalities": []any{"text", "image"}, "min_duration": 1, "max_duration": 30}}, "")
	cfgHome(t, config.Tree{"video_gen": map[string]any{"provider": "dual-i2v", "model": "gemini-like"}})
	schema := buildDynamicVideoSchema(nil)
	desc := schema["description"].(string)
	if !strings.Contains(desc, "image-to-video only") || strings.Contains(desc, "supports both text-to-video") {
		t.Fatal(desc)
	}
	props := schemaProps(schema)
	d := props["duration"].(map[string]any)
	if !pyrepr.Equal(d["minimum"], 3) || !pyrepr.Equal(d["maximum"], 10) {
		t.Fatal(d)
	}
	if _, ok := props["image_url"]; !ok {
		t.Fatal(props)
	}
}

// Surface matrix fixture recording outbound FAL and xAI calls with the bundled providers registered
type videoMatrix struct {
	home string
	mu   sync.Mutex
	fal  []fakeFalCall
	xai  []orCall
}

// Recording xAI transport whose finished job carries a stored public file
func (m *videoMatrix) RoundTrip(r *http.Request) (*http.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	reply := func(body string) *http.Response {
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(body)), Request: r}
	}
	if r.Method == http.MethodPost {
		raw, _ := io.ReadAll(r.Body)
		parsed, _ := pyjson.Loads(string(raw))
		js, _ := pyjson.Plain(parsed).(map[string]any)
		m.xai = append(m.xai, orCall{url: r.URL.String(), json: js})
		return reply(`{"request_id": "req-1"}`), nil
	}
	payload := m.xai[len(m.xai)-1].json
	so, _ := payload["storage_options"].(map[string]any)
	filename, _ := so["filename"].(string)
	if filename == "" {
		filename = "out.mp4"
	}
	model, _ := payload["model"].(string)
	if model == "" {
		model = "grok-imagine-video"
	}
	body, _ := pyjson.Dumps(map[string]any{"status": "done", "model": model, "video": map[string]any{"url": "https://xai-cdn/out.mp4", "duration": 8,
		"file_output": map[string]any{"file_id": "file-123", "filename": filename, "public_url": "https://xai-files.example/out.mp4",
			"public_url_expires_at": 1234567890}}})
	return reply(body), nil
}

func newVideoMatrix(t *testing.T) *videoMatrix {
	t.Helper()
	resetVideoRegistry(t)
	m := &videoMatrix{home: t.TempDir()}
	t.Setenv("ICEBATH_HOME", m.home)
	t.Setenv("FAL_KEY", "test-key")
	t.Setenv("XAI_API_KEY", "test-key")
	t.Setenv("FAL_VIDEO_MODEL", "")
	mod := &falClientModule{Submit: func(_ context.Context, _ *tools.Env, endpoint string, arguments map[string]any, _ map[string]string) (*falRequestHandle, error) {
		m.mu.Lock()
		m.fal = append(m.fal, fakeFalCall{endpoint, arguments})
		m.mu.Unlock()
		return completedFalHandle(map[string]any{"video": map[string]any{"url": "https://fake-fal/" + strings.ReplaceAll(endpoint, "/", "_") + ".mp4"}}), nil
	}}
	videoFalClientMu.Lock()
	prevMod := videoFalClient
	videoFalClient = mod
	videoFalClientMu.Unlock()
	prevClient, prevSleep, prevDiscover := xaiVideoHTTPClient, xaiVideoSleep, ensurePluginsDiscoveredHook
	xaiVideoHTTPClient = func() *http.Client { return &http.Client{Transport: m} }
	xaiVideoSleep = func(context.Context, time.Duration) error { return nil }
	ensurePluginsDiscoveredHook = func(context.Context, bool) {}
	t.Cleanup(func() {
		xaiVideoHTTPClient, xaiVideoSleep, ensurePluginsDiscoveredHook = prevClient, prevSleep, prevDiscover
		videoFalClientMu.Lock()
		videoFalClient = prevMod
		videoFalClientMu.Unlock()
	})
	for _, p := range []VideoGenProvider{newFalVideoGenProvider(), newXAIVideoGenProvider(), newOpenRouterVideoGenProvider(), newDeepInfraVideoGenProvider()} {
		if err := RegisterVideoGenProvider(p, ""); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

// Writes the config then invokes the registered tool handler and parses its JSON
func (m *videoMatrix) invoke(t *testing.T, cfg config.Tree, args map[string]any, toolName string) map[string]any {
	t.Helper()
	if err := config.Save(m.home, cfg); err != nil {
		t.Fatal(err)
	}
	entry := tools.Default.Get(toolName)
	if entry == nil {
		t.Fatal(toolName, "not registered")
	}
	return parseToolResult(t, entry.Handler(context.Background(), &tools.Call{Name: toolName, Args: args, Env: &tools.Env{Home: m.home, Config: cfg}}))
}

// Payload keys naming an image URL
func imageURLKeys(payload map[string]any, except string) []string {
	var out []string
	for k := range payload {
		if strings.Contains(k, "image") && strings.Contains(k, "url") && k != except {
			out = append(out, k)
		}
	}
	return out
}

func TestFalTextOnlyRoutesToTextEndpoint(t *testing.T) {
	for _, fid := range falFamilyOrder {
		if falFamilies[fid].TextEndpoint == "" {
			continue
		}
		t.Run(fid, func(t *testing.T) {
			m := newVideoMatrix(t)
			res := m.invoke(t, config.Tree{"video_gen": map[string]any{"provider": "fal", "model": fid}}, map[string]any{"prompt": "a dog running"}, "video_generate")
			if res["success"] != true || res["modality"] != "text" || res["provider"] != "fal" {
				t.Fatalf("%s: %v", fid, res["error"])
			}
			if len(m.fal) != 1 || m.fal[0].endpoint != falFamilies[fid].TextEndpoint {
				t.Fatal(m.fal)
			}
			if keys := imageURLKeys(m.fal[0].arguments, ""); len(keys) > 0 {
				t.Fatalf("%s text-only leaked image keys: %v", fid, keys)
			}
		})
	}
}

func TestFalImageToVideoRoutesToImageEndpoint(t *testing.T) {
	for _, fid := range falFamilyOrder {
		meta := falFamilies[fid]
		if meta.ImageEndpoint == "" {
			continue
		}
		t.Run(fid, func(t *testing.T) {
			m := newVideoMatrix(t)
			res := m.invoke(t, config.Tree{"video_gen": map[string]any{"provider": "fal", "model": fid}},
				map[string]any{"prompt": "animate this", "image_url": "https://example.com/i.png"}, "video_generate")
			if res["success"] != true || res["modality"] != "image" {
				t.Fatalf("%s: %v", fid, res["error"])
			}
			if len(m.fal) != 1 || m.fal[0].endpoint != meta.ImageEndpoint {
				t.Fatal(m.fal)
			}
			key := meta.ImageParamKey
			if key == "" {
				key = "image_url"
			}
			if m.fal[0].arguments[key] != "https://example.com/i.png" {
				t.Fatal(m.fal[0].arguments)
			}
			if keys := imageURLKeys(m.fal[0].arguments, key); len(keys) > 0 {
				t.Fatalf("%s sent extra image keys: %v", fid, keys)
			}
		})
	}
}

func TestXAITextOnlyViaToolSurface(t *testing.T) {
	m := newVideoMatrix(t)
	res := m.invoke(t, config.Tree{"video_gen": map[string]any{"provider": "xai"}}, map[string]any{"prompt": "a dog running"}, "video_generate")
	if res["success"] != true || res["modality"] != "text" || res["provider"] != "xai" {
		t.Fatal(res)
	}
	if len(m.xai) != 1 || !strings.HasSuffix(m.xai[0].url, "/videos/generations") {
		t.Fatal(m.xai)
	}
	payload := m.xai[0].json
	if _, ok := payload["image"]; ok {
		t.Fatal(payload)
	}
	if _, ok := payload["reference_images"]; ok {
		t.Fatal(payload)
	}
	so := payload["storage_options"].(map[string]any)
	if so["public_url"] != true {
		t.Fatal(so)
	}
	if _, ok := so["expires_after"]; ok {
		t.Fatal(so)
	}
	if res["video"] != "https://xai-files.example/out.mp4" || res["public_url"] != "https://xai-files.example/out.mp4" || res["temporary_url"] != "https://xai-cdn/out.mp4" {
		t.Fatal(res)
	}
}

func TestModelIsNeverAnAgentChoice(t *testing.T) {
	m := newVideoMatrix(t)
	res := m.invoke(t, config.Tree{"video_gen": map[string]any{"provider": "fal", "model": "pixverse-v6"}}, map[string]any{"prompt": "a dog", "model": "veo3.1"}, "video_generate")
	if res["success"] != true || res["model"] != "pixverse-v6" || m.fal[0].endpoint != "fal-ai/pixverse/v6/text-to-video" {
		t.Fatal(res, m.fal)
	}
	for _, schema := range []map[string]any{buildDynamicVideoSchema(&tools.Env{Home: m.home}), xaiVideoEditSchema(), xaiVideoExtendSchema()} {
		if _, ok := schemaProps(schema)["model"]; ok {
			t.Fatal(schema)
		}
	}
}
