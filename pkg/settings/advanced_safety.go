package settings

import "time"

// How Approvals treat risky commands in scheduled jobs
var ApprovalsCronMode = Register(&Enum{Meta: Meta{Key: "approvals.cron_mode", Label: "Scheduled job approvals", Short: "How Approvals treat risky commands in scheduled jobs.", Help: "Approve runs risky commands with nobody watching.", Group: GroupSafety, Order: 1000, Advanced: true}, Default: "deny", Values: []Choice{
	{Value: "deny", Label: "Deny", Meaning: "Block commands that would need approval."},
	{Value: "approve", Label: "Approve", Meaning: "Allow them without asking."},
}})

// Stop a session after this many Approvals denials in a row
var ApprovalsDenialBreakerThreshold = Register(&Int{Meta: Meta{Key: "approvals.denial_breaker_threshold", Label: "Denial limit", Short: "Stop a session after this many Approvals denials in a row.", Help: "Zero or less turns the limit off.", Group: GroupSafety, Order: 1010, Advanced: true}, Default: 3})

// Command patterns Approvals always block, even in yolo mode
var ApprovalsDeny = Register(&List{Meta: Meta{Key: "approvals.deny", Label: "Always-deny patterns", Short: "Command patterns Approvals always block, even in yolo mode.", Help: "Matched as globs against the command text.", Group: GroupSafety, Order: 1020, Advanced: true}})

// How long Discord approval buttons wait for an answer
var ApprovalsDiscordPromptTimeout = Register(&Duration{Meta: Meta{Key: "approvals.discord_prompt_timeout", Label: "Discord prompt wait", Short: "How long Discord approval buttons wait for an answer.", Group: GroupSafety, Order: 1030, Advanced: true}, Default: 5 * time.Minute, Min: at(30 * time.Second)})

// Ask before reconnecting tool servers from a chat command
var ApprovalsMcpReloadConfirm = Register(&Bool{Meta: Meta{Key: "approvals.mcp_reload_confirm", Label: "Confirm tool reload", Short: "Ask before reconnecting tool servers from a chat command.", Group: GroupSafety, Order: 1040, Advanced: true}, Default: true})

// How Approvals treat risky commands in one-shot runs (-q)
var ApprovalsSingleQueryMode = Register(&Enum{Meta: Meta{Key: "approvals.single_query_mode", Label: "One-shot approvals", Short: "How Approvals treat risky commands in one-shot runs (-q).", Help: "Approve runs risky commands with nobody watching.", Group: GroupSafety, Order: 1050, Advanced: true}, Default: "deny", Values: []Choice{
	{Value: "deny", Label: "Deny", Meaning: "Block commands that would need approval."},
	{Value: "approve", Label: "Approve", Meaning: "Allow them without asking."},
}})

// Extra rules the approval reviewer model follows
var ApprovalsSmartPolicy = Register(&String{Meta: Meta{Key: "approvals.smart_policy", Label: "Smart approval rules", Short: "Extra rules the approval reviewer model follows.", Help: "Free text added to the reviewer's instructions.", Group: GroupSafety, Order: 1060, Advanced: true}, Default: ""})

// How long an approval prompt waits before it is denied
var ApprovalsTimeout = Register(&Duration{Meta: Meta{Key: "approvals.timeout", Label: "Approval wait", Short: "How long an approval prompt waits before it is denied.", Group: GroupSafety, Order: 1070, Advanced: true}, Default: 5 * time.Minute})

// How Approvals treat risky commands on chat platforms
var ApprovalsUnattendedMode = Register(&Enum{Meta: Meta{Key: "approvals.unattended_mode", Label: "Unattended approvals", Short: "How Approvals treat risky commands on chat platforms.", Help: "Approve runs risky commands with nobody watching.", Group: GroupSafety, Order: 1080, Advanced: true}, Default: "deny", Values: []Choice{
	{Value: "deny", Label: "Deny", Meaning: "Block commands that would need approval."},
	{Value: "approve", Label: "Approve", Meaning: "Allow them without asking."},
}})

// Reuse sign-ins already saved by other command-line tools
var AuthAdoptExternalLogins = Register(&Bool{Meta: Meta{Key: "auth.adopt_external_logins", Label: "Adopt other logins", Short: "Reuse sign-ins already saved by other command-line tools.", Group: GroupSafety, Order: 1090, Advanced: true}, Default: true})

// How the Codex sign-in is done
var AuthCodexLoginFlow = Register(&Enum{Meta: Meta{Key: "auth.codex_login_flow", Label: "Codex login style", Short: "How the Codex sign-in is done.", Group: GroupSafety, Order: 1100, Advanced: true}, Default: "device_code", Values: []Choice{
	{Value: "device_code", Label: "Device code", Meaning: "Enter a code on another device."},
	{Value: "browser", Label: "Browser", Meaning: "Sign in through a local browser window."},
}})

