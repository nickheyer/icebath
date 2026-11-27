package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/internal/mcp"
	mcpconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/mcp/config"
	"github.com/nickheyer/icebath/internal/tools"
)

const (
	breakerThreshold = 3
	breakerCooldown  = 60 * time.Second
	// Longest a call waits for a server reconnecting, and for a stdio child started again
	recoveryWait = 15 * time.Second
	// Characters of the arguments an approval shows
	argumentsShown = 1200
)

// The stdio child was gone when the call was sent, or went while it ran
type exitedError struct {
	msg      string
	inFlight bool
	cause    error
}

func (e *exitedError) Error() string { return e.msg }

func (e *exitedError) Unwrap() error { return e.cause }

// A call a deliberate reconnect cut off
type tornError struct{ msg string }

func (e *tornError) Error() string { return e.msg }

// Error result with its message first and the extra members after it
func failure(message string, extra ...any) string {
	payload := map[string]any{"error": message}
	for i := 0; i+1 < len(extra); i += 2 {
		payload[extra[i].(string)] = extra[i+1]
	}
	return tools.JSON(payload)
}

// Seconds one call of a server's tool may run: its own timeout, else the setting
func (p *pool) timeout(s *spec) time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	if f, ok := seconds(p.cfg.Get(mcpconfig.ToolTimeout)); ok && f > 0 {
		return duration(f)
	}
	return 300 * time.Second
}

// Calls one tool of a server: through the trust gate and the spend limit, past the circuit breaker, recovering a dead stdio child, a lapsed
// sign in or an expired session once
func (p *pool) call(ctx context.Context, call *tools.Call, server string, tool *mcp.Tool) string {
	p.mu.Lock()
	srv := p.servers[server]
	p.mu.Unlock()
	if srv == nil {
		return failure(fmt.Sprintf("MCP server '%s' is not connected", server))
	}
	s := srv.currentSpec()
	var booked []*charge
	if refusal := p.gate(ctx, call, s, tool, &booked); refusal != "" {
		return refusal
	}
	refund := func() {
		for _, c := range booked {
			c.refund()
		}
	}
	if refusal := srv.tripped(); refusal != "" {
		refund()
		return refusal
	}
	timeout := p.timeout(s)
	live, refusal := p.acquire(ctx, server, timeout)
	if live == nil {
		refund()
		return refusal
	}
	op := "tools/call " + tool.Name
	r := &rendering{pool: p, server: server}
	once := func() (string, error) {
		return p.invoke(ctx, call, live, tool, r, timeout, op)
	}
	text := p.dispatch(ctx, live, op, once, tool.ReadOnly(), true)
	if !tools.Failed(text) {
		call.Media = append(call.Media, r.images...)
	}
	return text
}

// One attempt of a call under the server's call lock, the session's elicitation and sampling going to the calling session meanwhile
func (p *pool) invoke(ctx context.Context, call *tools.Call, srv *server, tool *mcp.Tool, r *rendering, timeout time.Duration, op string) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := srv.lock(callCtx); err != nil {
		return "", err
	}
	defer srv.unlock()
	untrack := srv.track(cancel)
	defer untrack()
	srv.called()
	srv.mu.Lock()
	srv.pending, srv.pendingCtx = call.Env, ctx
	session, stdio := srv.session, srv.stdio
	srv.mu.Unlock()
	defer func() {
		srv.mu.Lock()
		srv.pending, srv.pendingCtx = nil, nil
		srv.mu.Unlock()
	}()
	if stdio != nil && !stdio.Alive() {
		return "", &exitedError{msg: fmt.Sprintf("MCP stdio subprocess for '%s' had already exited when the call was dispatched", srv.name)}
	}
	if session == nil {
		return "", fmt.Errorf("MCP server '%s' has no active session", srv.name)
	}
	var progress mcp.ProgressFunc
	if call.Progress != nil {
		progress = mcp.ProgressFunc(call.Progress)
	}
	result, err := session.CallTool(callCtx, tool.Name, call.Args, progress)
	if err != nil {
		switch {
		case stdio != nil && (!stdio.Alive() || mcp.SessionExpired(err)):
			return "", &exitedError{msg: fmt.Sprintf("MCP stdio subprocess for '%s' exited mid-call", srv.name), inFlight: true, cause: err}
		case errors.Is(err, context.Canceled) && ctx.Err() == nil && srv.reconnecting():
			advice := "the request may already have been dispatched, so verify its effect before re-invoking"
			if tool.ReadOnly() {
				advice = "retry the request on the rebuilt session"
			}
			return "", &tornError{msg: fmt.Sprintf("MCP %s on '%s' was aborted by a reconnect teardown; %s", op, srv.name, advice)}
		case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
			return "", fmt.Errorf("MCP tool '%s' on '%s' timed out after %s", tool.Name, srv.name, timeout)
		}
		return "", err
	}
	srv.prove()
	return r.result(ctx, call.Env, result), nil
}

