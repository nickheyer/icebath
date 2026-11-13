package email

import (
	"bufio"
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
	"time"

	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

const (
	connectTimeout = 30 * time.Second
	imapMaxLine    = 16 << 20
)

// Network connection whose every read and write must make progress within the timeout
type idleConn struct {
	net.Conn
	timeout time.Duration
}

func (c *idleConn) Read(b []byte) (int, error) {
	if err := c.Conn.SetReadDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Read(b)
}

func (c *idleConn) Write(b []byte) (int, error) {
	if err := c.Conn.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}

// TCP connection to the host over the network family within the connect timeout
func dialTCP(ctx context.Context, network, host string, port int) (net.Conn, error) {
	d := net.Dialer{Timeout: connectTimeout}
	return d.DialContext(ctx, network, net.JoinHostPort(host, strconv.Itoa(port)))
}

// Hosts already warned about running without certificate verification
var unverifiedWarned sync.Map

// Client TLS settings for the host, verification skipped only when the operator turned it off
func tlsConfig(host string, verify bool) *tls.Config {
	if !verify && host != "127.0.0.1" && host != "::1" && host != "localhost" {
		if _, seen := unverifiedWarned.LoadOrStore(host, true); !seen {
			slog.Warn("TLS verification disabled for non-loopback host " + host)
		}
	}
	return &tls.Config{ServerName: host, InsecureSkipVerify: !verify, MinVersion: tls.VersionTLS12}
}

// Argument sent as a synchronizing literal
type imapLiteral []byte

// One server response with its literals inline
type imapResponse struct {
	tag    string
	status string
	kind   string
	code   string
	text   string
	data   []byte
}

// Tagged NO or BAD completion of a command
type imapStatusError struct {
	command, status, text string
}

func (e *imapStatusError) Error() string {
	return fmt.Sprintf("IMAP %s failed: %s %s", e.command, e.status, e.text)
}

// Whether the error is a NO completion, the refusal the mailbox loop tolerates
func isIMAPNo(err error) bool {
	var se *imapStatusError
	return errors.As(err, &se) && se.status == "NO"
}

// One IMAP session over one connection
type imapClient struct {
	conn    net.Conn
	r       *bufio.Reader
	w       *bufio.Writer
	seq     int
	caps    map[string]bool
	preauth bool
	bye     string
	broken  bool
	release func() bool
}

// Session to the endpoint with TLS established as configured and capabilities known
func dialIMAP(ctx context.Context, ep endpoint) (*imapClient, error) {
	raw, err := dialTCP(ctx, "tcp", ep.host, ep.port)
	if err != nil {
		return nil, err
	}
	c := &imapClient{caps: map[string]bool{}, release: context.AfterFunc(ctx, func() { raw.Close() })}
	var conn net.Conn = &idleConn{Conn: raw, timeout: connectTimeout}
	c.attach(conn)
	if ep.security == "tls" {
		tc := tls.Client(conn, tlsConfig(ep.host, ep.verify))
		if err := tc.HandshakeContext(ctx); err != nil {
			c.close()
			return nil, err
		}
		c.attach(tc)
	}
	if err := c.setup(ctx, ep); err != nil {
		c.close()
		return nil, err
	}
	return c, nil
}

// Greeting, STARTTLS when configured and the capability list
func (c *imapClient) setup(ctx context.Context, ep endpoint) error {
	if err := c.greet(); err != nil {
		return err
	}
	if ep.security == "starttls" {
		if err := c.startTLS(ctx, ep); err != nil {
			return err
		}
	}
	if len(c.caps) == 0 {
		return c.capability()
	}
	return nil
}

// Buffered reader and writer over the connection
func (c *imapClient) attach(conn net.Conn) {
	c.conn, c.r, c.w = conn, bufio.NewReader(conn), bufio.NewWriter(conn)
}

// Closes the connection and drops the cancellation hook
func (c *imapClient) close() {
	c.release()
	c.conn.Close()
}

// Logs out when the session is still healthy, then closes the connection either way
func (c *imapClient) logout() {
	if !c.broken && c.bye == "" {
		if _, err := c.exec("LOGOUT"); err != nil {
			slog.Debug(fmt.Sprintf("[Email] IMAP logout failed: %v", err))
		}
	}
	c.close()
}

// Server greeting, PREAUTH recorded and BYE refused
func (c *imapClient) greet() error {
	r, err := c.next()
	if err != nil {
		return err
	}
	switch {
	case r.tag == "*" && r.status == "OK":
	case r.tag == "*" && r.status == "PREAUTH":
		c.preauth = true
	case r.tag == "*" && r.status == "BYE":
		return fmt.Errorf("IMAP server refused the connection: %s", r.text)
	default:
		return fmt.Errorf("unexpected IMAP greeting: %s %s %s", r.tag, r.status, r.text)
	}
	return nil
}

// Upgrades the plaintext session to TLS and reloads the capabilities the upgrade invalidates
func (c *imapClient) startTLS(ctx context.Context, ep endpoint) error {
	if c.preauth {
		return errors.New("IMAP server authenticated the connection before STARTTLS could protect it")
	}
	if len(c.caps) == 0 {
		if err := c.capability(); err != nil {
			return err
		}
	}
	if !c.caps["STARTTLS"] {
		return errors.New("IMAP server does not advertise STARTTLS")
	}
	if _, err := c.exec("STARTTLS"); err != nil {
		return err
	}
	if c.r.Buffered() > 0 {
		return errors.New("IMAP server sent data after accepting STARTTLS")
	}
	tc := tls.Client(c.conn, tlsConfig(ep.host, ep.verify))
	if err := tc.HandshakeContext(ctx); err != nil {
		return err
	}
	c.attach(tc)
	c.caps = map[string]bool{}
	return c.capability()
}

// Capability list from the server
func (c *imapClient) capability() error {
	_, err := c.exec("CAPABILITY")
	return err
}

// Replaces the known capabilities with the listed ones
func (c *imapClient) setCaps(fields []string) {
	c.caps = map[string]bool{}
	for _, f := range fields {
		c.caps[strings.ToUpper(f)] = true
	}
}

// Authenticates with LOGIN unless the server pre-authenticated, refusing where the server disables it
func (c *imapClient) login(user, password string) error {
	if c.preauth {
		return nil
	}
	if c.caps["LOGINDISABLED"] {
		return errors.New("IMAP server disables LOGIN on this connection; set EMAIL_IMAP_SECURITY to tls or starttls")
	}
	_, err := c.exec("LOGIN", imapString(user), imapString(password))
	return err
}

// RFC 2971 client identification some providers require after login, sent only when advertised
func (c *imapClient) sendID() {
	if !c.caps["ID"] {
		slog.Debug("[Email] Server does not advertise IMAP ID capability; skipping ID")
		return
	}
	version := "0"
	if info, err := updaterecord.GetVersionInfo(); err == nil && info.BaseVersion != "" {
		version = info.BaseVersion
	}
	params := fmt.Sprintf(`("name" "icebath" "version" %s "vendor" "heyer.app" "support-url" "https://icebath.app")`, imapQuote(version))
	if _, err := c.exec("ID", params); err != nil {
		slog.Debug(fmt.Sprintf("[Email] IMAP ID command not accepted: %v", err))
	}
}

// Opens the mailbox read write
func (c *imapClient) selectMailbox(name string) error {
	_, err := c.exec("SELECT", imapString(name))
	return err
}

// UIDs matching the search key
func (c *imapClient) uidSearch(criteria string) ([]uint32, error) {
	rs, err := c.exec("UID SEARCH", criteria)
	if err != nil {
		return nil, err
	}
	var uids []uint32
	for _, r := range rs {
		if r.tag != "*" || r.kind != "SEARCH" {
			continue
		}
		for _, f := range strings.Fields(string(r.data)) {
			if strings.HasPrefix(f, "(") {
				break
			}
			if n, err := strconv.ParseUint(f, 10, 32); err == nil && n > 0 {
				uids = append(uids, uint32(n))
			}
		}
	}
	return uids, nil
}

// Value of the first attribute the selector accepts in the FETCH data of the UID, nil when the server sent none
func (c *imapClient) uidFetch(uid uint32, items string, want func(name string) bool) ([]byte, error) {
	id := strconv.FormatUint(uint64(uid), 10)
	rs, err := c.exec("UID FETCH", id, items)
	if err != nil {
		return nil, err
	}
	for _, r := range rs {
		if r.tag != "*" || r.kind != "FETCH" {
			continue
		}
		atts, perr := parseFetchAttributes(r.data)
		if perr != nil {
			slog.Debug(fmt.Sprintf("[Email] Unparseable FETCH data for UID %d: %v", uid, perr))
			continue
		}
		if got, ok := atts["UID"].(string); ok && got != id {
			continue
		}
		for name, v := range atts {
			if want(name) {
				b, _ := v.([]byte)
				return b, nil
			}
		}
	}
	return nil, nil
}

// Adds the seen flag to the UID without an untagged echo
func (c *imapClient) uidStoreSeen(uid uint32) error {
	_, err := c.exec("UID STORE", strconv.FormatUint(uint64(uid), 10), "+FLAGS.SILENT", `(\Seen)`)
	return err
}

// Quoted string with backslash escapes
func imapQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// Quoted string for printable ASCII, a literal for anything else
func imapString(s string) any {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return imapLiteral(s)
		}
	}
	return imapQuote(s)
}

