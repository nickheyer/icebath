package gatewaycli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	// Mutations the owner refuses without the execution generation they were read at
	generationFenced = map[string]bool{"delete": true, "rewind": true, "reset": true, "branch": true, "model": true,
		"compress": true, "workspace": true}
	// Launch options of the classic CLI that are creation policy on the gateway session
	launchPolicyFields = []string{"model", "provider", "toolsets", "reasoning", "api_key", "base_url", "max_turns",
		"ignore_rules", "checkpoints", "pass_session_id", "skills"}
	canonicalLocalID = regexp.MustCompile(`^local-([0-9a-f]{8})[0-9a-f]{56}$`)
	// Tree the classic CLI recorded for its worktree flag, assigned by the CLI package
	RecordedWorktree = func() map[string]string { return nil }
)

// Raised when another session already holds a title the gateway refused
type TitleInUseError struct {
	Msg   string
	Cause error
}

func (e *TitleInUseError) Error() string      { return e.Msg }
func (e *TitleInUseError) Unwrap() error      { return e.Cause }
func (e *TitleInUseError) PyTypeName() string { return "ValueError" }

// What a session open reads off the classic CLI
type OpenSessionCLI struct {
	SessionID           string
	Resumed             bool
	TypedAPIKey         any
	TypedToolsets       any
	ExplicitIgnoreRules any
}

// What a workspace move reads off the classic CLI
type WorkspaceCLI struct {
	SessionDB          any
	GatewaySessionOpen bool
	SessionID          string
}

// Session store of the classic CLI backed by the shared gateway session, reads local and writes gateway verbs
type GatewaySessionStore struct {
	Connection *GatewayConnection
	// Destination of the config warning a create prints
	Stderr           io.Writer
	mu               sync.Mutex
	snapshots        map[string]*pyjson.Object
	current          *pyjson.Object
	reader           *SurfaceLabelReader
	createParameters []any
	described        bool
}

// Store over a connection, the process wide one when connection is nil
func NewGatewaySessionStore(connection *GatewayConnection) *GatewaySessionStore {
	if connection == nil {
		connection = SharedGatewayConnection()
	}
	return &GatewaySessionStore{Connection: connection, Stderr: os.Stderr, snapshots: map[string]*pyjson.Object{}}
}

// Store over the process wide connection
func SessionStore() *GatewaySessionStore { return NewGatewaySessionStore(nil) }

// True until the viewer's gateway connection is final, a gateway hang up staying true
func (s *GatewaySessionStore) Bool() bool { return !s.Connection.Final() }

// Snapshot of the opened session, nil until one is open
func (s *GatewaySessionStore) Current() *pyjson.Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

// Releases this viewer's SQLite handles before offline maintenance, keeping its RPC store
func (s *GatewaySessionStore) CloseReader() error {
	s.mu.Lock()
	reader := s.reader
	s.reader = nil
	s.mu.Unlock()
	if reader != nil {
		return reader.Close()
	}
	return nil
}

// Read only reader opened on first use, nil while the gateway has not created state.db yet
func (s *GatewaySessionStore) Reader(ctx context.Context) *SurfaceLabelReader {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reader == nil {
		path := home.StatePath(home.Dir(ctx))
		if _, err := os.Stat(path); err != nil {
			return nil
		}
		db, err := state.NewSessionDB(ctx, path, true)
		if err != nil {
			slog.Debug(fmt.Sprintf("read-only state.db unavailable: %s", err.Error()))
			return nil
		}
		s.reader = NewSurfaceLabelReader(db)
	}
	return s.reader
}

// Python GatewayClientError carrying a message
func gatewayClientError(msg string) error { return &gateway.GatewayClientError{Msg: msg} }

// Refusal of a session store write the gateway does not carry
func (s *GatewaySessionStore) Refused(name string) error {
	return gatewayClientError(fmt.Sprintf("%s is not on the gateway session yet; nothing was written", name))
}

// One gateway request on this store's connection
func (s *GatewaySessionStore) Call(ctx context.Context, method string, params map[string]any) (any, error) {
	return s.Connection.Call(ctx, method, params)
}

// One gateway request answered as an ordered object, empty for any other answer
func (s *GatewaySessionStore) callObject(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
	result, err := s.Call(ctx, method, params)
	if err != nil {
		return nil, err
	}
	if obj, ok := pyjson.AsObject(result); ok {
		return obj, nil
	}
	return pyjson.NewObject(), nil
}

