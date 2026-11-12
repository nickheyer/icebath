package observability

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// ClassifySendErrorHook returns the gateway send error kind of an error text, empty for None, set by pkg/gateway
var ClassifySendErrorHook func(errText string) string

// SendResult is the getattr surface of a gateway send result the delivery recorders read, nil reading None
type SendResult struct {
	Success     any
	RawResponse map[string]any
	RetryAfter  any
	ErrorKind   any
	Error       any
	Retryable   any
}

// gatewayJob is one recording queued on the FIFO metrics worker with the caller's profile scope
type gatewayJob struct {
	ctx  context.Context
	fn   func(ctx context.Context)
	done chan struct{}
}

var (
	gatewayMu      sync.Mutex
	gatewayCond    = sync.NewCond(&gatewayMu)
	gatewayPending []gatewayJob
	gatewayRunning bool
)

// Queues a recording on the single metrics worker in a detached copy of the caller's context
func submit(ctx context.Context, fn func(ctx context.Context)) <-chan struct{} {
	done := make(chan struct{})
	gatewayMu.Lock()
	gatewayPending = append(gatewayPending, gatewayJob{context.WithoutCancel(ctx), fn, done})
	if !gatewayRunning {
		gatewayRunning = true
		go gatewayWorker()
	}
	gatewayCond.Signal()
	gatewayMu.Unlock()
	return done
}

// Runs queued recordings one at a time in submission order
func gatewayWorker() {
	for {
		gatewayMu.Lock()
		for len(gatewayPending) == 0 {
			gatewayCond.Wait()
		}
		job := gatewayPending[0]
		gatewayPending = gatewayPending[1:]
		gatewayMu.Unlock()
		guarded(job.ctx, job.fn)
		close(job.done)
	}
}

// Runs one recording, a panic logged at debug and dropped
func guarded(ctx context.Context, fn func(ctx context.Context)) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Gateway shared metric not recorded", "error", r)
		}
	}()
	fn(ctx)
}

// Drain waits for every queued recording, a timeout error when the worker does not reach the end in time
func Drain(timeout time.Duration) error {
	select {
	case <-submit(context.Background(), func(context.Context) {}):
		return nil
	case <-time.After(timeout):
		return context.DeadlineExceeded
	}
}

// getattr(value, "platform", fallback) through the MetricsPlatform method gateway objects carry
func platformAttr(value, fallback any) any {
	if p, ok := value.(interface{ MetricsPlatform() any }); ok {
		return p.MetricsPlatform()
	}
	return fallback
}

// Public platform name of an adapter, source or raw platform value
func platform(ctx context.Context, value any) string {
	return AdapterPlatform(ctx, platformAttr(value, value))
}

var statusClasses = map[int]string{401: "auth", 403: "forbidden", 413: "too_long", 429: "rate_limited"}

// codeClassRule maps any needle found in a fatal code or type name to a connection error class
type codeClassRule struct {
	needles []string
	class   string
}

var codeClasses = []codeClassRule{
	{[]string{"missing", "dependency", "npm", "config", "conflict", "lock", "bind"}, "config"},
	{[]string{"auth", "credential", "token", "unauthor", "forbidden", "login"}, "auth"},
	{[]string{"ratelimit", "rate_limit", "flood", "too_many", "toomany", "429"}, "rate_limited"},
	{[]string{"network", "connect", "timeout", "socket", "dns", "ssl", "lost", "exited", "stream"}, "network"},
}

var sendKindClasses = map[string]string{
	"too_long": "too_long", "forbidden": "forbidden", "rate_limited": "rate_limited", "transient": "network",
}

// First rule with a needle inside text, other when none matches
func matchCodeClass(text string, rules []codeClassRule) string {
	for _, rule := range rules {
		for _, n := range rule.needles {
			if strings.Contains(text, n) {
				return rule.class
			}
		}
	}
	return "other"
}

// Stored HTTP status of one holder read as status_code, status, then code, within 100 to 599
func holderStatus(holder any) (int, bool) {
	if resp, ok := holder.(*http.Response); ok && resp != nil {
		holder = struct{ StatusCode int }{resp.StatusCode}
	}
	readers := []func(any) (int, bool){
		func(h any) (int, bool) {
			if s, ok := h.(interface{ StatusCode() int }); ok {
				return s.StatusCode(), true
			}
			if s, ok := h.(struct{ StatusCode int }); ok {
				return s.StatusCode, true
			}
			return 0, false
		},
		func(h any) (int, bool) {
			s, ok := h.(interface{ Status() int })
			if !ok {
				return 0, false
			}
			return s.Status(), true
		},
		func(h any) (int, bool) {
			s, ok := h.(interface{ Code() int })
			if !ok {
				return 0, false
			}
			return s.Code(), true
		},
	}
	for _, read := range readers {
		if v, ok := read(holder); ok && v >= 100 && v <= 599 {
			return v, true
		}
	}
	return 0, false
}

