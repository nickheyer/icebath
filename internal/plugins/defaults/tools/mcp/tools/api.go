package tools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/mcp"
	mcpconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/mcp/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

// How one server of a profile stands, for the screens and commands
type Status struct {
	Name      string
	Transport string
	// connected, configured, connecting, failed, lazy, disabled or recycled
	State string
	Error string
	Tools int
	// Settings as kept
	Raw     map[string]any
	Enabled bool
	// Plugin that ships it, empty for one the profile sets up
	Plugin   string
	OAuth    bool
	SignedIn bool
	// Values its settings name as ${NAME}, and those of them that are not set
	Reads       []string
	Missing     []string
	Sampling    map[string]int
	Elicitation map[string]int
}

// Every server the profile has with how it stands, by name; nothing connects for it
func (m *Manager) Status(cfg *config.Config) []Status {
	p := m.pool(cfg)
	specs := p.specs()
	raw := table(p.cfg.Get(mcpconfig.Servers))
	for name := range raw {
		if _, ok := specs[name]; !ok {
			if entry := table(raw[name]); entry != nil {
				s := parseSpec(p.cfg, name, entry, p.workspaceDir())
				s.Enabled = false
				specs[name] = s
			}
		}
	}
	var out []Status
	for _, name := range slices.Sorted(maps.Keys(specs)) {
		s := specs[name]
		status := Status{Name: name, Transport: s.transportName(), State: "configured", Raw: s.Raw, Enabled: s.Enabled, Plugin: s.Plugin,
			OAuth: s.Auth == "oauth", SignedIn: s.Auth == "oauth" && p.store(name).HasTokens()}
		for path := range s.Missed {
			for _, m := range reference.FindAllStringSubmatch(fmt.Sprint(lookupPath(s.Raw, path)), -1) {
				if !slices.Contains(status.Missing, m[1]) {
					status.Missing = append(status.Missing, m[1])
				}
			}
		}
		sort.Strings(status.Missing)
		status.Reads = reads(s.Raw)
		p.mu.Lock()
		srv, connecting, failure := p.servers[name], p.connecting[name], p.failures[name]
		p.mu.Unlock()
		switch {
		case !s.Enabled:
			status.State = "disabled"
		case srv != nil:
			state, session, err := srv.snapshot()
			switch {
			case session != nil:
				status.State, status.Tools = "connected", len(p.serverDefs(srv))
			case state == stateLazy:
				status.State, status.Tools = "lazy", len(p.serverDefs(srv))
			case state == stateRecycled:
				status.State, status.Tools = "recycled", len(p.serverDefs(srv))
			case state == stateParked:
				status.State, status.Error = "failed", describe(cmp.Or(err, errors.New(failure)))
			default:
				status.State = "connecting"
			}
			srv.mu.Lock()
			if srv.sampler != nil {
				status.Sampling = srv.sampler.Metrics()
			}
			if srv.elicitor != nil {
				status.Elicitation = srv.elicitor.Metrics()
			}
			srv.mu.Unlock()
		case connecting:
			status.State = "connecting"
		case failure != "":
			status.State, status.Error = "failed", failure
		}
		out = append(out, status)
	}
	return out
}

// Value under a dotted path of a table, nil when there is none
func lookupPath(m map[string]any, path string) any {
	var v any = m
	for _, part := range strings.Split(strings.TrimPrefix(path, "."), ".") {
		name, at, indexed := strings.Cut(part, "[")
		next, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = next[name]
		if indexed {
			list, _ := v.([]any)
			var i int
			fmt.Sscanf(strings.TrimSuffix(at, "]"), "%d", &i)
			if i >= len(list) {
				return nil
			}
			v = list[i]
		}
	}
	return v
}

// What a reload changed
type Reload struct {
	Reconnected, Added, Removed, Failed []string
	Tools, Servers                      int
}

