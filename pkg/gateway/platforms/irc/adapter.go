package irc

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/dlclark/regexp2/v2"
	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Lowercased env values that read as true

// Markdown to plain text rewrites in order, images before links
var markdownRules = []struct {
	re   *regexp2.Regexp
	repl string
}{
	{pyre.MustCompile(`\*\*(.+?)\*\*`), "$1"},
	{pyre.MustCompile(`__(.+?)__`), "$1"},
	{pyre.MustCompile(`\*(.+?)\*`), "$1"},
	{pyre.MustCompile(`(?<!\w)_(.+?)_(?!\w)`), "$1"},
	{pyre.MustCompile("`(.+?)`"), "$1"},
	{pyre.MustCompile("```\\w*\\n?"), ""},
	{pyre.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`), "$2"},
	{pyre.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`), "$1 ($2)"},
}

// Trailing numeric suffix of a collision retried nick
var nickSuffixRE = pyre.MustCompile(`_(\d+)$`)

// One parsed IRC protocol line
type ircMessage struct {
	tags    map[string]string
	prefix  string
	command string
	params  []string
}

// Escaped characters of an IRCv3 tag value
var tagValueEscapes = [][2]string{{"\\", "\\\\"}, {";", "\\:"}, {" ", "\\s"}, {"\r", "\\r"}, {"\n", "\\n"}}

// IRCv3 tag value with its escapes undone
func unescapeTagValue(v string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] != '\\' || i+1 == len(v) {
			b.WriteByte(v[i])
			continue
		}
		i++
		switch v[i] {
		case ':':
			b.WriteByte(';')
		case 's':
			b.WriteByte(' ')
		case 'r':
			b.WriteByte('\r')
		case 'n':
			b.WriteByte('\n')
		default:
			b.WriteByte(v[i])
		}
	}
	return b.String()
}

// IRCv3 tag value escaped for an outgoing line
func escapeTagValue(v string) string {
	for _, pair := range tagValueEscapes {
		v = strings.ReplaceAll(v, pair[0], pair[1])
	}
	return v
}

// Tags, prefix, command and params of a raw IRC line with the trailing param last
func parseIRCMessage(raw string) ircMessage {
	prefix, trailing := "", ""
	tags := map[string]string{}
	if strings.HasPrefix(raw, "@") {
		var tagText string
		tagText, raw, _ = strings.Cut(raw[1:], " ")
		for _, tag := range strings.Split(tagText, ";") {
			key, value, _ := strings.Cut(tag, "=")
			if key != "" {
				tags[key] = unescapeTagValue(value)
			}
		}
		raw = strings.TrimLeft(raw, " ")
	}
	if strings.HasPrefix(raw, ":") {
		prefix, raw, _ = strings.Cut(raw[1:], " ")
	}
	if head, tail, found := strings.Cut(raw, " :"); found {
		raw, trailing = head, tail
	}
	parts := strings.Fields(raw)
	params := []string{}
	if len(parts) > 1 {
		params = append(params, parts[1:]...)
	}
	if trailing != "" {
		params = append(params, trailing)
	}
	command := ""
	if len(parts) > 0 {
		command = parts[0]
	}
	return ircMessage{tags: tags, prefix: prefix, command: command, params: params}
}

// Nickname part of a nick!user@host prefix
func extractNick(prefix string) string {
	nick, _, _ := strings.Cut(prefix, "!")
	return nick
}

// Wall clock milliseconds as a decimal string
func msID() string {
	return strconv.FormatInt(int64(float64(time.Now().UnixNano())/1e9*1000), 10)
}

// Scoped env value when non empty, else the extra key or the default
func envOrExtra(ctx context.Context, extra map[string]any, env, key string, def any) (any, error) {
	val, err := gateway.GetScopedSecret(ctx, env, "", false)
	if err != nil {
		return nil, err
	}
	if val != "" {
		return val, nil
	}
	if v, ok := extra[key]; ok {
		return v, nil
	}
	return def, nil
}

// Extra mapping of a platform config, empty when the config or its extra is unset
func configExtra(config *gateway.PlatformConfig) map[string]any {
	if config == nil || config.Extra == nil {
		return map[string]any{}
	}
	return config.Extra
}

