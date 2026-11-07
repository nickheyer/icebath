package a2a

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const ProtocolVersion = "1.0"

const (
	StateSubmitted     = "TASK_STATE_SUBMITTED"
	StateWorking       = "TASK_STATE_WORKING"
	StateInputRequired = "TASK_STATE_INPUT_REQUIRED"
	StateCompleted     = "TASK_STATE_COMPLETED"
	StateFailed        = "TASK_STATE_FAILED"
	StateCanceled      = "TASK_STATE_CANCELED"
	StateRejected      = "TASK_STATE_REJECTED"
	RoleUser           = "ROLE_USER"
	RoleAgent          = "ROLE_AGENT"
)

// A reply starting with this marker asks for clarification and moves the task to input required
const InputRequiredMarker = "[INPUT_REQUIRED]"

const (
	ErrParse              = -32700
	ErrInvalidParams      = -32602
	ErrMethodNotFound     = -32601
	ErrTaskNotFound       = -32001
	ErrTaskNotCancelable  = -32002
	ErrUnauthorized       = -32050
	ErrRateLimited        = -32051
	ErrUntrustedPeer      = -32052
	defaultMaxPingpong    = 5
	hardMaxPingpong       = 20
	rateLimitDefault      = 60
	rateWindowSeconds     = 60.0
	turnTTLSeconds        = 3600
	maxTerminalTasks      = 500
	orphanedTaskReply     = "[task orphaned — no reply produced]"
	defaultProviderOrgEnv = "Icebath Agent"
)

// Whether the state ends the task lifecycle
func IsTerminalState(state string) bool {
	switch state {
	case StateCompleted, StateFailed, StateCanceled, StateRejected:
		return true
	}
	return false
}

func envInt(name string, def int) int {
	if v, ok := os.LookupEnv(name); ok {
		return gateway.CoercePort(v, def)
	}
	return gateway.CoercePort(def, def)
}

// Inbound turns allowed per context from A2A_MAX_PINGPONG_TURNS, between one and twenty
func MaxPingpongTurns() int {
	return max(1, min(envInt("A2A_MAX_PINGPONG_TURNS", defaultMaxPingpong), hardMaxPingpong))
}

var nowTime = time.Now

func nowSeconds() float64 { return float64(nowTime().UnixNano()) / 1e9 }

// ISO 8601 UTC timestamp with millisecond precision
func NowISO() string { return nowTime().UTC().Format("2006-01-02T15:04:05.000") + "Z" }

