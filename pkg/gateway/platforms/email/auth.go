package email

import (
	"net/mail"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Longest From value parsed and the reasons and hints sender authentication logs
const (
	maxFromLen = 2048

	noAuthResultsReason    = "no Authentication-Results header"
	untrustedAuthservReason = "no Authentication-Results from trusted authserv-id"
	missingAuthservReason  = "authserv-id is not configured; refusing to trust Authentication-Results"
	optOutHint             = "set platforms.email.require_authenticated_sender: false (or EMAIL_TRUST_FROM_HEADER=true) to accept the risk."
	missingAuthservHint    = " Set EMAIL_AUTHSERV_ID (or platforms.email.authserv_id) to the receiving MTA's exact authserv-id, or " + optOutHint
)

// Operator fixable reasons a granted sender's mail fails authentication, with the fix each log line names
var dropHints = map[string]string{
	noAuthResultsReason:     " If your mail server does not stamp Authentication-Results, " + optOutHint,
	untrustedAuthservReason: " Check that platforms.email.authserv_id (EMAIL_AUTHSERV_ID) names your mail server.",
}

// Address substrings of automated senders that are ignored
var noreplyPatterns = []string{"noreply", "no-reply", "no_reply", "donotreply", "do-not-reply", "mailer-daemon", "postmaster",
	"bounce", "notifications@", "automated@", "auto-confirm", "auto-reply", "automailer"}

const quotedString = `"(?:[^"\\]|\\.)*"`

var (
	foldRE              = regexp.MustCompile(`\r?\n[ \t]+`)
	singleBracketFromRE = regexp.MustCompile(`^([^"<>]*)<([^<>\s]+)>\s*$`)
	commentRE           = regexp.MustCompile(`\([^()]*\)`)
	authMethodRE        = regexp.MustCompile(`(?i)^\s*(dmarc|dkim|spf)\s*=\s*([a-z]+)`)
	authPropRE          = regexp.MustCompile(`(?i)(header\.from|header\.d|smtp\.mailfrom|smtp\.from|envelope-from)\s*=\s*((?:` +
		quotedString + `|[^\s";])+)|(?:` + quotedString + `|[^\s"])+`)
	addressParser = &mail.AddressParser{WordDecoder: wordDecoder}
)

// Whether the address or the bulk mail headers mark the message as automated
func isAutomatedSender(address string, h mail.Header) bool {
	addr := strings.ToLower(address)
	for _, p := range noreplyPatterns {
		if strings.Contains(addr, p) {
			return true
		}
	}
	for _, v := range h["Auto-Submitted"] {
		if v != "" && strings.ToLower(v) != "no" {
			return true
		}
	}
	for _, v := range h["Precedence"] {
		if lv := strings.ToLower(v); lv == "bulk" || lv == "list" || lv == "junk" {
			return true
		}
	}
	for _, name := range []string{"X-Auto-Response-Suppress", "List-Unsubscribe"} {
		for _, v := range h[name] {
			if v != "" {
				return true
			}
		}
	}
	return false
}

// Comments removed innermost first until nothing changes
func stripComments(text string) string {
	for {
		stripped := commentRE.ReplaceAllString(text, " ")
		if stripped == text {
			return text
		}
		text = stripped
	}
}

// Bare lowercased mailbox of a From value, empty when it holds no single unambiguous address
func extractEmailAddress(raw string) string {
	value := foldRE.ReplaceAllString(raw, " ")
	if utf8.RuneCountInString(value) > maxFromLen || strings.Count(value, "(") > 64 {
		return ""
	}
	addr := ""
	if parsed, err := addressParser.Parse(value); err == nil {
		addr = parsed.Address
	}
	if addr == "" {
		if m := singleBracketFromRE.FindStringSubmatch(value); m != nil {
			display, bracketed := strings.TrimSpace(stripComments(m[1])), m[2]
			if len(bracketed) > 2 && strings.Contains(bracketed[1:len(bracketed)-1], "@") && !strings.ContainsAny(display, ";:()") &&
				(!strings.Contains(display, "@") || strings.EqualFold(display, bracketed)) {
				addr = bracketed
			}
		}
	}
	addr = strings.ToLower(strings.TrimSpace(addr))
	if !strings.Contains(addr, "@") {
		return ""
	}
	return addr
}

// Clauses of an Authentication-Results value split outside quotes and comments, false when one is unbalanced
func arClauses(text string) ([]string, bool) {
	var clauses []string
	var cur strings.Builder
	depth, quoted := 0, false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c == '\\' && (quoted || depth > 0) {
			if quoted {
				cur.WriteString(text[i:min(i+2, len(text))])
			}
			i++
			continue
		}
		switch {
		case quoted:
			quoted = c != '"'
			cur.WriteByte(c)
		case depth > 0:
			switch c {
			case '(':
				depth++
			case ')':
				depth--
			}
			if depth == 0 {
				cur.WriteByte(' ')
			}
		case c == '(':
			depth = 1
		case c == '"':
			quoted = true
			cur.WriteByte(c)
		case c == ')':
			return nil, false
		case c == ';':
			clauses = append(clauses, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if quoted || depth > 0 {
		return nil, false
	}
	return append(clauses, cur.String()), true
}

// Property and value pairs of one comment free clause, quoted string contents never read as properties
func authProps(clause string) [][2]string {
	var props [][2]string
	for _, m := range authPropRE.FindAllStringSubmatch(clause, -1) {
		if m[1] != "" {
			props = append(props, [2]string{strings.ToLower(m[1]), strings.Trim(m[2], `"`)})
		}
	}
	return props
}

// Lowercased domain part of an address
func domainOf(address string) string {
	if i := strings.LastIndexByte(address, '@'); i >= 0 {
		address = address[i+1:]
	}
	return strings.ToLower(strings.TrimSpace(address))
}

// Relaxed DMARC alignment, equal domains or one a dot suffix of the other
func domainsAligned(a, b string) bool {
	a = strings.TrimRight(strings.ToLower(strings.TrimSpace(a)), ".")
	b = strings.TrimRight(strings.ToLower(strings.TrimSpace(b)), ".")
	return a != "" && b != "" && (a == b || strings.HasSuffix(a, "."+b) || strings.HasSuffix(b, "."+a))
}

// One method verdict with the properties of its own clause
type authResult struct {
	verdict string
	props   [][2]string
}

// Whether every domain the named properties carry aligns with the From domain, some being required unless optional
func alignedProps(props [][2]string, names []string, fromDomain string, required bool) bool {
	found := false
	for _, p := range props {
		for _, n := range names {
			if p[0] != n {
				continue
			}
			found = true
			if !domainsAligned(domainOf(p[1]), fromDomain) {
				return false
			}
		}
	}
	return found || !required
}

// Whether the receiving server pinned by authserv id authenticated the From domain, with the reason
func verifySenderAuthentication(h mail.Header, fromAddr, authservID string) (bool, string) {
	fromDomain := domainOf(fromAddr)
	if fromDomain == "" {
		return false, "missing From domain"
	}
	values := h["Authentication-Results"]
	if len(values) == 0 {
		return false, noAuthResultsReason
	}
	trusted := strings.Join(strings.Fields(values[0]), " ")
	clauses, ok := arClauses(trusted)
	if !ok {
		return false, "unbalanced quote or comment in Authentication-Results"
	}
	if authservID == "" {
		return false, missingAuthservReason
	}
	if strings.ToLower(strings.TrimSpace(clauses[0])) != authservID {
		return false, untrustedAuthservReason
	}
	results := map[string][]authResult{}
	for _, clause := range clauses {
		if m := authMethodRE.FindStringSubmatch(clause); m != nil {
			method := strings.ToLower(m[1])
			results[method] = append(results[method], authResult{strings.ToLower(m[2]), authProps(clause)})
		}
	}
	if len(results["dmarc"]) > 1 {
		return false, "ambiguous dmarc result"
	}
	for _, r := range results["dmarc"] {
		if r.verdict == "pass" && alignedProps(r.props, []string{"header.from"}, fromDomain, false) {
			return true, "dmarc=pass"
		}
	}
	if spf := results["spf"]; len(spf) == 1 && spf[0].verdict == "pass" &&
		alignedProps(spf[0].props, []string{"smtp.mailfrom", "smtp.from", "envelope-from"}, fromDomain, true) {
		return true, "spf=pass aligned"
	}
	for _, r := range results["dkim"] {
		names := []string{"header.from"}
		for _, p := range r.props {
			if p[0] == "header.d" {
				names = []string{"header.d"}
				break
			}
		}
		if r.verdict == "pass" && alignedProps(r.props, names, fromDomain, true) {
			return true, "dkim=pass aligned"
		}
	}
	runes := []rune(trusted)
	return false, "authentication failed (" + string(runes[:min(len(runes), 120)]) + ")"
}
