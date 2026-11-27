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

// One key a cloud service reads, by the variable that holds it
type Key struct {
	Env, Prompt, URL string
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
