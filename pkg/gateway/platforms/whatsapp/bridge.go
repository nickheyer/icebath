package whatsapp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	_ "modernc.org/sqlite"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Launch arguments of the bridge, the --port, --session, --mode, --pair-only and --pair-json of bridge.js
type bridgeArgs struct {
	Port     int
	Session  string
	Mode     string
	PairOnly bool
	PairJSON bool
}

// Settings the bridge reads from its environment at start
type bridgeConfig struct {
	port              int
	session           string
	mode              string
	pairOnly          bool
	pairJSON          bool
	debug             bool
	forwardOwner      bool
	sendReadReceipts  bool
	dmPolicy          string
	groupPolicy       string
	allowedUsers      map[string]bool
	groupAllowedUsers map[string]bool
	replyPrefix       string
	maxMessageLength  float64
	chunkDelayMs      float64
	sendTimeoutMs     float64
	imageCacheDir     string
	documentCacheDir  string
	audioCacheDir     string
}

const (
	bridgeMaxQueueSize    = 100
	bridgeSessionDBName   = "whatsmeow.db"
	bridgeMediaRetryLimit = 60 * time.Second
)

// Integer prefix of a string the way parseInt reads it, NaN without leading digits
func jsParseInt(raw string) float64 {
	s := strings.TrimLeft(raw, " \t\n\r\v\f")
	end := 0
	if end < len(s) && (s[end] == '-' || s[end] == '+') {
		end++
	}
	digits := end
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == digits {
		return math.NaN()
	}
	n, _ := strconv.ParseFloat(s[:end], 64)
	return n
}

