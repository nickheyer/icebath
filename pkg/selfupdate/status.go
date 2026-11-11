package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Shared state of one status run, the config, the deep flag and the Nous login facts later sections read
type statusRun struct {
	ctx                  context.Context
	h                    string
	w                    io.Writer
	c                    *cliui.Console
	deep                 bool
	config               map[string]any
	nousLoggedIn         bool
	nousInferencePresent bool
	nousAccountInfo      *provider.NousPortalAccountInfo
}

// Connected platforms and home channels of the gateway config the platform rows and summary read
type statusGatewayConfig struct {
	connected []gateway.Platform
	homes     map[string]*gateway.HomeChannel
}

var (
	statusLoadConfig = func(ctx context.Context, h string) (map[string]any, error) {
		cfg, err := config.LoadContext(ctx, h, nil)
		return map[string]any(cfg), err
	}
	statusResolveRequestedProvider = func(ctx context.Context, h string) (string, error) {
		return provider.ResolveRequestedProvider(ctx, h, "")
	}
	statusResolveProvider = func(ctx context.Context, h, requested string) (string, error) {
		return provider.ResolveAuthProvider(statusPoolEnv(ctx, h), requested)
	}
	statusProviderLabel = provider.ProviderLabel
	statusGetEnvValue   = func(ctx context.Context, h, name string) (string, error) {
		v, _, err := config.GetEnvValue(ctx, h, name)
		return v, err
	}
	statusGatewaySnapshot   = gatewaycli.GetGatewayRuntimeSnapshot
	statusLoadGatewayConfig = func(ctx context.Context) (*statusGatewayConfig, error) {
		cfg, err := gateway.LoadGatewayConfig(ctx)
		if err != nil {
			return nil, err
		}
		out := &statusGatewayConfig{connected: cfg.GetConnectedPlatforms(ctx), homes: map[string]*gateway.HomeChannel{}}
		for _, p := range cfg.PlatformKeys() {
			if pc := cfg.Platforms[p]; pc != nil && pc.HomeChannel != nil {
				out.homes[string(p)] = pc.HomeChannel
			}
		}
		return out, nil
	}
	statusPluginEntries = func(ctx context.Context) []*gateway.PlatformEntry { return gateway.Registry.PluginEntries(ctx) }
	statusListProviders = provider.ListAuthenticatedProviders
)

var statusTerminalEnvRows = map[string][]struct {
	label, env, def string
	emptyIsUnset    bool
}{
	"ssh":     {{"SSH Host:", "TERMINAL_SSH_HOST", "(not set)", true}, {"SSH User:", "TERMINAL_SSH_USER", "(not set)", true}},
	"docker":  {{"Docker Image:", "TERMINAL_DOCKER_IMAGE", config.DefaultSandboxImage, false}},
	"daytona": {{"Daytona Image:", "TERMINAL_DAYTONA_IMAGE", config.DefaultSandboxImage, false}},
}

var statusGatewayFallback = map[string][2]string{"termux": {"unknown", "Termux / manual process"},
	"linux": {"unknown", "systemd/manual"}, "darwin": {"unknown", "launchd"}}

// Pool environment of the status home bound to the run context
func statusPoolEnv(ctx context.Context, h string) provider.PoolEnv {
	env := provider.HomePoolEnv(h)
	env.Ctx = ctx
	return env
}

// Green check or red cross for a status row
func (s *statusRun) checkMark(ok bool) string {
	if ok {
		return s.c.Color("✓", cliui.Green)
	}
	return s.c.Color("✗", cliui.Red)
}

func (s *statusRun) println(parts ...string) { fmt.Fprintln(s.w, strings.Join(parts, "")) }

// Blank line then a bold cyan diamond section heading
func (s *statusRun) section(title string) {
	s.println()
	s.println(s.c.Color("◆ "+title, cliui.Cyan, cliui.Bold))
}

