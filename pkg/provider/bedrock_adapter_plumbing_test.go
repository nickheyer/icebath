package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Records every log record with its level
type bcaLogs struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *bcaLogs) Enabled(context.Context, slog.Level) bool { return true }
func (c *bcaLogs) Handle(_ context.Context, rec slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, rec)
	return nil
}
func (c *bcaLogs) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *bcaLogs) WithGroup(string) slog.Handler      { return c }

func (c *bcaLogs) warnings() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, r := range c.records {
		if r.Level == slog.LevelWarn {
			out = append(out, r.Message)
		}
	}
	return out
}

// Captures the Bedrock logger for one test
func bcaCaptureBedrockLog(t *testing.T) *bcaLogs {
	logs := &bcaLogs{}
	prev := bedrockLog
	bedrockLog = slog.New(logs)
	t.Cleanup(func() { bedrockLog = prev })
	return logs
}

// Bedrock control plane fake with static keys, the client and discovery caches reset around the test
func bcaControlServer(t *testing.T, handler http.HandlerFunc) *[]*http.Request {
	bedrockIsolatedEnv(t)
	var mu sync.Mutex
	seen := &[]*http.Request{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		*seen = append(*seen, r.Clone(context.Background()))
		mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("AWS_ENDPOINT_URL_BEDROCK", srv.URL)
	t.Setenv("AWS_ENDPOINT_URL_BEDROCK_RUNTIME", srv.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", bedrockFakeAccessKey)
	t.Setenv("AWS_SECRET_ACCESS_KEY", bedrockFakeSecretKey)
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	reset := func() {
		ResetBedrockClientCache()
		ResetBedrockDiscoveryCache()
	}
	reset()
	t.Cleanup(reset)
	return seen
}

func bcaJSON(w http.ResponseWriter, status int, errType, body string) {
	w.Header().Set("Content-Type", "application/json")
	if errType != "" {
		w.Header().Set("X-Amzn-ErrorType", errType)
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// Context of a profile home with its own secret scope under active multiplexing
func bcaProfileCtx(t *testing.T, dir string) context.Context {
	ctx := home.WithOverride(context.Background(), dir)
	return WithSecretScope(ctx, BuildProfileSecretScope(dir), dir)
}

// Context of a profile home whose secret scope holds no AWS credential, refused under multiplexing
func bcaRefusedCtx(dir string) context.Context {
	return WithSecretScope(home.WithOverride(context.Background(), dir), map[string]string{}, dir)
}

func bcaMultiplex(t *testing.T) {
	SetMultiplexActive(true)
	t.Cleanup(func() { SetMultiplexActive(false) })
}

func bcaNoChain(t *testing.T) {
	prev := awsChainHasCredentialsFn
	awsChainHasCredentialsFn = func(bool) bool { return false }
	t.Cleanup(func() { awsChainHasCredentialsFn = prev })
}

func TestBedrockResolveAWSAuthEnvVar(t *testing.T) {
	bcaNoChain(t)
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if got := resolveAWSAuthEnvVarFrom(env(map[string]string{"AWS_ACCESS_KEY_ID": "AKIA..."}), true); got == "AWS_ACCESS_KEY_ID" {
		t.Fatal("an access key without its secret counted")
	}
	if got := resolveAWSAuthEnvVarFrom(env(map[string]string{}), true); got != "" {
		t.Fatalf("no auth resolved %q", got)
	}
}

func TestBedrockHasAWSCredentials(t *testing.T) {
	bedrockIsolatedEnv(t)
	bcaNoChain(t)
	if hasAWSCredentials(true) {
		t.Fatal("empty env has credentials")
	}
	t.Setenv("AWS_PROFILE", "default")
	if !hasAWSCredentials(true) {
		t.Fatal("AWS_PROFILE not counted")
	}
}

func TestBedrockTwoHomesMultiplexRefusesAmbientChainAndKeepsStandalone(t *testing.T) {
	dir := bedrockIsolatedEnv(t)
	homeA, homeB := filepath.Join(dir, "home-A"), filepath.Join(dir, "home-B")
	for h, env := range map[string]string{homeA: "AWS_ACCESS_KEY_ID=AKIA-A\nAWS_SECRET_ACCESS_KEY=secret-A\nAWS_BEARER_TOKEN_BEDROCK=bearer-A\n", homeB: ""} {
		if err := os.MkdirAll(h, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(h, ".env"), []byte(env), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIA-A")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret-A")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "bearer-A")
	ResetBedrockClientCache()
	t.Cleanup(ResetBedrockClientCache)
	if opts, err := scopedAWSSessionOptions(context.Background()); err != nil || opts != (awsSessionOptions{}) {
		t.Fatalf("standalone %+v %v", opts, err)
	}
	if got := resolveBedrockBearerTokenCtx(context.Background()); got != "bearer-A" {
		t.Fatalf("standalone bearer %q", got)
	}
	bcaMultiplex(t)
	want := awsSessionOptions{AccessKeyID: "AKIA-A", SecretAccessKey: "secret-A"}
	if opts, err := scopedAWSSessionOptions(bcaProfileCtx(t, homeA)); err != nil || opts != want {
		t.Fatalf("home A %+v %v", opts, err)
	}
	_, err := bedrockCachedClient(bcaProfileCtx(t, homeB), "bedrock-runtime", "us-east-1", 0)
	var refused BedrockScopedCredentialError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "refused for this profile") {
		t.Fatalf("home B client %v", err)
	}
	if got := resolveBedrockBearerTokenCtx(bcaProfileCtx(t, homeB)); got != "" {
		t.Fatalf("home B bearer %q", got)
	}
	if opts, err := scopedAWSSessionOptions(bcaProfileCtx(t, homeA)); err != nil || opts != want {
		t.Fatalf("home A again %+v %v", opts, err)
	}
	if got := resolveBedrockBearerTokenCtx(bcaProfileCtx(t, homeA)); got != "bearer-A" {
		t.Fatalf("home A bearer %q", got)
	}
}

func TestBedrockResolveRegion(t *testing.T) {
	bedrockIsolatedEnv(t)
	t.Setenv("AWS_REGION", "eu-west-1")
	t.Setenv("AWS_DEFAULT_REGION", "us-west-2")
	if got := resolveBedrockRegionLive(); got != "eu-west-1" {
		t.Fatal(got)
	}
	bedrockIsolatedEnv(t)
	if got := resolveBedrockRegionLive(); got != "us-east-1" {
		t.Fatal(got)
	}
}

const bcaFoundationModels = `{"modelSummaries": [
	{"modelId": "anthropic.claude-v2", "modelName": "Claude v2", "providerName": "Anthropic", "inputModalities": ["TEXT"],
	 "outputModalities": ["TEXT"], "responseStreamingSupported": true, "modelLifecycle": {"status": "ACTIVE"}},
	{"modelId": "amazon.titan-text", "modelName": "Titan", "providerName": "Amazon", "inputModalities": ["TEXT"],
	 "outputModalities": ["TEXT"], "responseStreamingSupported": true, "modelLifecycle": {"status": "ACTIVE"}}]}`

func bcaDiscoveryHandler(foundation string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/foundation-models" {
			bcaJSON(w, 200, "", foundation)
			return
		}
		bcaJSON(w, 200, "", `{"inferenceProfileSummaries": []}`)
	}
}

func TestBedrockDiscoverModels(t *testing.T) {
	t.Run("provider filter", func(t *testing.T) {
		bcaControlServer(t, bcaDiscoveryHandler(bcaFoundationModels))
		models := DiscoverBedrockModels(context.Background(), "us-east-1", []string{"anthropic"})
		if len(models) != 1 || models[0].ID != "anthropic.claude-v2" {
			t.Fatalf("%+v", models)
		}
	})
	t.Run("caches results", func(t *testing.T) {
		seen := bcaControlServer(t, bcaDiscoveryHandler(`{"modelSummaries": [{"modelId": "test-model", "modelName": "Test", "providerName": "Test",
			"inputModalities": ["TEXT"], "outputModalities": ["TEXT"], "responseStreamingSupported": true, "modelLifecycle": {"status": "ACTIVE"}}]}`))
		first := DiscoverBedrockModels(context.Background(), "us-east-1", nil)
		second := DiscoverBedrockModels(context.Background(), "us-east-1", nil)
		lists := 0
		for _, r := range *seen {
			if r.URL.Path == "/foundation-models" {
				lists++
			}
		}
		if lists != 1 || len(first) != 1 || !reflect.DeepEqual(first, second) {
			t.Fatalf("list calls %d %+v %+v", lists, first, second)
		}
	})
	t.Run("client error", func(t *testing.T) {
		dir := bedrockIsolatedEnv(t)
		ResetBedrockClientCache()
		ResetBedrockDiscoveryCache()
		t.Cleanup(ResetBedrockClientCache)
		bcaMultiplex(t)
		if models := DiscoverBedrockModels(bcaRefusedCtx(dir), "us-east-1", nil); len(models) != 0 || models == nil {
			t.Fatalf("%#v", models)
		}
	})
	t.Run("api error", func(t *testing.T) {
		bcaControlServer(t, func(w http.ResponseWriter, r *http.Request) {
			bcaJSON(w, 403, "AccessDeniedException", `{"message": "No creds"}`)
		})
		if models := DiscoverBedrockModels(context.Background(), "us-east-1", nil); len(models) != 0 || models == nil {
			t.Fatalf("%#v", models)
		}
	})
}

func TestBedrockExtractProviderFromARN(t *testing.T) {
	if got := extractBedrockProviderFromARN("arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-sonnet-4-6"); got != "anthropic" {
		t.Fatal(got)
	}
	if extractBedrockProviderFromARN("not-an-arn") != "" || extractBedrockProviderFromARN("") != "" {
		t.Fatal("invalid ARN yielded a provider")
	}
}

func bcaOpenAIIDForms() []string {
	var out []string
	for _, id := range BedrockOpenAIResponsesModelIDs {
		for _, prefix := range []string{"", "us.", "global."} {
			out = append(out, prefix+id)
		}
	}
	return out
}

func TestBedrockOpenAIClassifier(t *testing.T) {
	for _, id := range append(bcaOpenAIIDForms(), "openai.gpt-6-astra", "openai.gpt-6-sol", "openai.gpt-6-luna", "openai.gpt-6.1-sol") {
		if !isOpenAIBedrockModel(id) {
			t.Errorf("%s is off the OpenAI route", id)
		}
	}
	for _, id := range []string{"openai.gpt-oss-120b", "us.openai.gpt-oss-120b-1:0", "anthropic.claude-opus-5", "us.anthropic.claude-opus-5",
		"openai.gpt-99", "bogus.openai.gpt-6.1-sol", ""} {
		if isOpenAIBedrockModel(id) {
			t.Errorf("%s is on the OpenAI route", id)
		}
	}
}

func bcaForbidProbe(t *testing.T) {
	prev := bedrockProbeFn
	bedrockProbeFn = func(_ context.Context, model, _ string) int { t.Errorf("probe called for %s", model); return 0 }
	t.Cleanup(func() { bedrockProbeFn = prev })
}

func TestBedrockContextLength(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) *bcaLogs {
		bedrockIsolatedEnv(t)
		ResetBedrockClientCache()
		t.Cleanup(ResetBedrockClientCache)
		bcaForbidProbe(t)
		return bcaCaptureBedrockLog(t)
	}
	t.Run("unknown model gets the default", func(t *testing.T) {
		setup(t)
		if got := bedrockContextLength(ctx, "unknown.model-v1:0", "", true); got != BedrockDefaultContextLength {
			t.Fatal(got)
		}
	})
	t.Run("unknown fallback reported once with the override", func(t *testing.T) {
		logs := setup(t)
		for range 2 {
			if got := bedrockContextLength(ctx, "openai.gpt-99", "", false); got != BedrockDefaultContextLength {
				t.Fatal(got)
			}
		}
		w := logs.warnings()
		if len(w) != 1 || !strings.Contains(w[0], "openai.gpt-99") || !strings.Contains(w[0], "model.context_length") || !strings.Contains(w[0], "128,000") {
			t.Fatalf("%q", w)
		}
	})
	t.Run("known model does not warn", func(t *testing.T) {
		logs := setup(t)
		bedrockContextLength(ctx, "us.anthropic.claude-sonnet-4-6", "", false)
		if w := logs.warnings(); len(w) != 0 {
			t.Fatalf("%q", w)
		}
	})
	t.Run("profile prefixed OpenAI ids resolve like the bare id", func(t *testing.T) {
		setup(t)
		for _, id := range bcaOpenAIIDForms() {
			bare := id
			if !strings.HasPrefix(id, "openai.") {
				bare = strings.SplitN(id, ".", 2)[1]
			}
			if a, b := bedrockContextLength(ctx, id, "", false), bedrockContextLength(ctx, bare, "", false); a != b {
				t.Errorf("%s %d vs %s %d", id, a, bare, b)
			}
		}
	})
	t.Run("gpt 6.1 sol profile gets the card window", func(t *testing.T) {
		setup(t)
		if got := bedrockContextLength(ctx, "us.openai.gpt-6.1-sol", "", false); got != 1_000_000 {
			t.Fatal(got)
		}
	})
	t.Run("no region skips the probe and uses the table", func(t *testing.T) {
		setup(t)
		if got := bedrockContextLength(ctx, "anthropic.claude-opus-4-6", "", true); got != 1_000_000 {
			t.Fatal(got)
		}
	})
	t.Run("static catalog Claude ids match their Anthropic window", func(t *testing.T) {
		setup(t)
		for _, id := range staticCatalogModels("bedrock") {
			_, bare, found := strings.Cut(id, "anthropic.")
			if !found {
				continue
			}
			expected := 0
			if hit, ok := longestKeyMatch(defaultContextLengths, bare); ok {
				expected = hit.ctx
			}
			if actual := bedrockContextLength(ctx, id, "", false); actual != expected {
				t.Errorf("%s expected %d actual %d", id, expected, actual)
			}
		}
	})
	t.Run("million token Claude entries match model metadata", func(t *testing.T) {
		setup(t)
		for _, e := range defaultContextLengths {
			if !strings.HasPrefix(e.key, "claude-") || e.ctx < 1_000_000 {
				continue
			}
			if actual := bedrockContextLength(ctx, "anthropic."+strings.ReplaceAll(e.key, ".", "-"), "", false); actual != e.ctx {
				t.Errorf("%s expected %d actual %d", e.key, e.ctx, actual)
			}
		}
	})
	t.Run("OpenAI entries are sized and never exceed the direct API window", func(t *testing.T) {
		setup(t)
		for _, id := range BedrockOpenAIResponsesModelIDs {
			hit, ok := longestKeyMatch(defaultContextLengths, strings.SplitN(id, ".", 2)[1])
			hosted := bedrockContextLength(ctx, id, "", false)
			if !ok || !(BedrockDefaultContextLength < hosted && hosted <= hit.ctx) {
				t.Errorf("%s hosted %d direct %d %v", id, hosted, hit.ctx, ok)
			}
		}
	})
}

