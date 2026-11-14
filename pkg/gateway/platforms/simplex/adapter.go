package simplex

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"log/slog"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	_ "image/gif"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pypil"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/internal/pywebsockets"
)

const (
	maxMessageLength          = 8000
	wsRetryDelayInitial       = 2.0
	wsRetryDelayMax           = 60.0
	healthCheckInterval       = 30.0
	healthCheckStaleThreshold = 300.0
	corrPrefix                = "icebath-"
	thumbURIPrefix            = "data:image/jpg;base64,"
	defaultWSURL              = "ws://127.0.0.1:5225"
	maxPendingCorr            = 200
)

var (
	imageExts    = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true}
	audioExts    = map[string]bool{".mp3": true, ".wav": true, ".ogg": true, ".m4a": true, ".aac": true, ".opus": true}
	voiceTagExts = map[string]bool{".ogg": true, ".mp3": true, ".wav": true, ".m4a": true, ".opus": true}
	// Message content types whose text field carries the body
	textBearingTypes = map[string]bool{"text": true, "file": true, "image": true, "voice": true, "link": true, "video": true}
	// Mime prefix and message type pairs, the first matching pair naming the event type
	mediaKindPrecedence = []struct {
		prefix string
		kind   gateway.MessageType
	}{{"audio/", gateway.MessageTypeVoice}, {"image/", gateway.MessageTypePhoto}}

	mediaTagPathsRE = pyre.MustCompile(`MEDIA:(\S+)`)
	mediaTagRE      = pyre.MustCompile(`MEDIA:\S+`)
)

