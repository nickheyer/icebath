package provider

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const codexStderrTailLines = 12

var codexPermissionProfiles = map[string]string{
	"auto": "workspace-write", "approval-required": "read-only-with-approval",
	"unrestricted": "full-access", "yolo": "full-access",
}

// CodexTurnResult is the outcome of one turn through the codex app server, empty strings standing for None
type CodexTurnResult struct {
	FinalText          string
	ProjectedMessages  []map[string]any
	ToolIterations     int
	Interrupted        bool
	Error              string
	TurnID             string
	ThreadID           string
	SubmittedUserText  *string
	TokenUsageLast     *pyjson.Object
	ModelContextWindow int
	Compacted          bool
	ShouldRetire       bool
}

var codexTurnAbortedMarkers = []string{"<turn_aborted>", "<turn_aborted/>"}

// Text of a decoded id, empty for None
func cxIDText(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return pyrepr.Str(v)
}

// First primary or fallback value over the object sources that is not None
func codexFirstScopeID(lookups ...[3]any) any {
	for _, l := range lookups {
		src := l[0]
		if !cxIsObject(src) {
			continue
		}
		if observed := cxOr(cxGet(src, l[1].(string)), cxGet(src, l[2].(string))); observed != nil {
			return observed
		}
	}
	return nil
}

// Thread and turn identity a notification carries at top level, then on its turn or item
func codexNotificationScopeIDs(note any) (any, any) {
	if !cxIsObject(note) {
		return nil, nil
	}
	params := cxOr(cxGet(note, "params"), map[string]any{})
	if !cxIsObject(params) {
		return nil, nil
	}
	turn := cxOr(cxGet(params, "turn"), map[string]any{})
	item := cxOr(cxGet(params, "item"), map[string]any{})
	return codexFirstScopeID([3]any{params, "threadId", "thread_id"}, [3]any{turn, "threadId", "thread_id"}, [3]any{item, "threadId", "thread_id"}),
		codexFirstScopeID([3]any{params, "turnId", "turn_id"}, [3]any{turn, "id", "turnId"}, [3]any{item, "turnId", "turn_id"})
}

// False only for a notification explicitly scoped to a foreign thread or turn, unscoped ones are accepted
func codexNotificationBelongsToTurn(note any, threadID, turnID string) bool {
	if !cxIsObject(note) {
		return false
	}
	seenThread, seenTurn := codexNotificationScopeIDs(note)
	if threadID != "" && seenThread != nil && pyrepr.Str(seenThread) != threadID {
		return false
	}
	return !(turnID != "" && seenTurn != nil && pyrepr.Str(seenTurn) != turnID)
}

var codexTurnTextPartTypes = map[string]bool{"text": true, "input_text": true}

var codexTurnImagePartTypes = map[string]bool{"image": true, "image_url": true, "input_image": true}

// App server UserInput of one image part, a url for data and http references, a local path otherwise
func codexImagePartToTurnInput(item any) map[string]any {
	ref := cxOr(cxOr(cxOr(cxGet(item, "image_url"), cxGet(item, "url")), cxGet(item, "path")), cxGet(item, "image"))
	if cxIsObject(ref) {
		ref = cxOr(cxGet(ref, "url"), cxGet(ref, "path"))
	}
	text := ""
	if s, ok := ref.(string); ok {
		text = pystr.Strip(s)
	}
	if text == "" {
		return nil
	}
	for _, scheme := range []string{"data:", "http://", "https://"} {
		if strings.HasPrefix(text, scheme) {
			return map[string]any{"type": "image", "url": text}
		}
	}
	return map[string]any{"type": "localImage", "path": strings.TrimPrefix(text, "file://")}
}

// The turn start input list and the text the wire echoes back, images riding natively
func codexBuildTurnInput(userInput any) ([]any, string) {
	switch v := userInput.(type) {
	case string:
		return []any{map[string]any{"type": "text", "text": v}}, v
	case nil:
		return []any{map[string]any{"type": "text", "text": ""}}, ""
	case []any:
		var texts []string
		var images []any
		for _, item := range v {
			if !cxIsObject(item) {
				keep := item != nil
				if s, ok := item.(string); ok {
					keep = pystr.Strip(s) != ""
				}
				if keep {
					texts = append(texts, pyrepr.Str(item))
				}
				continue
			}
			partType, _ := cxGet(item, "type").(string)
			if codexTurnTextPartTypes[partType] {
				texts = append(texts, pyrepr.Str(cxOr(cxOr(cxGet(item, "text"), cxGet(item, "content")), "")))
			} else if codexTurnImagePartTypes[partType] {
				if mapped := codexImagePartToTurnInput(item); mapped != nil {
					images = append(images, mapped)
				}
			}
		}
		var kept []string
		for _, t := range texts {
			if t != "" {
				kept = append(kept, t)
			}
		}
		text := pystr.Strip(strings.Join(kept, "\n\n"))
		if text == "" && len(images) > 0 {
			text = "What do you see in this image?"
		}
		items := []any{}
		if text != "" || len(images) == 0 {
			items = append(items, map[string]any{"type": "text", "text": text})
		}
		return append(items, images...), text
	}
	text := pyrepr.Str(userInput)
	return []any{map[string]any{"type": "text", "text": text}}, text
}

var codexOAuthRefreshFailureHints = []string{
	"invalid_grant", "invalid grant", "refresh token", "refresh_token", "token refresh", "token_refresh",
	"token has expired", "expired_token", "token_expired", "expired token", "not authenticated", "unauthenticated",
	"re-authenticate", "reauthenticate", "please log in", "please login", "no auth profile",
}

