package commands

import (
	"strings"
)

// ShlexArgs splits the tokens after the command word, whitespace splitting on unbalanced quotes
func ShlexArgs(cmd string) []string {
	if strings.TrimSpace(cmd) == "" {
		return nil
	}
	tokens, err := Shlex(cmd)
	if err != nil {
		tokens = strings.Fields(cmd)
	}
	if len(tokens) == 0 {
		return nil
	}
	return tokens[1:]
}

// ShlexError reports unbalanced quoting
type ShlexError struct{ Message string }

func (e *ShlexError) Error() string { return e.Message }

// Shlex splits a line with POSIX shell quoting rules
func Shlex(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inWord := false
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case c == '\\':
			if i+1 >= len(runes) {
				return nil, &ShlexError{"No escaped character"}
			}
			i++
			cur.WriteRune(runes[i])
			inWord = true
		case c == '\'':
			inWord = true
			j := i + 1
			for j < len(runes) && runes[j] != '\'' {
				j++
			}
			if j >= len(runes) {
				return nil, &ShlexError{"No closing quotation"}
			}
			cur.WriteString(string(runes[i+1 : j]))
			i = j
		case c == '"':
			inWord = true
			j := i + 1
			for j < len(runes) && runes[j] != '"' {
				if runes[j] == '\\' && j+1 < len(runes) && strings.ContainsRune("\"\\$`\n", runes[j+1]) {
					j++
				}
				cur.WriteRune(runes[j])
				j++
			}
			if j >= len(runes) {
				return nil, &ShlexError{"No closing quotation"}
			}
			i = j
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(c)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out, nil
}

// SkillInfo describes one installed skill command
type SkillInfo struct {
	Name            string
	Description     string
	SkillDir        string
	SkillMDPath     string
	SkillIdentifier string
}

// BundleInfo describes one skill bundle
type BundleInfo struct {
	Name        string
	Slug        string
	Description string
	Skills      []string
}

// MaxStackedSkills caps the leading skill tokens one line may stack
const MaxStackedSkills = 5

// NormalizeSkillToken returns the canonical hyphenated slash key of a typed token
func NormalizeSkillToken(token string) string {
	return "/" + strings.ToLower(strings.ReplaceAll(strings.TrimLeft(token, "/"), "_", "-"))
}

// ResolveSlashKey maps a typed command to its /slug table key, underscores reading as hyphens
func ResolveSlashKey[T any](command string, table map[string]T) (string, bool) {
	if command == "" || table == nil {
		return "", false
	}
	exact := "/" + strings.ToLower(command)
	if _, ok := table[exact]; ok {
		return exact, true
	}
	key := "/" + strings.ToLower(strings.ReplaceAll(command, "_", "-"))
	if _, ok := table[key]; ok {
		return key, true
	}
	return "", false
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// QuickCommand is one user defined slash shortcut from config
type QuickCommand struct {
	Type        string
	Target      string
	Description string
}

// DestructiveSkipTokens bypass the destructive slash confirmation inline
var DestructiveSkipTokens = map[string]bool{"now": true, "--yes": true, "-y": true}
