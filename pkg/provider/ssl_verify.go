package provider

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

var (
	trustMu          sync.Mutex
	trustInstalled   *bool
	platformRoots    *x509.CertPool
	caConfigs        = map[string]*tls.Config{}
	platformBundle   string
	staleBundleMu    sync.Mutex
	staleBundleKnown = map[string]string{}
	caBundleMu       sync.Mutex
	caBundleWhere    *string
	reloadCABundle   = reloadCABundleFromSearch
)

// Well known Linux bundle files, read directly so an SSL_CERT_FILE override is unioned rather than substituted
var linuxRootBundles = []string{
	"/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt", "/etc/ssl/ca-bundle.pem",
	"/etc/pki/tls/cacert.pem", "/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", "/etc/ssl/cert.pem",
}

var linuxRootDirs = []string{"/etc/ssl/certs", "/etc/pki/tls/certs", "/system/etc/security/cacerts"}

var systemCertPool = x509.SystemCertPool

// Roots of the OS store as the stdlib context enumerates them, a seam the tests stand a store in for
var platformStoreCertificates = platformRootCertificates

// InstallTrustStore loads the OS trust store once, false when no platform root could be read
func InstallTrustStore() bool {
	trustMu.Lock()
	defer trustMu.Unlock()
	if trustInstalled != nil {
		return *trustInstalled
	}
	pool, err := systemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	loaded := err == nil
	if runtime.GOOS == "linux" {
		for _, bundle := range linuxRootBundles {
			if raw, readErr := os.ReadFile(bundle); readErr == nil && pool.AppendCertsFromPEM(raw) {
				loaded = true
			}
		}
		for _, dir := range linuxRootDirs {
			entries, readErr := os.ReadDir(dir)
			if readErr != nil {
				continue
			}
			for _, entry := range entries {
				if entry.IsDir() {
					continue
				}
				if raw, readErr := os.ReadFile(filepath.Join(dir, entry.Name())); readErr == nil && pool.AppendCertsFromPEM(raw) {
					loaded = true
				}
			}
		}
	}
	platformRoots = pool
	trustInstalled = &loaded
	if loaded {
		slog.Default().Debug("TLS trust: platform store")
	} else {
		slog.Default().Warn(fmt.Sprintf("truststore unavailable (%v); falling back to OpenSSL's default trust paths. Certificates trusted only by the OS store — a corporate root, for instance — will not verify.", err))
	}
	return loaded
}

// ResetTrustStoreForTests forgets the installed store and every cached context
func ResetTrustStoreForTests() {
	trustMu.Lock()
	defer trustMu.Unlock()
	trustInstalled, platformRoots, caConfigs, platformBundle = nil, nil, map[string]*tls.Config{}, ""
}

// CoerceInsecure reads an ssl_verify setting as a request to disable verification
func CoerceInsecure(sslVerify any) bool {
	switch v := sslVerify.(type) {
	case bool:
		return !v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "false", "0", "no", "off":
			return true
		}
	}
	return false
}

// A stable context identity so clients reuse their transport pool, union adds the bundle to the platform store
func sharedTLSConfig(caPath string, union bool) *tls.Config {
	InstallTrustStore()
	trustMu.Lock()
	defer trustMu.Unlock()
	key := fmt.Sprintf("%s|%t", caPath, union)
	if cfg, ok := caConfigs[key]; ok {
		return cfg
	}
	var pool *x509.CertPool
	if caPath != "" && !union {
		pool = stdlibTrustPool()
	} else {
		pool = platformRoots.Clone()
	}
	if caPath != "" {
		raw, err := os.ReadFile(caPath)
		if err != nil {
			return failedTLSConfig(err)
		}
		if !pool.AppendCertsFromPEM(raw) {
			return failedTLSConfig(errNoCertificateFound)
		}
	}
	cfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	caConfigs[key] = cfg
	return cfg
}

var errNoCertificateFound = errors.New("[X509: NO_CERTIFICATE_OR_CRL_FOUND] no certificate or crl found")

