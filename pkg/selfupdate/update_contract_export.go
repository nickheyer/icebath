package selfupdate

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Source update check of the scoped home with the cache bypassed when forced
func CheckForUpdatesForce(ctx context.Context, force bool) (*pyjson.Object, error) {
	return CheckForUpdates(ctx, SourceCheckOptions{Force: force})
}

// Code, message and command of the refusal of an in place update, an empty code when the install may update
func UpdateAdmissionRefusal(ctx context.Context, projectRoot string) (code, message, command string, err error) {
	refusal, err := evaluateUpdateAdmission(ctx, projectRoot)
	if refusal == nil || err != nil {
		return "", "", "", err
	}
	return refusal.Code, refusal.Message, refusal.UpdateCommand, nil
}

// Writes the refused receipt of a blocked dashboard update attempt, never failing the route
func RecordUpdateRefusal(ctx context.Context, code, message, command string) {
	recordRefusalReceipt(ctx, &UpdateRefusal{Code: code, Message: message, UpdateCommand: command})
}
