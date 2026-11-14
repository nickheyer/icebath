package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type turnAdapter struct {
	*BasePlatformAdapter
	rec      sync.Mutex
	sent     []string
	sendFn   func(ctx context.Context, content string) (SendResult, error)
	outcomes []ProcessingOutcome
	played   int
}

func (a *turnAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *turnAdapter) Disconnect(context.Context) error            { return nil }
func (a *turnAdapter) GetChatInfo(context.Context, string) (map[string]any, error) {
	return map[string]any{"name": "fixture", "type": "dm"}, nil
}
func (a *turnAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (SendResult, error) {
	if a.sendFn != nil {
		return a.sendFn(ctx, content)
	}
	a.rec.Lock()
	a.sent = append(a.sent, content)
	a.rec.Unlock()
	return SendResult{Success: true, MessageID: "sent-1"}, nil
}
func (a *turnAdapter) OnProcessingComplete(ctx context.Context, event *MessageEvent, outcome ProcessingOutcome) error {
	a.rec.Lock()
	a.outcomes = append(a.outcomes, outcome)
	a.rec.Unlock()
	return nil
}
func (a *turnAdapter) KeepTyping(ctx context.Context, chatID string, interval float64, metadata map[string]any, stop *InterruptEvent) {
	<-ctx.Done()
}
func (a *turnAdapter) PlayTTS(context.Context, string, string, string, string, map[string]any, map[string]any) (SendResult, error) {
	a.rec.Lock()
	a.played++
	a.rec.Unlock()
	return SendResult{Success: true, MessageID: "tts-1"}, nil
}

type deletingTurnAdapter struct {
	*turnAdapter
	deleted chan [2]string
}

func (a *deletingTurnAdapter) DeleteMessage(ctx context.Context, chatID, messageID string) (bool, error) {
	a.deleted <- [2]string{chatID, messageID}
	return true, nil
}

func newTurnAdapter(platform Platform) *turnAdapter {
	a := &turnAdapter{}
	cfg := NewPlatformConfig()
	cfg.Enabled = true
	a.BasePlatformAdapter = NewBasePlatformAdapter(cfg, platform, a)
	return a
}

func turnEvent(text, chatID string) *MessageEvent {
	ev := NewMessageEvent(text)
	ev.Source = &SessionSource{Platform: PlatformTelegram, ChatID: chatID, ChatType: "dm"}
	return ev
}

func (a *turnAdapter) snapshot() ([]string, []ProcessingOutcome, int) {
	a.rec.Lock()
	defer a.rec.Unlock()
	return append([]string(nil), a.sent...), append([]ProcessingOutcome(nil), a.outcomes...), a.played
}

func TestDiagnosticWakeExecutesWithoutFinalOrErrorEcho(t *testing.T) {
	for _, suppressed := range []bool{false, true} {
		dir := t.TempDir()
		t.Setenv("ICEBATH_HOME", dir)
		flag := "false"
		if suppressed {
			flag = "true"
		}
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("display: {suppress_warning_notifications: "+flag+"}"), 0o600); err != nil {
			t.Fatal(err)
		}
		a := newTurnAdapter(PlatformTelegram)
		ev := turnEvent("internal diagnostic", "chat")
		ev.Internal = true
		ev.Metadata = map[string]any{"notification_category": "diagnostic"}
		calls := 0
		a.SetMessageHandler(func(context.Context, *MessageEvent) (any, error) {
			calls++
			return "I repeat the technical diagnostic", nil
		})
		if err := a.ProcessMessageBackground(context.Background(), ev, "session"); err != nil {
			t.Fatal(err)
		}
		sent, _, _ := a.snapshot()
		want := 1
		if suppressed {
			want = 0
		}
		if calls != 1 || len(sent) != want {
			t.Fatalf("suppressed=%v handler calls %d finals %v", suppressed, calls, sent)
		}
		a.SetMessageHandler(func(context.Context, *MessageEvent) (any, error) {
			return nil, errors.New("RuntimeError: provider detail")
		})
		if err := a.ProcessMessageBackground(context.Background(), ev, "session"); err != nil {
			t.Fatal(err)
		}
		sent, outcomes, _ := a.snapshot()
		if (len(sent) > want) == suppressed {
			t.Fatalf("suppressed=%v error echo %v", suppressed, sent)
		}
		if outcomes[len(outcomes)-1] != ProcessingFailure {
			t.Fatalf("last outcome %v", outcomes)
		}
	}
}

