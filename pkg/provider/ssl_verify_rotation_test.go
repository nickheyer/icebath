package provider

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type caRotation struct {
	stale string
	live  string
}

func resetCABundleForTest() {
	caBundleMu.Lock()
	caBundleWhere = nil
	caBundleMu.Unlock()
	staleBundleMu.Lock()
	staleBundleKnown = map[string]string{}
	staleBundleMu.Unlock()
}

// Caches a bundle inside env-a then deletes env-a, with env-b on the live search path when replaced
func rotatedCABundle(t *testing.T, bundle []byte, replacement bool) caRotation {
	t.Helper()
	noCAEnv(t)
	InstallTrustStore()
	dir := t.TempDir()
	write := func(env string) string {
		path := filepath.Join(dir, env, "certifi", "cacert.pem")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, bundle, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	prevBundles, prevReload := linuxRootBundles, reloadCABundle
	t.Cleanup(func() {
		linuxRootBundles, reloadCABundle = prevBundles, prevReload
		resetCABundleForTest()
	})
	resetCABundleForTest()
	rotation := caRotation{stale: write("env-a")}
	linuxRootBundles = []string{rotation.stale}
	if replacement {
		rotation.live = write("env-b")
		linuxRootBundles = append(linuxRootBundles, rotation.live)
	}
	if got := DefaultCABundlePath(); got != rotation.stale {
		t.Fatalf("the bundle resolves into env-a first, got %q", got)
	}
	if err := os.RemoveAll(filepath.Join(dir, "env-a")); err != nil {
		t.Fatal(err)
	}
	return rotation
}

func captureSSLWarnings(t *testing.T) *recordingHandler {
	t.Helper()
	sink := &recordingHandler{}
	prev := slog.Default()
	slog.SetDefault(newRecordingLogger(sink))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return sink
}

func privateTLSServer(t *testing.T) (*httptest.Server, []byte) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("private provider"))
	}))
	t.Cleanup(srv.Close)
	return srv, pemOf(srv.Certificate())
}

func getThrough(cfg *tls.Config, url string) (string, error) {
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
	defer client.CloseIdleConnections()
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return string(body), err
}

func TestReplacedInstallEnvironmentReResolvesTheBundleInPlace(t *testing.T) {
	_, bundle := privateTLSServer(t)
	rotation := rotatedCABundle(t, bundle, true)
	sink := captureSSLWarnings(t)
	verify := ResolveTLSVerify(nil, "", nil, "")
	if verify.Insecure || verify.Config != sharedTLSConfig(rotation.live, InstallTrustStore()) {
		t.Fatal("the default context is built on the re-resolved bundle")
	}
	if got := DefaultCABundlePath(); got != rotation.live {
		t.Fatalf("the repair reaches every later consumer, got %q", got)
	}
	logged := sink.containing("TLS trust: certifi's CA bundle")
	if len(logged) != 1 || !strings.Contains(logged[0], "re-resolved") || !strings.Contains(logged[0], rotation.stale) {
		t.Fatalf("one re-resolved warning naming the dead path, got %v", logged)
	}
}

func TestRotationRepairAndWarningHappenOnce(t *testing.T) {
	_, bundle := privateTLSServer(t)
	rotatedCABundle(t, bundle, true)
	sink := captureSSLWarnings(t)
	reloads := 0
	reloadCABundle = func() string {
		reloads++
		return reloadCABundleFromSearch()
	}
	h := t.TempDir()
	for range 5 {
		if verify := ResolveTLSVerify(nil, "", nil, ""); verify.Insecure || verify.Config == nil {
			t.Fatal("a verifying default context")
		}
		SubprocessVerifyBundle(h)
	}
	if reloads != 1 || len(sink.containing("TLS trust: certifi's CA bundle")) != 1 {
		t.Fatalf("one repair and one warning, got %d reloads", reloads)
	}
}

func TestUnrecoverableBundleHandsOverThePlatformContext(t *testing.T) {
	srv, bundle := privateTLSServer(t)
	rotatedCABundle(t, bundle, false)
	sink := captureSSLWarnings(t)
	attempts := 0
	reloadCABundle = func() string {
		attempts++
		return ""
	}
	var verify TLSVerify
	for range 5 {
		verify = ResolveTLSVerify(nil, "", nil, "")
	}
	if verify.Insecure || verify.Config != sharedTLSConfig("", false) || verify.Config.InsecureSkipVerify {
		t.Fatal("the platform context is handed over")
	}
	if attempts != 1 {
		t.Fatalf("a dead path is not re-probed per client, got %d", attempts)
	}
	logged := sink.containing("TLS trust: certifi's CA bundle")
	if len(logged) != 1 || !strings.Contains(logged[0], "could not be re-resolved") {
		t.Fatalf("one unrecoverable warning, got %v", logged)
	}
	if _, err := getThrough(verify.Config, srv.URL); err == nil {
		t.Fatal("the platform context still verifies certificates")
	}
}

