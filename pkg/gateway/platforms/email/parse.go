package email

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyemail"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/mediacache"
)

var (
	// Address substrings of automated senders that are silently ignored
	noreplyPatterns = []string{"noreply", "no-reply", "no_reply", "donotreply", "do-not-reply", "mailer-daemon", "postmaster",
		"bounce", "notifications@", "automated@", "auto-confirm", "auto-reply", "automailer"}
	// Bulk mail headers and the test their value passes for an automated sender
	automatedHeaders = []struct {
		name   string
		lowers bool
		check  func(v string) bool
	}{
		{"Auto-Submitted", true, func(v string) bool { return pystr.Lower(v) != "no" }},
		{"Precedence", true, func(v string) bool { l := pystr.Lower(v); return l == "bulk" || l == "list" || l == "junk" }},
		{"X-Auto-Response-Suppress", false, func(v string) bool { return v != "" }},
		{"List-Unsubscribe", false, func(v string) bool { return v != "" }},
	}
	imageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true}
	// Charset labels seen in the wild that the codec registry does not know
	charsetAliases = map[string]string{"unknown-8bit": "utf-8", "unknown": "utf-8", "x-unknown": "utf-8", "default": "utf-8",
		"ansi_x3.110-1983": "latin-1", "cp-850": "cp850", "gb2312": "gb18030", "gbk": "gb18030", "ks_c_5601-1987": "cp949"}
	// Ordered pattern and replacement pairs of the HTML stripper
	htmlSubs = []struct {
		re   *regexp2.Regexp
		repl string
	}{
		{mustIgnoreCase(`<br\s*/?>`), "\n"}, {mustIgnoreCase(`<p[^>]*>`), "\n"}, {mustIgnoreCase(`</p>`), "\n"},
		{pyre.MustCompile(`<[^>]+>`), ""}, {pyre.MustCompile(`&nbsp;`), " "}, {pyre.MustCompile(`&amp;`), "&"},
		{pyre.MustCompile(`&lt;`), "<"}, {pyre.MustCompile(`&gt;`), ">"}, {pyre.MustCompile(`\n{3,}`), "\n\n"},
	}
	foldRE              = pyre.MustCompile(`\r?\n[ \t]+`)
	singleBracketFromRE = mustFullmatch(`([^"<>]*)<([^<>\s]+)>\s*`)
	commentRE           = pyre.MustCompile(`\([^()]*\)`)
	authMethodRE        = mustIgnoreCase(`^\s*(dmarc|dkim|spf)\s*=\s*([a-z]+)`)
	authPropRE          = mustIgnoreCase(`(header\.from|header\.d|smtp\.mailfrom|smtp\.from|envelope-from)\s*=\s*((?:"(?:[^"\\]|\\.)*"|[^\s";])+)` +
		`|(?:"(?:[^"\\]|\\.)*"|[^\s"])+`)
)

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
	optOutHint              = "set platforms.email.require_authenticated_sender: false (or EMAIL_TRUST_FROM_HEADER=true) to accept the risk."
	missingAuthservHint     = " Set EMAIL_AUTHSERV_ID (or platforms.email.authserv_id) to the receiving MTA's exact authserv-id, " +
		"or " + optOutHint
)

// Operator fixable reasons a granted sender fails authentication and the fix each log line names
var dropHints = map[string]string{
	noAuthResultsReason:     " If your mail server does not stamp Authentication-Results, " + optOutHint,
	untrustedAuthservReason: " Check that platforms.email.authserv_id (EMAIL_AUTHSERV_ID) names your mail server.",
}

func mustFullmatch(pattern string) *regexp2.Regexp {
	re, err := pyre.CompileFullmatch(pattern)
	if err != nil {
		panic(err)
	}
	return re
}

func mustIgnoreCase(pattern string) *regexp2.Regexp {
	re, err := pyre.CompileIgnoreCase(pattern)
	if err != nil {
		panic(err)
	}
	return re
}

