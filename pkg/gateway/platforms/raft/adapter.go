package raft

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"weak"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	defaultHost           = "127.0.0.1"
	defaultPort           = 0
	defaultPath           = "/wake"
	defaultRuntimeSession = "default"
	defaultMaxBodyBytes   = 16_384
	bridgeTokenHeader     = "x-raft-bridge-token"
	wakePrompt            = "Raft wake hint received. New Raft messages may be pending. " +
		"If you have not read the Raft manual in this session, run " +
		"`raft manual get raft-cli-overview` before using Raft commands."
)

// Payload keys read in order for the id of a wake delivery
var wakeIDKeys = []string{"eventId", "attemptId", "messageId", "delivery_id", "wake_id", "id"}

// RAFT_PROFILE of the active profile, a multiplexed secondary reading only its own secret scope
func resolveRaftProfile(ctx context.Context) string {
	if gateway.ProfileScoped(ctx) {
		value, _, err := provider.GetSecret(ctx, "RAFT_PROFILE")
		if err != nil {
			return ""
		}
		return pystr.Strip(value)
	}
	return pystr.Strip(os.Getenv("RAFT_PROFILE"))
}

// Whether the raft CLI is on PATH, a silent probe run on every gateway config load
func checkRaftRequirements() bool {
	_, found := pyshutil.Which("raft", nil)
	return found
}

// JSON failure body with the given status
func errorResponse(reason string, status int) (*gateway.WebResponse, error) {
	return gateway.WebJSONResponse(pyjson.ObjectOf("ok", false, "error", reason), status, nil)
}

// One spawned bridge child with the channel closed once it was reaped
type bridgeProcess struct {
	cmd    *exec.Cmd
	exited chan struct{}
}

// Local HTTP endpoint the Raft channel bridge delivers content free wake hints to
type RaftAdapter struct {
	*gateway.BasePlatformAdapter

	host           string
	port           int
	path           string
	runtimeSession string
	maxBodyBytes   int
	activityQueue  *ActivityQueue

	mu          sync.Mutex
	bridgeToken string
	runner      *gateway.WebAppRunner
	bridge      *bridgeProcess
}

// Python int of an extra value, the TypeError or ValueError int raises otherwise
func extraInt(extra map[string]any, key string, def int) (int, error) {
	value, ok := extra[key]
	if !ok {
		return def, nil
	}
	n, err := pyint.Int64(value)
	if pe, isPy := err.(*pyint.Error); isPy {
		return 0, &gateway.PyError{Kind: pe.Kind, Msg: pe.Msg}
	}
	return int(n), err
}

// Adapter for a platform config reading the bind address, wake path, bridge token and body cap from extra
func NewRaftAdapter(ctx context.Context, config *gateway.PlatformConfig) (*RaftAdapter, error) {
	if config == nil {
		config = gateway.NewPlatformConfig()
	}
	a := &RaftAdapter{activityQueue: NewActivityQueue(defaultActivityQueueCap)}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, gateway.Platform("raft"), a)
	extra := config.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	a.host = defaultHost
	if v, ok := extra["host"]; ok {
		a.host = pyrepr.Str(v)
	}
	var err error
	if a.port, err = extraInt(extra, "port", defaultPort); err != nil {
		return nil, err
	}
	var path any = defaultPath
	if v, ok := extra["path"]; ok && pyrepr.Truthy(v) {
		path = v
	}
	a.path = pystr.Strip(pyrepr.Str(path))
	if a.path == "" {
		a.path = defaultPath
	}
	if !strings.HasPrefix(a.path, "/") {
		a.path = "/" + a.path
	}
	if token := extra["bridge_token"]; pyrepr.Truthy(token) {
		a.bridgeToken = pystr.Strip(pyrepr.Str(token))
	}
	a.runtimeSession = defaultRuntimeSession
	if v, ok := extra["runtime_session"]; ok && pyrepr.Truthy(v) {
		a.runtimeSession = pyrepr.Str(v)
	}
	if a.maxBodyBytes, err = extraInt(extra, "max_body_bytes", defaultMaxBodyBytes); err != nil {
		return nil, err
	}
	return a, nil
}