// HTTP status an error or its response stores, never a computed message
func statusCode(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	if v, ok := holderStatus(err); ok {
		return v, true
	}
	if r, ok := err.(interface{ Response() any }); ok && r.Response() != nil {
		return holderStatus(r.Response())
	}
	return 0, false
}

// Python exception class names of an error from the class itself through BaseException
func pyExceptionMRO(err error) []string {
	var declared interface{ PyExceptionMRO() []string }
	if errors.As(err, &declared) {
		return declared.PyExceptionMRO()
	}
	base := []string{"Exception", "BaseException"}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return append([]string{"TimeoutError", "OSError"}, base...)
	}
	var errno syscall.Errno
	if errors.As(err, &errno) || errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrExist) || errors.Is(err, fs.ErrPermission) {
		cls := pypath.OSErrorClass(err)
		switch cls {
		case "OSError":
			return append([]string{"OSError"}, base...)
		case "BrokenPipeError", "ConnectionAbortedError", "ConnectionRefusedError", "ConnectionResetError":
			return append([]string{cls, "ConnectionError", "OSError"}, base...)
		}
		return append([]string{cls, "OSError"}, base...)
	}
	if mro := pyrepr.BuiltinExceptionMRO(pyrepr.ExceptionName(err)); mro != nil {
		return mro
	}
	var names []string
	for e := err; e != nil; e = errors.Unwrap(e) {
		t := reflect.TypeOf(e)
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		names = append(names, t.Name())
	}
	return append(names, base...)
}

// Fatal code class, None for an empty code
func codeClass(code any) (string, bool) {
	text := pystr.Lower(pystr.Strip(pyText(code)))
	if text == "" {
		return "", false
	}
	return matchCodeClass(text, codeClasses), true
}

// Connection error class from the error's type and HTTP status only, never its message
func exceptionClass(err error) string {
	if err == nil {
		return "other"
	}
	if status, ok := statusCode(err); ok {
		switch status {
		case 401, 403:
			return "auth"
		case 429:
			return "rate_limited"
		}
	}
	mro := pyExceptionMRO(err)
	for _, name := range mro {
		if name == "ImportError" {
			return "config"
		}
	}
	for _, name := range mro {
		if name == "TimeoutError" || name == "ConnectionError" {
			return "network"
		}
	}
	names := ""
	for _, name := range mro {
		names += pystr.Lower(name)
	}
	return matchCodeClass(names, codeClasses[1:])
}

// ConnectErrorClass classifies a failed connect by its fatal code when set, else by the error
func ConnectErrorClass(err error, fatalCode any) string {
	if pyrepr.Truthy(fatalCode) {
		if cls, ok := codeClass(fatalCode); ok {
			return cls
		}
		return "other"
	}
	return exceptionClass(err)
}

// Python int value of a raw response status, bools and floats excluded
func plainInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case int32:
		return int(n), true
	}
	return 0, false
}

// DeliveryFailureClass says why an outbound delivery failed from the result's typed fields or the error type
func DeliveryFailureClass(result *SendResult, err error) string {
	if err != nil {
		if status, ok := statusCode(err); ok {
			if cls, ok := statusClasses[status]; ok {
				return cls
			}
		}
		switch cls := exceptionClass(err); cls {
		case "auth", "network", "rate_limited":
			return cls
		}
		return "other"
	}
	if result == nil {
		result = &SendResult{}
	}
	if raw := result.RawResponse; raw != nil {
		status, ok := raw["status_code"]
		if !ok {
			status = raw["status"]
		}
		if n, ok := plainInt(status); ok {
			if cls, ok := statusClasses[n]; ok {
				return cls
			}
		}
	}
	if result.RetryAfter != nil {
		return "rate_limited"
	}
	kind := result.ErrorKind
	if kind == nil {
		kind = ClassifySendErrorHook(pyText(result.Error))
	}
	if k, ok := kind.(string); ok {
		if cls, ok := sendKindClasses[k]; ok {
			return cls
		}
	}
	if pyrepr.Truthy(result.Retryable) {
		return "network"
	}
	return "other"
}