// One name, mark and text row with the name padded to width
func (s *statusRun) row(name string, ok bool, text string, width int, sep string) {
	s.println("  ", pystr.Ljust(name, width), sep, s.checkMark(ok), " ", text)
}

// Indented label and value detail line under a row
func (s *statusRun) detail(label string, value any) { s.kvIndent(label, value, "    ", 12) }

func (s *statusRun) kv(label string, value any) { s.kvIndent(label, value, "  ", 14) }

func (s *statusRun) kvIndent(label string, value any, indent string, width int) {
	s.println(indent, pystr.Ljust(label, width), pyrepr.Str(value))
}

// Label with a mark followed by the on or off text
func (s *statusRun) kvFlag(label string, ok bool, on, off string) {
	text := off
	if ok {
		text = on
	}
	s.kv(label, s.checkMark(ok)+" "+text)
}

func statusConfigured(ok bool) string {
	if ok {
		return "configured"
	}
	return "not configured"
}

// First nonempty env value among the names, empty when none is set
func (s *statusRun) firstEnvValue(names ...string) (string, error) {
	for _, n := range names {
		v, err := statusGetEnvValue(s.ctx, s.h, n)
		if err != nil {
			return "", err
		}
		if v != "" {
			return v, nil
		}
	}
	return "", nil
}

// Mapping view of a config value, false for anything but a dict
func statusDict(v any) (map[string]any, bool) {
	switch t := v.(type) {
	case map[string]any:
		return t, true
	case config.Tree:
		return t, true
	case *pyjson.Object:
		return t.Vals, true
	}
	return nil, false
}

// Configured default model of a config, or the not set marker
func statusConfiguredModelLabel(cfg map[string]any) string {
	modelCfg := cfg["model"]
	if m, ok := statusDict(modelCfg); ok {
		modelCfg = statusOr(m["default"], m["name"], "")
	}
	if text, ok := modelCfg.(string); ok && pystr.Strip(text) != "" {
		return pystr.Strip(text)
	}
	return "(not set)"
}

// First truthy value, the last value when none is
func statusOr(values ...any) any {
	for _, v := range values[:len(values)-1] {
		if pyrepr.Truthy(v) {
			return v
		}
	}
	return values[len(values)-1]
}

// Provider label of the runtime resolution, custom when an OpenRouter resolution has a custom endpoint
func (s *statusRun) effectiveProviderLabel() (string, error) {
	requested, err := statusResolveRequestedProvider(s.ctx, s.h)
	if err != nil {
		return "", err
	}
	effective, err := statusResolveProvider(s.ctx, s.h, requested)
	if err != nil {
		var authErr *provider.AuthError
		if !errors.As(err, &authErr) {
			return "", err
		}
		effective = requested
		if effective == "" {
			effective = "auto"
		}
	}
	if effective == "openrouter" {
		var modelCfg any
		if cfg, err := statusLoadConfig(s.ctx, s.h); err == nil {
			modelCfg = cfg["model"]
		}
		configBaseURL := ""
		if m, ok := statusDict(modelCfg); ok {
			raw := statusOr(m["base_url"], "")
			text, isStr := raw.(string)
			if !isStr {
				return "", fmt.Errorf("AttributeError: '%s' object has no attribute 'strip'", pyrepr.TypeName(raw))
			}
			configBaseURL = pystr.Strip(text)
		}
		envBase, err := statusGetEnvValue(s.ctx, s.h, "OPENAI_BASE_URL")
		if err != nil {
			return "", err
		}
		if configBaseURL != "" || envBase != "" {
			effective = "custom"
		}
	}
	return statusProviderLabel(effective), nil
}

// One line pause banner when the global emergency stop is engaged, empty otherwise
func statusEstopLine(ctx context.Context) string {
	st := tools.GetState(ctx)
	if st == nil {
		return ""
	}
	reason := ""
	if st.Reason != "" {
		reason = " — reason: " + st.Reason
	}
	return "⏸️  PAUSED (global emergency stop" + reason + "; `icebath resume` to lift)"
}