// Trust pool with no injected platform verifier, so an explicit bundle replaces OS trust instead of falling back to it
func stdlibTrustPool() *x509.CertPool { return x509.NewCertPool() }

// Fails every handshake with the load error and stays uncached so the next client retries the bundle
func failedTLSConfig(err error) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true, VerifyConnection: func(tls.ConnectionState) error {
		return err
	}}
}

// Absolute path with symlinks resolved, the input path when it cannot be resolved
func resolvedPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// PlatformTLSConfig is the process wide client context with an SSL_CERT_FILE bundle unioned on top
func PlatformTLSConfig() *tls.Config {
	InstallTrustStore()
	if ca := strings.TrimSpace(os.Getenv("SSL_CERT_FILE")); ca != "" && fileExists(ca) {
		return sharedTLSConfig(resolvedPath(ca), true)
	}
	return sharedTLSConfig("", false)
}

// DefaultCABundlePath is the bundle the process resolved on first use, re-derived once when its path vanished
func DefaultCABundlePath() string {
	stale, ok := importCABundle()
	if !ok {
		return ""
	}
	if fileExists(stale) {
		return stale
	}
	return recoverBundleOnce(stale)
}

// Resolves the bundle once and caches it for the process, an unresolved search is retried on the next call
func importCABundle() (string, bool) {
	caBundleMu.Lock()
	defer caBundleMu.Unlock()
	if caBundleWhere != nil {
		return *caBundleWhere, true
	}
	path, ok := searchCABundle()
	if ok {
		caBundleWhere = &path
	}
	return path, ok
}

// First bundle file present on the live candidate list
func searchCABundle() (string, bool) {
	for _, bundle := range linuxRootBundles {
		if fileExists(bundle) {
			return bundle, true
		}
	}
	return "", false
}

// Drops the cached bundle and re-derives it from the live candidates, restoring the old one when none resolves
func reloadCABundleFromSearch() string {
	caBundleMu.Lock()
	defer caBundleMu.Unlock()
	if caBundleWhere == nil {
		return ""
	}
	dropped := caBundleWhere
	caBundleWhere = nil
	path, ok := searchCABundle()
	if !ok {
		caBundleWhere = dropped
		slog.Default().Debug("certifi could not be re-resolved: no CA bundle on the live search path")
		return ""
	}
	caBundleWhere = &path
	if fileExists(path) {
		return path
	}
	return ""
}

// Re-derives a vanished bundle path at most once, reporting it at most once
func recoverBundleOnce(stale string) string {
	staleBundleMu.Lock()
	if cached, known := staleBundleKnown[stale]; known && (cached == "" || fileExists(cached)) {
		staleBundleMu.Unlock()
		return cached
	}
	recovered := reloadCABundle()
	staleBundleKnown[stale] = recovered
	staleBundleMu.Unlock()
	if recovered != "" {
		slog.Default().Warn(fmt.Sprintf("TLS trust: certifi's CA bundle %s no longer exists — the install environment it belongs to was replaced under this process; re-resolved to %s. TLS clients built before this point used the dead path.", stale, recovered))
		return recovered
	}
	slog.Default().Warn(fmt.Sprintf("TLS trust: certifi's CA bundle %s no longer exists and certifi could not be re-resolved; using the platform trust store for the rest of this process.", stale))
	return ""
}

// The resolved bundle trusted the way a default client context trusts it, else the shared platform context
func defaultVerify() *tls.Config {
	bundle := DefaultCABundlePath()
	if bundle == "" {
		return sharedTLSConfig("", false)
	}
	return sharedTLSConfig(bundle, InstallTrustStore())
}

