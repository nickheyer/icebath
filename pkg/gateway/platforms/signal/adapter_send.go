package signal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Keyword arguments of one JSON-RPC call beyond its method and params
type rpcOptions struct {
	rpcID            string
	quietFailures    bool
	raiseOnRateLimit bool
	timeout          float64
}

// Sends one JSON-RPC 2.0 request to signal-cli, nil on any failure and a rate limit error only when asked
func (a *SignalAdapter) rpc(ctx context.Context, method string, params *pyjson.Object, o rpcOptions) (any, error) {
	client := a.httpClient()
	if client == nil {
		slog.Warn("Signal: RPC called but client not connected")
		return nil, nil
	}
	rpcID := o.rpcID
	if rpcID == "" {
		rpcID = fmt.Sprintf("%s_%d", method, int64(wallClock()*1000))
	}
	timeout := o.timeout
	if timeout == 0 {
		timeout = 30.0
	}
	logFailure := slog.Warn
	if o.quietFailures {
		logFailure = slog.Debug
	}
	result, err := a.rpcOnce(ctx, client, method, pyjson.ObjectOf("jsonrpc", "2.0", "method", method, "params", params, "id", rpcID),
		time.Duration(timeout*float64(time.Second)), o.raiseOnRateLimit, logFailure)
	var rateLimit *SignalRateLimitError
	switch {
	case err == nil:
		return result, nil
	case errors.As(err, &rateLimit):
		return nil, err
	case ctx.Err() != nil:
		return nil, ctx.Err()
	}
	logFailure(fmt.Sprintf("Signal RPC %s failed: %s", method, errText(err)))
	return nil, nil
}

