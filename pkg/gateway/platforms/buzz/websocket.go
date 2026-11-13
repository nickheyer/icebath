package buzz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/gateway"
)

const (
	wsAuthTimeout     = 20 * time.Second
	wsReadIdleTimeout = 300 * time.Second
	wsMaxMessageBytes = 2_000_000
	wsPingInterval    = 20 * time.Second
	wsPingTimeout     = 20 * time.Second
	wsCloseTimeout    = 5 * time.Second
	wsMembershipKind  = 44100
	wsMembershipSubID = "icebath-buzz-membership"
	wsMaxBackoff      = 30 * time.Second
	wsFrameQueue      = 16
)

// Failed write on the relay socket, the dead connection every loop tears down on
type wsWriteError struct{ err error }

func (e *wsWriteError) Error() string { return e.err.Error() }
func (e *wsWriteError) Unwrap() error { return e.err }

// One frame or the read error that ended the socket
type wsFrame struct {
	data []byte
	err  error
}

// One authenticated relay connection with its subscriptions and the frames its reader queued
type wsSession struct {
	conn    *websocket.Conn
	frames  chan wsFrame
	done    <-chan struct{}
	writeMu sync.Mutex
	subMu   sync.Mutex
	subs    map[string]string
	nextDM  int
	failure atomic.Pointer[error]
}

// Reads frames continuously so pings and pongs keep flowing while a frame is being handled
func (s *wsSession) readFrames() {
	for {
		_, raw, err := s.conn.ReadMessage()
		if err != nil {
			select {
			case s.frames <- wsFrame{err: err}:
			case <-s.done:
			}
			return
		}
		select {
		case s.frames <- wsFrame{data: raw}:
		case <-s.done:
			return
		}
	}
}

// Next queued frame within the timeout, the read error mapped to why the socket ended
func (s *wsSession) next(timeout time.Duration) ([]byte, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case f := <-s.frames:
		if f.err == nil {
			return f.data, nil
		}
		if failure := s.failure.Load(); failure != nil {
			return nil, *failure
		}
		if websocket.IsCloseError(f.err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
			return nil, errors.New("relay closed the WebSocket")
		}
		return nil, f.err
	case <-timer.C:
		return nil, errWSTimeout
	case <-s.done:
		return nil, context.Canceled
	}
}

// No frame arrived within the wait
var errWSTimeout = errors.New("timed out waiting for a relay frame")

// Writes one JSON frame, serialized against the other writers
func (s *wsSession) send(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return &wsWriteError{err}
	}
	return nil
}

// Channel of a subscription id, false for unknown ids
func (s *wsSession) channelOf(subID string) (string, bool) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	ch, ok := s.subs[subID]
	return ch, ok
}

// Records why the keepalive closed the socket so the reader reports it
func (s *wsSession) fail(err error) { s.failure.CompareAndSwap(nil, &err) }

// WebSocket URL of the relay, http mapped to ws and https to wss
func websocketURL(relayURL string) (string, error) {
	u, err := url.Parse(strip(relayURL))
	if err != nil {
		return "", err
	}
	scheme := map[string]string{"http": "ws", "https": "wss"}[u.Scheme]
	if scheme == "" {
		scheme = u.Scheme
	}
	if (scheme != "ws" && scheme != "wss") || u.Host == "" {
		return "", errors.New("Buzz relay URL must use http(s) or ws(s)")
	}
	netloc := u.Host
	if u.User != nil {
		netloc = u.User.String() + "@" + netloc
	}
	out := scheme + "://" + netloc + u.EscapedPath()
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out, nil
}

