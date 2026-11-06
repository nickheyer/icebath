package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

// The Advanced modal keys, every other answer key being a credential
var catalogOptionKeys = map[string]bool{"target_profile": true, "agent_half": true, "desktop_half": true, "enable": true,
	"force": true, "ref": true}

var (
	catalogCommitSHA = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	catalogTiers     = map[string]bool{"official": true, "verified": true, "community": true}
	catalogDashes    = regexp.MustCompile(`[-_]+`)
)

// The tui gateway launch home, the named profile home lookup and the session profile runtime scope, set by the arena slice
var (
	TUILaunchHome                 func() string
	TUIProfileHome                func(ctx context.Context, profile string) (string, error)
	TUISessionProfileRuntimeScope func(ctx context.Context, session map[string]any) (context.Context, func())
)

func catalogFlag(value string, def bool) bool {
	if value == "" {
		return def
	}
	return value == "1"
}

// Text up to and including the first sentence end, whitespace collapsed
func catalogFirstSentence(text string) string {
	text = strings.Join(pystr.SplitWhitespace(text, -1), " ")
	if head, _, ok := strings.Cut(text, ". "); ok {
		return head + "."
	}
	return text
}

// Title cased last path segment of an id, the id itself when nothing is left
func catalogDisplay(identifier string) string {
	leaf := strings.TrimRight(identifier, "/")
	if i := strings.LastIndex(leaf, "/"); i >= 0 {
		leaf = leaf[i+1:]
	}
	var parts []string
	for _, part := range catalogDashes.Split(leaf, -1) {
		if part != "" {
			parts = append(parts, pystr.Capitalize(part))
		}
	}
	if len(parts) == 0 {
		return identifier
	}
	return strings.Join(parts, " ")
}

func catalogSameHome(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	if errA != nil {
		ra, _ = filepath.Abs(a)
	}
	rb, errB := filepath.EvalSymlinks(b)
	if errB != nil {
		rb, _ = filepath.Abs(b)
	}
	return ra == rb
}

// Binds the profile at h with its home, secrets and terminal policy the way an RPC for that profile does
func CatalogTargetScope(ctx context.Context, h string) (context.Context, func()) {
	launch := TUILaunchHome()
	if catalogSameHome(h, launch) {
		return provider.WithLaunchProfileRuntimeScope(ctx, launch), func() {}
	}
	return TUISessionProfileRuntimeScope(ctx, map[string]any{"profile_home": h})
}

// Home of the profile the Advanced modal named, an error for one that does not exist
func catalogNamedHome(ctx context.Context, name string) (string, error) {
	h, err := TUIProfileHome(ctx, name)
	if err != nil {
		return "", err
	}
	if h == "" {
		return TUILaunchHome(), nil
	}
	return h, nil
}

// CatalogSkillMeta is the hub metadata of a skill row, never on the wire
type CatalogSkillMeta struct {
	Name, Description, Source, Identifier string
}

// CatalogInstaller is the host side of a catalog row, replaced by tests
type CatalogInstaller interface {
	PluginEntry(ctx context.Context, name string) (*plugins.PluginCatalogEntry, error)
	Refuse(ctx context.Context, entry *plugins.PluginCatalogEntry) error
	InstallPlugin(ctx context.Context, name string, force, enable bool, ref string, onStep func(plugins.InstallPhase)) (map[string]any, error)
	SkillMeta(ctx context.Context, identifier string) (*CatalogSkillMeta, error)
	InstallSkill(ctx context.Context, identifier string, force bool, source string) (map[string]any, error)
}

// HostInstaller reads the real plugin catalog, skills hub and installers
type HostInstaller struct{}

func (HostInstaller) PluginEntry(ctx context.Context, name string) (*plugins.PluginCatalogEntry, error) {
	return plugins.GetLiveCatalogEntry(ctx, name), nil
}