// Blank line then each line in the styles
func (s *statusRun) banner(lines []string, styles ...string) {
	s.println()
	for _, line := range lines {
		s.println(s.c.Color(line, styles...))
	}
}

func (s *statusRun) renderHeader() error {
	s.banner([]string{"┌─────────────────────────────────────────────────────────┐",
		"│                 ☤ Icebath Agent Status                  │",
		"└─────────────────────────────────────────────────────────┘"}, cliui.Cyan)
	if paused := statusEstopLine(s.ctx); paused != "" {
		s.banner([]string{paused}, cliui.Yellow, cliui.Bold)
	}
	return nil
}

// Loads the config into the run, an empty config when it cannot be read
func (s *statusRun) loadConfig() {
	cfg, err := statusLoadConfig(s.ctx, s.h)
	if err != nil || cfg == nil {
		cfg = map[string]any{}
	}
	s.config = cfg
}

func (s *statusRun) renderEnvironment() error {
	s.section("Environment")
	s.kv("Project:", config.GetProjectRoot())
	s.kv("Python:", runtime.Version())
	_, err := os.Stat(home.EnvPath(s.h))
	s.kvFlag(".env file:", err == nil, "exists", "not found")
	s.loadConfig()
	s.kv("Model:", statusConfiguredModelLabel(s.config))
	label, err := s.effectiveProviderLabel()
	if err != nil {
		return err
	}
	s.kv("Provider:", label)
	return nil
}

func (s *statusRun) renderTerminal() error {
	s.section("Terminal Backend")
	terminalCfg, ok := statusDict(s.config["terminal"])
	if !ok {
		terminalCfg = map[string]any{}
	}
	var terminalEnv any = os.Getenv("TERMINAL_ENV")
	if terminalEnv == "" {
		terminalEnv = "local"
		if v, has := terminalCfg["backend"]; has {
			terminalEnv = v
		}
	}
	s.kv("Backend:", terminalEnv)
	switch terminalEnv.(type) {
	case map[string]any, config.Tree, *pyjson.Object, []any:
		return fmt.Errorf("TypeError: unhashable type: '%s'", pyrepr.TypeName(terminalEnv))
	}
	name, isStr := terminalEnv.(string)
	rows, tabled := statusTerminalEnvRows[name]
	switch {
	case isStr && tabled:
		for _, r := range rows {
			value, set := os.LookupEnv(r.env)
			if r.emptyIsUnset && value == "" || !r.emptyIsUnset && !set {
				value = r.def
			}
			s.println("  ", pystr.Ljust(r.label, 13), " ", value)
		}
	case isStr && name == "vercel_sandbox":
		persistEnabled := true
		if persist, set := os.LookupEnv("TERMINAL_CONTAINER_PERSISTENT"); set {
			lower := strings.ToLower(persist)
			persistEnabled = lower == "1" || lower == "true" || lower == "yes" || lower == "on"
		} else if v, has := terminalCfg["container_persistent"]; has {
			persistEnabled = pyrepr.Truthy(v)
		}
		auth := tools.DescribeVercelAuth(nil)
		image := statusOr(os.Getenv("TERMINAL_VERCEL_RUNTIME"), terminalCfg["vercel_runtime"], os.Getenv("TERMINAL_VERCEL_IMAGE"),
			terminalCfg["vercel_image"], config.DefaultVercelImage)
		s.kv("Image:", image)
		s.kvFlag("SDK:", tools.GetTerminalBackendScoped(s.ctx, "vercel_sandbox", "") != nil, "installed",
			"missing (run icebath setup terminal and select Vercel Sandbox, then restart Icebath)")
		s.kv("Auth:", s.checkMark(auth.OK)+" "+auth.Label)
		for _, line := range auth.DetailLines {
			s.kv("Auth detail:", line)
		}
		if persistEnabled {
			s.kv("Persistence:", "snapshot filesystem")
		} else {
			s.kv("Persistence:", "ephemeral filesystem")
		}
		s.kv("Processes:", "live processes do not survive cleanup, snapshots, or sandbox recreation")
	default:
		s.renderPluginBackend(terminalEnv)
	}
	s.kvFlag("Sudo:", os.Getenv("SUDO_PASSWORD") != "", "enabled", "disabled")
	return nil
}

