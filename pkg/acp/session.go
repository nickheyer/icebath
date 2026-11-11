package acp

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycopy"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Per session state of one ACP managed agent
type SessionState struct {
	SessionID             string
	Agent                 any
	Cwd                   string
	Model                 string
	History               []map[string]any
	CancelEvent           *pythread.Event
	IsRunning             bool
	CommandOp             bool
	QueuedPrompts         []string
	RuntimeLock           sync.Mutex
	CurrentPromptText     string
	InterruptedPromptText string
	MessageIDs            *AssistantMessageIDAllocator
	Mode                  string
	ConfigOptions         map[string]any
}

// Keywords of one agent build, nil toolsets deriving them from config
type MakeAgentOptions struct {
	SessionID         string
	Cwd               string
	Model             string
	RequestedProvider string
	BaseURL           string
	APIMode           string
	EnabledToolsets   []string
	DisabledToolsets  []string
}

// Builds the session agent from its constructor keywords
var newAIAgent = func(ctx context.Context, o agent.AIAgentOptions) (any, error) {
	a, err := agent.NewAIAgent(ctx, o)
	if err != nil {
		return nil, err
	}
	return a, nil
}

// Effective config of the profile home
var loadSessionConfig = func(ctx context.Context) (config.Tree, error) {
	return config.LoadContext(ctx, home.Dir(ctx), nil)
}

// Runtime route of a requested provider for the target model
var resolveSessionRuntime = func(ctx context.Context, requested, targetModel string) (*provider.Runtime, error) {
	return provider.ResolveRuntimeProvider(ctx, home.Dir(ctx), requested, targetModel, "", "")
}

// Bounded wait for background MCP discovery before an agent snapshots its tools
var ensureMCPDiscoveryBeforeAgentBuild = func(ctx context.Context, cfg config.Tree) {
	env := &tools.Env{Home: home.Dir(ctx), Config: cfg, TerminalScope: tools.GetTerminalScope(ctx)}
	mcp.EnsureDiscoveryBeforeAgentBuild(ctx, env, slog.Default().With("logger", "acp_adapter.session"), nil, false, "acp-mcp-discovery")
}

// Binds a task id to the editor cwd for tools
var registerTaskCwdHook = registerTaskCwd

// Process shared session store of a database path
var acquireSessionDB = state.Acquire

// Thread safe manager of ACP sessions held in memory and persisted to the session store
type SessionManager struct {
	sessions      map[string]*SessionState
	order         []string
	lock          sync.Mutex
	restoreLock   sync.Mutex
	agentFactory  func() any
	dbInstance    *state.SessionDB
	dbLock        sync.Mutex
	cwdBackfilled bool
	restoreFn     func(ctx context.Context, sessionID string) *SessionState
}

// Manager building agents through agentFactory when given and persisting to db, the profile store when nil
func NewSessionManager(agentFactory func() any, db *state.SessionDB) *SessionManager {
	m := &SessionManager{sessions: map[string]*SessionState{}, agentFactory: agentFactory, dbInstance: db}
	m.restoreFn = m.restore
	return m
}

// New session with a unique id and a fresh agent
func (m *SessionManager) CreateSession(ctx context.Context, cwd string) (*SessionState, error) {
	cwd = translateACPCwd(cwd)
	sessionID := uuid.New().String()
	a, err := m.MakeAgent(ctx, MakeAgentOptions{SessionID: sessionID, Cwd: cwd})
	if err != nil {
		return nil, err
	}
	st := m.installState(ctx, sessionID, a, cwd, agentModel(a), []map[string]any{}, true)
	sessionLog.Info(fmt.Sprintf("Created ACP session %s (cwd=%s)", sessionID, cwd))
	return st, nil
}

// Session by id, restored from the store when it is not in memory, nil when unknown
func (m *SessionManager) GetSession(ctx context.Context, sessionID string) *SessionState {
	if st := m.memorySession(sessionID); st != nil {
		return st
	}
	m.restoreLock.Lock()
	defer m.restoreLock.Unlock()
	if st := m.memorySession(sessionID); st != nil {
		return st
	}
	return m.restoreFn(ctx, sessionID)
}

// Session held in memory, nil when absent
func (m *SessionManager) memorySession(sessionID string) *SessionState {
	m.lock.Lock()
	defer m.lock.Unlock()
	return m.sessions[sessionID]
}