// Starts the wake endpoint on the configured address, spawns the bridge child and marks the adapter connected
func (a *RaftAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	a.mu.Lock()
	if a.bridgeToken == "" {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			a.mu.Unlock()
			return false, err
		}
		a.bridgeToken = hex.EncodeToString(raw)
		slog.Info("[raft] Auto-generated bridge token")
	}
	a.mu.Unlock()
	app := gateway.NewWebApplication(int64(a.maxBodyBytes))
	routes := []struct {
		method, path string
		handler      gateway.WebHandler
	}{{http.MethodGet, "/health", a.handleHealth}, {http.MethodPost, a.path, a.handleWake},
		{http.MethodPost, "/activity", a.handleActivity}, {http.MethodGet, "/activity/drain", a.handleActivityDrain}}
	for _, route := range routes {
		add := app.AddGet
		if route.method == http.MethodPost {
			add = app.AddPost
		}
		if err := add(route.path, route.handler); err != nil {
			return false, err
		}
	}
	if a.port != 0 {
		conn, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(a.port)), time.Second)
		if err == nil {
			conn.Close()
			slog.Error(fmt.Sprintf("[raft] Port %d already in use. Set platforms.raft.extra.port in config", a.port))
			return false, nil
		}
	}
	host := a.host
	runner, err := gateway.StartWebApp(ctx, app, &host, a.port, gateway.ListenOptions{})
	if err != nil {
		return false, err
	}
	a.mu.Lock()
	a.runner = runner
	a.mu.Unlock()
	boundPort := a.port
	if boundPort == 0 {
		boundPort = runner.Port()
	}
	a.MarkConnected(ctx, "")
	activeAdaptersMu.Lock()
	activeAdapters[weak.Make(a)] = struct{}{}
	activeAdaptersMu.Unlock()
	slog.Info(fmt.Sprintf("[raft] Raft channel listening on %s:%d%s", a.host, boundPort, a.path))
	if err := a.spawnBridge(ctx, boundPort); err != nil {
		return false, err
	}
	a.WirePluginHandlers(ctx, nil)
	return true, nil
}

// Stops the bridge, shuts the endpoint down and marks the adapter disconnected
func (a *RaftAdapter) Disconnect(ctx context.Context) error {
	a.stopBridge()
	a.mu.Lock()
	runner := a.runner
	a.runner = nil
	a.mu.Unlock()
	if runner != nil {
		if err := runner.Cleanup(ctx); err != nil {
			return err
		}
	}
	activeAdaptersMu.Lock()
	delete(activeAdapters, weak.Make(a))
	activeAdaptersMu.Unlock()
	a.MarkDisconnected(ctx)
	slog.Info("[raft] Disconnected")
	return nil
}

// Starts raft agent bridge as an owned child pointed at the wake endpoint, skipped without the CLI or a profile
func (a *RaftAdapter) spawnBridge(ctx context.Context, port int) error {
	raftBin, found := pyshutil.Which("raft", nil)
	if !found {
		slog.Warn("[raft] raft CLI not found in PATH; bridge not spawned — wake-only polling mode")
		return nil
	}
	profile := resolveRaftProfile(ctx)
	if profile == "" {
		slog.Warn("[raft] RAFT_PROFILE not set; bridge not spawned")
		return nil
	}
	endpoint := fmt.Sprintf("http://%s:%d%s", a.host, port, a.path)
	args := []string{"--profile", profile, "agent", "bridge", "--wake-adapter", "wake-channel", "--wake-channel-endpoint", endpoint}
	env := tools.IcebathSubprocessEnv(&tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)}, false, nil)
	a.mu.Lock()
	env["RAFT_PROFILE"], env["RAFT_CHANNEL_TOKEN"] = profile, a.bridgeToken
	a.mu.Unlock()
	realHome, ok := env["ICEBATH_REAL_HOME"]
	if !ok {
		return &gateway.PyError{Kind: "KeyError", Msg: "'ICEBATH_REAL_HOME'"}
	}
	env["HOME"] = realHome
	cmd := exec.Command(raftBin, args...)
	cmd.Env = make([]string, 0, len(env))
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := tools.SpawnOwned(ctx, cmd, "raft-bridge"); err != nil {
		slog.Error("[raft] Failed to spawn bridge", "error", err)
		return nil
	}
	proc := &bridgeProcess{cmd: cmd, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(proc.exited)
	}()
	a.mu.Lock()
	a.bridge = proc
	a.mu.Unlock()
	slog.Info(fmt.Sprintf("[raft] Spawned bridge pid=%d profile=%s endpoint=%s", cmd.Process.Pid, profile, endpoint))
	return nil
}

