package plugins

import (
	"bufio"
	"context"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	xterm "github.com/charmbracelet/x/term"
	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	diffutil "github.com/nickheyer/icebath/pkg/utils/diff"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	mathutil "github.com/nickheyer/icebath/pkg/utils/math"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	regexutil "github.com/nickheyer/icebath/pkg/utils/regex"
	shellutil "github.com/nickheyer/icebath/pkg/utils/shell"
	termutil "github.com/nickheyer/icebath/pkg/utils/term"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
)

const (
	ShellHookDefaultTimeoutSeconds = 60
	ShellHookMaxTimeoutSeconds     = 300
	ShellHookAllowlistFilename     = "shell-hooks-allowlist.json"
	ShellHookBlockExitCode         = 2
	shellHookDefaultBlockMessage   = "Blocked by shell hook."
	shellHookStderrLimit           = 400
	shellHookNotDirectlyExecutable = "cannot be run directly on Windows (there is no shebang support): start it with its interpreter, e.g. 'bash <path>'"
)

var (
	shellHookBlockingEvents   = map[string]bool{"pre_tool_call": true}
	shellHookToolEvents       = map[string]bool{"pre_tool_call": true, "post_tool_call": true}
	shellHookTopLevelKeys     = map[string]bool{"tool_name": true, "args": true, "session_id": true, "parent_session_id": true}
	shellHookWindowsInterp    = map[string]string{".sh": "bash", ".bash": "bash", ".py": "python"}
	shellHookScriptExtensions = []string{".sh", ".bash", ".zsh", ".fish", ".py", ".pyw", ".rb", ".pl", ".lua", ".js", ".mjs", ".cjs", ".ts"}
	shellHookPreToolDialects  = [][4]string{{"action", "message", "reason", "args"}, {"decision", "reason", "message", "tool_input"}}
	shellHooksRegistered      = map[shellHookKey]bool{}
	shellHooksRegisteredMu    sync.Mutex
	shellHooksSyncMu          sync.Mutex
	shellHookLaunchConsent    = map[[3]string]bool{}
	shellHookProcessAccept    atomic.Bool
	shellHookSkipLogged                 = map[[3]string]bool{}
	shellHookStdinIsTTY                 = func() bool { return xterm.IsTerminal(os.Stdin.Fd()) }
	shellHookStdout           io.Writer = termutil.Stdout
	shellHookStdinReader      *bufio.Reader
	shellHookAtomicWrite      = config.AtomicJSONWriteMode
	shellHookReadLine         = func() (string, error) {
		if shellHookStdinReader == nil {
			shellHookStdinReader = bufio.NewReader(os.Stdin)
		}
		line, err := shellHookStdinReader.ReadString('\n')
		if err != nil && line == "" {
			return "", err
		}
		return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), nil
	}
)

// Identity of a wired shell hook, home event matcher and command
type shellHookKey struct{ home, event, matcher, command string }

// Matcher regex shared by shell hook specs and outbound webhook targets, compiled nil for a literal matcher
type toolMatcher struct {
	Matcher  string
	compiled *regexp2.Regexp
}

// Strips the matcher and compiles it, an invalid regex warned about under the kind and kept literal
func (m *toolMatcher) postInit(kind string) {
	m.Matcher = strings.TrimSpace(m.Matcher)
	m.compiled = nil
	if m.Matcher == "" {
		return
	}
	re, err := regexutil.Compile(m.Matcher, regexutil.Whole())
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("%s matcher %s is invalid (%v) — treating as literal equality", kind, errchain.Show(m.Matcher), err))
	}
	m.compiled = re
}

// Whether the tool name passes the matcher, a TypeError for a non string name against a regex
func (m *toolMatcher) MatchesTool(toolName any) (bool, error) {
	if m.Matcher == "" {
		return true, nil
	}
	if toolName == nil {
		return false, nil
	}
	name, isStr := toolName.(string)
	if m.compiled == nil {
		return isStr && name == m.Matcher, nil
	}
	if !isStr {
		return false, fmt.Errorf("tool name is %s where text was expected", errchain.Kind(toolName))
	}
	return m.compiled.MatchString(name)
}

// One parsed hooks entry, the matcher empty when absent
type ShellHookSpec struct {
	toolMatcher
	Event, Command string
	Timeout        int
	FailClosed     bool
}

// Outcome of one hook subprocess, Error empty and ReturnCode nil when not applicable
type ShellHookResult struct {
	ReturnCode     *int
	Stdout, Stderr string
	TimedOut       bool
	ElapsedSeconds float64
	Error          string
	Parsed         map[string]any
}

// Spec with its matcher stripped and compiled, an invalid regex kept for literal equality
func NewShellHookSpec(event, command, matcher string, timeout int, failClosed bool) *ShellHookSpec {
	s := &ShellHookSpec{toolMatcher: toolMatcher{Matcher: matcher}, Event: event, Command: command, Timeout: timeout, FailClosed: failClosed}
	s.postInit("shell hook")
	return s
}

// Resolved current home, the registration namespace of a profile
func shellHomeKey(ctx context.Context) string { return home.Key(home.Dir(ctx)) }

// Drops the current home's keys from a wired registry so one profile's reload never drops another's
func forgetHomeRegistrations[K comparable](ctx context.Context, registry map[K]bool, mu *sync.Mutex, homeOf func(K) string) {
	h := shellHomeKey(ctx)
	mu.Lock()
	defer mu.Unlock()
	for k := range registry {
		if homeOf(k) == h {
			delete(registry, k)
		}
	}
}

