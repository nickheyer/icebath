package dashboard

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	consolePrompt         = "icebath> "
	consoleCommandTimeout = 60 * time.Second
	consoleUnwindTimeout  = 10 * time.Second
	consoleOutputLimit    = 50000
)

// Outcome of one console line
type consoleLineResult struct {
	status, output, command, confirmation string
}

// Runs one console line inside the profile scope with the interrupt scope bound, both held only in the worker
func executeConsoleLine(ctx context.Context, execute func(ctx context.Context, line string, confirmed bool) (string, string, string, string, error),
	line string, confirmed bool, profile string, scope *agent.InterruptScope) (any, error) {
	sctx, _, release, err := profileScope(ctx, profile)
	if err != nil {
		return nil, err
	}
	defer release()
	status, output, command, confirmation, err := execute(agent.WithInterruptScope(sctx, scope), line, confirmed)
	if err != nil {
		return nil, err
	}
	return consoleLineResult{status, output, command, confirmation}, nil
}

// Stops the worker of a cancelled or timed out command and waits a bounded time for it to exit
func unwindConsoleWorker(worker *consoleFuture, scope *agent.InterruptScope, reason string) {
	if reason == "cancelled" {
		scope.Cancel("Console command "+reason, nil)
	} else {
		scope.CancelByHost("Console command " + reason)
	}
	if worker.Cancel() {
		return
	}
	select {
	case <-worker.Done():
	case <-time.After(consoleUnwindTimeout):
		routersLog().Warn(fmt.Sprintf("console worker still running %ss after %s", pyrepr.Float(consoleUnwindTimeout.Seconds()), reason))
	}
}

// Writes frames to one console socket one at a time and owns the prompt suffix
type consoleSender struct {
	ws *WebSocket
	mu sync.Mutex
}

func (s *consoleSender) send(ctx context.Context, payload *pyjson.Object) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ws.SendJSON(ctx, payload, false)
}

// Sends a frame of the given keys followed by the prompt
func (s *consoleSender) prompt(ctx context.Context, kv ...any) error {
	payload := pyjson.ObjectOf(kv...)
	payload.Set("prompt", consolePrompt)
	return s.send(ctx, payload)
}

// Sends an error frame with its keys in the order type, id, message, command, prompt
func (s *consoleSender) fail(ctx context.Context, message string, id *int, command, prompt *string) error {
	frame := pyjson.ObjectOf("type", "error")
	if id != nil {
		frame.Set("id", int64(*id))
	}
	frame.Set("message", message)
	if command != nil {
		frame.Set("command", *command)
	}
	if prompt != nil {
		frame.Set("prompt", *prompt)
	}
	return s.send(ctx, frame)
}

func (s *consoleSender) complete(ctx context.Context, status, command string, commandID int, prompt string) error {
	return s.send(ctx, pyjson.ObjectOf("type", "complete", "id", int64(commandID), "status", status, "command", command, "prompt", prompt))
}

func (s *consoleSender) failThenComplete(ctx context.Context, message, command string, commandID int, status string) error {
	if err := s.fail(ctx, message, &commandID, &command, nil); err != nil {
		return err
	}
	return s.complete(ctx, status, command, commandID, consolePrompt)
}

// Sends the frames a finished command produces for its status
func (s *consoleSender) sendResult(ctx context.Context, result consoleLineResult, commandID int) error {
	command := result.command
	switch result.status {
	case "ok":
		if result.output != "" {
			if err := s.send(ctx, pyjson.ObjectOf("type", "output", "id", int64(commandID), "stream", "stdout",
				"data", result.output, "command", command)); err != nil {
				return err
			}
		}
		return s.complete(ctx, "ok", command, commandID, consolePrompt)
	case "error":
		message := result.output
		if message == "" {
			message = "Command failed."
		}
		return s.failThenComplete(ctx, message, command, commandID, "error")
	case "confirm_required":
		message := result.confirmation
		if message == "" {
			message = "Run `" + command + "`?"
		}
		if err := s.prompt(ctx, "type", "confirm_required", "id", int64(commandID), "command", command, "message", message); err != nil {
			return err
		}
		return s.complete(ctx, "confirm_required", command, commandID, consolePrompt)
	case "clear":
		if err := s.send(ctx, pyjson.ObjectOf("type", "clear", "id", int64(commandID))); err != nil {
			return err
		}
		return s.complete(ctx, "clear", command, commandID, consolePrompt)
	case "exit":
		return s.complete(ctx, "exit", command, commandID, "")
	}
	return s.fail(ctx, "Unknown console result status: "+result.status, &commandID, &command, nil)
}

