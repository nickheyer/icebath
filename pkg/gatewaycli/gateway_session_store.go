package gatewaycli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

var (
	// Mutations the owner refuses without the execution generation they were read at
	generationFenced = map[string]bool{"delete": true, "rewind": true, "reset": true, "branch": true, "model": true,
		"compress": true, "workspace": true, "launch": true}
	// Launch options of the one shot chat that are creation policy on the gateway session
	launchPolicyFields = []string{"model", "provider", "toolsets", "reasoning", "api_key", "base_url", "max_turns",
		"ignore_rules", "checkpoints", "pass_session_id", "skills"}
	// Tree the one shot chat recorded for its worktree flag, assigned by the CLI package
	RecordedWorktree = func() map[string]string { return nil }
	// Folder the one shot chat's sessions work in, assigned by the CLI package; else the TERMINAL_CWD the process that started this one handed it
	LaunchFolder = func() string { return os.Getenv("TERMINAL_CWD") }
)

// Raised when another session already holds a title the gateway refused
type TitleInUseError struct {
	Msg   string
	Cause error
}

func (e *TitleInUseError) Error() string     { return e.Msg }
func (e *TitleInUseError) Unwrap() error     { return e.Cause }
func (e *TitleInUseError) ErrorKind() string { return "ValueError" }

// What a session open reads off the one shot chat
type OpenSessionCLI struct {
	SessionID           string
	Resumed             bool
	TypedAPIKey         any
	TypedToolsets       any
	ExplicitIgnoreRules any
}

// Session store of the one shot chat backed by the shared gateway session, reads local and writes gateway verbs
type GatewaySessionStore struct {
	Connection *GatewayConnection
	// Destination of the config warning a create prints
	Stderr    io.Writer
	mu        sync.Mutex
	snapshots map[string]*jsonutil.Object
	current   *jsonutil.Object
	reader    *SurfaceLabelReader
}

// Store over a connection, the process wide one when connection is nil
func NewGatewaySessionStore(connection *GatewayConnection) *GatewaySessionStore {
	if connection == nil {
		connection = SharedGatewayConnection()
	}
	return &GatewaySessionStore{Connection: connection, Stderr: os.Stderr, snapshots: map[string]*jsonutil.Object{}}
}

// Store over the process wide connection
func SessionStore() *GatewaySessionStore { return NewGatewaySessionStore(nil) }

// Snapshot of the opened session, nil until one is open
func (s *GatewaySessionStore) Current() *jsonutil.Object {
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
			pkgLog().Debug(fmt.Sprintf("read-only state.db unavailable: %s", err.Error()))
			return nil
		}
		s.reader = NewSurfaceLabelReader(db)
	}
	return s.reader
}

// One gateway request on this store's connection
func (s *GatewaySessionStore) Call(ctx context.Context, method string, params map[string]any) (any, error) {
	return s.Connection.Call(ctx, method, params)
}

// One gateway request answered as an ordered object, empty for any other answer
func (s *GatewaySessionStore) callObject(ctx context.Context, method string, params map[string]any) (*jsonutil.Object, error) {
	result, err := s.Call(ctx, method, params)
	if err != nil {
		return nil, err
	}
	if obj, ok := jsonutil.AsObject(result); ok {
		return obj, nil
	}
	return jsonutil.NewObject(), nil
}

// Records a snapshot as the current session and returns its stored id
func (s *GatewaySessionStore) adopt(snapshot *jsonutil.Object) (string, error) {
	sid, ok := snapshot.Value("stored_session_id").(string)
	if !ok {
		return "", &errorutil.Error{Kind: "KeyError", Msg: "snapshot is missing stored_session_id"}
	}
	s.mu.Lock()
	s.snapshots[sid] = snapshot
	s.current = snapshot
	s.mu.Unlock()
	s.Connection.State(sid).AdoptSnapshot(snapshot)
	return sid, nil
}

