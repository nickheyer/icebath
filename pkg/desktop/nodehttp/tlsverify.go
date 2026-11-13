package nodehttp

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"
)

var (
	oidSubjectAltName = asn1.ObjectIdentifier{2, 5, 29, 17}
	oidCommonName     = asn1.ObjectIdentifier{2, 5, 4, 3}
)

// OpenSSL short names of the name attributes a DirName entry prints
var shortNames = map[string]string{
	"2.5.4.3": "CN", "2.5.4.6": "C", "2.5.4.7": "L", "2.5.4.8": "ST", "2.5.4.10": "O", "2.5.4.11": "OU",
	"2.5.4.5": "serialNumber", "2.5.4.9": "street", "2.5.4.17": "postalCode", "1.2.840.113549.1.9.1": "emailAddress",
	"0.9.2342.19200300.100.1.25": "DC", "0.9.2342.19200300.100.1.1": "UID",
}

// Peer verification of a Node TLS client, the OpenSSL chain result first and checkServerIdentity second
func verifyConnection(host string) func(tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return &Error{Code: "UNABLE_TO_GET_ISSUER_CERT_LOCALLY", Message: "unable to get local issuer certificate"}
		}
		if err := verifyChain(cs.PeerCertificates, time.Now()); err != nil {
			return err
		}
		return checkServerIdentity(host, cs.PeerCertificates[0])
	}
}

// OpenSSL verify result of a presented chain, the error reported last winning as SSL_get_verify_result gives it
func verifyChain(certs []*x509.Certificate, now time.Time) error {
	for _, c := range certs {
		if now.Before(c.NotBefore) {
			return &Error{Code: "CERT_NOT_YET_VALID", Message: "certificate is not yet valid"}
		}
		if now.After(c.NotAfter) {
			return &Error{Code: "CERT_HAS_EXPIRED", Message: "certificate has expired"}
		}
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	_, err := certs[0].Verify(x509.VerifyOptions{Intermediates: inter, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	if err == nil {
		return nil
	}
	var unknown x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var roots x509.SystemRootsError
	var weak x509.InsecureAlgorithmError
	var critical x509.UnhandledCriticalExtension
	switch {
	case errors.As(err, &weak):
		return &Error{Code: "CA_MD_TOO_WEAK", Message: "CA signature digest algorithm too weak", Err: err}
	case errors.As(err, &critical):
		return &Error{Code: "UNHANDLED_CRITICAL_EXTENSION", Message: "unhandled critical extension", Err: err}
	case errors.As(err, &unknown), errors.As(err, &roots):
		top := certs[len(certs)-1]
		switch {
		case len(certs) == 1 && selfIssued(top):
			return &Error{Code: "DEPTH_ZERO_SELF_SIGNED_CERT", Message: "self-signed certificate", Err: err}
		case len(certs) == 1:
			return &Error{Code: "UNABLE_TO_VERIFY_LEAF_SIGNATURE", Message: "unable to verify the first certificate", Err: err}
		case selfIssued(top):
			return &Error{Code: "SELF_SIGNED_CERT_IN_CHAIN", Message: "self-signed certificate in certificate chain", Err: err}
		}
		return &Error{Code: "UNABLE_TO_GET_ISSUER_CERT_LOCALLY", Message: "unable to get local issuer certificate", Err: err}
	case errors.As(err, &invalid):
		switch invalid.Reason {
		case x509.NotAuthorizedToSign:
			return &Error{Code: "INVALID_CA", Message: "invalid CA certificate", Err: err}
		case x509.TooManyIntermediates:
			return &Error{Code: "CERT_CHAIN_TOO_LONG", Message: "certificate chain too long", Err: err}
		case x509.IncompatibleUsage:
			return &Error{Code: "INVALID_PURPOSE", Message: "unsupported certificate purpose", Err: err}
		case x509.CANotAuthorizedForThisName, x509.NameConstraintsWithoutSANs:
			return &Error{Code: "PERMITTED_VIOLATION", Message: "permitted subtree violation", Err: err}
		case x509.UnconstrainedName:
			return &Error{Code: "UNSUPPORTED_NAME_SYNTAX", Message: "unsupported or invalid name syntax", Err: err}
		case x509.NameMismatch:
			return &Error{Code: "SUBJECT_ISSUER_MISMATCH", Message: "subject issuer mismatch", Err: err}
		case x509.Expired:
			return &Error{Code: "CERT_HAS_EXPIRED", Message: "certificate has expired", Err: err}
		}
	}
	return &Error{Code: "CERT_SIGNATURE_FAILURE", Message: "certificate signature failure", Err: err}
}

// True when the certificate names itself as issuer with no conflicting key identifiers
func selfIssued(c *x509.Certificate) bool {
	if !bytes.Equal(c.RawSubject, c.RawIssuer) {
		return false
	}
	return len(c.AuthorityKeyId) == 0 || len(c.SubjectKeyId) == 0 || bytes.Equal(c.AuthorityKeyId, c.SubjectKeyId)
}

// Node tls.checkServerIdentity of host against the leaf certificate
func checkServerIdentity(host string, cert *x509.Certificate) error {
	altNames := subjectAltName(cert)
	var dnsNames, ips []string
	for _, name := range splitAltNames(altNames) {
		switch {
		case strings.HasPrefix(name, "DNS:"):
			dnsNames = append(dnsNames, name[4:])
		case strings.HasPrefix(name, "IP Address:"):
			ips = append(ips, canonicalizeIP(name[11:]))
		}
	}
	var cns []string
	for _, n := range cert.Subject.Names {
		if n.Type.Equal(oidCommonName) {
			cns = append(cns, fmt.Sprint(n.Value))
		}
	}
	hostname := strings.TrimSuffix(host, ".")
	valid := false
	reason := ""
	switch {
	case net.ParseIP(hostname) != nil:
		want := canonicalizeIP(hostname)
		for _, ip := range ips {
			valid = valid || ip == want
		}
		if !valid {
			reason = "IP: " + hostname + " is not in the cert's list: " + strings.Join(ips, ", ")
		}
	case len(dnsNames) > 0 || len(cns) > 0:
		parts := strings.Split(asciiLower(hostname), ".")
		if len(dnsNames) > 0 {
			for _, p := range dnsNames {
				valid = valid || matchHost(parts, p)
			}
			if !valid {
				reason = "Host: " + hostname + ". is not in the cert's altnames: " + altNames
			}
		} else {
			for _, p := range cns {
				valid = valid || matchHost(parts, p)
			}
			if !valid {
				reason = "Host: " + hostname + ". is not cert's CN: " + strings.Join(cns, ",")
			}
		}
	default:
		reason = "Cert does not contain a DNS name"
	}
	if valid {
		return nil
	}
	return &Error{Code: "ERR_TLS_CERT_ALTNAME_INVALID", Message: "Hostname/IP does not match certificate's altnames: " + reason}
}

// Lower cases the ASCII capitals only, as the host split of tls.js does
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return r
	}, s)
}

