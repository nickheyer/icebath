package httputil

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// Whether err holds a certificate the client refused: an unknown authority, a name or address it does not cover, a validity period the clock is outside of, or another chain check
func IsCertificateError(err error) bool {
	var cve *tls.CertificateVerificationError
	var hostPtr *x509.HostnameError
	var host x509.HostnameError
	var unknown x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var roots x509.SystemRootsError
	var constraint x509.ConstraintViolationError
	var critical x509.UnhandledCriticalExtension
	return errors.As(err, &cve) || errors.As(err, &hostPtr) || errors.As(err, &host) || errors.As(err, &unknown) ||
		errors.As(err, &invalid) || errors.As(err, &roots) || errors.As(err, &constraint) || errors.As(err, &critical)
}

// Environment variables naming a CA bundle to trust, in the order they are read
var caBundleVariables = [...]string{"REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE"}

// Bundle a transport trusts as it is on disk, a replaced or edited bundle telling itself apart
type caBundleKey struct {
	variable string
	path     string
	dir      bool
	size     int64
	modTime  int64
	missing  bool
}

// Clone of a base transport trusting one bundle
type caBundleEntry struct {
	key       caBundleKey
	transport *http.Transport
}

// The clone each base transport currently has, replaced (its idle connections closed) when the bundle changes
var caBundleTransports struct {
	mu     sync.Mutex
	byBase map[*http.Transport]caBundleEntry
}

// Transport trusting the CA bundle file or directory REQUESTS_CA_BUNDLE or CURL_CA_BUNDLE names on top of the roots t trusts, t itself when neither is set or t skips verification, http.DefaultTransport standing in for a nil t; a bundle that cannot be loaded fails every TLS handshake with the reason
func TrustEnvCA(t *http.Transport) http.RoundTripper {
	if t == nil {
		dt, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			return http.DefaultTransport
		}
		t = dt
	}
	variable, path := envCABundle()
	if path == "" || t.TLSClientConfig != nil && t.TLSClientConfig.InsecureSkipVerify {
		return t
	}
	key := caBundleKey{variable: variable, path: path}
	info, statErr := os.Stat(path)
	if statErr == nil {
		key.dir, key.size, key.modTime = info.IsDir(), info.Size(), info.ModTime().UnixNano()
	} else {
		key.missing = true
	}
	cache := &caBundleTransports
	cache.mu.Lock()
	defer cache.mu.Unlock()
	old, ok := cache.byBase[t]
	if ok && old.key == key {
		return old.transport
	}
	clone := t.Clone()
	clone.TLSClientConfig = caBundleConfig(t.TLSClientConfig, variable, path, info, statErr)
	if cache.byBase == nil {
		cache.byBase = map[*http.Transport]caBundleEntry{}
	}
	cache.byBase[t] = caBundleEntry{key: key, transport: clone}
	if ok {
		old.transport.CloseIdleConnections()
	}
	return clone
}

// Variable and path of the CA bundle the environment names, empty when none does
func envCABundle() (string, string) {
	for _, name := range caBundleVariables {
		if path := os.Getenv(name); path != "" {
			return name, path
		}
	}
	return "", ""
}

// TLS config whose roots add the bundle, or one failing every handshake with the reason the bundle cannot be used
func caBundleConfig(base *tls.Config, variable, path string, info fs.FileInfo, statErr error) *tls.Config {
	cfg := &tls.Config{}
	if base != nil {
		cfg = base.Clone()
	}
	fail := func(err error) *tls.Config {
		err = fmt.Errorf("cannot use the CA bundle %s names: %w", variable, err)
		cfg.InsecureSkipVerify = true
		cfg.VerifyConnection = func(tls.ConnectionState) error { return err }
		return cfg
	}
	if statErr != nil {
		return fail(statErr)
	}
	var pool *x509.CertPool
	if cfg.RootCAs != nil {
		pool = cfg.RootCAs.Clone()
	} else if system, err := x509.SystemCertPool(); err == nil && system != nil {
		pool = system
	} else {
		pool = x509.NewCertPool()
	}
	if info.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return fail(err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			if raw, err := os.ReadFile(filepath.Join(path, entry.Name())); err == nil {
				pool.AppendCertsFromPEM(raw)
			}
		}
	} else {
		raw, err := os.ReadFile(path)
		if err != nil {
			return fail(err)
		}
		if !pool.AppendCertsFromPEM(raw) {
			return fail(fmt.Errorf("%s holds no PEM certificate", path))
		}
	}
	cfg.RootCAs = pool
	return cfg
}
