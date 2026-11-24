package gateway

import (
	"context"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Model pick of a prepared snapshot frozen into its policy, the committed policy and refusal as the mutation returns them
func PrepareModel(ctx context.Context, a *SessionAuthority, live *LiveSession, payload, prepared *jsonutil.Object) (*jsonutil.Object, error) {
	return prepareModel(ctx, a, live, payload, prepared)
}

// Stored field dict of the policy as a receipt holds it
func (p *LocalSessionPolicy) AsDict() map[string]any { return p.asDict() }