// PlatformHealthFields projects one health event, nil for an event outside the contract
func PlatformHealthFields(ctx context.Context, platformValue any, event, errorClass string) map[string]string {
	if !PlatformHealthEvents.Has(event) {
		return nil
	}
	cls := errorClass
	if event == "connect_ok" || event == "reconnect" {
		cls = "none"
	} else if !PlatformErrorClasses.Has(errorClass) {
		cls = "other"
	}
	return map[string]string{"error_class": cls, "event": event, "platform": platform(ctx, platformValue)}
}

// Classifies and records one health row on the worker
func recordHealth(ctx context.Context, platformValue any, event string, err error, fatalCode any) {
	errorClass := "none"
	if event != "connect_ok" && event != "reconnect" {
		errorClass = ConnectErrorClass(err, fatalCode)
	}
	emit(ctx, PlatformHealthMark, func() map[string]string {
		return PlatformHealthFields(ctx, platformValue, event, errorClass)
	})
}

// Context scoped to the profile home owning the row, the submitting scope for an empty home
func inHome(ctx context.Context, homeDir string) context.Context {
	if homeDir == "" {
		return ctx
	}
	return home.WithOverride(ctx, homeDir)
}

var (
	failingMu       sync.Mutex
	failingConnects = map[[2]string]string{}
)

// UTC calendar day the failed connect latch compares against
var gatewayToday = func() string { return time.Now().UTC().Format("2006-01-02") }

// Counts a failed connect episode once per profile, platform and UTC day, every other event always
func recordConnect(ctx context.Context, platformValue any, event string, err error, fatalCode any, terminal bool) {
	key := [2]string{home.Dir(ctx), pyrepr.Str(enumText(platformValue))}
	day := gatewayToday()
	var counted bool
	failingMu.Lock()
	if event != "connect_failed" || terminal {
		prev, ok := failingConnects[key]
		delete(failingConnects, key)
		counted = ok && prev == day
	} else if !relayEnabled(ctx) {
		failingMu.Unlock()
		return
	} else {
		prev, ok := failingConnects[key]
		counted = ok && prev == day
		failingConnects[key] = day
	}
	failingMu.Unlock()
	if !(counted && event == "connect_failed") {
		recordHealth(ctx, platformValue, event, err, fatalCode)
	}
}

// Fatal error code an adapter set, None when it carries none
func fatalErrorCode(adapter any) any {
	if a, ok := adapter.(interface{ FatalErrorCode() any }); ok {
		return a.FatalErrorCode()
	}
	return nil
}

// RecordPlatformConnect counts one adapter connect attempt from the runner's connect seam
func RecordPlatformConnect(ctx context.Context, adapter, platformValue any, isReconnect, ok bool, err error) {
	event := "connect_failed"
	if ok {
		event = "connect_ok"
		if isReconnect {
			event = "reconnect"
		}
	}
	var fatalCode any
	if !ok && err == nil {
		fatalCode = fatalErrorCode(adapter)
	}
	retryable := true
	if a, isA := adapter.(interface{ FatalErrorRetryable() bool }); isA {
		retryable = a.FatalErrorRetryable()
	}
	terminal := !ok && err == nil && pyrepr.Truthy(fatalCode) && !retryable
	submit(ctx, func(ctx context.Context) { recordConnect(ctx, platformValue, event, err, fatalCode, terminal) })
}

// RecordPlatformDisconnect counts a live adapter losing its connection, homeDir naming the owning profile
func RecordPlatformDisconnect(ctx context.Context, adapter any, homeDir string) {
	fatalCode := fatalErrorCode(adapter)
	if code, ok := fatalCode.(string); ok && code == "relay_disabled" {
		return
	}
	submit(ctx, func(ctx context.Context) { recordHealth(inHome(ctx, homeDir), adapter, "disconnect", nil, fatalCode) })
}

// DeliveryFields projects one logical outbound delivery
func DeliveryFields(ctx context.Context, platformValue any, failureClass string) map[string]string {
	cls, outcome := failureClass, "failed"
	if !DeliveryFailureClasses.Has(failureClass) {
		cls = "other"
	}
	if failureClass == "none" {
		outcome = "sent"
	}
	return map[string]string{"failure_class": cls, "outcome": outcome, "platform": platform(ctx, platformValue)}
}

// The send result surface of a returned value, nil for anything else
func sendResultOf(v any) *SendResult {
	switch r := v.(type) {
	case *SendResult:
		return r
	case interface{ MetricsSendResult() *SendResult }:
		return r.MetricsSendResult()
	}
	return nil
}

