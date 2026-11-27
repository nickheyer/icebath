package config

const (
	Key                = "skills"
	Disabled           = Key + ".disabled"
	PlatformDisabled   = Key + ".platform_disabled"
	ExternalDirs       = Key + ".external_dirs"
	CreateDir          = Key + ".create_dir"
	TrustedProjectDirs = Key + ".trusted_project_dirs"
	ProjectDiscovery   = Key + ".project_discovery"
	AutoLoad           = Key + ".auto_load"
	TemplateVars       = Key + ".template_vars"
	InlineShell        = Key + ".inline_shell"
	InlineShellTimeout = Key + ".inline_shell_timeout"
	GuardAgentCreated  = Key + ".guard_agent_created"
	WriteApproval      = Key + ".write_approval"
	Ledger             = Key + ".ledger"
	LedgerMaxBytes     = Key + ".ledger_max_bytes"
	BundlesDir         = Key + ".bundles_dir"
	Tier1Advisory      = Key + ".tier1_advisory"
	// Section of the values skills declare they read, one setting each by the key the skill names
	Values = Key + ".config"
	// Section of the environment variables skills need, one secret setting each by the variable's name
	Variables = Key + ".env"
)
