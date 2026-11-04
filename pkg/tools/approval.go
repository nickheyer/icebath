package tools

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"gopkg.in/yaml.v3"
)

// Process wide yolo flag frozen at start so a skill cannot flip it through the environment later
var yoloModeFrozen = truthyEnvValue(os.Getenv("ICEBATH_YOLO_MODE"))

// ApprovalDecision is the shared verdict every tool slice reads before running a command
type ApprovalDecision struct {
	Allowed bool
	Reason  string
	Pattern string
}

// GuardResult is the full approval verdict of one gate, the dict shape hermes gates return
type GuardResult struct {
	Approved        bool
	Message         string
	PatternKey      string
	Description     string
	Outcome         string
	UserSummary     string
	Status          string
	Command         string
	ApprovalPending bool
	SmartApproved   bool
	SmartDenied     bool
	UserApproved    bool
	AllowPermanent  bool
	DenyReason      string
}

// Denied reports a non consent verdict carrying the retry contract rather than a plain block
func (r GuardResult) Denied() bool { return !r.Approved && r.Outcome != "" }

// PendingApproval is one approval parked for later review through approve or deny commands
type PendingApproval struct {
	Command        string
	PatternKey     string
	PatternKeys    []string
	Description    string
	SmartDenied    bool
	AllowPermanent bool
}

// GatewayNotifyFunc sends one approval request to the user through the gateway surface
type GatewayNotifyFunc func(data map[string]any) error

// ApprovalEntry is one pending dangerous command approval inside a gateway session
type ApprovalEntry struct {
	Data         map[string]any
	Acknowledged bool
	Settle       func(reason string)
	Observers    []func(reason string)
	Result       string
	Reason       string
	Cancelled    string
	Expired      bool
	done         chan struct{}
	once         sync.Once
}

// Builds an entry with a fresh request id and its own wake channel
func newApprovalEntry(data map[string]any) *ApprovalEntry {
	copied := make(map[string]any, len(data)+1)
	for k, v := range data {
		copied[k] = v
	}
	if _, ok := copied["request_id"]; !ok {
		copied["request_id"] = newRequestID()
	}
	return &ApprovalEntry{Data: copied, done: make(chan struct{})}
}

// Wakes every waiter exactly once
func (e *ApprovalEntry) set() { e.once.Do(func() { close(e.done) }) }

// Channel closed when the entry is resolved, withdrawn, or expired
func (e *ApprovalEntry) Done() <-chan struct{} { return e.done }

// RequestID of the entry as published to clients
func (e *ApprovalEntry) RequestID() string {
	s, _ := e.Data["request_id"].(string)
	return s
}

// Random hex request identity
func newRequestID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(raw[:])
}

// Per session approval state shared by every gate and the gateway queue
var approvalState = struct {
	sync.Mutex
	pending             map[string]PendingApproval
	sessionApproved     map[string]map[string]bool
	sessionYolo         map[string]bool
	launchYoloApplied   map[string]bool
	permanentByHome     map[string]map[string]bool
	baselineByHome      map[string]map[string]bool
	denialTally         map[string]int
	denialOrder         []string
	gatewayQueues       map[string][]*ApprovalEntry
	gatewayNotify       map[string]GatewayNotifyFunc
	permissionReleasers []func(sessionKey string)
	sessionClosers      []func(sessionKey string)
}{
	pending:           map[string]PendingApproval{},
	sessionApproved:   map[string]map[string]bool{},
	sessionYolo:       map[string]bool{},
	launchYoloApplied: map[string]bool{},
	permanentByHome:   map[string]map[string]bool{},
	baselineByHome:    map[string]map[string]bool{},
	denialTally:       map[string]int{},
	gatewayQueues:     map[string][]*ApprovalEntry{},
	gatewayNotify:     map[string]GatewayNotifyFunc{},
}

const denialTallyMaxSessions = 256

// Threshold from approvals.denial_breaker_threshold, default 3, zero or negative disables
func getDenialBreakerThreshold(env *Env) int {
	if v, ok := config.AsInt(getApprovalConfig(env)["denial_breaker_threshold"]); ok {
		return v
	}
	return 3
}

// Increments and returns the consecutive guardian denial count, evicting idle sessions past the cap
func recordDenial(sessionKey string) int {
	approvalState.Lock()
	defer approvalState.Unlock()
	count := approvalState.denialTally[sessionKey] + 1
	approvalState.denialTally[sessionKey] = count
	approvalState.denialOrder = removeKey(approvalState.denialOrder, sessionKey)
	approvalState.denialOrder = append(approvalState.denialOrder, sessionKey)
	for len(approvalState.denialTally) > denialTallyMaxSessions {
		oldest := approvalState.denialOrder[0]
		approvalState.denialOrder = approvalState.denialOrder[1:]
		delete(approvalState.denialTally, oldest)
	}
	return count
}

// Clears the session denial tally after an approval
func resetDenials(sessionKey string) {
	approvalState.Lock()
	defer approvalState.Unlock()
	delete(approvalState.denialTally, sessionKey)
	approvalState.denialOrder = removeKey(approvalState.denialOrder, sessionKey)
}

// Drops one key from an ordered key list
func removeKey(keys []string, key string) []string {
	out := keys[:0]
	for _, k := range keys {
		if k != key {
			out = append(out, k)
		}
	}
	return out
}

// Escalated hard stop text once the breaker tripped, empty otherwise
func denialBreakerAddendum(env *Env, sessionKey string) string {
	approvalState.Lock()
	count := approvalState.denialTally[sessionKey]
	approvalState.Unlock()
	threshold := getDenialBreakerThreshold(env)
	if threshold <= 0 || count < threshold {
		return ""
	}
	envLog(env).Warn("Smart-approval circuit breaker tripped", "session", sessionKey, "denials", count, "threshold", threshold)
	return fmt.Sprintf(" CIRCUIT BREAKER: %d consecutive commands were blocked by the security reviewer. STOP attempting variations of this operation. Report the blocked operation to the user and either ask them to run it manually or use /approve.", count)
}

// RegisterGatewayNotify installs the callback that sends approval requests for a session
func RegisterGatewayNotify(sessionKey string, cb GatewayNotifyFunc) {
	approvalState.Lock()
	defer approvalState.Unlock()
	approvalState.gatewayNotify[sessionKey] = cb
}

// UnregisterGatewayNotify removes the callback and wakes every blocked waiter of the session
func UnregisterGatewayNotify(sessionKey string) {
	approvalState.Lock()
	defer approvalState.Unlock()
	delete(approvalState.gatewayNotify, sessionKey)
	for _, entry := range approvalState.gatewayQueues[sessionKey] {
		entry.set()
	}
	delete(approvalState.gatewayQueues, sessionKey)
}

// ResolveGatewayApproval unblocks the oldest, the matching, or every pending approval and returns how many
func ResolveGatewayApproval(sessionKey, choice string, resolveAll bool, reason, requestID string) int {
	approvalState.Lock()
	defer approvalState.Unlock()
	queue := approvalState.gatewayQueues[sessionKey]
	if len(queue) == 0 {
		return 0
	}
	var targets, rest []*ApprovalEntry
	switch {
	case requestID != "":
		for _, e := range queue {
			if e.RequestID() == requestID {
				targets = append(targets, e)
			} else {
				rest = append(rest, e)
			}
		}
		if len(targets) == 0 {
			return 0
		}
	case resolveAll:
		targets = append(targets, queue...)
	default:
		targets = []*ApprovalEntry{queue[0]}
		rest = append(rest, queue[1:]...)
	}
	if len(rest) == 0 {
		delete(approvalState.gatewayQueues, sessionKey)
	} else {
		approvalState.gatewayQueues[sessionKey] = rest
	}
	for _, entry := range targets {
		entry.Result = choice
		if reason != "" {
			entry.Reason = reason
		}
		entry.set()
	}
	return len(targets)
}

// WithdrawGatewayApproval cancels one pending approval nobody can answer, false when no longer pending
func WithdrawGatewayApproval(sessionKey, requestID, cause string) bool {
	approvalState.Lock()
	defer approvalState.Unlock()
	queue := approvalState.gatewayQueues[sessionKey]
	for i, entry := range queue {
		if entry.RequestID() != requestID {
			continue
		}
		rest := append(append([]*ApprovalEntry{}, queue[:i]...), queue[i+1:]...)
		if len(rest) == 0 {
			delete(approvalState.gatewayQueues, sessionKey)
		} else {
			approvalState.gatewayQueues[sessionKey] = rest
		}
		entry.Cancelled = cause
		entry.set()
		return true
	}
	return false
}

