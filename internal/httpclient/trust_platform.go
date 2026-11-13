//go:build darwin || windows

package httpclient

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"slices"
)

// Platform verification with the extra certificates trusted alongside the OS store
func systemTrust(extra []*x509.Certificate) *trust {
	if !certVarsPresent() {
		pool, err := x509.SystemCertPool()
		t := &trust{roots: pool}
		if err != nil || pool == nil {
			t.roots, t.storeErr = x509.NewCertPool(), err
		}
		for _, c := range extra {
			t.roots.AddCert(c)
		}
		return t
	}
	var pool *x509.CertPool
	if len(extra) > 0 {
		pool = x509.NewCertPool()
		for _, c := range extra {
			pool.AddCert(c)
		}
	}
	return &trust{verify: func(cs tls.ConnectionState, host string) error { return verifyUnion(cs, host, pool) }}
}

// Reports whether SSL_CERT_FILE or SSL_CERT_DIR is present, which turns the standard verifier away from the OS store
func certVarsPresent() bool {
	_, file := os.LookupEnv("SSL_CERT_FILE")
	_, dir := os.LookupEnv(ambientDirsKey)
	return file || dir
}

// Checks the name, then accepts a chain to an extra root or one the OS trust service builds for servers
func verifyUnion(cs tls.ConnectionState, host string, extra *x509.CertPool) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("tls: server presented no certificate")
	}
	leaf := cs.PeerCertificates[0]
	fail := func(err error) error {
		return &tls.CertificateVerificationError{UnverifiedCertificates: cs.PeerCertificates, Err: err}
	}
	if err := leaf.VerifyHostname(host); err != nil {
		return fail(err)
	}
	if extra != nil {
		intermediates := x509.NewCertPool()
		for _, c := range cs.PeerCertificates[1:] {
			intermediates.AddCert(c)
		}
		opts := x509.VerifyOptions{DNSName: host, Roots: extra, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		if _, err := leaf.Verify(opts); err == nil {
			return nil
		}
	}
	chain, err := platformVerify(cs)
	if err != nil {
		return fail(err)
	}
	if len(chain) == 0 || !chain[0].Equal(leaf) {
		return fail(x509.UnknownAuthorityError{Cert: leaf})
	}
	if !chainAllowsServerAuth(chain) {
		return fail(x509.CertificateInvalidError{Cert: leaf, Reason: x509.IncompatibleUsage})
	}
	return nil
}

// Reports whether every certificate of a chain that limits its extended usages allows server authentication
func chainAllowsServerAuth(chain []*x509.Certificate) bool {
	for _, c := range chain {
		if len(c.ExtKeyUsage) == 0 && len(c.UnknownExtKeyUsage) == 0 {
			continue
		}
		allowed := slices.ContainsFunc(c.ExtKeyUsage, func(u x509.ExtKeyUsage) bool {
			switch u {
			case x509.ExtKeyUsageAny, x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageNetscapeServerGatedCrypto, x509.ExtKeyUsageMicrosoftServerGatedCrypto:
				return true
			}
			return false
		})
		if !allowed {
			return false
		}
	}
	return true
}