// JSON object of a console frame, an error text when it is no UTF-8 JSON object, neither when empty
func consoleJSONPayload(msg Message) (*pyjson.Object, string) {
	var raw string
	switch {
	case msg.Text != nil:
		raw = *msg.Text
	case msg.Bytes != nil:
		if !utf8.Valid(msg.Bytes) {
			return nil, "Console frames must be UTF-8 JSON."
		}
		raw = string(msg.Bytes)
	default:
		return nil, ""
	}
	parsed, err := pyjson.Loads(raw)
	if err != nil {
		return nil, "Console frames must be JSON objects."
	}
	payload, ok := parsed.(*pyjson.Object)
	if !ok {
		return nil, "Console frames must be JSON objects."
	}
	return payload, ""
}

// One running console command, cancel asked at most once and done closed when its goroutine ends
type consoleTask struct {
	cancel     chan struct{}
	done       chan struct{}
	cancelOnce sync.Once
	err        error
}

func (t *consoleTask) requestCancel() { t.cancelOnce.Do(func() { close(t.cancel) }) }

func (t *consoleTask) finished() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

// Command state of one console socket
type consoleSession struct {
	ctx        context.Context
	ws         *WebSocket
	out        *consoleSender
	execute    func(ctx context.Context, line string, confirmed bool) (string, string, string, string, error)
	profile    string
	mu         sync.Mutex
	active     *consoleTask
	pending    string
	generation int
}

// Whether a command of this socket is still running
func (s *consoleSession) running() *consoleTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil && !s.active.finished() {
		return s.active
	}
	return nil
}

// Runs one command on the console executor and reports its result unless a newer command replaced it
func (s *consoleSession) runCommand(task *consoleTask, line string, confirmed bool, commandID int) error {
	defer func() {
		s.mu.Lock()
		if commandID == s.generation {
			s.active = nil
		}
		s.mu.Unlock()
	}()
	current := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return commandID == s.generation
	}
	scope := agent.NewInterruptScope()
	workerCtx := context.WithoutCancel(s.ctx)
	worker := getConsoleExecutor().Submit(func() (any, error) {
		return executeConsoleLine(workerCtx, s.execute, line, confirmed, s.profile, scope)
	})
	timer := time.NewTimer(consoleCommandTimeout)
	defer timer.Stop()
	select {
	case <-task.cancel:
		unwindConsoleWorker(worker, scope, "cancelled")
		return nil
	case <-timer.C:
		unwindConsoleWorker(worker, scope, "timed out")
		if !current() {
			return nil
		}
		s.mu.Lock()
		s.pending = ""
		s.mu.Unlock()
		return s.out.failThenComplete(s.ctx, "Command timed out. Icebath Console returned to the prompt.", line, commandID, "timeout")
	case <-worker.Done():
	}
	value, err := worker.Result()
	if err != nil {
		if !current() {
			return nil
		}
		s.mu.Lock()
		s.pending = ""
		s.mu.Unlock()
		routersLog().Error("console command failed", "exc_info", err)
		message := pyExcStr(err)
		if message == "" {
			message = pyrepr.ExceptionName(err)
		}
		return s.out.failThenComplete(s.ctx, message, line, commandID, "error")
	}
	if !current() {
		return nil
	}
	result := value.(consoleLineResult)
	s.mu.Lock()
	s.pending = ""
	if result.status == "confirm_required" {
		s.pending = result.command
	}
	s.mu.Unlock()
	if err := s.out.sendResult(s.ctx, result, commandID); err != nil {
		return err
	}
	if result.status == "exit" {
		return s.ws.Close(s.ctx, 1000, "")
	}
	return nil
}

// Starts a command as the active one of a new generation
func (s *consoleSession) startCommand(line string, confirmed bool) {
	task := &consoleTask{cancel: make(chan struct{}), done: make(chan struct{})}
	s.mu.Lock()
	s.generation++
	commandID := s.generation
	s.active = task
	s.mu.Unlock()
	go func() {
		defer close(task.done)
		task.err = s.runCommand(task, line, confirmed, commandID)
	}()
}

