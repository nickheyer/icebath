package gateway

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func installCommandBusyPolicyDouble() {
	interrupt := map[string]bool{"new": true, "reset": true, "stop": true}
	known := map[string]bool{"new": true, "reset": true, "stop": true, "status": true, "approve": true, "deny": true,
		"help": true, "queue": true, "steer": true, "background": true, "model": true, "usage": true}
	CommandBusyPolicy = func(name string) (bool, bool) { return name != "" && known[name], interrupt[name] }
}

type sessionAdapter struct {
	*BasePlatformAdapter
	mu         sync.Mutex
	dispatched []string
	hold       chan struct{}
}

func (a *sessionAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *sessionAdapter) Disconnect(context.Context) error            { return nil }
func (a *sessionAdapter) GetChatInfo(context.Context, string) (map[string]any, error) {
	return nil, nil
}
func (a *sessionAdapter) Send(context.Context, string, string, string, map[string]any) (SendResult, error) {
	return SendResult{Success: true, MessageID: "x"}, nil
}
func (a *sessionAdapter) ProcessMessageBackground(ctx context.Context, event *MessageEvent, _ string) error {
	a.mu.Lock()
	a.dispatched = append(a.dispatched, event.Text)
	a.mu.Unlock()
	select {
	case <-a.hold:
	case <-ctx.Done():
	}
	return nil
}
func (a *sessionAdapter) started() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.dispatched...)
}

func newSessionAdapter(t *testing.T) (*sessionAdapter, *[]*MessageEvent) {
	cfg := NewPlatformConfig()
	cfg.Enabled = true
	a := &sessionAdapter{hold: make(chan struct{})}
	a.BasePlatformAdapter = NewBasePlatformAdapter(cfg, PlatformTelegram, a)
	handled := &[]*MessageEvent{}
	var hmu sync.Mutex
	a.SetMessageHandler(func(_ context.Context, event *MessageEvent) (any, error) {
		hmu.Lock()
		*handled = append(*handled, event)
		hmu.Unlock()
		return nil, nil
	})
	a.SetBusyTextPolicy("queue", 0.1, 1.0)
	t.Cleanup(func() { close(a.hold) })
	return a, handled
}

func sessionEvent(text, chatType string, threadID *string) *MessageEvent {
	uid := "u1"
	ev := NewMessageEvent(text)
	ev.MessageType = MessageTypeText
	ev.Source = &SessionSource{Platform: PlatformTelegram, ChatID: "12345", ChatType: chatType, UserID: &uid, ThreadID: threadID}
	id := "msg-" + string([]rune(text)[:min(8, len([]rune(text)))])
	ev.MessageID = &id
	return ev
}

