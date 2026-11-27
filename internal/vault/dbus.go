//go:build !darwin && !windows

package vault

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The D-Bus wire protocol as far as the Secret Service needs it: messages marshalled little endian, a session bus connection
// authenticated with EXTERNAL, method calls and the signals of prompts

type objectPath string
type busSignature string

type busVariant struct {
	sig string
	val any
}

type busEntry struct{ k, v any }

// Error reply of a call
type busError struct {
	name string
	body []any
}

func (e *busError) Error() string {
	if len(e.body) > 0 {
		if s, ok := e.body[0].(string); ok {
			return e.name + ": " + s
		}
	}
	return e.name
}

func isBusError(err error) bool {
	var be *busError
	return errors.As(err, &be)
}

const (
	busMethodCall   = 1
	busMethodReturn = 2
	busErrorReply   = 3
	busSignal       = 4
	busCallTimeout  = 25 * time.Second
)

// First complete type of a signature and the rest
func nextType(sig string) (string, string, error) {
	if sig == "" {
		return "", "", errors.New("empty signature")
	}
	switch sig[0] {
	case 'a':
		elem, rest, err := nextType(sig[1:])
		return "a" + elem, rest, err
	case '(', '{':
		closer := byte(')')
		if sig[0] == '{' {
			closer = '}'
		}
		i := 1
		for i < len(sig) && sig[i] != closer {
			_, rest, err := nextType(sig[i:])
			if err != nil {
				return "", "", err
			}
			i = len(sig) - len(rest)
		}
		if i >= len(sig) {
			return "", "", fmt.Errorf("unterminated %q", sig)
		}
		return sig[:i+1], sig[i+1:], nil
	case 'y', 'b', 'n', 'q', 'i', 'u', 'x', 't', 'd', 's', 'o', 'g', 'v', 'h':
		return sig[:1], sig[1:], nil
	}
	return "", "", fmt.Errorf("bad signature %q", sig)
}

// Complete types of a signature
func splitTypes(sig string) ([]string, error) {
	var out []string
	for sig != "" {
		t, rest, err := nextType(sig)
		if err != nil {
			return nil, err
		}
		out, sig = append(out, t), rest
	}
	return out, nil
}

func alignment(t byte) int {
	switch t {
	case 'n', 'q':
		return 2
	case 'b', 'i', 'u', 's', 'o', 'a', 'h':
		return 4
	case 'x', 't', 'd', '(', '{':
		return 8
	}
	return 1
}

type encoder struct{ buf []byte }

func (e *encoder) pad(n int) {
	for len(e.buf)%n != 0 {
		e.buf = append(e.buf, 0)
	}
}

func (e *encoder) u32(v uint32) { e.pad(4); e.buf = binary.LittleEndian.AppendUint32(e.buf, v) }

func integer(v any) int64 {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int16:
		return int64(x)
	case uint16:
		return int64(x)
	case int32:
		return int64(x)
	case uint32:
		return int64(x)
	case int64:
		return x
	case uint64:
		return int64(x)
	}
	return 0
}

// Appends one value of a complete type
func (e *encoder) encode(sig string, v any) error {
	e.pad(alignment(sig[0]))
	switch sig[0] {
	case 'y':
		b, ok := v.(byte)
		if !ok {
			return fmt.Errorf("want byte for y, got %T", v)
		}
		e.buf = append(e.buf, b)
	case 'b':
		if b, _ := v.(bool); b {
			e.u32(1)
		} else {
			e.u32(0)
		}
	case 'n', 'q':
		e.buf = binary.LittleEndian.AppendUint16(e.buf, uint16(integer(v)))
	case 'i', 'u', 'h':
		e.u32(uint32(integer(v)))
	case 'x', 't':
		e.buf = binary.LittleEndian.AppendUint64(e.buf, uint64(integer(v)))
	case 'd':
		f, _ := v.(float64)
		e.buf = binary.LittleEndian.AppendUint64(e.buf, math.Float64bits(f))
	case 's', 'o':
		s := fmt.Sprint(v)
		e.u32(uint32(len(s)))
		e.buf = append(append(e.buf, s...), 0)
	case 'g':
		s := fmt.Sprint(v)
		e.buf = append(append(append(e.buf, byte(len(s))), s...), 0)
	case 'v':
		dv, ok := v.(busVariant)
		if !ok {
			return fmt.Errorf("want variant, got %T", v)
		}
		if err := e.encode("g", dv.sig); err != nil {
			return err
		}
		return e.encode(dv.sig, dv.val)
	case 'a':
		return e.array(sig[1:], v)
	case '(':
		fields, err := splitTypes(sig[1 : len(sig)-1])
		if err != nil {
			return err
		}
		vals, ok := v.([]any)
		if !ok || len(vals) != len(fields) {
			return fmt.Errorf("struct %s wants %d fields", sig, len(fields))
		}
		for i, f := range fields {
			if err := e.encode(f, vals[i]); err != nil {
				return err
			}
		}
	case '{':
		fields, err := splitTypes(sig[1 : len(sig)-1])
		if err != nil || len(fields) != 2 {
			return fmt.Errorf("bad dict entry %s", sig)
		}
		entry, ok := v.(busEntry)
		if !ok {
			return fmt.Errorf("want dict entry, got %T", v)
		}
		if err := e.encode(fields[0], entry.k); err != nil {
			return err
		}
		return e.encode(fields[1], entry.v)
	default:
		return fmt.Errorf("cannot encode %s", sig)
	}
	return nil
}

