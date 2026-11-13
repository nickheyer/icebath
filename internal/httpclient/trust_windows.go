package httpclient

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Host name check skipped by CryptoAPI since the name is verified before the chain
const securityFlagIgnoreCertCNInvalid = 0x00001000

// Server authentication usage the chain must allow, NUL terminated for CryptoAPI
var serverAuthOID = []byte("1.3.6.1.5.5.7.3.1\x00")

// Chain CryptoAPI builds from the Windows certificate stores and accepts under the SSL server policy
func platformVerify(cs tls.ConnectionState) ([]*x509.Certificate, error) {
	leaf := cs.PeerCertificates[0]
	store, err := windows.CertOpenStore(windows.CERT_STORE_PROV_MEMORY, 0, 0, windows.CERT_STORE_DEFER_CLOSE_UNTIL_LAST_FREE_FLAG, 0)
	if err != nil {
		return nil, fmt.Errorf("x509: opening a memory store: %w", err)
	}
	defer windows.CertCloseStore(store, 0)
	encoding := uint32(windows.X509_ASN_ENCODING | windows.PKCS_7_ASN_ENCODING)
	leafContext, err := windows.CertCreateCertificateContext(encoding, &leaf.Raw[0], uint32(len(leaf.Raw)))
	if err != nil {
		return nil, fmt.Errorf("x509: reading the peer certificate: %w", err)
	}
	defer windows.CertFreeCertificateContext(leafContext)
	var stored *windows.CertContext
	if err := windows.CertAddCertificateContextToStore(store, leafContext, windows.CERT_STORE_ADD_ALWAYS, &stored); err != nil {
		return nil, fmt.Errorf("x509: storing the peer certificate: %w", err)
	}
	defer windows.CertFreeCertificateContext(stored)
	for _, c := range cs.PeerCertificates[1:] {
		ctx, err := windows.CertCreateCertificateContext(encoding, &c.Raw[0], uint32(len(c.Raw)))
		if err != nil {
			return nil, fmt.Errorf("x509: reading an intermediate certificate: %w", err)
		}
		err = windows.CertAddCertificateContextToStore(store, ctx, windows.CERT_STORE_ADD_ALWAYS, nil)
		windows.CertFreeCertificateContext(ctx)
		if err != nil {
			return nil, fmt.Errorf("x509: storing an intermediate certificate: %w", err)
		}
	}
	oids := []*byte{&serverAuthOID[0]}
	para := windows.CertChainPara{}
	para.Size = uint32(unsafe.Sizeof(para))
	para.RequestedUsage.Type = windows.USAGE_MATCH_TYPE_OR
	para.RequestedUsage.Usage.Length = uint32(len(oids))
	para.RequestedUsage.Usage.UsageIdentifiers = &oids[0]
	var chain *windows.CertChainContext
	if err := windows.CertGetCertificateChain(0, stored, nil, stored.Store, &para, 0, 0, &chain); err != nil {
		return nil, fmt.Errorf("x509: building the chain: %w", err)
	}
	defer windows.CertFreeCertificateChain(chain)
	switch status := chain.TrustStatus.ErrorStatus; {
	case status == windows.CERT_TRUST_NO_ERROR:
	case status&windows.CERT_TRUST_IS_NOT_TIME_VALID != 0:
		return nil, x509.CertificateInvalidError{Cert: leaf, Reason: x509.Expired}
	case status&windows.CERT_TRUST_IS_NOT_VALID_FOR_USAGE != 0:
		return nil, x509.CertificateInvalidError{Cert: leaf, Reason: x509.IncompatibleUsage}
	default:
		return nil, x509.UnknownAuthorityError{Cert: leaf}
	}
	ssl := windows.SSLExtraCertChainPolicyPara{AuthType: windows.AUTHTYPE_SERVER, Checks: securityFlagIgnoreCertCNInvalid}
	ssl.Size = uint32(unsafe.Sizeof(ssl))
	policy := windows.CertChainPolicyPara{ExtraPolicyPara: (windows.Pointer)(unsafe.Pointer(&ssl))}
	policy.Size = uint32(unsafe.Sizeof(policy))
	result := windows.CertChainPolicyStatus{}
	result.Size = uint32(unsafe.Sizeof(result))
	if err := windows.CertVerifyCertificateChainPolicy(windows.CERT_CHAIN_POLICY_SSL, chain, &policy, &result); err != nil {
		return nil, fmt.Errorf("x509: applying the SSL policy: %w", err)
	}
	switch windows.Handle(result.Error) {
	case 0:
	case windows.CERT_E_EXPIRED:
		return nil, x509.CertificateInvalidError{Cert: leaf, Reason: x509.Expired}
	case windows.CERT_E_UNTRUSTEDROOT:
		return nil, x509.UnknownAuthorityError{Cert: leaf}
	default:
		return nil, fmt.Errorf("x509: Windows rejected the certificate chain with status 0x%x", result.Error)
	}
	return builtChain(chain)
}

// Certificates of the final simple chain CryptoAPI built, copied out before the chain context is freed
func builtChain(chain *windows.CertChainContext) ([]*x509.Certificate, error) {
	if chain.ChainCount == 0 || chain.Chains == nil {
		return nil, errors.New("x509: CryptoAPI returned no simple chain")
	}
	simple := unsafe.Slice(chain.Chains, chain.ChainCount)[chain.ChainCount-1]
	if simple == nil || simple.NumElements == 0 || simple.Elements == nil {
		return nil, errors.New("x509: CryptoAPI returned an empty simple chain")
	}
	out := make([]*x509.Certificate, 0, simple.NumElements)
	for _, element := range unsafe.Slice(simple.Elements, simple.NumElements) {
		if element == nil || element.CertContext == nil || element.CertContext.EncodedCert == nil {
			return nil, errors.New("x509: CryptoAPI returned a chain element without a certificate")
		}
		raw := bytes.Clone(unsafe.Slice(element.CertContext.EncodedCert, element.CertContext.Length))
		cert, err := x509.ParseCertificate(raw)
		if err != nil {
			return nil, fmt.Errorf("x509: parsing the chain CryptoAPI built: %w", err)
		}
		out = append(out, cert)
	}
	return out, nil
}
