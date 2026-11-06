package gateway

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

const (
	RestartWaitReason          = "restart_wait"
	drainRequestFilename       = ".drain_request.json"
	DrainRequestMaxAgeSeconds  = 3600.0
	webSessionGatewaySocket    = "gateway"
	restartWaitSocketTicketURL = "/api/webapp/socket-ticket"
)

// DrainRunner is the runner state the restart wait checks read and the descriptor they stamp
type DrainRunner interface {
	SessionRuntimeDescriptor() map[string]any
	Draining() bool
	Running() bool
}

// Publishes the restart after turn wait as the drain reason unless a drain already named its own
func OpenRestartWait(runner DrainRunner) {
	descriptor := runner.SessionRuntimeDescriptor()
	if descriptor != nil && !pyrepr.Truthy(descriptor["drain_reason"]) {
		descriptor["drain_reason"] = RestartWaitReason
	}
}

// True while the runner is draining, still running and in a restart after turn wait
func RestartWaitOpen(runner DrainRunner) bool {
	descriptor := runner.SessionRuntimeDescriptor()
	reason, _ := descriptor["drain_reason"].(string)
	return runner.Draining() && runner.Running() && reason == RestartWaitReason
}

var restartWaitReads = regexp.MustCompile(`\A(?:/api/(?:health|status|profiles/sessions(?:/sidebar)?|sessions/[^/]+(?:/messages(?:/around)?|/timeline)?))\z`)

// True for an allowlisted GET while the runner is in a restart after turn wait
func RestartWaitRead(runner DrainRunner, method, path string) bool {
	return method == "GET" && restartWaitReads.MatchString(path) && RestartWaitOpen(runner)
}

// True for a gateway socket ticket mint during the restart wait, exempting it from the drain refusal only
func RestartWaitSocketTicket(runner DrainRunner, method, path, query string) bool {
	if method != "POST" || path != restartWaitSocketTicketURL {
		return false
	}
	socket := pyurl.ParseQS(query, false)["socket"]
	return len(socket) == 1 && socket[0] == webSessionGatewaySocket && RestartWaitOpen(runner)
}

var restartWaitWSMethods = map[string]bool{
	"ping": true, "runtime.describe": true, "session.resume": true, "session.detach": true, "session.events.since": true,
	"session.info": true, "session.list": true, "prompt.submit": true, "prompt.receipt": true, "session.interrupt": true,
	"approval.pending": true, "approval.received": true, "approval.respond": true, "clarify.respond": true, "clarify.lock": true,
}

// WebSocket verbs an interactive client may still call during the restart wait
func RestartWaitWSMethods() []string {
	out := make([]string, 0, len(restartWaitWSMethods))
	for m := range restartWaitWSMethods {
		out = append(out, m)
	}
	return out
}

// True when an interactive WebSocket verb is refused because the runner is draining
func WSMethodRefusedWhileDraining(runner DrainRunner, method string) bool {
	return runner.Draining() && !restartWaitWSMethods[method]
}

var (
	epochMu       sync.Mutex
	epochCached   *string
	procReadFile  = os.ReadFile
	expiryLogMu   sync.Mutex
	expiryLogged  *string
	drainEpochNow = func() time.Time { return time.Now().UTC() }
)

// Container or VM instantiation identity of boot id and PID 1 start time, empty when neither is readable
var CurrentInstantiationEpoch = func() string {
	epochMu.Lock()
	defer epochMu.Unlock()
	if epochCached != nil {
		return *epochCached
	}
	bootID, pid1Start := "", ""
	if b, err := procReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		bootID = pystr.Strip(string(b))
	}
	if b, err := procReadFile("/proc/1/stat"); err == nil {
		if i := strings.LastIndex(string(b), ")"); i >= 0 {
			if fields := pystr.SplitWhitespace(string(b)[i+1:], -1); len(fields) > 19 {
				pid1Start = fields[19]
			}
		}
	}
	epoch := ""
	if bootID != "" || pid1Start != "" {
		epoch = bootID + ":" + pid1Start
	}
	epochCached = &epoch
	return epoch
}

// Drops the cached instantiation epoch
func instantiationEpochCacheClear() {
	epochMu.Lock()
	epochCached = nil
	epochMu.Unlock()
}

