package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Global: true,
		Title:  "Server",
		About:  "Backend for the app and CLI",
		Settings: []config.Setting{
			{Key: Address, Title: "Address", About: "Address the server listens on", Default: "127.0.0.1:7433"},
			{Key: Token, Title: "Token", About: "Password clients connect with, generated on first start", Default: "", Secret: true},
		},
	}})
}