// ListGatewayApprovals returns replay safe snapshots of the unresolved approvals of one session
func ListGatewayApprovals(sessionKey string) []map[string]any {
	approvalState.Lock()
	defer approvalState.Unlock()
	out := []map[string]any{}
	for _, entry := range approvalState.gatewayQueues[sessionKey] {
		out = append(out, copyMap(entry.Data))
	}
	return out
}

// Finds one pending entry by request id under the lock
func findGatewayEntry(sessionKey, requestID string) *ApprovalEntry {
	for _, entry := range approvalState.gatewayQueues[sessionKey] {
		if entry.RequestID() == requestID {
			return entry
		}
	}
	return nil
}

// RegisterGatewaySettle attaches the hook run once when the wait ends by any path
func RegisterGatewaySettle(sessionKey, requestID string, settle func(reason string)) bool {
	approvalState.Lock()
	defer approvalState.Unlock()
	entry := findGatewayEntry(sessionKey, requestID)
	if entry == nil {
		return false
	}
	entry.Settle = settle
	return true
}

// ObserveGatewaySettle adds an observer without replacing the settle hook
func ObserveGatewaySettle(sessionKey, requestID string, observer func(reason string)) bool {
	approvalState.Lock()
	defer approvalState.Unlock()
	entry := findGatewayEntry(sessionKey, requestID)
	if entry == nil {
		return false
	}
	entry.Observers = append(entry.Observers, observer)
	return true
}

// AckGatewayApproval records that a client received a pending approval request
func AckGatewayApproval(sessionKey, requestID string) bool {
	approvalState.Lock()
	defer approvalState.Unlock()
	entry := findGatewayEntry(sessionKey, requestID)
	if entry == nil {
		return false
	}
	entry.Acknowledged = true
	return true
}

// HasBlockingApproval reports whether the session has a gateway approval waiting
func HasBlockingApproval(sessionKey string) bool {
	approvalState.Lock()
	defer approvalState.Unlock()
	return len(approvalState.gatewayQueues[sessionKey]) > 0
}

// PendingGatewayApprovalCount counts unresolved gateway approvals across every session
func PendingGatewayApprovalCount() int {
	approvalState.Lock()
	defer approvalState.Unlock()
	n := 0
	for _, q := range approvalState.gatewayQueues {
		n += len(q)
	}
	return n
}

// GetPendingGatewayApproval copies the oldest unresolved approval for reconnecting clients
func GetPendingGatewayApproval(sessionKey string) map[string]any {
	if sessionKey == "" {
		return nil
	}
	approvalState.Lock()
	defer approvalState.Unlock()
	queue := approvalState.gatewayQueues[sessionKey]
	if len(queue) == 0 {
		return nil
	}
	return copyMap(queue[0].Data)
}

// Shallow copy of a payload map
func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// SubmitPending stores a pending approval request for a session
func SubmitPending(sessionKey string, approval PendingApproval) {
	approvalState.Lock()
	defer approvalState.Unlock()
	approvalState.pending[sessionKey] = approval
}

// PendingApprovalFor returns the stored pending approval of a session
func PendingApprovalFor(sessionKey string) (PendingApproval, bool) {
	approvalState.Lock()
	defer approvalState.Unlock()
	p, ok := approvalState.pending[sessionKey]
	return p, ok
}

// ClearPendingApproval drops the stored pending approval of a session
func ClearPendingApproval(sessionKey string) {
	approvalState.Lock()
	defer approvalState.Unlock()
	delete(approvalState.pending, sessionKey)
}

// ApproveSession approves a pattern for this session only
func ApproveSession(sessionKey, patternKey string) {
	approvalState.Lock()
	defer approvalState.Unlock()
	set := approvalState.sessionApproved[sessionKey]
	if set == nil {
		set = map[string]bool{}
		approvalState.sessionApproved[sessionKey] = set
	}
	set[patternKey] = true
}

// RegisterPermissionModeReleaser adds a hook run on both yolo edges and at session clear
func RegisterPermissionModeReleaser(fn func(sessionKey string)) {
	approvalState.Lock()
	defer approvalState.Unlock()
	approvalState.permissionReleasers = append(approvalState.permissionReleasers, fn)
}

// RegisterSessionCloser adds a hook run when a session's approval state is cleared
func RegisterSessionCloser(fn func(sessionKey string)) {
	approvalState.Lock()
	defer approvalState.Unlock()
	approvalState.sessionClosers = append(approvalState.sessionClosers, fn)
}

// Drops resources whose immutable permission mode derives from yolo
func releasePermissionModeDependents(sessionKey string) {
	approvalState.Lock()
	hooks := append([]func(string){}, approvalState.permissionReleasers...)
	approvalState.Unlock()
	for _, fn := range hooks {
		fn(sessionKey)
	}
}

// Flips the session yolo flag and releases mode dependent resources
func setSessionYolo(sessionKey string, enabled bool) {
	if sessionKey == "" {
		return
	}
	approvalState.Lock()
	if enabled {
		approvalState.sessionYolo[sessionKey] = true
	} else {
		delete(approvalState.sessionYolo, sessionKey)
	}
	approvalState.Unlock()
	releasePermissionModeDependents(sessionKey)
}

// EnableSessionYolo enables the approval bypass for one session key
func EnableSessionYolo(sessionKey string) { setSessionYolo(sessionKey, true) }

// DisableSessionYolo disables the approval bypass for one session key
func DisableSessionYolo(sessionKey string) { setSessionYolo(sessionKey, false) }

// MarkLaunchYoloApplied records the launch yolo seed once per session, false when already seeded
func MarkLaunchYoloApplied(sessionKey string) bool {
	approvalState.Lock()
	defer approvalState.Unlock()
	if approvalState.launchYoloApplied[sessionKey] {
		return false
	}
	approvalState.launchYoloApplied[sessionKey] = true
	return true
}

// ClearSession removes every approval, yolo, pending, and gateway state of a session
func ClearSession(sessionKey string) {
	if sessionKey == "" {
		return
	}
	approvalState.Lock()
	delete(approvalState.sessionApproved, sessionKey)
	delete(approvalState.sessionYolo, sessionKey)
	delete(approvalState.launchYoloApplied, sessionKey)
	delete(approvalState.pending, sessionKey)
	for _, entry := range approvalState.gatewayQueues[sessionKey] {
		entry.Cancelled = "the session ended before the prompt was answered"
		entry.set()
	}
	delete(approvalState.gatewayQueues, sessionKey)
	closers := append([]func(string){}, approvalState.sessionClosers...)
	approvalState.Unlock()
	releasePermissionModeDependents(sessionKey)
	for _, fn := range closers {
		fn(sessionKey)
	}
}

// IsSessionYoloEnabled reports the bypass flag of one session key
func IsSessionYoloEnabled(sessionKey string) bool {
	if sessionKey == "" {
		return false
	}
	approvalState.Lock()
	defer approvalState.Unlock()
	return approvalState.sessionYolo[sessionKey]
}

// IsCurrentSessionYoloEnabled reports the bypass flag of the active approval session
func IsCurrentSessionYoloEnabled(env *Env) bool {
	return IsSessionYoloEnabled(getCurrentSessionKey(env, ""))
}

// Process yolo, session yolo, or an unattended one shot turn
func yoloActive(env *Env) bool {
	if yoloModeFrozen || IsCurrentSessionYoloEnabled(env) {
		return true
	}
	return sessionFlag(env, "unattended_turn", "ICEBATH_UNATTENDED_TURN")
}

// Profile key of the permanent allowlist governing this environment
func permanentHomeKey(env *Env) string {
	if env == nil {
		return ""
	}
	return env.Home
}

// Permanent allowlist of the active profile synced with the file, caller holds the lock
func permanentSet(env *Env) map[string]bool {
	key := permanentHomeKey(env)
	approved := approvalState.permanentByHome[key]
	if approved == nil {
		approved = map[string]bool{}
		approvalState.permanentByHome[key] = approved
	}
	syncPermanentFromDisk(env, approved, key)
	return approved
}

