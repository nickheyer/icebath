package media

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

var openAITestPNG, _ = hex.DecodeString("89504e470d0a1a0a0000000d49484452000000010000000108060000001f15c4" +
	"890000000d49444154789c6300010000000500010d0a2db40000000049454e44ae426082")

type openAICapturedRequest struct {
	Path   string
	Header http.Header
	JSON   map[string]any
	Fields map[string][]string
	Files  map[string][]openAIFile
}

// Fake Images API recording every request and answering with the body the test sets
type fakeOpenAIImages struct {
	mu       sync.Mutex
	srv      *httptest.Server
	requests []openAICapturedRequest
	options  []openAIClientOptions
	reply    func(r *http.Request) (int, string)
}

func newFakeOpenAIImages(t *testing.T) *fakeOpenAIImages {
	f := &fakeOpenAIImages{}
	f.reply = func(*http.Request) (int, string) {
		return 200, `{"data": [{"b64_json": "` + base64.StdEncoding.EncodeToString(openAITestPNG) + `"}]}`
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := openAICapturedRequest{Path: r.URL.Path, Header: r.Header.Clone(), Fields: map[string][]string{}, Files: map[string][]openAIFile{}}
		mt, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mt == "multipart/form-data" {
			mr := multipart.NewReader(r.Body, params["boundary"])
			for {
				p, err := mr.NextPart()
				if err != nil {
					break
				}
				data, _ := io.ReadAll(p)
				if p.FileName() != "" {
					c.Files[p.FormName()] = append(c.Files[p.FormName()], openAIFile{Name: p.FileName(), Data: data})
				} else {
					c.Fields[p.FormName()] = append(c.Fields[p.FormName()], string(data))
				}
			}
		} else {
			data, _ := io.ReadAll(r.Body)
			parsed, _ := pyjson.Loads(string(data))
			c.JSON, _ = pyjson.Plain(parsed).(map[string]any)
		}
		f.mu.Lock()
		f.requests = append(f.requests, c)
		f.mu.Unlock()
		status, body := f.reply(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(f.srv.Close)
	prev := newOpenAIClientHook
	newOpenAIClientHook = func(env *tools.Env, o openAIClientOptions) (*openAIClient, error) {
		f.mu.Lock()
		f.options = append(f.options, o)
		f.mu.Unlock()
		c, err := newOpenAIClient(env, o)
		if c != nil && (o.BaseURL == nil || !strings.HasPrefix(*o.BaseURL, f.srv.URL)) {
			c.BaseURL = f.srv.URL + "/v1"
		}
		return c, err
	}
	t.Cleanup(func() { newOpenAIClientHook = prev })
	return f
}

func (f *fakeOpenAIImages) last(t *testing.T) openAICapturedRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("no request reached the fake Images API")
	}
	return f.requests[len(f.requests)-1]
}

