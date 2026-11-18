package commands

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pydifflib"
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

// Python exception a slash command raises, its message the str the slash worker answers
type RaisedError struct{ Msg string }

func (e *RaisedError) Error() string { return e.Msg }

// Membership and lookup of bare in the raw quick_commands value with the TypeErrors in and indexing raise
func QuickCommandEntry(cfg config.Tree, bare string) (any, bool, error) {
	raw, ok := cfg.Lookup("quick_commands")
	if !ok {
		return nil, false, nil
	}
	if m := config.AsMap(raw); m != nil {
		entry, found := m[bare]
		return entry, found, nil
	}
	if s, ok := raw.(string); ok {
		if strings.Contains(s, bare) {
			return nil, false, &RaisedError{"string indices must be integers, not 'str'"}
		}
		return nil, false, nil
	}
	if rv := reflect.ValueOf(raw); rv.Kind() == reflect.Slice {
		for i := 0; i < rv.Len(); i++ {
			if s, ok := rv.Index(i).Interface().(string); ok && s == bare {
				return nil, false, &RaisedError{"list indices must be integers or slices, not str"}
			}
		}
		return nil, false, nil
	}
	return nil, false, &RaisedError{fmt.Sprintf("argument of type '%s' is not a container or iterable", config.PyTypeName(raw))}
}

// UnknownCommandLines returns the lead and pointer lines for a slash token with no handler
func UnknownCommandLines(typed string, known []string) (string, string) {
	base := typed
	if fields := strings.Fields(typed); len(fields) > 0 {
		base = fields[0]
	}
	if refusal, ok := SlashTierRefusal(base); ok {
		return refusal + " — nothing was sent.", "Type /help for the full list."
	}
	hint := ""
	if close := pydifflib.GetCloseMatches(base, known, 1, 0.6); len(close) > 0 {
		hint = " Did you mean " + close[0] + "?"
	}
	return "Unknown command " + base + " — nothing was sent." + hint, "Type /help for the full list."
}

// DestructiveSkipTokens bypass the destructive slash confirmation inline
var DestructiveSkipTokens = map[string]bool{"now": true, "--yes": true, "-y": true}
