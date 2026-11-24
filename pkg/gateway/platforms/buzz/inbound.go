package buzz

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dlclark/regexp2/v2"
	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/netpref"
	"github.com/nickheyer/icebath/pkg/internal/pymimetypes"
	"github.com/nickheyer/icebath/pkg/mediacache"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Websocket URL of the relay, http mapped to ws and https to wss
func (a *BuzzAdapter) websocketURL() (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(a.relayURL))
	if err != nil {
		return "", err
	}
	scheme := map[string]string{"http": "ws", "https": "wss"}[parsed.Scheme]
	if scheme == "" {
		scheme = parsed.Scheme
	}
	if (scheme != "ws" && scheme != "wss") || parsed.Host == "" {
		return "", errors.New("Buzz relay URL must use http(s) or ws(s)")
	}
	out := url.URL{Scheme: scheme, Host: parsed.Host, Path: parsed.Path, RawPath: parsed.RawPath, RawQuery: parsed.RawQuery}
	return out.String(), nil
}

// Starts the websocket loop, true when it authenticates within the timeout
func (a *BuzzAdapter) startWebsocket(ctx context.Context) bool {
	if _, err := a.websocketURL(); err != nil {
		pkgLog().Info(fmt.Sprintf("Buzz: WebSocket transport unavailable (%s); falling back to polling", err))
		return false
	}
	ready := make(chan struct{})
	a.mu.Lock()
	a.wsReady, a.wsReadyOnce = ready, &sync.Once{}
	a.membership = time.Now().Unix()
	a.mu.Unlock()
	task := gateway.SpawnAdapterTask(ctx, a.websocketLoop)
	a.mu.Lock()
	a.wsTask = task
	a.mu.Unlock()
	select {
	case <-ready:
		return true
	case <-time.After(wsAuthTimeout + 5*time.Second):
	case <-ctx.Done():
	}
	pkgLog().Warn("Buzz: WebSocket did not authenticate in time")
	gateway.CancelTask(ctx, task)
	a.mu.Lock()
	a.wsTask = nil
	a.mu.Unlock()
	return false
}

// Websocket connection with serialized writes and a pong clock for the keepalive
type relayConn struct {
	conn     *websocket.Conn
	writeMu  sync.Mutex
	pongMu   sync.Mutex
	lastPing time.Time
	lastPong time.Time
}

// Sends one compact JSON frame
func (c *relayConn) sendJSON(v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(wsAuthTimeout))
	return c.conn.WriteMessage(websocket.TextMessage, bytes.TrimRight(buf.Bytes(), "\n"))
}

// Next decoded frame within the timeout
func (c *relayConn) recvJSON(timeout time.Duration) (any, error) {
	_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
	_, raw, err := c.conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	var out any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, errMalformedFrame
	}
	return out, nil
}

// Frame that does not decode as JSON
var errMalformedFrame = errors.New("malformed frame")

// Pings every interval and closes the socket when the previous ping went unanswered
func (c *relayConn) keepalive(ctx context.Context) {
	ticker := time.NewTicker(wsPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		c.pongMu.Lock()
		missed := !c.lastPing.IsZero() && c.lastPong.Before(c.lastPing)
		c.lastPing = time.Now()
		c.pongMu.Unlock()
		if missed {
			_ = c.conn.Close()
			return
		}
		if err := c.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
			_ = c.conn.Close()
			return
		}
	}
}

// NIP-42 handshake, the AUTH challenge answered with a signed kind 22242 event and the relay OK awaited
func (a *BuzzAdapter) authenticateWebsocket(ctx context.Context, c *relayConn) error {
	first, err := c.recvJSON(wsAuthTimeout)
	if err != nil {
		return err
	}
	message, ok := first.([]any)
	if !ok || len(message) < 2 || strOf(message[0]) != "AUTH" {
		return errors.New("Buzz relay did not send a NIP-42 AUTH challenge")
	}
	a.mu.Lock()
	authTag, key := a.authTag, a.privateKey
	a.mu.Unlock()
	if authTag == "" {
		if tag, err := resolveAuthTag(ctx, a.extra); err == nil {
			authTag = tag
		}
	}
	wsURL, err := a.websocketURL()
	if err != nil {
		return err
	}
	event, err := BuildAuthEvent(AuthEventParams{PrivateKey: key, Challenge: strOf(message[1]), RelayURL: wsURL, AuthTagJSON: authTag})
	if err != nil {
		return err
	}
	if err := c.sendJSON([]any{"AUTH", event}); err != nil {
		return err
	}
	for {
		raw, err := c.recvJSON(wsAuthTimeout)
		if errors.Is(err, errMalformedFrame) {
			continue
		}
		if err != nil {
			return err
		}
		response, ok := raw.([]any)
		if !ok || len(response) == 0 {
			continue
		}
		switch strOf(response[0]) {
		case "OK":
			if len(response) >= 4 && strOf(response[1]) == event.ID {
				if accepted, _ := response[2].(bool); accepted {
					return nil
				}
				return fmt.Errorf("Buzz WebSocket AUTH rejected: %s", strOf(response[3]))
			}
		case "NOTICE", "CLOSED":
			detail := "authentication failed"
			if len(response) > 1 {
				detail = strOf(response[len(response)-1])
			}
			return fmt.Errorf("Buzz WebSocket AUTH failed: %s", detail)
		}
	}
}