// Writes the bytes and flushes, marking the session broken on failure
func (c *imapClient) write(b []byte) error {
	if _, err := c.w.Write(b); err != nil {
		c.broken = true
		return err
	}
	if err := c.w.Flush(); err != nil {
		c.broken = true
		return err
	}
	return nil
}

// Sends the tagged command, literals waiting for continuation, and returns its untagged responses
func (c *imapClient) exec(verb string, args ...any) ([]*imapResponse, error) {
	c.seq++
	tag := "I" + strconv.Itoa(c.seq)
	var got []*imapResponse
	line := []byte(tag + " " + verb)
	for _, arg := range args {
		line = append(line, ' ')
		switch v := arg.(type) {
		case string:
			line = append(line, v...)
		case imapLiteral:
			line = append(line, '{')
			line = strconv.AppendInt(line, int64(len(v)), 10)
			line = append(line, "}\r\n"...)
			if err := c.write(line); err != nil {
				return got, err
			}
			more, err := c.await(tag, verb, true)
			got = append(got, more...)
			if err != nil {
				return got, err
			}
			line = append(line[:0], v...)
		}
	}
	line = append(line, '\r', '\n')
	if err := c.write(line); err != nil {
		return got, err
	}
	more, err := c.await(tag, verb, false)
	return append(got, more...), err
}

