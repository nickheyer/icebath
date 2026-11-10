package commands

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
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

type busyKey struct{}

// WithBusy marks a context as dispatched while the agent runs
func WithBusy(ctx context.Context) context.Context { return context.WithValue(ctx, busyKey{}, true) }

// IsBusy reports whether the invocation runs while the agent is mid turn
func IsBusy(ctx context.Context) bool { v, _ := ctx.Value(busyKey{}).(bool); return v }

// Interrupter is implemented by hosts that can cancel the running turn before a command
type Interrupter interface {
	Interrupt(ctx context.Context) error
}

// UnboundError reports a registered command with no handler bound
type UnboundError struct{ Name string }

func (e *UnboundError) Error() string { return "commands: /" + e.Name + " has no handler bound" }

// UnknownCommandError reports a name no registry entry resolves
type UnknownCommandError struct{ Name string }

func (e *UnknownCommandError) Error() string { return "commands: unknown command /" + e.Name }

// BusyError refuses a command typed while the agent runs, with the text to show
type BusyError struct {
	Name string
	Text string
}

func (e *BusyError) Error() string { return e.Text }

// ErrNoInterrupter means an interrupt first command ran busy on a host that cannot interrupt
var ErrNoInterrupter = errors.New("commands: host cannot interrupt the running turn")

// SlashRejectedText is the catch all refusal for a command that cannot run mid turn
func SlashRejectedText(name string) string {
	return fmt.Sprintf("⏳ Agent is running — `/%s` can't run mid-turn. Wait for the current response or `/stop` first.", name)
}

// Dispatcher runs invocations against a registry with mid run variants keyed by busy handler
type Dispatcher struct {
	Registry *Registry
	mu       sync.RWMutex
	busy     map[string]Handler
}

// DefaultDispatcher dispatches against the default registry
var DefaultDispatcher = &Dispatcher{Registry: Default}

// BindBusy attaches the mid run variant for a busy handler key
func (d *Dispatcher) BindBusy(key string, h Handler) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.busy == nil {
		d.busy = map[string]Handler{}
	}
	d.busy[key] = h
}

func (d *Dispatcher) busyVariant(key string) Handler {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.busy[key]
}

// Dispatch runs an invocation through the default dispatcher honoring the busy policy
func Dispatch(ctx context.Context, inv *Invocation, host any, busy bool) error {
	return DefaultDispatcher.Dispatch(ctx, inv, host, busy)
}

// Dispatch runs the handler, or the mid run variant, refusal or interrupt the busy policy selects
func (d *Dispatcher) Dispatch(ctx context.Context, inv *Invocation, host any, busy bool) error {
	if inv == nil {
		return &UnknownCommandError{}
	}
	if inv.Def == nil {
		inv.Def = d.Registry.Lookup(strings.TrimLeft(strings.ToLower(inv.Name), "/"))
	}
	if inv.Def == nil {
		return &UnknownCommandError{Name: inv.Name}
	}
	inv.Name = inv.Def.Name
	inv.Host = host
	def := inv.Def
	if !busy {
		if def.Handler == nil {
			return &UnboundError{Name: def.Name}
		}
		return def.Handler(ctx, inv)
	}
	ctx = WithBusy(ctx)
	if def.BusyHandler != "" {
		if variant := d.busyVariant(def.BusyHandler); variant != nil {
			return variant(ctx, inv)
		}
		if text, ok := BusyRejectText[def.BusyHandler]; ok {
			return &BusyError{Name: def.Name, Text: text}
		}
	}
	switch def.BusyPolicy {
	case BusyInterruptThenDispatch:
		if def.Handler == nil {
			return &BusyError{Name: def.Name, Text: SlashRejectedText(def.Name)}
		}
		in, ok := host.(Interrupter)
		if !ok {
			return ErrNoInterrupter
		}
		if err := in.Interrupt(ctx); err != nil {
			return err
		}
		return def.Handler(ctx, inv)
	case BusyDispatch:
		if def.Handler != nil {
			return def.Handler(ctx, inv)
		}
	}
	return &BusyError{Name: def.Name, Text: SlashRejectedText(def.Name)}
}

// Unbound lists registered commands with no handler so startup can refuse to run with a hole
func Unbound() []string { return UnboundIn(Default) }

// UnboundIn lists commands of one registry that have no handler bound
func UnboundIn(r *Registry) []string {
	var out []string
	for _, n := range r.Names() {
		if d := r.Lookup(n); d != nil && d.Handler == nil {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// ExecuteKeyOf returns the shared executor key of a command, empty when it has none
func ExecuteKeyOf(name string) string { return MetaOf(name).Execute }
