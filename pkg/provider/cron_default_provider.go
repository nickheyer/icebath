package provider

import "github.com/nickheyer/icebath/pkg/cron/cronconfig"

// Provider every unpinned cron job resolves through, the cron.model_provider setting
func cronDefaultProvider(cfg map[string]any) string { return cronconfig.DefaultProvider(cfg) }