var codexPrimaryOnlyOAuthHints = []string{"401 unauthorized", "unauthorized", "oauth", "auth profile"}

const codexOAuthReauthHint = "Codex authentication failed — your ChatGPT/Codex login looks expired or invalid. Run `codex login` to refresh, " +
	"then retry. (Fall back to default runtime with `/codex-runtime auto` if the issue persists.)"

func codexContainsAny(text string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(text, n) {
			return true
		}
	}
	return false
}

// Re-auth hint when the operation's own error or the stderr tail proves the codex login is broken
func codexClassifyOAuthFailure(primary, stderr string) string {
	primaryL, stderrL := strings.ToLower(primary), strings.ToLower(stderr)
	if codexContainsAny(primaryL, codexOAuthRefreshFailureHints) || codexContainsAny(primaryL, codexPrimaryOnlyOAuthHints) {
		return codexOAuthReauthHint
	}
	if codexContainsAny(stderrL, codexOAuthRefreshFailureHints) {
		return codexOAuthReauthHint
	}
	return ""
}

// CodexServerRequestRouting is the approval policy used when no interactive approval callback is wired in
type CodexServerRequestRouting struct {
	AutoApproveExec       bool
	AutoApproveApplyPatch bool
}

// CodexThreadResumeError reports a stored thread that thread/resume did not hand back
type CodexThreadResumeError struct {
	CodexAppServerError
	ThreadID string
}

func (e *CodexThreadResumeError) PyTypeName() string { return "CodexThreadResumeError" }

// Matches a CodexAppServerError target as the Python subclass does
func (e *CodexThreadResumeError) As(target any) bool {
	if t, ok := target.(**CodexAppServerError); ok {
		*t = &e.CodexAppServerError
		return true
	}
	return false
}

func newCodexThreadResumeError(threadID, detail string) *CodexThreadResumeError {
	return &CodexThreadResumeError{
		CodexAppServerError: CodexAppServerError{Code: -32600, Message: fmt.Sprintf("codex thread %s could not be resumed: %s", pystr.Slice(threadID, 0, 8), detail)},
		ThreadID:            threadID,
	}
}

// Thread id under thread.id, thread.sessionId, sessionId or threadId as codex versions serialize it
func codexExtractThreadID(result any) any {
	threadObj := cxOr(cxGet(result, "thread"), map[string]any{})
	return cxOr(cxOr(cxOr(cxGet(threadObj, "id"), cxGet(threadObj, "sessionId")), cxGet(result, "sessionId")), cxGet(result, "threadId"))
}

// CodexRPCClient is the JSON-RPC surface a session drives, CodexAppServerClient in production
type CodexRPCClient interface {
	Initialize(clientName, clientTitle, clientVersion string, capabilities any, timeout time.Duration) (any, error)
	Request(method string, params any, timeout time.Duration) (any, error)
	Respond(requestID any, result any) error
	RespondError(requestID any, code int, message string, data any) error
	TakeNotification(timeout time.Duration) any
	TakeServerRequest(timeout time.Duration) any
	StderrTail(n int) []string
	IsAlive() bool
	Close(timeout time.Duration)
}

// CodexApprovalCallback asks the user about a codex command and answers once, session, always or deny
type CodexApprovalCallback func(command, description string, allowPermanent bool) (string, error)

// CodexAppServerSessionOptions configures one codex thread per icebath session
type CodexAppServerSessionOptions struct {
	Cwd                   string
	CodexBin              string
	CodexHome             string
	PermissionProfile     string
	ApprovalCallback      CodexApprovalCallback
	OnEvent               func(note any)
	RequestRouting        *CodexServerRequestRouting
	ClientFactory         func(codexBin, codexHome string) (CodexRPCClient, error)
	Model                 string
	ModelProvider         string
	DeveloperInstructions string
	ResumeThreadID        string
	HistorySeed           string
	ToolEnv               *tools.Env
}

type codexTierState struct {
	unknown bool
	tier    *string
}

// Client constructor every default session factory spawns through, swappable like the module attribute
var codexAppServerClientFactory = func(opts CodexAppServerOptions) (CodexRPCClient, error) {
	client, err := NewCodexAppServerClient(opts)
	if err != nil {
		return nil, err
	}
	return client, nil
}

// CodexAppServerSession owns one codex thread for an icebath session, one caller at a time
type CodexAppServerSession struct {
	cwd                   string
	codexBin              string
	codexHome             string
	resumeThreadID        string
	model                 string
	modelProvider         string
	serviceTierSent       codexTierState
	developerInstructions string
	historySeed           string
	permissionProfile     string
	approvalCallback      CodexApprovalCallback
	onEvent               func(note any)
	routing               CodexServerRequestRouting
	clientFactory         func(codexBin, codexHome string) (CodexRPCClient, error)
	toolEnv               *tools.Env
	clientMu              sync.Mutex
	client                CodexRPCClient
	threadID              string
	interrupt             atomic.Bool
	activeTurnMu          sync.Mutex
	activeTurnID          string
	pendingFileChanges    map[string]string
	closed                atomic.Bool
}

