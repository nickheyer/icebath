package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.SetDefault(BaseURL, "https://aiplatform.googleapis.com")
	cfg.SetDefault(DefaultModel, "google/gemini-3.6-flash")
}
