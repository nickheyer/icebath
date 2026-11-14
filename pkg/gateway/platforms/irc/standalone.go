package irc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Largest line the asyncio stream reader buffers before it reports an overrun
const streamLimit = 1 << 16

// Pump result of a server that closed the connection
type eofResult struct{}

// CR and LF turned to spaces and NUL removed, closing the IRC command injection path
func stripIRCControlChars(text string) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(text, "\r", " "), "\n", " "), "\x00", "")
}

// Whether the target names a channel by its leading sigil
func isIRCChannel(target string) bool {
	return target != "" && strings.ContainsRune("#&+!", []rune(target)[0])
}

// Standalone send failure envelope with the shared prefix
func saError(detail string) map[string]any {
	return gateway.SendError("IRC standalone send: " + detail)
}

// Raw line io of one short lived connection
type standaloneConn struct {
	conn   net.Conn
	buffer []byte
}

// Writes one protocol line
func (c *standaloneConn) raw(line string) error {
	_, err := c.conn.Write(encodeLine(line))
	return err
}

// Next CRLF terminated line before the deadline, timedOut for an expired deadline and io.EOF for a closed stream
func (c *standaloneConn) readUntilCRLF(ctx context.Context, deadline time.Time) (line []byte, timedOut bool, err error) {
	chunk := make([]byte, 4096)
	for {
		if idx := bytes.Index(c.buffer, []byte("\r\n")); idx >= 0 {
			if idx+2 > streamLimit {
				return nil, false, &gateway.PyError{Kind: "LimitOverrunError", Msg: "Separator is found, but chunk is longer than limit"}
			}
			line = append([]byte{}, c.buffer[:idx+2]...)
			c.buffer = append([]byte{}, c.buffer[idx+2:]...)
			return line, false, nil
		}
		if len(c.buffer) > streamLimit {
			return nil, false, &gateway.PyError{Kind: "LimitOverrunError", Msg: "Separator is not found, and chunk exceed the limit"}
		}
		if err := c.conn.SetReadDeadline(deadline); err != nil {
			return nil, false, err
		}
		n, rerr := c.conn.Read(chunk)
		c.buffer = append(c.buffer, chunk[:n]...)
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		if rerr == nil || n > 0 && bytes.Contains(c.buffer, []byte("\r\n")) {
			continue
		}
		var netErr net.Error
		if errors.As(rerr, &netErr) && netErr.Timeout() {
			return nil, true, nil
		}
		if errors.Is(rerr, io.EOF) {
			return nil, false, io.EOF
		}
		return nil, false, rerr
	}
}

// Feeds commands to onMsg until it answers, nil on timeout and eofResult on a closed stream, PINGs answered meanwhile
func (c *standaloneConn) pump(ctx context.Context, timeout time.Duration, onMsg func(cmd string) (any, error)) (any, error) {
	stop := context.AfterFunc(ctx, func() { _ = c.conn.SetReadDeadline(time.Now()) })
	defer stop()
	deadline := time.Now().Add(timeout)
	for time.Until(deadline) > 0 {
		rawLine, timedOut, err := c.readUntilCRLF(ctx, deadline)
		if timedOut {
			return nil, nil
		}
		if errors.Is(err, io.EOF) {
			return eofResult{}, nil
		}
		if err != nil {
			return nil, err
		}
		msg := parseIRCMessage(pystr.RstripChars(pystr.DecodeUTF8Replace(rawLine), "\r\n"))
		if msg.command == "PING" {
			token := ""
			if len(msg.params) > 0 {
				token = msg.params[0]
			}
			if err := c.raw("PONG :" + token); err != nil {
				return nil, err
			}
			continue
		}
		result, err := onMsg(msg.command)
		if err != nil {
			return nil, err
		}
		if result != nil {
			return result, nil
		}
	}
	return nil, nil
}

// Closes the stream, every failure swallowed
func (c *standaloneConn) close() {
	_ = c.conn.Close()
}

// First thirty code points of a nick
func clipNick(nick string) string { return pystr.Slice(nick, 0, 30) }