// Builds a session whose client spawns on first use
func NewCodexAppServerSession(opts CodexAppServerSessionOptions) *CodexAppServerSession {
	s := &CodexAppServerSession{
		cwd: opts.Cwd, codexBin: opts.CodexBin, codexHome: opts.CodexHome, resumeThreadID: opts.ResumeThreadID,
		model: pystr.Strip(opts.Model), modelProvider: pystr.Strip(opts.ModelProvider),
		developerInstructions: opts.DeveloperInstructions, historySeed: opts.HistorySeed,
		permissionProfile: opts.PermissionProfile, approvalCallback: opts.ApprovalCallback, onEvent: opts.OnEvent,
		clientFactory: opts.ClientFactory, toolEnv: opts.ToolEnv, pendingFileChanges: map[string]string{},
	}
	if s.cwd == "" {
		s.cwd, _ = os.Getwd()
	}
	if s.codexBin == "" {
		s.codexBin = "codex"
	}
	if s.permissionProfile == "" {
		mode, ok := os.LookupEnv("ICEBATH_TERMINAL_SECURITY_MODE")
		if !ok {
			mode = "auto"
		}
		if s.permissionProfile = codexPermissionProfiles[mode]; s.permissionProfile == "" {
			s.permissionProfile = "workspace-write"
		}
	}
	if opts.RequestRouting != nil {
		s.routing = *opts.RequestRouting
	}
	if s.clientFactory == nil {
		env := opts.ToolEnv
		s.clientFactory = func(codexBin, codexHome string) (CodexRPCClient, error) {
			return codexAppServerClientFactory(CodexAppServerOptions{CodexBin: codexBin, CodexHome: codexHome, ToolEnv: env})
		}
	}
	return s
}

func (s *CodexAppServerSession) currentClient() CodexRPCClient {
	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	return s.client
}

// Thread id of the started session, empty before start
func (s *CodexAppServerSession) ThreadID() string { return s.threadID }

// Spawns, handshakes and starts or resumes the thread, idempotent, a failed resume errors once
func (s *CodexAppServerSession) EnsureStarted() (string, error) {
	if s.threadID != "" {
		return s.threadID, nil
	}
	client := s.currentClient()
	if client == nil {
		created, err := s.clientFactory(s.codexBin, s.codexHome)
		if err != nil {
			return "", err
		}
		s.clientMu.Lock()
		s.client, client = created, created
		s.clientMu.Unlock()
		if _, err := client.Initialize("icebath", "Icebath Agent", Version, nil, 10*time.Second); err != nil {
			return "", err
		}
	}
	params := pyjson.NewObject()
	params.Set("cwd", s.cwd)
	params.Set("personality", "none")
	if s.developerInstructions != "" && pystr.Strip(s.developerInstructions) != "" {
		params.Set("developerInstructions", s.developerInstructions)
	}
	if s.modelProvider != "" {
		params.Set("modelProvider", s.modelProvider)
	}
	if s.model != "" {
		params.Set("model", s.model)
	}
	var threadID string
	if s.resumeThreadID != "" {
		wanted := s.resumeThreadID
		s.resumeThreadID = ""
		resumed, err := s.resumeThread(client, wanted, params)
		if err != nil {
			return "", err
		}
		threadID = resumed
		s.serviceTierSent = codexTierState{unknown: true}
		slog.Info(fmt.Sprintf("codex app-server thread resumed: id=%s cwd=%s", pystr.Slice(threadID, 0, 8), s.cwd))
	} else {
		if s.historySeed != "" {
			var parts []string
			if di, _ := params.Get("developerInstructions"); di != nil && di != "" {
				parts = append(parts, di.(string))
			}
			params.Set("developerInstructions", strings.Join(append(parts, s.historySeed), "\n\n"))
		}
		result, err := client.Request("thread/start", params, 15*time.Second)
		if err != nil {
			return "", err
		}
		raw := codexExtractThreadID(result)
		if !cxTruthy(raw) {
			keys := []string{}
			if o, ok := result.(*pyjson.Object); ok && o != nil {
				keys = append(keys, o.Keys...)
			} else if m, ok := result.(map[string]any); ok {
				for k := range m {
					keys = append(keys, k)
				}
			} else {
				return "", &codexPyError{"AttributeError", fmt.Sprintf("'%s' object has no attribute 'keys'", pyrepr.TypeName(result))}
			}
			sort.Strings(keys)
			return "", &CodexAppServerError{Code: -32603, Message: fmt.Sprintf("codex thread/start returned no thread id (payload keys: %s)", pyrepr.Repr(keys))}
		}
		threadID = cxIDText(raw)
		s.serviceTierSent = codexTierState{}
		slog.Info(fmt.Sprintf("codex app-server thread started: id=%s profile=%s cwd=%s", pystr.Slice(threadID, 0, 8), s.permissionProfile, s.cwd))
	}
	s.threadID = threadID
	return threadID, nil
}

// thread/resume of the stored id carrying the current thread start params
func (s *CodexAppServerSession) resumeThread(client CodexRPCClient, wanted string, params *pyjson.Object) (string, error) {
	body := pyjson.NewObject()
	body.Set("threadId", wanted)
	for _, k := range params.Keys {
		v, _ := params.Get(k)
		body.Set(k, v)
	}
	result, err := client.Request("thread/resume", body, 15*time.Second)
	if err != nil {
		var appErr *CodexAppServerError
		if errors.As(err, &appErr) {
			return "", newCodexThreadResumeError(wanted, appErr.Message)
		}
		return "", err
	}
	if got := codexExtractThreadID(result); got != wanted {
		return "", newCodexThreadResumeError(wanted, fmt.Sprintf("app-server answered with thread %s", pyrepr.Repr(pystr.Slice(pyrepr.Str(got), 0, 8))))
	}
	return wanted, nil
}