// Process home with the config on disk so provider methods without a call env read it
func openAIHomeEnv(t *testing.T, cfg config.Tree) *tools.Env {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	for _, k := range []string{"OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_IMAGE_MODEL", "OPENAI_PROJECT_ID", "OPENAI_ORG_ID"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	env := &tools.Env{Home: h}
	if cfg == nil {
		cfg = config.Tree{}
	}
	setImageConfig(t, env, cfg)
	return env
}

func TestOpenAIPickerMatchesResolvableCatalog(t *testing.T) {
	openAIHomeEnv(t, nil)
	p := newOpenAIImageGenProvider()
	rows := p.ListModels()
	if len(rows) != len(openAIImageModels.order) || len(rows) != 15 {
		t.Fatalf("%d rows", len(rows))
	}
	found := false
	for i, r := range rows {
		if _, ok := openAIImageModels.get(r["id"].(string)); !ok || r["id"] != openAIImageModels.order[i] {
			t.Fatal(r)
		}
		found = found || r["id"] == p.DefaultModel()
	}
	if !found || openAIImageModels.meta["gpt-image-2.5-sunburst-xhigh"]["display"] != "GPT Image 2.5 Sunburst (Xhigh)" {
		t.Fatal(rows)
	}
}

func TestOpenAIAvailabilityFollowsKey(t *testing.T) {
	openAIHomeEnv(t, nil)
	if newOpenAIImageGenProvider().IsAvailable() {
		t.Fatal("available without a key")
	}
	t.Setenv("OPENAI_API_KEY", "test")
	if !newOpenAIImageGenProvider().IsAvailable() {
		t.Fatal("unavailable with a key")
	}
	prev := newOpenAIClientHook
	newOpenAIClientHook = nil
	defer func() { newOpenAIClientHook = prev }()
	if newOpenAIImageGenProvider().IsAvailable() {
		t.Fatal("available without the client")
	}
	res, _ := newOpenAIImageGenProvider().Generate(context.Background(), nil, ImageGenRequest{Prompt: "a cat"})
	if res["error_type"] != "missing_dependency" || res["error"] != openAIMissing {
		t.Fatal(res)
	}
}

func TestOpenAIModelResolution(t *testing.T) {
	env := openAIHomeEnv(t, nil)
	t.Setenv("OPENAI_IMAGE_MODEL", "gpt-image-2-high")
	if id, meta := resolveOpenAIImageModel(env); id != "gpt-image-2-high" || meta["quality"] != "high" {
		t.Fatal(id, meta)
	}
	os.Unsetenv("OPENAI_IMAGE_MODEL")
	setImageConfig(t, env, config.Tree{"image_gen": map[string]any{"openai": map[string]any{"model": "gpt-image-2-low"}}})
	if id, meta := resolveOpenAIImageModel(env); id != "gpt-image-2-low" || meta["quality"] != "low" {
		t.Fatal(id, meta)
	}
}

func TestOpenAIConfigBaseURLAndKeyEnvReachClientAndAvailability(t *testing.T) {
	f := newFakeOpenAIImages(t)
	env := openAIHomeEnv(t, config.Tree{"image_gen": map[string]any{"openai": map[string]any{"base_url": f.srv.URL + "/v1/", "key_env": "IMAGE_GATEWAY_TOKEN"}}})
	t.Setenv("IMAGE_GATEWAY_TOKEN", "gateway-token")
	p := newOpenAIImageGenProvider()
	if !p.IsAvailable() {
		t.Fatal("key_env not honored by availability")
	}
	res, err := p.Generate(context.Background(), env, ImageGenRequest{Prompt: "a cat"})
	if err != nil || res["success"] != true {
		t.Fatal(res, err)
	}
	o := f.options[0]
	if *o.BaseURL != f.srv.URL+"/v1" || *o.APIKey != "gateway-token" || o.DefaultHeaders["OpenAI-Project"] != "" || o.HTTPClient == nil {
		t.Fatalf("%+v", o)
	}
	req := f.last(t)
	if req.Path != "/v1/images/generations" || req.Header.Get("Authorization") != "Bearer gateway-token" {
		t.Fatalf("%+v", req)
	}
	if v, ok := req.Header["Openai-Project"]; !ok || len(v) != 1 || v[0] != "" {
		t.Fatalf("project header %v", req.Header)
	}
}

func TestOpenAICustomModelIDPassesThroughWithoutQuality(t *testing.T) {
	f := newFakeOpenAIImages(t)
	env := openAIHomeEnv(t, config.Tree{"image_gen": map[string]any{"model": "fal-ai/flux-2/klein/9b", "openai": map[string]any{"model": "custom-image-model"}}})
	t.Setenv("OPENAI_API_KEY", "k")
	res, err := newOpenAIImageGenProvider().Generate(context.Background(), env, ImageGenRequest{Prompt: "a cat"})
	if err != nil || res["success"] != true || res["model"] != "custom-image-model" {
		t.Fatal(res, err)
	}
	if body := f.last(t).JSON; body["model"] != "custom-image-model" || body["quality"] != nil {
		t.Fatal(body)
	}
	if _, has := f.last(t).JSON["quality"]; has {
		t.Fatal("quality sent for a custom model")
	}
	setImageConfig(t, env, config.Tree{"image_gen": map[string]any{"model": "fal-ai/flux-2/klein/9b"}})
	if id, _ := resolveOpenAIImageModel(env); id != "gpt-image-2-medium" {
		t.Fatal(id)
	}
}

func TestOpenAINamedCustomEndpointSuppliesBaseURLAndKey(t *testing.T) {
	providers := map[string]any{"my-gateway": map[string]any{"name": "My Gateway", "api": "https://gw.example/v1", "key_env": "MY_GW_KEY"}}
	env := openAIHomeEnv(t, config.Tree{"providers": providers, "image_gen": map[string]any{"openai": map[string]any{"provider": "my-gateway"}}})
	t.Setenv("MY_GW_KEY", "gw-token")
	t.Setenv("IMAGE_ONLY_KEY", "image-token")
	ctx := context.Background()
	if b, k, err := resolveOpenAIEndpoint(ctx, env); err != nil || b != "https://gw.example/v1" || k != "gw-token" {
		t.Fatal(b, k, err)
	}
	if !newOpenAIImageGenProvider().IsAvailable() {
		t.Fatal("named endpoint key ignored")
	}
	setImageConfig(t, env, config.Tree{"providers": providers, "image_gen": map[string]any{"openai": map[string]any{"provider": "my-gateway", "key_env": "IMAGE_ONLY_KEY"}}})
	if b, k, err := resolveOpenAIEndpoint(ctx, env); err != nil || b != "https://gw.example/v1" || k != "image-token" {
		t.Fatal(b, k, err)
	}
	setImageConfig(t, env, config.Tree{"image_gen": map[string]any{"openai": map[string]any{"provider": "no-such"}}})
	if b, k, err := resolveOpenAIEndpoint(ctx, env); err != nil || b != "" || k != "" {
		t.Fatal(b, k, err)
	}
}

func TestOpenAICustomBaseURLUsesEnvOnlyProxyClient(t *testing.T) {
	f := newFakeOpenAIImages(t)
	for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "https_proxy", "http_proxy", "all_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	env := openAIHomeEnv(t, config.Tree{"image_gen": map[string]any{"openai": map[string]any{"base_url": f.srv.URL + "/v1"}}})
	t.Setenv("OPENAI_API_KEY", "k")
	res, err := newOpenAIImageGenProvider().Generate(context.Background(), env, ImageGenRequest{Prompt: "a cat"})
	if err != nil || res["success"] != true {
		t.Fatal(res, err)
	}
	hc := f.options[0].HTTPClient
	if hc == nil || hc == http.DefaultClient {
		t.Fatal("no keepalive client handed to the OpenAI client")
	}
	want := provider.BuildKeepaliveHTTPClient(f.srv.URL+"/v1", false, provider.TLSVerify{})
	if want == nil || reflect.TypeOf(hc.Transport) != reflect.TypeOf(want.Transport) {
		t.Fatalf("handed %T, keepalive client %v", hc.Transport, want)
	}
}

func TestOpenAILoadImageBytesBlocksCredentialStoreAndAllowsImages(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	os.MkdirAll(h, 0o755)
	t.Setenv("ICEBATH_HOME", h)
	env := &tools.Env{Home: h}
	auth := filepath.Join(h, "auth.json")
	os.WriteFile(auth, []byte(`{"api_key":"sk-secret"}`), 0o600)
	_, _, err := loadOpenAIImageBytes(context.Background(), env, auth)
	if _, ok := err.(*imageValueError); !ok || !strings.Contains(err.Error(), "credential store") {
		t.Fatalf("%T %v", err, err)
	}
	img := filepath.Join(filepath.Dir(h), "pic.png")
	os.WriteFile(img, []byte("\x89PNG\r\n\x1a\nfake-image-bytes"), 0o644)
	data, name, err := loadOpenAIImageBytes(context.Background(), env, img)
	if err != nil || string(data) != "\x89PNG\r\n\x1a\nfake-image-bytes" || name != "pic.png" {
		t.Fatal(name, err)
	}
	data, name, err = loadOpenAIImageBytes(context.Background(), env, "data:image/webp;base64,"+base64.StdEncoding.EncodeToString([]byte("abc")))
	if err != nil || string(data) != "abc" || name != "image.webp" {
		t.Fatal(name, err)
	}
	if _, _, err := loadOpenAIImageBytes(context.Background(), env, filepath.Join(h, "missing.png")); err == nil || !strings.HasPrefix(err.Error(), "[Errno 2] No such file or directory") {
		t.Fatal(err)
	}
}

func TestOpenAIGenerateRejectsEmptyPromptAndMissingKey(t *testing.T) {
	env := openAIHomeEnv(t, nil)
	res, _ := newOpenAIImageGenProvider().Generate(context.Background(), env, ImageGenRequest{Prompt: "", AspectRatio: "square"})
	if res["success"] != false || res["error_type"] != "invalid_argument" {
		t.Fatal(res)
	}
	res, _ = newOpenAIImageGenProvider().Generate(context.Background(), env, ImageGenRequest{Prompt: "a cat"})
	if res["success"] != false || res["error_type"] != "auth_required" {
		t.Fatal(res)
	}
}

func TestOpenAIB64SavesToCache(t *testing.T) {
	f := newFakeOpenAIImages(t)
	env := openAIHomeEnv(t, nil)
	t.Setenv("OPENAI_API_KEY", "test-key")
	res, err := newOpenAIImageGenProvider().Generate(context.Background(), env, ImageGenRequest{Prompt: "a cat", AspectRatio: "landscape"})
	if err != nil || res["success"] != true || res["model"] != "gpt-image-2-medium" || res["aspect_ratio"] != "landscape" || res["provider"] != "openai" || res["quality"] != "medium" {
		t.Fatal(res, err)
	}
	saved := res["image"].(string)
	if filepath.Dir(saved) != filepath.Join(env.Home, "cache", "generated", "images") {
		t.Fatal(saved)
	}
	if data, _ := os.ReadFile(saved); string(data) != string(openAITestPNG) {
		t.Fatal("cached bytes differ")
	}
	body := f.last(t).JSON
	if body["model"] != "gpt-image-2" || body["quality"] != "medium" || body["size"] != "1536x1024" || !pyjson.Equal(body["n"], 1) {
		t.Fatal(body)
	}
	if _, has := body["response_format"]; has {
		t.Fatal("response_format sent")
	}
}

type recordedAuxUsage struct {
	mu   sync.Mutex
	rows []struct {
		Session, Task string
		Usage         provider.AuxiliaryUsage
	}
}

func (r *recordedAuxUsage) RecordAuxiliaryUsage(_ context.Context, sessionID, task string, u provider.AuxiliaryUsage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, struct {
		Session, Task string
		Usage         provider.AuxiliaryUsage
	}{sessionID, task, u})
	return nil
}

