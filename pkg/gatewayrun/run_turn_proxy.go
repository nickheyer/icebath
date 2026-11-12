package gatewayrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Longest SSE line the proxy reader buffers before failing the stream
const gatewayProxySSEBufferMaxChars = 16 * 1024 * 1024

// Seconds a proxy connection may take to open and a proxy body read may stall
const (
	proxySockConnect = 30 * time.Second
	proxySockRead    = 1800 * time.Second
)

// Keyword arguments of one proxied agent run
type proxyRunParams struct {
	message            string
	contextPrompt      string
	history            []map[string]any
	source             *gateway.SessionSource
	sessionID          string
	sessionKey         string
	runGeneration      *int
	eventMessageID     *string
	scheduledHeartbeat bool
}

// Reader that cancels its request when one read stalls past the socket read timeout
type idleTimeoutReader struct {
	body    io.Reader
	timeout time.Duration
	cancel  context.CancelFunc
}

func (r *idleTimeoutReader) Read(p []byte) (int, error) {
	timer := time.AfterFunc(r.timeout, r.cancel)
	defer timer.Stop()
	return r.body.Read(p)
}

// Text of the first n runes
func firstRunes(text string, n int) string {
	runes := []rune(text)
	return string(runes[:min(len(runes), n)])
}

// Forwards the message to a remote API server and streams its SSE reply instead of running a local agent
func (r *GatewayRunner) runAgentViaProxy(ctx context.Context, p proxyRunParams) (map[string]any, error) {
	proxyURL := r.getProxyURL(ctx)
	if proxyURL == "" {
		return proxyErrorResult(i18n.T(ctx, "gateway.proxy.url_missing", "", nil)), nil
	}
	proxyKey := pystr.Strip(provider.GetSecretStr(ctx, "GATEWAY_PROXY_KEY", ""))
	runStillCurrent := r.runStillCurrentFn(p.sessionKey, p.runGeneration)
	staleResult := func(what string) map[string]any {
		generation := 0
		if p.runGeneration != nil {
			generation = *p.runGeneration
		}
		key := p.sessionKey
		if key == "" {
			key = "?"
		}
		runLog().Info(fmt.Sprintf("Discarding stale proxy %s for %s — generation %d is no longer current", what, key, generation))
		return map[string]any{"final_response": "", "messages": []any{}, "api_calls": 0, "tools": []any{},
			"history_offset": len(p.history), "session_id": p.sessionID, "response_previewed": false}
	}
	var apiMessages []map[string]any
	if p.contextPrompt != "" {
		apiMessages = append(apiMessages, map[string]any{"role": "system", "content": p.contextPrompt})
	}
	for _, msg := range p.history {
		role := msg["role"]
		if (role == "user" || role == "assistant") && pyrepr.Truthy(msg["content"]) {
			apiMessages = append(apiMessages, map[string]any{"role": role, "content": msg["content"]})
		}
	}
	apiMessages = append(apiMessages, map[string]any{"role": "user", "content": p.message})
	body, err := json.Marshal(map[string]any{"model": "icebath-agent", "messages": apiMessages, "stream": true})
	if err != nil {
		return nil, err
	}
	threadMetadata := r.threadMetadataForSource(p.source, p.eventMessageID)
	var consumer *gateway.GatewayStreamConsumer
	if !p.scheduledHeartbeat {
		consumer = r.proxyStreamConsumer(ctx, p.source, p.eventMessageID, threadMetadata, runStillCurrent)
	}
	var streamDone chan struct{}
	consumerCtx, cancelConsumer := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelConsumer()
	if consumer != nil {
		streamDone = make(chan struct{})
		go func() {
			defer close(streamDone)
			consumer.Run(consumerCtx)
		}()
	}
	adapter, err := r.deliveryAdapterFor(ctx, p.source)
	if err == nil && adapter != nil && !p.scheduledHeartbeat {
		_ = adapter.SendTyping(ctx, p.source.ChatID, gateway.TypingMetadataForTurn(adapter, p.source.ChatID, p.eventMessageID, threadMetadata))
	}
	start := time.Now()
	fullResponse, result, err := r.streamProxyReply(ctx, proxyURL, proxyKey, p.sessionID, body, consumer, runStillCurrent, staleResult)
	if consumer != nil {
		consumer.Finish(nil)
		select {
		case <-streamDone:
		case <-time.After(5 * time.Second):
			cancelConsumer()
			<-streamDone
		}
	}
	if err != nil {
		return nil, err
	}
	if result != nil {
		return result, nil
	}
	elapsed := time.Since(start).Seconds()
	if !runStillCurrent() {
		return staleResult("result"), nil
	}
	runLog().Info(fmt.Sprintf("proxy response: url=%s session=%s time=%.1fs response=%d chars", proxyURL, firstRunes(p.sessionID, 20), elapsed, utf8.RuneCountInString(fullResponse)))
	final := fullResponse
	if final == "" {
		final = i18n.T(ctx, "gateway.proxy.no_response", "", nil)
	}
	return map[string]any{
		"final_response": final,
		"messages":       []any{map[string]any{"role": "user", "content": p.message}, map[string]any{"role": "assistant", "content": fullResponse}},
		"api_calls":      1, "tools": []any{}, "history_offset": len(p.history), "session_id": p.sessionID,
		"response_previewed": consumer != nil && fullResponse != "",
	}, nil
}

