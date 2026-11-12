package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pymimetypes"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/mediacache"
)

// Running bridge as the adapter sees it, the Popen surface of the bridge.js child
type bridgeProc interface {
	Pid() int
	Poll() (int, bool)
	Terminate()
	Kill()
	Wait(timeout time.Duration) (int, bool)
}

var (
	// Sleep of the adapter loops, cancelled with the context
	whatsappSleep = func(ctx context.Context, d time.Duration) error {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}
	// Starts the bridge in this process with its own append handle on the bridge log
	spawnBridgeProcess = func(ctx context.Context, args bridgeArgs, logFH *os.File, env map[string]string) bridgeProc {
		var out io.Writer = io.Discard
		if logFH != nil {
			if fh, err := os.OpenFile(logFH.Name(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666); err == nil {
				out = fh
			}
		}
		return startBridge(context.WithoutCancel(ctx), args, out, env)
	}
)

// Env keys the bridge consumes, re-resolved from the profile scope for multiplexed profiles
var bridgePassthroughEnv = []string{
	"WHATSAPP_ALLOWED_USERS", "WHATSAPP_ALLOW_FROM", "WHATSAPP_DM_POLICY", "WHATSAPP_GROUP_POLICY",
	"WHATSAPP_GROUP_ALLOWED_USERS", "WHATSAPP_GROUP_ALLOW_FROM", "WHATSAPP_REQUIRE_MENTION",
	"WHATSAPP_MENTION_PATTERNS", "WHATSAPP_FREE_RESPONSE_CHATS", "WHATSAPP_DEBUG",
	"WHATSAPP_FORWARD_OWNER_MESSAGES", "WHATSAPP_MAX_MESSAGE_LENGTH",
	"WHATSAPP_CHUNK_DELAY_MS", "WHATSAPP_SEND_TIMEOUT_MS",
}

var textInjectExts = map[string]bool{".txt": true, ".md": true, ".csv": true, ".json": true, ".xml": true, ".yaml": true,
	".yml": true, ".log": true, ".py": true, ".js": true, ".ts": true, ".html": true, ".css": true}

const (
	maxTextInjectBytes = 100 * 1024
	ownerReplyPrefix   = "[owner reply] "
)

var nativeMediaTypes = map[string]gateway.MessageType{"location": gateway.MessageTypeLocation,
	"live_location": gateway.MessageTypeLocation, "sticker": gateway.MessageTypeSticker}

// Inbound mediaType needle to kind, ptt ahead of audio since a voice note is ptt
var mediaNeedles = []struct {
	needle string
	kind   gateway.MessageType
}{{"image", gateway.MessageTypePhoto}, {"video", gateway.MessageTypeVideo}, {"ptt", gateway.MessageTypeVoice}, {"audio", gateway.MessageTypeAudio}}

var mediaInfo = map[gateway.MessageType][2]string{
	gateway.MessageTypePhoto: {"image", "image/jpeg"}, gateway.MessageTypeVoice: {"audio", "audio/ogg"},
	gateway.MessageTypeAudio: {"audio", "audio/mpeg"}, gateway.MessageTypeVideo: {"video", "video/mp4"},
	gateway.MessageTypeDocument: {"document", ""},
}

var mediaTypeByBridgeKind = map[string]gateway.MessageType{"image": gateway.MessageTypePhoto, "video": gateway.MessageTypeVideo,
	"audio": gateway.MessageTypeVoice, "document": gateway.MessageTypeDocument}

var quotedMimeByBridgeKind = map[string]string{"image": "image/jpeg", "video": "video/mp4", "gif": "video/mp4", "audio": "audio/ogg",
	"ptt": "audio/ogg", "document": "application/octet-stream", "sticker": "image/webp"}

// Transport over the in-process whatsmeow bridge and its loopback HTTP API, behaviour from the WhatsApp mixin
type WhatsAppAdapter struct {
	*gateway.BasePlatformAdapter
	*WhatsAppBehaviorMixin

	profileHome          string
	bridgeProcess        bridgeProc
	foreignBridgeSession *string
	bridgeProbeTimedOut  bool
	bridgePort           int
	bridgeScript         string
	sessionPath          string
	sendReadReceipts     bool
	bridgeLogFH          *os.File
	bridgeLog            string
	httpSession          *http.Client
	shuttingDown         bool

	pollMu     sync.Mutex
	pollCancel context.CancelFunc
	pollDone   chan struct{}
}

var _ gateway.PlatformAdapter = (*WhatsAppAdapter)(nil)

// Absolute session directory, the one string bridge.pid and every health session comparison share
func sessionDir(ctx context.Context, extra map[string]any) string {
	raw := home.HermesDir(home.Dir(ctx), "platforms/whatsapp/session", "whatsapp/session")
	if v, ok := extra["session_path"]; ok {
		raw = pyrepr.Str(v)
	}
	expanded := raw
	if strings.HasPrefix(raw, "~") {
		if userHome, err := os.UserHomeDir(); err == nil && (raw == "~" || strings.HasPrefix(raw, "~/")) {
			expanded = userHome + raw[1:]
		}
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return expanded
	}
	return abs
}

// Scoped WHATSAPP env read with a default, the profile's own .env under multiplexing
func wenv(ctx context.Context, name, def string) (string, error) {
	return gateway.GetScopedSecret(ctx, name, def, false)
}

// Adapter over the platform config with the bridge port, session, policies and allowlists resolved
func NewWhatsAppAdapter(ctx context.Context, config *gateway.PlatformConfig) (*WhatsAppAdapter, error) {
	a := &WhatsAppAdapter{}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, gateway.PlatformWhatsApp, a)
	a.WhatsAppBehaviorMixin = NewWhatsAppBehaviorMixin(a.BasePlatformAdapter.Name(), config)
	a.Traits.SplitsLongMessages = true
	a.Traits.SerialReceiveLoop = true
	a.Traits.MaxMessageLength = whatsappMaxMessageLength
	extra := config.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	a.profileHome = home.Dir(ctx)
	port, err := StandaloneBridgePort(a.profileHome, extra["bridge_port"])
	if err != nil {
		slog.Warn(fmt.Sprintf("[whatsapp] %s; using port 3000", err.Error()))
		port = 3000
	}
	a.bridgePort = port
	if script, ok := extra["bridge_script"]; ok {
		a.bridgeScript = pyrepr.Str(script)
	} else {
		a.bridgeScript, _ = os.Executable()
	}
	a.sessionPath = sessionDir(ctx, extra)
	if rp, ok := extra["reply_prefix"]; ok && rp != nil {
		s := pyrepr.Str(rp)
		a.ReplyPrefix = &s
	}
	dmPolicy, err := gateway.ExtraOrSecret(ctx, extra, "dm_policy", "WHATSAPP_DM_POLICY", "pairing", true)
	if err != nil {
		return nil, err
	}
	a.DMPolicy = pystr.Lower(pystr.Strip(pyrepr.Str(dmPolicy)))
	rawAllow, err := a.SelectDMAllowlist(extra, []string{"WHATSAPP_ALLOWED_USERS"}, func(name string) (string, error) { return wenv(ctx, name, "") })
	if err != nil {
		return nil, err
	}
	a.AllowFrom = CoerceAllowList(rawAllow)
	groupPolicy, err := gateway.ExtraOrSecret(ctx, extra, "group_policy", "WHATSAPP_GROUP_POLICY", "pairing", true)
	if err != nil {
		return nil, err
	}
	a.GroupPolicy = pystr.Lower(pystr.Strip(pyrepr.Str(groupPolicy)))
	_, rawGroups, err := SelectAllowlist(extra, []string{"group_allow_from", "groupAllowFrom"},
		[]string{"WHATSAPP_GROUP_ALLOW_FROM", "WHATSAPP_GROUP_ALLOWED_USERS"}, func(name string) (string, error) { return wenv(ctx, name, "") })
	if err != nil {
		return nil, err
	}
	a.GroupAllowFrom = CoerceAllowList(rawGroups)
	switch rr := extra["send_read_receipts"].(type) {
	case bool:
		a.sendReadReceipts = rr
	case nil:
		a.sendReadReceipts = false
	default:
		a.sendReadReceipts = whatsappTruthy[pystr.Lower(pystr.Strip(pyrepr.Str(rr)))] && pyrepr.Truthy(rr)
	}
	if a.MentionPatterns, err = a.CompileMentionPatterns(ctx); err != nil {
		return nil, err
	}
	a.ConfigureTextBatchDelays()
	return a, nil
}