func obj(kv ...any) *pyjson.Object {
	o := pyjson.NewObject()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

func get(v any, key string) (any, bool) {
	switch o := v.(type) {
	case *pyjson.Object:
		if o == nil {
			return nil, false
		}
		return o.Get(key)
	case map[string]any:
		x, ok := o[key]
		return x, ok
	}
	return nil, false
}

func isDict(v any) bool {
	switch o := v.(type) {
	case *pyjson.Object:
		return o != nil
	case map[string]any:
		return true
	}
	return false
}

func getOr(v any, key string) any {
	x, _ := get(v, key)
	return x
}

func orValue(vals ...any) any {
	for _, v := range vals {
		if pyrepr.Truthy(v) {
			return v
		}
	}
	return vals[len(vals)-1]
}

// Agent Card fields, tenant being the routing key clients must echo in request params
type AgentCardOptions struct {
	Name              string
	URL               string
	Description       string
	Skills            []any
	Streaming         bool
	PushNotifications bool
	AuthRequired      bool
	Tenant            string
}

// A2A v1.0 Agent Card with its JSONRPC interface, capabilities and the bearer scheme when auth is required
func BuildAgentCard(o AgentCardOptions) *pyjson.Object {
	iface := obj("url", o.URL, "protocolBinding", "JSONRPC", "protocolVersion", ProtocolVersion)
	if o.Tenant != "" {
		iface.Set("tenant", o.Tenant)
	}
	org := defaultProviderOrgEnv
	if v, ok := os.LookupEnv("A2A_PROVIDER_ORG"); ok {
		org = v
	}
	providerURL := os.Getenv("A2A_PROVIDER_URL")
	if providerURL == "" {
		providerURL = o.URL
	}
	skills := o.Skills
	if len(skills) == 0 {
		skills = []any{}
	}
	card := obj(
		"name", o.Name,
		"description", o.Description,
		"url", o.URL,
		"version", "1.0.0",
		"provider", obj("organization", org, "url", providerURL),
		"supportedInterfaces", []any{iface},
		"capabilities", obj("streaming", o.Streaming, "pushNotifications", o.PushNotifications,
			"stateTransitionHistory", false, "extendedAgentCard", false),
		"defaultInputModes", []any{"text/plain"},
		"defaultOutputModes", []any{"text/plain"},
		"skills", skills,
	)
	if o.AuthRequired {
		card.Set("securitySchemes", obj("bearer", obj("type", "http", "scheme", "bearer")))
		card.Set("security", []any{obj("bearer", []any{})})
	}
	return card
}

// Skill descriptors from toolset names or a toolset to tool names mapping whose first ten tools become tags
func SkillsFromToolsets(toolsets any) []any {
	names := map[string][]any{}
	switch ts := toolsets.(type) {
	case *pyjson.Object:
		if ts != nil {
			for _, k := range ts.Keys {
				v, _ := ts.Get(k)
				names[k] = toolList(v)
			}
		}
	case map[string][]string:
		for k, v := range ts {
			names[k] = toolList(v)
		}
	case map[string]any:
		for k, v := range ts {
			names[k] = toolList(v)
		}
	case []string:
		for _, k := range ts {
			names[k] = nil
		}
	case []any:
		for _, k := range ts {
			names[pyrepr.Str(k)] = nil
		}
	}
	keys := make([]string, 0, len(names))
	for k := range names {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	skills := make([]any, 0, len(keys))
	for _, name := range keys {
		tags := []any{name}
		tools := names[name]
		for i, t := range tools {
			if i == 10 {
				break
			}
			tags = append(tags, pyrepr.Str(t))
		}
		skills = append(skills, obj("id", "toolset."+name, "name", name,
			"description", fmt.Sprintf("Icebath '%s' capabilities", name), "tags", tags))
	}
	if len(skills) == 0 {
		return []any{obj("id", "general", "name", "general", "description", "General-purpose conversational agent",
			"tags", []any{"general"})}
	}
	return skills
}

func toolList(v any) []any {
	switch t := v.(type) {
	case string:
		var out []any
		for _, r := range t {
			out = append(out, string(r))
		}
		return out
	case []any:
		return t
	case []string:
		out := make([]any, len(t))
		for i, s := range t {
			out[i] = s
		}
		return out
	}
	return nil
}

// JSON-RPC 2.0 result envelope
func JsonrpcResult(reqID, result any) *pyjson.Object {
	return obj("jsonrpc", "2.0", "id", reqID, "result", result)
}

// JSON-RPC 2.0 error envelope
func JsonrpcError(reqID any, code int, message string) *pyjson.Object {
	return obj("jsonrpc", "2.0", "id", reqID, "error", obj("code", code, "message", message))
}

// SendMessageResponse holding a task when the payload has a status and an id and a message otherwise
func SendMessageResponse(payload any) *pyjson.Object {
	if isDict(payload) && pyrepr.Truthy(getOr(payload, "status")) && pyrepr.Truthy(getOr(payload, "id")) {
		return obj("task", payload)
	}
	return obj("message", payload)
}

// Task or Message inside a v1.0 response, a legacy bare payload passing through
func UnwrapSendMessageResponse(result any) any {
	if isDict(result) {
		if t := getOr(result, "task"); isDict(t) {
			return t
		}
		if m := getOr(result, "message"); isDict(m) {
			return m
		}
	}
	return result
}

// StreamResponse with a task member
func StreamTask(task any) *pyjson.Object { return obj("task", task) }

func hex16() string { return strings.ReplaceAll(uuid.NewString(), "-", "")[:16] }

func NewTaskID() string { return "task-" + hex16() }

func NewContextID() string { return "ctx-" + hex16() }

// Context a first message without a contextId opens, named by the message so a retried send reopens it
func MessageContextID(scope, messageID string) string {
	sum := sha256.Sum256([]byte(scope + "\x00" + messageID))
	return "ctx-" + hex.EncodeToString(sum[:])[:16]
}

// v1.0 text Part
func TextPart(text string) *pyjson.Object { return obj("text", text, "mediaType", "text/plain") }

// v1.0 Message with a single text Part
func TextMessage(role, text, contextID string) *pyjson.Object {
	msg := obj("role", role, "parts", []any{TextPart(text)}, "messageId", strings.ReplaceAll(uuid.NewString(), "-", ""))
	if contextID != "" {
		msg.Set("contextId", contextID)
	}
	return msg
}

func fileNote(fname any, body string, mtype any) string {
	label := "[file]"
	if pyrepr.Truthy(fname) {
		label = "[file: " + pyrepr.Str(fname) + "]"
	}
	out := label + " " + body
	if pyrepr.Truthy(mtype) {
		out += " (" + pyrepr.Str(mtype) + ")"
	}
	return out
}

func jsonOrStr(data any) string {
	text, err := pyjson.Dumps(data, pyjson.EnsureASCII(false), pyjson.Default(func(v any) (any, error) { return pyrepr.Str(v), nil }))
	if err != nil {
		return pyrepr.Str(data)
	}
	return text
}

// Concatenated text of a Message, Task result or params payload across v1.0, v0.3 and older Part shapes
func ExtractText(messageOrParams *pyjson.Object) string {
	msg, ok := messageOrParams.Get("message")
	if !ok {
		msg = messageOrParams
	}
	var chunks []string
	var parts []any
	if isDict(msg) {
		parts, _ = getOr(msg, "parts").([]any)
	}
	for _, part := range parts {
		if !isDict(part) {
			continue
		}
		if txt, ok := getOr(part, "text").(string); ok {
			chunks = append(chunks, txt)
		} else if url, ok := getOr(part, "url").(string); ok && url != "" {
			chunks = append(chunks, fileNote(orValue(getOr(part, "filename"), getOr(part, "name"), ""), url,
				orValue(getOr(part, "mediaType"), getOr(part, "mimeType"), "")))
		} else if v03 := getOr(part, "file"); isDict(v03) && isString(getOr(v03, "fileWithUri")) {
			chunks = append(chunks, fileNote(orValue(getOr(v03, "name"), ""), getOr(v03, "fileWithUri").(string),
				orValue(getOr(v03, "mimeType"), "")))
		} else if raw, ok := getOr(part, "raw").(string); ok {
			chunks = append(chunks, fileNote(orValue(getOr(part, "filename"), ""),
				fmt.Sprintf("%d bytes base64-encoded", utf8.RuneCountInString(raw)), orValue(getOr(part, "mediaType"), "")))
		} else if data := getOr(part, "data"); data != nil {
			chunks = append(chunks, fmt.Sprintf("[data (%s)]\n%s", pyrepr.Str(orValue(getOr(part, "mediaType"), "application/json")), jsonOrStr(data)))
		}
	}
	return pystr.Strip(strings.Join(chunks, "\n"))
}

func isString(v any) bool {
	_, ok := v.(string)
	return ok
}

// messageId inside the v1.0 Message, repeated by a peer retrying a send
func ExtractMessageID(params *pyjson.Object) string {
	msg := orValue(getOr(params, "message"), obj())
	if !isDict(msg) {
		return ""
	}
	return pyrepr.Str(orValue(getOr(msg, "messageId"), ""))
}

// contextId inside the v1.0 Message, a legacy top level contextId accepted
func ExtractContextID(params *pyjson.Object) string {
	msg := orValue(getOr(params, "message"), obj())
	if isDict(msg) {
		if s := pyrepr.Str(orValue(getOr(msg, "contextId"), "")); s != "" {
			return s
		}
	}
	return pyrepr.Str(orValue(getOr(params, "contextId"), ""))
}

// v1.0 Task whose agent text becomes the status message and, once completed, its artifact
func BuildTask(taskID, contextID, state, agentText string) *pyjson.Object {
	status := obj("state", state, "timestamp", NowISO())
	task := obj("id", taskID, "contextId", contextID, "status", status)
	if agentText != "" {
		status.Set("message", TextMessage(RoleAgent, agentText, contextID))
		if state == StateCompleted {
			task.Set("artifacts", []any{obj("artifactId", strings.ReplaceAll(uuid.NewString(), "-", ""), "parts", []any{TextPart(agentText)})})
		}
	}
	return task
}

// StreamResponse with a statusUpdate member
func StatusUpdate(taskID, contextID, state, text string) *pyjson.Object {
	status := obj("state", state, "timestamp", NowISO())
	if text != "" {
		status.Set("message", TextMessage(RoleAgent, text, contextID))
	}
	return obj("statusUpdate", obj("taskId", taskID, "contextId", contextID, "status", status))
}

// StreamResponse with an artifactUpdate member
func ArtifactUpdate(taskID, contextID, text string) *pyjson.Object {
	artifact := obj("artifactId", strings.ReplaceAll(uuid.NewString(), "-", ""), "parts", []any{TextPart(text)})
	return obj("artifactUpdate", obj("taskId", taskID, "contextId", contextID, "artifact", artifact))
}

// One StreamResponse as an SSE data frame inside a JSON-RPC envelope unless the request id is nil
func SSEData(payload, reqID any) (string, error) {
	envelope := payload
	if reqID != nil {
		envelope = JsonrpcResult(reqID, payload)
	}
	text, err := pyjson.Dumps(envelope, pyjson.EnsureASCII(false))
	if err != nil {
		return "", err
	}
	return "data: " + text + "\n\n", nil
}

// Stream closure marker as an SSE comment
func SSEDone() string { return ": done\n\n" }

type turnEntry struct {
	count    int
	lastSeen float64
}

// Inbound turns per context, beyond the pingpong cap the adapter rejects
type TurnTracker struct {
	mu    sync.Mutex
	turns *pydict.Dict[string, turnEntry]
}

func NewTurnTracker() *TurnTracker {
	return &TurnTracker{turns: pydict.New[string, turnEntry]()}
}

// Increments and returns the context turn count after pruning contexts idle past an hour
func (t *TurnTracker) Track(contextID string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := nowSeconds()
	kept := pydict.New[string, turnEntry]()
	for cid, v := range t.turns.All() {
		if now-v.lastSeen <= turnTTLSeconds {
			kept.Set(cid, v)
		}
	}
	t.turns = kept
	prev, _ := t.turns.Get(contextID)
	count := prev.count + 1
	t.turns.Set(contextID, turnEntry{count, now})
	return count
}

func (t *TurnTracker) Reset(contextID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.turns.Delete(contextID)
}

// Sliding window request limiter with one bucket per authenticated identity
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string][]float64
}