// Closes the client and forgets the thread, idempotent
func (s *CodexAppServerSession) Close() {
	if s.closed.Swap(true) {
		return
	}
	s.activeTurnMu.Lock()
	s.activeTurnID = ""
	s.activeTurnMu.Unlock()
	s.clientMu.Lock()
	client := s.client
	s.client = nil
	s.clientMu.Unlock()
	if client != nil {
		func() {
			defer func() { _ = recover() }()
			client.Close(3 * time.Second)
		}()
	}
	s.threadID = ""
}

// Signals the active turn loop to issue turn/interrupt and unwind, idempotent
func (s *CodexAppServerSession) RequestInterrupt() { s.interrupt.Store(true) }

// Appends user guidance to the active codex turn through turn/steer
func (s *CodexAppServerSession) RequestSteer(text string) (bool, error) {
	cleaned := pystr.Strip(text)
	if cleaned == "" {
		return false, nil
	}
	s.activeTurnMu.Lock()
	turnID, threadID := s.activeTurnID, s.threadID
	s.activeTurnMu.Unlock()
	client := s.currentClient()
	if turnID == "" || threadID == "" || client == nil {
		return false, nil
	}
	params := pyjson.NewObject()
	params.Set("threadId", threadID)
	params.Set("input", []any{map[string]any{"type": "text", "text": cleaned}})
	params.Set("expectedTurnId", turnID)
	response, err := client.Request("turn/steer", params, 10*time.Second)
	if err != nil {
		if codexIsAppServerOrTimeout(err) {
			slog.Debug("turn/steer rejected for active Codex turn", "error", err)
			return false, nil
		}
		return false, err
	}
	var accepted any
	if cxIsObject(response) {
		accepted = cxGet(response, "turnId")
	}
	return accepted == nil || accepted == turnID, nil
}

func codexIsTimeout(err error) bool {
	var t *CodexRequestTimeoutError
	return errors.As(err, &t) || pyrepr.TypeName(err) == "TimeoutError"
}

func codexIsAppServerOrTimeout(err error) bool {
	var appErr *CodexAppServerError
	return errors.As(err, &appErr) || codexIsTimeout(err)
}

func codexIsTransportError(err error) bool {
	var t *CodexAppServerTransportError
	return errors.As(err, &t)
}

// User facing error with the force redacted stderr tail appended
func (s *CodexAppServerSession) formatErrorWithStderr(prefix string, detail any, tailLines int) (out string) {
	excStr := ""
	switch d := detail.(type) {
	case nil:
	case error:
		excStr = d.Error()
	case string:
		excStr = d
	default:
		excStr = pyrepr.Str(d)
	}
	base := prefix
	if excStr != "" {
		base = prefix + ": " + excStr
	}
	defer func() {
		if r := recover(); r != nil {
			out = base
		}
	}()
	var tail []string
	if client := s.currentClient(); client != nil {
		tail = client.StderrTail(tailLines)
	}
	var lines []string
	for _, line := range tail {
		if line != "" {
			lines = append(lines, pystr.Rstrip(line))
		}
	}
	joined := strings.Join(lines, "\n")
	if pystr.Strip(joined) == "" {
		return base
	}
	return fmt.Sprintf("%s\ncodex stderr (last %d lines):\n%s", base, len(tail), tools.RedactSensitiveTextWith(s.toolEnv, joined, tools.RedactOptions{Force: true}))
}

func (s *CodexAppServerSession) stderrBlob(n int) string {
	client := s.currentClient()
	if client == nil {
		return ""
	}
	return strings.Join(client.StderrTail(n), "\n")
}

// Records a terminal error and flags the session for respawn on the next turn
func codexRetire(result *CodexTurnResult, msg string) {
	result.Error = msg
	result.ShouldRetire = true
}

// OAuth failures retire with the re-auth hint, any other error carries the stderr tail
func (s *CodexAppServerSession) setClassifiedError(result *CodexTurnResult, prefix, classifyText string, detail any) {
	if hint := codexClassifyOAuthFailure(classifyText, s.stderrBlob(40)); hint != "" {
		codexRetire(result, hint)
		return
	}
	result.Error = s.formatErrorWithStderr(prefix, detail, codexStderrTailLines)
}

// Starts the session, a startup failure becoming a retiring result error
func (s *CodexAppServerSession) startFor(result *CodexTurnResult) (bool, error) {
	if _, err := s.EnsureStarted(); err != nil {
		if codexIsAppServerOrTimeout(err) {
			codexRetire(result, s.formatErrorWithStderr("codex app-server startup failed", err, codexStderrTailLines))
			return false, nil
		}
		return false, err
	}
	result.ThreadID = s.threadID
	return true, nil
}

// Issues a request, filling the result error on failure, a timeout always retires
func (s *CodexAppServerSession) requestFor(result *CodexTurnResult, method string, params any, label string) (any, bool, error) {
	reply, err := s.currentClient().Request(method, params, 10*time.Second)
	if err == nil {
		return reply, true, nil
	}
	var appErr *CodexAppServerError
	switch {
	case codexIsTransportError(err):
		codexRetire(result, s.formatErrorWithStderr(label+" failed", err, codexStderrTailLines))
	case errors.As(err, &appErr):
		s.setClassifiedError(result, label+" failed", appErr.Message, err)
	case codexIsTimeout(err):
		hint := codexClassifyOAuthFailure("", s.stderrBlob(40))
		if hint == "" {
			hint = s.formatErrorWithStderr(label+" timed out", err, codexStderrTailLines)
		}
		codexRetire(result, hint)
	default:
		return nil, false, err
	}
	return nil, false, nil
}