// Platform display name, the base name over the mixin's field
func (a *WhatsAppAdapter) Name() string { return a.BasePlatformAdapter.Name() }

// The adapter gates DMs and groups itself, the mixin answer over the base default
func (a *WhatsAppAdapter) EnforcesOwnAccessPolicy() bool {
	return a.WhatsAppBehaviorMixin.EnforcesOwnAccessPolicy()
}

// Markdown converted to WhatsApp syntax
func (a *WhatsAppAdapter) FormatMessage(content string) string { return FormatMessage(content) }

// Loopback URL of a bridge endpoint
func (a *WhatsAppAdapter) bridgeURL(path string) string {
	return fmt.Sprintf("http://127.0.0.1:%d/%s", a.bridgePort, path)
}

// Bridge HTTP answer of one request
type bridgeResponse struct {
	status int
	body   []byte
}

// Decoded JSON body of a bridge answer
func (r bridgeResponse) json() (any, error) { return pyjson.LoadsBytes(r.body) }

// Issues one request against a bridge endpoint bounded by the total timeout
func (a *WhatsAppAdapter) bridgeReq(ctx context.Context, client *http.Client, method, path string, timeout time.Duration, payload any) (bridgeResponse, error) {
	if client == nil {
		return bridgeResponse{}, errors.New("'NoneType' object has no attribute '" + strings.ToLower(method) + "'")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return bridgeResponse{}, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.bridgeURL(path), body)
	if err != nil {
		return bridgeResponse{}, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return bridgeResponse{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return bridgeResponse{}, err
	}
	return bridgeResponse{status: resp.StatusCode, body: raw}, nil
}

// Whether an error is a timeout rather than a refusal
func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

// Health probe with a fresh client, true with nil data for an unparseable 200 and errors passed through
func (a *WhatsAppAdapter) probeBridgeHealth(ctx context.Context) (bool, *pyjson.Object, error) {
	client := &http.Client{}
	defer client.CloseIdleConnections()
	resp, err := a.bridgeReq(ctx, client, http.MethodGet, "health", 2*time.Second, nil)
	if err != nil {
		return false, nil, err
	}
	if resp.status != 200 {
		return false, nil, nil
	}
	data, err := resp.json()
	if obj, ok := data.(*pyjson.Object); err == nil && ok {
		return true, obj, nil
	}
	return true, nil, nil
}

// Keeps the bridge handle, opens the persistent client and starts the poll loop
func (a *WhatsAppAdapter) attachToBridge(ctx context.Context, managed bridgeProc) {
	a.bridgeProcess = managed
	a.httpSession = &http.Client{}
	pollCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	a.pollMu.Lock()
	a.pollCancel, a.pollDone = cancel, done
	a.pollMu.Unlock()
	go func() {
		defer close(done)
		a.pollMessages(pollCtx)
	}()
}

// Adopts a connected bridge for this session serving the current build and read receipt config, else says why it restarts
func (a *WhatsAppAdapter) reuseRunningBridge(ctx context.Context, bridgePath string) bool {
	a.foreignBridgeSession = nil
	a.bridgeProbeTimedOut = false
	ok, data, err := a.probeBridgeHealth(ctx)
	if err != nil {
		if isTimeout(err) {
			a.bridgeProbeTimedOut = true
		}
		return false
	}
	if !ok || data == nil {
		return false
	}
	if reported := data.GetOr("session", nil); pyrepr.Truthy(reported) && pyrepr.Str(reported) != a.sessionPath {
		s := pyrepr.Str(reported)
		a.foreignBridgeSession = &s
		return false
	}
	status := pyrepr.Str(data.GetOr("status", "unknown"))
	if status != "connected" {
		fmt.Printf("[%s] Bridge found but not connected (status: %s), restarting\n", a.Name(), status)
		return false
	}
	runningHash := ""
	if v := data.GetOr("scriptHash", ""); pyrepr.Truthy(v) {
		runningHash = pyrepr.Str(v)
	}
	diskHash := fileContentHash(bridgePath)
	if runningHash != "" && diskHash != "" && runningHash == diskHash && pyrepr.Truthy(data.GetOr("sendReadReceipts", false)) == a.sendReadReceipts {
		fmt.Printf("[%s] Using existing bridge (status: %s)\n", a.Name(), status)
		a.MarkConnected(ctx, "")
		a.attachToBridge(ctx, nil)
		a.WirePluginHandlers(ctx, nil)
		return true
	}
	staleReason := "send_read_receipts config changed"
	if runningHash != diskHash {
		shown := runningHash
		if shown == "" {
			shown = "unversioned"
		}
		staleReason = fmt.Sprintf("running=%s, disk=%s", shown, diskHash)
	}
	fmt.Printf("[%s] Running bridge is stale (%s), restarting\n", a.Name(), staleReason)
	return false
}

// Bridge environment, the effective profile policy, profile resolved WHATSAPP values and the cache dirs
func (a *WhatsAppAdapter) bridgeEnv(ctx context.Context) (map[string]string, error) {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	replyPrefix, err := wenv(ctx, "WHATSAPP_REPLY_PREFIX", "")
	if err != nil {
		return nil, err
	}
	switch {
	case replyPrefix != "":
		env["WHATSAPP_REPLY_PREFIX"] = replyPrefix
	case a.ReplyPrefix != nil:
		env["WHATSAPP_REPLY_PREFIX"] = *a.ReplyPrefix
	default:
		delete(env, "WHATSAPP_REPLY_PREFIX")
	}
	env["WHATSAPP_SEND_READ_RECEIPTS"] = "false"
	if a.sendReadReceipts {
		env["WHATSAPP_SEND_READ_RECEIPTS"] = "true"
	}
	mode, err := wenv(ctx, "WHATSAPP_MODE", "self-chat")
	if err != nil {
		return nil, err
	}
	pairs := [][2]string{{"WHATSAPP_MODE", mode}}
	for _, k := range bridgePassthroughEnv {
		v, err := wenv(ctx, k, "")
		if err != nil {
			return nil, err
		}
		pairs = append(pairs, [2]string{k, v})
	}
	for _, kv := range pairs {
		if kv[1] != "" {
			env[kv[0]] = kv[1]
		} else {
			delete(env, kv[0])
		}
	}
	env["WHATSAPP_DM_POLICY"] = a.DMPolicy
	env["WHATSAPP_GROUP_POLICY"] = a.GroupPolicy
	for _, item := range []struct {
		key string
		ids map[string]bool
	}{{"WHATSAPP_ALLOWED_USERS", a.AllowFrom}, {"WHATSAPP_GROUP_ALLOWED_USERS", a.GroupAllowFrom}} {
		if len(item.ids) > 0 {
			env[item.key] = strings.Join(sortedKeys(item.ids), ",")
		} else {
			delete(env, item.key)
		}
	}
	dirs, err := cacheDirs(ctx)
	if err != nil {
		return nil, err
	}
	env["ICEBATH_IMAGE_CACHE_DIR"], env["ICEBATH_AUDIO_CACHE_DIR"], env["ICEBATH_DOCUMENT_CACHE_DIR"] = dirs[0], dirs[1], dirs[3]
	return env, nil
}

// Reports a dead bridge with its log path and a named retryable fatal error, always false
func (a *WhatsAppAdapter) bridgeDied(ctx context.Context, detail, code string) bool {
	fmt.Printf("[%s] %s\n", a.Name(), detail)
	fmt.Printf("[%s] Check log: %s\n", a.Name(), a.bridgeLogDisplay())
	a.closeBridgeLog()
	a.SetFatalError(ctx, code, fmt.Sprintf("%s (see %s)", detail, a.bridgeLogDisplay()), true)
	return false
}

// Bridge log path the way Python prints it, None before a log is opened
func (a *WhatsAppAdapter) bridgeLogDisplay() string {
	if a.bridgeLog == "" {
		return "None"
	}
	return a.bridgeLog
}

// Tri-state outcome of a health polling round
type healthOutcome int

const (
	healthTimeout healthOutcome = iota
	healthConnected
	healthDied
)

// Polls the health endpoint fifteen times a second apart, reporting a dead bridge and a connected one
func (a *WhatsAppAdapter) pollBridgeHealth(ctx context.Context, diedMsg string) (healthOutcome, bool, *pyjson.Object) {
	httpReady := false
	data := pyjson.NewObject()
	for attempt := 0; attempt < 15; attempt++ {
		if err := whatsappSleep(ctx, time.Second); err != nil {
			return healthTimeout, httpReady, data
		}
		if code, exited := a.bridgeProcess.Poll(); exited {
			unlinkQuietly(filepath.Join(a.sessionPath, "bridge.pid"))
			a.bridgeDied(ctx, strings.ReplaceAll(diedMsg, "{code}", strconv.Itoa(code)), "whatsapp_bridge_exited")
			return healthDied, httpReady, data
		}
		ok, d, err := a.probeBridgeHealth(ctx)
		if err != nil || !ok {
			continue
		}
		httpReady = true
		if d != nil {
			data = d
			if status, isStr := data.GetOr("status", nil).(string); isStr && status == "connected" {
				fmt.Printf("[%s] Bridge ready (status: connected)\n", a.Name())
				return healthConnected, httpReady, data
			}
		}
	}
	return healthTimeout, httpReady, data
}

// Waits for the bridge HTTP server then for the WhatsApp connection, proceeding with a warning past 30s
func (a *WhatsAppAdapter) waitForBridge(ctx context.Context) bool {
	outcome, httpReady, data := a.pollBridgeHealth(ctx, "Bridge process died (exit code {code})")
	if outcome == healthDied {
		return false
	}
	if !httpReady {
		return a.bridgeDied(ctx, "Bridge HTTP server did not start in 15s", "whatsapp_bridge_timeout")
	}
	if status, isStr := data.GetOr("status", nil).(string); !isStr || status != "connected" {
		fmt.Printf("[%s] Bridge HTTP ready, waiting for WhatsApp connection...\n", a.Name())
		outcome, _, _ = a.pollBridgeHealth(ctx, "Bridge process died during connection")
		if outcome == healthDied {
			return false
		}
		if outcome == healthTimeout {
			fmt.Printf("[%s] ⚠ WhatsApp not connected after 30s\n", a.Name())
			fmt.Printf("[%s]   Bridge log: %s\n", a.Name(), a.bridgeLogDisplay())
			fmt.Printf("[%s]   If session expired, re-pair: icebath whatsapp\n", a.Name())
		}
	}
	return true
}

// Bridge build and paired session present, else a named final fatal error since an unpaired bridge only shows QR codes
func (a *WhatsAppAdapter) preflight(ctx context.Context) bool {
	credsPath := filepath.Join(a.sessionPath, "creds.json")
	checks := []struct {
		path, warn, code, message string
	}{
		{a.bridgeScript, fmt.Sprintf("[%s] Bridge script not found: %s", a.Name(), a.bridgeScript),
			"whatsapp_bridge_missing", fmt.Sprintf("WhatsApp bridge script missing at %s.", a.bridgeScript)},
		{credsPath, fmt.Sprintf("[%s] WhatsApp is enabled but not paired (no creds.json at %s). Pair from the dashboard or run "+
			"`icebath whatsapp`; remove WHATSAPP_ENABLED from your .env to disable.", a.Name(), credsPath),
			"whatsapp_not_paired", "WhatsApp enabled but not paired — pair from the dashboard or run `icebath whatsapp`."},
	}
	for _, c := range checks {
		if _, err := os.Stat(c.path); err != nil {
			slog.Warn(c.warn)
			a.SetFatalError(ctx, c.code, c.message, false)
			return false
		}
	}
	slog.Info(fmt.Sprintf("[%s] Bridge found at %s", a.Name(), a.bridgeScript))
	return true
}

// Starts or adopts the bridge for this profile's port and session and waits for it to be ready
func (a *WhatsAppAdapter) Connect(ctx context.Context, isReconnect bool) (ok bool, retErr error) {
	secondary := a.Traits.RuntimeStatusPlatformKey != ""
	priorBridgeIsOurs := false
	extra := a.BasePlatformAdapter.Config.Extra
	if secondary {
		port, err := SecondaryBridgePort(a.profileHome, extra["bridge_port"])
		var verdict string
		if err == nil {
			a.bridgePort = port
			verdict, err = CheckSecondaryOwnership(a.sessionPath, a.bridgePort)
		}
		if err != nil {
			a.SetFatalError(ctx, "whatsapp_bridge_conflict", fmt.Sprintf("%s. Set platforms.whatsapp.extra.bridge_port to a distinct free port; "+
				"or stop the process holding it.", err.Error()), false)
			return false, nil
		}
		priorBridgeIsOurs = verdict == "ours"
	}
	if !a.preflight(ctx) {
		return false, nil
	}
	bridgePath := a.bridgeScript
	lockAcquired := false
	if acquired, err := a.AcquirePlatformLock(ctx, "whatsapp-session", a.sessionPath, "WhatsApp session"); err != nil {
		slog.Warn(fmt.Sprintf("[%s] Could not acquire session lock (non-fatal): %s", a.Name(), err.Error()))
	} else if !acquired {
		return false, nil
	} else {
		lockAcquired = true
	}
	defer func() {
		if !a.IsConnected() {
			if lockAcquired {
				a.ReleasePlatformLock()
			}
			a.closeBridgeLog()
		}
	}()
	defer func() {
		if r := recover(); r != nil {
			slog.Error(fmt.Sprintf("[%s] Failed to start bridge: %v", a.Name(), r))
			ok, retErr = false, nil
		}
	}()
	if err := os.MkdirAll(a.sessionPath, 0o777); err != nil {
		slog.Error(fmt.Sprintf("[%s] Failed to start bridge: %s", a.Name(), err.Error()))
		return false, nil
	}
	if (!secondary || priorBridgeIsOurs) && a.reuseRunningBridge(ctx, bridgePath) {
		return true, nil
	}
	if a.foreignBridgeSession != nil {
		foreign := *a.foreignBridgeSession
		a.SetFatalError(ctx, "whatsapp_bridge_foreign_session", fmt.Sprintf(
			"Port %d is already served by a WhatsApp bridge for a different session (%s). Give this profile its own port via "+
				"platforms.whatsapp.extra.bridge_port (one distinct bridge_port per profile); nothing on port %d was stopped.",
			a.bridgePort, foreign, a.bridgePort), false)
		return false, nil
	}
	if !secondary || priorBridgeIsOurs {
		killStaleBridgeByPidfile(a.sessionPath)
	}
	if !secondary {
		if a.bridgeProbeTimedOut {
			_ = whatsappSleep(ctx, time.Second)
			held := true
			if pids, err := portListenerPids(a.bridgePort); err == nil {
				held = len(pids) > 0
			}
			if held {
				a.SetFatalError(ctx, "whatsapp_bridge_unresponsive", fmt.Sprintf(
					"Port %d is held by a process that did not answer /health in time, so Icebath cannot tell whose WhatsApp bridge it is; "+
						"nothing on port %d was stopped. Stop that process, or give this profile its own port via platforms.whatsapp.extra.bridge_port.",
					a.bridgePort, a.bridgePort), true)
				return false, nil
			}
		}
		killPortProcess(a.bridgePort)
	}
	if !a.waitBridgePortFree(ctx) {
		return false, nil
	}
	a.bridgeLog = filepath.Join(filepath.Dir(a.sessionPath), "bridge.log")
	fh, err := os.OpenFile(a.bridgeLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		slog.Error(fmt.Sprintf("[%s] Failed to start bridge: %s", a.Name(), err.Error()))
		return false, nil
	}
	a.bridgeLogFH = fh
	mode, err := wenv(ctx, "WHATSAPP_MODE", "self-chat")
	if err != nil {
		slog.Error(fmt.Sprintf("[%s] Failed to start bridge: %s", a.Name(), err.Error()))
		return false, nil
	}
	env, err := a.bridgeEnv(ctx)
	if err != nil {
		slog.Error(fmt.Sprintf("[%s] Failed to start bridge: %s", a.Name(), err.Error()))
		return false, nil
	}
	a.bridgeProcess = spawnBridgeProcess(ctx, bridgeArgs{Port: a.bridgePort, Session: a.sessionPath, Mode: mode}, fh, env)
	port := a.bridgePort
	writeBridgePidfile(a.sessionPath, a.bridgeProcess.Pid(), &port)
	if !a.waitForBridge(ctx) {
		return false, nil
	}
	a.attachToBridge(ctx, a.bridgeProcess)
	a.MarkConnected(ctx, "")
	fmt.Printf("[%s] Bridge started on port %d\n", a.Name(), a.bridgePort)
	a.WirePluginHandlers(ctx, nil)
	return true, nil
}

// Closes the adapter's bridge log handle
func (a *WhatsAppAdapter) closeBridgeLog() {
	if a.bridgeLogFH != nil {
		_ = a.bridgeLogFH.Close()
		a.bridgeLogFH = nil
	}
}

// Bounded wait for the previous bridge to release its port, a named fatal when a live listener keeps it
func (a *WhatsAppAdapter) waitBridgePortFree(ctx context.Context) bool {
	deadline := time.Now().Add(bridgePortFreeTimeout)
	for !portIsFree(a.bridgePort) {
		if !time.Now().Before(deadline) {
			holders := "unknown"
			if pids, err := portListenerPids(a.bridgePort); err == nil && len(pids) > 0 {
				holders = pyrepr.Repr(intsAny(pids))
			}
			a.SetFatalError(ctx, "whatsapp_bridge_port_busy", fmt.Sprintf(
				"Port %d is still held by PID(s) %s after the previous bridge was stopped and %.0fs of waiting; no new bridge was spawned. "+
					"Stop the holder, or set platforms.whatsapp.extra.bridge_port to a free port.", a.bridgePort, holders, bridgePortFreeTimeout.Seconds()), true)
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
	return true
}

// Ints as a Python list value
func intsAny(in []int) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

// Whether the gateway runner flagged a signal driven stop
func runnerStopRequested(runner any) bool {
	r, ok := runner.(interface{ StopRequestedBySignal() bool })
	return ok && r.StopRequestedBySignal()
}

// Fatal message of a managed bridge that exited unexpectedly, empty while it runs or after a shutdown exit
func (a *WhatsAppAdapter) checkManagedBridgeExit(ctx context.Context) string {
	if a.bridgeProcess == nil {
		return ""
	}
	returncode, exited := a.bridgeProcess.Poll()
	if !exited {
		return ""
	}
	if (a.shuttingDown || runnerStopRequested(a.GatewayRunner)) && (returncode == 0 || returncode == -2 || returncode == -15) {
		slog.Info(fmt.Sprintf("[%s] Bridge exited during shutdown (code %d).", a.Name(), returncode))
		return ""
	}
	message := fmt.Sprintf("WhatsApp bridge process exited unexpectedly (code %d).", returncode)
	if !a.HasFatalError() {
		slog.Error(fmt.Sprintf("[%s] %s", a.Name(), message))
		a.SetFatalError(ctx, "whatsapp_bridge_exited", message, true)
		a.closeBridgeLog()
		_ = a.NotifyFatalError(ctx)
	}
	if m := a.FatalErrorMessage(); m != nil && *m != "" {
		return *m
	}
	return message
}

// Stops the bridge, at once when forced
func (a *WhatsAppAdapter) terminateBridge(force bool) {
	if force {
		a.bridgeProcess.Kill()
	} else {
		a.bridgeProcess.Terminate()
	}
}

// Stops the bridge, waiting out a forced stop, then releases the lock and the poll loop
func (a *WhatsAppAdapter) Disconnect(ctx context.Context) error {
	a.shuttingDown = true
	if a.bridgeProcess == nil {
		fmt.Printf("[%s] Disconnecting (external bridge left running)\n", a.Name())
	} else {
		a.terminateBridge(false)
		_ = whatsappSleep(ctx, time.Second)
		if _, exited := a.bridgeProcess.Poll(); !exited {
			a.terminateBridge(true)
			a.bridgeProcess.Wait(sigtermGrace)
		}
	}
	if a.bridgeProcess != nil || a.Traits.RuntimeStatusPlatformKey == "" {
		unlinkQuietly(filepath.Join(a.sessionPath, "bridge.pid"))
	}
	a.pollMu.Lock()
	cancel, done := a.pollCancel, a.pollDone
	a.pollCancel, a.pollDone = nil, nil
	a.pollMu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	if a.httpSession != nil {
		a.httpSession.CloseIdleConnections()
	}
	a.httpSession, a.bridgeProcess = nil, nil
	a.ReleasePlatformLock()
	a.MarkDisconnected(ctx)
	a.closeBridgeLog()
	fmt.Printf("[%s] Disconnected\n", a.Name())
	return nil
}

// Why the bridge cannot take a request now, empty when it can
func (a *WhatsAppAdapter) bridgeUnavailable(ctx context.Context) string {
	if !a.IsConnected() || a.httpSession == nil {
		return "Not connected"
	}
	return a.checkManagedBridgeExit(ctx)
}

// Posts to the bridge, the message id and raw answer on 200 and the answer text otherwise
func (a *WhatsAppAdapter) postBridgeMessage(ctx context.Context, path string, payload map[string]any, timeout time.Duration) gateway.SendResult {
	resp, err := a.bridgeReq(ctx, a.httpSession, http.MethodPost, path, timeout, payload)
	if err != nil {
		return gateway.SendResult{Success: false, Error: err.Error()}
	}
	if resp.status == 200 {
		data, err := resp.json()
		if err != nil {
			return gateway.SendResult{Success: false, Error: err.Error()}
		}
		obj, _ := data.(*pyjson.Object)
		var id any
		if obj != nil {
			id = obj.GetOr("messageId", nil)
		}
		return gateway.SendResult{Success: true, MessageID: id, RawResponse: data}
	}
	return gateway.SendResult{Success: false, Error: string(resp.body)}
}

// Sends formatted text in chunks that keep code blocks whole, the reply quote on the first chunk
func (a *WhatsAppAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	if unavailable := a.bridgeUnavailable(ctx); unavailable != "" {
		return gateway.SendResult{Success: false, Error: unavailable}, nil
	}
	if content == "" || pystr.Strip(content) == "" {
		return gateway.SendResult{Success: true}, nil
	}
	chatID = gateway.ToWhatsAppJID(chatID)
	limit, err := a.OutgoingChunkLimit(ctx)
	if err != nil {
		return gateway.SendResult{Success: false, Error: err.Error()}, nil
	}
	chunks := gateway.TruncateMessage(a.FormatMessage(content), limit, nil)
	sent := []string{}
	var last any
	for idx, chunk := range chunks {
		payload := map[string]any{"chatId": chatID, "message": chunk}
		if replyTo != "" && idx == 0 {
			payload["replyTo"] = replyTo
		}
		result := a.postBridgeMessage(ctx, "send", payload, 30*time.Second)
		if !result.Success {
			return gateway.SendResult{Success: false, Error: result.Error}, nil
		}
		last = result.MessageID
		if pyrepr.Truthy(last) {
			sent = append(sent, pyrepr.Str(last))
		}
		if len(chunks) > 1 {
			_ = whatsappSleep(ctx, 300*time.Millisecond)
		}
	}
	continuation := []any{}
	ids := []any{}
	for i, id := range sent {
		ids = append(ids, id)
		if i < len(sent)-1 {
			continuation = append(continuation, id)
		}
	}
	return gateway.SendResult{Success: true, MessageID: last, ContinuationMessageIDs: continuation,
		RawResponse: map[string]any{"message_ids": ids}}, nil
}

// Edits a sent message through the bridge
func (a *WhatsAppAdapter) EditMessage(ctx context.Context, chatID, messageID, content string, finalize bool) (gateway.SendResult, error) {
	if unavailable := a.bridgeUnavailable(ctx); unavailable != "" {
		return gateway.SendResult{Success: false, Error: unavailable}, nil
	}
	resp, err := a.bridgeReq(ctx, a.httpSession, http.MethodPost, "edit", 15*time.Second,
		map[string]any{"chatId": gateway.ToWhatsAppJID(chatID), "messageId": messageID, "message": content})
	if err != nil {
		return gateway.SendResult{Success: false, Error: err.Error()}, nil
	}
	if resp.status == 200 {
		return gateway.SendResult{Success: true, MessageID: messageID}, nil
	}
	return gateway.SendResult{Success: false, Error: string(resp.body)}, nil
}

// Sends a local file natively and indexes it so a later quote of our own attachment resolves
func (a *WhatsAppAdapter) sendMediaToBridge(ctx context.Context, chatID, filePath, mediaType, caption, fileName string) gateway.SendResult {
	if unavailable := a.bridgeUnavailable(ctx); unavailable != "" {
		return gateway.SendResult{Success: false, Error: unavailable}
	}
	if _, err := os.Stat(filePath); err != nil {
		return gateway.SendResult{Success: false, Error: "File not found: " + filePath}
	}
	jid := gateway.ToWhatsAppJID(chatID)
	payload := map[string]any{"chatId": jid, "filePath": filePath, "mediaType": mediaType}
	if caption != "" {
		payload["caption"] = caption
	}
	if fileName != "" {
		payload["fileName"] = fileName
	}
	result := a.postBridgeMessage(ctx, "send-media", payload, 120*time.Second)
	if result.Success && pyrepr.Truthy(result.MessageID) {
		mt, _, _ := pymimetypes.GuessType(filePath, true)
		if mt == "" {
			mt = mediaInfo[mediaTypeByBridgeKind[mediaType]][1]
		}
		if mt == "" {
			mt = "application/octet-stream"
		}
		gateway.RichSentRecordMedia(ctx, jid, result.MessageID, [][2]string{{filePath, mt}})
	}
	return result
}

// Native WhatsApp poll, the transport primitive under approval and clarify flows
func (a *WhatsAppAdapter) SendPoll(ctx context.Context, chatID, question string, options []string, selectableCount int) gateway.SendResult {
	if unavailable := a.bridgeUnavailable(ctx); unavailable != "" {
		return gateway.SendResult{Success: false, Error: unavailable}
	}
	opts := make([]any, len(options))
	for i, o := range options {
		opts[i] = o
	}
	return a.postBridgeMessage(ctx, "send-poll", map[string]any{"chatId": gateway.ToWhatsAppJID(chatID), "question": question,
		"options": opts, "selectableCount": selectableCount}, 30*time.Second)
}

// Multiple choice clarify as a native poll whose pick arrives as message text, else the text prompt
func (a *WhatsAppAdapter) SendClarify(ctx context.Context, chatID, question string, choices []string, clarifyID, sessionKey string, metadata map[string]any) (gateway.SendResult, error) {
	clean := []string{}
	for _, c := range choices {
		if s := pystr.Strip(c); s != "" {
			clean = append(clean, s)
		}
	}
	if len(clean) >= 2 && len(clean) <= 12 {
		result := a.SendPoll(ctx, chatID, pystr.Strip(question), clean, 1)
		if result.Success {
			return result, nil
		}
		slog.Warn(fmt.Sprintf("[%s] Native WhatsApp clarify poll failed; falling back to text: %s", a.Name(), pyrepr.Str(result.Error)))
	}
	return a.BasePlatformAdapter.SendClarify(ctx, chatID, question, choices, clarifyID, sessionKey, metadata)
}

// Native location pin with optional name and address
func (a *WhatsAppAdapter) SendLocation(ctx context.Context, chatID string, latitude, longitude float64, name, address string) gateway.SendResult {
	if unavailable := a.bridgeUnavailable(ctx); unavailable != "" {
		return gateway.SendResult{Success: false, Error: unavailable}
	}
	payload := map[string]any{"chatId": gateway.ToWhatsAppJID(chatID), "latitude": latitude, "longitude": longitude}
	if name != "" {
		payload["name"] = name
	}
	if address != "" {
		payload["address"] = address
	}
	return a.postBridgeMessage(ctx, "send-location", payload, 30*time.Second)
}

// Downloads the image URL into the cache and sends it natively, the base fallback on failure
func (a *WhatsAppAdapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	localPath, err := gateway.CacheImageFromURL(ctx, imageURL, ".jpg", 2)
	if err == nil {
		return a.sendMediaToBridge(ctx, chatID, localPath, "image", caption, ""), nil
	}
	return a.BasePlatformAdapter.SendImage(ctx, chatID, imageURL, caption, replyTo, metadata)
}

// Sends a local image natively
func (a *WhatsAppAdapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendMediaToBridge(ctx, chatID, imagePath, "image", caption, ""), nil
}

// Sends a local video natively
func (a *WhatsAppAdapter) SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendMediaToBridge(ctx, chatID, videoPath, "video", caption, ""), nil
}

// Sends audio as a voice note, a plain audio file going as a named document instead
func (a *WhatsAppAdapter) SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	if isVoice, ok := extra["is_voice"].(bool); ok && !isVoice {
		return a.SendDocument(ctx, chatID, audioPath, caption, "", replyTo, metadata, nil)
	}
	return a.sendMediaToBridge(ctx, chatID, audioPath, "audio", caption, ""), nil
}

// Sends a local file as a document named after the file unless a name is given
func (a *WhatsAppAdapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	if fileName == "" {
		fileName = filepath.Base(filePath)
	}
	return a.sendMediaToBridge(ctx, chatID, filePath, "document", caption, fileName), nil
}

// Shows the typing indicator, errors ignored
func (a *WhatsAppAdapter) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	if a.bridgeUnavailable(ctx) != "" {
		return nil
	}
	_, _ = a.bridgeReq(ctx, a.httpSession, http.MethodPost, "typing", 5*time.Second, map[string]any{"chatId": gateway.ToWhatsAppJID(chatID)})
	return nil
}

