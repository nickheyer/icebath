package config

import (
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// Dangerous command approval section
type ApprovalsConfig struct {
	Mode                    ApprovalMode     `yaml:"mode"`
	Timeout                 int              `yaml:"timeout"`
	DiscordPromptTimeout    int              `yaml:"discord_prompt_timeout"`
	CronMode                UnattendedChoice `yaml:"cron_mode"`
	SingleQueryMode         UnattendedChoice `yaml:"single_query_mode"`
	UnattendedMode          UnattendedChoice `yaml:"unattended_mode"`
	SmartPolicy             string           `yaml:"smart_policy"`
	DenialBreakerThreshold  int              `yaml:"denial_breaker_threshold"`
	Deny                    []string         `yaml:"deny"`
	MCPReloadConfirm        bool             `yaml:"mcp_reload_confirm" coerce:"bool"`
	DestructiveSlashConfirm bool             `yaml:"destructive_slash_confirm" coerce:"bool"`
	rest                    rest
}

type approvalsFields ApprovalsConfig

// Decodes the mapping and keeps unknown keys
func (a *ApprovalsConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*approvalsFields)(a), &a.rest)
}

// Writes the mapping with unknown keys after the named ones
func (a ApprovalsConfig) MarshalYAML() (any, error) {
	return encodeMapping(approvalsFields(a), a.rest)
}

// How dangerous commands are approved
type ApprovalMode string

// Approval modes
const (
	ApprovalManual ApprovalMode = "manual"
	ApprovalSmart  ApprovalMode = "smart"
	ApprovalOff    ApprovalMode = "off"
)

// Accepted approval modes read case-insensitively, false reading as off, true and any other value as manual
func ApprovalModes() []ApprovalMode {
	return []ApprovalMode{ApprovalManual, ApprovalSmart, ApprovalOff}
}

// Answer to a dangerous command where nobody can approve
type UnattendedChoice string

// Unattended choices
const (
	UnattendedDeny    UnattendedChoice = "deny"
	UnattendedApprove UnattendedChoice = "approve"
)

// Accepted unattended choices read case-insensitively, off, allow and yes also meaning approve and any other value deny
func UnattendedChoices() []UnattendedChoice {
	return []UnattendedChoice{UnattendedDeny, UnattendedApprove}
}

// User-defined slash command that bypasses the agent loop
type QuickCommand struct {
	Type        QuickCommandType `yaml:"type,omitempty"`
	Command     string           `yaml:"command,omitempty"`
	Target      string           `yaml:"target,omitempty"`
	Description string           `yaml:"description,omitempty"`
	rest        rest
}

type quickCommandFields QuickCommand

// Decodes the mapping and keeps unknown keys
func (q *QuickCommand) UnmarshalYAML(n *yaml.Node) error {
	return decodeEntry(n, (*quickCommandFields)(q), &q.rest)
}

// Writes the mapping with unknown keys after the named ones
func (q QuickCommand) MarshalYAML() (any, error) {
	return encodeMapping(quickCommandFields(q), q.rest)
}

// Kind of quick command
type QuickCommandType string

// Quick command kinds
const (
	QuickExec  QuickCommandType = "exec"
	QuickAlias QuickCommandType = "alias"
)

// Accepted quick command kinds
func QuickCommandTypes() []QuickCommandType {
	return []QuickCommandType{QuickExec, QuickAlias}
}

// Per-platform system prompt hint override, a bare text standing for append, written back in the form read
type PlatformHint struct {
	Append  *string `yaml:"append,omitempty" coerce:"exact"`
	Replace *string `yaml:"replace,omitempty" coerce:"exact"`
	bare    bareForm
	rest    rest
}

type platformHintFields PlatformHint

// Accepts a text or a mapping of append and replace, a value of the other shape replacing the entry whole
func (h *PlatformHint) UnmarshalYAML(n *yaml.Node) error {
	n, err := resolved(n)
	if err != nil {
		return rootError(err)
	}
	if n != nil && n.Kind == yaml.ScalarNode && !isNull(n) {
		if n.ShortTag() != tagStr {
			return rootError(fmt.Errorf("%w: %s is not a hint text", ErrScalar, kindName(n)))
		}
		s := n.Value
		*h = PlatformHint{Append: &s}
		h.bare = bareForm{node: plainCopy(n), snap: snapshot(reflect.ValueOf(platformHintFields(*h)))}
		return nil
	}
	if h.bare.node != nil {
		*h = PlatformHint{}
	}
	return decodeEntry(n, (*platformHintFields)(h), &h.rest)
}