// One RPC round trip, an RPC error object logged and answered with nil unless it is a wanted rate limit
func (a *SignalAdapter) rpcOnce(ctx context.Context, client *http.Client, method string, payload *pyjson.Object, timeout time.Duration, raiseOnRateLimit bool, logFailure func(string, ...any)) (any, error) {
	body, err := pyjson.Dumps(payload, pyjson.EnsureASCII(false), pyjson.Separators(",", ":"), pyjson.AllowNaN(false))
	if err != nil {
		return nil, err
	}
	rawURL := a.httpURL + "/api/v1/rpc"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader([]byte(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := pyhttpx.Do(client, req, timeout)
	if err != nil {
		var te *pyhttpx.TimeoutError
		if errors.As(err, &te) {
			return nil, &pyErr{"ReadTimeout", ""}
		}
		return nil, &pyErr{"ConnectError", pyhttpx.ErrorText(err)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &pyErr{"HTTPStatusError", resp.StatusErrorText(rawURL)}
	}
	decoded, err := resp.JSON()
	if err != nil {
		return nil, err
	}
	data, isDict := pyjson.Dict(decoded)
	if !isDict {
		switch decoded.(type) {
		case []any, string:
			return nil, noAttr(decoded, "get")
		}
		return nil, &pyErr{"TypeError", fmt.Sprintf("argument of type '%s' is not iterable", pyrepr.TypeName(decoded))}
	}
	if rpcErr, present := data["error"]; present {
		if plain := pyjson.Plain(rpcErr); raiseOnRateLimit && isSignalRateLimitError(plain) {
			retryAfter, found, err := extractRetryAfterSeconds(plain)
			if err != nil {
				return nil, err
			}
			rateLimit := &SignalRateLimitError{Message: errorMessage(plain)}
			if found {
				rateLimit.RetryAfter = &retryAfter
			}
			return nil, rateLimit
		}
		logFailure(fmt.Sprintf("Signal RPC error (%s): %s", method, pyrepr.Str(rpcErr)))
		return nil, nil
	}
	result := data["result"]
	if rm, isDict := pyjson.Dict(result); isDict && raiseOnRateLimit {
		if results, isList := rm["results"].([]any); isList {
			for _, r := range results {
				entry, isDict := pyjson.Dict(r)
				if !isDict || !pyrepr.Equal(entry["type"], "RATE_LIMIT_FAILURE") {
					continue
				}
				rateLimit := &SignalRateLimitError{Message: "Rate limit exceeded for recipient"}
				switch v := entry["retryAfterSeconds"].(type) {
				case float64:
					rateLimit.RetryAfter = &v
				case int64:
					seconds := float64(v)
					rateLimit.RetryAfter = &seconds
				}
				return nil, rateLimit
			}
		}
	}
	return result, nil
}

// Plain text passthrough for the base class send path, Send applying the rich styles itself
func (a *SignalAdapter) FormatMessage(content string) string { return content }

// Whether every recipient result of a send response succeeded, with the failure text otherwise
func validateSendResult(result any) (bool, string) {
	rm, isDict := pyjson.Dict(result)
	if !isDict {
		return true, ""
	}
	results, isList := rm["results"].([]any)
	if !isList {
		return true, ""
	}
	for _, r := range results {
		entry, isDict := pyjson.Dict(r)
		if !isDict {
			continue
		}
		if rtype := entry["type"]; pyrepr.Truthy(rtype) && !pyrepr.Equal(rtype, "SUCCESS") {
			return false, pyrepr.Str(rtype)
		}
		if success, present := entry["success"]; present && !pyrepr.Truthy(success) {
			return false, pyrepr.Str(pyrepr.Or(entry["failure"], "Recipient delivery failed"))
		}
	}
	return true, ""
}

// Cumulative UTF-16 offsets at every code point boundary of the text
func utf16Offsets(runes []rune) []int {
	offsets := make([]int, len(runes)+1)
	for i, r := range runes {
		offsets[i+1] = offsets[i] + utf16LenRunes([]rune{r})
	}
	return offsets
}

// Full message style ranges translated into the ranges local to one chunk
func stylesForChunk(textStyles []string, chunkStart, chunkEnd int) []string {
	adjusted := []string{}
	for _, styleString := range textStyles {
		parts := strings.SplitN(styleString, ":", 3)
		var start, length int
		ok := len(parts) == 3
		if ok {
			s, sok := pystr.IntText(parts[0])
			l, lok := pystr.IntText(parts[1])
			if ok = sok && lok && s.IsInt64() && l.IsInt64(); ok {
				start, length = int(s.Int64()), int(l.Int64())
			}
		}
		if !ok {
			slog.Debug(fmt.Sprintf("[Signal] Ignoring malformed textStyle range: %s", pyrepr.Repr(styleString)))
			continue
		}
		overlapStart, overlapEnd := max(start, chunkStart), min(start+length, chunkEnd)
		if overlapStart < overlapEnd {
			adjusted = append(adjusted, fmt.Sprintf("%d:%d:%s", overlapStart-chunkStart, overlapEnd-overlapStart, parts[2]))
		}
	}
	return adjusted
}

// One outbound chunk of converted text with its chunk local style ranges
type signalChunk struct {
	text   string
	styles []string
}

// Converted Signal text split into chunks under the limit with the style ranges translated per chunk
func splitSignalFormattedMessage(plainText string, textStyles []string, maxLength int) []signalChunk {
	if gateway.UTF16Len(plainText) <= maxLength {
		return []signalChunk{{plainText, textStyles}}
	}
	bodyLimit := max(1, maxLength-10)
	runes := pystr.Runes(plainText)
	offsets := utf16Offsets(runes)
	var chunks []signalChunk
	startIdx, totalU16 := 0, offsets[len(offsets)-1]
	for offsets[startIdx] < totalU16 {
		endBudget := min(totalU16, offsets[startIdx]+bodyLimit)
		endIdx := startIdx + 1
		for endIdx < len(offsets) && offsets[endIdx] <= endBudget {
			endIdx++
		}
		endIdx = max(endIdx-1, startIdx+1)
		chunks = append(chunks, signalChunk{pystr.FromRunes(runes[startIdx:endIdx]), stylesForChunk(textStyles, offsets[startIdx], offsets[endIdx])})
		startIdx = endIdx
	}
	if len(chunks) == 1 {
		return chunks
	}
	for i := range chunks {
		chunks[i].text = fmt.Sprintf("%s (%d/%d)", chunks[i].text, i+1, len(chunks))
	}
	return chunks
}

// Runs a send RPC, validates and tracks it, returning the result or the failed send result
func (a *SignalAdapter) rpcSend(ctx context.Context, params *pyjson.Object, failError string) (any, *gateway.SendResult, error) {
	result, err := a.rpc(ctx, "send", params, rpcOptions{})
	if err != nil {
		return nil, nil, err
	}
	if result == nil {
		return nil, &gateway.SendResult{Success: false, Error: failError}, nil
	}
	if ok, errMsg := validateSendResult(result); !ok {
		return nil, &gateway.SendResult{Success: false, Error: errMsg, RawResponse: result}, nil
	}
	a.trackSentTimestamp(result)
	return result, nil, nil
}

// Sends a text message with native Signal formatting, chunked after the markdown conversion
func (a *SignalAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	if err := a.stopTypingIndicator(ctx, chatID); err != nil {
		return gateway.SendResult{}, err
	}
	if pystr.Strip(content) == "" {
		return gateway.SendResult{Success: true}, nil
	}
	baseParams, err := a.withTarget(ctx, pyjson.ObjectOf("account", a.account), chatID, true)
	if err != nil {
		return gateway.SendResult{}, err
	}
	plain, styles := markdownToSignal(content)
	chunks := splitSignalFormattedMessage(plain, styles, a.Traits.MaxMessageLength)
	var lastResult any
	for idx, chunk := range chunks {
		params := baseParams.Copy()
		params.Set("message", chunk.text)
		if len(chunk.styles) == 1 {
			params.Set("textStyle", chunk.styles[0])
		} else if len(chunk.styles) > 0 {
			params.Set("textStyles", chunk.styles)
		}
		slog.Info(fmt.Sprintf("[Signal] Sending response chunk %d/%d (%d chars) to %s", idx+1, len(chunks), pystr.Len(chunk.text), chatID))
		result, failed, err := a.rpcSend(ctx, params, "RPC send failed")
		if err != nil {
			return gateway.SendResult{}, err
		}
		if failed != nil {
			return *failed, nil
		}
		lastResult = result
	}
	return gateway.SendResult{Success: true, RawResponse: lastResult}, nil
}

// Records an outbound message timestamp for echo filtering under the time to live and the hard cap
func (a *SignalAdapter) trackSentTimestamp(rpcResult any) {
	rm, isDict := pyjson.Dict(rpcResult)
	if !isDict || !pyrepr.Truthy(rm["timestamp"]) {
		return
	}
	ts := rm["timestamp"]
	a.rememberSentMessageTimestamp(ts)
	now, key := monotonic(), tsKey(ts)
	a.mu.Lock()
	defer a.mu.Unlock()
	recent := a.recentSentTimestamps
	recent.Delete(key)
	recent.Set(key, now)
	cutoff := now - recentSentTTLSeconds
	for recent.Len() > 0 {
		oldest := recent.Keys()[0]
		if at, _ := recent.Get(oldest); at >= cutoff {
			break
		}
		recent.Delete(oldest)
	}
	for recent.Len() > maxRecentTimestamps {
		recent.Delete(recent.Keys()[0])
	}
}

// Pops a timestamp matching one this adapter sent, true for an echo
func (a *SignalAdapter) consumeSentTimestamp(ts any) bool {
	if !pyrepr.Truthy(ts) {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.recentSentTimestamps.Delete(tsKey(ts))
}

// Typing indicator with a per chat backoff, three straight failures skipping the RPC for a growing cooldown
func (a *SignalAdapter) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	now := monotonic()
	a.mu.Lock()
	skipUntil := a.typingSkipUntil[chatID]
	a.mu.Unlock()
	if now < skipUntil {
		return nil
	}
	params, err := a.withTarget(ctx, pyjson.ObjectOf("account", a.account), chatID, true)
	if err != nil {
		return err
	}
	a.mu.Lock()
	fails := a.typingFailures[chatID]
	a.mu.Unlock()
	result, err := a.rpc(ctx, "sendTyping", params, rpcOptions{rpcID: "typing", quietFailures: fails != 0})
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if result != nil {
		delete(a.typingFailures, chatID)
		delete(a.typingSkipUntil, chatID)
		return nil
	}
	fails++
	a.typingFailures[chatID] = fails
	if fails >= 3 {
		a.typingSkipUntil[chatID] = now + math.Min(60.0, 16.0*math.Pow(2, float64(fails-3)))
	}
	return nil
}

// Local path of an image URL, or the reason it cannot be sent with its detail
func (a *SignalAdapter) resolveImagePath(ctx context.Context, imageURL string) (path, reason string, detail any, err error) {
	var filePath string
	if strings.HasPrefix(imageURL, "file://") {
		filePath = pyurl.Unquote(imageURL[7:])
	} else {
		cached, cerr := cacheImageFromURL(ctx, imageURL, ".jpg", 2)
		if cerr != nil {
			if ctx.Err() != nil {
				return "", "", nil, ctx.Err()
			}
			return "", "download", cerr, nil
		}
		filePath = cached
	}
	if filePath == "" {
		return "", "missing", nil, nil
	}
	info, serr := os.Stat(filePath)
	if serr != nil {
		return "", "missing", nil, nil
	}
	if size := info.Size(); size > signalMaxAttachmentSize {
		return "", "oversize", size, nil
	}
	return filePath, "", nil, nil
}

// Warning logged for an image skipped from a batch, by skip reason
func logSkippedImage(reason, imageURL string, detail any) {
	switch reason {
	case "download":
		slog.Warn(fmt.Sprintf("Signal: failed to download image %s: %s", imageURL, errText(detail.(error))))
	case "missing":
		slog.Warn(fmt.Sprintf("Signal: image file not found for %s", imageURL))
	case "oversize":
		slog.Warn(fmt.Sprintf("Signal: image too large (%d bytes), skipping %s", detail.(int64), imageURL))
	}
}

// Sends a batch of images in chunked send RPCs paced by the scheduler, success when any batch was accepted
func (a *SignalAdapter) SendMultipleImages(ctx context.Context, chatID string, images []gateway.ImageRef, metadata map[string]any, humanDelay float64) (gateway.SendResult, error) {
	if len(images) == 0 {
		return gateway.SendResult{Success: false, Error: "no images to send"}, nil
	}
	scheduler := GetScheduler()
	slog.Info(fmt.Sprintf("Signal send_multiple_images: received %d image(s) for %s — scheduler state: %s", len(images),
		pystr.Slice(chatID, 0, 30), pyrepr.Str(scheduler.State())))
	if err := a.stopTypingIndicator(ctx, chatID); err != nil {
		return gateway.SendResult{}, err
	}
	var attachments []any
	skipped := map[string]int{"download": 0, "missing": 0, "oversize": 0}
	for _, image := range images {
		filePath, reason, detail, err := a.resolveImagePath(ctx, image.URL)
		if err != nil {
			return gateway.SendResult{}, err
		}
		if reason == "" {
			attachments = append(attachments, filePath)
			continue
		}
		skipped[reason]++
		logSkippedImage(reason, image.URL, detail)
	}
	if len(attachments) == 0 {
		slog.Error(fmt.Sprintf("Signal: no valid images in batch of %d (download=%d missing=%d oversize=%d)", len(images),
			skipped["download"], skipped["missing"], skipped["oversize"]))
		return gateway.SendResult{Success: false, Error: "no valid images in batch"}, nil
	}
	slog.Info(fmt.Sprintf("Signal send_multiple_images: %d/%d images valid, sending in chunks", len(attachments), len(images)))
	baseParams, err := a.withTarget(ctx, pyjson.ObjectOf("account", a.account, "message", ""), chatID, true)
	if err != nil {
		return gateway.SendResult{}, err
	}
	var batches [][]any
	for i := 0; i < len(attachments); i += SignalMaxAttachmentsPerMsg {
		batches = append(batches, attachments[i:min(i+SignalMaxAttachmentsPerMsg, len(attachments))])
	}
	delivered := false
	for idx, batch := range batches {
		n := len(batch)
		estimated := scheduler.EstimateWait(n)
		slog.Debug(fmt.Sprintf("Signal batch %d/%d: %d attachments, estimated wait=%.1fs", idx+1, len(batches), n, estimated))
		if estimated >= SignalBatchPacingNoticeThreshold {
			if err := a.notifyBatchPacing(ctx, chatID, idx+1, len(batches), estimated); err != nil {
				return gateway.SendResult{}, err
			}
		}
		params := baseParams.Copy()
		params.Set("attachments", batch)
		accepted, err := a.sendAttachmentBatch(ctx, scheduler, params, n, fmt.Sprintf("%d/%d", idx+1, len(batches)))
		if err != nil {
			return gateway.SendResult{}, err
		}
		if accepted {
			delivered = true
		}
	}
	if delivered {
		return gateway.SendResult{Success: true}, nil
	}
	return gateway.SendResult{Success: false, Error: "all Signal attachment batches failed"}, nil
}

// Sends one attachment batch under the scheduler with rate limit feedback and one transient retry
func (a *SignalAdapter) sendAttachmentBatch(ctx context.Context, scheduler *SignalAttachmentScheduler, params *pyjson.Object, n int, label string) (bool, error) {
	sendTimeout, maxAttempts := signalSendTimeout(n), SignalRateLimitMaxAttempts
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if _, err := scheduler.Acquire(ctx, n); err != nil {
			return false, err
		}
		t0 := monotonic()
		result, err := a.rpc(ctx, "send", params, rpcOptions{raiseOnRateLimit: true, timeout: sendTimeout})
		var rateLimit *SignalRateLimitError
		if errors.As(err, &rateLimit) {
			scheduler.Feedback(rateLimit.RetryAfter, n)
			retryAfter := "unknown"
			if rateLimit.RetryAfter != nil && *rateLimit.RetryAfter != 0 {
				retryAfter = fmt.Sprintf("%.0fs", *rateLimit.RetryAfter)
			}
			if attempt >= maxAttempts {
				slog.Error(fmt.Sprintf("Signal: rate-limit retries exhausted on batch %s (%d attachments lost, server retry_after=%s)", label, n, retryAfter))
				return false, nil
			}
			slog.Warn(fmt.Sprintf("Signal: rate-limited on batch %s (attempt %d/%d, server retry_after=%s); scheduler will pace the retry",
				label, attempt, maxAttempts, retryAfter))
			continue
		}
		if err != nil {
			return false, err
		}
		duration := monotonic() - t0
		success, errMsg := false, "None"
		if result != nil {
			var failure string
			if success, failure = validateSendResult(result); !success {
				errMsg = failure
			}
		}
		if success {
			a.trackSentTimestamp(result)
			scheduler.ReportRPCDuration(duration, n)
			slog.Info(fmt.Sprintf("Signal batch %s: %d attachments sent in %.1fs (attempt %d/%d)", label, n, duration, attempt, maxAttempts))
			return true, nil
		}
		suffix := ""
		if result != nil {
			suffix = ": " + errMsg
		}
		slog.Error(fmt.Sprintf("Signal: RPC send failed for batch %s (%d attachments, attempt %d/%d, rpc_duration=%.1fs)%s",
			label, n, attempt, maxAttempts, duration, suffix))
		if attempt >= maxAttempts {
			return false, nil
		}
		backoff := math.Pow(2.0, float64(attempt))
		slog.Info(fmt.Sprintf("Signal: retrying batch %s after %.1fs backoff", label, backoff))
		if err := signalSleep(ctx, backoff); err != nil {
			return false, err
		}
	}
	return false, nil
}

// Tells the user about an inter batch pacing wait over the notice threshold, a failed notice only logged
func (a *SignalAdapter) notifyBatchPacing(ctx context.Context, chatID string, nextBatchIdx, totalBatches int, waitS float64) error {
	text := i18n.T(ctx, "platform.signal.batch_pacing_notice", "", map[string]any{"wait": formatWait(ctx, waitS), "batch": nextBatchIdx, "total": totalBatches})
	if _, err := a.Self().EmitWarning(ctx, chatID, text, "", nil, ""); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn(fmt.Sprintf("Signal: failed to send pacing notice: %s", errText(err)))
	}
	return nil
}

// Sends an image from an http, https or file URL as a Signal attachment
func (a *SignalAdapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	if err := a.stopTypingIndicator(ctx, chatID); err != nil {
		return gateway.SendResult{}, err
	}
	filePath, reason, detail, err := a.resolveImagePath(ctx, imageURL)
	if err != nil {
		return gateway.SendResult{}, err
	}
	switch reason {
	case "download":
		text := errText(detail.(error))
		slog.Warn(fmt.Sprintf("Signal: failed to download image: %s", text))
		return gateway.SendResult{Success: false, Error: text}, nil
	case "missing":
		return gateway.SendResult{Success: false, Error: "Image file not found"}, nil
	case "oversize":
		return gateway.SendResult{Success: false, Error: fmt.Sprintf("Image too large (%d bytes)", detail.(int64))}, nil
	}
	return a.sendFile(ctx, chatID, filePath, caption, "RPC send with attachment failed")
}

// Sends one local file as a Signal attachment through the send RPC
func (a *SignalAdapter) sendFile(ctx context.Context, chatID, filePath, caption, failError string) (gateway.SendResult, error) {
	params, err := a.withTarget(ctx, pyjson.ObjectOf("account", a.account, "message", caption, "attachments", []any{filePath}), chatID, true)
	if err != nil {
		return gateway.SendResult{}, err
	}
	_, failed, err := a.rpcSend(ctx, params, failError)
	if err != nil {
		return gateway.SendResult{}, err
	}
	if failed != nil {
		return *failed, nil
	}
	return gateway.SendResult{Success: true}, nil
}

// Sends any local file as a Signal attachment after the existence and size checks
func (a *SignalAdapter) sendAttachment(ctx context.Context, chatID, filePath, mediaLabel, caption string) (gateway.SendResult, error) {
	if err := a.stopTypingIndicator(ctx, chatID); err != nil {
		return gateway.SendResult{}, err
	}
	info, err := os.Stat(filePath)
	if errors.Is(err, fs.ErrNotExist) {
		return gateway.SendResult{Success: false, Error: fmt.Sprintf("%s file not found: %s", mediaLabel, filePath)}, nil
	}
	if err != nil {
		return gateway.SendResult{}, err
	}
	if size := info.Size(); size > signalMaxAttachmentSize {
		return gateway.SendResult{Success: false, Error: fmt.Sprintf("%s too large (%d bytes)", mediaLabel, size)}, nil
	}
	return a.sendFile(ctx, chatID, filePath, caption, fmt.Sprintf("RPC send %s failed", pystr.Lower(mediaLabel)))
}

// Sends a document as a Signal attachment
func (a *SignalAdapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendAttachment(ctx, chatID, filePath, "File", caption)
}

// Sends a local image as a native Signal attachment for the gateway media delivery path
func (a *SignalAdapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendAttachment(ctx, chatID, imagePath, "Image", caption)
}

// Sends audio as an attachment, Signal having no distinct voice message API
func (a *SignalAdapter) SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendAttachment(ctx, chatID, audioPath, "Audio", caption)
}

