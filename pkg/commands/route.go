package commands

import (
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pydifflib"
)

// CommandArg returns everything after the slash command word, optionally lowercased
func CommandArg(cmd string, lower bool) string {
	arg := SlashArgs(cmd)
	if lower {
		return strings.ToLower(arg)
	}
	return arg
}

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

var skillInvalidChars = regexp.MustCompile(`[^\w-]`)
var skillMultiHyphen = regexp.MustCompile(`-{2,}`)

// SlugifySkillName normalizes a skill or bundle name to its slash slug
func SlugifySkillName(name string) string {
	cmd := strings.ToLower(name)
	cmd = strings.ReplaceAll(cmd, " ", "-")
	cmd = strings.ReplaceAll(cmd, "_", "-")
	cmd = skillInvalidChars.ReplaceAllString(cmd, "")
	return strings.Trim(skillMultiHyphen.ReplaceAllString(cmd, "-"), "-")
}

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

// SkillCommandCollisionNote explains a skill whose slug a builtin command already owns
func SkillCommandCollisionNote(r *Registry, name string) string {
	slug := SlugifySkillName(name)
	if slug == "" || r.Lookup(slug) == nil {
		return ""
	}
	return "slash command /" + slug + " unavailable — name taken by built-in; use /skill " + name
}

// QuickCommand is one user defined slash shortcut from config
type QuickCommand struct {
	Type        string
	Target      string
	Description string
}

// QuickCommands reads the quick_commands mapping from config, empty when unset or malformed
func QuickCommands(cfg config.Tree) map[string]QuickCommand {
	out := map[string]QuickCommand{}
	for name, raw := range cfg.Map("quick_commands") {
		entry := config.AsMap(raw)
		if entry == nil {
			continue
		}
		var q QuickCommand
		q.Type, _ = entry["type"].(string)
		q.Target, _ = entry["target"].(string)
		q.Description, _ = entry["description"].(string)
		out[name] = q
	}
	return out
}

// AliasChainError reports a quick command alias chain that cannot resolve
type AliasChainError struct{ Code string }

func (e *AliasChainError) Error() string { return "quick command alias: " + e.Code }

// ResolveAliasChain follows alias quick commands until a builtin or exec quick command
func ResolveAliasChain(r *Registry, quick map[string]QuickCommand, name, arg string) (string, string, error) {
	seen := map[string]bool{}
	for r.Lookup(name) == nil {
		entry, ok := quick[name]
		if !ok {
			break
		}
		if seen[name] {
			return "", "", &AliasChainError{"invalid_params"}
		}
		seen[name] = true
		if entry.Type != "alias" && entry.Type != "exec" {
			return "", "", &AliasChainError{"unsupported_command"}
		}
		if entry.Type == "exec" {
			return name, arg, nil
		}
		target := strings.TrimLeft(strings.TrimSpace(entry.Target), "/")
		if strings.TrimSpace(target) == "" {
			return "", "", &AliasChainError{"invalid_params"}
		}
		parts := strings.Fields(target)
		name = parts[0]
		var joined []string
		if len(parts) > 1 {
			joined = append(joined, strings.Join(parts[1:], " "))
		}
		if arg != "" {
			joined = append(joined, arg)
		}
		arg = strings.Join(joined, " ")
	}
	return name, arg, nil
}

// UnknownCommandLines returns the lead and pointer lines for a slash token with no handler
func UnknownCommandLines(typed string, known []string) (string, string) {
	base := typed
	if fields := strings.Fields(typed); len(fields) > 0 {
		base = fields[0]
	}
	hint := ""
	if close := pydifflib.GetCloseMatches(base, known, 1, 0.6); len(close) > 0 {
		hint = " Did you mean " + close[0] + "?"
	}
	return "Unknown command " + base + " — nothing was sent." + hint, "Type /help for the full list."
}

// DestructiveSkipTokens bypass the destructive slash confirmation inline
var DestructiveSkipTokens = map[string]bool{"now": true, "--yes": true, "-y": true}

// SplitDestructiveSkip removes the command word and skip tokens and reports whether one was present
func SplitDestructiveSkip(cmdText string) (string, bool) {
	tokens := strings.Fields(cmdText)
	if len(tokens) == 0 {
		return "", false
	}
	if strings.HasPrefix(tokens[0], "/") {
		tokens = tokens[1:]
	}
	kept := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		if !DestructiveSkipTokens[strings.ToLower(tok)] {
			kept = append(kept, tok)
		}
	}
	return strings.Join(kept, " "), len(kept) != len(tokens)
}