// Handles one console frame, its error ending the socket
func (s *consoleSession) handleFrame(payload *pyjson.Object) error {
	ctx := s.ctx
	frameType := ""
	if v := payload.GetOr("type", nil); pyrepr.Truthy(v) {
		frameType = pystr.Lower(pystr.Strip(pyrepr.Str(v)))
	}
	if frameType == "ping" {
		return s.out.prompt(ctx, "type", "pong")
	}
	if frameType == "cancel" {
		s.mu.Lock()
		task := s.active
		pending := s.pending
		if task != nil && !task.finished() {
			s.generation++
			s.active = nil
			s.mu.Unlock()
			task.requestCancel()
			<-task.done
			if task.err != nil {
				return task.err
			}
			s.mu.Lock()
			s.pending = ""
			s.mu.Unlock()
			return s.out.prompt(ctx, "type", "complete", "status", "cancelled")
		}
		if pending != "" {
			s.pending = ""
			s.mu.Unlock()
			return s.out.prompt(ctx, "type", "complete", "status", "cancelled")
		}
		s.mu.Unlock()
		return s.out.prompt(ctx, "type", "complete", "status", "idle")
	}
	if s.running() != nil {
		return s.out.prompt(ctx, "type", "error", "message", "A console command is already running.")
	}
	s.mu.Lock()
	pending := s.pending
	s.mu.Unlock()
	text := func(keys ...string) string {
		for _, key := range keys {
			if v := payload.GetOr(key, nil); pyrepr.Truthy(v) {
				return pyrepr.Str(v)
			}
		}
		return ""
	}
	if frameType == "confirm" {
		command := text("command")
		if command == "" {
			command = pending
		}
		command = pystr.Strip(command)
		if pending == "" {
			return s.out.prompt(ctx, "type", "error", "message", "No command is waiting for confirmation.")
		}
		if command != pending {
			return s.out.prompt(ctx, "type", "error", "message", "Confirmation does not match the pending command.")
		}
		s.mu.Lock()
		s.pending = ""
		s.mu.Unlock()
		s.startCommand(command, true)
		return nil
	}
	if frameType == "input" || frameType == "command" {
		line := pystr.Strip(text("line", "command"))
		if line == "" {
			return s.out.prompt(ctx, "type", "complete", "status", "ok")
		}
		if pending != "" {
			return s.out.prompt(ctx, "type", "error", "message", "Confirm or cancel the pending command before running another one.")
		}
		s.startCommand(line, false)
		return nil
	}
	shown := frameType
	if shown == "" {
		shown = "?"
	}
	return s.out.prompt(ctx, "type", "error", "message", "Unsupported console frame: "+shown)
}

// Curated console of the dashboard overlay, JSON frames in and out with one command running at a time
func consoleWS(ctx context.Context, ws *WebSocket) error {
	peer, mode, cred, ok, err := wsGate(ctx, ws, "console")
	if err != nil || !ok {
		return err
	}
	if err := ws.Accept(ctx, nil, nil); err != nil {
		return err
	}
	profile := pystr.Strip(ws.QueryParams().Get("profile"))
	out := &consoleSender{ws: ws}
	empty := ""
	execute := ConsoleCLI.NewEngine(ctx, consoleOutputLimit)
	if profile != "" && pystr.Lower(profile) != "current" {
		if _, err := resolveProfileDir(profile); err != nil {
			if h, isHTTP := asHTTPException(err); isHTTP {
				detail := pyrepr.Str(h.Detail)
				if err := out.fail(ctx, detail, nil, nil, &empty); err != nil {
					return err
				}
				return ws.Close(ctx, 4400, wsCloseReason(detail))
			}
			routersLog().Error("console failed to initialize", "exc_info", err)
			if err := out.fail(ctx, "Console unavailable: "+err.Error(), nil, nil, &empty); err != nil {
				return err
			}
			return ws.Close(ctx, 1011, "")
		}
	}
	shown := profile
	if shown == "" {
		shown = "current"
	}
	routersLog().Info(fmt.Sprintf("console accepted peer=%s mode=%s cred=%s profile=%s", peer, mode, cred, shown))
	session := &consoleSession{ctx: ctx, ws: ws, out: out, execute: execute, profile: profile}
	defer func() {
		if task := session.running(); task != nil {
			task.requestCancel()
			<-task.done
		}
	}()
	var disconnect *WebSocketDisconnect
	if err := out.prompt(ctx, "type", "ready", "profile", shown); err != nil {
		return err
	}
	for {
		msg, err := ws.Receive(ctx)
		if err != nil {
			if wsReceiveRuntimeError(err) || errors.As(err, &disconnect) {
				return nil
			}
			return err
		}
		if msg.Type == "websocket.disconnect" {
			return nil
		}
		payload, problem := consoleJSONPayload(msg)
		if problem != "" {
			err = out.prompt(ctx, "type", "error", "message", problem)
		} else if payload != nil {
			err = session.handleFrame(payload)
		}
		if err != nil {
			if errors.As(err, &disconnect) {
				return nil
			}
			return err
		}
	}
}