func TestSubprocessVerifyBundleHandsOverABundleThatExists(t *testing.T) {
	_, bundle := privateTLSServer(t)
	rotation := rotatedCABundle(t, bundle, true)
	got := SubprocessVerifyBundle(t.TempDir())
	if got != rotation.live || !fileExists(got) {
		t.Fatalf("the live bundle is handed over, got %q", got)
	}
}

func TestSubprocessVerifyBundleNeverHandsOverTheDeletedBundle(t *testing.T) {
	_, bundle := privateTLSServer(t)
	rotation := rotatedCABundle(t, bundle, false)
	reloadCABundle = func() string { return "" }
	got := SubprocessVerifyBundle(t.TempDir())
	if got == rotation.stale {
		t.Fatal("the dead path is never handed over")
	}
	if got != "" {
		if _, err := os.Stat(got); err != nil {
			t.Fatalf("a handed over bundle exists, got %q", got)
		}
	}
}

func TestRotatedProcessKeepsTrustingTheBundleItWasConfiguredWith(t *testing.T) {
	srv, bundle := privateTLSServer(t)
	rotatedCABundle(t, bundle, true)
	body, err := getThrough(ResolveTLSVerify(nil, "", nil, "").Config, srv.URL)
	if err != nil || body != "private provider" {
		t.Fatalf("the re-resolved bundle trusts the private server, got %q %v", body, err)
	}
}

func TestExplicitBundleWorksWithoutTheTrustStore(t *testing.T) {
	noCAEnv(t)
	srv, bundle := privateTLSServer(t)
	path := filepath.Join(t.TempDir(), "provider-ca.pem")
	if err := os.WriteFile(path, bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	failed := false
	trustMu.Lock()
	trustInstalled, platformRoots = &failed, x509.NewCertPool()
	trustMu.Unlock()
	explicit := ResolveTLSVerify(nil, path, nil, "")
	if explicit.Insecure || explicit.Config.InsecureSkipVerify || explicit.Config.RootCAs == nil {
		t.Fatal("an explicit bundle yields a verifying context")
	}
	if body, err := getThrough(explicit.Config, srv.URL); err != nil || body != "private provider" {
		t.Fatalf("the explicit bundle is trusted, got %q %v", body, err)
	}
	fallback := ResolveTLSVerify(nil, "", nil, "")
	if fallback.Insecure || fallback.Config.InsecureSkipVerify {
		t.Fatal("an uninstalled store still verifies")
	}
}

func TestUnloadableBundleFailsEveryHandshakeUncached(t *testing.T) {
	noCAEnv(t)
	srv, _ := privateTLSServer(t)
	path := filepath.Join(t.TempDir(), "empty.pem")
	if err := os.WriteFile(path, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := ResolveTLSVerify(nil, path, nil, "").Config
	if first == ResolveTLSVerify(nil, path, nil, "").Config {
		t.Fatal("a bundle that failed to load is not cached")
	}
	if _, err := getThrough(first, srv.URL); err == nil || !strings.Contains(err.Error(), errNoCertificateFound.Error()) {
		t.Fatalf("the load error fails the request, got %v", err)
	}
}

func TestTrustStoreFailureDegradesToTheDefaultTrustPaths(t *testing.T) {
	noCAEnv(t)
	sink := captureSSLWarnings(t)
	prevPool, prevBundles, prevDirs := systemCertPool, linuxRootBundles, linuxRootDirs
	t.Cleanup(func() { systemCertPool, linuxRootBundles, linuxRootDirs = prevPool, prevBundles, prevDirs })
	systemCertPool = func() (*x509.CertPool, error) { return nil, errors.New("unavailable fixture") }
	linuxRootBundles, linuxRootDirs = nil, nil
	if InstallTrustStore() || InstallTrustStore() {
		t.Fatal("a store that cannot load reports false every time")
	}
	if len(sink.containing("truststore unavailable")) != 1 {
		t.Fatal("the failure is reported once")
	}
	verify := ResolveTLSVerify(nil, "", nil, "")
	if verify.Insecure || verify.Config == nil || verify.Config.InsecureSkipVerify {
		t.Fatal("a degraded store still verifies certificates")
	}
}
