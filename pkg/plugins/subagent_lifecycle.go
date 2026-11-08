package plugins

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const (
	PublicContractVersion     = 1
	maxGoalChars              = 16_000
	maxContextChars           = 32_000
	maxMetadataBytes          = 8_192
	maxResultChars            = 32_000
	terminalRetentionSeconds  = 3_600
	subagentExecutorMaxWorker = 8
)

// Request the public lifecycle API cannot safely accept, a ValueError in Python
type SubagentLifecycleError struct{ Message string }

func (e *SubagentLifecycleError) Error() string { return e.Message }

func lifecycleErr(format string, a ...any) error {
	return &SubagentLifecycleError{Message: fmt.Sprintf(format, a...)}
}

// Lifecycle state of a delegated subagent, a str enum in Python
type SubagentState string

const (
	SubagentPending         SubagentState = "PENDING"
	SubagentStarting        SubagentState = "STARTING"
	SubagentRunning         SubagentState = "RUNNING"
	SubagentSucceeded       SubagentState = "SUCCEEDED"
	SubagentFailed          SubagentState = "FAILED"
	SubagentInterrupted     SubagentState = "INTERRUPTED"
	SubagentCancelRequested SubagentState = "CANCEL_REQUESTED"
	SubagentCancelled       SubagentState = "CANCELLED"
	SubagentUnknown         SubagentState = "UNKNOWN"
)

func (s SubagentState) PythonEnumValue() any { return string(s) }

// Launch request of one delegated subagent, nil pointers and slices meaning None
type SubagentLaunchRequest struct {
	Goal             string         `json:"goal"`
	Context          *string        `json:"context"`
	Role             string         `json:"role"`
	Model            *string        `json:"model"`
	Provider         *string        `json:"provider"`
	AllowedToolsets  []string       `json:"allowed_toolsets"`
	BlockedTools     []string       `json:"blocked_tools"`
	WorkingDirectory *string        `json:"working_directory"`
	ParentSessionID  *string        `json:"parent_session_id"`
	CorrelationID    *string        `json:"correlation_id"`
	Metadata         map[string]any `json:"metadata"`
	TimeoutSeconds   *float64       `json:"timeout_seconds"`
	goalInvalid      bool
	contextInvalid   bool
	roleInvalid      bool
	providerInvalid  bool
}

// Serializable handle of a launched subagent carrying its capability
type SubagentHandle struct {
	ContractVersion int     `json:"contract_version"`
	SubagentID      string  `json:"subagent_id"`
	ParentSessionID *string `json:"parent_session_id"`
	CorrelationID   *string `json:"correlation_id"`
	CreatedAt       float64 `json:"created_at"`
	Provider        *string `json:"provider"`
	Model           *string `json:"model"`
	Role            string  `json:"role"`
	Depth           int     `json:"depth"`
	Capability      string  `json:"capability"`
	malformed       bool
}

var subagentHandleFields = []string{"contract_version", "subagent_id", "parent_session_id", "correlation_id", "created_at", "provider", "model", "role", "depth", "capability"}

