package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	stderrTailLines = 12
	reauthHint      = "Codex authentication failed — your ChatGPT/Codex login looks expired or invalid. Run `codex login` to refresh, " +
		"then retry. (Fall back to default runtime with `/codex-runtime auto` if the issue persists.)"
	DefaultTurnTimeout  = 600 * time.Second
	DefaultQuietTimeout = 90 * time.Second
	pollInterval        = 250 * time.Millisecond
)

var permissionProfiles = map[string]string{
	"auto": "workspace-write", "approval-required": "read-only-with-approval", "unrestricted": "full-access", "yolo": "full-access",
}

var oauthFailureHints = []string{
	"invalid_grant", "invalid grant", "refresh token", "refresh_token", "token refresh", "token_refresh",
	"token has expired", "expired_token", "token_expired", "expired token", "not authenticated", "unauthenticated",
	"re-authenticate", "reauthenticate", "please log in", "please login", "no auth profile",
}

var primaryOnlyOAuthHints = []string{"401 unauthorized", "unauthorized", "oauth", "auth profile"}

type ResumeError struct {
	ThreadID string
	Detail   string
}

func (e *ResumeError) Error() string {
	return "codex thread " + truncate(e.ThreadID, 8) + " could not be resumed: " + e.Detail
}

type TurnError struct {
	Message string
	Reauth  bool
}

func (e *TurnError) Error() string { return e.Message }

type Options struct {
	Bin                   string
	Home                  string
	Cwd                   string
	Model                 string
	ModelProvider         string
	DeveloperInstructions string
	ResumeThreadID        string
	HistorySeed           string
	PermissionProfile     string
	Approve               func(ctx context.Context, command, description string) string
	AutoApproveExec       bool
	AutoApprovePatch      bool
	ToolsServerName       string
	ExtraArgs             []string
	Env                   []string
	Version               string
}

type TurnInput struct {
	Message      wire.Message
	Model        string
	Effort       string
	ServiceTier  string
	Timeout      time.Duration
	QuietTimeout time.Duration
}

type TurnResult struct {
	FinalText      string
	Projected      []wire.Message
	ToolIterations int
	Interrupted    bool
	Err            error
	TurnID         string
	ThreadID       string
	SubmittedText  string
	Usage          *wire.Usage
	UsageRaw       json.RawMessage
	ContextWindow  int
	Compacted      bool
	Retire         bool
}

type Session struct {
	o          Options
	client     *client
	mu         sync.Mutex
	threadID   string
	activeTurn string
	closed     bool
	resume     string
	tierSent   string
	tierKnown  bool
	interrupt  chan struct{}
	fileEdits  map[string]string
	started    map[string]startedItem
}

type startedItem struct {
	args json.RawMessage
	at   time.Time
}

func classifyOAuth(primary, stderr string) bool {
	primary, stderr = strings.ToLower(primary), strings.ToLower(stderr)
	for _, hint := range oauthFailureHints {
		if strings.Contains(primary, hint) || strings.Contains(stderr, hint) {
			return true
		}
	}
	for _, hint := range primaryOnlyOAuthHints {
		if strings.Contains(primary, hint) {
			return true
		}
	}
	return false
}

