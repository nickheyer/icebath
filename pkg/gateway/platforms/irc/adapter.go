package irc

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
)

const (
	defaultMaxMessageLength = 450
	connectTimeout          = 30 * time.Second
	registrationTimeout     = 30 * time.Second
	writeTimeout            = 30 * time.Second
	idleTimeout             = 240 * time.Second
	pingTimeout             = 60 * time.Second
	floodDelay              = 300 * time.Millisecond
	nickservDelay           = 2 * time.Second
	quitGrace               = 500 * time.Millisecond
	maxBufferedLine         = 64 * 1024
)

// Platform value of IRC in the gateway registry
const platformIRC = gateway.Platform("irc")

// Registration refusal that no retry can fix
type regFailure struct{ code, message string }

// One live server connection with its reader, registration state and serialised writer
type session struct {
	conn       net.Conn
	cancel     context.CancelFunc
	done       chan struct{}
	registered chan struct{}
	failed     chan regFailure
	regOnce    sync.Once
	writeMu    sync.Mutex
	sendMu     sync.Mutex
	mu         sync.Mutex
	nick       string
	welcomed   bool
	closed     atomic.Bool
}

// Writes one protocol line terminated by CRLF
func (s *session) write(line string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err := s.conn.Write([]byte(line + "\r\n"))
	return err
}

// Nick the server currently knows the bot by
func (s *session) currentNick() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nick
}

func (s *session) setNick(nick string) {
	s.mu.Lock()
	s.nick = nick
	s.mu.Unlock()
}

// Whether the server sent its welcome
func (s *session) isWelcomed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.welcomed
}

// Records the welcome and wakes the connect call
func (s *session) markWelcomed() {
	s.mu.Lock()
	s.welcomed = true
	s.mu.Unlock()
	s.regOnce.Do(func() { close(s.registered) })
}

// Reports a registration refusal to the connect call once
func (s *session) fail(f regFailure) {
	select {
	case s.failed <- f:
	default:
	}
}

// Adapter relays an IRC channel and direct messages over a plain or TLS client connection
type Adapter struct {
	*gateway.BasePlatformAdapter

	cfg     settings
	allowed map[string]bool
	maxLen  int

	mu   sync.Mutex
	sess *session
}

// Adapter traits of IRC, no edits and a serial line reader
func adapterTraits() gateway.AdapterTraits {
	t := gateway.DefaultAdapterTraits()
	t.SerialReceiveLoop = true
	t.SplitsLongMessages = true
	t.SupportsMessageEditing = false
	t.MaxMessageLength = defaultMaxMessageLength
	return t
}

// Adapter reading server, identity, auth and access settings from the scoped env over extra
func NewAdapter(ctx context.Context, config *gateway.PlatformConfig) (*Adapter, error) {
	if config == nil {
		config = gateway.NewPlatformConfig()
	}
	a := &Adapter{allowed: map[string]bool{}}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, platformIRC, a)
	a.Traits = adapterTraits()
	cfg, err := loadSettings(ctx, config.Extra)
	if err != nil {
		return nil, err
	}
	if cfg.port == 0 {
		cfg.port = defaultPort(cfg.useTLS)
	}
	a.cfg = cfg
	for nick := range gateway.CoerceAllowSet(config.Extra["allowed_users"]) {
		a.allowed[strings.ToLower(nick)] = true
	}
	a.maxLen = defaultMaxMessageLength
	if n, ok := intOf(config.Extra["max_message_length"]); ok && n > 0 {
		a.maxLen = n
	} else if entry := gateway.Registry.Get(ctx, string(platformIRC)); entry != nil && entry.MaxMessageLength > 0 {
		a.maxLen = entry.MaxMessageLength
	}
	a.Traits.MaxMessageLength = a.maxLen
	return a, nil
}

// Display name of the platform
func (a *Adapter) Name() string { return "IRC" }

// Message length in bytes, the unit IRC lines are capped in
func (a *Adapter) MessageLenFn() func(string) int { return func(s string) int { return len(s) } }

// Live session, nil while disconnected
func (a *Adapter) currentSession() *session {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sess
}