// Sends a video as a Signal attachment
func (a *SignalAdapter) SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendAttachment(ctx, chatID, videoPath, "Video", caption)
}

// Stops the typing loop of a chat, sends the explicit stop RPC and clears the typing backoff
func (a *SignalAdapter) stopTypingIndicator(ctx context.Context, chatID string) error {
	a.mu.Lock()
	task := a.typingTasks[chatID]
	delete(a.typingTasks, chatID)
	a.mu.Unlock()
	gateway.CancelTask(ctx, task)
	if params, err := a.withTarget(ctx, pyjson.ObjectOf("account", a.account), chatID, true); err == nil {
		params.Set("stop", true)
		a.rpc(ctx, "sendTyping", params, rpcOptions{rpcID: "typing-stop", quietFailures: true})
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	a.mu.Lock()
	delete(a.typingFailures, chatID)
	delete(a.typingSkipUntil, chatID)
	a.mu.Unlock()
	return nil
}

// Stop typing hook the base adapter calls when its typing loop ends
func (a *SignalAdapter) StopTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	return a.stopTypingIndicator(ctx, chatID)
}

// Routes a sendReaction RPC to the chat without the service id upgrade
func (a *SignalAdapter) sendReactionRPC(ctx context.Context, chatID string, params *pyjson.Object) (bool, error) {
	if _, err := a.withTarget(ctx, params, chatID, false); err != nil {
		return false, err
	}
	result, err := a.rpc(ctx, "sendReaction", params, rpcOptions{})
	return result != nil, err
}