const (
	reauthMessage = "MCP server '%[1]s' requires re-authentication.%[2]s Run `icebath mcp login %[1]s`, or sign in from /mcp (or delete its files under the " +
		"profile's mcp-tokens folder and reload). Do NOT retry this tool — ask the user to re-authenticate."
	noRespawnMessage = "MCP server '%s' stdio subprocess had exited (this is not a timeout — the call never reached the server). A respawn was requested " +
		"but no fresh session came back within %.0fs. Wait a few seconds before retrying; if it keeps failing the server is not starting and needs the user."
	diedAgainMessage = "MCP server '%s' respawned its stdio subprocess and it exited again immediately. The server is not starting cleanly — do NOT retry " +
		"this tool; ask the user to check the server's command and its stderr log."
	uncertainStdioMessage = "MCP server '%s' lost its stdio subprocess after the tool call began. The operation may have completed, so icebath did not " +
		"replay it. Do NOT retry automatically; inspect the external state first."
	uncertainSessionMessage = "The MCP transport session to '%s' expired while this write-capable call was in flight, so the outcome is UNKNOWN — the " +
		"operation may or may not have taken effect server-side. It was NOT automatically retried to avoid a duplicate side effect. The connection has %s. " +
		"Verify whether the operation took effect (e.g. with a read-only tool) before re-invoking it."
)

// Runs the call, then on failure the recovery of its kind once: a dead stdio child started again, a session expired reconnected, a sign in
// that lapsed; every other failure strikes the breaker
func (p *pool) dispatch(ctx context.Context, srv *server, op string, once func() (string, error), readOnly, record bool) string {
	text, err := once()
	if err == nil {
		if record {
			srv.outcome(text)
		}
		return text
	}
	if ctx.Err() != nil {
		return failure(mcp.Redact("MCP call failed: " + err.Error()))
	}
	if exited, ok := errors.AsType[*exitedError](err); ok {
		return p.respawn(ctx, srv, exited, once, op)
	}
	if mcp.AuthFailure(err) {
		detail := ""
		if registration, ok := errors.AsType[*mcp.OAuthRegistrationError](err); ok {
			detail = " " + mcp.Redact(registration.Error())
		}
		if p.reauthorized(ctx, srv) {
			if text, err := once(); err == nil {
				srv.outcome(text)
				return text
			}
		}
		srv.strike(false)
		return failure(fmt.Sprintf(reauthMessage, srv.name, detail), "needs_reauth", true, "server", srv.name)
	}
	if mcp.SessionExpired(err) {
		if !readOnly {
			state := "not recovered yet"
			if p.reconnect(ctx, srv) {
				srv.reset()
				state = "been re-established"
			} else {
				srv.strike(false)
			}
			slog.Warn("MCP write-capable call failed with an expired session after it may have been dispatched; not retrying it", "server", srv.name, "op", op)
			return failure(fmt.Sprintf(uncertainSessionMessage, srv.name, state), "outcome_uncertain", true, "server", srv.name)
		}
		slog.Info("MCP session expired; reconnecting and retrying once", "server", srv.name, "op", op, "error", err)
		if p.reconnect(ctx, srv) {
			if text, err := once(); err == nil {
				srv.outcome(text)
				return text
			} else {
				slog.Warn("MCP retry after a session reconnect failed", "server", srv.name, "op", op, "error", err)
			}
		}
	}
	srv.strike(false)
	slog.Error("MCP call failed", "server", srv.name, "op", op, "error", err)
	return failure(mcp.Redact("MCP call failed: " + err.Error()))
}

// Starts a dead stdio child again and sends the call once more, unless it had already reached the server
func (p *pool) respawn(ctx context.Context, srv *server, exited *exitedError, once func() (string, error), op string) string {
	action := "respawning and retrying once"
	if exited.inFlight {
		action = "reconnecting without replay"
	}
	slog.Info("MCP stdio subprocess found dead; "+action, "server", srv.name, "op", op, "detail", exited.msg)
	reconnected := p.reconnect(ctx, srv)
	if exited.inFlight {
		srv.strike(false)
		return failure(fmt.Sprintf(uncertainStdioMessage, srv.name), "outcome_uncertain", true)
	}
	if !reconnected {
		srv.strike(false)
		return failure(fmt.Sprintf(noRespawnMessage, srv.name, recoveryWait.Seconds()))
	}
	text, err := once()
	if err == nil {
		srv.outcome(text)
		return text
	}
	srv.strike(false)
	if again, ok := errors.AsType[*exitedError](err); ok {
		slog.Warn("MCP stdio subprocess exited again right after its respawn; not retrying further", "server", srv.name, "detail", again.msg)
		if again.inFlight {
			return failure(fmt.Sprintf(uncertainStdioMessage, srv.name), "outcome_uncertain", true)
		}
		return failure(fmt.Sprintf(diedAgainMessage, srv.name))
	}
	return failure(mcp.Redact(fmt.Sprintf("MCP call failed after respawning the stdio subprocess for '%s': %s", srv.name, err)))
}