// Applies the on disk delta of command_allowlist since the last sync, caller holds the lock
func syncPermanentFromDisk(env *Env, approved map[string]bool, key string) {
	onDisk, err := readPermanentAllowlist(env)
	if err != nil {
		envLog(env).Warn("Failed to load permanent allowlist", "error", err)
		return
	}
	baseline := approvalState.baselineByHome[key]
	if setsEqual(onDisk, baseline) {
		return
	}
	for p := range baseline {
		if !onDisk[p] {
			delete(approved, p)
		}
	}
	for p := range onDisk {
		if !baseline[p] {
			approved[p] = true
		}
	}
	approvalState.baselineByHome[key] = onDisk
}

// Reports whether two string sets hold the same members
func setsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// IsApproved reports a session scoped or permanent approval, accepting legacy key aliases
func IsApproved(env *Env, sessionKey, patternKey string) bool {
	aliases := ApprovalKeyAliases(patternKey)
	approvalState.Lock()
	defer approvalState.Unlock()
	permanent := permanentSet(env)
	session := approvalState.sessionApproved[sessionKey]
	for alias := range aliases {
		if permanent[alias] || session[alias] {
			return true
		}
	}
	return false
}

// Permanent approval only, with alias compatibility
func isPermanentlyApproved(env *Env, patternKey string) bool {
	aliases := ApprovalKeyAliases(patternKey)
	approvalState.Lock()
	defer approvalState.Unlock()
	permanent := permanentSet(env)
	for alias := range aliases {
		if permanent[alias] {
			return true
		}
	}
	return false
}

// ApprovePermanent adds a pattern to the permanent allowlist in memory
func ApprovePermanent(env *Env, patternKey string) {
	approvalState.Lock()
	defer approvalState.Unlock()
	permanentSet(env)[patternKey] = true
}

// LoadPermanent replaces the permanent allowlist entries in memory
func LoadPermanent(env *Env, patterns map[string]bool) {
	approvalState.Lock()
	defer approvalState.Unlock()
	governing := permanentSet(env)
	for k := range governing {
		delete(governing, k)
	}
	for k := range patterns {
		governing[k] = true
	}
}

// Persists a session or always answer for each key, once persists nothing
func persistChoice(env *Env, sessionKey, choice string, keys []string) {
	for _, key := range keys {
		if choice != "session" && choice != "always" {
			continue
		}
		ApproveSession(sessionKey, key)
		if choice == "always" {
			ApprovePermanent(env, key)
			approvalState.Lock()
			grants := map[string]bool{}
			baseline := approvalState.baselineByHome[permanentHomeKey(env)]
			for p := range permanentSet(env) {
				if !baseline[p] {
					grants[p] = true
				}
			}
			approvalState.Unlock()
			SavePermanentAllowlist(env, grants)
		}
	}
}

// Secret lookup used when expanding the profile config
func configLookup(env *Env) config.Lookup {
	if env != nil && env.Secrets != nil {
		return env.Secrets.Lookup()
	}
	return func(string) (string, bool) { return "", false }
}

// Effective config document of the environment's home
func loadHomeConfig(env *Env) (config.Tree, error) {
	if env == nil || env.Home == "" {
		return config.Tree{}, nil
	}
	return config.Load(env.Home, configLookup(env))
}

// command_allowlist of the active profile as a set, empty on malformed input
func readPermanentAllowlist(env *Env) (map[string]bool, error) {
	doc, err := loadHomeConfig(env)
	if err != nil {
		return nil, err
	}
	raw, _ := doc.Lookup("command_allowlist")
	legacy := false
	if s, ok := raw.(string); ok {
		legacy = true
		var parsed any
		if yaml.Unmarshal([]byte(s), &parsed) != nil {
			raw = false
		} else {
			raw = parsed
		}
	}
	if raw == nil && !legacy {
		raw = []any{}
	}
	items, ok := raw.([]any)
	if !ok {
		envLog(env).Warn("Ignoring malformed command_allowlist; configure a list of strings.")
		return map[string]bool{}, nil
	}
	out := map[string]bool{}
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			envLog(env).Warn("Ignoring malformed command_allowlist; configure a list of strings.")
			return map[string]bool{}, nil
		}
		out[s] = true
	}
	if legacy {
		envLog(env).Warn("Recovered legacy string command_allowlist; re-save it as a list of strings.")
	}
	return out, nil
}

// LoadPermanentAllowlist syncs command_allowlist from config into the approval state
func LoadPermanentAllowlist(env *Env) map[string]bool {
	patterns, err := readPermanentAllowlist(env)
	if err != nil {
		envLog(env).Warn("Failed to load permanent allowlist", "error", err)
		return map[string]bool{}
	}
	approvalState.Lock()
	defer approvalState.Unlock()
	key := permanentHomeKey(env)
	approvalState.baselineByHome[key] = copySet(patterns)
	governing := approvalState.permanentByHome[key]
	if governing == nil {
		governing = map[string]bool{}
		approvalState.permanentByHome[key] = governing
	}
	for k := range governing {
		delete(governing, k)
	}
	for k := range patterns {
		governing[k] = true
	}
	return patterns
}

// Copy of a string set
func copySet(s map[string]bool) map[string]bool {
	out := make(map[string]bool, len(s))
	for k := range s {
		out[k] = true
	}
	return out
}

