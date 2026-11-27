package tools

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// One client of the service
type ClientStatus struct {
	Server  string
	Root    string
	Folders []string
	State   string
	Running bool
}

// How the service of a profile stands
type ServiceStatus struct {
	Enabled         bool
	WaitMode        string
	WaitTimeout     float64
	InstallStrategy string
	Clients         []ClientStatus
	// Server and root pairs that failed and are not tried again yet
	Broken          [][2]string
	Disabled        []string
	RetrySeconds    float64
	WarmupTimeout   float64
	Excludes        []string
	ExcludesInvalid bool
	Trusted         []string
	// Server and root pairs kept out of a workspace the user has not trusted
	Untrusted [][2]string
}

// One server the service knows
type ServerStatus struct {
	ID          string
	Extensions  []string
	Description string
	// installed, missing or manual-only
	Status string
	// Where its program is, empty when it is not there
	Path string
	// Starts in a workspace the user has not trusted
	UntrustedSafe bool
	Custom        bool
	// Key of its install recipe, empty for a server without one, and whether the recipe installs by itself
	Recipe    string
	Automatic bool
}

// The service of a profile, nil when language servers are off, every server it knows and the companion tools missing
type Report struct {
	Service  *ServiceStatus
	Servers  []ServerStatus
	Warnings []string
}

func (s *service) status() *ServiceStatus {
	folders, _ := tools.TrustedFolders(s.cfg)
	st := &ServiceStatus{Enabled: s.settings.enabled, WaitMode: s.settings.waitMode, WaitTimeout: s.settings.wait, InstallStrategy: s.settings.strategy,
		RetrySeconds: s.settings.retry, WarmupTimeout: s.settings.warmup, ExcludesInvalid: s.settings.excludeInvalid,
		Excludes: slices.Clone(s.settings.excludes), Trusted: folders, Clients: []ClientStatus{}, Broken: [][2]string{}, Untrusted: [][2]string{},
		Disabled: slices.Sorted(maps.Keys(s.settings.disabled))}
	if st.Disabled == nil {
		st.Disabled = []string{}
	}
	s.mu.Lock()
	clients := slices.Collect(maps.Values(s.clients))
	var broken, untrusted []key
	for k, f := range s.broken {
		if f.forever || time.Now().Before(f.until) {
			broken = append(broken, k)
		}
	}
	for k := range s.untrusted {
		untrusted = append(untrusted, k)
	}
	s.mu.Unlock()
	for _, c := range clients {
		state, folders, isRunning := c.snapshot()
		st.Clients = append(st.Clients, ClientStatus{Server: c.server, Root: c.root, Folders: folders, State: state, Running: isRunning})
	}
	slices.SortFunc(st.Clients, func(a, b ClientStatus) int { return strings.Compare(a.Server+"\x00"+a.Root, b.Server+"\x00"+b.Root) })
	for _, k := range sortedKeys(broken) {
		st.Broken = append(st.Broken, [2]string{k.server, k.root})
	}
	for _, k := range sortedKeys(untrusted) {
		if !s.trusted(k.root) {
			st.Untrusted = append(st.Untrusted, [2]string{k.server, k.root})
		}
	}
	return st
}

func sortedKeys(keys []key) []key {
	slices.SortFunc(keys, func(a, b key) int { return strings.Compare(a.server+"\x00"+a.root, b.server+"\x00"+b.root) })
	return keys
}

// Servers the settings declare ahead of the built in ones
func (s *service) known() []*server {
	return append(slices.Clone(s.customs), builtins...)
}

// Program a server runs: for a declared one its command; else the command the settings give when it is there, the binary of its recipe
// staged, in a Python environment or on PATH, then the names it goes by on PATH
func (m *Manager) locate(s *service, srv *server) (string, error) {
	if srv.custom {
		return which(terminal.ExpandUser(srv.program)), nil
	}
	if command := s.settings.commands[srv.id]; len(command) > 0 && command[0] != "" && fsx.Exists(command[0]) {
		return command[0], nil
	}
	found, err := m.existing(s.cfg, binOf(recipeKey(srv.id)))
	if err != nil || found != "" {
		return found, err
	}
	return which(srv.names...), nil
}

