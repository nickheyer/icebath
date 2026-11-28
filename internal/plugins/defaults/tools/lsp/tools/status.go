package tools

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	lspconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/lsp/config"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// One client of the service
type ClientStatus struct {
	Server  string
	Root    string
	Folders []string
	State   string
	Running bool
}

// How one server stands for a profile, the same whether code is checked or not
type ServerState struct {
	ID          string
	Description string
	Extensions  []string
	// One of the state words
	State string
	// Where its program is, empty when it is not there
	Path string
	// What the pane says about installing it: that icebath installs it the first time a file needs it, or how to install it yourself; empty
	// once it is there
	Install string
	// Enter installs it: it is not there, and icebath has a way to install it
	Installable bool
	// A server a person added, and the command it runs as a shell line
	Added   bool
	Command string
	// Starts in a folder the person has not trusted
	UntrustedSafe bool
	// Folders it runs in, failed in, and was kept out of for not being trusted
	Running, Failed, Skipped []string
	// When a failed server is tried again, in words; empty for not until icebath restarts
	Retry string
}

// How the language servers of a profile stand: whether code is checked and how, every server it knows, the folders servers were kept out
// of and the companion tools missing
type Report struct {
	Checking      bool
	Auto          bool
	WaitMode      string
	WaitTimeout   float64
	WarmupTimeout float64
	RetrySeconds  float64
	Servers       []ServerState
	// Folders servers were kept out of for not being trusted, each once
	Skipped         []string
	Excludes        []string
	ExcludesInvalid bool
	Trusted         []string
	Clients         []ClientStatus
	// Server and root pairs that failed and are not tried again yet, and those kept out of a folder the person has not trusted
	Broken, Untrusted [][2]string
	Disabled          []string
	Warnings          []string
}

