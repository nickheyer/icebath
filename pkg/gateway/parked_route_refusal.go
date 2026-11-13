package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Unacknowledged delivery routed to a parked profile, nothing admitted and answered retryably
type ParkedRouteRefused struct{}

const (
	ParkedRouteRefusedCode   = "profile_parked"
	ParkedRouteRefusedStatus = 503
)

// Headers a retryable parked route answer carries
var ParkedRouteRefusedHeaders = map[string]string{"Retry-After": "30"}

func (*ParkedRouteRefused) Error() string {
	return "The routed profile is not being served right now; retry this delivery"
}

// Producer admission refused because the delivery routes to the parked profile, retryable for as long as the park lasts
type ParkedProfileRefusal struct {
	*state.RuntimeStoreError
	Profile string
}

func NewParkedProfileRefusal(profile string) *ParkedProfileRefusal {
	field, detail := "profile", "the routed profile is parked"
	return &ParkedProfileRefusal{RuntimeStoreError: state.NewRuntimeStoreError("runtime_draining", &field, &detail), Profile: profile}
}

func (e *ParkedProfileRefusal) Unwrap() error { return e.RuntimeStoreError }

// Awaited dispatch scope whose transport has not acknowledged the delivery yet
type RetryableIngressScope struct {
	mu      sync.Mutex
	open    bool
	refused bool
}

// Whether a parked route inside the scope was recorded for a retryable answer
func (s *RetryableIngressScope) Refused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refused
}

type retryableIngressKey struct{}

// Scopes an awaited dispatch so a parked route is only recorded, closed when the returned func runs
func RetryableIngress(ctx context.Context) (context.Context, *RetryableIngressScope, func()) {
	scope := &RetryableIngressScope{open: true}
	return context.WithValue(ctx, retryableIngressKey{}, scope), scope, func() {
		scope.mu.Lock()
		scope.open = false
		scope.mu.Unlock()
	}
}

// Generic outage notice the chat gets, never naming the profile or why it is parked
const ParkedRouteNotice = "The agent is currently unavailable; please resend your message in a moment."

// One notice per profile, platform and chat within this many seconds
const ParkedNoticeWindowS = 60.0

var parkedEpoch = time.Now()

// Monotonic seconds the notice window reads, a package var so tests drive the clock
var parkedMonotonic = func() float64 { return time.Since(parkedEpoch).Seconds() }

type parkedNoticeKey struct{ profile, platform, chatID string }

// True and the window restarted when this chat was not told about the profile within the window
func parkedNoticeDue(adapter PlatformAdapter, profile, platform, chatID string) bool {
	base := adapter.Base()
	if base == nil {
		return true
	}
	base.mu.Lock()
	defer base.mu.Unlock()
	sent := lazyAttr(&base.parkedNoticeSentAt)
	now, key := parkedMonotonic(), parkedNoticeKey{profile, platform, chatID}
	for stale, at := range sent {
		if now-at >= ParkedNoticeWindowS {
			delete(sent, stale)
		}
	}
	if _, told := sent[key]; told {
		return false
	}
	sent[key] = now
	return true
}

// Adapter display name, the source platform, then a question mark
func parkedAdapterName(adapter PlatformAdapter, source *SessionSource) string {
	if adapter != nil {
		if name := adapter.Name(); name != "" {
			return name
		}
	}
	if source != nil && source.Platform != "" {
		return string(source.Platform)
	}
	return "?"
}

// Logs an inbound event dropped for an unserved profile route and refuses it in channel when that profile is parked
func dropRejectedRoute(ctx context.Context, adapter PlatformAdapter, source *SessionSource) {
	name := parkedAdapterName(adapter, source)
	profile := ""
	if source.rejectedRouteProfile != nil {
		profile = *source.rejectedRouteProfile
	}
	var runner any
	if adapter != nil {
		if base := adapter.Base(); base != nil {
			runner = base.GatewayRunner
		}
	}
	if adapter == nil || !profileParked(runner, profile) {
		slog.Warn(fmt.Sprintf("[%s] Dropping inbound event for %s: explicit profile route targets an unserved profile", name, source.ChatID))
		return
	}
	refuseParkedRoute(ctx, adapter, source, profile, "")
}

