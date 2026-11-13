package system

import (
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/dedupe"
)

const (
	notificationDedupeInterval = time.Second
	notificationRetentionTTL   = 10 * time.Minute
)

// NotifyWindow is a renderer window a notification focuses and messages
type NotifyWindow interface {
	IsDestroyed() bool
	IsMinimized() bool
	Restore()
	IsVisible() bool
	ShowInactive()
	IsFocused() bool
	Focus()
	Send(event string, payload any)
	// Starts or stops the taskbar or dock attention request
	FlashFrame(on bool)
	// Runs f once on the next focus of the window
	OnceFocus(f func())
}

// NotificationOptions are what the native notification shows
type NotificationOptions struct {
	Title   string
	Body    string
	Silent  bool
	Icon    string
	Actions []string
}

// NotificationEvents are the native notification callbacks, action carrying the button index
type NotificationEvents struct {
	Click  func()
	Action func(index int)
	Close  func()
	Failed func()
}

// Notification is one native notification
type Notification interface {
	// Delivers it, false when delivery failed
	Show() bool
	Close()
}

// Notifier creates native notifications
type Notifier interface {
	Supported() bool
	Create(o NotificationOptions, ev NotificationEvents) Notification
	// A close event releases the notification, the Linux daemon closing it for good
	ReleaseOnClose() bool
}

type delivery struct {
	done chan struct{}
	ok   bool
}

type notifications struct {
	mu         sync.Mutex
	duplicate  dedupe.Deduper
	deliveries map[string]*delivery
	flashing   map[NotifyWindow]bool
}

func newNotifications() *notifications {
	return &notifications{duplicate: dedupe.NewEventDeduper(notificationDedupeInterval), deliveries: map[string]*delivery{}, flashing: map[NotifyWindow]bool{}}
}

// Restores a minimized window, shows a hidden one without activation and takes focus only when it lacks it
func FocusWindow(w NotifyWindow) {
	if w == nil || w.IsDestroyed() {
		return
	}
	if w.IsMinimized() {
		w.Restore()
	}
	if !w.IsVisible() {
		w.ShowInactive()
	}
	if !w.IsFocused() {
		w.Focus()
	}
}

// Flashes an unfocused window until it is focused, once per window at a time
func (n *notifications) requestAttention(w NotifyWindow) {
	if w == nil || w.IsDestroyed() || w.IsFocused() {
		return
	}
	n.mu.Lock()
	if n.flashing[w] {
		n.mu.Unlock()
		return
	}
	n.flashing[w] = true
	n.mu.Unlock()
	w.FlashFrame(true)
	w.OnceFocus(func() {
		n.mu.Lock()
		delete(n.flashing, w)
		n.mu.Unlock()
		if !w.IsDestroyed() {
			w.FlashFrame(false)
		}
	})
}

// Action of a native action index, nil outside the action list
func resolveNotificationAction(actions []any, index int) any {
	if index < 0 || index >= len(actions) {
		return nil
	}
	return actions[index]
}

// Activation payload with only the keys the renderer would see defined
func activatePayload(pairs ...any) map[string]any {
	out := map[string]any{}
	for i := 0; i+1 < len(pairs); i += 2 {
		if v, ok := pairs[i+1].(undefinedOr); ok {
			if v.defined {
				out[pairs[i].(string)] = v.value
			}
			continue
		}
		out[pairs[i].(string)] = pairs[i+1]
	}
	return out
}

type undefinedOr struct {
	value   any
	defined bool
}

func optional(record any, key string) undefinedOr {
	v, ok := field(record, key)
	return undefinedOr{value: v, defined: ok}
}

