package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Serializes a wire request without HTML escaping and deep merges extra fields over it
func MarshalBody(v any, extra map[string]json.RawMessage) ([]byte, error) {
	body, err := marshal(v)
	if err != nil {
		return nil, err
	}
	if len(extra) == 0 {
		return body, nil
	}
	base, err := decodeObject(body)
	if err != nil {
		return nil, fmt.Errorf("protocol: body is not a JSON object: %w", err)
	}
	for k, raw := range extra {
		var val any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&val); err != nil {
			return nil, fmt.Errorf("protocol: extra field %q: %w", k, err)
		}
		base[k] = merge(base[k], val)
	}
	return marshal(base)
}

func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func decodeObject(body []byte) (map[string]any, error) {
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// Overlays src on dst, recursing into objects and replacing everything else
func merge(dst, src any) any {
	dm, dok := dst.(map[string]any)
	sm, sok := src.(map[string]any)
	if !dok || !sok {
		return src
	}
	for k, v := range sm {
		dm[k] = merge(dm[k], v)
	}
	return dm
}
