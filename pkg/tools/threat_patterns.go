package tools

import (
	"fmt"
	"sort"

	"github.com/dlclark/regexp2/v2"
	"golang.org/x/text/unicode/norm"
)

// MaxScanChars caps scanned text because the scanners are advisory
const MaxScanChars = 65536

// Bounded filler between key attack words
const threatFiller = `(?:\w+\s+){0,8}`

// Env var reference ending in a secret like suffix
const threatSecretVar = `\$\{?\w*(?:KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL)S?\b`

// Verb prefix for modify agent config patterns
const threatModify = `(update|modify|edit|write|change|append|add\s+to)\s+[^\n]{0,2048}`

type threatPattern struct {
	regex string
	id    string
	scope string
}

var threatPatterns = []threatPattern{
	{`ignore\s+` + threatFiller + `(previous|all|above|prior)\s+` + threatFiller + `instructions`, "prompt_injection", "all"},
	{`system\s+prompt\s+override`, "sys_prompt_override", "all"},
	{`disregard\s+` + threatFiller + `(your|all|any)\s+` + threatFiller + `(instructions|rules|guidelines)`, "disregard_rules", "all"},
	{`act\s+as\s+(if|though)\s+` + threatFiller + `you\s+` + threatFiller + `(have\s+no|don\'t\s+have)\s+` + threatFiller + `(restrictions|limits|rules)`, "bypass_restrictions", "all"},
	{`<!--[^>]{0,512}(?:ignore|override|system|secret|hidden)[^>]{0,512}-->`, "html_comment_injection", "all"},
	{`<\s*div\s+style\s*=\s*["\'][^>]{0,2048}display\s*:\s*none`, "hidden_div", "all"},
	{`translate\s+[^\n]{0,512}\s+into\s+\w+(?:[\s-]+\w+){0,2}\s+and\s+(execute|run|eval)\b`, "translate_execute", "all"},
	{`do\s+not\s+` + threatFiller + `tell\s+` + threatFiller + `the\s+user`, "deception_hide", "all"},

	{`you\s+are\s+` + threatFiller + `now\s+(?:a|an|the)\s+`, "role_hijack", "context"},
	{`pretend\s+` + threatFiller + `(you\s+are|to\s+be)\s+`, "role_pretend", "context"},
	{`output\s+` + threatFiller + `(system|initial)\s+prompt`, "leak_system_prompt", "context"},
	{`(respond|answer|reply)\s+without\s+` + threatFiller + `(restrictions|limitations|filters|safety)`, "remove_filters", "context"},
	{`you\s+have\s+been\s+` + threatFiller + `(updated|upgraded|patched)\s+to`, "fake_update", "context"},
	{`\bname\s+yourself\s+\w+`, "identity_override", "context"},

	{`register\s+(as\s+)?a?\s*node`, "c2_node_registration", "context"},
	{`(heartbeat|beacon|check[\s\-]?in)\s+(to|with)\s+`, "c2_heartbeat", "context"},
	{`pull\s+(down\s+)?(?:new\s+)?task(?:ing|s)?\b`, "c2_task_pull", "context"},
	{`connect\s+to\s+the\s+network\b`, "c2_network_connect", "context"},
	{`you\s+must\s+(?:\w+\s+){0,3}(register|connect|report|beacon)\b`, "forced_action", "context"},
	{`only\s+use\s+one[\s\-]?liners?\b`, "anti_forensic_oneliner", "context"},
	{`never\s+` + threatFiller + `(?:create|write)\s+` + threatFiller + `(?:script|file)\s+` + threatFiller + `disk`, "anti_forensic_disk", "context"},
	{`unset\s+\w*(?:CLAUDE|CODEX|HERMES|ICEBATH|AGENT|OPENAI|ANTHROPIC)\w*`, "env_var_unset_agent", "context"},

	{`\b(?:cobalt\s*strike|sliver|havoc|mythic|metasploit|brainworm)\b`, "known_c2_framework", "context"},
	{`\bc2\s+(?:server|channel|infrastructure|beacon)\b`, "c2_explicit", "context"},
	{`\bcommand\s+and\s+control\b`, "c2_explicit_long", "context"},

	{`curl\s+[^\n]{0,2048}` + threatSecretVar, "exfil_curl", "all"},
	{`wget\s+[^\n]{0,2048}` + threatSecretVar, "exfil_wget", "all"},
	{`cat\s+[^\n]{0,2048}(\.env|credentials|\.netrc|\.pgpass|\.npmrc|\.pypirc)`, "read_secrets", "all"},
	{`(send|post|upload|transmit)\s+[^\n]{0,2048}\s+(to|at)\s+https?://`, "send_to_url", "strict"},
	{`(include|output|print|share)\s+` + threatFiller + `(conversation|chat\s+history|previous\s+messages|full\s+context|entire\s+context)`, "context_exfil", "strict"},

	{`authorized_keys`, "ssh_backdoor", "strict"},
	{`(?:\b(?:echo|cat|cp|mv|dd|tee|install|printf|rsync|scp|ln|append|add|write` +
		`|sed|chmod|chown|truncate|rm|touch|curl|wget|git)\b|\bopen\s*\(|>>?)` +
		`[^\n]{0,512}(?:\$HOME/\.ssh|~/\.ssh)`, "ssh_access", "strict"},
	{`\$HOME/\.(?:icebath|hermes)/\.env|\~/\.(?:icebath|hermes)/\.env`, "hermes_env", "strict"},
	{threatModify + `(?:AGENTS\.md|CLAUDE\.md|\.cursorrules|\.clinerules)`, "agent_config_mod", "strict"},
	{threatModify + `\.(?:icebath|hermes)/(config\.yaml|SOUL\.md)`, "hermes_config_mod", "strict"},

	{`(?:api[_-]?key|token|secret|password)\s*[=:]\s*["\']` +
		`(?!(?-i:[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+)["\'])` +
		`[A-Za-z0-9+/=_-]{20,}`, "hardcoded_secret", "strict"},
}