// Chat name, kind and participants from the bridge, the chat id as a DM when it cannot answer
func (a *WhatsAppAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	if !a.IsConnected() || a.httpSession == nil {
		return map[string]any{"name": "Unknown", "type": "dm"}, nil
	}
	if a.checkManagedBridgeExit(ctx) == "" {
		resp, err := a.bridgeReq(ctx, a.httpSession, http.MethodGet, "chat/"+gateway.ToWhatsAppJID(chatID), 10*time.Second, nil)
		if err != nil {
			slog.Debug(fmt.Sprintf("Could not get WhatsApp chat info for %s: %s", chatID, err.Error()))
		} else if resp.status == 200 {
			data, err := resp.json()
			if obj, ok := data.(*pyjson.Object); err == nil && ok {
				kind := "dm"
				if pyrepr.Truthy(obj.GetOr("isGroup", nil)) {
					kind = "group"
				}
				return map[string]any{"name": obj.GetOr("name", chatID), "type": kind, "participants": obj.GetOr("participants", []any{})}, nil
			}
			if err != nil {
				slog.Debug(fmt.Sprintf("Could not get WhatsApp chat info for %s: %s", chatID, err.Error()))
			}
		}
	}
	return map[string]any{"name": chatID, "type": "dm"}, nil
}