// Wall clock seconds since the epoch
func nowSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Stripped non blank members of a comma separated value
func parseCommaList(value string) []string {
	out := []string{}
	for _, v := range strings.Split(value, ",") {
		if s := pystr.Strip(v); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Home scope and allowlist name sets already warned about, kept for the process
var nameAllowlistWarned = struct {
	sync.Mutex
	seen map[string]bool
}{seen: map[string]bool{}}

// Warns once per profile and value about allowlist entries that are not numeric contact ids
func warnNameAllowlistEntries(ctx context.Context) {
	raw := gateway.DecodeJSONListLiteral(gateway.PlatformGateEnv(ctx, "SIMPLEX_ALLOWED_USERS", ""))
	var entries []string
	if list, ok := raw.([]any); ok {
		for _, e := range list {
			entries = append(entries, pystr.Strip(pyrepr.Str(e)))
		}
	} else {
		entries = parseCommaList(pyrepr.Str(raw))
	}
	names := []any{}
	unique := map[string]bool{}
	for _, u := range entries {
		if u != "" && u != "*" && !pystr.StrIsDigit(u) {
			names = append(names, u)
			unique[u] = true
		}
	}
	if len(names) == 0 {
		return
	}
	sorted := make([]string, 0, len(unique))
	for u := range unique {
		sorted = append(sorted, u)
	}
	sort.Strings(sorted)
	key := gateway.CurrentScopeKey(ctx) + "\x00" + strings.Join(sorted, "\x00")
	nameAllowlistWarned.Lock()
	seen := nameAllowlistWarned.seen[key]
	nameAllowlistWarned.seen[key] = true
	nameAllowlistWarned.Unlock()
	if seen {
		return
	}
	slog.Warn(fmt.Sprintf("SimpleX: SIMPLEX_ALLOWED_USERS entries %s are not numeric contactIds and are ignored "+
		"(display names are not trusted; see /contacts for IDs)", pyrepr.Repr(names)))
}

// Id with its middle masked, short ids whole and an empty id as a marker
func redactID(contactID any) string {
	if !pyrepr.Truthy(contactID) {
		return "<none>"
	}
	rs := []rune(pyrepr.Str(contactID))
	if len(rs) <= 4 {
		return string(rs)
	}
	return string(rs[:2]) + "**" + string(rs[len(rs)-2:])
}

func isImageExt(ext string) bool { return imageExts[pystr.Lower(ext)] }

func isAudioExt(ext string) bool { return audioExts[pystr.Lower(ext)] }

// Mime type of an image or audio extension, octet stream for anything else
func mimeForExt(ext string) string {
	if isImageExt(ext) {
		return "image/" + pystr.LstripChars(ext, ".")
	}
	if isAudioExt(ext) {
		return "audio/" + pystr.LstripChars(ext, ".")
	}
	return "application/octet-stream"
}

// AttributeError an attribute read raises on a value of another type
func attributeError(v any, attr string) error {
	return &gateway.PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute '%s'", pyrepr.TypeName(v), attr)}
}

// dict get of a decoded JSON value, the AttributeError a non dict raises
func dictGet(v any, key string, def any) (any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, attributeError(v, "get")
	}
	if val, ok := m[key]; ok {
		return val, nil
	}
	return def, nil
}

// Value under the key, an empty dict when it is missing or falsy
func dictOrEmpty(v any, key string) (any, error) {
	val, err := dictGet(v, key, nil)
	if err != nil {
		return nil, err
	}
	if !pyrepr.Truthy(val) {
		return map[string]any{}, nil
	}
	return val, nil
}

// Value under the key when the holder is a dict, nil otherwise
func getIfDict(v any, key string, def any) any {
	if m, ok := v.(map[string]any); ok {
		if val, ok := m[key]; ok {
			return val
		}
		return def
	}
	return nil
}

// Local display name of a contact, member or group, else the display name of its nested profile
func displayName(obj any, profileKey string, def any) (any, error) {
	local, err := dictGet(obj, "localDisplayName", "")
	if err != nil {
		return nil, err
	}
	if pyrepr.Truthy(local) {
		return local, nil
	}
	profile, err := dictOrEmpty(obj, profileKey)
	if err != nil {
		return nil, err
	}
	return dictGet(profile, "displayName", def)
}

// Structured send command addressing the chat by id with the items as JSON
func sendCmd(chatID string, items []any) (string, error) {
	target := "@" + chatID
	if strings.HasPrefix(chatID, "group:") {
		target = "#" + chatID[6:]
	}
	encoded, err := pyjson.Dumps(items)
	if err != nil {
		return "", err
	}
	return "/_send " + target + " json " + encoded, nil
}

// Awaited daemon reply of one command, settled by the listener or cancelled at disconnect
type pendingResponse struct {
	result    chan any
	cancelled chan struct{}
	once      sync.Once
}

// SimpleX Chat adapter over the simplex-chat daemon WebSocket API
type SimplexAdapter struct {
	*gateway.BasePlatformAdapter

	wsURL          string
	autoAccept     bool
	groupAllowFrom map[string]bool

	mu                   sync.Mutex
	ws                   *pywebsockets.Conn
	wsTask               *gateway.AdapterTask
	healthTask           *gateway.AdapterTask
	lastWSActivity       float64
	pendingCorrIDs       map[string]bool
	pendingCorrOrder     []string
	pendingFileTransfers map[string]any
	pendingResponses     map[string]*pendingResponse
	corrCounter          int
}

// Class attributes the SimpleX adapter declares over the base defaults
func simplexAdapterTraits() gateway.AdapterTraits {
	traits := gateway.DefaultAdapterTraits()
	traits.MaxMessageLength = maxMessageLength
	traits.SerialReceiveLoop = true
	return traits
}

// Adapter for a platform config reading the daemon URL from extra and the accept and group policy from env then extra
func NewSimplexAdapter(ctx context.Context, config *gateway.PlatformConfig) (*SimplexAdapter, error) {
	if config == nil {
		config = gateway.NewPlatformConfig()
	}
	a := &SimplexAdapter{pendingCorrIDs: map[string]bool{}, pendingFileTransfers: map[string]any{},
		pendingResponses: map[string]*pendingResponse{}}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, gateway.Platform("simplex"), a)
	a.Traits = simplexAdapterTraits()
	extra := config.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	var wsURL any = defaultWSURL
	if v, ok := extra["ws_url"]; ok {
		wsURL = v
	}
	wsText, ok := wsURL.(string)
	if !ok {
		return nil, attributeError(wsURL, "rstrip")
	}
	a.wsURL = pystr.RstripChars(wsText, "/")
	envAuto, found, err := gateway.LookupScopedSecret(ctx, "SIMPLEX_AUTO_ACCEPT", false)
	if err != nil {
		return nil, err
	}
	if found {
		switch pystr.Lower(pystr.Strip(envAuto)) {
		case "0", "false", "no", "":
			a.autoAccept = false
		default:
			a.autoAccept = true
		}
	} else if v, ok := extra["auto_accept"]; ok {
		a.autoAccept = pyrepr.Truthy(v)
	} else {
		a.autoAccept = true
	}
	groupEnv, err := gateway.GetScopedSecret(ctx, "SIMPLEX_GROUP_ALLOWED", "", false)
	if err != nil {
		return nil, err
	}
	var groupAllowed any = groupEnv
	if groupEnv == "" {
		groupAllowed = ""
		if v, ok := extra["group_allowed"]; ok {
			groupAllowed = v
		}
	}
	groupText, ok := groupAllowed.(string)
	if !ok {
		return nil, attributeError(groupAllowed, "split")
	}
	a.groupAllowFrom = map[string]bool{}
	for _, g := range parseCommaList(groupText) {
		a.groupAllowFrom[g] = true
	}
	delayText := "0.8"
	if v, ok := os.LookupEnv("ICEBATH_SIMPLEX_TEXT_BATCH_DELAY"); ok {
		delayText = v
	}
	delay, err := pyint.ParseFloat(delayText)
	if pe, ok := err.(*pyint.Error); ok {
		return nil, &gateway.PyError{Kind: pe.Kind, Msg: pe.Msg}
	} else if err != nil {
		return nil, err
	}
	a.SetTextBatchDelays(delay, delay)
	groups := "disabled"
	if len(a.groupAllowFrom) > 0 {
		groups = "enabled"
	}
	slog.Info(fmt.Sprintf("SimpleX adapter initialized: url=%s auto_accept=%s groups=%s", a.wsURL, pyrepr.Str(a.autoAccept), groups))
	return a, nil
}

// Probes the daemon, then starts the listener and the idle monitor and marks the adapter connected
func (a *SimplexAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	if a.wsURL == "" {
		slog.Error("SimpleX: SIMPLEX_WS_URL is required")
		return false, nil
	}
	warnNameAllowlistEntries(ctx)
	probe, err := pywebsockets.Connect(ctx, a.wsURL, pywebsockets.ConnectOptions{OpenTimeout: 10 * time.Second})
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		slog.Error(fmt.Sprintf("SimpleX: cannot reach daemon at %s: %s", a.wsURL, err.Error()))
		return false, nil
	}
	probe.Close()
	a.SetRunning(true)
	a.mu.Lock()
	a.lastWSActivity = nowSeconds()
	a.wsTask = gateway.SpawnAdapterTask(ctx, a.wsListener)
	a.healthTask = gateway.SpawnAdapterTask(ctx, a.healthMonitor)
	a.mu.Unlock()
	a.MarkConnected(ctx, "")
	slog.Info(fmt.Sprintf("SimpleX: connected to %s", a.wsURL))
	a.WirePluginHandlers(ctx, nil)
	return true, nil
}