func (e *encoder) array(elem string, v any) error {
	var items []any
	switch x := v.(type) {
	case []any:
		items = x
	case []byte:
		for _, b := range x {
			items = append(items, b)
		}
	case []objectPath:
		for _, s := range x {
			items = append(items, s)
		}
	case []busEntry:
		for _, en := range x {
			items = append(items, en)
		}
	case map[string]string:
		for _, k := range slices.Sorted(maps.Keys(x)) {
			items = append(items, busEntry{k, x[k]})
		}
	case nil:
	default:
		return fmt.Errorf("cannot encode %T as a%s", v, elem)
	}
	e.u32(0)
	lenAt := len(e.buf) - 4
	e.pad(alignment(elem[0]))
	start := len(e.buf)
	for _, it := range items {
		if err := e.encode(elem, it); err != nil {
			return err
		}
	}
	binary.LittleEndian.PutUint32(e.buf[lenAt:], uint32(len(e.buf)-start))
	return nil
}

type decoder struct {
	buf   []byte
	pos   int
	order binary.ByteOrder
}

func (d *decoder) pad(n int) error {
	for d.pos%n != 0 {
		d.pos++
	}
	if d.pos > len(d.buf) {
		return io.ErrUnexpectedEOF
	}
	return nil
}

func (d *decoder) take(n int) ([]byte, error) {
	if n < 0 || d.pos+n > len(d.buf) {
		return nil, io.ErrUnexpectedEOF
	}
	b := d.buf[d.pos : d.pos+n]
	d.pos += n
	return b, nil
}

func (d *decoder) u32() (uint32, error) {
	if err := d.pad(4); err != nil {
		return 0, err
	}
	b, err := d.take(4)
	if err != nil {
		return 0, err
	}
	return d.order.Uint32(b), nil
}

// Reads one value of a complete type, byte arrays as []byte and dict arrays as []busEntry
func (d *decoder) decode(sig string) (any, error) {
	if err := d.pad(alignment(sig[0])); err != nil {
		return nil, err
	}
	switch sig[0] {
	case 'y':
		b, err := d.take(1)
		if err != nil {
			return nil, err
		}
		return b[0], nil
	case 'b':
		v, err := d.u32()
		return v != 0, err
	case 'n', 'q':
		b, err := d.take(2)
		if err != nil {
			return nil, err
		}
		if sig[0] == 'n' {
			return int16(d.order.Uint16(b)), nil
		}
		return d.order.Uint16(b), nil
	case 'i':
		v, err := d.u32()
		return int32(v), err
	case 'u', 'h':
		return d.u32()
	case 'x', 't', 'd':
		b, err := d.take(8)
		if err != nil {
			return nil, err
		}
		v := d.order.Uint64(b)
		switch sig[0] {
		case 'x':
			return int64(v), nil
		case 'd':
			return math.Float64frombits(v), nil
		}
		return v, nil
	case 's', 'o':
		n, err := d.u32()
		if err != nil {
			return nil, err
		}
		b, err := d.take(int(n) + 1)
		if err != nil {
			return nil, err
		}
		if sig[0] == 'o' {
			return objectPath(b[:n]), nil
		}
		return string(b[:n]), nil
	case 'g':
		nb, err := d.take(1)
		if err != nil {
			return nil, err
		}
		b, err := d.take(int(nb[0]) + 1)
		if err != nil {
			return nil, err
		}
		return busSignature(b[:nb[0]]), nil
	case 'v':
		s, err := d.decode("g")
		if err != nil {
			return nil, err
		}
		inner := string(s.(busSignature))
		if _, rest, err := nextType(inner); err != nil || rest != "" {
			return nil, fmt.Errorf("bad variant signature %q", inner)
		}
		val, err := d.decode(inner)
		return busVariant{sig: inner, val: val}, err
	case 'a':
		n, err := d.u32()
		if err != nil {
			return nil, err
		}
		elem := sig[1:]
		if err := d.pad(alignment(elem[0])); err != nil {
			return nil, err
		}
		end := d.pos + int(n)
		if end > len(d.buf) {
			return nil, io.ErrUnexpectedEOF
		}
		if elem == "y" {
			b, err := d.take(int(n))
			return append([]byte{}, b...), err
		}
		var entries []busEntry
		items := []any{}
		for d.pos < end {
			v, err := d.decode(elem)
			if err != nil {
				return nil, err
			}
			if elem[0] == '{' {
				entries = append(entries, v.(busEntry))
			} else {
				items = append(items, v)
			}
		}
		if elem[0] == '{' {
			return entries, nil
		}
		return items, nil
	case '(', '{':
		fields, err := splitTypes(sig[1 : len(sig)-1])
		if err != nil {
			return nil, err
		}
		vals := make([]any, 0, len(fields))
		for _, f := range fields {
			v, err := d.decode(f)
			if err != nil {
				return nil, err
			}
			vals = append(vals, v)
		}
		if sig[0] == '{' {
			return busEntry{vals[0], vals[1]}, nil
		}
		return vals, nil
	}
	return nil, fmt.Errorf("cannot decode %s", sig)
}

