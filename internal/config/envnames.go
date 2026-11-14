package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// Matched by every refused write to an env file, nothing having changed
var ErrEnvWriteRefused = errors.New("env write refused")

// Shape of a portable environment variable name
var envNameShape = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Run of characters a custom endpoint key name replaces with one underscore
var endpointSlugGap = regexp.MustCompile(`[^A-Z0-9]+`)

// Suffixes of the only variables sanitized on load, credentials that travel as HTTP header values
func credentialSuffixes() []string {
	return []string{"_API_KEY", "_TOKEN", "_SECRET", "_KEY"}
}

// Reports a variable whose value the loader keeps pure ASCII
func isCredentialName(name string) bool {
	return slices.ContainsFunc(credentialSuffixes(), func(suffix string) bool { return strings.HasSuffix(name, suffix) })
}

// Name families the env writer refuses whole because every member steers execution or injects config
func envNameDenyPrefixes() []string {
	return []string{"LD_", "DYLD_", "GIT_CONFIG_"}
}

// Names the env writer refuses because they steer how the next subprocess runs or where and how icebath runs
func envNameDenylist() []string {
	return []string{
		"LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT", "LD_DEBUG",
		"DYLD_INSERT_LIBRARIES", "DYLD_LIBRARY_PATH", "DYLD_FRAMEWORK_PATH",
		"DYLD_FALLBACK_LIBRARY_PATH", "DYLD_FALLBACK_FRAMEWORK_PATH",
		"PYTHONPATH", "PYTHONHOME", "PYTHONSTARTUP", "PYTHONUSERBASE",
		"PYTHONEXECUTABLE", "PYTHONNOUSERSITE", "PYTHONBREAKPOINT", "PYTHONCASEOK",
		"NODE_OPTIONS", "NODE_PATH",
		"PERL5OPT", "PERL5LIB", "PERLLIB", "RUBYOPT", "RUBYLIB", "CLASSPATH",
		"JAVA_TOOL_OPTIONS", "_JAVA_OPTIONS", "JDK_JAVA_OPTIONS",
		"GOFLAGS", "RUSTFLAGS",
		"PATH", "SHELL", "BROWSER", "EDITOR", "VISUAL", "PAGER", "MANPAGER",
		"GIT_SSH_COMMAND", "GIT_EXEC_PATH", "GIT_SHELL",
		"GIT_SSH", "GIT_ASKPASS", "SSH_ASKPASS", "SUDO_ASKPASS",
		"GIT_EDITOR", "GIT_SEQUENCE_EDITOR", "GIT_PAGER", "GIT_EXTERNAL_DIFF",
		"GIT_PROXY_COMMAND", "GIT_TEMPLATE_DIR", "GIT_DIR",
		"BASH_ENV", "ENV", "ZDOTDIR", "PROMPT_COMMAND", "VIMINIT", "EXINIT",
		"ICEBATH_HOME", "ICEBATH_PROFILE", "ICEBATH_CONFIG", "ICEBATH_ENV",
		"ICEBATH_CONFIG_PATH", "ICEBATH_ENV_PATH",
		"ICEBATH_OPTIONAL_MCPS",
		"ICEBATH_YOLO_MODE", "ICEBATH_ACCEPT_HOOKS", "ICEBATH_REDACT_SECRETS",
		"ICEBATH_INTERACTIVE", "ICEBATH_EXEC_ASK", "ICEBATH_GATEWAY_SESSION",
		"ICEBATH_CRON_SESSION", "ICEBATH_SINGLE_QUERY_SESSION",
		"ICEBATH_SESSION_KEY", "ICEBATH_SESSION_PLATFORM",
	}
}

// Env names of providers that left the core which stay profile managed, scrubbed at load, routed to the env file
func ProfileEnvNames() []string {
	return []string{"ICEBATH_COPILOT_ACP_COMMAND", "ICEBATH_COPILOT_ACP_ARGS", "COPILOT_CLI_PATH", "COPILOT_ACP_BASE_URL"}
}

// Env names of providers that left the core which choose an executable or its arguments, refused by the env writer
func LaunchEnvNames() []string {
	return []string{"ICEBATH_COPILOT_ACP_COMMAND", "ICEBATH_COPILOT_ACP_ARGS", "COPILOT_CLI_PATH"}
}