// Stops every server of the profile and connects those the settings now want again
func (m *Manager) Reload(ctx context.Context, cfg *config.Config) Reload {
	p := m.pool(cfg)
	p.mu.Lock()
	before := slices.Sorted(maps.Keys(p.servers))
	p.mu.Unlock()
	p.close()
	p.mu.Lock()
	p.cooldown, p.strikes, p.failures = map[string]time.Time{}, map[string]int{}, map[string]string{}
	p.run = nil
	p.mu.Unlock()
	run := p.discover("")
	select {
	case <-run.done:
	case <-ctx.Done():
	}
	var r Reload
	p.mu.Lock()
	var after []string
	for name, srv := range p.servers {
		if srv.available() {
			after = append(after, name)
		}
	}
	p.mu.Unlock()
	sort.Strings(after)
	for _, name := range after {
		if slices.Contains(before, name) {
			r.Reconnected = append(r.Reconnected, name)
		} else {
			r.Added = append(r.Added, name)
		}
	}
	for _, name := range before {
		if !slices.Contains(after, name) {
			r.Removed = append(r.Removed, name)
		}
	}
	r.Servers = len(after)
	r.Tools = len(p.definitions())
	for _, status := range m.Status(cfg) {
		if status.Enabled && (status.State == "failed" || status.State == "configured") {
			r.Failed = append(r.Failed, status.Name)
		}
	}
	return r
}

// Tool a probe found
type Summary struct {
	Name        string
	Description string
}

// What a probe found
type Probe struct {
	Tools []Summary
	// Prompts and resources the server lists, nil when it lists none or offers none
	Prompts, Resources *int
	// Characters of each tool's schema as the model sees it
	SchemaChars map[string]int
	// It answered the handshake
	Initialized bool
}

// Connects a server once with the settings given, lists what it offers and disconnects, registering nothing; a sign in it needs goes to the
// signer of the context
func (m *Manager) Probe(ctx context.Context, cfg *config.Config, name string, raw map[string]any, connectTimeout time.Duration, values map[string]string) (Probe, error) {
	if issues := validate(name, raw); len(issues) > 0 {
		return Probe{}, &ProbeError{Err: errors.New(strings.Join(issues, " ")), Class: "config_rejected"}
	}
	p := m.pool(cfg)
	s := parseSpecWith(p.cfg, name, raw, p.workspaceDir(), values)
	if connectTimeout > 0 {
		s.ConnectTimeout = connectTimeout
	} else if raw["connect_timeout"] == nil {
		s.ConnectTimeout = 30 * time.Second
	}
	srv := newServer(p, s)
	srv.probe = true
	connectCtx, cancel := context.WithTimeout(ctx, s.ConnectTimeout)
	defer cancel()
	err := srv.start(connectCtx)
	defer srv.shutdown()
	var result Probe
	srv.mu.Lock()
	result.Initialized = srv.result != nil
	srv.mu.Unlock()
	if err != nil {
		if errors.Is(connectCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			err = &slowConnect{fmt.Sprintf("Connecting to the MCP server %s took longer than %s, its time limit to connect, so it stopped. A sign-in "+
				"counts against the OAuth timeout as well.", name, strs.Span(s.ConnectTimeout))}
		}
		return result, &ProbeError{Err: errors.New(mcp.Redact(describe(err))), Class: failureClass(err), cause: err}
	}
	_, session, _ := srv.snapshot()
	listed := srv.offered()
	result.SchemaChars = map[string]int{}
	for _, t := range listed {
		description := []rune(t.Description)
		if len(description) > 80 {
			description = append(description[:77], []rune("...")...)
		}
		result.Tools = append(result.Tools, Summary{Name: t.Name, Description: string(description)})
		result.SchemaChars[t.Name] = len(fmt.Sprint(normalize(t.InputSchema))) + len(t.Description)
	}
	srv.mu.Lock()
	caps := srv.result.Capabilities
	srv.mu.Unlock()
	if s.Prompts && (caps == nil || caps.Prompts != nil) {
		if prompts, err := session.ListPrompts(connectCtx); err == nil {
			n := len(prompts)
			result.Prompts = &n
		}
	}
	if s.Resources && (caps == nil || caps.Resources != nil) {
		if resources, err := session.ListResources(connectCtx); err == nil {
			n := len(resources)
			result.Resources = &n
		}
	}
	return result, nil
}