// Subscribes a conversation from its high water mark, or from its start with the fetch limit when it has none
func (a *BuzzAdapter) sendChannelSubscription(c *relayConn, subscriptionID, channelID string) error {
	a.mu.Lock()
	var lastTS int64
	if st := a.state[channelID]; st != nil {
		lastTS = st.cursor.LastTS
	}
	a.mu.Unlock()
	filter := map[string]any{"kinds": dispatchKindList, "#h": []string{channelID}}
	if lastTS != 0 {
		filter["since"] = max(lastTS-1, 0)
	} else {
		filter["limit"] = fetchLimit
	}
	return c.sendJSON([]any{"REQ", subscriptionID, filter})
}

// Subscription ids of a live connection mapped to their conversation, empty for the membership subscription
type subscriptionMap struct {
	mu  sync.Mutex
	ids map[string]string
}

// Subscribes every watched conversation plus membership events tagged to this identity
func (a *BuzzAdapter) subscribeWebsocket(c *relayConn) (*subscriptionMap, error) {
	subs := &subscriptionMap{ids: map[string]string{}}
	a.mu.Lock()
	watched := append([]string(nil), a.cursors.ChannelOrder...)
	restricted := map[string]bool{}
	for k := range a.restricted {
		restricted[k] = true
	}
	self, since := a.selfPubkey, a.membership
	a.mu.Unlock()
	for index, channelID := range watched {
		if restricted[channelID] {
			continue
		}
		id := fmt.Sprintf("icebath-buzz-%d", index)
		subs.ids[id] = channelID
		if err := a.sendChannelSubscription(c, id, channelID); err != nil {
			return nil, err
		}
	}
	if self != "" {
		membership := map[string]any{"kinds": []int{wsMembershipKind}, "#p": []string{self}, "since": max(since-1, 0)}
		if err := c.sendJSON([]any{"REQ", wsMembershipSubID, membership}); err != nil {
			return nil, err
		}
		subs.ids[wsMembershipSubID] = ""
	}
	return subs, nil
}

// Rediscovers conversations and subscribes to any adopted since, fresh ones from their start
func (a *BuzzAdapter) rediscoverAndSubscribe(ctx context.Context, c *relayConn, subs *subscriptionMap) error {
	a.mu.Lock()
	before := map[string]bool{}
	for id := range a.state {
		before[id] = true
	}
	a.mu.Unlock()
	if err := a.discoverDMs(ctx, false); err != nil {
		return err
	}
	a.mu.Lock()
	watched := append([]string(nil), a.cursors.ChannelOrder...)
	a.mu.Unlock()
	for _, channelID := range watched {
		if before[channelID] {
			continue
		}
		subs.mu.Lock()
		id := fmt.Sprintf("icebath-buzz-dm-%d", len(subs.ids))
		subs.ids[id] = channelID
		subs.mu.Unlock()
		if err := a.sendChannelSubscription(c, id, channelID); err != nil {
			return err
		}
		pkgLog().Info("Buzz: subscribed to new conversation " + channelID)
	}
	return nil
}

// Error of a websocket write on a dead connection, the signal that tears the connection down
func isClosedError(err error) bool {
	var closeErr *websocket.CloseError
	return errors.As(err, &closeErr) || errors.Is(err, websocket.ErrCloseSent) || errors.Is(err, net.ErrClosed)
}

// Periodic rediscovery on the poll cadence, failures retried next tick except a closed socket
func (a *BuzzAdapter) wsDiscoveryLoop(ctx context.Context, c *relayConn, subs *subscriptionMap) error {
	interval := time.Duration(max(a.pollInterval*dmDiscoveryEvery, minPollInterval) * float64(time.Second))
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
		if err := a.rediscoverAndSubscribe(ctx, c, subs); err != nil {
			if ctx.Err() != nil || isClosedError(err) {
				return err
			}
			pkgLog().Warn("Buzz: WebSocket discovery sweep failed", "error", err)
		}
	}
}

// Persistent authenticated subscription with bounded reconnect backoff, since filters resuming on reconnect
func (a *BuzzAdapter) websocketLoop(ctx context.Context) error {
	backoff := 1.0
	reconnecting := false
	for {
		err := a.websocketSession(ctx, &reconnecting, &backoff)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !reconnecting {
			reconnecting = true
			a.MarkDegraded(ctx)
		}
		pkgLog().Warn(fmt.Sprintf("Buzz: WebSocket disconnected; retrying in %.1fs: %v", backoff, err))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(backoff * float64(time.Second))):
		}
		backoff = min(backoff*2, 30.0)
	}
}

// One websocket connection from dial to the first failure of its read or discovery loop
func (a *BuzzAdapter) websocketSession(ctx context.Context, reconnecting *bool, backoff *float64) error {
	wsURL, err := a.websocketURL()
	if err != nil {
		return err
	}
	netDialer := &net.Dialer{Timeout: wsAuthTimeout, FallbackDelay: 250 * time.Millisecond}
	dialer := websocket.Dialer{HandshakeTimeout: wsAuthTimeout, Proxy: http.ProxyFromEnvironment, NetDialContext: netpref.DialContext(netDialer.DialContext)}
	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return err
	}
	conn.SetReadLimit(wsMaxMessageBytes)
	c := &relayConn{conn: conn}
	conn.SetPongHandler(func(string) error {
		c.pongMu.Lock()
		c.lastPong = time.Now()
		c.pongMu.Unlock()
		return nil
	})
	defer func() {
		c.writeMu.Lock()
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(5*time.Second))
		c.writeMu.Unlock()
		_ = conn.Close()
	}()
	if err := a.authenticateWebsocket(ctx, c); err != nil {
		return err
	}
	subs, err := a.subscribeWebsocket(c)
	if err != nil {
		return err
	}
	a.mu.Lock()
	ready, once := a.wsReady, a.wsReadyOnce
	a.mu.Unlock()
	if ready != nil && once != nil {
		once.Do(func() { close(ready) })
	}
	if *reconnecting {
		*reconnecting = false
		a.MarkConnected(ctx, "")
	}
	*backoff = 1.0
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go c.keepalive(sessionCtx)
	errs := make(chan error, 2)
	go func() { errs <- a.wsReadLoop(sessionCtx, c, subs) }()
	go func() { errs <- a.wsDiscoveryLoop(sessionCtx, c, subs) }()
	first := <-errs
	cancel()
	_ = conn.Close()
	<-errs
	return first
}