// Unreact call of the send_message tool, refused with the keyword TypeError the author and timestamp signature raises
func (a *SignalAdapter) RemoveReaction(ctx context.Context, chatID, messageID string) (any, error) {
	return nil, &pyErr{"TypeError", "SignalAdapter.remove_reaction() got an unexpected keyword argument 'message_id'"}
}

// Reacts to the message identified by its author and millisecond timestamp
func (a *SignalAdapter) sendReaction(ctx context.Context, chatID, emoji string, targetAuthor, targetTimestamp any) (bool, error) {
	ok, err := a.sendReactionRPC(ctx, chatID, pyjson.ObjectOf("account", a.account, "emoji", emoji, "targetAuthor", targetAuthor,
		"targetTimestamp", targetTimestamp))
	if err != nil {
		return false, err
	}
	if !ok {
		slog.Debug(fmt.Sprintf("Signal: sendReaction failed (chat=%s, emoji=%s)", pystr.Slice(chatID, 0, 20), emoji))
	}
	return ok, nil
}

// Removes a reaction by sending an empty emoji with the remove flag
func (a *SignalAdapter) removeReaction(ctx context.Context, chatID string, targetAuthor, targetTimestamp any) (bool, error) {
	return a.sendReactionRPC(ctx, chatID, pyjson.ObjectOf("account", a.account, "emoji", "", "targetAuthor", targetAuthor,
		"targetTimestamp", targetTimestamp, "remove", true))
}