// Drain request marker inside the home, the active profile home when empty
func DrainRequestPath(ctx context.Context, h string) string {
	if h == "" {
		h = home.Dir(ctx)
	}
	return filepath.Join(h, drainRequestFilename)
}

// Writes the begin drain marker atomically stamped with the epoch, a rewrite refreshes its clock
func WriteDrainRequest(ctx context.Context, principal string, suppressNotification bool, h string) (*pyjson.Object, error) {
	payload := pyjson.NewObject()
	payload.Set("action", "drain")
	payload.Set("requested_at", pydatetime.ISOFormat(drainEpochNow()))
	payload.Set("principal", principal)
	payload.Set("epoch", CurrentInstantiationEpoch())
	payload.Set("suppress_notification", suppressNotification)
	if err := config.AtomicJSONWrite(DrainRequestPath(ctx, h), payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// Removes the drain marker, true when one existed
func ClearDrainRequest(ctx context.Context, h string) bool {
	path := DrainRequestPath(ctx, h)
	err := os.Remove(path)
	if err == nil {
		return true
	}
	if !errors.Is(err, fs.ErrNotExist) {
		slog.Warn(fmt.Sprintf("drain-control: failed to remove %s: %s", path, pypath.OSErrorText(err)))
	}
	return false
}

// True when requested_at parses and is past the max age, warning once per marker
func markerIsExpired(body *pyjson.Object) bool {
	raw, _ := body.Get("requested_at")
	requestedAt, ok := memoryParseISO(raw)
	if !ok {
		return false
	}
	age := float64(drainEpochNow().Sub(requestedAt).Microseconds()) / 1e6
	if age <= DrainRequestMaxAgeSeconds {
		return false
	}
	rawText := raw.(string)
	expiryLogMu.Lock()
	first := expiryLogged == nil || *expiryLogged != rawText
	if first {
		expiryLogged = &rawText
	}
	expiryLogMu.Unlock()
	if first {
		principal, _ := body.Get("principal")
		slog.Warn(fmt.Sprintf("drain-control: ignoring expired drain marker (requested_at=%s, age=%.0fs > max %.0fs, principal=%s) "+
			"— the drain that wrote it was never cancelled; treating as stale so the gateway keeps accepting turns.",
			rawText, age, DrainRequestMaxAgeSeconds, pyrepr.Str(principal)))
	}
	return true
}

// Marker body when present, same epoch and unexpired
func activeDrainBody(ctx context.Context, h string) *pyjson.Object {
	body := ReadDrainRequest(ctx, h)
	if body == nil {
		return nil
	}
	current := CurrentInstantiationEpoch()
	markerEpoch, _ := body.Get("epoch")
	if s, isStr := markerEpoch.(string); current != "" && pyrepr.Truthy(markerEpoch) && (!isStr || s != current) {
		return nil
	}
	if markerIsExpired(body) {
		return nil
	}
	return body
}

// True when an active begin drain marker exists
func DrainRequested(ctx context.Context, h string) bool { return activeDrainBody(ctx, h) != nil }

// True when an active marker asks to suppress the shutdown broadcast
func DrainNotificationSuppressed(ctx context.Context, h string) bool {
	body := activeDrainBody(ctx, h)
	if body == nil || body.Len() == 0 {
		return false
	}
	v, _ := body.Get("suppress_notification")
	return pyrepr.Truthy(v)
}

// Marker payload, an empty object when unparseable or not a dict, nil when absent or unreadable
func ReadDrainRequest(ctx context.Context, h string) *pyjson.Object {
	path := DrainRequestPath(ctx, h)
	b, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn(fmt.Sprintf("drain-control: failed to read %s: %s", path, pypath.OSErrorText(err)))
		}
		return nil
	}
	b = []byte(strings.TrimPrefix(string(b), "\xef\xbb\xbf"))
	if !utf8.Valid(b) {
		return pyjson.NewObject()
	}
	data, err := pyjson.Loads(string(b))
	if err != nil {
		return pyjson.NewObject()
	}
	if obj, ok := data.(*pyjson.Object); ok {
		return obj
	}
	return pyjson.NewObject()
}