const bcaProfileARN = "arn:aws:bedrock:us-west-2:123456789012:application-inference-profile/abcdef123456"

func bcaClearProfileCache(t *testing.T) {
	clearIt := func() {
		bedrockProfileMu.Lock()
		bedrockProfileCache = map[string]string{}
		bedrockProfileMu.Unlock()
	}
	clearIt()
	t.Cleanup(clearIt)
}

func bcaProfileHandler(w http.ResponseWriter, r *http.Request) {
	bcaJSON(w, 200, "", `{"models": [{"modelArn": "arn:aws:bedrock:us-west-2::foundation-model/anthropic.claude-sonnet-4-6"}]}`)
}

func TestBedrockInferenceProfileContextLength(t *testing.T) {
	ctx := context.Background()
	t.Run("ARN resolves the wrapped model window in the ARN region", func(t *testing.T) {
		seen := bcaControlServer(t, bcaProfileHandler)
		bcaClearProfileCache(t)
		t.Setenv("AWS_REGION", "us-east-1")
		for range 2 {
			if got := bedrockContextLength(ctx, bcaProfileARN, "", false); got != 1_000_000 {
				t.Fatal(got)
			}
		}
		if len(*seen) != 1 {
			t.Fatalf("GetInferenceProfile calls %d", len(*seen))
		}
		r := (*seen)[0]
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/inference-profiles/"+bedrockEscapeModelID(bcaProfileARN) ||
			!strings.Contains(r.Header.Get("Authorization"), "/us-west-2/bedrock/aws4_request") {
			t.Fatalf("%s %s %s", r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization"))
		}
	})
	t.Run("denied resolution falls back to the default with one warning", func(t *testing.T) {
		bcaControlServer(t, func(w http.ResponseWriter, r *http.Request) {
			bcaJSON(w, 403, "AccessDeniedException", `{"message": "AccessDeniedException"}`)
		})
		bcaClearProfileCache(t)
		logs := bcaCaptureBedrockLog(t)
		if got := bedrockContextLength(ctx, bcaProfileARN, "", false); got != BedrockDefaultContextLength {
			t.Fatal(got)
		}
		w := logs.warnings()
		if len(w) != 1 || !strings.Contains(w[0], bcaProfileARN) || !strings.Contains(w[0], "GetInferenceProfile") {
			t.Fatalf("%q", w)
		}
	})
	t.Run("profile wrapping Claude keeps prompt cache markers", func(t *testing.T) {
		bcaControlServer(t, bcaProfileHandler)
		bcaClearProfileCache(t)
		bcResetCachePoints(t)
		kwargs := buildBedrockConverseKwargs(newBedrockConverseInput(bcaProfileARN, bcMaps(t, `[{"role": "system", "content": "Be helpful."}, {"role": "user", "content": "Hi"}]`)))
		system := kwargs["system"].([]any)
		if kwargs["modelId"] != bcaProfileARN {
			t.Fatal(kwargs["modelId"])
		}
		bcSameJSON(t, system[len(system)-1], `{"cachePoint": {"type": "default"}}`)
	})
}

func TestBedrockContextProbe(t *testing.T) {
	t.Run("client unavailable", func(t *testing.T) {
		dir := bedrockIsolatedEnv(t)
		ResetBedrockClientCache()
		t.Cleanup(ResetBedrockClientCache)
		bcaMultiplex(t)
		if got := probeBedrockContextLengthLive(bcaRefusedCtx(dir), "any.model", "eu-central-1"); got != 0 {
			t.Fatal(got)
		}
	})
	t.Run("probe result beats the static table", func(t *testing.T) {
		seen := bcaControlServer(t, func(w http.ResponseWriter, r *http.Request) {
			bcaJSON(w, 400, "ValidationException", `{"message": "prompt is too long: 5000032 tokens > 1000000 maximum"}`)
		})
		if got := bedrockContextLength(context.Background(), "eu.anthropic.claude-opus-4-8", "eu-central-1", true); got != 1_000_000 {
			t.Fatal(got)
		}
		if len(*seen) != 1 || !strings.HasSuffix((*seen)[0].URL.Path, "/converse") {
			t.Fatalf("probe requests %d", len(*seen))
		}
	})
}

func TestBedrockIsAnthropicModel(t *testing.T) {
	for id, want := range map[string]bool{"us.anthropic.claude-sonnet-4-6": true, "us.amazon.nova-pro-v1:0": false,
		"au.anthropic.claude-haiku-4-5-20251001-v1:0": true, "au.anthropic.claude-sonnet-4-6": true} {
		if isAnthropicBedrockModel(id) != want {
			t.Errorf("%s want %v", id, want)
		}
	}
}

func TestBedrockInvalidateRuntimeClient(t *testing.T) {
	bedrockIsolatedEnv(t)
	ResetBedrockClientCache()
	t.Cleanup(ResetBedrockClientCache)
	ctx := context.Background()
	east, _ := bedrockCachedClient(ctx, "bedrock-runtime", "us-east-1", 0)
	east300, _ := bedrockCachedClient(ctx, "bedrock-runtime", "us-east-1", 300e9)
	west, _ := bedrockCachedClient(ctx, "bedrock-runtime", "us-west-2", 0)
	if east == nil || east300 == nil || west == nil || east == east300 {
		t.Fatal("clients not cached per region and timeout")
	}
	if !InvalidateBedrockRuntimeClient(ctx, "us-east-1") {
		t.Fatal("nothing evicted")
	}
	bedrockClientsMu.Lock()
	for k := range bedrockClients {
		if k.region == "us-east-1" {
			t.Errorf("us-east-1 kept %+v", k)
		}
	}
	kept := bedrockClients[bedrockClientKey{service: "bedrock-runtime", region: "us-west-2"}]
	bedrockClientsMu.Unlock()
	if kept != west {
		t.Fatal("us-west-2 client evicted")
	}
	ResetBedrockClientCache()
	if InvalidateBedrockRuntimeClient(ctx, "eu-west-1") {
		t.Fatal("evicted an uncached region")
	}
}

func TestBedrockIsStaleConnectionError(t *testing.T) {
	if !isBedrockStaleConnectionError(&AWSReadTimeoutError{}) {
		t.Fatal("read timeout not stale")
	}
	for _, err := range []error{io.ErrUnexpectedEOF, net.ErrClosed, &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}} {
		if !isBedrockStaleConnectionError(err) {
			t.Errorf("pool internal failure %v not stale", err)
		}
	}
	for _, err := range []error{errors.New("bad input"), errors.New("'missing'")} {
		if isBedrockStaleConnectionError(err) {
			t.Errorf("%v counted stale", err)
		}
	}
}

