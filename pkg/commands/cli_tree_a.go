package commands

import (
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/pkg/home"
)

func bf(cmd *cobra.Command, name, short, help string) { cmd.Flags().BoolP(name, short, false, help) }
func sf(cmd *cobra.Command, name, short, def, help string) {
	cmd.Flags().StringP(name, short, def, help)
}
func inf(cmd *cobra.Command, name, short string, def int, help string) {
	cmd.Flags().IntP(name, short, def, help)
}
func ff(cmd *cobra.Command, name, short string, def float64, help string) {
	cmd.Flags().Float64P(name, short, def, help)
}
func af(cmd *cobra.Command, name, short, help string) {
	cmd.Flags().StringArrayP(name, short, nil, help)
}
func cf(cmd *cobra.Command, name, short, help string) { cmd.Flags().CountP(name, short, help) }
func sliceF(cmd *cobra.Command, name, help string)    { cmd.Flags().StringSlice(name, nil, help) }

func acceptHooks(cmd *cobra.Command) {
	bf(cmd, "accept-hooks", "", "Auto-approve unseen shell hooks without a TTY prompt (equivalent to ICEBATH_ACCEPT_HOOKS=1 / hooks_auto_accept: true).")
	suppress(cmd, "accept-hooks")
}
func yesFlag(cmd *cobra.Command, help string) {
	if help == "" {
		help = "Skip confirmation prompt"
	}
	bf(cmd, "yes", "y", help)
}
func jsonFlag(cmd *cobra.Command, help string) { bf(cmd, "json", "", help) }

// ImportSource is one agent whose setup the migrate command can import
// ImportSource is one importer the migrate command offers, the plugins table
type ImportSource = provider.ImportSource

// ImportSources lists the importers the migrate command offers
var ImportSources = provider.ImportSources

const forceServedProfileHelp = "Start a separate gateway for this profile even when the default multiplexer already serves it (not recommended: two pollers on one bot token, port conflicts)"

func interruptHelp(verb string) string {
	return verb + " without asking even while sessions are live (turns, cron jobs, background delegations): they are interrupted. Without it the command names the live work and asks y/N (refuses when there is no terminal). --interrupt is the same flag"
}

func nousLoginFlags(cmd *cobra.Command, suffix string) {
	sf(cmd, "portal-url", "", "", "Portal base URL"+suffix+" (default: production portal)")
	sf(cmd, "inference-url", "", "", "Inference API base URL"+suffix+" (default: production inference API)")
	sf(cmd, "client-id", "", "", "OAuth client id to use"+suffix+" (default: icebath-cli)")
	sf(cmd, "scope", "", "", "OAuth scope to request"+suffix)
	bf(cmd, "no-browser", "", "Do not attempt to open the browser automatically"+ternary(suffix != "", " during Nous login", ""))
	ff(cmd, "timeout", "", 15.0, "HTTP request timeout in seconds"+suffix+" (default: 15)")
	sf(cmd, "ca-bundle", "", "", "Path to CA bundle PEM file for"+ternary(suffix != "", " Nous", "")+" TLS verification")
	bf(cmd, "insecure", "", "Disable TLS verification"+suffix+" (testing only)")
}

