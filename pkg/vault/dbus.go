package vault

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type dbusObjectPath string
type dbusSignature string

type dbusVariant struct {
	Sig string
	Val any
}

type dbusEntry struct{ K, V any }

// Error reply of a D-Bus call, the name jeepney surfaces as DBusErrorResponse
type dbusError struct {
	Name string
	Body []any
}

func (e *dbusError) Error() string {
	if len(e.Body) > 0 {
		if s, ok := e.Body[0].(string); ok {
			return e.Name + ": " + s
		}
	}
	return e.Name
}

const (
	dbusMethodCall   = 1
	dbusMethodReturn = 2
	dbusErrorReply   = 3
	dbusSignal       = 4
	dbusCallTimeout  = 25 * time.Second
)

// Splits the first complete type off a signature
func dbusNextType(sig string) (string, string, error) {
	if sig == "" {
		return "", "", errors.New("empty signature")
	}
	switch sig[0] {
	case 'a':
		elem, rest, err := dbusNextType(sig[1:])
		return "a" + elem, rest, err
	case '(', '{':
		closer := byte(')')
		if sig[0] == '{' {
			closer = '}'
		}
		i := 1
		for i < len(sig) && sig[i] != closer {
			_, rest, err := dbusNextType(sig[i:])
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

// Splits a signature into its complete types
func dbusSplit(sig string) ([]string, error) {
	var out []string
	for sig != "" {
		t, rest, err := dbusNextType(sig)
		if err != nil {
			return nil, err
		}
		out, sig = append(out, t), rest
	}
	return out, nil
}

func dbusAlign(t byte) int {
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

type dbusEncoder struct{ buf []byte }

func (e *dbusEncoder) pad(n int) {
	for len(e.buf)%n != 0 {
		e.buf = append(e.buf, 0)
	}
}

func (e *dbusEncoder) u32(v uint32) { e.pad(4); e.buf = binary.LittleEndian.AppendUint32(e.buf, v) }

// Appends one value of a complete type in little endian wire form
func (e *dbusEncoder) encode(sig string, v any) error {
	e.pad(dbusAlign(sig[0]))
	switch sig[0] {
	case 'y':
		b, ok := v.(byte)
		if !ok {
			return fmt.Errorf("want byte for y, got %T", v)
		}
		e.buf = append(e.buf, b)
	case 'b':
		b, _ := v.(bool)
		if b {
			e.u32(1)
		} else {
			e.u32(0)
		}
	case 'n', 'q':
		e.buf = binary.LittleEndian.AppendUint16(e.buf, uint16(dbusInt(v)))
	case 'i', 'u', 'h':
		e.u32(uint32(dbusInt(v)))
	case 'x', 't':
		e.buf = binary.LittleEndian.AppendUint64(e.buf, uint64(dbusInt(v)))
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
		dv, ok := v.(dbusVariant)
		if !ok {
			return fmt.Errorf("want variant, got %T", v)
		}
		if err := e.encode("g", dv.Sig); err != nil {
			return err
		}
		return e.encode(dv.Sig, dv.Val)
	case 'a':
		return e.encodeArray(sig[1:], v)
	case '(':
		fields, err := dbusSplit(sig[1 : len(sig)-1])
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
		fields, err := dbusSplit(sig[1 : len(sig)-1])
		if err != nil || len(fields) != 2 {
			return fmt.Errorf("bad dict entry %s", sig)
		}
		ent, ok := v.(dbusEntry)
		if !ok {
			return fmt.Errorf("want dict entry, got %T", v)
		}
		if err := e.encode(fields[0], ent.K); err != nil {
			return err
		}
		return e.encode(fields[1], ent.V)
	default:
		return fmt.Errorf("cannot encode %s", sig)
	}
	return nil
}

func (e *dbusEncoder) encodeArray(elem string, v any) error {
	var items []any
	switch x := v.(type) {
	case []any:
		items = x
	case []byte:
		for _, b := range x {
			items = append(items, b)
		}
	case []string:
		for _, s := range x {
			items = append(items, s)
		}
	case []dbusObjectPath:
		for _, s := range x {
			items = append(items, s)
		}
	case []dbusEntry:
		for _, en := range x {
			items = append(items, en)
		}
	case map[string]string:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			items = append(items, dbusEntry{k, x[k]})
		}
	case nil:
	default:
		return fmt.Errorf("cannot encode %T as a%s", v, elem)
	}
	e.u32(0)
	lenAt := len(e.buf) - 4
	e.pad(dbusAlign(elem[0]))
	start := len(e.buf)
	for _, it := range items {
		if err := e.encode(elem, it); err != nil {
			return err
		}
	}
	binary.LittleEndian.PutUint32(e.buf[lenAt:], uint32(len(e.buf)-start))
	return nil
}

func dbusInt(v any) int64 {
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

type dbusDecoder struct {
	buf   []byte
	pos   int
	order binary.ByteOrder
}

func (d *dbusDecoder) pad(n int) error {
	for d.pos%n != 0 {
		d.pos++
	}
	if d.pos > len(d.buf) {
		return io.ErrUnexpectedEOF
	}
	return nil
}

func (d *dbusDecoder) take(n int) ([]byte, error) {
	if d.pos+n > len(d.buf) || n < 0 {
		return nil, io.ErrUnexpectedEOF
	}
	b := d.buf[d.pos : d.pos+n]
	d.pos += n
	return b, nil
}

func (d *dbusDecoder) u32() (uint32, error) {
	if err := d.pad(4); err != nil {
		return 0, err
	}
	b, err := d.take(4)
	if err != nil {
		return 0, err
	}
	return d.order.Uint32(b), nil
}

// Reads one value of a complete type, arrays of bytes as []byte and dict arrays as []dbusEntry
func (d *dbusDecoder) decode(sig string) (any, error) {
	if err := d.pad(dbusAlign(sig[0])); err != nil {
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
			return dbusObjectPath(b[:n]), nil
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
		return dbusSignature(b[:nb[0]]), nil
	case 'v':
		s, err := d.decode("g")
		if err != nil {
			return nil, err
		}
		inner := string(s.(dbusSignature))
		if _, rest, err := dbusNextType(inner); err != nil || rest != "" {
			return nil, fmt.Errorf("bad variant signature %q", inner)
		}
		val, err := d.decode(inner)
		return dbusVariant{Sig: inner, Val: val}, err
	case 'a':
		n, err := d.u32()
		if err != nil {
			return nil, err
		}
		elem := sig[1:]
		if err := d.pad(dbusAlign(elem[0])); err != nil {
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
		var entries []dbusEntry
		items := []any{}
		for d.pos < end {
			v, err := d.decode(elem)
			if err != nil {
				return nil, err
			}
			if elem[0] == '{' {
				entries = append(entries, v.(dbusEntry))
			} else {
				items = append(items, v)
			}
		}
		if elem[0] == '{' {
			return entries, nil
		}
		return items, nil
	case '(', '{':
		fields, err := dbusSplit(sig[1 : len(sig)-1])
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
			return dbusEntry{vals[0], vals[1]}, nil
		}
		return vals, nil
	}
	return nil, fmt.Errorf("cannot decode %s", sig)
}

type dbusMessage struct {
	Type        byte
	Flags       byte
	Serial      uint32
	ReplySerial uint32
	Path        dbusObjectPath
	Interface   string
	Member      string
	ErrorName   string
	Destination string
	Sender      string
	Signature   string
	Body        []any
}

// Wire bytes of a message with its header fields and body
func (m *dbusMessage) marshal() ([]byte, error) {
	body := &dbusEncoder{}
	types, err := dbusSplit(m.Signature)
	if err != nil {
		return nil, err
	}
	if len(types) != len(m.Body) {
		return nil, fmt.Errorf("signature %q wants %d values, got %d", m.Signature, len(types), len(m.Body))
	}
	for i, t := range types {
		if err := body.encode(t, m.Body[i]); err != nil {
			return nil, err
		}
	}
	var fields []any
	add := func(code byte, sig string, val any) {
		fields = append(fields, []any{code, dbusVariant{sig, val}})
	}
	if m.Path != "" {
		add(1, "o", m.Path)
	}
	if m.Interface != "" {
		add(2, "s", m.Interface)
	}
	if m.Member != "" {
		add(3, "s", m.Member)
	}
	if m.ErrorName != "" {
		add(4, "s", m.ErrorName)
	}
	if m.ReplySerial != 0 {
		add(5, "u", m.ReplySerial)
	}
	if m.Destination != "" {
		add(6, "s", m.Destination)
	}
	if m.Sender != "" {
		add(7, "s", m.Sender)
	}
	if m.Signature != "" {
		add(8, "g", m.Signature)
	}
	head := &dbusEncoder{buf: []byte{'l', m.Type, m.Flags, 1}}
	head.u32(uint32(len(body.buf)))
	head.u32(m.Serial)
	if err := head.encode("a(yv)", fields); err != nil {
		return nil, err
	}
	head.pad(8)
	return append(head.buf, body.buf...), nil
}

// Reads one whole message off the stream
func dbusReadMessage(r io.Reader) (*dbusMessage, error) {
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
	d := &dbusDecoder{buf: buf, pos: 12, order: order}
	raw, err := d.decode("a(yv)")
	if err != nil {
		return nil, err
	}
	m := &dbusMessage{Type: fixed[1], Flags: fixed[2], Serial: order.Uint32(fixed[8:12])}
	for _, f := range raw.([]any) {
		pair := f.([]any)
		val := pair[1].(dbusVariant).Val
		switch pair[0].(byte) {
		case 1:
			m.Path, _ = val.(dbusObjectPath)
		case 2:
			m.Interface, _ = val.(string)
		case 3:
			m.Member, _ = val.(string)
		case 4:
			m.ErrorName, _ = val.(string)
		case 5:
			m.ReplySerial, _ = val.(uint32)
		case 6:
			m.Destination, _ = val.(string)
		case 7:
			m.Sender, _ = val.(string)
		case 8:
			if s, ok := val.(dbusSignature); ok {
				m.Signature = string(s)
			}
		}
	}
	d.pos = headLen
	if err := d.pad(8); err != nil {
		return nil, err
	}
	types, err := dbusSplit(m.Signature)
	if err != nil {
		return nil, err
	}
	for _, t := range types {
		v, err := d.decode(t)
		if err != nil {
			return nil, err
		}
		m.Body = append(m.Body, v)
	}
	return m, nil
}

// Session bus connection, authenticated with EXTERNAL and registered through Hello
type dbusConn struct {
	conn   net.Conn
	r      *bufio.Reader
	mu     sync.Mutex
	serial uint32
	queue  []*dbusMessage
	name   string
}

// Unix socket address of the first connectable entry of a D-Bus address string
func dbusSocketAddress(addr string) (string, error) {
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

// Connects to the session bus DBUS_SESSION_BUS_ADDRESS names, the KeyError jeepney raises when unset
func dbusDialSession() (*dbusConn, error) {
	addr := os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	if addr == "" {
		return nil, errors.New("KeyError: 'DBUS_SESSION_BUS_ADDRESS'")
	}
	return dbusDial(addr)
}

func dbusDial(addr string) (*dbusConn, error) {
	path, err := dbusSocketAddress(addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		return nil, err
	}
	c := &dbusConn{conn: conn, r: bufio.NewReader(conn)}
	conn.SetDeadline(time.Now().Add(dbusCallTimeout))
	uid := strconv.Itoa(os.Getuid())
	if _, err := fmt.Fprintf(conn, "\x00AUTH EXTERNAL %x\r\n", uid); err != nil {
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
	out, err := c.call("org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus", "Hello", "")
	if err != nil {
		conn.Close()
		return nil, err
	}
	if len(out) > 0 {
		c.name, _ = out[0].(string)
	}
	return c, nil
}

func (c *dbusConn) Close() error { return c.conn.Close() }

// Calls a method and returns its reply body, a D-Bus error reply as *dbusError
func (c *dbusConn) call(dest string, path dbusObjectPath, iface, member, sig string, args ...any) ([]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.serial++
	m := &dbusMessage{Type: dbusMethodCall, Serial: c.serial, Path: path, Interface: iface, Member: member,
		Destination: dest, Signature: sig, Body: args}
	raw, err := m.marshal()
	if err != nil {
		return nil, err
	}
	c.conn.SetDeadline(time.Now().Add(dbusCallTimeout))
	defer c.conn.SetDeadline(time.Time{})
	if _, err := c.conn.Write(raw); err != nil {
		return nil, err
	}
	for {
		reply, err := dbusReadMessage(c.r)
		if err != nil {
			return nil, err
		}
		if reply.ReplySerial == m.Serial && (reply.Type == dbusMethodReturn || reply.Type == dbusErrorReply) {
			if reply.Type == dbusErrorReply {
				return nil, &dbusError{Name: reply.ErrorName, Body: reply.Body}
			}
			return reply.Body, nil
		}
		if reply.Type == dbusSignal {
			c.queue = append(c.queue, reply)
		}
	}
}

// Blocks until a signal of member on path arrives, the queued ones first
func (c *dbusConn) waitSignal(path dbusObjectPath, iface, member string) (*dbusMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	match := func(m *dbusMessage) bool {
		return m.Type == dbusSignal && m.Path == path && m.Interface == iface && m.Member == member
	}
	for i, m := range c.queue {
		if match(m) {
			c.queue = append(c.queue[:i], c.queue[i+1:]...)
			return m, nil
		}
	}
	for {
		m, err := dbusReadMessage(c.r)
		if err != nil {
			return nil, err
		}
		if match(m) {
			return m, nil
		}
	}
}