// Reads frames until the relay closes or goes silent past the idle bound, both ending the connection
func (a *BuzzAdapter) wsReadLoop(ctx context.Context, c *relayConn, subs *subscriptionMap) error {
	for {
		raw, err := c.recvJSON(wsReadIdleTimeout)
		if errors.Is(err, errMalformedFrame) {
			pkgLog().Warn("Buzz: ignoring malformed WebSocket frame")
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				return fmt.Errorf("no WebSocket frame for %.0fs; assuming the connection went silent", wsReadIdleTimeout.Seconds())
			}
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) {
				return errors.New("relay closed the WebSocket")
			}
			return err
		}
		if message, ok := raw.([]any); ok && len(message) > 0 {
			if err := a.handleWSMessage(ctx, c, subs, message); err != nil {
				return err
			}
		}
	}
}

// Routes one relay frame, EVENT dispatched, a permanent CLOSED rejection dropping its channel, NOTICE logged
func (a *BuzzAdapter) handleWSMessage(ctx context.Context, c *relayConn, subs *subscriptionMap, message []any) error {
	switch strOf(message[0]) {
	case "EVENT":
		if len(message) < 3 {
			return nil
		}
		subscriptionID := strOf(message[1])
		event, ok := message[2].(map[string]any)
		if !ok {
			return nil
		}
		if subscriptionID == wsMembershipSubID {
			a.mu.Lock()
			a.membership = max(a.membership, intOf(event["created_at"]))
			a.mu.Unlock()
			return a.rediscoverAndSubscribe(ctx, c, subs)
		}
		subs.mu.Lock()
		channelID := subs.ids[subscriptionID]
		subs.mu.Unlock()
		if channelID == "" {
			return nil
		}
		a.mu.Lock()
		st := a.state[channelID]
		a.mu.Unlock()
		if st != nil {
			return a.handleEvents(ctx, channelID, st, []map[string]any{event})
		}
	case "CLOSED":
		detail := "subscription closed"
		if len(message) > 2 {
			detail = strOf(message[len(message)-1])
		}
		subID := ""
		if len(message) > 1 {
			subID = strOf(message[1])
		}
		subs.mu.Lock()
		closed := subs.ids[subID]
		subs.mu.Unlock()
		lowered := strings.ToLower(detail)
		rejected := strings.Contains(lowered, "restricted") || strings.Contains(lowered, "not a channel member") || strings.Contains(lowered, "auth-required")
		if !rejected || closed == "" {
			return errors.New(detail)
		}
		pkgLog().Warn(fmt.Sprintf("Buzz: relay permanently rejected channel %s (%s) — removing from watch list", closed, detail))
		subs.mu.Lock()
		delete(subs.ids, subID)
		subs.mu.Unlock()
		a.mu.Lock()
		a.restricted[closed] = true
		a.forgetLocked(closed)
		a.mu.Unlock()
	case "NOTICE":
		pkgLog().Warn("Buzz: relay notice: " + strOf(message[len(message)-1]))
	}
	return nil
}

// Polls every watched conversation on the interval until cancelled, rediscovering every few sweeps
func (a *BuzzAdapter) pollLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(a.pollInterval * float64(time.Second))):
		}
		a.mu.Lock()
		a.pollCount++
		sweep := a.pollCount
		a.mu.Unlock()
		err := func() error {
			if sweep%dmDiscoveryEvery == 0 {
				if err := a.discoverDMs(ctx, false); err != nil {
					return err
				}
			}
			a.mu.Lock()
			watched := append([]string(nil), a.cursors.ChannelOrder...)
			a.mu.Unlock()
			for _, channelID := range watched {
				if err := a.pollChannel(ctx, channelID); err != nil {
					return err
				}
			}
			return nil
		}()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			pkgLog().Warn("Buzz: poll sweep failed", "error", err)
		}
	}
}

// Installs a persisted cursor for the conversation, true when one existed
func (a *BuzzAdapter) restoreChannelState(channelID, chatType string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	restored, ok := a.cursors.RestoredCursors[channelID]
	if !ok {
		return false
	}
	delete(a.cursors.RestoredCursors, channelID)
	if restored.ChatType != "" {
		chatType = restored.ChatType
	}
	st := newChannelState(chatType)
	st.cursor.LastTS = restored.LastTS
	for _, id := range restored.Seen {
		st.addSeen(id)
	}
	a.watchLocked(channelID, st)
	return true
}

