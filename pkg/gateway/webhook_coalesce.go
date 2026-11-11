package gateway

import (
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	webhookCoalesceDefaultWindowSeconds  = 30.0
	webhookCoalesceDefaultMaxWaitSeconds = 300.0
)

var webhookTemplateFieldRE = regexp.MustCompile(`\{[a-zA-Z0-9_.]+\}`)

// Logger of the webhook coalescing module
func webhookCoalesceLog() *slog.Logger {
	return slog.Default().With("logger", "gateway.platforms.webhook_coalesce")
}

// Startup check of a route coalesce block, the ValueError a malformed block raises
func ValidateCoalesceConfig(routeName string, route *pyjson.Object) error {
	coalesce, found := route.Get("coalesce")
	if !found || coalesce == nil {
		return nil
	}
	for _, exclusive := range []string{"deliver_only", "cron_job"} {
		if pyrepr.Truthy(route.GetOr(exclusive, nil)) {
			return &PyError{Kind: "ValueError", Msg: fmt.Sprintf("[webhook] Route '%s' combines %s with coalesce. Coalescing only "+
				"applies to agent-mode routes.", routeName, exclusive)}
		}
	}
	block, isDict := pyjson.AsObject(coalesce)
	var key any
	if isDict {
		key = block.GetOr("key", nil)
	}
	if text, ok := key.(string); !ok || pystr.Strip(text) == "" {
		return &PyError{Kind: "ValueError", Msg: fmt.Sprintf("[webhook] Route '%s' has a coalesce block without a non-empty string 'key'. "+
			"Set coalesce.key to a payload field (e.g. 'pull_request.number') or a template "+
			"(e.g. '{repository.full_name}#{number}').", routeName)}
	}
	for _, field := range []string{"window_seconds", "max_wait_seconds"} {
		value := block.GetOr(field, nil)
		if value == nil {
			continue
		}
		if _, isBool := value.(bool); isBool || !webhookPositiveNumberValue(value) {
			return &PyError{Kind: "ValueError", Msg: fmt.Sprintf("[webhook] Route '%s' coalesce.%s must be a positive number, got %s.",
				routeName, field, pyrepr.Repr(value))}
		}
	}
	return nil
}

// Whether a value is an int or float greater than zero
func webhookPositiveNumberValue(value any) bool {
	switch v := value.(type) {
	case float64:
		return v > 0
	case float32:
		return v > 0
	}
	if n, ok := pyrepr.Int(value); ok {
		return n.Sign() > 0
	}
	return false
}

// Arguments the adapter spawner receives with a settled event besides payload, prompt and delivery id
type WebhookDispatchArgs struct {
	RouteConfig *pyjson.Object
	RouteName   string
	Profile     *string
	EventType   any
}

// WebhookPendingEvent is the newest event of a coalesce group with the group bookkeeping
type WebhookPendingEvent struct {
	Payload      any
	Prompt       string
	DeliveryID   string
	FirstAt      float64
	Count        int
	DispatchArgs WebhookDispatchArgs
	Superseded   []string
}

// Prompt with the coalescing note appended when the group held more than one event
func (e *WebhookPendingEvent) PromptWithNote() string {
	if e.Count <= 1 {
		return e.Prompt
	}
	return fmt.Sprintf("%s\n\n(Note: %d webhook events for this item arrived in quick succession "+
		"and were coalesced — this is the most recent one; earlier events are superseded.)", e.Prompt, e.Count)
}

// Spawner of the merged run of a settled group, its task awaited by a flush
type WebhookCoalesceDispatch func(payload any, prompt, deliveryID string, now float64, args WebhookDispatchArgs, superseded []string) *adapterTask

// Renderer of a coalesce key template against the payload, event type and route
type WebhookCoalesceRender func(template string, payload any, eventType any, routeName string) (string, error)

// One quiet window timer, cancelled by closing its channel
type webhookCoalesceTimer struct {
	cancel chan struct{}
	done   chan struct{}
}

// Whether the timer finished its wait or was cancelled
func (t *webhookCoalesceTimer) isDone() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

// WebhookCoalescer debounces events of one entity and dispatches only the newest once quiet
type WebhookCoalescer struct {
	dispatch WebhookCoalesceDispatch
	render   WebhookCoalesceRender
	mu       sync.Mutex
	pending  *pydict.Dict[string, *WebhookPendingEvent]
	timers   *pydict.Dict[string, *webhookCoalesceTimer]
}

// Coalescer dispatching through the adapter spawner and rendering keys with its prompt renderer
func NewWebhookCoalescer(dispatch WebhookCoalesceDispatch, render WebhookCoalesceRender) *WebhookCoalescer {
	return &WebhookCoalescer{dispatch: dispatch, render: render, pending: pydict.New[string, *WebhookPendingEvent](),
		timers: pydict.New[string, *webhookCoalesceTimer]()}
}

// Pending groups by key in arrival order
func (c *WebhookCoalescer) Pending() map[string]*WebhookPendingEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]*WebhookPendingEvent{}
	for key, entry := range c.pending.All() {
		out[key] = entry
	}
	return out
}

// Route qualified group key of a dotted field or brace template, false when a field did not resolve
func (c *WebhookCoalescer) GroupKey(routeName, keyTemplate string, payload, eventType any) (string, bool, error) {
	template := pystr.Strip(keyTemplate)
	if !containsRune(template, '{') {
		template = "{" + template + "}"
	}
	rendered, err := c.render(template, payload, eventType, routeName)
	if err != nil {
		return "", false, err
	}
	if webhookTemplateFieldRE.MatchString(rendered) {
		return "", false, nil
	}
	return routeName + "|" + rendered, true, nil
}

