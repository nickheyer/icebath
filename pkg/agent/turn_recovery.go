package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Sleep and wall clock seams the interruptible backoff slices its wait over
var (
	recoverySleep = time.Sleep
	recoveryNow   = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
)

// Force prints each line behind the log prefix as a diagnostic
func vlines(a *AIAgent, lines ...string) {
	for _, line := range lines {
		a.vprint(a.logPrefix+line, true, true)
	}
}

// Buffers each line so it surfaces only when every retry and fallback exhausts
func blines(a *AIAgent, lines ...string) {
	for _, line := range lines {
		a.bufferVprint(line)
	}
}

// Appends the stopped closing assistant row when the live transcript ends on a tool result
func closeStoppedToolSequence(messages *[]*state.Message, interruptText string) {
	rows := *messages
	if len(rows) == 0 || rows[len(rows)-1] == nil {
		return
	}
	tail := []map[string]any{{"role": rows[len(rows)-1].Role}}
	if closeInterruptedToolSequence(&tail, interruptText, true) {
		*messages = append(*messages, state.MessageFromDict(tail[1]))
	}
}

// Announces the abort, closes an open tool sequence, persists, hard clears the interrupt and returns the interrupted result
func abortTurnOnInterrupt(a *AIAgent, messages *[]*state.Message, conversationHistory []*state.Message, apiCallCount int, abortMessage, interruptText string) (map[string]any, error) {
	vlines(a, "⚡ "+abortMessage)
	a.dropTrailingEmptyResponseScaffolding(messages)
	closeStoppedToolSequence(messages, interruptText)
	if err := a.persistSession(messages, conversationHistory); err != nil {
		return nil, err
	}
	a.clearInterruptWith(false, true)
	return map[string]any{
		"final_response": interruptText, "messages": *messages, "api_calls": apiCallCount,
		"completed": false, "interrupted": true,
	}, nil
}

// Waits in 200 ms slices honouring interrupts, a pending redirect arms the restart, nil result when the wait completes
func interruptibleBackoffSleep(ctx context.Context, a *AIAgent, waitTime float64, retry *TurnRetryState, messages *[]*state.Message,
	conversationHistory []*state.Message, apiCallCount int, abortMessage, interruptText, activityLabel string) (map[string]any, error) {
	sleepEnd := recoveryNow() + waitTime
	touchCounter := 0
	for recoveryNow() < sleepEnd {
		if a.Interrupted() {
			if retry != nil && a.clearInterrupt(true) {
				retry.RestartWithRedirectedMessages = true
				return nil, nil
			}
			return abortTurnOnInterrupt(a, messages, conversationHistory, apiCallCount, abortMessage, interruptText)
		}
		recoverySleep(200 * time.Millisecond)
		touchCounter++
		if touchCounter%150 == 0 {
			a.touchActivity(fmt.Sprintf("%s, %ds remaining", activityLabel, int(sleepEnd-recoveryNow())), "", false)
		}
	}
	a.adoptNousKeyBeforeExpiry(ctx)
	return nil, nil
}

// The code and message of a failed or cancelled Codex response error, empty when absent
func codexSoftFailureError(response any) map[string]any {
	errorObj := state.ToolCallField(response, "error")
	if !pyrepr.Truthy(errorObj) {
		return map[string]any{}
	}
	var fields map[string]any
	if m, ok := errorObj.(map[string]any); ok {
		fields = m
	} else if isPyDict(errorObj) {
		raw, _ := json.Marshal(errorObj)
		_ = json.Unmarshal(raw, &fields)
	} else if code, message := state.ToolCallField(errorObj, "code"), state.ToolCallField(errorObj, "message"); code != nil || message != nil {
		fields = map[string]any{"code": code, "message": message}
	} else {
		fields = map[string]any{"message": pyrepr.Str(errorObj)}
	}
	out := map[string]any{}
	for k, v := range fields {
		if s, ok := v.(string); ok && pystr.Strip(s) != "" {
			out[k] = s
		}
	}
	return out
}

// Lowered stripped status string of a response, empty when it carries none
func responseStatus(response any) string {
	status := state.ToolCallField(response, "status")
	if !pyrepr.Truthy(status) {
		return ""
	}
	return strings.ToLower(pystr.Strip(pyrepr.Str(status)))
}

// Classified verdict and error context of a failed or cancelled Codex response, nil when it is not one
func classifyCodexSoftFailure(a *AIAgent, response any) (*provider.Classified, map[string]any) {
	if a.apiMode != "codex_responses" {
		return nil, map[string]any{}
	}
	if status := responseStatus(response); status != "failed" && status != "cancelled" {
		return nil, map[string]any{}
	}
	errFields := codexSoftFailureError(response)
	body, _ := json.Marshal(map[string]any{"error": errFields})
	message, _ := errFields["message"].(string)
	exc := &provider.APIError{Message: message, Body: string(body)}
	classified := provider.ClassifyError(provider.ClassifyInput{
		Err: exc, Body: string(body), Provider: a.provider, Model: a.model, BaseURL: a.baseURL, APIKey: a.apiKey,
	})
	return classified, provider.ExtractAPIErrorContext(exc)
}