// Resumes a session once per id this viewer uses, carrying the launch key the way the thin client does
func (s *GatewaySessionStore) Attach(ctx context.Context, sessionID string, typed bool) (*jsonutil.Object, error) {
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
	snapshot, ok := jsonutil.AsObject(result)
	if !ok {
		snapshot = jsonutil.NewObject()
	}
	if _, err := s.adopt(snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

// Creates or resumes the gateway session of a CLI and adopts its id, empty when the gateway refuses the target
func (s *GatewaySessionStore) OpenSession(ctx context.Context, cli OpenSessionCLI, launch map[string]any) (string, error) {
	key := ""
	if anyutil.IsTrue(launch["api_key"]) {
		key = anyutil.String(launch["api_key"])
	}
	typed := key != "" && anyutil.Equal(launch["api_key"], cli.TypedAPIKey)
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
	if anyutil.IsTrue(cli.ExplicitIgnoreRules) {
		checked["ignore_rules"] = cli.ExplicitIgnoreRules
	}
	relaunched, err := s.relaunch(ctx, cli.SessionID, checked, snapshot)
	if err != nil {
		return "", err
	}
	return s.adopt(relaunched)
}

// Creates a gateway session with the launch policy and an optional title, adopting and returning its id
func (s *GatewaySessionStore) Create(ctx context.Context, launch map[string]any, title string, recordCwd bool) (string, error) {
	params := creationPolicy(launch)
	cwd, err := launchCwd(ctx)
	if err != nil {
		return "", err
	}
	params["cwd"] = cwd
	source := provider.SessionSourceFor(nil, "cli")
	params["source"] = source
	params["row_cwd"] = ""
	if recordCwd && terminalIsLocal(ctx) && agent.CLIFamilySources[source] {
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
	if info, ok := jsonutil.AsObject(snapshot.Value("info")); ok && anyutil.IsTrue(info.Value("config_warning")) {
		fmt.Fprintf(s.Stderr, "Warning: %s\n", anyutil.String(info.Value("config_warning")))
	}
	return s.adopt(snapshot)
}

// Whether the terminal backend of the profile in ctx runs on this host, terminal.backend
func terminalIsLocal(ctx context.Context) bool {
	backend := strings.ToLower(strings.TrimSpace(settings.WhereCommandsRun.Get(ctx)))
	return backend == "" || backend == "local"
}

// client_env of this shell, the environment a session created from here starts with
func (s *GatewaySessionStore) ClientEnv(ctx context.Context) (map[string]any, error) {
	return ClientEnvPolicy(ctx)
}

// Required snapshot member, a KeyError when the owner left it out
func snapshotField(snapshot *jsonutil.Object, key string) (any, error) {
	value, ok := snapshot.Get(key)
	if !ok {
		return nil, &errorutil.Error{Kind: "KeyError", Msg: "snapshot is missing " + key}
	}
	return value, nil
}

// One session.mutate at the revision of a fresh read, fenced by generation for the operations that need it
func (s *GatewaySessionStore) Mutate(ctx context.Context, sessionID, operation string, payload map[string]any, snapshot *jsonutil.Object) (any, error) {
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
	rows, _ := snapshot.Value("messages").([]any)
	for _, row := range rows {
		message, ok := plainDict(row)
		if !ok {
			return nil, &errorutil.Error{Kind: "AttributeError", Msg: "snapshot message must be an object, got " + anyutil.Kind(row)}
		}
		if !anyutil.IsTrue(message["archived"]) {
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
		if !anyutil.Equal(state.RewindComparisonContent(liveView), state.RewindComparisonContent(warmLive)) {
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
		RewoundCount: pyIntOf(receipt.Value("rewound_count")), TurnsUndone: len(durableUser) - userOrdinal}, nil
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
	return anyutil.String(rows[0].Value("id")), nil
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
			pkgLog().Debug(fmt.Sprintf("redial for session.detach failed: %s", err.Error()))
			reachable = false
		}
	}
	subscription := s.leave(sessionID)
	if anyutil.IsTrue(subscription) && reachable {
		params := map[string]any{"session_id": sessionID, "subscription_id": subscription}
		if reason != "" {
			params["end_reason"] = reason
		}
		if _, err := s.Call(ctx, "session.detach", params); err != nil {
			if !isClientError(err) {
				return err
			}
			pkgLog().Debug(fmt.Sprintf("session.detach failed: %s", err.Error()))
		}
	}
	s.mu.Lock()
	if s.current == snapshot {
		s.current = nil
	}
	s.mu.Unlock()
	return nil
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
	policy, _ := jsonutil.Plain(requested).(map[string]any)
	for k, v := range launchOnlyPolicy(launch) {
		policy[k] = v
	}
	env := EnvLaunchFlags()
	for _, k := range env.Keys() {
		policy[k] = env.Value(k)
	}
	return policy
}

// Toolset list joined with commas, any other value unchanged
func joinedToolsets(v any) any {
	var items []string
	switch list := v.(type) {
	case []any:
		for _, item := range list {
			items = append(items, anyutil.String(item))
		}
	case []string:
		items = list
	default:
		return v
	}
	return strings.Join(items, ",")
}

// Host folder session.create sends as cwd, the selected worktree or the launch folder or this process's own
func launchCwd(ctx context.Context) (string, error) {
	terminalCwd := LaunchFolder()
	recorded := map[string]bool{}
	if tree := RecordedWorktree(); tree["path"] != "" {
		real, err := pathutil.Resolve(tree["path"])
		if err != nil {
			return "", err
		}
		recorded[real] = true
	}
	if terminalCwd != "" {
		real, err := pathutil.Resolve(terminalCwd)
		if err != nil {
			return "", err
		}
		if recorded[real] {
			return real, nil
		}
	}
	chosen := terminalCwd
	if chosen != "" && !terminalIsLocal(ctx) {
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
	return pathutil.Resolve(chosen)
}

// Run budget and verbose flags of the one shot chat as session policy
func launchOnlyPolicy(launch map[string]any) map[string]any {
	policy := map[string]any{}
	if launch["run_budget"] != nil {
		policy["run_budget"] = pyFloatOf(launch["run_budget"])
	}
	if anyutil.IsTrue(launch["verbose"]) {
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
		fmt.Sscanf(strings.TrimSpace(n), "%g", &f)
		return f
	}
	return float64(pyIntOf(v))
}

// Lays the launch flags that differ from the resumed session's frozen launch over it, the fresh snapshot after
func (s *GatewaySessionStore) relaunch(ctx context.Context, sessionID string, launch map[string]any, snapshot *jsonutil.Object) (*jsonutil.Object, error) {
	fields := map[string]any{}
	for _, key := range launchPolicyFields {
		fields[key] = launch[key]
	}
	fields["toolsets"] = joinedToolsets(fields["toolsets"])
	overrides, err := ResumeOverrides(ctx, fields, snapshot)
	if err != nil {
		return nil, err
	}
	var frozen map[string]any
	if info, ok := jsonutil.AsObject(snapshot.Value("info")); ok {
		frozen, _ = plainDict(info.Value("launch_request"))
	}
	for key, value := range launchOnlyPolicy(launch) {
		if !anyutil.Equal(frozen[key], value) {
			overrides[key] = value
		}
	}
	if len(overrides) == 0 {
		return snapshot, nil
	}
	if _, err := s.Mutate(ctx, sessionID, "launch", overrides, snapshot); err != nil {
		return nil, err
	}
	return s.callObject(ctx, "session.resume", map[string]any{"session_id": sessionID})
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
	cwd, err := launchCwd(ctx)
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
	sid, ok := snapshot.Value("stored_session_id").(string)
	if !ok {
		return "", &errorutil.Error{Kind: "KeyError", Msg: "snapshot is missing stored_session_id"}
	}
	return sid, nil
}