// New session carrying a deep copy of another session's history, nil when the original is unknown
func (m *SessionManager) ForkSession(ctx context.Context, sessionID, cwd string) (*SessionState, error) {
	cwd = translateACPCwd(cwd)
	original := m.GetSession(ctx, sessionID)
	if original == nil {
		return nil, nil
	}
	newID := uuid.New().String()
	a, err := m.MakeAgent(ctx, MakeAgentOptions{SessionID: newID, Cwd: cwd, Model: original.Model})
	if err != nil {
		return nil, err
	}
	model := original.Model
	if _, ok := a.(interface{ Model() string }); ok && agentModel(a) != "" {
		model = agentModel(a)
	}
	history, _ := pycopy.DeepCopy(original.History).([]map[string]any)
	if history == nil {
		history = []map[string]any{}
	}
	st := m.installState(ctx, newID, a, cwd, model, history, true)
	sessionLog.Info(fmt.Sprintf("Forked ACP session %s -> %s", sessionID, newID))
	return st, nil
}

// Info rows of every session with history, memory first then the store, newest first
func (m *SessionManager) ListSessions(ctx context.Context, cwd string) ([]*pyjson.Object, error) {
	normalized := ""
	if cwd != "" {
		normalized = normalizeCwdForCompare(cwd)
	}
	persisted, order, err := m.catalogRows(ctx)
	if err != nil {
		return nil, err
	}
	matches := func(sessionCwd string) bool {
		return normalized == "" || normalizeCwdForCompare(sessionCwd) == normalized
	}
	results := []*pyjson.Object{}
	seen := map[string]bool{}
	m.lock.Lock()
	for _, sid := range m.order {
		seen[sid] = true
	}
	for _, sid := range m.order {
		s := m.sessions[sid]
		if len(s.History) == 0 || !matches(s.Cwd) {
			continue
		}
		row := persisted[s.SessionID]
		if row == nil {
			row = pyjson.NewObject()
		}
		preview := ""
		if truthy(row.GetOr("preview", nil)) {
			preview = pyStr(row.GetOr("preview", nil))
		}
		updated := row.GetOr("last_active", nil)
		if !truthy(updated) {
			updated = row.GetOr("started_at", nil)
		}
		if !truthy(updated) {
			updated = float64(time.Now().UnixNano()) / 1e9
		}
		results = append(results, sessionInfo(s.SessionID, s.Cwd, s.Model, len(s.History), row.GetOr("title", nil),
			firstUserPreview(s.History, preview), updated))
	}
	m.lock.Unlock()
	for _, sid := range order {
		row := persisted[sid]
		count, _ := pyFloat(row.GetOr("message_count", nil))
		sessionCwd := pyStr(parseModelConfig(row.GetOr("model_config", nil)).GetOr("cwd", "."))
		if seen[sid] || int(count) <= 0 || !matches(sessionCwd) {
			continue
		}
		var model any = ""
		if truthy(row.GetOr("model", nil)) {
			model = row.GetOr("model", nil)
		}
		updated := row.GetOr("last_active", nil)
		if !truthy(updated) {
			updated = row.GetOr("started_at", nil)
		}
		results = append(results, sessionInfo(sid, sessionCwd, model, int(count), row.GetOr("title", nil), row.GetOr("preview", nil), updated))
	}
	sort.SliceStable(results, func(i, j int) bool {
		return updatedAtSortKey(results[i].GetOr("updated_at", nil)) > updatedAtSortKey(results[j].GetOr("updated_at", nil))
	})
	return results, nil
}

// Catalog rows of the manager's store, the profile store when none is bound
func (m *SessionManager) catalogRows(ctx context.Context) (map[string]*pyjson.Object, []string, error) {
	m.dbLock.Lock()
	db := m.dbInstance
	m.dbLock.Unlock()
	path := filepath.Join(home.Dir(ctx), "state.db")
	if db != nil {
		path = db.Path()
	}
	return ReadCatalogRows(ctx, path)
}

// Moves a session to a new working directory and rebinds its tool overrides, nil when unknown
func (m *SessionManager) UpdateCwd(ctx context.Context, sessionID, cwd string) *SessionState {
	cwd = translateACPCwd(cwd)
	st := m.GetSession(ctx, sessionID)
	if st == nil {
		return nil
	}
	st.Cwd = cwd
	registerTaskCwdHook(ctx, sessionID, cwd)
	m.persist(ctx, st)
	m.scheduleGitMetadata(ctx, st, m.claimCwdGeneration(ctx, st))
	return st
}

// Persists a session held in memory
func (m *SessionManager) SaveSession(ctx context.Context, sessionID string) {
	if st := m.memorySession(sessionID); st != nil {
		m.persist(ctx, st)
	}
}

