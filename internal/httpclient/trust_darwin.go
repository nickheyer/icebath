package httpclient

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"unsafe"
)

// Security framework status codes mapped to x509 errors
const (
	errSecCertificateExpired = -67818
	errSecNotTrusted         = -67843
)

// Chain the Security framework builds and trusts under the SSL server policy, stapled OCSP and SCTs included
func platformVerify(cs tls.ConnectionState) ([]*x509.Certificate, error) {
	b := loadCF()
	if b.err != nil {
		return nil, b.err
	}
	leaf := cs.PeerCertificates[0]
	certs := b.arrayCreate(0, len(cs.PeerCertificates), b.typeArrayCallbacks)
	if certs == 0 {
		return nil, errors.New("x509: could not allocate the certificate array")
	}
	defer b.release(certs)
	for _, c := range cs.PeerCertificates {
		data := b.data(c.Raw)
		if data == 0 {
			return nil, errors.New("x509: could not copy a peer certificate")
		}
		sec := b.certificateCreate(0, data)
		b.release(data)
		if sec == 0 {
			return nil, errors.New("x509: Security framework rejected a peer certificate encoding")
		}
		b.arrayAppend(certs, sec)
		b.release(sec)
	}
	policy := b.policySSL(true, 0)
	if policy == 0 {
		return nil, errors.New("x509: Security framework could not create the SSL policy")
	}
	defer b.release(policy)
	var trust uintptr
	if status := b.trustCreate(certs, policy, &trust); status != 0 || trust == 0 {
		return nil, fmt.Errorf("x509: SecTrustCreateWithCertificates failed with status %d", status)
	}
	defer b.release(trust)
	if len(cs.OCSPResponse) > 0 {
		if data := b.data(cs.OCSPResponse); data != 0 {
			b.trustSetOCSP(trust, data)
			b.release(data)
		}
	}
	if len(cs.SignedCertificateTimestamps) > 0 {
		if scts := b.arrayCreate(0, len(cs.SignedCertificateTimestamps), b.typeArrayCallbacks); scts != 0 {
			for _, sct := range cs.SignedCertificateTimestamps {
				if data := b.data(sct); data != 0 {
					b.arrayAppend(scts, data)
					b.release(data)
				}
			}
			b.trustSetSCTs(trust, scts)
			b.release(scts)
		}
	}
	var failure uintptr
	if !b.trustEvaluateWithErr(trust, &failure) {
		return nil, b.trustFailure(leaf, failure)
	}
	built := b.trustCopyChain(trust)
	if built == 0 {
		return nil, errors.New("x509: Security framework returned no certificate chain")
	}
	defer b.release(built)
	chain := make([]*x509.Certificate, 0, b.arrayCount(built))
	for i := 0; i < b.arrayCount(built); i++ {
		der := b.certificateCopyData(b.arrayAt(built, i))
		if der == 0 {
			return nil, errors.New("x509: Security framework returned an unreadable chain certificate")
		}
		raw := bytes.Clone(unsafe.Slice(b.dataBytes(der), b.dataLength(der)))
		b.release(der)
		cert, err := x509.ParseCertificate(raw)
		if err != nil {
			return nil, fmt.Errorf("x509: parsing the chain the Security framework built: %w", err)
		}
		chain = append(chain, cert)
	}
	return chain, nil
}

// x509 error of a failed evaluation, releasing the CFError it owns
func (b *cfBinding) trustFailure(leaf *x509.Certificate, failure uintptr) error {
	if failure == 0 {
		return x509.UnknownAuthorityError{Cert: leaf}
	}
	defer b.release(failure)
	code := b.errorCode(failure)
	description := ""
	if text := b.errorDescription(failure); text != 0 {
		description = b.goString(text)
		b.release(text)
	}
	switch code {
	case errSecCertificateExpired:
		return x509.CertificateInvalidError{Cert: leaf, Reason: x509.Expired, Detail: description}
	case errSecNotTrusted:
		return x509.UnknownAuthorityError{Cert: leaf}
	}
	return fmt.Errorf("x509: Security framework rejected the chain with status %d %s", code, description)
}