// Server and channel of a config, the scoped env winning over extra
func serverChannel(ctx context.Context, config *gateway.PlatformConfig) (any, any, error) {
	extra := configExtra(config)
	server, err := envOrExtra(ctx, extra, "IRC_SERVER", "server", "")
	if err != nil {
		return nil, nil, err
	}
	channel, err := envOrExtra(ctx, extra, "IRC_CHANNEL", "channel", "")
	return server, channel, err
}

// UTF-8 byte length of a code point run
func utf8Len(rs []rune) int { return len(string(rs)) }

// One line cut into UTF-8 chunks of at most limit bytes, breaking at a space when one sits past the first third
func chunkParagraph(paragraph string, limit int) []string {
	chunks := []string{}
	rs := []rune(paragraph)
	for len(rs) > 0 {
		if utf8Len(rs) <= limit {
			chunks = append(chunks, string(rs))
			break
		}
		low, high, splitAt := 1, len(rs), 0
		for low <= high {
			mid := (low + high) / 2
			if utf8Len(rs[:mid]) <= limit {
				splitAt, low = mid, mid+1
			} else {
				high = mid - 1
			}
		}
		space := -1
		for i := splitAt - 1; i >= 0; i-- {
			if rs[i] == ' ' {
				space = i
				break
			}
		}
		if space > splitAt/3 {
			splitAt = space
		}
		chunks = append(chunks, strings.TrimRightFunc(string(rs[:splitAt]), unicode.IsSpace))
		rs = []rune(strings.TrimLeftFunc(string(rs[splitAt:]), unicode.IsSpace))
	}
	return chunks
}

// Payload bytes left in a 510 byte line after the PRIVMSG header and CRLF
func privmsgBudget(target string) int {
	return 510 - (len("PRIVMSG "+target+" :") + 2)
}

// Every paragraph chunked to the byte limit in order
func splitLines(paragraphs []string, limit int) []string {
	out := []string{}
	for _, paragraph := range paragraphs {
		out = append(out, chunkParagraph(paragraph, limit)...)
	}
	return out
}

// Protocol line with its CRLF terminator as bytes
func encodeLine(line string) []byte { return []byte(line + "\r\n") }

// Basic markdown rewritten to plain text for IRC
func stripMarkdown(text string) string {
	for _, rule := range markdownRules {
		out, err := rule.re.Replace(text, rule.repl, -1, -1)
		if err != nil {
			panic(err)
		}
		text = out
	}
	return text
}

// Text a value holds, an AttributeError for any other type
func textValue(v any) (string, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	return "", &gateway.PyError{Kind: "AttributeError", Msg: fmt.Sprintf("expected text, got %s", anyutil.Kind(v))}
}

// str of the exception asyncio open_connection raises for a failed dial, empty for the wait_for timeout
func connectErrorText(err error) string {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return ""
	}
	var pe *gateway.PyError
	if errors.As(err, &pe) {
		return pe.Msg
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return err.Error()
	}
	var opErr *net.OpError
	var errno syscall.Errno
	if errors.As(err, &opErr) && opErr.Op == "dial" && errors.As(err, &errno) {
		if addr, ok := opErr.Addr.(*net.TCPAddr); ok {
			return fmt.Sprintf("could not connect to %s port %d: %s", addr.IP.String(), addr.Port, errno.Error())
		}
	}
	return err.Error()
}