// Clients of the service, failures not tried again yet, and servers kept out of folders not trusted
func (s *service) snapshot() (clients []ClientStatus, broken, untrusted [][2]string) {
	s.mu.Lock()
	live := slices.Collect(maps.Values(s.clients))
	var failed, kept []key
	for k, f := range s.broken {
		if f.forever || time.Now().Before(f.until) {
			failed = append(failed, k)
		}
	}
	for k := range s.untrusted {
		kept = append(kept, k)
	}
	s.mu.Unlock()
	for _, c := range live {
		state, folders, isRunning := c.snapshot()
		clients = append(clients, ClientStatus{Server: c.server, Root: c.root, Folders: folders, State: state, Running: isRunning})
	}
	slices.SortFunc(clients, func(a, b ClientStatus) int { return strings.Compare(a.Server+"\x00"+a.Root, b.Server+"\x00"+b.Root) })
	for _, k := range sortedKeys(failed) {
		broken = append(broken, [2]string{k.server, k.root})
	}
	for _, k := range sortedKeys(kept) {
		if !s.trusted(k.root) {
			untrusted = append(untrusted, [2]string{k.server, k.root})
		}
	}
	return clients, broken, untrusted
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
		return which(fsx.ExpandHome(srv.program)), nil
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

// Whether icebath installs the server by itself: a built in one whose recipe is not one to install by hand
func automatic(srv *server) bool {
	r, ok := recipes[recipeKey(srv.id)]
	return ok && !srv.custom && r.strategy != manualInstall
}

// How the server gets installed, for one that is not there: icebath does it the first time a file needs it, or does it when asked, or the
// person does it as the server's own entry says, which names the program to install
func (s *service) advice(srv *server) string {
	switch {
	case srv.custom && srv.program == "":
		return "It has no command yet. Give it the command that starts it."
	case srv.custom:
		return "Install " + srv.program + " yourself, since icebath can't install a server you added."
	case automatic(srv) && s.settings.auto:
		return "icebath installs it the first time a file needs it."
	case automatic(srv):
		return "icebath installs it with " + installers[recipes[recipeKey(srv.id)].strategy] + "."
	case srv.install != "":
		return srv.install
	}
	return "Install " + srv.names[0] + " yourself, so that icebath finds it on your PATH."
}

// Words of the tool each way of installing runs
var installers = map[string]string{npmInstall: "npm", goInstall: "Go", pipInstall: "pip"}

// How one server stands, from what the service holds of it now
func (m *Manager) stand(s *service, srv *server, clients []ClientStatus, broken, untrusted [][2]string) (ServerState, error) {
	path, err := m.locate(s, srv)
	if err != nil {
		return ServerState{}, err
	}
	st := ServerState{ID: srv.id, Description: srv.about, Extensions: slices.Clone(srv.extensions), Path: path, Added: srv.custom,
		UntrustedSafe: untrustedSafe[srv.id]}
	if srv.custom {
		st.Command = strs.Quote(s.settings.commands[srv.id])
	}
	errored := false
	for _, c := range clients {
		if c.Server != srv.id {
			continue
		}
		switch {
		case c.Running || c.State == starting:
			folders := c.Folders
			if len(folders) == 0 {
				folders = []string{c.Root}
			}
			st.Running = append(st.Running, folders...)
		case c.State == failed:
			errored = true
			st.Failed = append(st.Failed, c.Root)
		}
	}
	for _, pair := range broken {
		if pair[0] == srv.id && !slices.Contains(st.Failed, pair[1]) {
			st.Failed = append(st.Failed, pair[1])
		}
	}
	for _, pair := range untrusted {
		if pair[0] == srv.id {
			st.Skipped = append(st.Skipped, pair[1])
		}
	}
	if len(st.Failed) > 0 && s.settings.retry > 0 {
		st.Retry = strs.Span(duration(s.settings.retry))
	}
	switch {
	case s.settings.disabled[srv.id]:
		st.State = lspconfig.Off
	case len(st.Failed) > 0 || errored:
		st.State = lspconfig.Failed
	case len(st.Running) > 0:
		st.State = lspconfig.Running
	case path != "":
		st.State = lspconfig.Installed
	default:
		st.State = lspconfig.NotInstalled
	}
	if path == "" {
		st.Install, st.Installable = s.advice(srv), automatic(srv)
	}
	return st, nil
}

// The one answer to how the language servers of a profile stand, whether code is checked or not: every server it knows, the folders they
// were kept out of, and the companion tools missing
func (m *Manager) Report(cfg *config.Config) (Report, error) {
	s := m.service(cfg)
	trusted, _ := tools.TrustedFolders(s.cfg)
	report := Report{Checking: s.settings.enabled, Auto: s.settings.auto, WaitMode: s.settings.waitMode, WaitTimeout: s.settings.wait,
		WarmupTimeout: s.settings.warmup, RetrySeconds: s.settings.retry, Excludes: slices.Clone(s.settings.excludes),
		ExcludesInvalid: s.settings.excludeInvalid, Trusted: trusted, Disabled: slices.Sorted(maps.Keys(s.settings.disabled))}
	report.Clients, report.Broken, report.Untrusted = s.snapshot()
	for _, pair := range report.Untrusted {
		if !slices.Contains(report.Skipped, pair[1]) {
			report.Skipped = append(report.Skipped, pair[1])
		}
	}
	for _, srv := range s.known() {
		st, err := m.stand(s, srv, report.Clients, report.Broken, report.Untrusted)
		if err != nil {
			return Report{}, err
		}
		report.Servers = append(report.Servers, st)
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

// Installs a server a person asked for, the lines of the install handed to out as they come; what it came to, or why no server of the id
// can be installed
func (m *Manager) Install(ctx context.Context, cfg *config.Config, id string, out func(text string)) (Outcome, error) {
	s := m.service(cfg)
	srv := s.byID(id)
	if srv == nil {
		return Outcome{}, fmt.Errorf("No language server is called “%s”. Run icebath lsp list to see their names.", id)
	}
	return gather([]attempt{m.installOne(ctx, s, srv, out)}), nil
}

// Installs each server of the profile not installed that icebath can install, the lines of the installs handed to out as they come; what
// they came to, with what every other server not installed needs
func (m *Manager) InstallAll(ctx context.Context, cfg *config.Config, out func(text string)) Outcome {
	s := m.service(cfg)
	var results []attempt
	for _, srv := range s.known() {
		if ctx.Err() != nil {
			break
		}
		if path, err := m.locate(s, srv); err == nil && path != "" {
			continue
		}
		results = append(results, m.installOne(ctx, s, srv, out))
	}
	return gather(results)
}

// What an install of servers came to: those installed and the sentence that names them, then each thing the others need, said once with
// the servers it holds back
type Outcome struct {
	Installed []Installed
	// The servers installed by name, those that were there already, or that every server is; empty when none was
	Done  string
	Needs []Need
}

// A server an install left installed, where its program is, and whether it was there before
type Installed struct {
	Server, Path string
	Already      bool
}

// One thing servers that could not be installed need, said once
type Need struct {
	// Servers it holds back, in the order they were tried
	Servers []string
	// What they need and what to do, as one sentence that names them
	Text string
	// Setting that gives it another way and the value that does, empty for none
	Setting, Value string
	// The one server it holds back is installed by hand, as the text says
	ByHand bool
}

// What trying to install one server came to: installed with where and whether it was there before, or what stopped it
type attempt struct {
	server  string
	path    string
	already bool
	lack    *lack
}

// The one grouping of what an install came to, which the page and the command line both show: the servers installed, then each distinct
// thing the others need with the servers it holds back, in the order they were tried
func gather(results []attempt) Outcome {
	var outcome Outcome
	var lacks []lack
	held := map[lack][]string{}
	for _, r := range results {
		if r.lack == nil {
			outcome.Installed = append(outcome.Installed, Installed{Server: r.server, Path: r.path, Already: r.already})
			continue
		}
		if _, seen := held[*r.lack]; !seen {
			lacks = append(lacks, *r.lack)
		}
		held[*r.lack] = append(held[*r.lack], r.server)
	}
	for _, l := range lacks {
		outcome.Needs = append(outcome.Needs, Need{Servers: held[l], Text: l.text(held[l]), Setting: l.setting, Value: l.value, ByHand: l.named})
	}
	var installed, there []string
	for _, done := range outcome.Installed {
		if done.Already {
			there = append(there, done.Server)
		} else {
			installed = append(installed, done.Server)
		}
	}
	var said []string
	if len(installed) > 0 {
		said = append(said, "Installed "+strs.And(installed)+".")
	}
	switch {
	case len(there) == 1:
		said = append(said, there[0]+" is installed already.")
	case len(there) > 1:
		said = append(said, strs.And(there)+" are installed already.")
	case len(results) == 0:
		said = append(said, "Every language server is installed already.")
	}
	outcome.Done = strings.Join(said, " ")
	return outcome
}

// Installs one server when icebath can, the lines of the install handed to out as they come, saying what it came to
func (m *Manager) installOne(ctx context.Context, s *service, srv *server, out func(text string)) attempt {
	done := attempt{server: srv.id}
	path, err := m.locate(s, srv)
	switch {
	case err != nil:
		done.lack = &lack{say: strs.Full(err.Error())}
		return done
	case path != "":
		done.path, done.already = path, true
		return done
	case !automatic(srv):
		done.lack = &lack{say: s.advice(srv), named: true}
		return done
	}
	out("Installing " + srv.id + "…\n")
	// What stopped the install, told once the result says it, while the steps it took scroll as they come
	var stopped *lack
	ctx = withSay(withLack(ctx, func(l lack) { stopped = &l }), func(text string) { out(text + "\n") })
	path, err = m.install(ctx, s.cfg, recipeKey(srv.id), true, true)
	switch {
	case err != nil:
		done.lack = &lack{say: strs.Full(err.Error())}
	case path == "" && stopped != nil:
		done.lack = stopped
	case path == "":
		done.lack = &lack{say: "The installer said nothing about why."}
	default:
		done.path = path
	}
	return done
}
