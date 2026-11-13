package matrix

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/provider"
)

// In turn secret read with no env fallback, empty on a miss
func standaloneSecret(ctx context.Context, name string) (string, error) {
	v, ok, err := provider.GetSecret(ctx, name)
	if err != nil || !ok {
		return "", err
	}
	return v, nil
}

// Out of process delivery through the Client-Server API for cron without a running gateway
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	chatID := strOf(kwargs["chat_id"])
	message := strOf(kwargs["message"])
	var extra map[string]any
	var token string
	if pconfig != nil {
		extra = pconfig.Extra
		token = tokenOf(pconfig.Token)
	}
	homeserver := ""
	if v := extra["homeserver"]; pyTruthy(v) {
		homeserver = strOf(v)
	} else {
		secret, err := standaloneSecret(ctx, "MATRIX_HOMESERVER")
		if err != nil {
			return gateway.SendError("Matrix send failed: " + pyrepr.Str(err)), nil
		}
		homeserver = secret
	}
	homeserver = strings.TrimRight(homeserver, "/")
	if token == "" {
		secret, err := standaloneSecret(ctx, "MATRIX_ACCESS_TOKEN")
		if err != nil {
			return gateway.SendError("Matrix send failed: " + pyrepr.Str(err)), nil
		}
		token = secret
	}
	if homeserver == "" || token == "" {
		return gateway.SendError("Matrix not configured (MATRIX_HOMESERVER, MATRIX_ACCESS_TOKEN required)"), nil
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	doSend := func(payload map[string]any) (map[string]any, error) {
		sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		random := make([]byte, 4)
		_, _ = rand.Read(random)
		txnID := fmt.Sprintf("icebath_%d_%s", time.Now().UnixMilli(), hex.EncodeToString(random))
		endpoint := homeserver + "/_matrix/client/v3/rooms/" + pyurl.Quote(chatID, "") + "/send/m.room.message/" + txnID
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(sctx, http.MethodPut, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		text, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != 200 && resp.StatusCode != 201 {
			return gateway.SendError(fmt.Sprintf("Matrix API error (%d): %s", resp.StatusCode, string(text))), nil
		}
		var data map[string]any
		if err := json.Unmarshal(text, &data); err != nil {
			return nil, err
		}
		return map[string]any{"success": true, "platform": "matrix", "chat_id": chatID, "message_id": data["event_id"]}, nil
	}
	var result map[string]any
	for _, payload := range standalonePayloads(message) {
		r, err := doSend(payload)
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return gateway.SendError("Matrix API timeout (30s)"), nil
		}
		if err != nil {
			return gateway.SendError("Matrix send failed: " + pyrepr.Str(err)), nil
		}
		result = r
		if !pyTruthy(result["success"]) {
			return result, nil
		}
	}
	if result == nil {
		return gateway.SendError("Matrix send failed: cannot access local variable 'result' where it is not associated with a value"), nil
	}
	return result, nil
}

var standaloneHeadingRE = pyre.MustCompile(`<h[1-6]>(.*?)</h[1-6]>`)

// One m.room.message content per UTF-8 bounded chunk, HTML attached while it fits the content budget
func standalonePayloads(message string) []map[string]any {
	var payloads []map[string]any
	for _, chunk := range gateway.TruncateMessage(message, matrixMaxMessageLengthCeiling, utf8Len) {
		payload := map[string]any{"msgtype": "m.text", "body": chunk}
		tokenized, texStore := latexToTokens(chunk)
		rendered := renderMarkdown(standaloneMarkdown, tokenized)
		rendered = reSub(standaloneHeadingRE, rendered, func(m regexp2.Match) string { return "<strong>" + group(m, 1) + "</strong>" })
		formatted := map[string]any{"format": "org.matrix.custom.html", "formatted_body": tokensToMxMaths(rendered, texStore)}
		merged := maps.Clone(payload)
		maps.Copy(merged, formatted)
		if contentBytes(merged) <= maxContentBytes {
			maps.Copy(payload, formatted)
		}
		payloads = append(payloads, payload)
	}
	return payloads
}
