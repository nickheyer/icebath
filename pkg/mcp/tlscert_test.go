package mcp

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io/fs"
	"math/big"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// clientPEM writes a self signed client certificate and key, returning cert, key and combined paths
func clientPEM(t *testing.T, cn string) (string, string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	dir := t.TempDir()
	certPath, keyPath, combined := filepath.Join(dir, "client.crt"), filepath.Join(dir, "client.key"), filepath.Join(dir, "client.pem")
	for path, data := range map[string][]byte{certPath: certPEM, keyPath: keyPEM, combined: append(append([]byte(nil), certPEM...), keyPEM...)} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return certPath, keyPath, combined
}

// peerRecorder requires a client certificate on every TLS handshake and records its common name
type peerRecorder struct {
	mu  sync.Mutex
	cns []string
}

func (p *peerRecorder) tlsConfig() *tls.Config {
	return &tls.Config{ClientAuth: tls.RequireAnyClientCert, VerifyConnection: func(cs tls.ConnectionState) error {
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, c := range cs.PeerCertificates {
			p.cns = append(p.cns, c.Subject.CommonName)
		}
		return nil
	}}
}

func (p *peerRecorder) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.cns...)
}

// caBundle writes the TLS test server certificate as a PEM CA bundle
func caBundle(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveClientCertReturnsNoneWhenUnset(t *testing.T) {
	for _, cfg := range []map[string]any{{}, {"url": "https://x"}} {
		if c, err := resolveClientCert("srv", cfg["client_cert"], cfg["client_key"]); c != nil || err != nil {
			t.Fatalf("%v gave %v %v", cfg, c, err)
		}
	}
}

func TestResolveClientCertStringFormSinglePEM(t *testing.T) {
	_, _, combined := clientPEM(t, "combined")
	c, err := resolveClientCert("srv", combined, nil)
	if err != nil || c == nil || c.Leaf == nil || c.Leaf.Subject.CommonName != "combined" {
		t.Fatalf("combined PEM gave %v %v", c, err)
	}
}

func TestResolveClientCertListFormTwoElements(t *testing.T) {
	certPath, keyPath, _ := clientPEM(t, "pair")
	c, err := resolveClientCert("srv", []any{certPath, keyPath}, nil)
	if err != nil || c == nil || c.Leaf.Subject.CommonName != "pair" {
		t.Fatalf("pair gave %v %v", c, err)
	}
	if c, err := resolveClientCert("srv", certPath, keyPath); err != nil || c.Leaf.Subject.CommonName != "pair" {
		t.Fatalf("client_cert plus client_key gave %v %v", c, err)
	}
}

func TestResolveClientCertPasswordMustBeString(t *testing.T) {
	certPath, keyPath, _ := clientPEM(t, "pw")
	_, err := resolveClientCert("srv", []any{certPath, keyPath, 42}, nil)
	if err == nil || !regexp.MustCompile(`key passphrase.*must be a string`).MatchString(err.Error()) {
		t.Fatalf("got %v", err)
	}
}

func TestResolveClientCertRejectsNonRegularFile(t *testing.T) {
	_, err := resolveClientCert("srv", t.TempDir(), nil)
	var nf *fileNotFoundError
	if !errors.As(err, &nf) || !regexp.MustCompile(`srv.*client_cert.*not found`).MatchString(err.Error()) {
		t.Fatalf("directory must read as missing, got %v", err)
	}
}

func TestHTTPClientCertCertForwardedToAsyncClient(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	_, _, combined := clientPEM(t, "http-client")
	rec := &peerRecorder{}
	var posts atomic.Int64
	srv := httptest.NewUnstartedServer(jsonMCPHandler(&posts))
	srv.TLS = rec.tlsConfig()
	srv.StartTLS()
	t.Cleanup(srv.Close)
	s, r := httpTask(t, "remote", map[string]any{"url": srv.URL + "/mcp", "client_cert": combined, "ssl_verify": caBundle(t, srv)})
	if _, err := serveHTTPOnce(t, s, r); err != nil {
		t.Fatalf("mTLS connect failed: %v", err)
	}
	if cns := rec.seen(); len(cns) == 0 || cns[0] != "http-client" || posts.Load() == 0 {
		t.Fatalf("server saw client certs %v after %d posts", cns, posts.Load())
	}
	s2, r2 := httpTask(t, "remote-default-verify", map[string]any{"url": srv.URL + "/mcp", "client_cert": combined})
	if _, err := serveHTTPOnce(t, s2, r2); err == nil {
		t.Fatal("ssl_verify defaults on, a self signed server must fail verification")
	}
}

func TestHTTPClientCertMissingCertFileSurfacesClearError(t *testing.T) {
	isolateHome(t)
	s, r := httpTask(t, "remote", map[string]any{"url": "https://example.com/mcp", "client_cert": filepath.Join(t.TempDir(), "nope.pem")})
	_, err := s.runHTTP(t.Context(), r)
	if err == nil || !errors.Is(err, fs.ErrNotExist) || !regexp.MustCompile(`remote.*client_cert.*not found`).MatchString(err.Error()) {
		t.Fatalf("got %v", err)
	}
	if name := errTypeName(err); name != "FileNotFoundError" {
		t.Fatalf("error class %s", name)
	}
}

func TestSSEClientCertFactoryAlwaysInjectedWithDefaultTls(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	rec := &peerRecorder{}
	conf := rec.tlsConfig()
	conf.ClientAuth = tls.RequestClientCert
	l := startLegacySSE(t, rejectWith(405, "", ""), conf)
	s, r := httpTask(t, "sse-test", map[string]any{"url": l.URL + "/mcp", "transport": "sse"})
	if _, err := serveHTTPOnce(t, s, r); err == nil {
		t.Fatal("default verify must reject the self signed SSE server")
	}
	if cns := rec.seen(); len(cns) != 0 {
		t.Fatalf("no client cert configured yet the server saw %v", cns)
	}
}

func TestSSEClientCertFactoryInjectedWhenCertSet(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	_, _, combined := clientPEM(t, "sse-client")
	rec := &peerRecorder{}
	l := startLegacySSE(t, rejectWith(405, "", ""), rec.tlsConfig())
	s, r := httpTask(t, "sse-test", map[string]any{"url": l.URL + "/mcp", "transport": "sse", "client_cert": combined,
		"ssl_verify": caBundle(t, l.Server), "headers": map[string]any{"x": "y"}})
	if _, err := serveHTTPOnce(t, s, r); err != nil {
		t.Fatalf("SSE mTLS connect failed: %v", err)
	}
	if cns := rec.seen(); len(cns) == 0 || cns[0] != "sse-client" || l.lists.Load() == 0 {
		t.Fatalf("server saw client certs %v, lists %d", cns, l.lists.Load())
	}
}

func TestSSEClientCertFactoryForwardsCustomCaBundle(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	l := startLegacySSE(t, rejectWith(405, "", ""), &tls.Config{})
	s, r := httpTask(t, "sse-test", map[string]any{"url": l.URL + "/mcp", "transport": "sse", "ssl_verify": caBundle(t, l.Server)})
	if _, err := serveHTTPOnce(t, s, r); err != nil || l.lists.Load() == 0 {
		t.Fatalf("custom CA bundle must verify the SSE server, got %v lists %d", err, l.lists.Load())
	}
}
