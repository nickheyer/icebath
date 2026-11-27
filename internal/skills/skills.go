package skills

import (
	"context"
	"errors"
	"io/fs"
	"slices"
	"sync"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/hooks"
	"github.com/nickheyer/icebath/internal/packages"
	skillsconfig "github.com/nickheyer/icebath/internal/skills/config"
	"github.com/nickheyer/icebath/internal/tools"
)

// How a source's skills reach a profile
type Kind int

const (
	// Copied into each profile's skills folder and kept up to date there, the user's own edits kept: the bundled skills
	Seeded Kind = iota
	// Installed into a profile's skills folder on request: the official optional skills the hub offers
	Offered
	// Served in place under the source's name, as name:skill, and never changed: a plugin's own skills
	Namespaced
)

// Skills a plugin contributes
type Source struct {
	// Name of the set, such as "bundled" or "official"; the namespace of a namespaced source's skills
	Name string
	Kind Kind
	// Category folders over skill folders, each skill a folder holding a SKILL.md
	Files fs.FS
	// Folder the files are read from when they are on disk rather than in the binary, for messages
	Dir string
}

type Registrant interface {
	// Sources for the profile of the settings
	Sources(cfg *config.Config) []Source
}

// What runs sessions, the agent: takes text into a session as the user's next message to the model. A registrant that runs sessions says
// so by having this too
type Runner interface {
	// Sends the text to the model as the user's next message of the session, queued behind a reply in progress; display is what the user
	// typed, which the session's title and transcript show in place of the text
	Submit(ctx context.Context, session, text, display string) error
}

// What scans skills for dangerous content, the guard of the hub. A registrant that scans says so by having this too
type Scanner interface {
	// Scans the skill folder as coming from the source, such as "agent-created" or "project-local"
	Scan(ctx context.Context, cfg *config.Config, dir, source string) (Verdict, error)
}

// What a scan found
type Verdict struct {
	// safe, caution or dangerous
	Level string
	// Whether content like this may stay where it was written
	Allowed bool
	// Why it may not, in a few words
	Reason string
	// The findings, as the user and the model read them
	Report string
}

// What knows the apps a skill can require, such as a connected service: a skill requiring an app is offered only when the app is.
// A registrant that knows apps says so by having this too
type Apps interface {
	// Whether an app of the name is known, and whether it can be offered here
	App(name string) (declared, offerable bool)
}

var ErrNoRunner = errors.New("Nothing runs sessions here, so the message was not sent.")

type Registry struct {
	registrants []Registrant
	// What the host keeps across calls, shared with the registries nested in it
	state *state
	once  sync.Once
}

// What the host keeps across calls
type state struct {
	mu sync.Mutex
	// Scanned catalogs by the roots and settings they were scanned for
	catalogs map[string]catalogCache
	// Namespaced sources written to disk, by name, with the digest of their files
	materialized map[string]string
	// Profile folders whose seeded skills were synced by this process
	synced map[string]bool
	// What was already told once, such as a shadowed skill or a quarantined project skill
	told map[string]bool
	// What the skills each session loaded need from the commands it runs, by session
	needs map[string]*needs
	// Tools of the agent, whose toolsets a skill's conditions name
	tools *tools.Registry
	// Package manager, which installs the tools a skill declares it needs
	packages *packages.Packages
	// Hook points that hear of each change of a skill's state its usage records
	hooks *hooks.Hooks
}

func (r *Registry) shared() *state {
	r.once.Do(func() {
		if r.state == nil {
			r.state = &state{}
		}
	})
	return r.state
}

// Whether the key is seen for the first time, so what it stands for is told once per process
func (r *Registry) first(key string) bool {
	st := r.shared()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.told[key] {
		return false
	}
	if st.told == nil {
		st.told = map[string]bool{}
	}
	st.told[key] = true
	return true
}

// Sorted copy of a list
func sortedList(values []string) []string {
	out := slices.Clone(values)
	slices.Sort(out)
	return out
}

func (r *Registry) Register(registrant Registrant) {
	if nested, ok := registrant.(*Registry); ok {
		nested.state = r.shared()
	}
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Sources(cfg *config.Config) []Source {
	var sources []Source
	for _, registrant := range r.registrants {
		sources = append(sources, registrant.Sources(cfg)...)
	}
	return sources
}

// Sources of the kind, in the order registered
func (r *Registry) kind(cfg *config.Config, kind Kind) []Source {
	return slices.DeleteFunc(r.Sources(cfg), func(source Source) bool { return source.Kind != kind })
}

// First registrant with the capability, looked for in nested registries too
func capability[T any](r *Registry) (T, bool) {
	for _, registrant := range r.registrants {
		if nested, ok := registrant.(*Registry); ok {
			if found, ok := capability[T](nested); ok {
				return found, true
			}
			continue
		}
		if found, ok := registrant.(T); ok {
			return found, true
		}
	}
	var zero T
	return zero, false
}

// Sends the text to the model as the user's next message of the session
func (r *Registry) Submit(ctx context.Context, session, text, display string) error {
	runner, ok := capability[Runner](r)
	if !ok {
		return ErrNoRunner
	}
	return runner.Submit(ctx, session, text, display)
}

type Skills struct {
	Registry
}

// Skills over the tools of the agent, whose toolsets a skill's conditions name, and the package manager that installs what they need
func New(cfg *config.Config, toolset *tools.Registry, pm *packages.Packages, points *hooks.Hooks) *Skills {
	s := &Skills{}
	s.shared().tools = toolset
	s.shared().packages = pm
	s.shared().hooks = points
	skillsconfig.Register(cfg, func() []config.Option {
		scope := s.Scope(cfg, "")
		var options []config.Option
		for _, entry := range scope.Loadable(true) {
			options = append(options, config.Option{Value: entry.Load, Title: entry.Load, About: entry.Description})
		}
		for _, p := range scope.Plugins() {
			options = append(options, config.Option{Value: p.Qualified(), Title: p.Qualified(), About: p.Description})
		}
		return options
	})
	return s
}