// Terminates the bridge child, killing it after five seconds, and releases its process group once it is gone
func (a *RaftAdapter) stopBridge() {
	a.mu.Lock()
	proc := a.bridge
	a.bridge = nil
	a.mu.Unlock()
	if proc == nil {
		return
	}
	pid := proc.cmd.Process.Pid
	var err error
	if runtime.GOOS == "windows" {
		err = proc.cmd.Process.Kill()
	} else {
		err = proc.cmd.Process.Signal(syscall.SIGTERM)
	}
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		slog.Error("[raft] Error stopping bridge", "error", err)
	} else {
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-proc.exited:
			timer.Stop()
			slog.Info(fmt.Sprintf("[raft] Bridge process terminated (pid=%d)", pid))
		case <-timer.C:
			_ = proc.cmd.Process.Kill()
			slog.Warn(fmt.Sprintf("[raft] Bridge process killed after timeout (pid=%d)", pid))
		}
	}
	select {
	case <-proc.exited:
		tools.ReleaseProcessGroupOnceEmpty(pid, nil, 0)
	default:
	}
}

// The adapter never delivers, the agent sends through the raft CLI
func (a *RaftAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	slog.Debug("[raft] adapter send is a no-op; agent delivers via raft CLI")
	return gateway.SendResult{Success: true}, nil
}

// Chat info of a raft runtime session
func (a *RaftAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"name": "raft/" + chatID, "type": "raft"}, nil
}

// Health document with the runtime session and the activity queue size and endpoints
func (a *RaftAdapter) handleHealth(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	activity := pyjson.ObjectOf("queueSize", a.activityQueue.Size(), "endpoint", "/activity", "drainEndpoint", "/activity/drain")
	return gateway.WebJSONResponse(pyjson.ObjectOf("status", "ok", "platform", "raft", "runtimeSession", a.runtimeSession,
		"activity", activity), 200, nil)
}

// Whether the request carries the bridge token, compared in constant time
func (a *RaftAdapter) authorized(request *gateway.WebRequest) bool {
	token := request.Header.Get(bridgeTokenHeader)
	a.mu.Lock()
	bridgeToken := a.bridgeToken
	a.mu.Unlock()
	return bridgeToken != "" && token != "" && hmac.Equal([]byte(token), []byte(bridgeToken))
}

// Authorized and size capped body of a wake or activity request, or the error response to answer with
func (a *RaftAdapter) readBridgeBody(request *gateway.WebRequest, text bool) ([]byte, *gateway.WebResponse, error) {
	if !a.authorized(request) {
		resp, err := errorResponse("unauthorized", 401)
		return nil, resp, err
	}
	tooLarge := func() ([]byte, *gateway.WebResponse, error) {
		resp, err := errorResponse("payload_too_large", 413)
		return nil, resp, err
	}
	if request.ContentLength != nil && *request.ContentLength > int64(a.maxBodyBytes) {
		return tooLarge()
	}
	body, err := request.Read()
	if err == nil && text {
		_, err = pystr.DecodeUTF8(body)
	}
	if err != nil {
		var httpErr *gateway.WebHTTPError
		if errors.As(err, &httpErr) && httpErr.Status == http.StatusRequestEntityTooLarge {
			return tooLarge()
		}
		reason := "bad_request"
		if text {
			reason = pyrepr.ExceptionMessage(err)
		}
		resp, rerr := errorResponse(reason, 400)
		return nil, resp, rerr
	}
	if len(body) > a.maxBodyBytes {
		return tooLarge()
	}
	return body, nil, nil
}

