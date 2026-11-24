package jsonschemautil

import (
	"net/mail"
	"net/netip"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/idna"
)

// Check of a format the package knows in a draft, nil for any other name
func formatCheck(name string, d Draft) func(string) bool {
	switch name {
	case "date-time":
		return isDateTime
	case "date":
		return isDate
	case "time":
		if d == Draft3 {
			return isClockTime
		}
		return isTime
	case "duration":
		return isDuration
	case "email":
		return isEmail
	case "idn-email":
		return isIDNEmail
	case "hostname", "host-name":
		return isHostname
	case "idn-hostname":
		return isIDNHostname
	case "ipv4", "ip-address":
		return isIPv4
	case "ipv6":
		return isIPv6
	case "uri":
		return isURI
	case "uri-reference":
		return isURIReference
	case "iri":
		return isIRI
	case "iri-reference":
		return isIRIReference
	case "uri-template":
		return isURITemplate
	case "json-pointer":
		return isJSONPointer
	case "relative-json-pointer":
		return isRelativeJSONPointer
	case "uuid":
		return isUUID
	case "regex":
		return validPattern
	}
	return nil
}

// Value of a run of ASCII digits, false when s is empty or holds anything else
func digits(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for i := range len(s) {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// Whether s is an RFC 3339 full-date with a day that exists in its month
func isDate(s string) bool {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	y, okY := digits(s[:4])
	m, okM := digits(s[5:7])
	d, okD := digits(s[8:])
	return okY && okM && okD && m >= 1 && m <= 12 && d >= 1 && d <= daysIn(y, m)
}

func daysIn(year, month int) int {
	switch month {
	case 2:
		if year%4 == 0 && (year%100 != 0 || year%400 == 0) {
			return 29
		}
		return 28
	case 4, 6, 9, 11:
		return 30
	}
	return 31
}

// Whether s is an RFC 3339 full-time: hours, minutes, seconds, an optional fraction and Z or a numeric offset, a leap second only at 23:59:60 UTC
func isTime(s string) bool {
	if len(s) < 9 || s[2] != ':' || s[5] != ':' {
		return false
	}
	h, okH := digits(s[:2])
	m, okM := digits(s[3:5])
	sec, okS := digits(s[6:8])
	if !okH || !okM || !okS || h > 23 || m > 59 || sec > 60 {
		return false
	}
	rest := s[8:]
	if strings.HasPrefix(rest, ".") {
		i := 1
		for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
			i++
		}
		if i == 1 {
			return false
		}
		rest = rest[i:]
	}
	offset := 0
	switch {
	case rest == "Z" || rest == "z":
	case len(rest) == 6 && (rest[0] == '+' || rest[0] == '-') && rest[3] == ':':
		oh, okOH := digits(rest[1:3])
		om, okOM := digits(rest[4:])
		if !okOH || !okOM || oh > 23 || om > 59 {
			return false
		}
		offset = oh*60 + om
		if rest[0] == '-' {
			offset = -offset
		}
	default:
		return false
	}
	if sec == 60 {
		const day = 24 * 60
		utc := ((h*60+m-offset)%day + day) % day
		return utc == 23*60+59
	}
	return true
}

// Whether s is a time of day as draft 3 writes it, hh:mm:ss with no fraction or offset
func isClockTime(s string) bool {
	if len(s) != 8 || s[2] != ':' || s[5] != ':' {
		return false
	}
	h, okH := digits(s[:2])
	m, okM := digits(s[3:5])
	sec, okS := digits(s[6:])
	return okH && okM && okS && h <= 23 && m <= 59 && sec <= 60
}

// Whether s is an RFC 3339 date-time, a T or t between the date and the time
func isDateTime(s string) bool {
	if len(s) < 20 || s[10] != 'T' && s[10] != 't' {
		return false
	}
	return isDate(s[:10]) && isTime(s[11:])
}

// Whether s is an ISO 8601 duration as RFC 3339 appendix A writes it: P, then years, months and days in that order, then T with hours, minutes and seconds, or weeks alone
func isDuration(s string) bool {
	rest, ok := strings.CutPrefix(s, "P")
	if !ok || rest == "" {
		return false
	}
	date, clock, hasT := strings.Cut(rest, "T")
	if hasT && clock == "" {
		return false
	}
	if weeks, ok := strings.CutSuffix(date, "W"); ok && !hasT {
		_, ok := digits(weeks)
		return ok
	}
	return durationUnits(date, "YMD") && durationUnits(clock, "HMS")
}

// Whether s is a run of numbers each followed by one of the units, the units in their order and each at most once
func durationUnits(s, units string) bool {
	next := 0
	for s != "" {
		i := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == 0 || i == len(s) {
			return false
		}
		u := strings.IndexByte(units[next:], s[i])
		if u < 0 {
			return false
		}
		next += u + 1
		s = s[i+1:]
	}
	return true
}

// Whether s is an RFC 5321 mailbox in ASCII
func isEmail(s string) bool {
	for i := range len(s) {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return isIDNEmail(s)
}

// Whether s is a mailbox, its local part and domain possibly in Unicode, with no display name, angle brackets or comments
func isIDNEmail(s string) bool {
	at := strings.LastIndexByte(s, '@')
	if at <= 0 || at == len(s)-1 || strings.TrimSpace(s) != s || strings.ContainsAny(s, "<>") {
		return false
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Name != "" {
		return false
	}
	return strings.HasSuffix(a.Address, s[at:])
}

// Whether s is an RFC 1123 host name: dot separated labels of letters, digits and inner hyphens, at most 63 bytes each and 253 in all, a trailing dot allowed, A-labels valid punycode
func isHostname(s string) bool {
	s = strings.TrimSuffix(s, ".")
	if s == "" || len(s) > 253 {
		return false
	}
	for label := range strings.SplitSeq(s, ".") {
		if !isLabel(label) {
			return false
		}
	}
	return true
}

func isLabel(label string) bool {
	if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for i := range len(label) {
		if c := label[i]; !isAlnum(c) && c != '-' {
			return false
		}
	}
	if len(label) >= 4 && label[2:4] == "--" {
		if !strings.EqualFold(label[:2], "xn") {
			return false
		}
		_, err := idna.Punycode.ToUnicode(label)
		return err == nil
	}
	return true
}

// Whether s is an internationalized host name: one IDNA lookup maps to a valid host name
func isIDNHostname(s string) bool {
	ascii, err := idna.Lookup.ToASCII(s)
	return err == nil && isHostname(ascii)
}

func isIPv4(s string) bool {
	a, err := netip.ParseAddr(s)
	return err == nil && a.Is4()
}

func isIPv6(s string) bool {
	a, err := netip.ParseAddr(s)
	return err == nil && a.Is6() && a.Zone() == ""
}

func isURI(s string) bool { return validURI(s, false, false) }

func isURIReference(s string) bool { return validURI(s, true, false) }

func isIRI(s string) bool { return validURI(s, false, true) }

func isIRIReference(s string) bool { return validURI(s, true, true) }

// Whether s is a URI by RFC 3986, a relative reference also when relative is set, the characters RFC 3987 adds also allowed when iri is set
func validURI(s string, relative, iri bool) bool {
	if !utf8.ValidString(s) {
		return false
	}
	p := parseURI(s)
	if p.scheme == "" {
		if !relative {
			return false
		}
		if first, _, _ := strings.Cut(p.path, "/"); !p.hasAuthority && strings.Contains(first, ":") {
			return false
		}
	}
	if p.hasAuthority && !validAuthority(p.authority, iri) {
		return false
	}
	return validChars(p.path, "/:@", iri) && validChars(p.query, "/?:@", iri) && validChars(p.fragment, "/?:@", iri)
}

// Whether every character of s is unreserved, a sub-delimiter, one of extra, a valid percent escape, or with iri a character RFC 3987 allows
func validChars(s, extra string, iri bool) bool {
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '%':
			if i+2 >= len(s) || !isHex(s[i+1]) || !isHex(s[i+2]) {
				return false
			}
			i += 3
			continue
		case c >= utf8.RuneSelf:
			r, size := utf8.DecodeRuneInString(s[i:])
			if !iri || !ucschar(r) {
				return false
			}
			i += size
			continue
		case isUnreserved(c) || isSubDelim(c) || strings.IndexByte(extra, c) >= 0:
		default:
			return false
		}
		i++
	}
	return true
}

// Whether r is a character RFC 3987 allows unescaped in an IRI, private use ones included
func ucschar(r rune) bool {
	switch {
	case r >= 0xA0 && r <= 0xD7FF, r >= 0xE000 && r <= 0xFDCF, r >= 0xFDF0 && r <= 0xFFEF:
		return true
	case r >= 0x10000 && r <= 0x10FFFD:
		return r&0xFFFE != 0xFFFE
	}
	return false
}

// Whether a URI authority is user info, a host and a port as RFC 3986 spells them
func validAuthority(a string, iri bool) bool {
	host := a
	if i := strings.LastIndexByte(a, '@'); i >= 0 {
		if !validChars(a[:i], ":", iri) {
			return false
		}
		host = a[i+1:]
	}
	port := ""
	if strings.HasPrefix(host, "[") {
		end := strings.IndexByte(host, ']')
		if end < 0 || !validIPLiteral(host[1:end]) {
			return false
		}
		if rest := host[end+1:]; rest != "" {
			if rest[0] != ':' {
				return false
			}
			port = rest[1:]
		}
	} else {
		if i := strings.LastIndexByte(host, ':'); i >= 0 {
			host, port = host[:i], host[i+1:]
		}
		if !validChars(host, "", iri) {
			return false
		}
	}
	for i := range len(port) {
		if port[i] < '0' || port[i] > '9' {
			return false
		}
	}
	return true
}

// Whether the inside of an IP literal is an IPv6 address or an IPvFuture
func validIPLiteral(s string) bool {
	if len(s) > 1 && (s[0] == 'v' || s[0] == 'V') {
		dot := strings.IndexByte(s, '.')
		if dot < 2 || dot == len(s)-1 {
			return false
		}
		for i := 1; i < dot; i++ {
			if !isHex(s[i]) {
				return false
			}
		}
		rest := s[dot+1:]
		return !strings.Contains(rest, "%") && validChars(rest, ":", false)
	}
	a, err := netip.ParseAddr(s)
	return err == nil && a.Is6() && a.Zone() == ""
}

// Whether s is an RFC 6570 URI template: literals and {expressions} of an optional operator and comma separated variables, each with an optional :length or * modifier
func isURITemplate(s string) bool {
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '{':
			end := strings.IndexByte(s[i+1:], '}')
			if end < 0 || !validExpression(s[i+1:i+1+end]) {
				return false
			}
			i += end + 2
			continue
		case c == '%':
			if i+2 >= len(s) || !isHex(s[i+1]) || !isHex(s[i+2]) {
				return false
			}
			i += 3
			continue
		case c >= utf8.RuneSelf:
			r, size := utf8.DecodeRuneInString(s[i:])
			if !ucschar(r) {
				return false
			}
			i += size
			continue
		case c <= ' ' || c == 0x7f || strings.IndexByte("\"'<>\\^`{|}", c) >= 0:
			return false
		}
		i++
	}
	return true
}

