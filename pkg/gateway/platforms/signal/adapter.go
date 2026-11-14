package signal

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/mediacache"
)

const (
	signalMaxAttachmentSize   = 100 * 1024 * 1024
	maxMessageLength          = 8000
	sseRetryDelayInitial      = 2.0
	sseRetryDelayMax          = 60.0
	healthCheckInterval       = 30.0
	healthCheckStaleThreshold = 120.0
	maxRecentTimestamps       = 512
	recentSentTTLSeconds      = 300.0
	maxSentMessageTimestamps  = 500
)

// Magic byte prefixes claimed before the shared audio and video sniffer
var magicExtensions = []struct {
	magic []byte
	ext   string
}{{[]byte("\x89PNG"), ".png"}, {[]byte("\xff\xd8"), ".jpg"}, {[]byte("GIF8"), ".gif"}, {[]byte("%PDF"), ".pdf"}}

// First matching MIME prefix decides the inbound message type
var mediaTypeByMimePrefix = []struct {
	prefix string
	kind   gateway.MessageType
}{{"audio/", gateway.MessageTypeVoice}, {"image/", gateway.MessageTypePhoto}, {"video/", gateway.MessageTypeVideo}}

var outcomeReaction = map[gateway.ProcessingOutcome]string{gateway.ProcessingSuccess: "✅", gateway.ProcessingFailure: "❌"}

var quoteAuthorKeys = []string{"author", "authorNumber", "authorUuid", "authorAci", "authorServiceId", "authorServiceIdString"}

var truthyStrings = map[string]bool{"1": true, "true": true, "yes": true, "on": true}

var (
	imageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true}
	audioExts = map[string]bool{".mp3": true, ".wav": true, ".ogg": true, ".m4a": true, ".aac": true}
)

// Media cache writers, URL fetcher and wall clock the adapter reads through, swappable like module globals
var (
	cacheImageFromBytes    = gateway.CacheImageFromBytesAsync
	cacheAudioFromBytes    = gateway.CacheAudioFromBytesAsync
	cacheDocumentFromBytes = gateway.CacheDocumentFromBytesAsync
	cacheImageFromURL      = gateway.CacheImageFromURL
	wallClock              = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	lookupFFmpeg           = func() (string, bool) { return pyshutil.Which("ffmpeg", nil) }
)

// Python exception carried as a Go error with its class name
type pyErr struct{ kind, msg string }

func (e *pyErr) Error() string      { return e.msg }
func (e *pyErr) PyTypeName() string { return e.kind }

// AttributeError of calling a method on a value of the wrong type
func noAttr(v any, attr string) error {
	return &pyErr{"AttributeError", fmt.Sprintf("'%s' object has no attribute '%s'", pyrepr.TypeName(v), attr)}
}

// Dict view of a decoded JSON value, the AttributeError a get call raises otherwise
func dictOf(v any) (map[string]any, error) {
	if m, ok := pyjson.Dict(v); ok {
		return m, nil
	}
	return nil, noAttr(v, "get")
}

// Value under key or the default when the key is absent
func getOr(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// str of an exception as a percent s log argument renders it
func errText(err error) string { return pyrepr.ExceptionMessage(err) }

// Optional string pointer, nil for the empty value Python holds as None
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Text of a value used as an identifier, strings verbatim and other values through str
func identText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pyrepr.Str(v)
}

// Optional text of a decoded value, nil for None
func optText(v any) *string {
	if v == nil {
		return nil
	}
	s := identText(v)
	return &s
}

// Open SSE response with the httpx flag marking that its body iteration began
type sseStream struct {
	body     io.Closer
	consumed bool
}

// Dict key of a timestamp so equal ints and integral floats share one entry
func tsKey(v any) string {
	if f, ok := v.(float64); ok && f == math.Trunc(f) && !math.IsInf(f, 0) {
		return new(big.Float).SetFloat64(f).Text('f', 0)
	}
	if b, ok := v.(bool); ok {
		if b {
			return "1"
		}
		return "0"
	}
	return pyrepr.Str(v)
}

