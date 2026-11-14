package config

import (
	"context"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/fileutil"
)

// Value without its trailing comment, found after a closing quote or after whitespace in a bare value
func stripEnvComment(value string) string {
	value = trimEnvSpace(value)
	if value == "" {
		return value
	}
	if quote := value[0]; quote == '\'' || quote == '"' {
		for i := 1; i < len(value); {
			r, size := utf8.DecodeRuneInString(value[i:])
			switch {
			case quote == '"' && r == '\\':
				i += size
				if i < len(value) {
					_, next := utf8.DecodeRuneInString(value[i:])
					i += next
				}
			case r == rune(quote):
				if strings.HasPrefix(strings.TrimLeftFunc(value[i+1:], isEnvSpace), "#") {
					return value[:i+1]
				}
				return value
			default:
				i += size
			}
		}
		return value
	}
	for i := 0; i < len(value); {
		r, size := utf8.DecodeRuneInString(value[i:])
		if !isEnvSpace(r) {
			i += size
			continue
		}
		end := i + len(value[i:]) - len(strings.TrimLeftFunc(value[i:], isEnvSpace))
		if strings.HasPrefix(value[end:], "#") {
			return trimEnvSpace(value[:i])
		}
		i = end
	}
	return value
}

// Value without its surrounding quotes, a double quoted one with escaped quotes and backslashes restored
func unquoteEnvValue(raw string) string {
	value := trimEnvSpace(raw)
	if len(value) < 2 || value[0] != value[len(value)-1] {
		return value
	}
	switch value[0] {
	case '"':
		body := value[1 : len(value)-1]
		var b strings.Builder
		for i := 0; i < len(body); i++ {
			if body[i] == '\\' && i+1 < len(body) && (body[i+1] == '"' || body[i+1] == '\\') {
				i++
			}
			b.WriteByte(body[i])
		}
		return b.String()
	case '\'':
		return value[1 : len(value)-1]
	}
	return value
}

// Assignments of env text as readers of a home see them, names in first seen order, last value winning
func tokenizeEnv(text string) ([]string, map[string]string) {
	var names []string
	values := map[string]string{}
	for _, row := range tokenizerRows(text) {
		line := trimEnvSpace(row)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "export "); ok {
			line = strings.TrimLeftFunc(rest, isEnvSpace)
		}
		key, value, found := strings.Cut(line, "=")
		key = trimEnvSpace(key)
		if !found || key == "" {
			continue
		}
		if _, seen := values[key]; !seen {
			names = append(names, key)
		}
		values[key] = unquoteEnvValue(stripEnvComment(value))
	}
	return names, values
}

// Env file text for the tokenizer, one mark dropped and bytes that are not UTF-8 read one per character
func tokenizerText(raw []byte) string { return envLoaderText(raw) }

// Lines normalized for a rewrite, each ending in one line feed, assignments trimmed and comments and blanks kept
func sanitizeEnvLines(lines []string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		raw := strings.TrimRight(line, "\r\n")
		if trimmed := trimEnvSpace(raw); trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			raw = trimmed
		}
		out[i] = raw + "\n"
	}
	return out
}

// Normalized lines of the env file at path, read as UTF-8 with broken bytes replaced
func readEnvLines(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return sanitizeEnvLines(textLines(utf8ReplacedText(raw))), nil
}

// Value as an env line carries it, in double quotes when it holds a hash, a quote or whitespace
func QuoteEnvValue(value string) string {
	if !strings.ContainsAny(value, "#\"'") && !strings.ContainsFunc(value, isEnvSpace) {
		return value
	}
	return `"` + strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), `"`, `\"`) + `"`
}

// Reports an env line that assigns key, with or without export and with any spacing around the name
func EnvLineDefines(line, key string) bool {
	trimmed := trimEnvSpace(line)
	if rest, ok := strings.CutPrefix(trimmed, "export "); ok {
		trimmed = strings.TrimLeftFunc(rest, isEnvSpace)
	}
	name, _, found := strings.Cut(trimmed, "=")
	return found && envPolicyName(trimEnvSpace(name)) == envPolicyName(key)
}

// Atomically replaces the env file with lines in platform line breaks, its mode kept when asked, else owner only
func writeEnvLines(ctx context.Context, path string, lines []string, preserveMode bool) error {
	text := strings.ReplaceAll(strings.Join(lines, ""), "\n", nativeNewline)
	mode := fileutil.Perm(0o600)
	if _, err := os.Stat(path); preserveMode && err == nil {
		mode = fileutil.KeepPerm()
	}
	return fileutil.WriteFile(ctx, path, []byte(text), mode, fileutil.KeepOwner(),
		fileutil.TempPrefix(".env_"), fileutil.TempSuffix(".tmp"))
}