// Whether an env value is one of the enabled spellings bridge.js accepts
func envFlag(env map[string]string, key string) bool {
	v, ok := env[key]
	if !ok {
		return false
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Value of an env key, the default when the key is absent
func envOr(env map[string]string, key, def string) string {
	if v, ok := env[key]; ok {
		return v
	}
	return def
}

// Value of an env key when present and non empty, else the default
func envTruthyOr(env map[string]string, key, def string) string {
	if v := env[key]; v != "" {
		return v
	}
	return def
}

// Bridge settings from its arguments and environment with the bridge.js defaults
func newBridgeConfig(ctx context.Context, args bridgeArgs, env map[string]string) bridgeConfig {
	h := home.Dir(ctx)
	session := args.Session
	if session == "" {
		session = filepath.Join(h, "whatsapp", "session")
	}
	mode := args.Mode
	if mode == "" {
		mode = envTruthyOr(env, "WHATSAPP_MODE", "self-chat")
	}
	prefix := whatsappDefaultReplyPrefix
	if v, ok := env["WHATSAPP_REPLY_PREFIX"]; ok {
		prefix = strings.ReplaceAll(v, `\n`, "\n")
	}
	port := args.Port
	if port == 0 {
		port = 3000
	}
	return bridgeConfig{
		port: port, session: session, mode: mode, pairOnly: args.PairOnly, pairJSON: args.PairJSON,
		debug: envFlag(env, "WHATSAPP_DEBUG"), forwardOwner: envFlag(env, "WHATSAPP_FORWARD_OWNER_MESSAGES"),
		sendReadReceipts:  envFlag(env, "WHATSAPP_SEND_READ_RECEIPTS"),
		dmPolicy:          strings.ToLower(strings.TrimSpace(envTruthyOr(env, "WHATSAPP_DM_POLICY", "open"))),
		groupPolicy:       strings.ToLower(strings.TrimSpace(envTruthyOr(env, "WHATSAPP_GROUP_POLICY", "pairing"))),
		allowedUsers:      parseAllowedUsers(env["WHATSAPP_ALLOWED_USERS"]),
		groupAllowedUsers: parseAllowedUsers(env["WHATSAPP_GROUP_ALLOWED_USERS"]),
		replyPrefix:       prefix,
		maxMessageLength:  jsParseInt(envTruthyOr(env, "WHATSAPP_MAX_MESSAGE_LENGTH", "4096")),
		chunkDelayMs:      jsParseInt(envTruthyOr(env, "WHATSAPP_CHUNK_DELAY_MS", "300")),
		sendTimeoutMs:     jsParseInt(envTruthyOr(env, "WHATSAPP_SEND_TIMEOUT_MS", "60000")),
		imageCacheDir:     envTruthyOr(env, "ICEBATH_IMAGE_CACHE_DIR", filepath.Join(h, "image_cache")),
		documentCacheDir:  envTruthyOr(env, "ICEBATH_DOCUMENT_CACHE_DIR", filepath.Join(h, "document_cache")),
		audioCacheDir:     envTruthyOr(env, "ICEBATH_AUDIO_CACHE_DIR", filepath.Join(h, "audio_cache")),
	}
}

// Message the bridge sent or received, kept for quoting and poll vote decryption
type storedMessage struct {
	info    types.MessageInfo
	message *waE2E.Message
}

// The in-process WhatsApp bridge on whatsmeow serving the bridge.js HTTP API on a loopback port
type bridgeHost struct {
	ctx        context.Context
	cfg        bridgeConfig
	console    *bridgeConsole
	logFile    io.Closer
	scriptHash string
	started    time.Time

	mu               sync.Mutex
	client           *whatsmeow.Client
	container        *sqlstore.Container
	generation       int
	closedGeneration int
	connectionState  string
	messageQueue     []*pyjson.Object
	deliveryQueue    []*pyjson.Object
	server           *http.Server
	cachedVersion    *store.WAVersionContainer
	mediaRetries     map[types.MessageID]chan *events.MediaRetry

	sendMu         sync.Mutex
	recentlySent   *outboundIDTracker
	processedPolls *outboundIDTracker
	messageStore   *boundedLRU[*storedMessage]
	quotedMedia    *boundedLRU[*pyjson.Object]
	inbound        chan *events.Message

	exitOnce sync.Once
	done     chan struct{}
	exitCode int
}

var (
	liveBridgesMu sync.Mutex
	liveBridges   = map[string]*bridgeHost{}
)

// Stops the bridge this process serves for the session, waiting out the grace before forcing it
func stopInProcessBridge(sessionPath string) {
	abs, _ := filepath.Abs(sessionPath)
	liveBridgesMu.Lock()
	b := liveBridges[abs]
	liveBridgesMu.Unlock()
	if b == nil {
		return
	}
	b.Terminate()
	if _, exited := b.Wait(sigtermGrace); !exited {
		b.Kill()
		b.Wait(time.Second)
	}
}

// Starts the bridge in this process with its console on out, the spawn of the bridge.js child
func startBridge(ctx context.Context, args bridgeArgs, out io.Writer, env map[string]string) *bridgeHost {
	cfg := newBridgeConfig(ctx, args, env)
	exe, _ := os.Executable()
	b := &bridgeHost{ctx: ctx, cfg: cfg, console: &bridgeConsole{out: out}, scriptHash: fileContentHash(exe),
		started: time.Now(), connectionState: "disconnected", mediaRetries: map[types.MessageID]chan *events.MediaRetry{},
		recentlySent: newOutboundIDTracker(512), processedPolls: newOutboundIDTracker(512),
		messageStore: newBoundedLRU[*storedMessage](512), quotedMedia: newBoundedLRU[*pyjson.Object](512),
		inbound: make(chan *events.Message, 256), done: make(chan struct{})}
	if c, ok := out.(io.Closer); ok && out != os.Stdout && out != os.Stderr {
		b.logFile = c
	}
	_ = os.MkdirAll(cfg.session, 0o777)
	abs, _ := filepath.Abs(cfg.session)
	liveBridgesMu.Lock()
	liveBridges[abs] = b
	liveBridgesMu.Unlock()
	go b.inboundWorker()
	if cfg.pairOnly {
		if cfg.pairJSON {
			b.emitPairEvent(jsonObj("event", "started", "session", cfg.session))
		} else {
			b.console.log("📱 WhatsApp pairing mode")
			b.console.log("📁 Session: %s", cfg.session)
			b.console.log("")
		}
		go func() {
			if err := b.startSocket(); err != nil {
				b.emitPairEvent(jsonObj("event", "error", "error", err.Error()))
				if !cfg.pairJSON {
					b.console.log("%s", err.Error())
				}
				b.exit(1)
			}
		}()
		return b
	}
	go b.listen()
	return b
}

// Pairs the session by QR code in this process like bridge.js --pair-only and returns the bridge exit code
func RunPairing(ctx context.Context, session, mode string, out io.Writer, pairJSON bool) int {
	return RunPairingEnv(ctx, session, mode, nil, out, pairJSON)
}

// Pairing run whose bridge sees the process environment with the overrides applied on top
func RunPairingEnv(ctx context.Context, session, mode string, overrides map[string]string, out io.Writer, pairJSON bool) int {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	for k, v := range overrides {
		env[k] = v
	}
	b := startBridge(ctx, bridgeArgs{Session: session, Mode: mode, PairOnly: true, PairJSON: pairJSON}, out, env)
	select {
	case <-b.done:
	case <-ctx.Done():
		b.Terminate()
		<-b.done
	}
	return b.exitCode
}

// Binds the loopback HTTP server, a FATAL line and exit 1 when the port is taken
func (b *bridgeHost) listen() {
	ln, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(b.cfg.port)))
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) || strings.Contains(err.Error(), "address already in use") || strings.Contains(err.Error(), "Only one usage") {
			b.console.log("FATAL: port %d already in use on 127.0.0.1 — another bridge or process is listening there.", b.cfg.port)
		} else {
			b.console.log("FATAL: bridge HTTP server error: %s", err.Error())
		}
		b.exit(1)
		return
	}
	srv := &http.Server{Handler: b.httpHandler(), ReadHeaderTimeout: 30 * time.Second}
	b.mu.Lock()
	b.server = srv
	b.mu.Unlock()
	b.console.log("🌉 WhatsApp bridge listening on port %d (mode: %s)", b.cfg.port, b.cfg.mode)
	b.console.log("📁 Session stored in: %s", b.cfg.session)
	switch {
	case len(b.cfg.allowedUsers) > 0:
		users := make([]string, 0, len(b.cfg.allowedUsers))
		for _, u := range sortedKeys(b.cfg.allowedUsers) {
			users = append(users, u)
		}
		b.console.log("🔒 Allowed users: %s", strings.Join(users, ", "))
	case b.cfg.mode == "self-chat":
		b.console.log("🔒 Self-chat mode — only your own messages to yourself are processed.")
	case b.cfg.mode == "bot" && b.cfg.dmPolicy == "pairing":
		b.console.log("🤝 WHATSAPP_DM_POLICY=pairing — unknown DMs are forwarded for gateway pairing.")
	default:
		b.console.log("🔒 No WHATSAPP_ALLOWED_USERS set — incoming messages are rejected.")
		b.console.log("   Set WHATSAPP_ALLOWED_USERS=<phone> to authorize specific users,")
		b.console.log("   or WHATSAPP_ALLOWED_USERS=* for an explicit open bot.")
	}
	if b.cfg.mode == "bot" && b.cfg.forwardOwner {
		b.console.log("👤 WHATSAPP_FORWARD_OWNER_MESSAGES=true — owner-typed messages will be forwarded with fromOwner:true")
	}
	b.console.log("")
	b.scheduleReconnect(0)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		b.console.log("FATAL: bridge HTTP server error: %s", err.Error())
		b.exit(1)
	}
}