// RecordsDelivery runs one logical send with every retry inside it and counts it once
func RecordsDelivery(ctx context.Context, adapter, chatID any, send func() (any, error)) (any, error) {
	homeDir := chatHome(adapter, chatID)
	result, err := send()
	if err != nil {
		submit(ctx, func(ctx context.Context) { recordDelivery(inHome(ctx, homeDir), adapter, nil, err, chatID) })
		return result, err
	}
	if sr := sendResultOf(result); sr != nil {
		if _, isBool := sr.Success.(bool); isBool {
			submit(ctx, func(ctx context.Context) { recordDelivery(inHome(ctx, homeDir), adapter, sr, nil, chatID) })
		}
	}
	return result, nil
}

// Records one delivery row on the worker
func recordDelivery(ctx context.Context, adapter any, result *SendResult, err error, chatID any) {
	failureClass := "none"
	if err != nil || !pyrepr.Truthy(result.Success) {
		failureClass = DeliveryFailureClass(result, err)
	}
	emit(ctx, PlatformDeliveryMark, func() map[string]string {
		return DeliveryFields(ctx, sourcePlatform(adapter, chatID), failureClass)
	})
}

// StopsReplyClock runs a stream transport send or edit and stops the chat's reply clock on visible text
func StopsReplyClock(ctx context.Context, consumer any, sendOrEdit func() (any, error)) (any, error) {
	ok, err := sendOrEdit()
	if err != nil {
		return ok, err
	}
	if b, isBool := ok.(bool); isBool && b {
		if c, isC := consumer.(interface{ LastSentText() any }); isC && pyrepr.Truthy(c.LastSentText()) {
			var adapter, chatID any
			if a, isA := consumer.(interface{ MetricsAdapter() any }); isA {
				adapter = a.MetricsAdapter()
			}
			if ch, isCh := consumer.(interface{ MetricsChatID() any }); isCh {
				chatID = ch.MetricsChatID()
			}
			StopReplyClock(ctx, adapter, chatID, nil)
		}
	}
	return ok, nil
}

var replyThresholds = []threshold{{2.0, "lt_2s"}, {5.0, "2s_to_5s"}, {15.0, "5s_to_15s"}, {60.0, "15s_to_60s"}}

const (
	replyClockMax    = 1024
	replyClockMaxAge = 3600 * time.Second
	cronNotesMax     = 512
)

// clockKey is a platform value and chat id pair
type clockKey [2]string

// replyStart is a reply clock's monotonic start and owning home
type replyStart struct {
	at   time.Time
	home string
}

// orderedTable is an insertion ordered map bounded by eviction of its oldest key
type orderedTable[K comparable, V any] struct {
	keys []K
	m    map[K]V
}

func newOrderedTable[K comparable, V any]() *orderedTable[K, V] {
	return &orderedTable[K, V]{m: map[K]V{}}
}

// Removes and returns a key's value
func (t *orderedTable[K, V]) pop(k K) (V, bool) {
	v, ok := t.m[k]
	if ok {
		delete(t.m, k)
		for i, key := range t.keys {
			if key == k {
				t.keys = append(t.keys[:i], t.keys[i+1:]...)
				break
			}
		}
	}
	return v, ok
}

// Moves a key to the newest position and evicts the oldest beyond max
func (t *orderedTable[K, V]) put(k K, v V, max int) {
	t.pop(k)
	t.keys = append(t.keys, k)
	t.m[k] = v
	for len(t.keys) > max {
		delete(t.m, t.keys[0])
		t.keys = t.keys[1:]
	}
}

// Empties the table
func (t *orderedTable[K, V]) clear() {
	t.keys, t.m = nil, map[K]V{}
}

var (
	replyMu     sync.Mutex
	replyClocks = newOrderedTable[clockKey, replyStart]()
	chatHomes   = newOrderedTable[clockKey, string]()
	replyNow    = time.Now
)

// One connector adapter fronting several platforms
var frontingPlatforms = Set("relay")

// None or the empty string, the chat ids that key nothing
func emptyChatID(chatID any) bool {
	s, isStr := chatID.(string)
	return chatID == nil || (isStr && s == "")
}