// Posts the chat completion and folds its SSE deltas, a non nil result ending the run early
func (r *GatewayRunner) streamProxyReply(ctx context.Context, proxyURL, proxyKey, sessionID string, body []byte, consumer *gateway.GatewayStreamConsumer,
	runStillCurrent func() bool, staleResult func(string) map[string]any) (string, map[string]any, error) {
	fullResponse := ""
	consumeLine := func(line string) bool {
		line = pystr.Strip(line)
		if !strings.HasPrefix(line, "data: ") {
			return false
		}
		data := line[6:]
		if pystr.Strip(data) == "[DONE]" {
			return true
		}
		var frame map[string]any
		if json.Unmarshal([]byte(data), &frame) != nil {
			return false
		}
		choices, _ := frame["choices"].([]any)
		if len(choices) == 0 {
			return false
		}
		choice, ok := choices[0].(map[string]any)
		if !ok {
			return false
		}
		delta, ok := choice["delta"].(map[string]any)
		if _, present := choice["delta"]; present && !ok {
			return false
		}
		content, isText := delta["content"].(string)
		if _, present := delta["content"]; present && !isText && delta["content"] != nil {
			return false
		}
		if content != "" {
			fullResponse += content
			if consumer != nil {
				consumer.OnDelta(content)
			}
		}
		return false
	}
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, proxyURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return r.proxyConnectionError(ctx, proxyURL, fullResponse, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if proxyKey != "" {
		req.Header.Set("Authorization", "Bearer "+proxyKey)
	}
	if sessionID != "" {
		req.Header.Set("X-Icebath-Session-Id", sessionID)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{Timeout: proxySockConnect}).DialContext}}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", nil, ctx.Err()
		}
		return r.proxyConnectionError(ctx, proxyURL, fullResponse, err)
	}
	defer resp.Body.Close()
	reader := &idleTimeoutReader{body: resp.Body, timeout: proxySockRead, cancel: cancel}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(reader)
		errorText := pystr.DecodeUTF8Replace(raw)
		runLog().Warn(fmt.Sprintf("Proxy error (%d) from %s: %s", resp.StatusCode, proxyURL, firstRunes(errorText, 500)))
		return "", proxyErrorResult(i18n.T(ctx, "gateway.proxy.http_error", "", map[string]any{"status": resp.StatusCode, "error": firstRunes(errorText, 300)})), nil
	}
	buffer := ""
	sawDone := false
	chunk := make([]byte, 32*1024)
	for !sawDone {
		n, readErr := reader.Read(chunk)
		if n > 0 {
			if !runStillCurrent() {
				return "", staleResult("stream"), nil
			}
			buffer += pystr.DecodeUTF8Replace(chunk[:n])
			for strings.Contains(buffer, "\n") {
				line, rest, _ := strings.Cut(buffer, "\n")
				buffer = rest
				if consumeLine(line) {
					sawDone = true
					break
				}
			}
			if sawDone {
				break
			}
			if utf8.RuneCountInString(buffer) > gatewayProxySSEBufferMaxChars {
				return r.proxyConnectionError(ctx, proxyURL, fullResponse, errors.New("Proxy SSE stream exceeded max buffer size without a line boundary"))
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			if ctx.Err() != nil {
				return "", nil, ctx.Err()
			}
			return r.proxyConnectionError(ctx, proxyURL, fullResponse, readErr)
		}
	}
	if !sawDone && buffer != "" {
		sawDone = consumeLine(buffer)
	}
	if !sawDone {
		runLog().Warn(fmt.Sprintf("Proxy SSE stream from %s ended without [DONE] — response may be truncated (%d chars received)", proxyURL, utf8.RuneCountInString(fullResponse)))
		if fullResponse == "" {
			return "", proxyErrorResult(i18n.T(ctx, "gateway.proxy.closed_early", "", nil)), nil
		}
	}
	return fullResponse, nil, nil
}

// Logs a proxy failure, the connection error reply when nothing streamed, else the partial response
func (r *GatewayRunner) proxyConnectionError(ctx context.Context, proxyURL, fullResponse string, err error) (string, map[string]any, error) {
	runLog().Error(fmt.Sprintf("Proxy connection error to %s: %s", proxyURL, pyrepr.ExceptionMessage(err)))
	if fullResponse == "" {
		return "", proxyErrorResult(i18n.T(ctx, "gateway.proxy.connection_error", "", map[string]any{"error": pyrepr.ExceptionMessage(err)})), nil
	}
	return fullResponse, nil, nil
}