// Ends the turn early when codex exited or the session closed under the loop
func (s *CodexAppServerSession) subprocessDied(result *CodexTurnResult, client CodexRPCClient) bool {
	if client == nil || s.closed.Load() || s.currentClient() != client {
		result.Interrupted = true
		codexRetire(result, "codex app-server session closed while the turn was in flight")
		return true
	}
	if client.IsAlive() {
		return false
	}
	hint := codexClassifyOAuthFailure("", s.stderrBlob(60))
	if hint == "" {
		hint = s.formatErrorWithStderr("codex app-server subprocess exited unexpectedly", nil, 20)
	}
	codexRetire(result, hint)
	return true
}

// Fans one in scope notification out to display, accounting, file change tracking and the projector
func (s *CodexAppServerSession) absorbNotification(result *CodexTurnResult, projector *CodexEventProjector, note any) (CodexProjectionResult, bool) {
	if s.onEvent != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Debug("on_event callback raised", "panic", r)
				}
			}()
			s.onEvent(note)
		}()
	}
	codexApplyAccountingNotification(result, note)
	s.trackPendingFileChange(note)
	projection := projector.Project(note)
	result.ProjectedMessages = append(result.ProjectedMessages, projection.Messages...)
	if projection.IsToolIteration {
		result.ToolIterations++
	}
	aborted := false
	if projection.FinalText != nil {
		result.FinalText = *projection.FinalText
		if aborted = codexHasTurnAbortedMarker(*projection.FinalText); aborted {
			result.Interrupted = true
			if result.Error == "" {
				result.Error = "codex reported turn_aborted"
			}
		}
	}
	return projection, aborted
}

// CodexTurnOptions are the per turn settings sent on turn/start and the loop timeouts in seconds
type CodexTurnOptions struct {
	Model                   string
	ReasoningEffort         *string
	ServiceTier             *string
	TurnTimeout             float64
	NotificationPollTimeout float64
	PostToolQuietTimeout    float64
}

func codexSeconds(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }

func codexTierEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Sends a user message and blocks until turn/completed, bridging approvals and projecting items
func (s *CodexAppServerSession) RunTurn(userInput any, opts CodexTurnOptions) (*CodexTurnResult, error) {
	result, err := s.runTurn(userInput, opts)
	if err == nil {
		s.interrupt.Store(false)
	}
	return result, err
}

func (s *CodexAppServerSession) runTurn(userInput any, opts CodexTurnOptions) (*CodexTurnResult, error) {
	if opts.TurnTimeout == 0 {
		opts.TurnTimeout = 600
	}
	if opts.NotificationPollTimeout == 0 {
		opts.NotificationPollTimeout = 0.25
	}
	if opts.PostToolQuietTimeout == 0 {
		opts.PostToolQuietTimeout = 90
	}
	result := &CodexTurnResult{}
	started, err := s.startFor(result)
	if err != nil || !started {
		return result, err
	}
	if s.interrupt.Load() {
		result.Interrupted = true
		return result, nil
	}
	inputItems, submitted := codexBuildTurnInput(userInput)
	result.SubmittedUserText = &submitted
	params := pyjson.NewObject()
	params.Set("threadId", s.threadID)
	params.Set("input", inputItems)
	if opts.Model != "" {
		params.Set("model", opts.Model)
	}
	if opts.ReasoningEffort != nil {
		params.Set("effort", *opts.ReasoningEffort)
	}
	if s.serviceTierSent.unknown || !codexTierEqual(opts.ServiceTier, s.serviceTierSent.tier) {
		if opts.ServiceTier != nil {
			params.Set("serviceTier", *opts.ServiceTier)
		} else {
			params.Set("serviceTier", nil)
		}
	}
	ts, ok, err := s.requestFor(result, "turn/start", params, "turn/start")
	if err != nil || !ok {
		return result, err
	}
	s.serviceTierSent = codexTierState{tier: opts.ServiceTier}
	return result, s.runStartedTurn(result, ts, opts)
}

// Drives an accepted turn/start to completion with the quiet warning, approvals and projection
func (s *CodexAppServerSession) runStartedTurn(result *CodexTurnResult, ts any, opts CodexTurnOptions) error {
	projector := NewCodexEventProjector()
	client := s.currentClient()
	result.TurnID = cxIDText(cxGet(cxOr(cxGet(ts, "turn"), map[string]any{}), "id"))
	s.activeTurnMu.Lock()
	s.activeTurnID = result.TurnID
	s.activeTurnMu.Unlock()
	var lastToolCompletionAt *float64
	now := func() *float64 { t := codexMonotonic(); return &t }
	warnIfQuiet := func() bool {
		if lastToolCompletionAt == nil || codexMonotonic()-*lastToolCompletionAt <= opts.PostToolQuietTimeout {
			return false
		}
		lastToolCompletionAt = nil
		slog.Warn(fmt.Sprintf("codex has emitted no events for %.0fs after a tool result; still waiting (turn deadline %.0fs)", opts.PostToolQuietTimeout, opts.TurnTimeout))
		return false
	}
	onServerRequest := func(sreq any) (bool, error) {
		turnComplete := false
		for range 8 {
			pending := client.TakeNotification(0)
			if pending == nil {
				break
			}
			if !codexNotificationBelongsToTurn(pending, s.threadID, result.TurnID) {
				slog.Debug(fmt.Sprintf("ignoring foreign codex notification while draining server request: method=%s", pyrepr.Str(cxGet(pending, "method"))))
				continue
			}
			proj, aborted := s.absorbNotification(result, projector, pending)
			if proj.IsToolIteration {
				lastToolCompletionAt = now()
			}
			turnComplete = turnComplete || aborted
		}
		if err := s.handleServerRequest(sreq); err != nil {
			return false, err
		}
		lastToolCompletionAt = nil
		return turnComplete, nil
	}
	onNote := func(note any, method string) bool {
		projection, aborted := s.absorbNotification(result, projector, note)
		if projection.IsToolIteration {
			lastToolCompletionAt = now()
		} else if len(projection.Messages) > 0 || projection.FinalText != nil {
			lastToolCompletionAt = nil
		}
		if method != "turn/completed" {
			return aborted
		}
		turnObj := cxOr(cxGet(cxOr(cxGet(note, "params"), map[string]any{}), "turn"), map[string]any{})
		status := cxGet(turnObj, "status")
		if cxTruthy(status) && status != "completed" && status != "interrupted" && cxTruthy(cxGet(turnObj, "error")) {
			errMsg := formatResponsesError(pyjson.Plain(cxGet(turnObj, "error")), pyrepr.Str(status))
			s.setClassifiedError(result, "turn ended status="+pyrepr.Str(status), errMsg, errMsg)
		}
		return true
	}
	err := s.driveTurn(result, codexDriveOptions{
		turnTimeout: opts.TurnTimeout, pollTimeout: opts.NotificationPollTimeout, timeoutLabel: "turn",
		beforePoll: warnIfQuiet, onServerRequest: onServerRequest, onNote: onNote, acceptFinalTextAtDeadline: true,
	})
	s.activeTurnMu.Lock()
	s.activeTurnID = ""
	s.activeTurnMu.Unlock()
	return err
}

