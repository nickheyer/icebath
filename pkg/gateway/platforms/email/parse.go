package email

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/mediacache"
	mailutil "github.com/nickheyer/icebath/pkg/utils/mail"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	regexutil "github.com/nickheyer/icebath/pkg/utils/regex"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

var (
	// Address substrings of automated senders that are silently ignored
	noreplyPatterns = []string{"noreply", "no-reply", "no_reply", "donotreply", "do-not-reply", "mailer-daemon", "postmaster",
		"bounce", "notifications@", "automated@", "auto-confirm", "auto-reply", "automailer"}
	// Bulk mail header fields and the test their value passes for an automated sender
	automatedHeaders = []struct {
		name  string
		check func(v string) bool
	}{
		{"Auto-Submitted", func(v string) bool { return strings.ToLower(v) != "no" }},
		{"Precedence", func(v string) bool { l := strings.ToLower(v); return l == "bulk" || l == "list" || l == "junk" }},
		{"X-Auto-Response-Suppress", func(v string) bool { return v != "" }},
		{"List-Unsubscribe", func(v string) bool { return v != "" }},
	}
	imageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true}
	// Ordered pattern and replacement pairs of the HTML stripper
	htmlSubs = sync.OnceValue(func() []emailHTMLSub {
		return []emailHTMLSub{
			{regexp.MustCompile(`(?i)<br\s*/?>`), "\n"}, {regexp.MustCompile(`(?i)<p[^>]*>`), "\n"}, {regexp.MustCompile(`(?i)</p>`), "\n"},
			{regexp.MustCompile(`<[^>]+>`), ""}, {regexp.MustCompile(`&nbsp;`), " "}, {regexp.MustCompile(`&amp;`), "&"},
			{regexp.MustCompile(`&lt;`), "<"}, {regexp.MustCompile(`&gt;`), ">"}, {regexp.MustCompile(`\n{3,}`), "\n\n"},
		}
	})
	singleBracketFromRE = regexutil.Lazy(regexp.MustCompile, `\A(?:([^"<>]*)<([^<>\s\v\x{85}\p{Z}]+)>[\s\v\x{85}\p{Z}]*)\z`)
	commentRE           = regexutil.Lazy(regexp.MustCompile, `\([^()]*\)`)
	authMethodRE        = regexutil.Lazy(regexp.MustCompile, `(?i)^\s*(dmarc|dkim|spf)\s*=\s*([a-z]+)`)
	authPropRE          = regexutil.Lazy(regexp.MustCompile, `(?i)(header\.from|header\.d|smtp\.mailfrom|smtp\.from|envelope-from)\s*=\s*((?:"(?:[^"\\]|\\.)*"|[^\s";])+)`+
		`|(?:"(?:[^"\\]|\\.)*"|[^\s"])+`)
)

// One pattern and literal replacement of the HTML stripper
type emailHTMLSub struct {
	re   *regexp.Regexp
	repl string
}

const (
	// Longest From value parsed, a real mailbox with its display name staying far below it
	maxFromLen = 2048
	// Largest pre authorization header block accepted, one byte more is requested so a truncated one is refused
	maxPreauthHeaderBytes = 64 * 1024
	preauthFetch          = "(BODY.PEEK[HEADER.FIELDS (FROM AUTHENTICATION-RESULTS AUTO-SUBMITTED PRECEDENCE X-AUTO-RESPONSE-SUPPRESS " +
		"LIST-UNSUBSCRIBE)]<0.65537>)"
	noAuthResultsReason     = "no Authentication-Results header"
	untrustedAuthservReason = "no Authentication-Results from trusted authserv-id"
	missingAuthservReason   = "authserv-id is not configured; refusing to trust Authentication-Results"
	optOutHint              = "set email.require_authenticated_sender to false (or EMAIL_TRUST_FROM_HEADER=true) to accept the risk."
	missingAuthservHint     = " Set email.authserv_id (or EMAIL_AUTHSERV_ID) to the receiving MTA's exact authserv-id, " +
		"or " + optOutHint
)

// Operator fixable reasons a granted sender fails authentication and the fix each log line names
var dropHints = map[string]string{
	noAuthResultsReason:     " If your mail server does not stamp Authentication-Results, " + optOutHint,
	untrustedAuthservReason: " Check that email.authserv_id (EMAIL_AUTHSERV_ID) names your mail server.",
}