// Sorted members of a set
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Pid of the process hosting the bridge
func (b *bridgeHost) Pid() int { return os.Getpid() }

// Exit code once the bridge stopped, false while it runs
func (b *bridgeHost) Poll() (int, bool) {
	select {
	case <-b.done:
		return b.exitCode, true
	default:
		return 0, false
	}
}

// Stops the bridge the way SIGTERM stops the node bridge, exit code -15
func (b *bridgeHost) Terminate() { go b.exit(-15) }

// Stops the bridge at once the way SIGKILL does, exit code -9
func (b *bridgeHost) Kill() { b.exit(-9) }

// Waits up to the timeout for the bridge to stop and returns its exit code
func (b *bridgeHost) Wait(timeout time.Duration) (int, bool) {
	select {
	case <-b.done:
		return b.exitCode, true
	case <-time.After(timeout):
		return 0, false
	}
}

// Stops the server, the socket and the store and records the exit code, once
func (b *bridgeHost) exit(code int) {
	b.exitOnce.Do(func() {
		b.mu.Lock()
		srv, client, container := b.server, b.client, b.container
		b.server, b.client, b.container = nil, nil, nil
		b.connectionState = "disconnected"
		b.mu.Unlock()
		if srv != nil {
			_ = srv.Close()
		}
		if client != nil {
			client.Disconnect()
		}
		if container != nil {
			_ = container.Close()
		}
		abs, _ := filepath.Abs(b.cfg.session)
		liveBridgesMu.Lock()
		if liveBridges[abs] == b {
			delete(liveBridges, abs)
		}
		liveBridgesMu.Unlock()
		if b.logFile != nil {
			_ = b.logFile.Close()
		}
		b.exitCode = code
		close(b.done)
	})
}