// Whether the response has the named attribute, a dict row counting its keys
func responseHasField(response any, name string) bool {
	if m, ok := response.(map[string]any); ok {
		_, has := m[name]
		return has
	}
	return state.ToolCallField(response, name) != nil
}

// Whether the raw response is invalid for the transport with the error details, Codex output_text deferring to normalization
func validateResponseShape(a *AIAgent, response any) (bool, []string, error) {
	t := a.getTransport("")
	if t == nil {
		return false, nil, fmt.Errorf("no transport registered for api_mode %q", a.apiMode)
	}
	if t.ValidateResponse(response) {
		return false, []string{}, nil
	}
	if response == nil {
		return true, []string{"response is None"}, nil
	}
	log := conversationLoopLog()
	if a.apiMode == "codex_responses" {
		status := responseStatus(response)
		if status == "failed" || status == "cancelled" {
			msg, _ := codexSoftFailureError(response)["message"].(string)
			if msg == "" {
				msg = fmt.Sprintf("Responses API returned status '%s'", status)
			}
			log.Warn(fmt.Sprintf("Codex response status='%s' (error=%s). Routing to fallback. %s", status, msg, a.ClientLogContext()))
			return true, []string{fmt.Sprintf("response.status=%s: %s", status, msg)}, nil
		}
		outText, _ := state.ToolCallField(response, "output_text").(string)
		if stripped := pystr.Strip(outText); stripped != "" {
			log.Debug(fmt.Sprintf("Codex response.output is empty but output_text is present (%d chars); deferring to normalization.", len([]rune(stripped))))
			return false, []string{}, nil
		}
		log.Warn(fmt.Sprintf("Codex response.output is empty after stream backfill (status=%s, incomplete_details=%s, model=%s). %s",
			pyrepr.Str(state.ToolCallField(response, "status")), pyrepr.Str(state.ToolCallField(response, "incomplete_details")),
			pyrepr.Str(state.ToolCallField(response, "model")), fmt.Sprintf("api_mode=%s provider=%s", a.apiMode, a.provider)))
		return true, []string{"response.output is empty"}, nil
	}
	var detail string
	switch {
	case a.apiMode == "anthropic_messages":
		detail = "response.content invalid (not a non-empty list)"
	case a.apiMode == "bedrock_converse":
		detail = "Bedrock response invalid (no output or choices)"
	case !responseHasField(response, "choices"):
		detail = "response has no 'choices' attribute"
	case state.ToolCallField(response, "choices") == nil:
		detail = "response.choices is None"
	default:
		detail = "response.choices is empty"
	}
	return true, []string{detail}, nil
}

// Error message, provider name and failure hint for an empty or malformed response
func describeInvalidResponse(a *AIAgent, response any, apiDuration float64) (string, string, string) {
	errorMsg, providerName := "Unknown", "Unknown"
	responseTruthy := pyrepr.Truthy(response)
	errValue := state.ToolCallField(response, "error")
	hasError := responseTruthy && pyrepr.Truthy(errValue)
	if hasError {
		errorMsg, _ = codexSoftFailureError(response)["message"].(string)
		if errorMsg == "" {
			errorMsg = pyrepr.Str(errValue)
		}
		if metadata := state.ToolCallField(errValue, "metadata"); !isPyDict(errValue) && pyrepr.Truthy(metadata) {
			providerName = "Unknown"
			if name := state.ToolCallField(metadata, "provider_name"); name != nil {
				providerName = pyrepr.Str(name)
			}
		}
	} else if message := state.ToolCallField(response, "message"); responseTruthy && pyrepr.Truthy(message) {
		errorMsg = pyrepr.Str(message)
	}
	if model := state.ToolCallField(response, "model"); providerName == "Unknown" && responseTruthy && pyrepr.Truthy(model) {
		providerName = "model=" + pyrepr.Str(model)
	}
	if providerName == "Unknown" && responseTruthy && a.verboseLogging {
		slog.Debug(fmt.Sprintf("Response attributes for invalid response: %s", pyrepr.Str(responseAttrs(response))))
	}
	var code *int64
	if hasError {
		raw := state.ToolCallField(errValue, "code")
		if raw != nil {
			if n, err := pyint.Of(raw); err == nil && n.IsInt64() {
				v := n.Int64()
				code = &v
			}
		}
	}
	return errorMsg, providerName, failureHintFor(code, apiDuration)
}