// Failed probe: its message with credentials redacted, and the class of the failure
// A connect that ran past its time limit, said for a person and still a deadline to whoever checks
type slowConnect struct{ msg string }

func (e *slowConnect) Error() string { return e.msg }

func (e *slowConnect) Unwrap() error { return context.DeadlineExceeded }

type ProbeError struct {
	Err   error
	Class string
	cause error
}

func (e *ProbeError) Error() string { return e.Err.Error() }

func (e *ProbeError) Unwrap() error { return e.cause }

// Class of a failed probe: config_invalid, auth_required, server_start_failed or connect_failed
func failureClass(err error) string {
	switch {
	case errors.As(err, new(*mcp.InvalidURLError)):
		return "config_invalid"
	case mcp.AuthFailure(err), errors.As(err, new(*mcp.HTTPStatusError)) && (statusOf(err) == 401 || statusOf(err) == 403):
		return "auth_required"
	case missing(err) != "", errors.As(err, new(*abiError)):
		return "server_start_failed"
	case errors.As(err, new(*unsetError)):
		return "missing_credentials"
	}
	return "connect_failed"
}

func statusOf(err error) int {
	if status, ok := errors.AsType[*mcp.HTTPStatusError](err); ok {
		return status.Status
	}
	return 0
}

// The one command that fixes a failed probe: sign in, the missing command, or a check
func NextStep(name string, err error) string {
	switch class := failureClass(err); {
	case class == "auth_required":
		return "The server turned down the sign-in, so sign in to " + name + " again."
	case errors.As(err, new(*abiError)):
		return "Once it is rebuilt, test " + name + " again."
	case missing(err) != "" || strings.Contains(err.Error(), notFound):
		return "Install that program, or give its full path as the command of " + name + "."
	}
	return "Check that the server runs and that its address or command is right, then test " + name + " again."
}

// Sign in to a server in progress, reached by its id
type Signin struct {
	ID     string
	Server string
	Login  *mcp.Login
	// Closed once it ended; Err why it failed, nil when it succeeded
	Done  chan struct{}
	Err   error
	Tools int
	// The sign in ran, yet no token came back
	NoToken bool
}

// Signs in to a server again from scratch, in the browser or on another device, then reconnects it; the link waits on the sign in's Login
func (m *Manager) Login(cfg *config.Config, name, flow string) (*Signin, error) {
	p := m.pool(cfg)
	s, ok := p.spec(name)
	if !ok {
		return nil, fmt.Errorf("No MCP server is called “%s”. Run icebath mcp list to see yours.", name)
	}
	return m.loginWith(cfg, name, s.Raw, flow, nil)
}

// Signs in to a server of the settings given in place of the kept ones, for a server not saved yet
func (m *Manager) LoginWith(cfg *config.Config, name string, raw map[string]any, flow string) (*Signin, error) {
	return m.loginWith(cfg, name, raw, flow, nil)
}