// Records a snapshot as the current session and returns its stored id
func (s *GatewaySessionStore) adopt(snapshot *pyjson.Object) (string, error) {
	sid, ok := snapshot.Vals["stored_session_id"].(string)
	if !ok {
		return "", &pyrepr.FormatError{Class: "KeyError", Msg: "'stored_session_id'"}
	}
	s.mu.Lock()
	s.snapshots[sid] = snapshot
	s.current = snapshot
	s.mu.Unlock()
	s.Connection.State(sid).AdoptSnapshot(snapshot)
	return sid, nil
}

// Resumes a session once per id this viewer uses, carrying the launch key the way the thin client does
func (s *GatewaySessionStore) Attach(ctx context.Context, sessionID string, typed bool) (*pyjson.Object, error) {
	s.mu.Lock()
	known := s.snapshots[sessionID]
	s.mu.Unlock()
	if known != nil {
		return known, nil
	}
	result, err := s.Connection.Resume(ctx, s.Call, sessionID, typed)
	if err != nil {
		return nil, err
	}
	snapshot, ok := pyjson.AsObject(result)
	if !ok {
		snapshot = pyjson.NewObject()
	}
	if _, err := s.adopt(snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

// Creates or resumes the gateway session of a CLI and adopts its id, empty when the gateway refuses the target
func (s *GatewaySessionStore) OpenSession(ctx context.Context, cli OpenSessionCLI, launch map[string]any) (string, error) {
	key := ""
	if pyrepr.Truthy(launch["api_key"]) {
		key = pyrepr.Str(launch["api_key"])
	}
	typed := key != "" && pyjson.Equal(launch["api_key"], cli.TypedAPIKey)
	s.Connection.SetLaunchKey(key, typed)
	if !cli.Resumed {
		return s.Create(ctx, launch, "", true)
	}
	snapshot, err := s.Attach(ctx, cli.SessionID, typed)
	if err != nil {
		if isClientError(err) && ResumeRefusals[err.Error()] {
			return "", nil
		}
		return "", err
	}
	checked := make(map[string]any, len(launch)+2)
	for k, v := range launch {
		checked[k] = v
	}
	checked["toolsets"] = cli.TypedToolsets
	checked["ignore_rules"] = nil
	if pyrepr.Truthy(cli.ExplicitIgnoreRules) {
		checked["ignore_rules"] = cli.ExplicitIgnoreRules
	}
	if err := checkResumeLaunch(ctx, checked, snapshot); err != nil {
		return "", err
	}
	return s.adopt(snapshot)
}

// Creates a gateway session with the launch policy and an optional title, adopting and returning its id
func (s *GatewaySessionStore) Create(ctx context.Context, launch map[string]any, title string, recordCwd bool) (string, error) {
	params := creationPolicy(launch)
	cwd, err := launchCwd()
	if err != nil {
		return "", err
	}
	params["cwd"] = cwd
	source := agent.CreateSurface(provider.SessionSourceFor(nil, "cli"))
	params["source"] = source
	params["row_cwd"] = ""
	if recordCwd && terminalIsLocal() && agent.CLIFamilySources[source] {
		rowCwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		params["row_cwd"] = rowCwd
	}
	if title != "" {
		params["title"] = title
	}
	clientEnv, err := s.ClientEnv(ctx)
	if err != nil {
		return "", err
	}
	for k, v := range clientEnv {
		params[k] = v
	}
	params["request_id"] = uuid4Hex()
	snapshot, err := s.callObject(ctx, "session.create", params)
	if err != nil {
		return "", err
	}
	if info, ok := pyjson.AsObject(snapshot.Vals["info"]); ok && pyrepr.Truthy(info.Vals["config_warning"]) {
		fmt.Fprintf(s.Stderr, "Warning: %s\n", pyrepr.Str(info.Vals["config_warning"]))
	}
	return s.adopt(snapshot)
}

// Whether the terminal backend runs on this host
func terminalIsLocal() bool {
	backend := os.Getenv("TERMINAL_ENV")
	if backend == "" {
		backend = "local"
	}
	backend = strings.ToLower(pystr.Strip(backend))
	return backend == "" || backend == "local"
}

// client_env of this shell when the gateway takes it, the create contract described once per store
func (s *GatewaySessionStore) ClientEnv(ctx context.Context) (map[string]any, error) {
	s.mu.Lock()
	described, parameters := s.described, s.createParameters
	s.mu.Unlock()
	if !described {
		result, err := s.Call(ctx, "runtime.describe", map[string]any{})
		if err != nil {
			return nil, err
		}
		parameters = []any{}
		if runtime, ok := pyjson.AsObject(result); ok {
			if contract, ok := pyjson.AsObject(runtime.Vals["session_create"]); ok {
				if list, ok := contract.Vals["parameters"].([]any); ok {
					parameters = append(parameters, list...)
				}
			}
		}
		s.mu.Lock()
		s.described, s.createParameters = true, parameters
		s.mu.Unlock()
	}
	return ClientEnvPolicy(ctx, parameters)
}

// Forks the active transcript into a child the gateway attaches this viewer to, returning the child id
func (s *GatewaySessionStore) Branch(ctx context.Context, sessionID, title string) (string, error) {
	snapshot, err := s.callObject(ctx, "session.resume", map[string]any{"session_id": sessionID})
	if err != nil {
		return "", err
	}
	revision, err := snapshotField(snapshot, "revision")
	if err != nil {
		return "", err
	}
	generation, err := snapshotField(snapshot, "execution_generation")
	if err != nil {
		return "", err
	}
	payload := map[string]any{}
	if title != "" {
		payload["title"] = title
	}
	receipt, err := s.callObject(ctx, "session.mutate", map[string]any{"session_id": sessionID, "request_id": uuid4Hex(),
		"expected_revision": revision, "expected_generation": generation, "operation": "branch", "payload": payload})
	if err != nil {
		return "", err
	}
	child, _ := receipt.Vals["branched_session_id"].(string)
	if child == "" {
		return "", gatewayClientError("branch_failed")
	}
	attached, err := s.Attach(ctx, child, false)
	if err != nil {
		return "", err
	}
	if _, err := s.adopt(attached); err != nil {
		return "", err
	}
	return child, nil
}

// Required snapshot member, a KeyError when the owner left it out
func snapshotField(snapshot *pyjson.Object, key string) (any, error) {
	value, ok := snapshot.Get(key)
	if !ok {
		return nil, &pyrepr.FormatError{Class: "KeyError", Msg: pyrepr.Repr(key)}
	}
	return value, nil
}

// One session.mutate at the revision of a fresh read, fenced by generation for the operations that need it
func (s *GatewaySessionStore) Mutate(ctx context.Context, sessionID, operation string, payload map[string]any, snapshot *pyjson.Object) (any, error) {
	if snapshot == nil || snapshot.Len() == 0 {
		read, err := s.callObject(ctx, "session.resume", map[string]any{"session_id": sessionID})
		if err != nil {
			return nil, err
		}
		snapshot = read
	}
	params := map[string]any{"session_id": sessionID, "request_id": uuid4Hex(), "operation": operation, "payload": payload}
	if generationFenced[operation] {
		generation, err := snapshotField(snapshot, "execution_generation")
		if err != nil {
			return nil, err
		}
		params["expected_generation"] = generation
	}
	revision, err := snapshotField(snapshot, "revision")
	if err != nil {
		return nil, err
	}
	params["expected_revision"] = revision
	return s.Call(ctx, "session.mutate", params)
}

// Renames a session on the gateway, a refusal caused by another holder reported with that holder's id
func (s *GatewaySessionStore) SetSessionTitle(ctx context.Context, sessionID, title string) (bool, error) {
	if _, err := s.Mutate(ctx, sessionID, "rename", map[string]any{"title": title}, nil); err != nil {
		if isClientError(err) {
			holder, herr := s.titleHolder(ctx, sessionID, title)
			if herr != nil {
				return false, herr
			}
			if holder != "" {
				return false, &TitleInUseError{Msg: fmt.Sprintf("Title '%s' is already in use by session %s", title, holder), Cause: err}
			}
		}
		return false, err
	}
	s.mu.Lock()
	_, viewed := s.snapshots[sessionID]
	s.mu.Unlock()
	if viewed {
		cached := s.Connection.State(sessionID)
		cached.Lock()
		cached.Title = title
		cached.Unlock()
	}
	return true, nil
}

// Python int of a decoded number, zero for anything else
func pyIntOf(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case *big.Int:
		return int(n.Int64())
	case bool:
		if n {
			return 1
		}
	}
	return 0
}

// Rewinds to just before a user turn located on the gateway transcript and checked against the warm view
func (s *GatewaySessionStore) RewindUserTurn(ctx context.Context, sessionID string, userOrdinal int, o state.RewindUserTurnOptions) (*state.RewindOutcome, error) {
	snapshot, err := s.callObject(ctx, "session.resume", map[string]any{"session_id": sessionID})
	if err != nil {
		return nil, err
	}
	var durable []map[string]any
	rows, _ := snapshot.Vals["messages"].([]any)
	for _, row := range rows {
		message, ok := plainDict(row)
		if !ok {
			return nil, &pyrepr.FormatError{Class: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(row))}
		}
		if !pyrepr.Truthy(message["archived"]) {
			durable = append(durable, message)
		}
	}
	durableUser := state.UserTurnIndices(durable)
	if userOrdinal < 0 {
		userOrdinal = max(len(durableUser)+userOrdinal, 0)
	}
	if userOrdinal >= len(durableUser) {
		return nil, &state.RewindTargetUnavailableError{Msg: "target user message is no longer in session history"}
	}
	target := durable[durableUser[userOrdinal]]
	durablePrefix, liveView, err := agent.HistoryBeforeUserOriginatedTurn(durable, durableUser[userOrdinal])
	if err != nil {
		return nil, err
	}
	prefix := make([]map[string]any, len(durablePrefix))
	for i, message := range durablePrefix {
		row := make(map[string]any, len(message))
		for k, v := range message {
			if k != "row_id" {
				row[k] = v
			}
		}
		prefix[i] = row
	}
	if o.HasWarmHistory {
		var warm []map[string]any
		for _, message := range o.WarmHistory {
			if !state.IsEphemeralScaffolding(message) {
				warm = append(warm, message)
			}
		}
		warmUser := state.UserTurnIndices(warm)
		if len(warmUser) != len(durableUser) {
			return nil, &state.RewindRuntimeError{Msg: state.RewindHistoryChanged()}
		}
		warmPrefix, warmLive, err := agent.HistoryBeforeUserOriginatedTurn(warm, warmUser[userOrdinal])
		if err != nil {
			return nil, err
		}
		prefix = warmPrefix
		if !pyjson.Equal(state.RewindComparisonContent(liveView), state.RewindComparisonContent(warmLive)) {
			return nil, &state.RewindRuntimeError{Msg: state.RewindHistoryChanged()}
		}
	}
	liveText := ""
	if o.RequireRetryable {
		liveText, err = state.RetryableUserText(liveView["content"])
		if err != nil {
			return nil, err
		}
	} else {
		liveText = state.FlattenMessageText(liveView["content"], "\n")
	}
	if !isPyInt(target["row_id"]) {
		return nil, &state.RewindRuntimeError{Msg: "rewind target has no durable row identity"}
	}
	revision, err := snapshotField(snapshot, "revision")
	if err != nil {
		return nil, err
	}
	generation, err := snapshotField(snapshot, "execution_generation")
	if err != nil {
		return nil, err
	}
	receipt, err := s.callObject(ctx, "session.mutate", map[string]any{"session_id": sessionID, "request_id": uuid4Hex(),
		"expected_revision": revision, "expected_generation": generation, "operation": "rewind",
		"payload": map[string]any{"target_message_id": target["row_id"]}})
	if err != nil {
		return nil, err
	}
	return &state.RewindOutcome{Prefix: prefix, LiveView: liveView, LiveText: liveText,
		RewoundCount: pyIntOf(receipt.Vals["rewound_count"]), TurnsUndone: len(durableUser) - userOrdinal}, nil
}

