package observability

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
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	// SetupMarkerDirname is the per profile directory of pending provider setup markers
	SetupMarkerDirname = "provider_setup_markers"
	// SetupStaleAfter is how long a pending flow may run before it reads abandoned
	SetupStaleAfter   = 3600.0
	oauthSessionKey   = "_metrics_setup_flow"
	oauthFailureKey   = "_metrics_setup_failure"
	openRouterKeyName = "OPENROUTER_API_KEY"
)

var (
	notAProvider = Set("aux-config", "cancel", "reasoning", "remove-custom")
	notFailures  = Set("cancelled", "expired")
	sharedTokens = Set("GH_TOKEN", "GITHUB_TOKEN", "HF_TOKEN")
	oauthEndings = map[string][2]string{
		"approved": {"completed", "none"}, "cancelled": {"abandoned", "none"},
		"denied": {"failed", "auth"}, "expired": {"abandoned", "none"},
	}
	oauthReasons = map[string][2]string{
		"user_declined": {"abandoned", "none"}, "superseded": {"abandoned", "none"}, "timeout": {"abandoned", "none"},
		"account_retired": {"failed", "auth"}, "anon_unreachable": {"failed", "network"},
	}
)

// AuthErrorTraits answers the sign in error questions hermes_cli.auth_error_copy and hermes_cli.auth answer
type AuthErrorTraits struct {
	IsCancelled         func(err error) bool
	IsNetworkError      func(err error) bool
	IsDeviceCodeExpired func(err error) bool
	IsAuthError         func(err error) bool
	OAuthErrorCode      func(err error) string
}

// Hooks the setup flows read from packages above this one
var (
	// AuthErrorTraitsHook classifies sign in errors, set by pkg/provider
	AuthErrorTraitsHook *AuthErrorTraits
	// ProviderForAPIKeyEnvHook returns the registry provider whose api_key_env_vars hold a variable, set by pkg/provider
	ProviderForAPIKeyEnvHook func(envVar string) (string, bool)
	// ToolProviderEnvKeysHook lists every env var key a tool category provider row asks for
	ToolProviderEnvKeysHook = tools.ToolProviderEnvKeys
)

// SetupFlow is one started provider setup flow and its pending marker
type SetupFlow struct {
	Home     string
	Surface  string
	Provider string
	Marker   string
}

// Pending provider setup marker directory of a profile home
func setupMarkersDir(h string) string {
	return filepath.Join(h, "telemetry", "shared_metrics", SetupMarkerDirname)
}

// Context bound to a profile home when one is given
func boundHome(ctx context.Context, homeDir string) context.Context {
	if homeDir == "" {
		return ctx
	}
	return home.WithOverride(ctx, homeDir)
}

// The owning profile's shared metrics gate
func CollectionEnabled(ctx context.Context, homeDir string) (on bool) {
	defer func() {
		if recover() != nil {
			on = false
		}
	}()
	return relayEnabled(boundHome(ctx, homeDir))
}

// Bounded provider setup row, nil when the surface or event is outside the contract
func ProviderSetupFields(surface, provider, event, failureClass any) map[string]string {
	s, sOK := surface.(string)
	e, eOK := event.(string)
	if !sOK || !eOK || !ProviderSetupSurfaces.Has(s) || !ProviderSetupEvents.Has(e) {
		return nil
	}
	failure := "none"
	fc, _ := failureClass.(string)
	if e == "failed" && notFailures.Has(fc) {
		e = "abandoned"
	} else if e == "failed" {
		failure = "other"
		if fc != "none" && ProviderSetupFailureClasses.Has(fc) {
			failure = fc
		}
	}
	return map[string]string{"event": e, "failure_class": failure, "provider": ProviderMetricName(provider), "surface": s}
}

// Closed class of an error that ended a setup flow, cancelled and expired later reading abandoned
func SetupFailureClass(err error) string {
	if tagged, ok := err.(interface{ SetupFailureClass() string }); ok {
		return tagged.SetupFailureClass()
	}
	traits := AuthErrorTraitsHook
	if traits == nil {
		traits = &AuthErrorTraits{}
	}
	check := func(fn func(error) bool, e error) bool { return fn != nil && e != nil && fn(e) }
	_, setupCancelled := err.(interface{ SetupCancelled() })
	code := ""
	if traits.OAuthErrorCode != nil {
		code = traits.OAuthErrorCode(err)
	}
	if check(traits.IsCancelled, err) || setupCancelled || code == "access_denied" {
		return "cancelled"
	}
	if check(traits.IsNetworkError, err) || check(traits.IsNetworkError, errors.Unwrap(err)) {
		return "network"
	}
	if check(traits.IsDeviceCodeExpired, err) {
		return "expired"
	}
	if check(traits.IsAuthError, err) {
		return "auth"
	}
	return "other"
}

