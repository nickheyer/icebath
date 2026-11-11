package whatsappcloud

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"strings"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Field shape check returning ok or the reason the value is malformed
type cloudValidator func(value string) (bool, string)

// One check run on the stripped value, the reason fixed or computed from the value
type cloudRule struct {
	fails  func(s string) bool
	reason func(s string) string
}

// Fixed reason text of a rule
func fixed(reason string) func(string) string { return func(string) string { return reason } }

// Validator requiring a value then applying the rules in order to its stripped form
func rulesValidator(label string, rules ...cloudRule) cloudValidator {
	return func(value string) (bool, string) {
		if value == "" {
			return false, label + " is required"
		}
		s := pystr.Strip(value)
		for _, rule := range rules {
			if rule.fails(s) {
				return false, rule.reason(s)
			}
		}
		return true, ""
	}
}

// Validator of a numeric Meta id whose digit count falls within lo and hi
func numericIDValidator(label string, lo, hi int, expected string) cloudValidator {
	return rulesValidator(label,
		cloudRule{func(s string) bool { return !pystr.StrIsDigit(s) }, fixed(label + " must be numeric")},
		cloudRule{func(s string) bool { return pystr.Len(s) < lo || pystr.Len(s) > hi }, fixed(fmt.Sprintf("%s looks wrong (expected %s)", label, expected))})
}

var (
	validatePhoneNumberID = rulesValidator("Phone Number ID",
		cloudRule{func(s string) bool { return !pystr.StrIsDigit(s) }, fixed("Phone Number ID must be numeric (no '+', spaces, or dashes)")},
		cloudRule{func(s string) bool { return 10 <= pystr.Len(s) && pystr.Len(s) <= 12 }, fixed("That looks like a phone number — but this field needs the " +
			"Phone Number ID (Meta's internal ID, 15-17 digits, e.g. " +
			"'7794189252778687'). Look just BELOW the 'From' dropdown in " +
			"API Setup → it's labelled 'Phone number ID'.")},
		cloudRule{func(s string) bool { return pystr.Len(s) < 13 }, fixed("Phone Number ID looks too short (expected 13-18 digits)")},
		cloudRule{func(s string) bool { return pystr.Len(s) > 20 }, fixed("Phone Number ID looks too long (expected 13-18 digits)")})
	validateWabaID    = numericIDValidator("WABA ID", 10, 25, "10-25 digits")
	validateAppID     = numericIDValidator("App ID", 13, 20, "15-16 digits")
	validateAppSecret = rulesValidator("App Secret",
		cloudRule{func(s string) bool { return !lowerHex(pystr.Lower(s)) }, fixed("App Secret should be a hex string (only digits 0-9 and " +
			"letters a-f). Make sure you copied the 'App secret' from " +
			"Settings → Basic, not some other token.")},
		cloudRule{func(s string) bool { return pystr.Len(s) != 32 }, func(s string) string {
			return fmt.Sprintf("App Secret should be exactly 32 hex characters (got %d)", pystr.Len(s))
		}})
	validateAccessToken = rulesValidator("Access token",
		cloudRule{func(s string) bool { return !strings.HasPrefix(s, "EAA") }, notMetaTokenReason},
		cloudRule{func(s string) bool { return pystr.Len(s) < 100 }, func(s string) string {
			return fmt.Sprintf("Access token looks too short (%d chars, expected 100+)", pystr.Len(s))
		}})
)

// Whether the text is one or more lowercase hex digits
func lowerHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// Common paste mistakes for the access token field and what each actually is
var foreignTokenPrefixes = []struct {
	prefixes []string
	reason   string
}{
	{[]string{"sk-"}, "That's an OpenAI key (starts with 'sk-'), not a Meta " +
		"WhatsApp access token. Meta tokens start with 'EAA'."},
	{[]string{"xoxb-", "xoxp-"}, "That's a Slack token, not a Meta WhatsApp access token. " +
		"Meta tokens start with 'EAA'."},
	{[]string{"ghp_", "gho_"}, "That's a GitHub token, not a Meta WhatsApp access " +
		"token. Meta tokens start with 'EAA'."},
}

// Why a value is not a Meta access token, naming the service whose token it looks like
func notMetaTokenReason(s string) string {
	for _, foreign := range foreignTokenPrefixes {
		for _, prefix := range foreign.prefixes {
			if strings.HasPrefix(s, prefix) {
				return foreign.reason
			}
		}
	}
	return "Meta WhatsApp access tokens start with 'EAA'. Check that " +
		"you're copying from the right place (API Setup → 'Generate " +
		"access token', or Business Settings → System Users → " +
		"'Generate token' for a permanent one)."
}