// Persists the session's own approval bypass on the gateway route, where approvals run
func (s *GatewaySessionStore) SetSessionYolo(ctx context.Context, sessionID string, enabled bool) error {
	value := "0"
	if enabled {
		value = "1"
	}
	_, err := s.Call(ctx, "config.set", map[string]any{"session_id": sessionID, "key": "yolo", "value": value})
	return err
}

// Id of another session holding a title, empty without a reader, a title or a holder
func (s *GatewaySessionStore) titleHolder(ctx context.Context, sessionID, title string) (string, error) {
	db := s.Reader(ctx)
	if db == nil || title == "" {
		return "", nil
	}
	rows, err := db.DB().ReadAll(ctx, "SELECT id FROM sessions WHERE title = ? AND id != ?", title, sessionID)
	if err != nil || len(rows) == 0 {
		return "", err
	}
	return pyrepr.Str(rows[0].Vals["id"]), nil
}

// Drops this viewer's subscription record and returns its id, so no later reconnect resumes it
func (s *GatewaySessionStore) leave(sessionID string) any {
	cached := s.Connection.LookupState(sessionID)
	if cached == nil {
		return nil
	}
	cached.Lock()
	defer cached.Unlock()
	subscription := cached.SubscriptionID
	cached.SubscriptionID = nil
	return subscription
}

