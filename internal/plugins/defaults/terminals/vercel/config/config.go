package config

import terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"

const (
	Key = terminalconfig.Key + ".vercel_sandbox"
	// Image sandboxes start from, a Vercel managed image or a VCR repository[:tag]
	Image = Key + ".image"
	// Language runtime, which Vercel deprecated, taking the place of the image when set
	Runtime = Key + ".runtime"
	// Access token and the project and team it acts for
	Token     = Key + ".token"
	ProjectID = Key + ".project_id"
	TeamID    = Key + ".team_id"
	// Development token whose claims name the project and team
	OIDCToken = Key + ".oidc_token"
)

// Image a sandbox starts from when the setting names none
const DefaultImage = "vercel/sandbox/universal:latest"

// Runtimes the runtime setting may name
var Runtimes = []string{"node24", "node22", "python3.13"}
