package media

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const codexOfficialBase = "https://chatgpt.com/backend-api/codex"

var codexTestPNG, _ = hex.DecodeString("89504e470d0a1a0a0000000d49484452000000010000000108060000001f15c4" +
	"890000000d49444154789c6300010000000500010d0a2db40000000049454e44ae426082")

var codexTestB64 = base64.StdEncoding.EncodeToString(codexTestPNG)

// Recorded Codex images request with its body bytes
type codexSeenRequest struct {
	*http.Request
	Body []byte
}

// Fake Codex backend behind the images HTTP client seam, answering with respond when set
type codexFakeBackend struct {
	mu       sync.Mutex
	requests []codexSeenRequest
	respond  func(*http.Request) (*http.Response, error)
}

func codexJSONResponse(req *http.Request, status int, body string, h http.Header) *http.Response {
	if h == nil {
		h = http.Header{}
	}
	h.Set("Content-Type", "application/json")
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: req}
}

func newCodexFakeBackend(t *testing.T) *codexFakeBackend {
	t.Helper()
	b := &codexFakeBackend{}
	prev := codexImagesHTTPClient
	codexImagesHTTPClient = func() *http.Client {
		return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			var body []byte
			if req.Body != nil {
				body, _ = io.ReadAll(req.Body)
			}
			b.mu.Lock()
			b.requests = append(b.requests, codexSeenRequest{req, body})
			respond := b.respond
			b.mu.Unlock()
			if respond != nil {
				return respond(req)
			}
			return codexJSONResponse(req, 200, `{"created": 1, "data": [{"b64_json": "`+codexTestB64+`", "generation_id": "gen_1"}], `+
				`"background": "opaque", "output_format": "png", "quality": "low", "size": "1254x1254"}`,
				http.Header{"X-Codex-Imagegen-Request-Id": {"req_abc"}}), nil
		})}
	}
	t.Cleanup(func() { codexImagesHTTPClient = prev })
	return b
}

func (b *codexFakeBackend) seen() []codexSeenRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]codexSeenRequest(nil), b.requests...)
}

