package signal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Transport of out of process sends, a clone of the default transport when nil
var standaloneTransport http.RoundTripper

// Posts one batch and returns the decoded RPC response
type batchPoster func(ctx context.Context, attachments []any, message string) (any, error)

// The error member of a decoded RPC response the way the in operator and subscript read it
func rpcErrorMember(data any) (rpcErr any, present bool, err error) {
	if m, isDict := pyjson.Dict(data); isDict {
		rpcErr, present = m["error"]
		return rpcErr, present, nil
	}
	switch d := data.(type) {
	case []any:
		for _, item := range d {
			if pyrepr.Equal(item, "error") {
				return nil, false, &pyErr{"TypeError", "list indices must be integers or slices, not str"}
			}
		}
		return nil, false, nil
	case string:
		if strings.Contains(d, "error") {
			return nil, false, &pyErr{"TypeError", "string indices must be integers, not 'str'"}
		}
		return nil, false, nil
	}
	return nil, false, &pyErr{"TypeError", fmt.Sprintf("argument of type '%s' is not iterable", pyrepr.TypeName(data))}
}

// One batch under the scheduler with rate limit retries, lost when retries ran out and a payload for an RPC error
func signalSendBatch(ctx context.Context, post batchPoster, scheduler *SignalAttachmentScheduler, idx, nBatches int, attBatch []any, batchMessage string) (lost bool, errPayload map[string]any, err error) {
	n, maxAttempts := len(attBatch), SignalRateLimitMaxAttempts
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		var data, rpcErr any
		var present bool
		_, attemptErr := scheduler.Acquire(ctx, n)
		rpcT0 := monotonic()
		if attemptErr == nil {
			data, attemptErr = post(ctx, attBatch, batchMessage)
		}
		if attemptErr == nil {
			rpcErr, present, attemptErr = rpcErrorMember(data)
		}
		var retryAfter float64
		var found, rateLimited bool
		if attemptErr == nil {
			if !present {
				scheduler.ReportRPCDuration(monotonic()-rpcT0, n)
				return false, nil, nil
			}
			plain := pyjson.Plain(rpcErr)
			if !isSignalRateLimitError(plain) {
				return false, gateway.SendError(fmt.Sprintf("Signal RPC error on batch %d/%d: %s", idx+1, nBatches, pyrepr.Str(rpcErr))), nil
			}
			if retryAfter, found, attemptErr = extractRetryAfterSeconds(plain); attemptErr == nil {
				rateLimited = true
			}
		}
		if rateLimited {
			var serverRetryAfter *float64
			label := "unknown"
			if found {
				serverRetryAfter = &retryAfter
				if retryAfter != 0 {
					label = fmt.Sprintf("%.0fs", retryAfter)
				}
			}
			scheduler.Feedback(serverRetryAfter, n)
			if attempt >= maxAttempts {
				slog.Error(fmt.Sprintf("Signal: rate-limit retries exhausted on batch %d/%d (%d attachments lost, server retry_after=%s)",
					idx+1, nBatches, n, label))
				return true, nil, nil
			}
			slog.Warn(fmt.Sprintf("Signal: rate-limited on batch %d/%d (attempt %d/%d, server retry_after=%s); scheduler will pace the retry",
				idx+1, nBatches, attempt, maxAttempts, label))
			continue
		}
		if ctx.Err() != nil {
			return false, nil, ctx.Err()
		}
		if attempt >= maxAttempts {
			slog.Error(fmt.Sprintf("Signal: send error on batch %d/%d after %d attempts: %s", idx+1, nBatches, attempt, errText(attemptErr)))
			return true, nil, nil
		}
		slog.Warn(fmt.Sprintf("Signal: transient error on batch %d/%d (attempt %d/%d): %s; will retry", idx+1, nBatches, attempt, maxAttempts, errText(attemptErr)))
	}
	return false, nil, nil
}

// One send RPC posted on a fresh client with the given timeout
func standaloneRPCSend(ctx context.Context, httpURL string, params *pyjson.Object, idPrefix string, timeout float64) (*pyhttpx.Response, string, error) {
	payload := pyjson.ObjectOf("jsonrpc", "2.0", "method", "send", "params", params, "id", fmt.Sprintf("%s_%d", idPrefix, int64(wallClock()*1000)))
	body, err := pyjson.Dumps(payload, pyjson.EnsureASCII(false), pyjson.Separators(",", ":"), pyjson.AllowNaN(false))
	if err != nil {
		return nil, "", err
	}
	rawURL := httpURL + "/api/v1/rpc"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader([]byte(body)))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	transport := standaloneTransport
	if transport == nil {
		transport = http.DefaultTransport.(*http.Transport).Clone()
	}
	client := &http.Client{Transport: transport, CheckRedirect: pyhttpx.RedirectPolicy(false)}
	defer client.CloseIdleConnections()
	resp, err := pyhttpx.Do(client, req, time.Duration(timeout*float64(time.Second)))
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		var te *pyhttpx.TimeoutError
		if errors.As(err, &te) {
			return nil, "", &pyErr{"ReadTimeout", ""}
		}
		return nil, "", &pyErr{"ConnectError", pyhttpx.ErrorText(err)}
	}
	return resp, rawURL, nil
}