// Deletes a session on the gateway, a refusal handing back only the attachment its own read made
func (s *GatewaySessionStore) DeleteSession(ctx context.Context, sessionID string) (bool, error) {
	s.mu.Lock()
	delete(s.snapshots, sessionID)
	s.mu.Unlock()
	snapshot, err := s.callObject(ctx, "session.resume", map[string]any{"session_id": sessionID})
	if err != nil {
		return false, err
	}
	resolved := sessionID
	if pyrepr.Truthy(snapshot.Vals["session_id"]) {
		resolved = pyrepr.Str(snapshot.Vals["session_id"])
	}
	held := false
	if cached := s.Connection.LookupState(resolved); cached != nil {
		cached.Lock()
		held = cached.SubscriptionID != nil
		cached.Unlock()
	}
	if _, err := s.Mutate(ctx, sessionID, "delete", map[string]any{}, snapshot); err != nil {
		if isClientError(err) && !held && pyrepr.Truthy(snapshot.Vals["subscription_id"]) {
			_, derr := s.Call(ctx, "session.detach", map[string]any{"session_id": resolved, "subscription_id": snapshot.Vals["subscription_id"]})
			if derr != nil {
				if !isClientError(derr) {
					return false, derr
				}
				slog.Debug(fmt.Sprintf("session.detach after a refused delete failed: %s", derr.Error()))
			}
		}
		return false, err
	}
	s.leave(sessionID)
	return true, nil
}