func TestOpenAITokenUsageReachesSessionAccounting(t *testing.T) {
	for _, hasImage := range []bool{true, false} {
		f := newFakeOpenAIImages(t)
		data := `[{"b64_json": "` + base64.StdEncoding.EncodeToString(openAITestPNG) + `"}]`
		if !hasImage {
			data = "[]"
		}
		f.reply = func(*http.Request) (int, string) {
			return 200, `{"data": ` + data + `, "usage": {"input_tokens": 23, "output_tokens": 1056, "total_tokens": 1079}}`
		}
		env := openAIHomeEnv(t, nil)
		t.Setenv("OPENAI_API_KEY", "test-key")
		db := &recordedAuxUsage{}
		ctx := provider.SetAccountingContext(context.Background(), db, "sess-1")
		res, _ := newOpenAIImageGenProvider().Generate(ctx, env, ImageGenRequest{Prompt: "a cat", AspectRatio: "landscape"})
		if res["success"] != hasImage || (!hasImage && res["error_type"] != "empty_response") {
			t.Fatal(res)
		}
		if len(db.rows) != 1 || db.rows[0].Session != "sess-1" || db.rows[0].Task != "image_generation" {
			t.Fatalf("%+v", db.rows)
		}
		u := db.rows[0].Usage
		if u.Model != "gpt-image-2" || u.BillingProvider != "openai" || u.InputTokens != 23 || u.OutputTokens != 1056 {
			t.Fatalf("%+v", u)
		}
	}
}

