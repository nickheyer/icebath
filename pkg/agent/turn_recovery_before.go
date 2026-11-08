package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Python exception classes an isinstance check against RuntimeError accepts
var runtimeErrorClasses = []string{"RuntimeError", "NotImplementedError", "RecursionError", "PythonFinalizationError"}

// True when err or any error it wraps stands for one of the named Python exception classes
func errIsPyClass(err error, names ...string) bool {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if n, ok := e.(pyTypeNamed); ok && slices.Contains(names, n.PyTypeName()) {
			return true
		}
	}
	return false
}

// Body, message or text of an API error as Python str renders it
func apiErrorBodyText(err error) string {
	var apiErr *provider.APIError
	if errors.As(err, &apiErr) {
		if apiErr.Body != "" {
			if parsed, perr := pyjson.Loads(apiErr.Body); perr == nil && pyTruthy(parsed) {
				return pyrepr.Str(parsed)
			}
			return apiErr.Body
		}
		if apiErr.Message != "" {
			return apiErr.Message
		}
	}
	return err.Error()
}

// HTTP status an error carries, nil when it has none
func apiErrorStatus(err error) *int {
	if status, ok := surfaceExceptionStatus(err); ok && status != 0 {
		return &status
	}
	return nil
}

// Strips lone surrogates from canonical rows in place, clearing the persisted mark of every repaired row
func sanitizeMessageRowsSurrogates(messages []*state.Message) bool {
	found := false
	for _, m := range messages {
		if m == nil {
			continue
		}
		d := state.MessageToDict(m)
		if state.SanitizeMessagesSurrogates([]any{d}) {
			*m = *state.MessageFromDict(d)
			found = true
		}
	}
	return found
}

// Strips non ASCII from the default headers and the API key, true when either was repaired
func repairTransportCredentials(a *AIAgent) bool {
	repaired := false
	if headers, ok := a.clientKwargs["default_headers"].(map[string]any); ok {
		repaired = state.SanitizeStructureNonASCII(headers)
	}
	if a.apiKey != "" {
		clean := state.StripNonASCII(a.apiKey)
		if clean != a.apiKey {
			a.apiKey = clean
			if a.clientKwargs != nil {
				a.clientKwargs["api_key"] = clean
			}
			if c, ok := a.client.(interface{ SetAPIKey(string) }); ok && a.client != nil {
				c.SetAPIKey(clean)
			}
			repaired = true
			vlines(a, "⚠️  API key contained non-ASCII characters (bad copy-paste?) — stripped them. "+
				"If auth fails, re-copy the key from your provider's dashboard.")
		}
	}
	return repaired
}

// Surrogate strip first, then the ASCII only request fallback under a non UTF-8 locale, true when a retry can help
func recoverUnicodeEncodeError(a *AIAgent, apiError error, messages []*state.Message, apiMessages []any, apiKwargs map[string]any, activeSystemPrompt *string) bool {
	errStr := pystr.Lower(apiError.Error())
	isASCIICodec := strings.Contains(errStr, "'ascii'") || strings.Contains(errStr, "ascii")
	isSurrogateError := strings.Contains(errStr, "surrogate") || (strings.Contains(errStr, "'utf-8'") && !isASCIICodec)
	surrogatesFound := sanitizeMessageRowsSurrogates(messages)
	if apiMessages != nil {
		surrogatesFound = state.SanitizeMessagesSurrogates(apiMessages) || surrogatesFound
	}
	if apiKwargs != nil {
		surrogatesFound = state.SanitizeStructureSurrogates(apiKwargs) || surrogatesFound
	}
	if surrogatesFound || isSurrogateError {
		if surrogatesFound {
			a.dbFlushScanPrefix = nil
		}
		a.unicodeSanitizationPasses++
		if surrogatesFound {
			a.bufferVprint("⚠️  Stripped invalid surrogate characters from messages. Retrying...")
		} else {
			a.bufferVprint("⚠️  Surrogate encoding error — retrying after full-payload sanitization...")
		}
		return true
	}
	if !isASCIICodec {
		return false
	}
	if !runtimeUsesASCIIEncoding() {
		if !repairTransportCredentials(a) {
			return false
		}
		a.unicodeSanitizationPasses++
		vlines(a, "⚠️  Repaired non-ASCII request credentials/headers without changing conversation content. Retrying...")
		return true
	}

	a.forceASCIIPayload = true
	messagesSanitized := apiMessages != nil && state.SanitizeMessagesNonASCII(apiMessages)
	systemSanitized := false
	if activeSystemPrompt != nil {
		if sanitized := state.StripNonASCII(*activeSystemPrompt); sanitized != *activeSystemPrompt {
			*activeSystemPrompt = sanitized
			systemSanitized = true
		}
	}
	transportRepaired := repairTransportCredentials(a)
	a.unicodeSanitizationPasses++
	if messagesSanitized || systemSanitized || transportRepaired {
		vlines(a, "⚠️  System encoding is ASCII — stripped non-ASCII characters from request payload. Retrying...")
	} else {
		vlines(a, "⚠️  System encoding is ASCII — enabling full-payload sanitization for retry...")
	}
	return true
}