// Temp profile home with no Codex override, OpenAI key or image model in the environment
func codexHomeEnv(t *testing.T) *tools.Env {
	t.Helper()
	env := openAIHomeEnv(t, nil)
	for _, k := range []string{"ICEBATH_CODEX_BASE_URL", "ICEBATH_ALLOW_PRIVATE_URLS"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	return env
}

// auth.json with the Codex OAuth singleton token and no credential pool
func writeCodexSingleton(t *testing.T, h, token string) {
	t.Helper()
	data := `{"version": 1, "active_provider": "openai-codex", "providers": {"openai-codex": {"tokens": {"access_token": "` + token +
		`", "refresh_token": "rt"}, "last_refresh": "2026-09-24T00:00:00Z", "auth_mode": "chatgpt"}}}`
	if err := os.WriteFile(filepath.Join(h, "auth.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// auth.json holding only a manual api key pool row for openai-codex
func writeCodexPool(t *testing.T, h, key, rowBase string) {
	t.Helper()
	data := `{"version": 1, "providers": {}, "credential_pool": {"openai-codex": [{"id": "gw", "label": "gateway", "auth_type": "api_key", ` +
		`"priority": 0, "source": "manual", "access_token": "` + key + `", "base_url": "` + rowBase + `"}]}}`
	if err := os.WriteFile(filepath.Join(h, "auth.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func codexTestJWT(account string) string {
	enc := base64.RawURLEncoding.EncodeToString
	claims, _ := json.Marshal(map[string]any{"sub": account, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": account}})
	return enc([]byte(`{"alg": "RS256"}`)) + "." + enc(claims) + ".sig"
}

func TestOpenAICodexSetupSchemaHasNoRequiredEnvVars(t *testing.T) {
	codexHomeEnv(t)
	schema := newOpenAICodexImageGenProvider().GetSetupSchema()
	hint, _ := schema["post_setup_hint"].(string)
	if envVars, _ := schema["env_vars"].([]any); envVars == nil || len(envVars) != 0 || schema["post_setup"] != "openai_codex" ||
		!strings.Contains(hint, "icebath auth add openai-codex") || strings.Contains(hint, "icebath auth codex`") {
		t.Fatal(schema)
	}
}

func TestOpenAICodexAvailabilityFollowsCodexTokenOnly(t *testing.T) {
	env := codexHomeEnv(t)
	p := newOpenAICodexImageGenProvider()
	if p.IsAvailable() {
		t.Fatal("available without a Codex token")
	}
	t.Setenv("OPENAI_API_KEY", "sk-test")
	if p.IsAvailable() {
		t.Fatal("OPENAI_API_KEY alone made the Codex backend available")
	}
	writeCodexSingleton(t, env.Home, "tok")
	if !p.IsAvailable() {
		t.Fatal("unavailable with a Codex token")
	}
	prev := codexImagesHTTPClient
	codexImagesHTTPClient = nil
	defer func() { codexImagesHTTPClient = prev }()
	if p.IsAvailable() {
		t.Fatal("available without an HTTP client")
	}
	if res, _ := p.Generate(context.Background(), env, ImageGenRequest{Prompt: "a cat"}); res["error_type"] != "missing_dependency" {
		t.Fatal(res)
	}
}

func TestOpenAICodexCustomRouteKeepsImageRequestOnConfiguredHost(t *testing.T) {
	for _, edit := range []bool{false, true} {
		env := codexHomeEnv(t)
		writeCodexSingleton(t, env.Home, "codex-token")
		b := newCodexFakeBackend(t)
		t.Setenv("ICEBATH_CODEX_BASE_URL", "https://images.example.test/codex/")
		req := ImageGenRequest{Prompt: "a cat"}
		path := "images/generations"
		if edit {
			req.ImageURL, path = "data:image/png;base64,"+codexTestB64, "images/edits"
		}
		if res, _ := newOpenAICodexImageGenProvider().Generate(context.Background(), env, req); res["success"] != true {
			t.Fatal(res)
		}
		seen := b.seen()
		if len(seen) != 1 || seen[0].URL.String() != "https://images.example.test/codex/"+path ||
			seen[0].Header.Get("Authorization") != "Bearer codex-token" || seen[0].Header.Get("originator") != "codex_cli_rs" {
			t.Fatal(edit, seen)
		}
	}
}

func TestOpenAICodexBlankOverridePreservesOfficialRoute(t *testing.T) {
	for _, override := range []string{"", "   "} {
		env := codexHomeEnv(t)
		writeCodexSingleton(t, env.Home, "codex-token")
		b := newCodexFakeBackend(t)
		t.Setenv("ICEBATH_CODEX_BASE_URL", override)
		if res, _ := newOpenAICodexImageGenProvider().Generate(context.Background(), env, ImageGenRequest{Prompt: "a cat"}); res["success"] != true {
			t.Fatal(res)
		}
		seen := b.seen()
		if len(seen) != 1 || seen[0].URL.String() != codexOfficialBase+"/images/generations" || seen[0].Header.Get("originator") != "icebath-agent" {
			t.Fatalf("%q %v", override, seen)
		}
	}
}

func TestOpenAICodexProfileScopeRoutesEachRequestWithoutBorrowingProcessOverride(t *testing.T) {
	env := codexHomeEnv(t)
	writeCodexSingleton(t, env.Home, "codex-token")
	b := newCodexFakeBackend(t)
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	t.Setenv("ICEBATH_CODEX_BASE_URL", "https://other-profile.example.test/codex")
	for _, c := range []struct {
		scope map[string]string
		want  string
	}{
		{map[string]string{"ICEBATH_CODEX_BASE_URL": "https://profile.example.test/images-api/"}, "https://profile.example.test/images-api/images/generations"},
		{map[string]string{}, codexOfficialBase + "/images/generations"},
	} {
		ctx := provider.WithSecretScope(context.Background(), c.scope, "")
		if res, _ := newOpenAICodexImageGenProvider().Generate(ctx, env, ImageGenRequest{Prompt: "a cat"}); res["success"] != true {
			t.Fatal(res)
		}
		if seen := b.seen(); seen[len(seen)-1].URL.String() != c.want {
			t.Fatal(seen[len(seen)-1].URL, c.want)
		}
	}
}

func TestOpenAICodexCustomRouteFailureNeverRetriesOnOfficialHost(t *testing.T) {
	for _, failure := range []string{"http", "empty", "exception"} {
		env := codexHomeEnv(t)
		writeCodexSingleton(t, env.Home, "codex-token")
		b := newCodexFakeBackend(t)
		t.Setenv("ICEBATH_CODEX_BASE_URL", "https://images.example.test/codex")
		b.respond = func(req *http.Request) (*http.Response, error) {
			switch failure {
			case "exception":
				return nil, errors.New("fixture connection failed")
			case "http":
				return codexJSONResponse(req, 403, `{"error": {"message": "denied"}}`, nil), nil
			}
			return codexJSONResponse(req, 200, `{"data": []}`, nil), nil
		}
		if res, _ := newOpenAICodexImageGenProvider().Generate(context.Background(), env, ImageGenRequest{Prompt: "a cat"}); res["success"] != false {
			t.Fatal(failure, res)
		}
		if seen := b.seen(); len(seen) != 1 || seen[0].URL.Hostname() != "images.example.test" {
			t.Fatal(failure, seen)
		}
	}
}

func TestOpenAICodexReturnsAuthErrorWithoutCodexToken(t *testing.T) {
	env := codexHomeEnv(t)
	res, _ := newOpenAICodexImageGenProvider().Generate(context.Background(), env, ImageGenRequest{Prompt: "a cat"})
	if res["success"] != false || res["error_type"] != "auth_required" {
		t.Fatal(res)
	}
	if res, _ := newOpenAICodexImageGenProvider().Generate(context.Background(), env, ImageGenRequest{Prompt: "  "}); res["error_type"] != "invalid_argument" {
		t.Fatal(res)
	}
}

func TestOpenAICodexTextToImagePostsGenerationsWithNoHostModel(t *testing.T) {
	env := codexHomeEnv(t)
	writeCodexSingleton(t, env.Home, "codex-token")
	b := newCodexFakeBackend(t)
	res, _ := newOpenAICodexImageGenProvider().Generate(context.Background(), env, ImageGenRequest{Prompt: "a cat", AspectRatio: "portrait"})
	if res["success"] != true || res["model"] != "gpt-image-2-medium" || res["provider"] != "openai-codex" || res["quality"] != "medium" ||
		res["pixel_size"] != "1x1" || res["reported_quality"] != "low" || res["reported_size"] != "1254x1254" || res["imagegen_request_id"] != "req_abc" {
		t.Fatal(res)
	}
	saved, _ := res["image"].(string)
	if st, err := os.Stat(saved); err != nil || st.IsDir() || filepath.Dir(saved) != filepath.Join(env.Home, "cache", "generated", "images") ||
		!strings.HasPrefix(filepath.Base(saved), "openai_codex_") {
		t.Fatal(saved, err)
	}
	seen := b.seen()
	if len(seen) != 1 || !strings.HasSuffix(seen[0].URL.Path, "/backend-api/codex/images/generations") ||
		seen[0].Header.Get("Authorization") != "Bearer codex-token" || seen[0].Header.Get("x-codex-image-turn-id") == "" {
		t.Fatal(seen)
	}
	var body map[string]any
	if err := json.Unmarshal(seen[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"prompt": "a cat", "model": "gpt-image-2", "n": 1.0, "quality": "medium", "size": "1024x1536", "background": "opaque"}
	if !jsonEqual(body, want) {
		t.Fatal(body)
	}
	for _, k := range []string{"tools", "input", "instructions"} {
		if _, ok := body[k]; ok {
			t.Fatal(k)
		}
	}
}

func TestOpenAICodexCustomBaseReceivesTheImageRequest(t *testing.T) {
	env := codexHomeEnv(t)
	writeCodexSingleton(t, env.Home, "codex-token")
	b := newCodexFakeBackend(t)
	t.Setenv("ICEBATH_CODEX_BASE_URL", "https://codex-gw.example/backend-api/codex")
	if res, _ := newOpenAICodexImageGenProvider().Generate(context.Background(), env, ImageGenRequest{Prompt: "a cat"}); res["success"] != true {
		t.Fatal(res)
	}
	if seen := b.seen(); len(seen) != 1 || seen[0].URL.Hostname() != "codex-gw.example" || !strings.HasSuffix(seen[0].URL.Path, "/backend-api/codex/images/generations") {
		t.Fatal(seen)
	}
}

func TestOpenAICodexSourceImagesPostEditsWithInlineDataURLs(t *testing.T) {
	env := codexHomeEnv(t)
	writeCodexSingleton(t, env.Home, "codex-token")
	b := newCodexFakeBackend(t)
	local := filepath.Join(t.TempDir(), "ref.png")
	os.WriteFile(local, codexTestPNG, 0o644)
	dataURL := "data:image/png;base64," + codexTestB64
	res, _ := newOpenAICodexImageGenProvider().Generate(context.Background(), env, ImageGenRequest{Prompt: "edit these", ImageURL: local, ReferenceImageURLs: []string{dataURL}})
	if res["success"] != true || res["modality"] != "image" || res["input_image_count"] != 2 {
		t.Fatal(res)
	}
	seen := b.seen()
	var body struct{ Images []map[string]string }
	json.Unmarshal(seen[0].Body, &body)
	if !strings.HasSuffix(seen[0].URL.Path, "/backend-api/codex/images/edits") || len(body.Images) != 2 ||
		body.Images[0]["image_url"] != dataURL || body.Images[1]["image_url"] != dataURL {
		t.Fatal(seen[0].URL, body)
	}
}

func TestOpenAICodexRemoteSourceURLIsFetchedAndInlined(t *testing.T) {
	env := codexHomeEnv(t)
	writeCodexSingleton(t, env.Home, "codex-token")
	b := newCodexFakeBackend(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(codexTestPNG) }))
	defer srv.Close()
	t.Setenv("ICEBATH_ALLOW_PRIVATE_URLS", "1")
	res, _ := newOpenAICodexImageGenProvider().Generate(context.Background(), env, ImageGenRequest{Prompt: "edit", ImageURL: srv.URL + "/ref.png"})
	if res["success"] != true {
		t.Fatal(res)
	}
	var body struct{ Images []map[string]string }
	json.Unmarshal(b.seen()[0].Body, &body)
	if len(body.Images) != 1 || body.Images[0]["image_url"] != "data:image/png;base64,"+codexTestB64 {
		t.Fatal(body)
	}
}

func TestOpenAICodexRejectsNonImageLocalSource(t *testing.T) {
	env := codexHomeEnv(t)
	writeCodexSingleton(t, env.Home, "codex-token")
	b := newCodexFakeBackend(t)
	textPath := filepath.Join(t.TempDir(), "not-image.txt")
	os.WriteFile(textPath, []byte("hello"), 0o644)
	res, _ := newOpenAICodexImageGenProvider().Generate(context.Background(), env, ImageGenRequest{Prompt: "edit this", ImageURL: textPath})
	errText, _ := res["error"].(string)
	if res["success"] != false || res["error_type"] != "invalid_image_input" || !strings.Contains(errText, "not a supported image") || len(b.seen()) != 0 {
		t.Fatal(res)
	}
}

func TestOpenAICodexHTTPErrorMessageSurfacesVerbatimAndBounded(t *testing.T) {
	env := codexHomeEnv(t)
	writeCodexSingleton(t, env.Home, "codex-token")
	b := newCodexFakeBackend(t)
	raw, _ := json.Marshal(map[string]any{"metadata": strings.Repeat("x", 600),
		"error": map[string]any{"message": "Missing required parameter: 'prompt'.", "type": "invalid_request_error"}})
	b.respond = func(req *http.Request) (*http.Response, error) {
		return codexJSONResponse(req, 400, string(raw), nil), nil
	}
	res, _ := newOpenAICodexImageGenProvider().Generate(context.Background(), env, ImageGenRequest{Prompt: "a cat"})
	errText, _ := res["error"].(string)
	if res["success"] != false || res["error_type"] != "api_error" || !strings.Contains(errText, "HTTP 400") ||
		!strings.Contains(errText, "Missing required parameter: 'prompt'.") || len(errText) >= len(raw) {
		t.Fatal(res)
	}
	if got := summarizeCodexErrorBody(strings.Repeat("y", 900)); got != strings.Repeat("y", 500) {
		t.Fatal(len(got))
	}
}

func TestOpenAICodexMissingImageDataIsEmptyResponse(t *testing.T) {
	env := codexHomeEnv(t)
	writeCodexSingleton(t, env.Home, "codex-token")
	b := newCodexFakeBackend(t)
	b.respond = func(req *http.Request) (*http.Response, error) {
		return codexJSONResponse(req, 200, `{"created": 1, "data": []}`, nil), nil
	}
	if res, _ := newOpenAICodexImageGenProvider().Generate(context.Background(), env, ImageGenRequest{Prompt: "a cat"}); res["success"] != false || res["error_type"] != "empty_response" {
		t.Fatal(res)
	}
}

func TestOpenAICodexRegisterWiresProviderIntoRegistry(t *testing.T) {
	resetImageRegistry(t)
	env := imageEnv(t, config.Tree{})
	t.Setenv("ICEBATH_HOME", env.Home)
	plugins.InstallServices(&plugins.Services{ProviderRegistries: map[string]plugins.ScopedRegistry{"image_gen_provider": ImageGenScopedRegistry()}})
	t.Cleanup(func() { plugins.InstallServices(nil); plugins.ResetPluginManagersForTests() })
	plugins.ResetPluginManagersForTests()
	ctx := envContext(context.Background(), env)
	plugins.GetPluginManager(ctx).DiscoverAndLoad(ctx, true)
	if p, ok := GetImageGenProvider(ctx, "openai-codex", "").(*openAICodexImageGenProvider); !ok || p.Name() != "openai-codex" {
		t.Fatalf("registered %T", GetImageGenProvider(ctx, "openai-codex", ""))
	}
}

func TestOpenAICodexRemoteImageRefusesInternalTargetsBeforeConnect(t *testing.T) {
	var mu sync.Mutex
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.URL.Path)
		mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/to-metadata") {
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
			return
		}
		w.Write(codexTestPNG)
	}))
	defer srv.Close()
	env := codexHomeEnv(t)
	if out, err := codexRemoteImageToDataURL(context.Background(), env, srv.URL+"/img.png"); err == nil || out != "" || len(hits) != 0 {
		t.Fatal("loopback fetched", err, hits)
	}
	t.Setenv("ICEBATH_ALLOW_PRIVATE_URLS", "1")
	if out, err := codexRemoteImageToDataURL(context.Background(), env, srv.URL+"/to-metadata/img.png"); err == nil || out != "" {
		t.Fatal("redirect to metadata followed", err)
	}
	if len(hits) != 1 || hits[0] != "/to-metadata/img.png" {
		t.Fatal(hits)
	}
}

func TestOpenAICodexImageRequestGoesToThePoolEntryGateway(t *testing.T) {
	const gw = "https://codex-gw.example/backend-api/codex"
	for _, key := range []string{"dummy-gateway-pool-key", codexTestJWT("acct")} {
		env := codexHomeEnv(t)
		setImageConfig(t, env, config.Tree{"model": map[string]any{"provider": "openai-codex", "default": "gpt-5.5", "base_url": gw}})
		writeCodexPool(t, env.Home, key, codexOfficialBase)
		b := newCodexFakeBackend(t)
		b.respond = func(req *http.Request) (*http.Response, error) {
			return codexJSONResponse(req, 200, `{"data": [{"b64_json": "aGk="}]}`, nil), nil
		}
		res, _ := newOpenAICodexImageGenProvider().Generate(context.Background(), env, ImageGenRequest{Prompt: "a cat"})
		if res["success"] != true {
			t.Fatal(res)
		}
		seen := b.seen()
		for _, s := range seen {
			if s.Header.Get("Authorization") != "" && s.URL.Hostname() != "codex-gw.example" {
				t.Fatal("authorized traffic left the gateway", s.URL)
			}
		}
		if seen[0].Header.Get("Authorization") != "Bearer "+key {
			t.Fatal(seen[0].Header)
		}
	}
}