// Whether the bridge stopped
func (b *bridgeHost) exited() bool {
	_, done := b.Poll()
	return done
}

// Pair event JSON line with its timestamp, only in --pair-json mode
func (b *bridgeHost) emitPairEvent(event *pyjson.Object) {
	if !b.cfg.pairJSON {
		return
	}
	line := jsonObj("ts", time.Now().UnixMilli())
	for _, k := range event.Keys {
		line.Set(k, event.Vals[k])
	}
	b.console.jsonLine(line)
}

// Debug JSON line, only with WHATSAPP_DEBUG on
func (b *bridgeHost) emitDebugEvent(kv ...any) {
	if !b.cfg.debug {
		return
	}
	b.console.jsonLine(jsonObj(append([]any{"event", "debug"}, kv...)...))
}

// Runs a socket start after the delay, retrying every 5s while starting fails
func (b *bridgeHost) scheduleReconnect(delayMs int) {
	time.AfterFunc(time.Duration(delayMs)*time.Millisecond, func() {
		if b.exited() {
			return
		}
		if err := b.startSocket(); err != nil {
			b.console.log("⚠️  Reconnect failed (%s). Retrying in %ds...", err.Error(), 5)
			b.scheduleReconnect(5000)
		}
	})
}

// Latest WhatsApp Web version bounded to 15s, the last good one or the library default on failure
func (b *bridgeHost) resolveVersion() *store.WAVersionContainer {
	ctx, cancel := context.WithTimeout(b.ctx, 15*time.Second)
	defer cancel()
	version, err := whatsmeow.GetLatestVersion(ctx, http.DefaultClient)
	b.mu.Lock()
	defer b.mu.Unlock()
	if err != nil {
		reason := err.Error()
		if ctx.Err() != nil {
			reason = "version fetch timed out"
		}
		which := "library default"
		if b.cachedVersion != nil {
			which = "cached version"
		}
		b.console.log("⚠️  Baileys version fetch failed (%s); using %s.", reason, which)
		return b.cachedVersion
	}
	b.cachedVersion = version
	return version
}

// Session store of the bridge, opened once per bridge
func (b *bridgeHost) openContainer() (*sqlstore.Container, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.container != nil {
		return b.container, nil
	}
	dsn := "file:" + filepath.Join(b.cfg.session, bridgeSessionDBName) + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	container := sqlstore.NewWithDB(db, "sqlite", waLog.Noop)
	if err := container.Upgrade(b.ctx); err != nil {
		db.Close()
		return nil, err
	}
	b.container = container
	return container, nil
}

// Connects a fresh socket on the stored device, opening a QR pairing when no device is linked
func (b *bridgeHost) startSocket() error {
	container, err := b.openContainer()
	if err != nil {
		return err
	}
	device, err := container.GetFirstDevice(b.ctx)
	if err != nil {
		return err
	}
	if version := b.resolveVersion(); version != nil {
		store.SetWAVersion(*version)
	}
	store.SetOSInfo("Icebath Agent", [3]uint32{120, 0, 0})
	store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_CHROME.Enum()
	store.DeviceProps.RequireFullSync = new(bool)
	device.LIDs = lidFileStore{LIDStore: device.LIDs, dir: b.cfg.session}
	client := whatsmeow.NewClient(device, &bridgeWALogger{console: b.console, module: "whatsmeow"})
	client.EnableAutoReconnect = false
	client.DisableLoginAutoReconnect = true
	client.AutomaticMessageRerequestFromPhone = true
	b.mu.Lock()
	if b.client != nil {
		old := b.client
		defer old.Disconnect()
	}
	b.generation++
	gen := b.generation
	b.client = client
	b.mu.Unlock()
	client.AddEventHandler(func(evt any) { b.handleEvent(gen, client, evt) })
	if device.ID == nil {
		if _, err := os.Stat(filepath.Join(b.cfg.session, "creds.json")); err == nil {
			b.handleClose(gen, 401)
			return nil
		}
		qr, err := client.GetQRChannel(b.ctx)
		if err != nil {
			return err
		}
		go b.consumeQR(gen, qr)
	}
	return client.ConnectContext(b.ctx)
}