func optStrValue(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// Field dict of the handle in declaration order
func (h *SubagentHandle) ToDict() map[string]any {
	return map[string]any{"contract_version": h.ContractVersion, "subagent_id": h.SubagentID, "parent_session_id": optStrValue(h.ParentSessionID),
		"correlation_id": optStrValue(h.CorrelationID), "created_at": h.CreatedAt, "provider": optStrValue(h.Provider), "model": optStrValue(h.Model),
		"role": h.Role, "depth": h.Depth, "capability": h.Capability}
}

// Handle rebuilt from its dict, malformed when a field is missing or unknown
func SubagentHandleFromDict(value map[string]any) (*SubagentHandle, error) {
	if len(value) != len(subagentHandleFields) {
		return nil, lifecycleErr("Malformed subagent handle.")
	}
	for _, f := range subagentHandleFields {
		if _, ok := value[f]; !ok {
			return nil, lifecycleErr("Malformed subagent handle.")
		}
	}
	h := &SubagentHandle{}
	optStr := func(v any) (*string, bool) {
		if v == nil {
			return nil, true
		}
		s, ok := v.(string)
		return &s, ok
	}
	ok := true
	check := func(good bool) { ok = ok && good }
	n, isInt := pyrepr.Int(value["contract_version"])
	_, isBool := value["contract_version"].(bool)
	check(isInt && !isBool && n.IsInt64() && n.Int64() == PublicContractVersion)
	h.ContractVersion = PublicContractVersion
	id, good := value["subagent_id"].(string)
	check(good && id != "")
	h.SubagentID = id
	var g bool
	h.ParentSessionID, g = optStr(value["parent_session_id"])
	check(g)
	h.CorrelationID, g = optStr(value["correlation_id"])
	check(g)
	created, g := pyNumber(value["created_at"])
	_, isBool = value["created_at"].(bool)
	check(g && !isBool && !math.IsInf(created, 0) && !math.IsNaN(created))
	h.CreatedAt = created
	h.Provider, g = optStr(value["provider"])
	check(g)
	h.Model, g = optStr(value["model"])
	check(g)
	h.Role, g = value["role"].(string)
	check(g)
	depth, isInt := pyrepr.Int(value["depth"])
	_, isBool = value["depth"].(bool)
	check(isInt && !isBool && depth.IsInt64())
	if isInt && depth.IsInt64() {
		h.Depth = int(depth.Int64())
	}
	h.Capability, g = value["capability"].(string)
	check(g)
	h.malformed = !ok
	return h, nil
}

// Status snapshot, Handle the handle given when it is unknown
type SubagentStatus struct {
	Handle     any           `json:"handle"`
	State      SubagentState `json:"state"`
	UpdatedAt  float64       `json:"updated_at"`
	Diagnostic *string       `json:"diagnostic"`
}

// Outcome of waiting on a subagent
type SubagentTerminalState struct {
	Handle     any           `json:"handle"`
	State      SubagentState `json:"state"`
	Completed  bool          `json:"completed"`
	TimedOut   bool          `json:"timed_out"`
	Diagnostic *string       `json:"diagnostic"`
}

// Outcome of a cancellation request
type SubagentCancelResult struct {
	Accepted        bool          `json:"accepted"`
	AlreadyTerminal bool          `json:"already_terminal"`
	UnknownHandle   bool          `json:"unknown_handle"`
	Unsupported     bool          `json:"unsupported"`
	State           SubagentState `json:"state"`
}

// Terminal result snapshot of a subagent, ready once it finished
type SubagentResult struct {
	Handle               any            `json:"handle"`
	TerminalState        SubagentState  `json:"terminal_state"`
	Ready                bool           `json:"ready"`
	Summary              *string        `json:"summary"`
	StructuredPayload    map[string]any `json:"structured_payload"`
	StartedAt            *float64       `json:"started_at"`
	CompletedAt          *float64       `json:"completed_at"`
	ErrorClassification  *string        `json:"error_classification"`
	ErrorMessage         *string        `json:"error_message"`
	UsageMetadata        map[string]any `json:"usage_metadata"`
	ToolExecutionSummary map[string]any `json:"tool_execution_summary"`
	ResultHash           *string        `json:"result_hash"`
}

// Outcome of reconnecting a serialized handle
type SubagentReconnectResult struct {
	Connected  bool          `json:"connected"`
	State      SubagentState `json:"state"`
	Diagnostic *string       `json:"diagnostic"`
}

// Child agent the delegate tool built, as the lifecycle reads it
type SubagentChild interface {
	SubagentID() string
	Provider() *string
	Model() *string
	DelegateRole() (string, bool)
	DelegateDepth() int
}

// Arguments of tools.delegate_tool _build_child_preserving_parent_tools for one lifecycle launch
type SubagentChildSpec struct {
	Goal        string
	Context     *string
	Toolsets    []string
	Model       any
	Parent      any
	Role        string
	Credentials map[string]any
	RoutingCfg  map[string]any
}

// Delegate tool, toolset and parent agent collaborators the wiring installs for the lifecycle service
type SubagentDelegation interface {
	SessionID(parent any) string
	EnabledToolsets(parent any) ([]string, bool)
	ValidateToolset(ctx context.Context, name string) bool
	LoadConfig(ctx context.Context) map[string]any
	ResolveCredentials(ctx context.Context, routingCfg map[string]any, parent any) (map[string]any, error)
	BuildChild(ctx context.Context, spec SubagentChildSpec) (SubagentChild, error)
	RunChild(ctx context.Context, goal string, child SubagentChild, parent any) (any, error)
	HardInterrupt(child SubagentChild, reason, toolReason string) (bool, error)
	Steer(subagentID, text string) bool
}

type subagentRecord struct {
	handle      *SubagentHandle
	state       SubagentState
	updatedAt   float64
	agent       SubagentChild
	done        chan struct{}
	startedAt   *float64
	completedAt *float64
	result      *SubagentResult
}

type correlationKey struct {
	parent      string
	hasParent   bool
	correlation string
}

type subagentRegistry struct {
	mu           sync.Mutex
	records      map[string]*subagentRecord
	correlations map[correlationKey]string
}

var (
	lifecycleRegistry = &subagentRegistry{records: map[string]*subagentRecord{}, correlations: map[correlationKey]string{}}
	lifecycleExecutor = make(chan struct{}, subagentExecutorMaxWorker)
	lifecycleSecret   = func() []byte {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			panic(err)
		}
		return b
	}()
	lifecycleNow = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
)

