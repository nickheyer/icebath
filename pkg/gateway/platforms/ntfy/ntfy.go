package ntfy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/gateway"
)

const (
	defaultServer  = "https://ntfy.sh"
	maxBodyBytes   = 4096
	publishTimeout = 15 * time.Second
	uploadTimeout  = 120 * time.Second
	echoTag        = "icebath-agent"
)

// Platform value of ntfy in the gateway registry
const platformNtfy = gateway.Platform("ntfy")

// Env values that switch markdown on
var markdownTruthy = map[string]bool{"1": true, "true": true, "yes": true}

// Text form of a config or JSON value, empty for nil
func textOf(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprint(x)
	}
}

// Whether a config value is set to something other than false, zero or empty
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case int:
		return x != 0
	case int64:
		return x != 0
	default:
		return true
	}
}

// Setting from the scoped env, then extra, then the default, stripped
func setting(ctx context.Context, extra map[string]any, key, env, def string) (string, error) {
	v, err := gateway.ExtraOrSecret(ctx, extra, key, env, def, true)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(textOf(v)), nil
}

// Server URL without trailing slashes, ntfy.sh by default
func serverURL(ctx context.Context, extra map[string]any) (string, error) {
	s, err := setting(ctx, extra, "server", "NTFY_SERVER_URL", defaultServer)
	if err != nil {
		return "", err
	}
	if s = strings.TrimRight(s, "/"); s == "" {
		s = defaultServer
	}
	return s, nil
}

// Whether markdown publishing is on in extra or the scoped NTFY_MARKDOWN env
func markdownEnabled(ctx context.Context, extra map[string]any) (bool, error) {
	switch v := extra["markdown"].(type) {
	case string:
		if markdownTruthy[strings.ToLower(strings.TrimSpace(v))] {
			return true, nil
		}
	default:
		if truthy(v) {
			return true, nil
		}
	}
	env, err := gateway.GetScopedSecret(ctx, "NTFY_MARKDOWN", "", false)
	if err != nil {
		return false, err
	}
	return markdownTruthy[strings.ToLower(strings.TrimSpace(env))], nil
}

// Authorization header value of a token, Basic for user and password pairs and Bearer otherwise
func authHeader(token string) string {
	token = strings.TrimSpace(token)
	switch {
	case token == "":
		return ""
	case strings.Contains(token, ":"):
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(token))
	default:
		return "Bearer " + token
	}
}

// Header value safe for the wire, RFC 2047 encoded when it carries non ASCII or control text
func headerValue(s string) string {
	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			return mime.BEncoding.Encode("UTF-8", s)
		}
	}
	return s
}

// Random hex digits of the given length standing in for an id the server did not return
func randomID(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

// Byte length, the unit of the ntfy message cap
func byteLen(s string) int { return len(s) }

// Message chunks of at most the body cap in bytes
func bodyChunks(message string) []string {
	if !utf8.ValidString(message) {
		message = strings.ToValidUTF8(message, "�")
	}
	return gateway.TruncateMessage(message, maxBodyBytes, byteLen)
}

// Topic URL of the server with the topic path escaped
func topicURL(server, topic string) string {
	return server + "/" + topicPath(topic)
}

// One publish request, its body text or file and the headers that shape it
type publishRequest struct {
	method   string
	topic    string
	body     []byte
	message  string
	filename string
	attach   string
	markdown bool
}

// Message id and failure text of one publish, the error for a transport failure
func publish(ctx context.Context, client *http.Client, server, token string, p publishRequest) (string, string, error) {
	timeout := publishTimeout
	if p.filename != "" {
		timeout = uploadTimeout
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	method := p.method
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(rctx, method, topicURL(server, p.topic), bytes.NewReader(p.body))
	if err != nil {
		return "", "", err
	}
	if auth := authHeader(token); auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.Header.Set("X-Tags", echoTag)
	if p.filename != "" {
		req.Header.Set("X-Filename", headerValue(p.filename))
		if p.message != "" {
			req.Header.Set("X-Message", headerValue(p.message))
		}
	} else {
		req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	}
	if p.attach != "" {
		req.Header.Set("X-Attach", p.attach)
	}
	if p.markdown {
		req.Header.Set("X-Markdown", "true")
	}
	resp, err := client.Do(req)
	if err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() && ctx.Err() == nil {
			return "", "Timeout publishing to ntfy", nil
		}
		return "", "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		text := string(raw)
		if len(text) > 200 {
			text = text[:200]
		}
		return "", fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.ToValidUTF8(text, "")), nil
	}
	var answer struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &answer) != nil || answer.ID == "" {
		return randomID(12), "", nil
	}
	return answer.ID, "", nil
}

// Publish request uploading a local file as an attachment with the caption as its message
func filePublish(topic, path, caption, fileName string, markdown bool, data []byte) publishRequest {
	name := fileName
	if name == "" {
		name = filepath.Base(path)
	}
	return publishRequest{method: http.MethodPut, topic: topic, body: data, message: caption, filename: name, markdown: markdown && caption != ""}
}

// HTTP transport honouring proxy env only when the gateway trusts the environment
func newTransport(ctx context.Context) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if gateway.GatewayTrustEnv(ctx) {
		transport.Proxy = http.ProxyFromEnvironment
	}
	transport.DialContext = (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 15 * time.Second
	return transport
}