// The terminal and profile the Cloud API wizard reads from and saves to
type cloudWizard struct {
	ctx     context.Context
	home    string
	console *cliui.Console
	out     io.Writer
}

// Prints each line, an empty one a blank line
func (wz *cloudWizard) lines(lines ...string) {
	for _, line := range lines {
		fmt.Fprintln(wz.out, line)
	}
}

// Section title between two rules
func (wz *cloudWizard) header(title string) {
	wz.lines(strings.Repeat("─", 50), title, strings.Repeat("─", 50))
}

// One stripped line, empty on EOF, Ctrl-C or an empty answer, a secret read without echo on a terminal
func (wz *cloudWizard) prompt(message, def string, secret bool) string {
	suffix := ""
	if def != "" {
		suffix = " [" + def + "]"
	}
	var value string
	var err error
	if secret && wz.console.StdinIsTTY() {
		value, err = provider.Getpass(message + suffix + " (input hidden): ")
	} else {
		value, err = wz.console.LineInput(message + suffix + ": ")
	}
	if err != nil {
		fmt.Fprintln(wz.out)
		return ""
	}
	return pystr.Strip(value)
}

// Repeats the prompt until a valid value, empty when the user gives up
func (wz *cloudWizard) promptValidated(message string, validator cloudValidator, current, helpText string, secret bool) string {
	if helpText != "" {
		for _, line := range pystr.Splitlines(pystr.Strip(helpText), false) {
			fmt.Fprintf(wz.out, "  %s\n", line)
		}
	}
	attempts := 0
	for {
		attempts++
		value := wz.prompt("  → "+message, current, secret)
		if value == "" {
			return ""
		}
		ok, reason := validator(value)
		if ok {
			return pystr.Strip(value)
		}
		fmt.Fprintf(wz.out, "    ✗ %s\n", reason)
		if attempts >= 3 {
			again, err := wz.console.Input("    Try again, or press Enter to skip: ")
			if err != nil || pystr.Strip(again) == "" {
				return ""
			}
			attempts = 0
		}
	}
}

// Profile env value, empty when unset
func (wz *cloudWizard) envValue(key string) (string, error) {
	value, _, err := config.GetEnvValue(wz.ctx, wz.home, key)
	return value, err
}

// Saves the value or keeps the current one, printing which, the effective value returned
func (wz *cloudWizard) persist(envVar, value, current, saved, kept string) (string, error) {
	if value != "" {
		if err := config.SaveEnvValueContext(wz.ctx, wz.home, envVar, value); err != nil {
			return "", err
		}
		fmt.Fprintln(wz.out, strings.ReplaceAll(saved, "{v}", value))
		return value, nil
	}
	if current != "" {
		fmt.Fprintln(wz.out, strings.ReplaceAll(kept, "{v}", current))
	}
	return current, nil
}

// One required or optional credential step of the wizard
type credentialStep struct {
	title, envVar, label string
	validator            cloudValidator
	secret               bool
	preview              int
	saved, kept          string
	missing              []string
	required             bool
	helpText             string
}