// Detaches this viewer with an end reason, the owner ending the idle row once no viewer is left
func (s *GatewaySessionStore) EndSession(ctx context.Context, sessionID, reason string) error {
	s.mu.Lock()
	snapshot := s.snapshots[sessionID]
	delete(s.snapshots, sessionID)
	s.mu.Unlock()
	cached := s.Connection.LookupState(sessionID)
	reachable := false
	if cached != nil {
		cached.Lock()
		reachable = cached.SubscriptionID != nil
		cached.Unlock()
		reachable = reachable && !s.Connection.Final()
	}
	if reachable {
		err := s.Connection.Start(ctx, DefaultStartTimeout)
		if err == nil {
			cached.Lock()
			gapped := cached.Gaps != cached.AttachedGaps
			cached.Unlock()
			if gapped {
				_, err = s.Connection.Reattach(ctx, sessionID, nil)
			}
		}
		if err != nil {
			if !isClientError(err) {
				return err
			}
			slog.Debug(fmt.Sprintf("redial for session.detach failed: %s", err.Error()))
			reachable = false
		}
	}
	subscription := s.leave(sessionID)
	if pyrepr.Truthy(subscription) && reachable {
		params := map[string]any{"session_id": sessionID, "subscription_id": subscription}
		if reason != "" {
			params["end_reason"] = reason
		}
		if _, err := s.Call(ctx, "session.detach", params); err != nil {
			if !isClientError(err) {
				return err
			}
			slog.Debug(fmt.Sprintf("session.detach failed: %s", err.Error()))
		}
	}
	s.mu.Lock()
	if s.current == snapshot {
		s.current = nil
	}
	s.mu.Unlock()
	return nil
}

// Deletes a session only when its row has no messages, no title and no children, never on doubt
func (s *GatewaySessionStore) DeleteSessionIfEmpty(ctx context.Context, sessionID string) (bool, error) {
	db := s.Reader(ctx)
	if db == nil {
		return false, nil
	}
	rows, err := db.DB().ReadAll(ctx, "SELECT 1 FROM sessions WHERE id = ? AND title IS NULL"+
		" AND NOT EXISTS (SELECT 1 FROM messages WHERE messages.session_id = sessions.id)"+
		" AND NOT EXISTS (SELECT 1 FROM sessions child WHERE child.parent_session_id = sessions.id)", sessionID)
	if err != nil {
		slog.Debug(fmt.Sprintf("empty-session probe failed: %s", err.Error()))
		return false, nil
	}
	if len(rows) == 0 {
		return false, nil
	}
	deleted, err := s.DeleteSession(ctx, sessionID)
	if err != nil {
		if !isClientError(err) {
			return false, err
		}
		slog.Debug(fmt.Sprintf("empty session prune refused: %s", err.Error()))
		return false, nil
	}
	return deleted, nil
}

