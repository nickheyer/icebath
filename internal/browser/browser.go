// The browser the agent drives: the cloud browser services plugins bring, the browsers that hold pages of their own, such as Camofox, and
// the Chromium of this machine that the browser tools and the agent desktop start
package browser

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	browserconfig "github.com/nickheyer/icebath/internal/browser/config"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/packages"
)

// A cloud browser service: browsers on its machines the browser tools drive over the DevTools protocol, picked by browser.cloud_provider
type Provider struct {
	// Value of browser.cloud_provider that picks it
	Name  string
	Title string
	// What it offers, for the picker
	About string
	// Key of the section of its own settings, empty for none
	Section string
	// Variables it reads its keys from, and where to get them
	Keys []Key
	// Picked by its keys alone when the profile chose no service, the lowest first; zero never
	Detect int
	// Whether the profile has what it needs, such as its keys
	Available func(cfg *config.Config) bool
	// Opens a browser for the task
	Create func(ctx context.Context, cfg *config.Config, task string) (*Session, error)
	// Closes the browser of the id, false when it could not
	Close func(ctx context.Context, cfg *config.Config, id string) bool
}

// One key a cloud service reads, by the variable that holds it, and the setting that keeps it, which the variable sets over
type Key struct {
	Env, Prompt, URL string
	Setting          string
}

// A browser a cloud service opened for a task
type Session struct {
	// Id the service closes it by
	ID string
	// Label of it in the logs
	Name string
	// DevTools endpoint the tools drive it through
	CDP string
	// When the service ends it, zero for no set end
	Expires time.Time
	// What the service turned on for it, such as proxies or stealth
	Features map[string]bool
}

// A browser that holds pages of its own outside the DevTools session, such as a Camofox server, on whose page of a task the vault fills
// what the user saved
type Page struct {
	Name, Title string
	// Key of the section of its own settings, empty for none
	Section string
	// Whether it holds a live page for the task, never opening one
	Has func(cfg *config.Config, task string) bool
	// Runs the script on the task's page and answers its value; a secret script goes only where nothing between can read it
	Evaluate func(ctx context.Context, cfg *config.Config, task, script string, secret bool) (any, error)
}

type Registrant interface {
	Providers() []Provider
	Pages() []Page
}

// Services and page holders fixed once they are registered
type Declared struct {
	Services []Provider
	Holders  []Page
}

func (d Declared) Providers() []Provider { return d.Services }

func (d Declared) Pages() []Page { return d.Holders }