type busMessage struct {
	kind        byte
	flags       byte
	serial      uint32
	replySerial uint32
	path        objectPath
	iface       string
	member      string
	errorName   string
	destination string
	sender      string
	signature   string
	body        []any
}

// Bytes of a message with its header fields and body
func (m *busMessage) marshal() ([]byte, error) {
	body := &encoder{}
	types, err := splitTypes(m.signature)
	if err != nil {
		return nil, err
	}
	if len(types) != len(m.body) {
		return nil, fmt.Errorf("signature %q wants %d values, got %d", m.signature, len(types), len(m.body))
	}
	for i, t := range types {
		if err := body.encode(t, m.body[i]); err != nil {
			return nil, err
		}
	}
	var fields []any
	add := func(code byte, sig string, val any) { fields = append(fields, []any{code, busVariant{sig, val}}) }
	if m.path != "" {
		add(1, "o", m.path)
	}
	if m.iface != "" {
		add(2, "s", m.iface)
	}
	if m.member != "" {
		add(3, "s", m.member)
	}
	if m.errorName != "" {
		add(4, "s", m.errorName)
	}
	if m.replySerial != 0 {
		add(5, "u", m.replySerial)
	}
	if m.destination != "" {
		add(6, "s", m.destination)
	}
	if m.sender != "" {
		add(7, "s", m.sender)
	}
	if m.signature != "" {
		add(8, "g", m.signature)
	}
	head := &encoder{buf: []byte{'l', m.kind, m.flags, 1}}
	head.u32(uint32(len(body.buf)))
	head.u32(m.serial)
	if err := head.encode("a(yv)", fields); err != nil {
		return nil, err
	}
	head.pad(8)
	return append(head.buf, body.buf...), nil
}

// Reads one whole message off the stream
func readMessage(r io.Reader) (*busMessage, error) {
	fixed := make([]byte, 16)
	if _, err := io.ReadFull(r, fixed); err != nil {
		return nil, err
	}
	var order binary.ByteOrder = binary.LittleEndian
	if fixed[0] == 'B' {
		order = binary.BigEndian
	} else if fixed[0] != 'l' {
		return nil, fmt.Errorf("bad endianness byte %q", fixed[0])
	}
	bodyLen := order.Uint32(fixed[4:8])
	fieldsLen := order.Uint32(fixed[12:16])
	headLen := 16 + int(fieldsLen)
	total := headLen + (8-headLen%8)%8 + int(bodyLen)
	if total > 128<<20 {
		return nil, errors.New("D-Bus message too large")
	}
	buf := make([]byte, total)
	copy(buf, fixed)
	if _, err := io.ReadFull(r, buf[16:]); err != nil {
		return nil, err
	}
	d := &decoder{buf: buf, pos: 12, order: order}
	raw, err := d.decode("a(yv)")
	if err != nil {
		return nil, err
	}
	m := &busMessage{kind: fixed[1], flags: fixed[2], serial: order.Uint32(fixed[8:12])}
	for _, f := range raw.([]any) {
		pair := f.([]any)
		val := pair[1].(busVariant).val
		switch pair[0].(byte) {
		case 1:
			m.path, _ = val.(objectPath)
		case 2:
			m.iface, _ = val.(string)
		case 3:
			m.member, _ = val.(string)
		case 4:
			m.errorName, _ = val.(string)
		case 5:
			m.replySerial, _ = val.(uint32)
		case 6:
			m.destination, _ = val.(string)
		case 7:
			m.sender, _ = val.(string)
		case 8:
			if s, ok := val.(busSignature); ok {
				m.signature = string(s)
			}
		}
	}
	d.pos = headLen
	if err := d.pad(8); err != nil {
		return nil, err
	}
	types, err := splitTypes(m.signature)
	if err != nil {
		return nil, err
	}
	for _, t := range types {
		v, err := d.decode(t)
		if err != nil {
			return nil, err
		}
		m.body = append(m.body, v)
	}
	return m, nil
}

