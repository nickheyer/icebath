package media

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
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
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

const kreaJobID = "00000000-0000-0000-0000-000000000abc"

type kreaSeen struct {
	method, path string
	header       http.Header
	body         map[string]any
}

// One scripted response, dropped or delayed when asked
type kreaReply struct {
	status int
	body   string
	hang   time.Duration
	drop   bool
}

// Krea API double answering POSTs and GETs from scripted sequences whose last entry repeats
type kreaFake struct {
	mu           sync.Mutex
	srv          *httptest.Server
	seen         []kreaSeen
	posts, gets  []kreaReply
	npost, nget  int
	savedPrefix  []string
	savedURLs    []string
	saveFailures bool
}

func kreaSubmitBody(jobID string) string {
	return `{"job_id": "` + jobID + `", "status": "queued", "created_at": "2026-05-27T00:00:00Z", "completed_at": null, "result": null}`
}

func kreaCompletedJob(url string) string {
	return `{"job_id": "` + kreaJobID + `", "status": "completed", "created_at": "2026-05-27T00:00:00Z", "completed_at": "2026-05-27T00:00:30Z", "result": {"urls": ["` + url + `"]}}`
}

func (f *kreaFake) requests(method string) []kreaSeen {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []kreaSeen{}
	for _, s := range f.seen {
		if s.method == method {
			out = append(out, s)
		}
	}
	return out
}

// Temp home with KREA_API_KEY set, the base URL on the fake API, sleeps skipped and caching recorded
func kreaTestEnv(t *testing.T) (*tools.Env, *kreaFake) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	env := &tools.Env{Home: h}
	setImageConfig(t, env, config.Tree{})
	t.Setenv("KREA_API_KEY", "test-key-12345")
	t.Setenv("KREA_IMAGE_MODEL", "")
	os.Unsetenv("KREA_IMAGE_MODEL")
	f := &kreaFake{posts: []kreaReply{{200, kreaSubmitBody(kreaJobID), 0, false}}, gets: []kreaReply{{200, kreaCompletedJob("https://krea.cdn/img.png"), 0, false}}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.seen = append(f.seen, kreaSeen{r.Method, r.URL.Path, r.Header.Clone(), body})
		var rep kreaReply
		if r.Method == http.MethodPost {
			rep = f.posts[min(f.npost, len(f.posts)-1)]
			f.npost++
		} else {
			rep = f.gets[min(f.nget, len(f.gets)-1)]
			f.nget++
		}
		f.mu.Unlock()
		if rep.drop {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		if rep.hang > 0 {
			time.Sleep(rep.hang)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rep.status)
		io.WriteString(w, rep.body)
	}))
	t.Cleanup(f.srv.Close)
	prevBase, prevSleep, prevSave, prevTimeout := kreaBaseURL, kreaSleep, kreaSaveURLImage, kreaRequestTimeout
	kreaBaseURL = f.srv.URL
	kreaSleep = func(time.Duration) {}
	kreaSaveURLImage = func(_ context.Context, _ *tools.Env, rawURL, prefix string) (string, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.savedURLs, f.savedPrefix = append(f.savedURLs, rawURL), append(f.savedPrefix, prefix)
		if f.saveFailures {
			return "", errors.New("404")
		}
		return "/tmp/" + rawURL[strings.LastIndex(rawURL, "/")+1:], nil
	}
	t.Cleanup(func() {
		kreaBaseURL, kreaSleep, kreaSaveURLImage, kreaRequestTimeout = prevBase, prevSleep, prevSave, prevTimeout
	})
	return env, f
}