var codexClockBase = time.Now()

// Seconds on a monotonic clock, swapped by tests the way pytest patches time.monotonic
var codexMonotonic = func() float64 { return time.Since(codexClockBase).Seconds() }

type codexDriveOptions struct {
	turnTimeout               float64
	pollTimeout               float64
	timeoutLabel              string
	onServerRequest           func(sreq any) (bool, error)
	onNote                    func(note any, method string) bool
	beforePoll                func() bool
	preScopeFilter            func(note any, method string) bool
	acceptFinalTextAtDeadline bool
}

// Shared poll loop of a turn and a compaction until turn/completed or the deadline, which interrupts and retires
func (s *CodexAppServerSession) driveTurn(result *CodexTurnResult, o codexDriveOptions) error {
	deadline := codexMonotonic() + o.turnTimeout
	turnComplete := false
	client := s.currentClient()
	for codexMonotonic() < deadline && !turnComplete {
		if s.interrupt.Load() {
			s.issueInterrupt(result.TurnID)
			result.Interrupted = true
			break
		}
		if s.subprocessDied(result, client) {
			break
		}
		if o.beforePoll != nil && o.beforePoll() {
			break
		}
		if sreq := client.TakeServerRequest(0); sreq != nil {
			complete, err := o.onServerRequest(sreq)
			if err != nil {
				if codexIsTransportError(err) {
					codexRetire(result, s.formatErrorWithStderr("codex app-server request response failed", err, codexStderrTailLines))
					break
				}
				return err
			}
			turnComplete = complete
			continue
		}
		note := client.TakeNotification(codexSeconds(o.pollTimeout))
		if note == nil {
			continue
		}
		method := ""
		if cxHas(note, "method") {
			method = pyrepr.Str(cxGet(note, "method"))
		}
		if o.preScopeFilter != nil && !o.preScopeFilter(note, method) {
			continue
		}
		if !codexNotificationBelongsToTurn(note, s.threadID, result.TurnID) {
			slog.Debug(fmt.Sprintf("ignoring foreign codex notification: method=%s", method))
			continue
		}
		turnComplete = o.onNote(note, method)
	}
	if o.acceptFinalTextAtDeadline && !turnComplete && !result.Interrupted && result.FinalText != "" && result.Error == "" {
		slog.Warn("codex app-server turn reached deadline after a completed assistant message but before " +
			"turn/completed; accepting the assistant text as the terminal response")
		turnComplete = true
	}
	if !turnComplete && !result.Interrupted {
		s.issueInterrupt(result.TurnID)
		result.Interrupted = true
		if result.Error == "" {
			result.Error = s.formatErrorWithStderr(fmt.Sprintf("%s timed out after %ss", o.timeoutLabel, pyrepr.Str(o.turnTimeout)), nil, codexStderrTailLines)
		}
		result.ShouldRetire = true
	}
	return nil
}