// Routing keys a parent process injects that are scrubbed at load when the profile env file does not define them
func ProfileManagedEnvKeys() []string {
	return append([]string{"ICEBATH_ACP_AUTH_METHOD", "ICEBATH_ACP_AUTO_APPROVE"}, ProfileEnvNames()...)
}

// Credentials a parent mints for one child process, which a persisted value never replaces
func spawnCredentialKeys() []string {
	return []string{"ICEBATH_DASHBOARD_SESSION_TOKEN"}
}

// Names written to the env file by setup and provider flows outside the registry, which a reload may also remove
func ExtraEnvKeys() []string {
	return append([]string{
		"OPENAI_API_KEY", "OPENAI_BASE_URL", "ANTHROPIC_API_KEY", "ANTHROPIC_TOKEN",
		"DISCORD_HOME_CHANNEL", "DISCORD_HOME_CHANNEL_NAME",
		"TELEGRAM_HOME_CHANNEL", "TELEGRAM_HOME_CHANNEL_NAME",
		"SLACK_HOME_CHANNEL", "SLACK_HOME_CHANNEL_NAME",
		"SIGNAL_ACCOUNT", "SIGNAL_HTTP_URL", "SIGNAL_ALLOWED_USERS", "SIGNAL_GROUP_ALLOWED_USERS",
		"SIGNAL_HOME_CHANNEL", "SIGNAL_HOME_CHANNEL_NAME", "SMS_HOME_CHANNEL", "SMS_HOME_CHANNEL_NAME",
		"DINGTALK_CLIENT_ID", "DINGTALK_CLIENT_SECRET", "DINGTALK_HOME_CHANNEL", "DINGTALK_HOME_CHANNEL_NAME",
		"FEISHU_APP_ID", "FEISHU_APP_SECRET", "FEISHU_ENCRYPT_KEY", "FEISHU_VERIFICATION_TOKEN",
		"FEISHU_HOME_CHANNEL", "FEISHU_HOME_CHANNEL_NAME", "YUANBAO_HOME_CHANNEL", "YUANBAO_HOME_CHANNEL_NAME",
		"WECOM_BOT_ID", "WECOM_SECRET", "WECOM_CALLBACK_CORP_ID", "WECOM_CALLBACK_CORP_SECRET",
		"WECOM_CALLBACK_AGENT_ID", "WECOM_CALLBACK_TOKEN", "WECOM_CALLBACK_ENCODING_AES_KEY",
		"WECOM_CALLBACK_HOST", "WECOM_CALLBACK_PORT", "WECOM_HOME_CHANNEL", "WECOM_HOME_CHANNEL_NAME",
		"WEIXIN_ACCOUNT_ID", "WEIXIN_TOKEN", "WEIXIN_BASE_URL", "WEIXIN_CDN_BASE_URL",
		"WEIXIN_HOME_CHANNEL", "WEIXIN_HOME_CHANNEL_NAME", "WEIXIN_DM_POLICY", "WEIXIN_GROUP_POLICY",
		"WEIXIN_ALLOWED_USERS", "WEIXIN_GROUP_ALLOWED_USERS", "WEIXIN_ALLOW_ALL_USERS",
		"BLUEBUBBLES_SERVER_URL", "BLUEBUBBLES_PASSWORD", "BLUEBUBBLES_HOME_CHANNEL", "BLUEBUBBLES_HOME_CHANNEL_NAME",
		"QQ_APP_ID", "QQ_CLIENT_SECRET", "QQBOT_HOME_CHANNEL", "QQBOT_HOME_CHANNEL_NAME",
		"QQ_HOME_CHANNEL", "QQ_HOME_CHANNEL_NAME",
		"QQ_ALLOWED_USERS", "QQ_GROUP_ALLOWED_USERS", "QQ_ALLOW_ALL_USERS", "QQ_MARKDOWN_SUPPORT",
		"QQ_STT_API_KEY", "QQ_STT_BASE_URL", "QQ_STT_MODEL",
		"IRC_SERVER", "IRC_PORT", "IRC_NICKNAME", "IRC_CHANNEL", "IRC_USE_TLS", "IRC_SERVER_PASSWORD",
		"IRC_NICKSERV_PASSWORD", "TERMINAL_ENV", "TERMINAL_SSH_KEY", "TERMINAL_SSH_PORT",
		"ICEBATH_TOOL_PROGRESS_MODE",
		"WHATSAPP_MODE", "WHATSAPP_ENABLED",
		"MATTERMOST_HOME_CHANNEL", "MATTERMOST_HOME_CHANNEL_NAME", "MATTERMOST_REPLY_MODE",
		"MATRIX_PASSWORD", "MATRIX_ENCRYPTION", "MATRIX_DEVICE_ID", "MATRIX_HOME_ROOM",
		"MATRIX_REQUIRE_MENTION", "MATRIX_FREE_RESPONSE_ROOMS", "MATRIX_AUTO_THREAD", "MATRIX_DM_AUTO_THREAD",
		"MATRIX_RECOVERY_KEY",
		"ICEBATH_ACP_AUTH_METHOD", "ICEBATH_ACP_AUTO_APPROVE",
	}, ProfileEnvNames()...)
}