// Doctor rows of a plugin registered terminal backend, nothing when discovery or the provider fails
func (s *statusRun) renderPluginBackend(terminalEnv any) {
	defer func() { _ = recover() }()
	plugins.DiscoverPlugins(s.ctx, false)
	name, ok := terminalEnv.(string)
	if !ok {
		return
	}
	backend := tools.PluginTerminalBackend(s.ctx, name, tools.ScopeKey(s.h))
	if backend == nil {
		return
	}
	for _, c := range backend.DoctorChecks() {
		s.println("  ", c.Label, ": ", s.checkMark(c.OK), " ", c.Detail)
	}
}

// One row per plugin platform, judged by the gateway the same way the summary platform line is
func (s *statusRun) renderPlatforms() error {
	s.section("Messaging Platforms")
	var cfg *statusGatewayConfig
	var entries []*gateway.PlatformEntry
	if failed := statusRecover(func() error {
		plugins.DiscoverPlugins(s.ctx, false)
		var err error
		cfg, err = statusLoadGatewayConfig(s.ctx)
		return err
	}); failed {
		return nil
	}
	connected := map[string]bool{}
	for _, p := range cfg.connected {
		connected[string(p)] = true
	}
	labels := map[string]string{}
	values := []string{}
	entries = statusPluginEntries(s.ctx)
	for _, e := range entries {
		if _, seen := labels[e.Name]; !seen {
			values = append(values, e.Name)
		}
		labels[e.Name] = e.Label
	}
	sort.SliceStable(values, func(i, j int) bool {
		return strings.ToLower(labels[values[i]]) < strings.ToLower(labels[values[j]])
	})
	for _, value := range values {
		text := statusConfigured(connected[value])
		if hc := cfg.homes[value]; hc != nil {
			text += " (home: " + hc.ChatID + ")"
		}
		s.row(labels[value], connected[value], text, 12, "  ")
	}
	return nil
}

// Runs fn reporting whether it returned an error or panicked
func statusRecover(fn func() error) (failed bool) {
	defer func() {
		if recover() != nil {
			failed = true
		}
	}()
	return fn() != nil
}

func (s *statusRun) renderGateway() error {
	s.section("Gateway Service")
	if statusRecover(s.renderGatewayRows) {
		platform := runtime.GOOS
		fallback, ok := statusGatewayFallback[platform]
		if !ok {
			fallback = [2]string{"N/A", "(not supported on this platform)"}
		}
		s.kv("Status:", s.c.Color(fallback[0], cliui.Dim))
		s.kv("Manager:", fallback[1])
	}
	return nil
}

func (s *statusRun) renderGatewayRows() error {
	snapshot, err := statusGatewaySnapshot(s.ctx, false)
	if err != nil {
		return err
	}
	if !snapshot.Running() {
		served, err := gateway.NamedProfileServedByRunningMultiplexerFor(s.ctx, nil)
		if err != nil {
			return err
		}
		if served {
			s.kvFlag("Status:", true, "running (via the default-profile multiplexer)", "stopped")
			s.kv("Manage with:", "icebath gateway status   # from the default profile")
			return nil
		}
	}
	s.kvFlag("Status:", snapshot.Running(), "running", "stopped")
	s.kv("Manager:", snapshot.Manager)
	if len(snapshot.GatewayPIDs) > 0 {
		s.kv("PID(s):", gatewaycli.FormatGatewayPids(snapshot.GatewayPIDs, 3))
	}
	if snapshot.Running() {
		served, err := gateway.MultiplexerServedSecondaries()
		if err != nil {
			return err
		}
		if len(served) > 0 {
			s.kv("Serves:", strings.Join(served, ", "))
			urls, err := gateway.ServedProfileIngressURLs("")
			if err != nil {
				return err
			}
			names := append([]string{}, urls.Keys...)
			sort.Strings(names)
			for _, name := range names {
				perPlatform, ok := pyjson.AsObject(urls.Vals[name])
				if !ok {
					return fmt.Errorf("AttributeError: '%s' object has no attribute 'items'", pyrepr.TypeName(urls.Vals[name]))
				}
				platforms := append([]string{}, perPlatform.Keys...)
				sort.Strings(platforms)
				for _, platform := range platforms {
					s.kv("  "+name+"/"+platform+":", perPlatform.Vals[platform])
				}
			}
		}
	}
	if snapshot.HasProcessServiceMismatch() {
		s.kv("Service:", "installed but not managing the current running gateway")
	} else if snapshot.ServiceInstalled && !snapshot.ServiceRunning {
		s.kv("Service:", "installed but stopped")
	}
	return nil
}

