package tools

import (
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

// RedactOptions selects the redaction passes redact_sensitive_text applies
type RedactOptions struct {
	Force          bool
	CodeFile       bool
	FileRead       bool
	SecretFile     bool
	URLCredentials bool
}

var (
	strictURLParamRe    = regexp.MustCompile(`([?#&;])([A-Za-z0-9_.~+%\-]+)=([^#&;\s"'<>]*)`)
	strictURLUserinfoRe = regexp.MustCompile(`//[^/\s?#@]+@`)
	pytestDiagnosticRe  = regexp.MustCompile(`^(?P<prefix>[ \t]*E[ \t]{2,})(?P<body>.*)$`)
	pythonExceptionRe   = regexp.MustCompile(`^(?P<prefix>(?:[A-Za-z_]\w*\.)*[A-Za-z_]\w*(?:Error|Exception|Warning):[ \t]*)(?P<body>.*)$`)
	bearerResidueRe     = regexp.MustCompile(`(?i)\bBearer\s+(?:\[[^\]]+\]|[A-Za-z0-9._~+/-]{20,}=*)`)
	envDumpCommands     = setOf("env", "printenv", "set", "export", "declare")
	fileReadCommands    = setOf("cat", "head", "tail", "type", "bat", "less", "more", "nl", "zcat", "tac", "view", "batcat", "grep", "awk", "sed")
	patternFirstCmds    = setOf("grep", "awk", "sed")
)

// RedactionUnavailable replaces text whose redaction failed on the egress boundary
const RedactionUnavailable = "[redaction-unavailable]"

// Decodes a URL parameter name up to three unquote rounds for case insensitive matching
func canonicalURLParamName(name string) string {
	decoded := name
	for range 3 {
		next, err := url.QueryUnescape(decoded)
		if err != nil || next == decoded {
			break
		}
		decoded = next
	}
	folded := strings.ToLower(decoded)
	if sensitiveQueryParams[folded] {
		return folded
	}
	return strings.ReplaceAll(folded, "-", "_")
}

// Exported canonicalURLParamName
func CanonicalURLParamName(name string) string { return canonicalURLParamName(name) }

// Strict egress boundary redaction of URL credentials in absolute, relative and network references
func redactStrictURLCredentials(text string) string {
	text = strictURLParamRe.ReplaceAllStringFunc(text, func(m string) string {
		sub := strictURLParamRe.FindStringSubmatch(m)
		if sensitiveQueryParams[canonicalURLParamName(sub[2])] {
			return sub[1] + sub[2] + "=***"
		}
		return m
	})
	return strictURLUserinfoRe.ReplaceAllString(text, "//***:***@")
}

// RedactSensitiveTextWith applies every redaction pattern to a block of text under the given options
func RedactSensitiveTextWith(env *Env, text string, o RedactOptions) string {
	if text == "" {
		return text
	}
	text = redactRegisteredVaultValues(env, text)
	if !(o.Force || redactionEnabled(env)) {
		return text
	}
	codeFile := (o.CodeFile || o.FileRead) && !o.SecretFile
	prefixMask := maskToken
	if o.FileRead {
		prefixMask = maskTokenNonreusable
	}
	if hasKnownPrefixSubstring(text) {
		text = maskControlSplitTokens(text, prefixMask)
		text = replaceSpans(text, prefixScan(text), func(loc []int) string {
			return prefixMask(text[loc[0]:loc[1]])
		})
	}
	if strings.Contains(text, ".") {
		text = replaceSpans(text, scanMatches(zhipuRe, text, func(loc []int) bool {
			if r, ok := prevRune(text, loc[0]); ok && (isTokenBoundaryRune(r) || r == '.') {
				return false
			}
			if r, ok := nextRune(text, loc[1]); ok && (isTokenBoundaryRune(r) || r == '.') {
				return false
			}
			return true
		}), func(loc []int) string { return prefixMask(text[loc[0]:loc[1]]) })
	}
	if !codeFile {
		text = redactAssignments(text, o.FileRead)
	}
	if strings.Contains(text, "uthorization") || strings.Contains(text, "UTHORIZATION") {
		text = authHeaderRe.ReplaceAllStringFunc(text, func(m string) string {
			loc := authHeaderRe.FindStringSubmatchIndex(m)
			return group(m, loc, 1) + group(m, loc, 2) + maskToken(group(m, loc, 3))
		})
	}
	if strings.Contains(text, ":") {
		text = secretHeaderRe.ReplaceAllStringFunc(text, func(m string) string {
			loc := secretHeaderRe.FindStringSubmatchIndex(m)
			return group(m, loc, 1) + maskToken(group(m, loc, 2))
		})
		text = replaceSpans(text, scanMatches(telegramRe, text, func(loc []int) bool {
			r, ok := prevRune(text, loc[0])
			return !ok || !unicode.IsDigit(r)
		}), func(loc []int) string {
			return group(text, loc, 1) + group(text, loc, 2) + ":***"
		})
	}
	if strings.Contains(text, "BEGIN") && strings.Contains(text, "-----") {
		text = privateKeyRe.ReplaceAllString(text, "[REDACTED PRIVATE KEY]")
	}
	if strings.Contains(text, "://") {
		text = redactURLCredentials(text, codeFile)
	}
	if strings.Contains(text, "eyJ") {
		text = jwtRe.ReplaceAllStringFunc(text, maskToken)
	}
	if strings.Contains(text, "icebath-session=") || strings.Contains(text, "/webapp/launch#") {
		text = replaceSpans(text, scanMatches(webappLinkRe, text, func(loc []int) bool {
			if !strings.HasPrefix(text[loc[0]:], "icebath-session=") {
				return true
			}
			r, ok := prevRune(text, loc[0])
			return !ok || !isTokenBoundaryRune(r)
		}), func(loc []int) string { return group(text, loc, 1) + "***" })
	}
	if o.URLCredentials {
		text = redactStrictURLCredentials(text)
	}
	if strings.Contains(text, "&") && strings.Contains(text, "=") {
		text = redactFormBody(text)
	}
	if strings.Contains(text, "+") {
		text = replaceSpans(text, scanMatches(signalPhoneRe, text, func(loc []int) bool {
			r, ok := nextRune(text, loc[1])
			return !ok || !(unicode.IsLetter(r) || unicode.IsDigit(r))
		}), func(loc []int) string { return redactPhone(text[loc[0]:loc[1]]) })
	}
	return text
}

// RedactSensitiveText applies the default redaction passes to a block of text
func RedactSensitiveText(text string) string {
	return RedactSensitiveTextWith(nil, text, RedactOptions{})
}

// Pipeline and sequence segments split only on unquoted pipe, semicolon and ampersand
func commandSegments(command string) []string {
	var segments []string
	var buf strings.Builder
	var quote rune
	for _, ch := range command {
		if quote != 0 {
			buf.WriteRune(ch)
			if ch == quote {
				quote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			quote = ch
			buf.WriteRune(ch)
			continue
		}
		if ch == '|' || ch == ';' || ch == '&' {
			if seg := strings.TrimSpace(buf.String()); seg != "" {
				segments = append(segments, seg)
			}
			buf.Reset()
			continue
		}
		buf.WriteRune(ch)
	}
	if seg := strings.TrimSpace(buf.String()); seg != "" {
		segments = append(segments, seg)
	}
	return segments
}

// Reports whether the command reads a secret bearing file to stdout
func commandReadsSecretFile(env *Env, command string) bool {
	if command == "" {
		return false
	}
	for _, seg := range commandSegments(command) {
		tokens := strings.Fields(seg)
		if len(tokens) == 0 {
			continue
		}
		reader := strings.ToLower(tokens[0][strings.LastIndex(tokens[0], "/")+1:])
		if !fileReadCommands[reader] {
			continue
		}
		var positional []string
		for _, arg := range tokens[1:] {
			if !strings.HasPrefix(arg, "-") {
				positional = append(positional, arg)
			}
		}
		if patternFirstCmds[reader] && len(positional) > 0 {
			positional = positional[1:]
		}
		for _, arg := range positional {
			if isSecretFileArg(env, arg) {
				return true
			}
		}
	}
	return false
}

// IsEnvDumpCommand reports whether any segment starts with an environment dump command
func IsEnvDumpCommand(command string) bool {
	if command == "" {
		return false
	}
	for _, seg := range commandSegments(command) {
		tokens, err := shlexSplit(seg)
		if err != nil {
			tokens = strings.Fields(seg)
		}
		if len(tokens) > 0 && envDumpCommands[tokens[0]] {
			return true
		}
	}
	return false
}

// Masks repr fields only on pytest and exception lines in source preserving output
func redactPythonDiagnosticReprFields(text string) string {
	lines := splitLinesKeepEnds(text)
	for index, line := range lines {
		body, ending := line, ""
		switch {
		case strings.HasSuffix(line, "\r\n"):
			body, ending = line[:len(line)-2], "\r\n"
		case strings.HasSuffix(line, "\n") || strings.HasSuffix(line, "\r"):
			body, ending = line[:len(line)-1], line[len(line)-1:]
		}
		re := pytestDiagnosticRe
		m := re.FindStringSubmatch(body)
		if m == nil {
			re = pythonExceptionRe
			m = re.FindStringSubmatch(body)
		}
		if m != nil {
			lines[index] = m[re.SubexpIndex("prefix")] + redactPythonReprFields(m[re.SubexpIndex("body")]) + ending
		}
	}
	return strings.Join(lines, "")
}

// RedactForEgress scrubs text leaving the process for a remote reader, forced plus a bearer sweep
func RedactForEgress(env *Env, text string) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = RedactionUnavailable
		}
	}()
	text = RedactSensitiveTextWith(env, text, RedactOptions{Force: true})
	if strings.Contains(text, "earer") {
		text = bearerResidueRe.ReplaceAllString(text, "Bearer [redacted]")
	}
	return text
}

