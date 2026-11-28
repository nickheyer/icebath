// The browser tools: browser_exec runs the model's Python against a real browser, browser_cdp and browser_dialog reach a browser attached
// over DevTools, browser_vault fills what the user saved into the page
package tools

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/internal/botdesktop"
	"github.com/nickheyer/icebath/internal/browser"
	browserconfig "github.com/nickheyer/icebath/internal/browser/config"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/packages"
	"github.com/nickheyer/icebath/internal/profile"
	sessionstore "github.com/nickheyer/icebath/internal/sessions"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/internal/tools/redact"
	"github.com/nickheyer/icebath/internal/vault"
)

type registrant struct {
	// Tools of the session, asked whether it is offered vision_analyze
	registry *tools.Registry
	config   *config.Config
	browser  *browser.Browser
	desktop  *botdesktop.Desktop
	terminal *terminal.Terminal
	vault    *vault.Vault
	packages *packages.Packages

	live        liveOverrides
	launcher    launcher
	lightpanda  lightpandas
	sessions    sessions
	sandbox     sandboxes
	supervisors supervisors
	execs       execSessions
	exec        execState
	real        realLocks
}

// The browser host of the plugin: what /browser and `icebath browser` reach
type Browser struct{ r *registrant }

func Register(registry *tools.Registry, stores *sessionstore.Registry, profiles *profile.Registry, cfg *config.Config, host *browser.Browser,
	desktop *botdesktop.Desktop, term *terminal.Terminal, safe *vault.Vault, pm *packages.Packages) *Browser {
	r := &registrant{registry: registry, config: cfg, browser: host, desktop: desktop, terminal: term, vault: safe, packages: pm,
		live: liveOverrides{}, launcher: newLauncher(), lightpanda: lightpandas{servers: map[string]*lightpandaServer{}, probed: map[string]bool{}},
		sessions: newSessions(), sandbox: newSandboxes(), supervisors: newSupervisors(), execs: newExecSessions(), exec: newExecState()}
	registry.Register(r)
	profiles.Register(r)
	stores.OnRelease(func(ctx context.Context, session string) { r.cleanupTask(ctx, r.config, session) })
	b := &Browser{r: r}
	// Every write of the address of your own browser a person asks for attaches to it, or with none disconnects, as the page of Browser does
	cfg.Guard(browserconfig.CDPURL, config.Guard{Write: func(cfg *config.Config, _ string, value any) (bool, error) {
		address, _ := value.(string)
		if strings.TrimSpace(address) == "" {
			_, _, err := b.Disconnect(context.Background(), cfg)
			return true, err
		}
		b.Attach(context.Background(), cfg, address)
		return false, nil
	}})
	return b
}

func (r *registrant) Tools() []tools.Def {
	attached := func(env *tools.Env) bool { return r.overrideRaw(env.Config) != "" }
	return []tools.Def{
		{
			Name:      "browser_exec",
			Toolset:   "browser-use",
			Title:     "Browser",
			About:     "Open web pages in a browser, click, type and fill in forms",
			Group:     "Browser",
			Icon:      tools.IconFetch,
			Subject:   "code",
			Schema:    r.execSchema(nil),
			Dynamic:   r.execSchema,
			Handler:   r.execute,
			Effect:    tools.Mutates,
			Fallible:  true,
			Untrusted: true,
			// timeout_s stops it, up to its own limit
			Timeout: -1,
		},
		{
			Name:      "browser_cdp",
			Toolset:   "browser-cdp",
			Title:     "Browser command",
			About:     "Send low-level commands to the browser you connected",
			Group:     "Browser",
			Icon:      tools.IconFetch,
			Subject:   "method",
			Schema:    cdpSchema(),
			Handler:   r.cdp,
			Check:     attached,
			Lacks:     unattached,
			Fallible:  true,
			Untrusted: true,
		},
		{
			Name:      "browser_dialog",
			Toolset:   "browser-cdp",
			Title:     "Page pop-up",
			About:     "Answer a pop-up that blocks a page in the browser you connected",
			Group:     "Browser",
			Icon:      tools.IconAsk,
			Subject:   "action",
			Schema:    dialogSchema(),
			Handler:   r.dialog,
			Check:     attached,
			Lacks:     unattached,
			Effect:    tools.Mutates,
			Untrusted: true,
		},
		{
			Name:    "browser_vault",
			Toolset: "browser",
			Title:   "Autofill",
			About:   "Fill your saved logins, codes and cards into a web page",
			Group:   "Browser",
			Icon:    tools.IconWrite,
			Subject: "action",
			Schema:  r.vaultSchema(nil),
			Dynamic: r.vaultSchema,
			Handler: r.vaultTool,
			Effect:  tools.Mutates,
		},
	}
}

// Closes the browsers of the turn's task as it ends, unless browser.headed keeps the window for someone watching
func (r *registrant) Ended(env *tools.Env, session string) {
	if session == "" || headed(env.Config) {
		return
	}
	r.cleanupTask(context.Background(), env.Config, session)
}

// Closes the deleted profile's browsers and forgets the values its fills wrote
func (r *registrant) Delete(name string) error {
	if r.config.Exists(name) {
		r.cleanupProfile(context.Background(), r.config.In(name))
	}
	r.live.set(name, "")
	redact.Forget(name)
	return nil
}

// Closes a profile's browsers before its folder moves to a new name
func (r *registrant) Stop(name string) error {
	if r.config.Exists(name) {
		r.cleanupProfile(context.Background(), r.config.In(name))
	}
	r.live.set(name, "")
	return nil
}

// Keeps scrubbing the values the vault filled for a renamed profile under its new name
func (r *registrant) Rename(old, renamed string) error {
	redact.Rename(old, renamed)
	return nil
}

// Closes every browser this process started and reaps what crashed runs left, as icebath stops
func (r *registrant) Close() error {
	r.emergency(context.Background())
	return nil
}

// Endpoint the profile drives over DevTools, empty when none is attached
func (b *Browser) Endpoint(cfg *config.Config) string { return b.r.overrideRaw(cfg) }

// Attaches the profile's browser tools to the endpoint, an empty one detaching them; the profile's browsers closed on each side of the change
func (b *Browser) Attach(ctx context.Context, cfg *config.Config, endpoint string) {
	b.r.cleanupProfile(ctx, cfg)
	b.r.live.set(cfg.Profile(), strings.TrimSpace(endpoint))
	b.r.cleanupProfile(ctx, cfg)
}

// Ends the browser holding the user's own profile, that of the browser named or else of the default one
func (b *Browser) CloseProfile(cfg *config.Config, name string) (bool, string) {
	return CloseProfile(cfg, name)
}

// What the browser tools that drive your own browser lack to run
func unattached(*tools.Env) string { return "It needs your own browser connected." }

// The group of the browser tools, whose options are the area of the browser
func (r *registrant) Groups() []tools.Group {
	return []tools.Group{{Title: "Browser", Area: browserconfig.Key, Order: 40}}
}
