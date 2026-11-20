package gateway

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Locks one answer of a viewer's clarify batch, expired once the wait already ended
func clarifyLock(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	requestID, okRequest := params.GetOr("request_id", nil).(string)
	questionID, okQuestion := params.GetOr("question_id", nil).(string)
	if !okRequest || requestID == "" || !okQuestion || questionID == "" {
		return nil, runtimeRefusal("invalid_params")
	}
	answer := params.GetOr("answer", "")
	if _, isStr := answer.(string); answer != nil && !isStr {
		text, err := pyjson.Dumps(answer, pyjson.EnsureASCII(false))
		if err != nil {
			return nil, err
		}
		answer = text
	}
	var settleErr error
	entitled := func(sessionID string, events *SessionEvents) bool {
		ok, err := maySettle(ctx, c.authority, c.actor, sessionID, events)
		if err != nil {
			settleErr = err
		}
		return ok
	}
	remaining, err := LockAnswer(requestID, questionID, answer, entitled)
	if settleErr != nil {
		return nil, settleErr
	}
	var unknown *unknownQuestionError
	if errors.As(err, &unknown) {
		return nil, runtimeRefusal("invalid_params")
	}
	if err != nil {
		return nil, err
	}
	if remaining == nil {
		return pyjson.ObjectOf("status", "expired"), nil
	}
	out := make([]any, len(remaining))
	for i, qid := range remaining {
		out[i] = qid
	}
	return pyjson.ObjectOf("status", "ok", "remaining", out), nil
}