// Elements a Python for loop yields over a decoded JSON value, false when it is not iterable
func statusIterate(v any) ([]any, bool) {
	switch t := v.(type) {
	case []any:
		return t, true
	case *pyjson.Object:
		out := make([]any, len(t.Keys))
		for i, k := range t.Keys {
			out[i] = k
		}
		return out, true
	case string:
		out := []any{}
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, true
	}
	return nil, false
}

// Active and total count of the cron jobs file, zero when absent and an error marker when unreadable
func statusCronSummary(h string) string {
	jobsFile := filepath.Join(h, "cron", "jobs.json")
	if _, err := os.Stat(jobsFile); err != nil {
		return "0"
	}
	const unreadable = "(error reading jobs file)"
	doc, err := statusLoadJSON(jobsFile)
	if err != nil {
		return unreadable
	}
	obj, ok := pyjson.AsObject(doc)
	if !ok {
		return unreadable
	}
	var jobs any = []any{}
	if raw, has := obj.Vals["jobs"]; has {
		jobs = raw
	}
	items, ok := statusIterate(jobs)
	if !ok {
		return unreadable
	}
	active := 0
	for _, item := range items {
		job, ok := pyjson.AsObject(item)
		if !ok {
			return unreadable
		}
		enabled, has := job.Vals["enabled"]
		if !has || pyrepr.Truthy(enabled) {
			active++
		}
	}
	return fmt.Sprintf("%d active, %d total", active, len(items))
}

// JSON document of a utf-8-sig file
func statusLoadJSON(path string) (any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return nil, err
	}
	return pyjson.Loads(text)
}

func (s *statusRun) renderCron() error {
	s.section("Scheduled Jobs")
	s.kv("Jobs:", statusCronSummary(s.h))
	return nil
}

// Open gateway sessions of the read only session database, none when it cannot be opened or read
func statusGatewayRows(ctx context.Context, h string) (rows []*pyjson.Object) {
	defer func() {
		if recover() != nil {
			rows = nil
		}
	}()
	db, err := state.Open(filepath.Join(h, "state.db"), state.Options{ReadOnly: true})
	if err != nil {
		return nil
	}
	defer db.Close()
	rows, err = db.ListGatewaySessions(ctx, "", true)
	if err != nil {
		return nil
	}
	return rows
}

