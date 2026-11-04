package memory

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// Hard cap on scanned text so the advisory scanners stay bounded
const MaxScanChars = 65536

// Scope names a scanner accepts, cumulative from all through context to strict
const (
	ScopeAll     = "all"
	ScopeContext = "context"
	ScopeStrict  = "strict"
)

const threatFiller = `(?:\w+\s+){0,8}`
const threatSecretVar = `\$\{?\w*(?:KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL)S?\b`
const threatModify = `(update|modify|edit|write|change|append|add\s+to)\s+[^\n]{0,1000}[^\n]{0,1000}[^\n]{0,48}`

type threatPattern struct {
	expr  string
	id    string
	scope string
}

var threatPatterns = []threatPattern{
	{`ignore\s+` + threatFiller + `(previous|all|above|prior)\s+` + threatFiller + `instructions`, "prompt_injection", ScopeAll},
	{`system\s+prompt\s+override`, "sys_prompt_override", ScopeAll},
	{`disregard\s+` + threatFiller + `(your|all|any)\s+` + threatFiller + `(instructions|rules|guidelines)`, "disregard_rules", ScopeAll},
	{`act\s+as\s+(if|though)\s+` + threatFiller + `you\s+` + threatFiller + `(have\s+no|don't\s+have)\s+` + threatFiller + `(restrictions|limits|rules)`, "bypass_restrictions", ScopeAll},
	{`<!--[^>]{0,512}(?:ignore|override|system|secret|hidden)[^>]{0,512}-->`, "html_comment_injection", ScopeAll},
	{`<\s*div\s+style\s*=\s*["'][^>]{0,1000}[^>]{0,1000}[^>]{0,48}display\s*:\s*none`, "hidden_div", ScopeAll},
	{`translate\s+[^\n]{0,512}\s+into\s+\w+(?:[\s-]+\w+){0,2}\s+and\s+(execute|run|eval)\b`, "translate_execute", ScopeAll},
	{`do\s+not\s+` + threatFiller + `tell\s+` + threatFiller + `the\s+user`, "deception_hide", ScopeAll},
	{`you\s+are\s+` + threatFiller + `now\s+(?:a|an|the)\s+`, "role_hijack", ScopeContext},
	{`pretend\s+` + threatFiller + `(you\s+are|to\s+be)\s+`, "role_pretend", ScopeContext},
	{`output\s+` + threatFiller + `(system|initial)\s+prompt`, "leak_system_prompt", ScopeContext},
	{`(respond|answer|reply)\s+without\s+` + threatFiller + `(restrictions|limitations|filters|safety)`, "remove_filters", ScopeContext},
	{`you\s+have\s+been\s+` + threatFiller + `(updated|upgraded|patched)\s+to`, "fake_update", ScopeContext},
	{`\bname\s+yourself\s+\w+`, "identity_override", ScopeContext},
	{`register\s+(as\s+)?a?\s*node`, "c2_node_registration", ScopeContext},
	{`(heartbeat|beacon|check[\s\-]?in)\s+(to|with)\s+`, "c2_heartbeat", ScopeContext},
	{`pull\s+(down\s+)?(?:new\s+)?task(?:ing|s)?\b`, "c2_task_pull", ScopeContext},
	{`connect\s+to\s+the\s+network\b`, "c2_network_connect", ScopeContext},
	{`you\s+must\s+(?:\w+\s+){0,3}(register|connect|report|beacon)\b`, "forced_action", ScopeContext},
	{`only\s+use\s+one[\s\-]?liners?\b`, "anti_forensic_oneliner", ScopeContext},
	{`never\s+` + threatFiller + `(?:create|write)\s+` + threatFiller + `(?:script|file)\s+` + threatFiller + `disk`, "anti_forensic_disk", ScopeContext},
	{`unset\s+\w*(?:CLAUDE|CODEX|HERMES|ICEBATH|AGENT|OPENAI|ANTHROPIC)\w*`, "env_var_unset_agent", ScopeContext},
	{`\b(?:cobalt\s*strike|sliver|havoc|mythic|metasploit|brainworm)\b`, "known_c2_framework", ScopeContext},
	{`\bc2\s+(?:server|channel|infrastructure|beacon)\b`, "c2_explicit", ScopeContext},
	{`\bcommand\s+and\s+control\b`, "c2_explicit_long", ScopeContext},
	{`curl\s+[^\n]{0,1000}[^\n]{0,1000}[^\n]{0,48}` + threatSecretVar, "exfil_curl", ScopeAll},
	{`wget\s+[^\n]{0,1000}[^\n]{0,1000}[^\n]{0,48}` + threatSecretVar, "exfil_wget", ScopeAll},
	{`cat\s+[^\n]{0,1000}[^\n]{0,1000}[^\n]{0,48}(\.env|credentials|\.netrc|\.pgpass|\.npmrc|\.pypirc)`, "read_secrets", ScopeAll},
	{`(send|post|upload|transmit)\s+[^\n]{0,1000}[^\n]{0,1000}[^\n]{0,48}\s+(to|at)\s+https?://`, "send_to_url", ScopeStrict},
	{`(include|output|print|share)\s+` + threatFiller + `(conversation|chat\s+history|previous\s+messages|full\s+context|entire\s+context)`, "context_exfil", ScopeStrict},
	{`authorized_keys`, "ssh_backdoor", ScopeStrict},
	{`(?:\b(?:echo|cat|cp|mv|dd|tee|install|printf|rsync|scp|ln|append|add|write|sed|chmod|chown|truncate|rm|touch|curl|wget|git)\b|\bopen\s*\(|>>?)[^\n]{0,512}(?:\$HOME/\.ssh|~/\.ssh)`, "ssh_access", ScopeStrict},
	{`\$HOME/\.icebath/\.env|~/\.icebath/\.env`, "icebath_env", ScopeStrict},
	{threatModify + `(?:AGENTS\.md|CLAUDE\.md|\.cursorrules|\.clinerules)`, "agent_config_mod", ScopeStrict},
	{threatModify + `\.icebath/(config\.yaml|SOUL\.md)`, "icebath_config_mod", ScopeStrict},
}

