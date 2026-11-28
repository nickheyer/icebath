package web

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	webconfig "github.com/nickheyer/icebath/internal/web/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

// What a backend is asked to do
type Capability string

const (
	Searching Capability = "search"
	Reading   Capability = "extract"
)

// One search result
type Hit struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
	Position    int    `json:"position"`
}

// Answer of a search, as the model reads it
type Results struct {
	Success bool   `json:"success"`
	Data    *Data  `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
}

type Data struct {
	Web []Hit `json:"web"`
	// Free backend that answered in place of the one the search started at
	ServedBy string `json:"served_by,omitempty"`
	// Backend that failed the search the free ring then answered
	RescuedFrom string `json:"rescued_from,omitempty"`
	// What the failed backend said, and that the next search goes to it again
	BackendError string `json:"backend_error,omitempty"`
}

func Found(hits []Hit) *Results {
	return &Results{Success: true, Data: &Data{Web: append([]Hit{}, hits...)}}
}

func Failed(message string) *Results {
	return &Results{Error: message}
}

// Independent copy
func (r *Results) Clone() *Results {
	out := *r
	if r.Data != nil {
		data := *r.Data
		data.Web = slices.Clone(r.Data.Web)
		out.Data = &data
	}
	return &out
}

// Data block, made when there is none
func (r *Results) data() *Data {
	if r.Data == nil {
		r.Data = &Data{Web: []Hit{}}
	}
	return r.Data
}

// One page read, or why it could not be
type Page struct {
	URL     string
	Title   string
	Content string
	Error   string
	// URL that was asked for, when the page reports the one it ended at after redirects
	Source string
	// Rule of the website blocklist that refused the page
	Blocked *Block
}

func Document(url, title, content string) *Page {
	return &Page{URL: url, Title: title, Content: content}
}

func Unread(url, message string) *Page {
	return &Page{URL: url, Error: message}
}

// The same failure for every URL
func Unreadable(urls []string, message string) []*Page {
	pages := make([]*Page, len(urls))
	for i, url := range urls {
		pages[i] = Unread(url, message)
	}
	return pages
}

// One way of using a backend offered where backends are picked
type Plan struct {
	Title string
	// Price and needs in a few words, such as "free · no key"
	Badge string
	About string
	// Plan it pins in web.provider_tier, free or paid, none when empty
	Tier string
	// Setting of the key it asks for, where to get one and what to ask
	Key    string
	Prompt string
	Link   string
	// Kind of provider whose sign-in it runs on, empty for none
	SignIn string
}

// Free plan of a backend that the free ring goes round: anonymous search and page reading
type Free struct {
	Search  func(ctx context.Context, query string, limit int) (*Results, error)
	Extract func(ctx context.Context, urls []string) ([]*Page, error)
	// Whether the backend's own calls go to its free plan with the settings
	Used func(cfg *config.Config) bool
	// Whether an error of the ring stops the backend's call as it is, rather than coming back as the backend's own failed search or
	// pages
	Raises bool
}

// Reading of document files sent to a backend, such as a PDF whose pages have no text layer
type Parser struct {
	// Whether it can read them with the settings, its key set
	Ready func(cfg *config.Config) bool
	// Markdown of the file
	Parse func(ctx context.Context, cfg *config.Config, data []byte, filename string) (string, error)
}

// A service that answers web searches, reads pages, or both
type Backend struct {
	// As web.backend names it
	Name  string
	Title string
	// What it is and what it needs
	About string
	// Settings a person sets to use it, such as its key and its address
	Settings []string
	Plans    []Plan
	// Search returns the answer, or an error that stops the call. A backend that cannot search leaves it nil
	Search func(ctx context.Context, cfg *config.Config, query string, limit int) (*Results, error)
	// Extract returns one page for each URL, or an error that failed them all. A backend that cannot read pages leaves it nil
	Extract func(ctx context.Context, cfg *config.Config, urls []string, format string) ([]*Page, error)
	// Whether it is set up, read without the network: its key or address is set, or it is chosen where choosing is enough
	Ready func(cfg *config.Config) bool
	// Whether it answers without a key on a free plan of its own
	Keyless bool
	Free    *Free
	Parser  *Parser
	// Never picked on its own, only chosen by name: its credential is there for other features too
	Explicit bool
	// Place among several set up backends when none is chosen, lowest first so paid ones win over free ones. Zero takes no place
	Rank int
	// Used when nothing else is set up or free, its errors then saying how to set one up
	Fallback bool
}

func (b Backend) serves(capability Capability) bool {
	if capability == Searching {
		return b.Search != nil
	}
	return b.Extract != nil
}

// Whether it is set up
func (b Backend) Set(cfg *config.Config) bool {
	return b.Ready != nil && b.Ready(cfg)
}

// Whether it can answer on a free plan with the settings
func (b Backend) FreeReady(cfg *config.Config) bool {
	return (b.Keyless || b.Free != nil) && FreeOn(cfg) && Tier(cfg, b.Name) != webconfig.Paid
}

type Registrant interface {
	Backends() []Backend
}

type Registry struct {
	registrants []Registrant
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

// Every backend, in the order they were registered
func (r *Registry) Backends() []Backend {
	var backends []Backend
	for _, registrant := range r.registrants {
		backends = append(backends, registrant.Backends()...)
	}
	return backends
}

func (r *Registry) Backend(name string) (Backend, bool) {
	name = normal(name)
	for _, backend := range r.Backends() {
		if backend.Name == name {
			return backend, true
		}
	}
	return Backend{}, false
}

// Reader of document files of the first backend that can read them, when hosted reading is on; nil when none can
func (r *Registry) Parse(cfg *config.Config) func(ctx context.Context, data []byte, filename string) (string, error) {
	if !cfg.GetBool(webconfig.HostedParsing) {
		return nil
	}
	for _, backend := range r.Backends() {
		if parser := backend.Parser; parser != nil && parser.Ready(cfg) {
			return func(ctx context.Context, data []byte, filename string) (string, error) {
				return parser.Parse(ctx, cfg, data, filename)
			}
		}
	}
	return nil
}

// Name of the backend that serves the capability: the one pinned for it, else the one chosen for both, else the first set up, else a
// free one, else the fallback. Empty when there is none at all
func (r *Registry) Choice(cfg *config.Config, capability Capability) string {
	if name := r.Settled(cfg, capability); name != "" {
		return name
	}
	if name := r.free(cfg); name != "" {
		return name
	}
	for _, backend := range r.Backends() {
		if backend.Fallback {
			return backend.Name
		}
	}
	return ""
}

// Name of the backend the settings or what is set up give the capability: the one pinned for it, else the one chosen for both, else
// the first set up. Empty when it is left to the free ring
func (r *Registry) Settled(cfg *config.Config, capability Capability) string {
	if chosen := r.Named(cfg, capability); chosen != "" {
		return chosen
	}
	for _, backend := range r.Backends() {
		if !backend.Explicit && backend.Set(cfg) {
			return backend.Name
		}
	}
	return ""
}

// Backends of the free ring that can answer now, the ring entered at its start
func (r *Registry) Rotation(cfg *config.Config) []Backend {
	if !FreeOn(cfg) {
		return nil
	}
	return slices.DeleteFunc(r.ring(0), func(backend Backend) bool { return !backend.FreeReady(cfg) })
}

// First backend of the free ring that can answer, the ring entered at a random place so searches spread over them
func (r *Registry) free(cfg *config.Config) string {
	if !FreeOn(cfg) {
		return ""
	}
	for _, backend := range r.ring(-1) {
		if backend.FreeReady(cfg) {
			return backend.Name
		}
	}
	return ""
}

// Backend that answers the capability when it is asked for by what is set up rather than by name: the one chosen when it serves it, else
// the only one set up, else the first set up by rank, else a free one
func (r *Registry) Active(cfg *config.Config, capability Capability) (Backend, bool) {
	if chosen := r.Named(cfg, capability); chosen != "" {
		if backend, ok := r.Backend(chosen); ok && backend.serves(capability) {
			return backend, true
		}
	}
	var set []Backend
	for _, backend := range r.Backends() {
		if backend.serves(capability) && backend.Set(cfg) {
			set = append(set, backend)
		}
	}
	if len(set) == 1 {
		return set[0], true
	}
	ranked := slices.DeleteFunc(slices.Clone(set), func(backend Backend) bool { return backend.Rank == 0 })
	if len(ranked) > 0 {
		return slices.MinFunc(ranked, func(a, b Backend) int { return a.Rank - b.Rank }), true
	}
	if FreeOn(cfg) {
		for _, backend := range r.ring(-1) {
			if backend.serves(capability) && backend.FreeReady(cfg) {
				return backend, true
			}
		}
	}
	return Backend{}, false
}

// Backend chosen for the capability, pinned or for both, empty when none is
func (r *Registry) Named(cfg *config.Config, capability Capability) string {
	return cmp.Or(Chosen(cfg, pin(capability)), Chosen(cfg, webconfig.Backend))
}

// Whether some backend can answer: web_search and web_extract are offered only then
func (r *Registry) Available(cfg *config.Config) bool {
	if chosen := Chosen(cfg, webconfig.Backend); chosen != "" {
		if backend, ok := r.Backend(chosen); ok && backend.Set(cfg) {
			return true
		}
	}
	for _, backend := range r.Backends() {
		if !backend.Explicit && backend.Set(cfg) {
			return true
		}
	}
	for _, capability := range []Capability{Searching, Reading} {
		backend, ok := r.Active(cfg, capability)
		switch {
		case !ok:
		case backend.Explicit:
			if r.free(cfg) != "" {
				return true
			}
		case backend.Set(cfg) || backend.FreeReady(cfg):
			return true
		}
	}
	return false
}

// Backend that serves a call for the capability, or the error the model reads when none can
func (r *Registry) Serving(cfg *config.Config, capability Capability) (Backend, error) {
	name := r.Choice(cfg, capability)
	backend, ok := r.Backend(name)
	switch {
	case ok && backend.serves(capability):
		return backend, nil
	case ok && capability == Reading:
		return Backend{}, fmt.Errorf("%s is a search-only backend and cannot extract URL content. Set web.extract_backend to %s", backend.Title, r.readers())
	case !ok && name != "" && selected(cfg):
		return Backend{}, fmt.Errorf("web is configured to use '%s', but no registered web %s provider has that name. Pick another one with /web.", name, capability)
	}
	if backend, ok := r.Active(cfg, capability); ok {
		return backend, nil
	}
	if capability == Searching {
		return Backend{}, errors.New("No web search provider configured. Pick one with /web.")
	}
	return Backend{}, fmt.Errorf("No web extract provider configured. Set web.extract_backend to %s", r.readers())
}

// Names of the backends that read pages, as a sentence ending a list
func (r *Registry) readers() string {
	var names []string
	for _, backend := range r.Backends() {
		if backend.Extract != nil {
			names = append(names, backend.Name)
		}
	}
	if len(names) == 0 {
		return "a backend that reads pages."
	}
	return strs.Or(names) + "."
}

// Backends of the free ring in registration order, turned to start at the index given, at a random one for a negative index
func (r *Registry) ring(start int) []Backend {
	var ring []Backend
	for _, backend := range r.Backends() {
		if backend.Free != nil {
			ring = append(ring, backend)
		}
	}
	if len(ring) == 0 {
		return nil
	}
	if start < 0 {
		start = rand.IntN(len(ring))
	}
	return slices.Concat(ring[start:], ring[:start])
}

// Setting that pins a backend for the capability
func pin(capability Capability) string {
	if capability == Searching {
		return webconfig.SearchBackend
	}
	return webconfig.ExtractBackend
}

// Backend a web backend setting names, lower cased, empty when unset
func Chosen(cfg *config.Config, key string) string {
	return normal(cfg.GetString(key))
}

// Whether any web backend setting names the backend
func Picked(cfg *config.Config, name string) bool {
	for _, key := range []string{webconfig.Backend, webconfig.SearchBackend, webconfig.ExtractBackend} {
		if Chosen(cfg, key) == name {
			return true
		}
	}
	return false
}

// Whether a backend was ever chosen, for searching, reading or both
func selected(cfg *config.Config) bool {
	for _, key := range []string{webconfig.Backend, webconfig.SearchBackend, webconfig.ExtractBackend} {
		if Chosen(cfg, key) != "" {
			return true
		}
	}
	return false
}

func normal(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

type Web struct {
	Registry
}

func New(cfg *config.Config) *Web {
	w := &Web{}
	webconfig.Register(cfg, func(capability string) []config.Option {
		// Left empty, the main setting picks a service by the keys set and a pinned one follows the main setting
		options := []config.Option{{Value: "", Title: "Automatic", About: "The first service you have a key for, else a free one"}}
		if capability != "" {
			options = []config.Option{{Value: "", Title: "Main service", About: "The service that answers both searches and page reads"}}
		}
		for _, backend := range w.Backends() {
			if capability == "" || backend.serves(Capability(capability)) {
				options = append(options, config.Option{Value: backend.Name, Title: backend.Title, About: backend.About})
			}
		}
		return options
	})
	return w
}
