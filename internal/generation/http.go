package generation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Name the services are told the calls come from
var Agent = provider.UserAgent("Icebath-Agent")

// Why a JSON request failed, the error the model reads ready in Error
type Failure struct {
	// http, timeout, connection or invalid_json
	Kind  string
	Error string
	// Kind of failure the model reads, such as api_error
	Type string
	// Status and what the service said, for an http failure
	Status  int
	Message string
}

// Message of a failed JSON reply: its error's message, else the first 300 characters of the body
func Message(_ int, body []byte) string {
	var reply struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(body, &reply) == nil {
		if fields, ok := reply.Error.(map[string]any); ok {
			if message, ok := fields["message"]; ok {
				return fmt.Sprint(message)
			}
		}
	}
	return strs.Head(strings.ToValidUTF8(string(body), "�"), 300)
}

// Posts JSON and reads the JSON reply, or why it failed: a connect limit and a limit on each wait for the service; message words what a
// failed reply says, Message when nil
func Post(ctx context.Context, address string, headers map[string]string, payload any, connect, read time.Duration, label string,
	message func(status int, body []byte) string) (any, *Failure) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, &Failure{Kind: "request", Error: fmt.Sprintf("%s request failed: %v", label, err), Type: "api_error"}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(body))
	if err != nil {
		return nil, &Failure{Kind: "request", Error: fmt.Sprintf("%s request failed: %v", label, err), Type: "api_error"}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", Agent)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: connect}).DialContext,
		TLSHandshakeTimeout: read, ResponseHeaderTimeout: read, ForceAttemptHTTP2: true}
	defer transport.CloseIdleConnections()
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		var u *url.Error
		if errors.As(err, &u) && (strings.Contains(u.Err.Error(), "unsupported protocol scheme") || strings.Contains(u.Err.Error(), "no Host in request URL")) {
			return nil, &Failure{Kind: "request", Error: fmt.Sprintf("%s request failed: %v", label, err), Type: "api_error"}
		}
		return nil, transportFailure(err, label, read)
	}
	defer response.Body.Close()
	raw, err := Idle(response.Body, read, cancel)
	if err != nil {
		return nil, &Failure{Kind: "connection", Error: fmt.Sprintf("%s connection error: %v", label, err), Type: "connection_error"}
	}
	if response.StatusCode >= 400 {
		if message == nil {
			message = Message
		}
		said := message(response.StatusCode, raw)
		return nil, &Failure{Kind: "http", Error: fmt.Sprintf("%s image generation failed (%d): %s", label, response.StatusCode, said), Type: "api_error",
			Status: response.StatusCode, Message: said}
	}
	var reply any
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, &Failure{Kind: "invalid_json", Error: fmt.Sprintf("%s returned invalid JSON: %v", label, err), Type: "invalid_response", Message: err.Error()}
	}
	return reply, nil
}

// Failure of a request that got no reply: a limit that ran out, a host that can't be reached, or a broken connection
func transportFailure(err error, label string, read time.Duration) *Failure {
	var u *url.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &u) && u.Timeout() {
		return &Failure{Kind: "timeout", Error: fmt.Sprintf("%s image generation timed out (%ds)", label, int(read.Seconds())), Type: "timeout"}
	}
	var dns *net.DNSError
	var op *net.OpError
	if errors.As(err, &dns) || errors.As(err, &op) && op.Op == "dial" {
		return &Failure{Kind: "unreachable", Error: fmt.Sprintf("%s connection error: %v", label, err), Type: "connection_error"}
	}
	return &Failure{Kind: "connection", Error: fmt.Sprintf("%s connection error: %v", label, err), Type: "connection_error"}
}

// Body of a reply with every wait for more bounded by the limit, the request cancelled when one runs out
func Idle(r io.Reader, limit time.Duration, cancel context.CancelFunc) ([]byte, error) {
	if limit <= 0 {
		return io.ReadAll(r)
	}
	timer := time.AfterFunc(limit, cancel)
	defer timer.Stop()
	var out bytes.Buffer
	chunk := make([]byte, 32<<10)
	for {
		n, err := r.Read(chunk)
		out.Write(chunk[:n])
		timer.Reset(limit)
		if err == io.EOF {
			return out.Bytes(), nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// Reads a JSON object from a GET, an error naming the status and the head of the body when it failed
func Get(ctx context.Context, address string, headers map[string]string, timeout time.Duration) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", Agent)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if response.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", response.StatusCode, strs.Head(string(raw), 300))
	}
	var reply map[string]any
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, err
	}
	return reply, nil
}

// Object of a JSON value, nil for anything else
func Object(value any) map[string]any {
	object, _ := value.(map[string]any)
	return object
}

// List under the key of an object, nil when there is none
func List(value any, key string) []any {
	list, _ := Object(value)[key].([]any)
	return list
}

// Trimmed text under the key of an object, empty when it is no text
func Text(value any, key string) string {
	text, _ := Object(value)[key].(string)
	return strings.TrimSpace(text)
}