// Prints a managed bridge exit and reports whether one happened
func (a *WhatsAppAdapter) reportBridgeExit(ctx context.Context) bool {
	bridgeExit := a.checkManagedBridgeExit(ctx)
	if bridgeExit != "" {
		fmt.Printf("[%s] %s\n", a.Name(), bridgeExit)
	}
	return bridgeExit != ""
}

// Polls the bridge every second, logging delivery statuses and dispatching accepted messages
func (a *WhatsAppAdapter) pollMessages(ctx context.Context) {
	for a.IsConnected() {
		if a.httpSession == nil || a.reportBridgeExit(ctx) {
			return
		}
		resp, err := a.bridgeReq(ctx, a.httpSession, http.MethodGet, "messages", 30*time.Second, nil)
		if err == nil && resp.status == 200 {
			var data any
			if data, err = resp.json(); err == nil {
				items, _ := data.([]any)
				for _, item := range items {
					msg, ok := item.(*pyjson.Object)
					if !ok {
						continue
					}
					if pyrepr.Str(msg.GetOr("event", nil)) == "delivery_status" {
						a.logDeliveryStatus(msg)
						continue
					}
					event := a.buildMessageEvent(ctx, msg)
					if event == nil {
						continue
					}
					go a.sendReadReceipt(context.WithoutCancel(ctx), msg)
					if event.MessageType == gateway.MessageTypeText {
						_ = a.EnqueueTextEvent(ctx, event)
					} else {
						_ = a.HandleMessage(ctx, event)
					}
				}
			}
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if a.reportBridgeExit(ctx) {
				return
			}
			fmt.Printf("[%s] Poll error: %s\n", a.Name(), err.Error())
			if whatsappSleep(ctx, 5*time.Second) != nil {
				return
			}
		}
		if whatsappSleep(ctx, time.Second) != nil {
			return
		}
	}
}

