package tools

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/logging"
)

// Hands the logging setup the redacting formatter, detached stdio redaction and the profile policy resolver
func init() {
	logging.RedactingHandler = func(next slog.Handler) slog.Handler { return NewRedactingHandler(next, liveSecretScope) }
	logging.RedactSensitiveText = RedactSensitiveText
	logging.ResolveProfilePolicy = ResolveProfilePolicy
}

// Secret scope live in a record's context, nil outside every scope
func liveSecretScope(ctx context.Context) map[string]string {
	return profileSecrets().CurrentSecretScope(ctx)
}

// Records a profile's redaction policy from its own .env and config now so its routed records never wait
func ResolveProfilePolicy(profileHome string) {
	key := home.Key(profileHome)
	var envRaw any
	if v, ok := config.LoadEnv(profileHome)["ICEBATH_REDACT_SECRETS"]; ok {
		envRaw = v
	}
	NoteEnvPolicy(key, envRaw)
	tree, err := config.LoadReadonly(home.WithOverride(context.Background(), profileHome), profileHome, nil)
	if err != nil {
		slog.Debug(fmt.Sprintf("redaction policy for %s not read: %s", profileHome, err))
		return
	}
	security := tree["security"]
	if !pyrepr.Truthy(security) {
		NoteConfigPolicy(key, nil)
		return
	}
	section, ok := security.(map[string]any)
	if !ok {
		slog.Debug(fmt.Sprintf("redaction policy for %s not read: '%s' object has no attribute 'get'", profileHome, pyrepr.TypeName(security)))
		return
	}
	NoteConfigPolicy(key, section["redact_secrets"])
}
