// Package pysmtplib is an SMTP client with the command flow and error behaviour of CPython smtplib
package pysmtplib

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyemail"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrequests"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	smtpPort     = 25
	smtpSSLPort  = 465
	maxLine      = 8192
	maxChallenge = 5
)

// Error is a raised smtplib or socket exception, Class its class name and Text its str
type Error struct {
	Class string
	Text  string
}

func (e *Error) Error() string      { return e.Text }
func (e *Error) PyTypeName() string { return e.Class }

// Whether the error is an ssl.SSLError, the one failure the IPv4 fallback never retries
func (e *Error) IsSSL() bool { return e.Class == "SSLError" || e.Class == "SSLCertVerificationError" }

func plainError(class, text string) *Error { return &Error{Class: class, Text: text} }

// Exception carrying a reply code and message, its str the tuple of both
func responseError(class string, code int, msg []byte) *Error {
	return &Error{Class: class, Text: fmt.Sprintf("(%d, %s)", code, pyrepr.BytesRepr(msg))}
}

// OSError of a failed socket call, a timeout reading as the Python TimeoutError
func socketError(err error) *Error {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return plainError("TimeoutError", "timed out")
	}
	return plainError("OSError", pyhttpx.OSErrorText(err))
}

// Dialer opens the TCP connection of a session, the timeout zero for a blocking socket
type Dialer func(ctx context.Context, host string, port int, timeout time.Duration) (net.Conn, error)

// Connects to the first address of the host that answers, any family
func DialDefault(ctx context.Context, host string, port int, timeout time.Duration) (net.Conn, error) {
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, socketError(err)
	}
	return conn, nil
}

// Connects through IPv4 addresses only, the last failure raised when none answers
func DialIPv4(ctx context.Context, host string, port int, timeout time.Duration) (net.Conn, error) {
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, socketError(err)
	}
	var last error
	for _, ip := range ips {
		dialer := net.Dialer{Timeout: timeout}
		conn, err := dialer.DialContext(ctx, "tcp4", net.JoinHostPort(ip.String(), strconv.Itoa(port)))
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		last = socketError(err)
	}
	if last != nil {
		return nil, last
	}
	return nil, plainError("OSError", fmt.Sprintf("No IPv4 address found for %s:%d", host, port))
}

// Fully qualified name of the local host, the bare host name when no alias carries a dot
func getfqdn() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	addrs, err := net.LookupHost(name)
	if err != nil || len(addrs) == 0 {
		return name
	}
	names, err := net.LookupAddr(addrs[0])
	if err != nil || len(names) == 0 {
		return name
	}
	for _, alias := range names {
		if alias = strings.TrimSuffix(alias, "."); strings.Contains(alias, ".") {
			return alias
		}
	}
	return strings.TrimSuffix(names[0], ".")
}

// Name sent with EHLO, the host FQDN or its bracketed IPv4 address
func localHostname() string {
	if fqdn := getfqdn(); strings.Contains(fqdn, ".") {
		return fqdn
	}
	addr := "127.0.0.1"
	if name, err := os.Hostname(); err == nil {
		if ips, err := net.DefaultResolver.LookupIP(context.Background(), "ip4", name); err == nil && len(ips) > 0 {
			addr = ips[0].String()
		}
	}
	return "[" + addr + "]"
}

// One SMTP session with its negotiated extensions
type SMTP struct {
	conn          net.Conn
	file          *bufio.Reader
	timeout       time.Duration
	host          string
	localHostname string
	heloResp      []byte
	ehloResp      []byte
	hasHelo       bool
	hasEhlo       bool
	doesESMTP     bool
	features      map[string]string

	user, password     string
	authChallengeCount int
}

// Opens a session, over implicit TLS when a config is given, and checks the 220 greeting
func New(ctx context.Context, host string, port int, timeout time.Duration, dial Dialer, tlsConfig *tls.Config) (*SMTP, error) {
	s := &SMTP{timeout: timeout, features: map[string]string{}}
	if host != "" {
		defaultPort := smtpPort
		if tlsConfig != nil {
			defaultPort = smtpSSLPort
		}
		code, msg, err := s.connect(ctx, host, port, defaultPort, dial, tlsConfig)
		if err != nil {
			return nil, err
		}
		if code != 220 {
			s.Close()
			return nil, responseError("SMTPConnectError", code, msg)
		}
	}
	s.localHostname = localHostname()
	return s, nil
}