func TestOpenAISelectionReachesImageRequest(t *testing.T) {
	type combo struct{ api, quality string }
	var combos []combo
	for _, q := range []string{"low", "medium", "high"} {
		combos = append(combos, combo{"gpt-image-2", q})
	}
	for _, m := range []string{"gpt-image-2.5-flare", "gpt-image-2.5-sunburst"} {
		for _, q := range []string{"auto", "low", "medium", "high", "xhigh", "max"} {
			combos = append(combos, combo{m, q})
		}
	}
	for _, editing := range []bool{false, true} {
		for _, c := range combos {
			tier := c.api + "-" + c.quality
			if c.quality == "auto" {
				tier = c.api
			}
			f := newFakeOpenAIImages(t)
			env := openAIHomeEnv(t, config.Tree{"image_gen": map[string]any{"openai": map[string]any{"model": tier}}})
			t.Setenv("OPENAI_API_KEY", "test-key")
			src := filepath.Join(t.TempDir(), "source.png")
			os.WriteFile(src, openAITestPNG, 0o644)
			req := ImageGenRequest{Prompt: "a cat"}
			if editing {
				req.ImageURL = src
			}
			res, err := newOpenAIImageGenProvider().Generate(context.Background(), env, req)
			if err != nil || res["success"] != true || res["model"] != tier || res["quality"] != c.quality {
				t.Fatal(tier, editing, res, err)
			}
			got := f.last(t)
			model, quality := got.JSON["model"], got.JSON["quality"]
			if editing {
				if got.Path != "/v1/images/edits" || len(got.Files["image"]) != 1 || got.Files["image"][0].Name != "source.png" || string(got.Files["image"][0].Data) != string(openAITestPNG) {
					t.Fatalf("%+v", got)
				}
				model, quality = got.Fields["model"][0], got.Fields["quality"][0]
				if _, has := got.Fields["response_format"]; has || got.Fields["n"][0] != "1" || got.Fields["size"][0] != "1536x1024" {
					t.Fatalf("%+v", got.Fields)
				}
			} else if _, has := got.JSON["response_format"]; has {
				t.Fatal("response_format sent")
			}
			if model != c.api || quality != c.quality {
				t.Fatal(tier, model, quality)
			}
			if data, _ := os.ReadFile(res["image"].(string)); string(data) != string(openAITestPNG) {
				t.Fatal("cached bytes differ")
			}
		}
	}
}