// Prints each pairing QR code and maps the pairing outcomes onto connection closes
func (b *bridgeHost) consumeQR(gen int, ch <-chan whatsmeow.QRChannelItem) {
	for item := range ch {
		switch {
		case item.Event == whatsmeow.QRChannelEventCode:
			if b.cfg.pairJSON {
				b.emitPairEvent(jsonObj("event", "qr", "qr", item.Code))
				continue
			}
			b.console.log("\n📱 Scan this QR code with WhatsApp on your phone:\n")
			b.console.mu.Lock()
			qrterminal.GenerateWithConfig(item.Code, qrterminal.Config{Level: qrterminal.L, Writer: b.console.out, HalfBlocks: true,
				BlackChar: qrterminal.BLACK_BLACK, WhiteBlackChar: qrterminal.WHITE_BLACK, WhiteChar: qrterminal.WHITE_WHITE,
				BlackWhiteChar: qrterminal.BLACK_WHITE, QuietZone: 1})
			b.console.mu.Unlock()
			b.console.log("\nWaiting for scan...\n")
		case item == whatsmeow.QRChannelTimeout:
			b.handleClose(gen, 408)
		case item == whatsmeow.QRChannelClientOutdated:
			b.handleClose(gen, 405)
		case item == whatsmeow.QRChannelScannedWithoutMultidevice:
			b.handleClose(gen, 411)
		case item.Event == whatsmeow.QRChannelEventError, item == whatsmeow.QRChannelErrUnexpectedEvent:
			b.handleClose(gen, 500)
		}
	}
}

// Connection close of one socket generation, exit on logout and a 1s or 3s reconnect otherwise
func (b *bridgeHost) handleClose(gen int, reason int) {
	b.mu.Lock()
	if gen != b.generation || b.closedGeneration == gen {
		b.mu.Unlock()
		return
	}
	b.closedGeneration = gen
	b.connectionState = "disconnected"
	b.mu.Unlock()
	if reason == 401 {
		b.emitPairEvent(jsonObj("event", "error", "error", "logged_out", "reason", reason))
		if !b.cfg.pairJSON {
			b.console.log("❌ Logged out. Delete session and restart to re-authenticate.")
		}
		b.exit(1)
		return
	}
	b.emitPairEvent(jsonObj("event", "disconnected", "reason", reason))
	if !b.cfg.pairJSON {
		if reason == 515 {
			b.console.log("↻ WhatsApp requested restart (code 515). Reconnecting...")
		} else {
			b.console.log("⚠️  Connection closed (reason: %d). Reconnecting in 3s...", reason)
		}
	}
	if reason == 515 {
		b.scheduleReconnect(1000)
	} else {
		b.scheduleReconnect(3000)
	}
}

