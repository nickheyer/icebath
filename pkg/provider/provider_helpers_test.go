package provider

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// Builds a JSON response for the request with the status, payload bytes or encoding, and extra headers
func jsonResponse(req *http.Request, status int, payload any, headers map[string]string) *http.Response {
	body, ok := payload.([]byte)
	if !ok {
		body, _ = json.Marshal(payload)
	}
	resp := &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: req}
	resp.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		resp.Header.Set(k, v)
	}
	return resp
}

// Decodes a JSON text string, or the JSON encoding of any other value, into a map and panics on bad input
func jsonMap(v any) map[string]any {
	raw, ok := v.(string)
	if !ok {
		encoded, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		raw = string(encoded)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		panic(err)
	}
	return out
}
