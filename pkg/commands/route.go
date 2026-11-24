package commands

import (
	"strings"

	shellutil "github.com/nickheyer/icebath/pkg/utils/shell"
)

// ShlexArgs splits the tokens after the command word with POSIX shell quoting, whitespace splitting on unbalanced quotes
func ShlexArgs(cmd string) []string {
	if strings.TrimSpace(cmd) == "" {
		return nil
	}
	tokens, err := shellutil.Split(cmd)
	if err != nil {
		tokens = strings.Fields(cmd)
	}
	if len(tokens) == 0 {
		return nil
	}
	return tokens[1:]
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
