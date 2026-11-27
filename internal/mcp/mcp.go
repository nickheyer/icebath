// Package mcp speaks the Model Context Protocol: the client of a server over stdio, Streamable HTTP or SSE with its OAuth sign in, the
// server side icebath serves itself over, and the catalog of servers plugins offer for install
package mcp

import (
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
)

// Server icebath knows how to set up: one of the bundled catalog, of a folder of manifests, or one a plugin ships
type Entry struct {
	Name        string
	Description string
	// Page that documents it
	Source string
	Launch Launch
	Auth   Auth
	// Hosted connector it stands for, empty when none
	ConnectorSlug string
	Tools         ToolDefaults
	// Repository cloned for a server run from source, nil when none is
	Install *GitInstall
	// What the person reads once it is installed
	PostInstall string
	// Manifest it was read from, empty for one a plugin registered
	Manifest string
	// Plugin that ships it: such a server connects without being installed, unless the profile sets up one of the same name
	Plugin string
	// Why it can't be installed, empty when it can, and whether that is for needing a newer icebath
	Problem string
	Future  bool
}

// How a server is reached
type Launch struct {
	// stdio or http
	Type    string
	Command string
	Args    []string
	URL     string
	Env     map[string]string
}

// How a server signs in
type Auth struct {
	// none, api_key or oauth
	Type string
	// Values the person gives at install, kept for the server's settings to name as ${NAME}
	Env []EnvSpec
	// Provider of a third party sign in it relies on, empty for the server's own OAuth
	Provider string
	Scopes   []string
	// Settings of its oauth block
	OAuth map[string]any
}

// One value asked for at install
type EnvSpec struct {
	Name     string
	Prompt   string
	Required bool
	Secret   bool
	Default  string
}

// Tools turned on at install: only those listed, or all but those excluded
type ToolDefaults struct {
	Enabled  []string
	Excluded []string
}

// Repository cloned at a pinned ref, the commands run in it after
type GitInstall struct {
	URL       string
	Ref       string
	Bootstrap []string
}

// What adds servers to the catalog, read with the settings of the profile it is shown for
type Registrant interface {
	Catalog(cfg *config.Config) []Entry
}

type Registry struct {
	registrants []Registrant
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

// Every entry, those that can't be installed included
func (r *Registry) Catalog(cfg *config.Config) []Entry {
	var entries []Entry
	for _, registrant := range r.registrants {
		entries = append(entries, registrant.Catalog(cfg)...)
	}
	return entries
}

// Entries that can be installed, by name
func (r *Registry) Installable(cfg *config.Config) []Entry {
	entries := slices.DeleteFunc(r.Catalog(cfg), func(e Entry) bool { return e.Problem != "" })
	slices.SortFunc(entries, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
	return entries
}

// Entry that can be installed under the name, an official/ prefix allowed
func (r *Registry) Entry(cfg *config.Config, name string) (Entry, bool) {
	name = strings.TrimPrefix(strings.TrimSpace(name), "official/")
	for _, entry := range r.Installable(cfg) {
		if entry.Name == name {
			return entry, true
		}
	}
	return Entry{}, false
}

// Servers of the catalog plugins ship, which connect without being installed
func (r *Registry) Bundled(cfg *config.Config) []Entry {
	return slices.DeleteFunc(r.Installable(cfg), func(e Entry) bool { return e.Plugin == "" })
}

var keyChars = regexp.MustCompile(`[^A-Za-z0-9_]`)

// Name of the value holding a server's API key, MCP_<NAME>_API_KEY, which its Authorization header names
func KeyEnv(server string) string {
	return "MCP_" + strings.Trim(keyChars.ReplaceAllString(strings.ToUpper(server), "_"), "_") + "_API_KEY"
}

type MCP struct {
	Registry
}

func New() *MCP {
	return &MCP{}
}