// Plain or TLS connection to the server bounded by the timeout
func dial(ctx context.Context, server string, port int, useTLS bool, timeout time.Duration) (net.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	addr := net.JoinHostPort(server, strconv.Itoa(port))
	netDialer := &net.Dialer{Timeout: timeout, KeepAlive: 60 * time.Second}
	if !useTLS {
		return netDialer.DialContext(dctx, "tcp", addr)
	}
	tlsDialer := &tls.Dialer{NetDialer: netDialer, Config: &tls.Config{ServerName: server, MinVersion: tls.VersionTLS12}}
	return tlsDialer.DialContext(dctx, "tcp", addr)
}

// Waits the duration, false when the context ends first
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// Fails a connect attempt, closing the session, releasing the identity lock and recording the fatal error
func (a *Adapter) failConnect(ctx context.Context, code, message string, retryable bool) (bool, error) {
	a.closeSession(ctx, false)
	a.ReleasePlatformLock()
	a.SetFatalError(ctx, code, message, retryable)
	return false, nil
}

// Connects, registers with PASS, NICK and USER, identifies with NickServ and joins the channel
func (a *Adapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	c := a.cfg
	if c.server == "" || c.channel == "" {
		slog.Error("IRC: server and channel must be configured")
		a.SetFatalError(ctx, "config_missing", "IRC_SERVER and IRC_CHANNEL must be set", false)
		return false, nil
	}
	a.closeSession(ctx, false)
	a.ReleasePlatformLock()
	locked, err := a.AcquirePlatformLock(ctx, "irc", c.server+":"+c.nickname, fmt.Sprintf("IRC identity %s@%s", c.nickname, c.server))
	if err != nil || !locked {
		return false, err
	}
	conn, err := dial(ctx, c.server, c.port, c.useTLS, connectTimeout)
	if err != nil {
		if ctx.Err() != nil {
			a.ReleasePlatformLock()
			return false, ctx.Err()
		}
		slog.Error(fmt.Sprintf("IRC: failed to connect to %s:%d — %v", c.server, c.port, err))
		return a.failConnect(ctx, "connect_failed", err.Error(), true)
	}
	s := &session{conn: conn, done: make(chan struct{}), registered: make(chan struct{}), failed: make(chan regFailure, 1), nick: c.nickname}
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.cancel = cancel
	a.mu.Lock()
	a.sess = s
	a.mu.Unlock()
	go func() {
		defer close(s.done)
		a.receiveLoop(sctx, s)
	}()
	var lines []string
	if c.serverPassword != "" {
		lines = append(lines, "PASS "+stripControlChars(c.serverPassword))
	}
	lines = append(lines, "NICK "+c.nickname, "USER "+c.nickname+" 0 * :Icebath Agent")
	for _, line := range lines {
		if err := s.write(line); err != nil {
			slog.Error(fmt.Sprintf("IRC: failed to register with %s:%d — %v", c.server, c.port, err))
			return a.failConnect(ctx, "connect_failed", err.Error(), true)
		}
	}
	refused := func(f regFailure) (bool, error) {
		slog.Error("IRC: " + f.message)
		return a.failConnect(ctx, f.code, f.message, false)
	}
	timer := time.NewTimer(registrationTimeout)
	defer timer.Stop()
	select {
	case <-s.registered:
	case f := <-s.failed:
		return refused(f)
	case <-s.done:
		select {
		case f := <-s.failed:
			return refused(f)
		default:
		}
		slog.Error("IRC: server closed the connection during registration")
		return a.failConnect(ctx, "connection_lost", "IRC server closed the connection during registration", true)
	case <-timer.C:
		slog.Error("IRC: registration timed out")
		_ = a.Disconnect(ctx)
		a.SetFatalError(ctx, "registration_timeout", "IRC server did not send RPL_WELCOME", true)
		return false, nil
	case <-ctx.Done():
		a.closeSession(ctx, false)
		a.ReleasePlatformLock()
		return false, ctx.Err()
	}
	if c.nickservPassword != "" {
		if err := s.write("PRIVMSG NickServ :IDENTIFY " + stripControlChars(c.nickservPassword)); err != nil {
			return a.failConnect(ctx, "connection_lost", err.Error(), true)
		}
		if !sleepCtx(ctx, nickservDelay) {
			a.closeSession(ctx, false)
			a.ReleasePlatformLock()
			return false, ctx.Err()
		}
	}
	if err := s.write("JOIN " + stripControlChars(c.channel)); err != nil {
		return a.failConnect(ctx, "connection_lost", err.Error(), true)
	}
	a.MarkConnected(ctx, "")
	slog.Info(fmt.Sprintf("IRC: connected to %s:%d as %s, joined %s", c.server, c.port, s.currentNick(), c.channel))
	a.WirePluginHandlers(ctx, nil)
	return true, nil
}

