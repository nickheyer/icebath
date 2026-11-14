package env

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/model/secret"
	"github.com/nickheyer/icebath/internal/proc"
	"github.com/nickheyer/icebath/internal/tool"
)

type ClientEnv interface {
	ToolchainEnviron(ctx context.Context, base map[string]string) map[string]string
}

type Delegation interface {
	ChildEnv(ctx context.Context, env map[string]string) map[string]string
}

type DesktopEnv interface {
	PublishedEnv(ctx context.Context) (map[string]string, error)
}

type Toolchain interface {
	StagedBash(ctx context.Context) (string, error)
	UserFirstPath(ctx context.Context, path string) (string, error)
	RuntimeDirs(ctx context.Context) ([]string, error)
}

type HostOptions struct {
	Config      *config.Store
	Homes       *home.Resolver
	Secrets     *secret.Scope
	Policy      *Policy
	Passthrough *Passthrough
	ClientEnv   ClientEnv
	Delegation  Delegation
	Desktop     DesktopEnv
	Toolchain   Toolchain
	Guard       *proc.Guard
}

type Host struct {
	cfg         *config.Store
	homes       *home.Resolver
	secrets     *secret.Scope
	policy      *Policy
	passthrough *Passthrough
	client      ClientEnv
	delegation  Delegation
	desktop     DesktopEnv
	toolchain   Toolchain
	guard       *proc.Guard

	binOnce   sync.Once
	binDir    string
	bashOnce  sync.Once
	bashDirs  []string
	pruneOnce sync.Once
}

func NewHost(o HostOptions) (*Host, error) {
	switch {
	case o.Config == nil:
		return nil, &tool.UnfilledError{Name: "env.HostOptions.Config"}
	case o.Homes == nil:
		return nil, &tool.UnfilledError{Name: "env.HostOptions.Homes"}
	case o.Secrets == nil:
		return nil, &tool.UnfilledError{Name: "env.HostOptions.Secrets"}
	case o.Policy == nil:
		return nil, &tool.UnfilledError{Name: "env.HostOptions.Policy"}
	case o.Passthrough == nil:
		return nil, &tool.UnfilledError{Name: "env.HostOptions.Passthrough"}
	case o.ClientEnv == nil:
		return nil, &tool.UnfilledError{Name: "env.ClientEnv"}
	case o.Delegation == nil:
		return nil, &tool.UnfilledError{Name: "env.Delegation"}
	case o.Desktop == nil:
		return nil, &tool.UnfilledError{Name: "env.DesktopEnv"}
	case o.Toolchain == nil:
		return nil, &tool.UnfilledError{Name: "env.Toolchain"}
	case o.Guard == nil:
		return nil, &tool.UnfilledError{Name: "env.HostOptions.Guard"}
	}
	return &Host{cfg: o.Config, homes: o.Homes, secrets: o.Secrets, policy: o.Policy, passthrough: o.Passthrough,
		client: o.ClientEnv, delegation: o.Delegation, desktop: o.Desktop, toolchain: o.Toolchain, guard: o.Guard}, nil
}

func (h *Host) Policy() *Policy { return h.policy }

func (h *Host) Passthrough() *Passthrough { return h.passthrough }

func Environ() map[string]string {
	out := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		if windowsHost {
			k = strings.ToUpper(k)
		}
		out[k] = v
	}
	return out
}

func EnvList(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k + "=" + env[k]
	}
	return out
}

func cloneEnv(env map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = v
	}
	return out
}

func (h *Host) applyProfileHome(ctx context.Context, env map[string]string) {
	if v := home.Override(ctx); v != "" {
		env[home.EnvHome] = v
	}
	h.homes.ApplySubprocessHomeEnv(ctx, env)
}

func (h *Host) injectSessionContext(ctx context.Context, env map[string]string) {
	vars, engaged := h.policy.session.Vars(ctx)
	for _, v := range vars {
		switch {
		case v.Bound:
			env[v.Name] = v.Value
		case engaged:
			delete(env, v.Name)
		}
	}
}