func kreaGenerate(t *testing.T, env *tools.Env, req ImageGenRequest) map[string]any {
	t.Helper()
	if req.AspectRatio == "" {
		req.AspectRatio = "landscape"
	}
	out, err := newKreaImageGenProvider().Generate(context.Background(), env, req)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func kreaManagedConfig(origin string) func(*tools.Env) *tools.ManagedToolGatewayConfig {
	return func(*tools.Env) *tools.ManagedToolGatewayConfig {
		return &tools.ManagedToolGatewayConfig{Vendor: "krea", GatewayOrigin: origin, NousUserToken: "nous-tok-abc", ManagedMode: true}
	}
}

func useKreaManaged(t *testing.T, fn func(*tools.Env) *tools.ManagedToolGatewayConfig) {
	prev := kreaManagedGateway
	kreaManagedGateway = fn
	t.Cleanup(func() { kreaManagedGateway = prev })
}

func TestKreaImageGenProvider(t *testing.T) {
	t.Run("test_is_available_with_key", func(t *testing.T) {
		kreaTestEnv(t)
		t.Setenv("KREA_API_KEY", "sk-test")
		if !newKreaImageGenProvider().IsAvailable() {
			t.Fatal("not available with key")
		}
	})
}

func TestKreaModelResolution(t *testing.T) {
	t.Run("test_env_override_large", func(t *testing.T) {
		env, _ := kreaTestEnv(t)
		t.Setenv("KREA_IMAGE_MODEL", "krea-2-large")
		id, meta := kreaResolveModel(env, "")
		if id != "krea-2-large" || meta["path"] != "/generate/image/krea/krea-2/large" {
			t.Fatal(id, meta)
		}
	})
}

func TestKreaGenerate(t *testing.T) {
	t.Run("test_missing_api_key", func(t *testing.T) {
		env, _ := kreaTestEnv(t)
		t.Setenv("KREA_API_KEY", "")
		out := kreaGenerate(t, env, ImageGenRequest{Prompt: "test"})
		if out["success"] != false || !strings.Contains(out["error"].(string), "KREA_API_KEY") || out["error_type"] != "auth_required" {
			t.Fatal(out)
		}
	})
	t.Run("test_empty_prompt", func(t *testing.T) {
		env, _ := kreaTestEnv(t)
		out := kreaGenerate(t, env, ImageGenRequest{Prompt: "   "})
		if out["success"] != false || out["error_type"] != "invalid_argument" {
			t.Fatal(out)
		}
	})
	t.Run("test_successful_generation", func(t *testing.T) {
		env, f := kreaTestEnv(t)
		f.gets = []kreaReply{{200, kreaCompletedJob("https://krea.cdn/result.png"), 0, false}}
		out := kreaGenerate(t, env, ImageGenRequest{Prompt: "A cinematic lamp", Kwargs: map[string]any{"upscale": false}})
		if out["success"] != true || out["image"] != "/tmp/result.png" || out["provider"] != "krea" || out["model"] != "krea-2-medium" ||
			out["aspect_ratio"] != "landscape" || out["job_id"] != kreaJobID || out["resolution"] != "1K" || out["creativity"] != "medium" {
			t.Fatal(out)
		}
		if posts := f.requests("POST"); len(posts) != 1 || posts[0].path != "/generate/image/krea/krea-2/medium" {
			t.Fatal(posts)
		}
		if gets := f.requests("GET"); len(gets) != 1 || gets[0].path != "/jobs/"+kreaJobID {
			t.Fatal(gets)
		}
		if !reflect.DeepEqual(f.savedPrefix, []string{"krea_krea-2-medium"}) {
			t.Fatal(f.savedPrefix)
		}
	})
	t.Run("test_large_model_routes_to_large_endpoint", func(t *testing.T) {
		env, f := kreaTestEnv(t)
		t.Setenv("KREA_IMAGE_MODEL", "krea-2-large")
		kreaGenerate(t, env, ImageGenRequest{Prompt: "test"})
		if p := f.requests("POST"); p[0].path != "/generate/image/krea/krea-2/large" {
			t.Fatal(p[0].path)
		}
	})
	t.Run("test_aspect_ratio_mapping", func(t *testing.T) {
		env, f := kreaTestEnv(t)
		kreaGenerate(t, env, ImageGenRequest{Prompt: "test", AspectRatio: "square", Kwargs: map[string]any{"upscale": false}})
		if b := f.requests("POST")[0].body; b["aspect_ratio"] != "1:1" || b["resolution"] != "1K" {
			t.Fatal(b)
		}
	})
	t.Run("test_auth_header", func(t *testing.T) {
		env, f := kreaTestEnv(t)
		kreaGenerate(t, env, ImageGenRequest{Prompt: "test"})
		h := f.requests("POST")[0].header
		if h.Get("Authorization") != "Bearer test-key-12345" || h.Get("Content-Type") != "application/json" ||
			h.Get("User-Agent") != "Icebath-Agent/1.0 (krea-image-gen)" || h.Get("x-idempotency-key") != "" {
			t.Fatal(h)
		}
	})
	t.Run("test_passthrough_seed_styles_moodboards", func(t *testing.T) {
		env, f := kreaTestEnv(t)
		refs := []any{}
		for i := range 15 {
			refs = append(refs, map[string]any{"url": "https://x.com/" + string(rune('a'+i)) + ".png"})
		}
		kreaGenerate(t, env, ImageGenRequest{Prompt: "test", Kwargs: map[string]any{"seed": int64(42),
			"styles":                 []any{map[string]any{"id": "lora-1", "strength": 0.7}},
			"moodboards":             []any{map[string]any{"url": "https://x.com/mood.png"}, map[string]any{"url": "https://x.com/mood2.png"}},
			"image_style_references": refs, "creativity": "high", "upscale": false}})
		b := f.requests("POST")[0].body
		if b["seed"] != float64(42) || !reflect.DeepEqual(b["styles"], []any{map[string]any{"id": "lora-1", "strength": 0.7}}) ||
			len(b["moodboards"].([]any)) != 1 || len(b["image_style_references"].([]any)) != 10 || b["creativity"] != "high" {
			t.Fatal(b)
		}
	})
	t.Run("test_sliders_reach_payload_and_out_of_range_values_are_dropped", func(t *testing.T) {
		env, f := kreaTestEnv(t)
		kreaGenerate(t, env, ImageGenRequest{Prompt: "test", Kwargs: map[string]any{"intensity": int64(80), "complexity": int64(-100), "movement": int64(150), "upscale": false}})
		b := f.requests("POST")[0].body
		if _, has := b["movement"]; b["intensity"] != float64(80) || b["complexity"] != float64(-100) || has {
			t.Fatal(b)
		}
	})
	t.Run("test_string_style_references_converted_to_objects", func(t *testing.T) {
		env, f := kreaTestEnv(t)
		kreaGenerate(t, env, ImageGenRequest{Prompt: "test", Kwargs: map[string]any{"upscale": false,
			"image_style_references": []any{"https://x.com/a.png", map[string]any{"url": "https://x.com/b.png", "strength": 1.2}}}})
		want := []any{map[string]any{"url": "https://x.com/a.png", "strength": 0.6}, map[string]any{"url": "https://x.com/b.png", "strength": 1.2}}
		if b := f.requests("POST")[0].body; !reflect.DeepEqual(b["image_style_references"], want) {
			t.Fatal(b)
		}
	})
	t.Run("test_local_style_reference_is_embedded_as_data_uri", func(t *testing.T) {
		env, f := kreaTestEnv(t)
		image := filepath.Join(t.TempDir(), "ref.png")
		data := []byte("\x89PNG\r\n\x1a\nfake-image-bytes")
		os.WriteFile(image, data, 0o644)
		kreaGenerate(t, env, ImageGenRequest{Prompt: "test", ImageURL: image, Kwargs: map[string]any{"upscale": false}})
		want := []any{map[string]any{"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(data), "strength": 0.6}}
		if b := f.requests("POST")[0].body; !reflect.DeepEqual(b["image_style_references"], want) {
			t.Fatal(b)
		}
	})
	t.Run("test_unusable_local_style_references_are_refused_before_submit", func(t *testing.T) {
		for _, c := range []struct {
			names              []string
			errorType, message string
		}{
			{[]string{"nowhere.png"}, "invalid_image_url", "not found"},
			{[]string{"a.png", "b.png"}, "source_too_large", "total over"},
			{[]string{".env"}, "invalid_image_url", "Access denied"},
		} {
			t.Run(strings.Join(c.names, ","), func(t *testing.T) {
				env, f := kreaTestEnv(t)
				prev := kreaMaxLocalReferenceBytes
				kreaMaxLocalReferenceBytes = 10
				t.Cleanup(func() { kreaMaxLocalReferenceBytes = prev })
				for _, n := range []string{"a.png", "b.png"} {
					os.WriteFile(filepath.Join(env.Home, n), []byte("\x89PNG\r\n"), 0o644)
				}
				refs := []string{}
				for _, n := range c.names {
					refs = append(refs, filepath.Join(env.Home, n))
				}
				out := kreaGenerate(t, env, ImageGenRequest{Prompt: "test", ReferenceImageURLs: refs})
				if out["success"] != false || out["error_type"] != c.errorType || !strings.Contains(out["error"].(string), c.message) {
					t.Fatal(out)
				}
				if len(f.requests("POST")) != 0 {
					t.Fatal("request sent")
				}
			})
		}
	})
	t.Run("test_unknown_kwargs_ignored", func(t *testing.T) {
		env, _ := kreaTestEnv(t)
		out := kreaGenerate(t, env, ImageGenRequest{Prompt: "test", Kwargs: map[string]any{"fictional_param": "should be ignored", "num_images": int64(4)}})
		if out["success"] != true {
			t.Fatal(out)
		}
	})
}

func TestKreaGenerateErrors(t *testing.T) {
	t.Run("test_submit_http_error", func(t *testing.T) {
		env, f := kreaTestEnv(t)
		f.posts = []kreaReply{{401, `{"error": {"message": "Invalid API key"}}`, 0, false}}
		out := kreaGenerate(t, env, ImageGenRequest{Prompt: "test"})
		if e := out["error"].(string); out["success"] != false || out["error_type"] != "api_error" || !strings.Contains(e, "401") || !strings.Contains(e, "Invalid API key") {
			t.Fatal(out)
		}
	})
	t.Run("test_submit_failure_marks_whether_a_fal_fallback_is_safe", func(t *testing.T) {
		for _, c := range []struct {
			name     string
			reply    kreaReply
			eligible bool
		}{
			{"refused", kreaReply{}, true},
			{"429", kreaReply{429, `{"error": "nope"}`, 0, false}, true},
			{"disconnected", kreaReply{drop: true}, false},
			{"503", kreaReply{503, `{"error": "nope"}`, 0, false}, false},
			{"422", kreaReply{422, `{"error": "nope"}`, 0, false}, false},
			{"timeout", kreaReply{200, kreaSubmitBody(kreaJobID), time.Second, false}, false},
			{"invalid_json", kreaReply{200, "not json", 0, false}, false},
		} {
			t.Run(c.name, func(t *testing.T) {
				env, f := kreaTestEnv(t)
				f.posts = []kreaReply{c.reply}
				if c.name == "refused" {
					l, _ := net.Listen("tcp", "127.0.0.1:0")
					kreaBaseURL = "http://" + l.Addr().String()
					l.Close()
				}
				if c.name == "timeout" {
					kreaRequestTimeout = 200 * time.Millisecond
				}
				out := kreaGenerate(t, env, ImageGenRequest{Prompt: "test"})
				if out["success"] != false || out["fallback_eligible"] != c.eligible {
					t.Fatal(out)
				}
				if c.name == "timeout" && out["error"] != "Krea submit timed out (30s)" {
					t.Fatal(out)
				}
				if c.name == "503" && !strings.Contains(out["error"].(string), `(503): {"error": "nope"}`) {
					t.Fatal(out)
				}
			})
		}
	})
	t.Run("test_job_failed", func(t *testing.T) {
		env, f := kreaTestEnv(t)
		f.gets = []kreaReply{{200, `{"job_id": "abc", "status": "failed", "completed_at": "2026-05-27T00:01:00Z", "result": {"error": "NSFW content"}}`, 0, false}}
		out := kreaGenerate(t, env, ImageGenRequest{Prompt: "test"})
		if _, has := out["fallback_eligible"]; out["success"] != false || out["error_type"] != "api_error" || !strings.Contains(out["error"].(string), "NSFW") || has {
			t.Fatal(out)
		}
	})
	t.Run("test_completed_but_missing_urls", func(t *testing.T) {
		env, f := kreaTestEnv(t)
		f.gets = []kreaReply{{200, `{"job_id": "abc", "status": "completed", "completed_at": "2026-05-27T00:01:00Z", "result": {"urls": []}}`, 0, false}}
		out := kreaGenerate(t, env, ImageGenRequest{Prompt: "test"})
		if out["success"] != false || out["error_type"] != "empty_response" {
			t.Fatal(out)
		}
	})
	t.Run("test_url_download_failure_falls_back_to_bare_url", func(t *testing.T) {
		env, f := kreaTestEnv(t)
		f.gets = []kreaReply{{200, kreaCompletedJob("https://krea.cdn/expired-soon.png"), 0, false}}
		f.saveFailures = true
		out := kreaGenerate(t, env, ImageGenRequest{Prompt: "test"})
		if out["success"] != true || out["image"] != "https://krea.cdn/expired-soon.png" {
			t.Fatal(out)
		}
	})
	t.Run("test_polling_picks_up_completed_at_with_unknown_status", func(t *testing.T) {
		env, f := kreaTestEnv(t)
		f.gets = []kreaReply{{200, `{"job_id": "abc", "status": "intermediate-complete", "completed_at": "2026-05-27T00:01:00Z", "result": {"urls": ["https://krea.cdn/done.png"]}}`, 0, false}}
		out := kreaGenerate(t, env, ImageGenRequest{Prompt: "test"})
		if out["success"] != true || out["completed_at"] != "2026-05-27T00:01:00Z" {
			t.Fatal(out)
		}
	})
}

func TestKreaPollRetryPolicy(t *testing.T) {
	t.Run("test_poll_fails_fast_on_401", func(t *testing.T) {
		env, f := kreaTestEnv(t)
		f.gets = []kreaReply{{401, `{"error": "boom"}`, 0, false}}
		out := kreaGenerate(t, env, ImageGenRequest{Prompt: "test"})
		if out["success"] != false || out["error_type"] != "api_error" || !strings.Contains(out["error"].(string), "401") || len(f.requests("GET")) != 1 {
			t.Fatal(out, len(f.requests("GET")))
		}
	})
	t.Run("retries_transient_statuses_until_terminal", func(t *testing.T) {
		env, f := kreaTestEnv(t)
		var slept []time.Duration
		kreaSleep = func(d time.Duration) { slept = append(slept, d) }
		f.gets = []kreaReply{{503, `{}`, 0, false}, {200, `{"status": "processing"}`, 0, false}, {200, kreaCompletedJob("https://krea.cdn/a.png"), 0, false}}
		out := kreaGenerate(t, env, ImageGenRequest{Prompt: "test"})
		want := []time.Duration{2 * time.Second, 2600 * time.Millisecond, 3380 * time.Millisecond}
		if out["success"] != true || len(slept) != 3 || slept[0] != want[0] || slept[1] != want[1] || (slept[2]-want[2]).Abs() > time.Millisecond {
			t.Fatal(out, slept)
		}
	})
	t.Run("deadline_reports_last_status", func(t *testing.T) {
		env, f := kreaTestEnv(t)
		start := time.Now()
		calls := 0
		prev := kreaNow
		kreaNow = func() time.Time { calls++; return start.Add(time.Duration(calls) * 100 * time.Second) }
		t.Cleanup(func() { kreaNow = prev })
		f.gets = []kreaReply{{200, `{"status": "sampling"}`, 0, false}}
		out := kreaGenerate(t, env, ImageGenRequest{Prompt: "test"})
		if out["error_type"] != "timeout" || out["error"] != "Krea job "+kreaJobID+" did not complete within 180s (last status: sampling)" {
			t.Fatal(out)
		}
	})
}

func TestKreaManagedGateway(t *testing.T) {
	t.Run("test_managed_submit_uses_gateway_origin_and_nous_token", func(t *testing.T) {
		env, f := kreaTestEnv(t)
		useKreaManaged(t, kreaManagedConfig(f.srv.URL+"/"))
		kreaBaseURL = "http://direct.invalid"
		out := kreaGenerate(t, env, ImageGenRequest{Prompt: "A managed lamp", Kwargs: map[string]any{"upscale": false}})
		post, get := f.requests("POST"), f.requests("GET")
		if out["success"] != true || post[0].path != "/generate/image/krea/krea-2/medium" || post[0].header.Get("Authorization") != "Bearer nous-tok-abc" ||
			post[0].header.Get("x-idempotency-key") == "" || !strings.HasPrefix(get[0].path, "/jobs/") || get[0].header.Get("Authorization") != "Bearer nous-tok-abc" ||
			get[0].header.Get("x-idempotency-key") != "" {
			t.Fatal(out, post, get)
		}
	})
	t.Run("test_managed_429_concurrency_hint", func(t *testing.T) {
		env, f := kreaTestEnv(t)
		useKreaManaged(t, kreaManagedConfig(f.srv.URL))
		f.posts = []kreaReply{{429, `{"error": {"message": "maximum number of concurrent jobs"}}`, 0, false}}
		out := kreaGenerate(t, env, ImageGenRequest{Prompt: "test"})
		if e := out["error"].(string); out["success"] != false || !strings.Contains(e, "429") || !strings.Contains(strings.ToLower(e), "concurrency") {
			t.Fatal(out)
		}
	})
	t.Run("managed_refuses_loras_and_moodboards_before_submit", func(t *testing.T) {
		env, f := kreaTestEnv(t)
		useKreaManaged(t, kreaManagedConfig(f.srv.URL))
		out := kreaGenerate(t, env, ImageGenRequest{Prompt: "test", Kwargs: map[string]any{"moodboards": []any{map[string]any{"url": "https://m"}}}})
		if out["error_type"] != "unsupported_argument" || out["error"] != "Managed Krea (Nous Subscription) does not support moodboards. Set KREA_API_KEY to use Krea directly, or omit `moodboards`." ||
			len(f.requests("POST")) != 0 {
			t.Fatal(out)
		}
	})
}

func TestKreaExplicitModelOverride(t *testing.T) {
	t.Run("test_model_kwarg_overrides_config", func(t *testing.T) {
		env, _ := kreaTestEnv(t)
		id, meta := kreaResolveModel(env, "krea-2-large")
		if id != "krea-2-large" || meta["path"] != "/generate/image/krea/krea-2/large" {
			t.Fatal(id, meta)
		}
	})
	t.Run("test_turbo_routes_to_medium_turbo_endpoint", func(t *testing.T) {
		env, f := kreaTestEnv(t)
		out := kreaGenerate(t, env, ImageGenRequest{Prompt: "test", Kwargs: map[string]any{"model": "krea-2-medium-turbo", "upscale": false}})
		if out["success"] != true || out["model"] != "krea-2-medium-turbo" || f.requests("POST")[0].path != "/generate/image/krea/krea-2/medium-turbo" {
			t.Fatal(out)
		}
	})
}

// Generation with sequenced submit and poll replies, the enhance pair second when an enhance job is given
func kreaRunUpscale(t *testing.T, upscale any, enhanceJob string) (map[string]any, *kreaFake) {
	env, f := kreaTestEnv(t)
	f.posts = []kreaReply{{200, kreaSubmitBody(kreaJobID), 0, false}, {200, kreaSubmitBody("00000000-0000-0000-0000-00000000e0e0"), 0, false}}
	f.gets = []kreaReply{{200, kreaCompletedJob("https://krea.cdn/native.png"), 0, false}}
	if enhanceJob != "" {
		f.gets = append(f.gets, kreaReply{200, enhanceJob, 0, false})
	}
	kwargs := map[string]any{}
	if upscale != nil {
		kwargs["upscale"] = upscale
	}
	return kreaGenerate(t, env, ImageGenRequest{Prompt: "a lamp", Kwargs: kwargs}), f
}

func TestKreaUpscalePass(t *testing.T) {
	t.Run("test_upscale_routes_through_enhance_endpoint", func(t *testing.T) {
		out, f := kreaRunUpscale(t, true, `{"job_id": "00000000-0000-0000-0000-00000000e0e0", "status": "completed", "completed_at": "2026-05-27T00:01:00Z", "result": {"urls": ["https://krea.cdn/enhanced.png"]}}`)
		posts := f.requests("POST")
		if out["success"] != true || out["upscaled"] != true || out["upscale_factor"] != 2 || !strings.HasSuffix(out["image"].(string), "enhanced.png") || len(posts) != 2 {
			t.Fatal(out, posts)
		}
		b := posts[1].body
		if posts[1].path != "/generate/enhance/krea/enhance" || b["image_url"] != "https://krea.cdn/native.png" || b["image_scaling_factor"] != float64(2) || b["prompt"] != "a lamp" {
			t.Fatal(posts[1])
		}
		if gets := f.requests("GET"); gets[1].path != "/jobs/00000000-0000-0000-0000-00000000e0e0" {
			t.Fatal(gets)
		}
	})
	t.Run("test_upscale_failure_falls_back_to_native", func(t *testing.T) {
		out, f := kreaRunUpscale(t, true, `{"job_id": "00000000-0000-0000-0000-00000000e0e0", "status": "failed", "completed_at": "2026-05-27T00:01:00Z", "result": null}`)
		if out["success"] != true || out["upscaled"] != false || !strings.HasSuffix(out["image"].(string), "native.png") || len(f.requests("POST")) != 2 {
			t.Fatal(out)
		}
	})
	t.Run("test_medium_skips_upscale_by_default", func(t *testing.T) {
		out, f := kreaRunUpscale(t, nil, "")
		if out["success"] != true || out["upscaled"] != false || !strings.HasSuffix(out["image"].(string), "native.png") || len(f.requests("POST")) != 1 {
			t.Fatal(out)
		}
	})
	t.Run("test_explicit_false_disables_default", func(t *testing.T) {
		out, f := kreaRunUpscale(t, false, "")
		if out["success"] != true || out["upscaled"] != false || !strings.HasSuffix(out["image"].(string), "native.png") || len(f.requests("POST")) != 1 {
			t.Fatal(out)
		}
	})
	t.Run("config_upscale_applies_without_an_explicit_value", func(t *testing.T) {
		env, _ := kreaTestEnv(t)
		setImageConfig(t, env, config.Tree{"image_gen": map[string]any{"krea": map[string]any{"upscale": true}}})
		if !kreaUpscaleRequested(env, nil, map[string]any{"upscale": false}) || kreaUpscaleRequested(env, false, nil) {
			t.Fatal("upscale precedence")
		}
	})
}

func TestKreaRegistration(t *testing.T) {
	t.Run("test_register", func(t *testing.T) {
		resetImageRegistry(t)
		env := imageEnv(t, config.Tree{})
		t.Setenv("ICEBATH_HOME", env.Home)
		plugins.InstallServices(&plugins.Services{ProviderRegistries: map[string]plugins.ScopedRegistry{"image_gen_provider": ImageGenScopedRegistry()}})
		t.Cleanup(func() { plugins.InstallServices(nil); plugins.ResetPluginManagersForTests() })
		plugins.ResetPluginManagersForTests()
		ctx := envContext(context.Background(), env)
		plugins.GetPluginManager(ctx).DiscoverAndLoad(ctx, true)
		if p, ok := GetImageGenProvider(ctx, "krea", "").(*kreaImageGenProvider); !ok || p.Name() != "krea" {
			t.Fatalf("registered %T", GetImageGenProvider(ctx, "krea", ""))
		}
		if m := (imageGenBackendPlugin{name: "krea", version: "1.1.0"}).Manifest(); m.Version != "1.1.0" || m.Key != "image_gen/krea" {
			t.Fatal(m)
		}
	})
}

func TestKrea3(t *testing.T) {
	krea3 := func(t *testing.T, req ImageGenRequest) (map[string]any, kreaSeen) {
		env, f := kreaTestEnv(t)
		t.Setenv("KREA_IMAGE_MODEL", "krea-3")
		req.Prompt = "make the mug red"
		if req.Kwargs == nil {
			req.Kwargs = map[string]any{}
		}
		req.Kwargs["upscale"] = false
		out := kreaGenerate(t, env, req)
		return out, f.requests("POST")[0]
	}
	t.Run("test_submits_to_the_krea_3_path_with_fast_1k_defaults", func(t *testing.T) {
		out, post := krea3(t, ImageGenRequest{})
		want := map[string]any{"prompt": "make the mug red", "aspect_ratio": "16:9", "resolution": "1K", "thinking_mode": "fast"}
		if _, has := out["creativity"]; post.path != "/generate/image/krea/krea-3" || !reflect.DeepEqual(post.body, want) ||
			out["resolution"] != "1K" || out["thinking_mode"] != "fast" || has {
			t.Fatal(out, post)
		}
	})
	t.Run("test_an_edit_keeps_the_pictures_framing_and_sends_only_krea_3_fields", func(t *testing.T) {
		image := filepath.Join(t.TempDir(), "mug.png")
		data := []byte("\x89PNG\r\n\x1a\nmug")
		os.WriteFile(image, data, 0o644)
		_, post := krea3(t, ImageGenRequest{ImageURL: image, ReferenceImageURLs: []string{image, "https://example.com/red.png"},
			Kwargs: map[string]any{"thinking": "high", "resolution": "2k", "creativity": "raw", "intensity": int64(50)}})
		b := post.body
		want := []any{"data:image/png;base64," + base64.StdEncoding.EncodeToString(data), "https://example.com/red.png"}
		if !reflect.DeepEqual(b["image_urls"], want) || b["resolution"] != "2K" || b["thinking_mode"] != "high" {
			t.Fatal(b)
		}
		for _, k := range []string{"aspect_ratio", "creativity", "intensity", "image_style_references"} {
			if _, has := b[k]; has {
				t.Fatal(k, b)
			}
		}
	})
	t.Run("test_caps_reference_images_at_the_gateway_limit", func(t *testing.T) {
		refs := []string{}
		for i := range 6 {
			refs = append(refs, "https://example.com/"+string(rune('0'+i))+".png")
		}
		_, post := krea3(t, ImageGenRequest{ReferenceImageURLs: refs})
		if len(post.body["image_urls"].([]any)) != 4 {
			t.Fatal(post.body)
		}
	})
	t.Run("test_capabilities_follow_the_active_model", func(t *testing.T) {
		env, _ := kreaTestEnv(t)
		p := newKreaImageGenProvider()
		p.env = func() *tools.Env { return env }
		t.Setenv("KREA_IMAGE_MODEL", "krea-3")
		c3 := p.Capabilities()
		if _, has := c3["source_image_role"]; c3["max_reference_images"] != 4 || !reflect.DeepEqual(c3["creative_controls"], []any{"thinking", "resolution"}) || has {
			t.Fatal(c3)
		}
		t.Setenv("KREA_IMAGE_MODEL", "krea-2-medium")
		c2 := p.Capabilities()
		if c2["source_image_role"] != "style" || c2["creative_controls"].([]any)[0] != "creativity" {
			t.Fatal(c2)
		}
	})
	t.Run("test_capabilities_describe_the_stored_model_the_dispatcher_sends", func(t *testing.T) {
		env, f := kreaTestEnv(t)
		t.Setenv("KREA_IMAGE_MODEL", "krea-2-large")
		p := newKreaImageGenProvider()
		p.env = func() *tools.Env { return env }
		prev := kreaLoadConfig
		t.Cleanup(func() { kreaLoadConfig = prev })
		for _, c := range []struct {
			provider string
			gateway  func(*tools.Env) *tools.ManagedToolGatewayConfig
		}{{"nous", kreaManagedConfig(f.srv.URL)}, {"krea", func(*tools.Env) *tools.ManagedToolGatewayConfig { return nil }}} {
			kreaLoadConfig = func(*tools.Env) map[string]any { return map[string]any{"provider": c.provider, "model": "krea-3"} }
			useKreaManaged(t, c.gateway)
			if got := p.Capabilities()["creative_controls"]; !reflect.DeepEqual(got, []any{"thinking", "resolution"}) {
				t.Fatal(c.provider, got)
			}
		}
	})
}

func TestKreaFallbackToFalEnabled(t *testing.T) {
	for _, c := range []struct {
		cfg  config.Tree
		want bool
	}{
		{config.Tree{}, true},
		{config.Tree{"image_gen": map[string]any{"krea": map[string]any{"fallback_to_fal": false}}}, false},
		{config.Tree{"image_gen": map[string]any{"krea": map[string]any{"fallback_to_fal": "false"}}}, true},
		{config.Tree{"image_gen": map[string]any{"krea": map[string]any{"fallback_to_fal": nil}}}, true},
		{config.Tree{"image_gen": map[string]any{"krea": "off"}}, true},
	} {
		if got := kreaFallbackToFalEnabled(imageEnv(t, c.cfg)); got != c.want {
			t.Fatalf("%v -> %v", c.cfg, got)
		}
	}
}

func TestResolveManagedKreaGatewaySelection(t *testing.T) {
	sel := func(provider string) *tools.Env {
		if provider == "" {
			return imageEnv(t, config.Tree{})
		}
		return imageEnv(t, config.Tree{"image_gen": map[string]any{"provider": provider}})
	}
	t.Setenv("KREA_GATEWAY_URL", "https://krea-gateway.example.com")
	t.Setenv("TOOL_GATEWAY_USER_TOKEN", "managed-token")
	t.Setenv("KREA_API_KEY", "")
	useNousAccount(t, true)
	if gw := resolveManagedKreaGateway(sel("nous")); gw == nil || gw.Vendor != "krea" || gw.GatewayOrigin != "https://krea-gateway.example.com" || gw.NousUserToken != "managed-token" {
		t.Fatalf("nous selection %+v", gw)
	}
	if gw := resolveManagedKreaGateway(sel("")); gw == nil {
		t.Fatal("unconfigured without a key stays managed")
	}
	if gw := resolveManagedKreaGateway(sel("krea")); gw != nil {
		t.Fatal("explicit vendor selection pins direct")
	}
	t.Setenv("KREA_API_KEY", "direct")
	if gw := resolveManagedKreaGateway(sel("")); gw != nil {
		t.Fatal("direct key wins when unconfigured")
	}
	if gw := resolveManagedKreaGateway(sel("nous")); gw == nil {
		t.Fatal("nous selection keeps the gateway despite a direct key")
	}
	useNousAccount(t, false)
	if gw := resolveManagedKreaGateway(sel("nous")); gw != nil {
		t.Fatal("unentitled account has no gateway")
	}
}

func TestKreaSubmitErrorMessage(t *testing.T) {
	for body, want := range map[string]string{
		`{"error": {"message": "bad key"}}`: "bad key",
		`{"message": "m", "detail": "d"}`:   "m",
		`{"detail": "d"}`:                   "d",
		`{"error": "nope"}`:                 `{"error": "nope"}`,
		`["x"]`:                             `["x"]`,
		"not json":                          "not json",
		strings.Repeat("y", 400):            strings.Repeat("y", 300),
	} {
		if got := kreaSubmitErrorMessage(400, []byte(body)); got != want {
			t.Fatalf("%q -> %q", body, got)
		}
	}
}

func TestKreaModelIDsExportCopiesTheCatalog(t *testing.T) {
	ids := KreaModelIDs()
	if len(ids) == 0 || len(ids) != len(kreaModelIDs) {
		t.Fatalf("ids %v", ids)
	}
	for id := range ids {
		delete(ids, id)
		break
	}
	if len(KreaModelIDs()) != len(kreaModelIDs) {
		t.Fatal("export shares the package set")
	}
}
