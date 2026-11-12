package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Fills the current route from the config file, the defaults kept when it cannot be read
func (c *ModelSwitchContext) ReadConfig(ctx context.Context) { c.readConfig(ctx) }

// Session model override superseding the configured route
func (c *ModelSwitchContext) ApplyOverride(override map[string]any) { c.applyOverride(override) }

// Authority and live session of the local session a source belongs to, nils for a messaging chat
func LocalAuthorityLive(runner any, source *SessionSource) (*SessionAuthority, *LiveSession) {
	return localAuthorityLive(runner, source)
}

// Defers a typed model pick as the local session's canonical model mutation for the next boundary
func DeferLocalModelPick(ctx context.Context, a *SessionAuthority, live *LiveSession, result provider.ModelSwitchResult, effort string) (*pyjson.Object, error) {
	return deferLocalModelPick(ctx, a, live, result, effort)
}

// Adapter that renders a flat one tap choice picker, the send_choice_picker of telegram, discord and matrix
type ChoicePickerSender interface {
	SendChoicePicker(ctx context.Context, chatID, title string, choices []map[string]any, sessionKey string,
		onChoiceSelected func(ctx context.Context, chatID, value string) (string, error), metadata map[string]any) (SendResult, error)
}