// SavePermanentAllowlist writes the on disk list plus this process's grants back to config
func SavePermanentAllowlist(env *Env, patterns map[string]bool) {
	if env == nil || env.Home == "" {
		return
	}
	doc, err := loadHomeConfig(env)
	if err != nil {
		envLog(env).Warn("Could not save allowlist", "error", err)
		return
	}
	onDisk := map[string]bool{}
	for _, s := range config.AsStrings(doc.Get("command_allowlist")) {
		onDisk[s] = true
	}
	approvalState.Lock()
	defer approvalState.Unlock()
	key := permanentHomeKey(env)
	baseline := approvalState.baselineByHome[key]
	merged := copySet(onDisk)
	for p := range patterns {
		if !baseline[p] {
			merged[p] = true
		}
	}
	sorted := make([]string, 0, len(merged))
	for p := range merged {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	if err := doc.Set("command_allowlist", sorted); err != nil {
		envLog(env).Warn("Could not save allowlist", "error", err)
		return
	}
	if err := config.Save(env.Home, doc); err != nil {
		envLog(env).Warn("Could not save allowlist", "error", err)
		return
	}
	approvalState.baselineByHome[key] = copySet(merged)
	governing := approvalState.permanentByHome[key]
	if governing == nil {
		governing = map[string]bool{}
		approvalState.permanentByHome[key] = governing
	}
	for k := range governing {
		delete(governing, k)
	}
	for k := range merged {
		governing[k] = true
	}
}

// IsApprovalBypassActiveForSession is the three source bypass check without floors
func IsApprovalBypassActiveForSession(env *Env, sessionKey string) bool {
	return yoloModeFrozen || IsSessionYoloEnabled(sessionKey) || getApprovalMode(env) == "off"
}

// IsApprovalBypassActive reports whether the current approval context bypasses prompts
func IsApprovalBypassActive(env *Env) bool {
	return IsApprovalBypassActiveForSession(env, getCurrentSessionKey(env, ""))
}

// Approved verdict with no message
func approvedResult() GuardResult { return GuardResult{Approved: true} }

var userSummaryOutcomes = map[string]bool{"denied": true, "timeout": true, "notify_failed": true, "cancelled": true, "blocked": true}

var userSummaryNouns = map[string]string{"command": "command", "code": "code", "action": "action"}

// One plain sentence for the person who answered or did not
func userSummary(env *Env, outcome, noun string) string {
	window := formatApprovalWindow(getApprovalTimeout(env))
	nounText := noun
	if text, ok := userSummaryNouns[noun]; ok {
		nounText = text
	}
	if !userSummaryOutcomes[outcome] {
		outcome = "default"
	}
	switch outcome {
	case "denied":
		return fmt.Sprintf("You denied this %s — it did not run.", nounText)
	case "timeout":
		return fmt.Sprintf("No answer within %s — the %s did not run.", window, nounText)
	case "notify_failed":
		return fmt.Sprintf("The approval request could not be delivered — the %s did not run.", nounText)
	case "cancelled":
		return fmt.Sprintf("The approval prompt was withdrawn or never reached you — the %s did not run.", nounText)
	case "blocked":
		return fmt.Sprintf("This %s is not allowed in an unattended session — it did not run.", nounText)
	}
	return fmt.Sprintf("This %s did not run.", nounText)
}

// Standard non consent verdict the agent must not retry or rephrase
func deniedResult(env *Env, message, patternKey, description, outcome, noun string) GuardResult {
	return GuardResult{Message: message, PatternKey: patternKey, Description: description, Outcome: outcome,
		UserSummary: userSummary(env, outcome, noun)}
}

// Non interactive block without consent keys
func blockedResult(message, patternKey, description string) GuardResult {
	return GuardResult{Message: message, PatternKey: patternKey, Description: description}
}

// Human approval that resets the consecutive denial tally
func userApprovedResult(sessionKey, description string) GuardResult {
	resetDenials(sessionKey)
	return GuardResult{Approved: true, UserApproved: true, Description: description}
}

// Registered gateway notifier of a session or nil
func gatewayNotifyCb(sessionKey string) GatewayNotifyFunc {
	approvalState.Lock()
	defer approvalState.Unlock()
	return approvalState.gatewayNotify[sessionKey]
}

// Queues an approval nobody can answer now and returns the pending shape of the gate
func pendingResult(spec gateSpec, sessionKey, command, description, patternKey string, patternKeys []string, body string, smartDenied bool) GuardResult {
	pending := PendingApproval{Command: command, PatternKey: patternKey, Description: description, AllowPermanent: true}
	if spec.pendingKeys {
		pending.PatternKeys = append([]string{}, patternKeys...)
	}
	if smartDenied {
		pending.SmartDenied = true
		pending.AllowPermanent = false
	}
	SubmitPending(sessionKey, pending)
	if !spec.pendingKeys {
		return GuardResult{PatternKey: patternKey, Status: "approval_required", Command: command, Description: description,
			Message:        fmt.Sprintf("⚠️ This action is potentially dangerous (%s). Asking the user for approval.\n\n**Target:**\n```\n%s\n```", description, command),
			AllowPermanent: true}
	}
	if body == "" {
		body = fmt.Sprintf("**Command:**\n```\n%s\n```", command)
	}
	result := GuardResult{PatternKey: patternKey, Status: "pending_approval", ApprovalPending: true, Command: command, Description: description,
		Message:        fmt.Sprintf("⚠️ %s. Asking the user for approval.\n\n%s\n\nSTOP: do NOT re-run, rephrase, or re-issue this %s — each variant sends the user ANOTHER approval card. Wait for the user's decision; if this turn must end, report that approval is pending.", description, body, spec.noun),
		AllowPermanent: !smartDenied, SmartDenied: smartDenied}
	return result
}

// One non interactive context and the text every gate uses to explain it
type unattendedContext struct {
	name   string
	cfgKey string
	clause string
	scope  string
	trust  string
}

// approve or deny from approvals.<cfgKey>
func (u unattendedContext) mode(env *Env) string {
	switch u.name {
	case "single_query":
		return getSingleQueryApprovalMode(env)
	case "cron":
		return getCronApprovalMode(env)
	}
	return getUnattendedApprovalMode(env)
}

// BLOCKED sentence naming the context and the config key that would allow it
func (u unattendedContext) blockMessage(subject, noun, advice string) string {
	return fmt.Sprintf("BLOCKED: %s but %s. %s To allow %s %s, set approvals.%s: approve in config.yaml.", subject, u.clause, advice, noun, u.scope, u.cfgKey)
}

// Tail of the execute_code block message for this context
func (u unattendedContext) execTail() string {
	return fmt.Sprintf("%s%s. Use normal tools instead, or set approvals.%s: approve only if %s.", strings.ToUpper(u.clause[:1]), u.clause[1:], u.cfgKey, u.trust)
}

var singleQueryCtx = unattendedContext{"single_query", "single_query_mode",
	"single-query mode (-q) runs without a user present to approve it", "in single-query mode",
	"this single-query run is intentionally trusted"}

var cronCtx = unattendedContext{"cron", "cron_mode", "cron jobs run without a user present to approve it",
	"in cron jobs", "this cron profile is intentionally trusted"}

// Active unattended contexts in evaluation order, single query first, cron beats a platform marker
func unattendedContexts(env *Env) []unattendedContext {
	var contexts []unattendedContext
	if isSingleQueryApprovalContext(env) {
		contexts = append(contexts, singleQueryCtx)
	}
	if isCronApprovalContext(env) {
		contexts = append(contexts, cronCtx)
	} else if isUnattendedPlatformApprovalContext(env) {
		contexts = append(contexts, unattendedContext{"unattended", "unattended_mode",
			fmt.Sprintf("this session runs on an unattended platform (%s) with no user present to approve it", getSessionPlatform(env)),
			"on unattended platforms", "sessions on this surface are intentionally trusted"})
	}
	return contexts
}

// Pattern keys still needing consent with their joined description, ok false when nothing pends
func unapprovedFindings(command string, approved func(key string) bool) ([]string, string, bool) {
	patternKey, description, dangerous := DetectDangerousCommand(command)
	if !dangerous {
		return nil, "", false
	}
	type finding struct{ key, text string }
	findings := []finding{{patternKey, description}}
	for _, key := range DetectContentRuleKeys(command) {
		if key != patternKey {
			findings = append(findings, finding{key, key})
		}
	}
	var keys, texts []string
	for _, f := range findings {
		if !approved(f.key) {
			keys = append(keys, f.key)
			texts = append(texts, f.text)
		}
	}
	if len(keys) == 0 {
		return nil, "", false
	}
	return keys, strings.Join(texts, "; "), true
}

// Deny mode handling for one unattended context, ok false means allow
func unattendedDeny(env *Env, command string, ctx unattendedContext) (GuardResult, bool) {
	if ctx.mode(env) != "deny" {
		return GuardResult{}, false
	}
	keys, description, pending := unapprovedFindings(command, func(key string) bool { return isPermanentlyApproved(env, key) })
	if !pending {
		return GuardResult{}, false
	}
	result := GuardResult{Message: ctx.blockMessage(fmt.Sprintf("Command flagged as dangerous (%s)", description),
		"dangerous commands", "Find an alternative approach that avoids this command.")}
	if ctx.name == "single_query" {
		result.PatternKey = keys[0]
		result.Description = description
	}
	return result, true
}

// Wording and policy knobs of one human decision gate flavor
type gateSpec struct {
	noun            string
	transport       bool
	userApproved    bool
	redactCLI       bool
	pendingKeys     bool
	notifyFailed    string
	gatewayRefused  string
	transportDenied string
	cliTimeout      string
	cliDenied       string
	smartLog        string
}

const stopCommand = " The user has NOT consented to this action. Do NOT retry this command, do NOT rephrase it, and do NOT attempt the same outcome via a different command. Stop the current workflow and wait for the user to respond before taking any further destructive or irreversible action."

const stopAction = " The user has NOT consented to this action. Do NOT retry it, do NOT rephrase it, and do NOT attempt the same outcome via a different path."

var commandGate = gateSpec{noun: "command", transport: true, userApproved: true, pendingKeys: true,
	notifyFailed:    "BLOCKED: Failed to send approval request to user. Do NOT retry.",
	gatewayRefused:  "BLOCKED: Command {reason}.{reason_addendum}" + stopCommand + "{timeout_addendum}{breaker}",
	transportDenied: "BLOCKED: User denied this command through the selected approval transport. The user has NOT consented to this action. Do NOT retry or attempt the same outcome through another route.{breaker}",
	cliTimeout:      "BLOCKED: Command timed out without user response." + stopCommand + " Silence is not consent.{breaker}",
	cliDenied:       "BLOCKED: User denied this command." + stopCommand + "{breaker}",
	smartLog:        "Smart approval: auto-approved '{command}' ({description})"}

var executeCodeGate = gateSpec{noun: "code", transport: true, userApproved: true, redactCLI: true, pendingKeys: true,
	notifyFailed:    "BLOCKED: Failed to send execute_code approval request to user. Do NOT retry.",
	gatewayRefused:  "BLOCKED: execute_code script {reason}.{reason_addendum} The user has NOT consented to running this code. Do NOT retry, do NOT rephrase the script, and do NOT attempt the same outcome via a different tool.{timeout_addendum}{breaker}",
	transportDenied: "BLOCKED: User denied execute_code through the selected approval transport. The user has NOT consented.",
	cliTimeout:      "BLOCKED: Action timed out without user response." + stopAction + " Silence is not consent.{breaker}",
	cliDenied:       "BLOCKED: User denied execute_code script execution (matched '{description}'). Do NOT retry — the user has explicitly rejected it.{breaker}",
	smartLog:        "Smart approval: auto-approved execute_code for session {session_key}"}

var actionGate = gateSpec{noun: "action",
	notifyFailed:   "BLOCKED: Failed to send approval request to user. Do NOT retry.",
	gatewayRefused: "BLOCKED: Action {reason}.{reason_addendum}" + stopAction + "{timeout_addendum}",
	cliTimeout:     "BLOCKED: Action timed out without user response." + stopAction + " Silence is not consent.",
	cliDenied:      "BLOCKED: User denied this potentially dangerous action (matched '{description}'). Do NOT retry — the user has explicitly rejected it."}

// Fills the brace placeholders of a gate template
func formatTemplate(template string, fields map[string]string) string {
	for k, v := range fields {
		template = strings.ReplaceAll(template, "{"+k+"}", v)
	}
	return template
}

// Guardian LLM step, a result ends the gate, the bool allows an interactive owner override of a deny
func smartGate(ctx context.Context, env *Env, spec gateSpec, command, description, patternKey string, patternKeys []string, sessionKey string, humanPresent bool) (GuardResult, bool, bool) {
	verdict := smartVerdict(ctx, env, command, description, patternKey, patternKeys, sessionKey)
	if verdict == "approve" {
		resetDenials(sessionKey)
		envLog(env).Debug(formatTemplate(spec.smartLog, map[string]string{"command": truncateRunes(command, 60), "description": description, "session_key": sessionKey}))
		return GuardResult{Approved: true, SmartApproved: true, Description: description}, true, false
	}
	if verdict != "deny" {
		return GuardResult{}, false, false
	}
	recordDenial(sessionKey)
	if humanPresent {
		return GuardResult{}, false, true
	}
	return GuardResult{SmartDenied: true,
		Message: fmt.Sprintf("BLOCKED by smart approval: %s. The command was assessed as genuinely dangerous. Do NOT retry.%s", description, denialBreakerAddendum(env, sessionKey))}, true, true
}

// Inputs of the shared human decision engine
type humanDecisionInput struct {
	command, description, patternKey string
	patternKeys                      []string
	sessionKey                       string
	isCLI, isGateway, isAsk          bool
	smart                            bool
	pendingBody                      func() string
}

// Asks a human after the optional guardian step and turns the answer into the gate result
func humanDecision(ctx context.Context, env *Env, spec gateSpec, in humanDecisionInput) GuardResult {
	smartDenied := false
	if in.smart {
		result, done, denied := smartGate(ctx, env, spec, in.command, in.description, in.patternKey, in.patternKeys, in.sessionKey, in.isCLI || in.isGateway || in.isAsk)
		if done {
			return result
		}
		smartDenied = denied
	}
	pendingBody := ""
	if in.pendingBody != nil {
		pendingBody = in.pendingBody()
	}
	allowPermanent := !smartDenied

	deny := func(template, outcome string, fields map[string]string, denyReason string) GuardResult {
		breaker := ""
		if strings.Contains(template, "{breaker}") {
			breaker = denialBreakerAddendum(env, in.sessionKey)
		}
		all := map[string]string{"description": in.description, "breaker": breaker}
		for k, v := range fields {
			all[k] = v
		}
		result := deniedResult(env, formatTemplate(template, all), in.patternKey, in.description, outcome, spec.noun)
		result.DenyReason = denyReason
		return result
	}
	grant := func(choice string) GuardResult {
		if !smartDenied {
			persistChoice(env, in.sessionKey, choice, in.patternKeys)
		}
		if spec.userApproved {
			return userApprovedResult(in.sessionKey, in.description)
		}
		return approvedResult()
	}

	if spec.transport {
		surface := "cli"
		if in.isGateway || in.isAsk {
			surface = gatewayApprovalSurface(env)
		}
		attempt := presentWithSelectedTransport(ctx, env, transportRequest{command: in.command, description: in.description,
			patternKey: in.patternKey, patternKeys: in.patternKeys, sessionKey: in.sessionKey, surface: surface,
			allowSession: !smartDenied, allowPermanent: allowPermanent})
		choice, denied, hasDenied := transportChoice(env, attempt, in.patternKey, in.description)
		if hasDenied {
			return denied
		}
		if choice != "" {
			if choice == "deny" {
				recordDenial(in.sessionKey)
				return deny(spec.transportDenied, "denied", nil, "")
			}
			return grant(choice)
		}
	}

	if in.isGateway || in.isAsk {
		displayCommand := redactApprovalDisplay(env, in.command, false)
		displayDescription := redactApprovalDisplay(env, in.description, false)
		notify := gatewayNotifyCb(in.sessionKey)
		if notify != nil {
			data := map[string]any{"command": displayCommand, "pattern_key": in.patternKey,
				"pattern_keys": append([]string{}, in.patternKeys...), "description": displayDescription,
				"allow_permanent": !smartDenied, "allow_session": !smartDenied}
			if smartDenied {
				data["smart_denied"] = true
			}
			decision := awaitGatewayDecision(ctx, env, in.sessionKey, notify, data, "gateway")
			if decision.NotifyFailed {
				return deniedResult(env, spec.notifyFailed, in.patternKey, in.description, "notify_failed", spec.noun)
			}
			if decision.Cancelled != "" {
				return deny(spec.gatewayRefused, "cancelled", map[string]string{
					"reason":           fmt.Sprintf("approval was withdrawn before the user answered (%s)", decision.Cancelled),
					"reason_addendum":  "",
					"timeout_addendum": ""}, "")
			}
			if !decision.Resolved {
				return deny(spec.gatewayRefused, "timeout", map[string]string{"reason": "timed out without user response",
					"reason_addendum": "", "timeout_addendum": " Silence is not consent."}, decision.Reason)
			}
			if decision.Choice == "" || decision.Choice == "deny" {
				addendum := ""
				if decision.Reason != "" {
					addendum = fmt.Sprintf(" Reason given by the user: \"%s\".", decision.Reason)
				}
				return deny(spec.gatewayRefused, "denied", map[string]string{"reason": "denied by user",
					"reason_addendum": addendum, "timeout_addendum": ""}, decision.Reason)
			}
			return grant(decision.Choice)
		}
		if !shouldFallThroughToCLIApproval(in.isCLI, env != nil && env.Ask != nil, false) {
			if !spec.pendingKeys {
				displayCommand, displayDescription = in.command, in.description
			}
			return pendingResult(spec, in.sessionKey, displayCommand, displayDescription, in.patternKey, in.patternKeys, pendingBody, smartDenied)
		}
	}

	promptCommand, promptDescription := in.command, in.description
	if spec.redactCLI {
		promptCommand = redactApprovalDisplay(env, in.command, false)
		promptDescription = redactApprovalDisplay(env, in.description, false)
	}
	hookPayload := map[string]any{"command": promptCommand, "description": promptDescription, "pattern_key": in.patternKey,
		"pattern_keys": append([]string{}, in.patternKeys...), "session_key": in.sessionKey, "surface": "cli"}
	fireApprovalHook(env, "pre_approval_request", hookPayload)
	choice := promptDangerousApproval(ctx, env, promptOptions{command: promptCommand, description: promptDescription,
		allowPermanent: allowPermanent, allowSession: true, smartDenied: smartDenied})
	post := copyMap(hookPayload)
	post["choice"] = choice.value
	fireApprovalHook(env, "post_approval_response", post)
	switch choice.value {
	case "timeout":
		return deny(spec.cliTimeout, "timeout", nil, "")
	case "cancelled":
		cause := choice.cause
		if cause == "" {
			cause = "no answer"
		}
		return deny(spec.gatewayRefused, "cancelled", map[string]string{
			"reason":           fmt.Sprintf("was not approved: the approval prompt could not be delivered or was not answered (%s)", cause),
			"reason_addendum":  "",
			"timeout_addendum": " Silence is not consent."}, "")
	case "deny":
		return deny(spec.cliDenied, "denied", nil, "")
	}
	return grant(choice.value)
}

// Presence trio of the current context, cleared for single query and cron, ask only with an answering client
func presence(env *Env) (isCLI, isGateway, isAsk bool) {
	isCLI, isGateway = isInteractiveCLI(env), isGatewayApprovalContext(env)
	isAsk = sessionFlag(env, "exec_ask", "ICEBATH_EXEC_ASK")
	if isSingleQueryApprovalContext(env) || isCronApprovalContext(env) {
		return false, false, false
	}
	if isAsk && isUnattendedPlatformApprovalContext(env) &&
		(gatewayNotifyCb(getCurrentSessionKey(env, "default")) == nil || notifierBlindApprovalPlatforms[getSessionPlatform(env)]) {
		isAsk = false
	}
	return isCLI, isGateway, isAsk
}

// Options of the shared approval gate for a flagged action
type approvalGateOptions struct {
	patternKey, description, displayTarget string
	subject, noun, advice                  string
	cronDenyMessage                        string
	singleQueryDenyMessage                 string
	unattendedDenyMessage                  string
	autoapproveLogPrefix                   string
	failClosedWhenNoHuman                  bool
	noHumanBlockMessage                    string
	patternKeys                            []string
}

// Shared human approval gate for a flagged tool call or write, floors are the caller's job
func runApprovalGate(ctx context.Context, env *Env, o approvalGateOptions) GuardResult {
	if o.noun == "" {
		o.noun = "flagged actions"
	}
	if o.advice == "" {
		o.advice = "Find an alternative approach that avoids this action."
	}
	if yoloActive(env) || getApprovalMode(env) == "off" {
		return approvedResult()
	}
	sessionKey := getCurrentSessionKey(env, "default")
	if IsApproved(env, sessionKey, o.patternKey) {
		return approvedResult()
	}
	isCLI, isGateway, isAsk := presence(env)
	if !isCLI && !isGateway && !isAsk {
		log := envLog(env)
		denyMessages := map[string]string{"single_query": o.singleQueryDenyMessage, "cron": o.cronDenyMessage, "unattended": o.unattendedDenyMessage}
		contexts := unattendedContexts(env)
		handled := false
		for _, c := range contexts {
			if c.mode(env) == "deny" {
				message := denyMessages[c.name]
				if message == "" && c.name == "unattended" {
					message = c.blockMessage(fmt.Sprintf("approval required (%s)", o.description), "flagged actions", "Find an alternative approach that avoids this action.")
				} else if message == "" {
					message = c.blockMessage(o.subject, o.noun, o.advice)
				}
				return blockedResult(message, o.patternKey, o.description)
			}
			if c.name == "single_query" {
				log.Warn(fmt.Sprintf("%s (pattern: %s): %s — single-query auto-approve (approvals.single_query_mode: approve).", o.autoapproveLogPrefix, o.patternKey, o.description))
				return approvedResult()
			}
			handled = true
			break
		}
		if !handled && o.failClosedWhenNoHuman {
			log.Warn(fmt.Sprintf("%s (pattern: %s): %s — no interactive user/gateway present; BLOCKED (fail-closed). Set ICEBATH_INTERACTIVE or ICEBATH_GATEWAY_SESSION to answer the prompt.", o.autoapproveLogPrefix, o.patternKey, o.description))
			message := o.noHumanBlockMessage
			if message == "" {
				message = fmt.Sprintf("BLOCKED: approval required (%s) but no interactive user or gateway is present to approve it.", o.description)
			}
			return blockedResult(message, o.patternKey, o.description)
		}
		log.Warn(fmt.Sprintf("%s (pattern: %s): %s — set ICEBATH_INTERACTIVE or ICEBATH_GATEWAY_SESSION to require approval.", o.autoapproveLogPrefix, o.patternKey, o.description))
		return approvedResult()
	}
	keys := o.patternKeys
	if len(keys) == 0 {
		keys = []string{o.patternKey}
	}
	return humanDecision(ctx, env, actionGate, humanDecisionInput{command: o.displayTarget, description: o.description,
		patternKey: o.patternKey, patternKeys: keys, sessionKey: sessionKey, isCLI: isCLI, isGateway: isGateway, isAsk: isAsk})
}

// True when the backend is isolated enough to skip dangerous command prompts
func shouldSkipContainerGuards(envType string, hasHostAccess bool) bool {
	switch envType {
	case "docker":
		return !hasHostAccess
	case "modal", "daytona", "vercel_sandbox":
		return true
	}
	return providerFlagBool(envType, func(p BackendProvider) bool { return p.SkipContainerGuards() })
}

// Operator deny rules are never bypassable, evaluated before every fast path
func userDenyBlock(env *Env, command string) (GuardResult, bool) {
	denyPattern := matchUserDenyRule(env, command)
	if denyPattern == "" {
		return GuardResult{}, false
	}
	envLog(env).Warn("User deny rule blocked command", "rule", denyPattern, "command", truncateRunes(command, 200))
	return userDenyBlockResult(denyPattern), true
}

// Unconditional floors run before yolo, mode off, and cron approve mode
func floorBlock(env *Env, command string, sudoGuard bool) (GuardResult, bool) {
	if ok, desc := DetectHardlineCommand(command); ok {
		envLog(env).Warn("Hardline block", "description", desc, "command", truncateRunes(command, 200))
		return hardlineBlockResult(env, desc, command), true
	}
	if target := commandDeletesRuntime(command); target != "" {
		envLog(env).Warn("Runtime self-delete block", "target", target, "command", truncateRunes(command, 200))
		return hardlineBlockResult(env, "recursive/any delete of "+target, command), true
	}
	if sudoGuard {
		if ok, desc := CheckSudoStdinGuard(env, command); ok {
			envLog(env).Warn("Sudo stdin guard block", "description", desc, "command", truncateRunes(command, 200))
			return sudoStdinBlockResult(desc), true
		}
	}
	return userDenyBlock(env, command)
}

var runtimeSegmentSplitRe = regexp.MustCompile(`\n|&&|\|\||[;|]`)

var runtimeCommandPrefixes = map[string]bool{"sudo": true, "command": true, "nohup": true, "exec": true, "env": true, "nice": true, "time": true}

var runtimeDeletingCommands = map[string]bool{"rm": true, "rmdir": true, "rd": true, "del": true, "erase": true, "remove-item": true, "ri": true}

var runtimeFindDeleteRe = regexp.MustCompile(`(?i)(?:^|\s)-(?:delete|exec\s+rm\b)`)

var runtimeEnvAssignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// Words of a segment, shlex first and whitespace when quoting is unbalanced
func runtimeSplitWords(segment string) []string {
	words, err := shlexSplit(segment)
	if err != nil {
		return strings.Fields(segment)
	}
	return words
}

// Drops sudo style prefixes and environment assignments from the front of a word list
func runtimeStripPrefixes(words []string) []string {
	for len(words) > 0 {
		first := words[0]
		if runtimeCommandPrefixes[first] {
			words = words[1:]
			continue
		}
		if strings.Contains(first, "=") && !strings.HasPrefix(first, "-") && !strings.HasPrefix(first, "/") && runtimeEnvAssignRe.MatchString(first) {
			words = words[1:]
			continue
		}
		break
	}
	return words
}

// Protected runtime entries the delete scan consults, swapped by tests that fake a runtime layout
var protectedRuntimeEntries = protectedRuntime

// uv managed install layout, root and version of a cpython directory
var uvInstallDirRe = regexp.MustCompile(`(?i)^(?P<root>.*[/\\](?:uv[/\\])?python[/\\]cpython-(?P<version>\d+(?:\.\d+)*)(?:[-_][^/\\]*)?)(?:[/\\].*)?$`)

const uvInstallDescriptionPrefix = "the uv-managed Python install"

// True when a and b are the same path or one is an ancestor of the other
func runtimePathsOverlap(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	sep := string(filepath.Separator)
	return a == b || strings.HasPrefix(a, b+sep) || strings.HasPrefix(b, a+sep)
}

// Description of the protected runtime entry a deleted path touches, empty when none
func runtimeDeleteHit(path string) string {
	resolved := realPath(expandTilde(path))
	if resolved == "" {
		return ""
	}
	for _, entry := range protectedRuntimeEntries() {
		if runtimePathsOverlap(resolved, entry[0]) {
			return entry[1]
		}
	}
	return ""
}

// Matches a uv python uninstall spec against the running base version and directory name
func versionSpecMatches(spec, version, uvRootName string) bool {
	spec = strings.ToLower(strings.TrimSpace(spec))
	if spec == "" {
		return false
	}
	if spec == "--all" || spec == "-a" || spec == "all" {
		return true
	}
	if strings.HasPrefix(spec, "-") {
		return false
	}
	if strings.HasPrefix(strings.ToLower(uvRootName), spec) {
		return true
	}
	return spec == version || (strings.HasPrefix(version, spec) && strings.HasPrefix(version[len(spec):], "."))
}

// Description when uv python uninstall would remove the running base install
func uvUninstallTarget(words []string) string {
	var uvEntries [][2]string
	for _, entry := range protectedRuntimeEntries() {
		if strings.HasPrefix(entry[1], uvInstallDescriptionPrefix) {
			uvEntries = append(uvEntries, entry)
		}
	}
	if len(uvEntries) == 0 {
		return ""
	}
	pythonAt, uninstallAt := -1, -1
	for i, w := range words {
		lower := strings.ToLower(w)
		if pythonAt < 0 {
			if lower == "python" {
				pythonAt = i
			}
			continue
		}
		if lower == "uninstall" {
			uninstallAt = i
			break
		}
	}
	if pythonAt < 0 || uninstallAt < 0 {
		return ""
	}
	specs := words[uninstallAt+1:]
	for _, entry := range uvEntries {
		root, description := entry[0], entry[1]
		version := ""
		if m := uvInstallDirRe.FindStringSubmatch(strings.ReplaceAll(root, "\\", "/")); m != nil {
			version = m[uvInstallDirRe.SubexpIndex("version")]
		}
		rootName := filepath.Base(root)
		for _, spec := range specs {
			if versionSpecMatches(spec, version, rootName) {
				return description
			}
		}
	}
	return ""
}

// Description of the runtime path the command would delete, empty when none
func commandDeletesRuntime(command string) string {
	if strings.TrimSpace(command) == "" || len(protectedRuntimeEntries()) == 0 {
		return ""
	}
	for _, segment := range runtimeSegmentSplitRe.Split(command, -1) {
		words := runtimeStripPrefixes(runtimeSplitWords(segment))
		if len(words) == 0 {
			continue
		}
		name := strings.ToLower(filepath.Base(words[0]))
		if name == "uv" {
			if target := uvUninstallTarget(words); target != "" {
				return target
			}
			continue
		}
		if name == "find" && runtimeFindDeleteRe.MatchString(segment) {
			for _, word := range words[1:] {
				if !strings.HasPrefix(word, "-") {
					if hit := runtimeDeleteHit(word); hit != "" {
						return hit
					}
				}
			}
			continue
		}
		if !runtimeDeletingCommands[name] {
			continue
		}
		for _, word := range words[1:] {
			if strings.HasPrefix(word, "-") {
				continue
			}
			if hit := runtimeDeleteHit(word); hit != "" {
				return hit
			}
		}
	}
	return ""
}

// CheckDangerousCommand runs the pattern layer only and resolves approval through the shared gate
func CheckDangerousCommand(ctx context.Context, env *Env, command, envType string, hasHostAccess bool) GuardResult {
	if shouldSkipContainerGuards(envType, hasHostAccess) {
		if blocked, ok := userDenyBlock(env, command); ok {
			return blocked
		}
		return approvedResult()
	}
	if blocked, ok := floorBlock(env, command, false); ok {
		return blocked
	}
	if yoloActive(env) {
		return approvedResult()
	}
	if commandMatchesPermanentAllowlist(env, command) {
		return approvedResult()
	}
	sessionKey := getCurrentSessionKey(env, "default")
	keys, description, pending := unapprovedFindings(command, func(key string) bool { return IsApproved(env, sessionKey, key) })
	if !pending {
		return approvedResult()
	}
	return runApprovalGate(ctx, env, approvalGateOptions{patternKey: keys[0], patternKeys: keys, description: description,
		displayTarget: command, subject: fmt.Sprintf("Command flagged as dangerous (%s)", description),
		noun: "dangerous commands", advice: "Find an alternative approach that avoids this command.",
		autoapproveLogPrefix: "AUTO-APPROVED dangerous command in non-interactive non-gateway context"})
}

// RequestToolApproval escalates an arbitrary tool call to the same human gate as dangerous commands
func RequestToolApproval(ctx context.Context, env *Env, toolName, reason, ruleKey string) GuardResult {
	description := reason
	if description == "" {
		description = "Plugin requires approval for " + toolName
	}
	if ruleKey == "" {
		sum := sha256.Sum256([]byte(description))
		ruleKey = toolName + ":" + hex.EncodeToString(sum[:])[:12]
	}
	subject := fmt.Sprintf("Tool '%s' requires approval (%s)", toolName, description)
	return runApprovalGate(ctx, env, approvalGateOptions{patternKey: "plugin_rule:" + ruleKey, description: description,
		displayTarget: fmt.Sprintf("<%s> (plugin approval rule)", toolName), subject: subject, advice: "Find an alternative approach.",
		autoapproveLogPrefix:  fmt.Sprintf("plugin-escalated tool call '%s' in non-interactive non-gateway context", toolName),
		failClosedWhenNoHuman: true,
		noHumanBlockMessage:   fmt.Sprintf("BLOCKED: %s but no interactive user or gateway is present to approve it. A plugin flagged this action for human confirmation.", subject)})
}

// PreparedApprovalSlot is the terminal batch seam that pre decides a guard and publishes its prompt
type PreparedApprovalSlot interface {
	Preparing() bool
	ConsumePreparedGuard(ctx context.Context, env *Env, command, envType string, hasHostAccess bool) (GuardResult, bool)
	RegisterPreparedApproval(sessionKey string, entry *ApprovalEntry)
	ApprovalPublished()
}

type preparedSlotKey struct{}

type approvalTaskKey struct{}

// WithApprovalTaskID binds the task running a guarded command to the context
func WithApprovalTaskID(ctx context.Context, taskID string) context.Context {
	return context.WithValue(ctx, approvalTaskKey{}, taskID)
}

// ApprovalTaskID returns the task bound by CheckCommandApproval, empty when none
func ApprovalTaskID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(approvalTaskKey{}).(string)
	return id
}

