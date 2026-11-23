package tools

import (
	"context"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Hands the logging setup the redacting formatter, detached stdio redaction and the profile policy resolver
func init() {
	logging.RedactingHandler = func(next slog.Handler) slog.Handler { return NewRedactingHandler(next) }
	logging.RedactSensitiveText = RedactSensitiveText
	logging.ResolveProfilePolicy = ResolveProfilePolicy
}

// Records a profile's hide secrets setting now so its routed records never wait
func ResolveProfilePolicy(profileHome string) {
	on := settings.RedactSecrets.Get(home.WithOverride(context.Background(), profileHome))
	redactPolicy.Lock()
	redactPolicy.cfg[home.Key(profileHome)] = on
	redactPolicy.Unlock()
}
