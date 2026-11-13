package simplex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	corrPrefix       = "icebath-"
	dialTimeout      = 10 * time.Second
	pingInterval     = 20 * time.Second
	pingTimeout      = 20 * time.Second
	writeTimeout     = 30 * time.Second
	closeTimeout     = 10 * time.Second
	commandTimeout   = 30 * time.Second
	directoryTimeout = 10 * time.Second
)

var (
	errNotConnected = errors.New("SimpleX daemon not connected")
	errClosed       = errors.New("SimpleX daemon connection closed")
	corrCounter     atomic.Uint64
)

// Correlation id marking a command this process sent
func newCorrID() string {
	return fmt.Sprintf("%s%d-%d", corrPrefix, corrCounter.Add(1), time.Now().UnixMilli())
}

// Daemon error reply carrying the chatError the daemon returned
type daemonError struct {
	command string
	detail  string
	kind    string
}

func (e *daemonError) Error() string { return "simplex-chat rejected " + e.command + ": " + e.detail }

// One WebSocket connection to simplex-chat with replies matched to commands by corrId
type wsClient struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
	touch   func()

	mu      sync.Mutex
	waiters map[string]chan map[string]any
	closed  chan struct{}
	once    sync.Once
	readEnd chan struct{}
}

// Opens a connection to the daemon within the dial timeout
func dialClient(ctx context.Context, rawURL string) (*wsClient, error) {
	dialer := websocket.Dialer{Proxy: http.ProxyFromEnvironment, HandshakeTimeout: dialTimeout}
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	conn, resp, err := dialer.DialContext(dctx, rawURL, nil)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	c := &wsClient{conn: conn, waiters: map[string]chan map[string]any{}, closed: make(chan struct{}), readEnd: make(chan struct{})}
	conn.SetPongHandler(func(string) error {
		c.markActive()
		return conn.SetReadDeadline(time.Now().Add(pingInterval + pingTimeout))
	})
	return c, nil
}

// Records inbound traffic for the idle monitor
func (c *wsClient) markActive() {
	if c.touch != nil {
		c.touch()
	}
}

// Reads frames until the connection fails, resolving replies and handing every other frame to onFrame
func (c *wsClient) readLoop(onFrame func(map[string]any)) error {
	defer close(c.readEnd)
	go c.pingLoop()
	for {
		if err := c.conn.SetReadDeadline(time.Now().Add(pingInterval + pingTimeout)); err != nil {
			c.shutdown()
			return err
		}
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			c.shutdown()
			return err
		}
		c.markActive()
		var frame map[string]any
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if err := dec.Decode(&frame); err != nil {
			slog.Debug(fmt.Sprintf("[simplex] invalid JSON from daemon: %.100s", data))
			continue
		}
		if c.resolve(frame) {
			continue
		}
		if onFrame != nil {
			onFrame(frame)
		}
	}
}

// Sends protocol pings so a dead daemon connection is noticed within the ping timeout
func (c *wsClient) pingLoop() {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.closed:
			return
		case <-ticker.C:
			if err := c.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(pingTimeout)); err != nil {
				return
			}
		}
	}
}

// Delivers a reply to its waiting command, true for replies and for echoes of commands nobody awaits
func (c *wsClient) resolve(frame map[string]any) bool {
	corrID, _ := frame["corrId"].(string)
	if corrID == "" {
		return false
	}
	c.mu.Lock()
	ch, ok := c.waiters[corrID]
	delete(c.waiters, corrID)
	c.mu.Unlock()
	if ok {
		ch <- respOf(frame)
		return true
	}
	return strings.HasPrefix(corrID, corrPrefix)
}

// Marks the connection closed and wakes every waiting command
func (c *wsClient) shutdown() {
	c.once.Do(func() {
		close(c.closed)
		c.conn.Close()
	})
}

// Sends a close frame, waits for the read loop to see it and drops the socket
func (c *wsClient) close() {
	select {
	case <-c.closed:
		return
	default:
	}
	_ = c.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(closeTimeout))
	select {
	case <-c.readEnd:
	case <-time.After(closeTimeout):
	}
	c.shutdown()
}

// Writes one command frame under the writer lock
func (c *wsClient) write(corrID, cmd string) error {
	payload, err := marshal(map[string]any{"corrId": corrID, "cmd": cmd})
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	return c.conn.WriteMessage(websocket.TextMessage, payload)
}

// Sends a command and waits for its reply, a daemon error reply returned as a daemonError
func (c *wsClient) command(ctx context.Context, cmd string, timeout time.Duration) (map[string]any, error) {
	corrID := newCorrID()
	ch := make(chan map[string]any, 1)
	c.mu.Lock()
	c.waiters[corrID] = ch
	c.mu.Unlock()
	forget := func() {
		c.mu.Lock()
		delete(c.waiters, corrID)
		c.mu.Unlock()
	}
	if err := c.write(corrID, cmd); err != nil {
		forget()
		return nil, err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case resp := <-ch:
		if err := replyError(commandName(cmd), resp); err != nil {
			return resp, err
		}
		return resp, nil
	case <-c.closed:
		forget()
		return nil, errClosed
	case <-timer.C:
		forget()
		return nil, fmt.Errorf("simplex-chat did not answer %s within %s", commandName(cmd), timeout)
	case <-ctx.Done():
		forget()
		return nil, ctx.Err()
	}
}

