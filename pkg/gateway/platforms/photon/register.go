package photon

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysys"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Whether project credentials are set in the extra, the env or auth.json
func validateConfig(ctx context.Context, cfg *gateway.PlatformConfig) (bool, error) {
	extra := map[string]any{}
	if cfg != nil && cfg.Extra != nil {
		extra = cfg.Extra
	}
	secret := func(name string) string { v, _ := gateway.GetScopedSecret(ctx, name, "", false); return v }
	if (pyrepr.Truthy(extra["project_id"]) || secret("PHOTON_PROJECT_ID") != "") &&
		(pyrepr.Truthy(extra["project_secret"]) || secret("PHOTON_PROJECT_SECRET") != "") {
		return true, nil
	}
	id, sec, err := LoadProjectCredentials(ctx)
	return id != "" && sec != "", err
}

// Extra seeded from stored credentials so env only setups show in gateway status, nil without credentials
func envEnablement(ctx context.Context) map[string]any {
	id, sec, err := LoadProjectCredentials(ctx)
	if err != nil {
		panic(err)
	}
	if id == "" || sec == "" {
		return nil
	}
	seed, err := gateway.SeedExtraFromEnv(ctx, nil, "PHOTON_HOME_CHANNEL", "")
	if err != nil {
		panic(err)
	}
	out := map[string]any{"project_id": id, "project_secret": sec}
	for k, v := range seed {
		out[k] = v
	}
	return out
}

// Model facing description of iMessage formatting through Photon
const platformHint = "You are texting via iMessage (Photon). Write like a person texting: short and conversational, " +
	"answer first, no preamble or recap. Markdown mostly does not survive here: any message containing " +
	"a link is sent as plain text with all formatting stripped, headings flatten to bold, tables to " +
	"pipe-separated lines, and backtick or code-block text turns into Unicode look-alike " +
	"glyphs that break when copied. So no headers, tables, code fences or backticks; an occasional " +
	"**bold** word is fine in a message without links. Put a command or code snippet on its own line " +
	"as plain text so it copies and runs. Write links as bare URLs; a message that is only a URL " +
	"sends as a rich preview card. You can send files natively: write MEDIA:/absolute/path/to/file " +
	"in your response (images and video appear inline, audio as voice notes, other files as " +
	"attachments). Recipient identifiers are E.164 phone numbers; never expose them in responses " +
	"unless the user asked."

// Options of photon setup
type setupOptions struct {
	projectName, phone, firstName, lastName, email string
	noBrowser, skipSidecarInstall                  bool
}

// Line read from a terminal prompt, empty off a TTY or on EOF
func prompt(out io.Writer, label string) string {
	if !pysys.Isatty(os.Stdin) {
		return ""
	}
	fmt.Fprint(out, label)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(out)
		return ""
	}
	return strings.TrimSpace(line)
}