// Starts the WebSocket loop, true once it authenticates and subscribes within the timeout
func (a *Adapter) startWebsocket(ctx context.Context) bool {
	if _, err := websocketURL(a.relayURL); err != nil {
		slog.Info(fmt.Sprintf("Buzz: WebSocket transport unavailable (%v); falling back to polling", err))
		return false
	}
	ready := make(chan struct{})
	var once sync.Once
	a.mu.Lock()
	a.membershipSince = time.Now().Unix()
	a.mu.Unlock()
	task := gateway.SpawnAdapterTask(ctx, func(tctx context.Context) error {
		return a.websocketLoop(tctx, func() { once.Do(func() { close(ready) }) })
	})
	a.taskMu.Lock()
	a.wsTask = task
	a.taskMu.Unlock()
	timer := time.NewTimer(wsAuthTimeout + 5*time.Second)
	defer timer.Stop()
	select {
	case <-ready:
		return true
	case <-timer.C:
		slog.Warn("Buzz: WebSocket did not authenticate in time")
	case <-ctx.Done():
	}
	a.taskMu.Lock()
	if a.wsTask == task {
		a.wsTask = nil
	}
	a.taskMu.Unlock()
	gateway.CancelTask(ctx, task)
	return false
}

// Keeps an authenticated subscription alive with bounded reconnect backoff, health following each recovery
func (a *Adapter) websocketLoop(ctx context.Context, ready func()) error {
	backoff := time.Second
	reconnecting := false
	for {
		err := a.websocketSession(ctx, func() {
			ready()
			if reconnecting {
				reconnecting = false
				a.MarkConnected(ctx, "")
			}
			backoff = time.Second
		})
		if ctx.Err() != nil {
			return nil
		}
		if !reconnecting {
			reconnecting = true
			a.MarkDegraded(ctx)
		}
		slog.Warn(fmt.Sprintf("Buzz: WebSocket disconnected; retrying in %.1fs: %v", backoff.Seconds(), err))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, wsMaxBackoff)
	}
}

// One connection, authenticate, subscribe, then read and rediscover until either side finds the socket dead
func (a *Adapter) websocketSession(ctx context.Context, subscribed func()) error {
	wsURL, err := websocketURL(a.relayURL)
	if err != nil {
		return err
	}
	dialer := websocket.Dialer{Proxy: http.ProxyFromEnvironment, HandshakeTimeout: wsAuthTimeout,
		NetDialContext: (&net.Dialer{FallbackDelay: 250 * time.Millisecond}).DialContext}
	dctx, cancel := context.WithTimeout(ctx, wsAuthTimeout)
	conn, resp, err := dialer.DialContext(dctx, wsURL, nil)
	cancel()
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		return err
	}
	conn.SetReadLimit(wsMaxMessageBytes)
	sctx, stop := context.WithCancel(ctx)
	session := &wsSession{conn: conn, frames: make(chan wsFrame, wsFrameQueue), done: sctx.Done(), subs: map[string]string{}}
	var wg sync.WaitGroup
	defer func() {
		conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(wsCloseTimeout))
		stop()
		conn.Close()
		wg.Wait()
	}()
	pongs := make(chan struct{}, 1)
	conn.SetPongHandler(func(string) error {
		select {
		case pongs <- struct{}{}:
		default:
		}
		return nil
	})
	wg.Add(3)
	go func() {
		defer wg.Done()
		<-sctx.Done()
		conn.Close()
	}()
	go func() {
		defer wg.Done()
		keepalive(sctx, session, pongs)
	}()
	go func() {
		defer wg.Done()
		session.readFrames()
	}()
	if err := a.authenticateWebsocket(ctx, session, wsURL); err != nil {
		return err
	}
	if err := a.subscribeWebsocket(session); err != nil {
		return err
	}
	subscribed()
	errs := make(chan error, 2)
	go func() { errs <- a.wsReadLoop(sctx, session) }()
	go func() { errs <- a.wsDiscoveryLoop(sctx, session) }()
	err = <-errs
	stop()
	<-errs
	return err
}

// Pings the relay on an interval and closes the socket when a pong does not come back in time
func keepalive(ctx context.Context, session *wsSession, pongs chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wsPingInterval):
		}
		select {
		case <-pongs:
		default:
		}
		if err := session.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsPingTimeout)); err != nil {
			session.fail(fmt.Errorf("keepalive ping failed: %w", err))
			session.conn.Close()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-pongs:
		case <-time.After(wsPingTimeout):
			session.fail(errors.New("keepalive ping timeout"))
			session.conn.Close()
			return
		}
	}
}