// Connects the socket, wraps it in TLS when asked and reads the greeting
func (s *SMTP) connect(ctx context.Context, host string, port, defaultPort int, dial Dialer, tlsConfig *tls.Config) (int, []byte, error) {
	if port == 0 && strings.Index(host, ":") == strings.LastIndex(host, ":") {
		if i := strings.LastIndex(host, ":"); i >= 0 {
			n, ok := parsePort(host[i+1:])
			if !ok {
				return 0, nil, plainError("OSError", "nonnumeric port")
			}
			host, port = host[:i], n
		}
	}
	s.host = host
	if port == 0 {
		port = defaultPort
	}
	conn, err := dial(ctx, host, port, s.timeout)
	if err != nil {
		return 0, nil, err
	}
	s.conn = conn
	if tlsConfig != nil {
		if err := s.wrapTLS(ctx, tlsConfig); err != nil {
			conn.Close()
			s.conn = nil
			return 0, nil, err
		}
	}
	s.file = nil
	return s.getreply()
}

// Python int of a port text
func parsePort(text string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(strings.ReplaceAll(text, "_", "")))
	return n, err == nil
}

// Runs the TLS handshake over the current socket with the server name of the host
func (s *SMTP) wrapTLS(ctx context.Context, config *tls.Config) error {
	cfg := config.Clone()
	if cfg.ServerName == "" {
		cfg.ServerName = s.host
	}
	secured := tls.Client(s.conn, cfg)
	hctx := ctx
	if s.timeout > 0 {
		var cancel context.CancelFunc
		hctx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}
	if err := secured.HandshakeContext(hctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var netErr net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
			return plainError("TimeoutError", "_ssl.c:1015: The handshake operation timed out")
		}
		return plainError("SSLError", pyrequests.SSLErrorText(err))
	}
	s.conn = secured
	return nil
}

// Sends raw bytes, a failed write closing the session
func (s *SMTP) send(data []byte) error {
	if s.conn == nil {
		return plainError("SMTPServerDisconnected", "please run connect() first")
	}
	if s.timeout > 0 {
		_ = s.conn.SetWriteDeadline(time.Now().Add(s.timeout))
	}
	if _, err := s.conn.Write(data); err != nil {
		s.Close()
		return plainError("SMTPServerDisconnected", "Server not connected")
	}
	return nil
}

// Sends one command line, line breaks inside it refused
func (s *SMTP) putcmd(cmd, args string) error {
	line := cmd
	if args != "" {
		line = cmd + " " + args
	}
	if strings.ContainsAny(line, "\r\n") {
		shown := strings.ReplaceAll(strings.ReplaceAll(line, "\n", `\n`), "\r", `\r`)
		return plainError("ValueError", "command and arguments contain prohibited newline characters: "+shown)
	}
	if _, err := pycodecs.Encode(line, "ascii"); err != nil {
		return err
	}
	return s.send([]byte(line + "\r\n"))
}

// Reads one reply, a multi line one joined by line feeds, the code minus one when it does not parse
func (s *SMTP) getreply() (int, []byte, error) {
	if s.conn == nil {
		return 0, nil, plainError("SMTPServerDisconnected", "please run connect() first")
	}
	if s.file == nil {
		s.file = bufio.NewReaderSize(s.conn, maxLine+2)
	}
	var resp [][]byte
	errcode := -1
	for {
		if s.timeout > 0 {
			_ = s.conn.SetReadDeadline(time.Now().Add(s.timeout))
		}
		line, err := s.readLine()
		if err != nil {
			s.Close()
			return 0, nil, plainError("SMTPServerDisconnected", "Connection unexpectedly closed: "+socketError(err).Text)
		}
		if len(line) == 0 {
			s.Close()
			return 0, nil, plainError("SMTPServerDisconnected", "Connection unexpectedly closed")
		}
		if len(line) > maxLine {
			s.Close()
			return 0, nil, responseError("SMTPResponseException", 500, []byte("Line too long."))
		}
		tail := []byte{}
		if len(line) > 4 {
			tail = bytes.Trim(line[4:], " \t\r\n")
		}
		resp = append(resp, tail)
		code, ok := parsePort(string(line[:min(3, len(line))]))
		if !ok {
			errcode = -1
			break
		}
		errcode = code
		if len(line) < 4 || line[3] != '-' {
			break
		}
	}
	return errcode, bytes.Join(resp, []byte("\n")), nil
}

