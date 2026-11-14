package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Nous account double whose managed tool entitlement the test sets and whose probes it counts
type fakeNousAccount struct {
	enabled bool
	mu      sync.Mutex
	calls   int
}

func (a *fakeNousAccount) ManagedToolsEnabled(*tools.Env, bool) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	return a.enabled
}
func (a *fakeNousAccount) UnavailableMessage(*tools.Env, string, bool) string { return "" }
func (a *fakeNousAccount) PeekAccessToken(*tools.Env) string                  { return "" }
func (a *fakeNousAccount) ReadAccessToken(*tools.Env) string                  { return "" }
func (a *fakeNousAccount) SourceSuppressed(*tools.Env, string, string) bool   { return false }
func (a *fakeNousAccount) PoolAPIKey(*tools.Env, string) string               { return "" }
func (a *fakeNousAccount) IdentityState(*tools.Env) (bool, bool)              { return a.enabled, false }
func (a *fakeNousAccount) GuestAllowed(*tools.Env) bool                       { return false }
func (a *fakeNousAccount) LastMintFailure(*tools.Env) (tools.MintFailure, bool) {
	return tools.MintFailure{}, false
}
func (a *fakeNousAccount) AnonFailureCopy(*tools.Env, string, float64) string { return "" }
func (a *fakeNousAccount) EnsurePortalIdentity(*tools.Env) error              { return nil }
func (a *fakeNousAccount) ManagedToolsRolledOut(*tools.Env) bool              { return a.enabled }

func useNousAccount(t *testing.T, enabled bool) *fakeNousAccount {
	t.Helper()
	a := &fakeNousAccount{enabled: enabled}
	prev := tools.NousAccount
	tools.NousAccount = a
	t.Cleanup(func() { tools.NousAccount = prev })
	return a
}

// Captured endpoint and arguments of the patched FAL submit, answering one generated image
func captureFalSubmit(t *testing.T) *map[string]any {
	t.Helper()
	capture := map[string]any{}
	prev := submitFalRequestHook
	submitFalRequestHook = func(_ context.Context, _ *tools.Env, model string, arguments map[string]any) (*falRequestHandle, error) {
		capture["endpoint"] = model
		capture["arguments"] = arguments
		return completedFalHandle(map[string]any{"images": []any{map[string]any{"url": "https://out/img.png", "width": 1, "height": 1}}}), nil
	}
	t.Cleanup(func() { submitFalRequestHook = prev })
	return &capture
}

func pngBytes(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func solidRGBA(w, h int, c color.Color) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, c)
		}
	}
	return img
}

func TestFalEditPayloadIncludesImageURLs(t *testing.T) {
	p, err := buildFalEditPayload("fal-ai/nano-banana-pro", "make it night", []string{"https://x/y.png"}, "landscape", nil, nil)
	if err != nil || p["prompt"] != "make it night" || p["aspect_ratio"] != "16:9" {
		t.Fatalf("%v %v", p, err)
	}
	if urls, _ := p["image_urls"].([]any); len(urls) != 1 || urls[0] != "https://x/y.png" {
		t.Fatalf("image_urls %v", p["image_urls"])
	}
}

func TestFalEditPayloadSingularImageParamKlingImageV3(t *testing.T) {
	p, err := buildFalEditPayload("fal-ai/kling-image/v3/text-to-image", "make it winter", []string{"https://x/a.png", "https://x/b.png"}, "landscape", nil, nil)
	if err != nil || p["prompt"] != "make it winter" || p["image_url"] != "https://x/a.png" {
		t.Fatalf("%v %v", p, err)
	}
	if _, ok := p["image_urls"]; ok {
		t.Fatal("image_urls leaked")
	}
	if p["aspect_ratio"] != "16:9" || p["resolution"] != "2K" {
		t.Fatalf("payload %v", p)
	}
}