// Whether an allowlist record holds the pair, any event when event is nil
func shellHookEntryMatches(e any, event *string, command string) bool {
	m, ok := shellHookDict(e)
	if !ok {
		return false
	}
	if event != nil {
		if ev, ok := m["event"].(string); !ok || ev != *event {
			return false
		}
	}
	c, ok := m["command"].(string)
	return ok && c == command
}

// String keyed mapping view of a config or JSON value
func shellHookDict(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, m != nil
	case config.Tree:
		return map[string]any(m), m != nil
	case *jsonutil.Object:
		if m == nil {
			return nil, false
		}
		plain, _ := jsonutil.Plain(m).(map[string]any)
		return plain, true
	case map[any]any:
		return asMap(m), true
	}
	return nil, false
}

// UTC instant as isoformat with a Z suffix, microseconds only when present
func shellHookISO(t time.Time) string {
	t = t.UTC()
	s := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return s + "Z"
}

// Common stdin payload fields in wire order, the profile and owned kanban task resolved at fire time
func shellHookPayloadFields(ctx context.Context, kwargs map[string]any) *jsonutil.Object {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}
	extra := jsonutil.NewObject()
	keys := make([]string, 0, len(kwargs))
	for k := range kwargs {
		if !shellHookTopLevelKeys[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		extra.Set(k, kwargs[k])
	}
	extra.Set("kanban_task_id", tools.OwnedKanbanTask(ctx))
	var toolInput any
	if args, ok := shellHookDict(kwargs["args"]); ok {
		toolInput = args
	}
	session := kwargs["session_id"]
	if !anyutil.IsTrue(session) {
		session = kwargs["parent_session_id"]
	}
	if !anyutil.IsTrue(session) {
		session = ""
	}
	return jsonutil.ObjectOf("tool_name", kwargs["tool_name"], "tool_input", toolInput, "session_id", session,
		"cwd", cwd, "profile", profiles.GetActiveProfileName(ctx), "extra", extra)
}

// Stdin JSON of one firing, values that are not plain data written as their text
func serializeShellHookPayload(ctx context.Context, event string, kwargs map[string]any) (string, error) {
	payload := jsonutil.ObjectOf("hook_event_name", event)
	payload.Merge(shellHookPayloadFields(ctx, kwargs))
	return jsonutil.Encode(hookPayloadData(payload, 0), jsonutil.Spaced())
}

// Deepest nesting of lists and mappings a hook payload keeps as data, anything deeper written as its text
const hookPayloadMaxDepth = 100

// Hook payload value as plain JSON data: text, numbers, true or false, null and values that marshal themselves as they are, lists and mappings rebuilt with text keys, a nil pointer, function or channel as null and anything else (structs, pointers, functions, channels) written as its text, so a payload always encodes
func hookPayloadData(v any, depth int) any {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return nil
	}
	switch rv.Kind() {
	case reflect.Pointer, reflect.Func, reflect.Chan, reflect.UnsafePointer:
		if rv.IsNil() {
			return nil
		}
	}
	switch x := v.(type) {
	case string, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number:
		return v
	case *jsonutil.Object:
		if depth >= hookPayloadMaxDepth {
			return anyutil.String(v)
		}
		out := jsonutil.NewObject()
		for k, item := range x.All() {
			out.Set(k, hookPayloadData(item, depth+1))
		}
		return out
	case json.Marshaler, encoding.TextMarshaler:
		return v
	}
	switch rv.Kind() {
	case reflect.String:
		return rv.String()
	case reflect.Bool:
		return rv.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return rv.Uint()
	case reflect.Float32, reflect.Float64:
		return rv.Float()
	case reflect.Map:
		if depth >= hookPayloadMaxDepth {
			return anyutil.String(v)
		}
		out := make(map[string]any, rv.Len())
		for it := rv.MapRange(); it.Next(); {
			out[anyutil.String(it.Key().Interface())] = hookPayloadData(it.Value().Interface(), depth+1)
		}
		return out
	case reflect.Slice, reflect.Array:
		if rv.Type().Elem().Kind() == reflect.Uint8 || depth >= hookPayloadMaxDepth {
			return anyutil.String(v)
		}
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = hookPayloadData(rv.Index(i).Interface(), depth+1)
		}
		return out
	}
	return anyutil.String(v)
}

// Registers every consented configured hook once per home and returns the newly wired specs
func RegisterShellHooksFromConfig(ctx context.Context, acceptHooks, interactive bool) ([]*ShellHookSpec, error) {
	specs, _, _ := shellHookSpecs(ctx)
	if len(specs) == 0 {
		return []*ShellHookSpec{}, nil
	}
	manager, homeKey := GetPluginManager(ctx), shellHomeKey(ctx)
	shellHooksRegisteredMu.Lock()
	pending := specs[:0:0]
	for _, spec := range specs {
		if !shellHooksRegistered[shellHookKeyOf(homeKey, spec)] {
			pending = append(pending, spec)
		}
	}
	shellHooksRegisteredMu.Unlock()
	accept, allowed := resolveShellHookAccept(ctx, acceptHooks), AllowlistedShellHookPairs(ctx)
	consented := []*ShellHookSpec{}
	for _, spec := range pending {
		ok, err := shellHookConsented(ctx, spec, accept, allowed, interactive)
		if err != nil {
			return nil, err
		}
		if ok {
			consented = append(consented, spec)
		} else {
			pkgLog().Warn(fmt.Sprintf("shell hook for %s (%s) not allowlisted in %s — skipped. Use --accept-hooks / "+
				"ICEBATH_ACCEPT_HOOKS=1 / hooks_auto_accept: true, or approve it at a CLI prompt "+
				"for this profile.", spec.Event, spec.Command, ShellHookAllowlistPath(ctx)))
		}
	}
	shellHooksRegisteredMu.Lock()
	defer shellHooksRegisteredMu.Unlock()
	return wireShellHooks(manager, homeKey, consented), nil
}

