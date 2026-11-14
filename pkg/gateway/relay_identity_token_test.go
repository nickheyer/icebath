package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const relayFakeJWT = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1c2VyIn0.c2ln"

// Captured shape of the one request the identity resolver sent
type idpCapture struct {
	method, path, body, contentType string
	hits                            int
}

// Clean IdP env and an empty profile config, the autouse fixture of the resolver suite
func idpCleanEnv(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_MANAGED_DIR", filepath.Join(h, "no-managed"))
	for _, k := range []string{"GATEWAY_RELAY_IDP_TOKEN_URL", "GATEWAY_RELAY_IDP_CLIENT_ID", "GATEWAY_RELAY_IDP_CLIENT_SECRET", "GATEWAY_RELAY_IDP_SCOPE"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	return h
}

// Loopback IdP answering every request with body and recording what it received
func idpServer(t *testing.T, body string) (string, *idpCapture) {
	t.Helper()
	got := &idpCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got.method, got.path, got.body, got.contentType = r.Method, r.URL.Path, string(raw), r.Header.Get("Content-Type")
		got.hits++
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, got
}

func TestClientCredentialsViaEnv(t *testing.T) {
	idpCleanEnv(t)
	base, got := idpServer(t, `{"access_token": "idp-workload-token"}`)
	t.Setenv("GATEWAY_RELAY_IDP_TOKEN_URL", base+"/token")
	t.Setenv("GATEWAY_RELAY_IDP_CLIENT_ID", "agent-client")
	t.Setenv("GATEWAY_RELAY_IDP_CLIENT_SECRET", "shh")
	t.Setenv("GATEWAY_RELAY_IDP_SCOPE", "connector.provision")
	token, err := ResolveRelayIdentityToken(context.Background())
	if err != nil || token != "idp-workload-token" {
		t.Fatalf("token=%q err=%v", token, err)
	}
	if got.path != "/token" || got.method != "POST" {
		t.Fatalf("request %s %s", got.method, got.path)
	}
	for _, part := range []string{"grant_type=client_credentials", "client_id=agent-client", "client_secret=shh", "scope=connector.provision"} {
		if !strings.Contains(got.body, part) {
			t.Fatalf("body %q lacks %q", got.body, part)
		}
	}
	if got.contentType != "application/x-www-form-urlencoded" {
		t.Fatalf("content-type %q", got.contentType)
	}
}

func TestRaisesWhenNoAccessTokenInResponse(t *testing.T) {
	idpCleanEnv(t)
	base, _ := idpServer(t, `{"token_type": "Bearer"}`)
	t.Setenv("GATEWAY_RELAY_IDP_TOKEN_URL", base+"/token")
	t.Setenv("GATEWAY_RELAY_IDP_CLIENT_ID", "c")
	t.Setenv("GATEWAY_RELAY_IDP_CLIENT_SECRET", "s")
	_, err := ResolveRelayIdentityToken(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no access_token") {
		t.Fatalf("err=%v", err)
	}
}

func TestAmbientGetWhenNoClientCredentials(t *testing.T) {
	idpCleanEnv(t)
	base, got := idpServer(t, relayFakeJWT+"\n")
	t.Setenv("GATEWAY_RELAY_IDP_TOKEN_URL", base+"/access-token")
	token, err := ResolveRelayIdentityToken(context.Background())
	if err != nil || token != relayFakeJWT {
		t.Fatalf("token=%q err=%v", token, err)
	}
	if got.path != "/access-token" || got.method != "GET" || got.body != "" {
		t.Fatalf("request %s %s body=%q", got.method, got.path, got.body)
	}
}

func TestAmbientAcceptsJSONEnvelope(t *testing.T) {
	idpCleanEnv(t)
	base, _ := idpServer(t, `{"access_token": "`+relayFakeJWT+`"}`)
	t.Setenv("GATEWAY_RELAY_IDP_TOKEN_URL", base+"/access-token")
	if token, err := ResolveRelayIdentityToken(context.Background()); err != nil || token != relayFakeJWT {
		t.Fatalf("token=%q err=%v", token, err)
	}
}

func TestAmbientRejectsNonStringEnvelopeValues(t *testing.T) {
	for _, envelope := range []string{
		`{"access_token": 12345678901234567890123456789012}`,
		`{"access_token": true}`,
		`{"access_token": {"nested": "x"}}`,
		`{"access_token": ""}`,
		`{"access_token": null}`,
		`{"token": "wrong-field-name"}`,
	} {
		t.Run(envelope, func(t *testing.T) {
			idpCleanEnv(t)
			base, _ := idpServer(t, envelope)
			t.Setenv("GATEWAY_RELAY_IDP_TOKEN_URL", base+"/access-token")
			_, err := ResolveRelayIdentityToken(context.Background())
			if err == nil || !strings.Contains(err.Error(), "ambient") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestAmbientRejectsNonTokenBody(t *testing.T) {
	idpCleanEnv(t)
	base, _ := idpServer(t, "<html>404 not found</html>")
	t.Setenv("GATEWAY_RELAY_IDP_TOKEN_URL", base+"/access-token")
	if _, err := ResolveRelayIdentityToken(context.Background()); err == nil || !strings.Contains(err.Error(), "ambient") {
		t.Fatalf("err=%v", err)
	}
}

func TestAmbientRejectsEmptyBody(t *testing.T) {
	idpCleanEnv(t)
	base, _ := idpServer(t, "   \n")
	t.Setenv("GATEWAY_RELAY_IDP_TOKEN_URL", base+"/access-token")
	if _, err := ResolveRelayIdentityToken(context.Background()); err == nil || !strings.Contains(err.Error(), "ambient") {
		t.Fatalf("err=%v", err)
	}
}

func TestAmbientRejectsShortPlaintextErrorWords(t *testing.T) {
	for _, body := range []string{"unauthorized", "error", "access_denied", "null", "forbidden"} {
		idpCleanEnv(t)
		base, _ := idpServer(t, body)
		t.Setenv("GATEWAY_RELAY_IDP_TOKEN_URL", base+"/access-token")
		if _, err := ResolveRelayIdentityToken(context.Background()); err == nil || !strings.Contains(err.Error(), "ambient") {
			t.Fatalf("%s: err=%v", body, err)
		}
	}
}

func TestAmbientAcceptsLongOpaqueToken(t *testing.T) {
	idpCleanEnv(t)
	opaque := "v2_9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b"
	base, _ := idpServer(t, opaque)
	t.Setenv("GATEWAY_RELAY_IDP_TOKEN_URL", base+"/access-token")
	if token, err := ResolveRelayIdentityToken(context.Background()); err != nil || token != opaque {
		t.Fatalf("token=%q err=%v", token, err)
	}
}

func TestClientCredentialsStillSelectedWhenCredsPresent(t *testing.T) {
	idpCleanEnv(t)
	base, got := idpServer(t, `{"access_token": "cc-token"}`)
	t.Setenv("GATEWAY_RELAY_IDP_TOKEN_URL", base+"/token")
	t.Setenv("GATEWAY_RELAY_IDP_CLIENT_ID", "agent-client")
	t.Setenv("GATEWAY_RELAY_IDP_CLIENT_SECRET", "shh")
	if token, err := ResolveRelayIdentityToken(context.Background()); err != nil || token != "cc-token" {
		t.Fatalf("token=%q err=%v", token, err)
	}
	if got.method != "POST" {
		t.Fatalf("method %s", got.method)
	}
}

func TestPartialCredentialsClientIDOnlyRaisesWithoutGet(t *testing.T) {
	idpCleanEnv(t)
	base, got := idpServer(t, "x")
	t.Setenv("GATEWAY_RELAY_IDP_TOKEN_URL", base+"/token")
	t.Setenv("GATEWAY_RELAY_IDP_CLIENT_ID", "agent-client")
	_, err := ResolveRelayIdentityToken(context.Background())
	if err == nil || !strings.Contains(err.Error(), "client_secret missing") || got.hits != 0 {
		t.Fatalf("err=%v hits=%d", err, got.hits)
	}
}

func TestPartialCredentialsClientSecretOnlyRaisesWithoutGet(t *testing.T) {
	idpCleanEnv(t)
	base, got := idpServer(t, "x")
	t.Setenv("GATEWAY_RELAY_IDP_TOKEN_URL", base+"/token")
	t.Setenv("GATEWAY_RELAY_IDP_CLIENT_SECRET", "shh")
	_, err := ResolveRelayIdentityToken(context.Background())
	if err == nil || !strings.Contains(err.Error(), "client_id missing") || got.hits != 0 {
		t.Fatalf("err=%v hits=%d", err, got.hits)
	}
}

func TestAmbientViaConfigYAML(t *testing.T) {
	h := idpCleanEnv(t)
	base, got := idpServer(t, relayFakeJWT)
	cfg := "gateway:\n  idp:\n    token_url: " + base + "/access-token\n"
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if token, err := ResolveRelayIdentityToken(context.Background()); err != nil || token != relayFakeJWT {
		t.Fatalf("token=%q err=%v", token, err)
	}
	if got.method != "GET" {
		t.Fatalf("method %s", got.method)
	}
}