func TestFalMandatoryKeysSurviveWhitelist(t *testing.T) {
	sizes := map[string]any{"square": "1024x1024", "landscape": "1536x1024", "portrait": "1024x1536"}
	falModels["test/edit-model"] = map[string]any{"size_style": "image_size_preset", "sizes": sizes, "edit_supports": falKeySet([]string{"seed"})}
	falModels["test/text-model"] = map[string]any{"size_style": "image_size_preset", "sizes": sizes, "supports": falKeySet([]string{"seed"})}
	t.Cleanup(func() { delete(falModels, "test/edit-model"); delete(falModels, "test/text-model") })
	p, err := buildFalEditPayload("test/edit-model", "make it blue", []string{"https://x/y.png"}, "square", nil, nil)
	if err != nil || p["prompt"] != "make it blue" {
		t.Fatalf("%v %v", p, err)
	}
	if urls, _ := p["image_urls"].([]any); len(urls) != 1 || urls[0] != "https://x/y.png" {
		t.Fatalf("image_urls %v", p["image_urls"])
	}
	p, err = buildFalPayload("test/text-model", "a cat", "square", nil, nil)
	if err != nil || p["prompt"] != "a cat" {
		t.Fatalf("%v %v", p, err)
	}
}

func TestFalRoutingTextToImageUsesBaseEndpoint(t *testing.T) {
	t.Setenv("FAL_KEY", "test-key")
	env := imageEnv(t, config.Tree{"image_gen": map[string]any{"model": "fal-ai/nano-banana-pro"}})
	capture := captureFalSubmit(t)
	off := false
	out := decodeResult(t, imageGenerateTool(context.Background(), env, imageToolArgs{Prompt: "a cat", AspectRatio: "square", Upscale: off}))
	if out["success"] != true || out["modality"] != "text" || (*capture)["endpoint"] != "fal-ai/nano-banana-pro" {
		t.Fatalf("%v %v", out, *capture)
	}
	if _, ok := (*capture)["arguments"].(map[string]any)["image_urls"]; ok {
		t.Fatal("text request carried image_urls")
	}
}

func TestFalRoutingEditSkipsUpscaler(t *testing.T) {
	t.Setenv("FAL_KEY", "test-key")
	env := imageEnv(t, config.Tree{"image_gen": map[string]any{"model": "fal-ai/flux-2-pro"}})
	capture := captureFalSubmit(t)
	hit := false
	prev := upscaleImageHook
	upscaleImageHook = func(context.Context, *tools.Env, string, string) (map[string]any, error) { hit = true; return nil, nil }
	t.Cleanup(func() { upscaleImageHook = prev })
	out := decodeResult(t, imageGenerateTool(context.Background(), env, imageToolArgs{Prompt: "tweak", AspectRatio: "landscape", ImageURL: "https://in/src.png"}))
	if out["success"] != true || out["modality"] != "image" || (*capture)["endpoint"] != falModels["fal-ai/flux-2-pro"]["edit_endpoint"] || hit {
		t.Fatalf("%v %v upscaled=%v", out, *capture, hit)
	}
}

