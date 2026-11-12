package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Terminal, profile, package manager and process the WhatsApp wizard works through
type whatsAppWizard struct {
	ctx     context.Context
	home    string
	console *cliui.Console
	out     io.Writer
	pair    func(session, mode string) int
}

// Wizard over the process terminal, this profile and the installed package manager
func newWhatsAppWizard(ctx context.Context, h string, w io.Writer) *whatsAppWizard {
	base := cliui.Default()
	con := &cliui.Console{In: base.In, Out: w, Getenv: base.Getenv, StdinTTY: base.StdinTTY, StdoutTTY: base.StdoutTTY,
		TermSize: base.TermSize, MaskedSecretPrompt: base.MaskedSecretPrompt, OpenScreen: base.OpenScreen}
	return &whatsAppWizard{
		ctx: ctx, home: h, console: con, out: w,
		pair: func(session, mode string) int { return RunPairing(ctx, session, mode, w, false) },
	}
}

// Prints each line, an empty one a blank line, one call per banner block
func (wz *whatsAppWizard) say(lines ...string) { fmt.Fprintln(wz.out, strings.Join(lines, "\n")) }

// Reads one input line, the Ctrl-C or EOF error when the user cancelled
func (wz *whatsAppWizard) input(prompt string) (string, error) { return wz.console.Input(prompt) }

// Whether the input error is the KeyboardInterrupt or EOFError of a cancelled prompt
func promptCancelled(err error) bool {
	return errors.Is(err, cliui.ErrInterrupt) || errors.Is(err, cliui.ErrEOF)
}

// A y/N prompt, Ctrl-C or EOF counting as no
func (wz *whatsAppWizard) yesNo(prompt string) (bool, error) {
	response, err := wz.input(prompt)
	if err != nil {
		if !promptCancelled(err) {
			return false, err
		}
		response = "n"
	}
	response = pystr.Lower(pystr.Strip(response))
	return response == "y" || response == "yes", nil
}

// Profile env value, empty when unset
func (wz *whatsAppWizard) envValue(key string) (string, error) {
	value, _, err := config.GetEnvValue(wz.ctx, wz.home, key)
	return value, err
}

// Saves one profile env value
func (wz *whatsAppWizard) saveEnv(key, value string) error {
	return config.SaveEnvValueContext(wz.ctx, wz.home, key, value)
}

// Step one, the bot or self chat mode, empty when cancelled
func (wz *whatsAppWizard) whatsappChooseMode() (string, error) {
	current, err := wz.envValue("WHATSAPP_MODE")
	if err != nil {
		return "", err
	}
	if current != "" {
		label := "personal number (self-chat)"
		if current == "bot" {
			label = "separate bot number"
		}
		fmt.Fprintf(wz.out, "\n✓ Mode: %s\n", label)
		return current, nil
	}
	wz.say("", "How will you use WhatsApp with Icebath?", "",
		"  1. Separate bot number (recommended)",
		"     People message the bot's number directly — cleanest experience.",
		"     Requires a second phone number with WhatsApp installed on a device.", "",
		"  2. Personal number (self-chat)",
		"     You message yourself to talk to the agent.",
		"     Quick to set up, but the UX is less intuitive.", "")
	choice, err := wz.input("  Choose [1/2]: ")
	if err != nil {
		if promptCancelled(err) {
			fmt.Fprintln(wz.out, "\nSetup cancelled.")
			return "", nil
		}
		return "", err
	}
	if pystr.Strip(choice) == "1" {
		if err := wz.saveEnv("WHATSAPP_MODE", "bot"); err != nil {
			return "", err
		}
		wz.say("  ✓ Mode: separate bot number", "",
			"  ┌─────────────────────────────────────────────────┐",
			"  │  Getting a second number for the bot:           │",
			"  │                                                 │",
			"  │  Easiest: Install WhatsApp Business (free app)  │",
			"  │  on your phone with a second number:            │",
			"  │    • Dual-SIM: use your 2nd SIM slot            │",
			"  │    • Google Voice: free US number (voice.google) │",
			"  │    • Prepaid SIM: $3-10, verify once            │",
			"  │                                                 │",
			"  │  WhatsApp Business runs alongside your personal │",
			"  │  WhatsApp — no second phone needed.             │",
			"  └─────────────────────────────────────────────────┘")
		return "bot", nil
	}
	if err := wz.saveEnv("WHATSAPP_MODE", "self-chat"); err != nil {
		return "", err
	}
	fmt.Fprintln(wz.out, "  ✓ Mode: personal number (self-chat)")
	return "self-chat", nil
}