func sessionKeyOf(t *testing.T, a *sessionAdapter, ev *MessageEvent) string {
	key, err := BuildSessionKey(context.Background(), ev.Source, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func (b *BasePlatformAdapter) seedActiveSession(key string) *InterruptEvent {
	ev := NewInterruptEvent()
	b.mu.Lock()
	lazyAttr(&b.activeSessions)[key] = ev
	b.mu.Unlock()
	return ev
}

func (b *BasePlatformAdapter) pendingText(key string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ev, ok := b.pendingMessages[key]
	if !ok {
		return "", false
	}
	return ev.Text, true
}

func (b *BasePlatformAdapter) debounceState(key string) *TextDebounceState {
	b.mu.Lock()
	defer b.mu.Unlock()
	if state := b.textDebounce[key]; state != nil {
		copied := *state
		return &copied
	}
	return nil
}

func waitUntil(timeout time.Duration, predicate func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return predicate()
}

func TestNonDMMessageDoesNotWaitForTopicRecoveryExecutor(t *testing.T) {
	a, _ := newSessionAdapter(t)
	called := 0
	a.SetTopicRecoveryFn(func(*SessionSource) (*string, error) { called++; return nil, nil })
	if err := a.HandleMessage(context.Background(), sessionEvent("/status", "group", nil)); err != nil || called != 0 {
		t.Fatalf("group message ran topic recovery %d times err %v", called, err)
	}
}

func TestDMTopicRecoveryStaysOffloaded(t *testing.T) {
	a, _ := newSessionAdapter(t)
	var seen []*SessionSource
	a.SetTopicRecoveryFn(func(s *SessionSource) (*string, error) {
		seen = append(seen, s)
		topic := "topic-222"
		return &topic, nil
	})
	one := "1"
	ev := sessionEvent("hello", "dm", &one)
	original := ev.Source
	if err := a.HandleMessage(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != original || ev.Source.ThreadID == nil || *ev.Source.ThreadID != "topic-222" {
		t.Fatalf("recovery calls %d thread %v", len(seen), ev.Source.ThreadID)
	}
}

func TestRapidTextFollowupsAccumulateInsteadOfReplacing(t *testing.T) {
	ctx := context.Background()
	a, _ := newSessionAdapter(t)
	a.SetBusyTextPolicy("", 0.1, 1.0)
	key := sessionKeyOf(t, a, sessionEvent("part one", "dm", nil))
	guard := a.seedActiveSession(key)
	_ = a.HandleMessage(ctx, sessionEvent("part two", "dm", nil))
	_ = a.HandleMessage(ctx, sessionEvent("part three", "dm", nil))
	if text, _ := a.pendingText(key); text != "part two\npart three" || guard.IsSet() {
		t.Fatalf("pending %q interrupted %v", text, guard.IsSet())
	}
}

func TestDebounceResetsTimerOnNewArrival(t *testing.T) {
	ctx := context.Background()
	a, _ := newSessionAdapter(t)
	first := sessionEvent("one", "dm", nil)
	key := sessionKeyOf(t, a, first)
	a.seedActiveSession(key)
	_ = a.HandleMessage(ctx, first)
	task1 := a.debounceState(key).Task
	if task1 == nil || task1.finished() {
		t.Fatal("first arrival arms a live timer")
	}
	_ = a.HandleMessage(ctx, sessionEvent("two", "dm", nil))
	task2 := a.debounceState(key).Task
	if task2 == nil || task2 == task1 {
		t.Fatal("second arrival rearms the timer")
	}
	if !waitUntil(time.Second, task1.finished) || a.debounceState(key).Task != task2 {
		t.Fatal("first timer is cancelled and the second owns the slot")
	}
	_ = a.HandleMessage(ctx, sessionEvent("three", "dm", nil))
	if task3 := a.debounceState(key).Task; task3 == nil || task3 == task2 {
		t.Fatal("third arrival rearms the timer")
	}
	time.Sleep(200 * time.Millisecond)
	if !waitUntil(time.Second, func() bool { return a.debounceState(key) == nil }) {
		t.Fatal("burst flushed")
	}
	if text, _ := a.pendingText(key); text != "one\ntwo\nthree" {
		t.Fatalf("pending %q", text)
	}
}

func TestControlAndClarifyMessagesBypassTextDebounce(t *testing.T) {
	ctx := context.Background()
	a, handled := newSessionAdapter(t)
	_ = a.HandleMessage(ctx, sessionEvent("/status", "dm", nil))
	if !waitUntil(time.Second, func() bool { return len(a.started()) == 1 }) || a.started()[0] != "/status" {
		t.Fatalf("started %v", a.started())
	}
	answer := sessionEvent("clarify answer", "dm", nil)
	key := sessionKeyOf(t, a, answer)
	if a.debounceState(key) != nil {
		t.Fatal("a command never enters the debounce buffer")
	}
	a.seedActiveSession(key)
	old := pendingClarifyForSession
	pendingClarifyForSession = func(string) bool { return true }
	t.Cleanup(func() { pendingClarifyForSession = old })
	_ = a.HandleMessage(ctx, answer)
	if len(*handled) != 1 || (*handled)[0] != answer || a.debounceState(key) != nil {
		t.Fatalf("handled %d", len(*handled))
	}
	if _, ok := a.pendingText(key); ok {
		t.Fatal("clarify answer is not queued as a follow up")
	}
}

func TestFollowupStartedAsFreshTurnWhenSessionReleasedMidRoute(t *testing.T) {
	for _, mode := range []string{"queue", ""} {
		t.Run("mode="+mode, func(t *testing.T) {
			a, _ := newSessionAdapter(t)
			a.SetBusyTextPolicy(mode, 0.1, 1.0)
			followup := sessionEvent("did you get that?", "dm", nil)
			key := sessionKeyOf(t, a, followup)
			a.seedActiveSession(key)
			gate, entered := make(chan struct{}), make(chan struct{})
			a.SetBusySessionHandler(func(context.Context, *MessageEvent, string) (bool, error) {
				close(entered)
				<-gate
				a.mu.Lock()
				delete(a.activeSessions, key)
				delete(a.sessionTasks, key)
				a.mu.Unlock()
				return false, nil
			})
			done := make(chan error, 1)
			go func() { done <- a.HandleMessage(context.Background(), followup) }()
			<-entered
			close(gate)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if !waitUntil(1500*time.Millisecond, func() bool { return len(a.started()) > 0 }) {
				t.Fatal("follow up stranded")
			}
			if a.started()[0] != "did you get that?" || !followup.GatewayAccepted() || a.debounceState(key) != nil {
				t.Fatalf("started %v accepted %v", a.started(), followup.GatewayAccepted())
			}
			if _, ok := a.pendingText(key); ok {
				t.Fatal("nothing left holding the event")
			}
		})
	}
}

func TestFollowupStillQueuesWhenTheSessionStaysActive(t *testing.T) {
	a, _ := newSessionAdapter(t)
	a.SetBusyTextPolicy("", 0.1, 1.0)
	followup := sessionEvent("still busy here", "dm", nil)
	key := sessionKeyOf(t, a, followup)
	a.seedActiveSession(key)
	a.SetBusySessionHandler(func(context.Context, *MessageEvent, string) (bool, error) { return false, nil })
	if err := a.HandleMessage(context.Background(), followup); err != nil {
		t.Fatal(err)
	}
	if len(a.started()) != 0 || !a.HasActiveSession(key) {
		t.Fatal("a live guard keeps the queue behind behaviour")
	}
	if text, ok := a.pendingText(key); !ok || text != "still busy here" {
		t.Fatalf("pending %q", text)
	}
}

func TestHumanDelayOffAndInstalledRangeNeverTouchProcessEnv(t *testing.T) {
	t.Setenv("ICEBATH_HUMAN_DELAY_MODE", "custom")
	t.Setenv("ICEBATH_HUMAN_DELAY_MIN_MS", "10")
	t.Setenv("ICEBATH_HUMAN_DELAY_MAX_MS", "20")
	var calls [][2]float64
	old := humanDelayUniform
	humanDelayUniform = func(lo, hi float64) float64 { calls = append(calls, [2]float64{lo, hi}); return 1.5 }
	t.Cleanup(func() { humanDelayUniform = old })
	a, _ := newSessionAdapter(t)
	if a.getHumanDelay() != 0 || len(calls) != 0 {
		t.Fatal("no installed range means no delay and no draw")
	}
	a.SetHumanDelayRangeMs(&[2]int{1000, 2000})
	if a.getHumanDelay() != 1.5 || len(calls) != 1 || calls[0] != [2]float64{1.0, 2.0} {
		t.Fatalf("calls %v", calls)
	}
}

func TestOutputPathIsMP3ForNonOpusPlatforms(t *testing.T) {
	for _, p := range []Platform{PlatformDiscord, PlatformSlack, Platform("irc"), Platform("")} {
		path, err := BuildAutoTTSOutputPath(p)
		if err != nil || filepath.Ext(path) != ".mp3" {
			t.Fatalf("%q -> %q %v", p, path, err)
		}
	}
}