func TestFalRoutingLocalPathsReachFalAsDataURLs(t *testing.T) {
	t.Setenv("TERMINAL_ENV", "local")
	t.Setenv("FAL_KEY", "test-key")
	env := imageEnv(t, config.Tree{"image_gen": map[string]any{"model": "fal-ai/flux-2-pro"}})
	capture := captureFalSubmit(t)
	src := filepath.Join(t.TempDir(), "src.png")
	raw := pngBytes(t, solidRGBA(8, 8, color.NRGBA{255, 0, 0, 255}))
	if err := os.WriteFile(src, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	out := decodeResult(t, imageGenerateTool(context.Background(), env, imageToolArgs{Prompt: "tweak", AspectRatio: "landscape", ImageURL: src}))
	if out["success"] != true {
		t.Fatalf("%v", out)
	}
	u := (*capture)["arguments"].(map[string]any)["image_urls"].([]any)[0].(string)
	if !strings.HasPrefix(u, "data:image/png;base64,") {
		t.Fatalf("url %.60s", u)
	}
	if got, _ := base64.StdEncoding.DecodeString(strings.SplitN(u, ",", 2)[1]); !bytes.Equal(got, raw) {
		t.Fatal("inlined bytes differ from the host file")
	}
	clear(*capture)
	if err := os.WriteFile(filepath.Join(env.Home, ".env"), []byte("FAL_KEY=HOST-PRIVATE-KEY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := imageGenerateTool(context.Background(), env, imageToolArgs{Prompt: "tweak", AspectRatio: "landscape", ImageURL: filepath.Join(env.Home, ".env")})
	if decodeResult(t, res)["success"] != false || len(*capture) != 0 || strings.Contains(res, "HOST-PRIVATE-KEY") {
		t.Fatalf("secret file not refused %s %v", res, *capture)
	}
}

func TestFalOversizedSourcesShrinkUnderGatewayBudget(t *testing.T) {
	t.Setenv("TERMINAL_ENV", "local")
	env := imageEnv(t, config.Tree{})
	img := image.NewNRGBA(image.Rect(0, 0, 1600, 1600))
	r := rand.New(rand.NewPCG(1, 2))
	for i := range img.Pix {
		img.Pix[i] = byte(r.UintN(256))
	}
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 255
	}
	big := filepath.Join(t.TempDir(), "big.png")
	if err := os.WriteFile(big, pngBytes(t, img), 0o644); err != nil {
		t.Fatal(err)
	}
	urls, err := inlineFalSources(context.Background(), env, []string{big, "https://x/ref.png"})
	if err != nil || urls[1] != "https://x/ref.png" {
		t.Fatalf("%v %v", urls, err)
	}
	if !strings.HasPrefix(urls[0], "data:image/jpeg;base64,") || len(urls[0]) > falInlineBudget {
		t.Fatalf("inline source %.40s len %d", urls[0], len(urls[0]))
	}
}

// Provider whose capabilities advertise editing and which records what it received
func editCapableProvider() *fakeImageProvider {
	p := &fakeImageProvider{name: "editcap", caps: map[string]any{"modalities": []any{"text", "image"}, "max_reference_images": 4}}
	p.generate = func(req ImageGenRequest) (map[string]any, error) {
		modality := "text"
		if req.ImageURL != "" {
			modality = "image"
		}
		return map[string]any{"success": true, "image": "/tmp/out.png", "model": "editcap-1", "prompt": req.Prompt,
			"aspect_ratio": req.AspectRatio, "modality": modality, "provider": "editcap"}, nil
	}
	return p
}

func TestPluginDispatchForwardsImageURL(t *testing.T) {
	p := editCapableProvider()
	usePluginProviders(t, p)
	env := imageEnv(t, config.Tree{"image_gen": map[string]any{"provider": "editcap"}})
	raw, _ := dispatchToPluginProvider(context.Background(), env, "make night", "square",
		imageSources{ImageURL: "https://in/src.png", ReferenceImageURLs: []any{"https://in/ref.png"}}, nil)
	out := decodeResult(t, raw)
	if out["success"] != true || out["modality"] != "image" || len(p.calls) != 1 {
		t.Fatalf("%v", out)
	}
	if p.calls[0].ImageURL != "https://in/src.png" || !slices.Equal(p.calls[0].ReferenceImageURLs, []string{"https://in/ref.png"}) {
		t.Fatalf("received %+v", p.calls[0])
	}
}

func TestPluginDispatchLegacyProviderEditRequestSurfacesClearError(t *testing.T) {
	p := &fakeImageProvider{name: "legacy"}
	p.generate = func(req ImageGenRequest) (map[string]any, error) {
		if req.ImageURL != "" {
			return nil, &plugins.PluginHostError{TypeName: "TypeError", RemoteMessage: "generate() got an unexpected keyword argument 'image_url'"}
		}
		return map[string]any{"success": true, "image": "/tmp/legacy.png", "provider": "legacy"}, nil
	}
	usePluginProviders(t, p)
	env := imageEnv(t, config.Tree{"image_gen": map[string]any{"provider": "legacy"}})
	raw, _ := dispatchToPluginProvider(context.Background(), env, "edit it", "square", imageSources{ImageURL: "https://in/src.png"}, nil)
	if out := decodeResult(t, raw); out["success"] != false || out["error_type"] != "modality_unsupported" {
		t.Fatalf("%v", out)
	}
}

func TestImageGenerateDynamicSchemaWiredIntoRegistry(t *testing.T) {
	def := tools.Default.Get("image_generate")
	if def == nil || def.DynamicSchema == nil {
		t.Fatal("image_generate registered without dynamic schema overrides")
	}
	usePluginProviders(t)
	if _, ok := def.DynamicSchema(imageEnv(t, config.Tree{}))["description"]; !ok {
		t.Fatal("dynamic schema without description")
	}
}

func TestImageProviderDefaultCapabilitiesFailClosed(t *testing.T) {
	caps := defaultImageCapabilities()
	if m, _ := caps["modalities"].([]any); len(m) != 1 || m[0] != "text" {
		t.Fatalf("modalities %v", caps["modalities"])
	}
	if pyTruthy(caps["supports_upscale"]) {
		t.Fatal("default capabilities advertise upscale")
	}
}

// First catalog model without an edit endpoint and first edit model taking several references
func falSchemaModels(t *testing.T) (string, string) {
	t2i, multi := "", ""
	for _, id := range falModelOrder {
		meta := falModels[id]
		edit, _ := meta["edit_endpoint"].(string)
		if edit == "" && t2i == "" {
			t2i = id
		}
		if edit != "" && multi == "" && positiveInt(meta["max_reference_images"]) > 1 {
			multi = id
		}
	}
	if t2i == "" || multi == "" {
		t.Fatal("catalog lacks a text only or multi reference edit model")
	}
	return t2i, multi
}

func schemaProps(schema map[string]any) map[string]any {
	return schema["parameters"].(map[string]any)["properties"].(map[string]any)
}

func falSchemaFor(t *testing.T, model string) map[string]any {
	usePluginProviders(t)
	return buildDynamicImageSchema(imageEnv(t, config.Tree{"image_gen": map[string]any{"model": model}}))
}

func TestImageSchemaParamGatingFollowsFalCatalog(t *testing.T) {
	t2i, multi := falSchemaModels(t)
	s := falSchemaFor(t, t2i)
	props := schemaProps(s)
	if _, ok := props["image_url"]; ok {
		t.Fatal("text only model advertises image_url")
	}
	if _, ok := props["reference_image_urls"]; ok {
		t.Fatal("text only model advertises reference_image_urls")
	}
	if !strings.Contains(s["description"].(string), "cannot edit") {
		t.Fatalf("description %q", s["description"])
	}
	props = schemaProps(falSchemaFor(t, multi))
	refs, _ := props["reference_image_urls"].(map[string]any)
	if _, ok := props["image_url"]; !ok || refs == nil || positiveInt(refs["maxItems"]) != positiveInt(falModels[multi]["max_reference_images"]) {
		t.Fatalf("edit model props %v", props)
	}
	for _, m := range []string{t2i, multi} {
		props := schemaProps(falSchemaFor(t, m))
		if _, ok := props["upscale"]; !ok {
			t.Fatalf("%s hides upscale", m)
		}
		for name := range creativeControlParams() {
			if _, ok := props[name]; ok {
				t.Fatalf("%s advertises creative control %s", m, name)
			}
		}
	}
	fal := falSchemaFor(t, multi)
	fp := schemaProps(fal)
	if !jsonEqual(fp["image_url"], imageURLParam()) {
		t.Fatalf("fal image_url %v", fp["image_url"])
	}
	for _, k := range []string{"reference_image_urls", "prompt"} {
		if !strings.Contains(fp[k].(map[string]any)["description"].(string), "edit") {
			t.Fatalf("fal %s wording lacks edit", k)
		}
	}
}

func TestImageSchemaTextOnlyPluginProviderHidesEditAndUpscale(t *testing.T) {
	usePluginProviders(t, &fakeImageProvider{name: "codex", caps: map[string]any{"modalities": []any{"text"}, "max_reference_images": 0},
		models: []map[string]any{{"id": "img-1"}}})
	props := schemaProps(buildDynamicImageSchema(imageEnv(t, config.Tree{"image_gen": map[string]any{"provider": "codex"}})))
	var names []string
	for k := range props {
		names = append(names, k)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "aspect_ratio,prompt" {
		t.Fatalf("props %v", names)
	}
}

func TestImageStaticSchemaCarriesNoCapabilityArgs(t *testing.T) {
	var names []string
	for k := range schemaProps(imageGenerateSchema()) {
		names = append(names, k)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "aspect_ratio,prompt" {
		t.Fatalf("props %v", names)
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func TestImageFalStrictSelection(t *testing.T) {
	sel := func(provider string) *tools.Env {
		if provider == "" {
			return imageEnv(t, config.Tree{})
		}
		return imageEnv(t, config.Tree{"image_gen": map[string]any{"provider": provider}})
	}
	t.Setenv("FAL_QUEUE_GATEWAY_URL", "https://gateway.nousresearch.com")
	t.Setenv("TOOL_GATEWAY_USER_TOKEN", "managed-token")

	t.Setenv("FAL_KEY", "fal-key")
	useNousAccount(t, true)
	gw, err := resolveManagedFalGateway(sel("nous"))
	if err != nil || gw == nil || gw.Vendor != "fal-queue" || gw.GatewayOrigin != "https://gateway.nousresearch.com" || gw.NousUserToken != "managed-token" {
		t.Fatalf("nous selection %+v %v", gw, err)
	}

	useNousAccount(t, false)
	_, err = resolveManagedFalGateway(sel("nous"))
	var ve *imageValueError
	if !errors.As(err, &ve) || !strings.Contains(err.Error(), "image_gen is configured to use nous") || !strings.Contains(err.Error(), "icebath tools") {
		t.Fatalf("unentitled nous %v", err)
	}

	t.Setenv("FAL_KEY", "")
	acct := useNousAccount(t, true)
	_, err = resolveManagedFalGateway(sel("fal"))
	if !errors.As(err, &ve) || !strings.Contains(err.Error(), "FAL_KEY") || !strings.Contains(err.Error(), "image_gen is configured to use fal") ||
		!strings.Contains(err.Error(), "icebath tools") || acct.calls != 0 {
		t.Fatalf("fal without key %v managed probes %d", err, acct.calls)
	}
	if checkFalAPIKey(sel("fal")) {
		t.Fatal("broken fal selection reports available")
	}

	t.Setenv("FAL_KEY", "fal-key")
	acct = useNousAccount(t, true)
	if gw, err := resolveManagedFalGateway(sel("fal")); gw != nil || err != nil || acct.calls != 0 {
		t.Fatalf("fal with key %+v %v probes %d", gw, err, acct.calls)
	}
	if gw, err := resolveManagedFalGateway(sel("")); gw != nil || err != nil {
		t.Fatalf("autodetect with key %+v %v", gw, err)
	}
	t.Setenv("FAL_KEY", "")
	if gw, err := resolveManagedFalGateway(sel("")); err != nil || gw == nil || gw.NousUserToken != "managed-token" {
		t.Fatalf("autodetect without key %+v %v", gw, err)
	}
}

func TestManagedFalSubmitUsesGatewayOriginAndNousToken(t *testing.T) {
	var mu sync.Mutex
	var reqs []*http.Request
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs, bodies = append(reqs, r), append(bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"request_id": "req-123", "response_url": "http://127.0.0.1:3009/requests/req-123", ` +
			`"status_url": "http://127.0.0.1:3009/requests/req-123/status", "cancel_url": "http://127.0.0.1:3009/requests/req-123/cancel"}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("FAL_KEY", "")
	t.Setenv("FAL_QUEUE_GATEWAY_URL", srv.URL)
	t.Setenv("TOOL_GATEWAY_USER_TOKEN", "nous-token")
	useNousAccount(t, true)
	resetClient := func() {
		managedFalClientMu.Lock()
		managedFalClient, managedFalClientConfig = nil, [2]string{}
		managedFalClientMu.Unlock()
	}
	resetClient()
	t.Cleanup(resetClient)
	env := imageEnv(t, config.Tree{})
	h, err := submitFalRequest(context.Background(), env, "fal-ai/flux-2-pro", map[string]any{"prompt": "test prompt", "num_images": 1})
	if err != nil {
		t.Fatal(err)
	}
	first := managedFalClient
	if h.RequestID != "req-123" || h.ResponseURL != "http://127.0.0.1:3009/requests/req-123" ||
		h.StatusURL != "http://127.0.0.1:3009/requests/req-123/status" || h.CancelURL != "http://127.0.0.1:3009/requests/req-123/cancel" {
		t.Fatalf("handle %+v", h)
	}
	if len(reqs) != 1 || reqs[0].Method != http.MethodPost || reqs[0].URL.Path != "/fal-ai/flux-2-pro" {
		t.Fatalf("requests %d", len(reqs))
	}
	if !jsonEqual(decodeResult(t, bodies[0]), map[string]any{"prompt": "test prompt", "num_images": 1}) {
		t.Fatalf("body %s", bodies[0])
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(reqs[0].Header.Get("x-idempotency-key")) {
		t.Fatalf("idempotency key %q", reqs[0].Header.Get("x-idempotency-key"))
	}
	if reqs[0].Header.Get("Authorization") != "Key nous-token" {
		t.Fatalf("authorization %q", reqs[0].Header.Get("Authorization"))
	}
	if _, err := submitFalRequest(context.Background(), env, "fal-ai/flux-2-pro", map[string]any{"prompt": "again"}); err != nil || managedFalClient != first || first == nil {
		t.Fatalf("managed client rebuilt %v", err)
	}
}