// Ends the session, sending QUIT first when asked and the link is still up, and waits for its reader
func (a *Adapter) closeSession(ctx context.Context, quit bool) {
	a.mu.Lock()
	s := a.sess
	a.sess = nil
	a.mu.Unlock()
	if s == nil {
		return
	}
	if quit && !s.closed.Load() && s.write("QUIT :"+i18n.T(ctx, "platform.irc.quit_message", "", nil)) == nil {
		sleepCtx(ctx, quitGrace)
	}
	s.closed.Store(true)
	s.cancel()
	_ = s.conn.Close()
	<-s.done
}

// Releases the identity lock, marks the adapter disconnected and quits the server
func (a *Adapter) Disconnect(ctx context.Context) error {
	a.ReleasePlatformLock()
	a.MarkDisconnected(ctx)
	a.closeSession(ctx, true)
	return nil
}

// Reads protocol lines until the link drops, then reports a lost connection as a retryable fatal error
func (a *Adapter) receiveLoop(ctx context.Context, s *session) {
	err := a.readLines(ctx, s)
	s.closed.Store(true)
	if ctx.Err() != nil {
		return
	}
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		slog.Error(fmt.Sprintf("IRC: receive loop error: %v", err))
	}
	if a.IsConnected() && a.currentSession() == s {
		slog.Warn("IRC: connection lost, marking disconnected")
		a.SetFatalError(ctx, "connection_lost", "IRC connection closed unexpectedly", true)
		_ = a.NotifyFatalError(ctx)
	}
}

// Feeds every complete line to the dispatcher, pinging an idle server and giving up when it stays silent
func (a *Adapter) readLines(ctx context.Context, s *session) error {
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)
	pinged := false
	for {
		wait := idleTimeout
		if pinged {
			wait = pingTimeout
		}
		_ = s.conn.SetReadDeadline(time.Now().Add(wait))
		n, err := s.conn.Read(chunk)
		if n > 0 {
			pinged = false
			buf = append(buf, chunk[:n]...)
			for {
				i := bytes.IndexByte(buf, '\n')
				if i < 0 {
					break
				}
				line := strings.ToValidUTF8(strings.TrimRight(string(buf[:i]), "\r"), "�")
				buf = append(buf[:0], buf[i+1:]...)
				if line != "" {
					a.handleLine(ctx, s, line)
				}
			}
			if len(buf) > maxBufferedLine {
				slog.Warn(fmt.Sprintf("IRC: dropping %d bytes without a line break", len(buf)))
				buf = buf[:0]
			}
		}
		if err == nil {
			continue
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() && ctx.Err() == nil {
			if pinged {
				return fmt.Errorf("no reply from %s within %s", a.cfg.server, idleTimeout+pingTimeout)
			}
			if werr := s.write("PING :" + a.cfg.server); werr != nil {
				return werr
			}
			pinged = true
			continue
		}
		return err
	}
}

// Last parameter of a message, empty without parameters
func lastParam(m message) string {
	if len(m.params) == 0 {
		return ""
	}
	return m.params[len(m.params)-1]
}

