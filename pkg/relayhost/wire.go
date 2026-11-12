package relayhost

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"reflect"
	"strconv"
	"strings"
	"sync"
)

// One frame of the relay host channel, a request when Op is set and otherwise the reply to ID
type message struct {
	ID     int64           `json:"id"`
	Op     string          `json:"op,omitempty"`
	Origin int64           `json:"origin,omitempty"`
	Args   json.RawMessage `json:"args,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *wireError      `json:"error,omitempty"`
}

// Exception crossing the channel, GoErr naming the parent callback error it carries
type wireError struct {
	Class   string `json:"class"`
	Module  string `json:"module,omitempty"`
	Message string `json:"message"`
	GoErr   int64  `json:"goerr,omitempty"`
}

func (e *wireError) Error() string { return e.Class + ": " + e.Message }

// Channel closed error naming why the peer went away
var errChannelClosed = errors.New("NeMo Relay host channel closed")

// Line framed duplex channel whose replies wake their waiter and whose nested requests reach the waiter of their origin
type conn struct {
	r       *bufio.Reader
	w       io.Writer
	wmu     sync.Mutex
	mu      sync.Mutex
	next    int64
	pending map[int64]chan *message
	nested  bool
	closed  error
	done    chan struct{}
	request func(m *message)
}

func newConn(r io.Reader, w io.Writer, nested bool, request func(m *message)) *conn {
	return &conn{r: bufio.NewReaderSize(r, 1<<16), w: w, pending: map[int64]chan *message{}, nested: nested,
		done: make(chan struct{}), request: request}
}

func (c *conn) send(m *message) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.w.Write(append(data, '\n'))
	return err
}

// Sends a request and returns the channel its reply and routed nested requests arrive on
func (c *conn) open(op string, origin int64, args any) (int64, chan *message, error) {
	return c.openWith(op, origin, args, nil)
}

// Sends a request after before learned its id, so state keyed by it exists when the first callback arrives
func (c *conn) openWith(op string, origin int64, args any, before func(id int64)) (int64, chan *message, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return 0, nil, err
	}
	c.mu.Lock()
	if c.closed != nil {
		err := c.closed
		c.mu.Unlock()
		return 0, nil, err
	}
	c.next++
	id := c.next
	ch := make(chan *message, 8)
	c.pending[id] = ch
	c.mu.Unlock()
	if before != nil {
		before(id)
	}
	if err := c.send(&message{ID: id, Op: op, Origin: origin, Args: raw}); err != nil {
		c.forget(id)
		return 0, nil, err
	}
	return id, ch, nil
}

func (c *conn) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// Replies to a request with a result or an error
func (c *conn) reply(id int64, result any, failure *wireError) error {
	m := &message{ID: id, Error: failure}
	if failure == nil {
		raw, err := json.Marshal(result)
		if err != nil {
			m.Error = &wireError{Class: "TypeError", Message: err.Error()}
		} else {
			m.Result = raw
		}
	}
	return c.send(m)
}

// Reads frames until the peer closes, failing every waiter with the reason
func (c *conn) readLoop() {
	var err error
	for {
		var line []byte
		line, err = c.r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			m := &message{}
			if jerr := json.Unmarshal(line, m); jerr != nil {
				err = fmt.Errorf("NeMo Relay host sent an unreadable frame: %w", jerr)
				break
			}
			c.route(m)
		}
		if err != nil {
			break
		}
	}
	if errors.Is(err, io.EOF) {
		err = errChannelClosed
	}
	c.mu.Lock()
	c.closed = err
	waiters := c.pending
	c.pending = map[int64]chan *message{}
	c.mu.Unlock()
	for _, ch := range waiters {
		close(ch)
	}
	close(c.done)
}

func (c *conn) route(m *message) {
	c.mu.Lock()
	if m.Op == "" {
		ch := c.pending[m.ID]
		delete(c.pending, m.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- m
		}
		return
	}
	if c.nested && m.Origin != 0 {
		if ch := c.pending[m.Origin]; ch != nil {
			c.mu.Unlock()
			ch <- m
			return
		}
	}
	c.mu.Unlock()
	c.request(m)
}

func (c *conn) err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Wire form of a float keeping its Python float type, non finite values by name
func floatText(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func parseFloatText(s string) (float64, error) {
	switch s {
	case "nan":
		return math.NaN(), nil
	case "inf":
		return math.Inf(1), nil
	case "-inf":
		return math.Inf(-1), nil
	}
	return strconv.ParseFloat(s, 64)
}

// Lowers plain values to their tagged JSON tree, special handling the values only one side knows
func lowerValue(v any, depth int, special func(v any, depth int) (any, bool, error)) (any, error) {
	if depth > 200 {
		return nil, errors.New("ValueError: value nests too deeply for the NeMo Relay host")
	}
	if out, ok, err := special(v, depth); ok || err != nil {
		return out, err
	}
	switch x := v.(type) {
	case nil, bool, string:
		return x, nil
	case int:
		return int64(x), nil
	case int8:
		return int64(x), nil
	case int16:
		return int64(x), nil
	case int32:
		return int64(x), nil
	case int64:
		return x, nil
	case uint:
		return map[string]any{"$i": strconv.FormatUint(uint64(x), 10)}, nil
	case uint8:
		return int64(x), nil
	case uint16:
		return int64(x), nil
	case uint32:
		return int64(x), nil
	case uint64:
		return map[string]any{"$i": strconv.FormatUint(x, 10)}, nil
	case *big.Int:
		return map[string]any{"$i": x.String()}, nil
	case float32:
		return map[string]any{"$f": floatText(float64(x))}, nil
	case float64:
		return map[string]any{"$f": floatText(x)}, nil
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i, nil
		}
		f, err := x.Float64()
		if err != nil {
			return nil, err
		}
		return map[string]any{"$f": floatText(f)}, nil
	case []byte:
		return map[string]any{"$b": base64.StdEncoding.EncodeToString(x)}, nil
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			lowered, err := lowerValue(item, depth+1, special)
			if err != nil {
				return nil, err
			}
			out[i] = lowered
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(x))
		escape := false
		for k, item := range x {
			lowered, err := lowerValue(item, depth+1, special)
			if err != nil {
				return nil, err
			}
			out[k] = lowered
			escape = escape || strings.HasPrefix(k, "$")
		}
		if escape {
			return map[string]any{"$d": out}, nil
		}
		return out, nil
	}
	return lowerReflect(v, depth, special)
}

// Wire tree of a decoded JSON value, tagged nodes resolved by special
func liftValue(v any, special func(tag string, body any) (any, error)) (any, error) {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i, nil
		}
		b, ok := new(big.Int).SetString(x.String(), 10)
		if !ok {
			return nil, fmt.Errorf("ValueError: number %s does not parse", x)
		}
		return b, nil
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			lifted, err := liftValue(item, special)
			if err != nil {
				return nil, err
			}
			out[i] = lifted
		}
		return out, nil
	case map[string]any:
		if len(x) == 1 {
			for k, body := range x {
				if strings.HasPrefix(k, "$") {
					return liftTagged(k, body, special)
				}
			}
		}
		return liftMap(x, special)
	}
	return v, nil
}

func liftMap(x map[string]any, special func(tag string, body any) (any, error)) (map[string]any, error) {
	out := make(map[string]any, len(x))
	for k, item := range x {
		lifted, err := liftValue(item, special)
		if err != nil {
			return nil, err
		}
		out[k] = lifted
	}
	return out, nil
}

func liftTagged(tag string, body any, special func(tag string, body any) (any, error)) (any, error) {
	switch tag {
	case "$f":
		s, _ := body.(string)
		return parseFloatText(s)
	case "$i":
		s, _ := body.(string)
		b, ok := new(big.Int).SetString(s, 10)
		if !ok {
			return nil, fmt.Errorf("ValueError: integer %q does not parse", s)
		}
		if b.IsInt64() {
			return b.Int64(), nil
		}
		return b, nil
	case "$b":
		s, _ := body.(string)
		return base64.StdEncoding.DecodeString(s)
	case "$d":
		m, _ := body.(map[string]any)
		return liftMap(m, special)
	}
	return special(tag, body)
}

// Decodes a raw JSON payload keeping integers exact
func decodeJSON(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// Number of a decoded wire integer
func wireInt(v any) int64 {
	switch x := v.(type) {
	case json.Number:
		i, _ := x.Int64()
		return i
	case int64:
		return x
	case float64:
		return int64(x)
	}
	return 0
}

// Lowers typed slices, string keyed maps and pointers to them, refusing any other Go type
func lowerReflect(v any, depth int, special func(v any, depth int) (any, bool, error)) (any, error) {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer:
		if rv.IsNil() {
			return nil, nil
		}
		return lowerValue(rv.Elem().Interface(), depth+1, special)
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return []any{}, nil
		}
		out := make([]any, rv.Len())
		for i := range rv.Len() {
			lowered, err := lowerValue(rv.Index(i).Interface(), depth+1, special)
			if err != nil {
				return nil, err
			}
			out[i] = lowered
		}
		return out, nil
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			break
		}
		plain := make(map[string]any, rv.Len())
		for it := rv.MapRange(); it.Next(); {
			plain[it.Key().String()] = it.Value().Interface()
		}
		return lowerValue(plain, depth, special)
	case reflect.String:
		return rv.String(), nil
	case reflect.Bool:
		return rv.Bool(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int(), nil
	case reflect.Float32, reflect.Float64:
		return map[string]any{"$f": floatText(rv.Float())}, nil
	}
	return nil, &wireError{Class: "TypeError", Message: fmt.Sprintf("Object of type %T cannot cross to the NeMo Relay host", v)}
}
