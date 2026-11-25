package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.SetDefault(Provider, "")
	cfg.SetDefault(Model, "")
	cfg.SetDefault(ReasoningEffort, "")
	cfg.SetDefault(MaxTurns, 0)
	cfg.SetDefault(APIMaxRetries, 3)
	cfg.SetDefault(ContextFileMaxChars, 20000)
	cfg.SetDefault(ApprovalsMode, ApprovalsManual)
	cfg.SetDefault(ApprovalsTimeout, 300)
	cfg.SetDefault(ApprovalsAllowlist, []string{})
}