// Sends PASS, NICK and USER and waits for the welcome, retrying nick collisions, an error envelope on failure
func saRegister(ctx context.Context, conn *standaloneConn, nickBase string, serverPassword any) (map[string]any, error) {
	nickAttempts := 0
	standaloneNick := clipNick(nickBase + "-cron")
	onRegistration := func(cmd string) (any, error) {
		switch cmd {
		case "432", "433":
			nickAttempts++
			if nickAttempts > 5 {
				return saError("too many nick collisions"), nil
			}
			standaloneNick = clipNick(fmt.Sprintf("%s-cron-%d", nickBase, nickAttempts))
			if err := conn.raw("NICK " + standaloneNick); err != nil {
				return nil, err
			}
		case "464", "465":
			return saError(fmt.Sprintf("server rejected client (%s)", cmd)), nil
		}
		if cmd == "001" {
			return true, nil
		}
		return nil, nil
	}
	if pyrepr.Truthy(serverPassword) {
		password, err := strAttr(serverPassword, "replace")
		if err != nil {
			return nil, err
		}
		if err := conn.raw("PASS " + stripIRCControlChars(password)); err != nil {
			return nil, err
		}
	}
	if err := conn.raw("NICK " + standaloneNick); err != nil {
		return nil, err
	}
	if err := conn.raw("USER " + standaloneNick + " 0 * :Icebath Agent (cron)"); err != nil {
		return nil, err
	}
	registered, err := conn.pump(ctx, 15*time.Second, onRegistration)
	if err != nil {
		return nil, err
	}
	switch r := registered.(type) {
	case nil:
		return saError("registration timeout (no RPL_WELCOME)"), nil
	case eofResult:
		return saError("server closed connection during registration"), nil
	case map[string]any:
		return r, nil
	}
	return nil, nil
}

// Joins a channel target before the PRIVMSG, an error envelope only for an explicit rejection
func saJoin(ctx context.Context, conn *standaloneConn, target string) (map[string]any, error) {
	onJoin := func(cmd string) (any, error) {
		switch cmd {
		case "403", "405", "471", "473", "474", "475":
			return saError(fmt.Sprintf("JOIN %s rejected (%s)", target, cmd)), nil
		case "366", "JOIN":
			return true, nil
		}
		return nil, nil
	}
	if err := conn.raw("JOIN " + target); err != nil {
		return nil, err
	}
	joined, err := conn.pump(ctx, 5*time.Second, onJoin)
	if err != nil {
		return nil, err
	}
	if rejected, ok := joined.(map[string]any); ok {
		return rejected, nil
	}
	return nil, nil
}

// Opens a short lived connection under a cron nick, sends the message as PRIVMSG lines and quits
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	chatID, _ := kwargs["chat_id"].(string)
	message, _ := kwargs["message"].(string)
	extra := configExtra(pconfig)
	server, channel, err := serverChannel(ctx, pconfig)
	if err != nil {
		return nil, err
	}
	if !pyrepr.Truthy(server) || !pyrepr.Truthy(channel) {
		return saError("IRC_SERVER and IRC_CHANNEL must be configured"), nil
	}
	portValue, err := envOrExtra(ctx, extra, "IRC_PORT", "port", 6697)
	if err != nil {
		return nil, err
	}
	portInt, err := pyint.Of(portValue)
	if pe, ok := err.(*pyint.Error); ok && (pe.Kind == "TypeError" || pe.Kind == "ValueError") {
		return saError("invalid port " + pyrepr.Repr(portValue)), nil
	} else if err != nil {
		return nil, err
	}
	useTLSEnv, found, err := gateway.LookupScopedSecret(ctx, "IRC_USE_TLS", false)
	if err != nil {
		return nil, err
	}
	useTLS := true
	if found {
		useTLS = truthyValues[pystr.Lower(useTLSEnv)]
	} else if v, ok := extra["use_tls"]; ok {
		useTLS = pyrepr.Truthy(v)
	}
	var targetValue any = chatID
	if chatID == "" {
		targetValue = channel
	}
	target, ok := targetValue.(string)
	if !ok {
		return nil, &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("argument of type '%s' is not iterable", pyrepr.TypeName(targetValue))}
	}
	if strings.ContainsAny(target, "\r\n\x00 ") {
		return saError("chat_id contains illegal IRC characters"), nil
	}
	nickValue, err := envOrExtra(ctx, extra, "IRC_NICKNAME", "nickname", "icebath-bot")
	if err != nil {
		return nil, err
	}
	nickText, err := strAttr(nickValue, "rstrip")
	if err != nil {
		return nil, err
	}
	nickBase := pystr.Slice(pystr.RstripChars(nickText, "_0123456789-"), 0, 24)
	if nickBase == "" {
		nickBase = "icebath-bot"
	}
	plain := stripMarkdown(message)
	if !portInt.IsInt64() || portInt.Int64() < 0 || portInt.Int64() > 65535 {
		return gateway.SendError("IRC standalone connect failed: getsockaddrarg: port must be 0-65535."), nil
	}
	netConn, err := openConnection(ctx, server, int(portInt.Int64()), useTLS, 15*time.Second)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return gateway.SendError("IRC standalone connect failed: " + connectErrorText(err)), nil
	}
	conn := &standaloneConn{conn: netConn}
	defer conn.close()
	result, err := standaloneDeliver(ctx, conn, extra, nickBase, target, plain)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		slog.Debug("IRC standalone send raised", "error", err)
		return gateway.SendError("IRC standalone send failed: " + standaloneErrorText(err)), nil
	}
	return result, nil
}

