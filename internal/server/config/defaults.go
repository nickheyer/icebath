package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Global: true,
		Title:  "Server",
		About:  "The address and password apps use to reach icebath",
		Group:  config.GroupApp,
		Order:  40,
		Settings: []config.Setting{
			{Key: Address, Title: "Address", About: "The address and port the server listens on.",
				Help: "Such as 127.0.0.1:7433. Only this computer can reach an address that starts with 127.", Default: "127.0.0.1:7433", Apply: config.Restart},
			{Key: Token, Title: "Password", About: "The password apps connect with.", Help: "icebath makes one the first time the server starts.", Default: "",
				Secret: true, Apply: config.Restart},
		},
	}})
}
