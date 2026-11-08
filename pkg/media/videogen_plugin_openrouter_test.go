package media

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

func orVeo() map[string]any {
	return map[string]any{"id": "google/veo-3.1", "name": "Google: Veo 3.1", "supported_durations": []any{4, 6, 8},
		"supported_resolutions": []any{"720p", "1080p", "4K"}, "supported_aspect_ratios": []any{"16:9", "9:16"},
		"supported_frame_images": []any{"first_frame", "last_frame"}, "generate_audio": true, "seed": true,
		"pricing_skus": map[string]any{"duration_seconds_with_audio": "0.40", "duration_seconds_without_audio": "0.20"}}
}

func orHailuo() map[string]any {
	d := []any{}
	for i := 5; i < 16; i++ {
		d = append(d, i)
	}
	return map[string]any{"id": "minimax/hailuo-3-max", "name": "MiniMax: Hailuo 3 Max", "supported_durations": d,
		"supported_resolutions": []any{"768p", "480p"}, "supported_aspect_ratios": []any{"21:9", "16:9", "4:3", "1:1", "3:4", "9:16"},
		"supported_frame_images": []any{"first_frame"}, "generate_audio": false, "seed": false,
		"pricing_skus": map[string]any{"duration_seconds_480p": "0.05", "duration_seconds_768p": "0.08"}}
}

func orEdit() map[string]any {
	return map[string]any{"id": "black-forest-labs/flux-video-edit", "name": "FLUX Video Edit", "supported_durations": nil,
		"supported_resolutions": nil, "supported_aspect_ratios": nil, "supported_frame_images": nil,
		"generate_audio": false, "seed": false, "pricing_skus": map[string]any{"cents_per_second_output": "3"}}
}

// Provider with a fixed catalog and configured model
func orProvider(catalog []map[string]any, configured string) *openRouterVideoGenProvider {
	p := newOpenRouterVideoGenProvider()
	p.catalog = func(context.Context) []map[string]any { return catalog }
	p.configured = func(context.Context) string { return configured }
	return p
}

func TestCatalogDrivesPickerRowsAndSelectedModelCapabilities(t *testing.T) {
	p := orProvider([]map[string]any{orVeo(), orHailuo(), orEdit()}, "google/veo-3.1")
	rows := p.ListModels()
	if len(rows) != 2 || rows[0]["id"] != "google/veo-3.1" || rows[1]["id"] != "minimax/hailuo-3-max" {
		t.Fatal(rows)
	}
	if rows[0]["price"] != "$0.20–0.40/s" || rows[1]["max_duration"] != 15 {
		t.Fatal(rows[0]["price"], rows[1]["max_duration"])
	}
	veo := p.Capabilities()
	if veo["supports_audio"] != true || veo["supports_seed"] != true || !reflect.DeepEqual(veo["resolutions"], []any{"720p", "1080p", "4K"}) {
		t.Fatal(veo)
	}
	p.configured = func(context.Context) string { return "minimax/hailuo-3-max" }
	hailuo := p.Capabilities()
	if hailuo["supports_audio"] != false || hailuo["supports_seed"] != false || hailuo["max_duration"] != 15 {
		t.Fatal(hailuo)
	}
}

func TestPayloadClampsToModelLimitsAndDropsUnsupportedToggles(t *testing.T) {
	ninetyNine, seven, five, yes, no := 99, 7, 5, true, false
	payload := pyjson.Plain(openRouterBuildPayload(orHailuo(), openRouterPayloadInput{Model: "minimax/hailuo-3-max", Prompt: "p",
		ImageURL: "https://x/a.png", ReferenceImageURLs: []string{"https://x/r.png"}, Duration: &ninetyNine, AspectRatio: "2:3",
		Resolution: "720p", Audio: &yes, Seed: &seven})).(map[string]any)
	if payload["duration"] != 15 || payload["resolution"] != "768p" || payload["aspect_ratio"] != "3:4" {
		t.Fatal(payload)
	}
	if payload["frame_images"].([]any)[0].(map[string]any)["frame_type"] != "first_frame" {
		t.Fatal(payload["frame_images"])
	}
	want := []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://x/r.png"}}}
	if !reflect.DeepEqual(payload["input_references"], want) {
		t.Fatal(payload["input_references"])
	}
	if _, ok := payload["generate_audio"]; ok {
		t.Fatal("generate_audio sent to Hailuo")
	}
	if _, ok := payload["seed"]; ok {
		t.Fatal("seed sent to Hailuo")
	}
	veo := pyjson.Plain(openRouterBuildPayload(orVeo(), openRouterPayloadInput{Model: "google/veo-3.1", Prompt: "p",
		Duration: &five, AspectRatio: "16:9", Resolution: "1080p", Audio: &no, Seed: &seven})).(map[string]any)
	if veo["duration"] != 4 || veo["generate_audio"] != false || veo["seed"] != 7 {
		t.Fatal(veo)
	}
}

type orCall struct {
	url     string
	headers http.Header
	json    map[string]any
}