// Starts a conversation's high water mark from its newest events, history feeding parent lookups and DM latching
func (a *BuzzAdapter) seedChannel(ctx context.Context, channelID, chatType string) error {
	if a.restoreChannelState(channelID, chatType) {
		return nil
	}
	st := newChannelState(chatType)
	a.mu.Lock()
	a.watchLocked(channelID, st)
	a.mu.Unlock()
	res, err := a.runCLI(ctx, []string{"messages", "get", "--channel", channelID, "--limit", strconv.Itoa(fetchLimit)}, nil)
	if err != nil {
		return err
	}
	if res.code != 0 {
		pkgLog().Warn(fmt.Sprintf("Buzz: could not seed channel %s — %s", channelID, cliErrorMessage(res.stderr, res.code, "")))
		a.mu.Lock()
		st.cursor.LastTS = time.Now().Unix()
		a.mu.Unlock()
		return nil
	}
	for _, event := range parseJSONList(res.stdout) {
		a.mu.Lock()
		if id := strOf(event["id"]); anyutil.IsTrue(event["id"]) {
			st.addSeen(id)
		}
		st.cursor.LastTS = max(st.cursor.LastTS, intOf(event["created_at"]))
		rememberEventLocked(st, event)
		a.mu.Unlock()
		a.maybeLatchDM(channelID, st, event)
	}
	a.mu.Lock()
	st.trim()
	a.mu.Unlock()
	return nil
}

// Watches DMs from dms list and DM shaped channels list entries, adopting channels joined mid run in watch all mode
func (a *BuzzAdapter) discoverDMs(ctx context.Context, seed bool) error {
	res, err := a.runCLI(ctx, []string{"dms", "list"}, nil)
	if err != nil {
		return err
	}
	if res.code == 0 {
		for _, dm := range parseJSONList(res.stdout) {
			dmID := strOf(dm["dm_id"])
			a.mu.Lock()
			_, watched := a.state[dmID]
			skip := dmID == "" || watched || a.restricted[dmID]
			a.mu.Unlock()
			if skip {
				continue
			}
			if err := a.adoptConversation(ctx, dmID, seed); err != nil {
				return err
			}
			a.mu.Lock()
			if _, ok := a.channelNames[dmID]; !ok {
				a.channelNames[dmID] = "DM"
			}
			a.mu.Unlock()
		}
	}
	res, err = a.runCLI(ctx, []string{"channels", "list"}, nil)
	if err != nil {
		return err
	}
	if res.code != 0 {
		return nil
	}
	for _, ch := range parseJSONList(res.stdout) {
		chID := strOf(ch["channel_id"])
		if chID == "" {
			continue
		}
		a.mu.Lock()
		a.channelMeta[chID] = ch
		if _, ok := a.channelNames[chID]; !ok {
			name := strOf(ch["name"])
			if name == "" {
				name = chID
			}
			a.channelNames[chID] = name
		}
		restricted := a.restricted[chID]
		existing := a.state[chID]
		a.mu.Unlock()
		if restricted {
			continue
		}
		if a.mayReclassifyAsDM(chID) {
			if existing != nil {
				a.mu.Lock()
				existing.cursor.ChatType = "dm"
				a.mu.Unlock()
			} else if err := a.adoptConversation(ctx, chID, seed); err != nil {
				return err
			}
		} else if existing == nil && !seed && len(a.channels) == 0 {
			if err := a.seedChannel(ctx, chID, "group"); err != nil {
				return err
			}
			a.mu.Lock()
			name := a.channelNames[chID]
			a.mu.Unlock()
			pkgLog().Info(fmt.Sprintf("Buzz: adopted newly joined channel %s (%s)", chID, name))
		}
	}
	return nil
}

// Starts watching a DM, seeded at startup and restored or fresh mid run
func (a *BuzzAdapter) adoptConversation(ctx context.Context, channelID string, seed bool) error {
	if seed {
		return a.seedChannel(ctx, channelID, "dm")
	}
	if !a.restoreChannelState(channelID, "dm") {
		a.mu.Lock()
		a.watchLocked(channelID, newChannelState("dm"))
		a.mu.Unlock()
	}
	return nil
}

// Fetches events newer than the high water mark of a conversation and handles them
func (a *BuzzAdapter) pollChannel(ctx context.Context, channelID string) error {
	a.mu.Lock()
	st := a.state[channelID]
	var lastTS int64
	if st != nil {
		lastTS = st.cursor.LastTS
	}
	a.mu.Unlock()
	if st == nil {
		return nil
	}
	args := []string{"messages", "get", "--channel", channelID, "--limit", strconv.Itoa(fetchLimit)}
	if lastTS != 0 {
		args = append(args, "--since", strconv.FormatInt(lastTS, 10))
	}
	res, err := a.runCLI(ctx, args, nil)
	if err != nil {
		return err
	}
	if res.code != 0 {
		pkgLog().Debug(fmt.Sprintf("Buzz: poll of channel %s failed — %s", channelID, cliErrorMessage(res.stderr, res.code, "")))
		return nil
	}
	return a.handleEvents(ctx, channelID, st, parseJSONList(res.stdout))
}

// Handles a batch and persists the cursor once the inputs it returned on are durable, idle channels never rewriting it
func (a *BuzzAdapter) handleEvents(ctx context.Context, channelID string, st *channelState, events []map[string]any) error {
	a.mu.Lock()
	before := CursorMarkOf(st.cursor)
	a.mu.Unlock()
	for _, event := range events {
		if err := a.handleEvent(ctx, channelID, st, event); err != nil {
			return err
		}
	}
	a.mu.Lock()
	st.trim()
	changed := CursorMarkOf(st.cursor) != before
	var writer func()
	if changed {
		writer = a.cursors.CursorWriter(ctx)
	}
	a.mu.Unlock()
	if !changed {
		return nil
	}
	return gateway.WhenReturnedInputsDurable(ctx, a, func(context.Context) error {
		writer()
		return nil
	})
}

