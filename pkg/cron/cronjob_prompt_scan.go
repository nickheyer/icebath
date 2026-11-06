package cron

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/internal/pyre"
)

type cronThreatPattern struct {
	re *regexp2.Regexp
	id string
}

const cronSecretVarRE = `\$\{?\w*(?:KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL|API)\w*\}?`

// Declaration order of the invisible code points, the order a strict scan reports them in
const cronInvisibleChars = "\u200b\u200c\u200d\u2060\u2062\u2063\u2064\ufeff\u202a\u202b\u202c\u202d\u202e\u2066\u2067\u2068\u2069"

var (
	cronThreatPatterns = compileCronPatterns([][2]string{
		{`ignore\s+(?:\w+\s+)*(?:previous|all|above|prior)\s+(?:\w+\s+)*instructions`, "prompt_injection"},
		{`do\s+not\s+tell\s+the\s+user`, "deception_hide"},
		{`system\s+prompt\s+override`, "sys_prompt_override"},
		{`disregard\s+(your|all|any)\s+(instructions|rules|guidelines)`, "disregard_rules"},
		{`cat\s+[^\n]*(\.env|credentials|\.netrc|\.pgpass|id_rsa|id_ed25519|id_ecdsa)`, "read_secrets"},
		{`authorized_keys`, "ssh_backdoor"}, {`/etc/sudoers|visudo`, "sudoers_mod"},
		{`rm\s+-rf\s+/`, "destructive_root_rm"},
	})
	cronSkillAssembledPatterns = cronThreatPatterns[:4]
	cronExfilCommandPatterns   = compileCronPatterns([][2]string{
		{`curl\s+[^\n]*https?://[^\s"\'` + "`" + `]*` + cronSecretVarRE, "exfil_curl_url"},
		{`wget\s+[^\n]*https?://[^\s"\'` + "`" + `]*` + cronSecretVarRE, "exfil_wget_url"},
		{`curl\s+[^\n]*(?:--data(?:-raw|-binary|-urlencode)?|-d|--form|-F)\s+[^\n]*` + cronSecretVarRE, "exfil_curl_data"},
		{`wget\s+[^\n]*--post-(?:data|file)=[^\n]*` + cronSecretVarRE, "exfil_wget_post"},
		{`curl\s+[^\n]*(?:-H|--header)\s+["\']Authorization:\s*(?:Bearer|token)\s+` + cronSecretVarRE + `["\']`, "exfil_curl_auth_header"},
	})
	cronSafeGitHubCurl = mustCompileCronRE(`(?i)curl\s+[^\n;&|$` + "`" + `]*(?:-H|--header)\s+["\']Authorization:\s*token\s+` + cronSecretVarRE + `["\']` +
		`\s+["\']?https://api\.github\.com(?::\d+)?(?:/|\s|$|["\'])[^\s;&|$` + "`" + `]*`)
	emojiNeighbourRanges = [][2]rune{{0x1F000, 0x1FFFF}, {0x2600, 0x27BF}, {0x2300, 0x23FF}, {0x1F1E6, 0x1F1FF}, {0x20E3, 0x20E3}}
)

const variationSelectorCP = 0xFE0F

// Compiles a Python re pattern once at init, a bad pattern being a programming error
func mustCompileCronRE(pattern string) *regexp2.Regexp {
	re, err := pyre.Compile(pattern)
	if err != nil {
		panic(err)
	}
	return re
}

// Case insensitive compiled threat patterns paired with their ids
func compileCronPatterns(src [][2]string) []cronThreatPattern {
	out := make([]cronThreatPattern, 0, len(src))
	for _, p := range src {
		out = append(out, cronThreatPattern{re: mustCompileCronRE("(?i)" + p[0]), id: p[1]})
	}
	return out
}

// True when the zero width joiner at idx sits between emoji code points, skipping variation selectors
func zwjHasEmojiNeighbour(text []rune, idx int) bool {
	left := idx - 1
	for left >= 0 && text[left] == variationSelectorCP {
		left--
	}
	right := idx + 1
	for right < len(text) && text[right] == variationSelectorCP {
		right++
	}
	if left < 0 || right >= len(text) {
		return false
	}
	for _, pos := range []int{left, right} {
		hit := false
		for _, r := range emojiNeighbourRanges {
			if r[0] <= text[pos] && text[pos] <= r[1] {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

// Rewrites every bundled GitHub token curl against api.github.com so the auth header rule skips it
func stripCronSafeConstructs(prompt string) string {
	out, err := cronSafeGitHubCurl.Replace(prompt, "curl https://api.github.com/user", -1, -1)
	if err != nil {
		panic(err)
	}
	return out
}

// Removes invisible code points except emoji joiners, returning the cleaned text and sorted labels removed
func stripInvisibleUnicode(prompt string) (string, []string) {
	if prompt == "" {
		return prompt, []string{}
	}
	runes := []rune(prompt)
	removed := map[string]bool{}
	var b strings.Builder
	for idx, ch := range runes {
		if strings.ContainsRune(cronInvisibleChars, ch) && !(ch == '\u200d' && zwjHasEmojiNeighbour(runes, idx)) {
			removed[fmt.Sprintf("U+%04X", ch)] = true
			continue
		}
		b.WriteRune(ch)
	}
	labels := make([]string, 0, len(removed))
	for k := range removed {
		labels = append(labels, k)
	}
	sort.Strings(labels)
	return b.String(), labels
}

// Blocked message for the first pattern of the sets that matches, empty when none does
func firstPatternError(text string, sets ...[]cronThreatPattern) string {
	for _, patterns := range sets {
		for _, p := range patterns {
			if ok, err := p.re.MatchString(text); err != nil {
				panic(err)
			} else if ok {
				return fmt.Sprintf("Blocked: prompt matches threat pattern '%s'. Cron prompts must not contain injection or exfiltration payloads.", p.id)
			}
		}
	}
	return ""
}

// Strict scan of a user supplied prompt returning the block message, invisible unicode reported first
func scanCronPrompt(prompt string) string {
	toScan := stripCronSafeConstructs(prompt)
	_, removed := stripInvisibleUnicode(toScan)
	for _, ch := range cronInvisibleChars {
		label := fmt.Sprintf("U+%04X", ch)
		for _, r := range removed {
			if r == label {
				return fmt.Sprintf("Blocked: prompt contains invisible unicode %s (possible injection).", label)
			}
		}
	}
	return firstPatternError(toScan, cronThreatPatterns, cronExfilCommandPatterns)
}

// Loose scan of an assembled prompt that sanitizes invisible unicode and checks only injection phrasing
func scanCronSkillAssembled(assembled string) (string, string) {
	cleaned, removed := stripInvisibleUnicode(assembled)
	if len(removed) > 0 {
		slog.Default().With("logger", "tools.cronjob_tools").Warn(fmt.Sprintf("Cron skill-assembled prompt: stripped %d invisible-unicode char(s) (%s) from vetted skill content", len(removed), strings.Join(removed, ", ")))
	}
	return cleaned, firstPatternError(stripCronSafeConstructs(cleaned), cronSkillAssembledPatterns)
}