// Device login that stores the dashboard token, never printing any part of it
func runDeviceLogin(ctx context.Context, opts setupOptions, out, errOut io.Writer) int {
	printCode := func(code DeviceCode) {
		uri := pyrepr.Str(code.VerificationURIComplete)
		if !pyrepr.Truthy(code.VerificationURIComplete) {
			uri = pyrepr.Str(code.VerificationURI)
		}
		fmt.Fprintf(out, "\n┌─ Photon device login ────────────────────────────────────────\n│  Open this URL:  %s\n│  Enter the code: %s\n"+
			"│  (waiting for approval — Ctrl-C to cancel)\n└──────────────────────────────────────────────────────────────\n\n", uri, pyrepr.Str(code.UserCode))
	}
	if _, err := LoginDeviceFlow(ctx, DefaultClientID, !opts.noBrowser, printCode); err != nil {
		fmt.Fprintf(errOut, "login failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "✓ logged in — token saved to %s\n", authJSONPath(ctx))
	return 0
}

// Valid dashboard token, reused or obtained through device login, empty on failure
func setupToken(ctx context.Context, opts setupOptions, out, errOut io.Writer) string {
	token, err := LoadPhotonToken(ctx)
	if err == nil && token != "" {
		fmt.Fprintln(out, "[1/5] Checking existing Photon token...")
		if ok, err := CheckPhotonTokenValid(ctx, token); err == nil && ok {
			fmt.Fprintln(out, "  ✓ token is valid")
			fmt.Fprintln(out, "[1/5] Reusing existing Photon token")
			return token
		}
		fmt.Fprintln(out, "  ✗ token is stale (dashboard rejected it) — re-authenticating")
		_ = ClearPhotonToken(ctx)
	}
	fmt.Fprintln(out, "[1/5] No valid Photon token found — running device login...")
	if runDeviceLogin(ctx, opts, out, errOut) != 0 {
		return ""
	}
	token, _ = LoadPhotonToken(ctx)
	if token == "" {
		fmt.Fprintln(errOut, "login completed but token was not stored")
	}
	return token
}

// Dashboard project id, reused, found by name or created, empty on failure
func setupProject(ctx context.Context, token, name string, out, errOut io.Writer) string {
	dashboardID, err := LoadDashboardProjectID(ctx)
	if err == nil && dashboardID != "" {
		fmt.Fprintln(out, "[2/5] Reusing configured Photon project")
		return dashboardID
	}
	existing, err := FindProjectByName(ctx, token, name)
	if err != nil {
		fmt.Fprintf(errOut, "project setup failed: %v\n", err)
		return ""
	}
	if existing != nil && pyrepr.Truthy(existing.Vals["id"]) {
		fmt.Fprintf(out, "[2/5] Found existing project '%s'\n", name)
		return pyrepr.Str(existing.Vals["id"])
	}
	fmt.Fprintf(out, "[2/5] Creating Photon project '%s'...\n", name)
	created, err := CreateProject(ctx, token, name, DefaultLocation)
	if err != nil {
		fmt.Fprintf(errOut, "project setup failed: %v\n", err)
		return ""
	}
	fmt.Fprintln(out, "  ✓ project created")
	if created == nil || !pyrepr.Truthy(created.Vals["id"]) {
		fmt.Fprintln(errOut, "could not resolve a Photon project id")
		return ""
	}
	return pyrepr.Str(created.Vals["id"])
}

// Spectrum credentials, an existing valid secret reused so a running sidecar keeps sending, empty on failure
func setupCredentials(ctx context.Context, token, dashboardID, name string, out, errOut io.Writer) string {
	fmt.Fprintln(out, "[3/5] Provisioning Spectrum credentials...")
	existingID, existingSecret, _ := LoadProjectCredentials(ctx)
	secret := ""
	if existingID != "" && existingSecret != "" {
		if _, err := ListUsers(ctx, existingID, existingSecret, httpTimeout); err == nil {
			secret = existingSecret
		}
	}
	reused := secret != ""
	if !reused {
		var err error
		if secret, err = RegenerateProjectSecret(ctx, token, dashboardID); err != nil {
			fmt.Fprintf(errOut, "spectrum provisioning failed: %v\n", err)
			return ""
		}
	}
	if err := StoreProjectCredentials(ctx, dashboardID, secret, dashboardID, name); err != nil {
		fmt.Fprintf(errOut, "spectrum provisioning failed: %v\n", err)
		return ""
	}
	if reused {
		fmt.Fprintf(out, "  ✓ Spectrum ready (project id %s) — existing credentials valid\n", dashboardID)
	} else {
		fmt.Fprintf(out, "  ✓ Spectrum ready (project id %s) — new secret saved\n", dashboardID)
		fmt.Fprintln(out, "  ⚠ Project secret was regenerated. If the gateway is running, restart it so the sidecar picks up the new secret:\n      icebath gateway restart")
	}
	return secret
}

// Dedicated iMessage line of the project, empty when none can be fetched
func lookupIMessageLine(ctx context.Context, token, dashboardID string, errOut io.Writer) string {
	line, err := GetIMessageLine(ctx, token, dashboardID, false)
	if err != nil {
		fmt.Fprintf(errOut, "      (could not fetch the assigned line: %v)\n", err)
		return ""
	}
	if obj, ok := line.(*pyjson.Object); ok && pyrepr.Truthy(obj.Vals["phoneNumber"]) {
		return pyrepr.Str(obj.Vals["phoneNumber"])
	}
	return ""
}

// Seconds setup waits for the operator's opt in text to land
const optInWaitS = 300

// Opted in user row after a bounded wait on a TTY, nil when skipped or timed out
func waitForOptIn(ctx context.Context, dashboardID, secret, phone, agentNumber string, out io.Writer) *pyjson.Object {
	lineHint := agentNumber
	if lineHint == "" {
		lineHint = "the line Photon assigns to your number"
	}
	fmt.Fprintln(out, "\n      One manual step: opt in by texting your agent")
	fmt.Fprintf(out, "      Your number only enters iMessage routing after it has messaged the line\n      Photon assigned to it:\n        1. Open Messages on %s.\n        2. Text %s — one message of any content is enough.\n", phone, lineHint)
	if !pysys.Isatty(os.Stdin) {
		fmt.Fprintln(out, "      Non-interactive run — skipping the opt-in wait. Verify later with `icebath photon status`.")
		return nil
	}
	fmt.Fprintf(out, "      Setup waits here (up to %d minutes) and continues once the opt-in lands.\n      (Ctrl-C to skip — re-running setup resumes where it left off.)\n", optInWaitS/60)
	onWaiting := func(attempt int) {
		if attempt%6 == 0 {
			fmt.Fprintln(out, "      ... still waiting for your message to land")
		}
	}
	opted, err := WaitForOptedInUser(ctx, dashboardID, secret, phone, optInWaitS, 5, onWaiting, nil, nil)
	why := fmt.Sprintf("No opt-in seen within %d minutes.", optInWaitS/60)
	if ctx.Err() != nil {
		opted, why = nil, "Wait skipped (Ctrl-C)."
		fmt.Fprintln(out)
	} else if err != nil {
		opted, why = nil, err.Error()
	}
	if opted == nil {
		fmt.Fprintf(out, "      ⚠ %s\n        %s is not opted in yet: text %s once from it, then re-run\n        `icebath photon setup` (it resumes where it left off).\n", why, phone, lineHint)
		return nil
	}
	fmt.Fprintln(out, "  ✓ opt-in landed — routing is live")
	return opted
}

// Error class of an error carrying a Python type name
type pyTyped interface{ PyTypeName() string }

// First time setup, device login, project, credentials, phone, sidecar deps and platform enablement
func cmdSetup(ctx context.Context, opts setupOptions, out, errOut io.Writer) int {
	name := opts.projectName
	if name == "" {
		name = DefaultProjectName
	}
	token := setupToken(ctx, opts, out, errOut)
	if token == "" {
		return 1
	}
	dashboardID := setupProject(ctx, token, name, out, errOut)
	if dashboardID == "" {
		return 1
	}
	secret := setupCredentials(ctx, token, dashboardID, name, out, errOut)
	if secret == "" {
		return 1
	}
	phone := opts.phone
	if phone == "" {
		phone = prompt(out, "[4/5] Your iMessage phone number (E.164, e.g. +15551234567): ")
	}
	var agentNumber, registeredPhone, registeredUserID string
	routingLive := false
	if phone == "" {
		fmt.Fprintln(out, "      Skipped user registration (no phone given). Re-run with --phone later.")
	} else {
		user, created, err := RegisterUserIfAbsent(ctx, dashboardID, secret, phone, UserFields{FirstName: opts.firstName, LastName: opts.lastName, Email: opts.email})
		if err != nil {
			if typed, ok := err.(pyTyped); ok && typed.PyTypeName() == "ValueError" {
				fmt.Fprintf(errOut, "      invalid phone number: %v\n", err)
			} else {
				fmt.Fprintf(errOut, "      user registration failed: %v\n", err)
			}
			return 1
		}
		if created {
			fmt.Fprintln(out, "  ✓ phone registered")
		} else {
			fmt.Fprintln(out, "  ✓ phone already registered")
		}
		registeredPhone = phone
		if user != nil && pyrepr.Truthy(user.Vals["id"]) {
			registeredUserID = pyrepr.Str(user.Vals["id"])
		}
		agentNumber = UserAssignedLine(user)
		if agentNumber == "" {
			agentNumber = lookupIMessageLine(ctx, token, dashboardID, errOut)
		}
		switch {
		case UserOptedIn(user):
			routingLive = true
			fmt.Fprintln(out, "  ✓ number is opted in — routing is live")
		case agentNumber == "":
			fmt.Fprintf(out, "      ⚠ No iMessage line is assigned to %s yet — skipping the opt-in wait.\n        Once the Photon dashboard shows your line, text it once from %s,\n        then check with `icebath photon status`.\n", phone, phone)
		default:
			opted := waitForOptIn(ctx, dashboardID, secret, phone, agentNumber, out)
			routingLive = opted != nil
			if line := UserAssignedLine(opted); line != "" {
				agentNumber = line
			}
		}
		autoconfigureAccess(ctx, phone, out, errOut)
	}
	if agentNumber == "" && phone == "" {
		agentNumber = lookupIMessageLine(ctx, token, dashboardID, errOut)
	}
	if agentNumber != "" {
		fmt.Fprintf(out, "\n┌─ Your agent's iMessage number ───────────────────────────────\n│  📱 %s\n│  Text this number from your phone to talk to your agent.\n└──────────────────────────────────────────────────────────────\n", agentNumber)
	} else {
		fmt.Fprintln(out, "      No iMessage line assigned yet — check the Photon dashboard.")
	}
	if registeredPhone != "" {
		if err := StoreUserNumbers(ctx, registeredPhone, agentNumber, registeredUserID, dashboardID); err != nil {
			fmt.Fprintf(errOut, "      (could not save Photon status metadata: %v)\n", err)
		}
	}
	if opts.skipSidecarInstall {
		fmt.Fprintln(out, "[5/5] Skipping sidecar npm install (--skip-sidecar-install)")
	} else {
		fmt.Fprintln(out, "[5/5] Installing Node sidecar deps (spectrum-ts)...")
		if rc := installSidecar(ctx, out, errOut); rc != 0 {
			return rc
		}
	}
	if err := config.WritePlatformConfigField(ctx, home.Dir(ctx), "photon", "enabled", true, true); err != nil {
		fmt.Fprintf(errOut, "      (could not enable Photon in config: %v)\n", err)
	} else {
		fmt.Fprintln(out, "  ✓ photon platform enabled in config.yaml")
	}
	if phone == "" {
		fmt.Fprintln(out, "\n⚠ Photon setup saved, but iMessage routing is NOT live: no phone number is registered.\n  Finish with:  icebath photon setup --phone +15551234567")
		return 0
	}
	if !routingLive {
		target := agentNumber
		if target == "" {
			target = "your assigned line"
		}
		fmt.Fprintf(out, "\n⚠ Photon setup saved, but iMessage routing is NOT live: text %s once from %s.\n  Check with `icebath photon status`; start the gateway once it shows opted in.\n", target, phone)
		return 0
	}
	fmt.Fprintln(out, "\n✓ Photon setup complete.\n  Start the gateway:  icebath gateway start")
	return 0
}

// Whether PHOTON_TELEMETRY in the env or the home .env reads as on
func telemetryEnabled(ctx context.Context) bool {
	raw, _, err := config.GetEnvValue(ctx, home.Dir(ctx), "PHOTON_TELEMETRY")
	if err != nil {
		raw = os.Getenv("PHOTON_TELEMETRY")
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Credential, node, sidecar, telemetry and routing status
func cmdStatus(ctx context.Context, out, errOut io.Writer) int {
	phone, assigned, _ := LoadUserNumbers(ctx)
	if phone == "" || assigned == "" {
		if id, sec, _ := LoadProjectCredentials(ctx); id != "" && sec != "" {
			if _, _, err := RefreshUserNumbers(ctx, id, sec); err != nil {
				fmt.Fprintf(errOut, "      (could not refresh Photon user numbers: %v)\n", err)
			}
		}
	}
	emit := func(line string) { fmt.Fprintln(out, line) }
	if err := PrintCredentialSummary(ctx, emit); err != nil {
		fmt.Fprintln(errOut, err)
	}
	node := managedBinary(ctx, "node")
	if node == "" {
		node = "✗ missing (install Node 18+)"
	}
	fmt.Fprintf(out, "  node binary         : %s\n", node)
	deps := "✗ run `icebath photon install-sidecar`"
	if sidecarDepsInstalled(ctx) {
		deps = "✓ installed"
	}
	fmt.Fprintf(out, "  sidecar deps        : %s\n", deps)
	state := "off"
	if telemetryEnabled(ctx) {
		state = "on"
	}
	fmt.Fprintf(out, "  telemetry           : %s (`icebath photon telemetry on|off`)\n", state)
	if err := PrintRoutingStatus(ctx, emit); err != nil {
		fmt.Fprintln(errOut, err)
	}
	return 0
}

// Shows or toggles Spectrum telemetry for the sidecar
func cmdTelemetry(ctx context.Context, state string, out, errOut io.Writer) int {
	if state == "" {
		shown := "off"
		if telemetryEnabled(ctx) {
			shown = "on"
		}
		fmt.Fprintf(out, "Photon telemetry: %s\n  Toggle with `icebath photon telemetry on` / `icebath photon telemetry off`.\n", shown)
		return 0
	}
	value := "false"
	if state == "on" {
		value = "true"
	}
	if err := config.SaveEnvValueContext(ctx, home.Dir(ctx), "PHOTON_TELEMETRY", value); err != nil {
		fmt.Fprintf(errOut, "could not save PHOTON_TELEMETRY: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "✓ Spectrum telemetry turned %s (PHOTON_TELEMETRY in ~/.icebath/.env)\n  Restart the gateway for the sidecar to pick it up:  icebath gateway restart\n", state)
	return 0
}

// npm ci in the sidecar dir with npm install as the fallback, the bounded stderr persisted for status
func installSidecar(ctx context.Context, out, errOut io.Writer) int {
	npm, env, err := ensureManagedBinary(ctx, "npm", true)
	if err != nil {
		fmt.Fprintf(errOut, "Could not prepare Photon dependencies: %v\n", err)
		return 1
	}
	dir := SidecarDir(ctx)
	fmt.Fprintf(out, "  $ cd %s && %s ci\n", dir, npm)
	run := func(verb string) (int, string) {
		cmd := exec.CommandContext(ctx, npm, verb)
		cmd.Dir, cmd.Env = dir, envList(env)
		var stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = out, &stderr
		runErr := cmd.Run()
		if stderr.Len() > 0 {
			fmt.Fprint(errOut, stderr.String())
		}
		if cmd.ProcessState == nil {
			if runErr != nil {
				return 1, runErr.Error()
			}
			return 1, stderr.String()
		}
		return cmd.ProcessState.ExitCode(), stderr.String()
	}
	code, stderr := run("ci")
	if code != 0 {
		fmt.Fprintf(out, "  npm ci failed — falling back to:  %s install\n", npm)
		code, stderr = run("install")
	}
	errText := pystr.Slice(strings.TrimSpace(stderr), 0, NpmErrorLogMaxChars)
	if code != 0 {
		fmt.Fprintln(errOut, "npm install failed")
	}
	if code == 0 {
		_ = os.Remove(NpmErrorLog(ctx))
	} else if errText != "" {
		_ = os.WriteFile(NpmErrorLog(ctx), []byte(errText), 0o644)
	}
	return code
}

// Exit code error a photon subcommand returns, already reported on its own output
type exitCode int

func (exitCode) Error() string { return "" }

// Subcommand result as an error, nil for success
func codeErr(code int) error {
	if code == 0 {
		return nil
	}
	return exitCode(code)
}

// Binds the setup, status, install-sidecar and telemetry subcommands onto the photon command
func setupCLI(parser any) {
	cmd, ok := parser.(*cobra.Command)
	if !ok {
		panic(fmt.Sprintf("the CLI parser is %T, expected an argument parser", parser))
	}
	ctxOf := func(c *cobra.Command) context.Context {
		if c.Context() != nil {
			return c.Context()
		}
		return context.Background()
	}
	var opts setupOptions
	setup := &cobra.Command{Use: "setup", Short: "First-time setup (device login + project + user + sidecar)", Args: cobra.NoArgs, SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			return codeErr(cmdSetup(ctxOf(c), opts, c.OutOrStdout(), c.ErrOrStderr()))
		}}
	f := setup.Flags()
	f.StringVar(&opts.projectName, "project-name", "", "Project name (default: 'Icebath Agent')")
	f.StringVar(&opts.phone, "phone", "", "Your E.164 phone number (e.g. +15551234567)")
	f.StringVar(&opts.firstName, "first-name", "", "")
	f.StringVar(&opts.lastName, "last-name", "", "")
	f.StringVar(&opts.email, "email", "", "")
	f.BoolVar(&opts.noBrowser, "no-browser", false, "Don't try to open a browser for device login; print the URL only")
	f.BoolVar(&opts.skipSidecarInstall, "skip-sidecar-install", false, "Skip `npm install` inside the sidecar directory")
	status := &cobra.Command{Use: "status", Short: "Show login + project + sidecar dep state", Args: cobra.NoArgs, SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			return codeErr(cmdStatus(ctxOf(c), c.OutOrStdout(), c.ErrOrStderr()))
		}}
	install := &cobra.Command{Use: "install-sidecar", Short: "Run npm install inside the sidecar directory", Args: cobra.NoArgs, SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			return codeErr(installSidecar(ctxOf(c), c.OutOrStdout(), c.ErrOrStderr()))
		}}
	telemetry := &cobra.Command{Use: "telemetry [on|off]", Short: "Show or toggle Spectrum SDK telemetry (on/off)", Args: cobra.MaximumNArgs(1),
		ValidArgs: []string{"on", "off"}, SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			state := ""
			if len(args) == 1 {
				if state = args[0]; state != "on" && state != "off" {
					return fmt.Errorf("argument state: invalid choice: %s (choose from 'on', 'off')", pyrepr.Repr(state))
				}
			}
			return codeErr(cmdTelemetry(ctxOf(c), state, c.OutOrStdout(), c.ErrOrStderr()))
		}}
	cmd.AddCommand(setup, status, install, telemetry)
	cmd.RunE = func(c *cobra.Command, _ []string) error {
		return codeErr(cmdStatus(ctxOf(c), c.OutOrStdout(), c.ErrOrStderr()))
	}
}

// Photon setup from the unified gateway setup, the phone prompted on a TTY
func gatewaySetup(ctx context.Context) error {
	return codeErr(cmdSetup(ctx, setupOptions{}, os.Stdout, os.Stderr))
}

// Photon platform plugin compiled into the binary under the bundled platforms/photon key
type photonPlatformPlugin struct{}

func (photonPlatformPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "photon-platform", Version: "0.3.0", Kind: "platform", Source: "bundled", Key: "platforms/photon",
		Description: "Photon Spectrum gateway adapter for Icebath Agent: iMessage through Photon's managed Spectrum platform over a supervised Node sidecar running spectrum-ts.\n",
		RequiresEnv: []plugins.EnvSpec{
			{Name: "PHOTON_PROJECT_ID", Description: "Spectrum project id (the project's spectrumProjectId; set by `icebath photon setup`)", URL: "https://app.photon.codes/"},
			{Name: "PHOTON_PROJECT_SECRET", Description: "Project secret paired with the Spectrum project id (set by `icebath photon setup`)", URL: "https://app.photon.codes/", Secret: true},
		}}
}

// Adapter class the gateway reads the serial receive loop trait from
var photonAdapterClass = &gateway.AdapterClass{Name: "PhotonAdapter", Traits: photonAdapterTraits(),
	New: func(ctx context.Context, cfg *gateway.PlatformConfig) (gateway.PlatformAdapter, error) {
		return NewPhotonAdapter(ctx, cfg)
	}}

// Registers the Photon platform entry and the photon command
func (photonPlatformPlugin) Register(ctx *plugins.Context) error {
	_, err := ctx.RegisterPlatform(plugins.PlatformSpec{
		Name: "photon", Label: "iMessage via Photon",
		AdapterFactory: gateway.AdapterFactory(func(fctx context.Context, cfg *gateway.PlatformConfig) (any, error) {
			return NewPhotonAdapter(fctx, cfg)
		}),
		Check:       func() bool { return checkRequirements(context.Background()) },
		RequiredEnv: []string{"PHOTON_PROJECT_ID", "PHOTON_PROJECT_SECRET"},
		InstallHint: "Run: icebath photon setup  (logs in via device flow, creates a Spectrum project, links your phone number, installs the spectrum-ts sidecar).",
		Extra: map[string]any{
			"target_ref_syntax_fn":   gateway.RegexStrippedParser(sendDMGUIDPat),
			"scoped_check_fn":        checkRequirements,
			"scoped_validate_config": validateConfig,
			"is_connected":           validateConfig,
			"setup_fn":               gatewaySetup,
			"env_enablement_fn":      envEnablement,
			"cron_deliver_env_var":   "PHOTON_HOME_CHANNEL",
			"standalone_sender_fn":   standaloneSend,
			"allowed_users_env":      "PHOTON_ALLOWED_USERS",
			"allow_all_env":          "PHOTON_ALLOW_ALL_USERS",
			"max_message_length":     maxMessageLength,
			"emoji":                  "📱",
			"pii_safe":               true,
			"allow_update_command":   true,
			"platform_hint":          platformHint,
			"display_tier":           "low",
		},
	})
	if err != nil {
		return err
	}
	ctx.RegisterCLICommand("photon", "Set up and manage the Photon iMessage integration", setupCLI, nil,
		"Photon iMessage: device login, project and phone setup, sidecar dependencies, telemetry and status.")
	return nil
}

func init() {
	gateway.RegisterAdapterModule(photonAdapterClass)
	plugins.RegisterBundled(photonPlatformPlugin{})
}