// One accepted NIP-94 attachment of an event
type attachmentMeta struct {
	url      string
	sha256   string
	size     int64
	filename string
	mimeType string
}

// Accepted NIP-94 attachment metadata of an event and how many imeta tags were rejected
func parseImetaAttachments(event map[string]any) ([]attachmentMeta, int) {
	tags, ok := event["tags"].([]any)
	if !ok {
		return nil, 0
	}
	var attachments []attachmentMeta
	rejected := 0
	var totalDeclared int64
	for _, raw := range tags {
		tag, ok := raw.([]any)
		if !ok || len(tag) == 0 || strOf(tag[0]) != "imeta" {
			continue
		}
		if len(attachments) >= maxInboundAttachments {
			rejected++
			continue
		}
		fields := map[string]string{}
		for _, f := range tag[1:] {
			s, ok := f.(string)
			if !ok {
				continue
			}
			key, value, found := strings.Cut(s, " ")
			if _, dup := fields[key]; found && !dup {
				fields[key] = strings.TrimSpace(value)
			}
		}
		rawURL, digest := fields["url"], strings.ToLower(fields["x"])
		size, sizeErr := strconv.ParseInt(strings.TrimSpace(fields["size"]), 10, 64)
		parsed, urlErr := url.Parse(rawURL)
		portOK := true
		if urlErr == nil && parsed.Port() != "" {
			if n, err := strconv.Atoi(parsed.Port()); err != nil || n < 0 || n > 65535 {
				portOK = false
			}
		}
		if sizeErr != nil || urlErr != nil || !portOK {
			rejected++
			continue
		}
		if parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" ||
			!hex64RE.MatchString(digest) || size <= 0 || size > maxInboundAttachmentBytes || totalDeclared+size > maxInboundAttachmentBytes {
			rejected++
			continue
		}
		totalDeclared += size
		attachments = append(attachments, attachmentMeta{url: rawURL, sha256: digest, size: size,
			filename: safeAttachmentFilename(fields["filename"]), mimeType: textutil.First(fields["m"], 255)})
	}
	return attachments, rejected
}

// Fixed width diagnostic for malformed or excess attachment metadata
func attachmentRejectionNote(rejected int) string {
	count := strconv.Itoa(rejected)
	if rejected > 999 {
		count = "999+"
	}
	return "[" + count + " Buzz attachment(s) rejected as malformed or over limits.]"
}

// Downloads, integrity checks and caches one authorized attachment, nil on any failure
func (a *BuzzAdapter) downloadAttachment(ctx context.Context, meta attachmentMeta) *gateway.CachedMedia {
	parsed, err := url.Parse(meta.url)
	o := origin{}
	if err == nil {
		o.host = strings.TrimRight(strings.ToLower(parsed.Hostname()), ".")
		o.port = 443
		if p := parsed.Port(); p != "" {
			o.port, _ = strconv.Atoi(p)
		}
	}
	if err != nil || parsed.Scheme != "https" || !a.attachmentOrigins[o] {
		host := o.host
		if host == "" {
			host = "<missing>"
		}
		pkgLog().Warn(fmt.Sprintf("Buzz: refusing attachment from untrusted origin %s:%d", host, o.port))
		return nil
	}
	dlCtx, cancel := context.WithTimeout(ctx, attachmentDownloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, meta.url, nil)
	if err != nil {
		pkgLog().Warn("Buzz: attachment download failed: " + err.Error())
		return nil
	}
	req.Header.Set("Accept-Encoding", "identity")
	client := &http.Client{Transport: netpref.Follow(http.DefaultTransport.(*http.Transport).Clone()),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		pkgLog().Warn("Buzz: attachment download failed: " + err.Error())
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		pkgLog().Warn(fmt.Sprintf("Buzz: attachment download returned HTTP %d", resp.StatusCode))
		return nil
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		declared, err := strconv.ParseInt(strings.TrimSpace(cl), 10, 64)
		if err != nil {
			return nil
		}
		if declared != meta.size {
			pkgLog().Warn("Buzz: attachment Content-Length does not match imeta size")
			return nil
		}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, meta.size+1))
	if err != nil {
		pkgLog().Warn("Buzz: attachment download failed: " + err.Error())
		return nil
	}
	if int64(len(data)) > meta.size {
		pkgLog().Warn("Buzz: attachment exceeded its declared size")
		return nil
	}
	if int64(len(data)) != meta.size {
		pkgLog().Warn("Buzz: attachment size does not match imeta")
		return nil
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != meta.sha256 {
		pkgLog().Warn("Buzz: attachment SHA-256 does not match imeta")
		return nil
	}
	cached, err := gateway.CacheMediaBytesAsync(ctx, data, meta.filename, meta.mimeType, "")
	if err != nil {
		pkgLog().Warn("Buzz: attachment cache write failed: " + err.Error())
		return nil
	}
	return cached
}