// One gateway log line per delivery or read status change of a message we sent
func (a *WhatsAppAdapter) logDeliveryStatus(data *pyjson.Object) {
	recipient, errText := "", ""
	if v := data.GetOr("recipientId", nil); pyrepr.Truthy(v) {
		recipient = " recipient=" + pyrepr.Str(v)
	}
	if v := data.GetOr("error", nil); pyrepr.Truthy(v) {
		errText = " error=" + pyrepr.Str(v)
	}
	slog.Info(fmt.Sprintf("[%s] WhatsApp delivery status: message=%s chat=%s status=%s%s at=%s%s", a.Name(),
		pyrepr.Str(data.GetOr("messageId", nil)), pyrepr.Str(data.GetOr("chatId", nil)), pyrepr.Str(data.GetOr("status", nil)),
		recipient, pyrepr.Str(data.GetOr("timestamp", nil)), errText))
}

// Marks an accepted inbound message read when read receipts are on
func (a *WhatsAppAdapter) sendReadReceipt(ctx context.Context, data *pyjson.Object) {
	key, ok := data.GetOr("readReceiptKey", nil).(*pyjson.Object)
	if !a.sendReadReceipts || a.httpSession == nil || !ok {
		return
	}
	resp, err := a.bridgeReq(ctx, a.httpSession, http.MethodPost, "read", 5*time.Second, map[string]any{"key": key})
	if err != nil {
		slog.Warn(fmt.Sprintf("[%s] WhatsApp read receipt failed: %s", a.Name(), err.Error()))
		return
	}
	if resp.status != 200 {
		slog.Warn(fmt.Sprintf("[%s] WhatsApp read receipt failed with HTTP %d", a.Name(), resp.status))
	}
}