func TestOpenAIAspectRatioMappingRevisedPromptAndURLCache(t *testing.T) {
	f := newFakeOpenAIImages(t)
	env := openAIHomeEnv(t, nil)
	t.Setenv("OPENAI_API_KEY", "test-key")
	for aspect, size := range map[string]string{"landscape": "1536x1024", "square": "1024x1024", "portrait": "1024x1536"} {
		newOpenAIImageGenProvider().Generate(context.Background(), env, ImageGenRequest{Prompt: "a cat", AspectRatio: aspect})
		if f.last(t).JSON["size"] != size {
			t.Fatal(aspect, f.last(t).JSON)
		}
	}
	f.reply = func(*http.Request) (int, string) {
		return 200, `{"data": [{"b64_json": "` + base64.StdEncoding.EncodeToString(openAITestPNG) + `", "revised_prompt": "A photo of a cat"}]}`
	}
	res, _ := newOpenAIImageGenProvider().Generate(context.Background(), env, ImageGenRequest{Prompt: "a cat"})
	if res["revised_prompt"] != "A photo of a cat" {
		t.Fatal(res)
	}
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(openAITestPNG)
	}))
	defer imgSrv.Close()
	t.Setenv("ICEBATH_ALLOW_PRIVATE_URLS", "1")
	f.reply = func(*http.Request) (int, string) {
		return 200, `{"data": [{"b64_json": null, "url": "` + imgSrv.URL + `/img.png"}]}`
	}
	res, _ = newOpenAIImageGenProvider().Generate(context.Background(), env, ImageGenRequest{Prompt: "a cat"})
	img, _ := res["image"].(string)
	if res["success"] != true || !filepath.IsAbs(img) || strings.Contains(img, "127.0.0.1") {
		t.Fatal(res)
	}
	if data, _ := os.ReadFile(img); string(data) != string(openAITestPNG) {
		t.Fatal("URL result not cached")
	}
}

