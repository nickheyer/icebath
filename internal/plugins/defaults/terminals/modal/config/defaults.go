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
		Title:  "Modal",
		About:  "A Modal cloud sandbox for each session",
		Order:  40,
		Settings: []config.Setting{
			{Key: TokenID, Title: "Token ID", About: "Your Modal token ID.",
				Help: "Find the token at modal.com/settings. When none is kept here, icebath reads ~/.modal.toml.", Default: "", Env: []string{"MODAL_TOKEN_ID"},
				Secret: true},
			{Key: TokenSecret, Title: "Token secret", About: "Your Modal token secret.",
				Help: "Find the token at modal.com/settings. When none is kept here, icebath reads ~/.modal.toml.", Default: "",
				Env: []string{"MODAL_TOKEN_SECRET"}, Secret: true},
			{Key: Image, Title: "Image", About: "The image that Modal sandboxes start from.",
				Help: "A registry image or a Modal image ID that starts with im-. " +
					"Left at the default, icebath builds its own image on Modal.", Default: sandbox.Image, Env: []string{"TERMINAL_MODAL_IMAGE"}},
		},
	}})
}