// TCP stream to the server, TLS verified against the system roots when asked, bounded by the timeout
func openConnection(ctx context.Context, server any, port int, useTLS bool, timeout time.Duration) (net.Conn, error) {
	host, ok := server.(string)
	if !ok {
		return nil, &gateway.PyError{Kind: "TypeError", Msg: "getaddrinfo() argument 1 must be string or None"}
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	address := net.JoinHostPort(host, strconv.Itoa(port))
	if useTLS {
		dialer := &tls.Dialer{Config: &tls.Config{ServerName: host}}
		conn, err := dialer.DialContext(dctx, "tcp", address)
		if err != nil && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return conn, err
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(dctx, "tcp", address)
	if err != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return conn, err
}

// Sleeps the given duration unless the context ends first
func sleepFor(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// IRC gateway adapter over one TCP or TLS stream, addressed messages in and PRIVMSG lines out
type IRCAdapter struct {
	*gateway.BasePlatformAdapter

	server           any
	port             int
	nickname         any
	channel          any
	useTLS           bool
	serverPassword   any
	nickservPassword any
	allowedLower     map[string]bool
	maxMessageLength int

	mu                sync.Mutex
	conn              net.Conn
	closing           bool
	recvTask          *gateway.AdapterTask
	currentNick       any
	registered        bool
	messageTags       bool
	registrationEvent *gateway.InterruptEvent
}

// Context key carrying the server msgid of the PRIVMSG being dispatched
type msgIDKey struct{}

// Class attributes the IRC adapter declares over the base defaults
func ircAdapterTraits() gateway.AdapterTraits {
	traits := gateway.DefaultAdapterTraits()
	traits.SerialReceiveLoop = true
	return traits
}

// Adapter for a platform config, scoped env values winning over the extra keys
func NewIRCAdapter(ctx context.Context, config *gateway.PlatformConfig) (*IRCAdapter, error) {
	if config == nil {
		config = gateway.NewPlatformConfig()
	}
	a := &IRCAdapter{registrationEvent: gateway.NewInterruptEvent()}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, gateway.Platform("irc"), a)
	a.Traits = ircAdapterTraits()
	extra := configExtra(config)
	var err error
	if a.server, err = envOrExtra(ctx, extra, "IRC_SERVER", "server", ""); err != nil {
		return nil, err
	}
	port, err := envOrExtra(ctx, extra, "IRC_PORT", "port", 6697)
	if err != nil {
		return nil, err
	}
	a.port = gateway.CoercePort(port, 6697)
	if a.nickname, err = envOrExtra(ctx, extra, "IRC_NICKNAME", "nickname", "icebath-bot"); err != nil {
		return nil, err
	}
	if a.channel, err = envOrExtra(ctx, extra, "IRC_CHANNEL", "channel", ""); err != nil {
		return nil, err
	}
	sctx := gateway.SettingsContext(ctx)
	a.useTLS = settings.IrcUseTLS.Get(sctx)
	if a.serverPassword, err = envOrExtra(ctx, extra, "IRC_SERVER_PASSWORD", "server_password", ""); err != nil {
		return nil, err
	}
	if a.nickservPassword, err = envOrExtra(ctx, extra, "IRC_NICKSERV_PASSWORD", "nickserv_password", ""); err != nil {
		return nil, err
	}
	a.allowedLower = map[string]bool{}
	for _, nick := range settings.IrcAllowedUsers.Strings(sctx) {
		a.allowedLower[strings.ToLower(nick)] = true
	}
	a.maxMessageLength = settings.IrcMaxMessageLength.Get(sctx)
	a.currentNick = a.nickname
	return a, nil
}

// Display name of the adapter
func (a *IRCAdapter) Name() string { return "IRC" }

// Records a fatal error and reports the connect as failed
func (a *IRCAdapter) fail(ctx context.Context, code, message string, retryable bool) bool {
	a.SetFatalError(ctx, code, message, retryable)
	return false
}

// Open stream of the adapter, nil once closed or never opened
func (a *IRCAdapter) liveConn() net.Conn {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conn == nil || a.closing {
		return nil
	}
	return a.conn
}

// Connects to the server, registers, identifies with NickServ when configured and joins the channel
func (a *IRCAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	if !anyutil.IsTrue(a.server) || !anyutil.IsTrue(a.channel) {
		pkgLog().Error("IRC: server and channel must be configured")
		return a.fail(ctx, "config_missing", "IRC_SERVER and IRC_CHANNEL must be set", false), nil
	}
	server, nickname := anyutil.String(a.server), anyutil.String(a.nickname)
	acquired, err := a.AcquirePlatformLock(ctx, "irc", server+":"+nickname, "IRC identity "+nickname+"@"+server)
	if err != nil || !acquired {
		return false, err
	}
	conn, err := openConnection(ctx, a.server, a.port, a.useTLS, 30*time.Second)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		text := connectErrorText(err)
		pkgLog().Error(fmt.Sprintf("IRC: failed to connect to %s:%d — %s", server, a.port, text))
		return a.fail(ctx, "connect_failed", text, true), nil
	}
	a.mu.Lock()
	a.conn, a.closing = conn, false
	a.mu.Unlock()
	if anyutil.IsTrue(a.serverPassword) {
		if err := a.sendRaw("PASS " + anyutil.String(a.serverPassword)); err != nil {
			return false, err
		}
	}
	if err := a.sendRaw("CAP REQ :message-tags"); err != nil {
		return false, err
	}
	if err := a.sendRaw("NICK " + nickname); err != nil {
		return false, err
	}
	if err := a.sendRaw("USER " + nickname + " 0 * :Icebath Agent"); err != nil {
		return false, err
	}
	task := gateway.SpawnAdapterTask(ctx, func(tctx context.Context) error { return a.receiveLoop(tctx, conn) })
	a.mu.Lock()
	a.recvTask = task
	a.mu.Unlock()
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case <-a.registrationEvent.Done():
	case <-timer.C:
		pkgLog().Error("IRC: registration timed out")
		if err := a.Disconnect(ctx); err != nil {
			return false, err
		}
		return a.fail(ctx, "registration_timeout", "IRC server did not send RPL_WELCOME", true), nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
	if anyutil.IsTrue(a.nickservPassword) {
		if err := a.sendRaw("PRIVMSG NickServ :IDENTIFY " + anyutil.String(a.nickservPassword)); err != nil {
			return false, err
		}
		if err := sleepFor(ctx, 2*time.Second); err != nil {
			return false, err
		}
	}
	channel := anyutil.String(a.channel)
	if err := a.sendRaw("JOIN " + channel); err != nil {
		return false, err
	}
	a.MarkConnected(ctx, "")
	a.mu.Lock()
	currentNick := anyutil.String(a.currentNick)
	a.mu.Unlock()
	pkgLog().Info(fmt.Sprintf("IRC: connected to %s:%d as %s, joined %s", server, a.port, currentNick, channel))
	a.WirePluginHandlers(ctx, nil)
	return true, nil
}

// Releases the identity lock, quits, closes the stream and stops the receive loop
func (a *IRCAdapter) Disconnect(ctx context.Context) error {
	func() {
		defer func() { _ = recover() }()
		a.ReleasePlatformLock()
	}()
	a.MarkDisconnected(ctx)
	if conn := a.liveConn(); conn != nil {
		if err := a.sendRaw("QUIT :" + i18n.T(ctx, "platform.irc.quit_message", "", nil)); err == nil {
			_ = sleepFor(ctx, 500*time.Millisecond)
		}
		a.mu.Lock()
		a.closing = true
		a.mu.Unlock()
		_ = conn.Close()
	}
	a.mu.Lock()
	task := a.recvTask
	a.mu.Unlock()
	gateway.CancelTask(ctx, task)
	a.mu.Lock()
	a.conn = nil
	a.registered = false
	a.mu.Unlock()
	a.registrationEvent.Clear()
	return nil
}

// Reacts to a message by its server msgid with an IRCv3 +draft/react TAGMSG
func (a *IRCAdapter) AddReaction(ctx context.Context, chatID, messageID, emoji string) error {
	a.mu.Lock()
	tagged := a.messageTags
	a.mu.Unlock()
	if !tagged {
		return errors.New("this IRC server did not grant the message-tags capability, so it carries no reactions")
	}
	return a.sendRaw("@+draft/reply=" + escapeTagValue(messageID) + ";+draft/react=" + escapeTagValue(emoji) + " TAGMSG " + chatID)
}

// Sends the content as flood paced PRIVMSG lines, a write failure ending the send with its text
func (a *IRCAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	if a.liveConn() == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	for _, line := range a.splitMessage(content, chatID) {
		if err := a.sendRaw("PRIVMSG " + chatID + " :" + line); err != nil {
			return gateway.SendResult{Success: false, Error: err.Error()}, nil
		}
		if err := sleepFor(ctx, 300*time.Millisecond); err != nil {
			return gateway.SendResult{}, err
		}
	}
	return gateway.SendResult{Success: true, MessageID: msID()}, nil
}

// IRC has no typing indicator
func (a *IRCAdapter) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	return nil
}