// str of an exception raised while the standalone connection is in use
func standaloneErrorText(err error) string {
	var pe *gateway.PyError
	if errors.As(err, &pe) {
		return pe.Msg
	}
	return pyhttpx.OSErrorText(err)
}

// Registers, identifies, joins a channel target and sends every line, the success envelope carrying a fresh id
func standaloneDeliver(ctx context.Context, conn *standaloneConn, extra map[string]any, nickBase, target, plain string) (map[string]any, error) {
	serverPassword, err := envOrExtra(ctx, extra, "IRC_SERVER_PASSWORD", "server_password", "")
	if err != nil {
		return nil, err
	}
	if failure, err := saRegister(ctx, conn, nickBase, serverPassword); err != nil || failure != nil {
		return failure, err
	}
	nickservPassword, err := envOrExtra(ctx, extra, "IRC_NICKSERV_PASSWORD", "nickserv_password", "")
	if err != nil {
		return nil, err
	}
	if pyrepr.Truthy(nickservPassword) {
		password, err := strAttr(nickservPassword, "replace")
		if err != nil {
			return nil, err
		}
		if err := conn.raw("PRIVMSG NickServ :IDENTIFY " + stripIRCControlChars(password)); err != nil {
			return nil, err
		}
		if err := sleepFor(ctx, 2*time.Second); err != nil {
			return nil, err
		}
	}
	if isIRCChannel(target) {
		if failure, err := saJoin(ctx, conn, target); err != nil || failure != nil {
			return failure, err
		}
	}
	paragraphs := []string{}
	for _, p := range strings.Split(plain, "\n") {
		if q := pystr.Rstrip(stripIRCControlChars(p)); q != "" {
			paragraphs = append(paragraphs, q)
		}
	}
	lines := splitLines(paragraphs, privmsgBudget(target))
	for _, line := range lines {
		if err := conn.raw("PRIVMSG " + target + " :" + line); err != nil {
			return nil, err
		}
		if err := sleepFor(ctx, 300*time.Millisecond); err != nil {
			return nil, err
		}
	}
	if len(lines) == 0 {
		return saError("empty message after stripping"), nil
	}
	if err := conn.raw("QUIT :" + i18n.T(ctx, "platform.irc.standalone_quit", "", nil)); err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.conn.SetReadDeadline(time.Now()) })
	defer stop()
	if len(conn.buffer) == 0 {
		if err := conn.conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			return nil, err
		}
		drain := make([]byte, 1024)
		_, rerr := conn.conn.Read(drain)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var netErr net.Error
		if rerr != nil && !errors.Is(rerr, io.EOF) && !(errors.As(rerr, &netErr) && netErr.Timeout()) {
			return nil, rerr
		}
	}
	return map[string]any{"success": true, "message_id": msID()}, nil
}
