package irc

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
)

const (
	standaloneConnectTimeout  = 15 * time.Second
	standaloneRegisterTimeout = 15 * time.Second
	standaloneJoinTimeout     = 5 * time.Second
	standaloneQuitWait        = 2 * time.Second
	standaloneNickLen         = 30
	standaloneNickBaseLen     = 24
	maxNickAttempts           = 5
)

// Numerics of a refused JOIN
var joinRefusals = map[string]bool{"403": true, "405": true, "471": true, "473": true, "474": true, "475": true}

// How a pump wait ended
type pumpOutcome int

const (
	pumpDone pumpOutcome = iota
	pumpTimeout
	pumpEOF
)

// Short lived client connection of one standalone delivery
type standaloneConn struct {
	conn net.Conn
	r    *bufio.Reader
}

// Writes one protocol line terminated by CRLF
func (c *standaloneConn) raw(line string) error {
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err := c.conn.Write([]byte(line + "\r\n"))
	return err
}

// Feeds commands to the handler until it finishes or refuses, answering pings, a timeout or EOF ending the wait
func (c *standaloneConn) pump(timeout time.Duration, handle func(cmd string) (done bool, refusal string, err error)) (pumpOutcome, string, error) {
	deadline := time.Now().Add(timeout)
	_ = c.conn.SetReadDeadline(deadline)
	defer c.conn.SetReadDeadline(time.Time{})
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			var netErr net.Error
			switch {
			case errors.As(err, &netErr) && netErr.Timeout():
				return pumpTimeout, "", nil
			case errors.Is(err, io.EOF):
				return pumpEOF, "", nil
			}
			return pumpDone, "", err
		}
		m := parseMessage(strings.ToValidUTF8(strings.TrimRight(line, "\r\n"), "�"))
		if m.command == "PING" {
			first := ""
			if len(m.params) > 0 {
				first = m.params[0]
			}
			if err := c.raw("PONG :" + first); err != nil {
				return pumpDone, "", err
			}
			continue
		}
		done, refusal, err := handle(m.command)
		if err != nil || refusal != "" || done {
			return pumpDone, refusal, err
		}
	}
}

// First n runes of s
func runePrefix(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// Standalone failure envelope with the sender prefix
func standaloneError(detail string) map[string]any {
	return gateway.SendError("IRC standalone send: " + detail)
}

// Registers under a cron nick derived from the base, retrying collisions, a refusal text when the server says no
func (c *standaloneConn) register(nickBase, serverPassword string) (string, error) {
	nick := runePrefix(nickBase+"-cron", standaloneNickLen)
	attempts := 0
	if serverPassword != "" {
		if err := c.raw("PASS " + stripControlChars(serverPassword)); err != nil {
			return "", err
		}
	}
	if err := c.raw("NICK " + nick); err != nil {
		return "", err
	}
	if err := c.raw("USER " + nick + " 0 * :Icebath Agent (cron)"); err != nil {
		return "", err
	}
	outcome, refusal, err := c.pump(standaloneRegisterTimeout, func(cmd string) (bool, string, error) {
		switch cmd {
		case "432", "433":
			if attempts++; attempts > maxNickAttempts {
				return false, "too many nick collisions", nil
			}
			nick = runePrefix(fmt.Sprintf("%s-cron-%d", nickBase, attempts), standaloneNickLen)
			return false, "", c.raw("NICK " + nick)
		case "464", "465":
			return false, fmt.Sprintf("server rejected client (%s)", cmd), nil
		case "001":
			return true, "", nil
		}
		return false, "", nil
	})
	switch {
	case err != nil:
		return "", err
	case refusal != "":
		return refusal, nil
	case outcome == pumpTimeout:
		return "registration timeout (no RPL_WELCOME)", nil
	case outcome == pumpEOF:
		return "server closed connection during registration", nil
	}
	return "", nil
}

// Joins a channel target so moderated channels accept the message, a refusal text only on an explicit refusal
func (c *standaloneConn) join(target string) (string, error) {
	if err := c.raw("JOIN " + target); err != nil {
		return "", err
	}
	_, refusal, err := c.pump(standaloneJoinTimeout, func(cmd string) (bool, string, error) {
		if joinRefusals[cmd] {
			return false, fmt.Sprintf("JOIN %s rejected (%s)", target, cmd), nil
		}
		return cmd == "366" || cmd == "JOIN", "", nil
	})
	return refusal, err
}

// Out of process delivery over a short lived connection under a distinct cron nick that never collides with the gateway
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	chatID, _ := kwargs["chat_id"].(string)
	message, _ := kwargs["message"].(string)
	var extra map[string]any
	if pconfig != nil {
		extra = pconfig.Extra
	}
	s, err := loadSettings(ctx, extra)
	if err != nil {
		return gateway.SendError("IRC standalone send failed: " + err.Error()), nil
	}
	if s.server == "" || s.channel == "" {
		return standaloneError("IRC_SERVER and IRC_CHANNEL must be configured"), nil
	}
	port := s.port
	if port == 0 {
		if s.portRaw != nil && strings.TrimSpace(textOf(s.portRaw)) != "" {
			return standaloneError(fmt.Sprintf("invalid port %q", textOf(s.portRaw))), nil
		}
		port = defaultPort(s.useTLS)
	}
	target := chatID
	if target == "" {
		target = s.channel
	}
	if !validTarget(target) {
		return standaloneError("chat_id contains illegal IRC characters"), nil
	}
	nickBase := runePrefix(strings.TrimRight(s.nickname, "_0123456789-"), standaloneNickBaseLen)
	if nickBase == "" {
		nickBase = defaultNickname
	}
	lines := splitLines(message, target, privmsgBudget(target))
	if len(lines) == 0 {
		return standaloneError("empty message after stripping"), nil
	}
	conn, err := dial(ctx, s.server, port, s.useTLS, standaloneConnectTimeout)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return gateway.SendError("IRC standalone connect failed: " + err.Error()), nil
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	c := &standaloneConn{conn: conn, r: bufio.NewReader(conn)}
	failed := func(err error) (map[string]any, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return gateway.SendError("IRC standalone send failed: " + err.Error()), nil
	}
	refusal, err := c.register(nickBase, s.serverPassword)
	if err != nil {
		return failed(err)
	}
	if refusal != "" {
		return standaloneError(refusal), nil
	}
	if s.nickservPassword != "" {
		if err := c.raw("PRIVMSG NickServ :IDENTIFY " + stripControlChars(s.nickservPassword)); err != nil {
			return failed(err)
		}
		if !sleepCtx(ctx, nickservDelay) {
			return nil, ctx.Err()
		}
	}
	if isChannel(target) {
		refusal, err := c.join(target)
		if err != nil {
			return failed(err)
		}
		if refusal != "" {
			return standaloneError(refusal), nil
		}
	}
	for _, line := range lines {
		if err := c.raw("PRIVMSG " + target + " :" + line); err != nil {
			return failed(err)
		}
		if !sleepCtx(ctx, floodDelay) {
			return nil, ctx.Err()
		}
	}
	if err := c.raw("QUIT :" + i18n.T(ctx, "platform.irc.standalone_quit", "", nil)); err != nil {
		return failed(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(standaloneQuitWait))
	_, _ = c.r.Read(make([]byte, 1024))
	return map[string]any{"success": true, "message_id": msID()}, nil
}