// Message kind of a bridge payload, native kinds first then the media needles
func classifyBridgeMessage(data *pyjson.Object) gateway.MessageType {
	mediaType := ""
	if v := data.GetOr("mediaType", ""); pyrepr.Truthy(v) {
		mediaType = pyrepr.Str(v)
	}
	if kind, ok := nativeMediaTypes[mediaType]; ok {
		return kind
	}
	if !pyrepr.Truthy(data.GetOr("hasMedia", nil)) {
		return gateway.MessageTypeText
	}
	for _, n := range mediaNeedles {
		if strings.Contains(mediaType, n.needle) {
			return n.kind
		}
	}
	return gateway.MessageTypeDocument
}

// Cached media of a bridge payload, remote image and audio fetched and absolute paths kept only inside a cache dir
func (a *WhatsAppAdapter) collectBridgeMedia(ctx context.Context, data *pyjson.Object, msgType gateway.MessageType) ([]string, []string) {
	urls, kinds := []string{}, []string{}
	info, known := mediaInfo[msgType]
	label, defaultMime := info[0], info[1]
	bridgeMime := ""
	if v := data.GetOr("mime", nil); pyrepr.Truthy(v) {
		bridgeMime = pystr.Strip(pyrepr.Str(v))
	}
	list, _ := data.GetOr("mediaUrls", []any{}).([]any)
	for _, raw := range list {
		url := pyrepr.Str(raw)
		mt := bridgeMime
		if mt == "" {
			if msgType == gateway.MessageTypeDocument {
				mt = mediacache.SupportedDocumentTypes[strings.ToLower(filepath.Ext(url))]
				if mt == "" {
					mt = "application/octet-stream"
				}
			} else {
				mt = defaultMime
			}
		}
		remote := strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://")
		switch {
		case remote && (msgType == gateway.MessageTypePhoto || msgType == gateway.MessageTypeVoice || msgType == gateway.MessageTypeAudio):
			var cached string
			var err error
			if msgType == gateway.MessageTypePhoto {
				cached, err = gateway.CacheImageFromURL(ctx, url, ".jpg", 2)
			} else {
				cached, err = gateway.CacheAudioFromURL(ctx, url, ".ogg", 2)
			}
			if err == nil {
				url = cached
				fmt.Printf("[%s] Cached user %s: %s\n", a.Name(), label, url)
			} else {
				fmt.Printf("[%s] Failed to cache %s: %s\n", a.Name(), label, err.Error())
			}
			urls, kinds = append(urls, url), append(kinds, mt)
		case known && filepath.IsAbs(url):
			if isAllowedBridgePath(ctx, url) {
				urls, kinds = append(urls, url), append(kinds, mt)
				fmt.Printf("[%s] Using bridge-cached %s: %s\n", a.Name(), label, url)
			} else {
				fmt.Printf("[%s] Rejected bridge %s path outside cache dir: %s\n", a.Name(), label, url)
			}
		default:
			urls, kinds = append(urls, url), append(kinds, "unknown")
		}
	}
	return urls, kinds
}

