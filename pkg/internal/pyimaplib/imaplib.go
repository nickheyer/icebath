// Package pyimaplib is an IMAP4 client with the command, response and error behaviour of CPython imaplib
package pyimaplib

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrequests"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const maxLine = 1000000

// States each command is legal in, extension commands added by Xatom
var commands = struct {
	sync.Mutex
	states map[string][]string
}{states: map[string][]string{
	"CAPABILITY": {"NONAUTH", "AUTH", "SELECTED", "LOGOUT"}, "FETCH": {"SELECTED"}, "LOGIN": {"NONAUTH"},
	"LOGOUT": {"NONAUTH", "AUTH", "SELECTED", "LOGOUT"}, "SEARCH": {"SELECTED"}, "SELECT": {"AUTH", "SELECTED"},
	"STARTTLS": {"NONAUTH"}, "STORE": {"SELECTED"}, "UID": {"SELECTED"},
}}

var (
	continuationRE    = regexp.MustCompile(`^\+( (?P<data>.*))?`)
	literalRE         = regexp.MustCompile(`^.*{(?P<size>[0-9]+)}$`)
	responseCodeRE    = regexp.MustCompile(`^\[(?P<type>[A-Z-]+)( (?P<data>.*))?\]`)
	untaggedRE        = regexp.MustCompile(`^\* (?P<type>[A-Z-]+)( (?P<data>.*))?`)
	untaggedStatusRE  = regexp.MustCompile(`^\* (?P<data>[0-9]+) (?P<type>[A-Z-]+)( (?P<data2>.*))?`)
	controlCharsRE    = regexp.MustCompile("[\x00\r\n]")
	nonAstringCharRE  = regexp.MustCompile(`[(){ \x00-\x1f\x7f-\xff%*\\"]`)
	quotedRE          = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	quotedFullMatchRE = regexp.MustCompile(`^"(?:[^"\\]|\\.)*"$`)
)

// Error is an IMAP4.error, IMAP4.abort or IMAP4.readonly raised by a command, its str the message
type Error struct {
	Class string
	Msg   string
}

func (e *Error) Error() string      { return e.Msg }
func (e *Error) PyTypeName() string { return e.Class }

// Whether the error is an abort, a service failure that needs a fresh connection
func (e *Error) IsAbort() bool { return e.Class == "abort" || e.Class == "readonly" }

func imapError(format string, args ...any) *Error { return &Error{Class: "error", Msg: fmt.Sprintf(format, args...)} }
func imapAbort(format string, args ...any) *Error { return &Error{Class: "abort", Msg: fmt.Sprintf(format, args...)} }

// OSError is a socket or TLS failure raised outside the IMAP error classes, its str the Python text
type OSError struct {
	Class string
	Text  string
}

func (e *OSError) Error() string      { return e.Text }
func (e *OSError) PyTypeName() string { return e.Class }

// Literal is one untagged response item that carried a literal, the tuple of its line and the literal bytes
type Literal struct {
	Line []byte
	Data []byte
}

// OSError of a failed socket call, a timeout reading as the Python TimeoutError
func socketError(err error) *OSError {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return &OSError{Class: "TimeoutError", Text: "timed out"}
	}
	return &OSError{Class: "OSError", Text: pyhttpx.OSErrorText(err)}
}

// Whether a receive error is a ConnectionError the reader treats as the end of the stream
func isConnectionError(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNREFUSED)
}

// A to P string of a number, the tag prefix alphabet
func int2AP(num int) []byte {
	var val []byte
	for num > 0 {
		val = append([]byte{"ABCDEFGHIJKLMNOP"[num%16]}, val...)
		num /= 16
	}
	return val
}

// Net parenthesis nesting of data, parentheses inside quoted strings not counted
func parenDepth(data []byte, depth int) int {
	data = quotedRE.ReplaceAll(data, nil)
	return depth + bytes.Count(data, []byte("(")) - bytes.Count(data, []byte(")"))
}