// Whether an address or its bulk mail headers mark an automated sender, the error a non text header value raises
func isAutomatedSender(address string, headers map[string]pyemail.HeaderValue) (bool, error) {
	addr := pystr.Lower(address)
	for _, pattern := range noreplyPatterns {
		if strings.Contains(addr, pattern) {
			return true, nil
		}
	}
	for _, rule := range automatedHeaders {
		value, ok := headers[rule.name]
		if !ok || value == "" {
			continue
		}
		if rule.lowers && value.Has8Bit() {
			return false, &gateway.PyError{Kind: "AttributeError", Msg: "'Header' object has no attribute 'lower'"}
		}
		if rule.check(string(value)) {
			return true, nil
		}
	}
	return false, nil
}

// Text of a payload decoded with its charset label, an unknown label falling to its alias, utf-8, then latin-1
func safeDecode(payload []byte, charset string) string {
	label := pystr.Lower(pystr.StripChars(pystr.Strip(charset), "\"'"))
	if charset == "" || label == "" {
		label = "utf-8"
	}
	candidate := label
	if alias, ok := charsetAliases[label]; ok {
		candidate = alias
	}
	for _, name := range []string{candidate, "utf-8"} {
		if text, err := pycodecs.DecodeReplace(payload, name); err == nil {
			return text
		}
	}
	text, _ := pycodecs.DecodeReplace(payload, "latin-1")
	return text
}

// RFC 2047 header decoded to plain text, the raw value when its structure is malformed
func decodeHeaderValue(raw pyemail.HeaderValue) string {
	parts, err := pyemail.DecodeHeader(raw)
	if err != nil {
		return raw.Str()
	}
	texts := make([]string, len(parts))
	for i, part := range parts {
		if part.IsBytes {
			texts[i] = safeDecode(part.Bytes, part.Charset)
		} else {
			texts[i] = part.Text
		}
	}
	return strings.Join(texts, " ")
}

// Content-Disposition value of a part as text
func disposition(part *pyemail.Message) string {
	value, _ := part.Get("Content-Disposition")
	return value.Str()
}

// Decoded text of the first non attachment part of a content type
func firstBodyPart(msg *pyemail.Message, contentType string) string {
	for _, part := range msg.Walk() {
		if strings.Contains(disposition(part), "attachment") || part.GetContentType() != contentType {
			continue
		}
		if payload := part.GetPayloadDecoded(); len(payload) > 0 {
			charset, _ := part.GetContentCharset()
			return safeDecode(payload, charset)
		}
	}
	return ""
}

// Plain text body of a message, the HTML part stripped of tags when no plain part exists
func extractTextBody(msg *pyemail.Message) string {
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
	text := ""
	if payload := msg.GetPayloadDecoded(); len(payload) > 0 {
		charset, _ := msg.GetContentCharset()
		text = safeDecode(payload, charset)
	}
	if msg.GetContentType() == "text/html" {
		return stripHTML(text)
	}
	return text
}

// HTML reduced to text by tag removal and the four common entities
func stripHTML(html string) string {
	for _, sub := range htmlSubs {
		out, err := sub.re.Replace(html, sub.repl, -1, -1)
		if err != nil {
			panic(err)
		}
		html = out
	}
	return pystr.Strip(html)
}

// Text with every comment removed, innermost first, until nothing changes
func stripComments(text string) string {
	for {
		stripped, err := commentRE.Replace(text, " ", -1, -1)
		if err != nil || stripped == text {
			return text
		}
		text = stripped
	}
}

// Bare lowercased addr-spec of a From value, empty for a hostile, unparseable or domainless one
func extractEmailAddress(raw string) string {
	value, err := foldRE.Replace(raw, " ", -1, -1)
	if err != nil {
		return ""
	}
	if len([]rune(value)) > maxFromLen || strings.Count(value, "(") > 64 {
		return ""
	}
	addr := pyemail.ParseAddr(value).Addr
	if addr == "" {
		if m, _ := singleBracketFromRE.FindStringMatch(value); m != nil {
			display, bracketed := pystr.Strip(stripComments(m.GroupByNumber(1).String())), m.GroupByNumber(2).String()
			inner := pystr.Slice(bracketed, 1, -1)
			if strings.Contains(inner, "@") && !strings.ContainsAny(display, ";:()") &&
				(!strings.Contains(display, "@") || pystr.Lower(display) == pystr.Lower(bracketed)) {
				addr = bracketed
			}
		}
	}
	addr = pystr.Lower(pystr.Strip(addr))
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
	for m, _ := authPropRE.FindStringMatch(text); m != nil; m, _ = authPropRE.FindNextMatch(m) {
		if prop := m.GroupByNumber(1).String(); prop != "" {
			out = append(out, [2]string{pystr.Lower(prop), pystr.StripChars(m.GroupByNumber(2).String(), `"`)})
		}
	}
	return out
}