// Body with text readable documents up to 100KB prepended, plus one inlined flag per cached url
func (a *WhatsAppAdapter) injectDocumentText(cachedURLs []string, body string) (string, []bool) {
	inlined := make([]bool, len(cachedURLs))
	for i, docPath := range cachedURLs {
		if !textInjectExts[strings.ToLower(filepath.Ext(docPath))] {
			continue
		}
		fi, err := os.Stat(docPath)
		if err != nil {
			fmt.Printf("[%s] Failed to read document text: %s\n", a.Name(), err.Error())
			continue
		}
		if fi.Size() > maxTextInjectBytes {
			fmt.Printf("[%s] Skipping text injection for %s (%d bytes > %d)\n", a.Name(), docPath, fi.Size(), maxTextInjectBytes)
			continue
		}
		raw, err := os.ReadFile(docPath)
		if err != nil {
			fmt.Printf("[%s] Failed to read document text: %s\n", a.Name(), err.Error())
			continue
		}
		content := strings.ToValidUTF8(strings.TrimPrefix(string(raw), "\ufeff"), "�")
		name := filepath.Base(docPath)
		parts := strings.SplitN(name, "_", 3)
		display := name
		if len(parts) >= 3 {
			display = parts[2]
		}
		injection := fmt.Sprintf("[Content of %s]:\n%s", display, content)
		if body != "" {
			body = injection + "\n\n" + body
		} else {
			body = injection
		}
		inlined[i] = true
		fmt.Printf("[%s] Injected text content from: %s\n", a.Name(), docPath)
	}
	return body, inlined
}