// Process and deployment settings that every profile reads from the process, never profile secrets
func globalEnvNames() []string {
	return []string{
		"ICEBATH_HOME", "ICEBATH_PROFILE", "ICEBATH_GATEWAY_LOCK_DIR",
		"ICEBATH_MAX_ITERATIONS", "ICEBATH_API_TIMEOUT",
		"ICEBATH_REDACT_SECRETS", "ICEBATH_PORTAL_TIMEOUT_SECONDS",
		"_ICEBATH_GATEWAY",
		"PATH", "HOME", "USER", "LANG", "LC_ALL", "TZ", "PWD", "SHELL", "TMPDIR",
		"VIRTUAL_ENV", "PYTHONPATH", "SSL_CERT_FILE",
		"ICEBATH_KANBAN_DB", "ICEBATH_KANBAN_WORKSPACES_ROOT", "ICEBATH_KANBAN_BOARD",
		"API_SERVER_ENABLED", "API_SERVER_HOST", "API_SERVER_PORT",
		"API_SERVER_CORS_ORIGINS",
		"GATEWAY_RELAY_URL", "GATEWAY_RELAY_ENDPOINT",
		"GATEWAY_RELAY_ALLOW_DIRECT_PLATFORMS",
		"GATEWAY_RELAY_PLATFORMS", "GATEWAY_RELAY_BOT_IDS",
		"GATEWAY_RELAY_ROUTE_KEYS", "GATEWAY_RELAY_INSTANCE_ID",
		"GATEWAY_RELAY_WAKE_URL", "GATEWAY_RELAY_DISPLAY_NAME",
	}
}

// Reports a process global setting, read from the process environment under every profile
func IsGlobalEnv(name string) bool {
	if slices.Contains(globalEnvNames(), name) {
		return true
	}
	return slices.ContainsFunc([]string{"ICEBATH_KANBAN_", "ICEBATH_TELEGRAM_", "TERMINAL_"}, func(p string) bool { return strings.HasPrefix(name, p) })
}

// Env write refused for the name itself, malformed or on the writer denylist
type EnvNameError struct {
	Name   string
	Denied bool
}

// The refusal as the user reads it
func (e *EnvNameError) Error() string {
	if !e.Denied {
		return "Invalid environment variable name: " + quoted(e.Name)
	}
	return fmt.Sprintf("Environment variable %s is on the writer denylist. "+
		"Names that influence subprocess execution (LD_PRELOAD, PYTHONPATH, PATH, EDITOR, ...) "+
		"or icebath runtime location and security policy (ICEBATH_HOME, ICEBATH_YOLO_MODE, ...) "+
		"cannot be persisted via the env writer. If you really need this, edit ~/.icebath/.env "+
		"directly.", quoted(e.Name))
}

// Matches ErrEnvWriteRefused
func (e *EnvNameError) Unwrap() error { return ErrEnvWriteRefused }

// Env write refused by a package manager owned install or an administrator pinned variable
type EnvLockedError struct {
	Message string
}

// The refusal as the user reads it
func (e *EnvLockedError) Error() string { return e.Message }

// Matches ErrEnvWriteRefused
func (e *EnvLockedError) Unwrap() error { return ErrEnvWriteRefused }

// Text in upper case under the full Unicode case mapping, which expands letters such as the sharp s
func upperText(s string) string { return cases.Upper(language.Und).String(s) }

// Name of the variable holding the API key of a custom endpoint, one slot per endpoint identity
func CustomEndpointKeyEnv(identity string) string {
	slug := strings.Trim(endpointSlugGap.ReplaceAllString(upperText(identity), "_"), "_")
	if slug == "" {
		return "ICEBATH_CUSTOM_API_KEY"
	}
	return "ICEBATH_CUSTOM_" + slug + "_API_KEY"
}