// Banner fields of the current session snapshot as welcome banner arguments, nil until a session is open
func (s *GatewaySessionStore) BannerInventory() map[string]any {
	var info *pyjson.Object
	if current := s.Current(); current != nil {
		info, _ = pyjson.AsObject(current.Vals["info"])
	}
	if info == nil {
		return nil
	}
	if _, ok := info.Get("tools"); !ok {
		return nil
	}
	grouped, _ := pyjson.AsObject(info.Vals["tools"])
	if grouped == nil {
		grouped = pyjson.NewObject()
	}
	toolsetOf := map[string]any{}
	tools := []any{}
	for _, toolset := range grouped.Keys {
		names, _ := grouped.Vals[toolset].([]any)
		for _, name := range names {
			toolsetOf[pyrepr.Str(name)] = toolset
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": name}})
		}
	}
	availability, _ := plainDict(info.Vals["tool_availability"])
	if len(availability) == 0 {
		availability = map[string]any{"available_toolsets": []any{}, "unavailable_toolsets": []any{}, "lazy_tools": []any{},
			"disabled_tools": []any{}}
	}
	unavailable, _ := availability["unavailable_toolsets"].([]any)
	for _, raw := range unavailable {
		item, _ := raw.(map[string]any)
		names, _ := item["tools"].([]any)
		for _, name := range names {
			if _, known := toolsetOf[pyrepr.Str(name)]; known {
				continue
			}
			label, ok := item["id"]
			if !ok {
				label, ok = item["name"]
			}
			if !ok {
				label = ""
			}
			toolsetOf[pyrepr.Str(name)] = label
		}
	}
	skills, _ := pyjson.AsObject(info.Vals["skills"])
	if skills == nil {
		skills = pyjson.NewObject()
	}
	servers, _ := info.Vals["mcp_servers"].([]any)
	return map[string]any{
		"tools":                      tools,
		"get_toolset_for_tool":       func(name string) any { return toolsetOf[name] },
		"availability":               availability,
		"skills_by_category":         skills,
		"mcp_status":                 append([]any{}, servers...),
		"enabled_toolsets":           info.Vals["enabled_toolsets"],
		"context_length":             info.Vals["context_length"],
		"cwd":                        info.Vals["cwd"],
		"tool_availability_warnings": info.Vals["tool_availability_warnings"],
	}
}

// Eight character banner label of a canonical durable id, every other id as is
func SessionLabel(sessionID string) string {
	if match := canonicalLocalID.FindStringSubmatch(sessionID); match != nil {
		return match[1]
	}
	return sessionID
}

// Explicit launch flags as session.create policy, unset flags leaving the gateway to resolve the profile default
func creationPolicy(launch map[string]any) map[string]any {
	fields := map[string]any{}
	for _, key := range launchPolicyFields {
		fields[key] = launch[key]
	}
	fields["toolsets"] = joinedToolsets(fields["toolsets"])
	requested := RequestedPolicy(fields)
	requested.Delete("source")
	policy, _ := pyjson.Plain(requested).(map[string]any)
	for k, v := range classicOnlyPolicy(launch) {
		policy[k] = v
	}
	env := EnvLaunchFlags()
	for _, k := range env.Keys {
		policy[k] = env.Vals[k]
	}
	return policy
}

// Toolset list joined with commas, any other value unchanged
func joinedToolsets(v any) any {
	var items []string
	switch list := v.(type) {
	case []any:
		for _, item := range list {
			items = append(items, pyrepr.Str(item))
		}
	case []string:
		items = list
	default:
		return v
	}
	return strings.Join(items, ",")
}

