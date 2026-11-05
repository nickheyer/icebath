package provider

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func noCAEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SSL_CERT_FILE", "")
	t.Setenv("SSL_CERT_DIR", "")
	os.Unsetenv("SSL_CERT_FILE")
	os.Unsetenv("SSL_CERT_DIR")
	ResetTrustStoreForTests()
	t.Cleanup(ResetTrustStoreForTests)
}

func TestMissingExplicitBundleFallsBackToThePlatformStore(t *testing.T) {
	noCAEnv(t)
	sink := &recordingHandler{}
	verify := ResolveTLSVerify(newRecordingLogger(sink), filepath.Join(t.TempDir(), "nope.pem"), nil, "")
	if verify.Insecure || verify.Config != defaultVerify() {
		t.Fatal("the default verification is the fallback")
	}
	if len(sink.containing("does not exist")) != 1 {
		t.Fatal("the missing bundle is reported")
	}
}

func TestMissingBundleUnderACertEnvVarSharesThePlatformContext(t *testing.T) {
	noCAEnv(t)
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	bundle := filepath.Join(t.TempDir(), "roots.pem")
	if err := os.WriteFile(bundle, pemOf(srv.Certificate()), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", bundle)
	platform := ResolveTLSVerify(nil, "", nil, "").Config
	if ResolveTLSVerify(nil, filepath.Join(t.TempDir(), "nope.pem"), nil, "").Config != platform {
		t.Fatal("a missing bundle lands on the same shared context")
	}
	bundleCtx := PlatformTLSConfig()
	if bundleCtx != PlatformTLSConfig() || bundleCtx == platform {
		t.Fatal("the unioned bundle context is cached separately")
	}
}

func TestInsecureDisablesVerificationAndBeatsABundle(t *testing.T) {
	noCAEnv(t)
	for _, v := range []any{false, "false", "0", "no", "off", "FALSE"} {
		if verify := ResolveTLSVerify(nil, "", v, ""); !verify.Insecure || !verify.Config.InsecureSkipVerify {
			t.Fatalf("%v disables verification", v)
		}
	}
	if !ResolveTLSVerify(nil, DefaultCABundlePath(), false, "").Insecure {
		t.Fatal("insecure beats an explicit bundle")
	}
	if ResolveTLSVerify(nil, "", true, "").Insecure || ResolveTLSVerify(nil, "", "yes", "").Insecure {
		t.Fatal("truthy values keep verification")
	}
}

func TestExplicitProviderCAReplacesPlatformTrustOnRealHTTPS(t *testing.T) {
	noCAEnv(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("private provider")) }))
	t.Cleanup(server.Close)
	cert := server.Certificate()
	bundle := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(bundle, pemOf(cert), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: ResolveTLSVerify(nil, bundle, nil, server.URL).Config}}
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("the explicit bundle must trust the private server: %v", err)
	}
	resp.Body.Close()
	platformClient := &http.Client{Transport: &http.Transport{TLSClientConfig: ResolveTLSVerify(nil, "", nil, server.URL).Config}}
	if _, err := platformClient.Get(server.URL); err == nil {
		t.Fatal("the platform store must not trust the private server")
	}
	insecure := &http.Client{Transport: &http.Transport{TLSClientConfig: ResolveTLSVerify(nil, "", false, server.URL).Config}}
	if resp, err := insecure.Get(server.URL); err != nil {
		t.Fatalf("insecure accepts anything: %v", err)
	} else {
		resp.Body.Close()
	}
}

func pemOf(cert *x509.Certificate) []byte {
	return []byte("-----BEGIN CERTIFICATE-----\n" + base64Lines(cert.Raw) + "-----END CERTIFICATE-----\n")
}

func base64Lines(raw []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var out strings.Builder
	for i := 0; i < len(raw); i += 3 {
		var chunk [3]byte
		n := copy(chunk[:], raw[i:])
		v := uint(chunk[0])<<16 | uint(chunk[1])<<8 | uint(chunk[2])
		out.WriteByte(alphabet[v>>18&63])
		out.WriteByte(alphabet[v>>12&63])
		if n > 1 {
			out.WriteByte(alphabet[v>>6&63])
		} else {
			out.WriteByte('=')
		}
		if n > 2 {
			out.WriteByte(alphabet[v&63])
		} else {
			out.WriteByte('=')
		}
	}
	encoded := out.String()
	var lines strings.Builder
	for len(encoded) > 64 {
		lines.WriteString(encoded[:64] + "\n")
		encoded = encoded[64:]
	}
	lines.WriteString(encoded + "\n")
	return lines.String()
}

func TestSubprocessVerifyBundleKeepsOwnOverridesAndHandsOverThePlatformStore(t *testing.T) {
	noCAEnv(t)
	t.Setenv("REQUESTS_CA_BUNDLE", "/corp/proxy-ca.pem")
	if SubprocessVerifyBundle(t.TempDir()) != "" {
		t.Fatal("an operator override keeps the child's own default")
	}
	os.Unsetenv("REQUESTS_CA_BUNDLE")
	h := t.TempDir()
	bundle := SubprocessVerifyBundle(h)
	if bundle == "" || !fileExists(bundle) {
		t.Fatalf("a bundle that exists must be handed over, got %q", bundle)
	}
}

func TestPlatformStoreBundleSurvivesAnUnwritableHome(t *testing.T) {
	noCAEnv(t)
	blocker := filepath.Join(t.TempDir(), "home")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if PlatformStoreBundle(blocker) != "" {
		t.Fatal("a disk error must not escape as a bundle path")
	}
	h := t.TempDir()
	path := PlatformStoreBundle(h)
	if path == "" {
		t.Skip("no platform roots readable on this host")
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "BEGIN CERTIFICATE") || !strings.HasPrefix(path, h) {
		t.Fatal("the bundle is PEM under the home cache")
	}
	if PlatformStoreBundle(h) != path {
		t.Fatal("written once")
	}
	if cfg := PlatformTLSConfig(); cfg.RootCAs == nil || cfg.MinVersion != tls.VersionTLS12 {
		t.Fatal("the platform context carries the roots")
	}
}