// Stops both tasks, closes the socket, cancels pending batches and replies and marks the adapter disconnected
func (a *SimplexAdapter) Disconnect(ctx context.Context) error {
	a.SetRunning(false)
	a.mu.Lock()
	wsTask, healthTask := a.wsTask, a.healthTask
	a.mu.Unlock()
	gateway.CancelTask(ctx, wsTask)
	gateway.CancelTask(ctx, healthTask)
	a.mu.Lock()
	ws := a.ws
	a.ws = nil
	a.mu.Unlock()
	if ws != nil {
		ws.Close()
	}
	for _, task := range a.TextBatchTasks() {
		if !task.Done() {
			task.Cancel()
		}
	}
	a.mu.Lock()
	pending := a.pendingResponses
	a.pendingResponses = map[string]*pendingResponse{}
	a.mu.Unlock()
	for _, p := range pending {
		p.once.Do(func() { close(p.cancelled) })
	}
	a.ClearTextBatches()
	a.MarkDisconnected(ctx)
	slog.Info("SimpleX: disconnected")
	return nil
}

// Keeps one daemon socket open, dispatching every event and reconnecting with jittered backoff
func (a *SimplexAdapter) wsListener(ctx context.Context) error {
	backoff := wsRetryDelayInitial
	for a.IsConnected() {
		slog.Debug(fmt.Sprintf("SimpleX WS: connecting to %s", a.wsURL))
		ws, err := pywebsockets.Connect(ctx, a.wsURL, pywebsockets.ConnectOptions{PingInterval: 20 * time.Second,
			PingTimeout: 20 * time.Second, CloseTimeout: 10 * time.Second})
		if err == nil {
			a.mu.Lock()
			a.ws = ws
			a.lastWSActivity = nowSeconds()
			a.mu.Unlock()
			backoff = wsRetryDelayInitial
			slog.Info("SimpleX WS: connected")
			err = a.readEvents(ctx, ws)
			ws.Close()
		}
		a.mu.Lock()
		a.ws = nil
		a.mu.Unlock()
		if ctx.Err() != nil {
			return nil
		}
		var closed *pywebsockets.ConnectionClosed
		switch {
		case err == nil || !a.IsConnected():
		case errors.As(err, &closed):
			slog.Warn(fmt.Sprintf("SimpleX WS: connection closed: %s (reconnecting in %.0fs)", closed.Error(), backoff))
		default:
			slog.Warn(fmt.Sprintf("SimpleX WS: unexpected error: %s (reconnecting in %.0fs)", err.Error(), backoff))
		}
		if a.IsConnected() {
			timer := time.NewTimer(time.Duration((backoff + backoff*0.2*rand.Float64()) * float64(time.Second)))
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
			backoff = min(backoff*2, wsRetryDelayMax)
		}
	}
	return nil
}

// Handles every frame of one socket until the adapter stops or the connection ends, a clean close ending quietly
func (a *SimplexAdapter) readEvents(ctx context.Context, ws *pywebsockets.Conn) error {
	for {
		raw, err := ws.Recv(ctx)
		if err != nil {
			if pywebsockets.IsClosedOK(err) {
				return nil
			}
			return err
		}
		if !a.IsConnected() {
			return nil
		}
		a.mu.Lock()
		a.lastWSActivity = nowSeconds()
		a.mu.Unlock()
		var parsed any
		var perr error
		switch frame := raw.(type) {
		case string:
			parsed, perr = pyjson.Loads(frame)
		case []byte:
			parsed, perr = pyjson.LoadsBytes(frame)
		}
		if perr == nil {
			perr = a.handleEvent(ctx, pyjson.Plain(parsed))
		}
		if perr == nil {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var decodeErr *pyjson.DecodeError
		if errors.As(perr, &decodeErr) {
			slog.Debug(fmt.Sprintf("SimpleX WS: invalid JSON: %s", pystr.Slice(pyrepr.Str(raw), 0, 100)))
			continue
		}
		slog.Error("SimpleX WS: error handling event", "error", perr)
	}
}

// Logs application idleness of the socket, never reconnecting on it
func (a *SimplexAdapter) healthMonitor(ctx context.Context) error {
	for a.IsConnected() {
		timer := time.NewTimer(time.Duration(healthCheckInterval * float64(time.Second)))
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
		if !a.IsConnected() {
			break
		}
		a.mu.Lock()
		elapsed := nowSeconds() - a.lastWSActivity
		a.mu.Unlock()
		if elapsed > healthCheckStaleThreshold {
			slog.Debug(fmt.Sprintf("SimpleX: WS application-idle for %.0fs", elapsed))
		}
	}
	return nil
}

// TypeError a dict lookup raises for a list or dict key
func unhashable(v any) error {
	switch v.(type) {
	case []any, map[string]any:
		return &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("unhashable type: '%s'", pyrepr.TypeName(v))}
	}
	return nil
}

// Settles an awaited reply, drops the echo of an own command, else dispatches the event by its type
func (a *SimplexAdapter) handleEvent(ctx context.Context, event any) error {
	respValue, err := dictGet(event, "resp", nil)
	if err != nil {
		return err
	}
	resp := event
	if _, ok := respValue.(map[string]any); ok {
		resp = respValue
	}
	corrID := event.(map[string]any)["corrId"]
	if pyrepr.Truthy(corrID) {
		if err := unhashable(corrID); err != nil {
			return err
		}
		if id, ok := corrID.(string); ok {
			a.mu.Lock()
			pending := a.pendingResponses[id]
			delete(a.pendingResponses, id)
			a.mu.Unlock()
			if pending != nil {
				select {
				case pending.result <- resp:
				default:
				}
				return nil
			}
			if strings.HasPrefix(id, corrPrefix) {
				a.mu.Lock()
				a.discardCorrID(id)
				a.mu.Unlock()
				return nil
			}
		}
	}
	respType, err := dictGet(resp, "type", nil)
	if err != nil {
		return err
	}
	if !pyrepr.Truthy(respType) {
		respType = getIfDict(event, "type", "")
	}
	if err := unhashable(respType); err != nil {
		return err
	}
	name, _ := respType.(string)
	switch name {
	case "contactRequest":
		return a.onContactRequest(ctx, resp)
	case "rcvFileDescrReady":
		return a.onRcvFileDescrReady(ctx, resp)
	case "newChatItems":
		return a.onNewChatItems(ctx, resp)
	case "newChatItem":
		return a.onNewChatItem(ctx, resp)
	case "rcvFileComplete":
		return a.onRcvFileComplete(ctx, resp)
	}
	if pyrepr.Truthy(respType) {
		slog.Debug(fmt.Sprintf("SimpleX: unhandled event type: %s", pyrepr.Str(respType)))
	}
	return nil
}