// De-duplicates, gates and dispatches one relay event
func (a *BuzzAdapter) handleEvent(ctx context.Context, channelID string, st *channelState, event map[string]any) error {
	eventID := strOf(event["id"])
	createdAt := intOf(event["created_at"])
	a.mu.Lock()
	if eventID == "" || st.seen[eventID] {
		a.mu.Unlock()
		return nil
	}
	st.addSeen(eventID)
	st.cursor.LastTS = max(st.cursor.LastTS, createdAt)
	a.mu.Unlock()
	if !dispatchKinds[intOf(event["kind"])] {
		return nil
	}
	pubkey := strings.ToLower(strOf(event["pubkey"]))
	content, isText := event["content"].(string)
	attachments, rejectedAttachments := parseImetaAttachments(event)
	if pubkey == "" || !isText || (strings.TrimSpace(content) == "" && len(attachments) == 0 && rejectedAttachments == 0) {
		return nil
	}
	a.mu.Lock()
	rememberEventLocked(st, event)
	self := a.selfPubkey
	a.mu.Unlock()
	if pubkey == self {
		return nil
	}
	a.maybeLatchDM(channelID, st, event)
	a.mu.Lock()
	isDM := st.cursor.ChatType == "dm"
	a.mu.Unlock()
	replyParentID := eventReplyParentID(event)
	var replyMeta *eventMeta
	if replyParentID != "" {
		a.mu.Lock()
		if m, ok := st.meta[replyParentID]; ok {
			replyMeta = &m
		}
		a.mu.Unlock()
	}
	replyToIsOwn := replyMeta != nil && replyMeta.author == self
	if !isDM && a.requireMention && !a.isAddressed(event) && !replyToIsOwn {
		return nil
	}
	if len(a.allowedPubkeys) > 0 && !a.allowedPubkeys[pubkey] {
		if a.reactionOnly[pubkey] && pTagged(event, self) && a.isMentioned(content) {
			if _, err := a.SendReaction(ctx, channelID, eventID, "👀"); err != nil {
				return err
			}
		}
		pkgLog().Debug(fmt.Sprintf("Buzz: ignoring message from unauthorized pubkey %s…", textutil.First(pubkey, 8)))
		return nil
	}
	dispatchText := a.stripMention(content)
	threadID := extractThreadRoot(event)
	a.recordThreadRoot(eventID, event)
	chatType := "group"
	if isDM {
		chatType = "dm"
	}
	fetchAllowed := false
	if len(attachments) > 0 {
		auth := a.IsSenderAuthorized(pubkey, &chatType, &channelID, gateway.AuthorizationOpts{})
		fetchAllowed = auth != nil && *auth
	}
	var cached []*gateway.CachedMedia
	if fetchAllowed {
		for _, meta := range attachments {
			if m := a.downloadAttachment(ctx, meta); m != nil {
				cached = append(cached, m)
			}
		}
	}
	if rejectedAttachments > 0 {
		dispatchText = strings.TrimSpace(dispatchText + "\n" + attachmentRejectionNote(rejectedAttachments))
	}
	if failed := len(attachments) - len(cached); fetchAllowed && failed > 0 {
		dispatchText = strings.TrimSpace(fmt.Sprintf("%s\n[%d Buzz attachment(s) could not be downloaded or failed integrity checks.]", dispatchText, failed))
	}
	messageType := gateway.MessageTypeText
	if len(cached) > 0 {
		kinds := map[string]bool{}
		for _, m := range cached {
			kinds[m.Kind] = true
		}
		messageType = gateway.MessageTypeDocument
		if len(kinds) == 1 {
			messageType = attachmentKindType(cached[0].Kind)
		}
	}
	userName, err := a.resolveUserName(ctx, pubkey)
	if err != nil {
		return err
	}
	in := inboundMessage{text: dispatchText, chatID: channelID, chatType: chatType, userID: pubkey, userName: userName,
		messageID: eventID, createdAt: createdAt, threadID: threadID, replyToMessageID: replyParentID,
		replyToIsOwn: replyToIsOwn, messageType: messageType, raw: event}
	if replyMeta != nil {
		in.replyToText, in.replyToAuthorID = &replyMeta.snippet, &replyMeta.author
	}
	for _, m := range cached {
		in.mediaURLs = append(in.mediaURLs, m.Path)
		in.mediaTypes = append(in.mediaTypes, m.MediaType)
	}
	return a.dispatchMessage(ctx, in)
}

// Whether the channel metadata leaves a DM possible, a channel named DM with no description
func (a *BuzzAdapter) mayReclassifyAsDM(channelID string) bool {
	a.mu.Lock()
	meta, ok := a.channelMeta[channelID]
	a.mu.Unlock()
	if !ok {
		return false
	}
	return strings.TrimSpace(strOf(meta["name"])) == "DM" && strings.TrimSpace(strOf(meta["description"])) == ""
}

// Whether a kind 9 event from another user is p tagged to this identity without mentioning it in text
func (a *BuzzAdapter) isDirectMessageEvent(channelID string, event map[string]any) bool {
	a.mu.Lock()
	self := a.selfPubkey
	a.mu.Unlock()
	pubkey := strings.ToLower(strOf(event["pubkey"]))
	content, isText := event["content"].(string)
	return self != "" && a.mayReclassifyAsDM(channelID) && intOf(event["kind"]) == chatKind && pubkey != "" &&
		pubkey != self && pTagged(event, self) && isText && !a.isMentioned(content)
}

// Latches a group conversation to DM once a direct message is seen
func (a *BuzzAdapter) maybeLatchDM(channelID string, st *channelState, event map[string]any) {
	a.mu.Lock()
	already := st.cursor.ChatType == "dm"
	a.mu.Unlock()
	if already || !a.isDirectMessageEvent(channelID, event) {
		return
	}
	a.mu.Lock()
	st.cursor.ChatType = "dm"
	if _, ok := a.channelNames[channelID]; !ok {
		a.channelNames[channelID] = "DM"
	}
	a.mu.Unlock()
	pkgLog().Info(fmt.Sprintf("Buzz: conversation %s reclassified as DM (message p-tagged to self)", channelID))
}