// signal-cli JSON-RPC send with attachments batched and metered by the process wide scheduler
func sendSignal(ctx context.Context, extra map[string]any, chatID, message string, mediaFiles []gateway.MediaRef) (map[string]any, error) {
	result, err := sendSignalBatches(ctx, extra, chatID, message, mediaFiles)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return gateway.SendError(fmt.Sprintf("Signal send failed: %s", errText(err))), nil
	}
	return result, nil
}

// Batches of the send with the text on the first one, the result payload or the error the caller wraps
func sendSignalBatches(ctx context.Context, extra map[string]any, chatID, message string, mediaFiles []gateway.MediaRef) (map[string]any, error) {
	rawURL := getOr(extra, "http_url", "http://127.0.0.1:8080")
	urlText, ok := rawURL.(string)
	if !ok {
		return nil, noAttr(rawURL, "rstrip")
	}
	httpURL, account := pystr.RstripChars(urlText, "/"), getOr(extra, "account", "")
	if !pyrepr.Truthy(account) {
		return map[string]any{"error": "Signal account not configured"}, nil
	}
	var attachmentPaths []any
	for _, media := range mediaFiles {
		if _, err := os.Stat(media.Path); err == nil {
			attachmentPaths = append(attachmentPaths, media.Path)
		} else {
			slog.Warn(fmt.Sprintf("Signal media file not found, skipping: %s", media.Path))
		}
	}
	var attBatches [][]any
	for i := 0; i < len(attachmentPaths); i += SignalMaxAttachmentsPerMsg {
		attBatches = append(attBatches, attachmentPaths[i:min(i+SignalMaxAttachmentsPerMsg, len(attachmentPaths))])
	}
	if len(attBatches) == 0 {
		attBatches = [][]any{{}}
	}
	nBatches := len(attBatches)
	plainText, textStyles := markdownToSignal(message)
	rpcSend := func(ctx context.Context, text, idPrefix string, timeout float64, attachments []any, styled bool) (*pyhttpx.Response, string, error) {
		params := pyjson.ObjectOf("account", account, "message", text)
		if strings.HasPrefix(chatID, "group:") {
			params.Set("groupId", chatID[6:])
		} else {
			params.Set("recipient", []any{chatID})
		}
		if styled && text != "" && len(textStyles) > 0 {
			if len(textStyles) == 1 {
				params.Set("textStyle", textStyles[0])
			} else {
				params.Set("textStyles", textStyles)
			}
		}
		if len(attachments) > 0 {
			params.Set("attachments", attachments)
		}
		return standaloneRPCSend(ctx, httpURL, params, idPrefix, timeout)
	}
	post := func(ctx context.Context, batchAttachments []any, batchMessage string) (any, error) {
		resp, reqURL, err := rpcSend(ctx, batchMessage, "send", signalSendTimeout(len(batchAttachments)), batchAttachments, true)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, &pyErr{"HTTPStatusError", resp.StatusErrorText(reqURL)}
		}
		return resp.JSON()
	}
	scheduler := GetScheduler()
	slog.Info(fmt.Sprintf("send_message Signal: scheduler state=%s, %d attachment(s) in %d batch(es)", pyrepr.Str(scheduler.State()), len(attachmentPaths), nBatches))
	var failedBatches []string
	for idx, attBatch := range attBatches {
		if n := len(attBatch); n > 0 {
			if estimated := scheduler.EstimateWait(n); estimated >= SignalBatchPacingNoticeThreshold {
				notice := fmt.Sprintf("(More images coming — pausing ~%s for Signal rate limit, batch %d/%d.)", formatWait(ctx, estimated), idx+1, nBatches)
				if _, _, err := rpcSend(ctx, notice, "notice", 30.0, nil, false); err != nil {
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					slog.Warn(fmt.Sprintf("Signal: inline notice failed: %s", errText(err)))
				}
			}
		}
		batchMessage := ""
		if idx == 0 {
			batchMessage = plainText
		}
		lost, errPayload, err := signalSendBatch(ctx, post, scheduler, idx, nBatches, attBatch, batchMessage)
		switch {
		case err != nil:
			return nil, err
		case lost:
			failedBatches = append(failedBatches, fmt.Sprint(idx+1))
		case errPayload != nil:
			return errPayload, nil
		}
	}
	var warnings []string
	if len(attachmentPaths) < len(mediaFiles) {
		warnings = append(warnings, "Some media files were skipped (not found on disk)")
	}
	if len(failedBatches) > 0 {
		warnings = append(warnings, fmt.Sprintf("Signal rate-limited %d batch(es) (#%s)", len(failedBatches), strings.Join(failedBatches, ", #")))
	}
	if len(failedBatches) > 0 && len(failedBatches) == nBatches {
		return gateway.SendError(fmt.Sprintf("Signal: every batch (%d) hit rate limit; no attachments delivered", nBatches)), nil
	}
	shownChatID := chatID
	if strings.HasPrefix(chatID, "group:") {
		shownChatID = "group:***"
	}
	out := map[string]any{"success": true, "platform": "signal", "chat_id": shownChatID}
	if len(warnings) > 0 {
		out["warnings"] = warnings
	}
	return out, nil
}

// Out of process send where attachments ride the JSON-RPC attachments param
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	chatID, _ := kwargs["chat_id"].(string)
	message, _ := kwargs["message"].(string)
	mediaFiles, _ := kwargs["media_files"].([]gateway.MediaRef)
	extra := map[string]any{}
	if pconfig != nil && pconfig.Extra != nil {
		extra = pconfig.Extra
	}
	return sendSignal(ctx, extra, chatID, message, mediaFiles)
}