// Brings this home's wired hooks in step with the hooks setting of ctx and consent, never prompting and never disarming on bad input
func SyncShellHooksFromConfig(ctx context.Context) {
	shellHooksSyncMu.Lock()
	specs, run, complete := shellHookSpecs(ctx)
	if !run {
		shellHooksSyncMu.Unlock()
		return
	}
	allowlist, readable := readShellHookAllowlist(ctx)
	allowed := approvedShellHookPairs(allowlist["approvals"])
	homeKey := shellHomeKey(ctx)
	for pair := range allowed {
		delete(shellHookLaunchConsent, [3]string{homeKey, pair[0], pair[1]})
	}
	for c := range shellHookLaunchConsent {
		if c[0] == homeKey {
			allowed[[2]string{c[1], c[2]}] = true
		}
	}
	accept := resolveShellHookAccept(ctx, false)
	var wanted []*ShellHookSpec
	for _, spec := range specs {
		if accept || allowed[[2]string{spec.Event, spec.Command}] {
			wanted = append(wanted, spec)
		} else if k := [3]string{homeKey, spec.Event, spec.Command}; !shellHookSkipLogged[k] {
			shellHookSkipLogged[k] = true
			pkgLog().Warn(fmt.Sprintf("shell hook for %s (%s) not allowlisted in %s — skipped. Approve it at a CLI "+
				"prompt for this profile, or use --accept-hooks / ICEBATH_ACCEPT_HOOKS=1 / "+
				"hooks_auto_accept: true.", spec.Event, spec.Command, ShellHookAllowlistPath(ctx)))
		}
	}
	manager := GetPluginManager(ctx)
	shellHooksRegisteredMu.Lock()
	wireShellHooks(manager, homeKey, wanted)
	if !(readable && complete) {
		suspended := [3]string{homeKey, "", "removal"}
		if !shellHookSkipLogged[suspended] {
			shellHookSkipLogged[suspended] = true
			reason := "a hooks: entry in config.yaml is malformed"
			if !readable {
				reason = "the allowlist is unreadable"
			}
			pkgLog().Warn(fmt.Sprintf("shell hooks: %s, so removed or revoked hooks stay registered until it is fixed", reason))
		}
		shellHooksRegisteredMu.Unlock()
		shellHooksSyncMu.Unlock()
		return
	}
	configured := map[[3]string]bool{}
	for _, spec := range specs {
		configured[[3]string{homeKey, spec.Event, spec.Command}] = true
	}
	for c := range shellHookLaunchConsent {
		if c[0] == homeKey && !configured[c] {
			delete(shellHookLaunchConsent, c)
		}
	}
	keep := map[shellHookKey]bool{}
	for _, spec := range wanted {
		keep[shellHookKeyOf(homeKey, spec)] = true
	}
	var stale []shellHookKey
	staleSet := map[shellHookKey]bool{}
	for k := range shellHooksRegistered {
		if k.home == homeKey && !keep[k] {
			stale = append(stale, k)
			staleSet[k] = true
			delete(shellHooksRegistered, k)
		}
	}
	events := map[string]bool{}
	for _, k := range stale {
		events[k.event] = true
	}
	manager.mu.Lock()
	for event := range events {
		kept := []*hookEntry{}
		for _, cb := range manager.hooks[event] {
			if cb.shellKey == nil || !staleSet[*cb.shellKey] {
				kept = append(kept, cb)
			}
		}
		manager.hooks[event] = kept
	}
	manager.mu.Unlock()
	shellHooksRegisteredMu.Unlock()
	shellHooksSyncMu.Unlock()
	sort.Slice(stale, func(i, j int) bool {
		return stale[i].event+"\x00"+stale[i].command < stale[j].event+"\x00"+stale[j].command
	})
	for _, k := range stale {
		pkgLog().Info(fmt.Sprintf("shell hook unregistered (removed from config or allowlist): %s -> %s", k.event, k.command))
	}
}

// First use consent for every configured hook without wiring any, returning the consented specs
func RecordShellHookConsentFromConfig(ctx context.Context, acceptHooks bool) ([]*ShellHookSpec, error) {
	specs, _, _ := shellHookSpecs(ctx)
	if len(specs) == 0 {
		return []*ShellHookSpec{}, nil
	}
	accept, allowed := resolveShellHookAccept(ctx, acceptHooks), AllowlistedShellHookPairs(ctx)
	out := []*ShellHookSpec{}
	for _, spec := range specs {
		ok, err := shellHookConsented(ctx, spec, accept, allowed, true)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, spec)
		}
	}
	return out, nil
}

// Records a launch's --accept-hooks in the allowlist and holds in memory each approval the write lost
func AcceptShellHooksForLaunch(ctx context.Context) error {
	specs, err := RecordShellHookConsentFromConfig(ctx, true)
	if err != nil || len(specs) == 0 {
		return err
	}
	saved, homeKey := AllowlistedShellHookPairs(ctx), shellHomeKey(ctx)
	shellHooksSyncMu.Lock()
	defer shellHooksSyncMu.Unlock()
	for _, spec := range specs {
		if !saved[[2]string{spec.Event, spec.Command}] {
			shellHookLaunchConsent[[3]string{homeKey, spec.Event, spec.Command}] = true
		}
	}
	return nil
}

// Opts this process into every configured hook for its lifetime without exporting the variable
func AcceptShellHooksForProcess() { shellHookProcessAccept.Store(true) }