func ternary(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

func (c *CLI) addTreeA(root *cobra.Command) {
	model := c.newCmd(root, "model", "Select default model and provider", "Interactively select your inference provider and default model")
	bf(model, "refresh", "", "Wipe the model picker disk cache and re-fetch every provider's live /v1/models list.")
	nousLoginFlags(model, " for Nous login")

	moa := c.newCmd(root, "moa", "Configure Mixture of Agents provider/model slots", "Configure the provider/model set used by /moa <prompt>.")
	c.newCmd(moa, "list", "Show current MoA model slots", "", "ls")
	moaCfg := c.newCmd(moa, "configure", "Interactively pick MoA models", "", "config")
	c.pos(moaCfg, pOpt("name"))
	moaDel := c.newCmd(moa, "delete", "Delete a MoA preset", "", "rm")
	c.pos(moaDel, p("name"))

	fb := c.newCmd(root, "fallback", "Manage fallback providers (tried when the primary model fails)",
		"Manage the fallback provider chain.  Fallback providers are tried in order when the primary model fails with rate-limit, overload, or connection errors.  See: https://hermes-agent.nousresearch.com/docs/user-guide/features/fallback-providers")
	c.newCmd(fb, "list", "Show the current fallback chain (default when no subcommand)", "", "ls")
	c.newCmd(fb, "add", "Pick a provider + model (same picker as `icebath model`) and append to the chain", "")
	c.newCmd(fb, "remove", "Pick an entry to delete from the chain", "", "rm")
	c.newCmd(fb, "clear", "Remove all fallback entries", "")

	wt := c.newCmd(root, "worktree", "Audit and reclaim accumulated git worktrees and merged branches",
		"Audit, reclaim and restore the .worktrees/ trees Icebath sessions accumulate. Idle trees with uncommitted changes or unpushed commits are archived to refs/icebath/archive/<tree> before removal (worktrees.archive_dirty); `restore` brings one back. In-use trees are never touched. See: https://hermes-agent.nousresearch.com/docs/user-guide/git-worktrees")
	wtList := c.newCmd(wt, "list", "Classify every tree: age, size, verdict, reason (default action)", "", "ls", "audit")
	sf(wtList, "repo", "", "", "Repo root (default: current repo)")
	bf(wtList, "json", "", "Machine-readable audit output (trees, external trees, branches)")
	ff(wtList, "older-than", "", 0, "Treat reapable trees younger than DAYS as keep")
	wtPrune := c.newCmd(wt, "prune", "Remove safe trees and delete fully-merged local branches", "")
	sf(wtPrune, "repo", "", "", "Repo root (default: current repo)")
	bf(wtPrune, "json", "", "Machine-readable result (actions taken/planned, preserved trees)")
	ff(wtPrune, "older-than", "", 0, "Only reap trees idle for at least DAYS days (safety gates still apply)")
	bf(wtPrune, "dry-run", "", "Show the plan without changing anything")
	bf(wtPrune, "trees-only", "", "Only remove worktrees; leave local branches alone")
	bf(wtPrune, "branches-only", "", "Only delete merged local branches; leave worktrees alone")
	wtRestore := c.newCmd(wt, "restore", "Recreate an archived worktree (tracked, staged and untracked files)", "")
	c.pos(wtRestore, p("name"))
	sf(wtRestore, "path", "", "", "Restore here instead of the original path")
	sf(wtRestore, "repo", "", "", "Repo root (default: current repo)")

	browser := c.helpCmd(root, "browser", "Real-profile browsing helpers (close a browser locking its profile)",
		"Helpers for real-profile browsing (browser.use_real_profile). close-profile terminates the browser process tree holding your default profile so Icebath can copy it — DESTRUCTIVE (unsaved tabs in that browser are lost). The agent runs this only after you approve closing the browser.", 2)
	bClose := c.newCmd(browser, "close-profile", "Close the browser locking your real profile (asks nothing — run only with the user's explicit OK; loses unsaved tabs)", "")
	sf(bClose, "browser", "", "", "Override detected default browser (chrome/edge/brave/brave-origin/chromium)")

	secrets := c.helpCmd(root, "secrets", "Manage external secret sources (Bitwarden, 1Password)",
		"Pull API keys from an external secret manager at process startup instead of storing them in ~/.icebath/.env.  Supports Bitwarden Secrets Manager and 1Password.  See: https://hermes-agent.nousresearch.com/docs/user-guide/secrets/", 0)
	bw := c.helpCmd(secrets, "bitwarden", "Bitwarden Secrets Manager integration", "", 0, "bw")
	bwSetup := c.newCmd(bw, "setup", "Interactive wizard: install bws, store access token, pick project", "")
	sf(bwSetup, "project-id", "", "", "Pre-select a project UUID instead of prompting")
	sf(bwSetup, "access-token", "", "", "Provide the access token non-interactively (will be stored in .env)")
	sf(bwSetup, "server-url", "", "", "Bitwarden region / self-hosted endpoint. Examples: https://vault.bitwarden.com (US, default), https://vault.bitwarden.eu (EU), or your self-hosted URL. Skips the interactive region prompt.")
	c.newCmd(bw, "status", "Show config + binary + token validation status", "")
	bwToken := c.newCmd(bw, "token", "Rotate the access token: validate a new one and store it in .env", "")
	sf(bwToken, "access-token", "", "", "Provide the new token non-interactively (default: masked prompt)")
	bf(bwToken, "no-verify", "", "Store without probing Bitwarden first (not recommended)")
	bwSync := c.newCmd(bw, "sync", "Fetch secrets now and report what changed", "")
	bf(bwSync, "apply", "", "Actually export the secrets into the current shell's env (default: dry-run)")
	c.newCmd(bw, "disable", "Turn off the Bitwarden integration", "")
	bwInstall := c.newCmd(bw, "install", "Install the PM-pinned bws binary", "")
	bf(bwInstall, "force", "", "Verify and repair the managed copy")
	op := c.helpCmd(secrets, "onepassword", "1Password (op:// references) integration", "", 0, "op", "1password")
	opSetup := c.newCmd(op, "setup", "Verify the op CLI, set account / token env var, and enable", "")
	sf(opSetup, "account", "", "", "1Password account shorthand or sign-in address (op --account)")
	sf(opSetup, "token-env", "", "", "Env var holding a service-account token (default OP_SERVICE_ACCOUNT_TOKEN)")
	sf(opSetup, "token", "", "", "Service-account token to store in .env non-interactively")
	sf(opSetup, "binary-path", "", "", "Absolute path to the op binary (skips PATH lookup)")
	c.newCmd(op, "status", "Show config + op binary + references", "")
	opToken := c.newCmd(op, "token", "Rotate the service-account token: validate and store it in .env", "")
	sf(opToken, "token", "", "", "Provide the new token non-interactively (default: masked prompt)")
	bf(opToken, "no-verify", "", "Store without probing 1Password first (not recommended)")
	opSet := c.newCmd(op, "set", "Map an env var to an op:// reference", "")
	c.pos(opSet, p("env_var"), p("reference"))
	opRemove := c.newCmd(op, "remove", "Remove an env-var → reference mapping", "")
	c.pos(opRemove, p("env_var"))
	opSync := c.newCmd(op, "sync", "Resolve references now and report what changed", "")
	bf(opSync, "apply", "", "Actually export resolved values into the current shell (default: dry-run)")
	c.newCmd(op, "disable", "Turn off the 1Password integration", "")

	egress := c.helpCmd(root, "egress", "Manage the iron-proxy egress credential-injection firewall",
		"Manage iron-proxy, the optional TLS-intercepting egress firewall that swaps proxy tokens for real API credentials before outbound requests leave a sandbox.  Disabled by default.  See: https://hermes-agent.nousresearch.com/docs/user-guide/egress/iron-proxy", 0)
	egInstall := c.newCmd(egress, "install", "Install the PM-pinned iron-proxy binary", "")
	bf(egInstall, "force", "", "Verify and repair the managed copy")
	egSetup := c.newCmd(egress, "setup", "Interactive wizard: install + CA + mint tokens + write config", "")
	inf(egSetup, "tunnel-port", "", 0, "Override the tunnel port (default 9090)")
	bf(egSetup, "from-bitwarden", "", "Treat secrets as managed by Bitwarden — discover provider keys from secrets.bitwarden config instead of the current env.  Fails loudly if BW is unreachable rather than silently falling back.")
	bf(egSetup, "no-bitwarden", "", "Explicitly switch credential_source back to env on re-setup (only meaningful when the previous setup used --from-bitwarden).")
	bf(egSetup, "rotate-tokens", "", "Mint fresh proxy tokens for every provider (default is to preserve tokens for providers that already had one — avoids 401-ing already-running sandboxes on re-setup).")
	bf(egSetup, "restart", "", "If a daemon is already running, restart it automatically after writing the new config/tokens (non-interactive default on a tty is to ask).")
	bf(egSetup, "no-restart", "", "Do not restart a running daemon after setup; you'll need to run `icebath egress restart` yourself for changes to take effect.")
	egSetup.MarkFlagsMutuallyExclusive("restart", "no-restart")
	c.newCmd(egress, "start", "Start the managed iron-proxy", "")
	c.newCmd(egress, "stop", "Stop the managed iron-proxy", "")
	c.newCmd(egress, "restart", "Restart the managed iron-proxy (stop if running, then start)", "")
	c.newCmd(egress, "reload", "Hot-reload the running daemon's ruleset from proxy.yaml (management API — no restart, no dropped connections)", "")
	egStatus := c.newCmd(egress, "status", "Show proxy state and mappings", "")
	bf(egStatus, "show-tokens", "", "Print the proxy tokens (default: redacted prefix only). Beware: tokens may persist in your shell history.")
	c.newCmd(egress, "disable", "Turn off the proxy integration", "")
	c.newCmd(egress, "config", "Print the generated proxy.yaml path", "")

	migrate := c.newCmd(root, "migrate", "Move over from another agent, or migrate retired models and deprecated settings",
		"With no type: find another agent's setup on this computer (OpenClaw, Claude Code, Codex CLI, Cursor, Grok Bot, OpenBot) and offer to import it. Name an agent to import from it; xai and relay rewrite deprecated settings in the active config.yaml.")
	mXai := c.newCmd(migrate, "xai", "Migrate xAI models scheduled for retirement on May 15, 2026",
		"Scan config.yaml for references to xAI models retiring on May 15, 2026 and, with --apply, rewrite them in-place to the official replacements per the xAI migration guide. The original config.yaml is backed up before any rewrite.")
	bf(mXai, "apply", "", "Rewrite config.yaml in-place (default: dry-run, no writes)")
	bf(mXai, "no-backup", "", "Skip the timestamped backup of config.yaml when applying")
	mRelay := c.newCmd(migrate, "relay", "Convert legacy ICEBATH_NEMO_RELAY_ATIF_*/ATOF_* exporter vars into relay-plugins.toml",
		"The NeMo Relay cutover stopped reading the legacy exporter variables. Generate <icebath home>/relay-plugins.toml from them, point ICEBATH_NEMO_RELAY_PLUGINS_TOML at it, and comment the legacy lines out. `icebath update` runs this for every profile automatically.")
	bf(mRelay, "all-profiles", "", "Migrate the default home and every named profile (what `icebath update` does)")
	bf(mRelay, "no-validate", "", "Skip activating the generated file through Relay's validator before writing it")
	for _, src := range ImportSources {
		sub := c.newCmd(migrate, src.Name, "Import your "+src.Label+" setup into Icebath",
			"Import your "+src.Label+" setup into the active profile through the agent-importers plugin (installed from the plugin catalog when missing). Always previews first and asks before changing anything.")
		sf(sub, "source", "", "", "Where the agent's setup (or, for Grok Bot, its export file) is")
		bf(sub, "dry-run", "", "Preview only")
		bf(sub, "overwrite", "", "Replace existing items on name conflicts")
		bf(sub, "yes", "y", "Skip the confirmation prompt")
		if src.Name == "openclaw" {
			bf(sub, "migrate-secrets", "", "Also import OpenClaw's allowlisted API keys and bot tokens")
		}
		if src.Name == "grok-bot" || src.Name == "openbot" {
			bf(sub, "activate-routines", "", "Create the imported cron jobs running instead of paused")
			af(sub, "bot", "", "Only this Bot or agent, by name (repeatable)")
		}
	}

	codex := c.helpCmd(root, "codex-runtime", "Manage the optional codex app-server runtime (migrate MCP config)",
		"Noninteractive counterpart of the /codex-runtime slash command. Toggling the runtime itself stays in the chat command (`/codex-runtime on|off`); `migrate` re-projects Icebath' mcp_servers + installed codex plugins into the managed block of ~/.codex/config.toml for the selected profile.", 0)
	cxMigrate := c.newCmd(codex, "migrate", "Regenerate the icebath-managed block in codex's config.toml",
		"Idempotent: replaces the managed block, keeps user text verbatim, skips Icebath servers whose name the user already declares outside the block, validates the result as TOML before writing atomically.")
	bf(cxMigrate, "dry-run", "", "Report what would be written without touching config.toml")
	bf(cxMigrate, "json", "", "Print the migration report as JSON (for automation)")

	c.addGateway(root)
	c.addLSP(root)

	setup := c.newCmd(root, "setup", "Interactive setup wizard", "Configure Icebath Agent with an interactive wizard. Run a specific section: icebath setup model|tts|terminal|gateway|tools|telemetry|agent")
	c.pos(setup, pOptChoice("section", "", "model", "tts", "terminal", "gateway", "tools", "telemetry", "agent").withHelp("Run a specific setup section instead of the full wizard"))
	bf(setup, "non-interactive", "", "Non-interactive mode (use defaults/env vars)")
	bf(setup, "reset", "", "Reset configuration to defaults")
	bf(setup, "reconfigure", "", "(Default on existing installs.) Re-run the full wizard, showing current values as defaults. Kept for backwards compatibility — a bare 'icebath setup' now does this.")
	bf(setup, "quick", "", "On existing installs: only prompt for items that are missing or unset, instead of running the full reconfigure wizard.")
	bf(setup, "portal", "", "One-shot Nous Portal setup: log in via OAuth, pick a Nous model, set Nous as the inference provider, and opt into the Tool Gateway. Skips the rest of the wizard.")

	c.newCmd(root, "whatsapp", "Set up WhatsApp integration", "Configure WhatsApp and pair via QR code")
	c.newCmd(root, "whatsapp-cloud", "Set up WhatsApp Business Cloud API integration",
		"Configure the official Meta WhatsApp Business Cloud API adapter (Business account required, public webhook URL required). Distinct from `icebath whatsapp` which sets up the Baileys bridge for personal accounts.")

	slack := c.newCmd(root, "slack", "Slack integration helpers (manifest generation, etc.)", "Slack integration helpers for Icebath.")
	manifest := c.newCmd(slack, "manifest", "Print or write a Slack app manifest with every gateway command registered as a native slash (/btw, /stop, /model, ...)",
		"Generate a Slack app manifest that registers every gateway command in the command registry as a first-class Slack slash command (matching Discord and Telegram parity). Paste the output into Slack app config → Features → App Manifest → Edit, then Save. Reinstall the app if Slack prompts for it.")
	sf(manifest, "write", "", "", "Write manifest to a file instead of stdout. With no PATH writes to $ICEBATH_HOME/slack-manifest.json.")
	constTrue(manifest, "write")
	sf(manifest, "name", "", "", `Bot display name (default: "Icebath")`)
	sf(manifest, "description", "", "", "Bot description shown in Slack's app directory.")
	sf(manifest, "long-description", "", "", "Set Slack's long app description (175-4,000 characters).")
	sf(manifest, "long-description-file", "", "", "Read Slack's long app description from a UTF-8 text file (175-4,000 characters).")
	manifest.MarkFlagsMutuallyExclusive("long-description", "long-description-file")
	bf(manifest, "slashes-only", "", "Emit only the features.slash_commands array (for merging into an existing manifest manually).")
	bf(manifest, "no-assistant", "", "Omit Slack AI Assistant mode (assistant_view, assistant:write scope, assistant_thread_* events). DMs then render as a flat chat where bare slash commands (/help, /new) work inline instead of Slack's Assistant thread pane.")
	bf(manifest, "agent-view", "", "Emit Slack's Agent messaging experience (agent_view, app_home_opened + message.im) instead of the legacy assistant_view experience. This changes Slack's app messaging surface and cannot be reversed in Slack after applying the manifest.")
	manifest.MarkFlagsMutuallyExclusive("no-assistant", "agent-view")

	c.addSend(root)

	login := c.newCmd(root, "login", "", "Deprecated. Use `icebath auth` to manage credentials, `icebath model` to select a provider, or `icebath setup` for full setup.")
	login.Hidden = true
	sf(login, "provider", "", "", "(deprecated) Provider name; ignored — see `icebath model`")
	nousLoginFlags(login, "")

	logout := c.newCmd(root, "logout", "Clear authentication for an inference provider", "Remove stored credentials and reset provider config")
	sf(logout, "provider", "", "", "Provider to log out from (default: active provider)")
	c.choice(logout, "provider", "nous", "openai-codex", "xai-oauth")

	auth := c.newCmd(root, "auth", "Manage pooled provider credentials", "")
	aAdd := c.newCmd(auth, "add", "Add a pooled credential", "")
	c.pos(aAdd, p("provider"))
	sf(aAdd, "type", "", "", "Credential type to add")
	c.choice(aAdd, "type", "oauth", "api-key", "api_key")
	sf(aAdd, "label", "", "", "Optional display label")
	inf(aAdd, "priority", "", -1, "Place the new credential at this priority (0 = tried first under fill_first); appends last when omitted")
	sf(aAdd, "api-key", "", "", "API key value (otherwise prompted securely)")
	sf(aAdd, "portal-url", "", "", "Nous portal base URL")
	sf(aAdd, "inference-url", "", "", "Nous inference base URL")
	sf(aAdd, "client-id", "", "", "OAuth client id")
	sf(aAdd, "scope", "", "", "OAuth scope override")
	bf(aAdd, "no-browser", "", "Do not auto-open a browser for OAuth login")
	bf(aAdd, "browser", "", "openai-codex only: sign in with the browser authorization-code (PKCE) flow on http://localhost:1455/auth/callback instead of the default device-code flow; falls back to device code when that port is busy (config: auth.codex_login_flow)")
	ff(aAdd, "timeout", "", 0, "OAuth/network timeout in seconds")
	bf(aAdd, "insecure", "", "Disable TLS verification for OAuth login")
	sf(aAdd, "ca-bundle", "", "", "Custom CA bundle for OAuth login")
	aList := c.newCmd(auth, "list", "List pooled credentials", "")
	c.pos(aList, pOpt("provider"))
	aRemove := c.newCmd(auth, "remove", "Remove a pooled credential by index, id, or label", "")
	c.pos(aRemove, p("provider"), p("target"))
	aReset := c.newCmd(auth, "reset", "Clear exhaustion status for a provider's credentials (all, or one target)", "")
	c.pos(aReset, p("provider"), pOpt("target"))
	aPriority := c.newCmd(auth, "priority", "Move a pooled credential to a priority (0 = tried first under fill_first)", "")
	c.pos(aPriority, p("provider"), p("target"), pInt("priority"))
	aRefresh := c.newCmd(auth, "refresh", "Refresh a pooled OAuth credential's tokens and clear its cooldown", "")
	c.pos(aRefresh, p("provider"), pOpt("target"))
	aStatus := c.newCmd(auth, "status", "Show auth status for a provider", "")
	c.pos(aStatus, p("provider"))
	aLogout := c.newCmd(auth, "logout", "Log out a provider and clear stored auth state", "")
	c.pos(aLogout, p("provider"))
	aUpgrade := c.newCmd(auth, "upgrade", "Sign in with a Nous account, keeping your connectors", "")
	bf(aUpgrade, "no-browser", "", "Do not auto-open a browser for sign-in")
	ff(aUpgrade, "timeout", "", 0, "Network timeout in seconds")

	status := c.newCmd(root, "status", "Show status of all components", "Display status of Icebath Agent components")
	bf(status, "full", "", "Show every section (keys redacted) instead of the one-screen summary")
	c.aliasFlag(status, "all", "full")
	bf(status, "deep", "", "Run deep checks (implies --full; may take longer)")

	pause := c.newCmd(root, "pause", "Emergency stop: pause cron/kanban dispatch and new gateway turns",
		"Engage the global emergency stop. Halts NEW work only — cron dispatch, kanban dispatch, and new gateway turns — until `icebath resume`. Interactive CLI, TUI and Desktop turns are gateway turns, so they are held too: each gets \"Icebath is paused\" with the reason. Turns already running finish; in-flight work is never killed.")
	sf(pause, "reason", "", "", "Optional reason stored in the sentinel and shown to users")
	c.newCmd(root, "resume", "Lift the emergency stop set by `icebath pause`", "Remove the ESTOP sentinel; dispatch resumes on the next tick.")

	c.addCron(root)

	webhook := c.newCmd(root, "webhook", "Manage dynamic webhook subscriptions", "Create, list, and remove webhook subscriptions for event-driven agent activation")
	whSub := c.newCmd(webhook, "subscribe", "Create a webhook subscription", "", "add")
	c.pos(whSub, p("name"))
	sf(whSub, "prompt", "", "", "Prompt template with {dot.notation} payload refs")
	sf(whSub, "events", "", "", "Comma-separated event types to accept")
	sf(whSub, "description", "", "", "What this subscription does")
	sf(whSub, "skills", "", "", "Comma-separated skill names to load")
	sf(whSub, "deliver", "", "log", "Delivery target: log, telegram, discord, slack, etc.")
	sf(whSub, "deliver-chat-id", "", "", "Target chat ID for cross-platform delivery")
	sf(whSub, "secret", "", "", "HMAC secret (auto-generated if omitted)")
	sf(whSub, "route-profile", "", "", "Bind the route to a multiplexed profile: only POSTs to /p/PROFILE/webhooks/<name> are accepted and the agent runs as that profile (default: default; kept on update). Changing an existing route's profile rotates its HMAC secret unless a different --secret is supplied. Distinct from the global -p/--profile, which picks the gateway whose subscriptions file is written.")
	bf(whSub, "deliver-only", "", "Skip the agent — deliver the rendered prompt directly as the message. Zero LLM cost. Requires --deliver to be a real target (not 'log').")
	bf(whSub, "mirror-to-session", "", "Also write each delivered message into the target chat's session, so replying to it in that chat has context. Only for sources whose content you trust in your conversation.")
	sf(whSub, "script", "", "", "Filter/transform script under ~/.icebath/scripts/. The route payload is passed as JSON on stdin; empty stdout, [SILENT], or a nonzero exit code ignores the webhook.")
	sf(whSub, "cron-job", "", "", "Fire an existing cron job (by ID or name) when this route receives an event, instead of starting a fresh agent run. The rendered --prompt template is passed to the job as transient per-run context; the job's own prompt, skills, and delivery settings apply. Mutually exclusive with --deliver-only.")
	c.newCmd(webhook, "list", "List all dynamic subscriptions", "", "ls")
	whRm := c.newCmd(webhook, "remove", "Remove a subscription", "", "rm")
	c.pos(whRm, p("name"))
	whTest := c.newCmd(webhook, "test", "Send a test POST to a webhook route", "")
	c.pos(whTest, p("name"))
	sf(whTest, "payload", "", "", "JSON payload to send (default: test payload)")
}

func (c *CLI) addGateway(root *cobra.Command) {
	gw := c.newCmd(root, "gateway", "Messaging gateway management", "Manage the messaging gateway (Telegram, Discord, WhatsApp, Weixin, and more)")
	subDest(gw, "gateway_command")
	acceptHooks(gw)
	run := c.newCmd(gw, "run", "Run gateway in foreground (recommended for WSL and Docker)", "")
	cf(run, "verbose", "v", "Increase stderr log verbosity (-v=INFO, -vv=DEBUG)")
	bf(run, "quiet", "q", "Suppress all stderr log output")
	bf(run, "replace", "", "Replace any existing gateway instance (useful for systemd)")
	bf(run, "force", "", "Start a foreground gateway even when a systemd/launchd/s6 service already supervises this profile. Without --force, the command refuses because a second dispatcher escapes the service and can corrupt shared gateway state.")
	bf(run, "no-supervise", "", "Inside the s6-overlay Docker image, normally `gateway run` is automatically redirected to the supervised s6 service (so the gateway gets auto-restart on crash, plus a supervised dashboard if ICEBATH_DASHBOARD is set). Pass --no-supervise to opt out and get the historical pre-s6 foreground behavior: the gateway is the container's main process and the container exits with the gateway's exit code. No effect outside an s6 container.")
	bf(run, "external-supervisor", "", "Declare that an external process manager owns this foreground gateway. In-chat restarts and updates exit back to that manager instead of spawning a detached replacement. Use this when a launchd/systemd wrapper strips its native environment markers.")
	bf(run, "idle-exit", "", "")
	hide(run, "idle-exit")
	acceptHooks(run)
	ensure := c.newCmd(gw, "ensure", "Ensure a local runtime without installing or replacing a service", "")
	c.epilog(ensure, "Exit codes: 0 ready; 2 invalid invocation; 3 incompatible; 4 authorization/profile mismatch; 5 deadline; 6 draining/update-paused; 7 inaccessible/conflicting supervisor. Pending startup is not readiness.")
	bf(ensure, "json", "", "Emit one credential-free JSON result (default)")
	sf(ensure, "timeout", "", "60", "Total startup deadline in seconds (default: 60)")
	ensure.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		cmd.Println(`{"endpoint":null,"reason_code":"invalid_invocation","state":"inaccessible"}`)
		return &ExitError{Code: 2, Msg: "gateway ensure: invalid invocation"}
	})
	systemFlag := func(cmd *cobra.Command) { bf(cmd, "system", "", "Target the Linux system-level gateway service") }
	compatPlatform := func(cmd *cobra.Command) {
		sf(cmd, "platform", "", "", "")
		hide(cmd, "platform")
	}
	start := c.newCmd(gw, "start", "Start the installed systemd/launchd background service", "")
	systemFlag(start)
	bf(start, "all", "", "Kill ALL stale gateway processes across all profiles before starting")
	bf(start, "force", "", forceServedProfileHelp)
	compatPlatform(start)
	stop := c.newCmd(gw, "stop", "Stop gateway service", "")
	systemFlag(stop)
	bf(stop, "all", "", "Stop ALL gateway processes across all profiles")
	bf(stop, "interrupt", "y", interruptHelp("stop"))
	c.aliasFlag(stop, "yes", "interrupt")
	restart := c.newCmd(gw, "restart", "Restart gateway service", "")
	systemFlag(restart)
	bf(restart, "all", "", "Kill ALL gateway processes across all profiles before restarting")
	bf(restart, "force", "", forceServedProfileHelp)
	bf(restart, "interrupt", "y", interruptHelp("restart"))
	c.aliasFlag(restart, "yes", "interrupt")
	bf(restart, "detach", "", "Without an installed service, start the replacement in the background (as `gateway ensure` does) instead of in this terminal; used by apps that run the restart as a short-lived client")
	compatPlatform(restart)
	gstatus := c.newCmd(gw, "status", "Show gateway status", "")
	bf(gstatus, "deep", "", "Deep status check")
	bf(gstatus, "full", "l", "Show full, untruncated service/log output where supported")
	systemFlag(gstatus)
	compatPlatform(gstatus)
	install := c.newCmd(gw, "install", "Install gateway as a systemd/launchd background service", "")
	bf(install, "force", "", "Force reinstall, and install even when the default multiplexer already serves this profile")
	bf(install, "system", "", "Install as a Linux system-level service (starts at boot)")
	sf(install, "run-as-user", "", "", "User account the Linux system service should run as")
	bf(install, "start-now", "", "Start the gateway service immediately after installing")
	bf(install, "no-start-now", "", "Do not start the gateway service after installing")
	bf(install, "start-on-login", "", "Enable the service to start automatically on login/boot")
	bf(install, "no-start-on-login", "", "Do not enable the service to start on login/boot")
	noneDefault(install, "start-now", "start-on-login")
	storeFalse(install, "no-start-now", "start_now")
	storeFalse(install, "no-start-on-login", "start_on_login")
	bf(install, "if-missing", "", "Do nothing when a gateway service is already installed")
	bf(install, "force-unit-path", "", "Repoint a service definition that pins another ICEBATH_HOME at this one")
	bf(install, "elevated-handoff", "", "")
	hide(install, "elevated-handoff")
	uninstall := c.newCmd(gw, "uninstall", "Uninstall gateway service", "")
	systemFlag(uninstall)
	c.newCmd(gw, "list", "List all profiles and their gateway status", "")
	c.newCmd(gw, "setup", "Configure messaging platforms", "")
	legacy := c.newCmd(gw, "migrate-legacy", "Remove legacy icebath.service units from pre-rename installs",
		"Stop, disable, and remove legacy Icebath gateway unit files (e.g. icebath.service) left over from older installs. Profile units (icebath-gateway-<profile>.service) and unrelated third-party services are never touched.")
	bf(legacy, "dry-run", "", "List what would be removed without doing it")
	bf(legacy, "yes", "y", "Skip the confirmation prompt")
	gmigrate := c.newCmd(gw, "migrate", "Converge every per-profile gateway onto the ONE host gateway",
		"Converge this host onto the one-gateway-per-host model: stop and uninstall each secondary profile's gateway and its supervisor unit (systemd, launchd, Windows Scheduled Task), then restart the default profile's gateway so it serves every profile. Runs a preflight first (duplicate bot tokens, port-binding platforms without a /p/<profile>/ ingress) and changes nothing when blocked. Safe to re-run: a half-migrated host converges on the next run. Per-profile gateways are not a supported topology any more, so there is no rollback command.")
	bf(gmigrate, "multiplex", "", "Converge onto the one host gateway (default)")
	bf(gmigrate, "dry-run", "", "Print the plan and blockers without changing anything")
	bf(gmigrate, "yes", "y", "Apply without confirmation")
	enroll := c.newCmd(gw, "enroll", "Enroll this gateway with a relay connector (writes relay auth creds to .env)",
		"Redeem a single-use enrollment token with a relay connector. Authenticates as your Nous Portal account (the connector derives the authoritative tenant from it), mints this gateway's per-gateway secret and per-tenant delivery key, and writes GATEWAY_RELAY_ID / GATEWAY_RELAY_SECRET / GATEWAY_RELAY_DELIVERY_KEY into ~/.icebath/.env. Requires being logged in (icebath setup). Not available in managed installs.")
	sf(enroll, "token", "", "", "The single-use enrollment token from the connector (delivered with your gateway config). Also settable via GATEWAY_RELAY_ENROLL_TOKEN.")
	sf(enroll, "connector-url", "", "", "The connector base/relay URL, e.g. wss://connector.example.com/relay or https://connector.example.com. Also settable via GATEWAY_RELAY_URL / gateway.relay_url in config.yaml.")
	sf(enroll, "gateway-id", "", "", "A stable id for this gateway instance (kill-switch granularity). Defaults to gw-<hostname>.")
	sf(enroll, "wake-url", "", "", "Phase 5 §5.2 wake URL: a reachable URL the connector pokes (payload-free GET) to wake this gateway when buffered work arrives while it's idle/suspended, so it reconnects and drains. Persisted as GATEWAY_RELAY_WAKE_URL in ~/.icebath/.env and forwarded at provision. Optional — without it the gateway still drains whenever it next reconnects on its own.")

	proxy := c.newCmd(root, "proxy", "Local OpenAI-compatible proxy to OAuth providers",
		"Run a local HTTP server that forwards OpenAI-compatible requests to an OAuth-authenticated provider (e.g. Nous Portal). External apps can point at the proxy with any bearer token; the proxy attaches your real credentials.")
	pStart := c.newCmd(proxy, "start", "Run the proxy in the foreground", "")
	sf(pStart, "provider", "", "nous", "Upstream provider: nous or xai (default: nous). See `icebath proxy providers`.")
	sf(pStart, "host", "", "", "Bind address (default: 127.0.0.1). Use 0.0.0.0 to expose on LAN.")
	inf(pStart, "port", "", 0, "Bind port (default: 8645)")
	c.newCmd(proxy, "status", "Show which proxy upstreams are ready", "")
	c.newCmd(proxy, "providers", "List available proxy upstream providers", "")
}