// Lowercased domain part of an address
func domainOf(address string) string {
	if i := strings.LastIndex(address, "@"); i >= 0 {
		address = address[i+1:]
	}
	return pystr.Lower(pystr.Strip(address))
}

// Relaxed DMARC alignment, equal domains or one a dot suffix of the other
func domainsAligned(a, b string) bool {
	a = pystr.RstripChars(pystr.Lower(pystr.Strip(a)), ".")
	b = pystr.RstripChars(pystr.Lower(pystr.Strip(b)), ".")
	return a != "" && b != "" && (a == b || strings.HasSuffix(a, "."+b) || strings.HasSuffix(b, "."+a))
}

// One method verdict of an Authentication-Results header with the properties of its clause
type authResult struct {
	verdict string
	props   [][2]string
}

// Whether the From domain is authenticated by the first Authentication-Results header of the pinned server, with the reason
func verifySenderAuthentication(msg *pyemail.Message, fromAddr, authservID string) (bool, string) {
	fromDomain := domainOf(fromAddr)
	if fromDomain == "" {
		return false, "missing From domain"
	}
	headers := msg.GetAll("Authentication-Results")
	if len(headers) == 0 {
		return false, noAuthResultsReason
	}
	trusted := strings.Join(pystr.SplitWhitespace(headers[0].Str(), -1), " ")
	clauses, balanced := arClauses(trusted)
	if !balanced {
		return false, "unbalanced quote or comment in Authentication-Results"
	}
	if authservID == "" {
		return false, missingAuthservReason
	}
	if pystr.Lower(pystr.Strip(clauses[0])) != authservID {
		return false, untrustedAuthservReason
	}
	results := map[string][]authResult{}
	for _, clause := range clauses {
		if m, _ := authMethodRE.FindStringMatch(clause); m != nil {
			method := pystr.Lower(m.GroupByNumber(1).String())
			results[method] = append(results[method], authResult{pystr.Lower(m.GroupByNumber(2).String()), authProps(clause)})
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
	return false, fmt.Sprintf("authentication failed (%s)", pystr.Slice(trusted, 0, 120))
}

// One attachment cached to disk with its original name, kind and declared media type
type attachment struct {
	path      string
	filename  string
	kind      string
	mediaType string
}

// Caches every attachment and inline non text part of a multipart message, none when attachments are skipped
func extractAttachments(ctx context.Context, msg *pyemail.Message, skipAttachments bool) ([]attachment, error) {
	attachments := []attachment{}
	if !msg.IsMultipart() {
		return attachments, nil
	}
	for _, part := range msg.Walk() {
		disp, contentType := disposition(part), part.GetContentType()
		if skipAttachments || (!strings.Contains(disp, "attachment") &&
			(!strings.Contains(disp, "inline") || contentType == "text/plain" || contentType == "text/html")) {
			continue
		}
		filename := ""
		if fn, ok := part.GetFilename(); ok && fn != "" {
			filename = decodeHeaderValue(pyemail.HeaderValue(fn))
		} else {
			subtype := part.GetContentSubtype()
			if subtype == "" {
				subtype = "bin"
			}
			filename = "attachment." + subtype
		}
		payload := part.GetPayloadDecoded()
		if len(payload) == 0 {
			continue
		}
		var cachedPath, kind string
		if ext := pystr.Lower(pypath.PathSuffix(filename)); imageExts[ext] {
			path, err := mediacache.CacheImageFromBytes(ctx, home.Dir(ctx), payload, ext)
			if _, invalid := err.(*mediacache.ValueError); invalid {
				slog.Debug(fmt.Sprintf("Skipping non-image attachment %s (invalid magic bytes)", filename))
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

// Python repr of an IMAP UID as the bytes it arrived as
func uidRepr(uid string) string { return pyrepr.BytesRepr([]byte(uid)) }