// PlatformStoreBundle writes the OS store roots once to a PEM file under the home cache, empty when none
func PlatformStoreBundle(h string) string {
	InstallTrustStore()
	trustMu.Lock()
	defer trustMu.Unlock()
	if platformBundle != "" {
		if info, err := os.Stat(platformBundle); err == nil && !info.IsDir() {
			return platformBundle
		}
	}
	certs := platformStoreCertificates()
	if len(certs) == 0 {
		return ""
	}
	var buf strings.Builder
	for _, der := range certs {
		_ = pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	if h == "" {
		h = home.Process()
	}
	path := filepath.Join(home.CacheDir(h), "platform-ca-bundle.pem")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		slog.Default().Debug(fmt.Sprintf("platform CA bundle not written: %v", err))
		return ""
	}
	if err := config.AtomicWrite(path, []byte(buf.String()), 0o644); err != nil {
		slog.Default().Debug(fmt.Sprintf("platform CA bundle not written: %v", err))
		return ""
	}
	platformBundle = path
	return platformBundle
}

// DER encoded roots of the platform store read from the system bundle locations
func platformRootCertificates() [][]byte {
	var out [][]byte
	seen := map[string]bool{}
	add := func(raw []byte) {
		for {
			block, rest := pem.Decode(raw)
			if block == nil {
				return
			}
			if block.Type == "CERTIFICATE" {
				key := string(block.Bytes)
				if !seen[key] {
					seen[key] = true
					out = append(out, block.Bytes)
				}
			}
			raw = rest
		}
	}
	if ca := strings.TrimSpace(os.Getenv("SSL_CERT_FILE")); ca != "" {
		if raw, err := os.ReadFile(ca); err == nil {
			add(raw)
		}
	}
	for _, bundle := range linuxRootBundles {
		if raw, err := os.ReadFile(bundle); err == nil {
			add(raw)
		}
	}
	if len(out) == 0 && runtime.GOOS != "linux" {
		if ca := strings.TrimSpace(os.Getenv("SSL_CERT_DIR")); ca != "" {
			if entries, err := os.ReadDir(ca); err == nil {
				for _, entry := range entries {
					if raw, err := os.ReadFile(filepath.Join(ca, entry.Name())); err == nil {
						add(raw)
					}
				}
			}
		}
	}
	return out
}

// SubprocessVerifyBundle is the CA bundle path a child process should verify with, empty keeps its own default
func SubprocessVerifyBundle(h string) string {
	if os.Getenv("REQUESTS_CA_BUNDLE") != "" || os.Getenv("CURL_CA_BUNDLE") != "" {
		return ""
	}
	if frozen, ok := importCABundle(); ok && fileExists(frozen) {
		return ""
	}
	if bundle := DefaultCABundlePath(); bundle != "" {
		return bundle
	}
	for _, key := range []string{"SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if candidate := strings.TrimSpace(os.Getenv(key)); candidate != "" {
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}
	if live, ok := searchCABundle(); ok {
		return live
	}
	return PlatformStoreBundle(h)
}

// TLSVerify is the resolved verification mode for one HTTP client
type TLSVerify struct {
	Insecure bool
	Config   *tls.Config
}

// ResolveTLSVerify picks insecure, an explicit bundle replacing the store, or the platform store for a client
func ResolveTLSVerify(log *slog.Logger, caBundle string, sslVerify any, baseURL string) TLSVerify {
	if log == nil {
		log = slog.Default()
	}
	InstallTrustStore()
	if CoerceInsecure(sslVerify) {
		label := baseURL
		if label == "" {
			label = "a custom provider endpoint"
		}
		log.Warn(fmt.Sprintf("TLS certificate verification DISABLED (ssl_verify: false) for %s — this is intended for local development only and is unsafe on any network you do not fully control.", label))
		return TLSVerify{Insecure: true, Config: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}}
	}
	if effective := strings.TrimSpace(caBundle); effective != "" {
		path := expandTildePath(effective)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return TLSVerify{Config: sharedTLSConfig(resolvedPath(path), false)}
		}
		log.Warn(fmt.Sprintf("ssl_ca_cert path does not exist: %s — using the OS trust store instead", effective))
	}
	if os.Getenv("SSL_CERT_FILE") != "" || os.Getenv("SSL_CERT_DIR") != "" {
		return TLSVerify{Config: sharedTLSConfig("", false)}
	}
	return TLSVerify{Config: defaultVerify()}
}
