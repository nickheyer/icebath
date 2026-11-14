package provider

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestRequestsVerifyHandsOverThePlatformStoreWithoutOpensslFiles(t *testing.T) {
	srv, bundle := privateTLSServer(t)
	rotatedCABundle(t, bundle, false)
	reloadCABundle = func() string { return "" }
	h := filepath.Join(t.TempDir(), "home")
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("REQUESTS_CA_BUNDLE", "")
	t.Setenv("CURL_CA_BUNDLE", "")
	os.Unsetenv("REQUESTS_CA_BUNDLE")
	os.Unsetenv("CURL_CA_BUNDLE")
	prevBundles, prevStore := linuxRootBundles, platformStoreCertificates
	linuxRootBundles = []string{filepath.Join(t.TempDir(), "gone", "cacert.pem")}
	platformStoreCertificates = func() [][]byte { return [][]byte{srv.Certificate().Raw} }
	t.Cleanup(func() { linuxRootBundles, platformStoreCertificates = prevBundles, prevStore })
	trustMu.Lock()
	platformBundle = ""
	trustMu.Unlock()
	verify := SubprocessVerifyBundle(h)
	if verify == "" || !strings.HasPrefix(verify, h) {
		t.Fatalf("the platform store goes over as a bundle under the home, got %q", verify)
	}
	raw, err := os.ReadFile(verify)
	if err != nil || !strings.Contains(string(raw), "BEGIN CERTIFICATE") {
		t.Fatalf("bundle %q %v", raw, err)
	}
	cfg := ResolveTLSVerify(nil, verify, nil, "").Config
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 2 * time.Second}
	_, err = client.Get("https://127.0.0.1:9/")
	var opErr *net.OpError
	if err == nil || !errors.As(err, &opErr) {
		t.Fatalf("the request fails on the closed port, not on the bundle: %v", err)
	}
	if body, err := getThrough(cfg, srv.URL); err != nil || body != "private provider" {
		t.Fatalf("the handed over bundle trusts the root it holds, got %q %v", body, err)
	}
}

// Loopback TLS server answering every GET with a one model catalog
func catalogTLSServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "test-model", "context_length": 2048}}})
	}))
	t.Cleanup(srv.Close)
	bundle := filepath.Join(t.TempDir(), "loopback.pem")
	if err := os.WriteFile(bundle, pemOf(srv.Certificate()), 0o600); err != nil {
		t.Fatal(err)
	}
	return srv, bundle
}

// Writes the loopback custom provider entry with the given TLS settings into the home config
func writeLoopbackProvider(t *testing.T, h, baseURL string, settings map[string]any) {
	t.Helper()
	entry := map[string]any{"name": "loopback", "base_url": baseURL}
	for k, v := range settings {
		entry[k] = v
	}
	raw, err := json.Marshal(map[string]any{"custom_providers": []any{entry}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestProviderCaReachesTheRealProbeTransport(t *testing.T) {
	for _, probe := range []string{"metadata", "catalog"} {
		t.Run(probe, func(t *testing.T) {
			noCAEnv(t)
			srv, bundle := catalogTLSServer(t)
			h := t.TempDir()
			t.Setenv("ICEBATH_HOME", h)
			ctx := tools.WithToolEnv(context.Background(), &tools.Env{Home: h})
			public := DefaultCABundlePath()
			if public == "" {
				t.Skip("no public CA bundle on this host")
			}
			t.Setenv("SSL_CERT_FILE", "/missing-ambient.pem")
			discover := func() []string {
				ResetTrustStoreForTests()
				if probe == "metadata" {
					var ids []string
					for id := range FetchEndpointModelMetadata(ctx, srv.URL, "", true) {
						ids = append(ids, id)
					}
					return ids
				}
				return ProbeAPIModels(ctx, "", srv.URL, 5*time.Second, "", nil).Models
			}
			writeLoopbackProvider(t, h, srv.URL, map[string]any{"ssl_ca_cert": public})
			if got := discover(); len(got) != 0 {
				t.Fatalf("a public bundle must not trust the private server, got %v", got)
			}
			writeLoopbackProvider(t, h, srv.URL, map[string]any{"ssl_ca_cert": bundle})
			if got := discover(); strings.Join(got, ",") != "test-model" {
				t.Fatalf("the provider CA must reach the probe transport, got %v", got)
			}
			writeLoopbackProvider(t, h, srv.URL, map[string]any{"ssl_ca_cert": public, "ssl_verify": false})
			if got := discover(); strings.Join(got, ",") != "test-model" {
				t.Fatalf("ssl_verify false must reach the probe transport, got %v", got)
			}
		})
	}
}

func TestAmbientCaCannotBreakTheSharedClient(t *testing.T) {
	for _, variable := range []string{"ICEBATH_CA_BUNDLE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		t.Run(variable, func(t *testing.T) {
			noCAEnv(t)
			t.Setenv(variable, filepath.Join(t.TempDir(), "missing-ca"))
			if !InstallTrustStore() || !InstallTrustStore() {
				t.Fatal("the platform store installs under an ambient CA variable")
			}
			verify := ResolveTLSVerify(nil, "", nil, "")
			if verify.Insecure || verify.Config == nil || verify.Config.InsecureSkipVerify || verify.Config.RootCAs == nil {
				t.Fatalf("the shared client keeps verifying: %+v", verify)
			}
			if verify.Config.MinVersion < tls.VersionTLS12 {
				t.Fatal("the shared client keeps its TLS floor")
			}
			client := BuildKeepaliveHTTPClient("https://example.invalid", false, verify)
			if client == nil {
				t.Fatal("no client")
			}
			client.CloseIdleConnections()
		})
	}
}

func TestPlatformStoreBundleWritesUnderTheProcessHomeByDefault(t *testing.T) {
	noCAEnv(t)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	trustMu.Lock()
	platformBundle = ""
	trustMu.Unlock()
	if path := PlatformStoreBundle(""); path != "" && !strings.HasPrefix(path, home.Process()) {
		t.Fatalf("bundle %q outside the process home %q", path, home.Process())
	}
}