// Writes the bare text while the hint is unchanged since it was read, the mapping otherwise
func (h PlatformHint) MarshalYAML() (any, error) {
	return encodeSwitchable(platformHintFields(h), h.rest, h.bare)
}

// Custom personality, a bare prompt or list of prompt lines or a mapping, written back in the form read
type Personality struct {
	Description  string `yaml:"description,omitempty"`
	SystemPrompt string `yaml:"system_prompt,omitempty"`
	Tone         string `yaml:"tone,omitempty"`
	Style        string `yaml:"style,omitempty"`
	bare         bareForm
	rest         rest
}

type personalityFields Personality

// Keeps an explicit null as a personality with an empty prompt
func (Personality) holdsNull() {}

// Accepts a prompt scalar, a list of prompt lines, null or a mapping, another shape replacing the entry whole
func (p *Personality) UnmarshalYAML(n *yaml.Node) error {
	n, err := resolved(n)
	if err != nil {
		return rootError(err)
	}
	prompt, bare := "", true
	switch {
	case isNull(n):
	case n.Kind == yaml.ScalarNode:
		prompt = n.Value
	case n.Kind == yaml.SequenceNode:
		items, _, err := scalarItems(n)
		if err != nil {
			return err
		}
		var lines []string
		for _, it := range items {
			if it = strings.TrimSpace(it); it != "" {
				lines = append(lines, it)
			}
		}
		prompt = strings.Join(lines, "\n")
	default:
		bare = false
	}
	if bare {
		*p = Personality{SystemPrompt: prompt}
		p.bare = bareForm{node: plainCopy(n), snap: snapshot(reflect.ValueOf(personalityFields(*p)))}
		return nil
	}
	if p.bare.node != nil {
		*p = Personality{}
	}
	return decodeEntry(n, (*personalityFields)(p), &p.rest)
}

// Writes the bare value while the personality is unchanged since it was read, the mapping otherwise
func (p Personality) MarshalYAML() (any, error) {
	return encodeSwitchable(personalityFields(p), p.rest, p.bare)
}

// Login policy section
type AuthConfig struct {
	AdoptExternalLogins bool           `yaml:"adopt_external_logins" coerce:"bool"`
	CodexLoginFlow      CodexLoginFlow `yaml:"codex_login_flow"`
	rest                rest
}

type authFields AuthConfig

// Decodes the mapping and keeps unknown keys
func (a *AuthConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*authFields)(a), &a.rest)
}

// Writes the mapping with unknown keys after the named ones
func (a AuthConfig) MarshalYAML() (any, error) {
	return encodeMapping(authFields(a), a.rest)
}

// How the OpenAI Codex sign-in runs
type CodexLoginFlow string

// Codex sign-in flows
const (
	LoginDeviceCode CodexLoginFlow = "device_code"
	LoginBrowser    CodexLoginFlow = "browser"
)

// Accepted Codex sign-in flows
func CodexLoginFlows() []CodexLoginFlow {
	return []CodexLoginFlow{LoginDeviceCode, LoginBrowser}
}

// Security section
type SecurityConfig struct {
	AllowPrivateURLs                     bool              `yaml:"allow_private_urls"`
	FakeIPRanges                         StringList        `yaml:"fake_ip_ranges"`
	RedactSecrets                        *bool             `yaml:"redact_secrets"`
	AllowDataTrainingTiersNoninteractive bool              `yaml:"allow_data_training_tiers_noninteractive" coerce:"exact"`
	Approval                             ApprovalTransport `yaml:"approval"`
	ProtectedInstructionFiles            bool              `yaml:"protected_instruction_files" coerce:"exact"`
	ProtectedInstructionExtraPatterns    []string          `yaml:"protected_instruction_extra_patterns"`
	WebsiteBlocklist                     WebsiteBlocklist  `yaml:"website_blocklist"`
	AckedAdvisories                      []string          `yaml:"acked_advisories"`
	AllowLazyInstalls                    bool              `yaml:"allow_lazy_installs"`
	rest                                 rest
}

type securityFields SecurityConfig

// Decodes the mapping and keeps unknown keys
func (s *SecurityConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*securityFields)(s), &s.rest)
}

// Writes the mapping with unknown keys after the named ones
func (s SecurityConfig) MarshalYAML() (any, error) {
	return encodeMapping(securityFields(s), s.rest)
}

// Human approval presentation transport
type ApprovalTransport struct {
	Transport         string `yaml:"transport"`
	TransportFallback string `yaml:"transport_fallback"`
	rest              rest
}

type approvalTransportFields ApprovalTransport

// Decodes the mapping and keeps unknown keys
func (a *ApprovalTransport) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*approvalTransportFields)(a), &a.rest)
}