// WithPreparedApprovalSlot binds the batch slot of the running terminal call to the context
func WithPreparedApprovalSlot(ctx context.Context, slot PreparedApprovalSlot) context.Context {
	return context.WithValue(ctx, preparedSlotKey{}, slot)
}

// Batch slot bound to the context or nil
func preparedApprovalSlot(ctx context.Context) PreparedApprovalSlot {
	if ctx == nil {
		return nil
	}
	slot, _ := ctx.Value(preparedSlotKey{}).(PreparedApprovalSlot)
	return slot
}

// CheckAllCommandGuards runs every pre exec security check and returns one approval decision
func CheckAllCommandGuards(ctx context.Context, env *Env, command, envType string, hasHostAccess bool) GuardResult {
	if shouldSkipContainerGuards(envType, hasHostAccess) {
		if blocked, ok := userDenyBlock(env, command); ok {
			return blocked
		}
		return approvedResult()
	}
	if blocked, ok := floorBlock(env, command, true); ok {
		return blocked
	}
	if slot := preparedApprovalSlot(ctx); slot != nil {
		if prepared, ok := slot.ConsumePreparedGuard(ctx, env, command, envType, hasHostAccess); ok {
			return prepared
		}
	}
	approvalMode := getApprovalMode(env)
	if yoloActive(env) || approvalMode == "off" {
		return approvedResult()
	}
	if commandMatchesPermanentAllowlist(env, command) {
		return approvedResult()
	}
	isCLI, isGateway, isAsk := presence(env)
	if !isCLI && !isGateway && !isAsk {
		for _, c := range unattendedContexts(env) {
			if result, ok := unattendedDeny(env, command, c); ok {
				return result
			}
		}
		return approvedResult()
	}
	sessionKey := getCurrentSessionKey(env, "default")
	keys, description, pending := unapprovedFindings(command, func(key string) bool { return IsApproved(env, sessionKey, key) })
	if !pending {
		return approvedResult()
	}
	return humanDecision(ctx, env, commandGate, humanDecisionInput{command: command, description: description,
		patternKey: keys[0], patternKeys: keys, sessionKey: sessionKey, isCLI: isCLI, isGateway: isGateway, isAsk: isAsk,
		smart: approvalMode == "smart"})
}

