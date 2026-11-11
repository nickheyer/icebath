package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Round tripper that answers every request from a function
type doctorRoundTrip func(*http.Request) (*http.Response, error)

func (f doctorRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Response of a status with a text body for a fake round trip
func doctorHTTPResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), Proto: "HTTP/1.1",
		ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

// Doctor over a fresh home holding the given config.yaml body under a current version stamp
func doctorTestDoctor(t *testing.T, configBody string) (*Doctor, *bytes.Buffer) {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	if configBody != "" {
		seed := fmt.Sprintf("_config_version: %d\n%s", config.LatestConfigVersion(), configBody)
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(seed), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", h)
	config.ResetCache()
	config.InvalidateEnvCache()
	t.Cleanup(func() {
		config.ResetCache()
		config.InvalidateEnvCache()
	})
	out := &bytes.Buffer{}
	return &Doctor{W: out, Home: h, ProjectRoot: t.TempDir(), DisplayHome: h}, out
}

const doctorAzureBase = "https://res.services.ai.azure.com/anthropic"

type doctorSeenCall struct {
	Method, URL, Query string
	Header             http.Header
	Body               map[string]any
}

// Runs the Azure Foundry API key probe against a fake transport answering POST with a status and GET with 404
func doctorRunFoundryProbe(t *testing.T, status int, baseURLInEnv bool) (doctorProbeResult, []doctorSeenCall) {
	t.Helper()
	d, _ := doctorTestDoctor(t, "model:\n  provider: azure-foundry\n  base_url: "+doctorAzureBase+"\n  default: claude-sonnet-5\n")
	t.Setenv("AZURE_FOUNDRY_API_KEY", "k")
	if baseURLInEnv {
		t.Setenv("AZURE_FOUNDRY_BASE_URL", doctorAzureBase)
	} else {
		doctorUnsetEnv(t, "AZURE_FOUNDRY_BASE_URL")
	}
	var mu sync.Mutex
	var calls []doctorSeenCall
	e := newDoctorProbeEnv(context.Background(), d)
	e.client = &http.Client{Transport: doctorRoundTrip(func(r *http.Request) (*http.Response, error) {
		call := doctorSeenCall{Method: r.Method, URL: r.URL.Scheme + "://" + r.URL.Host + r.URL.Path, Query: r.URL.RawQuery, Header: r.Header}
		if r.Body != nil {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &call.Body)
		}
		mu.Lock()
		calls = append(calls, call)
		mu.Unlock()
		if r.Method == http.MethodPost {
			return doctorHTTPResponse(r, status, `{"id":"msg_1"}`), nil
		}
		return doctorHTTPResponse(r, 404, ""), nil
	})}
	res, err := e.probeAPIKeyProvider(doctorAPIKeyProvider{"Azure Foundry", []string{"AZURE_FOUNDRY_API_KEY", "AZURE_FOUNDRY_BASE_URL"}, "", "AZURE_FOUNDRY_BASE_URL", true})
	if err != nil {
		t.Fatal(err)
	}
	return res, calls
}

func TestAnthropicStyleFoundryProbePostsMessagesLikeTheRuntime(t *testing.T) {
	for _, status := range []int{200, 400} {
		res, calls := doctorRunFoundryProbe(t, status, true)
		if len(res.Issues) != 0 || !strings.Contains(res.Lines[0].Glyph, "✓") {
			t.Fatalf("status %d: %#v", status, res)
		}
		if len(calls) != 1 || calls[0].Method != "POST" {
			t.Fatalf("calls = %#v", calls)
		}
		c := calls[0]
		if c.URL != doctorAzureBase+"/v1/messages" || c.Query != "api-version=2025-04-15" {
			t.Fatalf("url = %q query = %q", c.URL, c.Query)
		}
		if c.Header.Get("Authorization") != "Bearer k" || c.Header.Get("x-api-key") != "" || c.Header.Get("anthropic-version") != "2023-06-01" {
			t.Fatalf("headers = %v", c.Header)
		}
		if c.Body["max_tokens"] != float64(1) || c.Body["model"] != "claude-sonnet-5" {
			t.Fatalf("body = %v", c.Body)
		}
	}
}

func TestFoundryBaseUrlFallsBackToConfigAnd404StaysVisible(t *testing.T) {
	res, calls := doctorRunFoundryProbe(t, 200, false)
	if len(calls) == 0 || calls[0].URL != doctorAzureBase+"/v1/messages" || len(res.Issues) != 0 {
		t.Fatalf("calls = %#v res = %#v", calls, res)
	}
	res, _ = doctorRunFoundryProbe(t, 404, true)
	if !strings.Contains(res.Lines[0].Detail, "HTTP 404") {
		t.Fatalf("res = %#v", res)
	}
}

