package selfupdate

import (
	"regexp"
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const debugRedacted = "[REDACTED]"

var debugEmailAddressRe = regexp2.MustCompile(`(?<![A-Za-z0-9._%+-])[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}(?![A-Za-z0-9._%+-])`, regexp2.None)

// Keeps the alphanumeric runes of a string
func alnumOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if pystr.IsAlnum(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Compact alphanumeric forms of the sensitive URL parameter names plus the support only aliases
var debugSensitiveURLParamNames = func() map[string]bool {
	set := map[string]bool{"authtoken": true, "privatekey": true, "secretkey": true, "xamzsecuritytoken": true, "securitytoken": true}
	for _, name := range tools.SensitiveQueryParamNames() {
		set[alnumOnly(name)] = true
	}
	return set
}()

const debugEncodedURLSep = `%(?:25)?(?:3f|23|26|3b)`

var (
	debugRawURLParamRe     = regexp2.MustCompile(`(?<sep>[?#&;])(?<key>[A-Za-z0-9_.~+%\-]+)=(?<value>[^?#&;\s"'<>]*)`, regexp2.None)
	debugEncodedURLParamRe = regexp2.MustCompile(`(?<sep>`+debugEncodedURLSep+`)(?<key>(?:(?!%(?:25)?3d|`+debugEncodedURLSep+`)[^\s"'<>]){1,160}?)(?<eq>%(?:25)?3d|=)(?<value>.*?)(?=`+debugEncodedURLSep+`|[\s"'<>]|$)`, regexp2.IgnoreCase)
)

// Header names whose values never leave a debug flow
var debugSensitiveHeaderNames = append([]string{"authorization", "proxy-authorization", "cookie", "set-cookie"}, tools.SecretHeaderNameList...)

// Alternation of the escaped sensitive header names
func debugHeaderNamePattern() string {
	parts := make([]string, len(debugSensitiveHeaderNames))
	for i, name := range debugSensitiveHeaderNames {
		parts[i] = regexp.QuoteMeta(name)
	}
	return strings.Join(parts, "|")
}

const (
	debugKeyChars      = `A-Za-z0-9._~+/\-`
	debugKeyEnd        = `(?![` + debugKeyChars + `=]|:[` + debugKeyChars + `])`
	debugKey           = `[` + debugKeyChars + `]+(?::[` + debugKeyChars + `]+)*=*` + debugKeyEnd
	debugNotWord       = `(?!(?-i:[A-Z]?[a-z]{1,19}|[A-Z]{1,19})` + debugKeyEnd + `)`
	debugSecretArgFlag = `--(?:api[-_]?key|access[-_]?token|refresh[-_]?token|id[-_]?token|auth[-_]?token|client[-_]?secret|private[-_]?key|secret[-_]?key|password|passwd|credential|token|secret)`
)

var (
	debugHN     = debugHeaderNamePattern()
	debugScheme = `(?:Bearer|Basic|Digest)\s+|\*\*\*[ \t]+(?=[` + debugKeyChars + `]{8,}=*(?![` + debugKeyChars + `=])|` + debugNotWord +
		`(?=[^\s:]{4}|(?=\S*[A-Za-z])(?=\S*[0-9])\S{6})` + debugKey + `|[A-Za-z]{4,7}[ \t]*(?:[\r\n'",;}\]]|$))(?!(?:` + debugHN + `)\b\s*[:=])`
	debugQuotedHeaderRe   = regexp2.MustCompile(`(?<prefix>['"]?(?:`+debugHN+`)['"]?\s*[:=]\s*)(?<quote>['"])(?<value>.*?)\k<quote>`, regexp2.IgnoreCase)
	debugBareHeaderRe     = regexp2.MustCompile(`(?<prefix>\b(?:`+debugHN+`)\b\s*[:=]\s*)(?<value>(?>(?:`+debugScheme+`)?)(?!\[REDACTED\])[^\s,}\]]+)`, regexp2.IgnoreCase)
	debugQuotedArgvRe     = regexp2.MustCompile(`(?<flag_quote>['"])(?<flag>`+debugSecretArgFlag+`)\k<flag_quote>(?<sep>\s*,\s*)(?<value_quote>['"])(?<value>.*?)\k<value_quote>`, regexp2.IgnoreCase)
	debugArgEqualsRe      = regexp2.MustCompile(`(?<flag>`+debugSecretArgFlag+`)(?<sep>\s*=\s*)(?<quote>['"]?)(?!\[REDACTED\])(?<value>[^\s,}\]'"]+)\k<quote>`, regexp2.IgnoreCase)
	debugArgOperandRe     = regexp2.MustCompile(`(?<flag>`+debugSecretArgFlag+`)(?<sep>\s+)(?<quote>['"]?)(?!\[REDACTED\])(?<value>[^\s,}\]'"]+)\k<quote>`, regexp2.IgnoreCase)
	debugHeaderArgvRe     = regexp2.MustCompile(`(?<head_quote>['"])--header\k<head_quote>\s*,\s*(?<name_quote>['"])(?<name>`+debugHN+`)\k<name_quote>\s*,\s*(?:(?<scheme_quote>['"])(?<scheme>Bearer|Basic|Digest)\k<scheme_quote>\s*,\s*)?(?<value_quote>['"])(?<value>.*?)\k<value_quote>`, regexp2.IgnoreCase)
	debugPlainHeaderArgRe = regexp2.MustCompile(`(?<prefix>--header\s+(?:`+debugHN+`)\s+)(?>(?:`+debugScheme+`)?)(?!\[REDACTED\])(?<value>[^\s,}\]]+)`, regexp2.IgnoreCase)
	debugSupportBearerRe  = regexp2.MustCompile(`(?<prefix>\bBearer\s+|\bBasic\s+`+debugNotWord+`)(?!(?:realm|scope|error|error_description|error_uri)=)(?=[^\s]{8})[`+debugKeyChars+`=]+(?::[`+debugKeyChars+`=]+)*`, regexp2.IgnoreCase)
)

// Text of a named group of a match
func mg(m regexp2.Match, name string) string {
	if g := m.GroupByName(name); g != nil {
		return g.String()
	}
	return ""
}

// Compact alphanumeric form of a decoded URL parameter name
func debugDecodedCompactName(value string) string {
	return alnumOnly(tools.CanonicalURLParamName(value))
}

// Stars out the values of sensitive URL parameters in raw and percent encoded form until the text settles
func debugRedactURLParams(text string) (string, error) {
	rawSub := func(m regexp2.Match) string {
		if !debugSensitiveURLParamNames[debugDecodedCompactName(mg(m, "key"))] {
			return m.String()
		}
		return mg(m, "sep") + mg(m, "key") + "=***"
	}
	encodedSub := func(m regexp2.Match) string {
		if !debugSensitiveURLParamNames[debugDecodedCompactName(mg(m, "key"))] {
			return m.String()
		}
		eq := mg(m, "eq")
		star := "*"
		if eq != "=" {
			star = eq[:len(eq)-2] + "2A"
		}
		return mg(m, "sep") + mg(m, "key") + eq + strings.Repeat(star, 3)
	}
	for range 4 {
		raw, err := debugRawURLParamRe.ReplaceFunc(text, rawSub, -1, -1)
		if err != nil {
			return "", err
		}
		redacted, err := debugEncodedURLParamRe.ReplaceFunc(raw, encodedSub, -1, -1)
		if err != nil {
			return "", err
		}
		if redacted == text {
			break
		}
		text = redacted
	}
	return text, nil
}

// Redacts header values, secret argv flags and bearer credentials in support text
func debugRedactHeadersAndArgv(text string) (string, error) {
	steps := []struct {
		re   *regexp2.Regexp
		eval func(regexp2.Match) string
	}{
		{debugQuotedHeaderRe, func(m regexp2.Match) string {
			return mg(m, "prefix") + mg(m, "quote") + debugRedacted + mg(m, "quote")
		}},
		{debugBareHeaderRe, func(m regexp2.Match) string { return mg(m, "prefix") + debugRedacted }},
		{debugHeaderArgvRe, func(m regexp2.Match) string {
			out := mg(m, "head_quote") + "--header" + mg(m, "head_quote") + ", " + mg(m, "name_quote") + mg(m, "name") + mg(m, "name_quote") + ", "
			if mg(m, "scheme") != "" {
				out += mg(m, "scheme_quote") + mg(m, "scheme") + mg(m, "scheme_quote") + ", "
			}
			return out + mg(m, "value_quote") + debugRedacted + mg(m, "value_quote")
		}},
		{debugQuotedArgvRe, func(m regexp2.Match) string {
			return mg(m, "flag_quote") + mg(m, "flag") + mg(m, "flag_quote") + mg(m, "sep") + mg(m, "value_quote") + debugRedacted + mg(m, "value_quote")
		}},
		{debugArgEqualsRe, func(m regexp2.Match) string {
			return mg(m, "flag") + mg(m, "sep") + mg(m, "quote") + debugRedacted + mg(m, "quote")
		}},
		{debugArgOperandRe, func(m regexp2.Match) string {
			return mg(m, "flag") + mg(m, "sep") + mg(m, "quote") + debugRedacted + mg(m, "quote")
		}},
		{debugPlainHeaderArgRe, func(m regexp2.Match) string { return mg(m, "prefix") + debugRedacted }},
		{debugSupportBearerRe, func(m regexp2.Match) string { return mg(m, "prefix") + debugRedacted }},
	}
	for _, step := range steps {
		out, err := step.re.ReplaceFunc(text, step.eval, -1, -1)
		if err != nil {
			return "", err
		}
		text = out
	}
	return text, nil
}

// Scrubs one debug or support value fail closed, any redactor failure returning the shared sentinel
func RedactDebugSupportText(value any) string { return redactDebugSupportText(value, -1) }

// Scrubs like RedactDebugSupportText then keeps at most maxChars characters
func RedactDebugSupportTextCapped(value any, maxChars int) string {
	return redactDebugSupportText(value, maxChars)
}

// Redaction pipeline of a support value with an optional character cap, negative for none
func redactDebugSupportText(value any, maxChars int) string {
	text := ""
	if value != nil {
		text = pyrepr.Str(value)
	}
	if text == "" {
		return text
	}
	out := tools.RedactSensitiveTextWith(nil, text, tools.RedactOptions{Force: true, URLCredentials: true})
	out, err := debugRedactURLParams(out)
	if err != nil {
		return tools.RedactionUnavailable
	}
	out, err = debugRedactHeadersAndArgv(out)
	if err != nil {
		return tools.RedactionUnavailable
	}
	out, err = debugEmailAddressRe.Replace(out, "[REDACTED_EMAIL]", -1, -1)
	if err != nil {
		return tools.RedactionUnavailable
	}
	if maxChars >= 0 {
		if runes := []rune(out); len(runes) > maxChars {
			return string(runes[:maxChars])
		}
	}
	return out
}
