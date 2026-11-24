package signal

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/netpref"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	httputil "github.com/nickheyer/icebath/pkg/utils/http"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Transport of out of process sends, a clone of the default transport when nil
var standaloneTransport http.RoundTripper

// Posts one batch and returns the decoded RPC response
type batchPoster func(ctx context.Context, attachments []any, message string) (any, error)

// The error member of a decoded RPC response; an array or text response holds none unless it names an error, which fails like any other kind of value
func rpcErrorMember(data any) (rpcErr any, present bool, err error) {
	if m, isDict := jsonutil.AsMap(data); isDict {
		rpcErr, present = m["error"]
		return rpcErr, present, nil
	}
	switch d := data.(type) {
	case []any:
		for _, item := range d {
			if item == "error" {
				return nil, false, errors.New("signal-cli RPC response is an array that names an error instead of an object")
			}
		}
		return nil, false, nil
	case string:
		if strings.Contains(d, "error") {
			return nil, false, errors.New("signal-cli RPC response is text that names an error instead of an object")
		}
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("signal-cli RPC response: %w", unexpectedKind("a JSON object", data))
}

// Text of a failed request to the signal-cli daemon at target for a log line or a send result, the kind of error and the proxy's host when the proxy URL
// the client reads from the environment holds a secret the raw text could repeat
func proxySafeErrText(err error, target string) string {
	return cmp.Or(gateway.DescribeProxyError(err, gateway.EnvProxyForRedaction(target)), err.Error())
}

// One batch posted to target under the scheduler with rate limit retries, lost when retries ran out and a payload for an RPC error
func signalSendBatch(ctx context.Context, post batchPoster, scheduler *SignalAttachmentScheduler, idx, nBatches int, attBatch []any, batchMessage, target string) (lost bool, errPayload map[string]any, err error) {
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
			plain := jsonutil.Plain(rpcErr)
			if !isSignalRateLimitError(plain) {
				return false, gateway.SendError(fmt.Sprintf("Signal RPC error on batch %d/%d: %s", idx+1, nBatches, anyutil.String(rpcErr))), nil
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
				pkgLog().Error(fmt.Sprintf("Signal: rate-limit retries exhausted on batch %d/%d (%d attachments lost, server retry_after=%s)",
					idx+1, nBatches, n, label))
				return true, nil, nil
			}
			pkgLog().Warn(fmt.Sprintf("Signal: rate-limited on batch %d/%d (attempt %d/%d, server retry_after=%s); scheduler will pace the retry",
				idx+1, nBatches, attempt, maxAttempts, label))
			continue
		}
		if ctx.Err() != nil {
			return false, nil, ctx.Err()
		}
		if attempt >= maxAttempts {
			pkgLog().Error(fmt.Sprintf("Signal: send error on batch %d/%d after %d attempts: %s", idx+1, nBatches, attempt, proxySafeErrText(attemptErr, target)))
			return true, nil, nil
		}
		pkgLog().Warn(fmt.Sprintf("Signal: transient error on batch %d/%d (attempt %d/%d): %s; will retry", idx+1, nBatches, attempt, maxAttempts, proxySafeErrText(attemptErr, target)))
	}
	return false, nil, nil
}

// One send RPC posted on a fresh client with the given timeout
func standaloneRPCSend(ctx context.Context, httpURL string, params *jsonutil.Object, idPrefix string, timeout float64) (*httputil.Response, error) {
	payload := jsonutil.ObjectOf("jsonrpc", "2.0", "method", "send", "params", params, "id", fmt.Sprintf("%s_%d", idPrefix, int64(wallClock()*1000)))
	body, err := jsonutil.Encode(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, httpURL+"/api/v1/rpc", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	transport := standaloneTransport
	if transport == nil {
		transport = netpref.Follow(http.DefaultTransport.(*http.Transport).Clone())
	}
	client := &http.Client{Transport: transport, CheckRedirect: httputil.RedirectPolicy(false)}
	defer client.CloseIdleConnections()
	resp, err := httputil.Do(client, req, httputil.EachPhase(time.Duration(timeout*float64(time.Second))))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return resp, nil
}

// signal-cli JSON-RPC send with attachments batched and metered by the process wide scheduler
func sendSignal(ctx context.Context, extra map[string]any, chatID, message string, mediaFiles []gateway.MediaRef) (map[string]any, error) {
	result, err := sendSignalBatches(ctx, extra, chatID, message, mediaFiles)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		target := ""
		if v := getOr(extra, "http_url", "http://127.0.0.1:8080"); anyutil.IsTrue(v) {
			target = anyutil.String(v)
		}
		return gateway.SendError("Signal send failed: " + proxySafeErrText(err, target)), nil
	}
	return result, nil
}

// Batches of the send with the text on the first one, the result payload or the error the caller wraps
func sendSignalBatches(ctx context.Context, extra map[string]any, chatID, message string, mediaFiles []gateway.MediaRef) (map[string]any, error) {
	rawURL := getOr(extra, "http_url", "http://127.0.0.1:8080")
	urlText, ok := rawURL.(string)
	if !ok {
		return nil, unexpectedKind("a string", rawURL)
	}
	httpURL, account := strings.TrimRight(urlText, "/"), getOr(extra, "account", "")
	if !anyutil.IsTrue(account) {
		return map[string]any{"error": "Signal account not configured"}, nil
	}
	var attachmentPaths []any
	for _, media := range mediaFiles {
		if _, err := os.Stat(media.Path); err == nil {
			attachmentPaths = append(attachmentPaths, media.Path)
		} else {
			pkgLog().Warn(fmt.Sprintf("Signal media file not found, skipping: %s", media.Path))
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
	rpcSend := func(ctx context.Context, text, idPrefix string, timeout float64, attachments []any, styled bool) (*httputil.Response, error) {
		params := jsonutil.ObjectOf("account", account, "message", text)
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
		resp, err := rpcSend(ctx, batchMessage, "send", signalSendTimeout(len(batchAttachments)), batchAttachments, true)
		if err != nil {
			return nil, err
		}
		if err := resp.CheckStatus(); err != nil {
			return nil, err
		}
		return resp.JSON()
	}
	scheduler := GetScheduler()
	pkgLog().Info(fmt.Sprintf("send_message Signal: scheduler state=%s, %d attachment(s) in %d batch(es)", anyutil.String(scheduler.State()), len(attachmentPaths), nBatches))
	var failedBatches []string
	for idx, attBatch := range attBatches {
		if n := len(attBatch); n > 0 {
			if estimated := scheduler.EstimateWait(n); estimated >= SignalBatchPacingNoticeThreshold {
				notice := fmt.Sprintf("(More images coming — pausing ~%s for Signal rate limit, batch %d/%d.)", formatWait(ctx, estimated), idx+1, nBatches)
				if _, err := rpcSend(ctx, notice, "notice", 30.0, nil, false); err != nil {
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					pkgLog().Warn("Signal: inline notice failed: " + proxySafeErrText(err, httpURL))
				}
			}
		}
		batchMessage := ""
		if idx == 0 {
			batchMessage = plainText
		}
		lost, errPayload, err := signalSendBatch(ctx, post, scheduler, idx, nBatches, attBatch, batchMessage, httpURL)
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