// Comma separated text split into stripped non empty items
func parseCommaList(value string) []string {
	out := []string{}
	for _, v := range strings.Split(value, ",") {
		if s := pystr.Strip(v); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Set of the items of a comma separated text
func commaSet(value string) map[string]bool {
	set := map[string]bool{}
	for _, v := range parseCommaList(value) {
		set[v] = true
	}
	return set
}

// File extension from magic bytes, WEBP claimed before the shared sniffer and zip or bin last
func guessExtension(data []byte) string {
	for _, m := range magicExtensions {
		if bytes.HasPrefix(data, m.magic) {
			return m.ext
		}
	}
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return ".webp"
	}
	if container, ok := mediacache.SniffContainer(data); ok {
		return mediacache.ContainerToExt[container]
	}
	if bytes.HasPrefix(data, []byte("PK")) {
		return ".zip"
	}
	return ".bin"
}

func isImageExt(ext string) bool { return imageExts[pystr.Lower(ext)] }

func isAudioExt(ext string) bool { return audioExts[pystr.Lower(ext)] }

// MIME type of a file extension from the shared table, octet stream when unknown
func extToMime(ext string) string {
	return gateway.MimeForExt(ext, nil, "application/octet-stream")
}

// Whether the path is an executable regular file
func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

// Raw ADTS AAC remuxed without loss to m4a bytes through ffmpeg, false when ffmpeg is absent or fails
func remuxAACToM4A(ctx context.Context, aacData []byte) ([]byte, bool) {
	ffmpeg, found := lookupFFmpeg()
	if !found {
		for _, p := range []string{"/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffmpeg"} {
			if isExecutableFile(p) {
				ffmpeg, found = p, true
				break
			}
		}
	}
	if !found {
		slog.Debug("Signal: ffmpeg not found, skipping AAC→M4A remux")
		return nil, false
	}
	src, err := os.CreateTemp("", "tmp*.aac")
	if err != nil {
		slog.Error("Signal: AAC→M4A remux error", "error", err)
		return nil, false
	}
	srcPath := src.Name()
	dstPath := srcPath[:len(srcPath)-4] + ".m4a"
	defer func() {
		os.Remove(srcPath)
		os.Remove(dstPath)
	}()
	_, werr := src.Write(aacData)
	if cerr := src.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		slog.Error("Signal: AAC→M4A remux error", "error", werr)
		return nil, false
	}
	proc, err := pysubprocess.RunWith(ctx, []string{ffmpeg, "-y", "-loglevel", "error", "-i", srcPath, "-c:a", "copy", "-movflags", "+faststart", dstPath},
		pysubprocess.Options{Timeout: 10 * time.Second})
	var timeout *pysubprocess.TimeoutExpired
	switch {
	case errors.As(err, &timeout):
		slog.Warn("Signal: AAC→M4A remux timed out (>10s)")
		return nil, false
	case err != nil:
		slog.Error("Signal: AAC→M4A remux error", "error", err)
		return nil, false
	}
	if proc.Returncode != 0 {
		slog.Warn(fmt.Sprintf("Signal: AAC→M4A remux failed (ffmpeg exit %d): %s", proc.Returncode, pystr.Slice(pystr.DecodeUTF8Replace(proc.Stderr), 0, 300)))
		return nil, false
	}
	out, err := os.ReadFile(dstPath)
	if err != nil {
		slog.Error("Signal: AAC→M4A remux error", "error", err)
		return nil, false
	}
	return out, true
}

// Integer value of a mention offset field with the given default when absent
func mentionInt(m map[string]any, key string, def int) (int, error) {
	v, ok := m[key]
	if !ok {
		return def, nil
	}
	n, isInt := pyint.Instance(v)
	if !isInt || !n.IsInt64() {
		return 0, &pyErr{"TypeError", fmt.Sprintf("slice indices must be integers or None or have an __index__ method, not '%s'", pyrepr.TypeName(v))}
	}
	return int(n.Int64()), nil
}

// Mention placeholders replaced from the end with readable at identifiers so earlier offsets hold
func renderMentions(text string, mentions any) (string, error) {
	if !pyrepr.Truthy(mentions) || !strings.ContainsRune(text, 0xFFFC) {
		return text, nil
	}
	list, ok := mentions.([]any)
	if !ok {
		return "", &pyErr{"TypeError", fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(mentions))}
	}
	type mention struct {
		start, length int
		identifier    string
	}
	parsed := make([]mention, 0, len(list))
	for _, item := range list {
		m, err := dictOf(item)
		if err != nil {
			return "", err
		}
		start, err := mentionInt(m, "start", 0)
		if err != nil {
			return "", err
		}
		length, err := mentionInt(m, "length", 1)
		if err != nil {
			return "", err
		}
		parsed = append(parsed, mention{start, length, identText(pyrepr.Or(pyrepr.Or(m["number"], m["uuid"]), "user"))})
	}
	sort.SliceStable(parsed, func(i, j int) bool { return parsed[i].start > parsed[j].start })
	for _, m := range parsed {
		n := pystr.Len(text)
		head, tail := pystr.Slice(text, 0, m.start), pystr.Slice(text, m.start+m.length, n)
		text = head + "@" + m.identifier + tail
	}
	return text, nil
}

// Whether the text is a UUID the way uuid.UUID accepts it
func parsesAsUUID(value string) bool {
	hex := strings.ReplaceAll(strings.ReplaceAll(value, "urn:", ""), "uuid:", "")
	hex = strings.ReplaceAll(strings.Trim(hex, "{}"), "-", "")
	if pystr.Len(hex) != 32 {
		return false
	}
	_, ok := pyint.ParseBase(hex, 16)
	return ok
}

// Whether the value already is a Signal service identifier, a PNI or u prefix or a UUID
func isSignalServiceID(value any) bool {
	s, ok := value.(string)
	if !ok || s == "" {
		return false
	}
	if strings.HasPrefix(s, "PNI:") || strings.HasPrefix(s, "u:") {
		return true
	}
	return parsesAsUUID(s)
}

// Whether the value is a plausible E.164 phone number
func looksLikeE164Number(value string) bool {
	if value == "" || !strings.HasPrefix(value, "+") {
		return false
	}
	digits := value[1:]
	n := pystr.Len(digits)
	return pystr.StrIsDigit(digits) && 7 <= n && n <= 15
}

// Signal runtime dependencies are compiled into the binary
func checkSignalRequirements() bool { return true }

// Stripped text of an extra value or of the scoped secret when the extra value is falsy
func extraOrSecretText(ctx context.Context, extra map[string]any, key, env string) (string, error) {
	v := getOr(extra, key, "")
	if !pyrepr.Truthy(v) {
		secret, err := gateway.GetScopedSecret(ctx, env, "", false)
		if err != nil {
			return "", err
		}
		v = secret
	}
	s, ok := v.(string)
	if !ok {
		return "", noAttr(v, "strip")
	}
	return pystr.Strip(s), nil
}

