package config

import terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"

const (
	Key = terminalconfig.Key + ".modal"
	// Image sandboxes start from, a registry reference or an im- image id
	Image = Key + ".image"
	// Token of the Modal account, ~/.modal.toml when unset
	TokenID     = Key + ".token_id"
	TokenSecret = Key + ".token_secret"
)
