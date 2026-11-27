package prompt

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

// Part of the system prompt a section goes in. Parts are joined in this order, what changes least first, so a provider's prompt cache keeps
// the longest prefix from turn to turn
type Tier int

const (
	// Same on every turn of every session: who the agent is and how it works
	Stable Tier = iota
	// Of the session's working directory: project instructions, the workspace
	Project
	// What the agent keeps across sessions and changes as it works: memory, skills
	Kept
	// Of the moment the prompt is built: the profile, the time, where the tools run
	Turn
)

// Tiers in the order they are joined
var Tiers = []Tier{Stable, Project, Kept, Turn}

func (t Tier) String() string {
	switch t {
	case Stable:
		return "stable"
	case Project:
		return "project"
	case Kept:
		return "kept"
	}
	return "turn"
}

type Section struct {
	Tier Tier
	// What the section is, such as "identity" or "context files", which prompt-size lists it by; a section without one is counted in its
	// tier alone
	Name string
	Text string
}

// What the sections of one turn are made for
type Env struct {
	// Settings of the profile the session runs on
	Config    *config.Config
	SessionID string
	Cwd       string
	Provider  string
	Model     string
	// Names of the tools offered on the turn
	Tools []string
	// Kind of the provider, by the name of its spec
	Kind string
	// Where the session is shown, such as "tui" or "cli": the surface whose hints the prompt carries
	Platform string
	// When the conversation started
	Started time.Time
	// Tokens the model takes in at once, 0 when not known
	ContextLength int
	// Tools offered on the turn as the model is sent them
	Definitions []map[string]any
	// The user asked for no project instructions, SOUL.md, memory or preloaded skills
	IgnoreRules bool
	// Skills the session was started with, such as by icebath -s, loaded into its prompt
	Skills []string
	// The session works in a code project with the coding focus posture on, so what lies outside coding is offered by name only
	Focus bool
	// The session answers one query and ends, such as icebath -z
	OneShot bool
	// Tells the user something about the prompt being built, such as a file cut to fit; nil when nobody is told
	Note func(text string)
	// Text the session keeps under the name from the first time it is made, so a prompt built again says it the same way; nil makes it
	// each time
	Keep func(name string, make func() string) string
}

// Tells the user, when someone is told
func (e *Env) Notify(text string) {
	if e.Note != nil {
		e.Note(text)
	}
}

// Text the session keeps under the name, made the first time
func (e *Env) Kept(name string, make func() string) string {
	if e.Keep == nil {
		return make()
	}
	return e.Keep(name, make)
}

// Whether the tool is offered
func (e *Env) Offers(tool string) bool {
	return slices.Contains(e.Tools, tool)
}

// Whether the tool is offered and takes the argument
func (e *Env) Takes(tool, argument string) bool {
	for _, definition := range e.Definitions {
		function, _ := definition["function"].(map[string]any)
		if function["name"] != tool {
			continue
		}
		parameters, _ := function["parameters"].(map[string]any)
		properties, _ := parameters["properties"].(map[string]any)
		_, ok := properties[argument]
		return ok
	}
	return false
}

type Registrant interface {
	Sections(ctx context.Context, env *Env) ([]Section, error)
}

// What keeps the system prompts of sessions, the agent: told to build them again, and asked what a new session would be sent. A registrant
// that keeps them says so by having these too
type Keeper interface {
	// Builds the prompt of the session again on its next turn, of every session for no id. Says the session the id names, and how many
	// sessions held a prompt
	Refresh(session string) (string, int, error)
	// What a new session in the directory, shown on the platform, would be sent: what its prompt is built for, and the toolset of each of
	// its tools by name
	Preview(ctx context.Context, directory, platform string) (*Env, map[string]string, error)
}

var ErrNoKeeper = errors.New("Nothing builds system prompts here.")

type Registry struct {
	registrants []Registrant
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Sections(ctx context.Context, env *Env) ([]Section, error) {
	var sections []Section
	for _, registrant := range r.registrants {
		more, err := registrant.Sections(ctx, env)
		if err != nil {
			return nil, err
		}
		sections = append(sections, more...)
	}
	return sections, nil
}

// System prompt of the turn: every section tier by tier, in the order registered within a tier
func (r *Registry) Build(ctx context.Context, env *Env) (string, error) {
	sections, err := r.Sections(ctx, env)
	if err != nil {
		return "", err
	}
	return Join(sections), nil
}

// Sections tier by tier, in their order within a tier, without the blank ones
func Join(sections []Section) string {
	sections = slices.Clone(sections)
	slices.SortStableFunc(sections, func(a, b Section) int { return cmp.Compare(a.Tier, b.Tier) })
	var parts []string
	for _, section := range sections {
		if text := strings.TrimSpace(section.Text); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// Start of the prompt the sections join into that stays the same from session to session: its stable tier
func Prefix(sections []Section) string {
	return Join(slices.DeleteFunc(slices.Clone(sections), func(section Section) bool { return section.Tier != Stable }))
}

func (r *Registry) Refresh(session string) (string, int, error) {
	keeper, ok := r.keeper()
	if !ok {
		return "", 0, ErrNoKeeper
	}
	return keeper.Refresh(session)
}

func (r *Registry) Preview(ctx context.Context, directory, platform string) (*Env, map[string]string, error) {
	keeper, ok := r.keeper()
	if !ok {
		return nil, nil, ErrNoKeeper
	}
	return keeper.Preview(ctx, directory, platform)
}

// First registrant that keeps prompts, looked for in nested registries too
func (r *Registry) keeper() (Keeper, bool) {
	for _, registrant := range r.registrants {
		if nested, ok := registrant.(*Registry); ok {
			if keeper, ok := nested.keeper(); ok {
				return keeper, true
			}
			continue
		}
		if keeper, ok := registrant.(Keeper); ok {
			return keeper, true
		}
	}
	return nil, false
}

type Prompt struct {
	Registry
}

func New() *Prompt {
	return &Prompt{}
}