func TestBedrockStreamingAccessDeniedDetection(t *testing.T) {
	denied := &AWSServiceError{Service: "bedrock-runtime", Operation: "ConverseStream", StatusCode: 403, Code: "AccessDeniedException",
		Message: "User: arn:aws:iam::123456789012:user/x is not authorized to perform: bedrock:InvokeModelWithResponseStream on resource: " +
			"arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-3-sonnet-20240229-v1:0"}
	if !isBedrockStreamingAccessDenied(denied) {
		t.Fatal("denial not detected")
	}
	if isBedrockStreamingAccessDenied(errors.New("boom")) || isBedrockStreamingAccessDenied(errors.New("stream not supported")) {
		t.Fatal("unrelated error detected")
	}
}

func TestBedrockBearerTokenRoutesToConverse(t *testing.T) {
	resolve := func(t *testing.T, bearer bool) *Runtime {
		bedrockIsolatedEnv(t)
		r := testResolver(t, map[string]any{"model": map[string]any{"default": "us.anthropic.claude-sonnet-4-6", "provider": "bedrock"}, "bedrock": map[string]any{}}, nil, "")
		t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
		if bearer {
			t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "test-bearer-token-123")
		}
		t.Setenv("AWS_DEFAULT_REGION", "us-east-1")
		return mustResolve(t, r, "bedrock", "", "", "")
	}
	t.Run("bearer forces converse for Claude", func(t *testing.T) {
		if rt := resolve(t, true); rt.APIMode != modeBedrockConverse || rt.BedrockAnthropic {
			t.Fatalf("%+v", rt)
		}
	})
	t.Run("SigV4 Claude keeps the AnthropicBedrock path", func(t *testing.T) {
		if rt := resolve(t, false); rt.APIMode != ModeAnthropicMessages || !rt.BedrockAnthropic {
			t.Fatalf("%+v", rt)
		}
	})
}