// Approve shell hooks without asking first
var HooksAutoAccept = Register(&Bool{Meta: Meta{Key: "hooks_auto_accept", Label: "Auto-accept hooks", Short: "Approve shell hooks without asking first.", Help: "Hooks run commands on your machine, so only enable this for trusted configs.", Group: GroupSafety, Order: 1110, Advanced: true}, Default: false})

// Environment variable holding the Bitwarden access token
var SecretsBitwardenAccessTokenEnv = Register(&String{Meta: Meta{Key: "secrets.bitwarden.access_token_env", Label: "Bitwarden token variable", Short: "Environment variable holding the Bitwarden access token.", Group: GroupSafety, Order: 1120, Advanced: true}, Default: "BWS_ACCESS_TOKEN"})

// Download the Bitwarden command-line tool when missing
var SecretsBitwardenAutoInstall = Register(&Bool{Meta: Meta{Key: "secrets.bitwarden.auto_install", Label: "Auto-install Bitwarden", Short: "Download the Bitwarden command-line tool when missing.", Group: GroupSafety, Order: 1130, Advanced: true}, Default: true})

// How long fetched Bitwarden secrets are reused
var SecretsBitwardenCacheTtlSeconds = Register(&Duration{Meta: Meta{Key: "secrets.bitwarden.cache_ttl_seconds", Label: "Bitwarden cache time", Short: "How long fetched Bitwarden secrets are reused.", Help: "Zero fetches them every time.", Group: GroupSafety, Order: 1140, Advanced: true}, Default: 5 * time.Minute})

// Load secrets from Bitwarden Secrets Manager at startup
var SecretsBitwardenEnabled = Register(&Bool{Meta: Meta{Key: "secrets.bitwarden.enabled", Label: "Bitwarden secrets", Short: "Load secrets from Bitwarden Secrets Manager at startup.", Group: GroupSafety, Order: 1150, Advanced: true}, Default: false})

// Keep an encrypted copy to use when Bitwarden is unreachable
var SecretsBitwardenEncryptedCacheEnabled = Register(&Bool{Meta: Meta{Key: "secrets.bitwarden.encrypted_cache.enabled", Label: "Offline Bitwarden cache", Short: "Keep an encrypted copy to use when Bitwarden is unreachable.", Group: GroupSafety, Order: 1160, Advanced: true}, Default: false})

// Oldest offline Bitwarden copy that may still be used
var SecretsBitwardenEncryptedCacheMaxStaleSeconds = Register(&Duration{Meta: Meta{Key: "secrets.bitwarden.encrypted_cache.max_stale_seconds", Label: "Offline cache age", Short: "Oldest offline Bitwarden copy that may still be used.", Help: "Zero means no age limit.", Group: GroupSafety, Order: 1170, Advanced: true}, Default: 0})

// Bitwarden values replace matching local environment values
var SecretsBitwardenOverrideExisting = Register(&Bool{Meta: Meta{Key: "secrets.bitwarden.override_existing", Label: "Bitwarden wins", Short: "Bitwarden values replace matching local environment values.", Group: GroupSafety, Order: 1180, Advanced: true}, Default: true})

// Bitwarden project to load secrets from
var SecretsBitwardenProjectId = Register(&String{Meta: Meta{Key: "secrets.bitwarden.project_id", Label: "Bitwarden project", Short: "Bitwarden project to load secrets from.", Help: "Empty loads everything the token can see.", Group: GroupSafety, Order: 1190, Advanced: true}, Default: ""})

// Address of your own Bitwarden server
var SecretsBitwardenServerUrl = Register(&String{Meta: Meta{Key: "secrets.bitwarden.server_url", Label: "Bitwarden server", Short: "Address of your own Bitwarden server.", Help: "Empty uses the Bitwarden cloud.", Group: GroupSafety, Order: 1200, Advanced: true}, Default: ""})

// 1Password account to use
var SecretsOnepasswordAccount = Register(&String{Meta: Meta{Key: "secrets.onepassword.account", Label: "1Password account", Short: "1Password account to use.", Help: "Empty uses the default account.", Group: GroupSafety, Order: 1210, Advanced: true}, Default: ""})

// Full path of the 1Password command-line tool
var SecretsOnepasswordBinaryPath = Register(&String{Meta: Meta{Key: "secrets.onepassword.binary_path", Label: "1Password tool path", Short: "Full path of the 1Password command-line tool.", Help: "Empty looks it up on the search path.", Group: GroupSafety, Order: 1220, Advanced: true}, Default: ""})

