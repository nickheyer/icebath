// Package hooks runs the user's shell hooks: commands the settings set on the agent's events, each handed the event as JSON on its standard
// input and answering with JSON on its standard output, run only once the user allowed it; and posts the events the settings name to
// outbound webhooks
package hooks

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/hooks"
	"github.com/nickheyer/icebath/internal/packages"
	shellconfig "github.com/nickheyer/icebath/internal/plugins/defaults/hooks/shellhooks/config"
	"github.com/nickheyer/icebath/internal/terminal"
)

// What the shell hooks and outbound webhooks set on the hook points, for every profile
type Registrant struct {
	// Package manager, whose Git Bash runs shell scripts on Windows
	packages *packages.Packages
	// Where commands run, whose environment of a profile's commands the hooks' commands get, the credentials the profile does not pass
	// through left out, as a process that serves several profiles keeps one profile's keys from another's commands
	terminal *terminal.Terminal
	mu       sync.Mutex
	profiles map[string]*profile
	// Posts of events waiting to be delivered
	outbound *queue
}

// What the registrant keeps of one profile
type profile struct {
	// Hooks of the events setting as last read, the setting they were read from, and whether it was read at all
	read     string
	specs    []Spec
	complete bool
	parsed   bool
	// Targets of the outbound setting as last read, the setting they were read from, and whether it was read at all
	posted  string
	targets []*Target
	listed  bool
	// Hooks wired now, in the order they were wired
	wired []Spec
	// Approvals a launch gave that the allowlist could not keep, held for the life of this process
	launched map[pair]bool
	// Approvals taken from the auto accept setting so far
	accepted map[pair]bool
	// Warnings given already
	warned map[string]bool
	// Allowlist as last read
	list *listed
}

func Register(registry *hooks.Registry, manager *packages.Packages, commands *terminal.Terminal) *Registrant {
	r := &Registrant{packages: manager, terminal: commands, profiles: map[string]*profile{}, outbound: newQueue()}
	registry.Register(r)
	return r
}

// State of the profile, made the first time it is asked for, r.mu held
func (r *Registrant) state(name string) *profile {
	p := r.profiles[name]
	if p == nil {
		p = &profile{launched: map[pair]bool{}, accepted: map[pair]bool{}, warned: map[string]bool{}}
		r.profiles[name] = p
	}
	return p
}

// Warns once of the text for the profile, repeats going to the debug log
func (r *Registrant) warn(name, text string) {
	r.mu.Lock()
	p := r.state(name)
	seen := p.warned[text]
	p.warned[text] = true
	r.mu.Unlock()
	if seen {
		slog.Debug(text)
		return
	}
	slog.Warn(text)
}

// Hooks of the profile's events setting, read again only when the setting changed
func (r *Registrant) configured(cfg *config.Config) ([]Spec, bool) {
	read, _ := encode(cfg.Get(shellconfig.Events), false)
	r.mu.Lock()
	p := r.state(cfg.Profile())
	if p.parsed && p.read == read {
		specs, complete := p.specs, p.complete
		r.mu.Unlock()
		return specs, complete
	}
	r.mu.Unlock()
	specs, complete := r.specs(cfg)
	r.mu.Lock()
	defer r.mu.Unlock()
	p.read, p.specs, p.complete, p.parsed = read, specs, complete, true
	return specs, complete
}

// Configured hooks of the profile, each warned about as it is read
func (r *Registrant) Configured(cfg *config.Config) []Spec {
	specs, _ := r.configured(cfg)
	return specs
}

// Pairs the profile allowed: those of its allowlist and those a launch allowed that the allowlist could not keep; and whether the
// allowlist could be read
func (r *Registrant) allowed(cfg *config.Config) (map[pair]bool, bool) {
	listed, readable := r.allowlist(cfg)
	allowed := maps.Clone(listed)
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.state(cfg.Profile())
	for consent := range p.launched {
		if allowed[consent] {
			delete(p.launched, consent)
			continue
		}
		allowed[consent] = true
	}
	return allowed, readable
}