// Stamps ended at on every live session and returns how many were ended
func (m *SessionManager) EndAllSessions(ctx context.Context, endReason string) int {
	if endReason == "" {
		endReason = "acp_disconnect"
	}
	db := m.GetDB(ctx)
	if db == nil {
		return 0
	}
	m.lock.Lock()
	ids := slices.Clone(m.order)
	m.lock.Unlock()
	ended := 0
	for _, sid := range ids {
		if err := db.EndSession(sid, endReason); err != nil {
			sessionLog.Debug(fmt.Sprintf("Failed to end ACP session %s\n%s", sid, err))
			continue
		}
		ended++
	}
	if ended > 0 {
		sessionLog.Info(fmt.Sprintf("Ended %d ACP session(s) on shutdown (%s)", ended, endReason))
	}
	return ended
}

// Registers a new state in memory, binds its cwd for tools and persists it when asked
func (m *SessionManager) installState(ctx context.Context, sessionID string, a any, cwd, model string, history []map[string]any, persist bool) *SessionState {
	st := &SessionState{SessionID: sessionID, Agent: a, Cwd: cwd, Model: model, History: history, CancelEvent: &pythread.Event{}}
	m.lock.Lock()
	if _, ok := m.sessions[sessionID]; !ok {
		m.order = append(m.order, sessionID)
	}
	m.sessions[sessionID] = st
	m.lock.Unlock()
	registerTaskCwdHook(ctx, sessionID, cwd)
	if persist {
		m.persist(ctx, st)
	}
	return st
}

// Process shared session store, acquired on first use and cwd backfilled once, nil when unavailable
func (m *SessionManager) GetDB(ctx context.Context) *state.SessionDB {
	m.dbLock.Lock()
	defer m.dbLock.Unlock()
	if m.dbInstance == nil {
		db, err := acquireSessionDB(ctx, filepath.Join(home.Dir(ctx), "state.db"))
		if err != nil {
			sessionLog.Debug("SessionDB unavailable for ACP persistence\n" + err.Error())
		} else {
			m.dbInstance = db
		}
	}
	if m.dbInstance != nil && !m.cwdBackfilled {
		m.cwdBackfilled = true
		repaired, err := m.dbInstance.BackfillACPSessionCwd(ctx)
		if err != nil {
			sessionLog.Debug("ACP session cwd backfill failed\n" + err.Error())
		} else if repaired > 0 {
			sessionLog.Info(fmt.Sprintf("Backfilled cwd for %d ACP session(s) from model_config", repaired))
		}
	}
	return m.dbInstance
}

// Creates or updates the session row then syncs the live message set
func (m *SessionManager) persist(ctx context.Context, st *SessionState) {
	db := m.GetDB(ctx)
	if db == nil {
		return
	}
	var modelStr *string
	if st.Model != "" {
		modelStr = &st.Model
	}
	meta := pyjson.NewObject()
	meta.Set("cwd", st.Cwd)
	for _, kv := range [][2]string{{"provider", agentProvider(st.Agent)}, {"base_url", agentBaseURL(st.Agent)}, {"api_mode", agentAPIMode(st.Agent)}} {
		if v := strings.TrimSpace(kv[1]); v != "" {
			meta.Set(kv[0], v)
		}
	}
	if err := m.persistRow(ctx, db, st, modelStr, meta); err != nil {
		sessionLog.Warning(fmt.Sprintf("Failed to persist ACP session %s\n%s", st.SessionID, err))
	}
}

func (m *SessionManager) persistRow(ctx context.Context, db *state.SessionDB, st *SessionState, modelStr *string, meta *pyjson.Object) error {
	row, err := db.GetSession(ctx, st.SessionID)
	if err != nil {
		return err
	}
	if row == nil {
		if len(st.History) == 0 {
			return nil
		}
		n := state.NewSession{ID: st.SessionID, Source: "acp", ModelConfig: meta.Vals, Cwd: st.Cwd}
		if modelStr != nil {
			n.Model = *modelStr
		}
		if err := db.CreateSession(n); err != nil {
			return err
		}
	} else {
		text, err := pyjson.Dumps(meta)
		if err == nil {
			err = db.UpdateSessionMeta(ctx, st.SessionID, text, modelStr)
		}
		if err != nil {
			sessionLog.Debug("Failed to update ACP session metadata\n" + err.Error())
		}
		if st.Cwd != "" {
			m.scheduleGitMetadata(ctx, st, m.claimCwdGeneration(ctx, st))
		}
	}
	if agentOwnsPersistence(st.Agent, db) {
		return nil
	}
	return db.ReplaceMessages(ctx, st.SessionID, st.History, state.ReplaceOptions{ActiveOnly: true})
}