func NewRateLimiter() *RateLimiter { return &RateLimiter{buckets: map[string][]float64{}} }

// Admits the request while the identity stays under A2A_RATE_LIMIT requests per minute
func (r *RateLimiter) Allow(identity string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	limit := max(1, envInt("A2A_RATE_LIMIT", rateLimitDefault))
	now := nowSeconds()
	bucket := r.buckets[identity]
	for len(bucket) > 0 && now-bucket[0] > rateWindowSeconds {
		bucket = bucket[1:]
	}
	if len(bucket) >= limit {
		r.buckets[identity] = bucket
		return false
	}
	r.buckets[identity] = append(bucket, now)
	return true
}

// Counters of A2A operations shared by the inbound adapter and the outbound tools
type Metrics struct {
	InboundTotal      atomic.Int64
	OutboundTotal     atomic.Int64
	StreamsStarted    atomic.Int64
	PushSent          atomic.Int64
	PushFailed        atomic.Int64
	TasksCompleted    atomic.Int64
	TasksFailed       atomic.Int64
	AntiLoopTriggers  atomic.Int64
	RateLimitTriggers atomic.Int64
	startTime         float64
	mu                sync.Mutex
	latencies         []float64
}

func NewMetrics() *Metrics { return &Metrics{startTime: nowSeconds()} }