// Whether Signal has the daemon URL and account it needs to connect
func validateSignalConfig(ctx context.Context, config *gateway.PlatformConfig) (bool, error) {
	extra := map[string]any{}
	if config != nil && config.Extra != nil {
		extra = config.Extra
	}
	httpURL, err := extraOrSecretText(ctx, extra, "http_url", "SIGNAL_HTTP_URL")
	if err != nil {
		return false, err
	}
	account, err := extraOrSecretText(ctx, extra, "account", "SIGNAL_ACCOUNT")
	if err != nil {
		return false, err
	}
	return httpURL != "" && account != "", nil
}

// Class attributes of the Signal adapter
func adapterTraits() gateway.AdapterTraits {
	t := gateway.DefaultAdapterTraits()
	t.MaxMessageLength = maxMessageLength
	t.SplitsLongMessages = true
	t.SerialReceiveLoop = true
	t.SupportsMessageEditing = false
	return t
}

// SignalAdapter bridges a signal-cli HTTP daemon, inbound over SSE and outbound over JSON-RPC
type SignalAdapter struct {
	*gateway.BasePlatformAdapter

	httpURL        string
	account        string
	ignoreStories  bool
	groupAllowFrom map[string]bool
	requireMention bool
	dmAllowFrom    map[string]bool
	// Attachment intake switch read on every envelope
	IgnoreAttachments bool

	mu                    sync.Mutex
	client                *http.Client
	sseTask               *gateway.AdapterTask
	healthMonitorTask     *gateway.AdapterTask
	typingTasks           map[string]*gateway.AdapterTask
	typingFailures        map[string]int
	typingSkipUntil       map[string]float64
	lastSSEActivity       float64
	sseResponse           *sseStream
	accountNormalized     string
	recentSentTimestamps  *pydict.Dict[string, float64]
	sentMessageTimestamps *pydict.Dict[string, struct{}]
	recipientUUIDByNumber map[string]string
	recipientNumberByUUID map[string]string
	recipientCacheLock    sync.Mutex
}

// Adapter configured from the platform extra and the profile scoped SIGNAL env
func NewSignalAdapter(ctx context.Context, config *gateway.PlatformConfig) (*SignalAdapter, error) {
	if config == nil {
		config = gateway.NewPlatformConfig()
	}
	a := &SignalAdapter{typingTasks: map[string]*gateway.AdapterTask{}, typingFailures: map[string]int{}, typingSkipUntil: map[string]float64{},
		recentSentTimestamps: pydict.New[string, float64](), sentMessageTimestamps: pydict.New[string, struct{}](),
		recipientUUIDByNumber: map[string]string{}, recipientNumberByUUID: map[string]string{}}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, gateway.PlatformSignal, a)
	a.Traits = adapterTraits()
	extra := config.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	rawURL := getOr(extra, "http_url", "http://127.0.0.1:8080")
	urlText, ok := rawURL.(string)
	if !ok {
		return nil, noAttr(rawURL, "rstrip")
	}
	a.httpURL = pystr.RstripChars(urlText, "/")
	rawAccount := getOr(extra, "account", "")
	account, ok := rawAccount.(string)
	if !ok {
		return nil, noAttr(rawAccount, "strip")
	}
	a.account = account
	a.ignoreStories = pyrepr.Truthy(getOr(extra, "ignore_stories", true))
	groups, err := gateway.GetScopedSecret(ctx, "SIGNAL_GROUP_ALLOWED_USERS", "", false)
	if err != nil {
		return nil, err
	}
	a.groupAllowFrom = commaSet(groups)
	if cfg := extra["require_mention"]; cfg != nil {
		a.requireMention = pyrepr.Truthy(cfg)
	} else {
		secret, err := gateway.GetScopedSecret(ctx, "SIGNAL_REQUIRE_MENTION", "false", false)
		if err != nil {
			return nil, err
		}
		if secret == "" {
			secret = "false"
		}
		a.requireMention = truthyStrings[pystr.Lower(secret)]
	}
	allowed, err := gateway.GetScopedSecret(ctx, "SIGNAL_ALLOWED_USERS", "*", false)
	if err != nil {
		return nil, err
	}
	a.dmAllowFrom = commaSet(allowed)
	a.accountNormalized = pystr.Strip(a.account)
	groupsState := "disabled"
	if len(a.groupAllowFrom) > 0 {
		groupsState = "enabled"
	}
	slog.Info(fmt.Sprintf("Signal adapter initialized: url=%s account=%s groups=%s", a.httpURL, gateway.RedactPhone(a.account), groupsState))
	return a, nil
}

// HTTP client in use, nil while disconnected
func (a *SignalAdapter) httpClient() *http.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.client
}

// Records SSE activity at the current wall clock
func (a *SignalAdapter) touchSSEActivity() {
	a.mu.Lock()
	a.lastSSEActivity = wallClock()
	a.mu.Unlock()
}

// One GET with the httpx timeout, transport failures carrying the httpx error text
func (a *SignalAdapter) get(ctx context.Context, client *http.Client, rawURL string, timeout time.Duration) (*pyhttpx.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := pyhttpx.Do(client, req, timeout)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &pyErr{"ConnectError", pyhttpx.ErrorText(err)}
	}
	return resp, nil
}