// Reads until the tagged completion, or until a continuation when one is awaited
func (c *imapClient) await(tag, verb string, continuation bool) ([]*imapResponse, error) {
	var got []*imapResponse
	for {
		r, err := c.next()
		if err != nil {
			return got, err
		}
		switch r.tag {
		case "+":
			if continuation {
				return got, nil
			}
		case tag:
			if continuation && r.status == "OK" {
				return got, fmt.Errorf("IMAP %s completed before its literal was sent", verb)
			}
			return got, completion(r, verb)
		default:
			got = append(got, r)
		}
	}
}

// Nil for an OK completion, the status error otherwise
func completion(r *imapResponse, verb string) error {
	if r.status == "OK" {
		return nil
	}
	text := r.text
	if r.code != "" {
		text = "[" + r.code + "] " + text
	}
	return &imapStatusError{command: verb, status: r.status, text: text}
}

// Next response, tracking BYE and capability announcements
func (c *imapClient) next() (*imapResponse, error) {
	raw, err := c.readResponse()
	if err != nil {
		c.broken = true
		if c.bye != "" {
			return nil, fmt.Errorf("IMAP server closed the connection: %s", c.bye)
		}
		return nil, err
	}
	r := parseIMAPResponse(raw)
	if r.tag == "*" && r.status == "BYE" {
		c.bye = strings.TrimSpace("[" + r.code + "] " + r.text)
		if r.code == "" {
			c.bye = r.text
		}
	}
	if r.tag == "*" && r.kind == "CAPABILITY" {
		c.setCaps(strings.Fields(string(r.data)))
	}
	if name, list, ok := strings.Cut(r.code, " "); ok && strings.EqualFold(name, "CAPABILITY") {
		c.setCaps(strings.Fields(list))
	}
	return r, nil
}