type doctorGitHubSeen struct{ Path, Authorization string }

// Serves the GitHub probe endpoint with a fixed status, refusing an installation token only on the user route
func doctorGitHubStandIn(t *testing.T, status *int) *[]doctorGitHubSeen {
	t.Helper()
	var mu sync.Mutex
	seen := &[]doctorGitHubSeen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		*seen = append(*seen, doctorGitHubSeen{r.URL.Path, auth})
		mu.Unlock()
		code, body := *status, `{"login": "octocat"}`
		switch {
		case code == 200 && r.URL.Path == "/user" && strings.HasPrefix(auth, "Bearer ghs_"):
			code, body = 403, `{"message": "Resource not accessible by integration", "documentation_url": "https://docs.github.com/rest"}`
		case code == 401:
			body = `{"message": "Bad credentials", "documentation_url": "https://docs.github.com/rest"}`
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}))
	parsed, err := url.Parse(doctorGitHubAPIProbeURL)
	if err != nil {
		t.Fatal(err)
	}
	saved := doctorGitHubAPIProbeURL
	doctorGitHubAPIProbeURL = srv.URL + parsed.Path
	t.Cleanup(func() {
		doctorGitHubAPIProbeURL = saved
		srv.Close()
	})
	return seen
}

// Runs the GitHub token row of the probe table over a home with the given .env body
func doctorGitHubProbeRow(t *testing.T, dotenv string, hasDotenv bool) doctorProbeResult {
	t.Helper()
	d, _ := doctorTestDoctor(t, "")
	if hasDotenv {
		if err := os.WriteFile(filepath.Join(d.Home, ".env"), []byte(dotenv), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	config.InvalidateEnvCache()
	doctorUnsetEnv(t, "GITHUB_TOKEN", "GH_TOKEN")
	doctorAPIKeyProvidersMu.Lock()
	saved := doctorAPIKeyProvidersCache
	doctorAPIKeyProvidersCache = []doctorAPIKeyProvider{}
	doctorAPIKeyProvidersMu.Unlock()
	t.Cleanup(func() {
		doctorAPIKeyProvidersMu.Lock()
		doctorAPIKeyProvidersCache = saved
		doctorAPIKeyProvidersMu.Unlock()
	})
	var probes []doctorProbe
	for _, p := range newDoctorProbeEnv(context.Background(), d).doctorBuildProbes() {
		if p.Label == "GitHub token" {
			probes = append(probes, p)
		}
	}
	if len(probes) != 1 {
		t.Fatal("GitHub token probe missing from the doctor's connectivity table")
	}
	results, err := doctorRunProbes(probes)
	if err != nil || len(results) != 1 {
		t.Fatalf("results = %#v %v", results, err)
	}
	return results[0]
}

func TestExpiredDotenvTokenIsRejectedAndTheEnvFileIsNamed(t *testing.T) {
	status := 401
	seen := doctorGitHubStandIn(t, &status)
	result := doctorGitHubProbeRow(t, "GITHUB_TOKEN=ghp_expired000000000000000000000000000000\n", true)
	if len(result.Lines) != 1 {
		t.Fatalf("result = %#v", result)
	}
	glyph, detail := result.Lines[0].Glyph, result.Lines[0].Detail
	if !strings.Contains(glyph, "✗") || !strings.Contains(detail, "GITHUB_TOKEN") || !strings.Contains(detail, "/.env") || !strings.Contains(detail, "rejected") {
		t.Fatalf("row = %q %q", glyph, detail)
	}
	if len(result.Issues) == 0 || !strings.Contains(result.Issues[0], "GITHUB_TOKEN") || !strings.Contains(result.Issues[0], "/.env") {
		t.Fatalf("issues = %q", result.Issues)
	}
	if len(*seen) == 0 || (*seen)[0].Authorization != "Bearer ghp_expired000000000000000000000000000000" {
		t.Fatalf("seen = %#v", *seen)
	}
	if strings.Contains(detail, "ghp_expired") || strings.Contains(result.Issues[0], "ghp_expired") {
		t.Fatal("the token leaked into the report")
	}
}

func TestValidTokenIsOkAndNoTokenIsSkipped(t *testing.T) {
	status := 200
	seen := doctorGitHubStandIn(t, &status)
	result := doctorGitHubProbeRow(t, "GH_TOKEN=gho_valid00000000000000000000000000000000\n", true)
	if len(result.Lines) != 1 || !strings.Contains(result.Lines[0].Glyph, "✓") || !strings.Contains(result.Lines[0].Detail, "GH_TOKEN") || len(result.Issues) != 0 {
		t.Fatalf("result = %#v", result)
	}
	*seen = nil
	result = doctorGitHubProbeRow(t, "", false)
	if len(result.Lines) != 0 || len(result.Issues) != 0 || len(*seen) != 0 {
		t.Fatalf("result = %#v seen = %#v", result, *seen)
	}
}

func TestActionsInstallationTokenIsAccepted(t *testing.T) {
	status := 200
	seen := doctorGitHubStandIn(t, &status)
	result := doctorGitHubProbeRow(t, "GITHUB_TOKEN=ghs_install000000000000000000000000000000\n", true)
	if len(result.Lines) != 1 || !strings.Contains(result.Lines[0].Glyph, "✓") || !strings.Contains(result.Lines[0].Detail, "GITHUB_TOKEN") || len(result.Issues) != 0 {
		t.Fatalf("result = %#v seen = %#v", result, *seen)
	}
}

// Runs the IPv6 route probe with a doubled resolver and connect over an optional network config
func doctorRunIPv6Probe(t *testing.T, connect func(string, time.Duration) error, configBody string, addrs []net.IP) doctorProbeResult {
	t.Helper()
	d, _ := doctorTestDoctor(t, configBody)
	e := newDoctorProbeEnv(context.Background(), d)
	e.lookupIPv6 = func(context.Context, string) ([]net.IP, error) { return addrs, nil }
	e.tcpConnect = connect
	result, err := e.probeIPv6Path()
	if err != nil {
		t.Fatal(err)
	}
	return result
}

var doctorAAAA = []net.IP{net.ParseIP("2001:db8::1")}

func TestDeadIpv6RouteWarnsAndNamesForceIpv4(t *testing.T) {
	var dialed string
	result := doctorRunIPv6Probe(t, func(addr string, _ time.Duration) error {
		dialed = addr
		return os.ErrDeadlineExceeded
	}, "", doctorAAAA)
	if len(result.Lines) != 1 || !strings.Contains(result.Lines[0].Glyph, "⚠") || !strings.Contains(result.Lines[0].Detail, "network.force_ipv4") {
		t.Fatalf("result = %#v", result)
	}
	if !strings.Contains(result.Lines[0].Detail, "connect timed out after 2s") || dialed != "[2001:db8::1]:443" {
		t.Fatalf("detail = %q dialed = %q", result.Lines[0].Detail, dialed)
	}
	if len(result.Issues) == 0 || !strings.Contains(result.Issues[0], "network.force_ipv4") {
		t.Fatalf("issues = %q", result.Issues)
	}
}

func TestHealthyOrAbsentIpv6NeverWarns(t *testing.T) {
	reachable := func(string, time.Duration) error { return nil }
	noRoute := func(string, time.Duration) error {
		return &net.OpError{Op: "dial", Net: "tcp6", Err: os.NewSyscallError("connect", doctorNoIPv6RouteErrnos()[0])}
	}
	if got := doctorRunIPv6Probe(t, reachable, "", doctorAAAA); len(got.Issues) != 0 || !strings.Contains(got.Lines[0].Glyph, "✓") {
		t.Fatalf("reachable = %#v", got)
	}
	if got := doctorRunIPv6Probe(t, noRoute, "", doctorAAAA); len(got.Issues) != 0 || !strings.Contains(got.Lines[0].Detail, "no IPv6 route") {
		t.Fatalf("no route = %#v", got)
	}
	if got := doctorRunIPv6Probe(t, reachable, "network:\n  force_ipv4: true\n", doctorAAAA); len(got.Lines) != 0 {
		t.Fatalf("force_ipv4 = %#v", got)
	}
	if got := doctorRunIPv6Probe(t, reachable, "", nil); len(got.Lines) != 0 {
		t.Fatalf("no AAAA = %#v", got)
	}
}

func TestRunProbesSanitizesEnvBeforeAnyWorkerBuildsAClient(t *testing.T) {
	doctorClashEnv(t)
	d, _ := doctorTestDoctor(t, "")
	e := newDoctorProbeEnv(context.Background(), d)
	seenInWorker := ""
	results, err := doctorRunProbes([]doctorProbe{{"stand-in", func() (doctorProbeResult, error) {
		seenInWorker = os.Getenv("NO_PROXY")
		return e.doctorConnectivityRow("stand-in", "ok", "", nil, ""), nil
	}}})
	if err != nil || len(results) != 1 || len(results[0].Lines) != 1 {
		t.Fatalf("results = %#v %v", results, err)
	}
	if line := results[0].Lines[0]; !strings.Contains(line.Glyph, "✓") || line.Label != "stand-in" {
		t.Fatalf("line = %#v", line)
	}
	if strings.Contains(seenInWorker, "[::1]") || strings.Contains(os.Getenv("NO_PROXY"), "[::1]") || os.Getenv("no_proxy") != "127.0.0.1,localhost,::1" {
		t.Fatalf("worker saw %q, NO_PROXY = %q no_proxy = %q", seenInWorker, os.Getenv("NO_PROXY"), os.Getenv("no_proxy"))
	}
}

func TestRunProbesLeavesACleanEnvironmentUntouched(t *testing.T) {
	doctorClashEnv(t)
	t.Setenv("NO_PROXY", "example.com,10.0.0.0/8")
	os.Unsetenv("no_proxy")
	doctorUnsetEnv(t, "AWS_EC2_METADATA_DISABLED")
	d, _ := doctorTestDoctor(t, "")
	e := newDoctorProbeEnv(context.Background(), d)
	imds := ""
	results, err := doctorRunProbes([]doctorProbe{{"stand-in", func() (doctorProbeResult, error) {
		imds = os.Getenv("AWS_EC2_METADATA_DISABLED")
		return e.doctorConnectivityRow("stand-in", "ok", "", nil, ""), nil
	}}})
	if err != nil || len(results) != 1 || !strings.Contains(results[0].Lines[0].Glyph, "✓") {
		t.Fatalf("results = %#v %v", results, err)
	}
	if got := os.Getenv("NO_PROXY"); got != "example.com,10.0.0.0/8" {
		t.Fatalf("NO_PROXY = %q", got)
	}
	if _, set := os.LookupEnv("AWS_EC2_METADATA_DISABLED"); set || imds != "true" {
		t.Fatalf("metadata switch during the run %q, still set after it %v", imds, set)
	}
}

func TestDoctorGeminiProbeKeepsAqKeysOnTheStudioHost(t *testing.T) {
	const studioModels = "https://generativelanguage.googleapis.com/v1beta/models"
	doctorUnsetEnv(t, "GEMINI_BASE_URL")
	d, _ := doctorTestDoctor(t, "")
	e := newDoctorProbeEnv(context.Background(), d)
	header := func(headers [][2]string, name string) string {
		for _, h := range headers {
			if strings.EqualFold(h[0], name) {
				return h[1]
			}
		}
		return ""
	}
	_, target, headers, err := e.doctorAPIKeyRequest("AQ.studio-key", "GEMINI_BASE_URL", studioModels)
	if err != nil || target != studioModels || header(headers, "x-goog-api-key") != "AQ.studio-key" || header(headers, "Authorization") != "" {
		t.Fatalf("target = %q headers = %v err = %v", target, headers, err)
	}
	if _, target, _, err = e.doctorAPIKeyRequest("AIza-studio-key", "GEMINI_BASE_URL", studioModels); err != nil || target != studioModels {
		t.Fatalf("target = %q err = %v", target, err)
	}
	t.Setenv("GEMINI_BASE_URL", "https://aiplatform.googleapis.com")
	_, target, headers, err = e.doctorAPIKeyRequest("AQ.express-key", "GEMINI_BASE_URL", studioModels)
	if err != nil || target != "https://aiplatform.googleapis.com/v1beta1/publishers/google/models" || header(headers, "x-goog-api-key") != "AQ.express-key" {
		t.Fatalf("target = %q headers = %v err = %v", target, headers, err)
	}
}

func TestBuildApikeyProvidersListSkipsDedicatedCheckProviders(t *testing.T) {
	names := map[string]bool{}
	for _, entry := range doctorBuildAPIKeyProvidersList(context.Background()) {
		names[strings.ToLower(entry.Name)] = true
	}
	if names["anthropic"] {
		t.Fatalf("Anthropic provider profile leaked into the generic bearer auth health check loop: %v", names)
	}
	if names["openrouter"] {
		t.Fatalf("OpenRouter has a dedicated check, the generic loop must skip it: %v", names)
	}
	for name := range names {
		if strings.Contains(name, "bedrock") {
			t.Fatalf("Bedrock uses AWS SDK credentials, the generic loop must skip it: %v", names)
		}
	}
	if !names["gmi cloud"] || !names["opencode go"] {
		t.Fatalf("static rows missing: %v", names)
	}
}
