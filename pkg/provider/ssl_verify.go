package provider

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
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
)

// Well known Linux bundle files, read directly so an SSL_CERT_FILE override is unioned rather than substituted
var linuxRootBundles = []string{
	"/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt", "/etc/ssl/ca-bundle.pem",
	"/etc/pki/tls/cacert.pem", "/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", "/etc/ssl/cert.pem",
}

var linuxRootDirs = []string{"/etc/ssl/certs", "/etc/pki/tls/certs", "/system/etc/security/cacerts"}

// InstallTrustStore loads the OS trust store once, false when no platform root could be read
func InstallTrustStore() bool {
	trustMu.Lock()
	defer trustMu.Unlock()
	if trustInstalled != nil {
		return *trustInstalled
	}
	pool, err := x509.SystemCertPool()
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
		slog.Default().Warn(fmt.Sprintf("platform trust store unavailable (%v); falling back to the runtime's default trust paths. Certificates trusted only by the OS store — a corporate root, for instance — will not verify.", err))
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
		pool = x509.NewCertPool()
	} else {
		pool = platformRoots.Clone()
	}
	if caPath != "" {
		if raw, err := os.ReadFile(caPath); err == nil {
			pool.AppendCertsFromPEM(raw)
		}
	}
	cfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	caConfigs[key] = cfg
	return cfg
}

// PlatformTLSConfig is the process wide client context with an SSL_CERT_FILE bundle unioned on top
func PlatformTLSConfig() *tls.Config {
	InstallTrustStore()
	if ca := strings.TrimSpace(os.Getenv("SSL_CERT_FILE")); ca != "" {
		if abs, err := filepath.Abs(ca); err == nil {
			if info, statErr := os.Stat(abs); statErr == nil && !info.IsDir() {
				return sharedTLSConfig(abs, true)
			}
		}
	}
	return sharedTLSConfig("", false)
}

// DefaultCABundlePath is an on disk CA bundle the platform resolves, re-derived once when a remembered path vanished
func DefaultCABundlePath() string {
	if ca := strings.TrimSpace(os.Getenv("SSL_CERT_FILE")); ca != "" {
		if info, err := os.Stat(ca); err == nil && !info.IsDir() {
			return ca
		}
		return recoverBundleOnce(ca)
	}
	for _, bundle := range linuxRootBundles {
		if info, err := os.Stat(bundle); err == nil && !info.IsDir() {
			return bundle
		}
	}
	return ""
}

// Re-derives a vanished bundle path at most once, reporting it at most once
func recoverBundleOnce(stale string) string {
	staleBundleMu.Lock()
	defer staleBundleMu.Unlock()
	if cached, known := staleBundleKnown[stale]; known {
		if cached == "" {
			return ""
		}
		if info, err := os.Stat(cached); err == nil && !info.IsDir() {
			return cached
		}
	}
	recovered := ""
	for _, bundle := range linuxRootBundles {
		if info, err := os.Stat(bundle); err == nil && !info.IsDir() {
			recovered = bundle
			break
		}
	}
	staleBundleKnown[stale] = recovered
	if recovered != "" {
		slog.Default().Warn(fmt.Sprintf("TLS trust: CA bundle %s no longer exists — re-resolved to %s. TLS clients built before this point used the dead path.", stale, recovered))
		return recovered
	}
	slog.Default().Warn(fmt.Sprintf("TLS trust: CA bundle %s no longer exists and no system bundle could be re-resolved; using the platform trust store for the rest of this process.", stale))
	return ""
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
	certs := platformRootCertificates()
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
	if bundle := DefaultCABundlePath(); bundle != "" {
		return bundle
	}
	if dir := strings.TrimSpace(os.Getenv("SSL_CERT_DIR")); dir != "" {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
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
			if abs, err := filepath.Abs(path); err == nil {
				path = abs
			}
			return TLSVerify{Config: sharedTLSConfig(path, false)}
		}
		log.Warn(fmt.Sprintf("ssl_ca_cert path does not exist: %s — using the OS trust store instead", effective))
	}
	return TLSVerify{Config: sharedTLSConfig("", false)}
}