// Connects to the signal-cli daemon under the phone lock and starts the SSE listener and health monitor
func (a *SignalAdapter) Connect(ctx context.Context, isReconnect bool) (connected bool, err error) {
	if a.httpURL == "" || a.account == "" {
		slog.Error("Signal: SIGNAL_HTTP_URL and SIGNAL_ACCOUNT are required")
		return false, nil
	}
	lockAcquired := false
	acquired, lockErr := a.AcquirePlatformLock(ctx, "signal-phone", a.account, "Signal account")
	switch {
	case lockErr != nil && ctx.Err() != nil:
		return false, ctx.Err()
	case lockErr != nil:
		slog.Warn(fmt.Sprintf("Signal: Could not acquire phone lock (non-fatal): %s", errText(lockErr)))
	case !acquired:
		return false, nil
	default:
		lockAcquired = true
	}
	client := gateway.LimitedHTTPClient(gateway.PlatformHTTPXLimits(), false)
	a.mu.Lock()
	a.client = client
	a.mu.Unlock()
	defer func() {
		if !a.IsConnected() {
			a.closeClient()
			if lockAcquired {
				a.ReleasePlatformLock()
			}
		}
	}()
	resp, err := a.get(ctx, client, a.httpURL+"/api/v1/check", 10*time.Second)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		slog.Error(fmt.Sprintf("Signal: cannot reach signal-cli at %s: %s", a.httpURL, errText(err)))
		return false, nil
	}
	if resp.StatusCode != 200 {
		slog.Error(fmt.Sprintf("Signal: health check failed (status %d)", resp.StatusCode))
		return false, nil
	}
	a.SetRunning(true)
	a.touchSSEActivity()
	sse := gateway.SpawnAdapterTask(ctx, a.sseListener)
	health := gateway.SpawnAdapterTask(ctx, a.healthMonitor)
	a.mu.Lock()
	a.sseTask, a.healthMonitorTask = sse, health
	a.mu.Unlock()
	slog.Info(fmt.Sprintf("Signal: connected to %s", a.httpURL))
	a.WirePluginHandlers(ctx, nil)
	return true, nil
}

// Releases the HTTP client and its pooled connections
func (a *SignalAdapter) closeClient() {
	a.mu.Lock()
	client := a.client
	a.client = nil
	a.mu.Unlock()
	if client != nil {
		client.CloseIdleConnections()
	}
}

// Stops the SSE listener, health monitor and typing loops, closes the client and releases the phone lock
func (a *SignalAdapter) Disconnect(ctx context.Context) error {
	a.SetRunning(false)
	a.mu.Lock()
	sse, health := a.sseTask, a.healthMonitorTask
	typing := a.typingTasks
	a.typingTasks = map[string]*gateway.AdapterTask{}
	a.mu.Unlock()
	for _, task := range []*gateway.AdapterTask{sse, health} {
		gateway.CancelTask(ctx, task)
	}
	for _, task := range typing {
		task.Cancel()
	}
	a.closeClient()
	a.ReleasePlatformLock()
	slog.Info("Signal: disconnected")
	return nil
}

// Dispatches one SSE line, keepalive comments counting as activity so the health monitor stays quiet
func (a *SignalAdapter) handleSSELine(ctx context.Context, line string) error {
	if strings.HasPrefix(line, ":") {
		a.touchSSEActivity()
		return nil
	}
	if !strings.HasPrefix(line, "data:") {
		return nil
	}
	dataStr := pystr.Strip(line[5:])
	if dataStr == "" {
		return nil
	}
	a.touchSSEActivity()
	envelope, err := pyjson.Loads(dataStr)
	if err != nil {
		slog.Debug(fmt.Sprintf("Signal SSE: invalid JSON: %s", pystr.Slice(dataStr, 0, 100)))
		return nil
	}
	if err := a.handleEnvelope(ctx, envelope); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Error("Signal SSE: error handling event", "error", err)
	}
	return nil
}