// Callbacks of the hooks the profile allowed and of its outbound webhooks. A hook's consent comes from the allowlist, a launch's
// --accept-hooks or the auto accept setting; while the allowlist cannot be read or an entry of the events setting is malformed, a hook
// wired before stays wired, so a mistake never disarms a guard
func (r *Registrant) Hooks(cfg *config.Config) []hooks.Hook {
	specs, complete := r.configured(cfg)
	var wanted []Spec
	if len(specs) > 0 {
		allowed, readable := r.allowed(cfg)
		accept := cfg.GetBool(shellconfig.AutoAccept)
		for _, spec := range specs {
			switch {
			case allowed[spec.pair()]:
				wanted = append(wanted, spec)
			case accept:
				r.autoAccept(cfg, spec)
				wanted = append(wanted, spec)
			default:
				r.warn(cfg.Profile(), fmt.Sprintf("shell hook for %s (%s) not allowlisted in %s — skipped. Approve it at a CLI prompt for this profile, or use "+
					"--accept-hooks / ICEBATH_ACCEPT_HOOKS=1 / %s: true.", spec.Event, spec.Command, Allowlist(cfg), shellconfig.AutoAccept))
			}
		}
		if !(readable && complete) {
			reason := "an entry of " + shellconfig.Events + " is malformed"
			if !readable {
				reason = "the allowlist is unreadable"
			}
			r.warn(cfg.Profile(), fmt.Sprintf("shell hooks: %s, so removed or revoked hooks stay registered until it is fixed", reason))
		}
		complete = readable && complete
	}
	if complete {
		r.forget(cfg.Profile(), specs)
	}
	var out []hooks.Hook
	for _, spec := range r.wire(cfg.Profile(), wanted, complete) {
		out = append(out, hooks.Hook{Point: spec.Event, Name: fmt.Sprintf("shell_hook[%s:%s]", spec.Event, spec.Command), Owner: "shellhooks",
			Run: r.callback(cfg, spec)})
	}
	return append(out, r.posts(cfg)...)
}