// Accepts an incoming contact request when auto accept is on
func (a *SimplexAdapter) onContactRequest(ctx context.Context, resp any) error {
	if !a.autoAccept {
		slog.Debug("SimpleX: unhandled event type: contactRequest")
		return nil
	}
	request, err := dictOrEmpty(resp, "contactRequest")
	if err != nil {
		return err
	}
	contactReqID, err := dictGet(request, "contactRequestId", nil)
	if err != nil {
		return err
	}
	if contactReqID != nil {
		slog.Info(fmt.Sprintf("SimpleX: auto-accepting contact request %s", redactID(pyrepr.Str(contactReqID))))
		if _, err := a.sendCommand(ctx, "/accept "+pyrepr.Str(contactReqID), 30.0); err != nil {
			return err
		}
	}
	return nil
}

// Starts the download of an XFTP file as soon as its description is ready
func (a *SimplexAdapter) onRcvFileDescrReady(ctx context.Context, resp any) error {
	rcvFile, err := dictOrEmpty(resp, "rcvFileTransfer")
	if err != nil {
		return err
	}
	if fileID := getIfDict(rcvFile, "fileId", nil); fileID != nil {
		slog.Debug(fmt.Sprintf("SimpleX: rcvFileDescrReady for fileId=%s — sending /freceive", pyrepr.Str(fileID)))
		return a.sendFireAndForget(ctx, "/freceive "+pyrepr.Str(fileID))
	}
	return nil
}

// Handles every chat item of a batch event
func (a *SimplexAdapter) onNewChatItems(ctx context.Context, resp any) error {
	chatItems, err := dictGet(resp, "chatItems", nil)
	if err != nil {
		return err
	}
	if !pyrepr.Truthy(chatItems) {
		return nil
	}
	items, ok := chatItems.([]any)
	if !ok {
		items = []any{chatItems}
	}
	for _, item := range items {
		if err := a.safeHandleChatItem(ctx, item, "SimpleX: error processing chat item"); err != nil {
			return err
		}
	}
	return nil
}

// Handles the single chat item event some daemon versions send
func (a *SimplexAdapter) onNewChatItem(ctx context.Context, resp any) error {
	return a.safeHandleChatItem(ctx, resp, "SimpleX: error processing chat item")
}

// Dict key of a file id, equal numbers sharing one key
func fileKey(fileID any) string {
	if n, ok := pyrepr.Int(fileID); ok {
		return "n:" + n.String()
	}
	return pyrepr.TypeName(fileID) + ":" + pyrepr.Repr(fileID)
}

// Delivers the chat item held back until its file transfer completed
func (a *SimplexAdapter) onRcvFileComplete(ctx context.Context, resp any) error {
	outer, err := dictOrEmpty(resp, "chatItem")
	if err != nil {
		return err
	}
	chatItemData, err := dictOrEmpty(outer, "chatItem")
	if err != nil {
		return err
	}
	fileInfo, err := dictOrEmpty(chatItemData, "file")
	if err != nil {
		return err
	}
	fileID := getIfDict(fileInfo, "fileId", nil)
	if fileID == nil {
		return nil
	}
	if err := unhashable(fileID); err != nil {
		return err
	}
	a.mu.Lock()
	pending, ok := a.pendingFileTransfers[fileKey(fileID)]
	delete(a.pendingFileTransfers, fileKey(fileID))
	a.mu.Unlock()
	if !ok {
		return nil
	}
	fileSource, err := dictOrEmpty(fileInfo, "fileSource")
	if err != nil {
		return err
	}
	filePath := getIfDict(fileSource, "filePath", nil)
	if !pyrepr.Truthy(filePath) {
		return nil
	}
	pendingItemData, err := dictOrEmpty(pending, "chatItem")
	if err != nil {
		return err
	}
	itemMap, isMap := pendingItemData.(map[string]any)
	if !isMap {
		return attributeError(pendingItemData, "setdefault")
	}
	file, has := itemMap["file"]
	if !has {
		file = map[string]any{}
		itemMap["file"] = file
	}
	fileMap, isMap := file.(map[string]any)
	if !isMap {
		return &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("'%s' object does not support item assignment", pyrepr.TypeName(file))}
	}
	fileMap["fileSource"] = map[string]any{"filePath": filePath}
	pending.(map[string]any)["chatItem"] = itemMap
	return a.safeHandleChatItem(ctx, pending, "SimpleX: error processing deferred file message")
}

// Handles one chat item, a failure logged and never raised, only task cancellation passed on
func (a *SimplexAdapter) safeHandleChatItem(ctx context.Context, item any, errMsg string) error {
	if err := a.handleChatItem(ctx, item); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Error(errMsg, "error", err)
	}
	return nil
}