// Chat info of a channel or a direct message nick
func (a *IRCAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	chatType := "dm"
	if strings.HasPrefix(chatID, "#") || strings.HasPrefix(chatID, "&") {
		chatType = "group"
	}
	return map[string]any{"name": chatID, "type": chatType}, nil
}

// Markdown stripped content split into lines that fit the configured length and the PRIVMSG byte budget
func (a *IRCAdapter) splitMessage(content, target string) []string {
	paragraphs := []string{}
	for _, p := range strings.Split(stripMarkdown(content), "\n") {
		if strings.TrimSpace(p) != "" {
			paragraphs = append(paragraphs, p)
		}
	}
	lines := splitLines(paragraphs, min(a.maxMessageLength, privmsgBudget(target)))
	if len(lines) == 0 {
		return []string{""}
	}
	return lines
}

// Writes one raw protocol line, nothing when the stream is closed
func (a *IRCAdapter) sendRaw(line string) error {
	conn := a.liveConn()
	if conn == nil {
		return nil
	}
	_, err := conn.Write(encodeLine(line))
	return err
}

// Reads CRLF terminated lines and dispatches each, a lost stream becoming a retryable fatal error
func (a *IRCAdapter) receiveLoop(ctx context.Context, conn net.Conn) (err error) {
	stop := context.AfterFunc(ctx, func() { _ = conn.SetReadDeadline(time.Now()) })
	defer stop()
	defer func() {
		if a.IsConnected() {
			pkgLog().Warn("IRC: connection lost, marking disconnected")
			a.SetFatalError(ctx, "connection_lost", "IRC connection closed unexpectedly", true)
			if nerr := a.NotifyFatalError(ctx); nerr != nil && err == nil {
				err = nerr
			}
		}
	}()
	buffer := []byte{}
	chunk := make([]byte, 4096)
	for {
		n, rerr := conn.Read(chunk)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if n == 0 {
			if rerr != nil && !isStreamEOF(rerr) {
				pkgLog().Error(fmt.Sprintf("IRC: receive loop error: %s", rerr.Error()))
			}
			return nil
		}
		buffer = append(buffer, chunk[:n]...)
		for {
			idx := strings.Index(string(buffer), "\r\n")
			if idx < 0 {
				break
			}
			line := buffer[:idx]
			buffer = append([]byte{}, buffer[idx+2:]...)
			if herr := a.handleLine(ctx, strings.ToValidUTF8(string(line), "\uFFFD")); herr != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				pkgLog().Warn(fmt.Sprintf("IRC: error handling line: %s", herr.Error()))
			}
		}
		if rerr != nil {
			if !isStreamEOF(rerr) {
				pkgLog().Error(fmt.Sprintf("IRC: receive loop error: %s", rerr.Error()))
			}
			return nil
		}
	}
}

