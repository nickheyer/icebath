package whatsapp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/lsp"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Outcome of one bridge subprocess, interrupted when Ctrl-C arrived while it ran
type bridgeRun struct {
	code        int
	stderr      string
	interrupted bool
}

// Terminal, profile, package manager and process the WhatsApp wizard works through
type whatsAppWizard struct {
	ctx       context.Context
	home      string
	console   *cliui.Console
	out       io.Writer
	findNode  func(command string) (string, error)
	ensure    func(name string) (map[string]string, error)
	installed func(name string) (*pm.InstalledPackage, error)
	nodeEnv   func() map[string]string
	run       func(argv []string, dir string, env map[string]string, captureStderr bool) (bridgeRun, error)
	bridgeDir func() string
}

// Wizard over the process terminal, this profile and the installed package manager
func newWhatsAppWizard(ctx context.Context, h string, w io.Writer) *whatsAppWizard {
	base := cliui.Default()
	con := &cliui.Console{In: base.In, Out: w, Getenv: base.Getenv, StdinTTY: base.StdinTTY, StdoutTTY: base.StdoutTTY,
		TermSize: base.TermSize, MaskedSecretPrompt: base.MaskedSecretPrompt, OpenScreen: base.OpenScreen}
	rt := &lsp.Runtime{InstalledBinary: func(name string) (string, bool) {
		selected, err := pm.Installed(ctx, name, false)
		if err != nil || selected == nil {
			return "", false
		}
		return selected.Binary, true
	}}
	return &whatsAppWizard{
		ctx: ctx, home: h, console: con, out: w,
		findNode: func(command string) (string, error) { return lsp.FindNodeExecutable(rt, command) },
		ensure: func(name string) (map[string]string, error) {
			runner, err := pm.Ensure(ctx, name, pm.EnsureOptions{Explicit: true})
			if err != nil {
				return nil, err
			}
			return runner.Env, nil
		},
		installed: func(name string) (*pm.InstalledPackage, error) { return pm.Installed(ctx, name, false) },
		nodeEnv:   func() map[string]string { return pm.WithIcebathNodePath(nil) },
		run:       runBridgeProcess,
		bridgeDir: func() string { return ResolveWhatsAppBridgeDir(ctx) },
	}
}

// Runs one bridge command in dir under env, stderr captured when asked, Ctrl-C reported rather than fatal
func runBridgeProcess(argv []string, dir string, env map[string]string, captureStderr bool) (bridgeRun, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cmd.Env = append(cmd.Env, k+"="+env[k])
	}
	var stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if captureStderr {
		cmd.Stdout, cmd.Stderr = nil, &stderr
	}
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	if err := cmd.Start(); err != nil {
		return bridgeRun{}, err
	}
	err := cmd.Wait()
	result := bridgeRun{code: cmd.ProcessState.ExitCode(), stderr: pystr.DecodeUTF8Replace(stderr.Bytes())}
	select {
	case <-interrupts:
		result.interrupted = true
	default:
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return result, err
	}
	return result, nil
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

// Whether the error is the InstallError or OSError the dependency preparation reports and stops on
func preparationFailure(err error) bool {
	var install *pm.InstallError
	var pathErr *os.PathError
	var execErr *exec.Error
	return errors.As(err, &install) || errors.As(err, &pathErr) || errors.As(err, &execErr)
}

// Text of a preparation failure as Python prints the exception
func preparationText(err error) string {
	var install *pm.InstallError
	if errors.As(err, &install) {
		return install.Error()
	}
	return pypath.OSErrorText(err)
}

// Step four, npm installs the bridge dependencies when needed, false meaning stop
func (wz *whatsAppWizard) whatsappInstallBridge(bridgeDir string) (bool, error) {
	if _, err := os.Stat(filepath.Join(bridgeDir, "node_modules")); err == nil {
		fmt.Fprintln(wz.out, "✓ Bridge dependencies already installed")
		return true, nil
	}
	fmt.Fprintln(wz.out, "\n→ Installing WhatsApp bridge dependencies (this can take a few minutes)...")
	npm, err := wz.findNode("npm")
	if err != nil {
		return false, err
	}
	result, err := func() (bridgeRun, error) {
		env := wz.nodeEnv()
		var err error
		if npm == "" {
			if env, err = wz.ensure("npm"); err != nil {
				return bridgeRun{}, err
			}
			installed, err := wz.installed("npm")
			if err != nil {
				return bridgeRun{}, err
			}
			if installed == nil || installed.Binary == "" {
				return bridgeRun{}, pm.NewInstallError("npm", "npm binary is missing after preparation", "")
			}
			npm = installed.Binary
		}
		return wz.run([]string{npm, "install", "--no-fund", "--no-audit", "--progress=false"}, bridgeDir, env, true)
	}()
	if err != nil {
		if preparationFailure(err) {
			fmt.Fprintf(wz.out, "  ✗ Bridge dependency preparation failed: %s\n", preparationText(err))
			return false, nil
		}
		return false, err
	}
	if result.interrupted {
		fmt.Fprintln(wz.out, "\n  ✗ Install cancelled")
		return false, nil
	}
	if result.code != 0 {
		preview := "(no output)"
		if text := pystr.Strip(result.stderr); text != "" {
			lines := pystr.Splitlines(text, false)
			preview = strings.Join(lines[max(0, len(lines)-30):], "\n")
		}
		wz.say("  ✗ npm install failed:", preview)
		return false, nil
	}
	fmt.Fprintln(wz.out, "  ✓ Dependencies installed")
	return true, nil
}

// Node binary for the pairing run, installing it through the package manager when absent
func (wz *whatsAppWizard) pairingNode() (string, error) {
	node, err := wz.findNode("node")
	if err != nil || node != "" {
		return node, err
	}
	if _, err := wz.ensure("node"); err != nil {
		return "", err
	}
	installed, err := wz.installed("node")
	if err != nil {
		return "", err
	}
	if installed == nil || installed.Binary == "" {
		return "", pm.NewInstallError("node", "Node.js binary is missing after preparation", "")
	}
	return installed.Binary, nil
}

// Sets up WhatsApp, choosing the mode, the allowlist, installing the bridge and pairing it by QR code
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
	bridgeDir := wz.bridgeDir()
	bridgeScript := filepath.Join(bridgeDir, "bridge.js")
	if _, err := os.Stat(bridgeScript); err != nil {
		fmt.Fprintf(wz.out, "\n✗ Bridge script not found at %s\n", bridgeScript)
		return nil
	}
	if ok, err := wz.whatsappInstallBridge(bridgeDir); err != nil || !ok {
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
	node, err := wz.pairingNode()
	if err != nil {
		var install *pm.InstallError
		if errors.As(err, &install) {
			fmt.Fprintf(wz.out, "  ✗ Node.js preparation failed: %s\n", install.Error())
			return nil
		}
		return err
	}
	if _, err := wz.run([]string{node, bridgeScript, "--pair-only", "--session", sessionDir}, bridgeDir, wz.nodeEnv(), false); err != nil {
		return err
	}
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
