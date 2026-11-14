package gateway

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type burstAdapter struct {
	*BasePlatformAdapter
	rec  sync.Mutex
	sent []string
}

func (a *burstAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *burstAdapter) Disconnect(context.Context) error            { return nil }
func (a *burstAdapter) GetChatInfo(context.Context, string) (map[string]any, error) {
	return map[string]any{"name": "fixture", "type": "dm"}, nil
}
func (a *burstAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (SendResult, error) {
	a.rec.Lock()
	a.sent = append(a.sent, content)
	a.rec.Unlock()
	return SendResult{Success: true, MessageID: "sent"}, nil
}
func (a *burstAdapter) KeepTyping(ctx context.Context, chatID string, interval float64, metadata map[string]any, stop *InterruptEvent) {
	<-ctx.Done()
}

// One admission row of the runtime double with its request id, text, media count and status
type burstRow struct {
	route, requestID, text string
	media                  int
	status                 string
	reply                  chan any
}

type burstTurn struct {
	text  string
	media int
}

// FIFO route turn of the runtime double, the holder plus its queued waiters
type burstRouteTurn struct {
	count   int
	waiters []chan struct{}
}

// Session authority and runner double settling the busy caller futures as the hermes messaging admission does
type burstRuntime struct {
	adapter  PlatformAdapter
	mu       sync.Mutex
	rows     []*burstRow
	executed []burstTurn
	turns    map[string]*burstRouteTurn
	epochs   map[string]int
	draining bool
	schedule bool
	capture  func(paths []string)
	running  chan struct{}
	runOnce  sync.Once
	finish   chan struct{}
	wake     chan struct{}
	stop     chan struct{}

	authority *SessionAuthority
}

// Bound launch authority marker, the double itself admitting turns the way the hermes authority does
func (r *burstRuntime) SessionAuthority() *SessionAuthority { return r.authority }

// Busy command policy of the hermes command registry for the commands these cases send
func installBurstCommandPolicy(t *testing.T) {
	saved := CommandBusyPolicy
	t.Cleanup(func() { CommandBusyPolicy = saved })
	CommandBusyPolicy = func(name string) (bool, bool) {
		switch name {
		case "new", "reset", "stop":
			return true, true
		case "restart", "status":
			return true, false
		}
		return false, false
	}
}

// Real base adapter wired to the runtime double, serial when the platform reads one input at a time
func newBurstRuntime(t *testing.T, a *burstAdapter, self PlatformAdapter, platform Platform, serial bool, busyMode string, extra map[string]any) *burstRuntime {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	installBurstCommandPolicy(t)
	cfg := NewPlatformConfig()
	cfg.Enabled = true
	if extra != nil {
		cfg.Extra = extra
	}
	a.BasePlatformAdapter = NewBasePlatformAdapter(cfg, platform, self)
	a.Traits.SerialReceiveLoop = serial
	r := &burstRuntime{adapter: self, schedule: true, authority: &SessionAuthority{}, turns: map[string]*burstRouteTurn{}, epochs: map[string]int{},
		running: make(chan struct{}), finish: make(chan struct{}), wake: make(chan struct{}, 1), stop: make(chan struct{})}
	a.SetBoundMessageHandler(r, r.handle)
	if busyMode != "" {
		a.SetBusyTextPolicy(busyMode, DefaultBusyTextDebounceSeconds, DefaultBusyTextHardCapSeconds)
	}
	go r.drain()
	t.Cleanup(func() {
		close(r.stop)
		a.CancelBackgroundTasks(context.Background())
	})
	return r
}

// Runner handler double, slash commands answered inline and every other input admitted to the route FIFO
func (r *burstRuntime) handle(ctx context.Context, event *MessageEvent) (any, error) {
	key, err := r.adapter.Base().EventSessionKey(ctx, event)
	if err != nil {
		return nil, err
	}
	cmd, _ := event.GetCommand()
	switch cmd {
	case "":
		return r.admit(ctx, key, event)
	case "restart":
		r.mu.Lock()
		r.draining = true
		r.mu.Unlock()
		return "Restarting gateway", nil
	case "new", "reset":
		r.mu.Lock()
		r.epochs[key]++
		for _, row := range r.rows {
			if row.route == key && row.status == "queued" {
				row.status = "terminal"
				row.reply <- nil
			}
		}
		r.mu.Unlock()
		return "New session started", nil
	}
	return nil, nil
}

// Admits one input, released once queued behind an earlier one and committed once its row is written
func (r *burstRuntime) admit(ctx context.Context, key string, event *MessageEvent) (any, error) {
	accepted, committed := busyAdmissionOf(ctx)
	ctx = context.WithValue(ctx, busyAdmissionKey{}, (*busyAdmission)(nil))
	r.mu.Lock()
	arrived := r.epochs[key]
	r.mu.Unlock()
	if err := r.acquire(ctx, key, func() {
		if accepted != nil {
			accepted.settle(nil, false)
		}
	}); err != nil {
		return nil, err
	}
	row, err := r.write(key, arrived, event)
	r.release(key)
	if err != nil || row == nil {
		return nil, err
	}
	if committed != nil {
		committed.settle(nil, false)
	}
	if accepted != nil {
		accepted.settle(nil, false)
	}
	r.kick()
	select {
	case reply := <-row.reply:
		return reply, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Captures the media and commits the row unless a later new fenced the route or admission closed
func (r *burstRuntime) write(key string, arrived int, event *MessageEvent) (*burstRow, error) {
	r.mu.Lock()
	draining, capture := r.draining, r.capture
	r.mu.Unlock()
	if draining {
		return nil, errors.New("RuntimeStoreError: runtime_draining")
	}
	if len(event.MediaURLs) > 0 && capture != nil {
		capture(event.MediaURLs)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.epochs[key] != arrived {
		return nil, nil
	}
	if r.draining {
		return nil, errors.New("RuntimeStoreError: runtime_draining")
	}
	requestID := ""
	if event.MessageID != nil {
		requestID = *event.MessageID
	}
	row := &burstRow{route: key, requestID: requestID, text: event.Text, media: len(event.MediaURLs), status: "queued",
		reply: make(chan any, 1)}
	r.rows = append(r.rows, row)
	event.setGatewayAccepted(true)
	return row, nil
}

// Takes the route turn in arrival order, calling queued when an earlier input holds it
func (r *burstRuntime) acquire(ctx context.Context, key string, queued func()) error {
	r.mu.Lock()
	turn := r.turns[key]
	if turn == nil {
		turn = &burstRouteTurn{}
		r.turns[key] = turn
	}
	turn.count++
	if turn.count == 1 {
		r.mu.Unlock()
		return nil
	}
	wait := make(chan struct{})
	turn.waiters = append(turn.waiters, wait)
	r.mu.Unlock()
	queued()
	select {
	case <-wait:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Hands the route turn to the next waiter
func (r *burstRuntime) release(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	turn := r.turns[key]
	turn.count--
	if len(turn.waiters) > 0 {
		close(turn.waiters[0])
		turn.waiters = turn.waiters[1:]
	}
	if turn.count == 0 {
		delete(r.turns, key)
	}
}

// Holder plus waiters of the route turn
func (r *burstRuntime) routeCount(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if turn := r.turns[key]; turn != nil {
		return turn.count
	}
	return 0
}

func (r *burstRuntime) kick() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Runs queued rows in order while admission is open, the A turn held until finish
func (r *burstRuntime) drain() {
	for {
		r.mu.Lock()
		var next *burstRow
		if r.schedule && !r.draining {
			for _, row := range r.rows {
				if row.status == "queued" {
					next = row
					break
				}
			}
		}
		if next == nil {
			r.mu.Unlock()
			select {
			case <-r.wake:
				continue
			case <-r.stop:
				return
			}
		}
		next.status = "running"
		r.executed = append(r.executed, burstTurn{next.text, next.media})
		r.mu.Unlock()
		if next.text == "A" {
			r.runOnce.Do(func() { close(r.running) })
			select {
			case <-r.finish:
			case <-r.stop:
				return
			}
		}
		r.mu.Lock()
		next.status = "terminal"
		r.mu.Unlock()
		next.reply <- "ok"
	}
}

func (r *burstRuntime) requestIDsLocked(sorted bool) []string {
	ids := []string{}
	for _, row := range r.rows {
		ids = append(ids, row.requestID)
	}
	if sorted {
		sort.Strings(ids)
	}
	return ids
}

func (r *burstRuntime) requestIDs(sorted bool) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requestIDsLocked(sorted)
}

func (r *burstRuntime) rowTurns() []burstTurn {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []burstTurn{}
	for _, row := range r.rows {
		out = append(out, burstTurn{row.text, row.media})
	}
	return out
}

func (r *burstRuntime) executedTurns() []burstTurn {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]burstTurn(nil), r.executed...)
}

func (r *burstRuntime) allTerminal() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, row := range r.rows {
		if row.status != "terminal" {
			return false
		}
	}
	return true
}

func (r *burstRuntime) setDraining(v bool) {
	r.mu.Lock()
	r.draining = v
	r.mu.Unlock()
	r.kick()
}

// Fails the test unless the predicate holds within the timeout
func burstWait(t *testing.T, timeout time.Duration, what string, predicate func() bool) {
	t.Helper()
	if !waitUntil(timeout, predicate) {
		t.Fatalf("timed out waiting for %s", what)
	}
}

// Starts turn A on the idle chat and holds it running, returning the chat's session key
func burstBusyOwner(t *testing.T, a *burstAdapter, r *burstRuntime, source *SessionSource) string {
	t.Helper()
	a.HandleMessage(context.Background(), burstText(source, "A", "A"))
	select {
	case <-r.running:
	case <-time.After(10 * time.Second):
		t.Fatal("turn A never started")
	}
	key, err := a.EventSessionKey(context.Background(), burstText(source, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// Lets turn A finish and waits until every follow up settled, returning the turns run after A
func burstSettle(t *testing.T, a *burstAdapter, r *burstRuntime, key string, turns int) []burstTurn {
	t.Helper()
	close(r.finish)
	burstWait(t, 15*time.Second, "the follow-ups to settle", func() bool {
		return !holdsBusyBurst(a, key) && len(r.executedTurns()) >= turns && r.allTerminal()
	})
	return r.executedTurns()[1:]
}

func heldBurst(a PlatformAdapter, key string) bool {
	bi := &a.Base().busyIngress
	bi.mu.Lock()
	defer bi.mu.Unlock()
	_, ok := bi.bursts[key]
	return ok
}

func burstText(source *SessionSource, text, id string) *MessageEvent {
	ev := NewMessageEvent(text)
	ev.Source = source
	if id != "" {
		ev.MessageID = &id
	}
	return ev
}

func burstPhoto(t *testing.T, source *SessionSource, n int) *MessageEvent {
	path := filepath.Join(t.TempDir(), fmt.Sprintf("p%d.png", n))
	if err := os.WriteFile(path, append([]byte("\x89PNG\r\n\x1a\n"+strings.Repeat("\x00", 24)), byte(n)), 0o600); err != nil {
		t.Fatal(err)
	}
	ev := burstText(source, "", fmt.Sprintf("P%d", n))
	ev.MessageType = MessageTypePhoto
	ev.MediaURLs, ev.MediaTypes = []string{path}, []string{"image/png"}
	return ev
}

func burstSource(platform Platform, chatID, chatType, userID string) *SessionSource {
	return &SessionSource{Platform: platform, ChatID: chatID, ChatType: chatType, UserID: &userID}
}

// Signal adapter double, its SSE listener reads the next envelope only after handle message returned
func burstSignal(t *testing.T, busyMode string) (*burstAdapter, *burstRuntime, *SessionSource) {
	a := &burstAdapter{}
	r := newBurstRuntime(t, a, a, PlatformSignal, true, busyMode, nil)
	return a, r, burstSource(PlatformSignal, "+15550001", "dm", "+15550001")
}

// Media capture held until release, a slow disk under the commit
func burstHoldCapture(r *burstRuntime) (entered, release chan struct{}) {
	entered, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	r.mu.Lock()
	r.capture = func([]string) {
		once.Do(func() { close(entered) })
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
	}
	r.mu.Unlock()
	return entered, release
}

func waitClosedWithin(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

func TestBusyBurstIsAdmittedOnce(t *testing.T) {
	type followUp struct {
		sender, text string
		photo        bool
	}
	cases := []struct {
		name      string
		followUps []followUp
		expected  []burstTurn
	}{
		{"split-text", []followUp{{"42", "first half", false}, {"42", "second half", false}},
			[]burstTurn{{"first half\nsecond half", 0}}},
		{"photo-burst", []followUp{{"42", "look", true}, {"42", "", true}}, []burstTurn{{"look", 2}}},
		{"two-senders", []followUp{{"42", "mine", false}, {"43", "theirs", false}},
			[]burstTurn{{"mine", 0}, {"theirs", 0}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &burstAdapter{}
			r := newBurstRuntime(t, a, a, PlatformDiscord, false, "queue", map[string]any{"group_sessions_per_user": false})
			dir := t.TempDir()
			event := func(sender, text string, photo bool, n int) *MessageEvent {
				ev := burstText(burstSource(PlatformDiscord, "room", "group", sender), text, fmt.Sprintf("m%d", n))
				if photo {
					path := filepath.Join(dir, fmt.Sprintf("p%d.png", n))
					if err := os.WriteFile(path, append([]byte("\x89PNG\r\n\x1a\n"+strings.Repeat("\x00", 24)), byte(n)), 0o600); err != nil {
						t.Fatal(err)
					}
					ev.MessageType = MessageTypePhoto
					ev.MediaURLs, ev.MediaTypes = []string{path}, []string{"image/png"}
				}
				return ev
			}
			a.HandleMessage(context.Background(), event("42", "A", false, 0))
			if !waitClosedWithin(r.running, 10*time.Second) {
				t.Fatal("turn A never started")
			}
			var sent []*MessageEvent
			for n, spec := range tc.followUps {
				sent = append(sent, event(spec.sender, spec.text, spec.photo, n+1))
			}
			key, err := a.EventSessionKey(context.Background(), sent[0])
			if err != nil {
				t.Fatal(err)
			}
			var callers sync.WaitGroup
			for _, e := range sent {
				callers.Add(1)
				go func() {
					defer callers.Done()
					a.HandleMessage(context.Background(), e)
				}()
				burstWait(t, 5*time.Second, "the input to be held or accepted", func() bool {
					return holdsBusyBurst(a, key) || e.GatewayAccepted()
				})
			}
			done := make(chan struct{})
			go func() { callers.Wait(); close(done) }()
			if !waitClosedWithin(done, 10*time.Second) {
				t.Fatal("callers did not return")
			}
			for _, e := range sent {
				if !e.GatewayAccepted() {
					t.Fatalf("an input was not accepted: %v", e.Text)
				}
			}
			if got := r.rowTurns()[1:]; !reflect.DeepEqual(got, tc.expected) {
				t.Fatalf("admitted %v, want %v", got, tc.expected)
			}
			close(r.finish)
			burstWait(t, 10*time.Second, "every merged turn executed", func() bool {
				return len(r.executedTurns()) >= 1+len(tc.expected)
			})
		})
	}
}

func TestSignalBusyPhotosMergeWithoutHoldingTheReader(t *testing.T) {
	a, r, source := burstSignal(t, "")
	key := burstBusyOwner(t, a, r, source)
	for _, n := range []int{1, 2} {
		a.HandleMessage(context.Background(), burstPhoto(t, source, n))
		if !heldBurst(a, key) {
			t.Fatal("the reader waited out the hold window")
		}
	}
	if got := burstSettle(t, a, r, key, 2); !reflect.DeepEqual(got, []burstTurn{{"", 2}}) {
		t.Fatalf("got %v", got)
	}
}

// IRC adapter double whose read loop awaits each PRIVMSG before reading the next line
type burstIRC struct{ burstAdapter }

func (a *burstIRC) receiveLoop(ctx context.Context, reader *bufio.Reader) error {
	for {
		line, err := reader.ReadString('\n')
		if line = strings.TrimRight(line, "\r\n"); line != "" {
			prefix, rest, _ := strings.Cut(line, " ")
			nick, _, _ := strings.Cut(strings.TrimPrefix(prefix, ":"), "!")
			if command, params, _ := strings.Cut(rest, " "); command == "PRIVMSG" {
				_, text, _ := strings.Cut(params, " :")
				source := burstSource(Platform("irc"), nick, "dm", nick)
				source.ChatName, source.UserName = &nick, &nick
				if herr := a.HandleMessage(ctx, burstText(source, text, "")); herr != nil {
					return herr
				}
			}
		}
		if err != nil {
			return nil
		}
	}
}

func TestIrcSplitTextMergesThroughTheReceiveLoop(t *testing.T) {
	a := &burstIRC{}
	r := newBurstRuntime(t, &a.burstAdapter, a, Platform("irc"), true, "queue", nil)
	nick := "sender"
	source := burstSource(Platform("irc"), nick, "dm", nick)
	source.ChatName, source.UserName = &nick, &nick
	key := burstBusyOwner(t, &a.burstAdapter, r, source)
	feed := bufio.NewReader(bytes.NewBufferString(":sender!u@h PRIVMSG testbot :first half\r\n" +
		":sender!u@h PRIVMSG testbot :second half\r\n"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.receiveLoop(ctx, feed); err != nil {
		t.Fatal(err)
	}
	if got := burstSettle(t, &a.burstAdapter, r, key, 2); !reflect.DeepEqual(got, []burstTurn{{"first half\nsecond half", 0}}) {
		t.Fatalf("got %v", got)
	}
}

func TestRestartAfterAHeldTextWaitsForItsCommit(t *testing.T) {
	a, r, source := burstSignal(t, "queue")
	key := burstBusyOwner(t, a, r, source)
	a.HandleMessage(context.Background(), burstText(source, "B", "B"))
	a.HandleMessage(context.Background(), burstText(source, "/restart", "R"))
	burstWait(t, 5*time.Second, "the burst to land", func() bool { return !holdsBusyBurst(a, key) })
	if got := r.requestIDs(false); !reflect.DeepEqual(got, []string{"A", "B"}) {
		t.Fatalf("rows %v", got)
	}
	r.setDraining(false)
	if got := burstSettle(t, a, r, key, 2); !reflect.DeepEqual(got, []burstTurn{{"B", 0}}) {
		t.Fatalf("got %v", got)
	}
}

func TestNewWaitsForABurstStillCommitting(t *testing.T) {
	a, r, source := burstSignal(t, "")
	entered, release := burstHoldCapture(r)
	key := burstBusyOwner(t, a, r, source)
	a.HandleMessage(context.Background(), burstPhoto(t, source, 1))
	if !waitClosedWithin(entered, 5*time.Second) {
		t.Fatal("the burst never reached its capture")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.HandleMessage(context.Background(), burstText(source, "/new", "N"))
	}()
	waitClosedWithin(done, 2*time.Second)
	close(release)
	if !waitClosedWithin(done, 10*time.Second) {
		t.Fatal("/new never returned")
	}
	if got := burstSettle(t, a, r, key, 1); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestNextBurstWaitsWhileTheChatsLastOneCommits(t *testing.T) {
	a, r, source := burstSignal(t, "")
	entered, release := burstHoldCapture(r)
	key := burstBusyOwner(t, a, r, source)
	a.HandleMessage(context.Background(), burstPhoto(t, source, 1))
	if !waitClosedWithin(entered, 5*time.Second) {
		t.Fatal("the burst never reached its capture")
	}
	second := make(chan struct{})
	photo := burstPhoto(t, source, 2)
	go func() {
		defer close(second)
		a.HandleMessage(context.Background(), photo)
	}()
	returned := waitClosedWithin(second, 2*time.Second)
	close(release)
	if returned {
		t.Fatal("the reader went on while the chat still had a burst committing")
	}
	if !waitClosedWithin(second, 10*time.Second) {
		t.Fatal("the second photo never returned")
	}
	if got := burstSettle(t, a, r, key, 3); !reflect.DeepEqual(got, []burstTurn{{"", 1}, {"", 1}}) {
		t.Fatalf("got %v", got)
	}
}

func TestRestartWaitsForAHeldTextQueuedBehindASlowCapture(t *testing.T) {
	a, r, source := burstSignal(t, "queue")
	entered, release := burstHoldCapture(r)
	clip := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(clip, bytes.Repeat([]byte("video-bytes"), 1000), 0o600); err != nil {
		t.Fatal(err)
	}
	video := burstText(source, "", "V")
	video.MessageType = MessageTypeVideo
	video.MediaURLs, video.MediaTypes = []string{clip}, []string{"video/mp4"}
	key, err := a.EventSessionKey(context.Background(), video)
	if err != nil {
		t.Fatal(err)
	}
	restart := make(chan struct{})
	func() {
		defer close(release)
		a.HandleMessage(context.Background(), video)
		if !waitClosedWithin(entered, 5*time.Second) {
			t.Fatal("V never reached its capture")
		}
		a.HandleMessage(context.Background(), burstText(source, "B", "B"))
		go func() {
			defer close(restart)
			a.HandleMessage(context.Background(), burstText(source, "/restart", "R"))
		}()
		burstWait(t, 5*time.Second, "B queued behind the capturing V", func() bool { return r.routeCount(key) >= 2 })
		if waitClosedWithin(restart, 2*time.Second) {
			t.Fatal("the restart passed before B was committed")
		}
	}()
	if !waitClosedWithin(restart, 15*time.Second) {
		t.Fatal("the restart never returned")
	}
	if got := r.requestIDs(true); !reflect.DeepEqual(got, []string{"B", "V"}) {
		t.Fatalf("rows %v", got)
	}
	r.setDraining(false)
	burstWait(t, 15*time.Second, "both turns to run", r.allTerminal)
}

func TestRestartWaitsForADirectBusyInputQueuedBehindASlowCapture(t *testing.T) {
	a, r, source := burstSignal(t, "")
	entered, release := burstHoldCapture(r)
	r.mu.Lock()
	r.schedule = false
	r.mu.Unlock()
	var passedMu sync.Mutex
	var passed [][]string
	realGate := burstGate
	t.Cleanup(func() { burstGate = realGate })
	burstGate = func(ctx context.Context, adapter PlatformAdapter, event *MessageEvent, sessionKey string) (bool, error) {
		behind, err := realGate(ctx, adapter, event, sessionKey)
		if cmd, _ := event.GetCommand(); cmd == "restart" {
			passedMu.Lock()
			passed = append(passed, r.requestIDs(true))
			passedMu.Unlock()
		}
		return behind, err
	}
	clip := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(clip, bytes.Repeat([]byte("video bytes"), 100), 0o600); err != nil {
		t.Fatal(err)
	}
	first := burstText(source, "A", "A")
	first.MessageType = MessageTypeVideo
	first.MediaURLs, first.MediaTypes = []string{clip}, []string{"video/mp4"}
	key, err := a.EventSessionKey(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	restart := make(chan struct{})
	func() {
		defer close(release)
		a.HandleMessage(context.Background(), first)
		if !waitClosedWithin(entered, 5*time.Second) {
			t.Fatal("A never reached its capture")
		}
		a.HandleMessage(context.Background(), burstText(source, "B", "B"))
		go func() {
			defer close(restart)
			a.HandleMessage(context.Background(), burstText(source, "/restart", "R"))
		}()
		burstWait(t, 10*time.Second, "B to queue behind the capture", func() bool {
			passedMu.Lock()
			gated := len(passed) > 0
			passedMu.Unlock()
			return gated || r.routeCount(key) >= 2
		})
	}()
	if !waitClosedWithin(restart, 10*time.Second) {
		t.Fatal("the restart never returned")
	}
	passedMu.Lock()
	got := append([][]string(nil), passed...)
	passedMu.Unlock()
	r.setDraining(false)
	if !reflect.DeepEqual(got, [][]string{{"A", "B"}}) {
		t.Fatalf("passed %v", got)
	}
}

// Buzz adapter double, both readers await handle events before the next batch and persist the cursor after
type burstBuzz struct {
	burstAdapter
	state       sync.Mutex
	lastTS      int
	seen        map[string]bool
	resolveName func(ctx context.Context, pubkey string) string
	writeLock   chan struct{}
	lockWaiters atomic.Int32
	snapshots   int
	written     int
	saved       func(lastTS int)
}

type burstBuzzEvent struct {
	id, pubkey, content string
	createdAt           int
}

func (a *burstBuzz) cursorMark() int {
	a.state.Lock()
	defer a.state.Unlock()
	return a.lastTS
}

// Cursor write of the channel state as of this call, skipped when a newer write already landed
func (a *burstBuzz) cursorWriter() func(ctx context.Context) error {
	a.state.Lock()
	lastTS := a.lastTS
	a.snapshots++
	generation := a.snapshots
	a.state.Unlock()
	return func(ctx context.Context) error {
		a.lockWaiters.Add(1)
		select {
		case a.writeLock <- struct{}{}:
			a.lockWaiters.Add(-1)
		case <-ctx.Done():
			a.lockWaiters.Add(-1)
			return ctx.Err()
		}
		defer func() { <-a.writeLock }()
		a.state.Lock()
		newer := generation > a.written
		if newer {
			a.written = generation
		}
		a.state.Unlock()
		if newer {
			a.saved(lastTS)
		}
		return nil
	}
}

// Dispatches a batch with each event marked seen first, persisting the cursor once it moved
func (a *burstBuzz) handleEvents(ctx context.Context, channelID string, events []burstBuzzEvent) error {
	before := a.cursorMark()
	for _, ev := range events {
		a.state.Lock()
		a.seen[ev.id] = true
		a.lastTS = max(a.lastTS, ev.createdAt)
		resolve := a.resolveName
		a.state.Unlock()
		name := resolve(ctx, ev.pubkey)
		source := burstSource(Platform("buzz"), channelID, "dm", ev.pubkey)
		source.UserName = &name
		if err := a.HandleMessage(ctx, burstText(source, ev.content, ev.id)); err != nil {
			return err
		}
	}
	if a.cursorMark() != before {
		return WhenReturnedInputsDurable(ctx, a, a.cursorWriter())
	}
	return nil
}

func burstBuzzAdapter(t *testing.T) (*burstBuzz, *burstRuntime, *SessionSource) {
	a := &burstBuzz{seen: map[string]bool{}, writeLock: make(chan struct{}, 1)}
	a.resolveName = func(_ context.Context, pubkey string) string { return pubkey[:8] }
	r := newBurstRuntime(t, &a.burstAdapter, a, Platform("buzz"), true, "queue", nil)
	return a, r, burstSource(Platform("buzz"), "chan", "dm", "aa11bb22")
}

func burstBuzzEventOf(i int, text string) burstBuzzEvent {
	return burstBuzzEvent{id: fmt.Sprintf("e%d", i), pubkey: "aa11bb22", content: text, createdAt: i}
}

type burstSavedCursor struct {
	lastTS int
	rows   map[string]bool
}

// Records every cursor write with the admission rows that existed when it landed
func burstRecordCursors(a *burstBuzz, r *burstRuntime) func() []burstSavedCursor {
	var mu sync.Mutex
	var saved []burstSavedCursor
	a.saved = func(lastTS int) {
		rows := map[string]bool{}
		for _, id := range r.requestIDs(false) {
			rows[id] = true
		}
		mu.Lock()
		saved = append(saved, burstSavedCursor{lastTS, rows})
		mu.Unlock()
	}
	return func() []burstSavedCursor {
		mu.Lock()
		defer mu.Unlock()
		return append([]burstSavedCursor(nil), saved...)
	}
}

// Every saved cursor names only events that had an admission row when it was written
func burstCursorsCommitted(t *testing.T, saved []burstSavedCursor) {
	for _, s := range saved {
		for n := 1; n <= s.lastTS; n++ {
			if !s.rows[fmt.Sprintf("e%d", n)] {
				t.Fatalf("cursor %d written before e%d committed %v", s.lastTS, n, saved)
			}
		}
	}
}

func TestBuzzSplitTextMergesThroughTheReader(t *testing.T) {
	a, r, source := burstBuzzAdapter(t)
	burstRecordCursors(a, r)
	key := burstBusyOwner(t, &a.burstAdapter, r, source)
	ctx := context.Background()
	if err := a.handleEvents(ctx, "chan", []burstBuzzEvent{burstBuzzEventOf(1, "first half")}); err != nil {
		t.Fatal(err)
	}
	if err := a.handleEvents(ctx, "chan", []burstBuzzEvent{burstBuzzEventOf(2, "second half")}); err != nil {
		t.Fatal(err)
	}
	if got := burstSettle(t, &a.burstAdapter, r, key, 2); !reflect.DeepEqual(got, []burstTurn{{"first half\nsecond half", 0}}) {
		t.Fatalf("got %v", got)
	}
}

func TestBuzzCursorNeverPassesAnUncommittedInput(t *testing.T) {
	a, r, source := burstBuzzAdapter(t)
	saved := burstRecordCursors(a, r)
	key := burstBusyOwner(t, &a.burstAdapter, r, source)
	ctx := context.Background()
	if err := a.handleEvents(ctx, "chan", []burstBuzzEvent{burstBuzzEventOf(1, "held")}); err != nil {
		t.Fatal(err)
	}
	if !heldBurst(a, key) || len(saved()) != 0 {
		t.Fatalf("returned with the input only held, saved %v", saved())
	}
	gate := make(chan struct{})
	a.state.Lock()
	real := a.resolveName
	a.resolveName = func(ctx context.Context, pubkey string) string {
		<-gate
		return real(ctx, pubkey)
	}
	a.state.Unlock()
	second := make(chan error, 1)
	go func() { second <- a.handleEvents(ctx, "chan", []burstBuzzEvent{burstBuzzEventOf(2, "next")}) }()
	burstWait(t, 15*time.Second, "the deferred cursor write", func() bool { return len(saved()) > 0 })
	close(gate)
	select {
	case err := <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second batch never returned")
	}
	if got := burstSettle(t, &a.burstAdapter, r, key, 3); !reflect.DeepEqual(got, []burstTurn{{"held", 0}, {"next", 0}}) {
		t.Fatalf("got %v", got)
	}
	if got := saved(); got[0].lastTS != 1 {
		t.Fatalf("saved %v", got)
	}
	burstCursorsCommitted(t, saved())
}

func TestBuzzCursorWriteWaitingOnDiskSkipsANewerHeldBurst(t *testing.T) {
	a, r, source := burstBuzzAdapter(t)
	saved := burstRecordCursors(a, r)
	key := burstBusyOwner(t, &a.burstAdapter, r, source)
	ctx := context.Background()
	a.writeLock <- struct{}{}
	if err := a.handleEvents(ctx, "chan", []burstBuzzEvent{burstBuzzEventOf(1, "held")}); err != nil {
		t.Fatal(err)
	}
	burstWait(t, 15*time.Second, "e1's committed write to queue on the lock", func() bool { return a.lockWaiters.Load() > 0 })
	if err := a.handleEvents(ctx, "chan", []burstBuzzEvent{burstBuzzEventOf(2, "next")}); err != nil {
		t.Fatal(err)
	}
	if !heldBurst(a, key) {
		t.Fatal("e2 is not held")
	}
	<-a.writeLock
	burstWait(t, 15*time.Second, "the waiting cursor write", func() bool { return len(saved()) > 0 })
	if got := saved(); got[0].lastTS != 1 {
		t.Fatalf("saved %v", got)
	}
	if got := burstSettle(t, &a.burstAdapter, r, key, 3); !reflect.DeepEqual(got, []burstTurn{{"held", 0}, {"next", 0}}) {
		t.Fatalf("got %v", got)
	}
	burstCursorsCommitted(t, saved())
}