// Accepts a content free wake hint and injects it as an internal message for the runtime session
func (a *RaftAdapter) handleWake(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	rawBody, failure, err := a.readBridgeBody(request, false)
	if failure != nil || err != nil {
		return failure, err
	}
	var payload any = map[string]any{}
	if len(bytes.TrimSpace(rawBody)) > 0 {
		parsed, perr := pyjson.LoadsBytes(rawBody)
		if perr != nil {
			var decodeErr *pyjson.DecodeError
			if errors.As(perr, &decodeErr) {
				return errorResponse("invalid_json", 400)
			}
			return nil, perr
		}
		payload = pyjson.Plain(parsed)
	}
	fields, ok := payload.(map[string]any)
	if !ok {
		return errorResponse("invalid_payload", 400)
	}
	if hasContentField(fields) {
		return errorResponse("content_not_allowed", 400)
	}
	notReady := func() (*gateway.WebResponse, error) {
		return gateway.WebJSONResponse(pyjson.ObjectOf("ok", false, "error", "not_ready", "runtimeSession", a.runtimeSession), 503, nil)
	}
	if a.MessageHandler() == nil {
		slog.Warn("[raft] Wake received before gateway message handler was attached")
		return notReady()
	}
	deliveryID := ""
	for _, key := range wakeIDKeys {
		if v := fields[key]; pyrepr.Truthy(v) {
			deliveryID = pyrepr.Str(v)
			break
		}
	}
	if deliveryID == "" {
		deliveryID = fmt.Sprintf("raft-wake-%d", int64(float64(time.Now().UnixNano())/1e9*1000))
	}
	chatName, userID, userName := "Raft channel", "raft-bridge", "Raft Bridge"
	source := a.BuildSource(ctx, a.runtimeSession, gateway.SourceFields{ChatName: &chatName, ChatType: "dm", UserID: &userID,
		UserName: &userName})
	event := gateway.NewMessageEvent(wakePrompt)
	event.MessageType, event.Source, event.RawMessage = gateway.MessageTypeText, source, fields
	event.MessageID, event.Internal = &deliveryID, true
	if err := a.HandleMessage(ctx, event); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		slog.Error("[raft] Failed to inject wake event", "error", err)
		return notReady()
	}
	return gateway.WebJSONResponse(pyjson.ObjectOf("ok", true, "runtimeSession", a.runtimeSession), 202, nil)
}

// Queues one activity event the bridge posts
func (a *RaftAdapter) handleActivity(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	rawText, failure, err := a.readBridgeBody(request, true)
	if failure != nil || err != nil {
		return failure, err
	}
	parsed, perr := pyjson.Loads(string(rawText))
	if perr == nil {
		perr = a.activityQueue.Push(parsed)
	}
	if perr != nil {
		var decodeErr *pyjson.DecodeError
		if errors.As(perr, &decodeErr) {
			return errorResponse("invalid_json", 400)
		}
		return errorResponse(pyrepr.ExceptionMessage(perr), 400)
	}
	return gateway.WebJSONResponse(pyjson.ObjectOf("ok", true), 202, nil)
}

// Drains up to the max query count of queued activity events for an authorized caller
func (a *RaftAdapter) handleActivityDrain(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	if !a.authorized(request) {
		return errorResponse("unauthorized", 401)
	}
	var raw any = "200"
	if values, ok := request.Query()["max"]; ok && len(values) > 0 {
		raw = values[0]
	}
	return gateway.WebJSONResponse(a.activityQueue.Drain(gateway.CoercePort(raw, 200)), 200, nil)
}

// Admits a wake hint, queueing it behind a busy session instead of interrupting the running turn
func (a *RaftAdapter) HandleMessage(ctx context.Context, event *gateway.MessageEvent) error {
	if event.Internal {
		return a.BasePlatformAdapter.HandleMessage(ctx, event)
	}
	if a.MessageHandler() == nil {
		return nil
	}
	sessionKey, err := a.EventSessionKey(ctx, event)
	if err != nil {
		return err
	}
	if a.HasActiveSession(sessionKey) {
		slog.Debug(fmt.Sprintf("[raft] Wake queued for busy session %s", sessionKey))
		a.MergePendingMessage(sessionKey, event, false)
		return nil
	}
	return a.BasePlatformAdapter.HandleMessage(ctx, event)
}

// Queues an activity event from a hook, an invalid one dropped
func (a *RaftAdapter) ReportActivity(event *pyjson.Object) {
	if err := a.activityQueue.Push(event); err != nil {
		slog.Debug("[raft] activity event dropped during validation", "error", err)
	}
}