// Socket events of one generation routed to the connection, message and receipt handlers
func (b *bridgeHost) handleEvent(gen int, client *whatsmeow.Client, raw any) {
	switch evt := raw.(type) {
	case *events.Connected:
		b.onOpen(gen, client)
	case *events.PairSuccess:
		b.writeCredsIdentity(client)
	case *events.ManualLoginReconnect:
		b.handleClose(gen, 515)
	case *events.LoggedOut:
		b.handleClose(gen, 401)
	case *events.StreamReplaced:
		b.handleClose(gen, 440)
	case *events.TemporaryBan:
		b.handleClose(gen, 402)
	case *events.ClientOutdated:
		b.handleClose(gen, 405)
	case *events.ConnectFailure:
		if evt.Reason.IsLoggedOut() {
			b.handleClose(gen, 401)
		} else {
			b.handleClose(gen, int(evt.Reason))
		}
	case *events.StreamError:
		code, err := strconv.Atoi(evt.Code)
		if err != nil {
			code = 500
		}
		b.handleClose(gen, code)
	case *events.CATRefreshError:
		b.handleClose(gen, 500)
	case *events.KeepAliveTimeout:
		client.Disconnect()
		b.handleClose(gen, 408)
	case *events.Disconnected:
		b.handleClose(gen, 428)
	case *events.Receipt:
		b.onReceipt(evt)
	case *events.MediaRetry:
		b.mu.Lock()
		ch := b.mediaRetries[evt.MessageID]
		b.mu.Unlock()
		if ch != nil {
			select {
			case ch <- evt:
			default:
			}
		}
	case *events.Message:
		select {
		case b.inbound <- evt:
		case <-b.done:
		}
	}
}

// Marks the connection open, records the linked identity and finishes a pair-only run
func (b *bridgeHost) onOpen(gen int, client *whatsmeow.Client) {
	b.mu.Lock()
	if gen != b.generation {
		b.mu.Unlock()
		return
	}
	b.connectionState = "connected"
	b.mu.Unlock()
	b.writeCredsIdentity(client)
	var user any
	if client.Store.ID != nil {
		name := any(nil)
		if client.Store.PushName != "" {
			name = client.Store.PushName
		} else if client.Store.BusinessName != "" {
			name = client.Store.BusinessName
		}
		user = jsonObj("id", client.Store.ID.String(), "name", name)
	}
	b.emitPairEvent(jsonObj("event", "connected", "user", user))
	if !b.cfg.pairJSON {
		b.console.log("✅ WhatsApp connected!")
	}
	if b.cfg.pairOnly {
		if !b.cfg.pairJSON {
			b.console.log("✅ Pairing complete. Credentials saved.")
		}
		time.AfterFunc(2*time.Second, func() { b.exit(0) })
	}
}

// Writes creds.json with the linked account identity, the pairing marker every caller checks
func (b *bridgeHost) writeCredsIdentity(client *whatsmeow.Client) {
	if client.Store.ID == nil {
		return
	}
	me := jsonObj("id", client.Store.ID.String())
	if !client.Store.LID.IsEmpty() {
		me.Set("lid", client.Store.LID.String())
	}
	if client.Store.PushName != "" {
		me.Set("name", client.Store.PushName)
	}
	text, err := pyjson.Dumps(jsonObj("me", me, "registered", true, "platform", "whatsmeow"), pyjson.Indent(2), pyjson.EnsureASCII(false))
	if err != nil {
		return
	}
	path := filepath.Join(b.cfg.session, "creds.json")
	tmp := path + ".tmp"
	if os.WriteFile(tmp, []byte(text), 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}

// Socket client and connection state, nil client when the bridge has none
func (b *bridgeHost) socket() (*whatsmeow.Client, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.client, b.connectionState
}

// Queues an inbound event for the next poll, dropping the oldest past the bound
func (b *bridgeHost) pushMessage(event *pyjson.Object) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.messageQueue = append(b.messageQueue, event)
	n := len(b.messageQueue)
	if n > bridgeMaxQueueSize {
		b.messageQueue = b.messageQueue[1:]
	}
	return n
}

// Queues a delivery status event under its own bound
func (b *bridgeHost) pushDelivery(event *pyjson.Object) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deliveryQueue = append(b.deliveryQueue, event)
	if len(b.deliveryQueue) > bridgeMaxQueueSize {
		b.deliveryQueue = b.deliveryQueue[1:]
	}
}

// Drains both queues, inbound messages first
func (b *bridgeHost) drainQueues() []any {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]any, 0, len(b.messageQueue)+len(b.deliveryQueue))
	for _, e := range b.messageQueue {
		out = append(out, e)
	}
	for _, e := range b.deliveryQueue {
		out = append(out, e)
	}
	b.messageQueue, b.deliveryQueue = nil, nil
	return out
}