// Platform a fronting adapter's chat really lives on, the adapter itself otherwise
func sourcePlatform(adapter, chatID any) any {
	p := platformAttr(adapter, nil)
	if !frontingPlatforms.Has(pyText(enumText(p))) || emptyChatID(chatID) {
		return adapter
	}
	resolve, ok := adapter.(interface {
		MetricsPlatformFor(chatID string) (any, error)
	})
	if !ok {
		return adapter
	}
	got, err := resolve.MetricsPlatformFor(pyrepr.Str(chatID))
	if err != nil || !pyrepr.Truthy(got) {
		return adapter
	}
	return got
}

// Reply clock key of a platform and chat, false when either is empty
func replyClockKey(platformValue, chatID any) (clockKey, bool) {
	value := pyText(enumText(platformValue))
	if value == "" || emptyChatID(chatID) {
		return clockKey{}, false
	}
	return clockKey{value, pyrepr.Str(chatID)}, true
}

// Chat id an inbound source or adapter carries
func chatIDAttr(v any) any {
	if c, ok := v.(interface{ MetricsChatID() any }); ok {
		return c.MetricsChatID()
	}
	return nil
}

// StartReplyClock starts the first reply clock of an accepted user turn, internal events never
func StartReplyClock(ctx context.Context, source any, internal bool) {
	if internal {
		return
	}
	key, ok := replyClockKey(platformAttr(source, nil), chatIDAttr(source))
	if !ok {
		return
	}
	h := home.Dir(ctx)
	replyMu.Lock()
	replyClocks.put(key, replyStart{replyNow(), h}, replyClockMax)
	chatHomes.put(key, h, replyClockMax)
	replyMu.Unlock()
}

// Home owning a chat's latest turn, a fronting adapter matching its one fronted chat, empty otherwise
func chatHome(adapter, chatID any) string {
	key, ok := replyClockKey(platformAttr(adapter, nil), chatID)
	if !ok {
		return ""
	}
	replyMu.Lock()
	defer replyMu.Unlock()
	if h, ok := chatHomes.m[key]; ok {
		return h
	}
	if frontingPlatforms.Has(key[0]) {
		var fronted []string
		for _, k := range chatHomes.keys {
			if k[1] == key[1] {
				fronted = append(fronted, chatHomes.m[k])
			}
		}
		if len(fronted) == 1 {
			return fronted[0]
		}
	}
	return ""
}

// StopReplyClock stops the chat's pending reply clock on its first outbound text, a failed send never
func StopReplyClock(ctx context.Context, adapter, chatID any, result any) {
	key, ok := replyClockKey(platformAttr(adapter, nil), chatID)
	if !ok {
		return
	}
	if result != nil {
		sr := sendResultOf(result)
		if sr == nil {
			return
		}
		if b, isBool := sr.Success.(bool); !isBool || !b {
			return
		}
	}
	replyMu.Lock()
	started, found := replyClocks.pop(key)
	startedKey := key
	if !found && frontingPlatforms.Has(key[0]) {
		var fronted []clockKey
		for _, k := range replyClocks.keys {
			if k[1] == key[1] {
				fronted = append(fronted, k)
			}
		}
		if len(fronted) == 1 {
			startedKey = fronted[0]
			started, found = replyClocks.pop(startedKey)
		}
	}
	replyMu.Unlock()
	if !found {
		return
	}
	elapsed := replyNow().Sub(started.at)
	if elapsed > replyClockMaxAge {
		return
	}
	var platformValue any
	if !frontingPlatforms.Has(startedKey[0]) {
		platformValue = startedKey[0]
	}
	seconds := elapsed.Seconds()
	submit(ctx, func(ctx context.Context) {
		emitReplyLatency(inHome(ctx, started.home), adapter, chatID, platformValue, seconds)
	})
}

// Records one first reply latency row labelled by the inbound platform
func emitReplyLatency(ctx context.Context, adapter, chatID, platformValue any, seconds float64) {
	emit(ctx, ReplyLatencyMark, func() map[string]string {
		p := platformValue
		if !pyrepr.Truthy(p) {
			p = sourcePlatform(adapter, chatID)
		}
		return ReplyLatencyFields(ctx, p, seconds)
	})
}

// ReplyLatencyFields projects one first reply latency
func ReplyLatencyFields(ctx context.Context, platformValue any, seconds float64) map[string]string {
	return map[string]string{
		"first_response_bucket": bucket(math.Max(0, seconds), replyThresholds, "gte_60s"),
		"platform":              platform(ctx, platformValue),
	}
}

var (
	cronMu       sync.Mutex
	cronDelivery = newOrderedTable[string, string]()
	cronSkipped  = map[string]bool{}
)

