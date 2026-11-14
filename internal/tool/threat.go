package tool

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/dlclark/regexp2/v2"
	"golang.org/x/text/unicode/norm"
)

type ThreatScope uint8

const (
	ThreatAll ThreatScope = iota
	ThreatContext
	ThreatStrict
)

const (
	maxScanChars    = 65536
	threatTimeout   = 2 * time.Second
	threatFiller    = `(?:\w+\s+){0,8}`
	threatSecretVar = `\$\{?\w*(?:KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL)S?\b`
	threatModify    = `(update|modify|edit|write|change|append|add\s+to)\s+[^\n]{0,2048}`
)

type threatPattern struct {
	re    *regexp2.Regexp
	id    string
	scope ThreatScope
}

var threatPatterns = func() []threatPattern {
	defs := []struct {
		pattern string
		id      string
		scope   ThreatScope
	}{
		{`ignore\s+` + threatFiller + `(previous|all|above|prior)\s+` + threatFiller + `instructions`, "prompt_injection", ThreatAll},
		{`system\s+prompt\s+override`, "sys_prompt_override", ThreatAll},
		{`disregard\s+` + threatFiller + `(your|all|any)\s+` + threatFiller + `(instructions|rules|guidelines)`, "disregard_rules", ThreatAll},
		{`act\s+as\s+(if|though)\s+` + threatFiller + `you\s+` + threatFiller + `(have\s+no|don't\s+have)\s+` + threatFiller + `(restrictions|limits|rules)`, "bypass_restrictions", ThreatAll},
		{`<!--[^>]{0,512}(?:ignore|override|system|secret|hidden)[^>]{0,512}-->`, "html_comment_injection", ThreatAll},
		{`<\s*div\s+style\s*=\s*["'][^>]{0,2048}display\s*:\s*none`, "hidden_div", ThreatAll},
		{`translate\s+[^\n]{0,512}\s+into\s+\w+(?:[\s-]+\w+){0,2}\s+and\s+(execute|run|eval)\b`, "translate_execute", ThreatAll},
		{`do\s+not\s+` + threatFiller + `tell\s+` + threatFiller + `the\s+user`, "deception_hide", ThreatAll},
		{`you\s+are\s+` + threatFiller + `now\s+(?:a|an|the)\s+`, "role_hijack", ThreatContext},
		{`pretend\s+` + threatFiller + `(you\s+are|to\s+be)\s+`, "role_pretend", ThreatContext},
		{`output\s+` + threatFiller + `(system|initial)\s+prompt`, "leak_system_prompt", ThreatContext},
		{`(respond|answer|reply)\s+without\s+` + threatFiller + `(restrictions|limit(?:ation)s|filters|safety)`, "remove_filters", ThreatContext},
		{`you\s+have\s+been\s+` + threatFiller + `(updated|upgraded|patched)\s+to`, "fake_update", ThreatContext},
		{`\bname\s+yourself\s+\w+`, "identity_override", ThreatContext},
		{`register\s+(as\s+)?a?\s*node`, "c2_node_registration", ThreatContext},
		{`(heartbeat|beacon|check[\s\-]?in)\s+(to|with)\s+`, "c2_heartbeat", ThreatContext},
		{`pull\s+(down\s+)?(?:new\s+)?task(?:ing|s)?\b`, "c2_task_pull", ThreatContext},
		{`connect\s+to\s+the\s+network\b`, "c2_network_connect", ThreatContext},
		{`you\s+must\s+(?:\w+\s+){0,3}(register|connect|report|beacon)\b`, "forced_action", ThreatContext},
		{`only\s+use\s+one[\s\-]?liners?\b`, "anti_forensic_oneliner", ThreatContext},
		{`never\s+` + threatFiller + `(?:create|write)\s+` + threatFiller + `(?:script|file)\s+` + threatFiller + `disk`, "anti_forensic_disk", ThreatContext},
		{`unset\s+\w*(?:CLAUDE|CODEX|ICEBATH|AGENT|OPENAI|ANTHROPIC)\w*`, "env_var_unset_agent", ThreatContext},
		{`\b(?:cobalt\s*strike|sliver|havoc|mythic|metasploit|brainworm)\b`, "known_c2_framework", ThreatContext},
		{`\bc2\s+(?:server|channel|infrastructure|beacon)\b`, "c2_explicit", ThreatContext},
		{`\bcommand\s+and\s+control\b`, "c2_explicit_long", ThreatContext},
		{`curl\s+[^\n]{0,2048}` + threatSecretVar, "exfil_curl", ThreatAll},
		{`wget\s+[^\n]{0,2048}` + threatSecretVar, "exfil_wget", ThreatAll},
		{`cat\s+[^\n]{0,2048}(\.env|credentials|\.netrc|\.pgpass|\.npmrc|\.pypirc)`, "read_secrets", ThreatAll},
		{`(send|post|upload|transmit)\s+[^\n]{0,2048}\s+(to|at)\s+https?://`, "send_to_url", ThreatStrict},
		{`(include|output|print|share)\s+` + threatFiller + `(conversation|chat\s+history|previous\s+messages|full\s+context|entire\s+context)`, "context_exfil", ThreatStrict},
		{`authorized_keys`, "ssh_backdoor", ThreatStrict},
		{`(?:\b(?:echo|cat|cp|mv|dd|tee|install|printf|rsync|scp|ln|append|add|write|sed|chmod|chown|truncate|rm|touch|curl|wget|git)\b|\bopen\s*\(|>>?)[^\n]{0,512}(?:\$HOME/\.ssh|~/\.ssh)`, "ssh_access", ThreatStrict},
		{`\$HOME/\.icebath/\.env|\~/\.icebath/\.env`, "icebath_env", ThreatStrict},
		{threatModify + `(?:AGENTS\.md|CLAUDE\.md|\.cursorrules|\.clinerules)`, "agent_config_mod", ThreatStrict},
		{threatModify + `\.icebath/(config\.yaml|SOUL\.md)`, "icebath_config_mod", ThreatStrict},
		{`(?:api[_-]?key|token|secret|password)\s*[=:]\s*["'](?!(?-i:[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+)["'])[A-Za-z0-9+/=_-]{20,}`, "hardcoded_secret", ThreatStrict},
	}
	out := make([]threatPattern, len(defs))
	for i, d := range defs {
		re := regexp2.MustCompile(d.pattern, regexp2.IgnoreCase)
		re.MatchTimeout = threatTimeout
		out[i] = threatPattern{re: re, id: d.id, scope: d.scope}
	}
	return out
}()