const executeCodeDescription = "execute_code script execution. The script can spawn subprocesses or mutate files without passing through terminal command approval; approval is one-shot for this run."

// A local session's own viewer turn with an approver attached and a registered notifier
func isLocalViewerTurn(env *Env) bool {
	return isLocalInteractiveSession(env) && isViewerTurnWithApprover(env) &&
		!isDelegatedChildProcessContext(env) && gatewayNotifyCb(getCurrentSessionKey(env, "default")) != nil
}

// CheckExecuteCodeGuard approves an execute_code script before its child process is spawned
func CheckExecuteCodeGuard(ctx context.Context, env *Env, code, envType string, hasHostAccess bool) GuardResult {
	patternKey := "execute_code"
	description := executeCodeDescription
	if envType == "vercel_sandbox" || shouldSkipContainerGuards(envType, hasHostAccess) {
		return approvedResult()
	}
	approvalMode := getApprovalMode(env)
	if yoloActive(env) || approvalMode == "off" {
		return approvedResult()
	}
	isCLI, isGateway, isAsk := presence(env)
	for _, c := range unattendedContexts(env) {
		if c.mode(env) == "deny" {
			return deniedResult(env, "BLOCKED: execute_code runs arbitrary local Python (including subprocess calls that bypass shell-string approval checks). "+c.execTail(),
				patternKey, description, "blocked", "code")
		}
		return approvedResult()
	}
	if (!isGateway && !isAsk) || isLocalViewerTurn(env) {
		return approvedResult()
	}
	sessionKey := getCurrentSessionKey(env, "default")
	command := fmt.Sprintf("execute_code <<'PY'\n%s\nPY", code)
	if IsApproved(env, sessionKey, patternKey) {
		return approvedResult()
	}
	return humanDecision(ctx, env, executeCodeGate, humanDecisionInput{command: command, description: description,
		patternKey: patternKey, patternKeys: []string{patternKey}, sessionKey: sessionKey,
		isCLI: isCLI, isGateway: isGateway, isAsk: isAsk, smart: approvalMode == "smart",
		pendingBody: func() string {
			return fmt.Sprintf("**Code:**\n```python\n%s\n```", redactApprovalDisplay(env, code, false))
		}})
}