// Module wide metrics the adapter and the tools record into
var SharedMetrics = NewMetrics()

// Keeps the latency among the last hundred completed inbound tasks
func (m *Metrics) RecordLatency(seconds float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.latencies = append(m.latencies, seconds)
	if len(m.latencies) > 100 {
		m.latencies = m.latencies[len(m.latencies)-100:]
	}
}

func (m *Metrics) AvgLatency() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.latencies) == 0 {
		return 0
	}
	sum := 0.0
	for _, l := range m.latencies {
		sum += l
	}
	return sum / float64(len(m.latencies))
}

// Uptime, every counter and the average latency in milliseconds
func (m *Metrics) Snapshot() *pyjson.Object {
	return obj("uptime_seconds", pyint.Round(nowSeconds()-m.startTime, 1),
		"inbound_total", m.InboundTotal.Load(), "outbound_total", m.OutboundTotal.Load(),
		"streams_started", m.StreamsStarted.Load(), "push_sent", m.PushSent.Load(), "push_failed", m.PushFailed.Load(),
		"tasks_completed", m.TasksCompleted.Load(), "tasks_failed", m.TasksFailed.Load(),
		"anti_loop_triggers", m.AntiLoopTriggers.Load(), "rate_limit_triggers", m.RateLimitTriggers.Load(),
		"avg_latency_ms", pyint.Round(m.AvgLatency()*1000, 1))
}