// Dispatches one protocol line, answering pings, tracking the nick and registration and routing PRIVMSG
func (a *Adapter) handleLine(ctx context.Context, s *session, raw string) {
	m := parseMessage(raw)
	var err error
	switch m.command {
	case "PING":
		first := ""
		if len(m.params) > 0 {
			first = m.params[0]
		}
		err = s.write("PONG :" + first)
	case "001":
		if len(m.params) > 0 {
			s.setNick(m.params[0])
		}
		s.markWelcomed()
	case "433":
		next := nextNick(a.cfg.nickname, s.currentNick())
		s.setNick(next)
		err = s.write("NICK " + next)
	case "432":
		if !s.isWelcomed() {
			s.fail(regFailure{"nickname_rejected", fmt.Sprintf("IRC server rejected nickname %s (432 erroneous nickname). Check IRC_NICKNAME.", s.currentNick())})
		}
	case "464":
		if !s.isWelcomed() {
			s.fail(regFailure{"server_password_rejected", "IRC server rejected the server password (464). Check IRC_SERVER_PASSWORD."})
		}
	case "465":
		if !s.isWelcomed() {
			s.fail(regFailure{"banned", "IRC server refused the connection (465): " + lastParam(m)})
		}
	case "403", "405", "471", "473", "474", "475":
		target := ""
		if len(m.params) > 1 {
			target = m.params[1]
		}
		slog.Warn(fmt.Sprintf("IRC: cannot join %s (%s): %s", target, m.command, lastParam(m)))
	case "KICK":
		if len(m.params) > 1 && strings.EqualFold(m.params[1], s.currentNick()) {
			slog.Warn(fmt.Sprintf("IRC: kicked from %s by %s: %s", m.params[0], extractNick(m.prefix), lastParam(m)))
		}
	case "ERROR":
		slog.Warn("IRC: server error: " + lastParam(m))
	case "NICK":
		if len(m.params) > 0 && strings.EqualFold(extractNick(m.prefix), s.currentNick()) {
			s.setNick(m.params[0])
		}
	case "PRIVMSG":
		if len(m.params) >= 2 {
			a.handlePrivmsg(ctx, s, extractNick(m.prefix), m.params[0], m.params[1])
		}
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("IRC: error handling line: %v", err))
	}
}

// Turns a PRIVMSG into a message event, channels only when addressed by nick and senders gated by the allowlist
func (a *Adapter) handlePrivmsg(ctx context.Context, s *session, sender, target, text string) {
	nick := s.currentNick()
	if strings.EqualFold(sender, nick) {
		return
	}
	if body, ok := strings.CutPrefix(text, "\x01ACTION "); ok {
		text = "* " + sender + " " + strings.TrimSuffix(body, "\x01")
	}
	if strings.HasPrefix(text, "\x01") {
		return
	}
	text = stripFormatting(text)
	channel := isChannel(target)
	if channel {
		addressed := false
		for _, prefix := range []string{nick + ":", nick + ",", nick + " "} {
			if len(text) >= len(prefix) && strings.EqualFold(text[:len(prefix)], prefix) {
				text, addressed = strings.TrimSpace(text[len(prefix):]), true
				break
			}
		}
		if !addressed {
			return
		}
	}
	if len(a.allowed) > 0 && !a.allowed[strings.ToLower(sender)] {
		slog.Debug("IRC: ignoring message from unauthorized user " + sender)
		return
	}
	if strings.TrimSpace(text) == "" {
		return
	}
	chatID, chatType := sender, "dm"
	if channel {
		chatID, chatType = target, "group"
	}
	id := randomID()
	source := a.BuildSource(ctx, chatID, gateway.SourceFields{ChatName: &chatID, ChatType: chatType, UserID: &sender, UserName: &sender, MessageID: &id})
	msg := gateway.NewMessageEvent(text)
	msg.Source, msg.MessageID = source, &id
	if err := a.HandleMessage(ctx, msg); err != nil && ctx.Err() == nil {
		slog.Warn(fmt.Sprintf("IRC: message handling failed: %v", err))
	}
}

// Sends the content as PRIVMSG lines split to the line budget with flood spacing
func (a *Adapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	s := a.currentSession()
	if s == nil || s.closed.Load() {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	if !validTarget(chatID) {
		return gateway.SendResult{Success: false, Error: "chat_id contains illegal IRC characters"}, nil
	}
	lines := splitLines(content, chatID, a.maxLen)
	if len(lines) == 0 {
		return gateway.SendResult{Success: true}, nil
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	for i, line := range lines {
		if err := s.write("PRIVMSG " + chatID + " :" + line); err != nil {
			result := gateway.SendResult{Success: false, Error: err.Error(), Retryable: true}
			if i > 0 {
				result.RawResponse = map[string]any{"partial_overflow": true, "delivered_chunks": i, "total_chunks": len(lines)}
			}
			return result, nil
		}
		if !sleepCtx(ctx, floodDelay) {
			return gateway.SendResult{}, ctx.Err()
		}
	}
	return gateway.SendResult{Success: true, MessageID: msID()}, nil
}

// Group for a channel target, direct message for a nick
func (a *Adapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	kind := "dm"
	if isChannel(chatID) {
		kind = "group"
	}
	return map[string]any{"name": chatID, "type": kind}, nil
}