func applyMsysDefaults(env map[string]string) {
	if !windowsHost {
		return
	}
	if _, ok := env["MSYS_NO_PATHCONV"]; !ok {
		env["MSYS_NO_PATHCONV"] = "1"
	}
	if _, ok := env["MSYS2_ARG_CONV_EXCL"]; !ok {
		env["MSYS2_ARG_CONV_EXCL"] = "*"
	}
}

func (h *Host) finalize(ctx context.Context, env map[string]string) map[string]string {
	h.applyProfileHome(ctx, env)
	h.injectSessionContext(ctx, env)
	StripSupervisedIdentity(env)
	applyMsysDefaults(env)
	return h.delegation.ChildEnv(ctx, env)
}

type envPart struct {
	items  map[string]string
	unwrap bool
}

func (h *Host) filterSecretEnv(ctx context.Context, part envPart, out map[string]string, pluginStrip, registered map[string]bool) error {
	keys := make([]string, 0, len(part.items))
	var forced []string
	for k := range part.items {
		if strings.HasPrefix(k, ForcePrefix) {
			forced = append(forced, k)
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sort.Strings(forced)
	for _, key := range keys {
		value := part.items[key]
		if IsInternalSecret(key) || pluginStrip[strings.ToUpper(key)] {
			continue
		}
		firstParty := h.policy.terminalFirstParty(ctx, key)
		passthrough, err := h.passthrough.Allowed(ctx, key)
		if err != nil {
			return err
		}
		blocked, err := h.policy.ProviderBlocklisted(ctx, key, registered)
		if err != nil {
			return err
		}
		if blocked && !(passthrough || firstParty) {
			continue
		}
		resolved := &value
		if passthrough && !firstParty {
			if resolved, err = h.passthrough.Resolve(ctx, key, &value); err != nil {
				return err
			}
		}
		if resolved != nil {
			out[key] = *resolved
		}
	}
	if !part.unwrap {
		return nil
	}
	for _, key := range forced {
		name := key[len(ForcePrefix):]
		if !IsInternalSecret(name) {
			out[name] = part.items[key]
		}
	}
	return nil
}

func pathEnvKey(env map[string]string) (string, bool) {
	if !windowsHost {
		return "PATH", true
	}
	if _, ok := env["PATH"]; ok {
		return "PATH", true
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.ToUpper(k) == "PATH" {
			return k, true
		}
	}
	return "", false
}

func (h *Host) scrubbedEnv(ctx context.Context, parts []envPart, pluginStrip map[string]bool, fixPath func(string) (string, error)) (map[string]string, error) {
	folded := map[string]bool{}
	for k := range pluginStrip {
		folded[strings.ToUpper(k)] = true
	}
	registered, err := h.policy.RegisteredAdapterSecrets(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, part := range parts {
		if err := h.filterSecretEnv(ctx, part, out, folded, registered); err != nil {
			return nil, err
		}
	}
	additions, err := h.passthrough.ScopedAdditions(ctx, out)
	if err != nil {
		return nil, err
	}
	for k, v := range additions {
		if !pluginStrip[k] {
			out[k] = v
		}
	}
	if key, ok := pathEnvKey(out); ok {
		fixed, err := fixPath(out[key])
		if err != nil {
			return nil, err
		}
		out[key] = h.prependBinDir(fixed)
	}
	return h.finalize(ctx, out), nil
}

func (h *Host) Sanitize(ctx context.Context, base, extra map[string]string) (map[string]string, error) {
	return h.scrubbedEnv(ctx, []envPart{{base, false}, {extra, true}}, h.policy.pluginStripKeys(),
		func(p string) (string, error) { return p, nil })
}

func (h *Host) ScrubCredentials(ctx context.Context, env map[string]string, inheritCredentials bool) error {
	homeSecrets, err := h.policy.homeAdapterSecrets(ctx)
	if err != nil {
		return err
	}
	strip := h.policy.alwaysStripFolded()
	for k := range h.policy.pluginStripKeys() {
		strip[strings.ToUpper(k)] = true
	}
	for k := range homeSecrets {
		strip[k] = true
	}
	registered, err := h.policy.registryAdapterSecrets(ctx)
	if err != nil {
		return err
	}
	for key := range env {
		drop := strip[strings.ToUpper(key)] || strings.HasPrefix(key, ForcePrefix) || IsInternalSecret(key)
		if !drop && !inheritCredentials {
			if drop, err = h.policy.ProviderBlocklisted(ctx, key, registered); err != nil {
				return err
			}
		}
		if drop {
			delete(env, key)
		}
	}
	return nil
}

func (h *Host) Subprocess(ctx context.Context, inheritCredentials bool, base map[string]string) (map[string]string, error) {
	env := Environ()
	if base != nil {
		env = cloneEnv(base)
	}
	if err := h.ScrubCredentials(ctx, env, inheritCredentials); err != nil {
		return nil, err
	}
	if _, ok := env["PYTHONUTF8"]; !ok {
		env["PYTHONUTF8"] = "1"
	}
	return h.finalize(ctx, env), nil
}

type BuildOptions struct {
	Base               map[string]string
	KeepSecrets        bool
	NoProfileHome      bool
	Extra              map[string]string
	StripLaunchProfile bool
}

func (h *Host) Build(ctx context.Context, o BuildOptions) (map[string]string, error) {
	env := Environ()
	if o.Base != nil {
		env = cloneEnv(o.Base)
	}
	if o.StripLaunchProfile {
		if err := h.StripLaunchProfile(ctx, env, ""); err != nil {
			return nil, err
		}
		if h.secrets.Multiplexed(ctx) {
			current, _ := h.secrets.Current(ctx)
			for k, v := range current {
				env[k] = v
			}
			h.RestoreManaged(env)
		}
	}
	if !o.KeepSecrets {
		return h.Sanitize(ctx, env, o.Extra)
	}
	if !o.NoProfileHome {
		h.applyProfileHome(ctx, env)
	}
	for k, v := range o.Extra {
		env[k] = v
	}
	StripSupervisedIdentity(env)
	return h.delegation.ChildEnv(ctx, env), nil
}

func (h *Host) ServedProfile(ctx context.Context, base map[string]string, targetHome string, inheritCredentials bool) (map[string]string, error) {
	var env map[string]string
	if base != nil {
		env = cloneEnv(base)
	} else {
		var err error
		if env, err = h.Subprocess(ctx, inheritCredentials, nil); err != nil {
			return nil, err
		}
	}
	target := targetHome
	if target == "" {
		target = home.Override(ctx)
	}
	if target != "" {
		env[home.EnvHome] = target
		h.homes.ApplyScratchTmpEnv(env)
		if RoutedHome(target) {
			if err := h.StripLaunchProfile(ctx, env, target); err != nil {
				return nil, err
			}
			if err := h.ScrubCredentials(ctx, env, false); err != nil {
				return nil, err
			}
		}
	}
	if !inheritCredentials {
		return env, nil
	}
	var secrets map[string]string
	if target != "" {
		var err error
		if secrets, err = h.secrets.Build(ctx, h.cfg.Env(), target); err != nil {
			return nil, err
		}
	} else {
		current, ok := h.secrets.Current(ctx)
		if !ok && h.secrets.Multiplexed(ctx) {
			return nil, fmt.Errorf("%w: a served profile child env inheriting credentials was requested with no target home "+
				"and no profile secret scope bound while multiplexing is on; the child would inherit the "+
				"launch profile's credentials. Bind the profile scope (or pass the target home) at the spawn site", secret.ErrUnscoped)
		}
		secrets = current
	}
	for k, v := range secrets {
		env[k] = v
	}
	return env, nil
}

func (h *Host) HostGateway(ctx context.Context, base map[string]string) (map[string]string, error) {
	root, err := home.DefaultRoot()
	if err != nil {
		return nil, err
	}
	return h.ServedProfile(ctx, base, root, true)
}

func RoutedHome(target string) bool {
	launch, err := home.RoutingHome()
	if err != nil {
		return true
	}
	a, err := filepath.Abs(target)
	if err != nil {
		return true
	}
	b, err := filepath.Abs(launch)
	if err != nil {
		return true
	}
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		a = ra
	}
	if rb, err := filepath.EvalSymlinks(b); err == nil {
		b = rb
	}
	if windowsHost {
		return !strings.EqualFold(a, b)
	}
	return a != b
}

func (h *Host) StripLaunchProfile(ctx context.Context, env map[string]string, targetHome string) error {
	target := targetHome
	if target == "" {
		target = home.Override(ctx)
	}
	if target == "" || !RoutedHome(target) {
		return nil
	}
	launch, err := home.RoutingHome()
	if err != nil {
		return err
	}
	cfgEnv := h.cfg.Env()
	managed := map[string]bool{}
	for _, k := range cfgEnv.ManagedKeys() {
		managed[strings.ToUpper(k)] = true
	}
	backend, _, err := h.cfg.Backend(ctx)
	if err != nil {
		return err
	}
	for _, k := range backend.DeploymentEnvNames() {
		managed[k] = true
	}
	launchFile, err := cfgEnv.File(home.WithOverride(ctx, launch))
	if err != nil {
		return err
	}
	candidates := make([]string, 0, len(launchFile))
	for k := range launchFile {
		candidates = append(candidates, k)
	}
	candidates = append(candidates, cfgEnv.LoadedKeys()...)
	candidates = append(candidates, config.TerminalEnvVars()...)
	candidates = append(candidates, cfgEnv.SourceSuppliedNames()...)
	residue := map[string]bool{}
	for _, k := range candidates {
		up := strings.ToUpper(k)
		if (!config.IsGlobalEnv(up) || strings.HasPrefix(up, "TERMINAL_")) && !managed[up] {
			residue[up] = true
		}
	}
	for k := range env {
		if residue[strings.ToUpper(k)] {
			delete(env, k)
		}
	}
	h.policy.StripProfileGateEnv(ctx, env)
	return nil
}

func (h *Host) RestoreManaged(env map[string]string) {
	for _, k := range h.cfg.Env().ManagedKeys() {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
}

func (h *Host) RunEnv(ctx context.Context, overrides map[string]string) (map[string]string, error) {
	base := Environ()
	if err := h.StripLaunchProfile(ctx, base, ""); err != nil {
		return nil, err
	}
	merged := h.client.ToolchainEnviron(ctx, base)
	for k, v := range overrides {
		merged[k] = v
	}
	run, err := h.scrubbedEnv(ctx, []envPart{{merged, true}}, map[string]bool{}, func(p string) (string, error) {
		return h.userToolsFirst(ctx, h.appendMissingSanePath(ctx, p))
	})
	if err != nil {
		return nil, err
	}
	published, err := h.desktop.PublishedEnv(ctx)
	if err != nil {
		slog.Debug("Bot Desktop environment unavailable for the terminal", "err", err)
		published = nil
	}
	if len(published) > 0 {
		for k, v := range published {
			run[k] = v
		}
		delete(run, "WAYLAND_DISPLAY")
	}
	return run, nil
}

var ErrNoPath = errors.New("the child environment has no PATH to reorder")

func (h *Host) TerminalEnv(ctx context.Context, env map[string]string) (map[string]string, error) {
	key, ok := pathEnvKey(env)
	if !ok {
		return env, nil
	}
	current, present := env[key]
	if !present {
		return nil, ErrNoPath
	}
	fixed, err := h.userToolsFirst(ctx, current)
	if err != nil {
		return nil, err
	}
	env[key] = fixed
	return env, nil
}