// Stored A2A task with its scope, push config and outcome
type TaskRecord struct {
	TaskID       string
	ContextID    string
	Peer         string
	AgentSlug    string
	Tenant       string
	InputID      string
	State        string
	Reply        string
	CreatedAt    float64
	CreatedISO   string
	PushURL      string
	PushConfigID string
	CompletedAt  *float64
}

// Terminal state and reply a task watch resolves with
type TaskWatch struct {
	once  sync.Once
	done  chan struct{}
	state string
	reply string
}

func newTaskWatch() *TaskWatch { return &TaskWatch{done: make(chan struct{})} }

func (w *TaskWatch) set(state, reply string) {
	w.once.Do(func() {
		w.state, w.reply = state, reply
		close(w.done)
	})
}

// Channel closed once the task reached a terminal state
func (w *TaskWatch) C() <-chan struct{} { return w.done }

func (w *TaskWatch) Done() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

// Waits for the terminal state and reply or the context
func (w *TaskWatch) Result(ctx context.Context) (string, string, error) {
	select {
	case <-w.done:
		return w.state, w.reply, nil
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
}

// In memory tasks kept after completion whose readers see only records inside their scope
type TaskStore struct {
	mu       sync.Mutex
	tasks    *pydict.Dict[string, *TaskRecord]
	watchers map[string][]*TaskWatch
}

func NewTaskStore() *TaskStore {
	return &TaskStore{tasks: pydict.New[string, *TaskRecord](), watchers: map[string][]*TaskWatch{}}
}

func inScope(rec *TaskRecord, agentSlug, tenant string) bool {
	return !((agentSlug != "" && rec.AgentSlug != agentSlug) || (tenant != "" && rec.Tenant != tenant))
}

func (s *TaskStore) scoped(taskID, agentSlug, tenant string) *TaskRecord {
	rec, ok := s.tasks.Get(taskID)
	if ok && inScope(rec, agentSlug, tenant) {
		return rec
	}
	return nil
}

func (s *TaskStore) pushRec(taskID, configID, agentSlug, tenant string) *TaskRecord {
	rec := s.scoped(taskID, agentSlug, tenant)
	if rec != nil && rec.PushURL != "" && (configID == "" || rec.PushConfigID == configID) {
		return rec
	}
	return nil
}

func pushConfigView(rec *TaskRecord) *pyjson.Object {
	return obj("configId", rec.PushConfigID, "taskId", rec.TaskID, "createdAt", rec.CreatedISO,
		"pushNotificationConfig", obj("url", rec.PushURL))
}

// Stores a submitted task, inputID naming the owner admission a forwarded task was sent as
func (s *TaskStore) Create(taskID, contextID, peer, agentSlug, tenant, inputID string) TaskRecord {
	rec := &TaskRecord{TaskID: taskID, ContextID: contextID, Peer: peer, AgentSlug: agentSlug, Tenant: tenant,
		InputID: inputID, State: StateSubmitted, CreatedAt: nowSeconds(), CreatedISO: NowISO()}
	s.mu.Lock()
	s.tasks.Set(taskID, rec)
	s.mu.Unlock()
	return *rec
}

// Moves a live task to the state, terminal tasks staying put
func (s *TaskStore) SetState(taskID, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.tasks.Get(taskID); ok && !IsTerminalState(rec.State) {
		rec.State = state
	}
}

// Attaches a push notification config to the scoped task and returns its view, nil outside scope
func (s *TaskStore) SetPushConfig(taskID, url, agentSlug, tenant string) *pyjson.Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.scoped(taskID, agentSlug, tenant)
	if rec == nil {
		return nil
	}
	rec.PushURL, rec.PushConfigID = url, "cfg-"+strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	return pushConfigView(rec)
}

