package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Host abort that Python raises as a BaseException outside the Exception hierarchy
type auxBaseException interface{ auxBaseException() }

// Interrupt an auxiliary stream raises, an Exception that still reads as a cancellation
type auxInterruptedError interface{ auxInterrupted() }

// Error object of an HTTP 200 body that carried no choices, nil otherwise
func auxEmbeddedProviderError(response any) map[string]any {
	m, ok := response.(map[string]any)
	if !ok {
		return nil
	}
	errObj, ok := m["error"].(map[string]any)
	if !ok || len(errObj) == 0 {
		return nil
	}
	return errObj
}

// True when the host aborted the call through explicit cancel, interrupt, task cancel, or shutdown
func auxIsCancellation(err error) bool {
	if auxIsBaseException(err) {
		return true
	}
	var ie auxInterruptedError
	return errors.As(err, &ie)
}

// True for errors Python raises outside the Exception hierarchy
func auxIsBaseException(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, tools.ErrKeyboardInterrupt) {
		return true
	}
	var be auxBaseException
	return errors.As(err, &be)
}

// Classifier reason for one auxiliary attempt error, unknown when it cannot say, never failing
func auxErrorClass(err error, provider, model string) (reason string) {
	if err == nil || auxIsBaseException(err) {
		return "unknown"
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Auxiliary error classification failed", "err", fmt.Sprint(r))
			reason = "unknown"
		}
	}()
	in := auxClassifyInput(err, provider, model)
	var re *AuxRuntimeError
	if errors.As(err, &re) && re.ProviderError != nil {
		msg := ""
		if v := re.ProviderError["message"]; auxTruthy(v) {
			msg = fmt.Sprint(v)
		}
		body, _ := json.Marshal(map[string]any{"error": re.ProviderError})
		in = ClassifyInput{Err: errors.New(msg), Body: string(body), Provider: provider, Model: model}
	}
	return string(ClassifyError(in).Reason)
}

// Classifier input carrying the status, body, and headers an auxiliary error exposes
func auxClassifyInput(err error, provider, model string) ClassifyInput {
	in := ClassifyInput{Err: err, Status: auxExcHTTPStatus(err), Provider: provider, Model: model}
	switch b := auxErrBody(err).(type) {
	case string:
		in.Body = b
	case nil:
	default:
		raw, _ := json.Marshal(b)
		in.Body = string(raw)
	}
	var sdk *AuxSDKError
	if errors.As(err, &sdk) {
		in.Headers = sdk.Headers
	}
	return in
}
