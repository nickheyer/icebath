package config

import terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"

const (
	Key = terminalconfig.Key + ".daytona"
	// Image sandboxes start from
	Image = Key + ".image"
	// Key of the Daytona account
	APIKey = Key + ".api_key"
	// Address of the Daytona API, Daytona's own when empty
	APIURL = Key + ".api_url"
	// Region sandboxes are made in, the account's default when empty
	Target = Key + ".target"
)