// RedactTerminalCommandOutput redacts terminal output with the assignment pass only for env dumps and secret file reads
func RedactTerminalCommandOutput(env *Env, output, command string, force bool) string {
	if output == "" {
		return output
	}
	codeFile := !(IsEnvDumpCommand(command) || commandReadsSecretFile(env, command))
	redacted := RedactSensitiveTextWith(env, output, RedactOptions{Force: force, CodeFile: codeFile})
	if codeFile && (force || redactionEnabled(env)) && strings.Contains(redacted, ":") && strings.Contains(redacted, "'") {
		redacted = redactPythonDiagnosticReprFields(redacted)
	}
	return redacted
}

// RedactTerminalOutput redacts terminal output when the command that produced it is unknown
func RedactTerminalOutput(env *Env, s string) string {
	return RedactTerminalCommandOutput(env, s, "", false)
}

// CommandApprovalIdentity is everything the approval path keys a decision on for the call's command
func CommandApprovalIdentity(env *Env, call *Call) string {
	command := ""
	if call != nil {
		if s, ok := call.Args["command"].(string); ok {
			command = s
		}
	}
	hardline, hardlineDesc := DetectHardlineCommand(command)
	patternKey, description, dangerous := DetectDangerousCommand(command)
	sudoBlocked, sudoDesc := CheckSudoStdinGuard(env, command)
	return JSON([]any{
		[]any{hardline, hardlineDesc},
		[]any{dangerous, patternKey, description},
		[]any{sudoBlocked, sudoDesc},
		commandDeletesRuntime(command),
		matchUserDenyRule(env, command),
		commandMatchesPermanentAllowlist(env, command),
	})
}

// Splits text into lines keeping their endings the way str.splitlines(keepends=True) does
func splitLinesKeepEnds(text string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c == '\n' || c == '\r' {
			end := i + 1
			if c == '\r' && i+1 < len(text) && text[i+1] == '\n' {
				end = i + 2
			}
			lines = append(lines, text[start:end])
			start = end
			i = end - 1
		}
	}
	if start < len(text) {
		lines = append(lines, text[start:])
	}
	return lines
}