// Reads one SSE connection line by line until it ends, fails or the adapter stops
func (a *SignalAdapter) readSSEStream(ctx context.Context, rawURL string, backoff *float64) error {
	client := a.httpClient()
	if client == nil {
		return &pyErr{"AttributeError", "'NoneType' object has no attribute 'stream'"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	stream, err := pyhttpx.Stream(client, req, 0)
	if err != nil {
		return &pyErr{"ConnectError", pyhttpx.ErrorText(err)}
	}
	defer stream.Body.Close()
	response := &sseStream{body: stream.Body}
	a.mu.Lock()
	a.sseResponse = response
	a.lastSSEActivity = wallClock()
	a.mu.Unlock()
	*backoff = sseRetryDelayInitial
	slog.Info("Signal SSE: connected")
	a.mu.Lock()
	response.consumed = true
	a.mu.Unlock()
	reader := bufio.NewReader(stream.Body)
	for {
		raw, readErr := reader.ReadBytes('\n')
		if !a.IsConnected() {
			return nil
		}
		if readErr == nil {
			if line := pystr.Strip(pystr.DecodeUTF8Replace(raw[:len(raw)-1])); line != "" {
				if err := a.handleSSELine(ctx, line); err != nil {
					return err
				}
			}
			continue
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		return &pyErr{"ReadError", pyhttpx.ErrorText(readErr)}
	}
}

// Listens for SSE events from the daemon, reconnecting with jittered exponential backoff
func (a *SignalAdapter) sseListener(ctx context.Context) error {
	rawURL := a.httpURL + "/api/v1/events?account=" + pyurl.Quote(a.account, "")
	backoff := sseRetryDelayInitial
	for a.IsConnected() {
		slog.Debug(fmt.Sprintf("Signal SSE: connecting to %s", rawURL))
		err := a.readSSEStream(ctx, rawURL, &backoff)
		if ctx.Err() != nil {
			break
		}
		if err != nil && a.IsConnected() {
			var pe *pyErr
			if errors.As(err, &pe) && (pe.kind == "ConnectError" || pe.kind == "ReadError") {
				slog.Warn(fmt.Sprintf("Signal SSE: HTTP error: %s (reconnecting in %.0fs)", errText(err), backoff))
			} else {
				slog.Warn(fmt.Sprintf("Signal SSE: error: %s (reconnecting in %.0fs)", errText(err), backoff))
			}
		}
		if a.IsConnected() {
			if signalSleep(ctx, backoff+backoff*0.2*rand.Float64()) != nil {
				break
			}
			backoff = math.Min(backoff*2, sseRetryDelayMax)
		}
	}
	a.mu.Lock()
	a.sseResponse = nil
	a.mu.Unlock()
	return nil
}

// Checks daemon health when SSE has been idle past the stale threshold and forces a reconnect on failure
func (a *SignalAdapter) healthMonitor(ctx context.Context) error {
	for a.IsConnected() {
		if err := signalSleep(ctx, healthCheckInterval); err != nil {
			return nil
		}
		if !a.IsConnected() {
			break
		}
		a.mu.Lock()
		elapsed := wallClock() - a.lastSSEActivity
		a.mu.Unlock()
		if elapsed <= healthCheckStaleThreshold {
			continue
		}
		slog.Warn(fmt.Sprintf("Signal: SSE idle for %.0fs, checking daemon health", elapsed))
		var resp *pyhttpx.Response
		var err error
		if client := a.httpClient(); client == nil {
			err = &pyErr{"AttributeError", "'NoneType' object has no attribute 'get'"}
		} else {
			resp, err = a.get(ctx, client, a.httpURL+"/api/v1/check", 10*time.Second)
		}
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			slog.Warn(fmt.Sprintf("Signal: health check error: %s, forcing reconnect", errText(err)))
			a.forceReconnect(ctx)
			continue
		}
		if resp.StatusCode == 200 {
			a.touchSSEActivity()
			slog.Debug("Signal: daemon healthy, SSE idle")
		} else {
			slog.Warn(fmt.Sprintf("Signal: health check failed (%d), forcing reconnect", resp.StatusCode))
			a.forceReconnect(ctx)
		}
	}
	return nil
}

// Closes an SSE response whose body iteration has not begun in a tracked background task and forgets it
func (a *SignalAdapter) forceReconnect(ctx context.Context) {
	a.mu.Lock()
	response := a.sseResponse
	if response == nil || response.consumed {
		a.mu.Unlock()
		return
	}
	a.sseResponse = nil
	a.mu.Unlock()
	a.TrackBackgroundTask(gateway.SpawnAdapterTask(ctx, func(context.Context) error {
		response.body.Close()
		return nil
	}))
}

// Promotes a Note to Self or group sync sent message to a data message envelope, nil for other sync events
func (a *SignalAdapter) unwrapSyncMessage(envelopeData map[string]any) (map[string]any, error) {
	syncMsg, isDict := pyjson.Dict(envelopeData["syncMessage"])
	if !isDict {
		return nil, nil
	}
	sentRaw := syncMsg["sentMessage"]
	sentMsg, isDict := pyjson.Dict(sentRaw)
	if !pyrepr.Truthy(sentRaw) || !isDict {
		return nil, nil
	}
	dest := pyrepr.Or(sentMsg["destinationNumber"], sentMsg["destination"])
	if !pyrepr.Equal(dest, a.accountNormalized) {
		groupInfo := sentMsg["groupInfo"]
		if !pyrepr.Truthy(groupInfo) {
			groupInfo = map[string]any{}
		}
		gm, err := dictOf(groupInfo)
		if err != nil {
			return nil, err
		}
		if !pyrepr.Truthy(gm["groupId"]) {
			return nil, nil
		}
	}
	if a.consumeSentTimestamp(sentMsg["timestamp"]) {
		return nil, nil
	}
	out := make(map[string]any, len(envelopeData)+1)
	for k, v := range envelopeData {
		out[k] = v
	}
	out["dataMessage"] = sentRaw
	return out, nil
}

// Gates a group message on the mention requirement and strips the bot's own mention from the text
func (a *SignalAdapter) applyGroupMentionRules(text string, dataMessage map[string]any) (bool, string, error) {
	accountNorm := a.accountNormalized
	if a.requireMention {
		mentionedInText := accountNorm != "" && strings.Contains(text, "@"+accountNorm)
		mentionedInMetadata := false
		mentions := dataMessage["mentions"]
		if pyrepr.Truthy(mentions) {
			list, ok := mentions.([]any)
			if !ok {
				return false, text, &pyErr{"TypeError", fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(mentions))}
			}
			for _, item := range list {
				m, err := dictOf(item)
				if err != nil {
					return false, text, err
				}
				if pyrepr.Equal(m["number"], accountNorm) || pyrepr.Equal(m["uuid"], accountNorm) {
					mentionedInMetadata = true
					break
				}
			}
		}
		if !mentionedInText && !mentionedInMetadata {
			slog.Debug("Signal: ignoring group message (require_mention=true, bot not mentioned)")
			return false, text, nil
		}
	}
	if text != "" && accountNorm != "" {
		text = strings.ReplaceAll(text, "@"+accountNorm, "")
		a.mu.Lock()
		botUUID := a.recipientUUIDByNumber[accountNorm]
		a.mu.Unlock()
		if botUUID != "" {
			text = strings.ReplaceAll(text, "@"+botUUID, "")
		}
		text = pystr.Strip(strings.ReplaceAll(text, "  ", " "))
	}
	return true, text, nil
}

// Whether a decoded size exceeds the attachment cap, the TypeError comparing a non number raises
func exceedsAttachmentCap(size any) (bool, error) {
	if f, ok := size.(float64); ok {
		return f > signalMaxAttachmentSize, nil
	}
	n, ok := pyint.Instance(size)
	if !ok {
		return false, &pyErr{"TypeError", fmt.Sprintf("'>' not supported between instances of '%s' and 'int'", pyrepr.TypeName(size))}
	}
	return n.Cmp(big.NewInt(signalMaxAttachmentSize)) > 0, nil
}

// Fetches and caches the inbound attachments, returning their cached paths and MIME types
func (a *SignalAdapter) collectAttachments(ctx context.Context, attachmentsData any) ([]string, []string, error) {
	list, ok := attachmentsData.([]any)
	if !ok {
		return nil, nil, &pyErr{"TypeError", fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(attachmentsData))}
	}
	mediaURLs, mediaTypes := []string{}, []string{}
	for _, item := range list {
		att, err := dictOf(item)
		if err != nil {
			return nil, nil, err
		}
		attID := att["id"]
		if !pyrepr.Truthy(attID) {
			continue
		}
		tooLarge, err := exceedsAttachmentCap(getOr(att, "size", int64(0)))
		if err != nil {
			return nil, nil, err
		}
		if tooLarge {
			slog.Warn(fmt.Sprintf("Signal: attachment too large (%s bytes), skipping", pyrepr.Str(att["size"])))
			continue
		}
		cachedPath, ext, err := a.fetchAttachment(ctx, attID)
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			slog.Error(fmt.Sprintf("Signal: failed to fetch attachment %s", pyrepr.Str(attID)), "error", err)
			continue
		}
		if cachedPath != "" {
			mediaURLs = append(mediaURLs, cachedPath)
			contentType := att["contentType"]
			if !pyrepr.Truthy(contentType) {
				contentType = extToMime(ext)
			}
			mediaTypes = append(mediaTypes, identText(contentType))
		}
	}
	return mediaURLs, mediaTypes, nil
}