// Specs of the hooks setting of ctx, run false inside a safe worker or in safe mode
func shellHookSpecs(ctx context.Context) (specs []*ShellHookSpec, run bool, complete bool) {
	if config.SafeWorkerEnabled() {
		return nil, false, true
	}
	if envEnabled(EnvSafeMode) {
		pkgLog().Info("ICEBATH_SAFE_MODE=1 — shell-hook registration skipped")
		return nil, false, true
	}
	specs, complete = parseShellHooks(settings.Hooks.Get(ctx))
	return specs, true, complete
}

func shellHookKeyOf(homeKey string, spec *ShellHookSpec) shellHookKey {
	return shellHookKey{homeKey, spec.Event, spec.Matcher, spec.Command}
}

// Appends a callback for each spec not yet wired for the home, caller holds the registry lock
func wireShellHooks(manager *Manager, homeKey string, specs []*ShellHookSpec) []*ShellHookSpec {
	registered := []*ShellHookSpec{}
	for _, spec := range specs {
		key := shellHookKeyOf(homeKey, spec)
		if shellHooksRegistered[key] {
			continue
		}
		entry := &hookEntry{fn: makeShellHookCallback(spec), name: fmt.Sprintf("shell_hook[%s:%s]", spec.Event, spec.Command),
			module: "plugins", shellKey: &key}
		manager.mu.Lock()
		manager.hooks[spec.Event] = append(manager.hooks[spec.Event], entry)
		manager.mu.Unlock()
		shellHooksRegistered[key] = true
		registered = append(registered, spec)
		matcher := "None"
		if spec.Matcher != "" {
			matcher = spec.Matcher
		}
		pkgLog().Info(fmt.Sprintf("shell hook registered: %s -> %s (matcher=%s, timeout=%ds, fail_closed=%s)",
			spec.Event, spec.Command, matcher, spec.Timeout, errchain.Show(spec.FailClosed)))
	}
	return registered
}

// Event and command pairs of the well formed approval records
func approvedShellHookPairs(approvals any) map[[2]string]bool {
	out := map[[2]string]bool{}
	list, _ := approvals.([]any)
	for _, e := range list {
		m, ok := shellHookDict(e)
		if !ok {
			continue
		}
		ev, ok1 := m["event"].(string)
		cmd, ok2 := m["command"].(string)
		if ok1 && ok2 {
			out[[2]string{ev, cmd}] = true
		}
	}
	return out
}

// Approved event and command pairs of the current home's allowlist
func AllowlistedShellHookPairs(ctx context.Context) map[[2]string]bool {
	return approvedShellHookPairs(LoadShellHookAllowlist(ctx)["approvals"])
}

// Allowlisted, or approved now by an opt in or a terminal prompt when asked, the approval cached in allowed
func shellHookConsented(ctx context.Context, spec *ShellHookSpec, accept bool, allowed map[[2]string]bool, prompt bool) (bool, error) {
	pair := [2]string{spec.Event, spec.Command}
	if allowed[pair] {
		return true, nil
	}
	if !(accept || prompt) {
		return false, nil
	}
	ok, err := promptAndRecordShellHook(ctx, spec.Event, spec.Command, accept)
	if err != nil || !ok {
		return false, err
	}
	allowed[pair] = true
	return true, nil
}

// Parsed hooks of the hooks setting of ctx without wiring any, for the hooks list command and doctor
func IterConfiguredShellHooks(ctx context.Context) []*ShellHookSpec {
	return parseShellHooksBlock(settings.Hooks.Get(ctx))
}

// Rewires this home's config hooks after a force reload cleared the manager, never prompting
func reRegisterConfigHooks(ctx context.Context) error {
	forgetHomeRegistrations(ctx, shellHooksRegistered, &shellHooksRegisteredMu, func(k shellHookKey) string { return k.home })
	_, err := RegisterShellHooksFromConfig(ctx, false, false)
	return err
}

// Clears the wiring registry and every in process consent
func ResetShellHooksForTests() {
	shellHooksRegisteredMu.Lock()
	clear(shellHooksRegistered)
	shellHooksRegisteredMu.Unlock()
	shellHooksSyncMu.Lock()
	clear(shellHookLaunchConsent)
	clear(shellHookSkipLogged)
	shellHooksSyncMu.Unlock()
	shellHookProcessAccept.Store(false)
}

// Specs of a hooks block, malformed entries warned about and skipped
func parseShellHooksBlock(hooksCfg any) []*ShellHookSpec {
	specs, _ := parseShellHooks(hooksCfg)
	return specs
}

// Specs of a hooks block and whether no entry was dropped as malformed
func parseShellHooks(hooksCfg any) ([]*ShellHookSpec, bool) {
	specs := []*ShellHookSpec{}
	if hooksCfg == nil {
		return specs, true
	}
	block, ok := shellHookDict(hooksCfg)
	if !ok {
		return specs, false
	}
	complete := true
	for _, eventName := range sortedKeys(block) {
		entries := block[eventName]
		if eventName == "output_spill" || eventName == "outbound" {
			continue
		}
		if shellUnsupportedHooks[eventName] {
			pkgLog().Warn(fmt.Sprintf("hook event %s is Python-plugin-only: shell hooks cannot return its directive, "+
				"so this registration is refused rather than silently ignored", errchain.Show(eventName)))
			continue
		}
		if !validHooks[eventName] {
			if suggestion := diffutil.CloseMatches(eventName, ValidHooks(), 1, 0.6); len(suggestion) > 0 {
				pkgLog().Warn(fmt.Sprintf("unknown hook event %s in hooks: config — did you mean %s?", errchain.Show(eventName), errchain.Show(suggestion[0])))
			} else {
				pkgLog().Warn(fmt.Sprintf("unknown hook event %s in hooks: config (valid: %s)", errchain.Show(eventName), strings.Join(ValidHooks(), ", ")))
			}
			complete = false
			continue
		}
		if entries == nil {
			continue
		}
		list, ok := entries.([]any)
		if !ok {
			pkgLog().Warn(fmt.Sprintf("hooks.%s must be a list of hook definitions; got %s", eventName, errchain.Kind(entries)))
			complete = false
			continue
		}
		for i, raw := range list {
			if spec := parseShellHookEntry(eventName, i, raw); spec != nil {
				specs = append(specs, spec)
			} else {
				complete = false
			}
		}
	}
	return specs, complete
}

