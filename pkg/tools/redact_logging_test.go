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
	profile := func(cfg string) string {
		h := t.TempDir()
		if cfg != "" {
			if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return h
	}
	for _, tc := range []struct {
		name, cfg string
		redacted  bool
	}{
		{"config opt out", "security:\n  redact_secrets: false\n", false},
		{"config opt in", "security:\n  redact_secrets: true\n", true},
		{"nothing recorded", "", true},
		{"security not a mapping", "security: strict\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := profile(tc.cfg)
			logging.ResolveProfilePolicy(h)
			var buf bytes.Buffer
			slog.New(logging.RedactingHandler(slog.NewTextHandler(&buf, nil))).WarnContext(home.WithOverride(context.Background(), h), "provider rejected key "+secret)
			if got := !strings.Contains(buf.String(), secret); got != tc.redacted {
				t.Fatalf("redacted %v, log %s", got, buf.String())
			}
		})
	}
}