func TestOpenAIClientErrorsRetriesAndMultipart(t *testing.T) {
	f := newFakeOpenAIImages(t)
	prevSleep := openAISleep
	var slept int
	openAISleep = func(context.Context, time.Duration) error { slept++; return nil }
	t.Cleanup(func() { openAISleep = prevSleep })
	key := "k"
	c, err := newOpenAIClientHook(nil, openAIClientOptions{APIKey: &key})
	if err != nil {
		t.Fatal(err)
	}
	f.reply = func(*http.Request) (int, string) { return 400, `{"error": {"message": "bad size", "code": null}}` }
	_, err = c.ImagesGenerate(context.Background(), map[string]any{"prompt": "p", "size": "1x1"})
	oe, ok := err.(*openAIError)
	if !ok || oe.Class != "BadRequestError" || err.Error() != "Error code: 400 - {'error': {'message': 'bad size', 'code': None}}" || slept != 0 {
		t.Fatalf("%v %d", err, slept)
	}
	calls := 0
	f.reply = func(*http.Request) (int, string) {
		calls++
		if calls < 3 {
			return 500, "upstream down"
		}
		return 200, `{"data": [{"url": "https://x/y.png"}]}`
	}
	resp, err := c.ImagesGenerate(context.Background(), map[string]any{"prompt": "p"})
	if err != nil || slept != 2 || openAIItemString(resp.Data[0], "url") != "https://x/y.png" || f.last(t).Header.Get("X-Stainless-Retry-Count") != "2" {
		t.Fatal(resp, err, slept)
	}
	f.reply = func(*http.Request) (int, string) { return 503, "" }
	_, err = c.ImagesGenerate(context.Background(), map[string]any{"prompt": "p"})
	if oe, ok := err.(*openAIError); !ok || oe.Class != "InternalServerError" || err.Error() != "Error code: 503" {
		t.Fatal(err)
	}
	if _, err := c.ImagesGenerate(context.Background(), map[string]any{"prompt": "p", "colour": "red"}); err == nil || err.Error() != "Images.generate() got an unexpected keyword argument 'colour'" {
		t.Fatal(err)
	}
	f.reply = func(*http.Request) (int, string) { return 200, `{"data": []}` }
	_, err = c.ImagesEdit(context.Background(), map[string]any{"prompt": "p", "n": 1, "image": []openAIFile{{Name: "a.png", Data: []byte("1")}, {Name: "b.jpg", Data: []byte("2")}}})
	got := f.last(t)
	if err != nil || got.Path != "/v1/images/edits" || len(got.Files["image[]"]) != 2 || got.Files["image[]"][1].Name != "b.jpg" || got.Fields["prompt"][0] != "p" {
		t.Fatalf("%+v %v", got, err)
	}
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	c.BaseURL = dead.URL
	if _, err := c.ImagesGenerate(context.Background(), map[string]any{"prompt": "p"}); err == nil || err.Error() != "Connection error." || err.(*openAIError).Class != "APIConnectionError" {
		t.Fatal(err)
	}
	if _, err := newOpenAIClient(nil, openAIClientOptions{}); err == nil || err.(*openAIError).Class != "OpenAIError" {
		t.Fatal(err)
	}
}

func TestOpenAILoadImageBytesRefusesInternalTargetsBeforeConnect(t *testing.T) {
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
		w.Header().Set("Content-Type", "image/png")
		w.Write(openAITestPNG)
	}))
	defer srv.Close()
	h := filepath.Join(t.TempDir(), ".icebath")
	os.MkdirAll(h, 0o755)
	t.Setenv("ICEBATH_HOME", h)
	env := &tools.Env{Home: h}
	t.Setenv("ICEBATH_ALLOW_PRIVATE_URLS", "")
	os.Unsetenv("ICEBATH_ALLOW_PRIVATE_URLS")
	if data, _, err := loadOpenAIImageBytes(context.Background(), env, srv.URL+"/img.png"); err == nil || data != nil || len(hits) != 0 {
		t.Fatal("loopback fetched", err, hits)
	}
	t.Setenv("ICEBATH_ALLOW_PRIVATE_URLS", "1")
	if data, _, err := loadOpenAIImageBytes(context.Background(), env, srv.URL+"/to-metadata/img.png"); err == nil || data != nil {
		t.Fatal("redirect to metadata followed", err)
	}
	if len(hits) != 1 || hits[0] != "/to-metadata/img.png" {
		t.Fatal(hits)
	}
}