// How long fetched 1Password secrets are reused
var SecretsOnepasswordCacheTtlSeconds = Register(&Duration{Meta: Meta{Key: "secrets.onepassword.cache_ttl_seconds", Label: "1Password cache time", Short: "How long fetched 1Password secrets are reused.", Help: "Zero fetches them every time.", Group: GroupSafety, Order: 1230, Advanced: true}, Default: 5 * time.Minute})

// Load secrets from 1Password at startup
var SecretsOnepasswordEnabled = Register(&Bool{Meta: Meta{Key: "secrets.onepassword.enabled", Label: "1Password secrets", Short: "Load secrets from 1Password at startup.", Group: GroupSafety, Order: 1240, Advanced: true}, Default: false})

// Variables to load, as name and op:// reference
var SecretsOnepasswordEnv = Register(&Map{Meta: Meta{Key: "secrets.onepassword.env", Label: "1Password references", Short: "Variables to load, as name and op:// reference.", Group: GroupSafety, Order: 1250, Advanced: true}})

// 1Password values replace matching local environment values
var SecretsOnepasswordOverrideExisting = Register(&Bool{Meta: Meta{Key: "secrets.onepassword.override_existing", Label: "1Password wins", Short: "1Password values replace matching local environment values.", Group: GroupSafety, Order: 1260, Advanced: true}, Default: true})

// Environment variable holding the service account token
var SecretsOnepasswordServiceAccountTokenEnv = Register(&String{Meta: Meta{Key: "secrets.onepassword.service_account_token_env", Label: "1Password token variable", Short: "Environment variable holding the service account token.", Group: GroupSafety, Order: 1270, Advanced: true}, Default: "OP_SERVICE_ACCOUNT_TOKEN"})

// Variable names no secret store may overwrite
var SecretsPreserveExisting = Register(&List{Meta: Meta{Key: "secrets.preserve_existing", Label: "Keep local values", Short: "Variable names no secret store may overwrite.", Group: GroupSafety, Order: 1280, Advanced: true}})

// Also expose secrets under profile-prefixed variable names
var SecretsProfileAlias = Register(&Bool{Meta: Meta{Key: "secrets.profile_alias", Label: "Profile name aliases", Short: "Also expose secrets under profile-prefixed variable names.", Group: GroupSafety, Order: 1290, Advanced: true}, Default: true})

// Security notices you have already acknowledged
var SecurityAckedAdvisories = Register(&List{Meta: Meta{Key: "security.acked_advisories", Label: "Dismissed advisories", Short: "Security notices you have already acknowledged.", Group: GroupSafety, Order: 1300, Advanced: true}})

// Let non-interactive runs go ahead on data-training plans
var SecurityAllowDataTrainingTiersNoninteractive = Register(&Bool{Meta: Meta{Key: "security.allow_data_training_tiers_noninteractive", Label: "Allow training plans", Short: "Let non-interactive runs go ahead on data-training plans.", Help: "Otherwise such runs stop and ask for acknowledgement.", Group: GroupSafety, Order: 1310, Advanced: true}, Default: false})

// Let tools install missing add-ons the first time they are used
var SecurityAllowLazyInstalls = Register(&Bool{Meta: Meta{Key: "security.allow_lazy_installs", Label: "Allow on-demand installs", Short: "Let tools install missing add-ons the first time they are used.", Group: GroupSafety, Order: 1320, Advanced: true}, Default: true})

// Let web tools open local and private network addresses
var SecurityAllowPrivateUrls = Register(&Bool{Meta: Meta{Key: "security.allow_private_urls", Label: "Allow private URLs", Short: "Let web tools open local and private network addresses.", Group: GroupSafety, Order: 1330, Advanced: true}, Default: false})

// Which channel delivers Approvals prompts
var SecurityApprovalTransport = Register(&String{Meta: Meta{Key: "security.approval.transport", Label: "Approval channel", Short: "Which channel delivers Approvals prompts.", Help: "Built-in uses the normal prompt. A plugin name sends prompts through that plugin.", Group: GroupSafety, Order: 1340, Advanced: true}, Default: "builtin"})

// What happens when the chosen approval channel fails
var SecurityApprovalTransportFallback = Register(&Enum{Meta: Meta{Key: "security.approval.transport_fallback", Label: "Approval fallback", Short: "What happens when the chosen approval channel fails.", Group: GroupSafety, Order: 1350, Advanced: true}, Default: "deny", Values: []Choice{
	{Value: "deny", Label: "Deny", Meaning: "Block the command."},
	{Value: "builtin", Label: "Built-in", Meaning: "Fall back to the normal prompt."},
}})

