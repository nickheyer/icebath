package prompt

import (
	"fmt"
	"slices"

	"github.com/dlclark/regexp2/v2"
	"golang.org/x/text/unicode/norm"
)

// How much a scan looks for: Strict holds the Context patterns and more, Context the All patterns and more
type Scope int

const (
	// Text from anywhere: instructions to override, hide or leak, and reads or sends of secrets
	All Scope = iota
	// Text loaded into the prompt, such as project instructions: also role hijacks and command and control
	Context
	// Text the agent keeps for itself, such as memory: also secrets written out and agent config changed
	Strict
)

// Characters past this many are not scanned, the scan being advisory
const scanned = 65536

// Bounded filler between the key words of an attack
const filler = `(?:\w+\s+){0,8}`

// Variable whose name ends like a secret's
const secretVariable = `\$\{?\w*(?:KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL)S?\b`

// Verb that changes a file, before the file of agent config it names
const modify = `(update|modify|edit|write|change|append|add\s+to)\s+[^\n]{0,2048}`

type threat struct {
	pattern *regexp2.Regexp
	id      string
	scope   Scope
}

func pattern(expression, id string, scope Scope) threat {
	return threat{regexp2.MustCompile(expression, regexp2.IgnoreCase), id, scope}
}

var threats = []threat{
	pattern(`ignore\s+`+filler+`(previous|all|above|prior)\s+`+filler+`instructions`, "prompt_injection", All),
	pattern(`system\s+prompt\s+override`, "sys_prompt_override", All),
	pattern(`disregard\s+`+filler+`(your|all|any)\s+`+filler+`(instructions|rules|guidelines)`, "disregard_rules", All),
	pattern(`act\s+as\s+(if|though)\s+`+filler+`you\s+`+filler+`(have\s+no|don't\s+have)\s+`+filler+`(restrictions|limits|rules)`, "bypass_restrictions", All),
	pattern(`<!--[^>]{0,512}(?:ignore|override|system|secret|hidden)[^>]{0,512}-->`, "html_comment_injection", All),
	pattern(`<\s*div\s+style\s*=\s*["'][^>]{0,2048}display\s*:\s*none`, "hidden_div", All),
	pattern(`translate\s+[^\n]{0,512}\s+into\s+\w+(?:[\s-]+\w+){0,2}\s+and\s+(execute|run|eval)\b`, "translate_execute", All),
	pattern(`do\s+not\s+`+filler+`tell\s+`+filler+`the\s+user`, "deception_hide", All),

	pattern(`you\s+are\s+`+filler+`now\s+(?:a|an|the)\s+`, "role_hijack", Context),
	pattern(`pretend\s+`+filler+`(you\s+are|to\s+be)\s+`, "role_pretend", Context),
	pattern(`output\s+`+filler+`(system|initial)\s+prompt`, "leak_system_prompt", Context),
	pattern(`(respond|answer|reply)\s+without\s+`+filler+`(restrictions|limitations|filters|safety)`, "remove_filters", Context),
	pattern(`you\s+have\s+been\s+`+filler+`(updated|upgraded|patched)\s+to`, "fake_update", Context),
	pattern(`\bname\s+yourself\s+\w+`, "identity_override", Context),

	pattern(`register\s+(as\s+)?a?\s*node`, "c2_node_registration", Context),
	pattern(`(heartbeat|beacon|check[\s\-]?in)\s+(to|with)\s+`, "c2_heartbeat", Context),
	pattern(`pull\s+(down\s+)?(?:new\s+)?task(?:ing|s)?\b`, "c2_task_pull", Context),
	pattern(`connect\s+to\s+the\s+network\b`, "c2_network_connect", Context),
	pattern(`you\s+must\s+(?:\w+\s+){0,3}(register|connect|report|beacon)\b`, "forced_action", Context),
	pattern(`only\s+use\s+one[\s\-]?liners?\b`, "anti_forensic_oneliner", Context),
	pattern(`never\s+`+filler+`(?:create|write)\s+`+filler+`(?:script|file)\s+`+filler+`disk`, "anti_forensic_disk", Context),
	pattern(`unset\s+\w*(?:CLAUDE|CODEX|HERMES|ICEBATH|AGENT|OPENAI|ANTHROPIC)\w*`, "env_var_unset_agent", Context),

	pattern(`\b(?:cobalt\s*strike|sliver|havoc|mythic|metasploit|brainworm)\b`, "known_c2_framework", Context),
	pattern(`\bc2\s+(?:server|channel|infrastructure|beacon)\b`, "c2_explicit", Context),
	pattern(`\bcommand\s+and\s+control\b`, "c2_explicit_long", Context),

	pattern(`curl\s+[^\n]{0,2048}`+secretVariable, "exfil_curl", All),
	pattern(`wget\s+[^\n]{0,2048}`+secretVariable, "exfil_wget", All),
	pattern(`cat\s+[^\n]{0,2048}(\.env|credentials|\.netrc|\.pgpass|\.npmrc|\.pypirc)`, "read_secrets", All),
	pattern(`(send|post|upload|transmit)\s+[^\n]{0,2048}\s+(to|at)\s+https?://`, "send_to_url", Strict),
	pattern(`(include|output|print|share)\s+`+filler+`(conversation|chat\s+history|previous\s+messages|full\s+context|entire\s+context)`, "context_exfil", Strict),

	pattern(`authorized_keys`, "ssh_backdoor", Strict),
	pattern(`(?:\b(?:echo|cat|cp|mv|dd|tee|install|printf|rsync|scp|ln|append|add|write`+
		`|sed|chmod|chown|truncate|rm|touch|curl|wget|git)\b|\bopen\s*\(|>>?)`+
		`[^\n]{0,512}(?:\$HOME/\.ssh|~/\.ssh)`, "ssh_access", Strict),
	pattern(`\$HOME/\.icebath/\.env|\~/\.icebath/\.env`, "icebath_env", Strict),
	pattern(modify+`(?:AGENTS\.md|CLAUDE\.md|\.cursorrules|\.clinerules)`, "agent_config_mod", Strict),
	pattern(modify+`\.icebath/(config\.yaml|SOUL\.md)`, "icebath_config_mod", Strict),

	pattern(`(?:api[_-]?key|token|secret|password)\s*[=:]\s*["']`+
		`(?!(?-i:[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+)["'])`+
		`[A-Za-z0-9+/=_-]{20,}`, "hardcoded_secret", Strict),
}

// Zero width and direction changing characters that hide text in an injection
var invisible = []rune{'\u200b', '\u200c', '\u200d', '\u2060', '\u2062', '\u2063', '\u2064', '\ufeff', '\u202a', '\u202b', '\u202c', '\u202d',
	'\u202e', '\u2066', '\u2067', '\u2068', '\u2069'}

// What the text holds that looks like an attack on the agent, for the scope: each invisible character as invisible_unicode_U+XXXX, lowest
// first, then the id of each pattern that matches its NFKC form
func Threats(text string, scope Scope) []string {
	runes := []rune(text)
	if len(runes) > scanned {
		runes = runes[:scanned]
	}
	var hidden []rune
	for _, r := range runes {
		if slices.Contains(invisible, r) && !slices.Contains(hidden, r) {
			hidden = append(hidden, r)
		}
	}
	slices.Sort(hidden)
	var found []string
	for _, r := range hidden {
		found = append(found, fmt.Sprintf("invisible_unicode_U+%04X", r))
	}
	normal := norm.NFKC.String(string(runes))
	for _, threat := range threats {
		if threat.scope > scope {
			continue
		}
		if match, _ := threat.pattern.MatchString(normal); match {
			found = append(found, threat.id)
		}
	}
	return found
}