func TestProcessMessagePausesTypingBeforeAStalledFinalSend(t *testing.T) {
	a := newTurnAdapter(PlatformTelegram)
	started := make(chan struct{})
	var once sync.Once
	a.sendFn = func(ctx context.Context, content string) (SendResult, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return SendResult{}, ctx.Err()
	}
	a.SetMessageHandler(func(context.Context, *MessageEvent) (any, error) {
		return "a long final answer the user can already read", nil
	})
	ev := turnEvent("hi", "77")
	id, user := "msg-1", "u-1"
	ev.MessageID, ev.Source.UserID = &id, &user
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.ProcessMessageBackground(ctx, ev, "agent:main:telegram:private:77") }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("final send never started")
	}
	a.mu.Lock()
	_, paused := a.typingPaused["77"]
	a.mu.Unlock()
	cancel()
	if !paused {
		t.Fatal("typing not paused while the final send is stalled")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled turn returned %v", err)
	}
}

func swapEphemeralWait(t *testing.T) *[]float64 {
	waits := &[]float64{}
	prev := ephemeralDeleteWait
	ephemeralDeleteWait = func(ctx context.Context, seconds float64) error {
		*waits = append(*waits, seconds)
		return nil
	}
	t.Cleanup(func() { ephemeralDeleteWait = prev })
	return waits
}

