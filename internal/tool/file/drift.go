package file

import (
	"strings"
)

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

func halveBackslashRuns(s string) string {
	var b strings.Builder
	b.Grow(len(s))
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

func correctBackslashDoubling(oldString, newString, content string) (string, string, error) {
	if !strings.Contains(oldString, `\`) || strings.Contains(content, oldString) {
		return oldString, newString, nil
	}
	if !allBackslashRunsEven(oldString) {
		return oldString, newString, nil
	}
	halved := halveBackslashRuns(oldString)
	if !strings.Contains(content, halved) {
		return oldString, newString, nil
	}
	if strings.Contains(newString, `\`) {
		return oldString, newString, &MatchError{Msg: "Escape-drift detected: halving old_string's backslashes matches the file, " +
			"but correcting new_string could remove intentional backslashes. " +
			"Re-read the file and resend old_string/new_string with their intended escaping."}
	}
	return halved, newString, nil
}

func stripSpuriousQuoteEscapes(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, `\'`, "'"), `\"`, `"`)
}

func quoteEscapesAllSpurious(texts ...string) bool {
	for _, q := range [...]byte{'\'', '"'} {
		escaped := `\` + string(q)
		present := false
		for _, s := range texts {
			if strings.Contains(s, escaped) {
				present = true
				break
			}
		}
		if !present {
			continue
		}
		for _, s := range texts {
			for i := 0; i < len(s); i++ {
				if s[i] != q {
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

func correctQuoteEscaping(oldString, newString, content string) (string, string, error) {
	suspect := (strings.Contains(oldString, `\'`) && strings.Contains(newString, `\'`)) ||
		(strings.Contains(oldString, `\"`) && strings.Contains(newString, `\"`))
	if !suspect || strings.Contains(content, oldString) {
		return oldString, newString, nil
	}
	if !quoteEscapesAllSpurious(oldString, newString) {
		return oldString, newString, nil
	}
	stripped := stripSpuriousQuoteEscapes(oldString)
	if !strings.Contains(content, stripped) {
		return oldString, newString, nil
	}
	var unproven []string
	for _, q := range [...]string{"'", `"`} {
		if strings.Contains(newString, `\`+q) && !strings.Contains(oldString, `\`+q) {
			unproven = append(unproven, `\`+q)
		}
	}
	if len(unproven) > 0 {
		return oldString, newString, &MatchError{Msg: "Escape-drift detected: removing the spurious quote escapes in old_string matches " +
			"the file, but new_string also contains " + strings.Join(unproven, " and ") + ", which old_string gives no evidence " +
			"for. Re-read the file and resend old_string/new_string with their intended escaping."}
	}
	return stripped, stripSpuriousQuoteEscapes(newString), nil
}

func correctEscapeDrift(oldString, newString, content string) (string, string, string, error) {
	correctedOld, correctedNew, err := correctQuoteEscaping(oldString, newString, content)
	if err != nil {
		return oldString, newString, "", err
	}
	if correctedOld != oldString {
		return correctedOld, correctedNew, "escape-drift auto-corrected: stripped a spurious backslash before a quote/apostrophe " +
			"in old_string/new_string (a JSON-double-escaping wire artifact) before matching.", nil
	}
	correctedOld, correctedNew, err = correctBackslashDoubling(oldString, newString, content)
	if err != nil {
		return oldString, newString, "", err
	}
	if correctedOld != oldString {
		return correctedOld, correctedNew, "escape-drift auto-corrected: halved doubled backslash runs in old_string " +
			"before matching; new_string has no backslashes and was left unchanged.", nil
	}
	return oldString, newString, "", nil
}

func detectNewlineLiteralDrift(content string, matches []span, oldString, newString string) string {
	if !strings.Contains(newString, `\n`) {
		return ""
	}
	oldLiterals := strings.Count(oldString, `\n`)
	for _, m := range matches {
		region := content[m.start:m.end]
		if strings.Contains(region, "\n") && oldLiterals > strings.Count(region, `\n`) {
			return "Escape-drift detected: old_string contains more literal " +
				"'\\\\n' sequences than the matched region of the file, which " +
				"has real line breaks there instead. This is almost always a " +
				"tool-call serialization artifact where a line break got escaped " +
				"one extra time; new_string would write it as backslash + n. " +
				"Re-read the file with read_file and pass old_string/new_string " +
				"with actual line breaks, keeping only the '\\\\n' sequences " +
				"that appear literally in the file."
		}
	}
	return ""
}

func detectBackslashDoubling(regions, oldString, newString string) string {
	oldRuns := backslashRuns(oldString)
	fileRuns := backslashRuns(regions)
	if len(oldRuns) == 0 || len(fileRuns) == 0 || len(oldRuns) != len(fileRuns) {
		return ""
	}
	same, doubled, strong := true, true, len(fileRuns) >= 2
	for i, f := range fileRuns {
		if oldRuns[i] != f {
			same = false
		}
		if oldRuns[i] != f*2 {
			doubled = false
		}
		if f >= 2 {
			strong = true
		}
	}
	if same || !doubled || !strong {
		return ""
	}
	newRuns := backslashRuns(newString)
	if len(newRuns) == len(fileRuns) {
		equal := true
		for i, f := range fileRuns {
			if newRuns[i] != f {
				equal = false
				break
			}
		}
		if equal {
			return ""
		}
	}
	return "Escape-drift detected: every backslash run in old_string is exactly " +
		"twice as long as in the matched region of the file (e.g. the file " +
		"has `\\\\` where old_string has `\\\\\\\\`). The tool-call arguments " +
		"were JSON-escaped one extra time; applying new_string verbatim would " +
		"double every backslash in the file. Re-read the file with read_file " +
		"and resend old_string/new_string with the backslash counts exactly " +
		"as they appear in the file."
}

func detectEscapeDrift(content string, matches []span, oldString, newString string) string {
	quoteSuspects := strings.Contains(newString, `\'`) || strings.Contains(newString, `\"`)
	if !quoteSuspects && !strings.Contains(oldString, `\`) {
		return ""
	}
	regions := matchedRegions(content, matches)
	if quoteSuspects {
		for _, suspect := range [...]string{`\'`, `\"`} {
			if strings.Contains(newString, suspect) && strings.Contains(oldString, suspect) && !strings.Contains(regions, suspect) {
				return "Escape-drift detected: old_string and new_string contain " +
					"the literal sequence " + quote(suspect) + " but the matched region of " +
					"the file does not. This is almost always a tool-call " +
					"serialization artifact where an apostrophe or quote got " +
					"prefixed with a spurious backslash. Re-read the file with " +
					"read_file and pass old_string/new_string without " +
					"backslash-escaping " + quote(suspect[1:]) + " characters."
			}
		}
	}
	if drift := detectNewlineLiteralDrift(content, matches, oldString, newString); drift != "" {
		return drift
	}
	return detectBackslashDoubling(regions, oldString, newString)
}

func unescapeNewString(newString, content string, matches []span) string {
	if !strings.Contains(newString, `\t`) && !strings.Contains(newString, `\r`) {
		return newString
	}
	regions := matchedRegions(content, matches)
	for _, pair := range [...][2]string{{`\t`, "\t"}, {`\r`, "\r"}} {
		if strings.Contains(newString, pair[0]) && strings.Contains(regions, pair[1]) {
			newString = strings.ReplaceAll(newString, pair[0], pair[1])
		}
	}
	return newString
}