func Start(ctx context.Context, o Options) (*Session, error) {
	if o.Cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			o.Cwd = wd
		}
	}
	if o.PermissionProfile == "" {
		mode := os.Getenv("ICEBATH_TERMINAL_SECURITY_MODE")
		if mode == "" {
			mode = "auto"
		}
		o.PermissionProfile = "workspace-write"
		if profile, ok := permissionProfiles[mode]; ok {
			o.PermissionProfile = profile
		}
	}
	if o.Version == "" {
		o.Version = "0.0.0"
	}
	o.Model, o.ModelProvider = strings.TrimSpace(o.Model), strings.TrimSpace(o.ModelProvider)
	c, err := startClient(o.Bin, o.Home, o.ExtraArgs, o.Env)
	if err != nil {
		return nil, err
	}
	if err := c.initialize(ctx, "icebath", "icebath", o.Version); err != nil {
		c.close()
		return nil, err
	}
	s := &Session{o: o, client: c, resume: o.ResumeThreadID, tierKnown: true, interrupt: make(chan struct{}, 1), fileEdits: map[string]string{}, started: map[string]startedItem{}}
	if err := s.ensureStarted(ctx); err != nil {
		var resumeErr *ResumeError
		if errors.As(err, &resumeErr) {
			return s, err
		}
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func extractThreadID(result wire.Value) string {
	thread := result.Get("thread")
	for _, v := range []wire.Value{thread.Get("id"), thread.Get("sessionId"), result.Get("sessionId"), result.Get("threadId")} {
		if v.Truthy() {
			return v.Display()
		}
	}
	return ""
}

func (s *Session) ensureStarted(ctx context.Context) error {
	s.mu.Lock()
	c, threadID := s.client, s.threadID
	s.mu.Unlock()
	if threadID != "" {
		return nil
	}
	if c == nil {
		return lostError("codex app-server session is closed")
	}
	params := (&wire.Dict{}).SetString("cwd", s.o.Cwd).SetString("personality", "none")
	instructions := ""
	if strings.TrimSpace(s.o.DeveloperInstructions) != "" {
		instructions = s.o.DeveloperInstructions
		params.SetString("developerInstructions", instructions)
	}
	if s.o.ModelProvider != "" {
		params.SetString("modelProvider", s.o.ModelProvider)
	}
	if s.o.Model != "" {
		params.SetString("model", s.o.Model)
	}
	if wanted := s.resume; wanted != "" {
		s.resume = ""
		resumeParams := (&wire.Dict{}).SetString("threadId", wanted)
		for _, m := range params.Members() {
			resumeParams.Set(m.Key, m.Value)
		}
		result, err := c.request(ctx, "thread/resume", resumeParams.JSON(), 15*time.Second)
		if err != nil {
			var rpcErr *RPCError
			if errors.As(err, &rpcErr) {
				return &ResumeError{ThreadID: wanted, Detail: rpcErr.Message}
			}
			return err
		}
		if got := extractThreadID(result); got != wanted {
			return &ResumeError{ThreadID: wanted, Detail: fmt.Sprintf("app-server answered with thread %q", truncate(got, 8))}
		}
		s.mu.Lock()
		s.threadID, s.tierKnown = wanted, false
		s.mu.Unlock()
		slog.Info("codex app-server thread resumed", "id", truncate(wanted, 8), "cwd", s.o.Cwd)
		return nil
	}
	if s.o.HistorySeed != "" {
		seeded := s.o.HistorySeed
		if instructions != "" {
			seeded = instructions + "\n\n" + s.o.HistorySeed
		}
		params.SetString("developerInstructions", seeded)
	}
	result, err := c.request(ctx, "thread/start", params.JSON(), 15*time.Second)
	if err != nil {
		return err
	}
	id := extractThreadID(result)
	if id == "" {
		keys := make([]string, 0, len(result.Fields()))
		for _, f := range result.Fields() {
			keys = append(keys, f.Key)
		}
		sort.Strings(keys)
		return &RPCError{Code: -32603, Message: fmt.Sprintf("codex thread/start returned no thread id (payload keys: %v)", keys)}
	}
	s.mu.Lock()
	s.threadID, s.tierSent, s.tierKnown = id, "", true
	s.mu.Unlock()
	slog.Info("codex app-server thread started", "id", truncate(id, 8), "profile", s.o.PermissionProfile, "cwd", s.o.Cwd)
	return nil
}

func (s *Session) ThreadID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.threadID
}

func (s *Session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	c := s.client
	s.client, s.threadID, s.activeTurn = nil, "", ""
	s.mu.Unlock()
	if c != nil {
		c.close()
	}
	return nil
}

func (s *Session) Interrupt() {
	select {
	case s.interrupt <- struct{}{}:
	default:
	}
}

func (s *Session) interrupted() bool { return len(s.interrupt) > 0 }

func (s *Session) clearInterrupt() {
	select {
	case <-s.interrupt:
	default:
	}
}