// Shows a native notification for a renderer, deduplicating repeats within a second and routing clicks and actions back
func (s *Service) Notify(source string, payload any) bool {
	n := s.notes
	sourceWindow := s.o.Host.Window(source)
	targetWindow := func() NotifyWindow {
		if sourceWindow != nil && !sourceWindow.IsDestroyed() {
			return sourceWindow
		}
		return s.o.Host.MainWindow()
	}
	if !s.o.Notifier.Supported() {
		return false
	}
	kind := ""
	if v := get(payload, "kind"); v != nil {
		kind = jsString(v)
	}
	scope := ""
	if v := coalesce(payload, "sessionId", "tag"); v != nil {
		scope = jsString(v)
	}
	key := kind + ":" + scope
	if n.duplicate(key, time.Now()) {
		n.mu.Lock()
		d := n.deliveries[key]
		n.mu.Unlock()
		if d == nil {
			return false
		}
		<-d.done
		return d.ok
	}
	actions, _ := get(payload, "actions").([]any)
	icon := ""
	if raw, ok := get(payload, "icon").(string); ok {
		icon = jsTrim(raw)
	}
	texts := make([]string, len(actions))
	for i, action := range actions {
		texts[i] = jsOr(get(action, "text"), "")
	}
	options := NotificationOptions{
		Title:   jsOr(get(payload, "title"), "Icebath"),
		Body:    jsOr(get(payload, "body"), ""),
		Silent:  jsTruthy(get(payload, "silent")),
		Icon:    icon,
		Actions: texts,
	}
	notifyID := func(w NotifyWindow) undefinedOr {
		if w == sourceWindow {
			return optional(payload, "notifyId")
		}
		return undefinedOr{}
	}
	var release func()
	events := NotificationEvents{
		Click: func() {
			release()
			w := targetWindow()
			if w == nil || w.IsDestroyed() {
				return
			}
			FocusWindow(w)
			focusSession := get(payload, "focusSessionId")
			if !jsTruthy(focusSession) {
				focusSession = get(payload, "sessionId")
			}
			if jsTruthy(focusSession) {
				w.Send("focus-session", focusSession)
			}
			if jsTruthy(get(payload, "activate")) || jsTruthy(get(payload, "notifyId")) {
				w.Send("notification-activate", activatePayload("activate", optional(payload, "activate"), "notifyId", notifyID(w), "tag", optional(payload, "tag")))
			}
		},
		Action: func(index int) {
			release()
			w := targetWindow()
			if w == nil || w.IsDestroyed() {
				return
			}
			action := resolveNotificationAction(actions, index)
			id := get(action, "id")
			if !jsTruthy(id) {
				return
			}
			if jsTruthy(get(payload, "sessionId")) && !jsTruthy(get(payload, "notifyId")) && !jsTruthy(get(payload, "activate")) {
				if w != sourceWindow {
					return
				}
				w.Send("notification-action", map[string]any{"sessionId": get(payload, "sessionId"), "actionId": id})
				return
			}
			FocusWindow(w)
			activate := optional(action, "activate")
			if !jsTruthy(activate.value) {
				activate = optional(payload, "activate")
			}
			w.Send("notification-activate", activatePayload("actionId", id, "activate", activate, "notifyId", notifyID(w), "tag", optional(payload, "tag")))
		},
		Failed: func() { release() },
	}
	if s.o.Notifier.ReleaseOnClose() {
		events.Close = func() { release() }
	} else {
		events.Close = func() {}
	}
	notification := s.o.Notifier.Create(options, events)
	release = retainNotification(notification)
	n.requestAttention(targetWindow())
	d := &delivery{done: make(chan struct{})}
	n.mu.Lock()
	n.deliveries[key] = d
	n.mu.Unlock()
	d.ok = notification.Show()
	close(d.done)
	time.AfterFunc(notificationDedupeInterval, func() {
		n.mu.Lock()
		if n.deliveries[key] == d {
			delete(n.deliveries, key)
		}
		n.mu.Unlock()
	})
	return d.ok
}

// Keeps a notification until a release event or closes it after the retention period, returning the release
func retainNotification(notification Notification) func() {
	var once sync.Once
	var timer *time.Timer
	release := func() {
		once.Do(func() {
			if timer != nil {
				timer.Stop()
			}
		})
	}
	timer = time.AfterFunc(notificationRetentionTTL, func() {
		notification.Close()
		release()
	})
	return release
}