func (c *CLI) addLSP(root *cobra.Command) {
	lsp := c.newCmd(root, "lsp", "Language Server Protocol management", "Manage the LSP layer that powers post-write semantic diagnostics in write_file/patch.")
	st := c.newCmd(lsp, "status", "Show LSP service status", "")
	bf(st, "json", "", "Emit machine-readable JSON")
	ls := c.newCmd(lsp, "list", "List supported language servers", "")
	bf(ls, "installed-only", "", "Only show servers whose binary is currently available")
	inst := c.newCmd(lsp, "install", "Install a server binary", "")
	c.pos(inst, p("server"))
	all := c.newCmd(lsp, "install-all", "Install every server with a known auto-install recipe", "")
	bf(all, "include-manual", "", "Even attempt servers marked manual-install")
	c.newCmd(lsp, "restart", "Tear down running LSP clients (next edit re-spawns)", "")
	which := c.newCmd(lsp, "which", "Print binary path for a server", "")
	c.pos(which, p("server"))
}

func (c *CLI) addSend(root *cobra.Command) {
	h := home.Process()
	send := c.newCmd(root, "send", "Send a message to a configured platform (scripts, cron jobs, CI).",
		"Pipe text from any shell script to any messaging platform Icebath is already configured for. Reuses the gateway's platform credentials ("+home.EnvPath(h)+" + "+home.ConfigPath(h)+") — no LLM, no agent loop, no running gateway required for bot-token platforms like Telegram/Discord/Slack/Signal.")
	c.epilog(send, "Examples:\n  icebath send --to telegram \"deploy finished\"\n  echo \"RAM 92%\" | icebath send --to telegram:-1001234567890\n  icebath send --to discord:#ops --file ./report.md\n  icebath send --to slack:#eng --subject \"[CI]\" --file build.log\n  icebath send --to whatsapp:GROUP@g.us --mention 15551234567 \"@15551234567 hello\"\n  icebath send --to telegram \"MEDIA:./chart.png\"   # send a media attachment\n  icebath send --list                  # all platforms\n  icebath send --list telegram         # filter by platform\n\nExit codes: 0 ok, 1 delivery/backend error, 2 usage error.")
	sf(send, "to", "t", "", "Delivery target. Format: 'platform' (home channel), 'platform:chat_id', 'platform:chat_id:thread_id', or 'platform:#channel-name'. Examples: telegram, telegram:-1001234567890:17585, discord:#ops, slack:C0123ABCD, signal:+15551234567.")
	c.pos(send, pOpt("message"))
	sf(send, "file", "f", "", "Read message body from PATH (text only). Use '-' to force stdin. To send an image/document as an attachment, use MEDIA:<path> in the message text instead.")
	sf(send, "subject", "s", "", "Prepend a subject/header line before the message body.")
	af(send, "mention", "", "WhatsApp only: add a native participant mention. Repeat for multiple recipients; bare phone numbers are normalized to JIDs. Include each matching @<number> near the start of the message text.")
	bf(send, "list", "l", "List available targets. Optional positional filter: `icebath send --list telegram`.")
	bf(send, "quiet", "q", "Suppress stdout on success (exit code only).")
	bf(send, "json", "", "Emit raw JSON result instead of human-readable output.")
	dest(send, "mention", "mentions")
	dest(send, "list", "list_targets")
}

