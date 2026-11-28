package web

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	webconfig "github.com/nickheyer/icebath/internal/web/config"
)

// States of a service, in the words every page and command says them in
const (
	InUse         = "In use"
	InTurns       = "In use, taking turns"
	InSearches    = "In use for searches"
	InPages       = "In use for pages"
	Ready         = "Ready"
	FreeState     = "Free"
	NeedsKey      = "Needs an API key"
	NeedsAddress  = "Needs a server address"
	NeedsSignIn   = "Needs a sign-in"
	NeedsSettings = "Needs setup"
)

// What a service is chosen for
type Use int

const (
	// Left as it is, when only the plan changes
	Unchanged Use = iota
	Both
	Searches
	Pages
)

// How a service stands for a profile
type Standing struct {
	// Its state in the state words
	State string
	// It can answer as it is: set up, or on a free plan
	Usable bool
	// It answers searches, reads pages, and does either taking turns with the other free services
	Searching, Reading, Rotating bool
	// What a plan of it needs, when it can't answer: the setting of its key or address, and whether that is a secret
	Needs string
}

// How the service stands for the profile: what it answers now, else whether it is ready, free, or what it needs first
func (r *Registry) Standing(cfg *config.Config, backend Backend) Standing {
	searching, reading := r.Settled(cfg, Searching), r.Settled(cfg, Reading)
	rotating := slices.ContainsFunc(r.Rotation(cfg), func(free Backend) bool { return free.Name == backend.Name })
	s := Standing{
		Searching: backend.Search != nil && (searching == backend.Name || searching == "" && rotating),
		Reading:   backend.Extract != nil && (reading == backend.Name || reading == "" && rotating),
		Rotating:  rotating && (searching == "" || reading == ""),
	}
	switch {
	case backend.Set(cfg):
		s.State, s.Usable = Ready, true
	case backend.FreeReady(cfg) || slices.ContainsFunc(backend.Plans, func(p Plan) bool { return p.Key == "" && p.Prompt == "" }):
		s.State, s.Usable = FreeState, true
	default:
		s.State = NeedsSettings
		for _, plan := range backend.Plans {
			if s.State != NeedsSettings {
				break
			}
			switch _, setting, known := cfg.Setting(plan.Key); {
			case known && setting.Secret:
				s.State, s.Needs = NeedsKey, plan.Key
			case plan.Key != "":
				s.State, s.Needs = NeedsAddress, plan.Key
			case plan.Prompt != "":
				s.State = NeedsSignIn
			}
		}
	}
	switch {
	case s.Rotating && (s.Searching || s.Reading):
		s.State = InTurns
	case s.Searching && s.Reading:
		s.State = InUse
	case s.Searching:
		s.State = InSearches
	case s.Reading:
		s.State = InPages
	}
	return s
}

// What answers searches and reads pages for the profile now, as one sentence
func (r *Registry) InUse(cfg *config.Config) string {
	search, searchTurns := r.answerer(cfg, Searching)
	read, readTurns := r.answerer(cfg, Reading)
	if !r.Available(cfg) || search == "" && read == "" && !searchTurns && !readTurns {
		return "No service is ready, so the agent can't search the web or read pages."
	}
	turns := "the free services take turns to "
	switch {
	case searchTurns && readTurns:
		return "The free services take turns to answer searches and read pages."
	case search == read:
		return search + " answers searches and reads pages."
	case searchTurns && read == "":
		return "The free services take turns to answer searches, and no service reads pages."
	case searchTurns:
		return read + " reads pages, and " + turns + "answer searches."
	case readTurns && search == "":
		return "The free services take turns to read pages, and no service answers searches."
	case readTurns:
		return search + " answers searches, and " + turns + "read pages."
	case search == "":
		return read + " reads pages, and no service answers searches."
	case read == "":
		return search + " answers searches, and no service reads pages."
	}
	return search + " answers searches, and " + read + " reads pages."
}