// Saves one row and waits for the store, a row the contract cannot build settling as is
func saveRow(ctx context.Context, fields map[string]string) bool {
	if fields == nil {
		return true
	}
	return EmitSaved(ctx, []Mark{{Name: ProviderSetupMark, Data: fields}}) == 1
}

// Records started and drops the pending marker, nil with no files and no rows when collection is off
func BeginProviderSetup(ctx context.Context, surface string, provider any, homeDir string) (flow *SetupFlow) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Provider setup start not recorded", "error", r)
			flow = nil
		}
	}()
	ctx = boundHome(ctx, homeDir)
	if !pyrepr.Truthy(provider) {
		return nil
	}
	if p, ok := provider.(string); ok && notAProvider.Has(p) {
		return nil
	}
	if !relayEnabled(ctx) {
		return nil
	}
	fields := ProviderSetupFields(surface, provider, "started", "none")
	if fields == nil {
		return nil
	}
	h := home.Dir(ctx)
	ReportAbandonedSetups(ctx, h)
	dir := setupMarkersDir(h)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Debug("Provider setup start not recorded", "error", err)
		return nil
	}
	pid := os.Getpid()
	marker := filepath.Join(dir, fmt.Sprintf("%s-%d-%d.json", surface, pid, time.Now().UnixNano()))
	record := pyjson.NewObject()
	record.Set("pid", int64(pid))
	var start any
	if ProcessStartTimeHook != nil {
		start = ProcessStartTimeHook(pid)
	}
	record.Set("start_time", start)
	record.Set("started_at", float64(time.Now().UnixNano())/1e9)
	record.Set("surface", surface)
	record.Set("provider", fields["provider"])
	if err := writeMarker(marker, record); err != nil {
		slog.Debug("Provider setup start not recorded", "error", err)
		return nil
	}
	saveRow(ctx, fields)
	return &SetupFlow{Home: h, Surface: surface, Provider: fields["provider"], Marker: marker}
}

// Records the flow's end once, only while its marker is still unclaimed
func FinishProviderSetup(ctx context.Context, flow *SetupFlow, event, failureClass string) {
	if flow == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Provider setup end not recorded", "error", r)
		}
	}()
	claimed := fmt.Sprintf("%s.%d%s", flow.Marker, os.Getpid(), reportingSuffix)
	if err := os.Rename(flow.Marker, claimed); err != nil {
		return
	}
	ctx = boundHome(ctx, flow.Home)
	fields := ProviderSetupFields(flow.Surface, flow.Provider, event, failureClass)
	SettleClaim(claimed, flow.Marker, saveRow(ctx, fields))
}

// Reports whether a pending marker's flow can no longer finish
func setupReportable(record *pyjson.Object) bool {
	if record == nil {
		return true
	}
	startedAt, _ := record.Get("started_at")
	started, ok := nonNegativeNumber(startedAt)
	if !ok || float64(time.Now().UnixNano())/1e9-started > SetupStaleAfter {
		return true
	}
	return !pidRecordIsLive(record)
}

// Reports every pending setup marker whose flow can no longer finish as abandoned
func ReportAbandonedSetups(ctx context.Context, h string) {
	dir := setupMarkersDir(h)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.HasPrefix(name, ".") || !strings.Contains(name, ".json") {
			continue
		}
		path := filepath.Join(dir, name)
		reporting := strings.HasSuffix(name, reportingSuffix)
		if reporting && claimerAlive(path) {
			continue
		}
		record, _ := readRecord(path)
		if !reporting && !setupReportable(record) {
			continue
		}
		original := filepath.Join(dir, strings.SplitN(name, ".json", 2)[0]+".json")
		claimed := fmt.Sprintf("%s.%d%s", original, os.Getpid(), reportingSuffix)
		if err := os.Rename(path, claimed); err != nil {
			continue
		}
		var fields map[string]string
		if record != nil {
			surface, _ := record.Get("surface")
			provider, _ := record.Get("provider")
			fields = ProviderSetupFields(surface, provider, "abandoned", "none")
		}
		SettleClaim(claimed, original, saveRow(ctx, fields))
	}
}

// Records a setup that starts and lands in one action, in the background when a handler must not wait
func RecordProviderSetupDone(ctx context.Context, surface string, provider any, homeDir string, background bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Provider setup not recorded", "error", r)
		}
	}()
	bound := boundHome(ctx, homeDir)
	if !pyrepr.Truthy(provider) || !relayEnabled(bound) {
		return
	}
	h := home.Dir(bound)
	run := func(ctx context.Context) {
		FinishProviderSetup(ctx, BeginProviderSetup(ctx, surface, provider, h), "completed", "none")
	}
	if background {
		go run(context.WithoutCancel(ctx))
		return
	}
	run(ctx)
}