// Whether a text holds the rune
func containsRune(text string, r rune) bool {
	for _, c := range text {
		if c == r {
			return true
		}
	}
	return false
}

// Buffers one event, false when the key did not resolve and the caller dispatches at once
func (c *WebhookCoalescer) Enqueue(routeName string, coalesce *pyjson.Object, payload, eventType any, prompt, deliveryID string,
	now float64, args WebhookDispatchArgs) (bool, error) {
	keyValue, _ := coalesce.Get("key")
	keyText, ok := keyValue.(string)
	if !ok {
		return false, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute 'strip'", pyrepr.TypeName(keyValue))}
	}
	groupKey, resolved, err := c.GroupKey(routeName, keyText, payload, eventType)
	if err != nil {
		return false, err
	}
	if !resolved {
		webhookCoalesceLog().Info(fmt.Sprintf("[webhook] coalesce key %s unresolved for delivery %s on route %s — dispatching immediately",
			pyrepr.Repr(keyText), deliveryID, routeName))
		return false, nil
	}
	window, err := webhookCoalesceSeconds(coalesce, "window_seconds", webhookCoalesceDefaultWindowSeconds)
	if err != nil {
		return false, err
	}
	maxWait, err := webhookCoalesceSeconds(coalesce, "max_wait_seconds", webhookCoalesceDefaultMaxWaitSeconds)
	if err != nil {
		return false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	existing, _ := c.pending.Get(groupKey)
	firstAt, count := now, 1
	var superseded []string
	if existing != nil {
		firstAt, count = existing.FirstAt, existing.Count+1
		superseded = append(append([]string(nil), existing.Superseded...), existing.DeliveryID)
	}
	args.RouteName, args.EventType = routeName, eventType
	entry := &WebhookPendingEvent{Payload: payload, Prompt: prompt, DeliveryID: deliveryID, FirstAt: firstAt, Count: count,
		DispatchArgs: args, Superseded: superseded}
	c.pending.Set(groupKey, entry)
	if existing != nil {
		webhookCoalesceLog().Info(fmt.Sprintf("[webhook] coalesced delivery %s superseded by %s (group=%s, %d events)",
			existing.DeliveryID, deliveryID, groupKey, count))
	}
	if old, found := c.timers.Get(groupKey); found {
		c.timers.Delete(groupKey)
		if !old.isDone() {
			close(old.cancel)
		}
	}
	delay := math.Min(window, math.Max(0, firstAt+maxWait-now))
	timer := &webhookCoalesceTimer{cancel: make(chan struct{}), done: make(chan struct{})}
	c.timers.Set(groupKey, timer)
	go c.runTimer(groupKey, timer, delay)
	return true, nil
}

// Float of a coalesce timing field, its default when absent
func webhookCoalesceSeconds(coalesce *pyjson.Object, field string, def float64) (float64, error) {
	value, found := coalesce.Get(field)
	if !found {
		return def, nil
	}
	return pyint.Float(value)
}

// Waits out the quiet window then settles the group, a cancelled timer leaving the pending entry
func (c *WebhookCoalescer) runTimer(groupKey string, timer *webhookCoalesceTimer, delay float64) {
	defer close(timer.done)
	if delay > 0 {
		wait := time.NewTimer(time.Duration(delay * float64(time.Second)))
		defer wait.Stop()
		select {
		case <-wait.C:
		case <-timer.cancel:
			return
		}
	}
	c.mu.Lock()
	select {
	case <-timer.cancel:
		c.mu.Unlock()
		return
	default:
	}
	if current, _ := c.timers.Get(groupKey); current == timer {
		c.timers.Delete(groupKey)
	}
	entry, found := c.pending.Get(groupKey)
	if found {
		c.pending.Delete(groupKey)
	}
	c.mu.Unlock()
	if entry != nil {
		c.settle(groupKey, entry)
	}
}

// Dispatches the newest event of a group with the coalescing note and its superseded ids
func (c *WebhookCoalescer) settle(groupKey string, entry *WebhookPendingEvent) *adapterTask {
	webhookCoalesceLog().Info(fmt.Sprintf("[webhook] coalesce settled group=%s events=%d delivery=%s", groupKey, entry.Count, entry.DeliveryID))
	now := float64(time.Now().UnixNano()) / 1e9
	return c.dispatch(entry.Payload, entry.PromptWithNote(), entry.DeliveryID, now, entry.DispatchArgs, entry.Superseded)
}

// Dispatches every pending group now and waits until each run is handed to the runner
func (c *WebhookCoalescer) Flush() {
	c.mu.Lock()
	for _, timer := range c.timers.All() {
		if !timer.isDone() {
			select {
			case <-timer.cancel:
			default:
				close(timer.cancel)
			}
		}
	}
	c.timers.Clear()
	pending := c.pending
	c.pending = pydict.New[string, *WebhookPendingEvent]()
	c.mu.Unlock()
	var handedOff []*adapterTask
	for groupKey, entry := range pending.All() {
		task := c.settleRecovering(groupKey, entry)
		if task != nil {
			handedOff = append(handedOff, task)
		}
	}
	for _, task := range handedOff {
		<-task.done
	}
}

// Settles a group during a flush, a dispatch panic logged and the flush continuing
func (c *WebhookCoalescer) settleRecovering(groupKey string, entry *WebhookPendingEvent) (task *adapterTask) {
	defer func() {
		if rec := recover(); rec != nil {
			webhookCoalesceLog().Error(fmt.Sprintf("[webhook] failed to flush coalesced group %s", groupKey), "error", rec)
			task = nil
		}
	}()
	return c.settle(groupKey, entry)
}