// Lets go of the approvals a launch gave hooks the events setting no longer sets
func (r *Registrant) forget(name string, specs []Spec) {
	configured := map[pair]bool{}
	for _, spec := range specs {
		configured[spec.pair()] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.state(name)
	for consent := range p.launched {
		if !configured[consent] {
			delete(p.launched, consent)
		}
	}
}

// Records the approval the auto accept setting gives a hook not yet on the allowlist, once
func (r *Registrant) autoAccept(cfg *config.Config, spec Spec) {
	r.mu.Lock()
	p := r.state(cfg.Profile())
	done := p.accepted[spec.pair()]
	p.accepted[spec.pair()] = true
	r.mu.Unlock()
	if done {
		return
	}
	if err := r.record(cfg, spec.Event, spec.Command); err != nil {
		slog.Warn(fmt.Sprintf("shell hook approval could not be recorded: %s -> %s: %v", spec.Event, spec.Command, err))
		return
	}
	slog.Info(fmt.Sprintf("shell hook auto-approved via --accept-hooks / env / config: %s -> %s", spec.Event, spec.Command))
}

// Brings the profile's wired hooks in step with those wanted: new ones wired after those there, and with complete those no longer wanted
// let go; gives the hooks wired now
func (r *Registrant) wire(name string, wanted []Spec, complete bool) []Spec {
	r.mu.Lock()
	p := r.state(name)
	want := map[key]Spec{}
	for _, spec := range wanted {
		if _, dup := want[spec.key()]; !dup {
			want[spec.key()] = spec
		}
	}
	var kept, gone []Spec
	have := map[key]bool{}
	for _, spec := range p.wired {
		if fresh, ok := want[spec.key()]; ok {
			kept = append(kept, fresh)
			have[spec.key()] = true
			continue
		}
		if !complete {
			kept = append(kept, spec)
			have[spec.key()] = true
			continue
		}
		gone = append(gone, spec)
	}
	var added []Spec
	for _, spec := range wanted {
		if !have[spec.key()] {
			have[spec.key()] = true
			kept = append(kept, spec)
			added = append(added, spec)
		}
	}
	p.wired = kept
	r.mu.Unlock()
	for _, spec := range added {
		matcher := cmp.Or(spec.Matcher.Text, "None")
		slog.Info(fmt.Sprintf("shell hook registered: %s -> %s (matcher=%s, timeout=%ds, fail_closed=%s)", spec.Event, spec.Command, matcher, spec.Timeout,
			show(spec.FailClosed)))
	}
	slices.SortFunc(gone, func(a, b Spec) int { return strings.Compare(a.Event+"\x00"+a.Command, b.Event+"\x00"+b.Command) })
	for _, spec := range gone {
		slog.Info(fmt.Sprintf("shell hook unregistered (removed from config or allowlist): %s -> %s", spec.Event, spec.Command))
	}
	return kept
}

// Callback of a hook: for the tools its matcher names, its command run with the event and its answer handed back
func (r *Registrant) callback(cfg *config.Config, spec Spec) func(context.Context, hooks.Event) (*hooks.Result, error) {
	point, _ := hooks.PointOf(spec.Event)
	return func(ctx context.Context, event hooks.Event) (*hooks.Result, error) {
		fields := hooks.Payload(event)
		if point.Tool {
			tool, _ := fields["tool_name"].(string)
			if ok, err := spec.Matcher.Matches(tool); err != nil || !ok {
				return nil, err
			}
		}
		run, err := r.once(ctx, cfg, spec, fields)
		if err != nil {
			return nil, err
		}
		return result(run.Directive), nil
	}
}

// Takes consent for every hook the profile's events setting sets, as a launch with --accept-hooks gives it: each recorded on the allowlist,
// and held for the life of this process when the allowlist could not keep it
func (r *Registrant) Accept(_ context.Context, cfg *config.Config) error {
	specs, _ := r.configured(cfg)
	if len(specs) == 0 {
		return nil
	}
	allowed, _ := r.allowed(cfg)
	var errs []error
	var accepted []Spec
	for _, spec := range specs {
		if allowed[spec.pair()] {
			continue
		}
		if err := r.record(cfg, spec.Event, spec.Command); err != nil {
			errs = append(errs, err)
			continue
		}
		allowed[spec.pair()] = true
		accepted = append(accepted, spec)
		slog.Info(fmt.Sprintf("shell hook auto-approved via --accept-hooks / env / config: %s -> %s", spec.Event, spec.Command))
	}
	saved, _ := r.allowlist(cfg)
	r.mu.Lock()
	p := r.state(cfg.Profile())
	for _, spec := range accepted {
		if !saved[spec.pair()] {
			p.launched[spec.pair()] = true
		}
	}
	r.mu.Unlock()
	return errors.Join(errs...)
}

// Allows one configured hook, as a CLI prompt or the hooks screen does
func (r *Registrant) Approve(cfg *config.Config, event, command string) error {
	spec, ok := r.Find(cfg, event, command)
	if !ok {
		return &Refusal{Message: "no such hook: " + event + " " + command}
	}
	return r.record(cfg, spec.Event, spec.Command)
}

// Configured hook of the event and command
func (r *Registrant) Find(cfg *config.Config, event, command string) (Spec, bool) {
	for _, spec := range r.Configured(cfg) {
		if spec.Event == event && spec.Command == command {
			return spec, true
		}
	}
	return Spec{}, false
}

// Waits a few seconds for the posts of events still queued, as icebath exits
func (r *Registrant) Close() error {
	r.outbound.flush(flushWait)
	return nil
}

// A change to the hooks refused for what was asked
type Refusal struct{ Message string }

func (e *Refusal) Error() string { return e.Message }
