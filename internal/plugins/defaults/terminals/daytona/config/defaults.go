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
		About:  "A Daytona cloud sandbox for each session",
		Order:  50,
		Settings: []config.Setting{
			{Key: APIKey, Title: "API key", About: "Your Daytona API key.", Help: "Get one at daytona.io.", Default: "", Env: []string{"DAYTONA_API_KEY"},
				Secret: true},
			{Key: Target, Title: "Region", About: "The region where Daytona sandboxes are made.", Default: "", Zero: "Your account's default",
				Env: []string{"DAYTONA_TARGET"}},
			{Key: Image, Title: "Image", About: "The image that Daytona sandboxes start from.",
				Help: "Left at the default, icebath builds its own sandbox image on Daytona.", Default: sandbox.Image, Env: []string{"TERMINAL_DAYTONA_IMAGE"}},
			{Key: APIURL, Title: "Base URL", About: "The address of the Daytona API.", Default: "", Zero: "The standard address", Env: []string{"DAYTONA_API_URL"},
				Advanced: true},
		},
	}})
}