func (s *TaskStore) GetPushConfig(taskID, configID, agentSlug, tenant string) *pyjson.Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec := s.pushRec(taskID, configID, agentSlug, tenant); rec != nil {
		return pushConfigView(rec)
	}
	return nil
}

func (s *TaskStore) ListPushConfigs(taskID, agentSlug, tenant string) []*pyjson.Object {
	if cfg := s.GetPushConfig(taskID, "", agentSlug, tenant); cfg != nil {
		return []*pyjson.Object{cfg}
	}
	return []*pyjson.Object{}
}

// Clears the matching push config and reports whether one existed
func (s *TaskStore) DeletePushConfig(taskID, configID, agentSlug, tenant string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.pushRec(taskID, configID, agentSlug, tenant)
	if rec != nil {
		rec.PushURL, rec.PushConfigID = "", ""
	}
	return rec != nil
}

// Takes the push url off the task so a notification goes out once
func (s *TaskStore) PopPushURL(taskID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.tasks.Get(taskID)
	if !ok {
		return ""
	}
	url := rec.PushURL
	rec.PushURL = ""
	return url
}

// Copy of the scoped task, false outside scope
func (s *TaskStore) Get(taskID, agentSlug, tenant string) (TaskRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec := s.scoped(taskID, agentSlug, tenant); rec != nil {
		return *rec, true
	}
	return TaskRecord{}, false
}

// Moves a live task to its terminal state once and resolves its watchers
func (s *TaskStore) Complete(taskID, state, reply string) (TaskRecord, bool) {
	s.mu.Lock()
	rec, ok := s.tasks.Get(taskID)
	if !ok || IsTerminalState(rec.State) {
		s.mu.Unlock()
		return TaskRecord{}, false
	}
	at := nowSeconds()
	rec.State, rec.Reply, rec.CompletedAt = state, reply, &at
	watchers := s.watchers[taskID]
	delete(s.watchers, taskID)
	s.trimLocked()
	out := *rec
	s.mu.Unlock()
	for _, w := range watchers {
		w.set(state, reply)
	}
	return out, true
}

// Watch resolving with the terminal state and reply, already resolved for a finished task and nil outside scope
func (s *TaskStore) Watch(taskID, agentSlug, tenant string) *TaskWatch {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.scoped(taskID, agentSlug, tenant)
	if rec == nil {
		return nil
	}
	w := newTaskWatch()
	if IsTerminalState(rec.State) {
		w.set(rec.State, rec.Reply)
	} else {
		s.watchers[taskID] = append(s.watchers[taskID], w)
	}
	return w
}

// Task listing filters, page size and offset given as the request supplied them
type ListOptions struct {
	ContextID string
	State     string
	PageSize  any
	Offset    int
	AgentSlug string
	Tenant    string
}

func pySlice[T any](items []T, start, stop int) []T {
	n := len(items)
	clamp := func(i int) int {
		if i < 0 {
			i += n
			if i < 0 {
				return 0
			}
		}
		return min(i, n)
	}
	lo, hi := clamp(start), clamp(stop)
	if lo >= hi {
		return []T{}
	}
	return items[lo:hi]
}

// Newest first task page with the next offset and the total, int of the page size raising like Python
func (s *TaskStore) List(o ListOptions) ([]TaskRecord, int, int, error) {
	raw := o.PageSize
	if !pyrepr.Truthy(raw) {
		raw = 50
	}
	n, err := pyint.Of(raw)
	if err != nil {
		return nil, 0, 0, err
	}
	pageSize := 100
	if n.IsInt64() {
		pageSize = int(max(1, min(n.Int64(), 100)))
	} else if n.Sign() < 0 {
		pageSize = 1
	}
	s.mu.Lock()
	vals := s.tasks.Values()
	var recs []TaskRecord
	for i := len(vals) - 1; i >= 0; i-- {
		r := vals[i]
		if inScope(r, o.AgentSlug, o.Tenant) && (o.ContextID == "" || r.ContextID == o.ContextID) && (o.State == "" || r.State == o.State) {
			recs = append(recs, *r)
		}
	}
	s.mu.Unlock()
	total := len(recs)
	page := pySlice(recs, o.Offset, o.Offset+pageSize)
	next := 0
	if o.Offset+pageSize < total {
		next = o.Offset + pageSize
	}
	return page, next, total, nil
}