// CronDeliveryKind is where a job's output goes as a closed kind, targets never leaving the machine
func CronDeliveryKind(job any) string {
	m, ok := job.(map[string]any)
	if !ok {
		return "other"
	}
	var parts []any
	switch d := m["deliver"].(type) {
	case []any:
		parts = d
	case []string:
		for _, p := range d {
			parts = append(parts, p)
		}
	default:
		for _, p := range strings.Split(pyText(d), ",") {
			parts = append(parts, p)
		}
	}
	kinds := map[string]bool{}
	for _, p := range parts {
		kind, _, _ := strings.Cut(pystr.Lower(pystr.Strip(pyrepr.Str(p))), ":")
		if kind != "" {
			kinds[kind] = true
		}
	}
	local := true
	for k := range kinds {
		if k != "local" {
			local = false
		}
	}
	if local {
		return "local"
	}
	if kinds["none"] || kinds["off"] {
		return "none"
	}
	if len(kinds) == 1 && kinds["webhook"] {
		return "webhook"
	}
	return "platform"
}

// Execution id of a job dict, false when missing or falsy
func cronExecutionID(job any) (string, bool) {
	m, ok := job.(map[string]any)
	if !ok || !pyrepr.Truthy(m["execution_id"]) {
		return "", false
	}
	return pyrepr.Str(m["execution_id"]), true
}

// NoteCronExecution remembers the delivery kind of an execution about to run or be refused
func NoteCronExecution(job any) {
	id, ok := cronExecutionID(job)
	if !ok {
		return
	}
	cronMu.Lock()
	cronDelivery.put(id, CronDeliveryKind(job), cronNotesMax)
	cronMu.Unlock()
}

// NoteCronSkipped marks an execution a gate decided does no agent work
func NoteCronSkipped(job any) {
	id, ok := cronExecutionID(job)
	if !ok {
		return
	}
	cronMu.Lock()
	if len(cronSkipped) < cronNotesMax {
		cronSkipped[id] = true
	}
	cronMu.Unlock()
}

// Non negative seconds between two ISO timestamps, zero when either fails to parse or they mix awareness
func isoSeconds(start, end any) float64 {
	e, endAware, err := pydatetime.ParseISOFormat(pyrepr.Str(end))
	if err != nil {
		return 0
	}
	s, startAware, err := pydatetime.ParseISOFormat(pyrepr.Str(start))
	if err != nil || startAware != endAware {
		return 0
	}
	return math.Max(0, e.Sub(s).Seconds())
}

// CronRunFields projects one cron run row
func CronRunFields(outcome, deliveryKind string, seconds float64) map[string]string {
	if !CronDeliveryKinds.Has(deliveryKind) {
		deliveryKind = "other"
	}
	if !CronRunOutcomes.Has(outcome) {
		outcome = "failed"
	}
	return map[string]string{
		"delivery_kind":   deliveryKind,
		"duration_bucket": DurationBucket(int(seconds * 1000)),
		"outcome":         outcome,
	}
}

// RecordCronFinish counts one execution reaching its write once terminal row
func RecordCronFinish(ctx context.Context, record map[string]any, deliveryOutcome *string) {
	if record == nil || !pyrepr.Truthy(record["id"]) {
		return
	}
	id := pyrepr.Str(record["id"])
	cronMu.Lock()
	kind, ok := cronDelivery.pop(id)
	if !ok {
		kind = "other"
	}
	gated := cronSkipped[id]
	delete(cronSkipped, id)
	cronMu.Unlock()
	started := record["started_at"]
	success := record["status"] == "completed"
	var outcome string
	switch {
	case record["status"] == "cancelled":
		outcome = "cancelled"
	case gated || (!pyrepr.Truthy(started) && !success && deliveryOutcome == nil):
		outcome = "skipped"
	case success:
		outcome = "success"
	default:
		outcome = "failed"
	}
	seconds := 0.0
	if pyrepr.Truthy(started) {
		seconds = isoSeconds(started, record["finished_at"])
	}
	submit(ctx, func(ctx context.Context) {
		emit(ctx, CronRunMark, func() map[string]string { return CronRunFields(outcome, kind, seconds) })
	})
}

// RecordCronMissed counts a scheduled occurrence the scheduler dropped without running it
func RecordCronMissed(ctx context.Context, job any) {
	kind := CronDeliveryKind(job)
	submit(ctx, func(ctx context.Context) {
		emit(ctx, CronRunMark, func() map[string]string { return CronRunFields("missed", kind, 0) })
	})
}
