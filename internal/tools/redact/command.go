package redact

import (
	"regexp"
	"strings"
)

var (
	// Commands that print the environment, whose output is all assignments
	envDumps = map[string]bool{"env": true, "printenv": true, "set": true, "export": true, "declare": true}
	// Commands that print the files they are given, and those of them whose first argument is a pattern
	fileReaders = map[string]bool{"cat": true, "head": true, "tail": true, "type": true, "bat": true, "less": true, "more": true, "nl": true,
		"zcat": true, "tac": true, "view": true, "batcat": true, "grep": true, "awk": true, "sed": true}
	patternFirst = map[string]bool{"grep": true, "awk": true, "sed": true}
	// Lines of a pytest failure and of a Python exception, whose repr of a mapping may hold credentials
	pytestDiagnostic = regexp.MustCompile(`^([ \t]*E[ \t]{2,})(.*)$`)
	pythonException  = regexp.MustCompile(`^((?:[A-Za-z_]\w*\.)*[A-Za-z_]\w*(?:Error|Exception|Warning):[ \t]*)(.*)$`)
)

// Output of a shell command with its secrets masked by their ends: the values of secret named assignments too when the command dumps the
// environment or prints a file that holds secrets, as secretFile judges each path it is given, else the credential fields of the Python
// diagnostics it printed
func Command(output, command string, secretFile func(path string) bool) string {
	if output == "" {
		return output
	}
	secret := EnvDump(command) || readsSecret(command, secretFile)
	output = Text(output, secret, Ends)
	if !secret && strings.Contains(output, ":") && strings.Contains(output, "'") {
		output = diagnostics(output)
	}
	return output
}

// Pipeline and sequence segments of a command, split on the pipes, semicolons and ampersands outside quotes
func segments(command string) []string {
	var out []string
	var buf strings.Builder
	var quote rune
	flush := func() {
		if segment := strings.TrimSpace(buf.String()); segment != "" {
			out = append(out, segment)
		}
		buf.Reset()
	}
	for _, r := range command {
		switch {
		case quote != 0:
			buf.WriteRune(r)
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
			buf.WriteRune(r)
		case r == '|' || r == ';' || r == '&':
			flush()
		default:
			buf.WriteRune(r)
		}
	}
	flush()
	return out
}

// Whether any segment of the command starts with a command that prints the environment
func EnvDump(command string) bool {
	for _, segment := range segments(command) {
		if envDumps[firstWord(segment)] {
			return true
		}
	}
	return false
}

// First word of a shell segment with its quotes and escapes taken out, the first run of non blanks when its quotes don't close
func firstWord(segment string) string {
	segment = strings.TrimLeft(segment, " \t\n")
	var word strings.Builder
	var quote rune
	escaped := false
	for _, r := range segment {
		switch {
		case escaped:
			word.WriteRune(r)
			escaped = false
		case quote == '\'':
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
		case r == '\\':
			escaped = true
		case quote == '"':
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
		case r == ' ' || r == '\t' || r == '\n':
			return word.String()
		default:
			word.WriteRune(r)
		}
	}
	if quote != 0 || escaped {
		if fields := strings.Fields(segment); len(fields) > 0 {
			return fields[0]
		}
		return ""
	}
	return word.String()
}

// Whether a segment of the command prints a file that holds secrets
func readsSecret(command string, secretFile func(path string) bool) bool {
	if secretFile == nil {
		return false
	}
	for _, segment := range segments(command) {
		words := strings.Fields(segment)
		if len(words) == 0 {
			continue
		}
		reader := strings.ToLower(words[0][strings.LastIndex(words[0], "/")+1:])
		if !fileReaders[reader] {
			continue
		}
		var paths []string
		for _, word := range words[1:] {
			if !strings.HasPrefix(word, "-") {
				paths = append(paths, word)
			}
		}
		if patternFirst[reader] && len(paths) > 0 {
			paths = paths[1:]
		}
		for _, path := range paths {
			if secretFile(path) {
				return true
			}
		}
	}
	return false
}

// Credential fields of the mapping reprs on pytest failure and Python exception lines masked, every other line kept as it is
func diagnostics(text string) string {
	var out strings.Builder
	for len(text) > 0 {
		end := strings.IndexAny(text, "\r\n")
		body, ending := text, ""
		if end >= 0 {
			body, ending = text[:end], text[end:end+1]
			if strings.HasPrefix(text[end:], "\r\n") {
				ending = "\r\n"
			}
		}
		text = text[len(body)+len(ending):]
		for _, re := range []*regexp.Regexp{pytestDiagnostic, pythonException} {
			if m := re.FindStringSubmatch(body); m != nil {
				body = m[1] + reprFields(m[2])
				break
			}
		}
		out.WriteString(body + ending)
	}
	return out.String()
}