// True when a docker volume spec bind mounts a host path
func dockerVolumeUsesHostPath(spec string) bool {
	vol := strings.TrimSpace(spec)
	if vol == "" {
		return false
	}
	for _, p := range []string{"/", "~", "./", "../"} {
		if strings.HasPrefix(vol, p) {
			return true
		}
	}
	return len(vol) >= 3 && vol[1] == ':' && (vol[2] == '/' || vol[2] == '\\')
}

// True when a docker sandbox exposes host paths through bind mounts
func dockerHasHostAccess(env *Env, policy TerminalPolicy) bool {
	if policy.Get("TERMINAL_ENV", "local") != "docker" {
		return false
	}
	if env != nil && isHostCwd(env.Cwd) && policy.GetBool("TERMINAL_DOCKER_MOUNT_CWD_TO_WORKSPACE", "false") {
		return true
	}
	var volumes []string
	if raw := policy.Get("TERMINAL_DOCKER_VOLUMES", ""); raw != "" {
		var parsed any
		if yaml.Unmarshal([]byte(raw), &parsed) == nil {
			volumes = config.AsStrings(parsed)
		} else {
			volumes = strings.Split(raw, ",")
		}
	}
	for _, vol := range volumes {
		if dockerVolumeUsesHostPath(vol) {
			return true
		}
	}
	return false
}