// Fails every live task older than the timeout outside the exclusions and returns the ids it failed
func (s *TaskStore) FailOrphans(timeoutSeconds float64, exclude map[string]struct{}) []string {
	s.mu.Lock()
	var stale []string
	for tid, rec := range s.tasks.All() {
		if _, skip := exclude[tid]; !skip && !IsTerminalState(rec.State) && nowSeconds()-rec.CreatedAt > timeoutSeconds {
			stale = append(stale, tid)
		}
	}
	s.mu.Unlock()
	out := []string{}
	for _, tid := range stale {
		if _, ok := s.Complete(tid, StateFailed, orphanedTaskReply); ok {
			out = append(out, tid)
		}
	}
	return out
}

func (s *TaskStore) trimLocked() {
	var terminal []string
	for tid, rec := range s.tasks.All() {
		if IsTerminalState(rec.State) {
			terminal = append(terminal, tid)
		}
	}
	for _, tid := range terminal[:max(0, len(terminal)-maxTerminalTasks)] {
		s.tasks.Delete(tid)
	}
}

// Stored record rendered as a v1.0 Task, artifacts dropped on request
func ToTask(rec TaskRecord, includeArtifacts bool) *pyjson.Object {
	task := BuildTask(rec.TaskID, rec.ContextID, rec.State, rec.Reply)
	if !includeArtifacts {
		task.Delete("artifacts")
	}
	return task
}

func convDir(ctx context.Context) string { return filepath.Join(home.Dir(ctx), "a2a_conversations") }

func convPath(ctx context.Context, contextID string) string {
	if contextID == "" {
		contextID = "default"
	}
	var b strings.Builder
	for _, c := range contextID {
		if pystr.IsAlnum(c) || c == '-' || c == '_' {
			b.WriteRune(c)
		}
	}
	safe := b.String()
	if safe == "" {
		safe = "default"
	}
	return filepath.Join(convDir(ctx), safe+".jsonl")
}

// Appends one message to the context conversation log, swallowing every failure
func PersistMessage(ctx context.Context, contextID, role, text, taskID, inputID string) {
	path := convPath(ctx, contextID)
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return
	}
	entry := obj("ts", nowSeconds(), "role", role, "text", text, "task_id", taskID)
	if inputID != "" {
		entry.Set("input_id", inputID)
	}
	line, err := pyjson.Dumps(entry, pyjson.EnsureASCII(false))
	if err != nil {
		return
	}
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return
	}
	defer fh.Close()
	_, _ = fh.WriteString(line + "\n")
}

// Whether the context log already holds the forwarded input
func LoggedInput(ctx context.Context, contextID, inputID string) bool {
	for _, entry := range LoadConversation(ctx, contextID, 0) {
		if v, ok := entry.Get("input_id"); ok {
			if s, isStr := v.(string); isStr && s == inputID {
				return true
			}
		}
	}
	return false
}

// Last limit messages of the context log, empty when missing or unreadable
func LoadConversation(ctx context.Context, contextID string, limit int) []*pyjson.Object {
	raw, err := os.ReadFile(convPath(ctx, contextID))
	if err != nil {
		return []*pyjson.Object{}
	}
	raw = []byte(strings.TrimPrefix(string(raw), "\uFEFF"))
	if !utf8.Valid(raw) {
		return []*pyjson.Object{}
	}
	out := []*pyjson.Object{}
	for _, line := range pystr.Splitlines(string(raw), false) {
		if pystr.Strip(line) == "" {
			continue
		}
		entry, err := pyjson.Loads(line)
		if err != nil {
			continue
		}
		if o, ok := entry.(*pyjson.Object); ok {
			out = append(out, o)
		}
	}
	return pySlice(out, -limit, len(out))
}

// Context ids with a persisted conversation, sorted
func ListConversations(ctx context.Context) []string {
	entries, err := os.ReadDir(convDir(ctx))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return []string{}
	}
	out := []string{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		stem := strings.TrimSuffix(name, ".jsonl")
		if stem == "" {
			stem = name
		}
		out = append(out, stem)
	}
	slices.Sort(out)
	return out
}