func (s *Session) Steer(text string) bool {
	cleaned := strings.TrimSpace(text)
	if cleaned == "" {
		return false
	}
	s.mu.Lock()
	turnID, threadID, c := s.activeTurn, s.threadID, s.client
	s.mu.Unlock()
	if turnID == "" || threadID == "" || c == nil {
		return false
	}
	item := (&wire.Dict{}).SetString("type", "text").SetString("text", cleaned)
	params := (&wire.Dict{}).SetString("threadId", threadID).Set("input", wire.Array(item.JSON())).SetString("expectedTurnId", turnID)
	resp, err := c.request(context.Background(), "turn/steer", params.JSON(), 10*time.Second)
	if err != nil {
		slog.Debug("turn/steer rejected for the active codex turn", "error", err)
		return false
	}
	accepted := resp.Get("turnId")
	return !accepted.Exists() || accepted.Kind() == wire.KindNull || accepted.Text() == turnID
}

func (s *Session) stderrBlob(c *client, n int) string {
	if c == nil {
		return ""
	}
	return strings.Join(c.stderrTail(n), "\n")
}

func (s *Session) withStderr(c *client, prefix, detail string, lines int) string {
	base := prefix
	if detail != "" {
		base = prefix + ": " + detail
	}
	if c == nil {
		return base
	}
	tail := c.stderrTail(lines)
	var kept []string
	for _, line := range tail {
		if line != "" {
			kept = append(kept, strings.TrimRight(line, " \t\r\n"))
		}
	}
	joined := strings.Join(kept, "\n")
	if strings.TrimSpace(joined) == "" {
		return base
	}
	return fmt.Sprintf("%s\ncodex stderr (last %d lines):\n%s", base, len(tail), wire.MaskCredentials(joined))
}

func retire(r *TurnResult, message string, reauth bool) {
	r.Err = &TurnError{Message: message, Reauth: reauth}
	r.Retire = true
}

func (s *Session) classified(r *TurnResult, c *client, prefix, classify, detail string) {
	if classifyOAuth(classify, s.stderrBlob(c, 40)) {
		retire(r, reauthHint, true)
		return
	}
	r.Err = &TurnError{Message: s.withStderr(c, prefix, detail, stderrTailLines)}
}

func (s *Session) startFor(ctx context.Context, r *TurnResult) (*client, bool) {
	s.mu.Lock()
	c := s.client
	s.mu.Unlock()
	if err := s.ensureStarted(ctx); err != nil {
		retire(r, s.withStderr(c, "codex app-server startup failed", err.Error(), stderrTailLines), false)
		return nil, false
	}
	r.ThreadID = s.ThreadID()
	return c, true
}

func (s *Session) requestFor(ctx context.Context, r *TurnResult, c *client, method string, params json.RawMessage) (wire.Value, bool) {
	resp, err := c.request(ctx, method, params, 10*time.Second)
	if err == nil {
		return resp, true
	}
	var rpcErr *RPCError
	switch {
	case errors.Is(err, ErrTransportLost):
		retire(r, s.withStderr(c, method+" failed", err.Error(), stderrTailLines), false)
	case errors.As(err, &rpcErr):
		s.classified(r, c, method+" failed", rpcErr.Message, rpcErr.Error())
	case errors.Is(err, ErrRequestTimeout):
		if classifyOAuth("", s.stderrBlob(c, 40)) {
			retire(r, reauthHint, true)
		} else {
			retire(r, s.withStderr(c, method+" timed out", err.Error(), stderrTailLines), false)
		}
	default:
		r.Interrupted = true
		r.Err = err
	}
	return wire.Value{}, false
}

func (s *Session) subprocessDied(r *TurnResult, c *client) bool {
	s.mu.Lock()
	gone := s.closed || s.client != c
	s.mu.Unlock()
	if c == nil || gone {
		r.Interrupted = true
		retire(r, "codex app-server session closed while the turn was in flight", false)
		return true
	}
	if c.alive() {
		return false
	}
	if classifyOAuth("", s.stderrBlob(c, 60)) {
		retire(r, reauthHint, true)
	} else {
		retire(r, s.withStderr(c, "codex app-server subprocess exited unexpectedly", "", 20), false)
	}
	return true
}