func (s *statusRun) renderSessions() error {
	s.section("Sessions")
	gatewayRows := statusGatewayRows(s.ctx, s.h)
	sessionsFile := filepath.Join(s.h, "sessions", "sessions.json")
	if len(gatewayRows) > 0 {
		s.kv("Active:", fmt.Sprintf("%d session(s)", len(gatewayRows)))
		freshest := 0.0
		for _, r := range gatewayRows {
			v, err := pyint.Float(statusOr(r.Vals["last_active"], 0))
			if err != nil {
				return err
			}
			if v > freshest {
				freshest = v
			}
		}
		if freshest > 0 {
			s.println("  Last activity:", pystr.Rjust(pydatetime.RelativeTime(freshest, "", time.Now()), 13))
		}
	} else if _, err := os.Stat(sessionsFile); err != nil {
		s.kv("Active:", 0)
	} else if data, err := statusLoadJSON(sessionsFile); err != nil {
		s.kv("Active:", "(error reading sessions file)")
	} else {
		entries := 0
		if obj, ok := pyjson.AsObject(data); ok {
			for _, k := range obj.Keys {
				if !strings.HasPrefix(k, "_") {
					entries++
				}
			}
		}
		s.kv("Active:", fmt.Sprintf("%d session(s)", entries))
	}
	var capacity *big.Int
	if statusRecover(func() error { capacity = activesessions.ResolveMaxConcurrentSessions(s.config); return nil }) {
		capacity = nil
	}
	if capacity == nil {
		return nil
	}
	var held []*pyjson.Object
	if statusRecover(func() error {
		var err error
		held, err = activesessions.RegistrySnapshot(s.ctx, s.h, false)
		return err
	}) {
		held = nil
	}
	style := cliui.Green
	if big.NewInt(int64(len(held))).Cmp(capacity) >= 0 {
		style = cliui.Yellow
	}
	s.kv("Slots:", s.c.Color(fmt.Sprintf("%d/%s in use", len(held), capacity.String()), style))
	now := float64(time.Now().UnixNano()) / 1e9
	startedAt := func(e *pyjson.Object) (float64, error) { return pyint.Float(statusOr(e.Vals["started_at"], 0)) }
	keys := make([]float64, len(held))
	for i, e := range held {
		k, err := startedAt(e)
		if err != nil {
			return err
		}
		keys[i] = k
	}
	order := make([]int, len(held))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return keys[order[a]] < keys[order[b]] })
	for _, i := range order {
		entry := held[i]
		started, err := pyint.Float(statusOr(entry.Vals["started_at"], now))
		if err != nil {
			return err
		}
		age := activesessions.FormatAge(now - started)
		s.println("                ", pystr.Ljust(pyrepr.Str(statusOr(entry.Vals["surface"], "unknown")), 17), " ",
			pystr.Ljust(pyrepr.Str(statusOr(entry.Vals["session_id"], "?")), 24), " ", age)
	}
	return nil
}

func (s *statusRun) renderDeep() error {
	if !s.deep {
		return nil
	}
	s.section("Deep Checks")
	if key := os.Getenv("OPENROUTER_API_KEY"); key != "" {
		req, err := http.NewRequestWithContext(s.ctx, http.MethodGet, home.OpenRouterModelsURL, nil)
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+key)
			var resp *pyhttpx.Response
			resp, err = pyhttpx.Do(pyhttpx.PlainClient(false), req, 10*time.Second)
			if err == nil {
				s.kvFlag("OpenRouter:", resp.StatusCode == 200, "reachable", fmt.Sprintf("error (%d)", resp.StatusCode))
			}
		}
		if err != nil {
			s.kv("OpenRouter:", s.checkMark(false)+" error: "+pyhttpx.ErrorText(err))
		}
	}
	conn, err := net.DialTimeout("tcp", "127.0.0.1:18789", time.Second)
	portInUse := err == nil
	if conn != nil {
		conn.Close()
	}
	if portInUse {
		s.kv("Port 18789:", "in use")
	} else {
		s.kv("Port 18789:", "available")
	}
	return nil
}

func (s *statusRun) renderFooter() error {
	s.banner([]string{strings.Repeat("─", 60), "  Run 'icebath doctor' for detailed diagnostics", "  Run 'icebath setup' to configure"},
		cliui.Dim)
	s.println()
	return nil
}

// Labels of the platforms the gateway would start, the registry label when one is registered
func (s *statusRun) connectedPlatformLabels() ([]string, error) {
	cfg, err := statusLoadGatewayConfig(s.ctx)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, p := range cfg.connected {
		label := string(p)
		if e := gateway.Registry.Get(s.ctx, string(p)); e != nil {
			label = e.Label
		}
		out = append(out, label)
	}
	return out, nil
}