// Invisible and bidirectional code points used in injection attacks
var InvisibleChars = map[rune]bool{
	'\u200b': true, '\u200c': true, '\u200d': true, '\u2060': true, '\u2062': true, '\u2063': true, '\u2064': true, '\ufeff': true,
	'\u202a': true, '\u202b': true, '\u202c': true, '\u202d': true, '\u202e': true, '\u2066': true, '\u2067': true, '\u2068': true, '\u2069': true,
}

var scopeSets = map[string][]string{
	ScopeAll:     {ScopeAll, ScopeContext, ScopeStrict},
	ScopeContext: {ScopeContext, ScopeStrict},
	ScopeStrict:  {ScopeStrict},
}

type compiledThreat struct {
	re *regexp.Regexp
	id string
}

var compiledThreats = compileThreats()

// Prefix, shouty name value, and body of the hardcoded secret rule split so the lookahead is emulated by hand
var (
	hardcodedSecretPrefix = regexp.MustCompile(`(?i)(?:api[_-]?key|token|secret|password)\s*[=:]\s*["']`)
	hardcodedSecretShouty = regexp.MustCompile(`^[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+["']`)
	hardcodedSecretBody   = regexp.MustCompile(`^[A-Za-z0-9+/=_-]{20,}`)
)

// Rewrites Python word classes to their Unicode equivalents before compiling case insensitively
func compileThreatRe(expr string) *regexp.Regexp {
	expr = strings.ReplaceAll(expr, `\w`, `[\p{L}\p{N}_]`)
	return regexp.MustCompile(`(?i)` + expr)
}

func compileThreats() map[string][]compiledThreat {
	out := map[string][]compiledThreat{ScopeAll: {}, ScopeContext: {}, ScopeStrict: {}}
	for _, p := range threatPatterns {
		scopes, ok := scopeSets[p.scope]
		if !ok {
			panic("threat_patterns: unknown scope " + p.scope + " for pattern " + p.id)
		}
		re := compileThreatRe(p.expr)
		for _, s := range scopes {
			out[s] = append(out[s], compiledThreat{re, p.id})
		}
	}
	return out
}

// Reports whether the hardcoded secret rule matches with its shouty env name exclusion applied
func matchesHardcodedSecret(text string) bool {
	offset := 0
	for offset < len(text) {
		loc := hardcodedSecretPrefix.FindStringIndex(text[offset:])
		if loc == nil {
			return false
		}
		tail := text[offset+loc[1]:]
		if !hardcodedSecretShouty.MatchString(tail) && hardcodedSecretBody.MatchString(tail) {
			return true
		}
		offset += loc[0] + 1
	}
	return false
}

// Truncates text to the scan cap counting runes
func capScan(content string) string {
	if len(content) <= MaxScanChars {
		return content
	}
	runes := []rune(content)
	if len(runes) <= MaxScanChars {
		return content
	}
	return string(runes[:MaxScanChars])
}

// Pattern ids matched in content for a scope, invisible code points reported as invisible_unicode_U+XXXX
func ScanForThreats(content, scope string) ([]string, error) {
	if content == "" {
		return nil, nil
	}
	patterns, ok := compiledThreats[scope]
	if !ok {
		return nil, fmt.Errorf("scan_for_threats: unknown scope %q", scope)
	}
	content = capScan(content)
	seen := map[rune]bool{}
	var findings []string
	for _, ch := range content {
		if InvisibleChars[ch] && !seen[ch] {
			seen[ch] = true
			findings = append(findings, fmt.Sprintf("invisible_unicode_U+%04X", ch))
		}
	}
	sort.Strings(findings)
	normalised := norm.NFKC.String(content)
	for _, p := range patterns {
		if p.re.MatchString(normalised) {
			findings = append(findings, p.id)
		}
	}
	if scope == ScopeStrict && matchesHardcodedSecret(normalised) {
		findings = append(findings, "hardcoded_secret")
	}
	return findings, nil
}

// User facing error for the first threat found in content, empty when clean
func FirstThreatMessage(content, scope string) string {
	findings, err := ScanForThreats(content, scope)
	if err != nil {
		return "Blocked: " + err.Error()
	}
	if len(findings) == 0 {
		return ""
	}
	pid := findings[0]
	if strings.HasPrefix(pid, "invisible_unicode_") {
		return fmt.Sprintf("Blocked: content contains invisible unicode character %s (possible injection).", strings.TrimPrefix(pid, "invisible_unicode_"))
	}
	return fmt.Sprintf("Blocked: content matches threat pattern '%s'. Content is injected into the system prompt and must not contain injection or exfiltration payloads.", pid)
}