// Whether an address or its bulk mail header fields mark an automated sender
func isAutomatedSender(address string, header mailutil.Header) bool {
	addr := strings.ToLower(address)
	for _, pattern := range noreplyPatterns {
		if strings.Contains(addr, pattern) {
			return true
		}
	}
	for _, rule := range automatedHeaders {
		if value := header.Get(rule.name); value != "" && rule.check(value) {
			return true
		}
	}
	return false
}

// Decoded text of the first part of a content type that is not an attachment
func firstBodyPart(msg *mailutil.Message, contentType string) string {
	for part := range msg.Walk() {
		if part.Disposition() == "attachment" || part.ContentType() != contentType {
			continue
		}
		if text := part.Text(); text != "" {
			return text
		}
	}
	return ""
}

// Plain text body of a message, the HTML part stripped of tags when no plain part exists
func extractTextBody(msg *mailutil.Message) string {
	if msg.IsMultipart() {
		html := firstBodyPart(msg, "text/html")
		if plain := firstBodyPart(msg, "text/plain"); plain != "" {
			return plain
		}
		if html != "" {
			return stripHTML(html)
		}
		return ""
	}
	text := msg.Text()
	if msg.ContentType() == "text/html" {
		return stripHTML(text)
	}
	return text
}

// HTML reduced to text by tag removal and the four common entities
func stripHTML(html string) string {
	for _, sub := range htmlSubs() {
		html = sub.re.ReplaceAllLiteralString(html, sub.repl)
	}
	return strings.TrimSpace(html)
}

// Text with every comment removed, innermost first, until nothing changes
func stripComments(text string) string {
	for {
		stripped := commentRE().ReplaceAllLiteralString(text, " ")
		if stripped == text {
			return text
		}
		text = stripped
	}
}