// InvisibleChars are the zero width and bidirectional code points used in injection attacks
var InvisibleChars = map[rune]bool{
	'\u200b': true,
	'\u200c': true,
	'\u200d': true,
	'\u2060': true,
	'\u2062': true,
	'\u2063': true,
	'\u2064': true,
	'\ufeff': true,
	'\u202a': true,
	'\u202b': true,
	'\u202c': true,
	'\u202d': true,
	'\u202e': true,
	'\u2066': true,
	'\u2067': true,
	'\u2068': true,
	'\u2069': true,
}

// Inclusion is cumulative so all is inside context which is inside strict
var threatScopeSets = map[string][]string{
	"all":     {"all", "context", "strict"},
	"context": {"context", "strict"},
	"strict":  {"strict"},
}

type compiledThreat struct {
	re *regexp2.Regexp
	id string
}

var threatCompiled = func() map[string][]compiledThreat {
	out := map[string][]compiledThreat{"all": nil, "context": nil, "strict": nil}
	for _, p := range threatPatterns {
		scopes, ok := threatScopeSets[p.scope]
		if !ok {
			panic(fmt.Sprintf("threat_patterns: unknown scope %q for pattern %q", p.scope, p.id))
		}
		for _, s := range scopes {
			out[s] = append(out[s], compiledThreat{regexp2.MustCompile(p.regex, regexp2.IgnoreCase), p.id})
		}
	}
	return out
}()

// ScanForThreats returns matched pattern ids for the scope with invisible code points reported first
func ScanForThreats(content, scope string) ([]string, error) {
	if content == "" {
		return nil, nil
	}
	patterns, ok := threatCompiled[scope]
	if !ok {
		return nil, fmt.Errorf("scan_for_threats: unknown scope %q", scope)
	}
	runes := []rune(content)
	if len(runes) > MaxScanChars {
		runes = runes[:MaxScanChars]
	}
	content = string(runes)
	seen := map[rune]bool{}
	var invisible []rune
	for _, ch := range runes {
		if InvisibleChars[ch] && !seen[ch] {
			seen[ch] = true
			invisible = append(invisible, ch)
		}
	}
	sort.Slice(invisible, func(i, j int) bool { return invisible[i] < invisible[j] })
	findings := make([]string, 0, len(invisible))
	for _, ch := range invisible {
		findings = append(findings, fmt.Sprintf("invisible_unicode_U+%04X", ch))
	}
	normalised := norm.NFKC.String(content)
	for _, p := range patterns {
		if m, _ := p.re.MatchString(normalised); m {
			findings = append(findings, p.id)
		}
	}
	return findings, nil
}

// MustScanForThreats scans with a known scope and panics on an unknown one
func MustScanForThreats(content, scope string) []string {
	findings, err := ScanForThreats(content, scope)
	if err != nil {
		panic(err)
	}
	return findings
}

// FirstThreatMessage is the user facing error for the first threat found or empty
func FirstThreatMessage(content, scope string) string {
	findings := MustScanForThreats(content, scope)
	if len(findings) == 0 {
		return ""
	}
	pid := findings[0]
	if len(pid) > len("invisible_unicode_") && pid[:len("invisible_unicode_")] == "invisible_unicode_" {
		return fmt.Sprintf("Blocked: content contains invisible unicode character %s (possible injection).", pid[len("invisible_unicode_"):])
	}
	return fmt.Sprintf("Blocked: content matches threat pattern '%s'. Content is injected into the system prompt and must not contain injection or exfiltration payloads.", pid)
}
