package plugins

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
)

const (
	CoreEventNamespace       = "hermes"
	eventEmitDepthCap        = 8
	eventPendingCapDefault   = 64
	eventDispatchWaitDefault = 2 * time.Second
)

// Event subscriber receiving a private deep copy of the payload under the emitter's context
type EventFunc func(ctx context.Context, payload map[string]any) (any, error)

// Host owned subscription ledger entry
type eventSubscription struct {
	owner string
	fn    EventFunc
	name  string
}

// Immutable dispatch envelope consumed by the event worker
type queuedPluginEvent struct {
	event         string
	payload       map[string]any
	subscriptions []*eventSubscription
	depth         int
	generation    int
	ctx           context.Context
}

type emitDepthKey struct{}

// Chain depth of the event delivery a context runs under
func emitDepth(ctx context.Context) int {
	d, _ := ctx.Value(emitDepthKey{}).(int)
	return d
}

// Deep copy of maps, slices, arrays, pointers and structs so a subscriber cannot mutate what the next sees
func deepCopy(v any) any {
	if v == nil {
		return nil
	}
	return deepCopyValue(reflect.ValueOf(v)).Interface()
}

// Recursive reflect copy behind deepCopy
func deepCopyValue(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(deepCopyValue(v.Elem()))
		return out
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		iter := v.MapRange()
		for iter.Next() {
			out.SetMapIndex(iter.Key(), deepCopyValue(iter.Value()))
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := range v.Len() {
			out.Index(i).Set(deepCopyValue(v.Index(i)))
		}
		return out
	case reflect.Array:
		out := reflect.New(v.Type()).Elem()
		for i := range v.Len() {
			out.Index(i).Set(deepCopyValue(v.Index(i)))
		}
		return out
	case reflect.Pointer:
		if v.IsNil() || v.Elem().Kind() == reflect.Func {
			return v
		}
		out := reflect.New(v.Elem().Type())
		out.Elem().Set(deepCopyValue(v.Elem()))
		return out
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		out.Set(v)
		for i := range v.NumField() {
			if out.Field(i).CanSet() {
				out.Field(i).Set(deepCopyValue(v.Field(i)))
			}
		}
		return out
	}
	return v
}

// Adds an owner tagged event subscription in registration order
func (m *Manager) subscribeEvent(owner, event string, fn EventFunc) error {
	if fn == nil {
		return errors.New("Event subscriber callback must be callable")
	}
	_, name := callbackNames(fn)
	m.eventMu.Lock()
	m.subscriptions[event] = append(m.subscriptions[event], &eventSubscription{owner: owner, fn: fn, name: name})
	m.eventMu.Unlock()
	return nil
}

// Removes every subscription of an owner, cancelling already queued deliveries, and returns the count
func (m *Manager) removePluginSubscriptions(owner string) int {
	removed := 0
	m.eventMu.Lock()
	defer m.eventMu.Unlock()
	for event, entries := range m.subscriptions {
		retained := make([]*eventSubscription, 0, len(entries))
		for _, e := range entries {
			if e.owner != owner {
				retained = append(retained, e)
			}
		}
		removed += len(entries) - len(retained)
		if len(retained) > 0 {
			m.subscriptions[event] = retained
		} else {
			delete(m.subscriptions, event)
		}
	}
	return removed
}

// Starts the single event worker goroutine once, caller holding the event lock
func (m *Manager) ensureEventWorkerLocked() {
	if m.eventWorkerRunning {
		return
	}
	m.eventWorkerRunning = true
	go m.eventWorkerLoop(m.eventQueue)
}

// Delivers queued events in order until a subscriber raises a user interrupt, which ends this worker
func (m *Manager) eventWorkerLoop(queue chan *queuedPluginEvent) {
	for item := range queue {
		interrupted := m.deliverEvent(item)
		m.markEventDone(item.generation)
		if interrupted {
			m.eventMu.Lock()
			m.eventWorkerRunning = false
			m.eventMu.Unlock()
			return
		}
	}
}

// Decrements the pending count of a generation and wakes waiters
func (m *Manager) markEventDone(generation int) {
	m.eventMu.Lock()
	if pending := m.eventPending[generation]; pending > 0 {
		m.eventPending[generation] = pending - 1
	}
	close(m.eventIdle)
	m.eventIdle = make(chan struct{})
	m.eventMu.Unlock()
}

// Delivers one queued event to every subscription still registered, true when a subscriber raised a user interrupt
func (m *Manager) deliverEvent(item *queuedPluginEvent) bool {
	m.eventMu.Lock()
	stale := item.generation != m.eventGeneration
	m.eventMu.Unlock()
	if stale {
		return false
	}
	ctx := context.WithValue(item.ctx, emitDepthKey{}, item.depth)
	for _, sub := range item.subscriptions {
		m.eventMu.Lock()
		if item.generation != m.eventGeneration {
			m.eventMu.Unlock()
			break
		}
		live := false
		for _, cur := range m.subscriptions[item.event] {
			if cur == sub {
				live = true
				break
			}
		}
		m.eventMu.Unlock()
		if !live {
			continue
		}
		payload, _ := deepCopy(item.payload).(map[string]any)
		v, err := guardedValue(func() (any, error) { return sub.fn(ctx, payload) })
		if err == nil {
			_, err = ResolvePluginCommandResult(ctx, v)
		}
		if isUserInterrupt(context.Background(), err) {
			return true
		}
		if err != nil {
			m.reportHookFailure(item.event, sub.name, item.payload, err, "Event")
		}
	}
	return false
}