// One line without its terminator, bounded so a broken server cannot grow it without limit
func (c *imapClient) readLine() ([]byte, error) {
	var line []byte
	for {
		chunk, err := c.r.ReadSlice('\n')
		line = append(line, chunk...)
		if err == nil {
			break
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
		if len(line) > imapMaxLine {
			return nil, errors.New("IMAP response line is longer than 16 MiB")
		}
	}
	line = bytes.TrimSuffix(line, []byte("\n"))
	return bytes.TrimSuffix(line, []byte("\r")), nil
}

// Length of the literal announced at the end of the line
func literalLength(line []byte) (int, bool) {
	if len(line) < 3 || line[len(line)-1] != '}' {
		return 0, false
	}
	open := bytes.LastIndexByte(line, '{')
	if open < 0 || open+1 >= len(line)-1 {
		return 0, false
	}
	n, err := strconv.ParseUint(string(line[open+1:len(line)-1]), 10, 31)
	if err != nil {
		return 0, false
	}
	return int(n), true
}

// One complete response with each announced literal read into place after its CRLF
func (c *imapClient) readResponse() ([]byte, error) {
	var buf []byte
	for {
		line, err := c.readLine()
		if err != nil {
			return nil, err
		}
		buf = append(buf, line...)
		n, ok := literalLength(line)
		if !ok {
			return buf, nil
		}
		buf = append(buf, '\r', '\n')
		start := len(buf)
		buf = append(buf, make([]byte, n)...)
		if _, err := io.ReadFull(c.r, buf[start:]); err != nil {
			return nil, err
		}
	}
}

// Tag, status or data kind, response code and text of one response
func parseIMAPResponse(raw []byte) *imapResponse {
	r := &imapResponse{}
	tag, rest, _ := bytes.Cut(raw, []byte(" "))
	r.tag = string(tag)
	if r.tag == "+" {
		r.text = string(rest)
		return r
	}
	word, after, _ := bytes.Cut(rest, []byte(" "))
	if r.tag == "*" {
		if _, err := strconv.ParseUint(string(word), 10, 32); err == nil {
			kind, data, _ := bytes.Cut(after, []byte(" "))
			r.kind, r.data = strings.ToUpper(string(kind)), data
			return r
		}
	}
	switch upper := strings.ToUpper(string(word)); upper {
	case "OK", "NO", "BAD", "BYE", "PREAUTH":
		r.status = upper
		if bytes.HasPrefix(after, []byte("[")) {
			if end := bytes.IndexByte(after, ']'); end > 0 {
				r.code, after = string(after[1:end]), bytes.TrimLeft(after[end+1:], " ")
			}
		}
		r.text = validUTF8(after)
	default:
		r.kind, r.data = upper, after
	}
	return r
}

// Cursor over FETCH data with literals inline
type imapParser struct {
	b []byte
	i int
}

// Whether the cursor reached the end
func (p *imapParser) done() bool { return p.i >= len(p.b) }

// Skips spaces
func (p *imapParser) skipSpaces() {
	for p.i < len(p.b) && p.b[p.i] == ' ' {
		p.i++
	}
}

// Consumes the byte when it is next
func (p *imapParser) consume(c byte) bool {
	if p.i < len(p.b) && p.b[p.i] == c {
		p.i++
		return true
	}
	return false
}

// Attribute name to upper case keyed values of one FETCH response
func parseFetchAttributes(b []byte) (map[string]any, error) {
	p := &imapParser{b: b}
	p.skipSpaces()
	if !p.consume('(') {
		return nil, errors.New("FETCH data is not a list")
	}
	atts := map[string]any{}
	for {
		p.skipSpaces()
		if p.done() || p.consume(')') {
			return atts, nil
		}
		name := p.attName()
		if name == "" {
			return atts, fmt.Errorf("malformed FETCH attribute at offset %d", p.i)
		}
		p.skipSpaces()
		v, err := p.value()
		if err != nil {
			return atts, err
		}
		atts[strings.ToUpper(name)] = v
	}
}

// Attribute name including its section brackets and partial origin
func (p *imapParser) attName() string {
	start := p.i
	for p.i < len(p.b) {
		switch p.b[p.i] {
		case '[':
			p.skipBracket()
		case '<':
			if end := bytes.IndexByte(p.b[p.i:], '>'); end >= 0 {
				p.i += end + 1
			} else {
				p.i = len(p.b)
			}
		case ' ', '(', ')':
			return string(p.b[start:p.i])
		default:
			p.i++
		}
	}
	return string(p.b[start:p.i])
}

// Skips a bracketed section, nested brackets and quoted strings included
func (p *imapParser) skipBracket() {
	depth := 0
	for p.i < len(p.b) {
		switch p.b[p.i] {
		case '"':
			_, _ = p.quoted()
			continue
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				p.i++
				return
			}
		}
		p.i++
	}
}

