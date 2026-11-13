package wecom

import (
	"bytes"
	"container/list"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Commands of the AI bot websocket gateway
const (
	defaultWSURL      = "wss://openws.work.weixin.qq.com"
	cmdSubscribe      = "aibot_subscribe"
	cmdCallback       = "aibot_msg_callback"
	cmdLegacyCallback = "aibot_callback"
	cmdEventCallback  = "aibot_event_callback"
	cmdPing           = "ping"
	cmdSend           = "aibot_send_msg"
	cmdRespond        = "aibot_respond_msg"
	cmdUploadInit     = "aibot_upload_media_init"
	cmdUploadChunk    = "aibot_upload_media_chunk"
	cmdUploadFinish   = "aibot_upload_media_finish"
)

// Limits and timings of the AI bot transport
const (
	maxMessageLength  = 4000
	splitThreshold    = 3900
	connectTimeout    = 20 * time.Second
	requestTimeout    = 15 * time.Second
	heartbeatInterval = 30 * time.Second
	wsPingInterval    = 60 * time.Second
	readIdleTimeout   = 90 * time.Second
	writeTimeout      = 15 * time.Second
	dedupMaxSize      = 1000
)

// Wait before each reconnect attempt, the last one repeating
var reconnectBackoff = []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second, 60 * time.Second}

// Errcode of a stale websocket subscription whose req_ids are dead
const errcodeNotSubscribed = 846609

// Request that WeCom did not answer within its timeout
var errRequestTimeout = errors.New("timed out waiting for the WeCom reply")

// Random lowercase hex of n bytes
func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Request id with the command prefix the gateway echoes back in its reply
func newReqID(prefix string) string { return prefix + "-" + randHex(16) }

// Object under key, empty when absent or not an object
func objAt(m map[string]any, key string) map[string]any {
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return map[string]any{}
}

// Text of a config or JSON scalar, empty for null
func textOf(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	}
	return fmt.Sprint(v)
}

// Trimmed text under key
func strAt(m map[string]any, key string) string { return strings.TrimSpace(textOf(m[key])) }

// Whether a config value is set to something other than its zero
func isSet(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case bool:
		return x
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// Boolean reading of a config value, words like false and off counting as false
func truthy(v any) bool {
	if s, ok := v.(string); ok {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "", "0", "false", "no", "off":
			return false
		}
		return true
	}
	return isSet(v)
}

// Float of a config value, the default when absent or not a number
func floatOf(v any, def float64) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return f
		}
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil {
			return f
		}
	}
	return def
}

// Req id from the headers of a frame
func payloadReqID(payload map[string]any) string { return textOf(objAt(payload, "headers")["req_id"]) }

// Errcode of a reply and whether it is a failure, absent and null codes counting as success
func errcodeOf(m map[string]any) (int64, bool) {
	switch v := m["errcode"].(type) {
	case nil:
		return 0, false
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n, n != 0
		}
		if f, err := v.Float64(); err == nil {
			return int64(f), f != 0
		}
	case float64:
		return int64(v), v != 0
	}
	return 0, true
}

// WeCom failure text of a reply, empty on success
func responseError(m map[string]any) string {
	if _, failed := errcodeOf(m); !failed {
		return ""
	}
	msg := textOf(m["errmsg"])
	if msg == "" {
		msg = "unknown error"
	}
	return fmt.Sprintf("WeCom errcode %s: %s", textOf(m["errcode"]), msg)
}

// Error naming the operation for a failed reply, nil on success
func raiseForWeComError(m map[string]any, operation string) error {
	if e := responseError(m); e != "" {
		return fmt.Errorf("%s failed: %s", operation, e)
	}
	return nil
}

// Decoded JSON object of a frame, numbers kept as json.Number
func decodeObject(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the JSON value")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("frame is not a JSON object")
	}
	return m, nil
}

// JSON with raw control characters inside strings escaped, the form WeCom sometimes sends
func escapeControlChars(data []byte) []byte {
	var out bytes.Buffer
	inString, escaped := false, false
	for _, c := range data {
		switch {
		case escaped:
			escaped = false
		case inString && c == '\\':
			escaped = true
		case c == '"':
			inString = !inString
		case inString && c < 0x20:
			fmt.Fprintf(&out, `\u%04x`, c)
			continue
		}
		out.WriteByte(c)
	}
	return out.Bytes()
}

// Frame object, retrying with raw control characters escaped, and whether the lenient pass was needed
func decodeFrame(data []byte) (map[string]any, bool, error) {
	m, err := decodeObject(data)
	if err == nil {
		return m, false, nil
	}
	if lenient, lerr := decodeObject(escapeControlChars(data)); lerr == nil {
		return lenient, true, nil
	}
	return nil, false, err
}

// JSON encoding without HTML escaping so content such as <think> goes out verbatim
func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Insertion ordered string map holding at most cap keys, a re-put making the key newest
type boundedMap struct {
	cap   int
	order *list.List
	index map[string]*list.Element
}

type boundedEntry struct{ key, value string }

func newBoundedMap(capacity int) *boundedMap {
	return &boundedMap{cap: capacity, order: list.New(), index: map[string]*list.Element{}}
}

// Stores the value as the newest key, evicting the oldest past the cap
func (m *boundedMap) put(key, value string) {
	if el, ok := m.index[key]; ok {
		m.order.Remove(el)
	}
	m.index[key] = m.order.PushBack(&boundedEntry{key, value})
	for m.order.Len() > m.cap {
		oldest := m.order.Front()
		m.order.Remove(oldest)
		delete(m.index, oldest.Value.(*boundedEntry).key)
	}
}

// Value of a key, empty when absent
func (m *boundedMap) get(key string) string {
	if el, ok := m.index[key]; ok {
		return el.Value.(*boundedEntry).value
	}
	return ""
}

// Forgets every key
func (m *boundedMap) clear() {
	m.order.Init()
	m.index = map[string]*list.Element{}
}