func TestProcessMessageUnwrapsEphemeralBeforeSend(t *testing.T) {
	waits := swapEphemeralWait(t)
	d := &deletingTurnAdapter{turnAdapter: &turnAdapter{}, deleted: make(chan [2]string, 1)}
	cfg := NewPlatformConfig()
	cfg.Enabled = true
	d.BasePlatformAdapter = NewBasePlatformAdapter(cfg, PlatformTelegram, d)
	ttl := 5
	d.SetMessageHandler(func(context.Context, *MessageEvent) (any, error) {
		return NewEphemeralReply("⚡ Stopped.", &ttl), nil
	})
	if err := d.ProcessMessageBackground(context.Background(), turnEvent("/stop", "42"), "agent:main:telegram:private:42"); err != nil {
		t.Fatal(err)
	}
	sent, _, _ := d.snapshot()
	if len(sent) != 1 || sent[0] != "⚡ Stopped." {
		t.Fatalf("sent %q", sent)
	}
	select {
	case got := <-d.deleted:
		if got != [2]string{"42", "sent-1"} || len(*waits) != 1 || (*waits)[0] != 5 {
			t.Fatalf("deleted %v after %v", got, *waits)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ephemeral delete never scheduled")
	}
}

func TestProcessMessageIncapablePlatformDoesNotScheduleDelete(t *testing.T) {
	waits := swapEphemeralWait(t)
	a := newTurnAdapter(PlatformTelegram)
	ttl := 5
	a.SetMessageHandler(func(context.Context, *MessageEvent) (any, error) {
		return NewEphemeralReply("⚡ Stopped.", &ttl), nil
	})
	if err := a.ProcessMessageBackground(context.Background(), turnEvent("/stop", "42"), "agent:main:telegram:private:42"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	sent, _, _ := a.snapshot()
	if len(sent) != 1 || sent[0] != "⚡ Stopped." || len(*waits) != 0 {
		t.Fatalf("sent %q waits %v", sent, *waits)
	}
}

func TestBaseAutoTTSSkipsPlaybackWhenToolReportsFailure(t *testing.T) {
	a := newTurnAdapter(PlatformTelegram)
	a.autoTTSDefault = true
	a.SetMessageHandler(func(context.Context, *MessageEvent) (any, error) { return "reply text", nil })
	ev := turnEvent("", "voice-chat")
	ev.MessageType = MessageTypeVoice
	var requested string
	prevMet, prevSynth := autoTTSRequirementsMet, autoTTSSynthesize
	autoTTSRequirementsMet = func(context.Context) bool { return true }
	autoTTSSynthesize = func(ctx context.Context, text, outputPath string) (string, error) {
		requested = outputPath
		if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(outputPath, []byte("partial"), 0o600); err != nil {
			return "", err
		}
		return `{"success": false, "error": "backend exploded"}`, nil
	}
	t.Cleanup(func() { autoTTSRequirementsMet, autoTTSSynthesize = prevMet, prevSynth })
	if err := a.ProcessMessageBackground(context.Background(), ev, "agent:main:telegram:dm:voice-chat"); err != nil {
		t.Fatal(err)
	}
	sent, _, played := a.snapshot()
	if played != 0 || len(sent) == 0 || sent[0] != "reply text" {
		t.Fatalf("played %d sent %q", played, sent)
	}
	if _, err := os.Stat(requested); requested == "" || !os.IsNotExist(err) {
		t.Fatalf("partial tts file %q left behind: %v", requested, err)
	}
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never happened", what)
	}
}

func (a *turnAdapter) sessionState(sk string) (active, task bool, background int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, active = a.activeSessions[sk]
	_, task = a.sessionTasks[sk]
	return active, task, len(a.backgroundTasks)
}

func waitReleased(a *turnAdapter, sk string) {
	for i := 0; i < 200; i++ {
		if active, task, _ := a.sessionState(sk); !active && !task {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCancelBackgroundTasksDrainsLateArrivals(t *testing.T) {
	a := newTurnAdapter(PlatformTelegram)
	ctx := context.Background()
	sk, err := a.EventSessionKey(ctx, turnEvent("M1", "42"))
	if err != nil {
		t.Fatal(err)
	}
	m1Started, m1Cleanup, m2Started, m2Cancelled := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	a.SetMessageHandler(func(hctx context.Context, ev *MessageEvent) (any, error) {
		if ev.Text == "M1" {
			close(m1Started)
			<-hctx.Done()
			close(m1Cleanup)
			time.Sleep(200 * time.Millisecond)
			return nil, hctx.Err()
		}
		close(m2Started)
		<-hctx.Done()
		close(m2Cancelled)
		return nil, hctx.Err()
	})
	if err := a.HandleMessage(ctx, turnEvent("M1", "42")); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, m1Started, "M1 start")
	cancelled := make(chan struct{})
	go func() { a.CancelBackgroundTasks(ctx); close(cancelled) }()
	waitClosed(t, m1Cleanup, "M1 cleanup")
	a.mu.Lock()
	delete(a.activeSessions, sk)
	a.mu.Unlock()
	if err := a.HandleMessage(ctx, turnEvent("M2", "42")); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, m2Started, "M2 start")
	waitClosed(t, cancelled, "cancel_background_tasks return")
	waitClosed(t, m2Cancelled, "late arrival M2 cancellation")
	if _, _, background := a.sessionState(sk); background != 0 {
		t.Fatalf("%d background tasks still tracked", background)
	}
}

func TestNormalPathReleasesSessionGuard(t *testing.T) {
	a := newTurnAdapter(PlatformTelegram)
	ctx := context.Background()
	sk, _ := a.EventSessionKey(ctx, turnEvent("solo", "42"))
	a.SetMessageHandler(func(context.Context, *MessageEvent) (any, error) { return "ok", nil })
	if err := a.HandleMessage(ctx, turnEvent("solo", "42")); err != nil {
		t.Fatal(err)
	}
	waitReleased(a, sk)
	a.CancelBackgroundTasks(ctx)
	if active, task, _ := a.sessionState(sk); active || task {
		t.Fatalf("normal path left active=%v task=%v", active, task)
	}
}

func TestDrainTaskCancellationReleasesSession(t *testing.T) {
	a := newTurnAdapter(PlatformTelegram)
	ctx := context.Background()
	sk, _ := a.EventSessionKey(ctx, turnEvent("M0", "42"))
	drainHit := make(chan struct{})
	a.SetMessageHandler(func(hctx context.Context, ev *MessageEvent) (any, error) {
		if ev.Text == "M0" {
			a.mu.Lock()
			lazyAttr(&a.pendingMessages)[sk] = turnEvent("M1", "42")
			a.mu.Unlock()
			return "ok", nil
		}
		close(drainHit)
		select {
		case <-hctx.Done():
			return nil, hctx.Err()
		case <-time.After(200 * time.Millisecond):
			return nil, nil
		}
	})
	if err := a.HandleMessage(ctx, turnEvent("M0", "42")); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, drainHit, "in-band drain")
	a.mu.Lock()
	drain := a.sessionTasks[sk]
	a.mu.Unlock()
	if drain == nil || drain.finished() {
		t.Fatal("in-band drain did not install a live drain task")
	}
	drain.cancel()
	waitReleased(a, sk)
	a.CancelBackgroundTasks(ctx)
	if active, task, _ := a.sessionState(sk); active || task {
		t.Fatalf("cancelled drain left active=%v task=%v", active, task)
	}
}

func TestInBandDrainRunsEachFollowUpOnAFreshTask(t *testing.T) {
	a := newTurnAdapter(PlatformTelegram)
	ctx := context.Background()
	sk, _ := a.EventSessionKey(ctx, turnEvent("M0", "42"))
	var tasks []*adapterTask
	done := make(chan struct{})
	a.SetMessageHandler(func(hctx context.Context, ev *MessageEvent) (any, error) {
		a.rec.Lock()
		tasks = append(tasks, currentAdapterTask(hctx))
		n := len(tasks)
		a.rec.Unlock()
		if n < 6 {
			a.mu.Lock()
			lazyAttr(&a.pendingMessages)[sk] = turnEvent("next", "42")
			a.mu.Unlock()
		} else {
			close(done)
		}
		return nil, nil
	})
	if err := a.HandleMessage(ctx, turnEvent("M0", "42")); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, done, "chained follow ups")
	waitReleased(a, sk)
	seen := map[*adapterTask]bool{}
	a.rec.Lock()
	defer a.rec.Unlock()
	for _, task := range tasks {
		if task == nil || seen[task] {
			t.Fatal("a follow up ran inside an earlier turn's task")
		}
		seen[task] = true
	}
}