// Step three, shows or sets the allowed users
func (wz *whatsAppWizard) whatsappAllowedUsers(mode string) error {
	current, err := wz.envValue("WHATSAPP_ALLOWED_USERS")
	if err != nil {
		return err
	}
	save := func(phone, label string) error {
		if err := wz.saveEnv("WHATSAPP_ALLOWED_USERS", strings.ReplaceAll(phone, " ", "")); err != nil {
			return err
		}
		fmt.Fprintf(wz.out, "  ✓ %s: %s\n", label, phone)
		return nil
	}
	if current != "" {
		fmt.Fprintf(wz.out, "✓ Allowed users: %s\n", current)
		update, err := wz.yesNo("\n  Update allowed users? [y/N] ")
		if err != nil || !update {
			return err
		}
		prompt := "  Your phone number (e.g. 15551234567): "
		if mode == "bot" {
			prompt = "  Phone numbers that can message the bot (comma-separated): "
		}
		phone, err := wz.console.LineInput(prompt)
		if err != nil {
			return err
		}
		if phone = pystr.Strip(phone); phone != "" {
			return save(phone, "Updated to")
		}
		return nil
	}
	fmt.Fprintln(wz.out)
	var phone string
	if mode == "bot" {
		fmt.Fprintln(wz.out, "  Who should be allowed to message the bot?")
		phone, err = wz.console.LineInput("  Phone numbers (comma-separated, or * for anyone): ")
	} else {
		phone, err = wz.console.LineInput("  Your phone number (e.g. 15551234567): ")
	}
	if err != nil {
		return err
	}
	if phone = pystr.Strip(phone); phone != "" {
		return save(phone, "Allowed users set")
	}
	fmt.Fprintln(wz.out, "  ⚠ No allowlist — the agent will respond to ALL incoming messages")
	return nil
}

// Sets up WhatsApp, choosing the mode and the allowlist, then pairing it by QR code in process
func CmdWhatsApp(ctx context.Context, h string, ns map[string]any, w io.Writer, requireTTY func(command string)) (int, error) {
	requireTTY("whatsapp")
	return 0, newWhatsAppWizard(ctx, h, w).setup()
}

// The wizard steps in order, enabling WhatsApp only once pairing succeeded
func (wz *whatsAppWizard) setup() error {
	wz.say("", "☤ WhatsApp Setup", strings.Repeat("=", 50))
	mode, err := wz.whatsappChooseMode()
	if err != nil || mode == "" {
		return err
	}
	fmt.Fprintln(wz.out)
	enabled, err := wz.envValue("WHATSAPP_ENABLED")
	if err != nil {
		return err
	}
	if pystr.Lower(enabled) == "true" {
		fmt.Fprintln(wz.out, "✓ WhatsApp is already enabled")
	}
	if err := wz.whatsappAllowedUsers(mode); err != nil {
		return err
	}
	sessionDir := filepath.Join(wz.home, "whatsapp", "session")
	if err := os.MkdirAll(sessionDir, 0o777); err != nil {
		return err
	}
	creds := filepath.Join(sessionDir, "creds.json")
	if _, err := os.Stat(creds); err == nil {
		fmt.Fprintln(wz.out, "✓ Existing WhatsApp session found")
		repair, err := wz.yesNo("\n  Re-pair? This will clear the existing session. [y/N] ")
		if err != nil {
			return err
		}
		if repair {
			_ = os.RemoveAll(sessionDir)
			if err := os.MkdirAll(sessionDir, 0o777); err != nil {
				return err
			}
			fmt.Fprintln(wz.out, "  ✓ Session cleared")
		} else {
			if enabled, err = wz.envValue("WHATSAPP_ENABLED"); err != nil {
				return err
			}
			if pystr.Lower(enabled) != "true" {
				if err := wz.saveEnv("WHATSAPP_ENABLED", "true"); err != nil {
					return err
				}
			}
			wz.say("\n✓ WhatsApp is configured and paired!", "  Start the gateway with: icebath gateway")
			return nil
		}
	}
	wz.say("", strings.Repeat("─", 50))
	if mode == "bot" {
		wz.say("📱 Open WhatsApp (or WhatsApp Business) on the", "   phone with the BOT's number, then scan:")
	} else {
		fmt.Fprintln(wz.out, "📱 Open WhatsApp on your phone, then scan:")
	}
	wz.say("", "   Settings → Linked Devices → Link a Device", strings.Repeat("─", 50), "")
	wz.pair(sessionDir, mode)
	fmt.Fprintln(wz.out)
	if _, err := os.Stat(creds); err != nil {
		fmt.Fprintln(wz.out, "⚠ Pairing may not have completed. Run 'icebath whatsapp' to try again.")
		return nil
	}
	if err := wz.saveEnv("WHATSAPP_ENABLED", "true"); err != nil {
		return err
	}
	wz.say("✓ WhatsApp paired successfully!", "")
	if mode == "bot" {
		wz.say("  Next steps:", "    1. Start the gateway:  icebath gateway",
			"    2. Send a message to the bot's WhatsApp number",
			"    3. The agent will reply automatically", "",
			"  Tip: Agent responses are prefixed with '☤ Icebath Agent'")
	} else {
		wz.say("  Next steps:", "    1. Start the gateway:  icebath gateway",
			"    2. Open WhatsApp → Message Yourself",
			"    3. Type a message — the agent will reply", "",
			"  Tip: Agent responses are prefixed with '☤ Icebath Agent'",
			"  so you can tell them apart from your own messages.")
	}
	wz.say("", "  Or install as a service: icebath gateway install")
	return nil
}