// Writes the cwd column and returns its new git metadata generation, nil without a row or a store
func (m *SessionManager) claimCwdGeneration(ctx context.Context, st *SessionState) *int64 {
	db := m.GetDB(ctx)
	if db == nil || st.Cwd == "" {
		return nil
	}
	generation, err := db.UpdateSessionCwdClaim(ctx, st.SessionID, st.Cwd, "", "", false)
	if err != nil {
		sessionLog.Debug(fmt.Sprintf("Failed to persist ACP session cwd column for %s\n%s", st.SessionID, err))
		return nil
	}
	return generation
}

// Probes git off the critical path and publishes branch and root under the claimed generation
func (m *SessionManager) scheduleGitMetadata(ctx context.Context, st *SessionState, generation *int64) {
	if generation == nil || *generation == 0 || st.Cwd == "" {
		return
	}
	sessionID, cwd, gen := st.SessionID, st.Cwd, *generation
	detached := context.WithoutCancel(ctx)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				sessionLog.Debug(fmt.Sprintf("Failed to publish ACP git metadata for %s\n%v", sessionID, r))
			}
		}()
		branch, root := tuigateway.Branch(cwd), tuigateway.CommonRepoRoot(cwd)
		if branch == "" && root == "" {
			return
		}
		if db := m.GetDB(detached); db != nil {
			if _, err := db.PublishSessionGitMetadata(detached, sessionID, cwd, gen, branch, root); err != nil {
				sessionLog.Debug(fmt.Sprintf("Failed to publish ACP git metadata for %s\n%s", sessionID, err))
			}
		}
	}()
}

// Loads an ACP session from the store into memory with a rebuilt agent, nil when absent or not ACP
func (m *SessionManager) restore(ctx context.Context, sessionID string) *SessionState {
	db := m.GetDB(ctx)
	if db == nil {
		return nil
	}
	row, err := db.GetSession(ctx, sessionID)
	if err != nil {
		sessionLog.Debug(fmt.Sprintf("Failed to query DB for ACP session %s\n%s", sessionID, err))
		return nil
	}
	if row == nil || row.GetOr("source", nil) != "acp" {
		return nil
	}
	if row.GetOr("ended_at", nil) != nil {
		if err := db.ReopenSession(ctx, sessionID); err != nil {
			sessionLog.Debug(fmt.Sprintf("Failed to reopen ACP session %s\n%s", sessionID, err))
		}
	}
	meta := parseModelConfig(row.GetOr("model_config", nil))
	cwd := pyStr(meta.GetOr("cwd", "."))
	orText := func(values ...any) string {
		for _, v := range values {
			if truthy(v) {
				return pyStr(v)
			}
		}
		return ""
	}
	model := orText(row.GetOr("model", nil))
	history, err := db.GetMessagesAsConversation(ctx, sessionID, state.ConversationOptions{RepairAlternation: true})
	if err != nil {
		sessionLog.Warning(fmt.Sprintf("Failed to load messages for ACP session %s\n%s", sessionID, err))
		history = []map[string]any{}
	}
	if history == nil {
		history = []map[string]any{}
	}
	a, err := m.MakeAgent(ctx, MakeAgentOptions{
		SessionID: sessionID, Cwd: cwd, Model: model, APIMode: orText(meta.GetOr("api_mode", nil)),
		RequestedProvider: orText(meta.GetOr("provider", nil), row.GetOr("billing_provider", nil)),
		BaseURL:           orText(meta.GetOr("base_url", nil), row.GetOr("billing_base_url", nil)),
	})
	if err != nil {
		sessionLog.Warning(fmt.Sprintf("Failed to recreate agent for ACP session %s\n%s", sessionID, err))
		return nil
	}
	if model == "" {
		model = agentModel(a)
	}
	st := m.installState(ctx, sessionID, a, cwd, model, history, false)
	sessionLog.Info(fmt.Sprintf("Restored ACP session %s from DB (%d messages)", sessionID, len(history)))
	return st
}

// Provider resolution failure surfaced in place of the bare agent build failure it caused
type resolveFailure struct {
	resolve error
	build   error
}

func (e *resolveFailure) Error() string { return e.resolve.Error() }