// Response body of a frame, the resp object or the frame itself for daemons that inline it
func respOf(frame map[string]any) map[string]any {
	if resp, ok := frame["resp"].(map[string]any); ok {
		return resp
	}
	return frame
}

// First word of a command for messages
func commandName(cmd string) string {
	name, _, _ := strings.Cut(cmd, " ")
	return name
}

// Daemon error of a reply, nil for a successful response
func replyError(name string, resp map[string]any) error {
	kind, _ := resp["type"].(string)
	chatErr, hasErr := resp["chatError"]
	if kind != "chatCmdError" && kind != "chatError" && !hasErr {
		return nil
	}
	detail, _ := marshal(chatErr)
	text := string(detail)
	if len(text) > 300 {
		text = text[:300]
	}
	return &daemonError{command: name, detail: text, kind: chatErrorKind(chatErr)}
}

// Innermost error type tag of a chatError value
func chatErrorKind(v any) string {
	m, _ := v.(map[string]any)
	kind, _ := m["type"].(string)
	for _, key := range []string{"errorType", "agentError", "storeError"} {
		if inner, ok := m[key].(map[string]any); ok {
			if t, _ := inner["type"].(string); t != "" {
				return t
			}
		}
	}
	return kind
}

// JSON without HTML escaping, the form the daemon command parser reads
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Object under key, empty when absent or another type
func obj(m map[string]any, key string) map[string]any {
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return map[string]any{}
}

// String under key, empty when absent or another type
func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// Decimal text of a numeric id value, empty when the value is not a whole number
func idText(v any) string {
	switch n := v.(type) {
	case json.Number:
		if _, err := strconv.ParseInt(n.String(), 10, 64); err == nil {
			return n.String()
		}
	case string:
		if isDigits(n) {
			return n
		}
	case float64:
		if n == float64(int64(n)) {
			return strconv.FormatInt(int64(n), 10)
		}
	case int:
		return strconv.Itoa(n)
	case int64:
		return strconv.FormatInt(n, 10)
	}
	return ""
}

// Whether s is a non empty run of ASCII digits
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Local display name, the profile display name when the local one is blank
func displayName(m map[string]any, profileKey string) string {
	if name := str(m, "localDisplayName"); name != "" {
		return name
	}
	return str(obj(m, profileKey), "displayName")
}

// Contact and group entries of the daemon's listings for the active user
func listContacts(ctx context.Context, c *wsClient, timeout time.Duration) ([]map[string]any, error) {
	resp, err := c.command(ctx, "/contacts", timeout)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	list, _ := resp["contacts"].([]any)
	for _, item := range list {
		if contact, ok := item.(map[string]any); ok {
			out = append(out, contact)
		}
	}
	return out, nil
}

// Group infos of the daemon's group listing, pairs of info and summary flattened to the info
func listGroups(ctx context.Context, c *wsClient, timeout time.Duration) ([]map[string]any, error) {
	resp, err := c.command(ctx, "/groups", timeout)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	list, _ := resp["groups"].([]any)
	for _, item := range list {
		if pair, ok := item.([]any); ok && len(pair) > 0 {
			item = pair[0]
		}
		if group, ok := item.(map[string]any); ok && idText(group["groupId"]) != "" {
			out = append(out, group)
		}
	}
	return out, nil
}

// Daemon chat reference of a target, numeric ids used as is and display names resolved to their unique id
func chatRef(ctx context.Context, c *wsClient, target string) (string, error) {
	target = strings.TrimSpace(target)
	if rest, isGroup := strings.CutPrefix(target, "group:"); isGroup {
		if isDigits(rest) {
			return "#" + rest, nil
		}
		groups, err := listGroups(ctx, c, directoryTimeout)
		if err != nil {
			return "", err
		}
		id, err := uniqueByName(groups, rest, "groupId", "groupProfile", "group")
		if err != nil {
			return "", err
		}
		return "#" + id, nil
	}
	if isDigits(target) {
		return "@" + target, nil
	}
	if target == "" {
		return "", errors.New("SimpleX send needs a contact id or group:<id> target")
	}
	contacts, err := listContacts(ctx, c, directoryTimeout)
	if err != nil {
		return "", err
	}
	id, err := uniqueByName(contacts, target, "contactId", "profile", "contact")
	if err != nil {
		return "", err
	}
	return "@" + id, nil
}

// Id of the single entry whose display name equals name
func uniqueByName(entries []map[string]any, name, idKey, profileKey, kind string) (string, error) {
	var ids []string
	for _, e := range entries {
		if displayName(e, profileKey) == name {
			if id := idText(e[idKey]); id != "" {
				ids = append(ids, id)
			}
		}
	}
	switch len(ids) {
	case 0:
		return "", fmt.Errorf("no SimpleX %s named %q", kind, name)
	case 1:
		return ids[0], nil
	}
	return "", fmt.Errorf("SimpleX %s name %q matches ids %s, address it by id", kind, name, strings.Join(ids, ", "))
}