// The shipped provider whose own API key lives in a variable, false for ecosystem tokens and tool keys unless connecting
func ProviderForAPIKeyEnv(envVar any, connecting bool) (string, bool) {
	name, _ := envVar.(string)
	if name == openRouterKeyName {
		return "openrouter", true
	}
	if !connecting {
		if sharedTokens.Has(name) {
			return "", false
		}
		if ToolProviderEnvKeysHook != nil {
			for _, key := range ToolProviderEnvKeysHook() {
				if key == name {
					return "", false
				}
			}
		}
	}
	if ProviderForAPIKeyEnvHook == nil {
		return "", false
	}
	return ProviderForAPIKeyEnvHook(name)
}

// Counts a provider API key saved from a settings form, only for a new or changed non empty value
func RecordAPIKeySaved(ctx context.Context, envVar, value, previous any, surface string, connecting bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("API key setup not recorded", "error", r)
		}
	}()
	v, ok := value.(string)
	if !ok || pystr.Strip(v) == "" || pyrepr.Equal(value, previous) {
		return
	}
	if !relayEnabled(ctx) {
		return
	}
	if provider, ok := ProviderForAPIKeyEnv(envVar, connecting); ok && provider != "" {
		RecordProviderSetupDone(ctx, surface, provider, "", true)
	}
}

// cliSetupState is the interactive CLI's provider flow state, the surface, open flow hints and a backed out flow
type cliSetupState struct {
	mu        sync.Mutex
	surface   string
	hints     *setupHints
	backedOut *backedOutFlow
}

// setupHints is what the shared save helpers report back to one open CLI flow
type setupHints struct {
	saved   bool
	failure string
}

// backedOutFlow is a flow the user left with Back, kept open for a resume
type backedOutFlow struct {
	flow   *SetupFlow
	before []any
}

var cliSetup cliSetupState

// Marks the open CLI flow as having persisted the provider and model choice
func NoteProviderSetupSaved() {
	cliSetup.mu.Lock()
	defer cliSetup.mu.Unlock()
	if cliSetup.hints != nil {
		cliSetup.hints.saved = true
	}
}

// Records the first failure class an open CLI flow reported before returning
func NoteProviderSetupFailure(failureClass string) {
	cliSetup.mu.Lock()
	defer cliSetup.mu.Unlock()
	if cliSetup.hints != nil && cliSetup.hints.failure == "" {
		cliSetup.hints.failure = failureClass
	}
}

// Notes a sign in failure's setup class on the open CLI flow
func NoteSignInFailure(err error) {
	defer func() { _ = recover() }()
	NoteProviderSetupFailure(SetupFailureClass(err))
}

// The configured model route, provider default model and base URL, or the bare model value
func modelRoute(ctx context.Context) ([]any, error) {
	tree, err := config.LoadContext(ctx, home.Dir(ctx), os.LookupEnv)
	if err != nil {
		return nil, err
	}
	model := tree["model"]
	d, ok := asDict(model)
	if !ok {
		return []any{model}, nil
	}
	return []any{d["provider"], d["default"], d["base_url"]}, nil
}

// Reports whether an error is the setup wizard's Back signal
func isGoBack(err error) bool {
	_, ok := err.(interface{ SetupGoBack() })
	return ok
}

// Ends a flow the user backed out of and never resumed as abandoned
func closeBackedOut(ctx context.Context) {
	cliSetup.mu.Lock()
	pending := cliSetup.backedOut
	cliSetup.backedOut = nil
	cliSetup.mu.Unlock()
	if pending != nil {
		FinishProviderSetup(ctx, pending.flow, "abandoned", "none")
	}
}

// Runs a provider picker entry point under its surface, closing a flow left by Back unless Back is replaying
func ProviderSetupSurface(ctx context.Context, surface string, body func(ctx context.Context) error) (err error) {
	cliSetup.mu.Lock()
	previous := cliSetup.surface
	cliSetup.surface = surface
	cliSetup.mu.Unlock()
	defer func() {
		cliSetup.mu.Lock()
		cliSetup.surface = previous
		cliSetup.mu.Unlock()
	}()
	panicked := true
	defer func() {
		if panicked {
			closeBackedOut(ctx)
		}
	}()
	err = body(ctx)
	panicked = false
	if err == nil || !isGoBack(err) {
		closeBackedOut(ctx)
	}
	return err
}

// Reports whether a backed out flow continues on this surface and provider
func resumes(flow *SetupFlow, surface string, provider any) bool {
	if p, ok := provider.(string); ok && notAProvider.Has(p) {
		return false
	}
	return flow.Surface == surface && flow.Provider == ProviderMetricName(provider)
}