func (c *CLI) addCron(root *cobra.Command) {
	cron := c.newCmd(root, "cron", "Cron job management", "Manage scheduled tasks")
	acceptHooks(cron)
	catchUp := func(cmd *cobra.Command) {
		sf(cmd, "catch-up", "", "", "Runs missed while the machine was off: 'on' runs once on return, 'off' skips them (each is booked as a 'missed' row in `icebath cron runs`). Default: cron.catch_up_missed for recurring jobs; one-shots skip.")
		c.choice(cmd, "catch-up", "on", "off")
	}
	list := c.newCmd(cron, "list", "List scheduled jobs", "")
	bf(list, "all", "", "Include disabled and completed jobs")
	create := c.newCmd(cron, "create", "Create a scheduled job", "", "add")
	c.pos(create, p("schedule"), pOpt("prompt"))
	sf(create, "name", "", "", "Optional human-friendly job name")
	sf(create, "deliver", "", "", "Delivery target: origin, local, telegram, discord, signal, platform:chat_id, or bot-chat[:profile] (inject output into a local profile's canonical Bot Chat as a message the bot responds to)")
	sf(create, "failure-deliver", "", "", "Override target for FAILURE notices only (same grammar as --deliver). 'local' suppresses failure notices entirely; run state stays visible in `icebath cron list`. Omit = failures follow --deliver.")
	inf(create, "repeat", "", 0, "Optional repeat count")
	catchUp(create)
	af(create, "skill", "", "Attach a skill. Repeat to add multiple skills.")
	sf(create, "script", "", "", "Path to a script under ~/.icebath/scripts/. Default mode: script stdout is injected into the agent's prompt each run. With --no-agent: the script IS the job and its stdout is delivered verbatim. .sh/.bash files run via bash, everything else via Python.")
	bf(create, "no-agent", "", "Skip the LLM entirely — run --script on schedule and deliver its stdout directly. Empty stdout = silent. Classic watchdog pattern (memory alerts, disk alerts, CI pings).")
	sf(create, "monitor-script", "", "", "Monitor mode: path to a cheap source script under ~/.icebath/scripts/ that runs each tick BEFORE the agent. Unchanged output (exact-bytes hash) suppresses the agent run entirely; changed output injects a MONITOR CHANGE DETECTED diff into the prompt. Script output must be stable (no timestamps). Mutually exclusive with --monitor-url; incompatible with --no-agent.")
	sf(create, "monitor-url", "", "", "Monitor mode: http(s) URL fetched with a bounded GET each tick instead of a script. Same hash-suppression semantics as --monitor-script.")
	sf(create, "workdir", "", "", "Absolute path for the job to run from. Injects AGENTS.md / CLAUDE.md / .cursorrules from that directory and uses it as the cwd for terminal/file/code_exec tools. Omit to preserve old behaviour (no project context files).")
	sf(create, "model", "", "", "Pin this job to a specific inference model (user-owned; the agent's cronjob tool cannot set this). Omit to follow cron.model, then the main agent model (`icebath model`), at fire time.")
	bf(create, "pin", "", "Lock the CURRENT main agent model (and its provider) onto this job so later `icebath model` changes never touch it. Ignored when --model is given.")
	sf(create, "provider", "", "", "Inference provider paired with --model (e.g. 'openrouter', 'nous').")
	sf(create, "reasoning-effort", "", "", "Pin this job's reasoning (thinking) effort: none, minimal, low, medium, high, xhigh, max, or ultra. Overrides agent.reasoning_effort and agent.reasoning_overrides for this job; unsupported levels are clamped by the provider at request time. Omit to follow config.")
	sf(create, "interpreter", "", "", "Absolute or ~ path to a Python in your own venv (e.g. ~/venvs/report/bin/python) for a .py --script / --monitor-script, so it can import packages Icebath does not ship. .sh/.bash still run under bash. Omit to use Icebath' Python.")
	sf(create, "timezone", "", "", "IANA zone for this job's schedule (e.g. Asia/Hong_Kong), so the cron expression is read as THAT zone's wall clock whatever the profile's timezone is. Omit to inherit the profile timezone.")
	sf(create, "grace-minutes", "", "", "Catch-up window override in minutes (0-1440) for a cron schedule. Default: half the period, clamped to 2-120 minutes. 0 = never catch up (a missed occurrence is skipped, not run late).")
	bf(create, "continuity", "", "Each run wakes up with the job's own previous output injected into its prompt, so it can dedupe against what was already reported and continue where the last run left off (scouts, monitors, incremental digests). First run is unchanged.")
	bf(create, "paused", "", "Create disabled in one write; resume to schedule, or explicitly run now.")
	sf(create, "paused-reason", "", "", "Auditable reason; requires --paused.")
	edit := c.newCmd(cron, "edit", "Edit an existing scheduled job", "")
	c.pos(edit, p("job_id"))
	sf(edit, "schedule", "", "", "New schedule")
	sf(edit, "prompt", "", "", "New prompt/task instruction")
	sf(edit, "name", "", "", "New job name")
	sf(edit, "deliver", "", "", "New delivery target")
	sf(edit, "failure-deliver", "", "", "Override target for failure notices (same grammar as --deliver; 'local' suppresses; '' clears the override)")
	inf(edit, "repeat", "", 0, "New repeat count")
	catchUp(edit)
	af(edit, "skill", "", "Replace the job's skills with this set. Repeat to attach multiple skills.")
	af(edit, "add-skill", "", "Append a skill without replacing the existing list. Repeatable.")
	af(edit, "remove-skill", "", "Remove a specific attached skill. Repeatable.")
	bf(edit, "clear-skills", "", "Remove all attached skills from the job")
	sf(edit, "script", "", "", "Path to a script under ~/.icebath/scripts/. Pass empty string to clear. With --no-agent the script IS the job; otherwise its stdout is injected into the agent's prompt each run.")
	bf(edit, "no-agent", "", "Enable no-agent mode on this job (requires --script or an existing script on the job).")
	bf(edit, "agent", "", "Disable no-agent mode on this job (reverts to LLM-driven execution).")
	bf(edit, "continuity", "", "Turn on run-to-run continuity: each run sees the job's own previous output (dedupe, continue where it left off).")
	bf(edit, "no-continuity", "", "Turn off run-to-run continuity (other context_from job refs are preserved).")
	sf(edit, "monitor-script", "", "", "Set/replace the monitor source script (see `icebath cron create --monitor-script`). Pass empty string to clear.")
	sf(edit, "monitor-url", "", "", "Set/replace the monitor source URL. Pass empty string to clear.")
	sf(edit, "workdir", "", "", "Absolute path for the job to run from (injects AGENTS.md etc. and sets terminal cwd). Pass empty string to clear.")
	sf(edit, "model", "", "", "Pin this job to a specific inference model (user-owned; the agent's cronjob tool cannot set this). Pass empty string to clear the pin and follow cron.model, then the main agent model.")
	bf(edit, "pin", "", "Lock the CURRENT main agent model (and its provider) onto this job.")
	bf(edit, "unpin", "", "Release the job's model pin so it follows the main agent model again.")
	edit.MarkFlagsMutuallyExclusive("pin", "unpin")
	sf(edit, "provider", "", "", "Inference provider paired with --model. Pass empty string to clear.")
	sf(edit, "reasoning-effort", "", "", "Pin this job's reasoning (thinking) effort: none, minimal, low, medium, high, xhigh, max, or ultra. Pass empty string to clear the pin and follow config resolution.")
	sf(edit, "interpreter", "", "", "Absolute or ~ path to a Python for a .py script / monitor script. Pass empty string to clear (back to Icebath' Python).")
	sf(edit, "timezone", "", "", "IANA zone for this job's schedule. Pass empty string to clear the pin (back to the profile timezone); next_run_at is recomputed in the new zone.")
	sf(edit, "grace-minutes", "", "", "Catch-up window override in minutes (0-1440). 0 = never catch up. Pass empty string to restore the computed window.")
	pause := c.newCmd(cron, "pause", "Pause a scheduled job", "")
	c.pos(pause, p("job_id"))
	resume := c.newCmd(cron, "resume", "Resume a paused job", "")
	c.pos(resume, p("job_id"))
	sf(resume, "at", "", "", "Re-arm at an ISO-8601 time")
	bf(resume, "run-now", "", "Re-arm to run now")
	run := c.newCmd(cron, "run", "Run a job on the next scheduler tick", "")
	c.pos(run, p("job_id"))
	acceptHooks(run)
	cancel := c.newCmd(cron, "cancel", "Stop a job's live run (its schedule is untouched)", "")
	c.pos(cancel, p("job_id"))
	sf(cancel, "reason", "", "", "Optional note recorded with the cancelled run")
	remove := c.newCmd(cron, "remove", "Remove a scheduled job", "", "rm", "delete")
	c.pos(remove, p("job_id"))
	c.newCmd(cron, "status", "Check if cron scheduler is running", "")
	runs := c.newCmd(cron, "runs", "Show durable execution attempts", "", "history")
	c.pos(runs, pOpt("job_id"))
	inf(runs, "limit", "", 20, "Rows to show (1-500)")
	incidents := c.newCmd(cron, "incidents", "List or acknowledge durable cron failure incidents", "")
	sf(incidents, "state", "", "", "Filter incidents by lifecycle state")
	c.choice(incidents, "state", "detected", "alerted", "resolved", "closed")
	c.pos(incidents, pOptChoice("incident_action", "list", "list", "ack"), pOpt("incident_id"))
	notepad := c.newCmd(cron, "notepad", "Read/write a job's durable notepad (persistent KV across runs)", "")
	c.pos(notepad, p("job_id"), pOptChoice("notepad_action", "list", "get", "set", "delete", "list"), pOpt("key"), pOpt("value"))
	c.newCmd(cron, "doctor", "Check scheduled jobs for common health issues", "")
	tick := c.newCmd(cron, "tick", "Run due jobs once and exit", "")
	acceptHooks(tick)
}