// One decoded relay frame read within the timeout, nil for a frame that is not a JSON list
func readFrame(session *wsSession, timeout time.Duration) ([]any, error) {
	raw, err := session.next(timeout)
	if errors.Is(err, errWSTimeout) {
		return nil, errors.New("timed out waiting for the relay during NIP-42 authentication")
	}
	if err != nil {
		return nil, err
	}
	decoded, err := decodeJSON(raw)
	if err != nil {
		return nil, err
	}
	list, _ := decoded.([]any)
	return list, nil
}

// Answers the relay's NIP-42 challenge with a signed kind 22242 event and waits for its OK
func (a *Adapter) authenticateWebsocket(ctx context.Context, session *wsSession, wsURL string) error {
	message, err := readFrame(session, wsAuthTimeout)
	if err != nil {
		return err
	}
	if len(message) < 2 || message[0] != "AUTH" {
		return errors.New("Buzz relay did not send a NIP-42 AUTH challenge")
	}
	a.mu.Lock()
	key, tag := a.privateKey, a.authTag
	a.mu.Unlock()
	if tag == "" {
		s := &settings{ctx: ctx, extra: a.extra}
		if resolved, err := s.authTag(); err == nil && s.err == nil {
			tag = resolved
		}
	}
	event, err := buildAuthEvent(key, textOf(message[1]), wsURL, tag)
	if err != nil {
		return err
	}
	if err := session.send([]any{"AUTH", event}); err != nil {
		return err
	}
	for {
		response, err := readFrame(session, wsAuthTimeout)
		if err != nil {
			return err
		}
		if len(response) == 0 {
			continue
		}
		if response[0] == "OK" && len(response) >= 4 && response[1] == event.ID {
			if ok, isBool := response[2].(bool); isBool && ok {
				return nil
			}
			return fmt.Errorf("Buzz WebSocket AUTH rejected: %s", textOf(response[3]))
		}
		if response[0] == "NOTICE" || response[0] == "CLOSED" {
			detail := "authentication failed"
			if len(response) > 1 {
				detail = textOf(response[len(response)-1])
			}
			return fmt.Errorf("Buzz WebSocket AUTH failed: %s", detail)
		}
	}
}

// Sends the REQ for one conversation, resuming from its high water mark or replaying a fresh one from its start
func (a *Adapter) sendChannelSubscription(session *wsSession, subID, channelID string) error {
	a.mu.Lock()
	var lastTS int64
	if st, ok := a.state.get(channelID); ok {
		lastTS = st.lastTS
	}
	a.mu.Unlock()
	kinds := make([]int64, 0, len(dispatchKinds))
	for k := range dispatchKinds {
		kinds = append(kinds, k)
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	filter := map[string]any{"kinds": kinds, "#h": []string{channelID}}
	if lastTS != 0 {
		filter["since"] = max(lastTS-1, 0)
	} else {
		filter["limit"] = fetchLimit
	}
	return session.send([]any{"REQ", subID, filter})
}

// Subscribes to every watched conversation and to membership events naming this identity
func (a *Adapter) subscribeWebsocket(session *wsSession) error {
	a.mu.Lock()
	channels := append([]string(nil), a.state.keys...)
	restricted := map[string]bool{}
	for k := range a.restricted {
		restricted[k] = true
	}
	self, since := a.selfPubkey, a.membershipSince
	a.mu.Unlock()
	for index, channelID := range channels {
		if restricted[channelID] {
			continue
		}
		subID := fmt.Sprintf("icebath-buzz-%d", index)
		session.subMu.Lock()
		session.subs[subID] = channelID
		session.subMu.Unlock()
		if err := a.sendChannelSubscription(session, subID, channelID); err != nil {
			return err
		}
	}
	if self != "" {
		filter := map[string]any{"kinds": []int{wsMembershipKind}, "#p": []string{self}, "since": max(since-1, 0)}
		if err := session.send([]any{"REQ", wsMembershipSubID, filter}); err != nil {
			return err
		}
		session.subMu.Lock()
		session.subs[wsMembershipSubID] = ""
		session.subMu.Unlock()
	}
	return nil
}

// Rediscovers conversations and subscribes to each one adopted since, fresh DMs replaying from their start
func (a *Adapter) rediscoverAndSubscribe(ctx context.Context, session *wsSession) error {
	a.discoverMu.Lock()
	defer a.discoverMu.Unlock()
	before := map[string]bool{}
	for _, id := range a.watchedChannels() {
		before[id] = true
	}
	if err := a.discoverDMs(ctx, false); err != nil {
		return err
	}
	for _, channelID := range a.watchedChannels() {
		if before[channelID] {
			continue
		}
		session.subMu.Lock()
		subID := fmt.Sprintf("icebath-buzz-dm-%d", session.nextDM)
		session.nextDM++
		session.subs[subID] = channelID
		session.subMu.Unlock()
		if err := a.sendChannelSubscription(session, subID, channelID); err != nil {
			return err
		}
		slog.Info("Buzz: subscribed to new conversation " + channelID)
	}
	return nil
}

// Periodic rediscovery on the poll cadence since relays do not announce every new conversation, a dead socket ending it
func (a *Adapter) wsDiscoveryLoop(ctx context.Context, session *wsSession) error {
	interval := max(secondsDuration(a.pollInterval.Seconds()*dmDiscoveryEvery), secondsDuration(minPollInterval))
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
		err := a.rediscoverAndSubscribe(ctx, session)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var writeErr *wsWriteError
		if errors.As(err, &writeErr) {
			return err
		}
		if err != nil {
			slog.Warn(fmt.Sprintf("Buzz: WebSocket discovery sweep failed: %v", err))
		}
	}
}

