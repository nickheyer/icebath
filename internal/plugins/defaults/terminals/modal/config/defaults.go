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
		About:  "Commands run in a serverless Modal cloud sandbox, one per session",
		Settings: []config.Setting{
			{Key: Image, Title: "Modal image", About: "Image Modal sandboxes start from, a registry reference or an im- image id; the default builds icebath's own sandbox image on Modal", Default: sandbox.Image, Env: []string{"TERMINAL_MODAL_IMAGE"}},
			{Key: TokenID, Title: "Modal token ID", About: "Token ID of your Modal account, from https://modal.com/settings; empty uses ~/.modal.toml", Default: "", Env: []string{"MODAL_TOKEN_ID"}, Secret: true},
			{Key: TokenSecret, Title: "Modal token secret", About: "Token secret of your Modal account, from https://modal.com/settings; empty uses ~/.modal.toml", Default: "", Env: []string{"MODAL_TOKEN_SECRET"}, Secret: true},
		},
	}})
}