// Asks the server to rebuild its session and waits for the new one
func (p *pool) reconnect(ctx context.Context, srv *server) bool {
	_, old, _ := srv.snapshot()
	if !srv.signal() {
		return false
	}
	deadline := time.Now().Add(recoveryWait)
	for time.Now().Before(deadline) {
		srv.mu.Lock()
		session, changed := srv.session, srv.changed
		srv.mu.Unlock()
		if session != nil && session != old {
			return true
		}
		select {
		case <-changed:
		case <-time.After(time.Until(deadline)):
		case <-ctx.Done():
			return false
		}
	}
	return false
}

// Whether the server's OAuth tokens changed on disk since its provider read them, its provider then read again and the session rebuilt
func (p *pool) reauthorized(ctx context.Context, srv *server) bool {
	srv.mu.Lock()
	provider := srv.provider
	srv.mu.Unlock()
	if provider == nil || !provider.HasTokens() {
		return false
	}
	p.forgetProvider(srv.name)
	return p.reconnect(ctx, srv)
}

// Breaker bookkeeping of a call's result: an error the server answered strikes as such, any other answer closes it
func (s *server) outcome(result string) {
	if tools.Failed(result) {
		s.strike(true)
		return
	}
	s.reset()
}

func (s *server) strike(application bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.breaker.failures++
	s.breaker.application = application && (s.breaker.failures == 1 || s.breaker.application)
	if s.breaker.failures >= breakerThreshold {
		s.breaker.opened = time.Now()
	}
}

func (s *server) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.breaker = breaker{}
}

// Error a call gets while the breaker is open, empty when calls may go, half open once the cooldown passed
func (s *server) tripped() string {
	s.mu.Lock()
	b := s.breaker
	s.mu.Unlock()
	if b.failures < breakerThreshold || b.opened.IsZero() || time.Since(b.opened) >= breakerCooldown {
		return ""
	}
	retry := max(1, int((breakerCooldown - time.Since(b.opened)).Seconds()))
	if b.application {
		return failure(fmt.Sprintf("MCP server '%s' rejected the last %d calls (it is reachable; see the error text those calls returned). Paused for ~%ds. "+
			"Do NOT repeat the same call — fix the arguments/URL/target or use a different approach.", s.name, b.failures, retry))
	}
	return failure(fmt.Sprintf("MCP server '%s' is unreachable after %d consecutive failures. Auto-retry available in ~%ds. Do NOT retry this tool yet — "+
		"use alternative approaches or ask the user to check the MCP server.", s.name, b.failures, retry))
}

// Server a call may go to, else the error it gets; a server down is asked to reconnect and strikes the breaker
func (p *pool) acquire(ctx context.Context, name string, timeout time.Duration) (*server, string) {
	srv, err := p.live(ctx, name, min(5*time.Second, timeout))
	if err == nil {
		return srv, ""
	}
	if srv == nil {
		return nil, failure(fmt.Sprintf("MCP server '%s' is not connected", name))
	}
	srv.strike(false)
	if state, _, _ := srv.snapshot(); state != stateParked && srv.signal() {
		return nil, failure(fmt.Sprintf("MCP server '%s' transport is down; reconnect requested. Do NOT retry this tool immediately — give it a few seconds "+
			"to come back.", name))
	}
	return nil, failure(fmt.Sprintf("MCP server '%s' is not connected", name) + ": " + describe(err))
}