// Writes the mapping with unknown keys after the named ones
func (a ApprovalTransport) MarshalYAML() (any, error) {
	return encodeMapping(approvalTransportFields(a), a.rest)
}

// Blocked website domains for web tools
type WebsiteBlocklist struct {
	Enabled     bool     `yaml:"enabled" coerce:"exact"`
	Domains     []string `yaml:"domains"`
	SharedFiles []string `yaml:"shared_files"`
	rest        rest
}

type websiteBlocklistFields WebsiteBlocklist

// Decodes the mapping and keeps unknown keys
func (w *WebsiteBlocklist) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*websiteBlocklistFields)(w), &w.rest)
}

// Writes the mapping with unknown keys after the named ones
func (w WebsiteBlocklist) MarshalYAML() (any, error) {
	return encodeMapping(websiteBlocklistFields(w), w.rest)
}

// Defaults of the approval section
func defaultApprovals() ApprovalsConfig {
	return ApprovalsConfig{
		Mode:                    ApprovalSmart,
		Timeout:                 300,
		DiscordPromptTimeout:    300,
		CronMode:                UnattendedDeny,
		SingleQueryMode:         UnattendedDeny,
		UnattendedMode:          UnattendedDeny,
		DenialBreakerThreshold:  3,
		Deny:                    []string{},
		MCPReloadConfirm:        true,
		DestructiveSlashConfirm: true,
	}
}

// Defaults of the security section
func defaultSecurity() SecurityConfig {
	return SecurityConfig{
		FakeIPRanges:                      ListOf(),
		RedactSecrets:                     new(true),
		Approval:                          ApprovalTransport{Transport: "builtin", TransportFallback: "deny"},
		ProtectedInstructionFiles:         true,
		ProtectedInstructionExtraPatterns: []string{},
		WebsiteBlocklist:                  WebsiteBlocklist{Domains: []string{}, SharedFiles: []string{}},
		AckedAdvisories:                   []string{},
		AllowLazyInstalls:                 true,
	}
}