// Waits until the current event generation is idle, false on timeout
func (m *Manager) waitForEventDispatch(timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	m.eventMu.Lock()
	generation := m.eventGeneration
	for {
		if m.eventPending[generation] == 0 {
			m.eventMu.Unlock()
			return true
		}
		idle := m.eventIdle
		m.eventMu.Unlock()
		select {
		case <-idle:
		case <-deadline.C:
			m.eventMu.Lock()
			ok := m.eventPending[generation] == 0
			m.eventMu.Unlock()
			return ok
		}
		m.eventMu.Lock()
	}
}

// Queues an event without blocking and returns the subscriber count scheduled
func (m *Manager) dispatchEvent(ctx context.Context, event string, payload map[string]any) int {
	depth := emitDepth(ctx)
	if depth >= eventEmitDepthCap {
		logWarn(fmt.Sprintf("Event bus recursion cap (%d) exceeded while dispatching '%s' — dropping this emit to prevent an infinite loop",
			eventEmitDepthCap, event))
		return 0
	}
	m.eventMu.Lock()
	defer m.eventMu.Unlock()
	subscriptions := append([]*eventSubscription(nil), m.subscriptions[event]...)
	if len(subscriptions) == 0 {
		return 0
	}
	generation := m.eventGeneration
	pending := m.eventPending[generation]
	budget := fmt.Sprintf("Event bus pending budget (%d) exhausted while dispatching '%s' — dropping this emit", m.eventPendingCap, event)
	if pending >= m.eventPendingCap {
		logWarn(budget)
		return 0
	}
	item := &queuedPluginEvent{event: event, payload: copyKwargs(payload), subscriptions: subscriptions,
		depth: depth + 1, generation: generation, ctx: context.WithoutCancel(ctx)}
	select {
	case m.eventQueue <- item:
	default:
		logWarn(budget)
		return 0
	}
	m.eventPending[generation] = pending + 1
	m.ensureEventWorkerLocked()
	return len(subscriptions)
}

// Renders every registered section in id order within the count and size budgets, failing open
func (m *Manager) RenderSystemPromptSections(sessionInfo map[string]any) []RenderedSystemPromptSection {
	if config.SafeWorkerEnabled() {
		return []RenderedSystemPromptSection{}
	}
	m.mu.RLock()
	ids := make([]string, 0, len(m.systemPromptSections))
	for id := range m.systemPromptSections {
		ids = append(ids, id)
	}
	sections := make([]*SystemPromptSection, 0, len(ids))
	sort.Strings(ids)
	for _, id := range ids {
		sections = append(sections, m.systemPromptSections[id])
	}
	m.mu.RUnlock()
	rendered := []RenderedSystemPromptSection{}
	total := utf8.RuneCountInString(PluginSectionsStart) + utf8.RuneCountInString(PluginSectionsEnd) + 2
	for _, section := range sections {
		if len(rendered) >= MaxSystemPromptSections {
			logWarn(fmt.Sprintf("Plugin system prompt section %s exceeded the section-count budget (%d) and was skipped",
				section.ID, MaxSystemPromptSections))
			continue
		}
		text, ok := renderPromptSectionText(section, sessionInfo)
		if !ok {
			continue
		}
		chars := utf8.RuneCountInString(FormatSystemPromptSection(section.ID, text))
		if len(rendered) > 0 {
			chars += 2
		}
		if total+chars > MaxSystemPromptSectionsTotalChars {
			logWarn(fmt.Sprintf("Plugin system prompt section %s (%s) exceeded the aggregate session budget (%d chars) and was skipped",
				section.ID, section.Plugin, MaxSystemPromptSectionsTotalChars))
			continue
		}
		rendered = append(rendered, RenderedSystemPromptSection{ID: section.ID, Content: text, Position: section.Position, Plugin: section.Plugin})
		total += chars
		logInfo(fmt.Sprintf("Session plugin prompt section: id=%s plugin=%s position=%s chars=%d",
			section.ID, section.Plugin, section.Position, utf8.RuneCountInString(text)))
	}
	return rendered
}

// Evaluates one section on a private copy of the session info and returns its stripped text when usable
func renderPromptSectionText(section *SystemPromptSection, sessionInfo map[string]any) (string, bool) {
	skip := func(detail string) {
		logWarn(fmt.Sprintf("Plugin system prompt section %s (%s) %s", section.ID, section.Plugin, detail))
	}
	value := section.Content
	if section.ContentFunc != nil {
		frozen, _ := deepCopy(copyKwargs(sessionInfo)).(map[string]any)
		v, err := guardedValue(func() (any, error) { return section.ContentFunc(frozen) })
		if isUserInterrupt(context.Background(), err) {
			raiseUserInterrupt(err)
		}
		if err != nil {
			skip(fmt.Sprintf("raised and was skipped: %v", err))
			return "", false
		}
		s, ok := v.(string)
		if !ok {
			skip(fmt.Sprintf("returned %s, not str; skipped", pyTypeName(v)))
			return "", false
		}
		value = s
	}
	text := strings.TrimSpace(value)
	if text == "" {
		return "", false
	}
	if strings.Contains(text, PluginSectionsStart) || strings.Contains(text, PluginSectionsEnd) {
		skip("contained a reserved persistence marker and was skipped")
		return "", false
	}
	if n := utf8.RuneCountInString(text); n > section.MaxChars {
		skip(fmt.Sprintf("exceeded max_chars (%d > %d) and was skipped", n, section.MaxChars))
		return "", false
	}
	return text, true
}

// Python type name of a decoded value, the Go type name for anything else
func pyTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return "int"
	case float32, float64:
		return "float"
	case string:
		return "str"
	case []byte:
		return "bytes"
	case map[string]any:
		return "dict"
	case []any:
		return "list"
	}
	return strings.TrimLeft(fmt.Sprintf("%T", v), "*")
}