// One validated hooks entry, nil with a warning when malformed
func parseShellHookEntry(event string, index int, raw any) *ShellHookSpec {
	warn := func(msg string) { pkgLog().Warn(fmt.Sprintf("hooks.%s[%d]", event, index) + msg) }
	entry, ok := shellHookDict(raw)
	if !ok {
		warn(fmt.Sprintf(" must be a mapping with a 'command' key; got %s", errchain.Kind(raw)))
		return nil
	}
	command, ok := entry["command"].(string)
	if !ok || strings.TrimSpace(command) == "" {
		warn(" is missing a non-empty 'command' field")
		return nil
	}
	matcherRaw, hasMatcher := entry["matcher"]
	matcher, isStr := matcherRaw.(string)
	if hasMatcher && matcherRaw != nil && !isStr {
		warn(".matcher must be a string regex; ignoring")
		hasMatcher = false
	}
	if hasMatcher && matcherRaw != nil && !shellHookToolEvents[event] {
		warn(fmt.Sprintf(".matcher=%s will be ignored at runtime — the matcher field is only honored for "+
			"pre_tool_call / post_tool_call.  The hook will fire on every %s event.", errchain.Show(matcher), event))
		hasMatcher = false
	}
	if !hasMatcher || matcherRaw == nil {
		matcher = ""
	}
	timeoutRaw, present := entry["timeout"]
	if !present {
		timeoutRaw = int64(ShellHookDefaultTimeoutSeconds)
	}
	timeout := ShellHookDefaultTimeoutSeconds
	if n, err := anyutil.ToInt(timeoutRaw); err != nil && !errors.Is(err, anyutil.ErrRange) {
		warn(fmt.Sprintf(".timeout must be an int (got %s); using default %ds", errchain.Show(timeoutRaw), ShellHookDefaultTimeoutSeconds))
	} else if n < 1 {
		warn(fmt.Sprintf(".timeout must be >=1; using default %ds", ShellHookDefaultTimeoutSeconds))
	} else if n > ShellHookMaxTimeoutSeconds {
		warn(fmt.Sprintf(".timeout=%ds exceeds max %ds; clamping", n, ShellHookMaxTimeoutSeconds))
		timeout = ShellHookMaxTimeoutSeconds
	} else {
		timeout = int(n)
	}
	failRaw, present := entry["fail_closed"]
	if !present {
		if failRaw, present = entry["failClosed"]; !present {
			failRaw = false
		}
	}
	failClosed, isBool := failRaw.(bool)
	if !isBool {
		warn(fmt.Sprintf(".fail_closed must be a boolean (got %s); using default false (fail open)", errchain.Show(failRaw)))
		failClosed = false
	}
	if failClosed && !shellHookBlockingEvents[event] {
		warn(fmt.Sprintf(".fail_closed=true will be ignored at runtime — fail_closed only applies to blocking-capable "+
			"events (%s).  The hook will fail open on %s like any other hook.", strings.Join(sortedKeys(shellHookBlockingEvents), ", "), event))
		failClosed = false
	}
	return NewShellHookSpec(event, strings.TrimSpace(command), matcher, timeout, failClosed)
}

// Argv with an interpreter prepended for an existing Windows script whose suffix names one
func windowsShellHookArgv(ctx context.Context, argv []string) ([]string, error) {
	kind, ok := shellHookWindowsInterp[strings.ToLower(filepath.Ext(argv[0]))]
	if !ok || !isFile(argv[0]) {
		return argv, nil
	}
	var interp string
	var err error
	if kind == "python" {
		interp, err = exec.LookPath("python")
	} else {
		interp, err = tools.FindBash(ctx)
	}
	if err != nil {
		return nil, err
	}
	return append([]string{interp}, argv...), nil
}

// Runs the hook command with the payload on stdin, reaping its whole tree on timeout or interrupt
func spawnShellHook(ctx context.Context, spec *ShellHookSpec, stdinJSON string) (*ShellHookResult, error) {
	argv, err := shellutil.SplitCommandLine(pathutil.ExpandUser(spec.Command))
	if err != nil {
		pkgLog().Debug("shell hook command unparsable: " + errchain.Text(err))
		return &ShellHookResult{Error: fmt.Sprintf("command %s cannot be parsed: its quotes or escapes are unbalanced", errchain.Show(spec.Command))}, nil
	}
	if len(argv) == 0 {
		return &ShellHookResult{Error: "empty command"}, nil
	}
	return spawnShellHookArgv(ctx, argv, "", spec.Timeout, stdinJSON)
}

