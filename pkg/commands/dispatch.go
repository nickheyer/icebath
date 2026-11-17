package commands

import (
	"strings"
)

// Kind classifies one typed composer line
type Kind int

const (
	// KindPrompt is ordinary text for the agent
	KindPrompt Kind = iota
	// KindSlash is a registered slash command or alias
	KindSlash
	// KindBang is a shell command typed with a leading exclamation mark
	KindBang
	// KindSkill is a slash token with no registered command, a quick, plugin, bundle or skill candidate
	KindSkill
)

func (k Kind) String() string {
	switch k {
	case KindSlash:
		return "slash"
	case KindBang:
		return "bang"
	case KindSkill:
		return "skill"
	}
	return "prompt"
}

// LooksLikeSlashCommand reports whether text starts a slash command rather than an absolute path
func LooksLikeSlashCommand(text string) bool {
	if text == "" || !strings.HasPrefix(text, "/") {
		return false
	}
	first := strings.Fields(text)[0]
	return !strings.Contains(first[1:], "/")
}

// SlashArgs returns the stripped text after the command word
func SlashArgs(cmd string) string {
	parts := strings.Fields(cmd)
	if len(parts) < 2 {
		return ""
	}
	rest := strings.TrimSpace(cmd)
	return strings.TrimSpace(rest[len(parts[0]):])
}

// Parse classifies a typed line and builds the invocation for slash and bang input
func Parse(line string) (*Invocation, Kind) {
	return ParseWith(Default, line)
}

// ParseWith classifies a typed line against a specific registry
func ParseWith(r *Registry, line string) (*Invocation, Kind) {
	if IsBangCommand(line) {
		shell := ParseBangCommand(line)
		return &Invocation{Name: "!", Raw: strings.TrimSpace(line), Args: shell, Argv: ShlexArgs("! " + shell)}, KindBang
	}
	if !LooksLikeSlashCommand(line) {
		return nil, KindPrompt
	}
	raw := strings.TrimSpace(line)
	base := strings.Fields(raw)[0]
	bare := strings.TrimLeft(strings.ToLower(base), "/")
	inv := &Invocation{Name: bare, Raw: raw, Args: SlashArgs(raw), Argv: ShlexArgs(raw)}
	if d := r.Lookup(bare); d != nil {
		inv.Name = d.Name
		inv.Def = d
		return inv, KindSlash
	}
	return inv, KindSkill
}

// ExecuteKeyOf returns the shared executor key of a command, empty when it has none
func ExecuteKeyOf(name string) string { return MetaOf(name).Execute }