// Bare lowercased addr-spec of an unfolded From value, empty for a hostile, unparseable or domainless one
func extractEmailAddress(value string) string {
	if utf8.RuneCountInString(value) > maxFromLen || strings.Count(value, "(") > 64 {
		return ""
	}
	addr := ""
	if parsed, err := mailutil.ParseAddress(value); err == nil {
		addr = parsed.Address
	}
	if addr == "" {
		if m := singleBracketFromRE().FindStringSubmatch(value); m != nil {
			display, bracketed := strings.TrimSpace(stripComments(m[1])), m[2]
			inner := textutil.DropLast(textutil.DropFirst(bracketed, 1), 1)
			if strings.Contains(inner, "@") && !strings.ContainsAny(display, ";:()") &&
				(!strings.Contains(display, "@") || strings.ToLower(display) == strings.ToLower(bracketed)) {
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

// Clauses of an Authentication-Results value split at semicolons outside quotes and comments, false when unbalanced
func arClauses(text string) ([]string, bool) {
	rs := []rune(text)
	var clauses []string
	var cur []rune
	depth, quoted := 0, false
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if c == '\\' && (quoted || depth > 0) {
			if quoted {
				cur = append(cur, rs[i:min(i+2, len(rs))]...)
			}
			i++
			continue
		}
		switch {
		case quoted:
			quoted = c != '"'
			cur = append(cur, c)
		case depth > 0:
			if c == '(' {
				depth++
			} else if c == ')' {
				depth--
			}
			if depth == 0 {
				cur = append(cur, ' ')
			}
		case c == '(':
			depth = 1
		case c == '"':
			quoted = true
			cur = append(cur, c)
		case c == ')':
			return nil, false
		case c == ';':
			clauses = append(clauses, string(cur))
			cur = nil
		default:
			cur = append(cur, c)
		}
	}
	if quoted || depth > 0 {
		return nil, false
	}
	return append(clauses, string(cur)), true
}

// Property and value pairs of one comment free clause, the property lowercased and outer quotes stripped
func authProps(text string) [][2]string {
	var out [][2]string
	for _, m := range authPropRE().FindAllStringSubmatch(text, -1) {
		if prop := m[1]; prop != "" {
			out = append(out, [2]string{strings.ToLower(prop), strings.Trim(m[2], `"`)})
		}
	}
	return out
}

// Lowercased domain part of an address
func domainOf(address string) string {
	if i := strings.LastIndex(address, "@"); i >= 0 {
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

// One method verdict of an Authentication-Results header with the properties of its clause
type authResult struct {
	verdict string
	props   [][2]string
}

// Whether the From domain is authenticated by the first Authentication-Results header of the pinned server, with the reason
func verifySenderAuthentication(header mailutil.Header, fromAddr, authservID string) (bool, string) {
	fromDomain := domainOf(fromAddr)
	if fromDomain == "" {
		return false, "missing From domain"
	}
	headers := header.Values("Authentication-Results")
	if len(headers) == 0 {
		return false, noAuthResultsReason
	}
	trusted := strings.Join(strings.Fields(headers[0]), " ")
	clauses, balanced := arClauses(trusted)
	if !balanced {
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
		if m := authMethodRE().FindStringSubmatch(clause); m != nil {
			method := strings.ToLower(m[1])
			results[method] = append(results[method], authResult{strings.ToLower(m[2]), authProps(clause)})
		}
	}
	aligned := func(props [][2]string, required bool, names ...string) bool {
		found := false
		for _, prop := range props {
			for _, name := range names {
				if prop[0] != name {
					continue
				}
				found = true
				if !domainsAligned(domainOf(prop[1]), fromDomain) {
					return false
				}
			}
		}
		return found || !required
	}
	if len(results["dmarc"]) > 1 {
		return false, "ambiguous dmarc result"
	}
	for _, r := range results["dmarc"] {
		if r.verdict == "pass" && aligned(r.props, false, "header.from") {
			return true, "dmarc=pass"
		}
	}
	if spf := results["spf"]; len(spf) == 1 && spf[0].verdict == "pass" && aligned(spf[0].props, true, "smtp.mailfrom", "smtp.from", "envelope-from") {
		return true, "spf=pass aligned"
	}
	for _, r := range results["dkim"] {
		name := "header.from"
		for _, prop := range r.props {
			if prop[0] == "header.d" {
				name = "header.d"
				break
			}
		}
		if r.verdict == "pass" && aligned(r.props, true, name) {
			return true, "dkim=pass aligned"
		}
	}
	return false, fmt.Sprintf("authentication failed (%s)", textutil.First(trusted, 120))
}

// One attachment cached to disk with its original name, kind and declared media type
type attachment struct {
	path      string
	filename  string
	kind      string
	mediaType string
}

// Caches every attachment and inline non text part of a multipart message, none when attachments are skipped
func extractAttachments(ctx context.Context, msg *mailutil.Message, skipAttachments bool) ([]attachment, error) {
	attachments := []attachment{}
	if !msg.IsMultipart() {
		return attachments, nil
	}
	for part := range msg.Walk() {
		disp, contentType := part.Disposition(), part.ContentType()
		if skipAttachments || (disp != "attachment" && (disp != "inline" || contentType == "text/plain" || contentType == "text/html")) {
			continue
		}
		filename := part.Filename()
		if filename == "" {
			_, subtype, _ := strings.Cut(contentType, "/")
			if subtype == "" {
				subtype = "bin"
			}
			filename = "attachment." + subtype
		}
		payload := part.Body()
		if len(payload) == 0 {
			continue
		}
		var cachedPath, kind string
		if ext := strings.ToLower(pathutil.Suffix(filename)); imageExts[ext] {
			path, err := mediacache.CacheImageFromBytes(ctx, home.Dir(ctx), payload, ext)
			if _, invalid := err.(*mediacache.ValueError); invalid {
				pkgLog().Debug(fmt.Sprintf("Skipping non-image attachment %s (invalid magic bytes)", filename))
				continue
			}
			if err != nil {
				return nil, err
			}
			cachedPath, kind = path, "image"
		} else {
			path, err := mediacache.CacheDocumentFromBytes(ctx, home.Dir(ctx), payload, filename)
			if err != nil {
				return nil, err
			}
			cachedPath, kind = path, "document"
		}
		attachments = append(attachments, attachment{path: cachedPath, filename: filename, kind: kind, mediaType: contentType})
	}
	return attachments, nil
}