// Author and timestamp of the message an event reacts to, false when the raw message carries neither
func extractReactionTarget(event *gateway.MessageEvent) (author, timestamp any, ok bool) {
	raw, isDict := event.RawMessage.(map[string]any)
	if !isDict || !pyrepr.Truthy(raw["sender"]) || !pyrepr.Truthy(raw["timestamp_ms"]) {
		return nil, nil, false
	}
	return raw["sender"], raw["timestamp_ms"], true
}

// Reactions gate, the env switch first and then the DM allowlist so strangers see no listening bot
func (a *SignalAdapter) reactionsEnabled(ctx context.Context, event *gateway.MessageEvent) (bool, error) {
	setting, err := gateway.GetScopedSecret(ctx, "SIGNAL_REACTIONS", "true", false)
	if err != nil {
		return false, err
	}
	switch pystr.Lower(setting) {
	case "false", "0", "no":
		return false, nil
	}
	sender := ""
	if event != nil && event.Source != nil && event.Source.UserID != nil {
		sender = *event.Source.UserID
	}
	return !(sender != "" && !a.dmAllowFrom["*"] && !a.dmAllowFrom[sender]), nil
}

// Reacts with eyes when processing of an event begins
func (a *SignalAdapter) OnProcessingStart(ctx context.Context, event *gateway.MessageEvent) error {
	enabled, err := a.reactionsEnabled(ctx, event)
	if err != nil || !enabled {
		return err
	}
	author, timestamp, ok := extractReactionTarget(event)
	if !ok {
		return nil
	}
	_, err = a.sendReaction(ctx, event.Source.ChatID, "👀", author, timestamp)
	return err
}