// Documentation of the approval, command, hint, personality, auth and security keys
func approvalDocs() map[string]string {
	return map[string]string{
		"approvals":                                         "Approvals for dangerous commands. mode: manual (always prompt) | smart (aux LLM auto-approves low-risk) | off (= --yolo). cron_mode, single_query_mode and unattended_mode: deny | approve, what to do when a cron job, a -q session (ICEBATH_INTERACTIVE=1 but nobody to answer), or an unattended platform (webhook, msgraph_webhook, api_server; no /approve channel) hits one. deny blocks instantly so the agent finds another way instead of waiting out the timeout and failing closed.",
		"approvals.mode":                                    "manual (always prompt) | smart (aux LLM auto-approves low-risk) | off (= --yolo).",
		"approvals.timeout":                                 "Seconds before an unanswered prompt fails closed (CLI and gateway). 60s proved too tight for Telegram and Discord push notifications, hence 300.",
		"approvals.discord_prompt_timeout":                  "Discord buttons stay clickable this long (min 30 s, capped at the safe maximum).",
		"approvals.cron_mode":                               "deny | approve for cron jobs that hit a dangerous command.",
		"approvals.single_query_mode":                       "What to do when a single-query (-q) session hits a dangerous command. -q runs export ICEBATH_INTERACTIVE=1 (for interactive sudo prompts) but have NO user waiting to answer approval prompts, so an unanswered prompt just waits the full timeout then fails closed. deny = block the command and let the agent find another way (default, safe; mirrors cron_mode deny); approve = auto-approve all dangerous commands in single-query mode.",
		"approvals.unattended_mode":                         "Unattended platforms bind a session platform like chat gateways do, but have no approval delivery and no /approve channel, so a pending approval there blocks for the full timeout with nobody to answer. deny = block the command instantly and let the agent find another way (default, safe); approve = auto-approve all dangerous commands on unattended platforms.",
		"approvals.smart_policy":                            "Extra rules appended to the smart-approval guardian's SYSTEM prompt, e.g. \"Always ESCALATE commands touching /etc\".",
		"approvals.denial_breaker_threshold":                "After this many consecutive guardian DENYs in a session, the deny message escalates to a hard-stop (report to user, ask for /approve). Approval resets; 0 off.",
		"approvals.deny":                                    "Case-insensitive fnmatch globs against terminal commands; a match blocks even under --yolo or mode=off. Quote in YAML when starting with * or containing {}, ! or :, e.g. \"git push --force*\".",
		"approvals.mcp_reload_confirm":                      "/reload-mcp confirms before rebuilding the MCP tool set (it invalidates the prompt cache, so the next message re-sends full input). \"Always Approve\" -> false.",
		"approvals.destructive_slash_confirm":               "/clear, /new, /reset, /undo confirm before discarding state (Approve Once, Always Approve, Cancel via tools.slash_confirm; native buttons on Telegram, Discord and Slack). \"Always Approve\" -> false. ICEBATH_TUI_NO_CONFIRM=1 skips the TUI modal.",
		"command_allowlist":                                 "Permanently allowed dangerous command patterns (added via \"always\" approval).",
		"quick_commands":                                    "User-defined quick commands that bypass the agent loop: {type: exec, command: ...} or {type: alias, target: ...}.",
		"platform_hints":                                    "Per-platform system-prompt hint overrides, keyed by platform name (whatsapp, slack, telegram, ...). Value: {\"append\": text} keeps the built-in hint and appends; {\"replace\": text} substitutes it; a bare string is shorthand for append. `replace` wins over `append` if both are given.",
		"hooks_auto_accept":                                 "Auto-accept shell-hook registrations without a TTY prompt (also --accept-hooks or ICEBATH_ACCEPT_HOOKS=1). Gateway, cron and non-interactive runs need one of these to pick up newly-added hooks.",
		"personalities":                                     "Custom personalities: {\"name\": \"system prompt\"} or {\"name\": {\"description\", \"system_prompt\", \"tone\", \"style\"}}.",
		"auth":                                              "Login policy (credentials themselves live in auth.json and .env).",
		"auth.adopt_external_logins":                        "Borrow and refresh the Codex CLI (~/.codex/auth.json) and Claude Code (~/.claude/.credentials.json) logins automatically when icebath has no usable login of its own. Their refresh tokens are single-use and rotate, so two programs on one login can log each other out; set false to make icebath use only its own logins (`icebath auth add <provider>`). `icebath auth add openai-codex` still offers the import interactively.",
		"auth.codex_login_flow":                             "How `icebath auth add openai-codex` and `icebath model` sign in to OpenAI Codex. \"device_code\" (default): open a URL, enter a code. \"browser\": authorization-code + PKCE on the loopback listener http://localhost:1455/auth/callback (the redirect OpenAI registered for the Codex client), for organizations that disable the device-code grant. Falls back to device code when that port is busy. `icebath auth add openai-codex --browser` opts in for one login without changing this key.",
		"security":                                          "Security: URL and private-network guards, redaction and approval presentation.",
		"security.allow_private_urls":                       "Private IPs for web, vision and media + cloud and CDP browsers; not local or sidecar Chromium.",
		"security.fake_ip_ranges":                           "CIDR blocks a local TUN proxy answers DNS with (Mihomo/Clash fake-ip, Surge enhanced). Answers inside these blocks are the proxy's sentinels, not internal hosts, so the guard dials them instead of rejecting them as private. Empty = normal private-address verdict.",
		"security.redact_secrets":                           "Secret redaction is ON by default: strings that look like API keys, tokens, and passwords are masked in tool output, logs, and chat responses before the model or user ever sees them. Set false to disable (e.g. when developing the redactor itself).",
		"security.allow_data_training_tiers_noninteractive": "Persisted acknowledgement for unattended model overrides whose tier lets the vendor train on prompts. The startup guard still warns every run; cost guards are unaffected.",
		"security.approval":                                 "Human-approval presentation transport. \"builtin\" = CLI, TUI, gateway and ACP surfaces; a plugin transport is used only when named explicitly. A transport timeout, error or invalid response DENIES unless transport_fallback is \"builtin\". Presentation only: plugins cannot detect, suppress, or auto-approve commands outside a correlated human response.",
		"security.protected_instruction_files":              "Writes to agent-instruction files (AGENTS.md, CLAUDE.md, SOUL.md, .cursorrules, project-local .icebath config) always need human approval, even under yolo.",
		"security.protected_instruction_extra_patterns":     "Extra fnmatch globs on the basename (e.g. \"*.mdc\") that count as instruction files.",
		"security.website_blocklist":                        "Domains web tools refuse, inline and from shared files.",
		"security.acked_advisories":                         "IDs of supply-chain advisories the user has read and acted on; acked ones stop the startup banner. Add via `icebath doctor --ack <id>`; remove by editing the list.",
		"security.allow_lazy_installs":                      "Lazy-install opt-in backend packages when a backend that needs them is first enabled (e.g. `elevenlabs`). false = require an explicit install for everything beyond the base set (restricted, audited or air-gapped environments).",
	}
}

// Answers when an approval transport fails, deny unless builtin is named
func ApprovalTransportFallbacks() []string {
	return []string{"deny", "builtin"}
}