// Error carrying the installer's own text when the catalog would refuse this entry here
func (HostInstaller) Refuse(ctx context.Context, entry *plugins.PluginCatalogEntry) error {
	if err := plugins.RaiseIfRemoved(ctx, entry.Name, entry.Repo); err != nil {
		return err
	}
	return plugins.RefuseIncompatibleCatalogEntry(ctx, entry)
}

func (HostInstaller) InstallPlugin(ctx context.Context, name string, force, enable bool, ref string, onStep func(plugins.InstallPhase)) (map[string]any, error) {
	return plugins.DashboardInstallPlugin(ctx, plugins.DashboardInstallOptions{Force: force, Enable: enable, CatalogName: name,
		Ref: ref, OnStep: onStep})
}

// Metadata from the first hub source that knows the identifier, no bundle download
func (HostInstaller) SkillMeta(ctx context.Context, identifier string) (*CatalogSkillMeta, error) {
	x := skills.NewHubCLI(skills.NewScope(home.Dir(ctx), nil))
	sctx, done := x.Hub.HTTPSession(ctx)
	defer done()
	for _, source := range x.ResolvedSources() {
		meta, err := source.Inspect(sctx, identifier)
		if err != nil || meta == nil {
			continue
		}
		id := meta.Identifier
		if id == "" {
			id = identifier
		}
		return &CatalogSkillMeta{Name: meta.Name, Description: meta.Description, Source: meta.Source, Identifier: id}, nil
	}
	return nil, nil
}

// Hub install seam the tests replace
var catalogDoInstall = func(ctx context.Context, x *skills.HubCLI, identifier string, o skills.HubInstallOptions) (skills.HubOutcome, error) {
	return x.DoInstall(ctx, identifier, o)
}

// Installs headless, success read from the hub lock and failure from the last printed line
func (HostInstaller) InstallSkill(ctx context.Context, identifier string, force bool, source string) (map[string]any, error) {
	scope := skills.NewScope(home.Dir(ctx), nil)
	x := skills.NewHubCLI(scope)
	entry := func() (*pyjson.Object, error) {
		installed, err := x.Hub.LockFile("").ByIdentifier()
		if err != nil {
			return nil, err
		}
		return skills.InstalledEntry(installed, identifier, source), nil
	}
	before, err := entry()
	if err != nil {
		return nil, err
	}
	if before != nil && !force {
		name, _ := before.Get("name")
		return map[string]any{"name": pyrepr.Str(name), "already_installed": true}, nil
	}
	var out bytes.Buffer
	console := &pyrich.Console{Out: &out, Width: 200, NoColor: true, NoHighlight: true}
	verdict, err := catalogDoInstall(ctx, x, identifier, skills.HubInstallOptions{Force: force, SkipConfirm: true, Console: console})
	if err != nil {
		return nil, err
	}
	after, err := entry()
	if err != nil {
		return nil, err
	}
	if after == nil && verdict != skills.HubFailed {
		builtin, err := scope.BundledSkillForInstall(identifier)
		if err != nil {
			return nil, err
		}
		if builtin != "" {
			return map[string]any{"name": builtin, "already_installed": verdict == skills.HubNoop}, nil
		}
	}
	if after == nil || (before != nil && catalogSameStamp(after, before)) {
		var lines []string
		for _, line := range pystr.Splitlines(out.String(), false) {
			if line = pystr.Strip(line); line != "" {
				lines = append(lines, line)
			}
		}
		if len(lines) == 0 {
			return nil, errors.New("the skill was not installed")
		}
		return nil, errors.New(lines[len(lines)-1])
	}
	name, _ := after.Get("name")
	return map[string]any{"name": pyrepr.Str(name), "already_installed": false}, nil
}

func catalogSameStamp(a, b *pyjson.Object) bool {
	x, _ := a.Get("updated_at")
	y, _ := b.Get("updated_at")
	return pyrepr.Repr(x) == pyrepr.Repr(y)
}

type catalogWork struct {
	done    chan struct{}
	outcome map[string]any
	err     string
}

func (w *catalogWork) isDone() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