// Tracks one CLI provider flow under the entry point's surface, ending it completed, failed or abandoned
func CLIProviderSetup(ctx context.Context, provider any, body func(ctx context.Context) error) error {
	cliSetup.mu.Lock()
	surface := cliSetup.surface
	pending := cliSetup.backedOut
	cliSetup.backedOut = nil
	cliSetup.mu.Unlock()
	if pending != nil && !resumes(pending.flow, surface, provider) {
		FinishProviderSetup(ctx, pending.flow, "abandoned", "none")
		pending = nil
	}
	var flow *SetupFlow
	var before []any
	if pending != nil {
		flow, before = pending.flow, pending.before
	} else if surface != "" {
		flow = BeginProviderSetup(ctx, surface, provider, "")
	}
	if flow == nil {
		return body(ctx)
	}
	if pending == nil {
		if route, err := modelRoute(ctx); err == nil {
			before = route
		}
	}
	hints := &setupHints{}
	cliSetup.mu.Lock()
	cliSetup.hints = hints
	cliSetup.mu.Unlock()
	clearHints := func() {
		cliSetup.mu.Lock()
		cliSetup.hints = nil
		cliSetup.mu.Unlock()
	}
	panicked := true
	defer func() {
		if panicked {
			r := recover()
			clearHints()
			FinishProviderSetup(ctx, flow, "failed", panicSetupClass(r))
			panic(r)
		}
	}()
	err := body(ctx)
	panicked = false
	clearHints()
	if err != nil {
		if isGoBack(err) {
			cliSetup.mu.Lock()
			cliSetup.backedOut = &backedOutFlow{flow: flow, before: before}
			cliSetup.mu.Unlock()
		} else {
			FinishProviderSetup(ctx, flow, "failed", SetupFailureClass(err))
		}
		return err
	}
	cliSetup.mu.Lock()
	landed, failure := hints.saved, hints.failure
	cliSetup.mu.Unlock()
	if !landed && before != nil {
		if route, rerr := modelRoute(ctx); rerr == nil {
			landed = !pyrepr.Equal(route, before)
		}
	}
	switch {
	case landed:
		FinishProviderSetup(ctx, flow, "completed", "none")
	case failure != "":
		FinishProviderSetup(ctx, flow, "failed", failure)
	default:
		FinishProviderSetup(ctx, flow, "abandoned", "none")
	}
	return nil
}

// Setup class of a recovered panic value, errors classified like returned ones
func panicSetupClass(r any) string {
	if err, ok := r.(error); ok {
		return SetupFailureClass(err)
	}
	return "other"
}

// Surface of a web setup, desktop when the Desktop owns this backend and dashboard otherwise
func WebSetupSurface() string {
	if tools.IsDesktopOwnedBackend(os.Args) {
		return "desktop"
	}
	return "dashboard"
}

// Web surface resolver the OAuth flows start on
var webSetupSurface = WebSetupSurface

// Starts an OAuth setup flow on the web surface
func BeginOAuthSetup(ctx context.Context, provider string, homeDir string) *SetupFlow {
	return BeginProviderSetup(ctx, webSetupSurface(), provider, homeDir)
}

// Stores a started flow on its OAuth session
func AttachOAuthSetup(sess map[string]any, flow *SetupFlow) {
	if flow != nil && sess != nil {
		sess[oauthSessionKey] = flow
	}
}

// Keeps a dead sign in poller's closed failure class for the settle
func NoteOAuthFailure(sess map[string]any, err error) {
	defer func() { _ = recover() }()
	if sess != nil {
		sess[oauthFailureKey] = SetupFailureClass(err)
	}
}

// Ends an OAuth session's flow from its status, nothing while pending or once ended
func SettleOAuthSetup(ctx context.Context, sess map[string]any, abandoned bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("OAuth setup end not recorded", "error", r)
		}
	}()
	if sess == nil || sess[oauthSessionKey] == nil {
		return
	}
	status := pyText(sess["status"])
	if pyrepr.Truthy(sess["cancelled"]) {
		status = "cancelled"
	}
	ending, ok := [2]string{}, false
	if status == "denied" || status == "error" {
		ending, ok = oauthReasons[pyText(sess["reason"])]
	}
	if !ok {
		ending, ok = oauthEndings[status]
	}
	if !ok && status == "error" {
		failure := "other"
		if v := sess[oauthFailureKey]; pyrepr.Truthy(v) {
			failure = pyText(v)
		}
		ending, ok = [2]string{"failed", failure}, true
	}
	if !ok {
		if !abandoned {
			return
		}
		ending = [2]string{"abandoned", "none"}
	}
	flow, _ := sess[oauthSessionKey].(*SetupFlow)
	delete(sess, oauthSessionKey)
	FinishProviderSetup(ctx, flow, ending[0], ending[1])
}