func scopeID(lookups ...[3]any) (string, bool) {
	for _, l := range lookups {
		src, _ := l[0].(wire.Value)
		if !src.IsObject() {
			continue
		}
		observed := src.Get(l[1].(string))
		if !observed.Truthy() {
			observed = src.Get(l[2].(string))
		}
		if observed.Exists() && observed.Kind() != wire.KindNull {
			return observed.Display(), true
		}
	}
	return "", false
}

func noteScope(note wire.Value) (thread string, hasThread bool, turn string, hasTurn bool) {
	params := note.Get("params")
	if !params.IsObject() {
		return "", false, "", false
	}
	turnObj, item := params.Get("turn"), params.Get("item")
	thread, hasThread = scopeID([3]any{params, "threadId", "thread_id"}, [3]any{turnObj, "threadId", "thread_id"}, [3]any{item, "threadId", "thread_id"})
	turn, hasTurn = scopeID([3]any{params, "turnId", "turn_id"}, [3]any{turnObj, "id", "turnId"}, [3]any{item, "turnId", "turn_id"})
	return thread, hasThread, turn, hasTurn
}

func belongsToTurn(note wire.Value, threadID, turnID string) bool {
	if !note.IsObject() {
		return false
	}
	thread, hasThread, turn, hasTurn := noteScope(note)
	if threadID != "" && hasThread && thread != threadID {
		return false
	}
	return !(turnID != "" && hasTurn && turn != turnID)
}

func formatTurnError(fault wire.Value, status string) string {
	field := func(name string) string {
		v := fault.Get(name)
		if v.IsString() || v.Truthy() {
			return strings.TrimSpace(v.Display())
		}
		return ""
	}
	code, message := field("code"), field("message")
	switch {
	case code != "" && message != "":
		return code + ": " + message
	case message != "":
		return message
	case code != "":
		return code
	case fault.Truthy():
		return fault.Display()
	}
	return "Responses API returned status '" + status + "'"
}

func hasAbortMarker(text string) bool {
	return strings.Contains(text, "<turn_aborted>") || strings.Contains(text, "<turn_aborted/>")
}

