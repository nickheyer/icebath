package commands

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
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

// Refusal of a registered command that has no handler bound
type UnboundError struct{ Name string }

func (e *UnboundError) Error() string { return "commands: /" + e.Name + " has no handler bound" }

// Refusal of a name no registry entry resolves
type UnknownCommandError struct{ Name string }

func (e *UnknownCommandError) Error() string {
	if refusal, ok := SlashTierRefusal(e.Name); ok {
		return refusal
	}
	return "commands: unknown command /" + e.Name
}

// Runs the handler bound to a typed command or alias against the host it hands its work to
func RunHandler(ctx context.Context, r *Registry, name, args string, host any) error {
	line := "/" + strings.TrimLeft(name, "/")
	if strings.TrimSpace(args) != "" {
		line += " " + args
	}
	inv, kind := ParseWith(r, line)
	if kind != KindSlash {
		return &UnknownCommandError{Name: strings.TrimLeft(strings.ToLower(name), "/")}
	}
	if inv.Def.Handler == nil {
		return &UnboundError{Name: inv.Def.Name}
	}
	inv.Host = host
	return inv.Def.Handler(ctx, inv)
}

// Gateway view of the default registry command a typed name binds a handler to, false when it binds none
func BoundGatewayCommand(name string) (gateway.RegistryCommand, bool) {
	d := Resolve(name)
	if d == nil || d.Handler == nil {
		return gateway.RegistryCommand{}, false
	}
	return gateway.RegistryCommand{Name: d.Name, BusyPolicy: string(d.BusyPolicy), BusyHandler: d.BusyHandler}, true
}

// Runs the default registry handler bound to a typed name against the host taking its turn
func RunBoundGatewayCommand(ctx context.Context, name, arg string, host any) error {
	return RunHandler(ctx, Default, name, arg, host)
}