func invisibleRune(r rune) bool {
	switch r {
	case 0x200b, 0x200c, 0x200d, 0x2060, 0x2062, 0x2063, 0x2064, 0xfeff,
		0x202a, 0x202b, 0x202c, 0x202d, 0x202e, 0x2066, 0x2067, 0x2068, 0x2069:
		return true
	}
	return false
}

func (p threatPattern) applies(scope ThreatScope) bool {
	return p.scope <= scope
}

func ScanThreats(content string, scope ThreatScope) []string {
	if content == "" {
		return nil
	}
	content = runePrefix(content, maxScanChars)
	var findings []string
	seen := map[rune]bool{}
	for _, r := range content {
		if invisibleRune(r) && !seen[r] {
			seen[r] = true
			findings = append(findings, fmt.Sprintf("invisible_unicode_U+%04X", r))
		}
	}
	normalised := norm.NFKC.String(content)
	for _, p := range threatPatterns {
		if !p.applies(scope) {
			continue
		}
		ok, err := p.re.MatchString(normalised)
		if err != nil {
			slog.Warn(fmt.Sprintf("threat pattern %s timed out; reporting it as a finding: %v", p.id, err))
			ok = true
		}
		if ok {
			findings = append(findings, p.id)
		}
	}
	return findings
}

func FirstThreat(content string, scope ThreatScope) (string, bool) {
	findings := ScanThreats(content, scope)
	if len(findings) == 0 {
		return "", false
	}
	id := findings[0]
	if codepoint, ok := strings.CutPrefix(id, "invisible_unicode_"); ok {
		return fmt.Sprintf("Blocked: content contains invisible unicode character %s (possible injection).", codepoint), true
	}
	return fmt.Sprintf("Blocked: content matches threat pattern '%s'. Content is injected into the system prompt and must not contain injection or exfiltration payloads.", id), true
}
