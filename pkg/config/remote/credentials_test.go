package remote

import (
	"context"
	"errors"
	"github.com/nickheyer/icebath/pkg/testfakes/configplane"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

type captureTransport struct {
	mu   sync.Mutex
	seen []*http.Request
	body []string
}

func (c *captureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.seen, c.body = append(c.seen, r), append(c.body, string(raw))
	c.mu.Unlock()
	return &http.Response{StatusCode: 200, Status: "200 OK", Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
}

func idpTestEnv(t *testing.T, tokenURL string) {
	t.Setenv(idpEnvPrefix+"TOKEN_URL", tokenURL)
	t.Setenv(idpEnvPrefix+"CLIENT_ID", configplane.ClientID)
	t.Setenv(idpEnvPrefix+"CLIENT_SECRET", configplane.ClientSecret)
	clearIDPCache()
}

func TestBootFailsClosedOnForbiddenPlaneURLWithoutRetry(t *testing.T) {
	p := setupPlane(t)
	t.Setenv(URLEnv, strings.Replace(p.URL, "127.0.0.1", "plane.example", 1))
	var sleeps []float64
	saved := sleep
	sleep = func(s float64) { sleeps = append(sleeps, s) }
	t.Cleanup(func() { sleep = saved })
	err := testBackend().Boot(context.Background(), p.Home)
	var unavailable *config.ConfigBackendUnavailable
	if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), "https") || len(sleeps) != 0 || len(p.Requests) != 0 || p.TokenRequests != 0 {
		t.Fatalf("err %v sleeps %v", err, sleeps)
	}
}

func TestIDPSecretSentToAllowedOnPremHTTPS(t *testing.T) {
	idpTestEnv(t, "https://idp.corp.internal/oauth2/token")
	capture := &captureTransport{}
	saved := credentialedClient.Transport
	credentialedClient.Transport = capture
	t.Cleanup(func() { credentialedClient.Transport = saved })
	_, err := idpToken(context.Background())
	var pce *PlaneCredentialError
	if !errors.As(err, &pce) || !strings.Contains(err.Error(), "no access_token") {
		t.Fatalf("err %v", err)
	}
	if len(capture.seen) != 1 || capture.seen[0].URL.String() != "https://idp.corp.internal/oauth2/token" || !strings.Contains(capture.body[0], configplane.ClientSecret) {
		t.Fatalf("captured %v", capture.body)
	}
}

func TestIDPRedirectIsNotFollowedWithTheSecret(t *testing.T) {
	for _, code := range []int{302, 307, 308} {
		var targetSeen, idpSeen []string
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			targetSeen = append(targetSeen, string(raw))
		}))
		idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			idpSeen = append(idpSeen, string(raw))
			w.Header().Set("Location", target.URL+"/token")
			w.WriteHeader(code)
		}))
		idpTestEnv(t, idp.URL+"/token")
		_, err := idpToken(context.Background())
		var derr *CredentialDestinationError
		if !errors.As(err, &derr) || derr.Retryable || len(idpSeen) != 1 || !strings.Contains(idpSeen[0], configplane.ClientSecret) || len(targetSeen) != 0 {
			t.Fatalf("%d: %v idp %v target %v", code, err, idpSeen, targetSeen)
		}
		target.Close()
		idp.Close()
	}
}

func TestRefusedURLNeverEchoesUserInfo(t *testing.T) {
	h := filepath.Join(t.TempDir(), "home")
	os.MkdirAll(h, 0o755)
	t.Setenv("ICEBATH_HOME", h)
	const userinfoURL = "https://u:p@example.com"
	t.Setenv(URLEnv, userinfoURL)
	t.Setenv(InstanceEnv, configplane.Instance)
	saved := sleep
	sleep = func(float64) {}
	ResetForTests()
	t.Cleanup(func() { sleep = saved; ResetForTests() })
	b := testBackend()
	err := b.Boot(context.Background(), h)
	var unavailable *config.ConfigBackendUnavailable
	if !errors.As(err, &unavailable) {
		t.Fatalf("err %v", err)
	}
	out := []string{err.Error(), b.Describe(context.Background(), h), ShownURL()}
	for _, bad := range []string{userinfoURL + ":bad", "https://u:p@"} {
		t.Setenv(URLEnv, bad)
		_, derr := Request(context.Background(), "GET", h, "default", "", nil, "")
		var cde *CredentialDestinationError
		if !errors.As(derr, &cde) {
			t.Fatalf("%s: %v", bad, derr)
		}
		out = append(out, derr.Error())
	}
	if !strings.Contains(out[0], "example.com") || !strings.Contains(out[1], "example.com") {
		t.Fatalf("out %v", out)
	}
	for _, text := range out {
		if strings.Contains(text, "u:p") {
			t.Fatalf("user info echoed in %q", text)
		}
	}
}

func TestRotatedIDPClientSecretFetchesANewToken(t *testing.T) {
	p := setupPlane(t)
	ctx := context.Background()
	if _, err := PlaneToken(ctx, p.Home); err != nil {
		t.Fatal(err)
	}
	first := p.TokenRequests
	if _, err := PlaneToken(ctx, p.Home); err != nil || p.TokenRequests != first {
		t.Fatalf("cache missed %v", err)
	}
	t.Setenv(idpEnvPrefix+"CLIENT_SECRET", "rotated-secret")
	var pce *PlaneCredentialError
	if _, err := PlaneToken(ctx, p.Home); !errors.As(err, &pce) || p.TokenRequests != first+1 {
		t.Fatalf("rotation %v %d", err, p.TokenRequests)
	}
}