// Approval of a write capable tool of an untrusted server and the server's spend limit; empty when the call may go, its charge booked
func (p *pool) gate(ctx context.Context, call *tools.Call, s *spec, tool *mcp.Tool, booked *[]*charge) string {
	gated := s.Trust == TrustUntrusted && !tool.ReadOnly()
	c := p.tallies.charge(s.Costs, s.Name, tool.Name, call.SessionID)
	if c == nil && !gated {
		return ""
	}
	if c != nil && !gated {
		if ok, _ := c.reserve(); ok {
			*booked = append(*booked, c)
			return ""
		}
	}
	var already float64
	over := false
	if c != nil {
		release := c.asking()
		defer release()
		if gated {
			already = c.spent()
			over = c.over(already)
		} else {
			var ok bool
			if ok, already = c.reserve(); ok {
				*booked = append(*booked, c)
				return ""
			}
			over = true
		}
	}
	var lines []string
	if c != nil {
		lines = c.lines(already)
	}
	if gated && !over {
		parts := append([]string{fmt.Sprintf("MCP tool '%s' on UNTRUSTED server '%s' wants to run.", tool.Name, s.Name)}, lines...)
		parts = append(parts, "This tool is write-capable (no readOnlyHint=true annotation) and may modify external state.")
		action := strings.Join(parts, " ")
		if len(call.Args) > 0 {
			action += "\n\nArguments:\n" + arguments(call.Args, call.RawArgs)
		}
		if call.Env == nil || call.Env.Approve == nil {
			return failure(fmt.Sprintf("MCP tool '%s' on untrusted server '%s' was blocked: the approval system was unavailable (fail-closed).", tool.Name, s.Name))
		}
		if err := call.Env.Approve(ctx, action, trustKey(s.Name, tool.Name)); err != nil {
			slog.Info("MCP trust gate: the user did not approve a write-capable tool of an untrusted server", "server", s.Name, "tool", tool.Name)
			return failure(fmt.Sprintf("The user did not approve running write-capable MCP tool '%s' on untrusted server '%s'. The command was NOT run. Do not "+
				"retry without explicit user direction.", tool.Name, s.Name))
		}
		if c != nil {
			c.book()
			*booked = append(*booked, c)
		}
		return ""
	}
	// Past the spend limit a call is approved once only, so every further paid call of the chat asks again
	var parts []string
	if gated {
		parts = append(parts, fmt.Sprintf("MCP tool '%s' on UNTRUSTED server '%s' wants to run.", tool.Name, s.Name), "This call goes past the chat's spend limit.")
		parts = append(parts, lines...)
		parts = append(parts, "This tool is write-capable (no readOnlyHint=true annotation) and may modify external state.")
	} else {
		parts = append([]string{fmt.Sprintf("MCP tool '%s' on server '%s' would take this chat past its spend limit.", tool.Name, s.Name)}, lines...)
	}
	question := strings.Join(parts, " ") + fmt.Sprintf(" Run '%s' once? Every further paid call in this chat asks again.", tool.Name)
	if len(call.Args) > 0 {
		question += "\n\nArguments:\n" + arguments(call.Args, call.RawArgs)
	}
	if call.Env == nil || call.Env.Clarify == nil {
		return failure(c.refusal(already))
	}
	reply, err := call.Env.Clarify(ctx, []tools.Question{{ID: "spend", Text: question, Choices: []string{"Run it once", "Don't run it"}}})
	if err == nil && reply.Outcome == tools.Submitted && len(reply.Answers["spend"]) > 0 && reply.Answers["spend"][0] == "Run it once" {
		c.book()
		*booked = append(*booked, c)
		return ""
	}
	slog.Info("MCP spend limit: the user did not approve a call past it", "server", s.Name, "tool", tool.Name)
	return failure(c.refusal(already))
}

// Names of arguments that hold credentials, whose values an approval masks
var secretArgument = []string{"password", "passwd", "secret", "token", "api_key", "apikey", "access_key", "private_key", "authorization", "credential",
	"client_secret", "x-api-key", "cookie", "session"}

func secretName(name string) bool {
	folded := strings.ToLower(name)
	for _, s := range secretArgument {
		if strings.Contains(folded, s) {
			return true
		}
	}
	return false
}

// Value with every credential named field at any depth masked
func mask(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			if item != nil && item != "" && secretName(k) {
				out[k] = "***"
			} else {
				out[k] = mask(item)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = mask(item)
		}
		return out
	}
	return v
}

// Arguments an approval shows: in the order the model wrote them, credentials masked, hidden characters made visible, cut to fit
func arguments(args map[string]any, raw string) string {
	order := mcp.Keys(json.RawMessage(raw))
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		a, b := index(order, keys[i]), index(order, keys[j])
		if a != b {
			return a < b
		}
		return keys[i] < keys[j]
	})
	masked := mask(args).(map[string]any)
	var b strings.Builder
	b.WriteString("{\n")
	for i, k := range keys {
		value, _ := json.MarshalIndent(masked[k], "  ", "  ")
		fmt.Fprintf(&b, "  %q: %s", k, value)
		if i < len(keys)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("}")
	text := mcp.Redact(tools.Visible(b.String()))
	if runes := []rune(text); len(runes) > argumentsShown {
		marker := "\n... [arguments truncated]"
		text = strings.TrimRightFunc(string(runes[:argumentsShown-len([]rune(marker))]), unicode.IsSpace) + marker
	}
	return text
}

func index(list []string, x string) int {
	for i, y := range list {
		if y == x {
			return i
		}
	}
	return len(list)
}
