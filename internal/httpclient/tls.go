package httpclient

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Trust and identity toward targets, with proxies always verified against the OS store and the environment bundles
type TLSOptions struct {
	CAFiles      []string
	RootCAFile   string
	Insecure     bool
	Certificates []tls.Certificate
}

// Variable naming a list of CA directories trusted on top of the OS store
const ambientDirsKey = "SSL_CERT_DIR"

// Returned inside a BundleError when a bundle holds no PEM certificate
var ErrEmptyBundle = errors.New("no PEM certificates found")

// Returned inside a BundleError when a configured bundle path names no regular file
var ErrNoBundleFile = errors.New("no CA bundle file at this path")

// CA bundle that could not be read or held no certificates, with the variable that named it when ambient
type BundleError struct {
	Source string
	Path   string
	Err    error
}

func (e *BundleError) Error() string {
	if e.Source != "" {
		return fmt.Sprintf("CA bundle %s from %s: %v", e.Path, e.Source, e.Err)
	}
	return fmt.Sprintf("CA bundle %s: %v", e.Path, e.Err)
}

func (e *BundleError) Unwrap() error { return e.Err }

// Verification resolved once per transport and stamped onto a config bound to each dialed host
type trust struct {
	roots    *x509.CertPool
	verify   func(cs tls.ConnectionState, host string) error
	insecure bool
	certs    []tls.Certificate
	storeErr error
}

// Client settings verifying the peer as host, with no ALPN so the caller picks its protocols
func (t *trust) config(host string) *tls.Config {
	c := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, Certificates: t.certs}
	switch {
	case t.insecure:
		c.InsecureSkipVerify = true
	case t.verify != nil:
		verify := t.verify
		c.InsecureSkipVerify = true
		c.VerifyConnection = func(cs tls.ConnectionState) error { return verify(cs, host) }
	default:
		c.RootCAs = t.roots
	}
	return c
}

// Handshake failure with the unreadable OS store named when the chain found no trusted root
func (t *trust) explain(err error) error {
	if t.storeErr == nil {
		return err
	}
	var unknown x509.UnknownAuthorityError
	if !errors.As(err, &unknown) {
		return err
	}
	return fmt.Errorf("%w, and the operating system trust store could not be read: %w", err, t.storeErr)
}

// Trust toward targets and toward HTTPS proxies
func buildTrust(o TLSOptions) (*trust, *trust, error) {
	ambient, err := ambientCertificates()
	if err != nil {
		return nil, nil, err
	}
	proxy := systemTrust(ambient)
	var target *trust
	switch {
	case o.Insecure:
		target = &trust{insecure: true}
	case o.RootCAFile != "":
		certs, err := loadBundleFile(o.RootCAFile)
		if err != nil {
			return nil, nil, err
		}
		pool := x509.NewCertPool()
		for _, c := range certs {
			pool.AddCert(c)
		}
		target = &trust{roots: pool}
	default:
		extra := ambient
		for _, path := range o.CAFiles {
			certs, err := loadBundleFile(path)
			if err != nil {
				return nil, nil, err
			}
			extra = append(extra, certs...)
		}
		target = systemTrust(extra)
	}
	target.certs = o.Certificates
	return target, proxy, nil
}

// Certificates of an explicitly configured bundle file, a missing path or a directory reported as ErrNoBundleFile
func loadBundleFile(path string) ([]*x509.Certificate, error) {
	expanded := expandHome(path)
	info, err := os.Stat(expanded)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, &BundleError{Path: path, Err: fmt.Errorf("%w, %w", ErrNoBundleFile, err)}
		}
		return nil, &BundleError{Path: path, Err: err}
	}
	if !info.Mode().IsRegular() {
		return nil, &BundleError{Path: path, Err: ErrNoBundleFile}
	}
	certs, err := loadPEMFile(expanded)
	if err != nil {
		return nil, &BundleError{Path: path, Err: err}
	}
	return certs, nil
}

// Certificates of every environment bundle naming an existing path, missing paths skipped
func ambientCertificates() ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	for _, v := range ambientBundles {
		path := envValue(v.key)
		if path == "" {
			continue
		}
		info, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, &BundleError{Source: v.key, Path: path, Err: err}
		}
		if info.IsDir() {
			if v.dir {
				out = append(out, loadPEMDir(path)...)
			}
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		certs, err := loadPEMFile(path)
		if err != nil {
			return nil, &BundleError{Source: v.key, Path: path, Err: err}
		}
		out = append(out, certs...)
	}
	for _, dir := range filepath.SplitList(envValue(ambientDirsKey)) {
		if dir = strings.TrimSpace(dir); dir == "" {
			continue
		}
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			out = append(out, loadPEMDir(dir)...)
		}
	}
	return out, nil
}

// One environment variable naming a CA bundle, and whether a directory there is read as a bundle
type ambientBundle struct {
	key string
	dir bool
}

// Bundle variables read when each transport is built, since the cached system pool never sees later changes
var ambientBundles = []ambientBundle{{key: "ICEBATH_CA_BUNDLE"}, {key: "SSL_CERT_FILE"}, {key: "REQUESTS_CA_BUNDLE", dir: true}, {key: "CURL_CA_BUNDLE", dir: true}}

// Certificates of a PEM bundle file, an error when it holds none or one fails to parse
func loadPEMFile(path string) ([]*x509.Certificate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	certs, err := parsePEMCertificates(raw)
	if err != nil {
		return nil, err
	}
	if len(certs) == 0 {
		return nil, ErrEmptyBundle
	}
	return certs, nil
}

// Certificates of every readable PEM file directly inside a directory, unreadable and malformed files skipped
func loadPEMDir(dir string) []*x509.Certificate {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []*x509.Certificate
	for _, entry := range entries {
		file := filepath.Join(dir, entry.Name())
		if st, err := os.Stat(file); err != nil || !st.Mode().IsRegular() {
			continue
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		if certs, err := parsePEMCertificates(raw); err == nil {
			out = append(out, certs...)
		}
	}
	return out
}

// Parsed CERTIFICATE blocks of PEM text, other block types ignored
func parsePEMCertificates(raw []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	for {
		block, rest := pem.Decode(raw)
		if block == nil {
			return out, nil
		}
		raw = rest
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parsing certificate %d: %w", len(out)+1, err)
		}
		out = append(out, cert)
	}
}

// Path with a leading tilde replaced by the home directory, the backslash form only where it separates paths
func expandHome(path string) string {
	path = strings.TrimSpace(path)
	tilde := path == "~" || strings.HasPrefix(path, "~/") || (filepath.Separator == '\\' && strings.HasPrefix(path, `~\`))
	if !tilde {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, path[1:])
}