func summarizeFileChanges(changes wire.Value) string {
	if len(changes.Elems()) == 0 {
		return "1 change pending"
	}
	kinds := map[string]int{}
	var paths []string
	for _, ch := range changes.Elems() {
		if !ch.IsObject() {
			continue
		}
		kind := ch.Path("kind", "type").Text()
		if kind == "" {
			kind = "update"
		}
		kinds[kind]++
		if path := ch.Get("path"); path.Truthy() {
			paths = append(paths, path.Display())
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
	preview := strings.Join(paths[:min(3, len(paths))], ", ")
	if len(paths) > 3 {
		preview += fmt.Sprintf(", +%d more", len(paths)-3)
	}
	if preview == "" {
		return strings.Join(counts, ", ")
	}
	return strings.Join(counts, ", ") + ": " + preview
}

func (s *Session) accounting(r *TurnResult, note wire.Value) {
	method, params := note.Get("method").Text(), note.Get("params")
	if !params.IsObject() {
		return
	}
	if method == "thread/tokenUsage/updated" {
		usage := params.Get("tokenUsage")
		if last := usage.Get("last"); last.IsObject() {
			count := func(key string) int {
				n, _ := last.Get(key).Int()
				return int(n)
			}
			cached := count("cachedInputTokens")
			r.UsageRaw = last.Raw()
			r.Usage = &wire.Usage{InputTokens: max(0, count("inputTokens")-cached), CacheReadTokens: cached, OutputTokens: count("outputTokens"),
				ReasoningTokens: count("reasoningOutputTokens"), Requests: 1}
		}
		if window, ok := usage.Get("modelContextWindow").Int(); ok && window > 0 {
			r.ContextWindow = int(window)
		}
		return
	}
	item := wire.Value{}
	if method == "item/started" || method == "item/completed" {
		item = params.Get("item")
	}
	if method == "thread/compacted" || (item.IsObject() && item.Get("type").Text() == "contextCompaction") {
		r.Compacted = true
		if v := params.Get("threadId"); v.Truthy() {
			r.ThreadID = v.Display()
		}
		if v := params.Get("turnId"); v.Truthy() {
			r.TurnID = v.Display()
		}
	}
}

func (s *Session) trackFileChange(note wire.Value) {
	item := note.Path("params", "item")
	id := item.Get("id").Text()
	if item.Get("type").Text() != "fileChange" || id == "" {
		return
	}
	switch note.Get("method").Text() {
	case "item/completed":
		delete(s.fileEdits, id)
	case "item/started":
		s.fileEdits[id] = summarizeFileChanges(item.Get("changes"))
	}
}

func (s *Session) absorb(r *TurnResult, p *projector, note wire.Value, sink wire.Sink) (projection, bool) {
	s.bridge(note, sink)
	s.accounting(r, note)
	s.trackFileChange(note)
	proj := p.project(note)
	r.Projected = append(r.Projected, proj.messages...)
	if proj.toolIteration {
		r.ToolIterations++
	}
	aborted := false
	if proj.hasFinal {
		r.FinalText = proj.finalText
		if aborted = hasAbortMarker(proj.finalText); aborted {
			r.Interrupted = true
			if r.Err == nil {
				r.Err = &TurnError{Message: "codex reported turn_aborted"}
			}
		}
	}
	return proj, aborted
}

func (s *Session) issueInterrupt(c *client, turnID string) {
	threadID := s.ThreadID()
	if c == nil || threadID == "" || turnID == "" {
		return
	}
	params := (&wire.Dict{}).SetString("threadId", threadID).SetString("turnId", turnID)
	if _, err := c.request(context.Background(), "turn/interrupt", params.JSON(), 5*time.Second); err != nil {
		if errors.Is(err, ErrRequestTimeout) {
			slog.Warn("turn/interrupt timed out")
		} else {
			slog.Debug("turn/interrupt was not needed", "error", err)
		}
	}
}

func (s *Session) approve(ctx context.Context, auto bool, prompt func() (string, string)) string {
	if auto {
		return "accept"
	}
	if s.o.Approve == nil {
		return "decline"
	}
	command, description := prompt()
	switch s.o.Approve(ctx, command, description) {
	case "once":
		return "accept"
	case "session", "always":
		return "acceptForSession"
	}
	return "decline"
}

func (s *Session) handleServerRequest(ctx context.Context, c *client, req wire.Value) error {
	method, params := req.Get("method").Text(), req.Get("params")
	id := req.Get("id").Raw()
	decision := func(value string) json.RawMessage { return (&wire.Dict{}).SetString("decision", value).JSON() }
	switch method {
	case "item/commandExecution/requestApproval":
		return c.respond(id, decision(s.approve(ctx, s.o.AutoApproveExec, func() (string, string) {
			cwd := params.Get("cwd").Text()
			if cwd == "" {
				cwd = s.o.Cwd
			}
			if cwd == "" {
				cwd = "<unknown>"
			}
			description := "Codex requests exec in " + cwd
			if reason := params.Get("reason"); reason.Truthy() {
				description += " — " + reason.Display()
			}
			return params.Get("command").Display(), description
		})))
	case "item/fileChange/requestApproval":
		return c.respond(id, decision(s.approve(ctx, s.o.AutoApprovePatch, func() (string, string) {
			reason, grantRoot := params.Get("reason").Text(), params.Get("grantRoot").Text()
			summary := s.fileEdits[params.Get("itemId").Text()]
			var parts []string
			for _, part := range []string{reason, summary} {
				if part != "" {
					parts = append(parts, part)
				}
			}
			if grantRoot != "" {
				parts = append(parts, "grants write to "+grantRoot)
			}
			detail := summary
			if detail == "" {
				detail = reason
			}
			command, description := "apply_patch", "Codex requests to apply a patch"
			if detail != "" {
				command = "apply_patch: " + detail
			}
			if len(parts) > 0 {
				description = strings.Join(parts, "; ")
			}
			return command, description
		})))
	case "item/permissions/requestApproval":
		return c.respond(id, decision("decline"))
	case "mcpServer/elicitation/request":
		action := "decline"
		if s.o.ToolsServerName != "" && params.Get("serverName").Text() == s.o.ToolsServerName {
			action = "accept"
		}
		return c.respond(id, (&wire.Dict{}).SetString("action", action).Set("content", json.RawMessage("null")).Set("_meta", json.RawMessage("null")).JSON())
	}
	slog.Warn("unknown codex server request", "method", method)
	return c.respondError(id, -32601, "Unsupported method: "+method)
}

type driver struct {
	timeout         time.Duration
	label           string
	beforePoll      func()
	onServerRequest func(req wire.Value) (bool, error)
	onNote          func(note wire.Value, method string) bool
	preScope        func(note wire.Value, method string) bool
	acceptFinalText bool
}

func (s *Session) drive(ctx context.Context, r *TurnResult, c *client, d driver) {
	deadline := time.Now().Add(d.timeout)
	complete := false
	for time.Now().Before(deadline) && !complete {
		if s.interrupted() || ctx.Err() != nil {
			s.issueInterrupt(c, r.TurnID)
			r.Interrupted = true
			break
		}
		if s.subprocessDied(r, c) {
			break
		}
		if d.beforePoll != nil {
			d.beforePoll()
		}
		if req, ok := c.requests.take(ctx, 0); ok {
			done, err := d.onServerRequest(req)
			if err != nil {
				retire(r, s.withStderr(c, "codex app-server request response failed", err.Error(), stderrTailLines), false)
				break
			}
			complete = done
			continue
		}
		note, ok := c.notes.take(ctx, pollInterval)
		if !ok {
			continue
		}
		method := note.Get("method").Text()
		if d.preScope != nil && !d.preScope(note, method) {
			continue
		}
		if !belongsToTurn(note, s.ThreadID(), r.TurnID) {
			slog.Debug("ignoring foreign codex notification", "method", method)
			continue
		}
		complete = d.onNote(note, method)
	}
	if d.acceptFinalText && !complete && !r.Interrupted && r.FinalText != "" && r.Err == nil {
		slog.Warn("codex app-server turn reached its deadline after a completed assistant message but before turn/completed, accepting the assistant text as the terminal response")
		complete = true
	}
	if !complete && !r.Interrupted {
		s.issueInterrupt(c, r.TurnID)
		r.Interrupted = true
		if r.Err == nil {
			r.Err = &TurnError{Message: s.withStderr(c, fmt.Sprintf("%s timed out after %s", d.label, d.timeout), "", stderrTailLines)}
		}
		r.Retire = true
	}
}

func (s *Session) RunTurn(ctx context.Context, in TurnInput, sink wire.Sink) (*TurnResult, error) {
	r := &TurnResult{}
	defer s.clearInterrupt()
	c, ok := s.startFor(ctx, r)
	if !ok {
		return r, r.Err
	}
	if s.interrupted() {
		r.Interrupted = true
		return r, nil
	}
	items, submitted := buildTurnInput(&in.Message)
	r.SubmittedText = submitted
	params := (&wire.Dict{}).SetString("threadId", r.ThreadID).Set("input", wire.Array(items...))
	if in.Model != "" {
		params.SetString("model", in.Model)
	}
	if in.Effort != "" {
		params.SetString("effort", in.Effort)
	}
	s.mu.Lock()
	sendTier := !s.tierKnown || s.tierSent != in.ServiceTier
	s.mu.Unlock()
	if sendTier {
		if in.ServiceTier == "" {
			params.Set("serviceTier", json.RawMessage("null"))
		} else {
			params.SetString("serviceTier", in.ServiceTier)
		}
	}
	ts, ok := s.requestFor(ctx, r, c, "turn/start", params.JSON())
	if !ok {
		return r, r.Err
	}
	s.mu.Lock()
	s.tierSent, s.tierKnown = in.ServiceTier, true
	r.TurnID = ts.Path("turn", "id").Text()
	s.activeTurn = r.TurnID
	s.mu.Unlock()
	timeout, quiet := in.Timeout, in.QuietTimeout
	if timeout <= 0 {
		timeout = DefaultTurnTimeout
	}
	if quiet <= 0 {
		quiet = DefaultQuietTimeout
	}
	p := &projector{}
	var lastTool time.Time
	s.drive(ctx, r, c, driver{
		timeout: timeout, label: "turn", acceptFinalText: true,
		beforePoll: func() {
			if !lastTool.IsZero() && time.Since(lastTool) > quiet {
				lastTool = time.Time{}
				slog.Warn("codex has emitted no events after a tool result, still waiting", "quiet", quiet.String(), "deadline", timeout.String())
			}
		},
		onServerRequest: func(req wire.Value) (bool, error) {
			complete := false
			for i := 0; i < 8; i++ {
				pending, ok := c.notes.take(ctx, 0)
				if !ok {
					break
				}
				if !belongsToTurn(pending, r.ThreadID, r.TurnID) {
					continue
				}
				proj, aborted := s.absorb(r, p, pending, sink)
				if proj.toolIteration {
					lastTool = time.Now()
				}
				complete = complete || aborted
			}
			err := s.handleServerRequest(ctx, c, req)
			lastTool = time.Time{}
			return complete, err
		},
		onNote: func(note wire.Value, method string) bool {
			proj, aborted := s.absorb(r, p, note, sink)
			if proj.toolIteration {
				lastTool = time.Now()
			} else if len(proj.messages) > 0 || proj.hasFinal {
				lastTool = time.Time{}
			}
			if method != "turn/completed" {
				return aborted
			}
			turn := note.Path("params", "turn")
			status := turn.Get("status").Text()
			if status != "" && status != "completed" && status != "interrupted" && turn.Get("error").Truthy() {
				message := formatTurnError(turn.Get("error"), status)
				s.classified(r, c, "turn ended status="+status, message, message)
			}
			return true
		},
	})
	s.mu.Lock()
	s.activeTurn = ""
	s.mu.Unlock()
	return r, r.Err
}

func (s *Session) Compact(ctx context.Context) (*TurnResult, error) {
	r := &TurnResult{}
	c, ok := s.startFor(ctx, r)
	if !ok {
		return r, r.Err
	}
	s.clearInterrupt()
	p := &projector{}
	threadID := r.ThreadID
	if _, ok := s.requestFor(ctx, r, c, "thread/compact/start", (&wire.Dict{}).SetString("threadId", threadID).JSON()); !ok {
		return r, r.Err
	}
	s.drive(ctx, r, c, driver{
		timeout: DefaultTurnTimeout, label: "compact turn",
		onServerRequest: func(req wire.Value) (bool, error) { return false, s.handleServerRequest(ctx, c, req) },
		preScope: func(note wire.Value, method string) bool {
			if r.TurnID != "" {
				return true
			}
			thread, hasThread, turn, hasTurn := noteScope(note)
			if method == "turn/started" {
				if hasThread && thread != threadID {
					return false
				}
				if !hasTurn {
					return false
				}
				r.TurnID = turn
				return true
			}
			return !(hasTurn || method == "item/completed" || method == "turn/completed")
		},
		onNote: func(note wire.Value, method string) bool {
			_, aborted := s.absorb(r, p, note, nil)
			if method != "turn/started" && method != "turn/completed" {
				return aborted
			}
			turn := note.Path("params", "turn")
			if id := turn.Get("id"); id.Truthy() {
				r.TurnID = id.Display()
			}
			if method == "turn/started" {
				return aborted
			}
			switch status := turn.Get("status").Text(); {
			case status == "interrupted":
				r.Interrupted = true
				if r.Err == nil {
					r.Err = &TurnError{Message: "compact turn interrupted"}
				}
			case status != "" && status != "completed":
				message := formatTurnError(turn.Get("error"), status)
				s.classified(r, c, "compact turn ended status="+status, message, message)
			}
			return true
		},
	})
	return r, r.Err
}