// Transport double recording submits and polls and answering them from a queue
type orSession struct {
	mu    sync.Mutex
	posts []orCall
	gets  []orCall
	polls []string
}

func newORSession() *orSession {
	return &orSession{polls: []string{`{"id": "job-1", "status": "in_progress"}`,
		`{"id": "job-1", "status": "completed", "unsigned_urls": ["https://evil.example/steal"], "usage": {"cost": 0.4}}`}}
}

func (s *orSession) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reply := func(status int, body string) *http.Response {
		return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(body)), Request: r}
	}
	if r.Method == http.MethodPost {
		raw, _ := io.ReadAll(r.Body)
		parsed, _ := pyjson.Loads(string(raw))
		m, _ := pyjson.Plain(parsed).(map[string]any)
		s.posts = append(s.posts, orCall{url: r.URL.String(), headers: r.Header.Clone(), json: m})
		return reply(202, `{"id": "job-1", "polling_url": "`+r.URL.String()+`/job-1", "status": "pending"}`), nil
	}
	s.gets = append(s.gets, orCall{url: r.URL.String(), headers: r.Header.Clone()})
	body := s.polls[0]
	s.polls = s.polls[1:]
	return reply(200, body), nil
}

type orSaved struct {
	url  string
	opts videoURLOptions
}

// Runs generate over the fake transport, returning the result, session and every bearer sent
func generateCapturing(t *testing.T, ctx context.Context, p *openRouterVideoGenProvider, req VideoGenRequest) (map[string]any, *orSession, []orSaved, []string) {
	t.Helper()
	session := newORSession()
	p.session = func() *http.Client { return &http.Client{Transport: session} }
	p.sleep = func(context.Context, time.Duration) error { return nil }
	var saved []orSaved
	clip := filepath.Join(t.TempDir(), "clip.mp4")
	prev := saveURLVideo
	saveURLVideo = func(_ context.Context, _ *tools.Env, rawURL string, o videoURLOptions) (string, error) {
		saved = append(saved, orSaved{rawURL, o})
		return clip, nil
	}
	t.Cleanup(func() { saveURLVideo = prev })
	res, err := p.Generate(ctx, nil, req)
	if err != nil {
		t.Fatal(err)
	}
	var bearers []string
	for _, c := range append(append([]orCall{}, session.posts...), session.gets...) {
		bearers = append(bearers, c.headers.Get("Authorization"))
	}
	for _, s := range saved {
		bearers = append(bearers, s.opts.Headers["Authorization"])
	}
	return res, session, saved, bearers
}