// Host folder session.create sends as cwd, the selected worktree or TERMINAL_CWD or this process's own
func launchCwd() (string, error) {
	terminalCwd := os.Getenv("TERMINAL_CWD")
	recorded := map[string]bool{}
	if tree := RecordedWorktree(); tree["path"] != "" {
		real, err := pypath.Realpath(tree["path"], pypath.Lax)
		if err != nil {
			return "", err
		}
		recorded[real] = true
	}
	if terminalCwd != "" {
		real, err := pypath.Realpath(terminalCwd, pypath.Lax)
		if err != nil {
			return "", err
		}
		if recorded[real] {
			return real, nil
		}
	}
	chosen := terminalCwd
	if chosen != "" && !terminalIsLocal() {
		if st, err := os.Stat(chosen); err != nil || !st.IsDir() {
			chosen = ""
		}
	}
	if chosen == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		chosen = cwd
	}
	return pypath.Realpath(chosen, pypath.Lax)
}

// Run budget and verbose flags of the classic CLI, which the thin client refuses
func classicOnlyPolicy(launch map[string]any) map[string]any {
	policy := map[string]any{}
	if launch["run_budget"] != nil {
		policy["run_budget"] = pyFloatOf(launch["run_budget"])
	}
	if pyrepr.Truthy(launch["verbose"]) {
		policy["verbose"] = true
	}
	return policy
}

// Python float of a number or numeric string
func pyFloatOf(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case string:
		var f float64
		fmt.Sscanf(pystr.Strip(n), "%g", &f)
		return f
	}
	return float64(pyIntOf(v))
}

// Refuses explicit launch flags on a resume that differ from the session's frozen route
func checkResumeLaunch(ctx context.Context, launch map[string]any, snapshot *pyjson.Object) error {
	fields := map[string]any{}
	for _, key := range launchPolicyFields {
		fields[key] = launch[key]
	}
	fields["toolsets"] = joinedToolsets(fields["toolsets"])
	if err := CheckResumePolicy(ctx, fields, snapshot); err != nil {
		return err
	}
	var frozen map[string]any
	if info, ok := pyjson.AsObject(snapshot.Vals["info"]); ok {
		frozen, _ = plainDict(info.Vals["launch_request"])
	}
	var differs []string
	classic := classicOnlyPolicy(launch)
	for _, key := range []string{"run_budget", "verbose"} {
		if value, ok := classic[key]; ok && !pyjson.Equal(frozen[key], value) {
			differs = append(differs, key)
		}
	}
	if len(differs) > 0 {
		return ResumePolicyError(differs)
	}
	return nil
}

// Moves the gateway session's frozen cwd into a new worktree first, then this process and TERMINAL_CWD follow
func MoveCLIWorkspace(ctx context.Context, cli WorkspaceCLI, wtInfo map[string]string) error {
	cwd, ok := wtInfo["path"]
	if !ok {
		return &pyrepr.FormatError{Class: "KeyError", Msg: "'path'"}
	}
	if store, ok := cli.SessionDB.(*GatewaySessionStore); ok && store != nil && cli.GatewaySessionOpen {
		if _, err := store.Mutate(ctx, cli.SessionID, "workspace", map[string]any{"cwd": cwd}, nil); err != nil {
			return err
		}
		tools.RecordTUIWorktree(ctx, wtInfo)
	}
	if err := os.Chdir(cwd); err != nil {
		return err
	}
	return os.Setenv("TERMINAL_CWD", cwd)
}

// Thirty two random hex digits the way uuid4 renders
func uuid4Hex() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[:])
}

// Creates a titled session through the gateway with no cwd stamp, returning its id
func CreateTitledSession(ctx context.Context, title string) (string, error) {
	return createTitledSession(ctx, SessionStore(), title)
}

// Titled create through the given store, the resolve or create step the owner runs under one lock
func createTitledSession(ctx context.Context, store *GatewaySessionStore, title string) (string, error) {
	params := creationPolicy(map[string]any{})
	cwd, err := launchCwd()
	if err != nil {
		return "", err
	}
	params["cwd"] = cwd
	params["source"] = "cli"
	params["row_cwd"] = ""
	clientEnv, err := store.ClientEnv(ctx)
	if err != nil {
		return "", err
	}
	for k, v := range clientEnv {
		params[k] = v
	}
	params["request_id"] = uuid4Hex()
	params["title"] = title
	snapshot, err := store.callObject(ctx, "session.create", params)
	if err != nil {
		return "", err
	}
	sid, ok := snapshot.Vals["stored_session_id"].(string)
	if !ok {
		return "", &pyrepr.FormatError{Class: "KeyError", Msg: "'stored_session_id'"}
	}
	return sid, nil
}