// CheckCommandApproval is the shared pre exec verdict for a terminal command of one task
func CheckCommandApproval(ctx context.Context, env *Env, command, taskID string) (ApprovalDecision, error) {
	policy, err := terminalPolicy(env)
	if err != nil {
		return ApprovalDecision{}, err
	}
	if taskID != "" {
		ctx = WithApprovalTaskID(ctx, taskID)
	}
	result := CheckAllCommandGuards(ctx, env, command, policy.Get("TERMINAL_ENV", "local"), dockerHasHostAccess(env, policy))
	return ApprovalDecision{Allowed: result.Approved, Reason: result.Message, Pattern: result.PatternKey}, nil
}

// CommandVerdict is the dry run answer of EvaluateCommand
type CommandVerdict struct {
	Command            string
	EnvType            string
	Verdict            string
	ExitCode           int
	Rule               string
	Detail             string
	NormalizedVariants []string
}

const (
	ExitAllow = 0
	ExitUsage = 1
	ExitAsk   = 2
	ExitDeny  = 3
)

var verdictExit = map[string]int{"allow": ExitAllow, "ask-approval": ExitAsk, "hardline-deny": ExitDeny, "user-deny": ExitDeny}

// EvaluateCommand composes the runtime evaluators in guard order without running, prompting, or persisting
func EvaluateCommand(env *Env, command, envType string) CommandVerdict {
	if envType == "" {
		envType = "local"
	}
	LoadPermanentAllowlist(env)
	variants := CommandVariants(command)
	result := func(verdict, rule, detail string) CommandVerdict {
		return CommandVerdict{Command: command, EnvType: envType, Verdict: verdict, ExitCode: verdictExit[verdict], Rule: rule, Detail: detail, NormalizedVariants: variants}
	}
	if shouldSkipContainerGuards(envType, false) {
		if deny := matchUserDenyRule(env, command); deny != "" {
			return result("user-deny", deny, "matches a user-defined approvals.deny rule in config.yaml (blocked even in an isolated container, under --yolo / mode=off)")
		}
		return result("allow", "", fmt.Sprintf("env_type '%s' is an isolated container backend; the runtime skips all command guards for it except approvals.deny", envType))
	}
	if ok, desc := DetectHardlineCommand(command); ok {
		return result("hardline-deny", desc, "matches the hardline blocklist (never bypassable, blocked even under --yolo / approvals.mode=off)")
	}
	if ok, desc := CheckSudoStdinGuard(env, command); ok {
		return result("hardline-deny", desc, "sudo stdin guard (unconditional block)")
	}
	if deny := matchUserDenyRule(env, command); deny != "" {
		return result("user-deny", deny, "matches a user-defined approvals.deny rule in config.yaml (blocked even under --yolo / mode=off)")
	}
	if yoloModeFrozen || IsCurrentSessionYoloEnabled(env) || getApprovalMode(env) == "off" {
		return result("allow", "", "approval bypass active (--yolo or approvals.mode: off); only hardline/deny rules would block")
	}
	if commandMatchesPermanentAllowlist(env, command) {
		return result("allow", "", "matches command_allowlist in config.yaml (permanently approved)")
	}
	if patternKey, description, dangerous := DetectDangerousCommand(command); dangerous {
		return result("ask-approval", description, fmt.Sprintf("matches a dangerous-command pattern; the runtime would raise an interactive approval prompt (pattern key: '%s')", patternKey))
	}
	return result("allow", "", "no guard matched; would run without a prompt")
}

// RenderVerdict formats a dry run verdict the way the approvals test command prints it
func RenderVerdict(v CommandVerdict) string {
	var b strings.Builder
	fmt.Fprintf(&b, "command : %s\n", v.Command)
	fmt.Fprintf(&b, "env-type: %s\n", v.EnvType)
	fmt.Fprintf(&b, "verdict : %s  (exit %d)\n", v.Verdict, v.ExitCode)
	if v.Rule != "" {
		fmt.Fprintf(&b, "rule    : %s\n", v.Rule)
	}
	if v.Detail != "" {
		fmt.Fprintf(&b, "detail  : %s\n", v.Detail)
	}
	b.WriteString("normalized trace (variants the detectors evaluated):\n")
	for _, variant := range v.NormalizedVariants {
		fmt.Fprintf(&b, "  - %s\n", variant)
	}
	return b.String()
}