// Credential steps one to three
var credentialSteps = []credentialStep{
	{"STEP 1 — Phone Number ID", "WHATSAPP_CLOUD_PHONE_NUMBER_ID", "Phone Number ID",
		validatePhoneNumberID, false, 0, "  ✓ Saved: {v}", "  ✓ Keeping existing: {v}",
		[]string{"\n✗ Phone Number ID is required. Aborting."}, true,
		"Found in: App Dashboard → WhatsApp → API Setup, in the\n" +
			"'Send and receive messages' section.\n" +
			"Look BELOW the 'From' dropdown — there's a 'Phone number ID'\n" +
			"line with the value (15-17 digits, e.g. '7794189252778687').\n" +
			"It is NOT the phone number itself (+1 555-...). That's the\n" +
			"single most common setup mistake."},
	{"STEP 2 — Access Token", "WHATSAPP_CLOUD_ACCESS_TOKEN", "Access Token",
		validateAccessToken, true, 15, "  ✓ Saved (token hidden)", "  ✓ Keeping existing token",
		[]string{"\n✗ Access Token is required. Aborting."}, true,
		"Two options for getting one:\n\n" +
			"  (a) TEMP — App Dashboard → WhatsApp → API Setup →\n" +
			"      'Generate access token' button. Lasts 24 hours.\n" +
			"      Fine for testing today; you'll have to regenerate\n" +
			"      tomorrow.\n\n" +
			"  (b) PERMANENT (production) — System User token. One-time\n" +
			"      setup, never expires:\n" +
			"      • business.facebook.com → Settings → System users →\n" +
			"        Add → Admin role\n" +
			"      • Assign Assets → your app (Manage app), your\n" +
			"        WhatsApp account (Manage WABAs)\n" +
			"      • Generate token → expiration: Never → permissions:\n" +
			"        business_management, whatsapp_business_messaging,\n" +
			"        whatsapp_business_management\n\n" +
			"Tokens start with 'EAA'."},
	{"STEP 3 — App Secret (required for webhook signature verification)", "WHATSAPP_CLOUD_APP_SECRET",
		"App Secret", validateAppSecret, true, 8, "  ✓ Saved (secret hidden)",
		"  ✓ Keeping existing App Secret",
		[]string{"\n⚠ Skipping App Secret — inbound webhooks will be refused",
			"   until you set WHATSAPP_CLOUD_APP_SECRET manually."}, false,
		"Found in: App Dashboard → Settings → Basic →\n" +
			"'App secret' field (click 'Show', enter your Facebook password).\n\n" +
			"If 'Show' doesn't appear, you may need Admin role on the app.\n" +
			"It's a 32-character lowercase hex string.\n\n" +
			"Without the App Secret, inbound webhook POSTs are refused\n" +
			"with HTTP 503 (we can't verify they actually came from Meta)."},
}

// Optional step four ids with their prompt label, env var, validator and help text
var optionalIDSteps = []struct {
	label, envVar string
	validator     cloudValidator
	helpText      string
}{
	{"App ID (optional, press Enter to skip)", "WHATSAPP_CLOUD_APP_ID", validateAppID,
		"Found in: App Dashboard → Settings → Basic → 'App ID' at the\n" +
			"top of the page. Numeric, ~15-16 digits.\n" +
			"Not required for messaging — useful only for analytics later."},
	{"WABA ID (optional, press Enter to skip)", "WHATSAPP_CLOUD_WABA_ID", validateWabaID,
		"WhatsApp Business Account ID. Found in: App Dashboard →\n" +
			"WhatsApp → API Setup, near the top — 'WhatsApp Business\n" +
			"Account ID'. Numeric, ~15+ digits.\n" +
			"When set, inbound webhooks must match this business account."},
}

// Runs one credential step, true when the wizard must abort
func (wz *cloudWizard) credentialStep(step credentialStep) (bool, error) {
	wz.header(step.title)
	current, err := wz.envValue(step.envVar)
	if err != nil {
		return false, err
	}
	shown := current
	if step.preview > 0 && current != "" {
		shown = pystr.Slice(current, 0, step.preview) + "..."
	}
	value := wz.promptValidated(step.label, step.validator, shown, step.helpText, step.secret)
	effective, err := wz.persist(step.envVar, value, current, step.saved, step.kept)
	if err != nil {
		return false, err
	}
	if effective == "" {
		wz.lines(step.missing...)
		if step.required {
			return true, nil
		}
	}
	fmt.Fprintln(wz.out)
	return false, nil
}

// Step four, the optional App ID and WABA ID, each env var mapped to its effective value
func (wz *cloudWizard) stepOptionalIDs() (map[string]string, error) {
	wz.header("STEP 4 — App ID & WABA ID (optional; WABA ID binds inbound webhooks)")
	ids := map[string]string{}
	for _, step := range optionalIDSteps {
		current, err := wz.envValue(step.envVar)
		if err != nil {
			return nil, err
		}
		value := wz.promptValidated(step.label, step.validator, current, step.helpText, false)
		if ids[step.envVar], err = wz.persist(step.envVar, value, current, "  ✓ Saved: {v}", "  ✓ Keeping existing: {v}"); err != nil {
			return nil, err
		}
	}
	fmt.Fprintln(wz.out)
	return ids, nil
}