// NIL as nil, strings and literals as bytes, lists as slices and atoms as text
func (p *imapParser) value() (any, error) {
	if p.done() {
		return nil, errors.New("missing FETCH value")
	}
	switch p.b[p.i] {
	case '"':
		return p.quoted()
	case '{':
		return p.literal()
	case '~':
		p.i++
		return p.literal()
	case '(':
		p.i++
		list := []any{}
		for {
			p.skipSpaces()
			if p.done() {
				return list, errors.New("unterminated FETCH list")
			}
			if p.consume(')') {
				return list, nil
			}
			v, err := p.value()
			if err != nil {
				return list, err
			}
			list = append(list, v)
		}
	}
	start := p.i
	for p.i < len(p.b) && p.b[p.i] != ' ' && p.b[p.i] != '(' && p.b[p.i] != ')' {
		if p.b[p.i] == '[' {
			p.skipBracket()
			continue
		}
		p.i++
	}
	atom := string(p.b[start:p.i])
	if strings.EqualFold(atom, "NIL") {
		return nil, nil
	}
	return atom, nil
}

// Quoted string with its escapes removed
func (p *imapParser) quoted() ([]byte, error) {
	p.i++
	out := []byte{}
	for p.i < len(p.b) {
		c := p.b[p.i]
		switch {
		case c == '\\' && p.i+1 < len(p.b):
			out = append(out, p.b[p.i+1])
			p.i += 2
		case c == '"':
			p.i++
			return out, nil
		default:
			out = append(out, c)
			p.i++
		}
	}
	return out, errors.New("unterminated quoted string")
}

// Literal bytes after their length announcement and CRLF
func (p *imapParser) literal() ([]byte, error) {
	end := bytes.IndexByte(p.b[p.i:], '}')
	if !p.consume('{') || end < 1 {
		return nil, errors.New("malformed literal")
	}
	n, err := strconv.Atoi(strings.TrimSuffix(string(p.b[p.i:p.i+end-1]), "+"))
	if err != nil || n < 0 {
		return nil, errors.New("malformed literal length")
	}
	p.i += end
	if !bytes.HasPrefix(p.b[p.i:], []byte("\r\n")) || p.i+2+n > len(p.b) {
		return nil, errors.New("truncated literal")
	}
	p.i += 2
	out := p.b[p.i : p.i+n : p.i+n]
	p.i += n
	return out, nil
}