// Runs an argument vector as a hook process in the working directory dir, the process directory when dir is empty, with the payload on stdin for at most the timeout in seconds, reaping its whole tree on timeout or interrupt
func spawnShellHookArgv(ctx context.Context, argv []string, dir string, timeout int, stdinJSON string) (*ShellHookResult, error) {
	r := &ShellHookResult{}
	failed := func(msg string) (*ShellHookResult, error) {
		r.Error = msg
		return r, nil
	}
	t0 := time.Now()
	if runtime.GOOS == "windows" {
		var err error
		if argv, err = windowsShellHookArgv(ctx, argv); err != nil {
			return failed(err.Error())
		}
	}
	env := tools.ToolEnvFrom(ctx)
	if env == nil {
		env = &tools.Env{Home: home.Dir(ctx)}
	}
	childEnv, err := tools.BuildSubprocessEnv(ctx, env, tools.SubprocessEnvOptions{InheritProfileHome: true, ScrubSecrets: tools.MultiplexActiveFor(ctx, env)})
	if err != nil {
		return failed(err.Error())
	}
	run, err := startShellHookProcess(argv, dir, childEnv, []byte(stdinJSON))
	if err != nil {
		return failed(shellHookPopenError(argv[0], err))
	}
	timer := time.NewTimer(time.Duration(timeout) * time.Second)
	defer timer.Stop()
	select {
	case <-run.done:
		code := run.returnCode()
		r.ReturnCode = &code
		r.Stdout = textutil.NormalizeNewlines(strings.ToValidUTF8(string(run.stdout.Bytes()), "\uFFFD"))
		r.Stderr = textutil.NormalizeNewlines(strings.ToValidUTF8(string(run.stderr.Bytes()), "\uFFFD"))
		r.ElapsedSeconds = mathutil.Round(time.Since(t0).Seconds(), 3)
		return r, nil
	case <-timer.C:
		run.reap()
		r.TimedOut = true
		r.ElapsedSeconds = mathutil.Round(time.Since(t0).Seconds(), 3)
		return r, nil
	case <-ctx.Done():
		run.reap()
		return nil, &UserInterrupt{Err: ctx.Err()}
	}
}

// Diagnostic of a failed process start per kind of failure
func shellHookPopenError(name string, err error) string {
	switch {
	case isNotFoundError(err):
		return "command not found"
	case isPermissionError(err):
		return "command not executable"
	case isBadExeFormat(err):
		return errchain.Show(name) + " " + shellHookNotDirectlyExecutable
	}
	return errchain.OSText(err)
}

// Callback invoke_hook calls per firing, nil outside the matcher or inside a safe worker
func makeShellHookCallback(spec *ShellHookSpec) HookFunc {
	return func(ctx context.Context, kwargs map[string]any) (any, error) {
		if config.SafeWorkerEnabled() {
			return nil, nil
		}
		if shellHookToolEvents[spec.Event] {
			ok, err := spec.MatchesTool(kwargs["tool_name"])
			if err != nil || !ok {
				return nil, err
			}
		}
		payload, err := serializeShellHookPayload(ctx, spec.Event, kwargs)
		if err != nil {
			return nil, err
		}
		r, err := spawnShellHook(ctx, spec, payload)
		if err != nil {
			return nil, err
		}
		if d := evaluateShellHookResult(spec, r); d != nil {
			return d, nil
		}
		return nil, nil
	}
}

func shellHookFailClosedBlock(spec *ShellHookSpec, reason string) map[string]any {
	return map[string]any{"action": "block", "message": fmt.Sprintf("hook %s failed closed: %s", spec.Command, reason)}
}

// Hook contribution of one run, failing open unless a fail closed blocking hook produced no directive
func evaluateShellHookResult(spec *ShellHookSpec, r *ShellHookResult) map[string]any {
	blocking := shellHookBlockingEvents[spec.Event]
	failClosed := spec.FailClosed && blocking
	if r.Error != "" {
		pkgLog().Warn(fmt.Sprintf("shell hook failed (event=%s command=%s): %s", spec.Event, spec.Command, r.Error))
	} else if r.TimedOut {
		pkgLog().Warn(fmt.Sprintf("shell hook timed out after %.2fs (event=%s command=%s)", r.ElapsedSeconds, spec.Event, spec.Command))
	}
	if r.Error != "" || r.TimedOut {
		if !failClosed {
			return nil
		}
		reason := r.Error
		if reason == "" {
			reason = fmt.Sprintf("timed out after %ds", spec.Timeout)
		}
		return shellHookFailClosedBlock(spec, reason)
	}
	stderr := strings.TrimSpace(r.Stderr)
	limited := textutil.First(stderr, shellHookStderrLimit)
	if stderr != "" {
		pkgLog().Debug(fmt.Sprintf("shell hook stderr (event=%s command=%s): %s", spec.Event, spec.Command, limited))
	}
	code := 0
	if r.ReturnCode != nil {
		code = *r.ReturnCode
	}
	if code == ShellHookBlockExitCode && blocking {
		if parsed := parseShellHookResponse(spec.Event, r.Stdout); parsed != nil && parsed["action"] == "block" {
			return parsed
		}
		message := limited
		if message == "" {
			message = shellHookDefaultBlockMessage
		}
		pkgLog().Info(fmt.Sprintf("shell hook exited %d — blocking (event=%s command=%s): %s", ShellHookBlockExitCode, spec.Event, spec.Command, message))
		return map[string]any{"action": "block", "message": message}
	}
	if code != 0 {
		pkgLog().Warn(fmt.Sprintf("shell hook exited %d (event=%s command=%s); stderr=%s", code, spec.Event, spec.Command, limited))
	}
	stdout := strings.TrimSpace(r.Stdout)
	parsed := parseShellHookResponse(spec.Event, stdout)
	if parsed == nil && failClosed && code != 0 {
		return shellHookFailClosedBlock(spec, fmt.Sprintf("hook exited %d with no directive", code))
	}
	if parsed == nil && failClosed && stdout != "" && !shellHookIsJSONObject(stdout) {
		return shellHookFailClosedBlock(spec, "unparseable stdout (expected a JSON object)")
	}
	return parsed
}