// Public attributes of a response as strings cut at 100 characters, the vars() view of the debug log
func responseAttrs(response any) map[string]any {
	out := map[string]any{}
	if m, ok := response.(map[string]any); ok {
		for k, v := range m {
			if !strings.HasPrefix(k, "_") {
				out[k] = pystr.Slice(pyrepr.Str(v), 0, 100)
			}
		}
	}
	return out
}

// Human readable hint from the provider error code and the response time
func failureHintFor(code *int64, apiDuration float64) string {
	if code != nil {
		switch c := *code; c {
		case 524:
			return fmt.Sprintf("upstream provider timed out (Cloudflare 524, %.0fs)", apiDuration)
		case 504:
			return fmt.Sprintf("upstream gateway timeout (504, %.0fs)", apiDuration)
		case 429:
			return "rate limited by upstream provider (429)"
		case 500, 502:
			return fmt.Sprintf("upstream server error (%d, %.0fs)", c, apiDuration)
		case 503, 529:
			return fmt.Sprintf("upstream provider overloaded (%d)", c)
		default:
			return fmt.Sprintf("upstream error (code %d, %.0fs)", c, apiDuration)
		}
	}
	if apiDuration < 10 {
		return fmt.Sprintf("fast response (%.1fs) — likely rate limited", apiDuration)
	}
	if apiDuration > 60 {
		return fmt.Sprintf("slow response (%.0fs) — likely upstream timeout", apiDuration)
	}
	return fmt.Sprintf("response time %.1fs", apiDuration)
}

var (
	recoveryPrint       = func(line string) { fmt.Println(line) }
	recoveryLocaleEnv   = os.Getenv
	imagePatchLimitRe   = regexp.MustCompile(`exceeding the limit of\s*(\p{Nd}{2,7})`)
	imageMaxAllowedRe   = regexp.MustCompile(`max allowed size(?:\s+for [^:]+)?:\s*(\p{Nd}{3,5})\s*pixels?`)
	asciiLocaleCodesets = map[string]bool{"ascii": true, "us-ascii": true, "ansi-x3.4-1968": true}
)

// Reports a locale codeset that really needs the ASCII only request fallback, the C locale coerced to UTF-8
func runtimeUsesASCIIEncoding() bool {
	name := ""
	for _, key := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := recoveryLocaleEnv(key); v != "" {
			name = v
			break
		}
	}
	codeset := ""
	if i := strings.IndexByte(name, '.'); i >= 0 {
		codeset = name[i+1:]
		if j := strings.IndexByte(codeset, '@'); j >= 0 {
			codeset = codeset[:j]
		}
	}
	encoding := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(codeset)), "_", "-")
	return asciiLocaleCodesets[encoding]
}

// Prints each line behind the log prefix through the notification platform's presentation preference
func plines(a *AIAgent, lines ...string) {
	platform := a.platform
	if a.notificationPlatform != nil {
		platform = *a.notificationPlatform
	}
	_, _ = config.RenderNotification(a.homeContext(), func() error {
		for _, line := range lines {
			recoveryPrint(a.logPrefix + line)
		}
		return nil
	}, platform, false, a.notificationConfig)
}

// Provider reported image side ceiling in pixels, a patch budget read as its square side, nil outside 512 to 8000
func imageErrorMaxDimension(err error) *int {
	if err == nil {
		return nil
	}
	parts := []string{}
	if s := err.Error(); s != "" {
		parts = append(parts, s)
	}
	var apiErr *provider.APIError
	if errors.As(err, &apiErr) {
		if apiErr.Message != "" {
			parts = append(parts, apiErr.Message)
		}
		if apiErr.Body != "" {
			parts = append(parts, apiErr.Body)
		}
	}
	text := strings.ToLower(strings.Join(parts, " "))
	inRange := func(n int64) *int {
		if n < 512 || n > 8000 {
			return nil
		}
		v := int(n)
		return &v
	}
	if strings.Contains(text, "patches after processing") {
		m := imagePatchLimitRe.FindStringSubmatch(text)
		if m == nil {
			return nil
		}
		limit, ok := pyint.Parse(m[1])
		if !ok {
			return nil
		}
		return inRange(new(big.Int).Sqrt(limit).Int64() * 32)
	}
	if !strings.Contains(text, "image") || !strings.Contains(text, "dimension") || !strings.Contains(text, "max allowed size") {
		return nil
	}
	m := imageMaxAllowedRe.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	n, ok := pyint.Parse(m[1])
	if !ok {
		return nil
	}
	return inRange(n.Int64())
}

// Strips image parts from this attempt's request copy, leaving the model unmarked, true when any were removed
func stripRequestImagesAndRetry(a *AIAgent, apiMessages *[]any) bool {
	if apiMessages != nil && stripImagesFromMessages(apiMessages) {
		vlines(a, "⚠️  Provider rejected a corrupted image — stripped images from the retry payload and retrying...")
		return true
	}
	return false
}