// Unicode, fast mode, image rejection, reasoning key rejection and Bedrock stream recoveries run before classification
func recoverBeforeClassification(ctx context.Context, a *AIAgent, s *LoopState, apiError error) (bool, error) {
	if errIsPyClass(apiError, "UnicodeEncodeError") && a.unicodeSanitizationPasses < 2 {
		if recoverUnicodeEncodeError(a, apiError, s.Messages, s.APIMessages, s.APIKwargs, &s.ActiveSystemPrompt) {
			return true, nil
		}
	}

	status := apiErrorStatus(apiError)
	if status != nil && *status == 429 && s.APIKwargs != nil {
		extraBody, _ := s.APIKwargs["extra_body"].(map[string]any)
		if provider.FastModeUnprovisioned(&provider.APIError{StatusCode: 429}, extraBody, apiErrorHeaders(apiError)) && a.markFastModeUnavailable() {
			vlines(a, fmt.Sprintf("⚠️  Fast mode isn't available for %s on this Anthropic organization — using standard speed for this session, retrying...", a.model))
			conversationLoopLog().Warn(fmt.Sprintf("%sFast mode: %s has a fast-mode limit of 0; standard speed for this session", a.logPrefix, a.model))
			return true, nil
		}
	}

	errBody := apiErrorBodyText(apiError)
	statusOK := status == nil || (*status >= 400 && *status < 500)
	modelKey := providerModelKey(a)
	if a.imageRejectingModels == nil {
		a.imageRejectingModels = map[[2]string]bool{}
	}
	corrupt := looksLikeCorruptImageRejection(errBody)
	if statusOK && (corrupt || (!a.imageRejectingModels[modelKey] && looksLikeImageContentRejection(errBody))) {
		if corrupt {
			if stripRequestImagesAndRetry(a, &s.APIMessages) {
				return true, nil
			}
		} else {
			a.imageRejectingModels[modelKey] = true
			vlines(a, "⚠️  Server rejected image content — sending text only to this model; "+
				"images stay in the session history.")
			return true, nil
		}
	}

	var sent any
	if s.APIKwargs != nil {
		sent = s.APIKwargs["messages"]
	}
	if !pyTruthy(sent) {
		sent = s.APIMessages
	}
	if statusOK {
		added, err := a.recordReasoningFieldRejection(errBody, sent)
		if err != nil {
			return false, err
		}
		if len(added) > 0 {
			keys := make([]string, 0, len(added))
			for k := range added {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			vlines(a, fmt.Sprintf("⚠️  %s rejected replayed %s — retrying without it for this model.", a.model, strings.Join(keys, ", ")))
			return true, nil
		}
	}

	if errIsPyClass(apiError, runtimeErrorClasses...) && strings.Contains(pystr.Lower(apiError.Error()), "unexpected event order") &&
		a.provider == "bedrock" && a.apiMode == "anthropic_messages" && !a.bedrockConverseFallbackAttempted {
		a.bedrockConverseFallbackAttempted = true
		a.apiMode = "bedrock_converse"
		if a.bedrockRegion == "" {
			a.bedrockRegion = "us-east-1"
		}
		a.client = nil
		a.clientKwargs = map[string]any{}
		vlines(a, "⚠️  AnthropicBedrock SDK streaming failed — falling back to native Converse API for this session.")
		return true, nil
	}
	return false, nil
}
