package buzz

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
)

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
	if err := s.SaveEnvValue(ctx, "BUZZ_RELAY_URL", strip(relay)); err != nil {
		return err
	}
	if key := s.Prompt("Nostr private key (nsec or hex; leave blank to keep current)", "", true); key != "" {
		if err := s.SaveEnvValue(ctx, "BUZZ_PRIVATE_KEY", strip(key)); err != nil {
			return err
		}
	} else {
		resolver := &settings{ctx: ctx, extra: map[string]any{}}
		resolved := resolver.privateKey()
		if resolver.err != nil {
			return resolver.err
		}
		if resolved == "" {
			s.PrintWarning("No private key configured — set BUZZ_PRIVATE_KEY before starting the gateway")
		}
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
		if err := s.SaveEnvValue(ctx, "BUZZ_HOME_CHANNEL", strip(homeChannel)); err != nil {
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