// Session bus connection
type busConn struct {
	conn   net.Conn
	r      *bufio.Reader
	mu     sync.Mutex
	serial uint32
	// Signals read while waiting for a reply
	queue []*busMessage
}

// Unix socket of the first entry of a bus address that names one
func socketOf(addr string) (string, error) {
	for _, entry := range strings.Split(addr, ";") {
		transport, params, ok := strings.Cut(entry, ":")
		if !ok || transport != "unix" {
			continue
		}
		for _, kv := range strings.Split(params, ",") {
			k, v, _ := strings.Cut(kv, "=")
			v, err := url.PathUnescape(v)
			if err != nil {
				continue
			}
			switch k {
			case "path":
				return v, nil
			case "abstract":
				return "@" + v, nil
			}
		}
	}
	return "", fmt.Errorf("no connectable address in %q", addr)
}

// Connects to the session bus DBUS_SESSION_BUS_ADDRESS names
func dialSession() (*busConn, error) {
	addr := os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	if addr == "" {
		return nil, errors.New("DBUS_SESSION_BUS_ADDRESS is not set, so the session D-Bus cannot be reached")
	}
	path, err := socketOf(addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		return nil, err
	}
	c := &busConn{conn: conn, r: bufio.NewReader(conn)}
	conn.SetDeadline(time.Now().Add(busCallTimeout))
	if _, err := fmt.Fprintf(conn, "\x00AUTH EXTERNAL %x\r\n", strconv.Itoa(os.Getuid())); err != nil {
		conn.Close()
		return nil, err
	}
	line, err := c.r.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "OK ") {
		conn.Close()
		return nil, fmt.Errorf("D-Bus authentication failed: %q", strings.TrimSpace(line))
	}
	if _, err := io.WriteString(conn, "BEGIN\r\n"); err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	if _, err := c.call("org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus", "Hello", ""); err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

func (c *busConn) Close() error { return c.conn.Close() }

// Calls a method and answers its reply's body, an error reply as *busError
func (c *busConn) call(dest string, path objectPath, iface, member, sig string, args ...any) ([]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.serial++
	m := &busMessage{kind: busMethodCall, serial: c.serial, path: path, iface: iface, member: member, destination: dest, signature: sig, body: args}
	raw, err := m.marshal()
	if err != nil {
		return nil, err
	}
	c.conn.SetDeadline(time.Now().Add(busCallTimeout))
	defer c.conn.SetDeadline(time.Time{})
	if _, err := c.conn.Write(raw); err != nil {
		return nil, err
	}
	for {
		reply, err := readMessage(c.r)
		if err != nil {
			return nil, err
		}
		if reply.replySerial == m.serial && (reply.kind == busMethodReturn || reply.kind == busErrorReply) {
			if reply.kind == busErrorReply {
				return nil, &busError{name: reply.errorName, body: reply.body}
			}
			return reply.body, nil
		}
		if reply.kind == busSignal {
			c.queue = append(c.queue, reply)
		}
	}
}

// Waits for a signal of member on path, those queued first
func (c *busConn) waitSignal(path objectPath, iface, member string) (*busMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	match := func(m *busMessage) bool {
		return m.kind == busSignal && m.path == path && m.iface == iface && m.member == member
	}
	for i, m := range c.queue {
		if match(m) {
			c.queue = append(c.queue[:i], c.queue[i+1:]...)
			return m, nil
		}
	}
	for {
		m, err := readMessage(c.r)
		if err != nil {
			return nil, err
		}
		if match(m) {
			return m, nil
		}
	}
}