type routeRunner struct {
	profile *string
	err     error
	seen    *string
}

func (r *routeRunner) ProfileNameForSource(ctx context.Context, source *SessionSource, adapterProfile *string) (*string, error) {
	r.seen = adapterProfile
	return r.profile, r.err
}

func TestBuildSourceStampsRoutedProfileAndTransport(t *testing.T) {
	a := newTurnAdapter(PlatformTelegram)
	empty, topic, thread, guild := "", "  ops  ", "7", "g1"
	src := a.BuildSource(context.Background(), "42", SourceFields{UserID: &empty, ChatTopic: &topic, ThreadID: &thread, GuildID: &guild})
	if src.ChatType != "dm" || src.UserID != nil || *src.ChatTopic != "ops" || *src.ThreadID != "7" || *src.ScopeID != "g1" ||
		src.Profile != nil || src.transportAdapterRef != PlatformAdapter(a) || src.ProfileRouteRejected {
		t.Fatalf("unrouted source %+v", src)
	}
	a.SetOwnerProfile("work")
	routed := "sales"
	runner := &routeRunner{profile: &routed}
	a.GatewayRunner = runner
	if src = a.BuildSource(context.Background(), "42", SourceFields{}); *src.Profile != "sales" || *runner.seen != "work" {
		t.Fatalf("routed profile %v seen %v", src.Profile, runner.seen)
	}
	runner.profile = nil
	if src = a.BuildSource(context.Background(), "42", SourceFields{}); *src.Profile != "work" {
		t.Fatalf("owner profile fallback %v", src.Profile)
	}
	runner.err = &ProfileRouteRejected{Args: []any{"refused", "blocked"}}
	src = a.BuildSource(context.Background(), "42", SourceFields{})
	if !src.ProfileRouteRejected || *src.rejectedRouteProfile != "blocked" || *src.Profile != "work" {
		t.Fatalf("rejected route %+v", src)
	}
	if got := a.FormatMessage("**x**"); got != "**x**" {
		t.Fatal(got)
	}
	if toolsets, ok := a.ToolsetsForSource(src); ok || toolsets != nil {
		t.Fatal("base toolset override present")
	}
}

func TestRequeueBackoffDelayBacksOffOnlyTheBouncedEvent(t *testing.T) {
	a := newTurnAdapter(PlatformTelegram)
	id := "m1"
	ev := turnEvent("x", "42")
	ev.MessageID = &id
	if d := a.requeueBackoffDelay("sk", ev, ev); d != 0 {
		t.Fatalf("first bounce delayed %v", d)
	}
	if d := a.requeueBackoffDelay("sk", ev, ev); d != 0.25 {
		t.Fatalf("second bounce %v", d)
	}
	a.requeueBackoffDelay("sk", ev, ev)
	if d := a.requeueBackoffDelay("sk", ev, ev); d != 1.0 {
		t.Fatalf("capped bounce %v", d)
	}
	other := turnEvent("y", "42")
	if d := a.requeueBackoffDelay("sk", other, ev); d != 0 || a.requeueCounts["sk"] != 0 {
		t.Fatalf("fresh follow up delayed %v", d)
	}
}
