package mailutil

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/textproto"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// Longest reply line accepted
	maxSMTPLine = 8192
	// Most lines one reply may have
	maxReplyLines = 1000
	// Most challenges one AUTH exchange may take
	maxChallenges = 5
)

// Client of one SMTP session, its commands run one at a time
type SMTP struct {
	session
	ext   map[string]string
	hello bool
}

// Session with the server at host and port, over implicit TLS when a configuration is given, its 220 greeting checked; port 0 means 465 with TLS and 25 without, timeout bounds the connect and each read and write when not zero, and a nil dial means Dial
func DialSMTP(ctx context.Context, host string, port int, timeout time.Duration, dial DialFunc, tlsConfig *tls.Config) (*SMTP, error) {
	if port == 0 {
		port = 25
		if tlsConfig != nil {
			port = 465
		}
	}
	c := &SMTP{}
	if err := c.open(ctx, "smtp", dial, host, port, timeout, tlsConfig); err != nil {
		return nil, err
	}
	err := c.run(ctx, func() error {
		code, msg, err := c.reply()
		if err != nil {
			return err
		}
		if code != 220 {
			return &SMTPError{Command: "greeting", Code: code, Message: msg}
		}
		return nil
	})
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// Upgrades the session to TLS with STARTTLS, which the server must advertise, and forgets the extensions it advertised in plain text
func (c *SMTP) StartTLS(ctx context.Context, config *tls.Config) error {
	return c.run(ctx, func() error {
		if c.secure {
			return fmt.Errorf("smtp: %w: STARTTLS on a connection that is already encrypted", errArgument)
		}
		if err := c.greet(); err != nil {
			return err
		}
		if _, ok := c.ext["STARTTLS"]; !ok {
			return fmt.Errorf("smtp: STARTTLS %w", ErrNotSupported)
		}
		code, msg, err := c.cmd("STARTTLS")
		if err != nil {
			return err
		}
		if code != 220 {
			return &SMTPError{Command: "STARTTLS", Code: code, Message: msg}
		}
		if err := c.handshake(ctx, config); err != nil {
			return err
		}
		c.ext, c.hello = nil, false
		return nil
	})
}

// Logs in with the first of CRAM-MD5, PLAIN and LOGIN the server advertises and accepts, the next tried when one is rejected; an error matching ErrAuthFailed when every one is, and ErrInsecure without sending anything over a connection without TLS unless AllowPlaintextLogin was called or the server is on this machine
func (c *SMTP) Login(ctx context.Context, user, password string) error {
	return c.run(ctx, func() error {
		if strings.IndexByte(user, 0) >= 0 || strings.IndexByte(password, 0) >= 0 {
			return fmt.Errorf("smtp: %w: user name or password contains NUL", errArgument)
		}
		if err := c.checkCredentials(); err != nil {
			return err
		}
		if err := c.greet(); err != nil {
			return err
		}
		offered, ok := c.ext["AUTH"]
		if !ok {
			return fmt.Errorf("smtp: AUTH %w", ErrNotSupported)
		}
		mechanisms := strings.Fields(strings.ToUpper(offered))
		var last error
		for _, mechanism := range [...]string{"CRAM-MD5", "PLAIN", "LOGIN"} {
			if !slices.Contains(mechanisms, mechanism) {
				continue
			}
			err := c.auth(mechanism, user, password)
			if err == nil {
				return nil
			}
			if !errors.Is(err, ErrAuthFailed) {
				return err
			}
			last = err
		}
		if last == nil {
			return fmt.Errorf("smtp: AUTH with any of %q %w", offered, ErrNotSupported)
		}
		return last
	})
}

// One AUTH exchange with a mechanism, each server challenge answered until the server decides
func (c *SMTP) auth(mechanism, user, password string) error {
	line := "AUTH " + mechanism
	var answer func(challenge []byte, n int) []byte
	switch mechanism {
	case "PLAIN":
		identity := []byte("\x00" + user + "\x00" + password)
		line += " " + base64.StdEncoding.EncodeToString(identity)
		answer = func([]byte, int) []byte { return identity }
	case "LOGIN":
		answer = func(_ []byte, n int) []byte {
			if n == 1 {
				return []byte(user)
			}
			return []byte(password)
		}
	case "CRAM-MD5":
		answer = func(challenge []byte, _ int) []byte {
			mac := hmac.New(md5.New, []byte(password))
			mac.Write(challenge)
			return []byte(user + " " + hex.EncodeToString(mac.Sum(nil)))
		}
	}
	code, msg, err := c.cmd(line)
	for n := 1; err == nil && code == 334; n++ {
		if n > maxChallenges {
			_, _, _ = c.cmd("*")
			return fmt.Errorf("smtp: AUTH %s still unfinished after %d challenges, last %q", mechanism, maxChallenges, msg)
		}
		challenge, decodeErr := base64.StdEncoding.DecodeString(strings.TrimSpace(msg))
		if decodeErr != nil {
			_, _, _ = c.cmd("*")
			return fmt.Errorf("smtp: AUTH %s: challenge %q is not base64", mechanism, msg)
		}
		code, msg, err = c.cmd(base64.StdEncoding.EncodeToString(answer(challenge, n)))
	}
	if err != nil {
		return err
	}
	if code == 235 || code == 503 {
		return nil
	}
	return &SMTPError{Command: "AUTH " + mechanism, Code: code, Message: msg}
}

// Sends the message from the address of its Sender or From field to every address of its To, Cc and Bcc fields, the Resent- fields instead when it carries one Resent- block, with its Bcc fields left out of what is sent; addresses outside ASCII go through SMTPUTF8 or, for a domain alone, punycode
func (c *SMTP) SendMessage(ctx context.Context, msg *Part) error {
	return c.run(ctx, func() error {
		from, recipients, err := envelope(msg.Header)
		if err != nil {
			return err
		}
		data, err := msg.bytes(func(name string) bool {
			return strings.EqualFold(name, "Bcc") || strings.EqualFold(name, "Resent-Bcc")
		})
		if err != nil {
			return err
		}
		return c.send(from, recipients, data)
	})
}

// Sender and recipient addresses of a message's header
func envelope(h Header) (string, []string, error) {
	prefix := ""
	switch n := len(h.Values("Resent-Date")); {
	case n == 1:
		prefix = "Resent-"
	case n > 1:
		return "", nil, fmt.Errorf("mail: %w: message has more than one Resent- block", errArgument)
	}
	sender, ok := h.Lookup(prefix + "Sender")
	if !ok {
		if sender, ok = h.Lookup(prefix + "From"); !ok {
			return "", nil, fmt.Errorf("mail: %w: message has no %sFrom field", errArgument, prefix)
		}
	}
	senders, err := parseAddressList(sender)
	if err != nil || len(senders) == 0 {
		return "", nil, fmt.Errorf("mail: %w: sender %q is no address", errArgument, sender)
	}
	var recipients []string
	for _, name := range [...]string{"To", "Cc", "Bcc"} {
		for _, value := range h.Values(prefix + name) {
			list, err := parseAddressList(value)
			if err != nil {
				return "", nil, fmt.Errorf("mail: %w: %s%s field %q: %w", errArgument, prefix, name, value, err)
			}
			for _, a := range list {
				recipients = append(recipients, a.Address)
			}
		}
	}
	if len(recipients) == 0 {
		return "", nil, fmt.Errorf("mail: %w: message has no recipients", errArgument)
	}
	return senders[0].Address, recipients, nil
}

// One mail transaction, failing when the server refuses the sender, every recipient or the data
func (c *SMTP) send(from string, recipients []string, data []byte) error {
	if err := c.greet(); err != nil {
		return err
	}
	from, needUTF8 := asciiAddress(from)
	from = smtpMailbox(from)
	if err := c.checkLine("sender", from); err != nil {
		return err
	}
	targets := make([]string, len(recipients))
	for i, r := range recipients {
		var wide bool
		targets[i], wide = asciiAddress(r)
		targets[i] = smtpMailbox(targets[i])
		if err := c.checkLine("recipient", targets[i]); err != nil {
			return err
		}
		needUTF8 = needUTF8 || wide
	}
	_, hasUTF8 := c.ext["SMTPUTF8"]
	if needUTF8 && !hasUTF8 {
		return fmt.Errorf("smtp: addresses outside ASCII need SMTPUTF8, %w", ErrNotSupported)
	}
	line := "MAIL FROM:<" + from + ">"
	if _, ok := c.ext["SIZE"]; ok {
		line += " SIZE=" + strconv.Itoa(len(data))
	}
	if needUTF8 {
		line += " SMTPUTF8"
	}
	code, msg, err := c.cmd(line)
	if err != nil {
		return err
	}
	if code != 250 {
		c.abort(code)
		return &SMTPError{Command: "MAIL FROM", Code: code, Message: msg}
	}
	var refused []error
	for _, to := range targets {
		code, msg, err := c.cmd("RCPT TO:<" + to + ">")
		if err != nil {
			return err
		}
		if code == 250 || code == 251 {
			continue
		}
		refused = append(refused, &SMTPError{Command: "RCPT TO <" + to + ">", Code: code, Message: msg})
		if code == 421 {
			c.abort(code)
			return fmt.Errorf("smtp: server closed the transaction: %w", errors.Join(refused...))
		}
	}
	if len(refused) == len(targets) {
		c.abort(0)
		return fmt.Errorf("smtp: every recipient refused: %w", errors.Join(refused...))
	}
	if code, msg, err = c.cmd("DATA"); err != nil {
		return err
	}
	if code != 354 {
		c.abort(code)
		return &SMTPError{Command: "DATA", Code: code, Message: msg}
	}
	dot := textproto.NewWriter(c.w).DotWriter()
	if _, err := dot.Write(data); err != nil {
		return fmt.Errorf("smtp: sending the message: %w", err)
	}
	if err := dot.Close(); err != nil {
		return fmt.Errorf("smtp: sending the message: %w", err)
	}
	if code, msg, err = c.reply(); err != nil {
		return err
	}
	if code != 250 {
		c.abort(code)
		return &SMTPError{Command: "message data", Code: code, Message: msg}
	}
	return nil
}

// Clears a refused transaction with RSET, or drops the connection when the reply says the server is closing it or RSET does not succeed
func (c *SMTP) abort(code int) {
	if code != 421 {
		rset, msg, err := c.cmd("RSET")
		if err == nil && rset == 250 {
			return
		}
		if err == nil {
			err = &SMTPError{Command: "RSET", Code: rset, Message: msg}
		}
		c.broken = fmt.Errorf("smtp: transaction could not be reset: %w", err)
		_ = c.raw.Close()
		return
	}
	c.broken = errors.New("smtp: server closed the connection")
	_ = c.raw.Close()
}

// Mailbox as the path of MAIL FROM or RCPT TO, a local part that is no dot-string written as a quoted string so spaces, angle brackets and quotes in it stay inside the path
func smtpMailbox(addr string) string {
	at := strings.LastIndexByte(addr, '@')
	if at < 0 || isDotString(addr[:at]) {
		return addr
	}
	local := addr[:at]
	var b strings.Builder
	b.Grow(len(addr) + 4)
	b.WriteByte('"')
	for i := 0; i < len(local); i++ {
		if c := local[i]; c == '"' || c == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(local[i])
	}
	b.WriteByte('"')
	b.WriteString(addr[at:])
	return b.String()
}

// Whether a local part is a dot-string of RFC 5321: atoms of atext, or of UTF-8 under SMTPUTF8, joined by single dots
func isDotString(s string) bool {
	if s == "" || s[0] == '.' || s[len(s)-1] == '.' || strings.Contains(s, "..") {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c >= 0x80, c == '.':
		case strings.IndexByte("!#$%&'*+-/=?^_`{|}~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// Ends the session with QUIT and closes the connection, which is closed even when the command fails
func (c *SMTP) Quit(ctx context.Context) error {
	err := c.run(ctx, func() error {
		_, _, err := c.cmd("QUIT")
		return err
	})
	if closeErr := c.Close(); err == nil {
		err = closeErr
	}
	return err
}

// Greets the server with EHLO, HELO when the server refuses that with a permanent error, unless this session already did
func (c *SMTP) greet() error {
	if c.hello {
		return nil
	}
	name := c.helloName()
	code, msg, err := c.cmd("EHLO " + name)
	if err != nil {
		return err
	}
	if code/100 == 2 {
		c.ext, c.hello = parseExtensions(msg), true
		return nil
	}
	if code/100 != 5 {
		return &SMTPError{Command: "EHLO", Code: code, Message: msg}
	}
	if code, msg, err = c.cmd("HELO " + name); err != nil {
		return err
	}
	if code/100 != 2 {
		return &SMTPError{Command: "HELO", Code: code, Message: msg}
	}
	c.ext, c.hello = map[string]string{}, true
	return nil
}

// Extensions an EHLO reply lists, keywords upper cased mapped to their parameters, the AUTH mechanisms of both spellings gathered
func parseExtensions(msg string) map[string]string {
	ext := map[string]string{}
	lines := strings.Split(msg, "\n")
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		keyword, params, _ := strings.Cut(line, " ")
		if k, old, ok := strings.Cut(keyword, "="); ok && strings.EqualFold(k, "AUTH") {
			keyword, params = "AUTH", strings.TrimSpace(old+" "+params)
		}
		if keyword == "" {
			continue
		}
		keyword = strings.ToUpper(keyword)
		params = strings.TrimSpace(params)
		if keyword == "AUTH" {
			params = strings.TrimSpace(ext["AUTH"] + " " + params)
		}
		ext[keyword] = params
	}
	return ext
}

// Name the client greets with: the domain name of this host, else the address literal of the connection's own end
func (c *SMTP) helloName() string {
	if name := hostDomain(); name != "" {
		return name
	}
	if local := c.raw.LocalAddr(); local != nil {
		if ap, err := netip.ParseAddrPort(local.String()); err == nil {
			addr := ap.Addr().Unmap().WithZone("")
			if addr.Is4() {
				return "[" + addr.String() + "]"
			}
			return "[IPv6:" + addr.String() + "]"
		}
	}
	return "[127.0.0.1]"
}

// Fully qualified domain name of this host, empty when it has none, looked up once per process
var hostDomain = sync.OnceValue(func() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	if !strings.Contains(name, ".") {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if addrs, err := net.DefaultResolver.LookupHost(ctx, name); err == nil && len(addrs) > 0 {
			if names, err := net.DefaultResolver.LookupAddr(ctx, addrs[0]); err == nil {
				for _, alias := range names {
					if alias = strings.TrimSuffix(alias, "."); strings.Contains(alias, ".") {
						name = alias
						break
					}
				}
			}
		}
	}
	name = strings.TrimSuffix(name, ".")
	if !strings.Contains(name, ".") {
		return ""
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.') {
			return ""
		}
	}
	return name
})

// Sends one command line and reads the reply
func (c *SMTP) cmd(line string) (int, string, error) {
	if err := c.checkLine("command", line); err != nil {
		return 0, "", err
	}
	_, _ = c.w.WriteString(line)
	_, _ = c.w.WriteString("\r\n")
	if err := c.flush(); err != nil {
		return 0, "", err
	}
	return c.reply()
}

// Code and text of the next reply, the lines of a multi line reply joined by line feeds
func (c *SMTP) reply() (int, string, error) {
	var lines []string
	for {
		line, err := c.readLine(maxSMTPLine)
		if err != nil {
			return 0, "", err
		}
		code, err := strconv.Atoi(line[:min(3, len(line))])
		if err != nil || len(line) < 3 || code < 100 || code > 599 {
			return 0, "", fmt.Errorf("smtp: malformed reply %q", line[:min(len(line), 200)])
		}
		more := len(line) > 3 && line[3] == '-'
		text := ""
		if len(line) > 3 {
			text = strings.TrimSpace(line[4:])
		}
		lines = append(lines, text)
		if !more {
			return code, strings.Join(lines, "\n"), nil
		}
		if len(lines) == maxReplyLines {
			return 0, "", fmt.Errorf("smtp: server sent a reply of more than %d lines", maxReplyLines)
		}
	}
}