func shellHookIsJSONObject(text string) bool {
	v, err := jsonutil.Decode(text)
	_, ok := v.(*jsonutil.Object)
	return err == nil && ok
}

// Validated string block message, primary first, else the default
func shellHookBlockMessage(primary, secondary any) string {
	raw := primary
	if !anyutil.IsTrue(raw) {
		raw = secondary
	}
	if s, ok := raw.(string); ok && s != "" {
		return s
	}
	return shellHookDefaultBlockMessage
}

// Block or modify in either dialect, or the approve escalation, from a pre_tool_call answer
func parseShellPreToolCall(data map[string]any) map[string]any {
	for _, d := range shellHookPreToolDialects {
		if data[d[0]] == "block" {
			return map[string]any{"action": "block", "message": shellHookBlockMessage(data[d[1]], data[d[2]])}
		}
	}
	for _, d := range shellHookPreToolDialects {
		if payload, ok := data[d[3]].(map[string]any); ok && data[d[0]] == "modify" {
			return map[string]any{"action": "modify", "args": payload}
		}
	}
	if data["action"] == "approve" {
		directive := map[string]any{"action": "approve"}
		for _, key := range []string{"message", "rule_key"} {
			if v, ok := data[key].(string); ok && strings.TrimSpace(v) != "" {
				directive[key] = strings.TrimSpace(v)
			}
		}
		return directive
	}
	return nil
}

// Continue nudge from a pre_verify answer, nil without a message
func parseShellPreVerify(data map[string]any) map[string]any {
	actionRaw := data["action"]
	if !anyutil.IsTrue(actionRaw) {
		actionRaw = data["decision"]
	}
	action := ""
	if anyutil.IsTrue(actionRaw) {
		action = strings.ToLower(strings.TrimSpace(anyutil.String(actionRaw)))
	}
	message := data["message"]
	if !anyutil.IsTrue(message) {
		message = data["reason"]
	}
	if m, ok := message.(string); ok && (action == "continue" || action == "block") && strings.TrimSpace(m) != "" {
		return map[string]any{"action": "continue", "message": strings.TrimSpace(m)}
	}
	return nil
}

func parseShellContext(data map[string]any) map[string]any {
	if c, ok := data["context"].(string); ok && strings.TrimSpace(c) != "" {
		return map[string]any{"context": c}
	}
	return nil
}

// Turn veto in either dialect, else injected context
func parseShellPreLLMCall(data map[string]any) map[string]any {
	for _, d := range shellHookPreToolDialects {
		if data[d[0]] == "block" {
			return map[string]any{"action": "block", "message": shellHookBlockMessage(data[d[1]], data[d[2]])}
		}
	}
	return parseShellContext(data)
}

// Wire shaped directive of a hook's stdout JSON, nil when empty, invalid or not an object
func parseShellHookResponse(event, stdout string) map[string]any {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return nil
	}
	v, err := jsonutil.Decode(stdout)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("shell hook stdout was not valid JSON (event=%s): %s", event, textutil.First(stdout, 200)))
		return nil
	}
	obj, ok := v.(*jsonutil.Object)
	if !ok {
		return nil
	}
	data, _ := jsonutil.Plain(obj).(map[string]any)
	switch event {
	case "pre_tool_call":
		return parseShellPreToolCall(data)
	case "pre_verify":
		return parseShellPreVerify(data)
	case "pre_llm_call":
		return parseShellPreLLMCall(data)
	}
	return parseShellContext(data)
}

// Path of the current home's shell hook allowlist
func ShellHookAllowlistPath(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), ShellHookAllowlistFilename)
}

// Parsed allowlist and whether it can be trusted, a missing file readable and a broken one not
func readShellHookAllowlist(ctx context.Context) (map[string]any, bool) {
	empty := func() map[string]any { return map[string]any{"approvals": []any{}} }
	raw, err := os.ReadFile(ShellHookAllowlistPath(ctx))
	if err != nil {
		return empty(), os.IsNotExist(err)
	}
	text, err := textutil.DecodeUTF8(textutil.TrimBOM(raw))
	if err != nil {
		return empty(), false
	}
	v, err := jsonutil.Decode(text)
	if err != nil {
		return empty(), false
	}
	data, ok := jsonutil.Plain(v).(map[string]any)
	if !ok {
		return empty(), false
	}
	_, readable := data["approvals"].([]any)
	if !readable {
		data["approvals"] = []any{}
	}
	return data, readable
}

// Parsed allowlist, an empty skeleton when absent or broken
func LoadShellHookAllowlist(ctx context.Context) map[string]any {
	data, _ := readShellHookAllowlist(ctx)
	return data
}

// Writes the allowlist atomically at mode 0600, an OS failure logged and the approval held in memory only
func SaveShellHookAllowlist(ctx context.Context, data map[string]any) error {
	p := ShellHookAllowlistPath(ctx)
	err := shellHookAtomicWrite(ctx, p, data, 0o600, jsonutil.Indent(2), jsonutil.SortKeys())
	if errorutil.IsSystem(err) {
		pkgLog().Warn(fmt.Sprintf("Failed to persist shell hook allowlist to %s: %s. The approval holds for this process "+
			"only (a gateway keeps a launch's --accept-hooks until it restarts); a gateway session "+
			"opened from a CLI prompt skips the hook, and the next run re-prompts.", p, errchain.OSText(err)))
		return nil
	}
	return err
}