type Registry struct {
	registrants []Registrant
	// Host the registry is nested in, nil for the host's own
	host *Browser
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Providers() []Provider {
	var out []Provider
	for _, registrant := range r.registrants {
		out = append(out, registrant.Providers()...)
	}
	return out
}

func (r *Registry) Pages() []Page {
	var out []Page
	for _, registrant := range r.registrants {
		out = append(out, registrant.Pages()...)
	}
	return out
}

// Host the registry is nested in
func (r *Registry) Host() *Browser {
	return r.host
}

// Cloud services, page holders and the Chromium of this machine
type Browser struct {
	Registry
	config   *config.Config
	packages *packages.Packages
}

func New(cfg *config.Config, pm *packages.Packages) *Browser {
	b := &Browser{config: cfg, packages: pm}
	browserconfig.Register(cfg, b.options)
	return b
}

// Adds a registrant; a registry nested in the host reaches it through Host
func (b *Browser) Register(registrant Registrant) {
	if nested, ok := registrant.(*Registry); ok {
		nested.host = b
	}
	b.Registry.Register(registrant)
}

// Choices of browser.cloud_provider beside local: every cloud service, then every browser holding pages of its own, which keeps pages here
func (b *Browser) options() []config.Option {
	var options []config.Option
	for _, p := range b.Providers() {
		options = append(options, config.Option{Value: p.Name, Title: p.Title, About: p.About})
	}
	for _, p := range b.Pages() {
		options = append(options, config.Option{Value: p.Name, Title: p.Title, About: "Pages open in " + p.Title + ", which brings tools of its own"})
	}
	return options
}

// Cloud service of the name
func (b *Browser) Provider(name string) (Provider, bool) {
	for _, p := range b.Providers() {
		if p.Name == name {
			return p, true
		}
	}
	return Provider{}, false
}

// Service the profile chose, lower case, and whether it chose one at all
func Chosen(cfg *config.Config) (string, bool) {
	if cfg.Stored(browserconfig.CloudProvider) == nil {
		return "", false
	}
	return cmp.Or(strings.ToLower(strings.TrimSpace(cfg.GetString(browserconfig.CloudProvider))), browserconfig.Local), true
}

// Error of a browser.cloud_provider naming a service no plugin brings
type UnknownError struct{ Name string }

func (e *UnknownError) Error() string {
	return "browser is configured to use '" + e.Name + "', but no registered browser plugin has that name (install the corresponding plugin " +
		"or fix the config key spelling). Pick another one in /settings → Browser."
}

// Cloud service pages open in for the profile, nil for this machine: the one it chose, local or a browser holding pages of its own keeping
// them here; left unchosen, the first service its keys alone pick
func (b *Browser) Cloud(cfg *config.Config) (*Provider, error) {
	name, chosen := Chosen(cfg)
	if chosen {
		if name == browserconfig.Local || slices.ContainsFunc(b.Pages(), func(p Page) bool { return p.Name == name }) {
			return nil, nil
		}
		p, ok := b.Provider(name)
		if !ok {
			return nil, &UnknownError{Name: name}
		}
		return &p, nil
	}
	detected := slices.DeleteFunc(b.Providers(), func(p Provider) bool { return p.Detect == 0 })
	slices.SortStableFunc(detected, func(x, y Provider) int { return x.Detect - y.Detect })
	for _, p := range detected {
		if p.Available != nil && p.Available(cfg) {
			return &p, nil
		}
	}
	return nil, nil
}

// Holder of the task's live page, nil when the DevTools session holds it
func (b *Browser) Holder(cfg *config.Config, task string) *Page {
	for _, p := range b.Pages() {
		if p.Has != nil && p.Has(cfg, task) {
			return &p
		}
	}
	return nil
}

// Groups of places, in the words of their headings
const (
	Here     = "On this computer"
	InCloud  = "Cloud services"
	OwnTools = "Browsers with tools of their own"
)

// A place pages can open as the profile sees it: this computer, a cloud service or a browser holding pages of its own
type Place struct {
	Name, Title, About, Section string
	// Heading it is listed under, one of the groups of places
	Group string
	// Its state in the state words, and whether pages can open there now
	State string
	Ready bool
	// Pages open there now
	Chosen bool
	// Settings that keep its keys, outside its own section
	Keys []string
}

// States of a place pages open, in the words every page and command says them in
const (
	Ready     = "Ready"
	NeedsKey  = "Needs an API key"
	NeedsMore = "Needs setup"
)

// Every place pages can open for the profile, this computer first, each with how it stands
func (b *Browser) Places(cfg *config.Config) []Place {
	cloud, err := b.Cloud(cfg)
	chosen, picked := Chosen(cfg)
	inUse := func(name string) bool {
		switch {
		case picked:
			return chosen == name
		case cloud != nil:
			return cloud.Name == name
		}
		return name == browserconfig.Local && err == nil
	}
	places := []Place{{Name: browserconfig.Local, Title: "Local browser", About: "A browser on this computer", Group: Here, State: Ready,
		Ready: true, Chosen: inUse(browserconfig.Local)}}
	for _, p := range b.Providers() {
		place := Place{Name: p.Name, Title: p.Title, About: p.About, Section: p.Section, Group: InCloud, State: Ready, Ready: true,
			Chosen: inUse(p.Name)}
		if p.Available != nil && !p.Available(cfg) {
			place.State, place.Ready = NeedsMore, false
			for _, key := range p.Keys {
				if strings.TrimSpace(cfg.Variable(key.Env)) == "" && (key.Setting == "" || strings.TrimSpace(cfg.GetString(key.Setting)) == "") {
					place.State = NeedsKey
					break
				}
			}
		}
		for _, key := range p.Keys {
			if key.Setting != "" && !strings.HasPrefix(key.Setting, p.Section+".") {
				place.Keys = append(place.Keys, key.Setting)
			}
		}
		places = append(places, place)
	}
	for _, p := range b.Pages() {
		places = append(places, Place{Name: p.Name, Title: p.Title, About: "Pages open in " + p.Title + ", which brings tools of its own",
			Section: p.Section, Group: OwnTools, State: Ready, Ready: true, Chosen: inUse(p.Name)})
	}
	return places
}
