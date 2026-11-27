package config

import (
	"github.com/nickheyer/icebath/internal/config"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
	"github.com/nickheyer/icebath/internal/terminal/sandbox"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: terminalconfig.Key,
		Title:  "Daytona",
		About:  "Commands run in a persistent Daytona cloud sandbox, one per session",
		Settings: []config.Setting{
			{Key: Image, Title: "Daytona image", About: "Image Daytona sandboxes start from; the default builds icebath's own sandbox image on Daytona", Default: sandbox.Image, Env: []string{"TERMINAL_DAYTONA_IMAGE"}},
			{Key: APIKey, Title: "Daytona API key", About: "API key of your Daytona account, from https://daytona.io", Default: "", Env: []string{"DAYTONA_API_KEY"}, Secret: true},
			{Key: APIURL, Title: "Daytona API address", About: "Address of the Daytona API; empty uses Daytona's own", Default: "", Env: []string{"DAYTONA_API_URL"}},
			{Key: Target, Title: "Daytona target", About: "Region Daytona sandboxes are made in; empty uses your account's default", Default: "", Env: []string{"DAYTONA_TARGET"}},
		},
	}})
}