// Turns a received direct or allowed group chat item into a message event, deferring voice files still downloading
func (a *SimplexAdapter) handleChatItem(ctx context.Context, chatItem any) error {
	chatInfo, err := dictOrEmpty(chatItem, "chatInfo")
	if err != nil {
		return err
	}
	chatItemData, err := dictOrEmpty(chatItem, "chatItem")
	if err != nil {
		return err
	}
	chatType, err := dictGet(chatInfo, "type", "")
	if err != nil {
		return err
	}
	meta, err := dictOrEmpty(chatItemData, "meta")
	if err != nil {
		return err
	}
	content, err := dictOrEmpty(chatItemData, "content")
	if err != nil {
		return err
	}
	msgContent, err := dictOrEmpty(content, "msgContent")
	if err != nil {
		return err
	}
	itemDirection, err := dictOrEmpty(chatItemData, "chatDir")
	if err != nil {
		return err
	}
	directionType := ""
	if _, ok := itemDirection.(map[string]any); ok {
		directionType, _ = getIfDict(itemDirection, "type", "").(string)
	}
	if directionType == "directSnd" || directionType == "groupSnd" {
		return nil
	}
	if contentType, _ := getIfDict(content, "type", "").(string); contentType != "rcvMsgContent" {
		return nil
	}
	msgTypeStr, _ := getIfDict(msgContent, "type", "").(string)
	var text any = ""
	if textBearingTypes[msgTypeStr] {
		text = getIfDict(msgContent, "text", "")
	}
	if !pyrepr.Truthy(text) && msgTypeStr != "image" && msgTypeStr != "file" && msgTypeStr != "voice" {
		return nil
	}
	isGroup := pyrepr.Equal(chatType, "group")
	var chatID, senderID string
	var chatName, senderName any
	switch {
	case pyrepr.Equal(chatType, "direct"):
		contact, err := dictOrEmpty(chatInfo, "contact")
		if err != nil {
			return err
		}
		contactID, err := dictGet(contact, "contactId", "")
		if err != nil {
			return err
		}
		senderID = pyrepr.Str(contactID)
		chatID = senderID
		if senderName, err = displayName(contact, "profile", ""); err != nil {
			return err
		}
		chatName = senderName
	case isGroup:
		groupInfo, err := dictOrEmpty(chatInfo, "groupInfo")
		if err != nil {
			return err
		}
		groupIDValue, err := dictGet(groupInfo, "groupId", "")
		if err != nil {
			return err
		}
		groupID := pyrepr.Str(groupIDValue)
		chatID = "group:" + groupID
		member, err := dictOrEmpty(itemDirection, "groupMember")
		if err != nil {
			return err
		}
		memberID, err := dictGet(member, "memberId", "")
		if err != nil {
			return err
		}
		senderID = pyrepr.Str(memberID)
		if senderName, err = displayName(member, "memberProfile", ""); err != nil {
			return err
		}
		if chatName, err = displayName(groupInfo, "groupProfile", chatID); err != nil {
			return err
		}
		if len(a.groupAllowFrom) == 0 {
			slog.Debug("SimpleX: ignoring group message (no SIMPLEX_GROUP_ALLOWED)")
			return nil
		}
		if !a.groupAllowFrom["*"] && !a.groupAllowFrom[groupID] {
			slog.Debug(fmt.Sprintf("SimpleX: group %s not in allowlist", redactID(groupID)))
			return nil
		}
	default:
		slog.Debug(fmt.Sprintf("SimpleX: unhandled chat type: %s", pyrepr.Str(chatType)))
		return nil
	}
	if senderID == "" {
		slog.Debug("SimpleX: ignoring message with no sender")
		return nil
	}
	mediaURLs, mediaTypes := []string{}, []string{}
	fileInfoValue, err := dictGet(chatItemData, "file", nil)
	if err != nil {
		return err
	}
	if fileInfo, ok := fileInfoValue.(map[string]any); ok && len(fileInfo) > 0 {
		fileSource, err := dictOrEmpty(fileInfo, "fileSource")
		if err != nil {
			return err
		}
		filePathValue := getIfDict(fileSource, "filePath", nil)
		fileID := fileInfo["fileId"]
		filePath, ext := "", ""
		if pyrepr.Truthy(filePathValue) {
			var isStr bool
			if filePath, isStr = filePathValue.(string); !isStr {
				return &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("expected str, bytes or os.PathLike object, not %s",
					pyrepr.TypeName(filePathValue))}
			}
			ext = pystr.Lower(pypath.PathSuffix(filePath))
		}
		if fileName := fileInfo["fileName"]; ext == "" && pyrepr.Truthy(fileName) {
			name, isStr := fileName.(string)
			if !isStr {
				return &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("expected str, bytes or os.PathLike object, not %s",
					pyrepr.TypeName(fileName))}
			}
			ext = pystr.Lower(pypath.PathSuffix(name))
		}
		if filePath == "" && isAudioExt(ext) && fileID != nil {
			if err := unhashable(fileID); err != nil {
				return err
			}
			slog.Info(fmt.Sprintf("SimpleX: voice file %s not yet received, accepting transfer", pyrepr.Str(fileID)))
			a.mu.Lock()
			a.pendingFileTransfers[fileKey(fileID)] = chatItem
			a.mu.Unlock()
			return a.sendFireAndForget(ctx, "/freceive "+pyrepr.Str(fileID))
		}
		if filePath != "" {
			mediaURLs = append(mediaURLs, filePath)
			mediaTypes = append(mediaTypes, mimeForExt(ext))
		}
	}
	userName := senderName
	if !pyrepr.Truthy(userName) {
		userName = senderID
	}
	chatNameText, userNameText := pyrepr.Str(chatName), pyrepr.Str(userName)
	sourceChatType := "dm"
	if isGroup {
		sourceChatType = "group"
	}
	source := a.BuildSource(ctx, chatID, gateway.SourceFields{ChatName: &chatNameText, ChatType: sourceChatType, UserID: &senderID,
		UserName: &userNameText})
	msgType := gateway.MessageTypeText
	if len(mediaTypes) > 0 {
		msgType = gateway.MessageTypeDocument
	precedence:
		for _, row := range mediaKindPrecedence {
			for _, mt := range mediaTypes {
				if strings.HasPrefix(mt, row.prefix) {
					msgType = row.kind
					break precedence
				}
			}
		}
	}
	tsValue, err := dictGet(meta, "itemTs", nil)
	if err != nil {
		return err
	}
	if !pyrepr.Truthy(tsValue) {
		tsValue = getIfDict(meta, "createdAt", "")
	}
	timestamp := time.Now().UTC()
	if pyrepr.Truthy(tsValue) {
		tsText, isStr := tsValue.(string)
		if !isStr {
			return &gateway.PyError{Kind: "TypeError", Msg: "fromisoformat: argument must be str"}
		}
		if parsed, perr := pydatetime.FromISOFormat(tsText); perr == nil {
			timestamp = parsed
		}
	}
	body := ""
	if pyrepr.Truthy(text) {
		var isStr bool
		if body, isStr = text.(string); !isStr {
			return &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("'%s' object is not subscriptable", pyrepr.TypeName(text))}
		}
	}
	msgEvent := gateway.NewMessageEvent(body)
	msgEvent.Source, msgEvent.MessageType = source, msgType
	msgEvent.MediaURLs, msgEvent.MediaTypes = mediaURLs, mediaTypes
	msgEvent.Timestamp, msgEvent.RawMessage = timestamp, chatItem
	slog.Debug(fmt.Sprintf("SimpleX: message from %s in %s: %s", redactID(senderID), pystr.Slice(chatID, 0, 20), pystr.Slice(body, 0, 50)))
	if msgType == gateway.MessageTypeText && body != "" {
		return a.EnqueueTextEvent(ctx, msgEvent)
	}
	return a.HandleMessage(ctx, msgEvent)
}