// Temp home with no OpenRouter key or base URL in the process environment
func orIsolation(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	for _, k := range []string{"OPENROUTER_API_KEY", "OPENROUTER_BASE_URL", "OPENAI_API_KEY", "OPENAI_BASE_URL"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	return h
}

func TestGenerateSubmitsPollsAndDownloadsFromConfiguredOrigin(t *testing.T) {
	orIsolation(t)
	t.Setenv("OPENROUTER_API_KEY", "sk-or-test")
	p := orProvider([]map[string]any{orVeo()}, "google/veo-3.1")
	six, yes := 6, true
	res, session, saved, _ := generateCapturing(t, context.Background(), p,
		VideoGenRequest{Prompt: "a fox", Duration: &six, Resolution: "1080p", AspectRatio: "16:9", Audio: &yes})
	if res["success"] != true {
		t.Fatal(res)
	}
	if !strings.HasSuffix(res["video"].(string), "clip.mp4") || res["cost"] != 0.4 || res["duration"] != 6 {
		t.Fatal(res)
	}
	post := session.posts[0]
	if post.url != "https://openrouter.ai/api/v1/videos" || post.json["model"] != "google/veo-3.1" || post.json["generate_audio"] != true {
		t.Fatal(post.url, post.json)
	}
	if post.headers.Get("Authorization") != "Bearer sk-or-test" {
		t.Fatal(post.headers)
	}
	if len(session.gets) != 2 || session.gets[0].url != "https://openrouter.ai/api/v1/videos/job-1" || session.gets[1].url != session.gets[0].url {
		t.Fatal(session.gets)
	}
	if saved[0].url != "https://openrouter.ai/api/v1/videos/job-1/content" {
		t.Fatal(saved[0].url)
	}
	if saved[0].opts.Headers["Authorization"] != "Bearer sk-or-test" || !saved[0].opts.RequireVideoContentType || !saved[0].opts.TrustedOrigin {
		t.Fatal(saved[0].opts)
	}
}

// Adds an api key credential to the openrouter pool like icebath auth add openrouter
func addPooledKey(t *testing.T, h, key, label, id string) {
	t.Helper()
	env := provider.PoolEnv{Store: provider.NewStore(h), Secrets: config.NewSecrets(h), Config: provider.ConfigLoader(h)}
	_, err := provider.LoadPool(env, "openrouter").AddEntry(provider.PooledCredential{Provider: "openrouter", ID: id, Label: label,
		AuthType: provider.AuthTypeAPIKey, Source: provider.SourceManual, AccessToken: key, BaseURL: "https://openrouter.ai/api/v1"})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPooledCredentialEnablesTheBackend(t *testing.T) {
	h := orIsolation(t)
	addPooledKey(t, h, "sk-or-pool", "pool", "a1b2c3")
	p := orProvider([]map[string]any{orVeo()}, "google/veo-3.1")
	if !p.IsAvailable() {
		t.Fatal("pooled key not seen")
	}
	res, _, _, bearers := generateCapturing(t, context.Background(), p, VideoGenRequest{Prompt: "a fox"})
	if res["success"] != true {
		t.Fatal(res)
	}
	if !reflect.DeepEqual(bearers, []string{"Bearer sk-or-pool", "Bearer sk-or-pool", "Bearer sk-or-pool", "Bearer sk-or-pool"}) {
		t.Fatal(bearers)
	}
}

func TestOneJobKeepsOneCredentialWhileThePoolRotates(t *testing.T) {
	h := orIsolation(t)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("credential_pool_strategies:\n  openrouter: round_robin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	addPooledKey(t, h, "sk-or-one", "one", "aaaaaa")
	addPooledKey(t, h, "sk-or-two", "two", "bbbbbb")
	res, _, _, bearers := generateCapturing(t, context.Background(), orProvider([]map[string]any{orVeo()}, "google/veo-3.1"), VideoGenRequest{Prompt: "a fox"})
	if res["success"] != true {
		t.Fatal(res)
	}
	if len(bearers) != 4 || (bearers[0] != "Bearer sk-or-one" && bearers[0] != "Bearer sk-or-two") {
		t.Fatal(bearers)
	}
	for _, b := range bearers {
		if b != bearers[0] {
			t.Fatal(bearers)
		}
	}
}

// Context routed to the profile home with its own secret scope while multiplexing is active
func routedProfile(t *testing.T, profileHome string) context.Context {
	t.Helper()
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	ctx := home.WithOverride(context.Background(), profileHome)
	return provider.WithSecretScope(ctx, provider.BuildProfileSecretScope(profileHome), profileHome)
}

func TestMultiplexedProfileSpendsItsOwnKeyNotTheLaunchProfiles(t *testing.T) {
	root := orIsolation(t)
	t.Setenv("OPENROUTER_API_KEY", "sk-or-launch-profile")
	t.Setenv("OPENROUTER_BASE_URL", "https://launch.example/api/v1")
	profileHome := filepath.Join(root, "profile-b")
	os.MkdirAll(profileHome, 0o755)
	os.WriteFile(filepath.Join(profileHome, ".env"), []byte("OPENROUTER_API_KEY=sk-or-profile-b\nOPENROUTER_BASE_URL=https://profile-b.example/api/v1\n"), 0o600)
	p := orProvider([]map[string]any{orVeo()}, "google/veo-3.1")
	res, session, _, bearers := generateCapturing(t, routedProfile(t, profileHome), p, VideoGenRequest{Prompt: "a fox"})
	if res["success"] != true {
		t.Fatal(res)
	}
	if !reflect.DeepEqual(bearers, []string{"Bearer sk-or-profile-b", "Bearer sk-or-profile-b", "Bearer sk-or-profile-b", "Bearer sk-or-profile-b"}) {
		t.Fatal(bearers)
	}
	if session.posts[0].url != "https://profile-b.example/api/v1/videos" {
		t.Fatal(session.posts[0].url)
	}
}

func TestMultiplexedProfileWithoutAKeyIsRefusedNotServedOnTheLaunchKey(t *testing.T) {
	root := orIsolation(t)
	t.Setenv("OPENROUTER_API_KEY", "sk-or-launch-profile")
	profileHome := filepath.Join(root, "profile-nokey")
	os.MkdirAll(profileHome, 0o755)
	os.WriteFile(filepath.Join(profileHome, ".env"), nil, 0o600)
	p := orProvider([]map[string]any{orVeo()}, "google/veo-3.1")
	ctx := routedProfile(t, profileHome)
	available := p.availableIn(ctx)
	res, session, _, bearers := generateCapturing(t, ctx, p, VideoGenRequest{Prompt: "a fox"})
	if available {
		t.Fatal("routed profile without a key reported available")
	}
	if res["success"] != false || res["error_type"] != "missing_credentials" {
		t.Fatal(res)
	}
	if len(session.posts) != 0 || len(bearers) != 0 {
		t.Fatal(session.posts, bearers)
	}
}

func TestGenerateRejectsLocalImagePathsBeforeSpending(t *testing.T) {
	orIsolation(t)
	t.Setenv("OPENROUTER_API_KEY", "sk-or-test")
	p := orProvider([]map[string]any{orVeo()}, "google/veo-3.1")
	p.session = func() *http.Client { t.Fatal("must not submit"); return nil }
	res, err := p.Generate(context.Background(), nil, VideoGenRequest{Prompt: "p", ImageURL: "/home/me/frame.png"})
	if err != nil || res["success"] != false || res["error_type"] != "invalid_request" {
		t.Fatal(res, err)
	}
}