type activeParentKey struct{}

// Context carrying the host owned parent agent of the current turn
func BindSubagentParent(ctx context.Context, parent any) context.Context {
	return context.WithValue(ctx, activeParentKey{}, parent)
}

// Resolver of a weakly held parent agent, nil once the agent is collected
type SubagentParentRef func() any

// Context carrying a weakly held parent so a context outliving the turn never pins the agent
func BindSubagentParentRef(ctx context.Context, ref SubagentParentRef) context.Context {
	return context.WithValue(ctx, activeParentKey{}, ref)
}

// Parent agent bound to this execution context, nil when none or collected
func GetActiveSubagentParent(ctx context.Context) any {
	v := ctx.Value(activeParentKey{})
	if ref, ok := v.(SubagentParentRef); ok {
		return ref()
	}
	return v
}

func strp(s string) *string { return &s }

// Text clipped to the result limit, nil for None
func clipResult(v any) *string {
	if v == nil {
		return nil
	}
	s := []rune(pyrepr.Str(v))
	if len(s) > maxResultChars {
		s = s[:maxResultChars]
	}
	out := string(s)
	return &out
}

func (h *SubagentHandle) wellFormed() bool {
	return h != nil && !h.malformed && h.ContractVersion == PublicContractVersion && h.SubagentID != "" &&
		!math.IsInf(h.CreatedAt, 0) && !math.IsNaN(h.CreatedAt)
}

var requestFields = map[string]bool{"goal": true, "context": true, "role": true, "model": true, "provider": true, "allowed_toolsets": true, "blocked_tools": true,
	"working_directory": true, "parent_session_id": true, "correlation_id": true, "metadata": true, "timeout_seconds": true}