// Batch key of a text event, the platform value and the chat id
func (a *SimplexAdapter) TextBatchKey(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	return string(event.Source.Platform) + ":" + event.Source.ChatID, nil
}

// Forgets a minted correlation id, the adapter lock held
func (a *SimplexAdapter) discardCorrID(id string) {
	if !a.pendingCorrIDs[id] {
		return
	}
	delete(a.pendingCorrIDs, id)
	for i, known := range a.pendingCorrOrder {
		if known == id {
			a.pendingCorrOrder = append(a.pendingCorrOrder[:i:i], a.pendingCorrOrder[i+1:]...)
			break
		}
	}
}

// Mints a correlation id and remembers it for echo filtering, the remembered set bounded
func (a *SimplexAdapter) makeCorrID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.corrCounter++
	corrID := fmt.Sprintf("%s%d-%d", corrPrefix, a.corrCounter, int64(nowSeconds()*1000))
	if !a.pendingCorrIDs[corrID] {
		a.pendingCorrIDs[corrID] = true
		a.pendingCorrOrder = append(a.pendingCorrOrder, corrID)
	}
	for len(a.pendingCorrOrder) > maxPendingCorr {
		delete(a.pendingCorrIDs, a.pendingCorrOrder[0])
		a.pendingCorrOrder = a.pendingCorrOrder[1:]
	}
	return corrID
}

// Open daemon socket, nil while disconnected
func (a *SimplexAdapter) socket() *pywebsockets.Conn {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ws
}

// JSON of a command frame with its correlation id first
func commandFrame(corrID, command string) (string, error) {
	return pyjson.Dumps(pyjson.ObjectOf("corrId", corrID, "cmd", command))
}

// Writes one command frame without waiting for a reply, dropped while disconnected and a write failure logged
func (a *SimplexAdapter) sendWS(corrID, command string) error {
	ws := a.socket()
	if ws == nil {
		slog.Debug("SimpleX: WS send dropped (not connected)")
		return nil
	}
	frame, err := commandFrame(corrID, command)
	if err != nil {
		return err
	}
	if err := ws.Send(frame); err != nil {
		slog.Warn(fmt.Sprintf("SimpleX: WS send error: %s", err.Error()))
	}
	return nil
}

