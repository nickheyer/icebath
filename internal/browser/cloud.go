package browser

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// What a cloud service answered: its status and body
type Reply struct {
	Status int
	Body   []byte
}

// Whether the service took the call
func (r *Reply) OK() bool { return r.Status < 400 }

// Answers a cloud browser service's API with a JSON body, nil sending none, within the timeout and through the environment's proxy
func Call(ctx context.Context, method, url string, headers map[string]string, body any, timeout time.Duration) (*Reply, error) {
	var payload io.Reader = http.NoBody
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = bytes.NewReader(data)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, payload)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	return &Reply{Status: resp.StatusCode, Body: data}, nil
}

// Label of a cloud session in the logs, unique to it
func SessionName(task string) string {
	buf := make([]byte, 4)
	rand.Read(buf)
	return "icebath_" + task + "_" + hex.EncodeToString(buf)
}

// Whether a release answer closed the session
func Released(status int) bool { return status == 200 || status == 201 || status == 204 }

// Releases a session of a service, saying how it went in the log, whether it closed
func Release(ctx context.Context, title, method, url, id string, headers map[string]string, body any) bool {
	reply, err := Call(ctx, method, url, headers, body, 10*time.Second)
	if err != nil {
		slog.Error(fmt.Sprintf("Exception closing %s session %s: %s", title, id, err))
		return false
	}
	if Released(reply.Status) {
		slog.Debug(fmt.Sprintf("Successfully closed %s session %s", title, id))
		return true
	}
	text := []rune(string(reply.Body))
	if len(text) > 200 {
		text = text[:200]
	}
	slog.Warn(fmt.Sprintf("Failed to close %s session %s: HTTP %d - %s", title, id, reply.Status, string(text)))
	return false
}
