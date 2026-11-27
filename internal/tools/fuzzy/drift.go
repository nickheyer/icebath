package fuzzy

import (
	"errors"
	"strings"
)

// Lengths of each run of backslashes in order
func runs(s string) []int {
	var out []int
	for i := 0; i < len(s); {
		if s[i] != '\\' {
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] == '\\' {
			j++
		}
		out = append(out, j-i)
		i = j
	}
	return out
}

// Every run of backslashes cut to half its length
func halve(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] == '\\' {
			j++
		}
		b.WriteString(strings.Repeat(`\`, (j-i)/2))
		i = j
	}
	return b.String()
}

// Doubled backslashes of old_string halved when the halved text is in the file and new_string has no backslash to get wrong
func correctDoubling(old, new, content string) (string, string, error) {
	if !strings.Contains(old, `\`) || strings.Contains(content, old) {
		return old, new, nil
	}
	for _, n := range runs(old) {
		if n%2 != 0 {
			return old, new, nil
		}
	}
	halved := halve(old)
	if !strings.Contains(content, halved) {
		return old, new, nil
	}
	if strings.Contains(new, `\`) {
		return old, new, errors.New("Escape-drift detected: halving old_string's backslashes matches the file, but correcting new_string could " +
			"remove intentional backslashes. Re-read the file and resend old_string/new_string with their intended escaping.")
	}
	return halved, new, nil
}

var unescapeQuotes = strings.NewReplacer(`\'`, `'`, `\"`, `"`)

// Whether every quote of each escaped kind has exactly one backslash before it, in every text
func spuriousQuotes(texts ...string) bool {
	for _, quote := range []byte{'\'', '"'} {
		escaped := `\` + string(quote)
		if !strings.Contains(strings.Join(texts, "\x00"), escaped) {
			continue
		}
		for _, s := range texts {
			for i := 0; i < len(s); i++ {
				if s[i] != quote {
					continue
				}
				backslashes := 0
				for k := i - 1; k >= 0 && s[k] == '\\'; k-- {
					backslashes++
				}
				if backslashes != 1 {
					return false
				}
			}
		}
	}
	return true
}

// Spurious quote escapes stripped from both texts when the stripped old_string is in the file
func correctQuotes(old, new, content string) (string, string, error) {
	suspect := strings.Contains(old, `\'`) && strings.Contains(new, `\'`) || strings.Contains(old, `\"`) && strings.Contains(new, `\"`)
	if !suspect || strings.Contains(content, old) || !spuriousQuotes(old, new) {
		return old, new, nil
	}
	stripped := unescapeQuotes.Replace(old)
	if !strings.Contains(content, stripped) {
		return old, new, nil
	}
	var unproven []string
	for _, q := range []string{`\'`, `\"`} {
		if strings.Contains(new, q) && !strings.Contains(old, q) {
			unproven = append(unproven, q)
		}
	}
	if len(unproven) > 0 {
		return old, new, errors.New("Escape-drift detected: removing the spurious quote escapes in old_string matches the file, but new_string also " +
			"contains " + strings.Join(unproven, " and ") + ", which old_string gives no evidence for. Re-read the file and resend " +
			"old_string/new_string with their intended escaping.")
	}
	return stripped, unescapeQuotes.Replace(new), nil
}

// Texts with a verified quote or backslash escaping artifact corrected, and the note saying what was corrected
func correctDrift(old, new, content string) (string, string, string, error) {
	fixedOld, fixedNew, err := correctQuotes(old, new, content)
	if err != nil {
		return old, new, "", err
	}
	if fixedOld != old {
		return fixedOld, fixedNew, "escape-drift auto-corrected: stripped a spurious backslash before a quote/apostrophe in old_string/new_string " +
			"(a JSON-double-escaping wire artifact) before matching.", nil
	}
	fixedOld, fixedNew, err = correctDoubling(old, new, content)
	if err != nil {
		return old, new, "", err
	}
	if fixedOld != old {
		return fixedOld, fixedNew, "escape-drift auto-corrected: halved doubled backslash runs in old_string before matching; new_string has no " +
			"backslashes and was left unchanged.", nil
	}
	return old, new, "", nil
}