// One IMAP4 connection with its protocol state and pending responses
type IMAP4 struct {
	host    string
	timeout time.Duration
	conn    net.Conn
	readbuf []byte

	state          string
	tagpre         []byte
	tagre          *regexp.Regexp
	tagnum         int
	tagged         map[string]*taggedResponse
	untagged       map[string][]any
	isReadonly     bool
	tlsEstablished bool
	welcome        []byte

	// Capabilities the server advertised, upper cased
	Capabilities []string
}

// Completion of one tagged command
type taggedResponse struct {
	typ  string
	data []any
}

// Opens a connection, over implicit TLS when a config is given, reads the greeting and the capabilities
func Open(ctx context.Context, host string, port int, timeout time.Duration, tlsConfig *tls.Config) (*IMAP4, error) {
	m := &IMAP4{host: host, timeout: timeout, state: "LOGOUT", tagged: map[string]*taggedResponse{}, untagged: map[string][]any{}}
	dialHost := host
	if dialHost == "" {
		dialHost = "localhost"
	}
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(dialHost, strconv.Itoa(port)))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, socketError(err)
	}
	m.conn = conn
	if tlsConfig != nil {
		if err := m.wrapTLS(ctx, tlsConfig); err != nil {
			conn.Close()
			return nil, err
		}
	}
	if err := m.connect(); err != nil {
		m.Shutdown()
		return nil, err
	}
	return m, nil
}

// Runs the TLS handshake over the current socket with the server name of the host
func (m *IMAP4) wrapTLS(ctx context.Context, config *tls.Config) error {
	cfg := config.Clone()
	if cfg.ServerName == "" {
		cfg.ServerName = m.host
	}
	secured := tls.Client(m.conn, cfg)
	hctx := ctx
	if m.timeout > 0 {
		var cancel context.CancelFunc
		hctx, cancel = context.WithTimeout(ctx, m.timeout)
		defer cancel()
	}
	if err := secured.HandshakeContext(hctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var netErr net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
			return &OSError{Class: "TimeoutError", Text: "_ssl.c:1015: The handshake operation timed out"}
		}
		return &OSError{Class: "SSLError", Text: pyrequests.SSLErrorText(err)}
	}
	m.conn = secured
	return nil
}

// Reads the greeting, sets the starting state and learns the capabilities
func (m *IMAP4) connect() error {
	m.tagpre = int2AP(4096 + rand.Intn(65535-4096+1))
	m.tagre = regexp.MustCompile(`^(?P<tag>` + string(m.tagpre) + `[0-9]+) (?P<type>[A-Z]+) (?P<data>.*)`)
	welcome, err := m.getResponse()
	if err != nil {
		return err
	}
	m.welcome = welcome
	switch {
	case m.untagged["PREAUTH"] != nil:
		m.state = "AUTH"
	case m.untagged["OK"] != nil:
		m.state = "NONAUTH"
	default:
		return imapError("invalid greeting: %s", pystr.DecodeASCIIReplace(welcome))
	}
	if err := m.refreshCapabilities(true); err != nil {
		return err
	}
	for _, version := range []string{"IMAP4REV1", "IMAP4"} {
		for _, capability := range m.Capabilities {
			if capability == version {
				return nil
			}
		}
	}
	return imapError("server not IMAP4 compliant")
}

// Reads exactly size bytes, fewer when the stream ends first
func (m *IMAP4) read(size int) ([]byte, error) {
	for len(m.readbuf) < size {
		chunk, eof, err := m.recv()
		if err != nil {
			return nil, err
		}
		if eof {
			break
		}
		m.readbuf = append(m.readbuf, chunk...)
	}
	n := min(size, len(m.readbuf))
	out := append([]byte{}, m.readbuf[:n]...)
	m.readbuf = m.readbuf[n:]
	return out, nil
}