// How a server stands: installed, missing, or one that installs only by hand
func (m *Manager) state(s *service, srv *server) (string, string, error) {
	path, err := m.locate(s, srv)
	if err != nil {
		return "", "", err
	}
	if path != "" {
		return installed, path, nil
	}
	if srv.custom {
		return manualOnly, "", nil
	}
	if r, ok := recipes[recipeKey(srv.id)]; ok && r.strategy == manualInstall {
		return manualOnly, "", nil
	}
	return missing, "", nil
}

// The service of the profile and every server it knows
func (m *Manager) Report(cfg *config.Config) (Report, error) {
	s := m.service(cfg)
	report := Report{}
	if s.settings.enabled {
		report.Service = s.status()
	}
	for _, srv := range s.known() {
		state, path, err := m.state(s, srv)
		if err != nil {
			return Report{}, err
		}
		row := ServerStatus{ID: srv.id, Extensions: slices.Clone(srv.extensions), Description: srv.about, Status: state, Path: path,
			UntrustedSafe: untrustedSafe[srv.id], Custom: srv.custom}
		if r, ok := recipes[recipeKey(srv.id)]; ok && !srv.custom {
			row.Recipe, row.Automatic = recipeKey(srv.id), r.strategy != manualInstall
		}
		report.Servers = append(report.Servers, row)
	}
	warnings, err := m.warnings(s.cfg)
	if err != nil {
		return Report{}, err
	}
	report.Warnings = warnings
	return report, nil
}

// Server of the id the profile knows, nil for none
func (s *service) byID(id string) *server {
	for _, srv := range s.known() {
		if srv.id == id {
			return srv
		}
	}
	return nil
}

// Program a server runs from, empty when it is not there; an error for an id no server or recipe has
func (m *Manager) Which(cfg *config.Config, id string) (string, error) {
	s := m.service(cfg)
	if srv := s.byID(id); srv != nil {
		return m.locate(s, srv)
	}
	if _, ok := recipes[id]; ok {
		return m.existing(s.cfg, binOf(id))
	}
	return "", fmt.Errorf("No language server is called “%s”. Run icebath lsp list to see their names.", id)
}

// Installs a server a person asked for, its lines handed to out and errs; the exit status
func (m *Manager) Install(ctx context.Context, cfg *config.Config, id string, out, errs func(text string)) int {
	s := m.service(cfg)
	srv := s.byID(id)
	key := recipeKey(id)
	if _, ok := recipes[key]; srv == nil && !ok {
		errs("✗ No language server is called “" + id + "”. Run icebath lsp list to see their names.")
		return 2
	}
	if srv != nil {
		state, _, err := m.state(s, srv)
		if err != nil {
			errs(err.Error())
			return 1
		}
		if state == installed {
			out("✓ " + id + " is already installed.\n")
			return 0
		}
		if srv.custom {
			errs("✗ " + id + " is a server you declared in the settings, which runs the command you gave. Install " + srv.program + " yourself.")
			return 1
		}
	}
	out("Installing " + id + "…\n")
	path, err := m.install(withSay(ctx, errs), s.cfg, key, "auto", true)
	if err != nil {
		errs(err.Error())
		return 1
	}
	if path != "" {
		out("✓ Installed " + id + " at " + path + ".\n")
		return 0
	}
	if r, ok := recipes[key]; ok && r.strategy == manualInstall {
		errs("✗ " + id + " is not one that icebath can install. Install it yourself.")
	} else {
		errs("✗ Could not install " + id + ". The lines above or the log of icebath say why.")
	}
	return 1
}

// Installs every built in server whose recipe installs by itself, the manual ones too when asked; the exit status
func (m *Manager) InstallAll(ctx context.Context, cfg *config.Config, manual bool, out, errs func(text string)) int {
	s := m.service(cfg)
	code := 0
	for _, srv := range recipeServers(manual) {
		state, _, err := m.state(s, srv)
		if err != nil {
			errs(err.Error())
			return 1
		}
		if state == installed {
			out("✓ " + srv.id + " is already installed.\n")
			continue
		}
		key := recipeKey(srv.id)
		out("Installing " + srv.id + "…\n")
		path, err := m.install(withSay(ctx, errs), s.cfg, key, "auto", true)
		if err != nil {
			out("✗ Could not install " + srv.id + ".\n")
			errs(err.Error())
			code = 1
			continue
		}
		if path != "" {
			out("✓ Installed " + srv.id + " at " + path + ".\n")
		} else {
			out("✗ Could not install " + srv.id + ". The lines above or the log of icebath say why.\n")
			code = 1
		}
	}
	return code
}