// Refuses an event routed to the parked profile, retryably inside a retryable ingress scope, else in channel off the caller
func refuseParkedRoute(ctx context.Context, adapter PlatformAdapter, source *SessionSource, profile, replyTo string) {
	name := parkedAdapterName(adapter, source)
	platform := string(source.Platform)
	if platform == "" {
		platform = "?"
	}
	quoted := pyrepr.Repr(profile)
	if scope, _ := ctx.Value(retryableIngressKey{}).(*RetryableIngressScope); scope != nil {
		scope.mu.Lock()
		open := scope.open
		if open {
			scope.refused = true
		}
		scope.mu.Unlock()
		if open {
			slog.Warn(fmt.Sprintf("[%s] Refusing inbound event for %s: routed profile %s on %s is parked (reason=profile_parked); "+
				"nothing was admitted, the delivery is answered retryably", name, source.ChatID, quoted, platform))
			return
		}
	}
	if !parkedNoticeDue(adapter, profile, platform, source.ChatID) {
		slog.Info(fmt.Sprintf("[%s] Refusing inbound event for %s: routed profile %s on %s is parked (reason=profile_parked); "+
			"nothing was admitted, the chat was already told within %.0fs", name, source.ChatID, quoted, platform, ParkedNoticeWindowS))
		return
	}
	slog.Warn(fmt.Sprintf("[%s] Refusing inbound event for %s: routed profile %s on %s is parked (reason=profile_parked); "+
		"nothing was admitted, a resend notice to the sender is scheduled", name, source.ChatID, quoted, platform))
	if replyTo == "" && source.MessageID != nil {
		replyTo = *source.MessageID
	}
	task := spawnAdapterTask(context.WithoutCancel(ctx), func(tctx context.Context) error {
		sendParkedRefusal(tctx, adapter, source, replyTo)
		return nil
	})
	if base := adapter.Base(); base != nil {
		base.trackBackgroundTask(task)
	}
}

// Runner thread metadata a parked refusal replies under, gateway/run.py _thread_metadata_for_source
type threadMetadataRunner interface {
	ThreadMetadataForSource(source *SessionSource, replyToMessageID *string) map[string]any
}

// Replies in the sender chat and thread as an interim send, logging only safe fields of a failure
func sendParkedRefusal(ctx context.Context, adapter PlatformAdapter, source *SessionSource, replyTo string) {
	name := adapter.Name()
	fail := func(err error) {
		slog.Warn(fmt.Sprintf("[%s] Could not send the parked-profile refusal to %s", name, source.ChatID), "exc_info", err)
	}
	runner, ok := adapter.Base().GatewayRunner.(threadMetadataRunner)
	if !ok {
		fail(fmt.Errorf("AttributeError: '%s' object has no attribute '_thread_metadata_for_source'", pyrepr.TypeName(adapter.Base().GatewayRunner)))
		return
	}
	threadMetadata := runner.ThreadMetadataForSource(source, nil)
	metadata := map[string]any{}
	for k, v := range threadMetadata {
		metadata[k] = v
	}
	metadata["_interim_send"] = true
	result, err := adapter.Send(ctx, source.ChatID, ParkedRouteNotice, replyTo, metadata)
	if err != nil {
		fail(err)
		return
	}
	if !result.Success {
		kind := "None"
		if result.ErrorKind != nil {
			kind = *result.ErrorKind
		}
		slog.Warn(fmt.Sprintf("[%s] The parked-profile refusal to %s was not delivered (error_kind=%s, retryable=%s)",
			name, source.ChatID, kind, pyrepr.Repr(result.Retryable)))
	}
}