// One receive from the socket under the timeout, eof for a closed or reset stream
func (m *IMAP4) recv() ([]byte, bool, error) {
	if m.timeout > 0 {
		_ = m.conn.SetReadDeadline(time.Now().Add(m.timeout))
	}
	buf := make([]byte, 8192)
	n, err := m.conn.Read(buf)
	if n > 0 {
		return buf[:n], false, nil
	}
	if err == nil {
		return nil, false, nil
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) || isConnectionError(err) {
		return nil, true, nil
	}
	return nil, false, socketError(err)
}

// Reads one line with its terminator, a shorter result when the stream ends first
func (m *IMAP4) readline() ([]byte, error) {
	for {
		if pos := bytes.IndexByte(m.readbuf, '\n'); pos >= 0 {
			line := append([]byte{}, m.readbuf[:pos+1]...)
			m.readbuf = m.readbuf[pos+1:]
			if len(line) > maxLine {
				return nil, imapError("got more than %d bytes", maxLine)
			}
			return line, nil
		}
		if len(m.readbuf) >= maxLine {
			break
		}
		chunk, eof, err := m.recv()
		if err != nil {
			return nil, err
		}
		if eof {
			break
		}
		m.readbuf = append(m.readbuf, chunk...)
	}
	line := m.readbuf
	m.readbuf = nil
	if len(line) > maxLine {
		return nil, imapError("got more than %d bytes", maxLine)
	}
	return line, nil
}

// Sends data to the server under the timeout
func (m *IMAP4) send(data []byte) error {
	if m.timeout > 0 {
		_ = m.conn.SetWriteDeadline(time.Now().Add(m.timeout))
	}
	if _, err := m.conn.Write(data); err != nil {
		return socketError(err)
	}
	return nil
}

// Closes the socket
func (m *IMAP4) Shutdown() {
	if m.conn != nil {
		_ = m.conn.Close()
	}
}

// Identifies the client with a plain text password and enters the authenticated state
func (m *IMAP4) Login(user, password string) (string, []any, error) {
	for _, text := range []string{user, password} {
		if _, err := pycodecs.Encode(text, "ascii"); err != nil {
			return "", nil, err
		}
	}
	typ, dat, err := m.simpleCommand("LOGIN", m.astring([]byte(user)), m.quote([]byte(password)))
	if err != nil {
		return "", nil, err
	}
	if typ != "OK" {
		last, _ := dat[len(dat)-1].([]byte)
		return "", nil, imapError("%s", pystr.DecodeUTF8Replace(last))
	}
	m.state = "AUTH"
	if err := m.refreshCapabilities(false); err != nil {
		return "", nil, err
	}
	return typ, dat, nil
}

// Ends the session with LOGOUT and closes the socket
func (m *IMAP4) Logout() (string, []any, error) {
	m.state = "LOGOUT"
	typ, dat, err := m.simpleCommand("LOGOUT")
	if err != nil {
		return "", nil, err
	}
	m.Shutdown()
	return typ, dat, nil
}

// Selects a mailbox for read and write access, the state falling back to authenticated when the server refuses
func (m *IMAP4) Select(mailbox string) (string, []any, error) {
	m.untagged = map[string][]any{}
	m.isReadonly = false
	typ, dat, err := m.simpleCommand("SELECT", m.astring([]byte(mailbox)))
	if err != nil {
		return "", nil, err
	}
	if typ != "OK" {
		m.state = "AUTH"
		return typ, dat, nil
	}
	m.state = "SELECTED"
	if m.untagged["READ-ONLY"] != nil {
		return "", nil, &Error{Class: "readonly", Msg: pyrepr.Repr(mailbox) + " is not writable"}
	}
	if exists := m.untagged["EXISTS"]; exists != nil {
		return typ, exists, nil
	}
	return typ, []any{nil}, nil
}