// UTC time of an epoch milliseconds value, the current time when it is absent or outside the datetime range
func envelopeTimestamp(tsMs any) (time.Time, error) {
	now := time.Now().UTC()
	if !pyrepr.Truthy(tsMs) {
		return now, nil
	}
	var seconds float64
	if f, ok := tsMs.(float64); ok {
		seconds = f / 1000
	} else if n, ok := pyint.Instance(tsMs); ok {
		f, _ := new(big.Float).SetInt(n).Float64()
		seconds = f / 1000
	} else {
		return now, &pyErr{"TypeError", fmt.Sprintf("unsupported operand type(s) for /: '%s' and 'int'", pyrepr.TypeName(tsMs))}
	}
	if math.IsNaN(seconds) {
		return now, nil
	}
	if math.Abs(seconds) >= 9.2e18 {
		return now, &pyErr{"OverflowError", "timestamp out of range for platform time_t"}
	}
	if seconds < -62135596800 || seconds >= 253402300800 {
		return now, nil
	}
	whole, frac := math.Modf(seconds)
	return time.Unix(int64(whole), int64(math.RoundToEven(frac*1e6))*1000).UTC(), nil
}

// Processes one signal-cli envelope into a message event and hands it to the gateway
func (a *SignalAdapter) handleEnvelope(ctx context.Context, envelope any) error {
	outer, err := dictOf(envelope)
	if err != nil {
		return err
	}
	inner := getOr(outer, "envelope", envelope)
	envelopeData, isDict := pyjson.Dict(inner)
	if !isDict {
		return &pyErr{"TypeError", fmt.Sprintf("argument of type '%s' is not iterable", pyrepr.TypeName(inner))}
	}
	_, isNoteToSelf := envelopeData["syncMessage"]
	if isNoteToSelf {
		if envelopeData, err = a.unwrapSyncMessage(envelopeData); err != nil || envelopeData == nil {
			return err
		}
	}
	senderRaw := pyrepr.Or(pyrepr.Or(envelopeData["sourceNumber"], envelopeData["sourceUuid"]), envelopeData["source"])
	senderNameRaw := getOr(envelopeData, "sourceName", "")
	senderUUIDRaw := getOr(envelopeData, "sourceUuid", "")
	a.rememberRecipientIdentifiers(senderRaw, senderUUIDRaw)
	if !pyrepr.Truthy(senderRaw) {
		slog.Debug("Signal: ignoring envelope with no sender")
		return nil
	}
	sender := identText(senderRaw)
	if a.accountNormalized != "" && pyrepr.Equal(senderRaw, a.accountNormalized) && !isNoteToSelf {
		return nil
	}
	if a.ignoreStories && pyrepr.Truthy(envelopeData["storyMessage"]) {
		return nil
	}
	dataMessageRaw := envelopeData["dataMessage"]
	if !pyrepr.Truthy(dataMessageRaw) {
		edit := envelopeData["editMessage"]
		if !pyrepr.Truthy(edit) {
			edit = map[string]any{}
		}
		em, err := dictOf(edit)
		if err != nil {
			return err
		}
		dataMessageRaw = em["dataMessage"]
	}
	if !pyrepr.Truthy(dataMessageRaw) {
		return nil
	}
	dataMessage, err := dictOf(dataMessageRaw)
	if err != nil {
		return err
	}
	var groupInfo map[string]any
	var groupIDRaw any
	if raw := dataMessage["groupInfo"]; pyrepr.Truthy(raw) {
		if groupInfo, err = dictOf(raw); err != nil {
			return err
		}
		groupIDRaw = groupInfo["groupId"]
	}
	isGroup := pyrepr.Truthy(groupIDRaw)
	groupID := ""
	if isGroup {
		groupID = identText(groupIDRaw)
		if !a.groupAllowed(groupID) {
			return nil
		}
	}
	chatID := sender
	if isGroup {
		chatID = "group:" + groupID
	}
	textRaw := getOr(dataMessage, "message", "")
	text := ""
	if pyrepr.Truthy(textRaw) {
		s, ok := textRaw.(string)
		if !ok {
			return noAttr(textRaw, "strip")
		}
		text = s
		if mentions := getOr(dataMessage, "mentions", []any{}); pyrepr.Truthy(mentions) {
			if text, err = renderMentions(text, mentions); err != nil {
				return err
			}
		}
	}
	if isGroup {
		mentioned, stripped, err := a.applyGroupMentionRules(text, dataMessage)
		if err != nil {
			return err
		}
		if !mentioned {
			return nil
		}
		text = stripped
	}
	quoteRaw := dataMessage["quote"]
	if !pyrepr.Truthy(quoteRaw) {
		quoteRaw = map[string]any{}
	}
	quoteData, err := dictOf(quoteRaw)
	if err != nil {
		return err
	}
	var replyToID *string
	if id := quoteData["id"]; pyrepr.Truthy(id) {
		replyToID = optional(pyrepr.Str(id))
	}
	replyToAuthor := extractQuoteAuthor(quoteRaw)
	attachmentsData := getOr(dataMessage, "attachments", []any{})
	mediaURLs, mediaTypes := []string{}, []string{}
	if pyrepr.Truthy(attachmentsData) && !a.IgnoreAttachments {
		if mediaURLs, mediaTypes, err = a.collectAttachments(ctx, attachmentsData); err != nil {
			return err
		}
	}
	if pystr.Strip(text) == "" && len(mediaURLs) == 0 {
		slog.Debug(fmt.Sprintf("Signal: skipping contentless envelope from %s (%d attachments)", gateway.RedactPhone(sender), len(mediaURLs)))
		return nil
	}
	fields := gateway.SourceFields{ChatType: "dm", UserID: &sender}
	senderName := ""
	if pyrepr.Truthy(senderNameRaw) {
		senderName = identText(senderNameRaw)
	}
	if groupInfo != nil {
		fields.ChatName = optText(groupInfo["groupName"])
	} else {
		fields.ChatName = optText(senderNameRaw)
	}
	userName := senderName
	if userName == "" {
		userName = sender
	}
	fields.UserName = &userName
	if pyrepr.Truthy(senderUUIDRaw) {
		alt := identText(senderUUIDRaw)
		fields.UserIDAlt = &alt
	}
	if isGroup {
		fields.ChatType = "group"
		fields.ChatIDAlt = &groupID
	}
	source := a.BuildSource(ctx, chatID, fields)
	msgType := gateway.MessageTypeText
	if len(mediaTypes) > 0 {
		msgType = gateway.MessageTypeDocument
	prefixes:
		for _, p := range mediaTypeByMimePrefix {
			for _, m := range mediaTypes {
				if strings.HasPrefix(m, p.prefix) {
					msgType = p.kind
					break prefixes
				}
			}
		}
	}
	tsMs := getOr(envelopeData, "timestamp", int64(0))
	timestamp, err := envelopeTimestamp(tsMs)
	if err != nil {
		return err
	}
	event := gateway.NewMessageEvent(text)
	event.Source, event.MessageType = source, msgType
	event.MediaURLs, event.MediaTypes, event.Timestamp = mediaURLs, mediaTypes, timestamp
	var rawQuote any
	if len(quoteData) > 0 {
		rawQuote = quoteRaw
	}
	event.RawMessage = map[string]any{"sender": senderRaw, "timestamp_ms": tsMs, "quote": rawQuote}
	event.ReplyToMessageID = replyToID
	event.ReplyToText = optText(quoteData["text"])
	event.ReplyToAuthorID = replyToAuthor
	event.ReplyToAuthorName = optText(pyrepr.Or(quoteData["authorName"], quoteData["authorProfileName"]))
	event.ReplyToIsOwnMessage = a.quoteReferencesOwnMessage(replyToID, replyToAuthor)
	slog.Debug(fmt.Sprintf("Signal: message from %s in %s: %s", gateway.RedactPhone(sender), pystr.Slice(chatID, 0, 20), pystr.Slice(text, 0, 50)))
	return a.HandleMessage(ctx, event)
}

