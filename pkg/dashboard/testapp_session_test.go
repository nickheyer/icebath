package dashboard

import (
	"encoding/json"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Test application client sending the session token on every request, the TestClient fixture with the session header
type sessionClient struct{ *testClient }

// Session client over the dashboard test application
func newSessionClient(t *testing.T) sessionClient {
	return sessionClient{newTestClient(t, dashboardTestApp(t))}
}

func (c sessionClient) get(target string) *testResponse {
	return c.request("GET", target, nil, sessionHeaderName, SessionToken())
}

// Request with a JSON body, no body when body is nil
func (c sessionClient) send(method, target string, body any) *testResponse {
	if body == nil {
		return c.request(method, target, nil, sessionHeaderName, SessionToken())
	}
	raw, err := json.Marshal(body)
	if err != nil {
		c.t.Fatal(err)
	}
	return c.request(method, target, raw, "content-type", "application/json", sessionHeaderName, SessionToken())
}

// Decoded JSON object of a response as plain maps, failing the case on any other shape
func jsonObject(t *testing.T, r *testResponse) map[string]any {
	t.Helper()
	m, ok := pyjson.Plain(r.JSON(t)).(map[string]any)
	if !ok {
		t.Fatalf("status %d body %s is not an object", r.Status, r.Body)
	}
	return m
}
