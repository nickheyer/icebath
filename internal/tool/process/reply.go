package process

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

type Reply struct {
	keys []string
	vals map[string]any
}

func newReply(pairs ...any) *Reply {
	r := &Reply{vals: map[string]any{}}
	for i := 0; i+1 < len(pairs); i += 2 {
		r.set(pairs[i].(string), pairs[i+1])
	}
	return r
}

func (r *Reply) set(key string, v any) {
	if _, ok := r.vals[key]; !ok {
		r.keys = append(r.keys, key)
	}
	r.vals[key] = v
}

func (r *Reply) merge(o *Reply) {
	for _, k := range o.keys {
		r.set(k, o.vals[k])
	}
}

func (r *Reply) Get(key string) (any, bool) {
	v, ok := r.vals[key]
	return v, ok
}

func (r *Reply) str(key string) string {
	s, _ := r.vals[key].(string)
	return s
}

func (r *Reply) Status() string { return r.str("status") }

func (r *Reply) exitCode() *int {
	code, _ := r.vals["exit_code"].(*int)
	return code
}

func (r *Reply) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range r.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		if err := encodeValue(&buf, k); err != nil {
			return nil, err
		}
		buf.WriteByte(':')
		if err := encodeValue(&buf, r.vals[k]); err != nil {
			return nil, err
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func encodeValue(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case float64:
		buf.WriteString(jsonFloat(x))
		return nil
	case *int:
		if x == nil {
			buf.WriteString("null")
		} else {
			buf.WriteString(strconv.Itoa(*x))
		}
		return nil
	}
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	buf.Truncate(buf.Len() - 1)
	return nil
}

func jsonFloat(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "Infinity"
	case math.IsInf(v, -1):
		return "-Infinity"
	}
	return formatSeconds(v)
}

func formatSeconds(v float64) string {
	switch {
	case math.IsNaN(v):
		return "nan"
	case math.IsInf(v, 1):
		return "inf"
	case math.IsInf(v, -1):
		return "-inf"
	case v == 0:
		if math.Signbit(v) {
			return "-0.0"
		}
		return "0.0"
	}
	e := strconv.FormatFloat(v, 'e', -1, 64)
	exp, _ := strconv.Atoi(e[strings.IndexByte(e, 'e')+1:])
	if exp < -4 || exp >= 16 {
		return e
	}
	f := strconv.FormatFloat(v, 'f', -1, 64)
	if !strings.ContainsRune(f, '.') {
		f += ".0"
	}
	return f
}

func notFound(id string) *Reply {
	return newReply("status", "not_found", "error", (&notFoundError{id: id}).Error())
}

func errorReply(msg string) *Reply { return newReply("status", "error", "error", msg) }