// CatalogRunner holds the resolved catalog facts and install work of one operation's rows
type CatalogRunner struct {
	installer CatalogInstaller
	env       *tools.Env

	mu          sync.Mutex
	opID        string
	facts       map[string]any
	work        map[string]*catalogWork
	approvedEnv map[string]*pyjson.Object
	home        string
	label       string
}

var (
	catalogRunnersMu sync.Mutex
	catalogRunners   = map[string]*CatalogRunner{}
)

// Runner over installer built on the tool call's context, its home the default install target
func OpenCatalogRunner(ctx context.Context, env *tools.Env, installer CatalogInstaller) *CatalogRunner {
	if installer == nil {
		installer = HostInstaller{}
	}
	h := home.Dir(ctx)
	return &CatalogRunner{installer: installer, env: env, facts: map[string]any{}, work: map[string]*catalogWork{},
		approvedEnv: map[string]*pyjson.Object{}, home: h, label: home.ProfileNameFor(h)}
}

func (r *CatalogRunner) labelValue() any {
	if r.label == "" {
		return nil
	}
	return r.label
}

func (r *CatalogRunner) fact(name string) (any, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.facts[name]
	return f, ok
}

func catalogExtraOf(op *tools.ConnectionOperation, t *tools.ConnTarget) *pyjson.Object {
	var extra *pyjson.Object
	op.UpdateTarget(t, func(t *tools.ConnTarget) {
		if t.Extra != nil {
			extra = t.Extra.Copy()
		}
	})
	if extra == nil {
		extra = pyjson.NewObject()
	}
	return extra
}

// Resolves each id and draws its row, a failure drawn with its reason
func (r *CatalogRunner) Prepare(ctx context.Context, op *tools.ConnectionOperation) {
	catalogRunnersMu.Lock()
	catalogRunners[op.OpID] = r
	catalogRunnersMu.Unlock()
	r.mu.Lock()
	r.opID = op.OpID
	r.mu.Unlock()
	for _, t := range op.Targets {
		extra := pyjson.ObjectOf("display", catalogDisplay(t.Name), "target_profile", r.labelValue())
		op.UpdateTarget(t, func(t *tools.ConnTarget) { t.Extra = extra })
		if err := r.resolve(ctx, op, t); err != nil {
			connFail(op, t, r.detail(err, t))
		}
	}
}

func (r *CatalogRunner) resolve(ctx context.Context, op *tools.ConnectionOperation, t *tools.ConnTarget) error {
	if t.Kind == "plugin" {
		entry, err := r.installer.PluginEntry(ctx, t.Name)
		if err != nil {
			return err
		}
		if entry == nil {
			return fmt.Errorf("'%s' is not in the Icebath plugin catalog", t.Name)
		}
		r.mu.Lock()
		r.facts[t.Name] = entry
		r.mu.Unlock()
		row, err := catalogPluginRow(ctx, entry, r.labelValue())
		if err != nil {
			return err
		}
		required := make([]*pyjson.Object, 0, len(entry.Capabilities.RequiresEnv))
		for _, name := range entry.Capabilities.RequiresEnv {
			required = append(required, pyjson.ObjectOf("name", name, "required", false, "secret", true, "default", ""))
		}
		op.UpdateTarget(t, func(t *tools.ConnTarget) { t.Extra, t.RequiredEnv = row, required })
		return r.installer.Refuse(ctx, entry)
	}
	meta, err := r.installer.SkillMeta(ctx, t.Name)
	if err != nil {
		return err
	}
	if meta == nil {
		return fmt.Errorf("'%s' was not found in the skills hub", t.Name)
	}
	r.mu.Lock()
	r.facts[t.Name] = meta
	r.mu.Unlock()
	display := meta.Name
	if display == "" {
		display = catalogDisplay(t.Name)
	}
	tier := "community"
	if meta.Source == "official" {
		tier = "official"
	}
	extra := pyjson.ObjectOf("display", display, "description", catalogFirstSentence(meta.Description), "tier", tier,
		"target_profile", r.labelValue())
	op.UpdateTarget(t, func(t *tools.ConnTarget) { t.Extra = extra })
	return nil
}