// Whether the text addresses this agent by npub, hex key or @name
func (a *BuzzAdapter) isMentioned(content string) bool {
	a.mu.Lock()
	self, npub, display := a.selfPubkey, a.selfNpub, a.displayName
	a.mu.Unlock()
	lowered := strings.ToLower(content)
	var patterns []string
	if self != "" && hex64RE.MatchString(self) {
		patterns = append(patterns, `(?<![0-9a-f])`+regexp2.Escape(self)+`(?![0-9a-f])`)
	}
	if npub != "" {
		patterns = append(patterns, `(?<![a-z0-9])`+regexp2.Escape(strings.ToLower(npub))+`(?![a-z0-9])`)
	}
	if display != "" {
		patterns = append(patterns, `(?<![\w@])@`+regexp2.Escape(strings.ToLower(display))+`(?=$|[\s,;.!?:)\]}])`)
	}
	for _, p := range patterns {
		if re2Match(mustRE2(p, regexp2.None), lowered) {
			return true
		}
	}
	return false
}

// Whether a group event carries an explicit text or p tag address
func (a *BuzzAdapter) isAddressed(event map[string]any) bool {
	content, ok := event["content"].(string)
	if !ok {
		return false
	}
	a.mu.Lock()
	self := a.selfPubkey
	a.mu.Unlock()
	return a.isMentioned(content) || (self != "" && pTagged(event, self))
}

// Text without a leading mention of this agent so a command after it is recognized
func (a *BuzzAdapter) stripMention(content string) string {
	text := strings.TrimSpace(content)
	a.mu.Lock()
	self, npub, display := a.selfPubkey, a.selfNpub, a.displayName
	a.mu.Unlock()
	var candidates []string
	if display != "" {
		candidates = append(candidates, "@"+regexp2.Escape(display)+`(?=$|[\s,;.!?:)\]}])`)
	}
	if npub != "" {
		candidates = append(candidates, "@?"+regexp2.Escape(npub)+`(?![a-z0-9])`)
	}
	if self != "" {
		candidates = append(candidates, "@?"+regexp2.Escape(self)+`(?![0-9a-f])`)
	}
	if len(candidates) == 0 {
		return text
	}
	re := mustRE2(`^(?:`+strings.Join(candidates, "|")+`)[\s:,]*`, regexp2.IgnoreCase)
	out, err := re.Replace(text, "", -1, 1)
	if err != nil {
		return text
	}
	return strings.TrimSpace(out)
}

// Display name of a public key, cached negatively too, an npub prefix standing in
func (a *BuzzAdapter) resolveUserName(ctx context.Context, pubkey string) (string, error) {
	a.mu.Lock()
	cached, ok := a.userNames[pubkey]
	a.mu.Unlock()
	if ok {
		return cached, nil
	}
	res, err := a.runCLI(ctx, []string{"users", "get", "--pubkey", pubkey}, nil)
	if err != nil {
		return "", err
	}
	name := ""
	if res.code == 0 {
		if profiles := parseJSONList(res.stdout); len(profiles) > 0 {
			name = strings.TrimSpace(strOf(profiles[0]["display_name"]))
		}
	}
	if name == "" {
		fallback := hexToNpub(pubkey)
		if fallback == "" {
			fallback = pubkey
		}
		name = textutil.First(fallback, 16)
	}
	a.mu.Lock()
	a.userNames[pubkey] = name
	a.mu.Unlock()
	return name, nil
}

// Same relay media references localized into the cache when the sender is explicitly authorized
func (a *BuzzAdapter) localizeInboundMedia(ctx context.Context, text, messageID, userID, chatType, chatID string) (string, []string, []string, gateway.MessageType, error) {
	urls, replacements := findRelayMediaRefs(text, a.relayURL)
	if len(urls) == 0 {
		return text, nil, nil, gateway.MessageTypeText, nil
	}
	if auth := a.IsSenderAuthorized(userID, &chatType, &chatID, gateway.AuthorizationOpts{}); auth == nil || !*auth {
		who := userID
		if who == "" {
			who = "?"
		}
		pkgLog().Warn(fmt.Sprintf("Buzz: not localizing %d media reference(s) in message %s — sender %s… is not explicitly authorized",
			len(urls), textutil.First(messageID, 12), textutil.First(who, 8)))
		return text, nil, nil, gateway.MessageTypeText, nil
	}
	cleaned := replaceMediaRefs(text, replacements)
	var mediaURLs, mediaTypes []string
	kinds := map[string]bool{}
	for _, u := range urls {
		parsed, err := url.Parse(u)
		if err != nil {
			continue
		}
		m := mediaPathRE.FindStringSubmatch(parsed.Path)
		if m == nil {
			continue
		}
		ext := strings.ToLower(m[2])
		if ext == "" {
			ext = ".bin"
		}
		label := m[1][:12] + ext
		cached, err := a.fetchRelayMedia(ctx, u, label)
		if err != nil {
			if ctx.Err() != nil {
				return "", nil, nil, "", ctx.Err()
			}
			pkgLog().Warn(fmt.Sprintf("Buzz: failed to localize inbound media %s (%s)", label, anyutil.Kind(err)))
			continue
		}
		if cached == nil {
			pkgLog().Warn("Buzz: rejected invalid inbound media " + label)
			continue
		}
		mediaURLs = append(mediaURLs, cached.Path)
		mediaTypes = append(mediaTypes, cached.MediaType)
		kinds[cached.Kind] = true
	}
	if len(mediaURLs) > 0 {
		pkgLog().Info(fmt.Sprintf("Buzz: localized %d inbound media attachment(s) for message %s", len(mediaURLs), textutil.First(messageID, 12)))
	}
	messageType := gateway.MessageTypeText
	if len(kinds) > 0 {
		messageType = gateway.MessageTypeDocument
		for _, pair := range []struct {
			kind string
			typ  gateway.MessageType
		}{{"image", gateway.MessageTypePhoto}, {"audio", gateway.MessageTypeAudio}, {"video", gateway.MessageTypeVideo}} {
			if kinds[pair.kind] {
				messageType = pair.typ
				break
			}
		}
	}
	if cleaned == "" {
		cleaned = "(Buzz media attachment unavailable)"
		if len(mediaURLs) > 0 {
			cleaned = "(attachment)"
		}
	}
	return cleaned, mediaURLs, mediaTypes, messageType, nil
}