// Whether a read error is the peer closing the stream
func isStreamEOF(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed)
}

// Dispatches one protocol line, PING, welcome, nick collision, PRIVMSG and own nick changes
func (a *IRCAdapter) handleLine(ctx context.Context, raw string) error {
	msg := parseIRCMessage(raw)
	command, params := msg.command, msg.params
	switch {
	case command == "PING":
		token := ""
		if len(params) > 0 {
			token = params[0]
		}
		return a.sendRaw("PONG :" + token)
	case command == "001":
		a.mu.Lock()
		a.registered = true
		a.mu.Unlock()
		a.registrationEvent.Set()
		if len(params) > 0 {
			a.mu.Lock()
			a.currentNick = params[0]
			a.mu.Unlock()
		}
	case command == "433":
		a.mu.Lock()
		current := a.currentNick
		a.mu.Unlock()
		currentText, isStr := current.(string)
		if !isStr {
			return &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("the current nick must be text, got %s", anyutil.Kind(current))}
		}
		m, err := nickSuffixRE.FindStringMatch(currentText)
		if err != nil {
			return err
		}
		var next string
		if m != nil {
			base, err := textValue(a.nickname)
			if err != nil {
				return err
			}
			n, err := strconv.ParseInt(m.GroupByNumber(1).String(), 10, 64)
			if err != nil {
				return err
			}
			next = strings.TrimRight(base, "_0123456789") + "_" + strconv.FormatInt(n+1, 10)
		} else if anyutil.Equal(current, a.nickname) {
			next = anyutil.String(a.nickname) + "_"
		} else {
			next = anyutil.String(a.nickname) + "_1"
		}
		a.mu.Lock()
		a.currentNick = next
		a.mu.Unlock()
		return a.sendRaw("NICK " + next)
	case command == "CAP" && len(params) >= 3 && (params[1] == "ACK" || params[1] == "NAK"):
		if params[1] == "ACK" && slices.Contains(strings.Fields(params[2]), "message-tags") {
			a.mu.Lock()
			a.messageTags = true
			a.mu.Unlock()
		}
		return a.sendRaw("CAP END")
	case command == "PRIVMSG" && len(params) >= 2:
		if msgID := msg.tags["msgid"]; msgID != "" {
			ctx = context.WithValue(ctx, msgIDKey{}, msgID)
		}
		return a.handlePrivmsg(ctx, extractNick(msg.prefix), params[0], params[1])
	case command == "NICK" && len(params) > 0:
		a.mu.Lock()
		current := a.currentNick
		a.mu.Unlock()
		currentText, err := textValue(current)
		if err != nil {
			return err
		}
		if strings.ToLower(extractNick(msg.prefix)) == strings.ToLower(currentText) {
			a.mu.Lock()
			a.currentNick = params[0]
			a.mu.Unlock()
		}
	}
	return nil
}

