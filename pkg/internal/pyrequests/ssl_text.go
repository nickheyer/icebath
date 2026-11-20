package pyrequests

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"strings"
)

// Whether a TLS failure is a certificate the client refused, every chain, name and validity check the handshake runs
func CertificateVerifyFailed(err error) bool {
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

// str of the ssl module exception a failed handshake raises, the strerror its repr carries
func SSLErrorText(err error) string {
	repr := sslErrorRepr(nil, err)
	_, rest, found := strings.Cut(repr, ", ")
	if !found || len(rest) < 3 {
		return repr
	}
	return rest[1 : len(rest)-2]
}