// Downloads one relay media object through the authenticated CLI into a scratch dir and caches it
func (a *BuzzAdapter) fetchRelayMedia(ctx context.Context, rawURL, label string) (*gateway.CachedMedia, error) {
	tempDir, err := os.MkdirTemp("", "icebath-buzz-media-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tempDir)
	downloadPath := filepath.Join(tempDir, "buzz_"+label)
	res, err := a.runCLI(ctx, []string{"media", "get", "-o", downloadPath, rawURL}, nil)
	if err != nil {
		return nil, err
	}
	st, statErr := os.Stat(downloadPath)
	if res.code != 0 || statErr != nil || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("buzz media get exited %d", res.code)
	}
	if err := mediacache.ValidateInboundMediaSize(ctx, home.Dir(ctx), st.Size(), "Buzz media", nil); err != nil {
		return nil, err
	}
	mimeType := mimeutil.TypeByName(filepath.Base(downloadPath))
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	data, err := os.ReadFile(downloadPath)
	if err != nil {
		return nil, err
	}
	return gateway.CacheMediaBytesAsync(ctx, data, filepath.Base(downloadPath), mimeType, "")
}

// Fields of one inbound message handed to the gateway
type inboundMessage struct {
	text             string
	chatID           string
	chatType         string
	userID           string
	userName         string
	messageID        string
	createdAt        int64
	threadID         string
	replyToMessageID string
	replyToText      *string
	replyToAuthorID  *string
	replyToIsOwn     bool
	mediaURLs        []string
	mediaTypes       []string
	messageType      gateway.MessageType
	raw              any
}

// Builds the message event with localized relay media and hands it to the gateway, then acknowledges with a reaction
func (a *BuzzAdapter) dispatchMessage(ctx context.Context, in inboundMessage) error {
	if a.MessageHandler() == nil {
		return nil
	}
	text, localizedURLs, localizedTypes, localizedType, err := a.localizeInboundMedia(ctx, in.text, in.messageID, in.userID, in.chatType, in.chatID)
	if err != nil {
		return err
	}
	mediaURLs, mediaTypes := append([]string(nil), in.mediaURLs...), append([]string(nil), in.mediaTypes...)
	for i, path := range localizedURLs {
		dup := false
		for _, have := range mediaURLs {
			dup = dup || have == path
		}
		if !dup {
			mediaURLs = append(mediaURLs, path)
			mediaTypes = append(mediaTypes, localizedTypes[i])
		}
	}
	messageType := in.messageType
	if messageType == gateway.MessageTypeText {
		messageType = localizedType
	} else if len(localizedURLs) > 0 && localizedType != messageType && localizedType != gateway.MessageTypeText {
		messageType = gateway.MessageTypeDocument
	}
	a.mu.Lock()
	chatName, ok := a.channelNames[in.chatID]
	a.mu.Unlock()
	if !ok {
		chatName = in.chatID
	}
	fields := gateway.SourceFields{ChatName: &chatName, ChatType: in.chatType, UserID: &in.userID, UserName: &in.userName, MessageID: &in.messageID}
	if in.threadID != "" {
		fields.ThreadID = &in.threadID
	}
	source := a.BuildSource(ctx, in.chatID, fields)
	event := gateway.NewMessageEvent(text)
	event.MessageType, event.Source, event.RawMessage = messageType, source, in.raw
	event.MessageID = &in.messageID
	event.MediaURLs, event.MediaTypes = mediaURLs, mediaTypes
	event.MediaTextInlined = make([]*bool, len(mediaURLs))
	for i := range event.MediaTextInlined {
		f := false
		event.MediaTextInlined[i] = &f
	}
	event.Timestamp = time.Now()
	if in.createdAt != 0 {
		event.Timestamp = time.Unix(in.createdAt, 0)
	}
	if in.replyToMessageID != "" {
		event.ReplyToMessageID = &in.replyToMessageID
	}
	event.ReplyToText, event.ReplyToAuthorID, event.ReplyToIsOwnMessage = in.replyToText, in.replyToAuthorID, in.replyToIsOwn
	if err := a.HandleMessage(ctx, event); err != nil {
		return err
	}
	if _, err := a.SendReaction(ctx, in.chatID, in.messageID, "👀"); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		pkgLog().Debug(fmt.Sprintf("Buzz: reaction failed for message %s", textutil.First(in.messageID, 12)), "error", err)
	}
	return nil
}