// Group policy from the group allowlist, unset disabling groups and a star allowing all
func (a *SignalAdapter) groupAllowed(groupID string) bool {
	if len(a.groupAllowFrom) == 0 {
		slog.Debug("Signal: ignoring group message (no SIGNAL_GROUP_ALLOWED_USERS)")
		return false
	}
	if !a.groupAllowFrom["*"] && !a.groupAllowFrom[groupID] {
		label := "?"
		if groupID != "" {
			label = pystr.Slice(groupID, 0, 8)
		}
		slog.Debug(fmt.Sprintf("Signal: group %s not in allowlist", label))
		return false
	}
	return true
}

// Caches a number to service id mapping observed on an envelope or contact
func (a *SignalAdapter) rememberRecipientIdentifiers(number, serviceID any) {
	if !pyrepr.Truthy(number) || !pyrepr.Truthy(serviceID) || !isSignalServiceID(serviceID) {
		return
	}
	n, id := identText(number), serviceID.(string)
	a.mu.Lock()
	a.recipientUUIDByNumber[n] = id
	a.recipientNumberByUUID[id] = n
	a.mu.Unlock()
}

// Best available sender identifier of quote metadata, nil when it names none
func extractQuoteAuthor(quoteData any) *string {
	m, ok := pyjson.Dict(quoteData)
	if !ok {
		return nil
	}
	for _, k := range quoteAuthorKeys {
		if v := m[k]; pyrepr.Truthy(v) {
			s := pyrepr.Str(v)
			return &s
		}
	}
	return nil
}