// Length of the inbound queue
func (b *bridgeHost) queueLength() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.messageQueue)
}

// Runs a send on the serial queue bounded by the send timeout, the send still running if the timeout wins
func (b *bridgeHost) sendWithTimeout(fn func(ctx context.Context) (string, error)) (string, error) {
	type result struct {
		id  string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		b.sendMu.Lock()
		defer b.sendMu.Unlock()
		id, err := fn(b.ctx)
		ch <- result{id, err}
	}()
	timeout := b.cfg.sendTimeoutMs
	wait := time.Duration(0)
	if !math.IsNaN(timeout) && timeout > 0 {
		wait = time.Duration(timeout * float64(time.Millisecond))
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case r := <-ch:
		return r.id, r.err
	case <-timer.C:
		return "", fmt.Errorf("sendMessage timed out after %ss", jsNumber(timeout/1000))
	}
}

// Sleeps the configured delay between chunks, no delay for a non number
func (b *bridgeHost) chunkDelay() {
	if d := b.cfg.chunkDelayMs; !math.IsNaN(d) && d > 0 {
		time.Sleep(time.Duration(d * float64(time.Millisecond)))
	}
}

// Prefix of self chat outbound text, the text unchanged in bot mode
func (b *bridgeHost) formatOutgoingMessage(message string) string {
	if b.cfg.mode != "self-chat" || b.cfg.replyPrefix == "" {
		return message
	}
	return b.cfg.replyPrefix + message
}

// Chunks of outbound text under the configured length
func (b *bridgeHost) split(message string) []string {
	limit := 0
	if m := b.cfg.maxMessageLength; !math.IsNaN(m) && !math.IsInf(m, 0) {
		limit = int(m)
	}
	return splitLongMessage(message, limit)
}

// Store wrapper that mirrors every LID mapping into the lid-mapping files of the session directory
type lidFileStore struct {
	store.LIDStore
	dir string
}

// Stores the mapping and writes its forward and reverse files
func (s lidFileStore) PutLIDMapping(ctx context.Context, lid, pn types.JID) error {
	if err := s.LIDStore.PutLIDMapping(ctx, lid, pn); err != nil {
		return err
	}
	writeLidMappingFiles(s.dir, lid, pn)
	return nil
}

// Stores the mappings and writes each forward and reverse file
func (s lidFileStore) PutManyLIDMappings(ctx context.Context, mappings []store.LIDMapping) error {
	if err := s.LIDStore.PutManyLIDMappings(ctx, mappings); err != nil {
		return err
	}
	for _, m := range mappings {
		writeLidMappingFiles(s.dir, m.LID, m.PN)
	}
	return nil
}

// Writes lid-mapping-<phone>.json naming the LID and lid-mapping-<lid>_reverse.json naming the phone
func writeLidMappingFiles(dir string, lid, pn types.JID) {
	if lid.User == "" || pn.User == "" {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "lid-mapping-"+pn.User+".json"), []byte(strconv.Quote(lid.User)), 0o666)
	_ = os.WriteFile(filepath.Join(dir, "lid-mapping-"+lid.User+"_reverse.json"), []byte(strconv.Quote(pn.User)), 0o666)
}

// Library logger writing warnings and errors as pino style JSON lines like the Baileys warn logger
type bridgeWALogger struct {
	console *bridgeConsole
	module  string
}

func (l *bridgeWALogger) line(level int, msg string, args ...any) {
	host, _ := os.Hostname()
	l.console.jsonLine(jsonObj("level", level, "time", time.Now().UnixMilli(), "pid", os.Getpid(), "hostname", host,
		"module", l.module, "msg", fmt.Sprintf(msg, args...)))
}

func (l *bridgeWALogger) Warnf(msg string, args ...any)  { l.line(40, msg, args...) }
func (l *bridgeWALogger) Errorf(msg string, args ...any) { l.line(50, msg, args...) }
func (l *bridgeWALogger) Infof(string, ...any)           {}
func (l *bridgeWALogger) Debugf(string, ...any)          {}
func (l *bridgeWALogger) Sub(module string) waLog.Logger {
	return &bridgeWALogger{console: l.console, module: l.module + "/" + module}
}
