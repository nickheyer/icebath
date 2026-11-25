package cron

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	regexutil "github.com/nickheyer/icebath/pkg/utils/regex"
)

type cronThreatPattern struct {
	re *regexp.Regexp
	id string
}

const cronSecretVarRE = `\$\{?\w*(?:KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL|API)\w*\}?`

// Class body of every Unicode space: the model reading a prompt takes NBSP or U+3000 between two words or arguments as a space
const cronSpaceChars = `\s\v\x{85}\p{Z}`

// One separator between words or arguments, as cronSpaceChars reads it
const cronSpace = `[` + cronSpaceChars + `]`

// One character of a filler word between the injection keywords: letters, marks, numbers and connectors of any script
const cronWordChar = `[\p{L}\p{M}\p{N}\p{Pc}\x{200C}\x{200D}]`

// Declaration order of the invisible code points, the order a strict scan reports them in
const cronInvisibleChars = "\u200b\u200c\u200d\u2060\u2062\u2063\u2064\ufeff\u202a\u202b\u202c\u202d\u202e\u2066\u2067\u2068\u2069"

var (
	cronThreatPatterns = sync.OnceValue(func() []cronThreatPattern {
		return compileCronPatterns([][2]string{
			{`ignore` + cronSpace + `+(?:` + cronWordChar + `+` + cronSpace + `+)*(?:previous|all|above|prior)` + cronSpace + `+(?:` + cronWordChar + `+` + cronSpace + `+)*instructions`, "prompt_injection"},
			{`do` + cronSpace + `+not` + cronSpace + `+tell` + cronSpace + `+the` + cronSpace + `+user`, "deception_hide"},
			{`system` + cronSpace + `+prompt` + cronSpace + `+override`, "sys_prompt_override"},
			{`disregard` + cronSpace + `+(your|all|any)` + cronSpace + `+(instructions|rules|guidelines)`, "disregard_rules"},
			{`cat` + cronSpace + `+[^\n]*(\.env|credentials|\.netrc|\.pgpass|id_rsa|id_ed25519|id_ecdsa)`, "read_secrets"},
			{`authorized_keys`, "ssh_backdoor"}, {`/etc/sudoers|visudo`, "sudoers_mod"},
			{`rm` + cronSpace + `+-rf` + cronSpace + `+/`, "destructive_root_rm"},
		})
	})
	cronExfilCommandPatterns = sync.OnceValue(func() []cronThreatPattern {
		return compileCronPatterns([][2]string{
			{`curl` + cronSpace + `+[^\n]*https?://[^` + cronSpaceChars + `"\'` + "`" + `]*` + cronSecretVarRE, "exfil_curl_url"},
			{`wget` + cronSpace + `+[^\n]*https?://[^` + cronSpaceChars + `"\'` + "`" + `]*` + cronSecretVarRE, "exfil_wget_url"},
			{`curl` + cronSpace + `+[^\n]*(?:--data(?:-raw|-binary|-urlencode)?|-d|--form|-F)` + cronSpace + `+[^\n]*` + cronSecretVarRE, "exfil_curl_data"},
			{`wget` + cronSpace + `+[^\n]*--post-(?:data|file)=[^\n]*` + cronSecretVarRE, "exfil_wget_post"},
			{`curl` + cronSpace + `+[^\n]*(?:-H|--header)` + cronSpace + `+["\']Authorization:` + cronSpace + `*(?:Bearer|token)` + cronSpace + `+` + cronSecretVarRE + `["\']`, "exfil_curl_auth_header"},
		})
	})
	cronSafeGitHubCurl = regexutil.Lazy(regexp.MustCompile, `(?i)curl`+cronSpace+`+[^\n;&|$`+"`"+`]*(?:-H|--header)`+cronSpace+`+["\']Authorization:`+cronSpace+`*token`+cronSpace+`+`+cronSecretVarRE+`["\']`+
		cronSpace+`+["\']?https://api\.github\.com(?::\d+)?(?:/|`+cronSpace+`|$|["\'])[^`+cronSpaceChars+`;&|$`+"`"+`]*`)
	emojiNeighbourRanges = [][2]rune{{0x1F000, 0x1FFFF}, {0x2600, 0x27BF}, {0x2300, 0x23FF}, {0x1F1E6, 0x1F1FF}, {0x20E3, 0x20E3}}
)

const variationSelectorCP = 0xFE0F

// Case insensitive compiled threat patterns paired with their ids
func compileCronPatterns(src [][2]string) []cronThreatPattern {
	out := make([]cronThreatPattern, 0, len(src))
	for _, p := range src {
		out = append(out, cronThreatPattern{re: regexp.MustCompile("(?i)" + p[0]), id: p[1]})
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
	return cronSafeGitHubCurl().ReplaceAllLiteralString(prompt, "curl https://api.github.com/user")
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
			if p.re.MatchString(text) {
				return fmt.Sprintf("Blocked: prompt matches threat pattern %q. Cron prompts must not contain injection or exfiltration payloads.", p.id)
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
	return firstPatternError(toScan, cronThreatPatterns(), cronExfilCommandPatterns())
}

// Loose scan of an assembled prompt that sanitizes invisible unicode and checks only injection phrasing
func scanCronSkillAssembled(assembled string) (string, string) {
	cleaned, removed := stripInvisibleUnicode(assembled)
	if len(removed) > 0 {
		pkgLog().Warn(fmt.Sprintf("Cron skill-assembled prompt: stripped %d invisible-unicode char(s) (%s) from vetted skill content", len(removed), strings.Join(removed, ", ")))
	}
	return cleaned, firstPatternError(stripCronSafeConstructs(cleaned), cronThreatPatterns()[:4])
}
