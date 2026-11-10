package buzz

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Private key fields of a buzz credentials record in lookup order
var keyFields = []string{"nsec", "private_key_hex", "private_key"}

// Directory the buzz CLI writes credentials records into
func defaultCredentialsDir() string { return pypath.Expanduser("~/.config/buzz") }

// Scoped secret read with the one shot profile scope fallback of startup gates
func getScopedSecret(ctx context.Context, name, def string) (string, error) {
	return gateway.GetScopedSecret(ctx, name, def, true)
}

// Configured credentials file, the scoped secret first and the platform extra after
func configuredCredentialsFile(ctx context.Context, extra map[string]any) (string, error) {
	configured, err := getScopedSecret(ctx, "BUZZ_CREDENTIALS_FILE", "")
	if err != nil {
		return "", err
	}
	if configured = pystr.Strip(configured); configured != "" {
		return configured, nil
	}
	file, _ := extra["credentials_file"].(string)
	return pystr.Strip(file), nil
}

// Credentials files to read, the configured one or the buzz CLI records unless multiplexing
func credentialsCandidates(ctx context.Context, extra map[string]any) ([]string, error) {
	configured, err := configuredCredentialsFile(ctx, extra)
	if err != nil {
		return nil, err
	}
	if configured != "" {
		return []string{pypath.Expanduser(configured)}, nil
	}
	if provider.IsMultiplexActive(ctx) {
		return nil, nil
	}
	matches, err := filepath.Glob(filepath.Join(defaultCredentialsDir(), "*credentials*.json"))
	if err != nil {
		return nil, nil
	}
	sort.Strings(matches)
	return matches, nil
}

// First non empty private key field of a credentials record, stripped
func credentialsKey(data *pyjson.Object) string {
	for _, field := range keyFields {
		if value, ok := data.Vals[field].(string); ok && pystr.Strip(value) != "" {
			return pystr.Strip(value)
		}
	}
	return ""
}

// First credentials record that carries a private key, unreadable or malformed files skipped
func resolveCredentialsData(ctx context.Context, extra map[string]any) (*pyjson.Object, error) {
	paths, err := credentialsCandidates(ctx, extra)
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		text, err := pystr.DecodeUTF8Sig(raw)
		if err != nil {
			continue
		}
		decoded, err := pyjson.Loads(text)
		if err != nil {
			continue
		}
		if data, ok := decoded.(*pyjson.Object); ok && credentialsKey(data) != "" {
			return data, nil
		}
	}
	return pyjson.NewObject(), nil
}

// Nostr private key, the scoped secret first and the credentials record after
func resolvePrivateKey(ctx context.Context, extra map[string]any) (string, error) {
	key, err := getScopedSecret(ctx, "BUZZ_PRIVATE_KEY", "")
	if err != nil {
		return "", err
	}
	if key = pystr.Strip(key); key != "" {
		return key, nil
	}
	data, err := resolveCredentialsData(ctx, extra)
	if err != nil {
		return "", err
	}
	return credentialsKey(data), nil
}

// Buzz wizard of icebath gateway setup writing the relay, key, channels and access settings to the home .env
func InteractiveSetup(ctx context.Context) error {
	s := gateway.WizardSetup
	ask := func(label, env string) (string, error) {
		current, err := s.GetEnvValue(ctx, env)
		if err != nil {
			return "", err
		}
		return s.Prompt(label, current, false), nil
	}
	s.PrintHeader("Buzz")
	existingRelay, err := s.GetEnvValue(ctx, "BUZZ_RELAY_URL")
	if err != nil {
		return err
	}
	declined, err := gateway.PlatformSetup.DeclinesReconfigure(ctx, "Buzz", "Reconfigure Buzz?", "BUZZ_RELAY_URL")
	if err != nil || declined {
		return err
	}
	s.PrintInfo("Connect Icebath to a Buzz community (Block's Nostr-based human+agent platform).")
	s.PrintInfo("   Requires the buzz CLI binary and a Nostr key that is a community member.")
	gateway.WizardPrintln("")
	relay := s.Prompt("Relay URL (e.g. https://mycommunity.communities.buzz.xyz)", existingRelay, false)
	if relay == "" {
		s.PrintWarning("Relay URL is required — skipping Buzz setup")
		return nil
	}
	if err := s.SaveEnvValue(ctx, "BUZZ_RELAY_URL", pystr.Strip(relay)); err != nil {
		return err
	}
	if key := s.Prompt("Nostr private key (nsec or hex; leave blank to keep current)", "", true); key != "" {
		if err := s.SaveEnvValue(ctx, "BUZZ_PRIVATE_KEY", pystr.Strip(key)); err != nil {
			return err
		}
	} else if resolved, err := resolvePrivateKey(ctx, nil); err != nil {
		return err
	} else if resolved == "" {
		s.PrintWarning("No private key configured — set BUZZ_PRIVATE_KEY before starting the gateway")
	}
	channels, err := ask("Channel UUIDs to watch (comma-separated, empty = all joined channels)", "BUZZ_CHANNELS")
	if err != nil {
		return err
	}
	if channels != "" {
		if err := s.SaveEnvValue(ctx, "BUZZ_CHANNELS", strings.ReplaceAll(channels, " ", "")); err != nil {
			return err
		}
	}
	homeChannel, err := ask("Home channel UUID for cron/notification delivery (optional)", "BUZZ_HOME_CHANNEL")
	if err != nil {
		return err
	}
	if homeChannel != "" {
		if err := s.SaveEnvValue(ctx, "BUZZ_HOME_CHANNEL", pystr.Strip(homeChannel)); err != nil {
			return err
		}
	}
	gateway.WizardPrintln("")
	s.PrintInfo("🔒 Access control: restrict who can talk to the agent")
	if s.PromptYesNo("Allow all community members to talk to the agent?", false) {
		if err := s.SaveEnvValue(ctx, "BUZZ_ALLOW_ALL_USERS", "true"); err != nil {
			return err
		}
		if err := s.SaveEnvValue(ctx, "BUZZ_ALLOWED_USERS", ""); err != nil {
			return err
		}
		s.PrintWarning("⚠️  Open access — anyone in the community can command the agent.")
	} else {
		if err := s.SaveEnvValue(ctx, "BUZZ_ALLOW_ALL_USERS", "false"); err != nil {
			return err
		}
		allowed, err := ask("Allowed users (comma-separated npubs or hex pubkeys, empty to deny everyone)", "BUZZ_ALLOWED_USERS")
		if err != nil {
			return err
		}
		if err := s.SaveEnvValue(ctx, "BUZZ_ALLOWED_USERS", strings.ReplaceAll(allowed, " ", "")); err != nil {
			return err
		}
	}
	gateway.WizardPrintln("")
	s.PrintSuccess("Buzz configuration saved to ~/.icebath/.env")
	s.PrintInfo("Restart the gateway for changes to take effect: icebath gateway restart")
	return nil
}