// Providers the model picker offers from cached catalogs, no endpoint probes
func (s *statusRun) authenticatedProviderNames() ([]string, error) {
	pick, err := provider.LoadPickerContext(s.ctx)
	if err != nil {
		return nil, err
	}
	zero := 0
	rows, err := statusListProviders(s.ctx, provider.AuthenticatedProvidersOptions{UserProviders: pick.UserProviders,
		CustomProviders: pick.CustomProviders, ExcludedProviders: pick.ExcludedProviders, MaxModels: &zero,
		NonBlockingCatalogs: true, SkipCustomProviderProbe: true})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		name, has := row["name"]
		if !has {
			return nil, fmt.Errorf("KeyError: 'name'")
		}
		out = append(out, pyrepr.Str(name))
	}
	return out, nil
}

// Whether the gateway runs and its text, running via the default multiplexer for a served satellite profile
func (s *statusRun) gatewayState() (bool, string, error) {
	snapshot, err := statusGatewaySnapshot(s.ctx, false)
	if err != nil {
		return false, "", err
	}
	if snapshot.Running() {
		return true, "running", nil
	}
	served, err := gateway.NamedProfileServedByRunningMultiplexerFor(s.ctx, nil)
	if err != nil {
		return false, "", err
	}
	if served {
		return true, "running (via the default-profile multiplexer)", nil
	}
	return false, "stopped", nil
}

// One summary row, a failing probe printing unknown for its own row only
func (s *statusRun) summaryRow(label string, probe func() ([]string, error), empty string) {
	var values []string
	if statusRecover(func() error {
		var err error
		values, err = probe()
		return err
	}) {
		s.kv(label, "unknown")
		return
	}
	seen := map[string]bool{}
	unique := []string{}
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			unique = append(unique, v)
		}
	}
	text := strings.Join(unique, ", ")
	if text == "" {
		text = empty
	}
	s.kv(label, text)
}

// Default status, one line per component with the full flag printing every section
func (s *statusRun) renderSummary() error {
	if err := s.renderHeader(); err != nil {
		return err
	}
	s.println()
	s.loadConfig()
	s.kv("Model:", statusConfiguredModelLabel(s.config))
	label, err := s.effectiveProviderLabel()
	if err != nil {
		return err
	}
	s.kv("Provider:", label)
	s.summaryRow("Providers:", s.authenticatedProviderNames, "none connected")
	var running bool
	var text string
	if statusRecover(func() error {
		var err error
		running, text, err = s.gatewayState()
		return err
	}) {
		s.kv("Gateway:", "unknown")
	} else {
		s.kvFlag("Gateway:", running, text, text)
	}
	s.summaryRow("Platforms:", s.connectedPlatformLabels, "none configured")
	s.kv("Jobs:", statusCronSummary(s.h))
	s.banner([]string{"  Run 'icebath status --full' for every section"}, cliui.Dim)
	s.println()
	return nil
}

// Print order of the full status
func (s *statusRun) sections() []func() error {
	return []func() error{s.renderHeader, s.renderEnvironment, s.renderAPIKeys, s.renderAuthProviders, s.renderNousGateway,
		s.renderAPIKeyProviders, s.renderTerminal, s.renderPlatforms, s.renderGateway, s.renderCron, s.renderSessions,
		s.renderDeep, s.renderFooter}
}

// One screen summary by default, every section under the full or deep flag
func ShowStatus(ctx context.Context, h string, ns map[string]any, w io.Writer) (int, error) {
	s := &statusRun{ctx: ctx, h: h, w: w, c: &cliui.Console{Out: w}, deep: pyrepr.Truthy(ns["deep"]), config: map[string]any{}}
	renders := []func() error{s.renderSummary}
	if pyrepr.Truthy(ns["full"]) || s.deep {
		renders = s.sections()
	}
	for _, render := range renders {
		if err := render(); err != nil {
			return 1, err
		}
	}
	return 0, nil
}
