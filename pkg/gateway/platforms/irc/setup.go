package irc

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// IRC wizard of icebath gateway setup writing the server, identity, auth and access settings to the home .env
func InteractiveSetup(ctx context.Context) error {
	s := gateway.WizardSetup
	info := func(lines ...string) {
		for _, line := range lines {
			s.PrintInfo(line)
		}
	}
	get := func(name string) (string, error) { return s.GetEnvValue(ctx, name) }
	required := func(label, env, def, what string) (bool, error) {
		value := s.Prompt(label, def, false)
		if value == "" {
			s.PrintWarning(what + " is required — skipping IRC setup")
			return false, nil
		}
		return true, s.SaveEnvValue(ctx, env, pystr.Strip(value))
	}
	s.PrintHeader("IRC")
	existingServer, err := get("IRC_SERVER")
	if err != nil {
		return err
	}
	declined, err := gateway.PlatformSetup.DeclinesReconfigure(ctx, "IRC", "Reconfigure IRC?", "IRC_SERVER")
	if err != nil || declined {
		return err
	}
	info("Connect Icebath to an IRC network. Uses Python stdlib — no extra packages needed.",
		"   Works with Libera.Chat, OFTC, your own ZNC/InspIRCd, etc.")
	gateway.WizardPrintln("")
	if ok, err := required("IRC server hostname (e.g. irc.libera.chat)", "IRC_SERVER", existingServer, "Server"); err != nil || !ok {
		return err
	}
	useTLS := s.PromptYesNo("Use TLS (recommended)?", true)
	tls, defaultPort := "false", "6667"
	if useTLS {
		tls, defaultPort = "true", "6697"
	}
	if err := s.SaveEnvValue(ctx, "IRC_USE_TLS", tls); err != nil {
		return err
	}
	currentPort, err := get("IRC_PORT")
	if err != nil {
		return err
	}
	if port := s.Prompt("Port (default "+defaultPort+")", currentPort, false); port != "" {
		if n, ok := pyint.Parse(port); ok {
			if err := s.SaveEnvValue(ctx, "IRC_PORT", n.String()); err != nil {
				return err
			}
		} else {
			s.PrintWarning("Invalid port — using default " + defaultPort)
		}
	} else if again, err := get("IRC_PORT"); err != nil {
		return err
	} else if again != "" {
		if err := s.SaveEnvValue(ctx, "IRC_PORT", ""); err != nil {
			return err
		}
	}
	for _, r := range [][3]string{{"Bot nickname (e.g. icebath-bot)", "IRC_NICKNAME", "Nickname"},
		{"Channel to join (e.g. #icebath — comma-separate for multiple)", "IRC_CHANNEL", "Channel"}} {
		current, err := get(r[1])
		if err != nil {
			return err
		}
		if ok, err := required(r[0], r[1], current, r[2]); err != nil || !ok {
			return err
		}
	}
	gateway.WizardPrintln("")
	info("🔑 Optional authentication", "   Leave blank to skip.")
	for _, a := range [][3]string{{"Configure a server password (PASS command)?", "Server password", "IRC_SERVER_PASSWORD"},
		{"Identify with NickServ on connect?", "NickServ password", "IRC_NICKSERV_PASSWORD"}} {
		if !s.PromptYesNo(a[0], false) {
			continue
		}
		if secret := s.Prompt(a[1], "", true); secret != "" {
			if err := s.SaveEnvValue(ctx, a[2], secret); err != nil {
				return err
			}
		}
	}
	gateway.WizardPrintln("")
	info("🔒 Access control: restrict who can message the bot",
		"   IRC nicks are not authenticated — anyone can claim any nick.",
		"   For public channels, pair with NickServ-only mode on your network",
		"   if you want stronger identity guarantees.")
	if s.PromptYesNo("Allow all users in the channel to talk to the bot?", false) {
		if err := s.SaveEnvValue(ctx, "IRC_ALLOW_ALL_USERS", "true"); err != nil {
			return err
		}
		if err := s.SaveEnvValue(ctx, "IRC_ALLOWED_USERS", ""); err != nil {
			return err
		}
		s.PrintWarning("⚠️  Open access — any nick in the channel can command the bot.")
	} else {
		if err := s.SaveEnvValue(ctx, "IRC_ALLOW_ALL_USERS", "false"); err != nil {
			return err
		}
		current, err := get("IRC_ALLOWED_USERS")
		if err != nil {
			return err
		}
		if allowed := s.Prompt("Allowed nicks (comma-separated, leave empty to deny everyone)", current, false); allowed != "" {
			if err := s.SaveEnvValue(ctx, "IRC_ALLOWED_USERS", strings.ReplaceAll(allowed, " ", "")); err != nil {
				return err
			}
			s.PrintSuccess("Allowlist configured")
		} else {
			if err := s.SaveEnvValue(ctx, "IRC_ALLOWED_USERS", ""); err != nil {
				return err
			}
			s.PrintInfo("No nicks allowed — the bot will ignore all messages until you add nicks.")
		}
	}
	gateway.WizardPrintln("")
	s.PrintSuccess("IRC configuration saved to ~/.icebath/.env")
	s.PrintInfo("Restart the gateway for changes to take effect: icebath gateway restart")
	return nil
}