// Triggers codex native history compaction of the current thread and waits for its turn/completed
func (s *CodexAppServerSession) CompactThread(turnTimeout, notificationPollTimeout float64) (*CodexTurnResult, error) {
	if turnTimeout == 0 {
		turnTimeout = 600
	}
	if notificationPollTimeout == 0 {
		notificationPollTimeout = 0.25
	}
	result := &CodexTurnResult{}
	started, err := s.startFor(result)
	if err != nil || !started {
		return result, err
	}
	s.interrupt.Store(false)
	projector := NewCodexEventProjector()
	params := pyjson.NewObject()
	params.Set("threadId", s.threadID)
	if _, ok, err := s.requestFor(result, "thread/compact/start", params, "thread/compact/start"); err != nil || !ok {
		return result, err
	}
	preScopeFilter := func(note any, method string) bool {
		if result.TurnID != "" {
			return true
		}
		observedThread, observedTurn := codexNotificationScopeIDs(note)
		if method == "turn/started" {
			if observedThread != nil && pyrepr.Str(observedThread) != s.threadID {
				slog.Debug(fmt.Sprintf("ignoring foreign compact turn/started: thread=%s", pyrepr.Str(observedThread)))
				return false
			}
			if observedTurn == nil {
				slog.Debug("ignoring compact turn/started without a turn id")
				return false
			}
			result.TurnID = pyrepr.Str(observedTurn)
		} else if observedTurn != nil || method == "item/completed" || method == "turn/completed" {
			slog.Debug(fmt.Sprintf("ignoring codex notification before compact turn start: method=%s", method))
			return false
		}
		return true
	}
	onNote := func(note any, method string) bool {
		_, aborted := s.absorbNotification(result, projector, note)
		if method != "turn/started" && method != "turn/completed" {
			return aborted
		}
		turnObj := cxOr(cxGet(cxOr(cxGet(note, "params"), map[string]any{}), "turn"), map[string]any{})
		if id := cxGet(turnObj, "id"); cxTruthy(id) {
			result.TurnID = cxIDText(id)
		}
		if method == "turn/started" {
			return aborted
		}
		status := cxGet(turnObj, "status")
		if status == "interrupted" {
			result.Interrupted = true
			if result.Error == "" {
				result.Error = "compact turn interrupted"
			}
		} else if cxTruthy(status) && status != "completed" {
			errMsg := formatResponsesError(pyjson.Plain(cxGet(turnObj, "error")), pyrepr.Str(status))
			s.setClassifiedError(result, "compact turn ended status="+pyrepr.Str(status), errMsg, errMsg)
		}
		return true
	}
	onServerRequest := func(sreq any) (bool, error) { return false, s.handleServerRequest(sreq) }
	err = s.driveTurn(result, codexDriveOptions{
		turnTimeout: turnTimeout, pollTimeout: notificationPollTimeout, timeoutLabel: "compact turn",
		onServerRequest: onServerRequest, onNote: onNote, preScopeFilter: preScopeFilter,
	})
	return result, err
}

// Sends turn/interrupt for the turn, failures logged and never fatal
func (s *CodexAppServerSession) issueInterrupt(turnID string) {
	client := s.currentClient()
	if client == nil || s.threadID == "" || turnID == "" {
		return
	}
	params := pyjson.NewObject()
	params.Set("threadId", s.threadID)
	params.Set("turnId", turnID)
	if _, err := client.Request("turn/interrupt", params, 5*time.Second); err != nil {
		var appErr *CodexAppServerError
		switch {
		case errors.As(err, &appErr):
			slog.Debug(fmt.Sprintf("turn/interrupt non-fatal: %s", err.Error()))
		case codexIsTimeout(err):
			slog.Warn("turn/interrupt timed out")
		}
	}
}

// Answers a codex approval or elicitation request, unknown methods get a JSON-RPC error so codex never hangs
func (s *CodexAppServerSession) handleServerRequest(req any) error {
	client := s.currentClient()
	if client == nil {
		return nil
	}
	method := ""
	if cxHas(req, "method") {
		method = pyrepr.Str(cxGet(req, "method"))
	}
	rid := cxGet(req, "id")
	params := cxOr(cxGet(req, "params"), map[string]any{})
	var reply any
	switch method {
	case "item/commandExecution/requestApproval":
		reply = map[string]any{"decision": s.decideExecApproval(params)}
	case "item/fileChange/requestApproval":
		reply = map[string]any{"decision": s.decideApplyPatchApproval(params)}
	case "item/permissions/requestApproval":
		reply = map[string]any{"decision": "decline"}
	case "mcpServer/elicitation/request":
		reply = codexRespondElicitation(params)
	default:
		slog.Warn(fmt.Sprintf("Unknown codex server request: %s", method))
		return client.RespondError(rid, -32601, "Unsupported method: "+method, nil)
	}
	return client.Respond(rid, reply)
}

// Accepts elicitation from the icebath tools server and declines every other server
func codexRespondElicitation(params any) any {
	action := "decline"
	if cxStr(params, "serverName", "") == CodexToolsMCPServerName {
		action = "accept"
	}
	out := pyjson.NewObject()
	out.Set("action", action)
	out.Set("content", nil)
	out.Set("_meta", nil)
	return out
}

// Auto approves, declines without a callback, else asks through the prompt the callback renders
func (s *CodexAppServerSession) runApprovalCallback(autoApprove bool, prompt func() (string, string), logLabel string) (decision string) {
	if autoApprove {
		return "accept"
	}
	if s.approvalCallback == nil {
		return "decline"
	}
	command, description := prompt()
	defer func() {
		if r := recover(); r != nil {
			slog.Error(fmt.Sprintf("approval_callback raised on %s", logLabel), "panic", r)
			decision = "decline"
		}
	}()
	choice, err := s.approvalCallback(command, description, false)
	if err != nil {
		slog.Error(fmt.Sprintf("approval_callback raised on %s", logLabel), "error", err)
		return "decline"
	}
	return codexApprovalChoiceToDecision(choice)
}

func (s *CodexAppServerSession) decideExecApproval(params any) string {
	prompt := func() (string, string) {
		where := cxStr(params, "cwd", "")
		if where == "" {
			where = s.cwd
		}
		if where == "" {
			where = "<unknown>"
		}
		description := "Codex requests exec in " + where
		if reason := cxGet(params, "reason"); cxTruthy(reason) {
			description += " — " + pyrepr.Str(reason)
		}
		return cxStr(params, "command", ""), description
	}
	return s.runApprovalCallback(s.routing.AutoApproveExec, prompt, "exec request")
}