// Build failure the resolution failure replaced
func (e *resolveFailure) Unwrap() error { return e.resolve }

// Build failure the resolution failure replaced, the Python cause
func (e *resolveFailure) Cause() error { return e.build }

// Agent for a session from the factory when one is set, else a real agent on the resolved runtime route
func (m *SessionManager) MakeAgent(ctx context.Context, o MakeAgentOptions) (any, error) {
	if m.agentFactory != nil {
		return m.agentFactory(), nil
	}
	cfg, err := loadSessionConfig(ctx)
	if err != nil {
		return nil, err
	}
	defaultModel, configProvider := "", ""
	switch modelCfg := cfg["model"].(type) {
	case map[string]any:
		if truthy(modelCfg["default"]) {
			defaultModel = pyStr(modelCfg["default"])
		}
		if truthy(modelCfg["provider"]) {
			configProvider = pyStr(modelCfg["provider"])
		}
	case string:
		defaultModel = strings.TrimSpace(modelCfg)
	}
	enabled := o.EnabledToolsets
	if enabled == nil {
		portable := plugins.GetPortableMCPServerNamesNowait(ctx)
		resolved := tools.Default.PlatformTools(ctx, tools.ScopeKey(home.Dir(ctx)), cfg, "acp", tools.PlatformToolsOptions{PortableMCPServers: portable, IncludeDefaultMCPServers: true})
		servers := tools.EnabledMCPServerNames(cfg, portable)
		var plain, mcpNames []string
		for name := range resolved {
			if servers[name] {
				mcpNames = append(mcpNames, name)
			} else {
				plain = append(plain, name)
			}
		}
		sort.Strings(plain)
		sort.Strings(mcpNames)
		if plain == nil {
			plain = []string{}
		}
		enabled = expandACPEnabledToolsets(plain, false, mcpNames)
	}
	model := o.Model
	if model == "" {
		model = defaultModel
	}
	opts := agent.AIAgentOptions{
		Platform: "acp", QuietMode: true, SessionID: o.SessionID, EnabledToolsets: slices.Clone(enabled),
		Model: model, Cwd: o.Cwd, ReasoningConfig: config.ResolveReasoningConfig(cfg, model),
	}
	if db := m.GetDB(ctx); db != nil {
		opts.SessionDB = db
	}
	if o.DisabledToolsets != nil {
		opts.DisabledToolsets = slices.Clone(o.DisabledToolsets)
	} else {
		agentCfg, _ := cfg["agent"].(map[string]any)
		if parsed := tools.ParseConfigStringList(agentCfg["disabled_toolsets"]); len(parsed) > 0 {
			opts.DisabledToolsets = parsed
		}
	}
	requested := o.RequestedProvider
	if requested == "" {
		requested = configProvider
	}
	var resolveErr error
	if runtime, err := resolveSessionRuntime(ctx, requested, model); err != nil {
		resolveErr = err
		sessionLog.Debug("ACP session falling back to default provider resolution\n" + err.Error())
	} else if runtime != nil {
		opts.Provider, opts.APIMode, opts.BaseURL = runtime.Provider, string(runtime.APIMode), runtime.BaseURL
		if o.APIMode != "" {
			opts.APIMode = o.APIMode
		}
		if o.BaseURL != "" {
			opts.BaseURL = o.BaseURL
		}
		opts.APIKey, opts.CredentialPool = runtime.APIKey, runtime.CredentialPool
		opts.Command, opts.Args = runtime.Command, slices.Clone(runtime.Args)
		if opts.Args == nil {
			opts.Args = []string{}
		}
		if len(runtime.RequestOverrides) > 0 && (o.BaseURL == "" || o.BaseURL == runtime.BaseURL) {
			opts.RequestOverrides = runtime.RequestOverrides
		}
	}
	registerTaskCwdHook(ctx, o.SessionID, o.Cwd)
	func() {
		defer func() {
			if r := recover(); r != nil {
				sessionLog.Debug(fmt.Sprintf("ACP: bounded MCP discovery wait failed\n%v", r))
			}
		}()
		ensureMCPDiscoveryBeforeAgentBuild(ctx, cfg)
	}()
	a, err := newAIAgent(ctx, opts)
	if err != nil {
		if resolveErr != nil {
			return nil, &resolveFailure{resolve: resolveErr, build: err}
		}
		return nil, err
	}
	if p, ok := a.(interface{ SetPrintFn(func(string)) }); ok {
		p.SetPrintFn(func(s string) { acpStderrPrint(s) })
	}
	return a, nil
}