// Approves a row with the Advanced values, nil env reusing what the user approved before
func (r *CatalogRunner) approve(ctx context.Context, op *tools.ConnectionOperation, t *tools.ConnTarget, env *pyjson.Object) {
	r.mu.Lock()
	approved := pyjson.NewObject()
	if prev := r.approvedEnv[t.Name]; prev != nil {
		approved = prev.Copy()
	}
	r.mu.Unlock()
	if env != nil {
		for _, k := range env.Keys {
			approved.Set(k, env.Vals[k])
		}
	}
	actor := tools.ActorBackendWatcher
	if op.StateOf(t) == tools.TargetFailed {
		actor = tools.ActorUser
	}
	if _, ok := r.fact(t.Name); !ok {
		if err := r.resolve(ctx, op, t); err != nil {
			connFail(op, t, r.detail(err, t))
			return
		}
	}
	if msg := r.checkAnswer(t, approved); msg != "" {
		connFail(op, t, msg)
		return
	}
	r.mu.Lock()
	r.approvedEnv[t.Name] = approved
	r.mu.Unlock()
	options := pyjson.ObjectOf("force", catalogFlag(catalogGet(approved, "force"), false),
		"enable", catalogFlag(catalogGet(approved, "enable"), true), "ref", nil)
	if ref := catalogGet(approved, "ref"); ref != "" {
		options.Set("ref", ref)
	}
	extra := catalogExtraOf(op, t)
	if profile := pystr.Strip(catalogGet(approved, "target_profile")); profile != "" {
		extra.Set("target_profile", profile)
	} else {
		extra.Set("target_profile", r.labelValue())
	}
	extra.Set("approved", options)
	empty := ""
	if !connMove(op, t, tools.TargetInitiated, actor, tools.TransitionFields{Detail: &empty, Extra: extra}) {
		return
	}
	r.spawn(ctx, op, t, approved, options)
}

func catalogGet(o *pyjson.Object, key string) string {
	v, _ := o.Get(key)
	s, _ := v.(string)
	return s
}

func (r *CatalogRunner) checkAnswer(t *tools.ConnTarget, env *pyjson.Object) string {
	if t.Kind == "plugin" && catalogGet(env, "agent_half") == "0" {
		return "only the desktop half was selected; install it from Settings, Plugins"
	}
	if ref := catalogGet(env, "ref"); ref != "" && !catalogCommitSHA.MatchString(ref) {
		return "the pin must be a full 40-character commit SHA"
	}
	fact, _ := r.fact(t.Name)
	declared := map[string]bool{}
	for _, name := range catalogDeclaredEnv(fact) {
		declared[name] = true
	}
	var undeclared []string
	for _, k := range env.Keys {
		if !catalogOptionKeys[k] && !declared[k] {
			undeclared = append(undeclared, k)
		}
	}
	if len(undeclared) > 0 {
		sort.Strings(undeclared)
		return fmt.Sprintf("'%s' does not declare %s", t.Name, strings.Join(undeclared, ", "))
	}
	return ""
}

// Env names a resolved catalog fact declares, none for a skill
func catalogDeclaredEnv(fact any) []string {
	if entry, ok := fact.(*plugins.PluginCatalogEntry); ok && entry != nil {
		return append([]string(nil), entry.Capabilities.RequiresEnv...)
	}
	return nil
}

func (r *CatalogRunner) spawn(ctx context.Context, op *tools.ConnectionOperation, t *tools.ConnTarget, env, options *pyjson.Object) {
	work := &catalogWork{done: make(chan struct{})}
	r.mu.Lock()
	r.work[t.Name] = work
	r.mu.Unlock()
	step := func(phase plugins.InstallPhase) {
		if !op.Settled() && op.StateOf(t) == tools.TargetInitiated {
			_ = op.Refresh(t.Name, "", "", tools.ActorBackendWatcher, string(phase))
		}
	}
	wctx := context.WithoutCancel(ctx)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				work.outcome, work.err = nil, r.detail(fmt.Errorf("%v", p), t)
			}
			close(work.done)
			op.Wake.Set()
		}()
		outcome, err := r.install(wctx, t, env, options, step)
		if err != nil {
			work.err = r.detail(err, t)
			return
		}
		work.outcome = outcome
	}()
}