// Reads relay frames until the socket closes or goes silent, each event handled before the next read
func (a *Adapter) wsReadLoop(ctx context.Context, session *wsSession) error {
	for {
		raw, err := session.next(wsReadIdleTimeout)
		if errors.Is(err, errWSTimeout) {
			return fmt.Errorf("no WebSocket frame for %.0fs; assuming the connection went silent", wsReadIdleTimeout.Seconds())
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		decoded, err := decodeJSON(raw)
		if err != nil {
			slog.Warn("Buzz: ignoring malformed WebSocket frame")
			continue
		}
		if message, ok := decoded.([]any); ok && len(message) > 0 {
			if err := a.handleWSMessage(ctx, session, message); err != nil {
				return err
			}
		}
	}
}

// Routes one relay frame, events to their conversation, permanent rejections dropping the channel, other closes reconnecting
func (a *Adapter) handleWSMessage(ctx context.Context, session *wsSession, message []any) error {
	switch message[0] {
	case "EVENT":
		if len(message) < 3 {
			return nil
		}
		subID := textOf(message[1])
		event, ok := message[2].(map[string]any)
		if !ok {
			return nil
		}
		if subID == wsMembershipSubID {
			a.mu.Lock()
			if ts := intOr0(event["created_at"]); ts > a.membershipSince {
				a.membershipSince = ts
			}
			a.mu.Unlock()
			return a.rediscoverAndSubscribe(ctx, session)
		}
		channelID, _ := session.channelOf(subID)
		a.mu.Lock()
		st, watched := a.state.get(channelID)
		a.mu.Unlock()
		if channelID != "" && watched {
			return a.handleEvents(ctx, channelID, st, []map[string]any{event})
		}
	case "CLOSED":
		var detail any = "subscription closed"
		if len(message) > 2 {
			detail = message[len(message)-1]
		}
		subID := ""
		if len(message) > 1 {
			subID = textOf(message[1])
		}
		closedChannel, _ := session.channelOf(subID)
		text := textOf(detail)
		lowered := strings.ToLower(text)
		rejected := strings.Contains(lowered, "restricted") || strings.Contains(lowered, "not a channel member") ||
			strings.Contains(lowered, "auth-required")
		if !rejected || closedChannel == "" {
			return errors.New(text)
		}
		slog.Warn(fmt.Sprintf("Buzz: relay permanently rejected channel %s (%s) — removing from watch list", closedChannel, text))
		session.subMu.Lock()
		delete(session.subs, subID)
		session.subMu.Unlock()
		a.mu.Lock()
		a.restricted[closedChannel] = true
		a.state.remove(closedChannel)
		a.mu.Unlock()
	case "NOTICE":
		slog.Warn(fmt.Sprintf("Buzz: relay notice: %s", textOf(message[len(message)-1])))
	}
	return nil
}