// Sends a command and waits for the reply with its correlation id, nil when disconnected, timed out or failed
func (a *SimplexAdapter) sendCommand(ctx context.Context, command string, timeout float64) (any, error) {
	ws := a.socket()
	if ws == nil {
		slog.Warn("SimpleX: command sent but WebSocket not connected")
		return nil, nil
	}
	corrID := a.makeCorrID()
	pending := &pendingResponse{result: make(chan any, 1), cancelled: make(chan struct{})}
	a.mu.Lock()
	a.pendingResponses[corrID] = pending
	a.mu.Unlock()
	forget := func() {
		a.mu.Lock()
		delete(a.pendingResponses, corrID)
		a.mu.Unlock()
	}
	frame, err := commandFrame(corrID, command)
	if err == nil {
		err = ws.Send(frame)
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("SimpleX: command failed: %s — %s", pystr.Slice(command, 0, 50), err.Error()))
		forget()
		return nil, nil
	}
	timer := time.NewTimer(time.Duration(timeout * float64(time.Second)))
	defer timer.Stop()
	select {
	case result := <-pending.result:
		return result, nil
	case <-timer.C:
		slog.Warn(fmt.Sprintf("SimpleX: command timed out: %s", pystr.Slice(command, 0, 50)))
		forget()
		return nil, nil
	case <-pending.cancelled:
		return nil, context.Canceled
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Sends a command the daemon never answers with a correlation id
func (a *SimplexAdapter) sendFireAndForget(ctx context.Context, command string) error {
	return a.sendWS(a.makeCorrID(), command)
}

// Sends a structured payload and waits for the reply, the given error text when none arrives
func (a *SimplexAdapter) sendItems(ctx context.Context, chatID string, items []any, failure string) (gateway.SendResult, error) {
	command, err := sendCmd(chatID, items)
	if err != nil {
		return gateway.SendResult{}, err
	}
	result, err := a.sendCommand(ctx, command, 30.0)
	if err != nil {
		return gateway.SendResult{}, err
	}
	if result != nil {
		return gateway.SendResult{Success: true}, nil
	}
	return gateway.SendResult{Success: false, Error: failure}, nil
}

// Sends the text without waiting for a reply, then every MEDIA tag path as a voice note or a document
func (a *SimplexAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	mediaPaths := []string{}
	for m, err := mediaTagPathsRE.FindStringMatch(content); m != nil; m, err = mediaTagPathsRE.FindNextMatch(m) {
		if err != nil {
			return gateway.SendResult{}, err
		}
		mediaPaths = append(mediaPaths, m.GroupByNumber(1).String())
	}
	if len(mediaPaths) > 0 {
		stripped, err := mediaTagRE.Replace(content, "", -1, -1)
		if err != nil {
			return gateway.SendResult{}, err
		}
		content = pystr.Strip(stripped)
	}
	if content != "" {
		command, err := sendCmd(chatID, []any{pyjson.ObjectOf("msgContent", pyjson.ObjectOf("type", "text", "text", content))})
		if err != nil {
			return gateway.SendResult{}, err
		}
		if err := a.sendWS(a.makeCorrID(), command); err != nil {
			return gateway.SendResult{}, err
		}
	}
	for _, path := range mediaPaths {
		var result gateway.SendResult
		var err error
		if _, ext := pypath.Splitext(path); voiceTagExts[pystr.Lower(ext)] {
			result, err = a.SendVoice(ctx, chatID, path, "", "", nil, nil)
		} else {
			result, err = a.SendDocument(ctx, chatID, path, "", "", "", nil, nil)
		}
		if err != nil {
			return gateway.SendResult{}, err
		}
		if !result.Success {
			return result, nil
		}
	}
	return gateway.SendResult{Success: true}, nil
}

// Contacts and groups of the daemon for the channel directory, nil while the socket is down or the daemon is silent
func (a *SimplexAdapter) ListChannels(ctx context.Context) (any, error) {
	if a.socket() == nil {
		return nil, nil
	}
	resp, err := a.sendCommand(ctx, "/contacts", 10.0)
	if err != nil || resp == nil {
		return nil, err
	}
	channels := []any{}
	contacts, err := dictGet(resp, "contacts", nil)
	if err != nil {
		return nil, err
	}
	contactList, err := iterItems(contacts)
	if err != nil {
		return nil, err
	}
	for _, contact := range contactList {
		if _, ok := contact.(map[string]any); !ok {
			continue
		}
		contactID := getIfDict(contact, "contactId", nil)
		name, err := displayName(contact, "profile", "")
		if err != nil {
			return nil, err
		}
		if contactID == nil && !pyrepr.Truthy(name) {
			continue
		}
		label := name
		if !pyrepr.Truthy(label) {
			label = contactID
		}
		channels = append(channels, map[string]any{"id": pyrepr.Str(label), "name": pyrepr.Str(label), "type": "dm"})
	}
	if resp, err = a.sendCommand(ctx, "/groups", 10.0); err != nil {
		return nil, err
	}
	var groupList []any
	if resp != nil {
		groups, err := dictGet(resp, "groups", nil)
		if err != nil {
			return nil, err
		}
		if groupList, err = iterItems(groups); err != nil {
			return nil, err
		}
	}
	for _, group := range groupList {
		if pair, ok := group.([]any); ok && len(pair) > 0 {
			group = pair[0]
		}
		groupID := getIfDict(group, "groupId", nil)
		if groupID == nil {
			continue
		}
		name, err := displayName(group, "groupProfile", "")
		if err != nil {
			return nil, err
		}
		if !pyrepr.Truthy(name) {
			name = pyrepr.Str(groupID)
		}
		channels = append(channels, map[string]any{"id": "group:" + pyrepr.Str(groupID), "name": pyrepr.Str(name), "type": "group"})
	}
	return channels, nil
}

// Items a for loop yields over a decoded JSON value, nothing for a falsy one and a TypeError for a non iterable
func iterItems(v any) ([]any, error) {
	if !pyrepr.Truthy(v) {
		return nil, nil
	}
	switch t := v.(type) {
	case []any:
		return t, nil
	case map[string]any:
		out := make([]any, 0, len(t))
		for k := range t {
			out = append(out, k)
		}
		return out, nil
	case string:
		out := make([]any, 0, len(t))
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, nil
	}
	return nil, &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(v))}
}

