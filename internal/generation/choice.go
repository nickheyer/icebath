package generation

import (
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	generationconfig "github.com/nickheyer/icebath/internal/generation/config"
)

// How a service stands for a profile, in the words every page and message says it in
type State int

const (
	// Makes things of the kind for the agent now
	InUse State = iota
	// Its key or account is there
	Ready
	// Ready, and costs nothing
	Free
	// Lacks the API key of its own it asks for
	NeedsKey
	// Lacks the account at a provider it uses
	NotConnected
)

func (s State) String() string {
	switch s {
	case InUse:
		return "In use"
	case Ready:
		return "Ready"
	case Free:
		return "Free"
	case NeedsKey:
		return "Needs an API key"
	}
	return "Not connected"
}

// Whether the agent makes things of the kind at all: images always, videos only while Make videos is on
func On(cfg *config.Config, kind Kind) bool {
	return kind != Video || cfg.GetBool(generationconfig.VideoEnabled)
}

// Name of the service that makes things of the kind for the agent now, empty while none does: videos turned off, the service chosen not
// there or not ready, or none ready
func (r *Registry) Serving(cfg *config.Config, kind Kind) string {
	if !On(cfg, kind) {
		return ""
	}
	service, err := r.Active(cfg, kind)
	if err != nil || !service.Set(cfg) {
		return ""
	}
	return service.Name
}

// How the service stands for the profile: in use while it makes things of the kind for the agent, else ready, free or what it lacks
func (r *Registry) State(cfg *config.Config, kind Kind, s Service) State {
	switch {
	case r.Serving(cfg, kind) == s.Name:
		return InUse
	case s.Set(cfg) && s.Setup.Badge == "free":
		return Free
	case s.Set(cfg):
		return Ready
	case s.Setup.Key != "":
		return NeedsKey
	}
	return NotConnected
}

// What keeps the agent from making things of the kind, a sentence; empty when nothing does or the person turned them off
func (r *Registry) Hindrance(cfg *config.Config, kind Kind) string {
	if !On(cfg, kind) {
		return ""
	}
	noun := kind.noun()
	service, err := r.Active(cfg, kind)
	switch {
	case err != nil && Chosen(cfg, kind) != "":
		return fmt.Sprintf("Your settings name the %s service %s, which this icebath does not have. Pick one below.", kind, Chosen(cfg, kind))
	case err != nil:
		return "The agent can't make " + noun + " until a service is ready. Pick one below."
	case !service.Set(cfg) && service.Setup.Key != "":
		return service.Title + " can't make " + noun + " until it has an API key."
	case !service.Set(cfg):
		return service.Title + " can't make " + noun + " until its account is connected."
	}
	return ""
}

// Uses the service of the name for things of the kind, the model it has kept as it was; the first service that is ready for no name. A
// name no service of the kind has is refused in a person's words
func (r *Registry) Choose(cfg *config.Config, kind Kind, name string) error {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return cfg.Set(kind.choice(), nil)
	}
	if _, ok := r.Service(kind, name); !ok {
		return fmt.Errorf("No %s service is called %s.", kind, name)
	}
	return cfg.Set(kind.choice(), name)
}

// Turns the making of videos on or off, the one control of whether the agent makes them
func SetVideos(cfg *config.Config, on bool) error {
	return cfg.Set(generationconfig.VideoEnabled, on)
}

// What a choice of service says once it is kept, Automatic for no name
func (r *Registry) Note(cfg *config.Config, kind Kind, name string) string {
	noun := strings.ToUpper(kind.noun()[:1]) + kind.noun()[1:]
	if name == "" {
		return noun + " now come from the first service that is ready."
	}
	service, _ := r.Service(kind, name)
	switch {
	case !On(cfg, kind):
		return noun + " now come from " + service.Title + " once Make videos is on."
	case !service.Set(cfg):
		return noun + " now come from " + service.Title + " once it is ready."
	}
	return service.Title + " makes " + kind.noun() + " now."
}

// Plural of what the kind makes, lower case
func (k Kind) noun() string {
	return string(k) + "s"
}

// Takes every write a person asks for of the choice of service and of the switch of videos through the functions the pages use
func (r *Registry) guard(cfg *config.Config) {
	for _, kind := range []Kind{Image, Video} {
		cfg.Guard(kind.choice(), config.Guard{Write: func(cfg *config.Config, _ string, value any) (bool, error) {
			name, _ := value.(string)
			return true, r.Choose(cfg, kind, name)
		}})
	}
	cfg.Guard(generationconfig.VideoEnabled, config.Guard{Write: func(cfg *config.Config, key string, value any) (bool, error) {
		on, ok := value.(bool)
		if !ok {
			return true, cfg.Set(key, nil)
		}
		return true, SetVideos(cfg, on)
	}})
}
