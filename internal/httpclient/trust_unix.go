//go:build !darwin && !windows

package httpclient

import "crypto/x509"

// Standard verification against the distribution bundle files and directories with the extra certificates added
func systemTrust(extra []*x509.Certificate) *trust {
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