func (s *CodexAppServerSession) decideApplyPatchApproval(params any) string {
	prompt := func() (string, string) {
		reason, grantRoot := cxGet(params, "reason"), cxGet(params, "grantRoot")
		changeSummary := s.pendingFileChanges[cxKey(cxOr(cxGet(params, "itemId"), ""))]
		var parts []string
		if cxTruthy(reason) {
			parts = append(parts, pyrepr.Str(reason))
		}
		if changeSummary != "" {
			parts = append(parts, changeSummary)
		}
		if cxTruthy(grantRoot) {
			parts = append(parts, "grants write to "+pyrepr.Str(grantRoot))
		}
		detail := changeSummary
		if detail == "" && cxTruthy(reason) {
			detail = pyrepr.Str(reason)
		}
		command := "apply_patch"
		if detail != "" {
			command = "apply_patch: " + detail
		}
		description := "Codex requests to apply a patch"
		if len(parts) > 0 {
			description = strings.Join(parts, "; ")
		}
		return command, description
	}
	return s.runApprovalCallback(s.routing.AutoApproveApplyPatch, prompt, "apply_patch")
}

// Dict key of a decoded scalar, strings and other values kept in separate key spaces
func cxKey(v any) string {
	if s, ok := v.(string); ok {
		return "s:" + s
	}
	return "r:" + pyrepr.Repr(v)
}

// Tracks fileChange items from started to completed so the apply_patch prompt can show the changeset
func (s *CodexAppServerSession) trackPendingFileChange(note any) {
	method := ""
	if cxHas(note, "method") {
		method = pyrepr.Str(cxGet(note, "method"))
	}
	item := cxOr(cxGet(cxOr(cxGet(note, "params"), map[string]any{}), "item"), map[string]any{})
	itemID := cxOr(cxGet(item, "id"), "")
	if cxGet(item, "type") != "fileChange" || !cxTruthy(itemID) {
		return
	}
	switch method {
	case "item/completed":
		delete(s.pendingFileChanges, cxKey(itemID))
	case "item/started":
		s.pendingFileChanges[cxKey(itemID)] = codexSummarizeFileChanges(cxOr(cxGet(item, "changes"), []any{}))
	}
}

// One line count and path preview of a fileChange item's changes
func codexSummarizeFileChanges(rawChanges any) string {
	if !cxTruthy(rawChanges) {
		return "1 change pending"
	}
	kinds := map[string]int{}
	var paths []string
	for _, ch := range cxList(rawChanges) {
		if !cxIsObject(ch) {
			continue
		}
		kinds[cxStr(cxOr(cxGet(ch, "kind"), map[string]any{}), "type", "update")]++
		if p := cxGet(ch, "path"); cxTruthy(p) {
			paths = append(paths, cxStr(ch, "path", ""))
		}
	}
	names := make([]string, 0, len(kinds))
	for k := range kinds {
		names = append(names, k)
	}
	sort.Strings(names)
	counts := make([]string, len(names))
	for i, k := range names {
		counts[i] = fmt.Sprintf("%d %s", kinds[k], k)
	}
	countText := strings.Join(counts, ", ")
	preview := strings.Join(paths[:min(3, len(paths))], ", ")
	if len(paths) > 3 {
		preview += fmt.Sprintf(", +%d more", len(paths)-3)
	}
	if preview != "" {
		return countText + ": " + preview
	}
	return countText
}

// Python int of a decoded value, bools counting as ints and floats refused
func cxInt(v any) (int64, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case float64, float32:
		return 0, false
	}
	if n, ok := pyrepr.Int(v); ok && n.IsInt64() {
		return n.Int64(), true
	}
	return 0, false
}

// Captures token usage and compaction boundaries a notification reports
func codexApplyAccountingNotification(result *CodexTurnResult, note any) {
	if !cxIsObject(note) {
		return
	}
	method := cxStr(note, "method", "")
	params := cxOr(cxGet(note, "params"), map[string]any{})
	if !cxIsObject(params) {
		return
	}
	if method == "thread/tokenUsage/updated" {
		usage := cxOr(cxGet(params, "tokenUsage"), map[string]any{})
		if cxIsObject(usage) {
			last, window := cxGet(usage, "last"), cxGet(usage, "modelContextWindow")
			if cxIsObject(last) {
				result.TokenUsageLast = codexCopyObject(last)
			}
			if n, ok := cxInt(window); ok && n > 0 {
				result.ModelContextWindow = int(n)
			}
		}
		return
	}
	var item any
	if method == "item/started" || method == "item/completed" {
		item = cxGet(params, "item")
	}
	if method == "thread/compacted" || (cxIsObject(item) && cxGet(item, "type") == "contextCompaction") {
		result.Compacted = true
		if id := cxGet(params, "threadId"); cxTruthy(id) {
			result.ThreadID = cxIDText(id)
		}
		if id := cxGet(params, "turnId"); cxTruthy(id) {
			result.TurnID = cxIDText(id)
		}
	}
}

// Shallow ordered copy of a decoded object
func codexCopyObject(v any) *pyjson.Object {
	out := pyjson.NewObject()
	switch o := v.(type) {
	case *pyjson.Object:
		for _, k := range o.Keys {
			val, _ := o.Get(k)
			out.Set(k, val)
		}
	case map[string]any:
		keys := make([]string, 0, len(o))
		for k := range o {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out.Set(k, o[k])
		}
	}
	return out
}

// Codex approval decision wire value of an icebath approval choice
func codexApprovalChoiceToDecision(choice string) string {
	switch choice {
	case "once":
		return "accept"
	case "session", "always":
		return "acceptForSession"
	}
	return "decline"
}

// True when text carries a raw turn_aborted marker, terminal without turn/completed
func codexHasTurnAbortedMarker(text string) bool {
	return text != "" && codexContainsAny(text, codexTurnAbortedMarkers)
}