// PNG or JPEG path of an image, converting other formats to PNG, with a 128 pixel JPEG thumbnail data URI
func prepareImage(filePath string) (string, string, error) {
	suffix := pystr.Lower(pypath.PathSuffix(filePath))
	needsPNG := suffix != ".png" && suffix != ".jpg" && suffix != ".jpeg"
	pngPath := filePath
	if needsPNG {
		var err error
		if pngPath, err = pypath.WithSuffix(filePath, ".png"); err != nil {
			return "", "", err
		}
	}
	f, err := os.Open(filePath)
	if err != nil {
		return "", "", err
	}
	src, _, err := image.Decode(f)
	f.Close()
	if errors.Is(err, image.ErrFormat) {
		return "", "", &gateway.PyError{Kind: "UnidentifiedImageError", Msg: "cannot identify image file " + pyrepr.StrRepr(filePath)}
	}
	if err != nil {
		return "", "", err
	}
	if needsPNG {
		out, err := os.Create(pngPath)
		if err != nil {
			return "", "", err
		}
		if err := png.Encode(out, src); err != nil {
			out.Close()
			return "", "", err
		}
		if err := out.Close(); err != nil {
			return "", "", err
		}
	}
	nrgba := pypil.ToNRGBA(src)
	var thumb *image.NRGBA
	_, paletted := src.(*image.Paletted)
	_, gray := src.(*image.Gray)
	if paletted {
		thumb = nearestThumbnail(nrgba, 128)
	} else {
		hasAlpha := false
		for i := 3; i < len(nrgba.Pix); i += 4 {
			if nrgba.Pix[i] != 0xff {
				hasAlpha = true
				break
			}
		}
		thumb = pypil.Thumbnail(nrgba, 128, hasAlpha)
	}
	var encodable image.Image
	if gray {
		luma := image.NewGray(thumb.Rect)
		draw.Draw(luma, luma.Rect, thumb, thumb.Rect.Min, draw.Src)
		encodable = luma
	} else {
		flat := image.NewNRGBA(thumb.Rect)
		draw.Draw(flat, flat.Rect, &image.Uniform{C: color.White}, image.Point{}, draw.Src)
		draw.Draw(flat, flat.Rect, thumb, thumb.Rect.Min, draw.Over)
		encodable = flat
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, encodable, &jpeg.Options{Quality: 70}); err != nil {
		return "", "", err
	}
	return pngPath, thumbURIPrefix + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// Copy shrunk to fit the side box by nearest neighbour, the resampling a palette image gets
func nearestThumbnail(img *image.NRGBA, side int) *image.NRGBA {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	fw, fh, ok := pypil.ThumbnailSize(w, h, side)
	if !ok || (fw == w && fh == h) {
		out := image.NewNRGBA(img.Rect)
		copy(out.Pix, img.Pix)
		return out
	}
	out := image.NewNRGBA(image.Rect(0, 0, fw, fh))
	for y := 0; y < fh; y++ {
		sy := min(int((float64(y)+0.5)*float64(h)/float64(fh)), h-1)
		for x := 0; x < fw; x++ {
			sx := min(int((float64(x)+0.5)*float64(w)/float64(fw)), w-1)
			copy(out.Pix[out.PixOffset(x, y):out.PixOffset(x, y)+4], img.Pix[img.PixOffset(sx, sy):img.PixOffset(sx, sy)+4])
		}
	}
	return out
}

// Whether a path names an existing file or directory
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Sends an image from a file URL or a downloaded web URL with an inline thumbnail
func (a *SimplexAdapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	var filePath string
	if strings.HasPrefix(imageURL, "file://") {
		filePath = pyurl.Unquote(imageURL[7:])
	} else {
		cached, err := gateway.CacheImageFromURL(ctx, imageURL, ".jpg", 2)
		if err != nil {
			if ctx.Err() != nil {
				return gateway.SendResult{}, ctx.Err()
			}
			slog.Warn(fmt.Sprintf("SimpleX: failed to download image: %s", pyrepr.ExceptionMessage(err)))
			return gateway.SendResult{Success: false, Error: pyrepr.ExceptionMessage(err)}, nil
		}
		filePath = cached
	}
	if filePath == "" || !pathExists(filePath) {
		return gateway.SendResult{Success: false, Error: "Image file not found"}, nil
	}
	pngPath, thumbURI, err := prepareImage(filePath)
	if err != nil {
		slog.Warn(fmt.Sprintf("SimpleX: failed to prepare image: %s", pyrepr.ExceptionMessage(err)))
		return gateway.SendResult{Success: false, Error: "Failed to prepare image: " + pyrepr.ExceptionMessage(err)}, nil
	}
	item := pyjson.ObjectOf("filePath", pngPath, "msgContent", pyjson.ObjectOf("type", "image", "image", thumbURI, "text", caption))
	return a.sendItems(ctx, chatID, []any{item}, "Failed to send image")
}

// Sends a local image through the file URL path of the image send
func (a *SimplexAdapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.SendImage(ctx, chatID, "file://"+imagePath, caption, "", metadata)
}

// Sends a video as a file attachment
func (a *SimplexAdapter) SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.SendDocument(ctx, chatID, videoPath, caption, "", "", nil, nil)
}

// Sends a local file as a file message
func (a *SimplexAdapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	if !pathExists(filePath) {
		return gateway.SendResult{Success: false, Error: "File not found"}, nil
	}
	item := pyjson.ObjectOf("filePath", filePath, "msgContent", pyjson.ObjectOf("type", "file", "text", caption))
	return a.sendItems(ctx, chatID, []any{item}, "Failed to send document")
}

// Sends an audio file as an inline voice note carrying the duration the caller passed
func (a *SimplexAdapter) SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	if !pathExists(audioPath) {
		return gateway.SendResult{Success: false, Error: "Voice file not found"}, nil
	}
	var duration any = 0
	if v, ok := extra["duration"]; ok {
		duration = v
	}
	item := pyjson.ObjectOf("msgContent", pyjson.ObjectOf("type", "voice", "text", caption, "duration", duration),
		"fileSource", pyjson.ObjectOf("filePath", audioPath))
	return a.sendItems(ctx, chatID, []any{item}, "Failed to send voice message")
}

// SimpleX has no typing indicator
func (a *SimplexAdapter) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	return nil
}

// Chat info of a group or direct chat id
func (a *SimplexAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	if strings.HasPrefix(chatID, "group:") {
		return map[string]any{"chat_id": chatID, "type": "group", "name": chatID[6:]}, nil
	}
	return map[string]any{"chat_id": chatID, "type": "dm", "name": chatID}, nil
}
