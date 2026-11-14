package tools

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/logging"
)

// The logging hooks run the tools redactor and each routed profile's policy resolve_profile_policy recorded
func TestLoggingHooksRedactWithTheResolvedProfilePolicy(t *testing.T) {
	installTestEnvHooks(t)
	pinRedactPolicy(t, true)
	secret := "sk-proj-HOOKEDPROFILE" + strings.Repeat("c", 24)
	if got := logging.RedactSensitiveText("key " + secret); got != RedactSensitiveText("key "+secret) || strings.Contains(got, secret) {
		t.Fatalf("stdio hook %q", got)
	}
	profile := func(env, cfg string) string {
		h := t.TempDir()
		if env != "" {
			if err := os.WriteFile(filepath.Join(h, ".env"), []byte(env), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if cfg != "" {
			if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return h
	}
	for _, tc := range []struct {
		name, env, cfg string
		redacted       bool
	}{
		{"env opt out", "ICEBATH_REDACT_SECRETS=false\n", "", false},
		{"config opt out", "", "security:\n  redact_secrets: false\n", false},
		{"env wins over config", "ICEBATH_REDACT_SECRETS=true\n", "security:\n  redact_secrets: false\n", true},
		{"nothing recorded", "", "", true},
		{"security not a mapping", "", "security: strict\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := profile(tc.env, tc.cfg)
			logging.ResolveProfilePolicy(h)
			var buf bytes.Buffer
			slog.New(logging.RedactingHandler(slog.NewTextHandler(&buf, nil))).WarnContext(home.WithOverride(context.Background(), h), "provider rejected key "+secret)
			if got := !strings.Contains(buf.String(), secret); got != tc.redacted {
				t.Fatalf("redacted %v, log %s", got, buf.String())
			}
		})
	}
}
