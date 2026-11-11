package selfupdate

import "context"

// Refusal of an in place update for the dashboard update route, nil when the install may update
func EvaluateUpdateAdmission(ctx context.Context, projectRoot string) (*UpdateRefusal, error) {
	return evaluateUpdateAdmission(ctx, projectRoot)
}

// Writes the refused receipt of a blocked dashboard update attempt, never failing the route
func RecordRefusalReceipt(ctx context.Context, refusal *UpdateRefusal) {
	recordRefusalReceipt(ctx, refusal)
}