// Next reply line with its terminator, at most one byte past the line cap, empty at the end of the stream
func (s *SMTP) readLine() ([]byte, error) {
	var line []byte
	for len(line) <= maxLine {
		b, err := s.file.ReadByte()
		if err != nil {
			if len(line) > 0 || errors.Is(err, io.EOF) {
				return line, nil
			}
			return nil, err
		}
		line = append(line, b)
		if b == '\n' {
			break
		}
	}
	return line, nil
}

// Sends a command and returns its reply
func (s *SMTP) docmd(cmd, args string) (int, []byte, error) {
	if err := s.putcmd(cmd, args); err != nil {
		return 0, nil, err
	}
	return s.getreply()
}

func (s *SMTP) helo() (int, []byte, error) {
	code, msg, err := s.docmd("helo", s.localHostname)
	if err != nil {
		return 0, nil, err
	}
	s.heloResp, s.hasHelo = msg, true
	return code, msg, nil
}

var (
	oldstyleAuthRE = regexp.MustCompile(`(?i)^auth=(.*)`)
	featureRE      = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9\-]*) ?`)
)

// Sends EHLO and records the extensions the server lists
func (s *SMTP) ehlo() (int, []byte, error) {
	s.features = map[string]string{}
	code, msg, err := s.docmd("ehlo", s.localHostname)
	if err != nil {
		return 0, nil, err
	}
	if code == -1 && len(msg) == 0 {
		s.Close()
		return 0, nil, plainError("SMTPServerDisconnected", "Server not connected")
	}
	s.ehloResp, s.hasEhlo = msg, true
	if code != 250 {
		return code, msg, nil
	}
	s.doesESMTP = true
	lines := strings.Split(latin1(msg), "\n")
	for _, each := range lines[1:] {
		if m := oldstyleAuthRE.FindStringSubmatch(each); m != nil {
			s.features["auth"] = s.features["auth"] + " " + m[1]
			continue
		}
		if m := featureRE.FindStringSubmatch(each); m != nil {
			feature := strings.ToLower(m[1])
			params := pystr.Strip(each[len(m[1]):])
			if feature == "auth" {
				s.features[feature] = s.features[feature] + " " + params
			} else {
				s.features[feature] = params
			}
		}
	}
	return code, msg, nil
}

// Text of reply bytes under latin-1
func latin1(data []byte) string {
	rs := make([]rune, len(data))
	for i, b := range data {
		rs[i] = rune(b)
	}
	return string(rs)
}

func (s *SMTP) hasExtn(opt string) bool {
	_, ok := s.features[strings.ToLower(opt)]
	return ok
}

// Sends RSET, a dropped connection left for the next command to report
func (s *SMTP) rset() {
	_, _, _ = s.docmd("rset", "")
}

// Greets the server with EHLO, falling back to HELO, unless one already ran this session
func (s *SMTP) ehloOrHeloIfNeeded() error {
	if s.hasHelo || s.hasEhlo {
		return nil
	}
	code, _, err := s.ehlo()
	if err != nil {
		return err
	}
	if code >= 200 && code <= 299 {
		return nil
	}
	code, resp, err := s.helo()
	if err != nil {
		return err
	}
	if code < 200 || code > 299 {
		return responseError("SMTPHeloError", code, resp)
	}
	return nil
}

// One AUTH exchange with a mechanism, the server challenges answered by the mechanism's responder
func (s *SMTP) auth(mechanism string, authobject func(challenge []byte) (string, bool, error)) (int, []byte, error) {
	encode := func(text string) (string, error) {
		if _, err := pycodecs.Encode(text, "ascii"); err != nil {
			return "", err
		}
		return base64.StdEncoding.EncodeToString([]byte(text)), nil
	}
	initial, has, err := authobject(nil)
	if err != nil {
		return 0, nil, err
	}
	var code int
	var resp []byte
	if has {
		response, err := encode(initial)
		if err != nil {
			return 0, nil, err
		}
		if code, resp, err = s.docmd("AUTH", mechanism+" "+response); err != nil {
			return 0, nil, err
		}
		s.authChallengeCount = 1
	} else {
		if code, resp, err = s.docmd("AUTH", mechanism); err != nil {
			return 0, nil, err
		}
		s.authChallengeCount = 0
	}
	for code == 334 {
		s.authChallengeCount++
		challenge, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(resp)))
		if err != nil {
			return 0, nil, plainError("Error", "Incorrect padding")
		}
		answer, _, err := authobject(challenge)
		if err != nil {
			return 0, nil, err
		}
		response, err := encode(answer)
		if err != nil {
			return 0, nil, err
		}
		if code, resp, err = s.docmd(response, ""); err != nil {
			return 0, nil, err
		}
		if s.authChallengeCount > maxChallenge {
			return 0, nil, plainError("SMTPException", fmt.Sprintf("Server AUTH mechanism infinite loop. Last response: (%d, %s)",
				code, pyrepr.BytesRepr(resp)))
		}
	}
	if code == 235 || code == 503 {
		return code, resp, nil
	}
	return 0, nil, responseError("SMTPAuthenticationError", code, resp)
}

// CRAM-MD5 responder, nothing to send before the challenge
func (s *SMTP) authCramMD5(challenge []byte) (string, bool, error) {
	if challenge == nil {
		return "", false, nil
	}
	if _, err := pycodecs.Encode(s.password, "ascii"); err != nil {
		return "", false, err
	}
	mac := hmac.New(md5.New, []byte(s.password))
	mac.Write(challenge)
	return s.user + " " + hex.EncodeToString(mac.Sum(nil)), true, nil
}

// PLAIN responder, the identity sent with the AUTH command itself
func (s *SMTP) authPlain(challenge []byte) (string, bool, error) {
	return "\x00" + s.user + "\x00" + s.password, true, nil
}

// LOGIN responder, the user first and the password on the second challenge
func (s *SMTP) authLogin(challenge []byte) (string, bool, error) {
	if challenge == nil || s.authChallengeCount < 2 {
		return s.user, true, nil
	}
	return s.password, true, nil
}

// Logs in with the first of CRAM-MD5, PLAIN and LOGIN the server advertises and accepts
func (s *SMTP) Login(user, password string) error {
	if err := s.ehloOrHeloIfNeeded(); err != nil {
		return err
	}
	if !s.hasExtn("auth") {
		return plainError("SMTPNotSupportedError", "SMTP AUTH extension not supported by server.")
	}
	advertised := map[string]bool{}
	for _, name := range strings.Fields(s.features["auth"]) {
		advertised[name] = true
	}
	s.user, s.password = user, password
	responders := []struct {
		name string
		fn   func([]byte) (string, bool, error)
	}{{"CRAM-MD5", s.authCramMD5}, {"PLAIN", s.authPlain}, {"LOGIN", s.authLogin}}
	var last error
	tried := false
	for _, method := range responders {
		if !advertised[method.name] {
			continue
		}
		tried = true
		_, _, err := s.auth(method.name, method.fn)
		if err == nil {
			return nil
		}
		var smtpErr *Error
		if !errors.As(err, &smtpErr) || smtpErr.Class != "SMTPAuthenticationError" {
			return err
		}
		last = err
	}
	if !tried {
		return plainError("SMTPException", "No suitable authentication method found.")
	}
	return last
}

// Puts the session into TLS mode and forgets the plain text greeting state
func (s *SMTP) Starttls(ctx context.Context, tlsConfig *tls.Config) error {
	if err := s.ehloOrHeloIfNeeded(); err != nil {
		return err
	}
	if !s.hasExtn("starttls") {
		return plainError("SMTPNotSupportedError", "STARTTLS extension not supported by server.")
	}
	code, reply, err := s.docmd("STARTTLS", "")
	if err != nil {
		return err
	}
	if code != 220 {
		return responseError("SMTPResponseException", code, reply)
	}
	if err := s.wrapTLS(ctx, tlsConfig); err != nil {
		return err
	}
	s.file = nil
	s.heloResp, s.ehloResp, s.hasHelo, s.hasEhlo = nil, nil, false, false
	s.features, s.doesESMTP = map[string]string{}, false
	return nil
}

// Address in angle brackets for a MAIL or RCPT command
func quoteaddr(addrstring string) string {
	parsed := pyemail.ParseAddr(addrstring)
	if parsed.Name == "" && parsed.Addr == "" {
		if strings.HasPrefix(pystr.Strip(addrstring), "<") {
			return addrstring
		}
		return "<" + addrstring + ">"
	}
	return "<" + parsed.Addr + ">"
}

var leadingDotRE = regexp.MustCompile(`(?m)^\.`)

// Runs one mail transaction, every refused recipient or a refused sender or body raised
func (s *SMTP) sendmail(fromAddr string, toAddrs []string, msg []byte) error {
	if err := s.ehloOrHeloIfNeeded(); err != nil {
		return err
	}
	options := ""
	if s.doesESMTP && s.hasExtn("size") {
		options = " " + fmt.Sprintf("size=%d", len(msg))
	}
	code, resp, err := s.docmd("mail", "from:"+quoteaddr(fromAddr)+options)
	if err != nil {
		return err
	}
	if code != 250 {
		if code == 421 {
			s.Close()
		} else {
			s.rset()
		}
		return &Error{Class: "SMTPSenderRefused", Text: fmt.Sprintf("(%d, %s, %s)", code, pyrepr.BytesRepr(resp), pyrepr.Repr(fromAddr))}
	}
	var refused []string
	refusedText := func() string { return "{" + strings.Join(refused, ", ") + "}" }
	for _, each := range toAddrs {
		code, resp, err := s.docmd("rcpt", "to:"+quoteaddr(each))
		if err != nil {
			return err
		}
		if code != 250 && code != 251 {
			refused = append(refused, fmt.Sprintf("%s: (%d, %s)", pyrepr.Repr(each), code, pyrepr.BytesRepr(resp)))
		}
		if code == 421 {
			s.Close()
			return plainError("SMTPRecipientsRefused", refusedText())
		}
	}
	if len(refused) == len(toAddrs) {
		s.rset()
		return plainError("SMTPRecipientsRefused", refusedText())
	}
	code, resp, err = s.data(msg)
	if err != nil {
		return err
	}
	if code != 250 {
		if code == 421 {
			s.Close()
		} else {
			s.rset()
		}
		return responseError("SMTPDataError", code, resp)
	}
	return nil
}

// Sends the message body after DATA with leading periods doubled and the closing period line
func (s *SMTP) data(msg []byte) (int, []byte, error) {
	code, repl, err := s.docmd("data", "")
	if err != nil {
		return 0, nil, err
	}
	if code != 354 {
		return 0, nil, responseError("SMTPDataError", code, repl)
	}
	q := leadingDotRE.ReplaceAll(msg, []byte(".."))
	if !bytes.HasSuffix(q, []byte("\r\n")) {
		q = append(q, '\r', '\n')
	}
	q = append(q, '.', '\r', '\n')
	if err := s.send(q); err != nil {
		return 0, nil, err
	}
	return s.getreply()
}

// Serializes a message and sends it from its sender header to the addresses of its To, Bcc and Cc headers
func (s *SMTP) SendMessage(msg *pyemail.Part) error {
	if err := s.ehloOrHeloIfNeeded(); err != nil {
		return err
	}
	prefix := ""
	switch resent := msg.GetAll("Resent-Date"); {
	case len(resent) == 1:
		prefix = "Resent-"
	case len(resent) > 1:
		return plainError("ValueError", "message has more than one 'Resent-' header block")
	}
	header := func(name string) (string, bool) { return msg.Get(prefix + name) }
	fromField, ok := header("Sender")
	if !ok {
		if fromField, ok = header("From"); !ok {
			fromField = "None"
		}
	}
	fromAddr := pyemail.GetAddresses([]string{fromField})[0].Addr
	var addrFields []string
	for _, name := range []string{"To", "Bcc", "Cc"} {
		if value, ok := header(name); ok {
			addrFields = append(addrFields, value)
		}
	}
	var toAddrs []string
	for _, a := range pyemail.GetAddresses(addrFields) {
		toAddrs = append(toAddrs, a.Addr)
	}
	if _, err := pycodecs.Encode(fromAddr+strings.Join(toAddrs, ""), "ascii"); err != nil {
		if !s.hasExtn("smtputf8") {
			return plainError("SMTPNotSupportedError", "One or more source or delivery addresses require internationalized email support, "+
				"but the server does not advertise the required SMTPUTF8 capability")
		}
		return plainError("TypeError", "'utf8' is an invalid keyword argument for Compat32")
	}
	flat, err := msg.Without("Bcc", "Resent-Bcc").AsBytes()
	if err != nil {
		return err
	}
	return s.sendmail(fromAddr, toAddrs, flat)
}

// Closes the connection
func (s *SMTP) Close() {
	s.file = nil
	if conn := s.conn; conn != nil {
		s.conn = nil
		_ = conn.Close()
	}
}

// Ends the session with QUIT and closes the connection
func (s *SMTP) Quit() error {
	_, _, err := s.docmd("quit", "")
	if err != nil {
		return err
	}
	s.ehloResp, s.heloResp, s.hasEhlo, s.hasHelo = nil, nil, false, false
	s.features, s.doesESMTP = map[string]string{}, false
	s.Close()
	return nil
}