// Node wildcard match of host labels against one certificate name
func matchHost(hostParts []string, pattern string) bool {
	if pattern == "" {
		return false
	}
	patternParts := strings.Split(pattern, ".")
	if len(hostParts) != len(patternParts) {
		return false
	}
	for _, p := range patternParts {
		if p == "" {
			return false
		}
		for _, r := range p {
			if r < 0x21 || r > 0x7f {
				return false
			}
		}
	}
	for i := len(hostParts) - 1; i > 0; i-- {
		if hostParts[i] != patternParts[i] {
			return false
		}
	}
	hostSub, patternSub := hostParts[0], patternParts[0]
	pieces := strings.Split(patternSub, "*")
	if len(pieces) == 1 || strings.Contains(patternSub, "xn--") {
		return hostSub == patternSub
	}
	if len(pieces) > 2 || len(patternParts) <= 2 {
		return false
	}
	prefix, suffix := pieces[0], pieces[1]
	if len(prefix)+len(suffix) > len(hostSub) {
		return false
	}
	return strings.HasPrefix(hostSub, prefix) && strings.HasSuffix(hostSub, suffix)
}

// Canonical text of an IP address as inet_pton then inet_ntop give it, the input when it does not parse
func canonicalizeIP(s string) string {
	if a, err := netip.ParseAddr(s); err == nil {
		return a.String()
	}
	return s
}

