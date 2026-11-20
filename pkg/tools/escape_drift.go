package tools

import "strings"

// Refusal raised when replacement backslashes cannot be told apart from serialization drift
type ambiguousEscapeDriftError struct{ msg string }

func (e *ambiguousEscapeDriftError) Error() string { return e.msg }

// Lengths of each maximal backslash run in order
func backslashRuns(s string) []int {
	var runs []int
	for i := 0; i < len(s); {
		if s[i] != '\\' {
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] == '\\' {
			j++
		}
		runs = append(runs, j-i)
		i = j
	}
	return runs
}

// Replaces every maximal backslash run with half its length
func halveBackslashRuns(s string) string {
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

func allBackslashRunsEven(s string) bool {
	for _, n := range backslashRuns(s) {
		if n%2 != 0 {
			return false
		}
	}
	return true
}

// Halves a doubled anchor verified in the file only when the replacement has no backslashes
func maybeCorrectBackslashDoubling(oldString, newString, content string) (string, string, error) {
	if !strings.Contains(oldString, `\`) || strings.Contains(content, oldString) {
		return oldString, newString, nil
	}
	if !allBackslashRunsEven(oldString) {
		return oldString, newString, nil
	}
	halvedOld := halveBackslashRuns(oldString)
	if !strings.Contains(content, halvedOld) {
		return oldString, newString, nil
	}
	if strings.Contains(newString, `\`) {
		return oldString, newString, &ambiguousEscapeDriftError{"Escape-drift detected: halving old_string's backslashes matches the file, " +
			"but correcting new_string could remove intentional backslashes. " +
			"Re-read the file and resend old_string/new_string with their intended escaping."}
	}
	return halvedOld, newString, nil
}

// Turns each backslash quote and backslash apostrophe into the bare character
func stripSpuriousQuoteEscapes(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, `\'`, `'`), `\"`, `"`)
}

// Reports whether every quote of each escaped kind carries exactly one preceding backslash
func quoteEscapesAllSpurious(strs ...string) bool {
	for _, quote := range []byte{'\'', '"'} {
		escaped := `\` + string(quote)
		touched := false
		for _, s := range strs {
			if strings.Contains(s, escaped) {
				touched = true
				break
			}
		}
		if !touched {
			continue
		}
		for _, s := range strs {
			for i := 0; i < len(s); i++ {
				if s[i] != quote {
					continue
				}
				run := 0
				for k := i - 1; k >= 0 && s[k] == '\\'; k-- {
					run++
				}
				if run != 1 {
					return false
				}
			}
		}
	}
	return true
}

// Strips spurious quote escapes when the stripped anchor is verified in the file
func maybeCorrectQuoteEscaping(oldString, newString, content string) (string, string, error) {
	hasQuoteSuspect := (strings.Contains(oldString, `\'`) && strings.Contains(newString, `\'`)) ||
		(strings.Contains(oldString, `\"`) && strings.Contains(newString, `\"`))
	if !hasQuoteSuspect || strings.Contains(content, oldString) {
		return oldString, newString, nil
	}
	if !quoteEscapesAllSpurious(oldString, newString) {
		return oldString, newString, nil
	}
	strippedOld := stripSpuriousQuoteEscapes(oldString)
	if !strings.Contains(content, strippedOld) {
		return oldString, newString, nil
	}
	var unproven []string
	for _, q := range []string{`'`, `"`} {
		if strings.Contains(newString, `\`+q) && !strings.Contains(oldString, `\`+q) {
			unproven = append(unproven, `\`+q)
		}
	}
	if len(unproven) > 0 {
		return oldString, newString, &ambiguousEscapeDriftError{"Escape-drift detected: removing the spurious quote escapes in old_string matches " +
			"the file, but new_string also contains " + strings.Join(unproven, " and ") + ", which old_string gives no evidence " +
			"for. Re-read the file and resend old_string/new_string with their intended escaping."}
	}
	return strippedOld, stripSpuriousQuoteEscapes(newString), nil
}

// Applies the verified quote or backslash correction and returns the note describing it
func maybeCorrectEscapeDrift(oldString, newString, content string) (string, string, string, error) {
	correctedOld, correctedNew, err := maybeCorrectQuoteEscaping(oldString, newString, content)
	if err != nil {
		return oldString, newString, "", err
	}
	if correctedOld != oldString {
		return correctedOld, correctedNew, "escape-drift auto-corrected: stripped a spurious backslash before a quote/apostrophe " +
			"in old_string/new_string (a JSON-double-escaping wire artifact) before matching.", nil
	}
	correctedOld, correctedNew, err = maybeCorrectBackslashDoubling(oldString, newString, content)
	if err != nil {
		return oldString, newString, "", err
	}
	if correctedOld != oldString {
		return correctedOld, correctedNew, "escape-drift auto-corrected: halved doubled backslash runs in old_string " +
			"before matching; new_string has no backslashes and was left unchanged.", nil
	}
	return oldString, newString, "", nil
}