// Quoted attachment path and mime, inbound from the bridge cache and ours from the outbound index
func (a *WhatsAppAdapter) quotedMedia(ctx context.Context, data *pyjson.Object, rawReplyID any) [][2]string {
	quotedType := ""
	if v := data.GetOr("quotedMediaType", nil); pyrepr.Truthy(v) {
		quotedType = pystr.Strip(pyrepr.Str(v))
	}
	accepted := [][2]string{}
	list, _ := data.GetOr("quotedMediaUrls", nil).([]any)
	for _, raw := range list {
		path, isStr := raw.(string)
		if !isStr || !filepath.IsAbs(path) || !isAllowedBridgePath(ctx, path) {
			fmt.Printf("[%s] Rejected quoted-media path outside cache dir: %s\n", a.Name(), pyrepr.Str(raw))
			continue
		}
		mt := quotedMimeByBridgeKind[quotedType]
		if mt == "" {
			mt = "application/octet-stream"
		}
		accepted = append(accepted, [2]string{path, mt})
	}
	if len(accepted) == 0 && rawReplyID != nil {
		accepted = gateway.RichSentLookupMedia(ctx, data.GetOr("chatId", ""), pyrepr.Str(rawReplyID))
	}
	for _, p := range accepted {
		fmt.Printf("[%s] Attached quoted-reply media: %s\n", a.Name(), p[0])
	}
	return accepted
}

// Message event of a bridge payload after the intake gate, media cached and quoted media folded in
func (a *WhatsAppAdapter) buildMessageEvent(ctx context.Context, data *pyjson.Object) (event *gateway.MessageEvent) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("[%s] Error building event: %v\n", a.Name(), r)
			event = nil
		}
	}()
	raw := data.Vals
	allowed, err := a.ShouldProcessMessage(ctx, raw)
	if err != nil {
		fmt.Printf("[%s] Error building event: %s\n", a.Name(), err.Error())
		return nil
	}
	if !allowed {
		return nil
	}
	msgType := classifyBridgeMessage(data)
	chatType := "dm"
	if pyrepr.Truthy(data.GetOr("isGroup", false)) {
		chatType = "group"
	}
	optStr := func(key string) *string {
		v := data.GetOr(key, nil)
		if v == nil {
			return nil
		}
		s := pyrepr.Str(v)
		return &s
	}
	source := a.BuildSource(ctx, pyrepr.Str(data.GetOr("chatId", "")), gateway.SourceFields{ChatName: optStr("chatName"),
		ChatType: chatType, UserID: optStr("senderId"), UserName: optStr("senderName"), MessageID: optStr("messageId")})
	cachedURLs, mediaTypes := a.collectBridgeMedia(ctx, data, msgType)
	body := pyrepr.Str(data.GetOr("body", ""))
	if pyrepr.Truthy(data.GetOr("isGroup", nil)) {
		body = CleanBotMentionText(body, raw)
	}
	if msgType == gateway.MessageTypeVoice && len(cachedURLs) > 0 && pystr.Lower(pystr.Strip(body)) == "[ptt received]" {
		body = ""
	}
	quoted := pyrepr.Truthy(data.GetOr("hasQuotedMessage", nil))
	var rawReplyID any
	if quoted {
		rawReplyID = data.GetOr("quotedMessageId", nil)
		for _, p := range a.quotedMedia(ctx, data, rawReplyID) {
			cachedURLs = append(cachedURLs, p[0])
			mediaTypes = append(mediaTypes, p[1])
		}
	}
	var inlined []bool
	if msgType == gateway.MessageTypeDocument && len(cachedURLs) > 0 {
		body, inlined = a.injectDocumentText(cachedURLs, body)
	}
	metadata := map[string]any{}
	if nt := pystr.Strip(pyrepr.Str(orEmpty(data.GetOr("nativeType", nil)))); nt != "" {
		metadata["whatsapp_native_type"] = nt
	}
	if native, ok := data.GetOr("nativeMetadata", nil).(*pyjson.Object); ok && native.Len() > 0 {
		metadata["whatsapp_native"] = native
	}
	if pyrepr.Truthy(data.GetOr("fromOwner", nil)) {
		metadata["whatsapp_from_owner"] = true
		if !strings.HasPrefix(body, ownerReplyPrefix) {
			body = ownerReplyPrefix + body
		}
	}
	inlinedPtrs := make([]*bool, len(inlined))
	for i := range inlined {
		v := inlined[i]
		inlinedPtrs[i] = &v
	}
	ev := gateway.NewMessageEvent(body)
	ev.MessageType = msgType
	ev.Source = source
	ev.RawMessage = data
	ev.MessageID = optStr("messageId")
	ev.MediaURLs = cachedURLs
	ev.MediaTypes = mediaTypes
	ev.MediaTextInlined = inlinedPtrs
	ev.Metadata = metadata
	if rawReplyID != nil {
		s := pyrepr.Str(rawReplyID)
		ev.ReplyToMessageID = &s
	}
	if qt := pystr.Strip(pyrepr.Str(orEmpty(data.GetOr("quotedText", nil)))); qt != "" {
		ev.ReplyToText = &qt
	}
	if quoted {
		if author := NormalizeWhatsAppID(data.GetOr("quotedParticipant", nil)); author != "" {
			ev.ReplyToAuthorID = &author
		}
		ev.ReplyToIsOwnMessage = MessageIsReplyToBot(raw)
	}
	return ev
}

// Empty string for a falsy value, the value otherwise
func orEmpty(v any) any {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return v
}