// Whether a quote points at a message this adapter sent, by timestamp or by author identity
func (a *SignalAdapter) quoteReferencesOwnMessage(replyToID, replyToAuthor *string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if replyToID != nil && *replyToID != "" && a.sentMessageTimestamps.Has(*replyToID) {
		return true
	}
	if replyToAuthor == nil || *replyToAuthor == "" {
		return false
	}
	author, acct := pystr.Strip(*replyToAuthor), a.accountNormalized
	if acct == "" {
		return false
	}
	if author == acct {
		return true
	}
	if uuid, ok := a.recipientUUIDByNumber[acct]; ok && author == uuid {
		return true
	}
	number, ok := a.recipientNumberByUUID[author]
	return ok && number == acct
}

// Keeps a bounded most recently used cache of outbound timestamps for quote matching
func (a *SignalAdapter) rememberSentMessageTimestamp(timestamp any) {
	if timestamp == nil {
		return
	}
	key := pyrepr.Str(timestamp)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sentMessageTimestamps.Delete(key)
	a.sentMessageTimestamps.Set(key, struct{}{})
	for a.sentMessageTimestamps.Len() > maxSentMessageTimestamps {
		a.sentMessageTimestamps.Delete(a.sentMessageTimestamps.Keys()[0])
	}
}

// Service id of a listContacts entry whose number or recipient is the phone number, empty otherwise
func (a *SignalAdapter) extractContactUUID(contact any, phoneNumber string) string {
	m, ok := pyjson.Dict(contact)
	if !ok {
		return ""
	}
	serviceID := pyrepr.Or(m["uuid"], m["serviceId"])
	if profile, isDict := pyjson.Dict(m["profile"]); !pyrepr.Truthy(serviceID) && isDict {
		serviceID = pyrepr.Or(profile["serviceId"], profile["uuid"])
	}
	if pyrepr.Truthy(serviceID) && isSignalServiceID(serviceID) &&
		(pyrepr.Equal(m["number"], phoneNumber) || pyrepr.Equal(m["recipient"], phoneNumber)) {
		return serviceID.(string)
	}
	return ""
}

// Cached service id of a number
func (a *SignalAdapter) cachedRecipientUUID(number string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.recipientUUIDByNumber[number]
}

// Preferred recipient identifier of a direct chat, a phone number upgraded to its service id when known
func (a *SignalAdapter) resolveRecipient(ctx context.Context, chatID string) (string, error) {
	if chatID == "" || strings.HasPrefix(chatID, "group:") || !looksLikeE164Number(chatID) {
		return chatID, nil
	}
	if cached := a.cachedRecipientUUID(chatID); cached != "" {
		return cached, nil
	}
	a.recipientCacheLock.Lock()
	defer a.recipientCacheLock.Unlock()
	if cached := a.cachedRecipientUUID(chatID); cached != "" {
		return cached, nil
	}
	contacts, err := a.rpc(ctx, "listContacts", pyjson.ObjectOf("account", a.account, "allRecipients", true), rpcOptions{})
	if err != nil {
		return "", err
	}
	if list, ok := contacts.([]any); ok {
		for _, contact := range list {
			var number any
			if m, isDict := pyjson.Dict(contact); isDict {
				number = m["number"]
			}
			if serviceID := a.extractContactUUID(contact, chatID); pyrepr.Truthy(number) && serviceID != "" {
				a.rememberRecipientIdentifiers(number, serviceID)
			}
		}
	}
	if cached := a.cachedRecipientUUID(chatID); cached != "" {
		return cached, nil
	}
	return chatID, nil
}

// Adds the groupId or recipient routing key of the chat to the params in place
func (a *SignalAdapter) withTarget(ctx context.Context, params *pyjson.Object, chatID string, resolve bool) (*pyjson.Object, error) {
	if strings.HasPrefix(chatID, "group:") {
		params.Set("groupId", chatID[6:])
		return params, nil
	}
	recipient := chatID
	if resolve {
		var err error
		if recipient, err = a.resolveRecipient(ctx, chatID); err != nil {
			return nil, err
		}
	}
	params.Set("recipient", []any{recipient})
	return params, nil
}

// Fetches an attachment over JSON-RPC and caches it, returning the cached path and extension
func (a *SignalAdapter) fetchAttachment(ctx context.Context, attachmentID any) (string, string, error) {
	result, err := a.rpc(ctx, "getAttachment", pyjson.ObjectOf("account", a.account, "id", attachmentID), rpcOptions{})
	if err != nil {
		return "", "", err
	}
	if !pyrepr.Truthy(result) {
		return "", "", nil
	}
	if m, isDict := pyjson.Dict(result); isDict {
		result = m["data"]
		if !pyrepr.Truthy(result) {
			slog.Warn("Signal: attachment response missing 'data' key")
			return "", "", nil
		}
	}
	encoded, ok := result.(string)
	if !ok {
		return "", "", &pyErr{"TypeError", fmt.Sprintf("argument should be a bytes-like object or ASCII string, not '%s'", pyrepr.TypeName(result))}
	}
	rawData, err := pybase64.B64Decode(encoded)
	if err != nil {
		return "", "", err
	}
	ext := guessExtension(rawData)
	if ext == ".aac" {
		if remuxed, ok := remuxAACToM4A(ctx, rawData); ok {
			rawData, ext = remuxed, ".m4a"
		}
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
	}
	cache := cacheDocumentFromBytes
	switch {
	case isImageExt(ext):
		cache = cacheImageFromBytes
	case isAudioExt(ext):
		cache = cacheAudioFromBytes
	}
	path, err := cache(ctx, rawData, ext)
	return path, ext, err
}