// Splits a subjectaltname string on commas outside the quoted names Node escapes
func splitAltNames(altNames string) []string {
	if altNames == "" {
		return nil
	}
	if !strings.Contains(altNames, `"`) {
		return strings.Split(altNames, ", ")
	}
	var out []string
	var cur strings.Builder
	quoted := false
	for i := 0; i < len(altNames); i++ {
		c := altNames[i]
		switch {
		case quoted && c == '\\' && i+1 < len(altNames):
			if altNames[i+1] == 'u' && i+5 < len(altNames) {
				var v int
				fmt.Sscanf(altNames[i+2:i+6], "%04x", &v)
				cur.WriteRune(rune(v))
				i += 5
			} else {
				cur.WriteByte(altNames[i+1])
				i++
			}
		case c == '"':
			quoted = !quoted
		case !quoted && c == ',' && i+1 < len(altNames) && altNames[i+1] == ' ':
			out = append(out, cur.String())
			cur.Reset()
			i++
		default:
			cur.WriteByte(c)
		}
	}
	return append(out, cur.String())
}

// subjectaltname text Node prints for a certificate, entries in extension order joined by comma and space
func subjectAltName(cert *x509.Certificate) string {
	var raw []byte
	for _, ext := range cert.Extensions {
		if ext.Id.Equal(oidSubjectAltName) {
			raw = ext.Value
		}
	}
	if raw == nil {
		return ""
	}
	var seq asn1.RawValue
	if rest, err := asn1.Unmarshal(raw, &seq); err != nil || len(rest) != 0 {
		return ""
	}
	var parts []string
	rest := seq.Bytes
	for len(rest) > 0 {
		var v asn1.RawValue
		var err error
		if rest, err = asn1.Unmarshal(rest, &v); err != nil {
			break
		}
		switch v.Tag {
		case 1:
			parts = append(parts, printAltName(v.Bytes, "email", false))
		case 2:
			parts = append(parts, printAltName(v.Bytes, "DNS", false))
		case 6:
			parts = append(parts, printAltName(v.Bytes, "URI", false))
		case 7:
			parts = append(parts, "IP Address:"+printIP(v.Bytes))
		case 8:
			var oid asn1.ObjectIdentifier
			if _, err := asn1.Unmarshal(append([]byte{0x06, byte(len(v.Bytes))}, v.Bytes...), &oid); err == nil {
				parts = append(parts, "Registered ID:"+oid.String())
			}
		case 4:
			parts = append(parts, "DirName:"+printDirName(v.Bytes))
		case 0:
			parts = append(parts, "othername:<unsupported>")
		}
	}
	return strings.Join(parts, ", ")
}

// One alt name as Node PrintAltName writes it, JSON style quoted when it holds unsafe characters
func printAltName(name []byte, prefix string, utf8 bool) string {
	safe := true
	for _, c := range name {
		if c == '"' || c == '\\' || c == ',' || c == '\'' || c < ' ' || (!utf8 && c > '~') || (utf8 && c == 0x7f) {
			safe = false
			break
		}
	}
	if safe {
		return prefix + ":" + string(name)
	}
	var b strings.Builder
	b.WriteString(`"` + prefix + ":")
	for _, c := range name {
		switch {
		case c == '\\':
			b.WriteString(`\\`)
		case c == '"':
			b.WriteString(`\"`)
		case (c >= ' ' && c != ',' && c <= '~') || (utf8 && c&0x80 != 0):
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, `\u00%02x`, c)
		}
	}
	b.WriteString(`"`)
	return b.String()
}

// IP address of a SAN entry as OpenSSL prints it, IPv6 as eight upper case hex groups
func printIP(b []byte) string {
	switch len(b) {
	case 4:
		return net.IP(b).String()
	case 16:
		groups := make([]string, 8)
		for i := range groups {
			groups[i] = fmt.Sprintf("%X", int(b[2*i])<<8|int(b[2*i+1]))
		}
		return strings.Join(groups, ":")
	}
	return fmt.Sprintf("<invalid length=%d>", len(b))
}

// Directory name of a SAN entry in the OpenSSL one line form
func printDirName(raw []byte) string {
	var rdns [][]struct {
		Type  asn1.ObjectIdentifier
		Value any
	}
	if _, err := asn1.Unmarshal(raw, &rdns); err != nil {
		return "<invalid>"
	}
	var parts []string
	for _, set := range rdns {
		for _, atv := range set {
			name := shortNames[atv.Type.String()]
			if name == "" {
				name = atv.Type.String()
			}
			parts = append(parts, name+" = "+fmt.Sprint(atv.Value))
		}
	}
	return strings.Join(parts, ", ")
}
