package dashboard

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Round tripper answering every request through a function, a zero status failing the request like a missing fake method
type probeTransport func(req *http.Request) (int, string, string)

func (f probeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	status, contentType, body := f(req)
	if status == 0 {
		return nil, errors.New("the fake client has no " + req.Method + " method")
	}
	h := http.Header{}
	if contentType != "" {
		h.Set("content-type", contentType)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

// Installs a probe client answering through fn for the rest of the test
func installProbeClient(t *testing.T, fn probeTransport) {
	t.Helper()
	prev := endpointProbeClient
	endpointProbeClient = func(string) *http.Client { return &http.Client{Transport: fn} }
	t.Cleanup(func() { endpointProbeClient = prev })
}

func TestLocalEndpointProbesBypassEnvProxy(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	for _, tc := range []struct {
		url       string
		trustsEnv bool
	}{
		{"http://127.0.0.1:8080/v1/models", false},
		{"http://localhost:11434/v1/models", false},
		{"http://192.168.1.20:8000/v1/models", false},
		{"https://api.example.com/v1/models", true},
	} {
		client := newEndpointProbeClient(tc.url)
		trusts := client.Transport.(*http.Transport).Proxy != nil
		if trusts != tc.trustsEnv {
			t.Errorf("%s trusts env %v, want %v", tc.url, trusts, tc.trustsEnv)
		}
	}
}

func TestOpenaiBaseUrlProbeNamesTheHttpStatusInsteadOfNoModels(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	installProbeClient(t, func(*http.Request) (int, string, string) {
		return 502, "application/json", `{"error": "proxy upstream unavailable"}`
	})
	body := pyjson.ObjectOf("key", "OPENAI_BASE_URL", "value", "http://127.0.0.1:8080/v1", "api_key", "")
	out := validateProviderCredential(context.Background(), body)
	if out.GetOr("ok", nil) != false || out.GetOr("reachable", nil) != true {
		t.Fatalf("verdict %v", out)
	}
	if !strings.Contains(out.GetOr("message", "").(string), "HTTP 502") {
		t.Fatalf("message %v", out.GetOr("message", ""))
	}
}

// Runs one endpoint validator over a body the way the named route does
func runEndpointValidator(route string, key, baseURL, apiKey string) *pyjson.Object {
	if route == "/api/providers/validate" {
		return validateProviderCredential(context.Background(), pyjson.ObjectOf("key", key, "value", baseURL, "api_key", apiKey))
	}
	return validateCustomEndpoint(context.Background(), pyjson.ObjectOf("id", "", "name", "local", "base_url", baseURL,
		"api_key", apiKey, "model", ""))
}

func TestBareRootProbeResolvesToTheV1BaseThatServedModels(t *testing.T) {
	for _, route := range []string{"/api/providers/validate", "/api/providers/custom-endpoints/validate"} {
		t.Run(route, func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			var mu sync.Mutex
			var seen []string
			installProbeClient(t, func(req *http.Request) (int, string, string) {
				u := req.URL.String()
				if req.Method != http.MethodGet {
					return 0, "", ""
				}
				mu.Lock()
				seen = append(seen, u)
				mu.Unlock()
				if strings.HasSuffix(u, "/v1/models") {
					return 200, "application/json", `{"data": [{"id": "local-model"}]}`
				}
				return 404, "application/json", `{"error": "Unexpected endpoint"}`
			})
			data := runEndpointValidator(route, "OPENAI_BASE_URL", "http://127.0.0.1:39080/", "")
			if !slices.Equal(seen, []string{"http://127.0.0.1:39080/models", "http://127.0.0.1:39080/v1/models"}) {
				t.Fatalf("seen %v", seen)
			}
			if data.GetOr("ok", nil) != true || !pyjson.Equal(data.GetOr("models", nil), []any{"local-model"}) {
				t.Fatalf("data %v", data)
			}
			if data.GetOr("resolved_base_url", nil) != "http://127.0.0.1:39080/v1" {
				t.Fatalf("resolved %v", data.GetOr("resolved_base_url", nil))
			}
		})
	}
}

func TestBareRootProbeReportsTheV1KeyRejectionNotTheRoot404(t *testing.T) {
	for _, route := range []string{"/api/providers/validate", "/api/providers/custom-endpoints/validate"} {
		t.Run(route, func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			installProbeClient(t, func(req *http.Request) (int, string, string) {
				if strings.HasSuffix(req.URL.String(), "/v1/models") {
					return 401, "application/json", `{"error": "unauthorized"}`
				}
				return 404, "application/json", `{"error": "unauthorized"}`
			})
			data := runEndpointValidator(route, "OPENAI_BASE_URL", "http://127.0.0.1:39080", "k")
			message := data.GetOr("message", "").(string)
			if route == "/api/providers/validate" && !strings.Contains(message, "401") {
				t.Fatalf("message %q", message)
			}
			if data.GetOr("ok", nil) != false || data.GetOr("reachable", nil) != true {
				t.Fatalf("data %v", data)
			}
			if strings.Contains(message, "404") {
				t.Fatalf("message names the root 404: %q", message)
			}
		})
	}
}

// Probe host answering every GET and POST with 200 and the given body
func modelsProbeHost(t *testing.T, contentType, body string) {
	installProbeClient(t, func(*http.Request) (int, string, string) { return 200, contentType, body })
}

func TestCustomEndpointProbeWarnsWhenNoModelsParse(t *testing.T) {
	for _, tc := range []struct{ name, contentType, body, needle string }{
		{"spa_html", "text/html", "<html><body><div id=root></div></body></html>", "text/html"},
		{"non_json_body", "text/plain", "OK", "instead of a JSON model list"},
		{"empty_model_list", "application/json", `{"data": []}`, "advertised no models"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			modelsProbeHost(t, tc.contentType, tc.body)
			data := validateCustomEndpoint(context.Background(), pyjson.ObjectOf("id", "", "name", "x",
				"base_url", "https://spa.example.com/v1", "api_key", "", "model", ""))
			message := data.GetOr("message", "").(string)
			if data.GetOr("ok", nil) != false || data.GetOr("reachable", nil) != true {
				t.Fatalf("data %v", data)
			}
			if !strings.Contains(message, tc.needle) || !strings.Contains(message, "https://spa.example.com/v1") {
				t.Fatalf("message %q", message)
			}
			if !pyjson.Equal(data.GetOr("models", nil), []any{}) {
				t.Fatalf("models %v", data.GetOr("models", nil))
			}
		})
	}
}

func TestCustomEndpointProbePassesOnAJsonModelsReply(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	modelsProbeHost(t, "application/json", `{"data": [{"id": "gpt-4o"}, {"id": "gpt-4o-mini"}], "object": "list"}`)
	data := validateCustomEndpoint(context.Background(), pyjson.ObjectOf("id", "", "name", "x",
		"base_url", "https://api.example.com/v1", "api_key", "", "model", ""))
	if data.GetOr("ok", nil) != true || data.GetOr("reachable", nil) != true {
		t.Fatalf("data %v", data)
	}
	if !pyjson.Equal(data.GetOr("models", nil), []any{"gpt-4o", "gpt-4o-mini"}) || data.GetOr("message", nil) != "" {
		t.Fatalf("data %v", data)
	}
}
