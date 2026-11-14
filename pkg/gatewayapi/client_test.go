package gatewayapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Adapter over a platform config carrying the optional key and CORS origins
func makeAdapter(t *testing.T, apiKey string, corsOrigins []any) *APIServerAdapter {
	t.Helper()
	extra := map[string]any{}
	if apiKey != "" {
		extra["key"] = apiKey
	}
	if corsOrigins != nil {
		extra["cors_origins"] = corsOrigins
	}
	adapter, err := NewAPIServerAdapter(context.Background(), &gateway.PlatformConfig{Enabled: true, Extra: extra})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

// Application of the adapter with the CORS and security header middlewares and its routes, not started
func createApp(t *testing.T, adapter *APIServerAdapter) *gateway.WebApplication {
	t.Helper()
	app := gateway.NewWebApplication(1024 * 1024)
	app.Middlewares = []gateway.WebMiddleware{corsMiddlewareFor(app), securityHeadersMiddleware}
	app.State["api_server_adapter"] = adapter
	for _, route := range adapter.httpRouteTable() {
		if err := app.AddRoute(route.method, route.path, route.handler); err != nil {
			t.Fatal(err)
		}
	}
	return app
}

// Loopback client of a started application, the aiohttp test client
type testClient struct {
	t    *testing.T
	base string
}

// Starts the application on a loopback port for the test's lifetime
func newTestClient(t *testing.T, app *gateway.WebApplication) *testClient {
	t.Helper()
	host := "127.0.0.1"
	runner, err := gateway.StartWebApp(context.Background(), app, &host, 0, gateway.ListenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runner.Cleanup(context.Background()) })
	return &testClient{t: t, base: fmt.Sprintf("http://127.0.0.1:%d", runner.Port())}
}

// Sends one request, a non byte body encoded as JSON, and answers the response with its body read
func (c *testClient) do(method, path string, body any, headers map[string]string) (*http.Response, []byte) {
	c.t.Helper()
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		reader = bytes.NewReader(b)
	case string:
		reader = bytes.NewReader([]byte(b))
	default:
		encoded, err := json.Marshal(b)
		if err != nil {
			c.t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
		if headers == nil {
			headers = map[string]string{}
		}
		if _, set := headers["Content-Type"]; !set {
			headers["Content-Type"] = "application/json"
		}
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		c.t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	return resp, data
}

// Decoded JSON object of a response body
func jsonBody(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("body %q: %v", data, err)
	}
	return out
}

// Message of an OpenAI error envelope
func errorMessage(t *testing.T, data []byte) string {
	t.Helper()
	envelope, _ := jsonBody(t, data)["error"].(map[string]any)
	message, _ := envelope["message"].(string)
	return message
}