func TestBedrockAnthropicMessagesPreservesModelIDs(t *testing.T) {
	for _, tc := range []struct {
		route Route
		want  bool
	}{{Route{Provider: "bedrock", Model: "global.anthropic.claude-opus-4-7", Mode: ModeAnthropicMessages}, true},
		{Route{Provider: "custom", BaseURL: "https://s3.us-east-1.amazonaws.com", Model: "claude-opus-4-7", Mode: ModeAnthropicMessages, APIKey: "k"}, false}} {
		client, err := newAnthropicClient(tc.route)
		if err != nil {
			t.Fatal(err)
		}
		if got := client.(*anthropicClient).preserveDots(); got != tc.want {
			t.Errorf("%s %s preserve dots %v", tc.route.Provider, tc.route.BaseURL, got)
		}
	}
	for _, id := range []string{"global.anthropic.claude-opus-4-7", "anthropic.claude-opus-4-7"} {
		if got := normalizeModelName(id, false); got != id {
			t.Errorf("%s normalized to %s", id, got)
		}
	}
	kwargs, err := anthBuildKwargs(anthBuildInput{model: "global.anthropic.claude-opus-4-7", messages: bcMaps(t, `[{"role": "user", "content": "hi"}]`), maxTokens: 1024})
	if err != nil || kwargs["model"] != "global.anthropic.claude-opus-4-7" {
		t.Fatalf("%v %v", kwargs["model"], err)
	}
}