// URL safe random token of n bytes, secrets.token_urlsafe
func tokenURLSafe(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Step five, generates or keeps the webhook verify token and returns the effective one
func (wz *cloudWizard) stepVerifyToken() (string, error) {
	wz.header("STEP 5 — Verify Token (auto-generated)")
	verifyToken, err := wz.envValue("WHATSAPP_CLOUD_VERIFY_TOKEN")
	if err != nil {
		return "", err
	}
	regen := "y"
	if verifyToken != "" {
		fmt.Fprintf(wz.out, "  An existing verify token is already set (%s...).\n", pystr.Slice(verifyToken, 0, 8))
		answer, err := wz.console.Input("  Generate a new one? [y/N]: ")
		if err != nil {
			answer = "n"
		}
		regen = pystr.Lower(pystr.Strip(answer))
	}
	if regen == "y" || regen == "yes" {
		label := "Generated"
		if verifyToken != "" {
			label = "New verify token"
		}
		verifyToken = tokenURLSafe(32)
		if err := config.SaveEnvValueContext(wz.ctx, wz.home, "WHATSAPP_CLOUD_VERIFY_TOKEN", verifyToken); err != nil {
			return "", err
		}
		fmt.Fprintf(wz.out, "  ✓ %s: %s\n", label, verifyToken)
	} else {
		fmt.Fprintln(wz.out, "  ✓ Keeping existing verify token")
	}
	wz.lines("", "  → COPY THIS TOKEN NOW. You'll paste it into Meta's webhook",
		"    configuration dialog (next step).", "")
	return verifyToken, nil
}

// One allowlist entry with whitespace, dashes and plus signs removed
func cleanRecipient(part string) string {
	var b strings.Builder
	for _, r := range part {
		if !pystr.IsSpace(r) && r != '-' && r != '+' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Step six, the recipient allowlist
func (wz *cloudWizard) stepAllowlist() error {
	wz.header("STEP 6 — Recipient Allowlist")
	wz.lines("", "  Who is allowed to message the bot? (Comma-separated phone",
		"  numbers with country code, no '+' / spaces / dashes. Use '*'",
		"  to allow anyone — only safe if you've also configured Meta's",
		"  recipient whitelist for app-development mode.)", "")
	allowDefault, err := wz.envValue("WHATSAPP_CLOUD_ALLOWED_USERS")
	if err != nil {
		return err
	}
	suffix := ""
	if allowDefault != "" {
		suffix = " [" + allowDefault + "]"
	}
	allowed, err := wz.console.LineInput("  → Allowed users" + suffix + ": ")
	if err != nil {
		allowed = ""
	} else if allowed = pystr.Strip(allowed); allowed == "" {
		allowed = allowDefault
	}
	if allowed != "" {
		var parts []string
		for _, part := range strings.Split(allowed, ",") {
			if pystr.Strip(part) != "" {
				parts = append(parts, cleanRecipient(part))
			}
		}
		allowed = strings.Join(parts, ",")
		if err := config.SaveEnvValueContext(wz.ctx, wz.home, "WHATSAPP_CLOUD_ALLOWED_USERS", allowed); err != nil {
			return err
		}
		fmt.Fprintf(wz.out, "  ✓ Saved: %s\n", allowed)
	} else {
		wz.lines("  ⚠ No allowlist — every inbound message will be denied.",
			"    Re-run this wizard or set WHATSAPP_CLOUD_ALLOWED_USERS manually.")
	}
	fmt.Fprintln(wz.out)
	return nil
}

// Interactive wizard for the WhatsApp Cloud API adapter, 0 on success and 1 on abort
func (wz *cloudWizard) runWhatsAppCloudSetup() (int, error) {
	wz.lines("", "☤ WhatsApp Business Cloud API Setup", strings.Repeat("=", 50), "",
		"This wizard configures Icebath to talk to WhatsApp via Meta's",
		"official Cloud API. It's the production-grade path:", "",
		"  • No QR codes, no Node.js bridge subprocess",
		"  • Stable connection — no account-ban risk",
		"  • Business account required (not personal WhatsApp)",
		"  • Public webhook URL required (Cloudflare Tunnel, ngrok,",
		"    or your own reverse proxy with TLS)", "",
		"If you don't have a Meta app set up yet, follow these steps",
		"FIRST, then come back and re-run this wizard:", "",
		"  1. https://developers.facebook.com/apps → Create App",
		"     → 'Connect with customers through WhatsApp'",
		"  2. App Dashboard → WhatsApp → API Setup",
		"  3. Click 'Generate access token' (temp 24h token is fine to",
		"     start; switch to a System User permanent token later)", "")
	if _, err := wz.console.Input("Press Enter to continue, or Ctrl+C to abort... "); err != nil {
		fmt.Fprintln(wz.out, "\nSetup cancelled.")
		return 1, nil
	}
	fmt.Fprintln(wz.out)
	for _, step := range credentialSteps {
		abort, err := wz.credentialStep(step)
		if err != nil {
			return 0, err
		}
		if abort {
			return 1, nil
		}
	}
	ids, err := wz.stepOptionalIDs()
	if err != nil {
		return 0, err
	}
	effectiveWaba := ids["WHATSAPP_CLOUD_WABA_ID"]
	verifyToken, err := wz.stepVerifyToken()
	if err != nil {
		return 0, err
	}
	if err := wz.stepAllowlist(); err != nil {
		return 0, err
	}
	wz.header("SETUP COMPLETE — Next steps")
	wz.lines("", "  Icebath needs a public HTTPS URL to receive WhatsApp messages.",
		"  The recommended path is Cloudflare Tunnel (free, no port",
		"  forwarding, no DNS setup).", "",
		"    1. Install cloudflared (one-time, if you don't have it):",
		"         Windows:  winget install Cloudflare.cloudflared",
		"         macOS:    brew install cloudflared",
		"         Linux:    https://github.com/cloudflare/cloudflared/releases", "",
		"       Alternatives: ngrok, or your own domain + reverse proxy",
		"       with TLS.", "",
		"    2. Start the tunnel in a separate terminal:",
		"         cloudflared tunnel --url http://localhost:8090",
		"       Note the printed https://<random>.trycloudflare.com URL.", "",
		"    3. Start the Icebath gateway in another terminal:",
		"         icebath gateway", "",
		"    4. Verify your local config is reachable. From a third",
		"       terminal, with the tunnel URL substituted:", "",
		"         curl 'https://YOUR-TUNNEL.trycloudflare.com/whatsapp/webhook?\\",
		fmt.Sprintf("               hub.mode=subscribe&hub.verify_token=%s&\\", verifyToken),
		"               hub.challenge=hello'", "",
		"       Expected: HTTP 200 with body 'hello'.",
		"       Also try: curl https://YOUR-TUNNEL.trycloudflare.com/health",
		"       (should return JSON with verify_token_configured: true).", "",
		"    5. Configure Meta to point at your tunnel:",
		"         App Dashboard → WhatsApp → Configuration → Edit webhook",
		"         Callback URL: <tunnel-url>/whatsapp/webhook",
		"         Verify Token: "+verifyToken,
		"         → Click 'Verify and save'",
		"         → Then 'Manage' webhook fields → subscribe to 'messages'", "",
		"    6. Add your phone to Meta's recipient list:",
		"         App Dashboard → WhatsApp → API Setup → 'To' →",
		"         'Manage phone number list'", "",
		"    7. DM the bot's test number from your phone.", "")
	wz.header("Optional: polish your bot's WhatsApp profile")
	deepLink := ""
	if effectiveWaba != "" {
		deepLink = "?waba_id=" + effectiveWaba
	}
	wz.lines("", "  WhatsApp shows a display name and profile picture for your bot",
		"  in every chat header and contact list. These are set in Meta's",
		"  Business Manager, not via this wizard — but here's where to do",
		"  it once you're up and running:", "",
		"    • Display name + profile picture:",
		"        https://business.facebook.com/wa/manage/phone-numbers/"+deepLink)
	if effectiveWaba == "" {
		fmt.Fprintln(wz.out, "        (select your WhatsApp Business Account on that page)")
	}
	wz.lines("        Display-name changes go through a ~24-48h Meta review.", "",
		"    • About, description, website, hours, business category:",
		"        Same page → click your phone number → 'Edit profile'.", "",
		"    • Verified badge (the green check):",
		"        Requires Meta's business verification process —",
		"        Business Manager → Security Center → Start Verification.", "",
		"  Docs: https://hermes-agent.nousresearch.com/docs/user-guide/",
		"        messaging/whatsapp-cloud", "")
	return 0, nil
}

// Sets up the WhatsApp Business Cloud API on a terminal, the official Meta integration
func CmdWhatsAppCloud(ctx context.Context, h string, ns map[string]any, w io.Writer, requireTTY func(command string)) (int, error) {
	requireTTY("whatsapp-cloud")
	base := cliui.Default()
	con := &cliui.Console{In: base.In, Out: w, Getenv: base.Getenv, StdinTTY: base.StdinTTY, StdoutTTY: base.StdoutTTY,
		TermSize: base.TermSize, MaskedSecretPrompt: base.MaskedSecretPrompt, OpenScreen: base.OpenScreen}
	return (&cloudWizard{ctx: ctx, home: h, console: con, out: w}).runWhatsAppCloudSetup()
}