// Read modify write of the allowlist serialised across processes by a sibling flock file
func lockedUpdateShellHookApprovals(ctx context.Context, update func(data map[string]any)) error {
	p := ShellHookAllowlistPath(ctx)
	if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
		return err
	}
	release, err := lockShellHookAllowlist(p)
	if err != nil {
		return err
	}
	defer release()
	data := LoadShellHookAllowlist(ctx)
	update(data)
	return SaveShellHookAllowlist(ctx, data)
}

// Approves an unseen pair by opt in or a terminal prompt, true only when granted
func promptAndRecordShellHook(ctx context.Context, event, command string, acceptHooks bool) (bool, error) {
	if acceptHooks {
		if err := recordShellHookApproval(ctx, event, command); err != nil {
			return false, err
		}
		pkgLog().Info(fmt.Sprintf("shell hook auto-approved via --accept-hooks / env / config: %s -> %s", event, command))
		return true, nil
	}
	if !shellHookStdinIsTTY() {
		return false, nil
	}
	fmt.Fprintf(shellHookStdout, "\n⚠ Icebath is about to register a shell hook that will run a\n  command on your behalf.\n\n"+
		"    Event:   %s\n    Command: %s\n\n"+
		"  Commands run with your full user credentials.  Only approve\n  commands you trust.\n", event, command)
	fmt.Fprint(shellHookStdout, "Allow this hook to run? [y/N]: ")
	line, err := shellHookReadLine()
	if err != nil {
		fmt.Fprintln(shellHookStdout)
		return false, nil
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	if answer != "y" && answer != "yes" {
		return false, nil
	}
	if err := recordShellHookApproval(ctx, event, command); err != nil {
		return false, err
	}
	return true, nil
}

// Replaces any record of the pair with a fresh approval carrying the script mtime
func recordShellHookApproval(ctx context.Context, event, command string) error {
	entry := map[string]any{"event": event, "command": command, "approved_at": shellHookISO(time.Now()), "script_mtime_at_approval": nil}
	if mtime, ok := ShellHookScriptMtimeISO(command); ok {
		entry["script_mtime_at_approval"] = mtime
	}
	return lockedUpdateShellHookApprovals(ctx, func(data map[string]any) {
		kept := []any{}
		list, _ := data["approvals"].([]any)
		for _, e := range list {
			if !shellHookEntryMatches(e, &event, command) {
				kept = append(kept, e)
			}
		}
		data["approvals"] = append(kept, entry)
	})
}

// Removes every allowlist record of the command and returns how many went
func RevokeShellHook(ctx context.Context, command string) (int, error) {
	removed := 0
	err := lockedUpdateShellHookApprovals(ctx, func(data map[string]any) {
		list, _ := data["approvals"].([]any)
		kept := []any{}
		for _, e := range list {
			if !shellHookEntryMatches(e, nil, command) {
				kept = append(kept, e)
			}
		}
		removed = len(list) - len(kept)
		data["approvals"] = kept
	})
	return removed, err
}

// First token with a script suffix, else the first path like token, else the first token
func shellHookScriptPath(command string) string {
	parts, err := shellutil.SplitCommandLine(command)
	if err != nil {
		return command
	}
	if len(parts) == 0 {
		parts = []string{command}
	}
	for _, p := range parts {
		lower := strings.ToLower(p)
		for _, ext := range shellHookScriptExtensions {
			if strings.HasSuffix(lower, ext) {
				return p
			}
		}
	}
	for _, p := range parts {
		if strings.Contains(p, "/") || strings.HasPrefix(p, "~") {
			return p
		}
	}
	return parts[0]
}

// Whether any opt in channel approves every hook: the argument, the process flag, or hooks_auto_accept of ctx with its ICEBATH_ACCEPT_HOOKS override
func resolveShellHookAccept(ctx context.Context, acceptArg bool) bool {
	return acceptArg || shellHookProcessAccept.Load() || settings.HooksAutoAccept.Get(ctx)
}

// Allowlist record of the pair, nil when none
func ShellHookAllowlistEntry(ctx context.Context, event, command string) map[string]any {
	list, _ := LoadShellHookAllowlist(ctx)["approvals"].([]any)
	for _, e := range list {
		if shellHookEntryMatches(e, &event, command) {
			m, _ := shellHookDict(e)
			return m
		}
	}
	return nil
}

// ISO mtime of the command's resolved script, false when it is missing
func ShellHookScriptMtimeISO(command string) (string, bool) {
	path := shellHookScriptPath(command)
	if path == "" {
		return "", false
	}
	fi, err := os.Stat(pathutil.ExpandUser(path))
	if err != nil {
		return "", false
	}
	mt := fi.ModTime()
	ts := float64(mt.Unix()) + float64(mt.Nanosecond())*1e-9
	return shellHookISO(timeutil.FromEpoch(ts)), true
}

// Whether the command runs as configured, a bare script needing execute and an interpreted one read access
func ShellHookScriptIsExecutable(command string) bool {
	path := shellHookScriptPath(command)
	expanded := pathutil.ExpandUser(path)
	if path == "" || !isFile(expanded) {
		return false
	}
	argv, err := shellutil.SplitCommandLine(command)
	if err != nil {
		return false
	}
	if len(argv) > 0 && argv[0] == path {
		return shellHookAccess(expanded, true)
	}
	return shellHookAccess(expanded, false)
}

// Fires one hook with a synthetic payload through the production path, the parsed directive attached
func RunShellHookOnce(ctx context.Context, spec *ShellHookSpec, kwargs map[string]any) (*ShellHookResult, error) {
	payload, err := serializeShellHookPayload(ctx, spec.Event, kwargs)
	if err != nil {
		return nil, err
	}
	r, err := spawnShellHook(ctx, spec, payload)
	if err != nil {
		return nil, err
	}
	r.Parsed = evaluateShellHookResult(spec, r)
	return r, nil
}