func (r *CatalogRunner) install(ctx context.Context, t *tools.ConnTarget, env, options *pyjson.Object, step func(plugins.InstallPhase)) (map[string]any, error) {
	h := r.home
	if named := pystr.Strip(catalogGet(env, "target_profile")); named != "" {
		var err error
		if h, err = catalogNamedHome(ctx, named); err != nil {
			return nil, err
		}
	}
	force, _ := options.Get("force")
	enable, _ := options.Get("enable")
	ref, _ := options.Get("ref")
	refText, _ := ref.(string)
	result, done, err := func() (map[string]any, bool, error) {
		sctx, release := CatalogTargetScope(ctx, h)
		defer release()
		creds := pyjson.NewObject()
		for _, k := range env.Keys {
			if v := catalogGet(env, k); !catalogOptionKeys[k] && v != "" {
				creds.Set(k, v)
			}
		}
		if err := catalogSaveCredentials(sctx, creds); err != nil {
			return nil, true, err
		}
		if t.Kind == "skill" {
			fact, _ := r.fact(t.Name)
			meta := fact.(*CatalogSkillMeta)
			identifier := meta.Identifier
			if identifier == "" {
				identifier = t.Name
			}
			step(plugins.InstallPhaseDownloading)
			out, err := r.installer.InstallSkill(sctx, identifier, force.(bool), meta.Source)
			return out, true, err
		}
		out, err := r.installer.InstallPlugin(sctx, t.Name, force.(bool), enable.(bool), refText, step)
		return out, err != nil, err
	}()
	if done {
		return result, err
	}
	if !pyrepr.Truthy(result["ok"]) {
		if msg := result["error"]; pyrepr.Truthy(msg) {
			return nil, errors.New(pyrepr.Str(msg))
		}
		return nil, errors.New("the install failed")
	}
	out := map[string]any{"enabled": enable}
	for k, v := range result {
		out[k] = v
	}
	return out, nil
}

// Moves each finished install to connected with what went live, or to failed with its reason
func (r *CatalogRunner) Observe(op *tools.ConnectionOperation) {
	for _, t := range op.Targets {
		r.mu.Lock()
		work := r.work[t.Name]
		r.mu.Unlock()
		if op.Settled() || op.StateOf(t) != tools.TargetInitiated || work == nil || !work.isDone() {
			continue
		}
		r.mu.Lock()
		delete(r.work, t.Name)
		r.mu.Unlock()
		if work.err != "" {
			connFail(op, t, work.err)
			continue
		}
		extra, detail := catalogInstalledRow(t, catalogExtraOf(op, t), work.outcome)
		connMove(op, t, tools.TargetConnected, tools.ActorBackendWatcher, tools.TransitionFields{Detail: &detail, Extra: extra})
	}
}

// Drops the runner from the answer registry
func (r *CatalogRunner) Close() {
	r.mu.Lock()
	id := r.opID
	r.mu.Unlock()
	if id != "" {
		catalogRunnersMu.Lock()
		delete(catalogRunners, id)
		catalogRunnersMu.Unlock()
	}
}

// Error text with approved credentials and secrets redacted, the Advanced choices left readable
func (r *CatalogRunner) detail(err error, t *tools.ConnTarget) string {
	text := err.Error()
	if text == "" {
		text = fmt.Sprintf("%T", err)
	}
	r.mu.Lock()
	approved := r.approvedEnv[t.Name]
	r.mu.Unlock()
	if approved != nil {
		for _, k := range approved.Keys {
			if v := catalogGet(approved, k); !catalogOptionKeys[k] && v != "" && utf8.RuneCountInString(v) > 3 {
				text = strings.ReplaceAll(text, v, "[REDACTED]")
			}
		}
	}
	return orText(tools.RedactSensitiveTextWith(r.env, text, tools.RedactOptions{Force: true}), "error")
}

