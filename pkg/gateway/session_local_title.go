package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/state"
)

// Sanitized title of a request, invalid_params for a non string, an empty or an overlong one
func validateTitle(title any) (string, error) {
	text, ok := title.(string)
	if !ok {
		return "", runtimeRefusal("invalid_params")
	}
	cleaned, err := state.SanitizeTitle(text)
	if err != nil {
		if titleValueError(err) {
			return "", runtimeRefusal("invalid_params")
		}
		return "", err
	}
	if cleaned == "" {
		return "", runtimeRefusal("invalid_params")
	}
	return cleaned, nil
}

// Sets a user provenance title on a just created local session, a concurrent holder of it being a conflict
func titleNewSession(ctx context.Context, a *SessionAuthority, ref SessionRef, title string) error {
	if _, err := a.DB.SetSessionTitle(ctx, ref.SessionID, title); err != nil {
		if titleValueError(err) {
			return runtimeRefusal("admission_conflict")
		}
		return err
	}
	return nil
}

// Whether a title store error is the ValueError Python raises for a bad or held title
func titleValueError(err error) bool {
	return isPyValueError(err) || pyErrorKind(err) == "ValueError"
}