// Anthropic SDK 400 as classify_api_error sees it, the str of the exception carrying the body repr
func bcaAnthropic400(message string, body map[string]any) ClassifyInput {
	if body == nil {
		body = map[string]any{"message": message}
	}
	data, _ := json.Marshal(body)
	return ClassifyInput{Err: errors.New("Error code: 400 - " + bedrockPyRepr(body)), Status: 400, Body: string(data),
		Provider: "bedrock", Model: "global.anthropic.claude-opus-5-5"}
}

func TestBedrockInvalidRedactedThinkingDataClassifiesAsThinkingSignature(t *testing.T) {
	rejection := "messages.3.content.10: Invalid `data` in `redacted_thinking` block"
	for name, body := range map[string]map[string]any{"bedrock-body": nil,
		"anthropic-body": {"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": rejection}}} {
		got := ClassifyError(bcaAnthropic400(rejection, body))
		if got.Reason != ReasonThinkingSignature || !got.Retryable {
			t.Errorf("%s classified %s retryable %v", name, got.Reason, got.Retryable)
		}
	}
}

func TestBedrockNeighbouring400sKeepTheirClass(t *testing.T) {
	for message, want := range map[string]FailoverReason{
		"messages.1.content.0: Invalid `signature` in `thinking` block": ReasonThinkingSignature,
		"thinking_signature_invalid: redacted_thinking invalid":         ReasonInvalidEncryptedContent,
		"messages.1.content.0: Invalid `data` in `image` block":         ReasonFormatError,
	} {
		if got := ClassifyError(bcaAnthropic400(message, nil)).Reason; got != want {
			t.Errorf("%q classified %s want %s", message, got, want)
		}
	}
}