// Address ranges a local DNS proxy hands out for real sites
var SecurityFakeIpRanges = Register(&List{Meta: Meta{Key: "security.fake_ip_ranges", Label: "Fake IP ranges", Short: "Address ranges a local DNS proxy hands out for real sites.", Help: "Web tools check the real address instead of these.", Group: GroupSafety, Order: 1360, Advanced: true}})

// Extra file names the agent needs approval to edit
var SecurityProtectedInstructionExtraPatterns = Register(&List{Meta: Meta{Key: "security.protected_instruction_extra_patterns", Label: "Extra protected files", Short: "Extra file names the agent needs approval to edit.", Group: GroupSafety, Order: 1370, Advanced: true}})

// Ask before the agent edits project instruction files
var SecurityProtectedInstructionFiles = Register(&Bool{Meta: Meta{Key: "security.protected_instruction_files", Label: "Protect instruction files", Short: "Ask before the agent edits project instruction files.", Group: GroupSafety, Order: 1380, Advanced: true}, Default: true})

// Websites web tools refuse to open
var SecurityWebsiteBlocklistDomains = Register(&List{Meta: Meta{Key: "security.website_blocklist.domains", Label: "Blocked sites", Short: "Websites web tools refuse to open.", Group: GroupSafety, Order: 1390, Advanced: true}})

// Block web tools from the sites on the list
var SecurityWebsiteBlocklistEnabled = Register(&Bool{Meta: Meta{Key: "security.website_blocklist.enabled", Label: "Website blocklist", Short: "Block web tools from the sites on the list.", Group: GroupSafety, Order: 1400, Advanced: true}, Default: false})

// Files of blocked sites to load as well
var SecurityWebsiteBlocklistSharedFiles = Register(&List{Meta: Meta{Key: "security.website_blocklist.shared_files", Label: "Blocklist files", Short: "Files of blocked sites to load as well.", Group: GroupSafety, Order: 1410, Advanced: true}})

// Full path of the Bitwarden command-line tool
var VaultBitwardenBinaryPath = Register(&String{Meta: Meta{Key: "vault.bitwarden.binary_path", Label: "Bitwarden tool path", Short: "Full path of the Bitwarden command-line tool.", Help: "Empty looks it up on the search path.", Group: GroupSafety, Order: 1420, Advanced: true}, Default: ""})

// Offer Bitwarden as a source of website logins
var VaultBitwardenEnabled = Register(&Bool{Meta: Meta{Key: "vault.bitwarden.enabled", Label: "Bitwarden logins", Short: "Offer Bitwarden as a source of website logins.", Group: GroupSafety, Order: 1430, Advanced: true}, Default: true})

// Where a new vault's encryption key is kept
var VaultKeyStorage = Register(&Enum{Meta: Meta{Key: "vault.key_storage", Label: "Vault key storage", Short: "Where a new vault's encryption key is kept.", Group: GroupSafety, Order: 1440, Advanced: true}, Default: "file", Values: []Choice{
	{Value: "file", Label: "File", Meaning: "A key file in the icebath home folder."},
	{Value: "keychain", Label: "OS keychain", Meaning: "The system keychain."},
}})

// 1Password account to use for website logins
var VaultOnepasswordAccount = Register(&String{Meta: Meta{Key: "vault.onepassword.account", Label: "1Password account", Short: "1Password account to use for website logins.", Help: "Empty uses the default account.", Group: GroupSafety, Order: 1450, Advanced: true}, Default: ""})

// Full path of the 1Password command-line tool
var VaultOnepasswordBinaryPath = Register(&String{Meta: Meta{Key: "vault.onepassword.binary_path", Label: "1Password tool path", Short: "Full path of the 1Password command-line tool.", Help: "Empty looks it up on the search path.", Group: GroupSafety, Order: 1460, Advanced: true}, Default: ""})

// Offer 1Password as a source of website logins
var VaultOnepasswordEnabled = Register(&Bool{Meta: Meta{Key: "vault.onepassword.enabled", Label: "1Password logins", Short: "Offer 1Password as a source of website logins.", Group: GroupSafety, Order: 1470, Advanced: true}, Default: true})

// Environment variable holding the service account token
var VaultOnepasswordServiceAccountTokenEnv = Register(&String{Meta: Meta{Key: "vault.onepassword.service_account_token_env", Label: "1Password token variable", Short: "Environment variable holding the service account token.", Group: GroupSafety, Order: 1480, Advanced: true}, Default: "OP_SERVICE_ACCOUNT_TOKEN"})