// Launch request from the dataclass or a mapping of its fields
func coerceSubagentRequest(request any) (*SubagentLaunchRequest, error) {
	switch r := request.(type) {
	case *SubagentLaunchRequest:
		return r, nil
	case SubagentLaunchRequest:
		return &r, nil
	}
	values := llmDict(request)
	if values == nil {
		return nil, lifecycleErr("request must be a SubagentLaunchRequest or a mapping of its fields.")
	}
	var unknown []string
	for k := range values {
		if !requestFields[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, lifecycleErr("Unknown request fields: %s.", strings.Join(unknown, ", "))
	}
	goal, has := values["goal"]
	if !has {
		return nil, lifecycleErr("Malformed subagent launch request: SubagentLaunchRequest.__init__() missing 1 required positional argument: 'goal'")
	}
	r := &SubagentLaunchRequest{Role: "leaf", Metadata: map[string]any{}}
	r.Goal, has = goal.(string)
	r.goalInvalid = !has
	optional := func(name string, invalid *bool) *string {
		v, present := values[name]
		if !present || v == nil {
			return nil
		}
		s, ok := v.(string)
		if !ok {
			if invalid != nil {
				*invalid = true
			}
			s = pyrepr.Str(v)
		}
		return &s
	}
	r.Context = optional("context", &r.contextInvalid)
	r.Provider = optional("provider", &r.providerInvalid)
	r.Model = optional("model", nil)
	r.WorkingDirectory = optional("working_directory", nil)
	r.ParentSessionID = optional("parent_session_id", nil)
	r.CorrelationID = optional("correlation_id", nil)
	if role, present := values["role"]; present {
		s, ok := role.(string)
		r.Role, r.roleInvalid = s, !ok
	}
	for name, dst := range map[string]*[]string{"allowed_toolsets": &r.AllowedToolsets, "blocked_tools": &r.BlockedTools} {
		if list, ok := values[name].([]any); ok {
			*dst = make([]string, len(list))
			for i, item := range list {
				(*dst)[i] = pyrepr.Str(item)
			}
		}
	}
	if m := llmDict(values["metadata"]); m != nil {
		r.Metadata = m
	}
	if t, present := values["timeout_seconds"]; present && t != nil {
		f, _ := pyNumber(t)
		r.TimeoutSeconds = &f
	}
	return r, nil
}

// Handle from the dataclass or its dict, nil for anything else or a malformed dict
func coerceSubagentHandle(handle any) *SubagentHandle {
	switch h := handle.(type) {
	case *SubagentHandle:
		return h
	case SubagentHandle:
		return &h
	}
	if m := llmDict(handle); m != nil {
		h, err := SubagentHandleFromDict(m)
		if err != nil {
			return nil
		}
		return h
	}
	return nil
}

// Plugin safe lifecycle service over the host delegate tool, children in process with terminal retention
type SubagentLifecycleService struct {
	parentResolver func(ctx context.Context) any
}

// Service resolving the parent bound to the calling context
func NewSubagentLifecycleService(resolver func(ctx context.Context) any) *SubagentLifecycleService {
	if resolver == nil {
		resolver = GetActiveSubagentParent
	}
	return &SubagentLifecycleService{parentResolver: resolver}
}

func lifecycleDelegation() (SubagentDelegation, error) {
	return needService("SubagentLifecycle", func(s *Services) SubagentDelegation { return s.SubagentLifecycle })
}

func sessionIDOf(d SubagentDelegation, parent any) *string {
	if parent == nil {
		return nil
	}
	if id := d.SessionID(parent); id != "" {
		return &id
	}
	return nil
}

func keyFor(parent *string, correlation string) correlationKey {
	if parent == nil {
		return correlationKey{correlation: correlation}
	}
	return correlationKey{parent: *parent, hasParent: true, correlation: correlation}
}

// Validates, builds the child through the delegate tool and starts it on the lifecycle executor
func (s *SubagentLifecycleService) Launch(ctx context.Context, request any) (*SubagentHandle, error) {
	parent := s.parentResolver(ctx)
	if parent == nil {
		return nil, lifecycleErr("No active Icebath parent session is available.")
	}
	d, err := lifecycleDelegation()
	if err != nil {
		return nil, err
	}
	req, err := coerceSubagentRequest(request)
	if err != nil {
		return nil, err
	}
	if err := validateSubagentRequest(ctx, d, req, parent); err != nil {
		return nil, err
	}
	parentSession := sessionIDOf(d, parent)
	if req.ParentSessionID != nil && *req.ParentSessionID != "" && (parentSession == nil || *req.ParentSessionID != *parentSession) {
		return nil, lifecycleErr("parent_session_id does not match the active session.")
	}
	correlation := strOr(req.CorrelationID, "")
	key := keyFor(parentSession, correlation)
	lifecycleRegistry.mu.Lock()
	cleanupLocked()
	_, duplicate := lifecycleRegistry.correlations[key]
	lifecycleRegistry.mu.Unlock()
	if correlation != "" && duplicate {
		return nil, lifecycleErr("Duplicate correlation_id for this parent session.")
	}
	routing := d.LoadConfig(ctx)
	if req.Provider != nil {
		routing = map[string]any{"provider": *req.Provider, "model": strOr(req.Model, "")}
	}
	creds, err := d.ResolveCredentials(ctx, routing, parent)
	if err != nil {
		return nil, lifecycleErr("%s", err.Error())
	}
	var model any = creds["model"]
	if req.Model != nil && *req.Model != "" {
		model = *req.Model
	}
	var toolsets []string
	if len(req.AllowedToolsets) > 0 {
		toolsets = append([]string(nil), req.AllowedToolsets...)
	}
	child, err := d.BuildChild(ctx, SubagentChildSpec{Goal: req.Goal, Context: req.Context, Toolsets: toolsets, Model: model, Parent: parent,
		Role: req.Role, Credentials: creds, RoutingCfg: routing})
	if err != nil {
		return nil, err
	}
	subagentID := ""
	if child != nil {
		subagentID = child.SubagentID()
	}
	if subagentID == "" {
		return nil, lifecycleErr("Icebath failed to assign a child identity.")
	}
	created := lifecycleNow()
	role, ok := child.DelegateRole()
	if !ok {
		role = req.Role
	}
	depth := child.DelegateDepth()
	if depth == 0 {
		depth = 1
	}
	handle := &SubagentHandle{ContractVersion: PublicContractVersion, SubagentID: subagentID, ParentSessionID: parentSession, CorrelationID: req.CorrelationID,
		CreatedAt: created, Provider: child.Provider(), Model: child.Model(), Role: role, Depth: depth, Capability: lifecycleCapability(subagentID, parentSession, created)}
	record := &subagentRecord{handle: handle, state: SubagentPending, updatedAt: created, agent: child, done: make(chan struct{})}
	lifecycleRegistry.mu.Lock()
	lifecycleRegistry.records[subagentID] = record
	if correlation != "" {
		lifecycleRegistry.correlations[key] = subagentID
	}
	lifecycleRegistry.mu.Unlock()
	go func() {
		lifecycleExecutor <- struct{}{}
		defer func() { <-lifecycleExecutor }()
		runSubagent(context.WithoutCancel(ctx), d, record, req.Goal, parent)
	}()
	return handle, nil
}

// Current state of a handle, UNKNOWN_HANDLE for anything not owned by the active parent
func (s *SubagentLifecycleService) Status(ctx context.Context, handle any) *SubagentStatus {
	record := s.record(ctx, handle)
	if record == nil {
		return &SubagentStatus{Handle: handle, State: SubagentUnknown, UpdatedAt: lifecycleNow(), Diagnostic: strp("UNKNOWN_HANDLE")}
	}
	lifecycleRegistry.mu.Lock()
	defer lifecycleRegistry.mu.Unlock()
	return &SubagentStatus{Handle: record.handle, State: record.state, UpdatedAt: record.updatedAt}
}

// Waits for the child to finish, timed out when the timeout elapses first, nil timeout waiting forever
func (s *SubagentLifecycleService) Wait(ctx context.Context, handle any, timeoutSeconds *float64) *SubagentTerminalState {
	record := s.record(ctx, handle)
	if record == nil {
		return &SubagentTerminalState{Handle: handle, State: SubagentUnknown, Completed: true, Diagnostic: strp("UNKNOWN_HANDLE")}
	}
	if timeoutSeconds != nil {
		timer := time.NewTimer(time.Duration(math.Max(*timeoutSeconds, 0) * float64(time.Second)))
		defer timer.Stop()
		select {
		case <-record.done:
		case <-timer.C:
			lifecycleRegistry.mu.Lock()
			defer lifecycleRegistry.mu.Unlock()
			return &SubagentTerminalState{Handle: record.handle, State: record.state, TimedOut: true}
		}
	} else {
		<-record.done
	}
	lifecycleRegistry.mu.Lock()
	defer lifecycleRegistry.mu.Unlock()
	return &SubagentTerminalState{Handle: record.handle, State: record.state, Completed: record.result != nil}
}

// Requests a hard interrupt of a running child, accepted only when the child took it
func (s *SubagentLifecycleService) Cancel(ctx context.Context, handle any, reason string) *SubagentCancelResult {
	record := s.record(ctx, handle)
	if record == nil {
		return &SubagentCancelResult{UnknownHandle: true, State: SubagentUnknown}
	}
	lifecycleRegistry.mu.Lock()
	if record.result != nil {
		state := record.state
		lifecycleRegistry.mu.Unlock()
		return &SubagentCancelResult{AlreadyTerminal: true, State: state}
	}
	agent := record.agent
	record.state = SubagentCancelRequested
	record.updatedAt = lifecycleNow()
	lifecycleRegistry.mu.Unlock()
	accepted := false
	if agent != nil {
		if d, err := lifecycleDelegation(); err == nil {
			func() {
				defer func() { _ = recover() }()
				r := []rune(reason)
				if len(r) > 500 {
					r = r[:500]
				}
				ok, err := d.HardInterrupt(agent, "Lifecycle cancellation requested: "+string(r), "subagent cancellation requested")
				accepted = err == nil && ok
			}()
		}
	}
	return &SubagentCancelResult{Accepted: accepted, Unsupported: !accepted, State: SubagentCancelRequested}
}

// Terminal result, NOT_READY while running and UNKNOWN_HANDLE for foreign handles
func (s *SubagentLifecycleService) Result(ctx context.Context, handle any) *SubagentResult {
	record := s.record(ctx, handle)
	if record == nil {
		return &SubagentResult{Handle: handle, TerminalState: SubagentUnknown, ErrorClassification: strp("UNKNOWN_HANDLE"), UsageMetadata: map[string]any{}, ToolExecutionSummary: map[string]any{}}
	}
	lifecycleRegistry.mu.Lock()
	defer lifecycleRegistry.mu.Unlock()
	if record.result != nil {
		return record.result
	}
	return &SubagentResult{Handle: record.handle, TerminalState: record.state, ErrorClassification: strp("NOT_READY"), UsageMetadata: map[string]any{}, ToolExecutionSummary: map[string]any{}}
}

// Whether a handle still names a record of this process, never relaunching work
func (s *SubagentLifecycleService) Reconnect(ctx context.Context, handle any) *SubagentReconnectResult {
	record := s.record(ctx, handle)
	if record == nil {
		return &SubagentReconnectResult{State: SubagentUnknown, Diagnostic: strp("RECONNECT_UNAVAILABLE")}
	}
	lifecycleRegistry.mu.Lock()
	defer lifecycleRegistry.mu.Unlock()
	return &SubagentReconnectResult{Connected: true, State: record.state}
}

// Queues text for the child next iteration boundary while it accepts steering
func (s *SubagentLifecycleService) Steer(ctx context.Context, handle any, text any) bool {
	record := s.record(ctx, handle)
	t, ok := text.(string)
	if record == nil || !ok || pyStrip(t) == "" {
		return false
	}
	lifecycleRegistry.mu.Lock()
	terminal := record.result != nil
	lifecycleRegistry.mu.Unlock()
	if terminal {
		return false
	}
	d, err := lifecycleDelegation()
	if err != nil {
		return false
	}
	return d.Steer(record.handle.SubagentID, pyStrip(t))
}

// Record of a well formed capability verified handle owned by the active parent
func (s *SubagentLifecycleService) record(ctx context.Context, handle any) *subagentRecord {
	h := coerceSubagentHandle(handle)
	if !h.wellFormed() {
		return nil
	}
	expected := lifecycleCapability(h.SubagentID, h.ParentSessionID, h.CreatedAt)
	if !hmac.Equal([]byte(h.Capability), []byte(expected)) {
		return nil
	}
	d, err := lifecycleDelegation()
	if err != nil {
		return nil
	}
	active := sessionIDOf(d, s.parentResolver(ctx))
	if (active == nil) != (h.ParentSessionID == nil) || (active != nil && *active != *h.ParentSessionID) {
		return nil
	}
	lifecycleRegistry.mu.Lock()
	defer lifecycleRegistry.mu.Unlock()
	return lifecycleRegistry.records[h.SubagentID]
}

// Drops terminal snapshots past the retention window, never live work
func cleanupLocked() {
	cutoff := lifecycleNow() - terminalRetentionSeconds
	for id, record := range lifecycleRegistry.records {
		if record.result != nil && record.completedAt != nil && *record.completedAt < cutoff {
			delete(lifecycleRegistry.records, id)
			if record.handle.CorrelationID != nil && *record.handle.CorrelationID != "" {
				delete(lifecycleRegistry.correlations, keyFor(record.handle.ParentSessionID, *record.handle.CorrelationID))
			}
		}
	}
}

// Runs the child through the delegate tool and stores the hashed terminal result
func runSubagent(ctx context.Context, d SubagentDelegation, record *subagentRecord, goal string, parent any) {
	defer close(record.done)
	lifecycleRegistry.mu.Lock()
	if record.state != SubagentCancelRequested {
		record.state = SubagentRunning
	}
	started := lifecycleNow()
	record.startedAt, record.updatedAt = &started, started
	agent := record.agent
	lifecycleRegistry.mu.Unlock()
	result := &SubagentResult{Handle: record.handle, Ready: true, StartedAt: &started, UsageMetadata: map[string]any{}, ToolExecutionSummary: map[string]any{}}
	raw, err := func() (raw any, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = &HostBuiltinError{Type: "RuntimeError", Message: pyrepr.Str(r)}
			}
		}()
		return d.RunChild(ctx, goal, agent, parent)
	}()
	if err != nil {
		result.TerminalState = SubagentFailed
		result.ErrorClassification = strp(pyErrorTypeName(err))
		result.ErrorMessage = clipResult(err.Error())
	} else {
		m, isDict := raw.(map[string]any)
		if !isDict {
			m = map[string]any{}
		}
		status := "error"
		if v, ok := m["status"]; ok {
			status = pyrepr.Str(v)
		}
		lifecycleRegistry.mu.Lock()
		cancelRequested := record.state == SubagentCancelRequested
		lifecycleRegistry.mu.Unlock()
		switch {
		case status == "interrupted" && cancelRequested:
			result.TerminalState = SubagentCancelled
		case status == "interrupted":
			result.TerminalState = SubagentInterrupted
		case status == "completed":
			result.TerminalState = SubagentSucceeded
		default:
			result.TerminalState = SubagentFailed
		}
		result.Summary = clipResult(m["summary"])
		if pyrepr.Truthy(m["error"]) {
			result.ErrorMessage = clipResult(m["error"])
		}
		if result.TerminalState != SubagentSucceeded {
			result.ErrorClassification = strp(strings.ToUpper(status))
		}
		if isDict {
			result.UsageMetadata = map[string]any{"api_calls": dictGet(m, "api_calls", 0)}
			result.ToolExecutionSummary = map[string]any{"duration_seconds": dictGet(m, "duration_seconds", 0)}
		}
	}
	completed := lifecycleNow()
	result.CompletedAt = &completed
	result.ResultHash = strp(subagentResultHash(result))
	lifecycleRegistry.mu.Lock()
	record.agent, record.result, record.state = nil, result, result.TerminalState
	record.completedAt, record.updatedAt = &completed, completed
	lifecycleRegistry.mu.Unlock()
}