// Signs in to a server of the settings given, which commit keeps once a token came; a sign in that ends without one undoes what it wrote
func (m *Manager) loginWith(cfg *config.Config, name string, raw map[string]any, flow string, commit func() error) (*Signin, error) {
	p := m.pool(cfg)
	s := parseSpec(p.cfg, name, raw, p.workspaceDir())
	if !s.remote() {
		return nil, fmt.Errorf("The MCP server %s runs on this computer and has no address, so it has no sign-in.", name)
	}
	if s.Auth != "oauth" {
		return nil, fmt.Errorf("The MCP server %s is reached at an address with no sign-in. Change how it connects to an address with sign-in "+
			"first.", name)
	}
	flow = cmp.Or(flow, s.OAuth.Flow)
	if flow != "browser" && flow != "device" {
		return nil, errors.New("The OAuth setting flow must be browser or device.")
	}
	signin := &Signin{ID: strs.Random(8), Server: name, Login: mcp.NewLogin(), Done: make(chan struct{})}
	m.logins.Store(signin.ID, signin)
	go func() {
		defer close(signin.Done)
		ctx, cancel := context.WithTimeout(context.Background(), max(s.ConnectTimeout, s.OAuth.Timeout+15*time.Second))
		defer cancel()
		go func() {
			select {
			case <-signin.Login.Ended():
				cancel()
			case <-ctx.Done():
			}
		}()
		signin.Err = m.signIn(ctx, p, s, raw, flow, signin, commit)
		if signin.Err != nil {
			signin.Login.Fail(signin.Err)
		}
		time.AfterFunc(10*time.Minute, func() { m.logins.Delete(signin.ID) })
	}()
	return signin, nil
}

func (m *Manager) signIn(ctx context.Context, p *pool, s *spec, raw map[string]any, flow string, signin *Signin, commit func() error) error {
	store := p.store(s.Name)
	snapshot := store.Snapshot()
	undo := func(err error) error {
		if commit != nil {
			if restore := store.Restore(snapshot); restore != nil {
				return errors.Join(err, restore)
			}
		}
		return err
	}
	p.forgetProvider(s.Name)
	if flow == "device" {
		provider, err := p.provider(s, mcp.HTTPOptions{URL: s.URL, Strict: s.Strict, SSLVerify: s.SSLVerify, ClientCert: s.ClientCert, ClientKey: s.ClientKey,
			ConnectTimeout: s.ConnectTimeout, Server: s.Name})
		if err != nil {
			return err
		}
		if err := provider.LoginDevice(ctx, signin.Login.ShowDevice); err != nil {
			return undo(err)
		}
	} else if err := store.Remove(true); err != nil {
		return fmt.Errorf("Could not forget the earlier sign-in of the server: %w", err)
	}
	p.forgetProvider(s.Name)
	probe, err := m.Probe(mcp.WithSigner(ctx, signin.Login), p.cfg, s.Name, raw, max(s.ConnectTimeout, s.OAuth.Timeout+15*time.Second), nil)
	if err != nil {
		if humanized, ok := mcp.HumanizeRegistration(s.Name, err.Error(), s.URL); ok {
			return undo(errors.New(humanized))
		}
		return undo(err)
	}
	signin.Tools = len(probe.Tools)
	if !store.HasTokens() {
		signin.NoToken = true
		return undo(errors.New("The server answered but gave no sign-in token, so the sign-in did not finish. Providers such as Google Drive and " +
			"Atlassian need an OAuth client that you create there, with its client_id and client_secret in the oauth part of the server's settings."))
	}
	if commit != nil {
		if err := commit(); err != nil {
			return undo(err)
		}
	}
	p.forgetProvider(s.Name)
	p.mu.Lock()
	srv := p.servers[s.Name]
	p.mu.Unlock()
	if srv == nil || !srv.signal() {
		p.discover("")
	}
	return nil
}

// Sign in of the id, false when there is none
func (m *Manager) Signin(id string) (*Signin, bool) {
	value, ok := m.logins.Load(id)
	if !ok {
		return nil, false
	}
	return value.(*Signin), true
}

// Names of the tools of the server the model sees now
func (m *Manager) ServerTools(cfg *config.Config, name string) []string {
	p := m.pool(cfg)
	var out []string
	for _, def := range p.definitions() {
		if def.Toolset == "mcp-"+name {
			out = append(out, def.Name)
		}
	}
	return out
}
