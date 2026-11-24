package mailutil

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// Client of one IMAP4rev1 session, its commands run one at a time
type IMAP struct {
	session
	tagNum     int
	caps       map[string]bool
	capsGen    int
	bye        string
	maxLiteral int64
}

// Sets the largest literal, such as the content of a fetched message, that a response may bring into memory: 128 MiB until set and again for zero or less; a larger one fails its command with a *LiteralTooLargeError, read past and dropped up to 1 GiB so the connection stays usable and closing the connection beyond that
func (c *IMAP) SetMaxLiteral(n int64) {
	c.mu.Lock()
	c.maxLiteral = n
	c.mu.Unlock()
}

// Largest literal a response may bring into memory
func (c *IMAP) literalLimit() int64 {
	if c.maxLiteral > 0 {
		return c.maxLiteral
	}
	return defaultMaxLiteral
}

// Selected mailbox with the counters the server reported for it
type Mailbox struct {
	Name        string
	Exists      uint32 // messages in the mailbox
	UIDValidity uint32 // UIDVALIDITY the server reported, zero when it gave none
	UIDNext     uint32 // UID the next message will get, zero when the server gave none
}

// Session with the server at host and port, over implicit TLS when a configuration is given, its greeting and capabilities read; port 0 means 993 with TLS and 143 without, timeout bounds the connect and each read and write when not zero, and a nil dial means Dial
func DialIMAP(ctx context.Context, host string, port int, timeout time.Duration, dial DialFunc, tlsConfig *tls.Config) (*IMAP, error) {
	if port == 0 {
		port = 143
		if tlsConfig != nil {
			port = 993
		}
	}
	c := &IMAP{}
	if err := c.open(ctx, "imap", dial, host, port, timeout, tlsConfig); err != nil {
		return nil, err
	}
	if err := c.run(ctx, c.greet); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// Reads the greeting and learns the capabilities, failing for a server that refuses the connection or does not speak IMAP4rev1
func (c *IMAP) greet() error {
	r, err := c.readResponse()
	if err != nil {
		return err
	}
	if r.tag != "*" {
		return fmt.Errorf("imap: greeting tagged %q", r.tag)
	}
	switch r.kind {
	case "OK", "PREAUTH":
	case "BYE":
		return fmt.Errorf("imap: server refused the connection: %s", r.text)
	default:
		return fmt.Errorf("imap: unexpected greeting %s", r.kind)
	}
	c.untagged(r)
	if c.caps == nil {
		if err := c.capability(); err != nil {
			return err
		}
	}
	if !c.caps["IMAP4REV1"] && !c.caps["IMAP4REV2"] && !c.caps["IMAP4"] {
		return fmt.Errorf("imap: IMAP4rev1 %w", ErrNotSupported)
	}
	return nil
}

// Whether the server advertised the capability, compared without regard to case
func (c *IMAP) HasCapability(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.caps[strings.ToUpper(name)]
}

// Upgrades the session to TLS with STARTTLS, which the server must advertise, and learns the capabilities again over the encrypted connection
func (c *IMAP) StartTLS(ctx context.Context, config *tls.Config) error {
	return c.run(ctx, func() error {
		if c.secure {
			return fmt.Errorf("imap: %w: STARTTLS on a connection that is already encrypted", errArgument)
		}
		if !c.caps["STARTTLS"] {
			return fmt.Errorf("imap: STARTTLS %w", ErrNotSupported)
		}
		if _, err := c.execute("STARTTLS", nil, nil); err != nil {
			return err
		}
		if err := c.handshake(ctx, config); err != nil {
			return err
		}
		c.caps = nil
		return c.capability()
	})
}

// Logs in with LOGIN, an error matching ErrAuthFailed when the server rejects the credentials and ErrInsecure without sending anything over a connection without TLS unless AllowPlaintextLogin was called or the server is on this machine; user name and password may hold any text but NUL
func (c *IMAP) Login(ctx context.Context, user, password string) error {
	return c.run(ctx, func() error {
		if c.caps["LOGINDISABLED"] {
			return fmt.Errorf("imap: LOGIN before STARTTLS %w", ErrNotSupported)
		}
		if err := c.checkCredentials(); err != nil {
			return err
		}
		userArg, err := stringArg(user, true)
		if err != nil {
			return err
		}
		passwordArg, err := stringArg(password, true)
		if err != nil {
			return err
		}
		gen := c.capsGen
		if _, err := c.execute("LOGIN", []cmdArg{{text: " "}, userArg, {text: " "}, passwordArg}, nil); err != nil {
			return err
		}
		if c.capsGen == gen {
			return c.capability()
		}
		return nil
	})
}

// Identifies the client with the RFC 2971 ID command, which the server must advertise, and returns what the server says about itself
func (c *IMAP) ID(ctx context.Context, fields map[string]string) (map[string]string, error) {
	server := map[string]string{}
	err := c.run(ctx, func() error {
		if !c.caps["ID"] {
			return fmt.Errorf("imap: ID %w", ErrNotSupported)
		}
		args := []cmdArg{{text: " NIL"}}
		if len(fields) > 0 {
			args = []cmdArg{{text: " ("}}
			for i, key := range slices.Sorted(maps.Keys(fields)) {
				keyArg, err := stringArg(key, false)
				if err != nil {
					return err
				}
				valueArg, err := stringArg(fields[key], false)
				if err != nil {
					return err
				}
				if i > 0 {
					args = append(args, cmdArg{text: " "})
				}
				args = append(args, keyArg, cmdArg{text: " "}, valueArg)
			}
			args = append(args, cmdArg{text: ")"})
		}
		_, err := c.execute("ID", args, func(r *response) error {
			if r.kind != "ID" {
				return nil
			}
			if r.err != nil {
				return r.err
			}
			if len(r.fields) == 0 {
				return nil
			}
			list, _ := r.fields[0].([]any)
			for i := 0; i+1 < len(list); i += 2 {
				server[valueText(list[i])] = valueText(list[i+1])
			}
			return nil
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return server, nil
}

// Opens a mailbox for reading and writing, an error matching ErrReadOnly when the server grants reading only; names outside ASCII are sent in modified UTF-7
func (c *IMAP) Select(ctx context.Context, name string) (*Mailbox, error) {
	mailbox := &Mailbox{Name: name}
	err := c.run(ctx, func() error {
		arg, err := stringArg(encodeMailbox(name), true)
		if err != nil {
			return err
		}
		r, err := c.execute("SELECT", []cmdArg{{text: " "}, arg}, func(r *response) error {
			switch {
			case r.kind == "EXISTS":
				mailbox.Exists = r.num
			case r.kind == "OK" && r.code == "UIDVALIDITY":
				mailbox.UIDValidity = parseNumber(r.arg)
			case r.kind == "OK" && r.code == "UIDNEXT":
				mailbox.UIDNext = parseNumber(r.arg)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if r.code == "READ-ONLY" {
			return fmt.Errorf("imap: SELECT %q: %w", name, ErrReadOnly)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return mailbox, nil
}

// UIDs of the messages of the selected mailbox that match the search criteria, such as ALL or UNSEEN, given in IMAP syntax; both the SEARCH answer and the ESEARCH answer of IMAP4rev2 servers are read
func (c *IMAP) UIDSearch(ctx context.Context, criteria string) ([]uint32, error) {
	var uids []uint32
	err := c.run(ctx, func() error {
		if err := c.checkFragment("search criteria", criteria); err != nil {
			return err
		}
		_, err := c.execute("UID SEARCH", []cmdArg{{text: " " + criteria}}, func(r *response) error {
			switch {
			case r.kind != "SEARCH" && r.kind != "ESEARCH":
				return nil
			case r.err != nil:
				return r.err
			case r.kind == "SEARCH" && uids == nil:
				uids = r.nums
				return nil
			case r.kind == "SEARCH":
				uids = append(uids, r.nums...)
				return nil
			}
			found, err := esearchUIDs(r.fields)
			uids = append(uids, found...)
			return err
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return uids, nil
}

// Most UIDs the sequence set of one ESEARCH answer may stand for
const maxSearchUIDs = 1 << 24

// UIDs an ESEARCH answer lists under ALL, none when it lists no match
func esearchUIDs(fields []any) ([]uint32, error) {
	isUID, set := false, ""
	for i := 0; i < len(fields); i++ {
		name, ok := fields[i].(string)
		if !ok {
			continue
		}
		switch strings.ToUpper(name) {
		case "UID":
			isUID = true
		case "ALL":
			if i+1 < len(fields) {
				set = valueText(fields[i+1])
			}
			i++
		default:
			i++
		}
	}
	switch {
	case set == "":
		return nil, nil
	case !isUID:
		return nil, &syntaxError{msg: "ESEARCH answer to UID SEARCH lists sequence numbers"}
	}
	return expandSet(set)
}

// Numbers of an IMAP sequence set such as 2,10:11, ranges written either way round; an error for * or for a set standing for more than maxSearchUIDs numbers
func expandSet(set string) ([]uint32, error) {
	var out []uint32
	for item := range strings.SplitSeq(set, ",") {
		first, last, isRange := strings.Cut(item, ":")
		lo, err := strconv.ParseUint(first, 10, 32)
		hi := lo
		if err == nil && isRange {
			hi, err = strconv.ParseUint(last, 10, 32)
		}
		if err != nil || lo == 0 || hi == 0 {
			return nil, &syntaxError{msg: "bad sequence set " + strconv.Quote(set[:min(len(set), 200)])}
		}
		if lo > hi {
			lo, hi = hi, lo
		}
		if uint64(len(out))+hi-lo+1 > maxSearchUIDs {
			return nil, &syntaxError{msg: fmt.Sprintf("sequence set stands for more than %d numbers", maxSearchUIDs)}
		}
		for n := lo; n <= hi; n++ {
			out = append(out, uint32(n))
		}
	}
	return out, nil
}

// Error for raw command text that would break the command it is written into: a line break or NUL, or a literal announced at its end
func (c *IMAP) checkFragment(what, text string) error {
	if err := c.checkLine(what, text); err != nil {
		return err
	}
	if _, ok := trailingLiteral(text); ok {
		return fmt.Errorf("imap: %w: %s ends with a literal announcement", errArgument, what)
	}
	return nil
}

// Content of the first body section a UID FETCH of the message brings back, the items given in IMAP syntax such as (BODY[]) or (BODY.PEEK[HEADER]), RFC822 being gone from IMAP4rev2; an error matching ErrNotFound when the server returns none for the UID, and a *LiteralTooLargeError when the content is over the SetMaxLiteral limit
func (c *IMAP) UIDFetch(ctx context.Context, uid uint32, items string) ([]byte, error) {
	var data []byte
	found := false
	err := c.run(ctx, func() error {
		items = strings.TrimSpace(items)
		if err := c.checkFragment("fetch items", items); err != nil {
			return err
		}
		if !strings.HasPrefix(items, "(") {
			switch strings.ToUpper(items) {
			case "ALL", "FAST", "FULL":
			default:
				items = "(" + items + ")"
			}
		}
		_, err := c.execute("UID FETCH", []cmdArg{{text: " " + strconv.FormatUint(uint64(uid), 10) + " " + items}}, func(r *response) error {
			if r.kind != "FETCH" || found {
				return nil
			}
			if r.err != nil {
				return r.err
			}
			if len(r.fields) == 0 {
				return nil
			}
			list, _ := r.fields[0].([]any)
			var body []byte
			hasBody, hasUID, matched := false, false, false
			for i := 0; i+1 < len(list); i += 2 {
				name := strings.ToUpper(valueText(list[i]))
				switch {
				case name == "UID":
					hasUID = true
					n, err := strconv.ParseUint(valueText(list[i+1]), 10, 32)
					matched = err == nil && uint32(n) == uid
				case !hasBody && isBodyItem(name):
					body, hasBody = list[i+1].([]byte)
				}
			}
			if hasBody && (matched || !hasUID) {
				data, found = body, true
			}
			return nil
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("imap: UID FETCH %d: %w", uid, ErrNotFound)
	}
	return data, nil
}

// Whether a FETCH item name carries message content
func isBodyItem(name string) bool {
	return strings.HasPrefix(name, "BODY[") || strings.HasPrefix(name, "BINARY[") || name == "RFC822" || name == "RFC822.HEADER" || name == "RFC822.TEXT"
}

// Changes the flags of a message with UID STORE, item one of FLAGS, +FLAGS and -FLAGS with or without .SILENT and flags such as \Seen or $Forwarded
func (c *IMAP) UIDStore(ctx context.Context, uid uint32, item string, flags ...string) error {
	return c.run(ctx, func() error {
		if !isStoreItem(item) {
			return fmt.Errorf("imap: %w: store item %q", errArgument, item)
		}
		for _, flag := range flags {
			if !isFlag(flag) {
				return fmt.Errorf("imap: %w: flag %q", errArgument, flag)
			}
		}
		line := " " + strconv.FormatUint(uint64(uid), 10) + " " + item + " (" + strings.Join(flags, " ") + ")"
		_, err := c.execute("UID STORE", []cmdArg{{text: line}}, nil)
		return err
	})
}

// Whether s names a STORE data item: FLAGS, +FLAGS or -FLAGS, each with or without .SILENT
func isStoreItem(s string) bool {
	if s != "" && (s[0] == '+' || s[0] == '-') {
		s = s[1:]
	}
	return strings.EqualFold(s, "FLAGS") || strings.EqualFold(s, "FLAGS.SILENT")
}

// Whether s is a flag of RFC 3501: an atom, or a backslash and an atom
func isFlag(s string) bool {
	s = strings.TrimPrefix(s, `\`)
	return isAtom(s) && !strings.Contains(s, "]")
}

// Ends the session with LOGOUT and closes the connection, which is closed even when the command fails
func (c *IMAP) Logout(ctx context.Context) error {
	err := c.run(ctx, func() error {
		if _, err := c.execute("LOGOUT", nil, nil); err != nil && c.bye == "" {
			return err
		}
		return nil
	})
	if closeErr := c.Close(); err == nil {
		err = closeErr
	}
	return err
}

// Asks the server for its capabilities
func (c *IMAP) capability() error {
	gen := c.capsGen
	if _, err := c.execute("CAPABILITY", nil, nil); err != nil {
		return err
	}
	if c.capsGen == gen {
		return errors.New("imap: server sent no CAPABILITY response")
	}
	return nil
}

// Records what a response says about the session: capabilities and a coming BYE
func (c *IMAP) untagged(r *response) {
	switch {
	case r.kind == "CAPABILITY" && r.err == nil:
		list := make([]string, 0, len(r.fields))
		for _, field := range r.fields {
			list = append(list, valueText(field))
		}
		c.setCaps(list)
	case r.code == "CAPABILITY":
		c.setCaps(strings.Fields(r.arg))
	case r.kind == "BYE":
		c.bye = r.text
		if c.bye == "" {
			c.bye = "BYE"
		}
	}
}

func (c *IMAP) setCaps(list []string) {
	c.caps = make(map[string]bool, len(list))
	for _, capability := range list {
		c.caps[strings.ToUpper(capability)] = true
	}
	c.capsGen++
}

// Runs one tagged command, handing each untagged response to onData, and returns the completion; a NO or BAD completion comes back as an *IMAPError
func (c *IMAP) execute(name string, args []cmdArg, onData func(*response) error) (*response, error) {
	c.tagNum++
	tag := "A" + strconv.Itoa(c.tagNum)
	if err := c.send(tag, name, args); err != nil {
		return nil, err
	}
	var dataErr error
	for {
		r, err := c.readResponse()
		if err != nil {
			if c.bye != "" && errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("imap: %s: server ended the session: %s", name, c.bye)
			}
			return nil, err
		}
		switch r.tag {
		case "*":
			c.untagged(r)
			if onData != nil && dataErr == nil {
				dataErr = onData(r)
			}
		case "+":
		case tag:
			c.untagged(r)
			if err := completion(name, r); err != nil {
				return r, err
			}
			return r, dataErr
		default:
			return nil, fmt.Errorf("imap: %s: response tagged %q while waiting for %s", name, r.tag, tag)
		}
	}
}

// Error a tagged completion stands for, nil for OK
func completion(name string, r *response) error {
	switch r.kind {
	case "OK":
		return nil
	case "NO", "BAD":
		return &IMAPError{Command: name, Status: r.kind, Code: r.code, Text: r.text}
	}
	return fmt.Errorf("imap: %s: unexpected completion %s", name, r.kind)
}

// Writes a tagged command, waiting for the server's go-ahead before each synchronizing literal
func (c *IMAP) send(tag, name string, args []cmdArg) error {
	_, _ = c.w.WriteString(tag)
	_ = c.w.WriteByte(' ')
	_, _ = c.w.WriteString(name)
	for _, a := range args {
		if !a.isLit {
			_, _ = c.w.WriteString(a.text)
			continue
		}
		nonSync := c.caps["LITERAL+"] || c.caps["LITERAL-"] && len(a.literal) <= 4096
		_ = c.w.WriteByte('{')
		_, _ = c.w.WriteString(strconv.Itoa(len(a.literal)))
		if nonSync {
			_ = c.w.WriteByte('+')
		}
		_, _ = c.w.WriteString("}\r\n")
		if !nonSync {
			if err := c.flush(); err != nil {
				return err
			}
			if err := c.awaitContinuation(tag, name); err != nil {
				return err
			}
		}
		_, _ = c.w.Write(a.literal)
	}
	_, _ = c.w.WriteString("\r\n")
	return c.flush()
}

// Reads until the server asks for the rest of a command, failing when it completes the command instead
func (c *IMAP) awaitContinuation(tag, name string) error {
	for {
		r, err := c.readResponse()
		if err != nil {
			return err
		}
		switch r.tag {
		case "+":
			return nil
		case "*":
			c.untagged(r)
		case tag:
			if err := completion(name, r); err != nil {
				return err
			}
			return fmt.Errorf("imap: %s: server completed the command before its literal", name)
		default:
			return fmt.Errorf("imap: %s: response tagged %q while waiting for %s", name, r.tag, tag)
		}
	}
}

// Unsigned 32 bit number of a response code argument, zero when it is none
func parseNumber(s string) uint32 {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 32)
	if err != nil {
		return 0
	}
	return uint32(n)
}

var mailboxEncoding = base64.NewEncoding("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+,").WithPadding(base64.NoPadding)

// Mailbox name in the modified UTF-7 of RFC 3501, & written as &- and runs outside printable ASCII as &base64 of UTF-16-
func encodeMailbox(name string) string {
	plain := true
	for i := 0; i < len(name); i++ {
		if c := name[i]; c < 0x20 || c > 0x7e || c == '&' {
			plain = false
			break
		}
	}
	if plain {
		return name
	}
	var b strings.Builder
	var run []uint16
	flush := func() {
		if len(run) == 0 {
			return
		}
		units := make([]byte, 2*len(run))
		for i, u := range run {
			binary.BigEndian.PutUint16(units[2*i:], u)
		}
		b.WriteByte('&')
		b.WriteString(mailboxEncoding.EncodeToString(units))
		b.WriteByte('-')
		run = run[:0]
	}
	for _, r := range name {
		switch {
		case r == '&':
			flush()
			b.WriteString("&-")
		case r >= 0x20 && r <= 0x7e:
			flush()
			b.WriteRune(r)
		default:
			run = utf16.AppendRune(run, r)
		}
	}
	flush()
	return b.String()
}