func dictGet(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// Hex sha256 of the result dict without its hash, sorted keys and str for anything unserializable
func subagentResultHash(r *SubagentResult) string {
	var handle any = r.Handle
	if h, ok := r.Handle.(*SubagentHandle); ok {
		handle = h.ToDict()
	}
	floatOrNil := func(p *float64) any {
		if p == nil {
			return nil
		}
		return *p
	}
	var payload any
	if r.StructuredPayload != nil {
		payload = r.StructuredPayload
	}
	text, err := pyjson.Dumps(map[string]any{"handle": handle, "terminal_state": string(r.TerminalState), "ready": r.Ready, "summary": optStrValue(r.Summary),
		"structured_payload": payload, "started_at": floatOrNil(r.StartedAt), "completed_at": floatOrNil(r.CompletedAt),
		"error_classification": optStrValue(r.ErrorClassification), "error_message": optStrValue(r.ErrorMessage), "usage_metadata": r.UsageMetadata,
		"tool_execution_summary": r.ToolExecutionSummary}, pyjson.SortKeys(true), pyjson.Default(func(v any) (any, error) { return pyrepr.Str(v), nil }))
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// HMAC capability binding a subagent id to its parent session and creation time
func lifecycleCapability(subagentID string, parentSession *string, created float64) string {
	mac := hmac.New(sha256.New, lifecycleSecret)
	mac.Write([]byte(fmt.Sprintf("%s|%s|%.6f", subagentID, strOr(parentSession, ""), created)))
	return hex.EncodeToString(mac.Sum(nil))
}

// Request rejections in upstream order, then metadata size, then toolset existence and parent scope
func validateSubagentRequest(ctx context.Context, d SubagentDelegation, r *SubagentLaunchRequest, parent any) error {
	switch {
	case r.goalInvalid || pyStrip(r.Goal) == "" || len([]rune(r.Goal)) > maxGoalChars:
		return lifecycleErr("goal must be a non-empty string of at most 16000 characters.")
	case r.Context != nil && (r.contextInvalid || len([]rune(*r.Context)) > maxContextChars):
		return lifecycleErr("context must be a string of at most 32000 characters.")
	case r.roleInvalid || (r.Role != "leaf" && r.Role != "orchestrator"):
		return lifecycleErr("role must be 'leaf' or 'orchestrator'.")
	case r.Provider != nil && (r.providerInvalid || pyStrip(*r.Provider) == "" || len([]rune(*r.Provider)) > 64):
		return lifecycleErr("provider must be a non-empty string of at most 64 characters.")
	case r.TimeoutSeconds != nil:
		return lifecycleErr("Per-launch timeout is not supported; configure delegation timeout explicitly.")
	case r.WorkingDirectory != nil:
		return lifecycleErr("working_directory is not supported because Icebath delegates use isolated task environments.")
	case len(r.BlockedTools) > 0:
		return lifecycleErr("Per-tool blocking is not supported; use allowed_toolsets. Icebath always blocks unsafe child tools.")
	}
	metadata := r.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	text, err := pyjson.Dumps(metadata, pyjson.SortKeys(true))
	if err != nil {
		return lifecycleErr("metadata must be JSON-serializable.")
	}
	if len(text) > maxMetadataBytes {
		return lifecycleErr("metadata exceeds 8192 bytes.")
	}
	if len(r.AllowedToolsets) == 0 {
		return nil
	}
	unknownSet := map[string]bool{}
	for _, name := range r.AllowedToolsets {
		if !d.ValidateToolset(ctx, name) {
			unknownSet[name] = true
		}
	}
	if len(unknownSet) > 0 {
		return lifecycleErr("Unknown toolsets: %s.", strings.Join(sortedKeys(unknownSet), ", "))
	}
	if enabled, ok := d.EnabledToolsets(parent); ok {
		allowed := map[string]bool{}
		for _, name := range enabled {
			allowed[name] = true
		}
		for _, name := range r.AllowedToolsets {
			if !allowed[name] {
				return lifecycleErr("Requested toolsets would broaden parent permissions.")
			}
		}
	}
	return nil
}

var lifecycleMethods = map[string][]pyParam{
	"launch": {req("request")}, "status": {req("handle")}, "wait": {req("handle"), kwo("timeout_seconds", nil)},
	"cancel": {req("handle"), kwReq("reason")}, "result": {req("handle")}, "reconnect": {req("handle")}, "steer": {req("handle"), req("text")},
}

// The seven lifecycle methods for a hosted plugin, each a plain method
func (s *SubagentLifecycleService) HostAttr(_ context.Context, name string) (any, bool, bool, error) {
	if _, ok := lifecycleMethods[name]; ok {
		return nil, true, false, nil
	}
	return nil, false, false, &HostBuiltinError{Type: "AttributeError", Message: fmt.Sprintf("'SubagentLifecycleService' object has no attribute %s", pyrepr.Repr(name))}
}

// Runs a lifecycle method for a hosted plugin with the Python argument binding
func (s *SubagentLifecycleService) HostCall(ctx context.Context, method string, args []any, kwargs map[string]any) (any, error) {
	params, ok := lifecycleMethods[method]
	if !ok {
		return nil, &HostBuiltinError{Type: "AttributeError", Message: fmt.Sprintf("'SubagentLifecycleService' object has no attribute %s", pyrepr.Repr(method))}
	}
	a, _, err := bindPyOn("SubagentLifecycleService", method, params, args, kwargs)
	if err != nil {
		return nil, err
	}
	switch method {
	case "launch":
		return s.Launch(ctx, a["request"])
	case "status":
		return s.Status(ctx, a["handle"]), nil
	case "wait":
		var timeout *float64
		if a["timeout_seconds"] != nil {
			f, ok := pyNumber(a["timeout_seconds"])
			if !ok {
				return nil, &HostBuiltinError{Type: "TypeError", Message: fmt.Sprintf("timeout_seconds must be a number, not %s", pyTypeName(a["timeout_seconds"]))}
			}
			timeout = &f
		}
		return s.Wait(ctx, a["handle"], timeout), nil
	case "cancel":
		reason, ok := a["reason"].(string)
		if !ok {
			return nil, &HostBuiltinError{Type: "TypeError", Message: fmt.Sprintf("'%s' object is not subscriptable", pyTypeName(a["reason"]))}
		}
		return s.Cancel(ctx, a["handle"], reason), nil
	case "result":
		return s.Result(ctx, a["handle"]), nil
	case "reconnect":
		return s.Reconnect(ctx, a["handle"]), nil
	}
	return s.Steer(ctx, a["handle"], a["text"]), nil
}