// Upgrades the connection to TLS and relearns the capabilities
func (m *IMAP4) Starttls(ctx context.Context, tlsConfig *tls.Config) error {
	if m.tlsEstablished {
		return imapAbort("TLS session already established")
	}
	supported := false
	for _, capability := range m.Capabilities {
		supported = supported || capability == "STARTTLS"
	}
	if !supported {
		return imapAbort("TLS not supported by server")
	}
	typ, _, err := m.simpleCommand("STARTTLS")
	if err != nil {
		return err
	}
	if typ != "OK" {
		return imapError("Couldn't establish TLS session")
	}
	if err := m.wrapTLS(ctx, tlsConfig); err != nil {
		return err
	}
	m.readbuf, m.tlsEstablished = nil, true
	return m.getCapabilities()
}

// Parenthesized form of a flag or part list
func setQuote(arg string) string {
	if arg != "" && arg[0] == '(' && arg[len(arg)-1] == ')' {
		return arg
	}
	return "(" + arg + ")"
}

// Runs SEARCH, FETCH or STORE addressed by UID and returns the matching untagged data
func (m *IMAP4) UID(command string, args ...string) (string, []any, error) {
	command = strings.ToUpper(command)
	commands.Lock()
	states, known := commands.states[command]
	commands.Unlock()
	if !known {
		return "", nil, imapError("Unknown IMAP4 UID command: %s", pyrepr.Repr(command))
	}
	if !contains(states, m.state) {
		return "", nil, imapError("command %s illegal in state %s, only allowed in states %s", command, m.state, strings.Join(states, ", "))
	}
	switch command {
	case "FETCH":
		if len(args) != 2 {
			return "", nil, fmt.Errorf("ValueError: not enough values to unpack (expected 2, got %d)", len(args))
		}
		parts := args[1]
		if upper := strings.ToUpper(parts); upper != "ALL" && upper != "FULL" && upper != "FAST" {
			parts = setQuote(parts)
		}
		args = []string{args[0], parts}
	case "STORE":
		if len(args) != 3 {
			return "", nil, fmt.Errorf("ValueError: not enough values to unpack (expected 3, got %d)", len(args))
		}
		args = []string{args[0], args[1], setQuote(args[2])}
	}
	wire := make([][]byte, 0, len(args)+1)
	wire = append(wire, []byte(command))
	for _, arg := range args {
		wire = append(wire, []byte(arg))
	}
	typ, dat, err := m.simpleCommand("UID", wire...)
	if err != nil {
		return "", nil, err
	}
	name := "FETCH"
	if command == "SEARCH" {
		name = command
	}
	typ, dat = m.untaggedResponse(typ, dat, name)
	return typ, dat, nil
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

// Runs an extension command the server advertises, legal from now on in the current state
func (m *IMAP4) Xatom(name string, args ...string) (string, []any, error) {
	name = strings.ToUpper(name)
	commands.Lock()
	if _, known := commands.states[name]; !known {
		commands.states[name] = []string{m.state}
	}
	commands.Unlock()
	wire := make([][]byte, 0, len(args))
	for _, arg := range args {
		wire = append(wire, []byte(arg))
	}
	return m.simpleCommand(name, wire...)
}

// Stores one untagged response item under its type
func (m *IMAP4) appendUntagged(typ string, dat any) {
	if dat == nil {
		dat = []byte{}
	}
	m.untagged[typ] = append(m.untagged[typ], dat)
}

// Raises the abort a pending BYE means
func (m *IMAP4) checkBye() error {
	if bye := m.untagged["BYE"]; len(bye) > 0 {
		last, _ := bye[len(bye)-1].([]byte)
		return imapAbort("%s", pystr.DecodeASCIIReplace(last))
	}
	return nil
}

// Sends one tagged command and returns its tag
func (m *IMAP4) command(name string, args ...[]byte) (string, error) {
	commands.Lock()
	states := commands.states[name]
	commands.Unlock()
	if !contains(states, m.state) {
		return "", imapError("command %s illegal in state %s, only allowed in states %s", name, m.state, strings.Join(states, ", "))
	}
	for _, typ := range []string{"OK", "NO", "BAD"} {
		delete(m.untagged, typ)
	}
	if m.untagged["READ-ONLY"] != nil && !m.isReadonly {
		return "", &Error{Class: "readonly", Msg: "mailbox status changed to READ-ONLY"}
	}
	tag := string(m.tagpre) + strconv.Itoa(m.tagnum)
	m.tagnum++
	m.tagged[tag] = nil
	data := []byte(tag + " " + name)
	for _, arg := range args {
		if arg == nil {
			continue
		}
		if controlCharsRE.Match(arg) {
			return "", fmt.Errorf("ValueError: NUL, CR and LF not allowed in commands")
		}
		data = append(append(data, ' '), arg...)
	}
	if err := m.send(append(data, '\r', '\n')); err != nil {
		return "", imapAbort("socket error: %s", err.Error())
	}
	return tag, nil
}

// Waits for the completion of a command, a BAD answer or a BYE raised as its error
func (m *IMAP4) commandComplete(name, tag string) (string, []any, error) {
	logout := name == "LOGOUT"
	if !logout {
		if err := m.checkBye(); err != nil {
			return "", nil, err
		}
	}
	typ, data, err := m.getTaggedResponse(tag, logout)
	if err != nil {
		var imapErr *Error
		if errors.As(err, &imapErr) {
			class := "error"
			if imapErr.IsAbort() {
				class = "abort"
			}
			return "", nil, &Error{Class: class, Msg: fmt.Sprintf("command: %s => %s", name, imapErr.Msg)}
		}
		return "", nil, err
	}
	if !logout {
		if err := m.checkBye(); err != nil {
			return "", nil, err
		}
	}
	if typ == "BAD" {
		return "", nil, imapError("%s command error: %s %s", name, typ, pyrepr.Repr(data))
	}
	return typ, data, nil
}

// Asks the server for its capabilities
func (m *IMAP4) getCapabilities() error {
	typ, dat, err := m.simpleCommand("CAPABILITY")
	if err != nil {
		return err
	}
	_, dat = m.untaggedResponse(typ, dat, "CAPABILITY")
	if len(dat) == 1 && dat[0] == nil {
		return imapError("no CAPABILITY response from server")
	}
	last, _ := dat[len(dat)-1].([]byte)
	m.Capabilities = strings.Fields(strings.ToUpper(string(last)))
	return nil
}

// Takes the capabilities from a pending untagged CAPABILITY, else asks for them
func (m *IMAP4) refreshCapabilities(consume bool) error {
	pending := m.untagged["CAPABILITY"]
	if pending == nil {
		return m.getCapabilities()
	}
	last, _ := pending[len(pending)-1].([]byte)
	if consume {
		delete(m.untagged, "CAPABILITY")
	}
	m.Capabilities = strings.Fields(strings.ToUpper(string(last)))
	return nil
}

// Named group of a match, nil when the group did not take part
func group(re *regexp.Regexp, match [][]byte, name string) []byte {
	if i := re.SubexpIndex(name); i >= 0 && i < len(match) {
		return match[i]
	}
	return nil
}

// Reads one response and files it as tagged or untagged, nil for a continuation request
func (m *IMAP4) getResponse() ([]byte, error) {
	resp, err := m.getLine()
	if err != nil {
		return nil, err
	}
	for len(resp) == 0 {
		if resp, err = m.getLine(); err != nil {
			return nil, err
		}
	}
	var typ string
	var dat []byte
	if match := m.tagre.FindSubmatch(resp); match != nil {
		tag := string(group(m.tagre, match, "tag"))
		if _, pending := m.tagged[tag]; !pending {
			return nil, imapAbort("unexpected tagged response: %s", pyrepr.BytesRepr(resp))
		}
		typ, dat = string(group(m.tagre, match, "type")), group(m.tagre, match, "data")
		m.tagged[tag] = &taggedResponse{typ: typ, data: []any{dat}}
	} else {
		var dat2 []byte
		re := untaggedRE
		match := re.FindSubmatch(resp)
		if match == nil {
			re = untaggedStatusRE
			if match = re.FindSubmatch(resp); match != nil {
				dat2 = group(re, match, "data2")
			}
		}
		if match == nil {
			if cont := continuationRE.FindSubmatch(resp); cont != nil {
				return nil, nil
			}
			return nil, imapAbort("unexpected response: %s", pyrepr.BytesRepr(resp))
		}
		typ, dat = string(group(re, match, "type")), group(re, match, "data")
		if dat == nil {
			dat = []byte{}
		}
		if len(dat2) > 0 {
			dat = append(append(append([]byte{}, dat...), ' '), dat2...)
		}
		depth := 0
		for {
			lit := literalRE.FindSubmatch(dat)
			if lit == nil {
				break
			}
			size, _ := strconv.Atoi(string(group(literalRE, lit, "size")))
			data, err := m.read(size)
			if err != nil {
				return nil, err
			}
			m.appendUntagged(typ, Literal{Line: dat, Data: data})
			depth = parenDepth(dat, depth)
			if dat, err = m.getLine(); err != nil {
				return nil, err
			}
			for len(dat) == 0 && depth > 0 {
				if dat, err = m.getLine(); err != nil {
					return nil, err
				}
			}
		}
		m.appendUntagged(typ, dat)
	}
	if typ == "OK" || typ == "NO" || typ == "BAD" {
		if code := responseCodeRE.FindSubmatch(dat); code != nil {
			m.appendUntagged(string(group(responseCodeRE, code, "type")), group(responseCodeRE, code, "data"))
		}
	}
	return resp, nil
}

// Reads responses until the tagged one arrives, a BYE standing in for it when expected
func (m *IMAP4) getTaggedResponse(tag string, expectBye bool) (string, []any, error) {
	for {
		if result := m.tagged[tag]; result != nil {
			delete(m.tagged, tag)
			return result.typ, result.data, nil
		}
		if expectBye {
			if bye := m.untagged["BYE"]; bye != nil {
				delete(m.untagged, "BYE")
				return "BYE", bye, nil
			}
		}
		if err := m.checkBye(); err != nil {
			return "", nil, err
		}
		if _, err := m.getResponse(); err != nil {
			return "", nil, err
		}
	}
}

// Next response line without its CRLF, an abort for a closed stream or an unterminated line
func (m *IMAP4) getLine() ([]byte, error) {
	line, err := m.readline()
	if err != nil {
		return nil, err
	}
	if len(line) == 0 {
		return nil, imapAbort("socket error: EOF")
	}
	if !bytes.HasSuffix(line, []byte("\r\n")) {
		return nil, imapAbort("socket error: unterminated line: %s", pyrepr.BytesRepr(line))
	}
	return line[:len(line)-2], nil
}

// Quoted string form of an argument
func (m *IMAP4) quote(arg []byte) []byte {
	arg = bytes.ReplaceAll(arg, []byte(`\`), []byte(`\\`))
	arg = bytes.ReplaceAll(arg, []byte(`"`), []byte(`\"`))
	return append(append([]byte{'"'}, arg...), '"')
}

// Argument as an astring, quoted unless it is already quoted or made of atom characters only
func (m *IMAP4) astring(arg []byte) []byte {
	if quotedFullMatchRE.Match(arg) {
		return arg
	}
	if len(arg) > 0 && !nonAstringCharRE.Match(arg) {
		return arg
	}
	return m.quote(arg)
}

// Sends a command and waits for its completion
func (m *IMAP4) simpleCommand(name string, args ...[]byte) (string, []any, error) {
	tag, err := m.command(name, args...)
	if err != nil {
		return "", nil, err
	}
	return m.commandComplete(name, tag)
}

// Untagged data of a response type collected for a command, a single nil when the server sent none
func (m *IMAP4) untaggedResponse(typ string, dat []any, name string) (string, []any) {
	if typ == "NO" {
		return typ, dat
	}
	data, present := m.untagged[name]
	if !present {
		return typ, []any{nil}
	}
	delete(m.untagged, name)
	return typ, data
}
