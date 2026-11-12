package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Model pick of a prepared snapshot frozen into its policy, the committed policy and refusal as the mutation returns them
func PrepareModel(ctx context.Context, a *SessionAuthority, live *LiveSession, payload, prepared *pyjson.Object) (*pyjson.Object, error) {
	return prepareModel(ctx, a, live, payload, prepared)
}

// Stored field dict of the policy as a receipt holds it
func (p *LocalSessionPolicy) AsDict() map[string]any { return p.asDict() }