// Who answers the capability: the title of the service the settings or what is set up give it, or that the free services take turns,
// nothing for none
func (r *Registry) answerer(cfg *config.Config, capability Capability) (string, bool) {
	if name := r.Settled(cfg, capability); name != "" {
		if backend, ok := r.Backend(name); ok {
			return backend.Title, false
		}
		return name, false
	}
	if slices.ContainsFunc(r.Rotation(cfg), func(free Backend) bool { return free.serves(capability) }) {
		return "", true
	}
	return "", false
}

// Chooses what the service of the name is used for and its plan, clearing what the choice replaces, and answers what is in use for what
// now; no name leaves every choice to Automatic. A plan of auto puts it back to what its key decides, an empty one leaves it
func (r *Registry) Choose(cfg *config.Config, name string, use Use, plan string) (string, error) {
	writes := map[string]any{}
	if name == "" {
		for _, key := range []string{webconfig.Backend, webconfig.SearchBackend, webconfig.ExtractBackend} {
			writes[key] = nil
		}
		if err := write(cfg, writes); err != nil {
			return "", err
		}
		return "Web search is now Automatic. " + r.InUse(cfg), nil
	}
	backend, ok := r.Backend(name)
	if !ok {
		return "", fmt.Errorf("There is no web search service named %s.", name)
	}
	switch {
	case use == Both && backend.Extract == nil:
		return "", fmt.Errorf("%s can't read pages, so it can't be used for searches and pages.", backend.Title)
	case use == Both && backend.Search == nil:
		return "", fmt.Errorf("%s can't search, so it can't be used for searches and pages.", backend.Title)
	case use == Searches && backend.Search == nil:
		return "", fmt.Errorf("%s can't search, so it can't be used for searches.", backend.Title)
	case use == Pages && backend.Extract == nil:
		return "", fmt.Errorf("%s can't read pages, so it can't be used for pages.", backend.Title)
	}
	if plan != "" {
		tiers := map[string]any{}
		if held, ok := cfg.Get(webconfig.ProviderTier).(map[string]any); ok {
			tiers = maps.Clone(held)
		}
		delete(tiers, backend.Name)
		if plan != webconfig.Auto {
			tiers[backend.Name] = plan
		}
		writes[webconfig.ProviderTier] = tiers
	}
	chosen := func(key string) bool { return Chosen(cfg, key) == backend.Name }
	switch use {
	case Both:
		writes[webconfig.Backend], writes[webconfig.SearchBackend], writes[webconfig.ExtractBackend] = backend.Name, nil, nil
	case Searches:
		writes[webconfig.SearchBackend] = backend.Name
		if chosen(webconfig.Backend) {
			writes[webconfig.Backend] = nil
		}
		if chosen(webconfig.ExtractBackend) {
			writes[webconfig.ExtractBackend] = nil
		}
	case Pages:
		writes[webconfig.ExtractBackend] = backend.Name
		if chosen(webconfig.Backend) {
			writes[webconfig.Backend] = nil
		}
		if chosen(webconfig.SearchBackend) {
			writes[webconfig.SearchBackend] = nil
		}
	}
	if err := write(cfg, writes); err != nil {
		return "", err
	}
	if use == Unchanged {
		return backend.Title + " is now on its " + planWords(plan) + ". " + r.InUse(cfg), nil
	}
	return r.InUse(cfg), nil
}

// Words of a plan as a sentence names it
func planWords(plan string) string {
	switch plan {
	case webconfig.Free:
		return "free plan"
	case webconfig.Paid:
		return "paid plan"
	}
	return "plan that its key decides"
}

// Saves the values by key in a fixed order, nil putting a default back
func write(cfg *config.Config, writes map[string]any) error {
	keys := slices.Sorted(maps.Keys(writes))
	for _, key := range keys {
		if err := cfg.Set(key, writes[key]); err != nil {
			return fmt.Errorf("%s was not saved. %s", strings.TrimPrefix(key, webconfig.Key+"."), err)
		}
	}
	return nil
}