func validExpression(e string) bool {
	if e == "" {
		return false
	}
	if strings.IndexByte("+#./;?&", e[0]) >= 0 {
		e = e[1:]
	} else if strings.IndexByte("=,!@|", e[0]) >= 0 {
		return false
	}
	if e == "" {
		return false
	}
	for spec := range strings.SplitSeq(e, ",") {
		name := spec
		if before, ok := strings.CutSuffix(spec, "*"); ok {
			name = before
		} else if before, length, ok := strings.Cut(spec, ":"); ok {
			name = before
			if _, ok := digits(length); !ok || len(length) > 4 || length[0] == '0' {
				return false
			}
		}
		if !validVarname(name) {
			return false
		}
	}
	return true
}

func validVarname(s string) bool {
	if s == "" || s[0] == '.' || s[len(s)-1] == '.' || strings.Contains(s, "..") {
		return false
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '%':
			if i+2 >= len(s) || !isHex(s[i+1]) || !isHex(s[i+2]) {
				return false
			}
			i += 3
			continue
		case c == '_' || c == '.' || isAlnum(c):
		default:
			return false
		}
		i++
	}
	return true
}

// Whether s is an RFC 6901 JSON pointer
func isJSONPointer(s string) bool {
	if s == "" {
		return true
	}
	if s[0] != '/' {
		return false
	}
	for i := range len(s) {
		if s[i] == '~' && (i+1 >= len(s) || s[i+1] != '0' && s[i+1] != '1') {
			return false
		}
	}
	return true
}

// Whether s is a relative JSON pointer: a non-negative integer, an optional signed index adjustment, then # or a JSON pointer
func isRelativeJSONPointer(s string) bool {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 || i > 1 && s[0] == '0' {
		return false
	}
	rest := s[i:]
	if rest != "" && (rest[0] == '+' || rest[0] == '-') {
		j := 1
		for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
			j++
		}
		if j == 1 || j > 2 && rest[1] == '0' {
			return false
		}
		rest = rest[j:]
	}
	return rest == "#" || isJSONPointer(rest)
}

// Whether s is a UUID in its hyphenated hexadecimal form
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := range 36 {
		switch i {
		case 8, 13, 18, 23:
			if s[i] != '-' {
				return false
			}
		default:
			if !isHex(s[i]) {
				return false
			}
		}
	}
	return true
}