// Row a catalog plugin draws, its presence read from the pinned manifest
func catalogPluginRow(ctx context.Context, entry *plugins.PluginCatalogEntry, label any) (*pyjson.Object, error) {
	presence, err := plugins.CatalogPresence(ctx, entry)
	if err != nil {
		return nil, err
	}
	display := entry.Title
	if display == "" {
		display = catalogDisplay(entry.Name)
	}
	tier := entry.Tier
	if !catalogTiers[tier] {
		tier = "community"
	}
	var requires any
	if entry.RequiresIcebath != "" {
		requires = entry.RequiresIcebath
	}
	row := pyjson.ObjectOf("display", display, "description", catalogFirstSentence(entry.Description), "tier", tier,
		"repo", entry.Repo, "sha", entry.Sha, "requires_icebath", requires, "has_desktop_half", false, "target_profile", label,
		"app_state", presence.State)
	if len(entry.Platforms) > 0 {
		platforms := make([]any, len(entry.Platforms))
		for i, p := range entry.Platforms {
			platforms[i] = p
		}
		row.Set("platforms", platforms)
	}
	if entry.Subdir != "" {
		row.Set("subdir", entry.Subdir)
	}
	return row, nil
}

func catalogList(v any) []any {
	switch l := v.(type) {
	case []any:
		return l
	case []map[string]any:
		out := make([]any, len(l))
		for i, x := range l {
			out[i] = x
		}
		return out
	case []string:
		out := make([]any, len(l))
		for i, x := range l {
			out[i] = x
		}
		return out
	case []*pyjson.Object:
		out := make([]any, len(l))
		for i, x := range l {
			out[i] = x
		}
		return out
	}
	return nil
}

func catalogDict(v any) map[string]any {
	switch d := v.(type) {
	case map[string]any:
		return d
	case *pyjson.Object:
		if d != nil {
			return d.Vals
		}
	}
	return nil
}

// Connected row fields with what went live and the same notes in one line for the model
func catalogInstalledRow(t *tools.ConnTarget, extra *pyjson.Object, outcome map[string]any) (*pyjson.Object, string) {
	if t.Kind == "skill" {
		already := pyrepr.Truthy(outcome["already_installed"])
		detail := ""
		if already {
			detail = "already installed; left as it is (Advanced, force reinstall replaces it)"
		}
		extra.Set("skill", outcome["name"])
		extra.Set("tools", []any{})
		extra.Set("already_installed", already)
		return extra, detail
	}
	live := catalogDict(catalogDict(outcome["activation"])["live_now"])
	toolNames := []any{}
	serverErrors := []any{}
	var notes []string
	for _, s := range catalogList(live["mcp_servers"]) {
		server := catalogDict(s)
		if pyrepr.Truthy(server["connected"]) {
			toolNames = append(toolNames, catalogList(server["tools"])...)
			continue
		}
		errText := ""
		if pyrepr.Truthy(server["error"]) {
			errText = pyrepr.Str(server["error"])
		}
		serverErrors = append(serverErrors, pyjson.ObjectOf("name", server["name"], "error", errText))
		shown := errText
		if shown == "" {
			shown = "unknown error"
		}
		notes = append(notes, fmt.Sprintf("MCP server %s not connected: %s", pyrepr.Str(server["name"]), shown))
	}
	enabled := true
	if v, ok := outcome["enabled"]; ok {
		enabled = pyrepr.Truthy(v)
	}
	missing := append([]any{}, catalogList(outcome["missing_env"])...)
	if !enabled {
		notes = append(notes, "installed but not enabled")
	}
	if len(missing) > 0 {
		names := make([]string, len(missing))
		for i, m := range missing {
			names[i] = pyrepr.Str(m)
		}
		notes = append(notes, fmt.Sprintf("set %s to finish setup", strings.Join(names, ", ")))
	}
	for _, s := range catalogList(live["skills"]) {
		if name := catalogDict(s)["name"]; pyrepr.Truthy(name) {
			extra.Set("skill", name)
			break
		}
	}
	extra.Set("tools", toolNames)
	extra.Set("enabled", enabled)
	extra.Set("missing_env", missing)
	extra.Set("server_errors", serverErrors)
	return extra, strings.Join(notes, "; ")
}

