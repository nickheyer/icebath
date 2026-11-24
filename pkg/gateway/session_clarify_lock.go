package gateway

import (
	"context"
	"errors"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Locks one answer of a viewer's clarify batch, expired once the wait already ended
func clarifyLock(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *jsonutil.Object) (any, error) {
	requestID, okRequest := params.ValueOr("request_id", nil).(string)
	questionID, okQuestion := params.ValueOr("question_id", nil).(string)
	if !okRequest || requestID == "" || !okQuestion || questionID == "" {
		return nil, runtimeRefusal("invalid_params")
	}
	answer := params.ValueOr("answer", "")
	if _, isStr := answer.(string); answer != nil && !isStr {
		text, err := jsonutil.Encode(answer, jsonutil.Spaced())
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
		return jsonutil.ObjectOf("status", "expired"), nil
	}
	out := make([]any, len(remaining))
	for i, qid := range remaining {
		out[i] = qid
	}
	return jsonutil.ObjectOf("status", "ok", "remaining", out), nil
}