// Swaps the eyes reaction for the outcome mark, a cancelled turn keeping the eyes
func (a *SignalAdapter) OnProcessingComplete(ctx context.Context, event *gateway.MessageEvent, outcome gateway.ProcessingOutcome) error {
	if outcome == gateway.ProcessingCancelled {
		return nil
	}
	enabled, err := a.reactionsEnabled(ctx, event)
	if err != nil || !enabled {
		return err
	}
	author, timestamp, ok := extractReactionTarget(event)
	if !ok {
		return nil
	}
	if _, err := a.removeReaction(ctx, event.Source.ChatID, author, timestamp); err != nil {
		return err
	}
	if emoji := outcomeReaction[outcome]; emoji != "" {
		_, err = a.sendReaction(ctx, event.Source.ChatID, emoji, author, timestamp)
	}
	return err
}

// Name, type and id of a chat, the contact name looked up for direct chats
func (a *SignalAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	if strings.HasPrefix(chatID, "group:") {
		return map[string]any{"name": chatID, "type": "group", "chat_id": chatID}, nil
	}
	result, err := a.rpc(ctx, "getContact", pyjson.ObjectOf("account", a.account, "contactAddress", chatID), rpcOptions{})
	if err != nil {
		return nil, err
	}
	var name any
	if rm, isDict := pyjson.Dict(result); isDict {
		name = pyrepr.Or(rm["name"], rm["profileName"])
	}
	if !pyrepr.Truthy(name) {
		name = chatID
	}
	return map[string]any{"name": name, "type": "dm", "chat_id": chatID}, nil
}