// Writes each credential into the bound profile's .env after the writer name check
func catalogSaveCredentials(ctx context.Context, env *pyjson.Object) error {
	for _, k := range env.Keys {
		if err := config.ValidateEnvNameForWrite(k); err != nil {
			return err
		}
		if err := config.SaveEnvValueContext(ctx, home.Dir(ctx), k, catalogGet(env, k)); err != nil {
			return err
		}
	}
	return nil
}

// Reports whether a catalog runner drives the operation
func CatalogOwns(opID string) bool {
	catalogRunnersMu.Lock()
	defer catalogRunnersMu.Unlock()
	_, ok := catalogRunners[opID]
	return ok
}

func catalogRecover(err *error) {
	if p := recover(); p != nil {
		it, ok := p.(*tools.IllegalTransition)
		if !ok {
			panic(p)
		}
		*err = it
	}
}

// Applies the card's answer to a catalog operation, skip, approve, Try again and Continue
func CatalogApplyAnswer(ctx context.Context, op *tools.ConnectionOperation, raw string) (err error) {
	defer catalogRecover(&err)
	v, _ := pyjson.Loads(raw)
	answer, _ := v.(*pyjson.Object)
	if answer == nil {
		answer = pyjson.NewObject()
	}
	catalogRunnersMu.Lock()
	runner := catalogRunners[op.OpID]
	catalogRunnersMu.Unlock()
	rawTargets, _ := answer.Get("targets")
	entries, _ := rawTargets.([]any)
	for _, e := range entries {
		entry, ok := e.(*pyjson.Object)
		if !ok {
			continue
		}
		name, _ := entry.Get("name")
		nameText := ""
		if pyrepr.Truthy(name) {
			nameText = pyrepr.Str(name)
		}
		t := op.Target(pystr.Strip(nameText))
		if t == nil {
			continue
		}
		statusRaw, _ := entry.Get("status")
		status := ""
		if pyrepr.Truthy(statusRaw) {
			status = pystr.Lower(pyrepr.Str(statusRaw))
		}
		state := op.StateOf(t)
		switch {
		case status == "skipped":
			if _, err := op.Transition(t.Name, tools.TargetSkipped, tools.ActorUser, tools.TransitionFields{}); err != nil {
				var it *tools.IllegalTransition
				if !errors.As(err, &it) || (!op.ResolvedTarget(t) && !op.Settled()) {
					return err
				}
			}
		case status == "approved" && runner != nil && (state == tools.TargetPending || state == tools.TargetFailed):
			rawEnv, _ := entry.Get("env")
			var env *pyjson.Object
			if obj, ok := rawEnv.(*pyjson.Object); ok {
				env = pyjson.NewObject()
				for _, k := range obj.Keys {
					env.Set(k, pyrepr.Str(obj.Vals[k]))
				}
			}
			runner.approve(ctx, op, t, env)
		}
	}
	if by, _ := answer.Get("settled_by"); by == string(tools.SettleContinue) && !op.AllResolved() {
		op.Settle(tools.SettleContinue)
	}
	return nil
}

// Approves every named failed row again, the frozen result message once the operation settled
func CatalogRetry(ctx context.Context, op *tools.ConnectionOperation, names []string) (msg string, err error) {
	defer catalogRecover(&err)
	catalogRunnersMu.Lock()
	runner := catalogRunners[op.OpID]
	catalogRunnersMu.Unlock()
	if runner == nil || op.Settled() {
		return "this operation has settled; its result is frozen", nil
	}
	for _, name := range names {
		if t := op.Target(name); t != nil && op.StateOf(t) == tools.TargetFailed {
			runner.approve(ctx, op, t, nil)
		}
	}
	return "", nil
}