// Filters one PRIVMSG by sender, CTCP kind, channel addressing and the allowlist before dispatch
func (a *IRCAdapter) handlePrivmsg(ctx context.Context, senderNick, target, text string) error {
	a.mu.Lock()
	current := a.currentNick
	a.mu.Unlock()
	currentNick, err := textValue(current)
	if err != nil {
		return err
	}
	if strings.ToLower(senderNick) == strings.ToLower(currentNick) {
		return nil
	}
	if strings.HasPrefix(text, "\x01ACTION ") && strings.HasSuffix(text, "\x01") {
		rs := []rune(text)
		body := ""
		if len(rs) > 9 {
			body = string(rs[8 : len(rs)-1])
		}
		text = "* " + senderNick + " " + body
	}
	if strings.HasPrefix(text, "\x01") {
		return nil
	}
	isChannel := strings.HasPrefix(target, "#") || strings.HasPrefix(target, "&")
	if isChannel {
		addressed := false
		for _, prefix := range []string{currentNick + ":", currentNick + ",", currentNick + " "} {
			if strings.HasPrefix(strings.ToLower(text), strings.ToLower(prefix)) {
				rs, skip := []rune(text), len([]rune(prefix))
				if skip > len(rs) {
					skip = len(rs)
				}
				text = strings.TrimSpace(string(rs[skip:]))
				addressed = true
				break
			}
		}
		if !addressed {
			return nil
		}
	}
	if len(a.allowedLower) > 0 && !a.allowedLower[strings.ToLower(senderNick)] {
		pkgLog().Debug(fmt.Sprintf("IRC: ignoring message from unauthorized user %s", senderNick))
		return nil
	}
	chatID, chatType := senderNick, "dm"
	if isChannel {
		chatID, chatType = target, "group"
	}
	return a.dispatchMessage(ctx, text, chatID, chatType, senderNick, senderNick)
}

// Builds a text message event with a fresh id and hands it to the base handler
func (a *IRCAdapter) dispatchMessage(ctx context.Context, text, chatID, chatType, userID, userName string) error {
	if a.MessageHandler() == nil {
		return nil
	}
	source := a.BuildSource(ctx, chatID, gateway.SourceFields{ChatName: &chatID, ChatType: chatType, UserID: &userID, UserName: &userName})
	event := gateway.NewMessageEvent(text)
	event.MessageType, event.Source = gateway.MessageTypeText, source
	messageID, _ := ctx.Value(msgIDKey{}).(string)
	if messageID == "" {
		messageID = strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	event.MessageID = &messageID
	return a.HandleMessage(ctx, event)
}
